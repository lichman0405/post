package persistence

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/audit"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// AuditStore is the production audit adapter over PostgreSQL: it appends
// audit_log rows (auth events on their own connection; state-changing
// actions through appendAudit inside the owning store's transaction) and
// serves the Activity page queries. The table itself is append-only by
// database triggers (migrations 00014/00015): this store has no UPDATE or
// DELETE path, and none is offered.
type AuditStore struct {
	pool *pgxpool.Pool
}

// NewAuditStore builds the store on pool. The pool may be lazy (OpenLazy):
// the API keeps starting while PostgreSQL is down.
func NewAuditStore(pool *pgxpool.Pool) *AuditStore {
	return &AuditStore{pool: pool}
}

// Record implements authn.AuditRecorder: appends one audit row for an auth
// event (login success/failure, signup, logout). These events have no
// PostgreSQL state change to share a transaction with (sessions live in
// Redis), so the row is its own statement; the caller treats a failure as
// best-effort, never as a reason to fail the auth outcome.
//
// Unlike appendAudit, the entry is authoritative as-is: the auth service
// knows the actor of each event, and "" means "no actor" (an unknown-email
// login), never "fill from the request" — the request's session holder is
// not the actor of a login attempt.
func (s *AuditStore) Record(ctx context.Context, e domain.AuditEntry) error {
	params, err := auditParams(ctx, e, false)
	if err != nil {
		return err
	}
	if _, err := sqlc.New(s.pool).RecordAuditLogEntry(ctx, params); err != nil {
		return fmt.Errorf("persistence: record audit %s: %w", e.Action, err)
	}
	return nil
}

// ListProjectActivity implements audit.Store: the project's audit rows,
// newest first, keyset-paginated on (occurred_at, id).
func (s *AuditStore) ListProjectActivity(ctx context.Context, projectID string, before *audit.Cursor, limit int) ([]domain.AuditRecord, error) {
	id, err := textUUID(projectID)
	if err != nil {
		return nil, nil // an id that is not a uuid matches no rows
	}
	beforeTS, beforeID, err := cursorParams(before)
	if err != nil {
		return nil, fmt.Errorf("persistence: list project activity: %w", err)
	}
	rows, err := sqlc.New(s.pool).ListProjectAuditEntries(ctx, sqlc.ListProjectAuditEntriesParams{
		ProjectID: id,
		BeforeTs:  beforeTS,
		BeforeID:  beforeID,
		PageLimit: int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("persistence: list project activity: %w", err)
	}
	out := make([]domain.AuditRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, auditRecordFromListRow(
			r.ID, r.ActorID, r.Via, r.Action, r.TargetRef, r.ProjectID,
			r.OrganizationID, r.CorrelationID, r.BeforeSummary, r.AfterSummary,
			r.Metadata, r.OccurredAt, r.ActorHandle, r.ActorDisplayName))
	}
	return out, nil
}

// ListOrganizationActivity implements audit.Store: the organization's audit
// rows, newest first, keyset-paginated on (occurred_at, id).
func (s *AuditStore) ListOrganizationActivity(ctx context.Context, orgID string, before *audit.Cursor, limit int) ([]domain.AuditRecord, error) {
	id, err := textUUID(orgID)
	if err != nil {
		return nil, nil // an id that is not a uuid matches no rows
	}
	beforeTS, beforeID, err := cursorParams(before)
	if err != nil {
		return nil, fmt.Errorf("persistence: list organization activity: %w", err)
	}
	rows, err := sqlc.New(s.pool).ListOrganizationAuditEntries(ctx, sqlc.ListOrganizationAuditEntriesParams{
		OrganizationID: id,
		BeforeTs:       beforeTS,
		BeforeID:       beforeID,
		PageLimit:      int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("persistence: list organization activity: %w", err)
	}
	out := make([]domain.AuditRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, auditRecordFromListRow(
			r.ID, r.ActorID, r.Via, r.Action, r.TargetRef, r.ProjectID,
			r.OrganizationID, r.CorrelationID, r.BeforeSummary, r.AfterSummary,
			r.Metadata, r.OccurredAt, r.ActorHandle, r.ActorDisplayName))
	}
	return out, nil
}

// cursorParams renders a keyset cursor (or the top of the log) as the two
// SQL params: a nil cursor means "no lower bound". The id must parse as a
// uuid. The service rejects non-uuid cursor ids before they get here, so a
// failure is unreachable through the HTTP surface — but it is an error,
// never a panic (this path once panicked on a client-forged cursor).
func cursorParams(before *audit.Cursor) (pgtype.Timestamptz, pgtype.UUID, error) {
	if before == nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, nil
	}
	id, err := textUUID(before.ID)
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, fmt.Errorf("persistence: audit cursor id: %w", err)
	}
	return pgtype.Timestamptz{Time: before.OccurredAt, Valid: true}, id, nil
}

