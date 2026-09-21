package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/assetpublish"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/persistence/sqlc"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// AssetDeriveStore is the production adapter for the research asset
// fork/derive command (T0708): the Idempotency-Key ledger read the command
// checks before doing any work, and the derivation transaction itself.
//
// # What the transaction is, and why every decision is made inside it
//
// A derivation is a governed write over the same two tables a publish
// writes, plus the lineage edge that is its whole content (docs/11 §5), so
// it runs the publish's transaction shape — and adds to it the two decisions
// that are about the OTHER version, the parent:
//
//	lock the project (the one the new asset belongs to)
//	  →  re-check the ledger (replay is a read)
//	  →  resolve the parent version by pid@version
//	  →  apply the READ GATE (may this caller read that version at all?)
//	  →  apply the RIGHTS VERDICT over the parent's stored declaration
//	  →  resolve the current state, re-run Gate and Preview over it
//	  →  refuse (writing nothing), or
//	     insert the NEW asset row (its own pid)
//	     insert the version row
//	     insert the version's declared credits and usages
//	     insert the LINEAGE EDGE (parent version id → child version id)
//	     insert the ledger row
//	     append the audit row (with the rights verdict and the confirmation)
//	     record the research event and its outbox row
//
// The read gate and the rights verdict are decided HERE rather than in the
// command, and that is the one place this store differs in shape from
// AssetPublishStore. The parent's declaration cannot change (its version row
// is append-only, so the bytes read here are the bytes every later reader
// sees) — but the parent's PROJECT's visibility is a mutable column, and
// membership changes too, so a "may this caller read the parent" answered
// outside the transaction would be an answer about a state that can move
// between the check and the write. One definition, run where it commits.
//
// # A refusal writes nothing
//
// Every refusal fires before or inside the transaction and unwinds it: the
// parent resolution, the read gate, the rights verdict, the preview, the
// gate and the ladder check all run before the first INSERT, and a refusal
// AFTER an insert (a minted pid that turns out to be taken) rolls the
// transaction back. A refused derivation therefore leaves no asset, no
// version, no edge, no ledger row and — deliberately — no audit row: the
// audit log records changes that happened, and a refusal changed nothing.
//
// # The parent is READ, never written
//
// research_asset_versions carries the 00014 append-only row trigger. This
// store does not rely on it to keep the parent immutable: nothing here
// updates or deletes any version row, and the parent is touched by exactly
// one statement, the SELECT that resolves it.
type AssetDeriveStore struct {
	pool *pgxpool.Pool
	val  *rsgvalidation.Validator
	// members answers "is this caller a member of that project", for the
	// read gate over the parent. It is the SAME adapter the command's
	// authorization uses (*persistence.ProjectStore in production): there is
	// one implementation of that question in this build, and asking it from
	// inside the transaction is what makes the gate a fact about the state
	// being written against.
	members assets.MembershipPort
}

// NewAssetDeriveStore builds the store on pool, with the ladder's gate
// engine for the asset-document check and the membership read the parent
// gate needs. The pool may be lazy (OpenLazy): the API keeps starting while
// PostgreSQL is down.
func NewAssetDeriveStore(pool *pgxpool.Pool, val *rsgvalidation.Validator, members assets.MembershipPort) *AssetDeriveStore {
	return &AssetDeriveStore{pool: pool, val: val, members: members}
}

