package persistence

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/attestations"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// AttestationStore is the production adapter for the attestation command
// (T0812): the read that resolves what the decision is about, the write
// transaction that re-runs that decision over its own view, and the public
// read (GET /api/v1/attestations/{attestationId}).
//
// # The gate runs INSIDE the write transaction
//
// docs/22 §7 requires a command to re-run its own gate server-side rather
// than trust a precheck. Attest therefore resolves the attesting project,
// the organization's standing setting, the target version, the basis state
// and the internal review, re-runs attestations.Judge over them, and only
// then inserts — in ONE transaction, so what is decided and what is written
// come from the same snapshot.
//
// # Why there is no SELECT ... FOR UPDATE here, unlike the other stores
//
// The knowledge publication store takes the project row lock because two
// publishes of one project genuinely race: a version is published at most
// once, and the loser of that race must be answered rather than half-written.
// An attestation has no such rule (migration 00120: two attestations with
// the same shape are legitimate, and nothing sums them), so the question is
// narrower: can any input Judge reads change between the resolution and the
// insert, in a way that would make a written row wrong?
//
// The answer is no, and it is a property of the schema rather than of this
// file:
//
//   - the TARGET's two visibility axes are immutable. scientific_object_
//     versions.visibility_policy_id is written once, by
//     CreateScientificObjectVersion, and the table is append-only (00014);
//     research_asset_versions.visibility is written once by the asset
//     publish, and that table is append-only too. projects.visibility has
//     no UPDATE path at all — projects.sql writes it in the INSERT and
//     never again.
//   - the BASIS state and the REVIEW are append-only rows (project_states,
//     00014). A review is INSERT-only: no query in this repository updates
//     or deletes one.
//   - the ORGANIZATION's standing setting is the one mutable input, and it
//     is read inside the transaction (so the write decides on the value it
//     committed against), and it only ever narrows what a READ shows
//     (attestations.Present). A flip after the write cannot make a written
//     row disclose more than it promised.
//
// So a lock would serialize writers without protecting an invariant, and a
// lock with no rule behind it reads as safety while being ceremony. If a
// future migration gives any of those columns an UPDATE path, this note is
// the thing that has to be revisited.
//
// # The wall between the two reads
//
// ResolveFacts reads the private side (which project attests, which of its
// states the statement rests on, which review authorised it); that is what
// the decision is made on. GetPublicAttestation reads a DIFFERENT query
// (ResolvePublicAttestation) whose result has no column for any of the
// three, so the public projection cannot disclose what it was never handed.
type AttestationStore struct {
	pool *pgxpool.Pool
}

// NewAttestationStore builds the store on pool. The pool may be lazy
// (OpenLazy): the API keeps starting while PostgreSQL is down.
func NewAttestationStore(pool *pgxpool.Pool) *AttestationStore {
	return &AttestationStore{pool: pool}
}

// ResolveFacts implements attestations.StorePort: everything the
// attestation decision reads, over the pool (the preview path) or over a
// transaction (the write path, via resolveAttestationFacts).
func (s *AttestationStore) ResolveFacts(ctx context.Context, in attestations.ResolveRequest) (attestations.Facts, error) {
	return resolveAttestationFacts(ctx, sqlc.New(s.pool), in)
}

