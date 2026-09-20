package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/credit"
	"github.com/lichman0405/post/internal/contribution"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// CreditStore is the production adapter for the credit attribution and
// credit dispute commands (T0809): the append-only declaration chain, the
// dispute current-state row and its event pair, and the reads of both.
//
// Plain pgx for the credit tables, like OpportunityStore and LedgerStore
// and for the same reason: these columns belong to this subsystem, and
// sqlc queries would put them on the shared persistence surface. The audit
// row goes through appendAudit (sqlc), because audit_log is shared.
//
// # DeclareAttribution: one transaction, and the lock that orders it
//
//	lock the project        →  the project the authorization was resolved
//	                           against must exist (GetProjectByIDForUpdate)
//	lock the target         →  serializes every declaration about it
//	verify the target belongs to the project the caller named
//	read the current chain  →  ordinal = max + 1, and the previous parties
//	resolve every party     →  each must be a real user/organization row
//	append the statement
//	append its party rows
//	append the audit row
//
// The target row lock is what makes the ordinal a sequence rather than a
// race, exactly as the asset row lock does for the rights-holder chain
// (00082): two declarations about one target take the lock in turn, so the
// second reads the first's ordinal and writes the next one. Nothing is
// read-then-written without it.
//
// # What the declare writes does not touch
//
// Nothing outside credit_attribution_statements,
// credit_attribution_parties and audit_log. In particular NO
// contribution_events row is read, written or rewritten: docs/13 §2's
// correction is a new declaration, and the ledger's record of the events
// the credit is about is the ledger's. That is the property the acceptance
// "修正不改 ledger 原事件" is checked through, and it holds by
// construction — this file has no statement that writes a ledger row.
//
// # OpenDispute / CloseDispute: the state row, its event, its audit
//
//	open:   lock project, resolve target, INSERT the dispute row (state
//	        open), record credit.dispute_opened, append the audit row
//	close:  lock the dispute row, refuse unless it is still open, UPDATE
//	        state/resolution/resolved_at, record credit.dispute_resolved,
//	        append the audit row
//
// The UPDATE touches exactly the three columns 00103's guard leaves open:
// the row's identity, its project, its opener, its target, its claim and
// its opening instant are pinned by the trigger, so a resolution answers
// the claim and never edits it (docs/13 §3's "不得直接改原事件").
//
// # The event payload carries identity and references, not prose
//
// The dispute's claim and the decision's resolution are NOT copied into
// the event payloads; the payload names the dispute, its target, the
// ledger evidence the claim points at, and (for a close) the outcome. That
// is the rule internal/persistence/review_store.go states for a review
// body ("identity and reference only ... never the review prose") and
// docs/52 states for payloads generally: an event is a fact about an act,
// not a place prose is copied to. Both strings stay where they were
// written — the claim in a column 00103 pins, the resolution in the row
// that records the decision — so nothing is lost by leaving them out, and
// the append-only ledger keeps the sequence of opens and closes that is
// the dispute's history.
type CreditStore struct {
	pool *pgxpool.Pool
}

// NewCreditStore builds the store on pool. The pool may be lazy
// (OpenLazy): the API keeps starting while PostgreSQL is down.
func NewCreditStore(pool *pgxpool.Pool) *CreditStore {
	return &CreditStore{pool: pool}
}

// The event names the dispute pair is recorded under. They are the two
// registered names in specs/events/event-types.yaml, which the ledger's
// mapping table already projects (internal/contribution/ledger.go): this
// store is their first producer.
const (
	eventCreditDisputeOpened   = "credit.dispute_opened"
	eventCreditDisputeResolved = "credit.dispute_resolved"
)

