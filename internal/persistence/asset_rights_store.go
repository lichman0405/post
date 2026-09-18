package persistence

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/assetpublish"
	"github.com/lichman0405/post/internal/application/assetrights"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// AssetRightsStore is the production adapter for the asset rights-holder
// governance command (T0711): the append-only chain write and its reads,
// plus the credit rows the publish transaction writes.
//
// # One transaction, and what it is
//
// ChangeHolder runs everything in ONE transaction, in this order:
//
//	lock the project          →  the project the authorization was resolved
//	                             against must exist (GetProjectByIDForUpdate)
//	lock the asset by pid     →  serializes every change to this asset
//	                             (GetResearchAssetByPIDForUpdate)
//	verify the asset's origin project is the one the caller named
//	resolve the holder        →  the named user or organization must exist
//	read the current holder   →  the previous holder, or none
//	refuse a no-op change
//	append the event          →  ordinal = current + 1
//	append the audit row      →  with before/after read from the chain
//
// The asset row lock is what makes the ordinal a sequence rather than a
// race: two transfers of one asset take the lock in turn, so the second
// reads the first's holder as its previous holder and writes the next
// ordinal. Nothing here reads-then-writes without a lock: the previous
// holder a change records is read under the same lock that serializes the
// append, so the chain cannot fork.
//
// # The asset's origin project is verified, not assumed
//
// The command resolves the actor's membership in the project the request
// named and then this store looks up the asset. If the asset belongs to a
// DIFFERENT project, the change is refused with ErrAssetNotFound: a caller
// authorized in project A must not be able to write governance for an
// asset of project B by naming A. The refusal is the same not-found an
// unknown pid gets, so the pair cannot be used as an oracle.
//
// # What the write does not touch
//
// Nothing outside asset_rights_holder_events and audit_log. In particular
// no research_asset_versions row (a transfer is not a publication, and
// versions are append-only anyway — 00014), no research_assets row (the
// asset's id, pid, slug, title and origin project are untouched), and no
// asset_version_parties row (a creator is a signature, not an ownership
// stake: 换持有者不是换作者).
type AssetRightsStore struct {
	pool *pgxpool.Pool
}

// NewAssetRightsStore builds the store on pool. The pool may be lazy
// (OpenLazy): the API keeps starting while PostgreSQL is down.
func NewAssetRightsStore(pool *pgxpool.Pool) *AssetRightsStore {
	return &AssetRightsStore{pool: pool}
}