// auditRecordFromListRow converts one joined audit row to the domain value.
// The summaries stay raw jsonb bytes: the transport embeds them untouched.
func auditRecordFromListRow(id pgtype.UUID, actorID pgtype.UUID, via, action string,
	targetRef *string, projectID, organizationID pgtype.UUID, correlationID string,
	beforeSummary, afterSummary, metadata []byte, occurredAt pgtype.Timestamptz,
	actorHandle, actorDisplayName *string) domain.AuditRecord {
	return domain.AuditRecord{
		ID:               pgUUIDToText(id),
		ActorID:          uuidPtr(actorID),
		ActorHandle:      actorHandle,
		ActorDisplayName: actorDisplayName,
		Via:              via,
		Action:           action,
		TargetRef:        targetRef,
		ProjectID:        uuidPtr(projectID),
		OrganizationID:   uuidPtr(organizationID),
		CorrelationID:    correlationID,
		BeforeSummary:    beforeSummary,
		AfterSummary:     afterSummary,
		Metadata:         metadata,
		OccurredAt:       occurredAt.Time,
	}
}

// uuidPtr converts a nullable pgx uuid to *string (nil when NULL).
func uuidPtr(u pgtype.UUID) *string {
	if !u.Valid {
		return nil
	}
	s := pgUUIDToText(u)
	return &s
}

// appendAudit writes one audit entry inside the caller's transaction — the
// stores call it after the state change they own, so the audit row commits
// or rolls back with the action itself (docs/53: the audit record of a
// high-risk action is part of the action, never a separate best-effort
// write). Actor, via and correlation id come from the request context (the
// auth guard attaches them); fields set on the entry win, so a store that
// knows the actor explicitly (project/organization creation) can pass it.
//
// Fail-closed: a failed audit write fails the whole action.
func appendAudit(ctx context.Context, q *sqlc.Queries, e domain.AuditEntry) error {
	params, err := auditParams(ctx, e, true)
	if err != nil {
		return err
	}
	if _, err := q.RecordAuditLogEntry(ctx, params); err != nil {
		return fmt.Errorf("persistence: audit %s: %w", e.Action, err)
	}
	return nil
}

// auditParams renders one entry into sqlc parameters. With fillFromRequest,
// empty actor/via/correlation id fields are filled from the request context
// (the auth guard attaches them) — the stores rely on that. Without it (the
// auth recorder), the entry is authoritative as-is: "" means NULL, never a
// request-derived value. Via falls back to "internal" either way — a call
// without any request identity still lands in the log, it just has no HTTP
// actor to name.
func auditParams(ctx context.Context, e domain.AuditEntry, fillFromRequest bool) (sqlc.RecordAuditLogEntryParams, error) {
	actor, via, corr := e.ActorID, e.Via, e.CorrelationID
	if fillFromRequest {
		if info, ok := domain.RequestInfoFrom(ctx); ok {
			if actor == "" {
				actor = info.ActorID
			}
			if via == "" {
				via = info.Via
			}
			if corr == "" {
				corr = info.CorrelationID
			}
		}
	}
	if via == "" {
		via = domain.ViaInternal
	}

	var actorID pgtype.UUID
	if actor != "" {
		id, err := textUUID(actor)
		if err != nil {
			return sqlc.RecordAuditLogEntryParams{}, fmt.Errorf("persistence: audit actor id: %w", err)
		}
		actorID = id
	}
	projectID, err := optionalUUID(e.ProjectID)
	if err != nil {
		return sqlc.RecordAuditLogEntryParams{}, fmt.Errorf("persistence: audit project id: %w", err)
	}
	organizationID, err := optionalUUID(e.OrganizationID)
	if err != nil {
		return sqlc.RecordAuditLogEntryParams{}, fmt.Errorf("persistence: audit organization id: %w", err)
	}

	return sqlc.RecordAuditLogEntryParams{
		ActorID:        actorID,
		Via:            via,
		Action:         e.Action,
		TargetRef:      nullableText(e.TargetRef),
		ProjectID:      projectID,
		OrganizationID: organizationID,
		CorrelationID:  corr,
		BeforeSummary:  jsonbOrNil(e.BeforeSummary),
		AfterSummary:   jsonbOrNil(e.AfterSummary),
		Metadata:       jsonbOrDefault(e.Metadata),
	}, nil
}

// optionalUUID parses a scope id; "" means "no scope" (NULL).
func optionalUUID(s string) (pgtype.UUID, error) {
	if s == "" {
		return pgtype.UUID{}, nil
	}
	return textUUID(s)
}

// nullableText renders "" as NULL (target_ref may be absent).
func nullableText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// jsonbOrNil marshals a summary; nil means NULL (before/after may be
// absent).
func jsonbOrNil(v any) []byte {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		// Summaries are maps of scalars built by the stores; a marshal
		// failure here is a programming error, and the row must not
		// silently lose its summary.
		panic(fmt.Sprintf("persistence: audit summary marshal: %v", err))
	}
	return b
}

// jsonbOrDefault marshals metadata; nil means the canonical '{}' (the
// column is NOT NULL DEFAULT '{}').
func jsonbOrDefault(v any) []byte {
	if v == nil {
		return []byte("{}")
	}
	return jsonbOrNil(v)
}

var _ audit.Store = (*AuditStore)(nil)