// DeclareAttribution implements credit.StorePort. See the type doc for the
// transaction it runs; this method is the transaction.
func (s *CreditStore) DeclareAttribution(ctx context.Context, req credit.DeclareRequest) (contribution.CreditAttribution, error) {
	projectID, err := textUUID(req.ProjectID)
	if err != nil {
		return contribution.CreditAttribution{}, credit.ErrProjectNotFound
	}
	actorID, err := textUUID(req.Actor.User.ID)
	if err != nil {
		return contribution.CreditAttribution{}, fmt.Errorf("%w: acting user id: %v", credit.ErrStore, err)
	}
	kind, value, ok := contribution.ParseCreditTargetRef(req.TargetRef)
	if !ok {
		return contribution.CreditAttribution{}, fmt.Errorf("%w: target ref %q", credit.ErrValidation, req.TargetRef)
	}
	correlationID, err := creditCorrelationID(ctx)
	if err != nil {
		return contribution.CreditAttribution{}, err
	}

	var out contribution.CreditAttribution
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if _, err := q.GetProjectByIDForUpdate(ctx, projectID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return credit.ErrProjectNotFound
			}
			return err
		}
		if err := resolveCreditTarget(ctx, tx, kind, value, projectID, true); err != nil {
			return err
		}

		ordinal := 1
		var previous *contribution.CreditAttribution
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(MAX(ordinal), 0) FROM credit_attribution_statements
			  WHERE project_id = $1 AND target_kind = $2 AND target_ref = $3`,
			projectID, string(kind), req.TargetRef).Scan(&ordinal); err != nil {
			return err
		}
		ordinal++
		if ordinal > 1 {
			prev, err := readAttribution(ctx, tx, req.ProjectID, req.TargetRef, ordinal-1)
			if err != nil {
				return err
			}
			previous = &prev
		}

		// Every party must be a real row of the table its kind names,
		// resolved BEFORE anything is written: an append-only declaration
		// that named nobody would be a record no later reader could repair
		// (the rule asset_rights_store.resolveParty states for the same
		// reason).
		identities := make([]contribution.CreditParty, 0, len(req.Parties))
		for _, p := range req.Parties {
			id, err := textUUID(p.Party.ID)
			if err != nil {
				return fmt.Errorf("%w: party id %q is not a uuid: %v", credit.ErrValidation, p.Party.ID, err)
			}
			identity, err := creditPartyIdentity(ctx, tx, p.Party.Kind, id)
			if err != nil {
				return err
			}
			identities = append(identities, contribution.CreditParty{
				Party:       contribution.PartyRef{Kind: string(p.Party.Kind), ID: pgUUIDToText(id)},
				Role:        p.Role,
				Position:    p.Position,
				Handle:      identity.Handle,
				DisplayName: identity.DisplayName,
			})
		}

		var statementID pgtype.UUID
		var recordedAt pgtype.Timestamptz
		if err := tx.QueryRow(ctx,
			`INSERT INTO credit_attribution_statements
			   (project_id, target_kind, target_ref, ordinal, recorded_by)
			 VALUES ($1, $2, $3, $4, $5)
			 RETURNING id, recorded_at`,
			projectID, string(kind), req.TargetRef, ordinal, actorID).Scan(&statementID, &recordedAt); err != nil {
			return err
		}
		for _, p := range identities {
			partyID, err := textUUID(p.Party.ID)
			if err != nil {
				return fmt.Errorf("%w: party id %q is not a uuid: %v", credit.ErrValidation, p.Party.ID, err)
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO credit_attribution_parties
				   (statement_id, role, party_kind, party_id, "position")
				 VALUES ($1, $2, $3, $4, $5)`,
				statementID, string(p.Role), p.Party.Kind, partyID, p.Position); err != nil {
				return err
			}
		}

		// The audit row commits with the declaration (docs/53: the audit
		// record of a high-risk action is part of the action). Its before
		// half is the declaration this one corrects — read from the chain
		// under the same lock, null when there was none, never a default.
		audit := req.Audit
		audit.CorrelationID = correlationID
		audit.BeforeSummary = attributionSummary(previous)
		audit.AfterSummary = attributionSummary(&contribution.CreditAttribution{
			TargetRef: req.TargetRef,
			Ordinal:   ordinal,
			Parties:   identities,
		})
		audit.Metadata = map[string]any{
			"target_kind": string(kind),
			"ordinal":     ordinal,
			"parties":     len(identities),
		}
		if err := appendAudit(ctx, q, audit); err != nil {
			return err
		}

		out = contribution.CreditAttribution{
			ID:         pgUUIDToText(statementID),
			ProjectID:  req.ProjectID,
			TargetKind: kind,
			TargetRef:  req.TargetRef,
			Ordinal:    ordinal,
			Parties:    identities,
			RecordedBy: req.Actor.User.ID,
			RecordedAt: recordedAt.Time,
		}
		return nil
	})
	if err != nil {
		return contribution.CreditAttribution{}, mapCreditStoreError(err)
	}
	return out, nil
}

