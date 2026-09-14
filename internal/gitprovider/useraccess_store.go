package gitprovider

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The canonical-store side of user git access (T0304): the only reader
// and writer of git_user_identities, git_repo_access and git_access_tokens,
// plus the provision-row lookup the access service needs (the same table
// the provisioner writes, 00022 — this package owns it). Plain pgx, like
// PGProvisionStore: these columns belong to this package and must never
// ride the shared sqlc persistence surface.

// RepoRef is the provider-side identity of a project's provisioned
// repository, read from git_repository_provisions.
type RepoRef struct {
	ProjectID string
	Owner     string
	Name      string
}

// AccessRecord is one git_repo_access row (the repo access mapping).
type AccessRecord struct {
	ProjectID  string
	UserID     string
	Permission AccessLevel
	GrantedAt  time.Time
	RevokedAt  *time.Time
}

// TokenRecord is one git_access_tokens row. It never carries the token
// value (docs/55 SECRET — Gitea reveals it once, the platform stores
// nothing that can become a credential).
type TokenRecord struct {
	ID            string // canonical uuid
	ProjectID     string
	UserID        string
	GiteaUsername string
	TokenName     string
	GiteaTokenID  int64
	Scope         AccessLevel
	Status        string // "active" | "revoked"
	IssuedAt      time.Time
	RevokedAt     *time.Time
}

// Sentinel errors for the access store (callers distinguish "no row" from
// "store failed" — the difference decides between a 404 and a 503).
var (
	// ErrRepoNotProvisioned: the project has no git_repository_provisions
	// row — there is no repository to grant access to.
	ErrRepoNotProvisioned = errors.New("gitprovider: repository not provisioned")
	// ErrAccessNotFound: no git_repo_access row for (project, user).
	ErrAccessNotFound = errors.New("gitprovider: repository access not found")
	// ErrTokenNotFound: no git_access_tokens row with this id.
	ErrTokenNotFound = errors.New("gitprovider: git token not found")
)

// AccessStore is the canonical-store port the user-access service
// consumes. The concrete adapter is *PGUserAccessStore in this package.
type AccessStore interface {
	// RepoRef returns the project's provisioned repository reference.
	// ErrProjectNotFound when the project does not exist (or its id is
	// not a valid uuid), ErrRepoNotProvisioned when it has no repository.
	RepoRef(ctx context.Context, projectID string) (RepoRef, error)
	// UpsertGitIdentity records (or refreshes) the user → shadow-account
	// mapping. The mapping is deterministic (the name derives from the
	// user id), so an existing row always holds the same value.
	UpsertGitIdentity(ctx context.Context, userID, giteaUsername string) error
	// GetAccess returns one access row. ErrAccessNotFound when the grant
	// does not exist.
	GetAccess(ctx context.Context, projectID, userID string) (AccessRecord, error)
	// UpsertAccess records (or refreshes) a grant: an existing row is
	// updated in place — re-granting after a revocation is a new grant
	// (revoked_at cleared, granted_at refreshed), never a second row.
	UpsertAccess(ctx context.Context, rec AccessRecord) error
	// MarkAccessRevoked records an access revocation (sets revoked_at).
	// A grant that does not exist is not an error — the goal state "no
	// active access" already holds.
	MarkAccessRevoked(ctx context.Context, projectID, userID string) error
	// InsertToken records one minted token and returns it with the
	// canonical id filled in (the database generates it, RETURNING id —
	// the caller needs it to answer the issue call).
	InsertToken(ctx context.Context, rec TokenRecord) (TokenRecord, error)
	// GetToken returns one token row. ErrTokenNotFound when it does not
	// exist.
	GetToken(ctx context.Context, tokenID string) (TokenRecord, error)
	// ListTokens returns one user's tokens for one project, newest first
	// (active and revoked — the history stays visible, domain invariant
	// 8).
	ListTokens(ctx context.Context, projectID, userID string) ([]TokenRecord, error)
	// MarkTokenRevoked records a token revocation (status → revoked,
	// revoked_at set). A token that does not exist is not an error.
	MarkTokenRevoked(ctx context.Context, tokenID string) error
}

// PGUserAccessStore is the PostgreSQL adapter for the git-access tables.
type PGUserAccessStore struct {
	pool *pgxpool.Pool
}

// NewUserAccessStore builds the store on pool. The pool may be lazy
// (persistence.OpenLazy): the API keeps starting while PostgreSQL is down.
func NewUserAccessStore(pool *pgxpool.Pool) *PGUserAccessStore {
	return &PGUserAccessStore{pool: pool}
}

// RepoRef implements AccessStore.
func (s *PGUserAccessStore) RepoRef(ctx context.Context, projectID string) (RepoRef, error) {
	var ref RepoRef
	// COALESCE, not a nullable scan: pgx v5 refuses to scan NULL into
	// *string (the raw error would surface as a 503 instead of the 409
	// contract below), and '' is already the sentinel the guard tests —
	// so the guard below stays the single decision point. Pinned by
	// TestPGUserAccessStoreRepoRef (tests/integration), which failed with
	// "cannot scan NULL into *string" before this fix.
	err := s.pool.QueryRow(ctx,
		`SELECT p.id, COALESCE(g.owner, ''), COALESCE(g.name, '')
		   FROM projects p
		   LEFT JOIN git_repository_provisions g ON g.project_id = p.id
		  WHERE p.id = $1`, projectID).Scan(&ref.ProjectID, &ref.Owner, &ref.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return RepoRef{}, ErrProjectNotFound
	}
	if err != nil {
		return RepoRef{}, err
	}
	if ref.Owner == "" || ref.Name == "" {
		return RepoRef{}, ErrRepoNotProvisioned
	}
	return ref, nil
}

