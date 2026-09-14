package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/gitprovider"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

// The canonical-store side of branch-ref sync (T0303), against real
// PostgreSQL only (no Gitea needed): the trigger-maintained mapping rows
// (migration 00031), the backlog work-list policy, and SyncBranchRef's
// transaction guards. These run in CI's postgres-only matrix alongside the
// live-Gitea tests in gitea_branch_ref_test.go; the trigger rejection
// cases live in migration_test.go (TestConstraintEnforcement).

const branchRefTaskID = "T0303"

const refTestHeadSHA = "0123456789abcdef0123456789abcdef01234567"

// branchRefFixture: one migrated test database, one user, one project, its
// genesis state and the branches service — branch creation goes through
// the product path, like every consumer. The genesis state may carry its
// provider-side commit (the T0305 ingestion fact: project_states is
// append-only, so the sha arrives WITH the row, never by UPDATE).
type branchRefFixture struct {
	pool     *pgxpool.Pool
	branches *branches.Service
	alice    domain.User
	project  domain.Project
	genesis  domain.ProjectState
}

func newBranchRefFixture(t *testing.T, ctx context.Context, genesisSHA string) *branchRefFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), branchRefTaskID)
	alice, err := persistence.NewCredentialStore(pool).CreateWithPassword(
		ctx, "branchref-alice@example.com", "hash", "branchref-alice", "Alice")
	if err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	project, _, err := persistence.NewProjectStore(pool).CreateProject(ctx, domain.Project{
		Slug:            "branchref-project",
		Name:            "Branch Ref Project",
		Purpose:         "T0303 store fixtures",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, alice.ID)
	if err != nil {
		t.Fatalf("create fixture project: %v", err)
	}
	var genesisID string
	if err := pool.QueryRow(ctx, `INSERT INTO project_states
		(project_id, state_hash, manifest_version, git_commit_sha)
		VALUES ($1, $2, 'v1', NULLIF($3, '')) RETURNING id`,
		project.ID, "genesis-"+genesisSHA, genesisSHA).Scan(&genesisID); err != nil {
		t.Fatalf("create genesis state: %v", err)
	}
	return &branchRefFixture{
		pool:     pool,
		branches: branches.NewService(persistence.NewBranchStore(pool)),
		alice:    alice,
		project:  project,
		genesis:  domain.ProjectState{ID: genesisID},
	}
}

func (f *branchRefFixture) createBranch(t *testing.T, ctx context.Context, name string) domain.Branch {
	t.Helper()
	branch, err := f.branches.Create(ctx, branches.CreateBranchParams{
		ProjectID:   f.project.ID,
		Name:        name,
		Visibility:  domain.BranchVisibilityPrivate,
		BaseStateID: f.genesis.ID,
		CreatedBy:   f.alice.ID,
	})
	if err != nil {
		t.Fatalf("create branch %s: %v", name, err)
	}
	return branch
}

// provision lands the T0301 mapping row the backlog join gates on.
func (f *branchRefFixture) provision(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := f.pool.Exec(ctx, `INSERT INTO git_repository_provisions
		(project_id, owner, name, gitea_repo_id, webhook_id, webhook_secret)
		VALUES ($1::uuid, 'post-git-svc', 'p-' || $1, 1, 1, 's')`, f.project.ID); err != nil {
		t.Fatalf("seed provision row: %v", err)
	}
}

// mappingRow probes one git_branch_refs row.
func (f *branchRefFixture) mappingRow(t *testing.T, ctx context.Context, branchID string) (gitRef, state, forkSHA, headSHA string, closeRequested bool) {
	t.Helper()
	if err := f.pool.QueryRow(ctx, `SELECT git_ref, sync_state, COALESCE(fork_sha, ''),
		COALESCE(head_sha, ''), close_requested_at IS NOT NULL
		FROM git_branch_refs WHERE branch_id = $1`, branchID).
		Scan(&gitRef, &state, &forkSHA, &headSHA, &closeRequested); err != nil {
		t.Fatalf("probe git_branch_refs: %v", err)
	}
	return
}