// OpenDispute implements credit.StorePort.
func (s *CreditStore) OpenDispute(ctx context.Context, req credit.OpenDisputeRequest) (contribution.CreditDispute, error) {
	projectID, err := textUUID(req.ProjectID)
	if err != nil {
		return contribution.CreditDispute{}, credit.ErrProjectNotFound
	}
	actorID, err := textUUID(req.Actor.User.ID)
	if err != nil {
		return contribution.CreditDispute{}, fmt.Errorf("%w: acting user id: %v", credit.ErrStore, err)
	}
	kind, value, ok := contribution.ParseCreditTargetRef(req.TargetRef)
	if !ok {
		return contribution.CreditDispute{}, fmt.Errorf("%w: target ref %q", credit.ErrValidation, req.TargetRef)
	}
	correlationID, err := creditCorrelationID(ctx)
	if err != nil {
		return contribution.CreditDispute{}, err
	}

	var out contribution.CreditDispute
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if _, err := q.GetProjectByIDForUpdate(ctx, projectID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return credit.ErrProjectNotFound
			}
			return err
		}
		// The target must exist and belong to the project the caller's
		// authorization was resolved against. No row lock is taken: a
		// dispute does not order itself against anything about the target
		// (the chain is the declaration's, and its lock is the
		// declaration's).
		if err := resolveCreditTarget(ctx, tx, kind, value, projectID, false); err != nil {
			return err
		}

		var disputeID pgtype.UUID
		var openedAt pgtype.Timestamptz
		if err := tx.QueryRow(ctx,
			`INSERT INTO credit_disputes (project_id, opened_by, target_ref, claim, state)
			 VALUES ($1, $2, $3, $4, 'open')
			 RETURNING id, opened_at`,
			projectID, actorID, req.TargetRef, req.Claim).Scan(&disputeID, &openedAt); err != nil {
			return err
		}

		payload, err := json.Marshal(disputeOpenedPayload{
			PayloadVersion: 1,
			DisputeID:      pgUUIDToText(disputeID),
			TargetRef:      req.TargetRef,
			TargetKind:     string(kind),
			EvidenceRefs:   req.EvidenceRefs,
		})
		if err != nil {
			return fmt.Errorf("persistence: render dispute opened payload: %w", err)
		}
		// The event is PRIVATE, deliberately and not by fallback: a claim
		// is an accusation, docs/13 §3 keeps a dispute out of the public
		// reputation until it is resolved, and docs/12 §3 forbids an event
		// being more visible than its subject — no surface makes a credit
		// dispute public, so there is nothing to widen to.
		if err := events.Record(ctx, tx, events.Event{
			EventType:     eventCreditDisputeOpened,
			ActorID:       req.Actor.User.ID,
			ProjectID:     req.ProjectID,
			Visibility:    events.VisibilityPrivate,
			CorrelationID: correlationID,
			Payload:       payload,
		}); err != nil {
			return err
		}

		audit := req.Audit
		audit.CorrelationID = correlationID
		audit.AfterSummary = map[string]any{
			"dispute_id": pgUUIDToText(disputeID),
			"state":      string(contribution.DisputeStateOpen),
		}
		audit.Metadata = map[string]any{
			"target_kind":    string(kind),
			"evidence_count": len(req.EvidenceRefs),
		}
		if err := appendAudit(ctx, q, audit); err != nil {
			return err
		}

		out = contribution.CreditDispute{
			ID:           pgUUIDToText(disputeID),
			ProjectID:    req.ProjectID,
			OpenedBy:     req.Actor.User.ID,
			TargetKind:   kind,
			TargetRef:    req.TargetRef,
			Claim:        req.Claim,
			State:        contribution.DisputeStateOpen,
			OpenedAt:     openedAt.Time,
			EvidenceRefs: req.EvidenceRefs,
		}
		return nil
	})
	if err != nil {
		return contribution.CreditDispute{}, mapCreditStoreError(err)
	}
	return out, nil
}