// ChangeHolder implements assetrights.StorePort. See the type doc for the
// transaction it runs; this method is the transaction.
func (s *AssetRightsStore) ChangeHolder(ctx context.Context, req assetrights.ChangeRequest) (assetrights.HolderChange, error) {
	projectID, err := textUUID(req.ProjectID)
	if err != nil {
		return assetrights.HolderChange{}, assetrights.ErrProjectNotFound
	}
	actorID, err := textUUID(req.Actor.User.ID)
	if err != nil {
		return assetrights.HolderChange{}, fmt.Errorf("%w: acting user id: %v", assetrights.ErrStore, err)
	}
	holderID, err := textUUID(req.Holder.ID)
	if err != nil {
		return assetrights.HolderChange{}, fmt.Errorf("%w: holder id %q is not a uuid: %v", assetrights.ErrValidation, req.Holder.ID, err)
	}
	// One correlation id for the change: the request's when the
	// observability middleware attached one, else a fresh one — the same
	// fallback the publish and release stores use, so the audit row and
	// the event always share one trace id.
	correlationID := ""
	if info, ok := domain.RequestInfoFrom(ctx); ok {
		correlationID = info.CorrelationID
	}
	if correlationID == "" {
		id, err := observability.NewCorrelationID()
		if err != nil {
			return assetrights.HolderChange{}, fmt.Errorf("%w: change correlation id: %v", assetrights.ErrStore, err)
		}
		correlationID = id.String()
	}

	var out assetrights.HolderChange
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if _, err := q.GetProjectByIDForUpdate(ctx, projectID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return assetrights.ErrProjectNotFound
			}
			return err
		}

		asset, err := q.GetResearchAssetByPIDForUpdate(ctx, req.AssetPID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return assetrights.ErrAssetNotFound
			}
			return err
		}
		assetID := asset.ID
		// Two PARSED uuids are compared, not the request's text against the
		// row's text: a uuid has one value and several spellings (case),
		// and prepare trims the request without folding it, so a text
		// comparison answered "not found" for the caller's own project
		// written in another case. Both sides are already values here —
		// projectID came out of textUUID above.
		if projectID != asset.OriginProjectID {
			// The asset exists but not in the project the caller's
			// authorization was resolved against. Refused as not-found for
			// the same reason the project read gate refuses: the caller has
			// established no right to know where else this pid points.
			return assetrights.ErrAssetNotFound
		}

		// The holder must be a real row of the table its kind names,
		// resolved BEFORE anything is written: an append-only chain that
		// named nobody would be a record no later reader could repair.
		holder, err := resolveParty(ctx, q, req.Holder, holderID)
		if err != nil {
			return err
		}

		// The current holder, read under the asset lock. No row means the
		// asset has never had one, which is a state rather than an error:
		// the first event of such an asset records a NULL previous holder
		// and ordinal 1.
		ordinal := int32(1)
		var previous *assetrights.PartyIdentity
		current, err := q.GetCurrentAssetRightsHolder(ctx, assetID)
		switch {
		case err == nil:
			ordinal = current.Ordinal + 1
			prev, prevErr := storedPartyIdentity(ctx, q,
				domain.Party{Kind: domain.PartyKind(current.HolderKind), ID: current.HolderID})
			if prevErr != nil {
				return prevErr
			}
			previous = &prev
			if prev.Party == holder.Party {
				return assetrights.ErrNoChange
			}
		case errors.Is(err, pgx.ErrNoRows):
			// never held: fall through with a NULL previous holder
		default:
			return err
		}

		row, err := q.CreateAssetRightsHolderEvent(ctx, sqlc.CreateAssetRightsHolderEventParams{
			AssetID:            assetID,
			Ordinal:            ordinal,
			HolderKind:         string(holder.Party.Kind),
			HolderID:           holderID,
			PreviousHolderKind: previousKind(previous),
			PreviousHolderID:   previousID(previous),
			RecordedBy:         actorID,
		})
		if err != nil {
			return err
		}

		// The audit row commits with the event (docs/53: the audit record
		// of a high-risk action is part of the action). Its two summaries
		// are read from the chain this transaction just read — the before
		// half is the previous holder, null when there was none, and never
		// a default.
		audit := req.Audit
		audit.CorrelationID = correlationID
		audit.BeforeSummary = partySummary(previous)
		audit.AfterSummary = partySummary(&holder)
		audit.Metadata = map[string]any{
			"asset_pid": req.AssetPID,
			"ordinal":   int(ordinal),
		}
		if err := appendAudit(ctx, q, audit); err != nil {
			return err
		}

		out = assetrights.HolderChange{
			EventID:    pgUUIDToText(row.ID),
			AssetID:    pgUUIDToText(row.AssetID),
			AssetPID:   req.AssetPID,
			Ordinal:    int(row.Ordinal),
			Holder:     holder,
			Previous:   previous,
			RecordedBy: pgUUIDToText(row.RecordedBy),
			RecordedAt: row.RecordedAt.Time,
		}
		return nil
	})
	if err != nil {
		return assetrights.HolderChange{}, mapRightsStoreError(err)
	}
	return out, nil
}