// UpsertGitIdentity implements AccessStore.
func (s *PGUserAccessStore) UpsertGitIdentity(ctx context.Context, userID, giteaUsername string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO git_user_identities (user_id, gitea_username)
		 VALUES ($1, $2)
		 ON CONFLICT (user_id) DO UPDATE SET gitea_username = EXCLUDED.gitea_username`,
		userID, giteaUsername)
	return err
}

// GetAccess implements AccessStore.
func (s *PGUserAccessStore) GetAccess(ctx context.Context, projectID, userID string) (AccessRecord, error) {
	var rec AccessRecord
	var revoked *time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT project_id, user_id, permission, granted_at, revoked_at
		   FROM git_repo_access WHERE project_id = $1 AND user_id = $2`,
		projectID, userID).Scan(&rec.ProjectID, &rec.UserID, &rec.Permission,
		&rec.GrantedAt, &revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return AccessRecord{}, ErrAccessNotFound
	}
	if err != nil {
		return AccessRecord{}, err
	}
	rec.RevokedAt = revoked
	return rec, nil
}

// UpsertAccess implements AccessStore. A revoked grant that is granted
// again becomes a fresh grant (revoked_at cleared) — the same row, so
// the mapping stays one row per (project, user) forever.
func (s *PGUserAccessStore) UpsertAccess(ctx context.Context, rec AccessRecord) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO git_repo_access (project_id, user_id, permission, granted_at, revoked_at)
		 VALUES ($1, $2, $3, now(), NULL)
		 ON CONFLICT (project_id, user_id) DO UPDATE SET
		   permission = EXCLUDED.permission, granted_at = now(), revoked_at = NULL`,
		rec.ProjectID, rec.UserID, string(rec.Permission))
	return err
}

// MarkAccessRevoked implements AccessStore.
func (s *PGUserAccessStore) MarkAccessRevoked(ctx context.Context, projectID, userID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE git_repo_access SET revoked_at = now()
		  WHERE project_id = $1 AND user_id = $2 AND revoked_at IS NULL`,
		projectID, userID)
	return err
}

// InsertToken implements AccessStore.
func (s *PGUserAccessStore) InsertToken(ctx context.Context, rec TokenRecord) (TokenRecord, error) {
	// RETURNING id, issued_at: the issue response carries issued_at
	// show-once, so the record the service returns must hold the
	// database-assigned timestamp (RETURNING id alone left it zero and
	// the wire showed 0001-01-01T00:00:00Z).
	err := s.pool.QueryRow(ctx,
		`INSERT INTO git_access_tokens
		   (project_id, user_id, gitea_username, token_name, gitea_token_id, scope, status)
		 VALUES ($1, $2, $3, $4, $5, $6, 'active')
		 RETURNING id, issued_at`,
		rec.ProjectID, rec.UserID, rec.GiteaUsername, rec.TokenName,
		rec.GiteaTokenID, string(rec.Scope)).Scan(&rec.ID, &rec.IssuedAt)
	return rec, err
}

// GetToken implements AccessStore.
func (s *PGUserAccessStore) GetToken(ctx context.Context, tokenID string) (TokenRecord, error) {
	var rec TokenRecord
	var revoked *time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT id, project_id, user_id, gitea_username, token_name, gitea_token_id,
		        scope, status, issued_at, revoked_at
		   FROM git_access_tokens WHERE id = $1`, tokenID).Scan(
		&rec.ID, &rec.ProjectID, &rec.UserID, &rec.GiteaUsername, &rec.TokenName,
		&rec.GiteaTokenID, &rec.Scope, &rec.Status, &rec.IssuedAt, &revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenRecord{}, ErrTokenNotFound
	}
	if err != nil {
		return TokenRecord{}, err
	}
	rec.RevokedAt = revoked
	return rec, nil
}

// ListTokens implements AccessStore.
func (s *PGUserAccessStore) ListTokens(ctx context.Context, projectID, userID string) ([]TokenRecord, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, project_id, user_id, gitea_username, token_name, gitea_token_id,
		        scope, status, issued_at, revoked_at
		   FROM git_access_tokens WHERE project_id = $1 AND user_id = $2
		  ORDER BY issued_at DESC, id`, projectID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TokenRecord
	for rows.Next() {
		var rec TokenRecord
		var revoked *time.Time
		if err := rows.Scan(&rec.ID, &rec.ProjectID, &rec.UserID, &rec.GiteaUsername,
			&rec.TokenName, &rec.GiteaTokenID, &rec.Scope, &rec.Status, &rec.IssuedAt,
			&revoked); err != nil {
			return nil, err
		}
		rec.RevokedAt = revoked
		out = append(out, rec)
	}
	return out, rows.Err()
}

// MarkTokenRevoked implements AccessStore.
func (s *PGUserAccessStore) MarkTokenRevoked(ctx context.Context, tokenID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE git_access_tokens SET status = 'revoked', revoked_at = now()
		  WHERE id = $1 AND status = 'active'`, tokenID)
	return err
}