// CloseDispute implements credit.StorePort.
func (s *CreditStore) CloseDispute(ctx context.Context, req credit.CloseDisputeRequest) (contribution.CreditDispute, error) {
	projectID, err := textUUID(req.ProjectID)
	if err != nil {
		return contribution.CreditDispute{}, credit.ErrProjectNotFound
	}
	if _, err := textUUID(req.Actor.User.ID); err != nil {
		return contribution.CreditDispute{}, fmt.Errorf("%w: acting user id: %v", credit.ErrStore, err)
	}
	disputeID, err := textUUID(req.DisputeID)
	if err != nil {
		return contribution.CreditDispute{}, fmt.Errorf("%w: dispute id %q is not a uuid: %v", credit.ErrValidation, req.DisputeID, err)
	}
	if !req.Outcome.Terminal() {
		return contribution.CreditDispute{}, fmt.Errorf("%w: outcome %q is not a decision", credit.ErrValidation, req.Outcome)
	}
	correlationID, err := creditCorrelationID(ctx)
	if err != nil {
		return contribution.CreditDispute{}, err
	}

	var out contribution.CreditDispute
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if _, err := q.GetProjectByIDForUpdate(ctx, projectID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return credit.ErrProjectNotFound
			}
			return err
		}

		// The row lock is taken before the state is read, so two deciders
		// race on the lock rather than on the value: the second reads the
		// first's state and is refused. The state is read from the ROW and
		// not from the request, because it is the row that decides whether
		// the dispute is still open.
		stored, err := readDisputeRow(ctx, tx, disputeID, projectID, true)
		if err != nil {
			return err
		}
		if stored.State != contribution.DisputeStateOpen {
			return credit.ErrDisputeClosed
		}

		var resolvedAt pgtype.Timestamptz
		// The compare-and-swap is a second line of defence behind the lock
		// (and behind 00103's guard, which refuses the same write at the
		// trigger): a close that found the row already decided writes
		// nothing at all.
		if err := tx.QueryRow(ctx,
			`UPDATE credit_disputes
			    SET state = $1, resolution = $2, resolved_at = now()
			  WHERE id = $3 AND project_id = $4 AND state = 'open'
			  RETURNING resolved_at`,
			string(req.Outcome), req.Resolution, disputeID, projectID).Scan(&resolvedAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return credit.ErrDisputeClosed
			}
			return err
		}

		payload, err := json.Marshal(disputeResolvedPayload{
			PayloadVersion: 1,
			DisputeID:      pgUUIDToText(disputeID),
			TargetRef:      stored.TargetRef,
			TargetKind:     string(stored.TargetKind),
			Outcome:        string(req.Outcome),
		})
		if err != nil {
			return fmt.Errorf("persistence: render dispute resolved payload: %w", err)
		}
		if err := events.Record(ctx, tx, events.Event{
			EventType:     eventCreditDisputeResolved,
			ActorID:       req.Actor.User.ID,
			ProjectID:     req.ProjectID,
			Visibility:    events.VisibilityPrivate,
			CorrelationID: correlationID,
			Payload:       payload,
		}); err != nil {
			return err
		}

		audit := req.Audit
		audit.CorrelationID = correlationID
		audit.BeforeSummary = map[string]any{"state": string(stored.State)}
		audit.AfterSummary = map[string]any{"state": string(req.Outcome)}
		audit.Metadata = map[string]any{
			"target_kind": string(stored.TargetKind),
			"opened_by":   stored.OpenedBy,
		}
		if err := appendAudit(ctx, q, audit); err != nil {
			return err
		}

		out = stored
		out.State = req.Outcome
		out.Resolution = req.Resolution
		out.ResolvedAt = &resolvedAt.Time
		return nil
	})
	if err != nil {
		return contribution.CreditDispute{}, mapCreditStoreError(err)
	}
	return out, nil
}

// CurrentAttribution implements credit.Reader: the target's greatest-ordinal
// declaration, or nil when it has never been declared.
func (s *CreditStore) CurrentAttribution(ctx context.Context, projectID, targetRef string) (*contribution.CreditAttribution, error) {
	rows, err := s.AttributionHistory(ctx, projectID, targetRef)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	current := rows[len(rows)-1]
	return &current, nil
}

