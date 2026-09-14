package integration

import (
	"errors"
	"testing"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/gitprovider"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

// TestPGUserAccessStoreRepoRef covers the REAL store adapter against real
// PostgreSQL and the real migrated schema (T0304 review finding, blocking):
// the unprovisioned branch of RepoRef was dead code — pgx v5 refuses to
// scan NULL into *string, so a project without a git_repository_provisions
// row answered the raw scan error (→ 503 on the wire) instead of
// ErrRepoNotProvisioned (→ 409 PROJECT_NOT_PROVISIONED). The previous
// tests injected the sentinel error through fakes, so they were green
// while the contract was unreachable in production. This test failed
// before the RepoRef fix ("cannot scan NULL into *string") and passes
// after; it also pins the InsertToken issued_at backfill (minor finding:
// the issue response carried 0001-01-01T00:00:00Z because RETURNING only
// read id).
func TestPGUserAccessStoreRepoRef(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), "T0304S")

	users := persistence.NewCredentialStore(pool)
	owner, err := users.CreateWithPassword(ctx,
		"store-ref-owner@example.com", "hash", "store-ref-owner", "Store Ref Owner")
	if err != nil {
		t.Fatalf("store test: seed owner: %v", err)
	}
	projects := persistence.NewProjectStore(pool)
	proj, _, err := projects.CreateProject(ctx, domain.Project{
		Slug:            "store-ref",
		Name:            "Store Ref",
		Purpose:         "T0304 store test",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, owner.ID)
	if err != nil {
		t.Fatalf("store test: create project: %v", err)
	}

	store := gitprovider.NewUserAccessStore(pool)

	// An EXISTING project with NO provision row must answer the sentinel —
	// this is the branch that was dead before the fix.
	if _, err := store.RepoRef(ctx, proj.ID); !errors.Is(err, gitprovider.ErrRepoNotProvisioned) {
		t.Fatalf("store test: RepoRef(existing, unprovisioned) = %v, want ErrRepoNotProvisioned", err)
	}

	// A project that does not exist must answer ErrProjectNotFound.
	if _, err := store.RepoRef(ctx, "99999999-9999-4999-8999-999999999999"); !errors.Is(err, gitprovider.ErrProjectNotFound) {
		t.Fatalf("store test: RepoRef(missing) = %v, want ErrProjectNotFound", err)
	}

	// InsertToken must return the database-assigned issued_at, not the
	// zero value (the show-once issue response carries it).
	if err := store.UpsertGitIdentity(ctx, owner.ID, "u-"+owner.ID); err != nil {
		t.Fatalf("store test: upsert identity: %v", err)
	}
	rec, err := store.InsertToken(ctx, gitprovider.TokenRecord{
		ProjectID:     proj.ID,
		UserID:        owner.ID,
		GiteaUsername: "u-" + owner.ID,
		TokenName:     "store-test-token",
		GiteaTokenID:  1,
		Scope:         gitprovider.AccessRead,
		Status:        "active",
	})
	if err != nil {
		t.Fatalf("store test: InsertToken: %v", err)
	}
	if rec.ID == "" {
		t.Fatal("store test: InsertToken returned no canonical id")
	}
	if rec.IssuedAt.IsZero() {
		t.Fatal("store test: InsertToken returned a zero issued_at — the issue response would carry 0001-01-01T00:00:00Z")
	}
}