// LookupDerivation implements assets.DeriveStorePort: the derivation an
// Idempotency-Key already created, or nil when the key has no ledger entry
// yet. The command checks it before resolving anything, so a replay never
// re-runs a gate (a replay is a read).
//
// A project id that is not a uuid text cannot have derived anything: the
// key's scope is a project row, and answering "no entry" for one that cannot
// exist is the truth rather than an error. The authorization has already run
// by the time this is called, so this cannot be used to probe.
func (s *AssetDeriveStore) LookupDerivation(ctx context.Context, projectID, idempotencyKey string) (*assets.DerivedAsset, error) {
	pID, err := textUUID(projectID)
	if err != nil {
		return nil, nil
	}
	row, err := sqlc.New(s.pool).GetDerivedAsset(ctx, sqlc.GetDerivedAssetParams{
		ProjectID:      pID,
		IdempotencyKey: idempotencyKey,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("persistence: read asset derive creation: %w", err)
	}
	out := derivedAssetFromRow(row)
	return &out, nil
}

// Derive implements assets.DeriveStorePort. See the type doc for the
// transaction it runs; this method is the transaction.
func (s *AssetDeriveStore) Derive(ctx context.Context, req assets.DeriveRequest) (assets.DerivedAsset, error) {
	projectID, err := textUUID(req.ProjectID)
	if err != nil {
		return assets.DerivedAsset{}, assets.ErrDeriveProjectNotFound
	}
	actorID, err := textUUID(req.Actor.User.ID)
	if err != nil {
		return assets.DerivedAsset{}, fmt.Errorf("%w: deriving actor id: %v", assets.ErrDeriveStore, err)
	}
	parentPID, parentVersion, ok := assets.ParseParentVersionRef(string(req.Parent))
	if !ok {
		// Unreachable: the command validates the reference's shape before it
		// calls the store. Kept as the honest answer rather than a lookup
		// that would report a malformed reference as an absent version.
		return assets.DerivedAsset{}, fmt.Errorf("%w: parent %s is not a canonical pid@version", assets.ErrDeriveValidation, quoteText(string(req.Parent)))
	}
	// One correlation id for the whole derivation: the request's when the
	// observability middleware attached one, else a fresh one — the same
	// fallback the publish store uses, so the audit row, the research event
	// and the outbox row share one trace id.
	correlationID := ""
	if info, ok := domain.RequestInfoFrom(ctx); ok {
		correlationID = info.CorrelationID
	}
	if correlationID == "" {
		id, err := observability.NewCorrelationID()
		if err != nil {
			return assets.DerivedAsset{}, fmt.Errorf("%w: derive correlation id: %v", assets.ErrDeriveStore, err)
		}
		correlationID = id.String()
	}

	var out assets.DerivedAsset
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		project, err := q.GetProjectByIDForUpdate(ctx, projectID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return assets.ErrDeriveProjectNotFound
			}
			return err
		}

		// The ledger, re-checked under the project lock. The command's
		// LookupDerivation ran outside any transaction, so it is an
		// optimization; this read is the guarantee, because the lock
		// serializes every derivation into this project. Two concurrent
		// requests carrying one key therefore produce one derivation: the
		// second waits on the lock, finds the first's ledger row here, and
		// replays it.
		if req.IdempotencyKey != nil {
			row, err := q.GetDerivedAsset(ctx, sqlc.GetDerivedAssetParams{
				ProjectID:      projectID,
				IdempotencyKey: *req.IdempotencyKey,
			})
			switch {
			case err == nil:
				out = derivedAssetFromRow(row)
				return nil
			case errors.Is(err, pgx.ErrNoRows):
				// first derivation with this key — fall through to the work
			default:
				return err
			}
		}

		// The parent, resolved to the VERSION ROW id every later statement
		// is keyed by. The pid@version pair names at most one row
		// (UNIQUE(asset_id, version), 00010), and a pair that names none is
		// the same answer the read gate below gives for one this caller may
		// not open.
		parent, err := q.GetDeriveParentVersion(ctx, sqlc.GetDeriveParentVersionParams{
			Pid:     string(parentPID),
			Version: parentVersion,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return assets.ErrDeriveParentNotFound
			}
			return err
		}

		// The read gate: the caller may derive from a version it could open
		// on the asset page, and from no other. "Readable ⇒ derivable" is
		// the rule; "id guessed ⇒ derivable" is what ADR-024 forbids, and
		// the refusal is the SAME error as a version that does not exist, so
		// this path cannot be used to enumerate versions.
		member, err := s.callerMayReadParent(ctx, parent, req.Actor.User.ID)
		if err != nil {
			return err
		}
		if !assets.ParentReadable(parent.OriginProjectID, assets.Visibility(parent.ProjectVisibility), assets.Visibility(parent.Visibility), member) {
			return assets.ErrDeriveParentNotFound
		}

		// The rights verdict, over the parent's STORED declaration — the
		// bytes in the row, read here and never a summary of them. Its
		// answer, and the confirmation it was decided on, are recorded in
		// the audit row below.
		verdict, err := assets.RequireDerivable(parent.RightsJson, req.Confirmation)
		if err != nil {
			return err
		}

		// The state, the gate and the preview, over THIS transaction's view
		// of the repository — the same three the publish runs, over the
		// child candidate. A derivation writes a published version, so every
		// rule a publication is held to holds here (docs/11 §3's checklist,
		// docs/23 §4's no-hidden-private-dependency re-check).
		state, err := NewAssetStateStore(tx).ResolvePreviewState(ctx, req.Candidate)
		if err != nil {
			return err
		}
		if state.Asset != nil {
			// The pid was minted a moment ago and already names an asset: a
			// 32^26 collision, or a generator that repeated itself. Either
			// way the create below must not overwrite an identity.
			return assets.ErrDeriveAssetExists
		}
		preview, err := assets.Preview(assets.PreviewRequest{ProjectID: req.ProjectID, Candidate: req.Candidate}, state)
		if err != nil {
			return err
		}
		if reasons := refusalReasons(preview, true); len(reasons) > 0 {
			return &assets.DeriveRefused{Preview: preview, Reasons: reasons}
		}
		gate, err := assets.Gate(req.Candidate)
		if err != nil {
			// Unreachable: every refusal Gate raises is also a preview
			// blocker, and the blockers above refused first. Kept because
			// "unreachable" is a claim about two components agreeing, and a
			// claim that is checked costs one call.
			return &assets.DeriveRefused{
				Preview: preview,
				Reasons: []string{"the publish gate refused the derived candidate: " + err.Error()},
			}
		}
		rightsJSON, err := rightsBytes(gate)
		if err != nil {
			return err
		}
		reasons, err := requirePublishedAssetDocument(s.val, req.ProjectID, req.Candidate, gate, rightsJSON)
		if err != nil {
			return err
		}
		if len(reasons) > 0 {
			return &assets.DeriveRefused{Preview: preview, Reasons: reasons}
		}

		// The new identity. CreateResearchAssetWithPID is the publish's own
		// create (one INSERT of one asset row, pid supplied by the
		// application): a derivation creates an asset exactly the way a
		// publish that creates one does, and reusing the statement is what
		// keeps "an asset row" one definition.
		assetRow, err := q.CreateResearchAssetWithPID(ctx, sqlc.CreateResearchAssetWithPIDParams{
			AssetType:       string(req.Candidate.AssetType),
			Slug:            req.Slug,
			Title:           req.Title,
			OriginProjectID: mustProjectUUID(req.ProjectID),
			Pid:             string(req.Candidate.AssetPID),
		})
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
				strings.Contains(pgErr.ConstraintName, "research_assets_pid") {
				return assets.ErrDeriveAssetExists
			}
			return err
		}
		version, err := q.PublishResearchAssetVersion(ctx, sqlc.PublishResearchAssetVersionParams{
			AssetID:         assetRow.ID,
			Version:         req.Candidate.Version,
			SourceReleaseID: sourceReleaseID(req.Candidate.OriginRefs),
			Manifest:        gate.ManifestJSON,
			RightsJson:      rightsJSON,
			Visibility:      string(req.Candidate.Visibility),
			IntegrityHash:   req.Candidate.IntegrityHash,
			PublishedBy:     actorID,
			OriginRefs:      req.Candidate.OriginRefs,
		})
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
				strings.Contains(pgErr.ConstraintName, "research_asset_versions_asset_id_version") {
				return assets.ErrDeriveVersionImmutable
			}
			return err
		}
		if err := insertVersionCredits(ctx, q, version.ID, actorID, req.Candidate.CreatorIDs); err != nil {
			return err
		}
		if err := recordUsageDeclarations(ctx, q, projectID, req.Usages, state.Pins); err != nil {
			return err
		}

		// The lineage edge: the whole point of the command, and the one row
		// no other path in this build writes. Both ends are version ROW ids
		// — the parent's is the one the resolution above read, the child's
		// is the one the insert just assigned — because that is what
		// asset_lineage is keyed by, and because an asset id or a slug would
		// leave "derived from WHAT" unanswerable the moment the parent
		// publishes another version (asset_derive.sql states the rule; this
		// is where it is obeyed).
		if _, err := q.CreateAssetLineage(ctx, sqlc.CreateAssetLineageParams{
			ParentAssetVersionID: parent.VersionID,
			ChildAssetVersionID:  version.ID,
			RelationType:         string(req.Relation),
		}); err != nil {
			return fmt.Errorf("persistence: record the lineage edge: %w", err)
		}

		out = assets.DerivedAsset{
			ID:             pgUUIDToText(version.ID),
			AssetID:        pgUUIDToText(version.AssetID),
			AssetPID:       assetRow.Pid,
			Version:        version.Version,
			Visibility:     assets.Visibility(version.Visibility),
			IntegrityHash:  version.IntegrityHash,
			OriginRefs:     version.OriginRefs,
			PublishedBy:    pgUUIDToText(version.PublishedBy),
			PublishedAt:    version.PublishedAt.Time,
			Manifest:       json.RawMessage(version.Manifest),
			RightsJSON:     json.RawMessage(version.RightsJson),
			ParentAssetPID: parent.AssetPid,
			ParentVersion:  parent.Version,
			Relation:       req.Relation,
		}

		if req.IdempotencyKey != nil {
			versionID, err := textUUID(out.ID)
			if err != nil {
				return err
			}
			if _, err := q.CreateAssetDeriveCreation(ctx, sqlc.CreateAssetDeriveCreationParams{
				ProjectID:      projectID,
				IdempotencyKey: *req.IdempotencyKey,
				AssetVersionID: versionID,
				// The parent's row id as the resolution returned it: a uuid
				// column, read back as one, and written to a uuid column.
				ParentAssetVersionID: parent.VersionID,
				RelationType:         string(req.Relation),
			}); err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
					strings.Contains(pgErr.ConstraintName, "asset_derive_creations_project_id_idempotency_key") {
					// Unreachable under the project lock (two derivations
					// with one key serialize, so the second reads the first's
					// ledger row above), and mapped rather than wrapped
					// because if it ever DID fire, what happened is exactly
					// what IDEMPOTENCY_CONFLICT names: the key is taken.
					return assets.ErrDeriveIdempotencyConflict
				}
				return fmt.Errorf("persistence: record the derive ledger entry: %w", err)
			}
		}

		// The audit row names the assigned version id AND the rights verdict
		// the transaction decided on: the command rendered the row from the
		// request, and this store fills the two things only it has — the row
		// id, and what the parent's declaration said together with the
		// confirmation it was read against. That second addition is the
		// record the task requires: for an `unspecified` parent, the row
		// says which actor derived, from which version, and on the strength
		// of which explicit confirmation.
		audit := req.Audit
		audit.TargetRef = "asset_version:" + out.ID
		audit.CorrelationID = correlationID
		summary, err := deriveAuditSummary(audit.AfterSummary, verdict)
		if err != nil {
			return err
		}
		audit.AfterSummary = summary
		if err := appendAudit(ctx, q, audit); err != nil {
			return err
		}
		// The child version is a published version, so the research event is
		// the publication event: research_asset.version_published is the
		// type that records "an asset version became immutable and readable"
		// (specs/events/event-types.yaml), and that is what happened. The
		// LINEAGE half of the act is recorded in the audit row's action and
		// summary, which is where the two acts differ; a second event type
		// would have to be declared in specs/events/**, which this task's
		// scope does not reach.
		return recordAssetVersionPublished(ctx, q, project.Visibility, actorID, projectID, correlationID,
			assetpublish.PublishedVersion{
				ID:            out.ID,
				AssetID:       out.AssetID,
				AssetPID:      out.AssetPID,
				Version:       out.Version,
				Visibility:    out.Visibility,
				IntegrityHash: out.IntegrityHash,
				OriginRefs:    out.OriginRefs,
			})
	})
	if err != nil {
		return assets.DerivedAsset{}, mapAssetDeriveError(err)
	}
	return out, nil
}

