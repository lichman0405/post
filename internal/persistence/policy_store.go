package persistence

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// PolicyStore is the production policy.PolicyStore adapter over
// PostgreSQL (sqlc generated queries, pgx). The canonical
// policy_versions table is append-only — every write is an INSERT of a
// new version row — and the org lower-bound invariant is enforced inside
// the write transaction: project-policy inserts row-lock the owning
// organization (the same lock org-policy inserts take), so a policy
// publication and a lower-bound check can never interleave.
type PolicyStore struct {
	pool *pgxpool.Pool
}

// NewPolicyStore builds the store on pool. The pool may be lazy
// (OpenLazy): the API keeps starting while PostgreSQL is down.
func NewPolicyStore(pool *pgxpool.Pool) *PolicyStore {
	return &PolicyStore{pool: pool}
}

// CreateOrgVersion implements policy.PolicyStore. The insert and its
// audit row commit in one transaction, serialized on the organization row
// lock (GetOrganizationByIDForUpdate) — concurrent policy writes for the
// same organization cannot interleave their "latest" reads and inserts.
func (s *PolicyStore) CreateOrgVersion(ctx context.Context, v domain.PolicyVersion, audit domain.AuditEntry) (domain.PolicyVersion, error) {
	oID, err := textUUID(v.Scope.OrganizationID)
	if err != nil {
		return domain.PolicyVersion{}, policy.ErrOrgNotFound
	}
	createdBy, err := textUUID(v.CreatedBy)
	if err != nil {
		return domain.PolicyVersion{}, fmt.Errorf("persistence: policy creator id: %w", err)
	}
	var created domain.PolicyVersion
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if _, err := q.GetOrganizationByIDForUpdate(ctx, oID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return policy.ErrOrgNotFound
			}
			return err
		}
		row, err := q.CreatePolicyVersion(ctx, sqlc.CreatePolicyVersionParams{
			OrganizationID: oID,
			Version:        v.Version,
			PolicyJson:     mustPolicyJSON(v.Policy),
			CreatedBy:      createdBy,
		})
		if err != nil {
			return err
		}
		created, err = policyVersionFromRow(row)
		if err != nil {
			return err
		}
		return appendAudit(ctx, q, audit)
	})
	if err != nil {
		return domain.PolicyVersion{}, mapPolicyWriteError(err)
	}
	return created, nil
}

// CreateProjectVersion implements policy.PolicyStore. When orgID is
// non-nil the transaction row-locks the organization, re-reads its latest
// policy under that lock and runs againstOrg — the lower-bound check the
// service already applied against the state it read — so a concurrent
// org-policy publication can never be slipped under. The insert and its
// audit row commit with the check.
func (s *PolicyStore) CreateProjectVersion(ctx context.Context, v domain.PolicyVersion, orgID *string, againstOrg func(orgPolicy *domain.Policy) error, audit domain.AuditEntry) (domain.PolicyVersion, error) {
	pID, err := textUUID(v.Scope.ProjectID)
	if err != nil {
		return domain.PolicyVersion{}, policy.ErrProjectNotFound
	}
	createdBy, err := textUUID(v.CreatedBy)
	if err != nil {
		return domain.PolicyVersion{}, fmt.Errorf("persistence: policy creator id: %w", err)
	}
	var created domain.PolicyVersion
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if orgID != nil {
			oID, err := textUUID(*orgID)
			if err != nil {
				return policy.ErrOrgNotFound
			}
			if _, err := q.GetOrganizationByIDForUpdate(ctx, oID); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return policy.ErrOrgNotFound
				}
				return err
			}
			row, err := q.LatestPolicyVersionByOrg(ctx, oID)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				if err := againstOrg(nil); err != nil {
					return err
				}
			case err != nil:
				return err
			default:
				latest, err := policyVersionFromRow(row)
				if err != nil {
					return err
				}
				if err := againstOrg(&latest.Policy); err != nil {
					return err
				}
			}
		} else if err := againstOrg(nil); err != nil {
			return err
		}
		row, err := q.CreatePolicyVersion(ctx, sqlc.CreatePolicyVersionParams{
			ProjectID:  pID,
			Version:    v.Version,
			PolicyJson: mustPolicyJSON(v.Policy),
			CreatedBy:  createdBy,
		})
		if err != nil {
			return err
		}
		created, err = policyVersionFromRow(row)
		if err != nil {
			return err
		}
		return appendAudit(ctx, q, audit)
	})
	if err != nil {
		return domain.PolicyVersion{}, mapPolicyWriteError(err)
	}
	return created, nil
}

// GetVersion implements policy.PolicyStore.
func (s *PolicyStore) GetVersion(ctx context.Context, id string) (domain.PolicyVersion, error) {
	u, err := textUUID(id)
	if err != nil {
		return domain.PolicyVersion{}, policy.ErrPolicyNotFound
	}
	row, err := sqlc.New(s.pool).GetPolicyVersion(ctx, u)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.PolicyVersion{}, policy.ErrPolicyNotFound
	}
	if err != nil {
		return domain.PolicyVersion{}, fmt.Errorf("persistence: get policy version: %w", err)
	}
	v, err := policyVersionFromRow(row)
	if err != nil {
		return domain.PolicyVersion{}, err
	}
	return v, nil
}