// AttributionHistory implements credit.Reader: every declaration ever made
// about one target, oldest first. Nothing in the table is ever updated or
// deleted (00103's triggers), so this is the whole chain — which is what
// docs/13 §2's "旧 attribution ... 保留" buys, and what a correction is
// visible through.
func (s *CreditStore) AttributionHistory(ctx context.Context, projectID, targetRef string) ([]contribution.CreditAttribution, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, project_id, target_kind, target_ref, ordinal, recorded_by, recorded_at
		   FROM credit_attribution_statements
		  WHERE project_id = $1 AND target_ref = $2
		  ORDER BY ordinal`,
		projectID, targetRef)
	if err != nil {
		return nil, fmt.Errorf("persistence: read credit attribution history: %w", err)
	}
	defer rows.Close()

	out := []contribution.CreditAttribution{}
	ids := []pgtype.UUID{}
	for rows.Next() {
		var (
			id         pgtype.UUID
			projID     pgtype.UUID
			kind, ref  string
			ordinal    int32
			recordedBy pgtype.UUID
			recordedAt pgtype.Timestamptz
		)
		if err := rows.Scan(&id, &projID, &kind, &ref, &ordinal, &recordedBy, &recordedAt); err != nil {
			return nil, fmt.Errorf("persistence: read credit attribution history: %w", err)
		}
		ids = append(ids, id)
		out = append(out, contribution.CreditAttribution{
			ID:         pgUUIDToText(id),
			ProjectID:  pgUUIDToText(projID),
			TargetKind: contribution.CreditTargetKind(kind),
			TargetRef:  ref,
			Ordinal:    int(ordinal),
			RecordedBy: pgUUIDToText(recordedBy),
			RecordedAt: recordedAt.Time,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("persistence: read credit attribution history: %w", err)
	}
	for i := range out {
		parties, err := readAttributionParties(ctx, s.pool, ids[i])
		if err != nil {
			return nil, err
		}
		out[i].Parties = parties
	}
	return out, nil
}

// GetDispute implements credit.Reader.
func (s *CreditStore) GetDispute(ctx context.Context, projectID, disputeID string) (contribution.CreditDispute, error) {
	pid, err := textUUID(projectID)
	if err != nil {
		return contribution.CreditDispute{}, credit.ErrProjectNotFound
	}
	did, err := textUUID(disputeID)
	if err != nil {
		return contribution.CreditDispute{}, credit.ErrDisputeNotFound
	}
	row, err := readDisputeRow(ctx, s.pool, did, pid, false)
	if err != nil {
		return contribution.CreditDispute{}, mapCreditStoreError(err)
	}
	return row, nil
}

// ListDisputes implements credit.Reader: the project's disputes, newest
// first.
func (s *CreditStore) ListDisputes(ctx context.Context, projectID string) ([]contribution.CreditDispute, error) {
	return s.listDisputes(ctx, projectID, "")
}

// ListTargetDisputes implements credit.Reader: the disputes about one
// target, newest first.
func (s *CreditStore) ListTargetDisputes(ctx context.Context, projectID, targetRef string) ([]contribution.CreditDispute, error) {
	return s.listDisputes(ctx, projectID, targetRef)
}

func (s *CreditStore) listDisputes(ctx context.Context, projectID, targetRef string) ([]contribution.CreditDispute, error) {
	args := []any{projectID}
	filter := ""
	if targetRef != "" {
		filter = " AND target_ref = $2"
		args = append(args, targetRef)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, project_id, opened_by, target_ref, claim, state, resolution, opened_at, resolved_at
		   FROM credit_disputes
		  WHERE project_id = $1`+filter+`
		  ORDER BY opened_at DESC, id DESC`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("persistence: list credit disputes: %w", err)
	}
	defer rows.Close()

	out := []contribution.CreditDispute{}
	for rows.Next() {
		row, err := scanDispute(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("persistence: list credit disputes: %w", err)
	}
	return out, nil
}

// creditRowScanner is the one shape both a pgx.Row and a pgx.Rows
// satisfy, so readDisputeRow and listDisputes scan the same columns with
// the same code.
type creditRowScanner interface {
	Scan(dest ...any) error
}

