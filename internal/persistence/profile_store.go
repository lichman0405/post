package persistence

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/profile"
	"github.com/lichman0405/post/internal/domain"
)

// ProfileStore is the production profile.ProfileStore adapter over
// PostgreSQL: identity facts from users joined with profile content from
// profiles (migration 00017, shipped with T0102). The JOIN is a LEFT JOIN
// on purpose: a user whose profile row is missing (created by raw SQL, or
// before the migration backfill) still reads with an empty bio — an
// identity must never become unreadable because of the profile projection
// (docs/21 §6: nothing disappears). On a database that has not applied the
// profiles migration yet (a dev stack behind), every method answers a loud
// ErrProfilesMigrationMissing instead of a silent 500 — never a confusing
// failure (the T0101 ErrAuthMigrationMissing pattern).
type ProfileStore struct {
	pool *pgxpool.Pool
}

// NewProfileStore builds the store on pool. The pool may be lazy
// (OpenLazy): the API keeps starting while PostgreSQL is down.
func NewProfileStore(pool *pgxpool.Pool) *ProfileStore {
	return &ProfileStore{pool: pool}
}

// ErrProfilesMigrationMissing is returned when the profiles migration has
// not been applied yet (the profiles table does not exist) — a database
// behind on migrations, not a code defect.
var ErrProfilesMigrationMissing = errors.New("persistence: profiles migration 00017 missing (profiles table does not exist)")

// profileSelect is the shared read: identity + bio. u.email is coalesced
// to ” because the profile read must serve identities it did not create
// (raw-SQL imports can leave email NULL) — an identity must never become
// unreadable because of the profile projection, and the public profile
// payload never carries email anyway.
const profileSelect = `
SELECT u.id, u.handle, COALESCE(u.email, ''), u.display_name, u.created_at, u.disabled_at,
       COALESCE(p.bio, '')
  FROM users u LEFT JOIN profiles p ON p.user_id = u.id`

func scanProfile(row pgx.Row) (domain.Profile, error) {
	var p domain.Profile
	err := row.Scan(&p.User.ID, &p.User.Handle, &p.User.Email,
		&p.User.DisplayName, &p.User.CreatedAt, &p.User.DisabledAt, &p.Bio)
	if err != nil {
		return domain.Profile{}, err
	}
	return p, nil
}

// GetByUserID implements profile.ProfileStore. The id is the uuid text
// form; a value that is not a uuid cannot name a profile and answers
// ErrNotFound (PostgreSQL's 22P02), never a 500.
func (s *ProfileStore) GetByUserID(ctx context.Context, userID string) (domain.Profile, error) {
	p, err := scanProfile(s.pool.QueryRow(ctx, profileSelect+` WHERE u.id = $1`, userID))
	if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
		return domain.Profile{}, profile.ErrNotFound
	}
	if isUndefinedTable(err) {
		return domain.Profile{}, ErrProfilesMigrationMissing
	}
	if err != nil {
		return domain.Profile{}, fmt.Errorf("persistence: get profile by id: %w", err)
	}
	return p, nil
}

// GetByHandle implements profile.ProfileStore.
func (s *ProfileStore) GetByHandle(ctx context.Context, handle string) (domain.Profile, error) {
	p, err := scanProfile(s.pool.QueryRow(ctx, profileSelect+` WHERE u.handle = $1`, handle))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Profile{}, profile.ErrNotFound
	}
	if isUndefinedTable(err) {
		return domain.Profile{}, ErrProfilesMigrationMissing
	}
	if err != nil {
		return domain.Profile{}, fmt.Errorf("persistence: get profile by handle: %w", err)
	}
	return p, nil
}

// Update implements profile.ProfileStore. The two statements run in one
// transaction; the profile row is materialized first (ON CONFLICT DO
// NOTHING) so an owner edit self-heals a missing row (a user created by
// raw SQL). A handle collision (users.handle UNIQUE, 23505) is the one
// expected domain outcome and maps to ErrHandleTaken.
func (s *ProfileStore) Update(ctx context.Context, userID string, upd profile.Update) (domain.Profile, error) {
	err := WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE users SET
			  handle       = COALESCE($2, handle),
			  display_name = COALESCE($3, display_name)
			WHERE id = $1`, userID, upd.Handle, upd.DisplayName)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return profile.ErrNotFound
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO profiles (user_id) VALUES ($1) ON CONFLICT (user_id) DO NOTHING`, userID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx,
			`UPDATE profiles SET bio = COALESCE($2, bio), updated_at = now() WHERE user_id = $1`,
			userID, upd.Bio)
		return err
	})
	if err != nil {
		var pgErr *pgconn.PgError
		switch {
		case errors.Is(err, profile.ErrNotFound) || isInvalidText(err):
			// A value that is not a uuid cannot name a profile (22P02),
			// same as GetByUserID: ErrNotFound, never a 500.
			return domain.Profile{}, profile.ErrNotFound
		case errors.As(err, &pgErr) && pgErr.Code == "23505" && strings.Contains(pgErr.ConstraintName, "handle"):
			return domain.Profile{}, profile.ErrHandleTaken
		case isUndefinedTable(err):
			return domain.Profile{}, ErrProfilesMigrationMissing
		default:
			return domain.Profile{}, fmt.Errorf("persistence: update profile: %w", err)
		}
	}
	return s.GetByUserID(ctx, userID)
}

// isUndefinedTable reports whether err is PostgreSQL's undefined_table
// (42P01) — the profiles migration has not been applied.
func isUndefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
}

// isInvalidText reports whether err is PostgreSQL's invalid_text_representation
// (22P02) — a value that cannot be a uuid, which therefore cannot name a
// profile.
func isInvalidText(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

var _ profile.ProfileStore = (*ProfileStore)(nil)
