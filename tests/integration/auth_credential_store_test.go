package integration

// T0101: the pgx CredentialStore against a REAL PostgreSQL.
//
// The store is fully exercised whenever the auth migration is in place
// (users.password_hash, shipped as infra/migrations/00016_auth_password.sql).
// A database that has not applied it yet (e.g. a dev stack behind on
// migrations) makes the suite skip with a loud, printed reason (docs/66:
// skips must explain themselves, never hide a gap) — the tests themselves
// need no editing once the migration lands.

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

// authTaskID namespaces this task's test databases (test_T0101_<run_id>).
const authTaskID = "T0101"

// authColumnPresent probes the live catalog for users.password_hash — the
// marker that the auth migration (00016, shipped in infra/migrations) has
// been applied to this database.
func authColumnPresent(t *testing.T, ctx context.Context, pool *pgxpool.Pool) bool {
	t.Helper()
	var exists bool
	err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = 'users'
			  AND column_name = 'password_hash')`).Scan(&exists)
	if err != nil {
		t.Fatalf("probe auth column: %v", err)
	}
	return exists
}

// TestCredentialStorePasswordRoundtrip: create with a real argon2id hash,
// find by email and by id, then the negative cases (unknown email,
// duplicate email) and the OIDC-only shape (NULL password_hash).
func TestCredentialStorePasswordRoundtrip(t *testing.T) {
	ctx := context.Background()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), authTaskID)

	if !authColumnPresent(t, ctx, pool) {
		t.Skip("auth migration (users.password_hash) not applied to this database — infra/migrations/00016_auth_password.sql is shipped in-repo; migrate the database and this test runs")
	}
	store := persistence.NewCredentialStore(pool)

	hash, err := authn.HashPassword("integration-password-123")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	user, err := store.CreateWithPassword(ctx, "int-alice@example.com", hash, "int-alice", "Alice Int")
	if err != nil {
		t.Fatalf("CreateWithPassword: %v", err)
	}
	if user.Email != "int-alice@example.com" || user.Handle != "int-alice" {
		t.Fatalf("created user = %+v", user)
	}

	// FindByEmail returns the record with the hash.
	rec, err := store.FindByEmail(ctx, "int-alice@example.com")
	if err != nil {
		t.Fatalf("FindByEmail: %v", err)
	}
	if rec.PasswordHash != hash || rec.User.ID != user.ID {
		t.Error("FindByEmail record does not match the created user")
	}
	if rec.User.DisabledAt != nil {
		t.Error("fresh account must not be disabled")
	}

	// GetByID resolves the same identity.
	rec2, err := store.GetByID(ctx, user.ID)
	if err != nil || rec2.User.Email != "int-alice@example.com" {
		t.Fatalf("GetByID: %v, %+v", err, rec2)
	}

	// Unknown email -> ErrUserNotFound.
	if _, err := store.FindByEmail(ctx, "nobody@example.com"); !errors.Is(err, authn.ErrUserNotFound) {
		t.Errorf("unknown email = %v, want authn.ErrUserNotFound", err)
	}

	// Duplicate email -> ErrEmailTaken.
	if _, err := store.CreateWithPassword(ctx, "int-alice@example.com", hash, "int-alice-2", "Alice Two"); !errors.Is(err, authn.ErrEmailTaken) {
		t.Errorf("duplicate email = %v, want authn.ErrEmailTaken", err)
	}

	// OIDC-only account: no password hash (NULL), findable, and a password
	// signup on the same email is refused (one identity per email).
	_, err = store.CreateOIDC(ctx, "int-bob@example.com", "int-bob", "Bob Int")
	if err != nil {
		t.Fatalf("CreateOIDC: %v", err)
	}
	oidcRec, err := store.FindByEmail(ctx, "int-bob@example.com")
	if err != nil {
		t.Fatalf("FindByEmail (oidc): %v", err)
	}
	if oidcRec.PasswordHash != "" {
		t.Error("OIDC-only account must have an empty password hash")
	}
	if _, err := store.CreateWithPassword(ctx, "int-bob@example.com", hash, "int-bob-2", "Bob Two"); !errors.Is(err, authn.ErrEmailTaken) {
		t.Errorf("password signup on an OIDC account = %v, want authn.ErrEmailTaken (same email, one identity)", err)
	}
}
