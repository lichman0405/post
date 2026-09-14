package resolutions

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/domain"
)

// PGStore is the production adapter over PostgreSQL for the migration
// 00054 conflict_resolutions table plus the evidence read (the
// evidence_assertions join). It lives in this package because the task's
// allowed_scope excludes internal/persistence (package doc) — same pool
// contract, same fail-closed error mapping as the persistence stores.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore builds the store on pool. The pool may be lazy (OpenLazy):
// the API keeps starting while PostgreSQL is down.
func NewPGStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{pool: pool}
}

const resolutionColumns = `id, project_id, base_state_id, source_state_id, target_state_id,
	target_kind, target_id, conflict_code, conflict_fields, conflict_payload_keys,
	conflict_other_object_id, resolution, note, decided_by, decided_at, updated_at`

// SavePlan upserts the decisions and appends the audit entry in one
// transaction, then returns the full updated plan. The audit row is part
// of the action (fail-closed, docs/53): a failed audit write fails the
// whole save, and the human who decided is named explicitly — the record
// that a human, not an agent, made the call (docs/60).
func (s *PGStore) SavePlan(ctx context.Context, in SavePlanParams) ([]domain.ConflictResolution, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolutions: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, d := range in.Decisions {
		if err := s.upsertOne(ctx, tx, in, d); err != nil {
			return nil, err
		}
	}
	if err := s.appendAudit(ctx, tx, in); err != nil {
		return nil, err
	}
	plan, err := listPlan(ctx, tx, in.ProjectID, in.BaseStateID, in.SourceStateID, in.TargetStateID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("resolutions: commit: %w", err)
	}
	return plan, nil
}

