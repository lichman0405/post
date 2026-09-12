package integration

// T0102: the pgx ProfileStore against a REAL PostgreSQL — the same graph
// the production binary wires (credential store creates the identity and,
// atomically, its profile row; the profile store reads/updates it).
//
// The profiles migration (infra/migrations/00017_profiles.sql) ships in
// this task, and testdb.Setup always migrates to head, so unlike the
// T0101 auth test there is no "migration not applied yet" probe: this
// suite requires 00017 by construction.

import (
	"context"
	"errors"
	"testing"

	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/profile"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

// profileTaskID namespaces this task's test databases (test_T0102_<run_id>).
const profileTaskID = "T0102"

func strPtr(s string) *string { return &s }

// TestProfileStoreSignupCreatesRowAndRoundtrips: a signup through the
// credential store lands the identity AND an empty-bio profile row in one
// statement (the CTE); the profile store then reads it by id and by
// handle, updates all three editable fields, and the old handle stops
// resolving while the id-keyed row never moves.
func TestProfileStoreSignupCreatesRowAndRoundtrips(t *testing.T) {
	ctx := context.Background()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), profileTaskID)
	creds := persistence.NewCredentialStore(pool)
	store := persistence.NewProfileStore(pool)

	hash, err := authn.HashPassword("integration-password-123")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	user, err := creds.CreateWithPassword(ctx, "prof-alice@example.com", hash, "prof-alice", "Prof Alice")
	if err != nil {
		t.Fatalf("CreateWithPassword: %v", err)
	}

	// The signup created the profile row with an empty bio.
	p, err := store.GetByUserID(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetByUserID: %v", err)
	}
	if p.Bio != "" || p.User.Handle != "prof-alice" || p.User.DisplayName != "Prof Alice" {
		t.Errorf("fresh profile = %+v", p)
	}

	// By-handle resolves the same identity.
	byHandle, err := store.GetByHandle(ctx, "prof-alice")
	if err != nil || byHandle.User.ID != user.ID {
		t.Fatalf("GetByHandle: %v, %+v", err, byHandle)
	}

	// Update all three editable fields.
	updated, err := store.Update(ctx, user.ID, profile.Update{
		Handle:      strPtr("prof-alice-2"),
		DisplayName: strPtr("Prof Alice II"),
		Bio:         strPtr("materials informatics"),
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.User.Handle != "prof-alice-2" || updated.User.DisplayName != "Prof Alice II" ||
		updated.Bio != "materials informatics" {
		t.Errorf("updated profile = %+v", updated)
	}

	// The id-keyed read shows the new values; the new handle resolves; the
	// old handle is gone (handles are current projections, not history).
	again, err := store.GetByUserID(ctx, user.ID)
	if err != nil || again.Bio != "materials informatics" || again.User.Handle != "prof-alice-2" {
		t.Errorf("re-read by id: %v, %+v", err, again)
	}
	if _, err := store.GetByHandle(ctx, "prof-alice-2"); err != nil {
		t.Errorf("GetByHandle(new): %v", err)
	}
	if _, err := store.GetByHandle(ctx, "prof-alice"); !errors.Is(err, profile.ErrNotFound) {
		t.Errorf("GetByHandle(old) = %v, want ErrNotFound", err)
	}
}

// TestProfileStoreHandleConflictAndNotFounds: the database UNIQUE enforces
// handle ownership (23505 → ErrHandleTaken), and unknown ids — including
// malformed uuids — answer ErrNotFound everywhere.
func TestProfileStoreHandleConflictAndNotFounds(t *testing.T) {
	ctx := context.Background()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), profileTaskID)
	creds := persistence.NewCredentialStore(pool)
	store := persistence.NewProfileStore(pool)

	hash, err := authn.HashPassword("integration-password-123")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	alice, err := creds.CreateWithPassword(ctx, "prof-alice@example.com", hash, "prof-alice", "Prof Alice")
	if err != nil {
		t.Fatalf("CreateWithPassword alice: %v", err)
	}
	if _, err := creds.CreateWithPassword(ctx, "prof-bob@example.com", hash, "prof-bob", "Prof Bob"); err != nil {
		t.Fatalf("CreateWithPassword bob: %v", err)
	}

	// Taking Bob's handle is refused by the unique index.
	if _, err := store.Update(ctx, alice.ID, profile.Update{Handle: strPtr("prof-bob")}); !errors.Is(err, profile.ErrHandleTaken) {
		t.Errorf("handle conflict = %v, want ErrHandleTaken", err)
	}

	// Unknown and malformed ids: ErrNotFound (never a scan error).
	for _, id := range []string{"00000000-0000-4000-8000-000000000000", "not-a-uuid"} {
		if _, err := store.GetByUserID(ctx, id); !errors.Is(err, profile.ErrNotFound) {
			t.Errorf("GetByUserID(%q) = %v, want ErrNotFound", id, err)
		}
		if _, err := store.Update(ctx, id, profile.Update{Bio: strPtr("x")}); !errors.Is(err, profile.ErrNotFound) {
			t.Errorf("Update(%q) = %v, want ErrNotFound", id, err)
		}
	}
	if _, err := store.GetByHandle(ctx, "nobody"); !errors.Is(err, profile.ErrNotFound) {
		t.Errorf("GetByHandle(nobody) = %v, want ErrNotFound", err)
	}
}

// TestProfileStoreMaterializesMissingRow: an identity inserted by raw SQL
// (a path that bypasses the credential store CTE — e.g. data imported
// outside the API) has no profile row yet; reads must answer with an empty
// bio rather than failing, and the first Update must materialize the row
// so the 1:1 shape is restored.
func TestProfileStoreMaterializesMissingRow(t *testing.T) {
	ctx := context.Background()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), profileTaskID)
	store := persistence.NewProfileStore(pool)

	var rawID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (handle, display_name) VALUES ('prof-raw', 'Raw User') RETURNING id`).Scan(&rawID); err != nil {
		t.Fatalf("raw user insert: %v", err)
	}

	// Read without a profile row: empty bio, no error.
	p, err := store.GetByUserID(ctx, rawID)
	if err != nil {
		t.Fatalf("GetByUserID (no profile row): %v", err)
	}
	if p.Bio != "" || p.User.Handle != "prof-raw" {
		t.Errorf("profile without row = %+v", p)
	}

	// Update materializes the row.
	updated, err := store.Update(ctx, rawID, profile.Update{Bio: strPtr("retro bio")})
	if err != nil {
		t.Fatalf("Update (no profile row): %v", err)
	}
	if updated.Bio != "retro bio" {
		t.Errorf("updated bio = %q", updated.Bio)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM profiles WHERE user_id = $1`, rawID).Scan(&n); err != nil {
		t.Fatalf("profile row probe: %v", err)
	}
	if n != 1 {
		t.Errorf("profile row count after Update = %d, want 1", n)
	}
	if again, err := store.GetByUserID(ctx, rawID); err != nil || again.Bio != "retro bio" {
		t.Errorf("re-read after materialization: %v, %+v", err, again)
	}
}