// Latest implements policy.PolicyStore.
func (s *PolicyStore) Latest(ctx context.Context, scope domain.PolicyScope) (domain.PolicyVersion, error) {
	rows, err := s.list(ctx, scope, true)
	if err != nil {
		return domain.PolicyVersion{}, err
	}
	if len(rows) == 0 {
		return domain.PolicyVersion{}, policy.ErrPolicyNotFound
	}
	return rows[0], nil
}

// List implements policy.PolicyStore: every version of the scope, newest
// first.
func (s *PolicyStore) List(ctx context.Context, scope domain.PolicyScope) ([]domain.PolicyVersion, error) {
	return s.list(ctx, scope, false)
}

// list runs the scoped version query (latest = LIMIT 1 on the newest).
func (s *PolicyStore) list(ctx context.Context, scope domain.PolicyScope, latest bool) ([]domain.PolicyVersion, error) {
	q := sqlc.New(s.pool)
	var rows []sqlc.PolicyVersion
	var err error
	switch {
	case scope.OrganizationID != "":
		oID, uerr := textUUID(scope.OrganizationID)
		if uerr != nil {
			return nil, policy.ErrPolicyNotFound
		}
		if latest {
			var row sqlc.PolicyVersion
			row, err = q.LatestPolicyVersionByOrg(ctx, oID)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, policy.ErrPolicyNotFound
			}
			if err == nil {
				rows = []sqlc.PolicyVersion{row}
			}
		} else {
			rows, err = q.ListPolicyVersionsByOrg(ctx, oID)
		}
	case scope.ProjectID != "":
		pID, uerr := textUUID(scope.ProjectID)
		if uerr != nil {
			return nil, policy.ErrPolicyNotFound
		}
		if latest {
			var row sqlc.PolicyVersion
			row, err = q.LatestPolicyVersionByProject(ctx, pID)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, policy.ErrPolicyNotFound
			}
			if err == nil {
				rows = []sqlc.PolicyVersion{row}
			}
		} else {
			rows, err = q.ListPolicyVersionsByProject(ctx, pID)
		}
	default:
		return nil, policy.ErrPolicyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("persistence: list policy versions: %w", err)
	}
	out := make([]domain.PolicyVersion, 0, len(rows))
	for _, row := range rows {
		v, err := policyVersionFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// mustPolicyJSON renders the policy document for the jsonb column. A
// rendering failure here means the service passed an invalid document —
// a programming error that must not reach the wire as a 500, so the
// panic is deliberate: the service validates before calling.
func mustPolicyJSON(p domain.Policy) []byte {
	b, err := p.MarshalJSON()
	if err != nil {
		panic("persistence: invalid policy document: " + err.Error())
	}
	return b
}

// policyVersionFromRow converts a sqlc policy_versions row to the domain
// value. A stored policy_json that no longer parses is a store failure
// (fail closed — a corrupt governance document grants nothing).
func policyVersionFromRow(row sqlc.PolicyVersion) (domain.PolicyVersion, error) {
	p, err := domain.PolicyFromJSON(row.PolicyJson)
	if err != nil {
		return domain.PolicyVersion{}, fmt.Errorf("persistence: stored policy_json of %s is invalid: %w", pgUUIDToText(row.ID), err)
	}
	scope := domain.PolicyScope{}
	if row.OrganizationID.Valid {
		scope.OrganizationID = pgUUIDToText(row.OrganizationID)
	}
	if row.ProjectID.Valid {
		scope.ProjectID = pgUUIDToText(row.ProjectID)
	}
	return domain.PolicyVersion{
		ID:        pgUUIDToText(row.ID),
		Scope:     scope,
		Version:   row.Version,
		Policy:    p,
		CreatedBy: pgUUIDToText(row.CreatedBy),
		CreatedAt: row.CreatedAt.Time,
	}, nil
}

// mapPolicyWriteError translates policy_versions driver errors onto the
// policy service's sentinels.
func mapPolicyWriteError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, policy.ErrPolicyNotFound),
		errors.Is(err, policy.ErrOrgNotFound),
		errors.Is(err, policy.ErrProjectNotFound),
		errors.Is(err, policy.ErrProjectRelaxesOrg):
		return err
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "23505" &&
			(strings.Contains(pgErr.ConstraintName, "policy_versions_org_version_idx") ||
				strings.Contains(pgErr.ConstraintName, "policy_versions_project_version_idx")):
			return policy.ErrVersionTaken
		case pgErr.Code == "23514" && strings.Contains(pgErr.ConstraintName, "policy_versions"):
			return policy.ErrValidation
		case pgErr.Code == "23503" && strings.Contains(pgErr.ConstraintName, "organization_id"):
			return policy.ErrOrgNotFound
		case pgErr.Code == "23503" && strings.Contains(pgErr.ConstraintName, "project_id"):
			return policy.ErrProjectNotFound
		}
	}
	return fmt.Errorf("persistence: write policy version: %w", err)
}

var _ policy.PolicyStore = (*PolicyStore)(nil)