// resolveAttestationFacts is ResolveFacts over a caller-supplied querier, so
// the write transaction resolves over ITS OWN view rather than through a
// second connection that would see a different snapshot.
//
// # The four not-founds, and why two of them are collapsed early
//
// A request names four things that must exist: the attesting project, the
// target version, the basis state and the internal review. A missing project
// or target is ErrProjectNotFound / ErrTargetNotFound.
//
// The target is resolved for the REQUEST'S READER (ResolveRequest.ReaderID),
// so "the target does not exist" and "the target is not one this reader may
// read" are the same ErrTargetNotFound — deliberately, and for the reason
// the basis state case below records: this surface is reachable by any
// project owner, and an answer that differed would be an existence oracle
// over every private version in the database.
//
// The basis state and the review are resolved WITHOUT a project filter and
// then checked here, because the two failures are not the same size:
//
//   - a state or review of ANOTHER project answers ErrBasisNotFound /
//     ErrReviewNotFound, the same answer a nonexistent id gets. Telling the
//     caller "that exists, but not for you" would be an existence oracle
//     over every other project's private rows (docs/45), and this API is
//     reachable by any project owner.
//   - a state or review of the ATTESTING project that is simply the wrong
//     one is left to Judge, which reports it as a named reason. The caller
//     is a member of that project and can already read its own rows, so
//     there is nothing to withhold; and "this review judged a different
//     state" is a mistake worth one sentence rather than a 404.
func resolveAttestationFacts(ctx context.Context, q *sqlc.Queries, in attestations.ResolveRequest) (attestations.Facts, error) {
	projectUUID, err := textUUID(in.ProjectID)
	if err != nil {
		return attestations.Facts{}, attestations.ErrProjectNotFound
	}
	attester, err := q.ResolveAttestationAttester(ctx, projectUUID)
	if errors.Is(err, pgx.ErrNoRows) {
		return attestations.Facts{}, attestations.ErrProjectNotFound
	}
	if err != nil {
		return attestations.Facts{}, fmt.Errorf("%w: resolve attesting project: %v", attestations.ErrStore, err)
	}
	facts := attestations.Facts{
		ProjectID:      pgUUIDToText(attester.ProjectID),
		OrganizationID: pgUUIDToText(attester.OrganizationID),
	}
	if attester.OrganizationAttestationAttribution != nil {
		facts.OrganizationSetting = *attester.OrganizationAttestationAttribution
	}

	target, err := resolveAttestationTarget(ctx, q, in)
	if err != nil {
		return attestations.Facts{}, err
	}
	facts.Target = target

	stateUUID, err := textUUID(in.BasisStateID)
	if err != nil {
		return attestations.Facts{}, attestations.ErrBasisNotFound
	}
	state, err := q.ResolveAttestationBasisState(ctx, stateUUID)
	if errors.Is(err, pgx.ErrNoRows) {
		return attestations.Facts{}, attestations.ErrBasisNotFound
	}
	if err != nil {
		return attestations.Facts{}, fmt.Errorf("%w: resolve basis state: %v", attestations.ErrStore, err)
	}
	if pgUUIDToText(state.ProjectID) != facts.ProjectID {
		return attestations.Facts{}, attestations.ErrBasisNotFound
	}
	facts.Basis = attestations.BasisFacts{
		StateID:   pgUUIDToText(state.StateID),
		ProjectID: pgUUIDToText(state.ProjectID),
		StateHash: state.StateHash,
	}

	reviewUUID, err := textUUID(in.InternalReviewID)
	if err != nil {
		return attestations.Facts{}, attestations.ErrReviewNotFound
	}
	review, err := q.ResolveAttestationInternalReview(ctx, reviewUUID)
	if errors.Is(err, pgx.ErrNoRows) {
		return attestations.Facts{}, attestations.ErrReviewNotFound
	}
	if err != nil {
		return attestations.Facts{}, fmt.Errorf("%w: resolve internal review: %v", attestations.ErrStore, err)
	}
	if pgUUIDToText(review.PullRequestProjectID) != facts.ProjectID {
		return attestations.Facts{}, attestations.ErrReviewNotFound
	}
	facts.Review = attestations.ReviewFacts{
		ReviewID:             pgUUIDToText(review.ReviewID),
		Kind:                 review.ReviewKind,
		Decision:             review.Decision,
		ReviewedStateID:      pgUUIDToText(review.ReviewedStateID),
		PullRequestProjectID: pgUUIDToText(review.PullRequestProjectID),
	}
	return facts, nil
}