// callerMayReadParent answers the membership half of the parent read gate:
// whether the caller is a member of the parent version's originating
// project.
//
// The read is SKIPPED when both the project and the version are public,
// because the gate's rule admits everyone then and the answer cannot change
// it — but it is skipped only in that case: a private project or a private
// version means the answer decides, and "could not read the membership" is
// never "not a member" (that would turn an outage into a permission
// decision) and never "a member" either (that would be a hole). It is an
// error, and errors.ErrMemberNotFound and ErrProjectNotFound are both the
// answer "no": an unknown project is an unknown membership, exactly as the
// authorization reads them.
func (s *AssetDeriveStore) callerMayReadParent(ctx context.Context, parent sqlc.GetDeriveParentVersionRow, userID string) (bool, error) {
	publicProject := assets.Visibility(parent.ProjectVisibility) == assets.VisibilityPublic
	publicVersion := assets.Visibility(parent.Visibility) == assets.VisibilityPublic
	if publicProject && publicVersion {
		return false, nil
	}
	if s.members == nil {
		// A store wired without the membership read cannot answer the gate,
		// and "cannot check" is not "passed": the derivation is refused
		// rather than written unchecked.
		return false, fmt.Errorf("%w: the derive store was wired without the membership read", assets.ErrDeriveStore)
	}
	if _, err := s.members.GetMembership(ctx, parent.OriginProjectID, userID); err != nil {
		switch {
		case errors.Is(err, projects.ErrMemberNotFound), errors.Is(err, projects.ErrProjectNotFound):
			return false, nil
		default:
			return false, fmt.Errorf("%w: read the parent project membership: %v", assets.ErrDeriveStore, err)
		}
	}
	return true, nil
}

