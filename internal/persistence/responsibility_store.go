package persistence

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/responsibilities"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// ResponsibilityStore is the production responsibilities.RuleStore adapter
// over PostgreSQL (sqlc generated queries, pgx) — the canonical tables
// research_owner_rules and responsibility_assignments (migration 00084,
// docs/04 §3).
//
// Every write is ONE transaction carrying both the row and its audit row
// (T0110's discipline, the same one-unit rule the policy and schema-profile
// surfaces follow): a routing rule that exists without the record of who
// wrote it would be project data with no history, and the tables are
// deliberately not append-only, so the audit log IS the history.
//
// Nothing here is read by internal/authz. docs/04 §3:「责任用于 Review
// routing，不自动赋予更高访问权限」— holding a label resolves the
// conditional submit_scientific_review verdict and nothing else.
type ResponsibilityStore struct {
	pool *pgxpool.Pool
}

// NewResponsibilityStore builds the store on pool. The pool may be lazy
// (OpenLazy): the API keeps starting while PostgreSQL is down.
func NewResponsibilityStore(pool *pgxpool.Pool) *ResponsibilityStore {
	return &ResponsibilityStore{pool: pool}
}

// ListRules implements responsibilities.RuleStore. An unknown or malformed
// project has no rules: empty list, not an error (the same read discipline
// as the PR list) — the required-review calculation then reports every
// change unrouted, which refuses rather than permits.
func (s *ResponsibilityStore) ListRules(ctx context.Context, projectID string) ([]domain.ResearchOwnerRule, error) {
	id, err := textUUID(projectID)
	if err != nil {
		return nil, nil
	}
	rows, err := sqlc.New(s.pool).ListResearchOwnerRules(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("persistence: list research owner rules: %w", err)
	}
	out := make([]domain.ResearchOwnerRule, 0, len(rows))
	for _, row := range rows {
		out = append(out, researchOwnerRuleFromRow(row))
	}
	return out, nil
}

// CreateRule implements responsibilities.RuleStore: the rule row and its
// audit row commit together.
func (s *ResponsibilityStore) CreateRule(ctx context.Context, in responsibilities.CreateRuleParams) (domain.ResearchOwnerRule, error) {
	projectID, err := textUUID(in.ProjectID)
	if err != nil {
		return domain.ResearchOwnerRule{}, responsibilities.ErrProjectNotFound
	}
	createdBy, err := textUUID(in.CreatedBy)
	if err != nil {
		return domain.ResearchOwnerRule{}, fmt.Errorf("%w: rule author id", responsibilities.ErrStore)
	}
	var rule domain.ResearchOwnerRule
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		row, err := q.CreateResearchOwnerRule(ctx, sqlc.CreateResearchOwnerRuleParams{
			ProjectID:      projectID,
			MatchKind:      string(in.MatchKind),
			MatchValue:     in.MatchValue,
			Responsibility: in.Responsibility,
			CreatedBy:      createdBy,
		})
		if err != nil {
			return mapResponsibilityWriteError(err)
		}
		rule = researchOwnerRuleFromRow(row)
		return appendAudit(ctx, q, in.Audit)
	})
	if err != nil {
		return domain.ResearchOwnerRule{}, err
	}
	return rule, nil
}

// DeleteRule implements responsibilities.RuleStore. The delete is
// project-scoped and answers whether a row was removed; the audit row is
// written only when one was — a delete that removed nothing is not a
// change and must not leave a record claiming one.
func (s *ResponsibilityStore) DeleteRule(ctx context.Context, in responsibilities.DeleteRuleParams) (bool, error) {
	projectID, err := textUUID(in.ProjectID)
	if err != nil {
		return false, responsibilities.ErrProjectNotFound
	}
	ruleID, err := textUUID(in.RuleID)
	if err != nil {
		return false, nil // cannot name a rule; nothing was deleted
	}
	deleted := false
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		rows, err := q.DeleteResearchOwnerRule(ctx, sqlc.DeleteResearchOwnerRuleParams{
			ID:        ruleID,
			ProjectID: projectID,
		})
		if err != nil {
			return mapResponsibilityWriteError(err)
		}
		if rows == 0 {
			return nil
		}
		deleted = true
		return appendAudit(ctx, q, in.Audit)
	})
	if err != nil {
		return false, err
	}
	return deleted, nil
}

// ListAssignments implements responsibilities.RuleStore. Unknown project:
// empty list, not an error (same discipline as ListRules).
func (s *ResponsibilityStore) ListAssignments(ctx context.Context, projectID string) ([]domain.ResponsibilityAssignment, error) {
	id, err := textUUID(projectID)
	if err != nil {
		return nil, nil
	}
	rows, err := sqlc.New(s.pool).ListResponsibilityAssignments(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("persistence: list responsibility assignments: %w", err)
	}
	out := make([]domain.ResponsibilityAssignment, 0, len(rows))
	for _, row := range rows {
		out = append(out, responsibilityAssignmentFromRow(row))
	}
	return out, nil
}