// scanDispute reads the nine columns the credit_disputes queries select,
// in the order disputeColumns names them.
func scanDispute(row creditRowScanner) (contribution.CreditDispute, error) {
	var (
		id, projID, openedBy pgtype.UUID
		targetRef            string
		claim, state         string
		resolution           *string
		openedAt             pgtype.Timestamptz
		resolvedAt           pgtype.Timestamptz
	)
	if err := row.Scan(&id, &projID, &openedBy, &targetRef, &claim, &state, &resolution, &openedAt, &resolvedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return contribution.CreditDispute{}, credit.ErrDisputeNotFound
		}
		return contribution.CreditDispute{}, err
	}
	kind, _, _ := contribution.ParseCreditTargetRef(targetRef)
	out := contribution.CreditDispute{
		ID:         pgUUIDToText(id),
		ProjectID:  pgUUIDToText(projID),
		OpenedBy:   pgUUIDToText(openedBy),
		TargetKind: kind,
		TargetRef:  targetRef,
		Claim:      claim,
		State:      contribution.DisputeState(state),
		OpenedAt:   openedAt.Time,
	}
	if resolution != nil {
		out.Resolution = *resolution
	}
	if resolvedAt.Valid {
		t := resolvedAt.Time
		out.ResolvedAt = &t
	}
	return out, nil
}

// readDisputeRow reads one dispute of one project. withLock takes the row
// lock the close path needs; a missing row (or one of another project —
// answered identically, so the pair cannot be used as an oracle) is
// ErrDisputeNotFound.
func readDisputeRow(ctx context.Context, db interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, disputeID, projectID pgtype.UUID, withLock bool) (contribution.CreditDispute, error) {
	sql := `SELECT id, project_id, opened_by, target_ref, claim, state, resolution, opened_at, resolved_at
	          FROM credit_disputes
	         WHERE id = $1 AND project_id = $2`
	if withLock {
		sql += ` FOR UPDATE`
	}
	row, err := scanDispute(db.QueryRow(ctx, sql, disputeID, projectID))
	if err != nil {
		return contribution.CreditDispute{}, err
	}
	return row, nil
}

// readAttribution reads one statement of a target's chain by ordinal.
func readAttribution(ctx context.Context, tx pgx.Tx, projectID, targetRef string, ordinal int) (contribution.CreditAttribution, error) {
	var (
		id         pgtype.UUID
		projID     pgtype.UUID
		kind, ref  string
		ord        int32
		recordedBy pgtype.UUID
		recordedAt pgtype.Timestamptz
	)
	if err := tx.QueryRow(ctx,
		`SELECT id, project_id, target_kind, target_ref, ordinal, recorded_by, recorded_at
		   FROM credit_attribution_statements
		  WHERE project_id = $1 AND target_ref = $2 AND ordinal = $3`,
		projectID, targetRef, ordinal).Scan(&id, &projID, &kind, &ref, &ord, &recordedBy, &recordedAt); err != nil {
		return contribution.CreditAttribution{}, err
	}
	parties, err := readAttributionParties(ctx, tx, id)
	if err != nil {
		return contribution.CreditAttribution{}, err
	}
	return contribution.CreditAttribution{
		ID:         pgUUIDToText(id),
		ProjectID:  pgUUIDToText(projID),
		TargetKind: contribution.CreditTargetKind(kind),
		TargetRef:  ref,
		Ordinal:    int(ord),
		Parties:    parties,
		RecordedBy: pgUUIDToText(recordedBy),
		RecordedAt: recordedAt.Time,
	}, nil
}