// resolveAttestationTarget resolves the one target pin the request carries.
//
// The KIND is derived here and never stored: an object target's kind is its
// object's object_type ('protocol' or 'claim'; anything else is reported by
// Judge as not attestable), and an asset target's is TargetKindAsset.
//
// # The read is reader-relative, and that is the whole of this function's privacy
//
// Both queries carry a reader predicate (see queries/attestations.sql), so a
// target the reader may not read resolves to pgx.ErrNoRows — the same answer
// an id that names nothing gets. The collapse is deliberate and it is the
// reason the facts below can be handed to BuildPreview at all: every fact
// here (the title, the object id, the version id, the owning project's
// visibility) is somebody else's private data when the row is private, and
// the refusal path DOES hand the target facts back to the caller in the 409
// body. What makes that safe is not a rendering rule but this: the facts are
// only ever built for a version the caller could have read anyway, so the
// refusal path has no unreadable target to disclose and there is nothing
// left for "does this version exist" to be asked through.
//
// The reader is the actor. An id that does not parse, or an empty one, is
// sent as SQL NULL — which makes the membership arm NULL rather than true
// and leaves exactly the public rows readable: fail closed, the direction
// events_audit.sql and evidence.sql take with their own readers.
func resolveAttestationTarget(ctx context.Context, q *sqlc.Queries, in attestations.ResolveRequest) (attestations.TargetFacts, error) {
	reader := mustUUID(in.ReaderID)
	switch {
	case in.TargetObjectVersionID != "" && in.TargetAssetVersionID == "":
		versionUUID, err := textUUID(in.TargetObjectVersionID)
		if err != nil {
			return attestations.TargetFacts{}, attestations.ErrTargetNotFound
		}
		row, err := q.ResolveAttestationTargetObject(ctx, sqlc.ResolveAttestationTargetObjectParams{
			ObjectVersionID: versionUUID,
			ReaderUserID:    reader,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return attestations.TargetFacts{}, attestations.ErrTargetNotFound
		}
		if err != nil {
			return attestations.TargetFacts{}, fmt.Errorf("%w: resolve attestation target object: %v", attestations.ErrStore, err)
		}
		return attestations.TargetFacts{
			Kind:                    attestations.TargetKind(row.ObjectType),
			VersionID:               pgUUIDToText(row.VersionID),
			ObjectID:                pgUUIDToText(row.ObjectID),
			Title:                   row.Title,
			LifecycleState:          row.LifecycleState,
			VisibilityPolicyID:      uuidTextPtr(row.VisibilityPolicyID),
			OwningProjectVisibility: row.OwningProjectVisibility,
		}, nil
	case in.TargetAssetVersionID != "" && in.TargetObjectVersionID == "":
		versionUUID, err := textUUID(in.TargetAssetVersionID)
		if err != nil {
			return attestations.TargetFacts{}, attestations.ErrTargetNotFound
		}
		row, err := q.ResolveAttestationTargetAsset(ctx, sqlc.ResolveAttestationTargetAssetParams{
			AssetVersionID: versionUUID,
			ReaderUserID:   reader,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return attestations.TargetFacts{}, attestations.ErrTargetNotFound
		}
		if err != nil {
			return attestations.TargetFacts{}, fmt.Errorf("%w: resolve attestation target asset: %v", attestations.ErrStore, err)
		}
		return attestations.TargetFacts{
			Kind:      attestations.TargetKindAsset,
			VersionID: pgUUIDToText(row.VersionID),
			ObjectID:  pgUUIDToText(row.AssetID),
			Title:     row.Title,
			// Both axes, because Judge decides over both: the version's own
			// visibility, and the visibility of the project that ORIGINATED
			// the asset (research_assets.origin_project_id). Handing Judge
			// one of the two is how a public-flagged version inside a
			// private project would be read as a public target.
			AssetVisibility:         row.Visibility,
			OwningProjectVisibility: row.OwningProjectVisibility,
		}, nil
	default:
		// The command refused both-nil and both-set before any read; a
		// request that still carries neither names no target.
		return attestations.TargetFacts{}, attestations.ErrTargetNotFound
	}
}

// Attest implements attestations.StorePort: the write.
//
// The sequence is the whole point: resolve over THIS transaction's view,
// re-run Judge over what was resolved, and only then insert the attestation
// row and append the audit row — in one transaction. A refusal writes
// nothing at all: no attestation row, no audit row, no trace that the caller
// tried (docs/45).
func (s *AttestationStore) Attest(ctx context.Context, req attestations.AttestRequest) (attestations.Attested, error) {
	createdBy, err := textUUID(req.Actor.User.ID)
	if err != nil {
		return attestations.Attested{}, fmt.Errorf("%w: attesting actor id: %v", attestations.ErrStore, err)
	}
	want := attestations.Want{
		ValidationType:   req.ValidationType,
		ValidationResult: req.ValidationResult,
		OrgVisibility:    req.OrgVisibility,
	}
	var out attestations.Attested
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)

		// The facts over THIS transaction's view, and the decision over
		// them. This is the re-run: the preview the caller saw decided over
		// the state at that moment, and this decides over the state the
		// write actually commits against.
		facts, err := resolveAttestationFacts(ctx, q, req.Resolve)
		if err != nil {
			return err
		}
		if reasons := attestations.Judge(facts, want.Named()); len(reasons) > 0 {
			return &attestations.Refused{
				Preview: attestations.BuildPreview(facts, want),
				Reasons: attestationRefusalReasonLines(reasons),
			}
		}

		row, err := q.CreateAttestation(ctx, sqlc.CreateAttestationParams{
			Pid:                     req.PID,
			TargetObjectVersionID:   uuidOrNull(req.Resolve.TargetObjectVersionID),
			TargetAssetVersionID:    uuidOrNull(req.Resolve.TargetAssetVersionID),
			AttestingProjectID:      mustUUID(facts.ProjectID),
			AttestingOrganizationID: uuidOrNull(facts.OrganizationID),
			BasisStateID:            mustUUID(facts.Basis.StateID),
			InternalReviewID:        mustUUID(facts.Review.ReviewID),
			ValidationType:          req.ValidationType,
			ValidationResult:        req.ValidationResult,
			OrgVisibility:           req.OrgVisibility,
			CreatedBy:               createdBy,
		})
		if err != nil {
			// The pid index is the only uniqueness this table has, so a
			// 23505 here is the generator repeating itself — a defect, not
			// a caller's error. Mapped rather than wrapped for the same
			// reason knowledgepublish maps its pid collision: what the
			// caller can act on is "this was already issued".
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
				constraintNames(pgErr, "attestations_pid_uniq") {
				return fmt.Errorf("%w: the pid generator produced a pid that is already in use", attestations.ErrStore)
			}
			return err
		}
		out = attestations.Attested{
			PID:              row.Pid,
			ValidationType:   row.ValidationType,
			ValidationResult: row.ValidationResult,
			OrgVisibility:    row.OrgVisibility,
			CreatedAt:        row.CreatedAt.Time,
		}

		// The audit row names the pid the insert assigned; the command left
		// the target ref empty. Actor, via and correlation id are filled
		// from the request context by appendAudit, the same way every other
		// governance write's audit row gets them.
		audit := req.Audit
		audit.TargetRef = "attestation:" + out.PID
		if err := appendAudit(ctx, q, audit); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return attestations.Attested{}, err
	}
	return out, nil
}