// CurrentHolder implements assetrights.HolderReader: the party the chain
// currently names, or nil when the asset has never had one.
func (s *AssetRightsStore) CurrentHolder(ctx context.Context, assetPID string) (*assetrights.HolderEvent, error) {
	assetID, err := s.assetIDByPID(ctx, assetPID)
	if err != nil {
		return nil, err
	}
	if assetID == nil {
		return nil, assetrights.ErrAssetNotFound
	}
	q := sqlc.New(s.pool)
	row, err := q.GetCurrentAssetRightsHolder(ctx, *assetID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("persistence: read current rights holder: %w", err)
	}
	event, err := holderEventFromParts(ctx, q, row.ID, row.Ordinal,
		row.HolderKind, row.HolderID, row.PreviousHolderKind, row.PreviousHolderID,
		row.RecordedBy, row.RecordedAt)
	if err != nil {
		return nil, err
	}
	return &event, nil
}

// HolderHistory implements assetrights.HolderReader: the asset's whole
// chain, oldest first. Every row is returned, because nothing in the table
// is ever updated or deleted (00082's triggers) — the previous holder of
// each change is one of these rows.
func (s *AssetRightsStore) HolderHistory(ctx context.Context, assetPID string) ([]assetrights.HolderEvent, error) {
	assetID, err := s.assetIDByPID(ctx, assetPID)
	if err != nil {
		return nil, err
	}
	if assetID == nil {
		return nil, assetrights.ErrAssetNotFound
	}
	q := sqlc.New(s.pool)
	rows, err := q.ListAssetRightsHolderEvents(ctx, *assetID)
	if err != nil {
		return nil, fmt.Errorf("persistence: read rights holder history: %w", err)
	}
	out := make([]assetrights.HolderEvent, 0, len(rows))
	for _, row := range rows {
		event, err := holderEventFromParts(ctx, q, row.ID, row.Ordinal,
			row.HolderKind, row.HolderID, row.PreviousHolderKind, row.PreviousHolderID,
			row.RecordedBy, row.RecordedAt)
		if err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, nil
}

// assetIDByPID resolves the internal asset id behind a pid. A pid that is
// not a uuid-shaped row id is impossible (00064), and a pid naming no
// asset answers nil rather than an error, so the caller decides which
// outcome that is.
func (s *AssetRightsStore) assetIDByPID(ctx context.Context, assetPID string) (*pgtype.UUID, error) {
	row, err := sqlc.New(s.pool).GetResearchAssetByPIDRow(ctx, assetPID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("persistence: resolve asset %q: %w", assetPID, err)
	}
	return &row.ID, nil
}

// holderEventFromParts assembles one chain row, resolving the identities
// of both parties. The previous holder is resolved through the same
// table-lookup the current one is: a chain row names a kind and an id, and
// the name rendered beside it comes from the table that kind names — never
// from the other kind's shape.
func holderEventFromParts(ctx context.Context, q *sqlc.Queries, id string, ordinal int32,
	holderKind, holderID string, prevKind *string, prevID pgtype.UUID,
	recordedBy string, recordedAt pgtype.Timestamptz) (assetrights.HolderEvent, error) {
	holder, err := storedPartyIdentity(ctx, q, domain.Party{Kind: domain.PartyKind(holderKind), ID: holderID})
	if err != nil {
		return assetrights.HolderEvent{}, err
	}
	var previous *assetrights.PartyIdentity
	if prevKind != nil {
		// The pair is all-or-nothing in the database (00082's CHECK), so a
		// kind without an id is a row nothing could have written. It is
		// refused as a store failure rather than read as a party with an
		// empty id, which would be a chain row naming nobody.
		if !prevID.Valid {
			return assetrights.HolderEvent{}, fmt.Errorf(
				"%w: chain row %s records a previous holder kind %q with no id, which 00082's CHECK refuses",
				assetrights.ErrStore, id, *prevKind)
		}
		prev, err := storedPartyIdentity(ctx, q, domain.Party{Kind: domain.PartyKind(*prevKind), ID: pgUUIDToText(prevID)})
		if err != nil {
			return assetrights.HolderEvent{}, err
		}
		previous = &prev
	}
	return assetrights.HolderEvent{
		EventID:    id,
		Ordinal:    int(ordinal),
		Holder:     holder,
		Previous:   previous,
		RecordedBy: recordedBy,
		RecordedAt: recordedAt.Time,
	}, nil
}

// storedPartyIdentity resolves a party that is already STORED. A stored
// pair whose row has gone missing is a store failure rather than a
// not-found outcome: the write path refused to store one, so an unresolvable
// row means the record is broken, and rendering a chain whose holder has no
// name would hide that.
func storedPartyIdentity(ctx context.Context, q *sqlc.Queries, party domain.Party) (assetrights.PartyIdentity, error) {
	id, err := textUUID(party.ID)
	if err != nil {
		return assetrights.PartyIdentity{}, fmt.Errorf("%w: stored %s id %q is not a uuid: %v", assetrights.ErrStore, party.Kind, party.ID, err)
	}
	identity, err := resolveParty(ctx, q, party, id)
	if errors.Is(err, assetrights.ErrPartyNotFound) {
		return assetrights.PartyIdentity{}, fmt.Errorf("%w: stored %s holder %q names no row", assetrights.ErrStore, party.Kind, party.ID)
	}
	return identity, err
}

// resolveParty reads the identity of one party from the table its kind
// names, and answers ErrPartyNotFound when the id is not a row of it. The
// kind decides the table — that is the whole reason the kind is stored
// beside the id — and there is no path here that resolves an id without
// consulting the kind first.
func resolveParty(ctx context.Context, q *sqlc.Queries, party domain.Party, id pgtype.UUID) (assetrights.PartyIdentity, error) {
	switch party.Kind {
	case domain.PartyUser:
		row, err := q.GetPartyUser(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return assetrights.PartyIdentity{}, assetrights.ErrPartyNotFound
		}
		if err != nil {
			return assetrights.PartyIdentity{}, err
		}
		return assetrights.PartyIdentity{
			Party:       domain.Party{Kind: domain.PartyUser, ID: row.ID},
			Handle:      row.Handle,
			DisplayName: row.DisplayName,
		}, nil
	case domain.PartyOrganization:
		row, err := q.GetPartyOrganization(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return assetrights.PartyIdentity{}, assetrights.ErrPartyNotFound
		}
		if err != nil {
			return assetrights.PartyIdentity{}, err
		}
		return assetrights.PartyIdentity{
			Party:       domain.Party{Kind: domain.PartyOrganization, ID: row.ID},
			Handle:      row.Slug,
			DisplayName: row.Name,
		}, nil
	default:
		// Unreachable from the command (prepare refuses it) and from a
		// stored row (00082's CHECK admits the two identity kinds). Kept as
		// a refusal rather than a panic: a kind with no table has no
		// identity to read, and the honest answer is to refuse.
		return assetrights.PartyIdentity{}, fmt.Errorf("%w: no identity table for party kind %q", assetrights.ErrValidation, party.Kind)
	}
}

// previousKind renders the previous holder's kind for the insert — NULL
// when there is no previous holder (the first designation of an unheld
// asset), never a zero value that would name a table nobody was in.
func previousKind(previous *assetrights.PartyIdentity) *string {
	if previous == nil {
		return nil
	}
	kind := string(previous.Party.Kind)
	return &kind
}

// previousID renders the previous holder's id for the insert, NULL
// alongside the NULL kind (00082's all-or-nothing CHECK).
func previousID(previous *assetrights.PartyIdentity) pgtype.UUID {
	if previous == nil {
		return pgtype.UUID{}
	}
	id, err := textUUID(previous.Party.ID)
	if err != nil {
		return pgtype.UUID{}
	}
	return id
}

// partySummary renders one party for the audit row's before/after
// summaries. The pair is always both halves — kind and id — and the kind
// is spelled by name rather than as a flag, so a reader of the audit log
// can tell an organization from a person without joining anything.
func partySummary(identity *assetrights.PartyIdentity) any {
	if identity == nil {
		return nil
	}
	return map[string]any{
		"holder_kind": string(identity.Party.Kind),
		"holder_id":   identity.Party.ID,
	}
}

// insertVersionCredits writes one credit row per declared creator id, in
// the order the publish declared them (position 0, 1, …). It runs inside
// the publish transaction, between the version insert and the audit row,
// so a version row that exists always has exactly the credits its publish
// carried.
//
// The role is RoleCreator for every entry because that is what the field
// IS: internal/assets.PublishCandidate.CreatorIDs is the version's
// creators (docs/11 §3's "creators/contributors" item, whose second half
// this platform does not yet collect). Writing them all as "contributor"
// or as a merged credit would be the conflation docs/11 §6 forbids.
//
// Each id is a user id: the publish gate refuses a creator id that is not
// a uuid (internal/assets.validCreatorIDs), so a conversion failure after
// the normalization below means the gate and this store disagree about
// what a creator id is — a wiring failure, reported as one
// (assetpublish.ErrStore) rather than blamed on the caller whose request
// the gate approved. It fails the publish instead of dropping the credit:
// a version that exists without the credits its publish declared would be
// exactly the half-recorded state these tables exist to close.
//
// # Why this layer normalizes an id the gate already blessed
//
// Because the gate blessed the caller's TEXT and this function turns that
// text into a stored key. Gate is a predicate: it trims and folds case on
// a copy, decides, and returns the candidate untouched — it does not hand
// this store a tidied list. pgtype.UUID.Scan, which textUUID uses, accepts
// no whitespace at all, so a "  <uuid>  " the gate admits would fail here
// and reach the caller as a 503 on a request that succeeded before 00082
// existed (T0711 review, round 8). The fix is not to narrow the gate — a
// padded id is a user id, the gate's duplicate rule already folds case,
// and refusing it now would turn today's accepted request into a failed
// one — and it is not to re-spell the rule here either: the value is
// assets.CanonicalCreatorID, the same function the gate's own rules call,
// so the two layers cannot drift apart again.
//
// Deleting this call as redundant defence ("the gate already checked") is
// how that returns. Case is the quieter half of the same point:
// pgtype.UUID.Scan reads hex, so it accepts the uppercase spelling, and
// the column is a uuid — 128 bits, not text — so an uppercase id would
// have stored the same value anyway. What it would not have been is the
// same STRING, and this store's rows are keyed by it (00082's UNIQUE
// (asset_version_id, role, party_id)): one user written two ways would be
// two keys the gate reasoned about as one.
func insertVersionCredits(ctx context.Context, q *sqlc.Queries, versionID pgtype.UUID, recordedBy pgtype.UUID, creatorIDs []string) error {
	for i, raw := range creatorIDs {
		id := assets.CanonicalCreatorID(raw)
		partyID, err := textUUID(id)
		if err != nil {
			return fmt.Errorf("%w: the publish gate admitted creator_ids[%d] %q, which is not a user id: %v", assetpublish.ErrStore, i, raw, err)
		}
		if _, err := q.InsertAssetVersionParty(ctx, sqlc.InsertAssetVersionPartyParams{
			AssetVersionID: versionID,
			Role:           string(domain.RoleCreator),
			PartyKind:      string(domain.PartyUser),
			PartyID:        partyID,
			Position:       int32(i),
			RecordedBy:     recordedBy,
		}); err != nil {
			return err
		}
	}
	return nil
}

// mapRightsStoreError keeps the governance sentinels (the command's
// contract) and wraps everything else with the persistence context.
func mapRightsStoreError(err error) error {
	switch {
	case err == nil,
		errors.Is(err, assetrights.ErrValidation),
		errors.Is(err, assetrights.ErrProjectNotFound),
		errors.Is(err, assetrights.ErrAssetNotFound),
		errors.Is(err, assetrights.ErrPartyNotFound),
		errors.Is(err, assetrights.ErrNoChange),
		errors.Is(err, assetrights.ErrStore):
		return err
	}
	return fmt.Errorf("persistence: rights holder change write: %w", err)
}