// TestBranchRefMappingRowAutoCreated: branch creation through the product
// service produces exactly one mapping row (the trigger, migration 00031)
// with the DERIVED ref — the semantic branch and its Git ref mapping can
// never drift apart, whatever path inserted the row.
func TestBranchRefMappingRowAutoCreated(t *testing.T) {
	ctx := testCtx(t)
	fx := newBranchRefFixture(t, ctx, "")

	branch := fx.createBranch(t, ctx, "feature-x")
	if branch.GitRef != "refs/heads/feature-x" {
		t.Errorf("service-derived git ref = %q, want refs/heads/feature-x", branch.GitRef)
	}

	gitRef, state, _, _, _ := fx.mappingRow(t, ctx, branch.ID)
	if gitRef != "refs/heads/feature-x" {
		t.Errorf("mapping git_ref = %q, want refs/heads/feature-x", gitRef)
	}
	if state != "pending" {
		t.Errorf("mapping sync_state = %q, want pending", state)
	}
}

// TestBranchRefBacklogGatedByProvision: the backlog join on
// git_repository_provisions is the gate — an unprovisioned project's
// branch never enters the work list (there is no repository to sync
// into), and once provisioned it appears with the semantic fork point
// (the base state's git_commit_sha) and the close flag false.
func TestBranchRefBacklogGatedByProvision(t *testing.T) {
	ctx := testCtx(t)
	fx := newBranchRefFixture(t, ctx, refTestHeadSHA)
	branch := fx.createBranch(t, ctx, "feature-x")

	store := gitprovider.NewBranchRefStore(fx.pool)
	backlog, err := store.BranchRefBacklog(ctx)
	if err != nil {
		t.Fatalf("BranchRefBacklog: %v", err)
	}
	for _, p := range backlog {
		if p.BranchID == branch.ID {
			t.Fatal("unprovisioned branch entered the backlog — there is no repository to sync into")
		}
	}

	fx.provision(t, ctx)
	backlog, err = store.BranchRefBacklog(ctx)
	if err != nil {
		t.Fatalf("BranchRefBacklog (provisioned): %v", err)
	}
	var got *gitprovider.PendingBranchRef
	for i := range backlog {
		if backlog[i].BranchID == branch.ID {
			got = &backlog[i]
		}
	}
	if got == nil {
		t.Fatal("provisioned branch missing from the backlog")
	}
	if got.GitRef != "refs/heads/feature-x" || got.Name != "feature-x" {
		t.Errorf("backlog item ref/name = %s/%s", got.GitRef, got.Name)
	}
	if got.Owner != "post-git-svc" || got.Repo != "p-"+fx.project.ID {
		t.Errorf("backlog item repository = %s/%s", got.Owner, got.Repo)
	}
	if got.ForkSHA != refTestHeadSHA {
		t.Errorf("backlog item fork sha = %q, want the base state's %s", got.ForkSHA, refTestHeadSHA)
	}
	if got.Close {
		t.Error("active branch's work item is a close — must be a create")
	}
}