// readAttributionParties reads one statement's party rows, in the order the
// declaring caller sent them (role, then position).
func readAttributionParties(ctx context.Context, db interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, statementID pgtype.UUID) ([]contribution.CreditParty, error) {
	rows, err := db.Query(ctx,
		`SELECT role, party_kind, party_id, "position"
		   FROM credit_attribution_parties
		  WHERE statement_id = $1
		  ORDER BY role, "position"`, statementID)
	if err != nil {
		return nil, fmt.Errorf("persistence: read credit declaration parties: %w", err)
	}
	defer rows.Close()

	out := []contribution.CreditParty{}
	for rows.Next() {
		var (
			role, kind string
			partyID    pgtype.UUID
			position   int32
		)
		if err := rows.Scan(&role, &kind, &partyID, &position); err != nil {
			return nil, fmt.Errorf("persistence: read credit declaration parties: %w", err)
		}
		out = append(out, contribution.CreditParty{
			Party:    contribution.PartyRef{Kind: kind, ID: pgUUIDToText(partyID)},
			Role:     contribution.CreditRole(role),
			Position: int(position),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("persistence: read credit declaration parties: %w", err)
	}
	return out, nil
}

// creditPartyIdentity resolves a party to the display facts the table its
// kind names holds for it. A party that resolves to nothing is
// ErrPartyNotFound: a stored party that names nobody would be a dangling
// reference in an append-only declaration.
func creditPartyIdentity(ctx context.Context, tx pgx.Tx, kind domain.PartyKind, id pgtype.UUID) (creditIdentity, error) {
	var handle, displayName string
	switch kind {
	case domain.PartyUser:
		if err := tx.QueryRow(ctx,
			`SELECT handle, display_name FROM users WHERE id = $1`, id).Scan(&handle, &displayName); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return creditIdentity{}, credit.ErrPartyNotFound
			}
			return creditIdentity{}, err
		}
	case domain.PartyOrganization:
		if err := tx.QueryRow(ctx,
			`SELECT slug, name FROM organizations WHERE id = $1`, id).Scan(&handle, &displayName); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return creditIdentity{}, credit.ErrPartyNotFound
			}
			return creditIdentity{}, err
		}
	default:
		// The command refuses every kind outside the two identity kinds;
		// this is the store's own backstop, so a future caller cannot
		// write a party whose kind has no table.
		return creditIdentity{}, fmt.Errorf("%w: party kind %q names no identity table", credit.ErrValidation, kind)
	}
	return creditIdentity{Handle: handle, DisplayName: displayName}, nil
}

// creditIdentity is a resolved party's display facts.
type creditIdentity struct {
	Handle      string
	DisplayName string
}

// resolveCreditTarget checks that a target ref names a real row of the
// table its kind names AND that the row belongs to the project the caller's
// authorization was resolved against. A row of another project is refused
// as not-found, the same outcome as a ref that names nothing: a caller
// authorized in project A must not be able to write credit for a target of
// project B by naming A, and must not be able to learn from this command
// where else the ref points.
//
// withLock takes a row lock on the target, which is what serializes the
// per-target declaration chain's ordinal.
//
// The three kinds are docs/13 §2's own, and each is resolved against the
// table its kind names — an asset by its pid (the identity a citation
// resolves), a release and a finding by their row ids.
func resolveCreditTarget(ctx context.Context, tx pgx.Tx, kind contribution.CreditTargetKind, value string, projectID pgtype.UUID, withLock bool) error {
	suffix := ""
	if withLock {
		suffix = " FOR UPDATE"
	}
	var owner pgtype.UUID
	var err error
	switch kind {
	case contribution.CreditTargetAsset:
		err = tx.QueryRow(ctx,
			`SELECT origin_project_id FROM research_assets WHERE pid = $1`+suffix, value).Scan(&owner)
	case contribution.CreditTargetRelease:
		id, convErr := textUUID(value)
		if convErr != nil {
			return fmt.Errorf("%w: release ref value %q is not a uuid", credit.ErrValidation, value)
		}
		err = tx.QueryRow(ctx,
			`SELECT project_id FROM releases WHERE id = $1`+suffix, id).Scan(&owner)
	case contribution.CreditTargetFinding:
		id, convErr := textUUID(value)
		if convErr != nil {
			return fmt.Errorf("%w: finding ref value %q is not a uuid", credit.ErrValidation, value)
		}
		err = tx.QueryRow(ctx,
			`SELECT so.project_id
			   FROM findings f JOIN scientific_objects so ON so.id = f.object_id
			  WHERE f.version_id = $1`+lockClauseForJoin(withLock, "f"), id).Scan(&owner)
	default:
		return fmt.Errorf("%w: target kind %q names no target table", credit.ErrValidation, kind)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return credit.ErrTargetNotFound
	}
	if err != nil {
		return err
	}
	if owner != projectID {
		return credit.ErrTargetNotFound
	}
	return nil
}

