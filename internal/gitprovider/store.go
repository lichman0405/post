package gitprovider

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The canonical-store side of provisioning (T0301): the only writer of
// projects.provision_status / git_repository_external_id and the only
// reader and writer of the git_repository_provisions table. This package
// owns those columns by task assignment (internal/persistence holds the
// product stores), which is also what keeps the webhook secret off every
// other read path.

// PendingProject is the slice of the project row provisioning needs. The
// work list (the provisioning backlog) derives from provision_status IN
// ('pending', 'failed') (00019): a pending project is one whose repository
// does not exist yet, a failed one had its last attempt rejected.
type PendingProject struct {
	ID   string
	Slug string
	Name string
}

// ProvisionRecord is the outcome of one successful provisioning: the
// provider-side facts plus the webhook HMAC secret (docs/55 SECRET — it
// exists in the canonical store because T0305 must verify push
// signatures, and in no other read path).
type ProvisionRecord struct {
	ProjectID     string
	Owner         string
	Name          string
	GiteaRepoID   int64
	WebhookID     int64
	WebhookSecret string
}

// ErrProjectNotFound: the project row does not exist (or is not a valid
// uuid — indistinguishable to the caller).
var ErrProjectNotFound = errors.New("gitprovider: project not found")

// PGProvisionStore is the PostgreSQL adapter for the provisioning tables
// (plain pgx: the columns belong to this package, sqlc queries would put
// them on the shared persistence surface). It implements ProvisionStore,
// the canonical-store port the provisioner consumes.
type PGProvisionStore struct {
	pool *pgxpool.Pool
}

// NewProvisionStore builds the store on pool. The pool may be lazy
// (persistence.OpenLazy): the API keeps starting while PostgreSQL is down.
func NewProvisionStore(pool *pgxpool.Pool) *PGProvisionStore {
	return &PGProvisionStore{pool: pool}
}

// ProvisioningBacklog lists every project awaiting provisioning, oldest
// first: provision_status = 'pending' (never attempted) or 'failed' (the
// last attempt was rejected). The boot sweep re-enqueues the whole backlog
// on every API start — that is the bounded retry policy: a transient
// provider outage marks rows failed, and the next sweep (or a redelivered
// job) re-attempts them, with no hot retry loop.
func (s *PGProvisionStore) ProvisioningBacklog(ctx context.Context) ([]PendingProject, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, slug, name FROM projects
		 WHERE provision_status IN ('pending', 'failed')
		 ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingProject
	for rows.Next() {
		var p PendingProject
		if err := rows.Scan(&p.ID, &p.Slug, &p.Name); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ProvisionedRepositories lists every provisioned repository, oldest first
// — the main-protection sweep's work list (T0302): the platform re-verifies
// the Gitea rule on every one of them, so a rule an operator removed or
// drifted is re-applied without the platform remembering provider state.
func (s *PGProvisionStore) ProvisionedRepositories(ctx context.Context) ([]Repository, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT owner, name FROM git_repository_provisions ORDER BY provisioned_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Repository
	for rows.Next() {
		var r Repository
		if err := rows.Scan(&r.Owner, &r.Name); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Provision serializes one provisioning attempt per project: the project
// row is locked FOR UPDATE, an already-provisioned project is skipped
// (idempotency — a redelivered job is a no-op), otherwise fn performs the
// provider work while the lock is held and its outcome is committed in the
// same transaction. fn must be quick (the lock is held across it; the job
// timeout bounds it) and must not use this store itself.
//
// On success the project moves to provisioned with the external reference
// and the provision record is upserted (re-provisioning a failed project
// overwrites the previous partial record). On failure the project moves
// to failed — the canonical store records the state, the job layer's
// structured logs carry the redacted reason, and the error is returned to
// the loop for retry. A failed row stays in the backlog
// (ProvisioningBacklog), so the next boot sweep or redelivered job
// re-attempts it: a transient provider outage never wedges a project.
func (s *PGProvisionStore) Provision(ctx context.Context, projectID string, fn func(PendingProject) (*ProvisionRecord, error)) (provisioned, skipped bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var p PendingProject
	var status string
	err = tx.QueryRow(ctx,
		`SELECT id, slug, name, provision_status FROM projects WHERE id = $1 FOR UPDATE`,
		projectID).Scan(&p.ID, &p.Slug, &p.Name, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, ErrProjectNotFound
	}
	if err != nil {
		return false, false, err
	}
	if status == "provisioned" {
		if err := tx.Commit(ctx); err != nil {
			return false, false, err
		}
		return false, true, nil
	}

	rec, fnErr := fn(p)
	if fnErr != nil {
		// The canonical failure record is the state transition; the
		// reason travels on the returned error (the job loop logs it
		// with the job's correlation id — the provider errors are built
		// redacted by construction, never carrying the token or the
		// configured URLs).
		if _, uerr := tx.Exec(ctx,
			`UPDATE projects SET provision_status = 'failed' WHERE id = $1`,
			projectID); uerr != nil {
			if cerr := tx.Commit(ctx); cerr != nil {
				return false, false, fmt.Errorf("%w (and recording the failure failed: %v)", fnErr, cerr)
			}
			return false, false, fmt.Errorf("%w (recording the failure failed: %v)", fnErr, uerr)
		}
		if err := tx.Commit(ctx); err != nil {
			return false, false, err
		}
		return false, false, fnErr
	}
	if rec == nil {
		// fn succeeded but returned no record — a programming error on the
		// caller's side, never a panic on the provisioning path. The
		// transaction rolls back and the row keeps its previous status (it
		// stays in the backlog for the next sweep).
		return false, false, errors.New("gitprovider: provisioning function returned no record")
	}

	if _, err := tx.Exec(ctx,
		`UPDATE projects SET provision_status = 'provisioned',
		        git_repository_external_id = $2
		 WHERE id = $1`,
		projectID, rec.Owner+"/"+rec.Name); err != nil {
		return false, false, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO git_repository_provisions
		   (project_id, owner, name, gitea_repo_id, webhook_id, webhook_secret)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (project_id) DO UPDATE SET
		   owner = EXCLUDED.owner, name = EXCLUDED.name,
		   gitea_repo_id = EXCLUDED.gitea_repo_id, webhook_id = EXCLUDED.webhook_id,
		   webhook_secret = EXCLUDED.webhook_secret, provisioned_at = now()`,
		rec.ProjectID, rec.Owner, rec.Name, rec.GiteaRepoID, rec.WebhookID, rec.WebhookSecret); err != nil {
		return false, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, false, err
	}
	return true, false, nil
}