// TestBranchRefSyncCreateCommitsRecord: SyncBranchRef runs the callback
// once with the backlog work item and commits the outcome atomically —
// synced, fork point and head recorded. A redelivered job is skipped (the
// row left the backlog), the callback never runs again.
func TestBranchRefSyncCreateCommitsRecord(t *testing.T) {
	ctx := testCtx(t)
	fx := newBranchRefFixture(t, ctx, refTestHeadSHA)
	branch := fx.createBranch(t, ctx, "feature-x")
	fx.provision(t, ctx)

	store := gitprovider.NewBranchRefStore(fx.pool)
	calls := 0
	synced, skipped, err := store.SyncBranchRef(ctx, branch.ID, func(p gitprovider.PendingBranchRef) (*gitprovider.BranchRefRecord, error) {
		calls++
		if p.ForkSHA != refTestHeadSHA || p.Close {
			t.Errorf("callback work item = fork %q close %v, want fork %s create", p.ForkSHA, p.Close, refTestHeadSHA)
		}
		return &gitprovider.BranchRefRecord{BranchID: p.BranchID, ForkSHA: p.ForkSHA, HeadSHA: refTestHeadSHA}, nil
	})
	if err != nil || !synced || skipped {
		t.Fatalf("SyncBranchRef = synced %v skipped %v err %v, want a clean sync", synced, skipped, err)
	}
	if calls != 1 {
		t.Fatalf("callback ran %d times, want 1", calls)
	}

	gitRef, state, forkSHA, headSHA, _ := fx.mappingRow(t, ctx, branch.ID)
	if gitRef != "refs/heads/feature-x" || state != "synced" {
		t.Errorf("mapping row = %s/%s, want refs/heads/feature-x/synced", gitRef, state)
	}
	if forkSHA != refTestHeadSHA || headSHA != refTestHeadSHA {
		t.Errorf("mapping row shas = fork %q head %q, want both %s", forkSHA, headSHA, refTestHeadSHA)
	}

	// Redelivery: the row is terminal for the create direction — skipped,
	// the provider work unreachable.
	synced, skipped, err = store.SyncBranchRef(ctx, branch.ID, func(gitprovider.PendingBranchRef) (*gitprovider.BranchRefRecord, error) {
		calls++
		return nil, errors.New("must not run")
	})
	if err != nil || synced || !skipped {
		t.Fatalf("redelivered SyncBranchRef = synced %v skipped %v err %v, want skipped", synced, skipped, err)
	}
	if calls != 1 {
		t.Errorf("callback ran %d times after redelivery, want still 1", calls)
	}
}

// TestBranchRefSyncFailureKeepsRetryable: a provider failure moves the row
// to 'failed' (still in the backlog — the boot sweep's bounded retry) and
// a later attempt recovers it. The failed state is direction-preserving:
// the row carries the same work item on retry.
func TestBranchRefSyncFailureKeepsRetryable(t *testing.T) {
	ctx := testCtx(t)
	fx := newBranchRefFixture(t, ctx, refTestHeadSHA)
	branch := fx.createBranch(t, ctx, "feature-x")
	fx.provision(t, ctx)

	store := gitprovider.NewBranchRefStore(fx.pool)
	_, _, err := store.SyncBranchRef(ctx, branch.ID, func(gitprovider.PendingBranchRef) (*gitprovider.BranchRefRecord, error) {
		return nil, errors.New("provider down")
	})
	if err == nil || err.Error() != "provider down" {
		t.Fatalf("SyncBranchRef error = %v, want the provider failure", err)
	}
	_, state, _, _, _ := fx.mappingRow(t, ctx, branch.ID)
	if state != "failed" {
		t.Errorf("sync_state after failure = %q, want failed", state)
	}
	backlog, err := store.BranchRefBacklog(ctx)
	if err != nil {
		t.Fatalf("BranchRefBacklog: %v", err)
	}
	found := false
	for _, p := range backlog {
		if p.BranchID == branch.ID {
			found = true
		}
	}
	if !found {
		t.Error("failed branch missing from the backlog — no sweep could ever re-attempt it")
	}

	// Recovery: the next attempt runs the callback again and lands synced.
	synced, skipped, err := store.SyncBranchRef(ctx, branch.ID, func(gitprovider.PendingBranchRef) (*gitprovider.BranchRefRecord, error) {
		return &gitprovider.BranchRefRecord{BranchID: branch.ID, HeadSHA: refTestHeadSHA}, nil
	})
	if err != nil || !synced || skipped {
		t.Fatalf("recovery SyncBranchRef = synced %v skipped %v err %v", synced, skipped, err)
	}
	_, state, _, _, _ = fx.mappingRow(t, ctx, branch.ID)
	if state != "synced" {
		t.Errorf("sync_state after recovery = %q, want synced", state)
	}
}