// GetPublicAttestation implements both attestations.StorePort and
// attestations.ReadPort: what a public reader is shown of one attestation.
//
// It returns the value ALREADY PROJECTED by attestations.Present, because
// the projection is part of the read rather than a step after it: the query
// behind it does not select the attesting project, the basis state or the
// internal review, so there is no private column here for a caller to forget
// to drop. The org's standing setting is read by the same query and at the
// same instant as the promise — the two attribution gates are applied to one
// snapshot rather than to two reads taken at different times.
//
// A pid that names nothing and a string that is not a pid at all are both
// "false, nil": neither can resolve, and a caller must not be able to tell
// them apart (docs/45).
func (s *AttestationStore) GetPublicAttestation(ctx context.Context, pid string) (attestations.PublicAttestation, bool, error) {
	if !attestations.ValidPID(pid) {
		return attestations.PublicAttestation{}, false, nil
	}
	row, err := sqlc.New(s.pool).ResolvePublicAttestation(ctx, pid)
	if errors.Is(err, pgx.ErrNoRows) {
		return attestations.PublicAttestation{}, false, nil
	}
	if err != nil {
		return attestations.PublicAttestation{}, false, fmt.Errorf("%w: resolve public attestation: %v", attestations.ErrStore, err)
	}
	return attestations.Present(publicAttestationRow(row)), true, nil
}

// publicAttestationRow renders the public read into the projection's input.
//
// The target's kind is derived from which pin the row carries, exactly as
// the resolution read derives it — and for the same reason it is not stored:
// a stored kind could disagree with the row it points at.
func publicAttestationRow(row sqlc.ResolvePublicAttestationRow) attestations.Row {
	out := attestations.Row{
		PID:              row.Pid,
		ValidationType:   row.ValidationType,
		ValidationResult: row.ValidationResult,
		OrgVisibility:    row.OrgVisibility,
		CreatedAt:        row.CreatedAt.Time,
	}
	switch {
	case row.TargetObjectVersionID.Valid:
		target := attestations.RowTarget{
			VersionID: pgUUIDToText(row.TargetObjectVersionID),
			ObjectID:  pgUUIDToText(row.ObjectID),
		}
		if row.ObjectType != nil {
			target.Kind = attestations.TargetKind(*row.ObjectType)
		}
		if row.ObjectVersionTitle != nil {
			target.Title = *row.ObjectVersionTitle
		}
		out.Target = target
	case row.TargetAssetVersionID.Valid:
		target := attestations.RowTarget{
			Kind:      attestations.TargetKindAsset,
			VersionID: pgUUIDToText(row.TargetAssetVersionID),
			ObjectID:  pgUUIDToText(row.AssetID),
		}
		if row.AssetTitle != nil {
			target.Title = *row.AssetTitle
		}
		out.Target = target
	}
	if row.AttestingOrganizationID.Valid {
		org := &attestations.RowOrganization{ID: pgUUIDToText(row.AttestingOrganizationID)}
		if row.OrganizationSlug != nil {
			org.Slug = *row.OrganizationSlug
		}
		if row.OrganizationName != nil {
			org.Name = *row.OrganizationName
		}
		if row.OrganizationAttestationAttribution != nil {
			org.Setting = *row.OrganizationAttestationAttribution
		}
		out.Organization = org
	}
	return out
}

// attestationRefusalReasonLines renders a refusal report's entries for
// *Refused. It carries the CODE beside the sentence, unlike the package's
// own preview-of-a-refusal helper, because the store's refusal is the one a
// caller reads out of a failed publish and a client branches on the code.
func attestationRefusalReasonLines(reasons []attestations.Reason) []string {
	out := make([]string, 0, len(reasons))
	for _, r := range reasons {
		out = append(out, r.Code+": "+r.Detail)
	}
	return out
}

// uuidOrNull renders a text uuid as a nullable parameter: empty is SQL NULL,
// which is what the two optional pins and the optional organization need.
func uuidOrNull(s string) pgtype.UUID {
	if s == "" {
		return pgtype.UUID{}
	}
	return mustUUID(s)
}