// Assign implements responsibilities.RuleStore. Holding a label is ONE
// fact, so the write is idempotent by construction: the primary key
// refuses a second row, the store reads the existing one back, and the
// audit row is written only when the assignment actually appeared — a
// repeated assignment is a no-op that leaves the log alone (the same
// discipline the freeze store applies to a repeated freeze).
func (s *ResponsibilityStore) Assign(ctx context.Context, in responsibilities.AssignParams) (domain.ResponsibilityAssignment, error) {
	projectID, err := textUUID(in.ProjectID)
	if err != nil {
		return domain.ResponsibilityAssignment{}, responsibilities.ErrProjectNotFound
	}
	userID, err := textUUID(in.UserID)
	if err != nil {
		return domain.ResponsibilityAssignment{}, responsibilities.ErrUserNotFound
	}
	createdBy, err := textUUID(in.CreatedBy)
	if err != nil {
		return domain.ResponsibilityAssignment{}, fmt.Errorf("%w: assignment author id", responsibilities.ErrStore)
	}
	var assignment domain.ResponsibilityAssignment
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		row, err := q.CreateResponsibilityAssignment(ctx, sqlc.CreateResponsibilityAssignmentParams{
			ProjectID:      projectID,
			UserID:         userID,
			Responsibility: in.Responsibility,
			CreatedBy:      createdBy,
		})
		switch {
		case err == nil:
			assignment = responsibilityAssignmentFromRow(row)
			return appendAudit(ctx, q, in.Audit)
		case errors.Is(err, pgx.ErrNoRows):
			// The row already exists: the assignment is the fact the caller
			// asked for, and nothing is written (no second row, no audit
			// row claiming a change that did not happen).
			existing, rerr := q.GetResponsibilityAssignment(ctx, sqlc.GetResponsibilityAssignmentParams{
				ProjectID:      projectID,
				UserID:         userID,
				Responsibility: in.Responsibility,
			})
			if rerr != nil {
				return mapResponsibilityWriteError(rerr)
			}
			assignment = responsibilityAssignmentFromRow(existing)
			return nil
		default:
			return mapResponsibilityWriteError(err)
		}
	})
	if err != nil {
		return domain.ResponsibilityAssignment{}, err
	}
	return assignment, nil
}

// Unassign implements responsibilities.RuleStore: the audit row is written
// only when a row was actually removed (same rule as DeleteRule).
func (s *ResponsibilityStore) Unassign(ctx context.Context, in responsibilities.UnassignParams) (bool, error) {
	projectID, err := textUUID(in.ProjectID)
	if err != nil {
		return false, responsibilities.ErrProjectNotFound
	}
	userID, err := textUUID(in.UserID)
	if err != nil {
		return false, nil // cannot name a user; nothing was removed
	}
	removed := false
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		rows, err := q.DeleteResponsibilityAssignment(ctx, sqlc.DeleteResponsibilityAssignmentParams{
			ProjectID:      projectID,
			UserID:         userID,
			Responsibility: in.Responsibility,
		})
		if err != nil {
			return mapResponsibilityWriteError(err)
		}
		if rows == 0 {
			return nil
		}
		removed = true
		return appendAudit(ctx, q, in.Audit)
	})
	if err != nil {
		return false, err
	}
	return removed, nil
}

// LabelsForUser implements responsibilities.RuleStore: the labels one user
// holds in one project, sorted, which is the resolver's raw answer
// (docs/04 §3). Unknown project or user: empty list, not an error — the
// resolver's caller turns "no labels" into a refusal, never into a
// permission.
func (s *ResponsibilityStore) LabelsForUser(ctx context.Context, projectID, userID string) ([]string, error) {
	pid, err := textUUID(projectID)
	if err != nil {
		return nil, nil
	}
	uid, err := textUUID(userID)
	if err != nil {
		return nil, nil
	}
	labels, err := sqlc.New(s.pool).ListResponsibilitiesForUser(ctx, sqlc.ListResponsibilitiesForUserParams{
		ProjectID: pid,
		UserID:    uid,
	})
	if err != nil {
		return nil, fmt.Errorf("persistence: list responsibilities for user: %w", err)
	}
	return labels, nil
}

// mapResponsibilityWriteError turns a failed routing/assignment write into
// the package outcomes: the uniqueness violations are the expected domain
// conflicts (the same rule twice, the same assignment twice — the latter
// is handled before this mapping, so only the rule conflict can arrive
// here), the foreign keys are the missing-identity outcomes, and check
// violations on the validated input are validation results.
func mapResponsibilityWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return responsibilities.ErrRuleExists
		case "23503":
			return responsibilities.ErrUserNotFound
		case "23502", "23514", "22P02":
			return responsibilities.ErrValidation
		}
	}
	if isInvalidText(err) {
		return responsibilities.ErrValidation
	}
	return err
}

// researchOwnerRuleFromRow converts a sqlc research_owner_rules row to the
// domain value.
func researchOwnerRuleFromRow(row sqlc.ResearchOwnerRule) domain.ResearchOwnerRule {
	return domain.ResearchOwnerRule{
		ID:             pgUUIDToText(row.ID),
		ProjectID:      pgUUIDToText(row.ProjectID),
		MatchKind:      domain.ResearchOwnerMatchKind(row.MatchKind),
		MatchValue:     row.MatchValue,
		Responsibility: row.Responsibility,
		CreatedBy:      pgUUIDToText(row.CreatedBy),
		CreatedAt:      row.CreatedAt.Time,
	}
}

// responsibilityAssignmentFromRow converts a sqlc responsibility_assignments
// row to the domain value.
func responsibilityAssignmentFromRow(row sqlc.ResponsibilityAssignment) domain.ResponsibilityAssignment {
	return domain.ResponsibilityAssignment{
		ProjectID:      pgUUIDToText(row.ProjectID),
		UserID:         pgUUIDToText(row.UserID),
		Responsibility: row.Responsibility,
		CreatedBy:      pgUUIDToText(row.CreatedBy),
		CreatedAt:      row.CreatedAt.Time,
	}
}
