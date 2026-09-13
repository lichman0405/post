package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/gitprovider"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

// The canonical-store side of provisioning, exercised against real
// PostgreSQL only (no Gitea needed): the backlog work-list policy and the
// provision transaction's guards. These run in CI's postgres-only matrix
// alongside the live-Gitea tests in gitea_provisioning_test.go.

// storeFixture seeds a migrated test database with one user and one
// provision-pending project.
type storeFixture struct {
	pool    *pgxpool.Pool
	user    domain.User
	project domain.Project
}

func newStoreFixture(t *testing.T, ctx context.Context) *storeFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), giteaProvisionTaskID)
	users := persistence.NewCredentialStore(pool)
	user, err := users.CreateWithPassword(ctx, "gitea-store@example.com", "hash", "gitea-store", "Gitea Store")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	project, _, err := persistence.NewProjectStore(pool).CreateProject(ctx, domain.Project{
		Slug:            "gitea-store",
		Name:            "Gitea Store",
		Purpose:         "T0301 store fixtures",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, user.ID)
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	return &storeFixture{pool: pool, user: user, project: project}
}

func (fx *storeFixture) backlogIDs(t *testing.T, ctx context.Context) []string {
	t.Helper()
	backlog, err := gitprovider.NewProvisionStore(fx.pool).ProvisioningBacklog(ctx)
	if err != nil {
		t.Fatalf("ProvisioningBacklog: %v", err)
	}
	var ids []string
	for _, p := range backlog {
		ids = append(ids, p.ID)
	}
	return ids
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestProvisioningBacklogCoversFailed: the boot sweep's work list is
// provision_status IN ('pending', 'failed') — a failed project stays in
// the backlog (the bounded retry policy), a provisioned one leaves it.
func TestProvisioningBacklogCoversFailed(t *testing.T) {
	ctx := testCtx(t)
	fx := newStoreFixture(t, ctx)
	id := fx.project.ID

	if !containsID(fx.backlogIDs(t, ctx), id) {
		t.Fatal("pending project missing from the backlog")
	}

	if _, err := fx.pool.Exec(ctx, `UPDATE projects SET provision_status = 'failed' WHERE id = $1`, id); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	if !containsID(fx.backlogIDs(t, ctx), id) {
		t.Error("failed project missing from the backlog — the boot sweep could never re-attempt it")
	}

	if _, err := fx.pool.Exec(ctx,
		`UPDATE projects SET provision_status = 'provisioned', git_repository_external_id = 'o/n' WHERE id = $1`, id); err != nil {
		t.Fatalf("mark provisioned: %v", err)
	}
	if containsID(fx.backlogIDs(t, ctx), id) {
		t.Error("provisioned project still in the backlog")
	}
}

// TestProvisionReattemptsFailedRow: a failed row is not terminal — the
// next Provision call runs the callback again and the row recovers (the
// re-attempt path the boot sweep enqueues into).
func TestProvisionReattemptsFailedRow(t *testing.T) {
	ctx := testCtx(t)
	fx := newStoreFixture(t, ctx)
	if _, err := fx.pool.Exec(ctx, `UPDATE projects SET provision_status = 'failed' WHERE id = $1`, fx.project.ID); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	store := gitprovider.NewProvisionStore(fx.pool)
	provisioned, skipped, err := store.Provision(ctx, fx.project.ID, func(p gitprovider.PendingProject) (*gitprovider.ProvisionRecord, error) {
		return &gitprovider.ProvisionRecord{
			ProjectID: p.ID, Owner: "o", Name: "n",
			GiteaRepoID: 1, WebhookID: 2, WebhookSecret: "s",
		}, nil
	})
	if err != nil || !provisioned || skipped {
		t.Fatalf("Provision = provisioned %v skipped %v err %v, want a successful re-attempt", provisioned, skipped, err)
	}
}

// TestProvisionNilRecordRejected: a provisioning callback that succeeds
// but returns no record is a programming error the store reports — never
// a panic — and the transaction rolls back: the row keeps its previous
// status (it stays in the backlog) and nothing is recorded.
func TestProvisionNilRecordRejected(t *testing.T) {
	ctx := testCtx(t)
	fx := newStoreFixture(t, ctx)
	store := gitprovider.NewProvisionStore(fx.pool)

	provisioned, skipped, err := store.Provision(ctx, fx.project.ID,
		func(gitprovider.PendingProject) (*gitprovider.ProvisionRecord, error) {
			return nil, nil
		})
	if provisioned || skipped {
		t.Errorf("Provision = provisioned %v skipped %v, want false/false", provisioned, skipped)
	}
	if err == nil || !strings.Contains(err.Error(), "no record") {
		t.Errorf("Provision error = %v, want the no-record error", err)
	}

	var status string
	if err := fx.pool.QueryRow(ctx,
		`SELECT provision_status FROM projects WHERE id = $1`, fx.project.ID).Scan(&status); err != nil {
		t.Fatalf("probe status: %v", err)
	}
	if status != "pending" {
		t.Errorf("provision_status = %q, want pending (the transaction rolled back)", status)
	}
	var rows int
	if err := fx.pool.QueryRow(ctx,
		`SELECT count(*) FROM git_repository_provisions WHERE project_id = $1`, fx.project.ID).Scan(&rows); err != nil {
		t.Fatalf("count provision rows: %v", err)
	}
	if rows != 0 {
		t.Errorf("provision rows = %d, want 0", rows)
	}
}