// upsertOne upserts one decision row by its full classifier key.
func (s *PGStore) upsertOne(ctx context.Context, tx pgx.Tx, in SavePlanParams, d domain.ConflictResolution) error {
	fields, err := json.Marshal(d.Fields)
	if err != nil {
		return fmt.Errorf("%w: fields: %v", ErrValidation, err)
	}
	payloadKeys, err := json.Marshal(d.PayloadKeys)
	if err != nil {
		return fmt.Errorf("%w: payload_keys: %v", ErrValidation, err)
	}
	var otherObjectID *string
	if d.OtherObjectID != nil && *d.OtherObjectID != "" {
		otherObjectID = d.OtherObjectID
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO conflict_resolutions
			(project_id, base_state_id, source_state_id, target_state_id,
			 target_kind, target_id, conflict_code, conflict_fields,
			 conflict_payload_keys, conflict_other_object_id, resolution,
			 note, decided_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (project_id, base_state_id, source_state_id, target_state_id,
		             target_kind, target_id, conflict_code, conflict_fields,
		             conflict_payload_keys, conflict_other_object_id)
		DO UPDATE SET resolution = EXCLUDED.resolution,
		              note = EXCLUDED.note,
		              decided_by = EXCLUDED.decided_by,
		              decided_at = now(),
		              updated_at = now()`,
		in.ProjectID, in.BaseStateID, in.SourceStateID, in.TargetStateID,
		string(d.TargetKind), d.TargetID, d.Code,
		string(fields), string(payloadKeys), otherObjectID,
		string(d.Kind), d.Note, d.DecidedBy,
	); err != nil {
		return fmt.Errorf("resolutions: upsert %s %s: %w", d.TargetKind, d.TargetID, err)
	}
	return nil
}

// appendAudit writes one audit_log row inside the open transaction: actor
// is the human who decided (explicit — the service knows it), via and
// correlation id come from the request context when present (the auth
// guard attaches them) and fall back to "internal". The after summary
// names every saved decision; the metadata carries the pinned triple.
func (s *PGStore) appendAudit(ctx context.Context, tx pgx.Tx, in SavePlanParams) error {
	actor := ""
	via := domain.ViaInternal
	correlation := ""
	if len(in.Decisions) > 0 {
		actor = in.Decisions[0].DecidedBy
	}
	if info, ok := domain.RequestInfoFrom(ctx); ok {
		if info.Via != "" {
			via = info.Via
		}
		correlation = info.CorrelationID
	}
	// audit_log.correlation_id is NOT NULL: a call outside the HTTP edge
	// (no observability middleware) still lands in the log, it just has no
	// request to name — the same fallback shape as the persistence writer's
	// via.
	if correlation == "" {
		correlation = domain.ViaInternal
	}
	type decisionSummary struct {
		TargetKind domain.ConflictResolutionTargetKind `json:"target_kind"`
		TargetID   string                              `json:"target_id"`
		Code       string                              `json:"code"`
		Kind       domain.ResolutionKind               `json:"kind"`
	}
	summaries := make([]decisionSummary, 0, len(in.Decisions))
	for _, d := range in.Decisions {
		summaries = append(summaries, decisionSummary{TargetKind: d.TargetKind, TargetID: d.TargetID, Code: d.Code, Kind: d.Kind})
	}
	after, err := json.Marshal(summaries)
	if err != nil {
		return fmt.Errorf("resolutions: audit summary: %v", err)
	}
	metadata, err := json.Marshal(map[string]string{
		"base_state_id":   in.BaseStateID,
		"source_state_id": in.SourceStateID,
		"target_state_id": in.TargetStateID,
	})
	if err != nil {
		return fmt.Errorf("resolutions: audit metadata: %v", err)
	}
	targetRef := "project:" + in.ProjectID
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_log (actor_id, via, action, target_ref, project_id,
		                       correlation_id, before_summary, after_summary, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, NULL, $7, $8)`,
		nullString(actor), via, domain.ActionConflictResolutionSaved, targetRef,
		in.ProjectID, correlation, string(after), string(metadata),
	); err != nil {
		return fmt.Errorf("resolutions: audit: %w", err)
	}
	return nil
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ListPlan implements StorePort: the decisions recorded for the triple,
// ordered by (target_kind, target_id) for stable rendering.
func (s *PGStore) ListPlan(ctx context.Context, projectID, baseStateID, sourceStateID, targetStateID string) ([]domain.ConflictResolution, error) {
	return listPlan(ctx, s.pool, projectID, baseStateID, sourceStateID, targetStateID)
}

// listPlan runs the plan read on the given querier (the transaction after
// a save, or the pool for a plain read).
func listPlan(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, projectID, baseStateID, sourceStateID, targetStateID string) ([]domain.ConflictResolution, error) {
	rows, err := q.Query(ctx, `
		SELECT `+resolutionColumns+` FROM conflict_resolutions
		WHERE project_id = $1 AND base_state_id = $2 AND source_state_id = $3 AND target_state_id = $4
		ORDER BY target_kind, target_id, conflict_code`, projectID, baseStateID, sourceStateID, targetStateID)
	if err != nil {
		return nil, fmt.Errorf("resolutions: list plan: %w", err)
	}
	defer rows.Close()
	plan := []domain.ConflictResolution{}
	for rows.Next() {
		var r domain.ConflictResolution
		var fields, payloadKeys []byte
		var otherObjectID *string
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.BaseStateID, &r.SourceStateID, &r.TargetStateID,
			&r.TargetKind, &r.TargetID, &r.Code, &fields, &payloadKeys, &otherObjectID,
			&r.Kind, &r.Note, &r.DecidedBy, &r.DecidedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("resolutions: scan plan row: %w", err)
		}
		if err := json.Unmarshal(fields, &r.Fields); err != nil {
			return nil, fmt.Errorf("resolutions: decode conflict_fields: %w", err)
		}
		if err := json.Unmarshal(payloadKeys, &r.PayloadKeys); err != nil {
			return nil, fmt.Errorf("resolutions: decode conflict_payload_keys: %w", err)
		}
		r.OtherObjectID = otherObjectID
		plan = append(plan, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolutions: list plan: %w", err)
	}
	return plan, nil
}

// ListEvidenceForObjectVersion implements StorePort: the evidence
// assertions targeting one object version, joined with the evidence
// objects they cite (id, type, title, version), oldest first.
func (s *PGStore) ListEvidenceForObjectVersion(ctx context.Context, objectVersionID string) ([]EvidenceItem, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ea.relation_type, ea.evidence_type, ea.directness, ea.reasoning_note,
		       ea.review_state, sov.object_id, so.object_type, sov.title, sov.version_no
		FROM evidence_assertions ea
		JOIN scientific_object_versions sov ON sov.id = ea.evidence_object_version_id
		JOIN scientific_objects so ON so.id = sov.object_id
		WHERE ea.target_object_version_id = $1
		ORDER BY ea.created_at, ea.id`, objectVersionID)
	if err != nil {
		return nil, fmt.Errorf("resolutions: list evidence: %w", err)
	}
	defer rows.Close()
	out := []EvidenceItem{}
	for rows.Next() {
		var e EvidenceItem
		if err := rows.Scan(&e.RelationType, &e.EvidenceType, &e.Directness, &e.ReasoningNote,
			&e.ReviewState, &e.EvidenceObjectID, &e.EvidenceObjectType, &e.EvidenceTitle,
			&e.EvidenceObjectVerNo); err != nil {
			return nil, fmt.Errorf("resolutions: scan evidence row: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolutions: list evidence: %w", err)
	}
	return out, nil
}

// The compile-time port assertion: the pgx adapter implements the store
// port the service consumes.
var _ StorePort = (*PGStore)(nil)