// deriveAuditSummary renders the audit row's after_summary for a derivation:
// the facts the command put there (which version, which parent, which
// relation) plus the three the transaction alone knows — the parent's
// declaration, the confirmation, and whether the confirmation was what made
// the derivation permitted.
//
// It REPLACES the summary rather than mutating the command's map, and it
// refuses to render at all when the command handed it something that is not
// one: a row that silently dropped the rights verdict would be an audit
// record of a different event, which is worse than no derivation.
func deriveAuditSummary(base any, verdict assets.DerivativesVerdict) (map[string]any, error) {
	carried, ok := base.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: the derive audit row carries no summary to record the rights verdict in", assets.ErrDeriveStore)
	}
	summary := make(map[string]any, len(carried)+3)
	for k, v := range carried {
		summary[k] = v
	}
	summary["derivatives_declared"] = string(verdict.Permission)
	summary[assets.DerivativesConfirmationField] = verdict.Confirmation
	summary["derivatives_confirmation_required"] = verdict.ConfirmationRequired
	return summary, nil
}

// derivedAssetFromRow converts one GetDerivedAsset row — the replay read and
// the ledger's own account of a derivation.
func derivedAssetFromRow(row sqlc.GetDerivedAssetRow) assets.DerivedAsset {
	return assets.DerivedAsset{
		ID:             pgUUIDToText(row.ID),
		AssetID:        pgUUIDToText(row.AssetID),
		AssetPID:       row.AssetPid,
		Version:        row.Version,
		Visibility:     assets.Visibility(row.Visibility),
		IntegrityHash:  row.IntegrityHash,
		OriginRefs:     row.OriginRefs,
		PublishedBy:    pgUUIDToText(row.PublishedBy),
		PublishedAt:    row.PublishedAt.Time,
		Manifest:       json.RawMessage(row.Manifest),
		RightsJSON:     json.RawMessage(row.RightsJson),
		ParentAssetPID: row.ParentAssetPid,
		ParentVersion:  row.ParentVersion,
		Relation:       assets.DeriveRelation(row.RelationType),
	}
}

// quoteText renders a value the way Go source would, so a refusal line shows
// the exact bytes it refused.
func quoteText(s string) string { return fmt.Sprintf("%q", s) }

// mapAssetDeriveError keeps the store's own sentinels (the command's
// contract) and wraps everything else with the persistence context.
func mapAssetDeriveError(err error) error {
	switch {
	case err == nil,
		errors.Is(err, assets.ErrDeriveValidation),
		errors.Is(err, assets.ErrDeriveProjectNotFound),
		errors.Is(err, assets.ErrDeriveParentNotFound),
		errors.Is(err, assets.ErrDeriveRightsRefused),
		errors.Is(err, assets.ErrDeriveAssetExists),
		errors.Is(err, assets.ErrDeriveVersionImmutable),
		errors.Is(err, assets.ErrDeriveIdempotencyConflict),
		errors.Is(err, assets.ErrDeriveRefused),
		errors.Is(err, assets.ErrDeriveStore):
		return err
	}
	return fmt.Errorf("persistence: asset derive write: %w", err)
}