// lockClauseForJoin renders a FOR UPDATE clause that locks only the joined
// finding row; PostgreSQL refuses a bare FOR UPDATE over a join for the
// reason the clause exists — it would lock the scientific_objects row too.
func lockClauseForJoin(withLock bool, alias string) string {
	if !withLock {
		return ""
	}
	return " FOR UPDATE OF " + alias
}

// disputeOpenedPayload is the payload of credit.dispute_opened: identity
// and references only (the claim itself stays in the row — see the type
// doc), with the ledger evidence the claim points at.
type disputeOpenedPayload struct {
	PayloadVersion int      `json:"payload_version"`
	DisputeID      string   `json:"dispute_id"`
	TargetRef      string   `json:"target_ref"`
	TargetKind     string   `json:"target_kind"`
	EvidenceRefs   []string `json:"evidence_refs"`
}

// disputeResolvedPayload is the payload of credit.dispute_resolved: which
// dispute, about what, and how it was decided. The decision's reasoning
// stays in the row.
type disputeResolvedPayload struct {
	PayloadVersion int    `json:"payload_version"`
	DisputeID      string `json:"dispute_id"`
	TargetRef      string `json:"target_ref"`
	TargetKind     string `json:"target_kind"`
	Outcome        string `json:"outcome"`
}

// attributionSummary renders one declaration state for an audit summary:
// the ordinal and the parties under their roles. It is a summary of the
// REQUEST's content, not of anybody's standing — no count is written as a
// score and nothing here ranks a declarer (docs/13 §4, CLAUDE.md §9
// invariant 13).
func attributionSummary(a *contribution.CreditAttribution) any {
	if a == nil {
		return map[string]any{"declaration": "none"}
	}
	parties := map[string][]string{}
	for _, p := range a.Parties {
		parties[string(p.Role)] = append(parties[string(p.Role)], p.Party.Kind+":"+p.Party.ID)
	}
	return map[string]any{
		"ordinal": a.Ordinal,
		"parties": parties,
	}
}

// creditCorrelationID resolves one correlation id for the whole command:
// the request's when the observability middleware attached one, else a
// fresh id — the same fallback the publish and rights-holder stores use, so
// the audit row and the event always share one trace id.
func creditCorrelationID(ctx context.Context) (string, error) {
	if info, ok := domain.RequestInfoFrom(ctx); ok && info.CorrelationID != "" {
		return info.CorrelationID, nil
	}
	id, err := observability.NewCorrelationID()
	if err != nil {
		return "", fmt.Errorf("%w: credit correlation id: %v", credit.ErrStore, err)
	}
	return id.String(), nil
}

// mapCreditStoreError keeps the sentinels of the application package and
// turns everything else into ErrStore.
func mapCreditStoreError(err error) error {
	switch {
	case err == nil,
		errors.Is(err, credit.ErrValidation),
		errors.Is(err, credit.ErrProjectNotFound),
		errors.Is(err, credit.ErrTargetNotFound),
		errors.Is(err, credit.ErrPartyNotFound),
		errors.Is(err, credit.ErrDisputeNotFound),
		errors.Is(err, credit.ErrDisputeClosed),
		errors.Is(err, credit.ErrStore):
		return err
	}
	if strings.Contains(err.Error(), "credit_disputes:") {
		// A refusal raised by migration 00103's state guard. It is a
		// constraint the request ran into, not a store failure: the guard's
		// message names the rule (identity/claim immutable, a closed
		// dispute terminal, a close that states its resolution), and the
		// outcome is the same — the write did not happen.
		return fmt.Errorf("%w: %v", credit.ErrDisputeClosed, err)
	}
	return fmt.Errorf("%w: %v", credit.ErrStore, err)
}

// Compile-time proof the adapter satisfies both halves of the credit
// surface, so a signature change in the application package is a build
// failure here rather than a wiring surprise.
var (
	_ credit.StorePort = (*CreditStore)(nil)
	_ credit.Reader    = (*CreditStore)(nil)
)