// TestBranchRefCloseRecordsHeadAndCloses: closing the semantic lifecycle
// (merge and abort are the two terminal paths) moves the row to 'closing'
// with the close direction in the backlog; the sync commits 'closed' with
// the final head recorded, and a redelivered job skips (closed is
// terminal, migration 00031's guard enforces it for ANY update path).
func TestBranchRefCloseRecordsHeadAndCloses(t *testing.T) {
	ctx := testCtx(t)
	fx := newBranchRefFixture(t, ctx, "")
	fx.provision(t, ctx)

	for _, tc := range []struct {
		name   string
		closer func(t *testing.T, ctx context.Context, branch domain.Branch)
	}{
		{"merge", func(t *testing.T, ctx context.Context, branch domain.Branch) {
			if _, err := fx.branches.Merge(ctx, fx.project.ID, branch.ID); err != nil {
				t.Fatalf("Merge: %v", err)
			}
		}},
		{"abort", func(t *testing.T, ctx context.Context, branch domain.Branch) {
			if _, err := fx.branches.Abort(ctx, fx.project.ID, branch.ID); err != nil {
				t.Fatalf("Abort: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			branch := fx.createBranch(t, ctx, "feature-"+tc.name)
			tc.closer(t, ctx, branch)

			gitRef, state, _, _, closeRequested := fx.mappingRow(t, ctx, branch.ID)
			if gitRef != "refs/heads/feature-"+tc.name || state != "closing" || !closeRequested {
				t.Fatalf("mapping row after close = %s/%s (requested %v), want refs/heads/feature-%s/closing with close_requested_at",
					gitRef, state, closeRequested, tc.name)
			}

			store := gitprovider.NewBranchRefStore(fx.pool)
			backlog, err := store.BranchRefBacklog(ctx)
			if err != nil {
				t.Fatalf("BranchRefBacklog: %v", err)
			}
			var closeItem *gitprovider.PendingBranchRef
			for i := range backlog {
				if backlog[i].BranchID == branch.ID {
					closeItem = &backlog[i]
				}
			}
			if closeItem == nil || !closeItem.Close {
				t.Fatal("closed branch missing from the backlog (or not a close direction)")
			}

			synced, skipped, err := store.SyncBranchRef(ctx, branch.ID, func(p gitprovider.PendingBranchRef) (*gitprovider.BranchRefRecord, error) {
				if !p.Close {
					t.Error("close work item has Close=false — the syncer would CREATE the ref")
				}
				return &gitprovider.BranchRefRecord{BranchID: p.BranchID, HeadSHA: refTestHeadSHA}, nil
			})
			if err != nil || !synced || skipped {
				t.Fatalf("close SyncBranchRef = synced %v skipped %v err %v", synced, skipped, err)
			}
			_, state, _, headSHA, _ := fx.mappingRow(t, ctx, branch.ID)
			if state != "closed" || headSHA != refTestHeadSHA {
				t.Errorf("mapping row after sync = %s (head %q), want closed with the final head recorded", state, headSHA)
			}

			// Redelivery after close: skipped, no provider work.
			synced, skipped, err = store.SyncBranchRef(ctx, branch.ID, func(gitprovider.PendingBranchRef) (*gitprovider.BranchRefRecord, error) {
				t.Error("closed row's redelivery ran the provider callback")
				return nil, errors.New("must not run")
			})
			if err != nil || synced || !skipped {
				t.Errorf("redelivered close = synced %v skipped %v err %v, want skipped", synced, skipped, err)
			}
		})
	}
}

// TestBranchRefSyncSkipsUnknownBranch: a job for a branch with no backlog
// row (deleted, unmapped, or unprovisioned — indistinguishable and all
// "nothing to sync") is a silent skip, not an error.
func TestBranchRefSyncSkipsUnknownBranch(t *testing.T) {
	ctx := testCtx(t)
	fx := newBranchRefFixture(t, ctx, "")
	store := gitprovider.NewBranchRefStore(fx.pool)

	synced, skipped, err := store.SyncBranchRef(ctx, "11111111-2222-4333-8444-555555555555", func(gitprovider.PendingBranchRef) (*gitprovider.BranchRefRecord, error) {
		t.Error("unknown branch ran the provider callback")
		return nil, errors.New("must not run")
	})
	if err != nil || synced || !skipped {
		t.Fatalf("SyncBranchRef (unknown) = synced %v skipped %v err %v, want skipped", synced, skipped, err)
	}
}
