package persistence

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/domain"
)

// CredentialStore is the production authn.UserStore adapter over
// PostgreSQL.
//
// The password_hash column used below is added by migration
// 00016_auth_password.sql (infra/migrations, shipped with T0101). On a
// database that has not applied it yet (a dev stack behind on migrations),
// CreateWithPassword and FindByEmail answer a loud ErrAuthMigrationMissing
// (pg code 42703 undefined_column detected) instead of a silent 500 —
// never a confusing failure.
type CredentialStore struct {
	pool *pgxpool.Pool
}

// NewCredentialStore builds the store on pool. The pool may be lazy
// (OpenLazy): the API keeps starting while PostgreSQL is down.
func NewCredentialStore(pool *pgxpool.Pool) *CredentialStore {
	return &CredentialStore{pool: pool}
}

// ErrAuthMigrationMissing is returned when the auth migration has not been
// applied yet (users.password_hash does not exist) — a database behind on
// migrations, not a code defect.
var ErrAuthMigrationMissing = errors.New("persistence: auth migration 00016 missing (users.password_hash column does not exist)")

// FindByEmail implements authn.UserStore. The email column is UNIQUE, so
// the lookup is an index scan; the returned record's PasswordHash is empty
// when the account is OIDC-only (NULL column).
func (s *CredentialStore) FindByEmail(ctx context.Context, email string) (authn.UserRecord, error) {
	rec, err := scanUser(s.pool.QueryRow(ctx,
		`SELECT id, handle, email, display_name, created_at, disabled_at, password_hash
		   FROM users WHERE email = $1`, email))
	if errors.Is(err, pgx.ErrNoRows) {
		return authn.UserRecord{}, authn.ErrUserNotFound
	}
	if isUndefinedColumn(err) {
		return authn.UserRecord{}, ErrAuthMigrationMissing
	}
	if err != nil {
		return authn.UserRecord{}, fmt.Errorf("persistence: find user by email: %w", err)
	}
	return rec, nil
}

// GetByID implements authn.UserStore. The id is the uuid text form; pgx
// encodes/decodes the users.id uuid column from/to it natively.
func (s *CredentialStore) GetByID(ctx context.Context, id string) (authn.UserRecord, error) {
	rec, err := scanUser(s.pool.QueryRow(ctx,
		`SELECT id, handle, email, display_name, created_at, disabled_at, password_hash
		   FROM users WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return authn.UserRecord{}, authn.ErrUserNotFound
	}
	if isUndefinedColumn(err) {
		return authn.UserRecord{}, ErrAuthMigrationMissing
	}
	if err != nil {
		return authn.UserRecord{}, fmt.Errorf("persistence: find user by id: %w", err)
	}
	return rec, nil
}

// CreateWithPassword implements authn.UserStore. Handle collisions are
// resolved with a deterministic suffix (the domain requires a unique
// handle and a derived one may collide — L1 decision). The args order
// matches the INSERT column order exactly (handle, email, ...) — a swap
// here stores the email as the handle and vice versa.
func (s *CredentialStore) CreateWithPassword(ctx context.Context, email, passwordHash, handle, displayName string) (domain.User, error) {
	return s.createUser(ctx, handle,
		`INSERT INTO users (handle, email, display_name, password_hash)
		 VALUES ($1, $2, $3, $4)`, handle, email, displayName, passwordHash)
}

// CreateOIDC implements authn.UserStore.
func (s *CredentialStore) CreateOIDC(ctx context.Context, email, handle, displayName string) (domain.User, error) {
	return s.createUser(ctx, handle,
		`INSERT INTO users (handle, email, display_name)
		 VALUES ($1, $2, $3)`, handle, email, displayName)
}

// createUser runs one INSERT and translates constraint violations:
// email uniqueness -> ErrEmailTaken, handle uniqueness -> retry with a
// suffix, missing password_hash column -> ErrAuthMigrationMissing. The
// args slice is always (handle, email, display_name[, password_hash])
// matching the SQL column order, and handle is always args[0] for the
// collision retry.
func (s *CredentialStore) createUser(ctx context.Context, handle, sql string, args ...any) (domain.User, error) {
	// Insert with the given handle; on handle collision derive a new one
	// and retry once. Collisions are rare (derived handles), so a single
	// retry is enough in practice.
	row := s.pool.QueryRow(ctx, sql+" RETURNING id, handle, email, display_name, created_at, disabled_at", args...)
	var user domain.User
	if err := row.Scan(&user.ID, &user.Handle, &user.Email, &user.DisplayName, &user.CreatedAt, &user.DisabledAt); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch {
			case pgErr.Code == "23505" && strings.Contains(pgErr.ConstraintName, "email"):
				return domain.User{}, authn.ErrEmailTaken
			case pgErr.Code == "23505" && strings.Contains(pgErr.ConstraintName, "handle"):
				retry := handle + "-" + shortSuffix()
				row = s.pool.QueryRow(ctx, sql+" RETURNING id, handle, email, display_name, created_at, disabled_at",
					replaceArg(args, 0, retry)...)
				err = row.Scan(&user.ID, &user.Handle, &user.Email, &user.DisplayName, &user.CreatedAt, &user.DisabledAt)
				if err != nil {
					return domain.User{}, fmt.Errorf("persistence: create user (handle retry): %w", err)
				}
				return user, nil
			}
		}
		if isUndefinedColumn(err) {
			return domain.User{}, ErrAuthMigrationMissing
		}
		return domain.User{}, fmt.Errorf("persistence: create user: %w", err)
	}
	return user, nil
}

func scanUser(row pgx.Row) (authn.UserRecord, error) {
	var rec authn.UserRecord
	var passwordHash *string
	err := row.Scan(&rec.User.ID, &rec.User.Handle, &rec.User.Email,
		&rec.User.DisplayName, &rec.User.CreatedAt, &rec.User.DisabledAt, &passwordHash)
	if err != nil {
		return authn.UserRecord{}, err
	}
	if passwordHash != nil {
		rec.PasswordHash = *passwordHash
	}
	return rec, nil
}

// isUndefinedColumn reports whether err is PostgreSQL's undefined_column
// (42703) — the auth migration has not been applied.
func isUndefinedColumn(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42703"
}

// shortSuffix derives a compact handle suffix from the current time.
func shortSuffix() string {
	return fmt.Sprintf("%x", time.Now().UnixNano()%0xffff)
}

// replaceArg returns args with the element at index replaced (index is the
// 0-based position of the handle argument in the SQL args list, i.e. 0 —
// createUser always passes handle first to match the INSERT column order).
func replaceArg(args []any, index int, value string) []any {
	out := make([]any, len(args))
	copy(out, args)
	out[index] = value
	return out
}

var _ authn.UserStore = (*CredentialStore)(nil)
