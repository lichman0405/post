package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

// Task T0205: Research Branch Domain — over a REAL PostgreSQL. Proves the
// four requirements and both acceptance criteria end to end:
//
//   - branch from base state: a branch forks a state of the same project
//     (a foreign or missing fork point is rejected, no existence leak);
//   - public/private visibility: new branches default to the project's
//     preset (docs/09 §1) and may never be more visible than it
//     (docs/12 §3);
//   - branch lifecycle: active → merged | aborted, terminal; main is
//     protected; closed branches accept no commits — enforced by the
//     commit compare-and-swap AND by the database trigger (migration
//     00028) on any update path;
//   - current head state: the branches.base_state_id projection (docs/21
//     §5) is readable and advances only through commits on that branch.
//
// The two required acceptance tests are TestBranchHeadEvolvesIndependently
// (branch head 可独立演化) and TestMainAndBranchStateDoNotMix (main 与
// branch state 不混淆).
const branchTaskID = "T0205"

// branchFixture seeds alice, an organization, a project with the given
// visibility preset and its genesis state (through the states service).
// Branch creation goes through the branches service — the surface under
// test — commits through the states service, exactly like the consumers.
type branchFixture struct {
	states   *states.Service
	branches *branches.Service
	pool     *pgxpool.Pool
	alice    domain.User
	project  domain.Project
	genesis  domain.ProjectState
}

func newBranchFixture(t *testing.T, ctx context.Context, visibility domain.ProjectVisibility) *branchFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), branchTaskID)
	alice, err := persistence.NewCredentialStore(pool).CreateWithPassword(
		ctx, "branch-alice@example.com", "hash", "branch-alice", "Alice")
	if err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	org, _, err := persistence.NewOrgStore(pool).CreateOrganization(ctx, domain.Organization{
		Slug: "branch-fixture", Name: "Branch Fixture",
	}, alice.ID, todayUTC())
	if err != nil {
		t.Fatalf("create fixture org: %v", err)
	}
	orgID := org.ID
	project, _, err := persistence.NewProjectStore(pool).CreateProject(ctx, domain.Project{
		OrganizationID:  &orgID,
		Slug:            "branch-project",
		Name:            "Branch Project",
		Purpose:         "fixture purpose",
		Visibility:      visibility,
		ProvisionStatus: domain.ProvisionPending,
	}, alice.ID)
	if err != nil {
		t.Fatalf("create fixture project: %v", err)
	}
	stateSvc := states.NewService(persistence.NewStateStore(pool))
	genesis, err := stateSvc.CreateInitialState(ctx, states.CreateInitialStateParams{
		ProjectID:       project.ID,
		ManifestVersion: "v1",
	})
	if err != nil {
		t.Fatalf("create genesis state: %v", err)
	}
	return &branchFixture{
		states:   stateSvc,
		branches: branches.NewService(persistence.NewBranchStore(pool)),
		pool:     pool,
		alice:    alice,
		project:  project,
		genesis:  genesis,
	}
}

// createBranch creates a branch through the service under test.
func (f *branchFixture) createBranch(t *testing.T, ctx context.Context, name, baseStateID string, visibility domain.BranchVisibility) domain.Branch {
	t.Helper()
	branch, err := f.branches.Create(ctx, branches.CreateBranchParams{
		ProjectID:   f.project.ID,
		Name:        name,
		Visibility:  visibility,
		BaseStateID: baseStateID,
		CreatedBy:   f.alice.ID,
	})
	if err != nil {
		t.Fatalf("create branch %s: %v", name, err)
	}
	return branch
}

// commit lands one state transition on the branch through the states
// service. The semantic write is a no-op: these tests are about branch
// semantics, not member rows (T0204 covers those).
func (f *branchFixture) commit(t *testing.T, ctx context.Context, branchID string, base *string, msg, entityID string) domain.ProjectState {
	t.Helper()
	state, _, err := f.states.Commit(ctx, states.CommitParams{
		ProjectID: f.project.ID,
		BranchID:  branchID,
		ActorID:   f.alice.ID,
		Via:       domain.ViaAPI,
		Message:   msg,
		Operations: []domain.StateOperation{
			{Kind: domain.OperationObjectVersionCreated, EntityID: entityID, VersionNo: 1},
		},
		BaseStateID:     base,
		ManifestVersion: "v1",
	}, func(context.Context, states.Transaction, string) error { return nil })
	if err != nil {
		t.Fatalf("commit %q on branch %s: %v", msg, branchID, err)
	}
	return state
}

// head returns the branch's current head through the branches service.
func (f *branchFixture) head(t *testing.T, ctx context.Context, branchID string) domain.ProjectState {
	t.Helper()
	state, err := f.branches.GetHead(ctx, branchID)
	if err != nil {
		t.Fatalf("GetHead(%s): %v", branchID, err)
	}
	return state
}

// TestBranchHeadEvolvesIndependently is the acceptance test "branch head 可
// 独立演化": main's head and the branch's head are two independent
// pointers — commits on one never move the other, in either direction, at
// any time.
func TestBranchHeadEvolvesIndependently(t *testing.T) {
	ctx := testCtx(t)
	f := newBranchFixture(t, ctx, domain.VisibilityPublic)

	main := f.createBranch(t, ctx, "main", f.genesis.ID, "")
	c1 := f.commit(t, ctx, main.ID, &f.genesis.ID, "first main commit", "obj-c1")

	// The branch forks main's current head — the fork point itself stays
	// main's state.
	feature := f.createBranch(t, ctx, "feature/screening", c1.ID, "")
	if feature.BaseStateID == nil || *feature.BaseStateID != c1.ID {
		t.Fatalf("feature base = %v, want main's head %s", feature.BaseStateID, c1.ID)
	}
	f1 := f.commit(t, ctx, feature.ID, &c1.ID, "feature work 1", "obj-f1")

	// Main evolves; the feature head stays put.
	c2 := f.commit(t, ctx, main.ID, &c1.ID, "second main commit", "obj-c2")
	if got := f.head(t, ctx, main.ID); got.ID != c2.ID {
		t.Errorf("main head = %q, want %q", got.ID, c2.ID)
	}
	if got := f.head(t, ctx, feature.ID); got.ID != f1.ID {
		t.Errorf("feature head = %q after main commit, want untouched %q", got.ID, f1.ID)
	}
	if c2.ID == f1.ID {
		t.Fatal("main and feature heads collided — states are identical")
	}

	// The feature evolves; main's head stays put.
	f2 := f.commit(t, ctx, feature.ID, &f1.ID, "feature work 2", "obj-f2")
	if got := f.head(t, ctx, feature.ID); got.ID != f2.ID {
		t.Errorf("feature head = %q, want %q", got.ID, f2.ID)
	}
	if got := f.head(t, ctx, main.ID); got.ID != c2.ID {
		t.Errorf("main head = %q after feature commit, want untouched %q", got.ID, c2.ID)
	}

	// And again the other way — interleaved evolution, never a shared
	// pointer.
	c3 := f.commit(t, ctx, main.ID, &c2.ID, "third main commit", "obj-c3")
	if got := f.head(t, ctx, main.ID); got.ID != c3.ID {
		t.Errorf("main head = %q, want %q", got.ID, c3.ID)
	}
	if got := f.head(t, ctx, feature.ID); got.ID != f2.ID {
		t.Errorf("feature head = %q after main commit, want untouched %q", got.ID, f2.ID)
	}

	// The head states themselves remember their branch: c2 is main's, f1
	// is the feature's — the pointers and the states agree.
	b2 := f.head(t, ctx, feature.ID)
	if b2.BranchID == nil || *b2.BranchID != feature.ID {
		t.Errorf("feature head state's branch = %v, want %s", b2.BranchID, feature.ID)
	}
	m3 := f.head(t, ctx, main.ID)
	if m3.BranchID == nil || *m3.BranchID != main.ID {
		t.Errorf("main head state's branch = %v, want %s", m3.BranchID, main.ID)
	}
}

// TestMainAndBranchStateDoNotMix is the acceptance test "main 与 branch
// state 不混淆": the state chain of main and the state chain of the branch
// are disjoint — the shared fork point stays visible only on the branch
// that committed it, and no later state of one chain appears in the other.
func TestMainAndBranchStateDoNotMix(t *testing.T) {
	ctx := testCtx(t)
	f := newBranchFixture(t, ctx, domain.VisibilityPublic)

	main := f.createBranch(t, ctx, "main", f.genesis.ID, "")
	c1 := f.commit(t, ctx, main.ID, &f.genesis.ID, "main commit 1", "obj-c1")
	feature := f.createBranch(t, ctx, "feature/exp2", c1.ID, "")
	f1 := f.commit(t, ctx, feature.ID, &c1.ID, "feature commit 1", "obj-f1")
	c2 := f.commit(t, ctx, main.ID, &c1.ID, "main commit 2", "obj-c2")

	// Main's chain contains exactly main's own states — and not the
	// feature's.
	mainChain, err := f.states.ListStates(ctx, main.ID)
	if err != nil {
		t.Fatalf("ListStates(main): %v", err)
	}
	if len(mainChain) != 2 || mainChain[0].ID != c1.ID || mainChain[1].ID != c2.ID {
		t.Errorf("main state chain = %v, want [%s %s] (no feature states)", stateIDs(mainChain), c1.ID, c2.ID)
	}
	mainCommits, err := f.states.ListCommits(ctx, main.ID)
	if err != nil {
		t.Fatalf("ListCommits(main): %v", err)
	}
	if len(mainCommits) != 2 || mainCommits[0].ResultStateID != c1.ID || mainCommits[1].ResultStateID != c2.ID {
		t.Errorf("main commit chain = %v, want the two main commits", commitIDs(mainCommits))
	}

	// The feature's chain contains exactly its own states: NOT the shared
	// fork point c1 (that state belongs to main) and NOT main's later c2.
	featureChain, err := f.states.ListStates(ctx, feature.ID)
	if err != nil {
		t.Fatalf("ListStates(feature): %v", err)
	}
	if len(featureChain) != 1 || featureChain[0].ID != f1.ID {
		t.Errorf("feature state chain = %v, want exactly [%s] — the fork point and main's later states must not appear", stateIDs(featureChain), f1.ID)
	}
	featureCommits, err := f.states.ListCommits(ctx, feature.ID)
	if err != nil {
		t.Fatalf("ListCommits(feature): %v", err)
	}
	if len(featureCommits) != 1 || featureCommits[0].ResultStateID != f1.ID {
		t.Errorf("feature commit chain = %v, want exactly the feature commit", commitIDs(featureCommits))
	}

	// The fork point is still traceable through the parent chain — the
	// feature's first state names c1 as its parent, the shared ancestor.
	if f1.ParentStateID == nil || *f1.ParentStateID != c1.ID {
		t.Errorf("feature first state parent = %v, want the fork point %s", f1.ParentStateID, c1.ID)
	}
	// The genesis belongs to no branch at all.
	if f.genesis.BranchID != nil {
		t.Errorf("genesis branch = %v, want nil", f.genesis.BranchID)
	}
}

// TestCreateBranchFromBaseState covers "branch from base state": the fork
// point becomes the branch's initial head, and only a state of the same
// project can be forked — a foreign or missing fork point is rejected
// without leaking which state exists where.
func TestCreateBranchFromBaseState(t *testing.T) {
	ctx := testCtx(t)
	f := newBranchFixture(t, ctx, domain.VisibilityPublic)

	branch := f.createBranch(t, ctx, "exp/from-genesis", f.genesis.ID, domain.BranchVisibilityPrivate)
	if branch.ProjectID != f.project.ID {
		t.Errorf("branch project = %q, want %q", branch.ProjectID, f.project.ID)
	}
	if branch.BaseStateID == nil || *branch.BaseStateID != f.genesis.ID {
		t.Errorf("branch base = %v, want the genesis %s", branch.BaseStateID, f.genesis.ID)
	}
	if branch.Lifecycle != domain.BranchLifecycleActive {
		t.Errorf("branch lifecycle = %q, want active", branch.Lifecycle)
	}
	if branch.GitRef != "refs/heads/exp/from-genesis" {
		t.Errorf("branch git ref = %q, want refs/heads/exp/from-genesis", branch.GitRef)
	}
	if branch.CreatedBy != f.alice.ID {
		t.Errorf("branch created_by = %q, want %q", branch.CreatedBy, f.alice.ID)
	}
	// The initial head is the fork point itself, readable both ways.
	if got := f.head(t, ctx, branch.ID); got.ID != f.genesis.ID {
		t.Errorf("branch head = %q, want the fork point %q", got.ID, f.genesis.ID)
	}
	got, err := f.branches.Get(ctx, f.project.ID, branch.ID)
	if err != nil {
		t.Fatalf("GetBranch: %v", err)
	}
	if got.ID != branch.ID {
		t.Errorf("GetBranch = %q, want %q", got.ID, branch.ID)
	}

	// A second project: its state is not forkable by this project.
	other, _, err := persistence.NewProjectStore(f.pool).CreateProject(ctx, domain.Project{
		OrganizationID:  f.project.OrganizationID,
		Slug:            "branch-other-project",
		Name:            "Other Project",
		Purpose:         "fixture purpose",
		Visibility:      domain.VisibilityPublic,
		ProvisionStatus: domain.ProvisionPending,
	}, f.alice.ID)
	if err != nil {
		t.Fatalf("create other project: %v", err)
	}
	otherGenesis, err := f.states.CreateInitialState(ctx, states.CreateInitialStateParams{
		ProjectID:       other.ID,
		ManifestVersion: "v1",
	})
	if err != nil {
		t.Fatalf("create other genesis: %v", err)
	}

	for _, tc := range []struct {
		name       string
		branchName string
		project    string
		base       string
		wantIs     error
	}{
		{"foreign project's state", "exp/foreign-project-state", f.project.ID, otherGenesis.ID, branches.ErrBaseStateNotFound},
		{"missing state", "exp/missing-state", f.project.ID, "99999999-9999-9999-9999-999999999999", branches.ErrBaseStateNotFound},
		{"unknown project", "exp/unknown-project", "88888888-8888-8888-8888-888888888888", f.genesis.ID, projects.ErrProjectNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.branches.Create(ctx, branches.CreateBranchParams{
				ProjectID:   tc.project,
				Name:        tc.branchName,
				BaseStateID: tc.base,
				CreatedBy:   f.alice.ID,
			})
			if !errors.Is(err, tc.wantIs) {
				t.Fatalf("Create error = %v, want %v", err, tc.wantIs)
			}
		})
	}

	// A duplicate name in the project is rejected.
	_, err = f.branches.Create(ctx, branches.CreateBranchParams{
		ProjectID:   f.project.ID,
		Name:        "exp/from-genesis",
		BaseStateID: f.genesis.ID,
		CreatedBy:   f.alice.ID,
	})
	if !errors.Is(err, branches.ErrBranchNameTaken) {
		t.Fatalf("duplicate-name Create error = %v, want ErrBranchNameTaken", err)
	}
}

// TestBranchVisibilityDefaultsAndLimits covers "public/private
// visibility": the default follows the project's preset (docs/09 §1) and
// an explicit value may never be more visible than the project (docs/12 §3
// — widening goes through the publication flow, never branch creation).
func TestBranchVisibilityDefaultsAndLimits(t *testing.T) {
	ctx := testCtx(t)

	t.Run("public project defaults public", func(t *testing.T) {
		f := newBranchFixture(t, ctx, domain.VisibilityPublic)
		branch := f.createBranch(t, ctx, "exp/default-public", f.genesis.ID, "")
		if branch.Visibility != domain.BranchVisibilityPublic {
			t.Errorf("visibility = %q, want public (project preset)", branch.Visibility)
		}
	})
	t.Run("public project may go private explicitly", func(t *testing.T) {
		f := newBranchFixture(t, ctx, domain.VisibilityPublic)
		branch := f.createBranch(t, ctx, "exp/explicit-private", f.genesis.ID, domain.BranchVisibilityPrivate)
		if branch.Visibility != domain.BranchVisibilityPrivate {
			t.Errorf("visibility = %q, want private (explicit)", branch.Visibility)
		}
	})
	t.Run("private project defaults private", func(t *testing.T) {
		f := newBranchFixture(t, ctx, domain.VisibilityPrivate)
		branch := f.createBranch(t, ctx, "exp/default-private", f.genesis.ID, "")
		if branch.Visibility != domain.BranchVisibilityPrivate {
			t.Errorf("visibility = %q, want private (project preset)", branch.Visibility)
		}
	})
	t.Run("private project cannot host a public branch", func(t *testing.T) {
		f := newBranchFixture(t, ctx, domain.VisibilityPrivate)
		_, err := f.branches.Create(ctx, branches.CreateBranchParams{
			ProjectID:   f.project.ID,
			Name:        "exp/explicit-public",
			Visibility:  domain.BranchVisibilityPublic,
			BaseStateID: f.genesis.ID,
			CreatedBy:   f.alice.ID,
		})
		if !errors.Is(err, branches.ErrPublicBranchInPrivateProject) {
			t.Fatalf("Create error = %v, want ErrPublicBranchInPrivateProject", err)
		}
		// Nothing was created.
		list, err := f.branches.List(ctx, f.project.ID)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) != 0 {
			t.Errorf("rejected create left branches behind: %+v", list)
		}
	})
}

// TestBranchLifecycle covers the "branch lifecycle" requirement: active →
// merged | aborted is terminal (docs/43), main is protected, closed
// branches accept no commits, and the closed branch's history — head
// pointer included — is immutable on every update path.
func TestBranchLifecycle(t *testing.T) {
	ctx := testCtx(t)
	f := newBranchFixture(t, ctx, domain.VisibilityPublic)

	main := f.createBranch(t, ctx, "main", f.genesis.ID, "")
	merged := f.createBranch(t, ctx, "exp/merged", f.genesis.ID, "")
	aborted := f.createBranch(t, ctx, "exp/aborted", f.genesis.ID, "")
	f.commit(t, ctx, merged.ID, &f.genesis.ID, "merged branch work", "obj-m")
	f.commit(t, ctx, aborted.ID, &f.genesis.ID, "aborted branch work", "obj-a")

	// Terminal transitions.
	got, err := f.branches.Merge(ctx, f.project.ID, merged.ID)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if got.Lifecycle != domain.BranchLifecycleMerged {
		t.Errorf("merged branch lifecycle = %q, want merged", got.Lifecycle)
	}
	got, err = f.branches.Abort(ctx, f.project.ID, aborted.ID)
	if err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if got.Lifecycle != domain.BranchLifecycleAborted {
		t.Errorf("aborted branch lifecycle = %q, want aborted", got.Lifecycle)
	}

	// main is protected in both directions.
	if _, err := f.branches.Merge(ctx, f.project.ID, main.ID); !errors.Is(err, branches.ErrMainProtected) {
		t.Errorf("Merge(main) error = %v, want ErrMainProtected", err)
	}
	if _, err := f.branches.Abort(ctx, f.project.ID, main.ID); !errors.Is(err, branches.ErrMainProtected) {
		t.Errorf("Abort(main) error = %v, want ErrMainProtected", err)
	}
	// main is still active and still accepts commits.
	f.commit(t, ctx, main.ID, &f.genesis.ID, "main keeps working", "obj-main")

	// Terminal states are terminal.
	if _, err := f.branches.Merge(ctx, f.project.ID, merged.ID); !errors.As(err, new(*branches.NotActiveError)) {
		t.Errorf("Merge(merged) error = %v, want *branches.NotActiveError", err)
	}
	if _, err := f.branches.Abort(ctx, f.project.ID, aborted.ID); !errors.As(err, new(*branches.NotActiveError)) {
		t.Errorf("Abort(aborted) error = %v, want *branches.NotActiveError", err)
	}

	// A branch of another project reports "not found" for lifecycle ops
	// too (never leak a foreign entity's existence).
	other, _, err := persistence.NewProjectStore(f.pool).CreateProject(ctx, domain.Project{
		OrganizationID:  f.project.OrganizationID,
		Slug:            "branch-lifecycle-other",
		Name:            "Other Project",
		Purpose:         "fixture purpose",
		Visibility:      domain.VisibilityPublic,
		ProvisionStatus: domain.ProvisionPending,
	}, f.alice.ID)
	if err != nil {
		t.Fatalf("create other project: %v", err)
	}
	otherGenesis, err := f.states.CreateInitialState(ctx, states.CreateInitialStateParams{
		ProjectID:       other.ID,
		ManifestVersion: "v1",
	})
	if err != nil {
		t.Fatalf("create other genesis: %v", err)
	}
	var otherBranch domain.Branch
	otherBranch, err = f.branches.Create(ctx, branches.CreateBranchParams{
		ProjectID:   other.ID,
		Name:        "exp/foreign",
		BaseStateID: otherGenesis.ID,
		CreatedBy:   f.alice.ID,
	})
	if err != nil {
		t.Fatalf("create other branch: %v", err)
	}
	if _, err := f.branches.Merge(ctx, f.project.ID, otherBranch.ID); !errors.Is(err, branches.ErrBranchNotFound) {
		t.Errorf("Merge(foreign) error = %v, want ErrBranchNotFound", err)
	}

	// Closed branches accept no commits — the head compare-and-swap now
	// requires an active lifecycle (BRANCH_NOT_ACTIVE), and nothing is
	// written.
	before := f.countRows(t, ctx)
	for _, tc := range []struct {
		name   string
		branch domain.Branch
	}{
		{"merged", merged},
		{"aborted", aborted},
	} {
		t.Run("commit on "+tc.name, func(t *testing.T) {
			_, _, err := f.states.Commit(ctx, states.CommitParams{
				ProjectID: f.project.ID,
				BranchID:  tc.branch.ID,
				ActorID:   f.alice.ID,
				Via:       domain.ViaAPI,
				Message:   "must not land",
				Operations: []domain.StateOperation{
					{Kind: domain.OperationObjectVersionCreated, EntityID: "obj-ghost", VersionNo: 1},
				},
				BaseStateID:     tc.branch.BaseStateID,
				ManifestVersion: "v1",
			}, func(context.Context, states.Transaction, string) error { return nil })
			var na *states.BranchNotActiveError
			if !errors.As(err, &na) {
				t.Fatalf("Commit error = %v, want *states.BranchNotActiveError", err)
			}
			if na.Code() != "BRANCH_NOT_ACTIVE" {
				t.Errorf("Commit error code = %q, want BRANCH_NOT_ACTIVE", na.Code())
			}
		})
	}
	after := f.countRows(t, ctx)
	if before != after {
		t.Fatalf("closed-branch commits wrote rows: counts %d → %d", before, after)
	}
	// The closed branches' heads are still readable — history is
	// immutable, not gone.
	if got := f.head(t, ctx, merged.ID); got.BranchID == nil || *got.BranchID != merged.ID {
		t.Errorf("merged head state branch = %v, want %s", got.BranchID, merged.ID)
	}
}

// TestBranchLifecycleDatabaseGuard proves migration 00028: even a direct
// SQL update — bypassing every service and store — cannot move a closed
// branch's head or lifecycle. The database itself rejects it.
func TestBranchLifecycleDatabaseGuard(t *testing.T) {
	ctx := testCtx(t)
	f := newBranchFixture(t, ctx, domain.VisibilityPublic)
	merged := f.createBranch(t, ctx, "exp/db-guard", f.genesis.ID, "")
	if _, err := f.branches.Merge(ctx, f.project.ID, merged.ID); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"head pointer must not move", "UPDATE branches SET base_state_id = NULL WHERE id = $1"},
		{"lifecycle must not move", "UPDATE branches SET lifecycle_state = 'active' WHERE id = $1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.pool.Exec(ctx, tc.sql, merged.ID)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "P0001" {
				t.Fatalf("direct update error = %v, want the P0001 lifecycle guard", err)
			}
		})
	}
	// The branch is still merged.
	got, err := f.branches.Get(ctx, f.project.ID, merged.ID)
	if err != nil {
		t.Fatalf("GetBranch: %v", err)
	}
	if got.Lifecycle != domain.BranchLifecycleMerged {
		t.Errorf("lifecycle after rejected updates = %q, want merged", got.Lifecycle)
	}
}

// TestBranchHeadReads covers the "current head state" requirement's read
// side: the head projection is readable per branch, unknown branches
// report not-found (including foreign ones), and listing returns the
// branches in creation order with their lifecycle intact.
func TestBranchHeadReads(t *testing.T) {
	ctx := testCtx(t)
	f := newBranchFixture(t, ctx, domain.VisibilityPublic)

	main := f.createBranch(t, ctx, "main", f.genesis.ID, "")
	first := f.createBranch(t, ctx, "exp/first", f.genesis.ID, "")
	second := f.createBranch(t, ctx, "exp/second", f.genesis.ID, "")
	if _, err := f.branches.Merge(ctx, f.project.ID, first.ID); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	list, err := f.branches.List(ctx, f.project.ID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 || list[0].ID != main.ID || list[1].ID != first.ID || list[2].ID != second.ID {
		t.Errorf("List = %+v, want main, exp/first, exp/second in creation order", list)
	}
	if list[1].Lifecycle != domain.BranchLifecycleMerged {
		t.Errorf("listed merged branch lifecycle = %q, want merged", list[1].Lifecycle)
	}

	// Unknown and foreign branch ids report the same not-found outcome.
	other, _, err := persistence.NewProjectStore(f.pool).CreateProject(ctx, domain.Project{
		OrganizationID:  f.project.OrganizationID,
		Slug:            "branch-read-other",
		Name:            "Other Project",
		Purpose:         "fixture purpose",
		Visibility:      domain.VisibilityPublic,
		ProvisionStatus: domain.ProvisionPending,
	}, f.alice.ID)
	if err != nil {
		t.Fatalf("create other project: %v", err)
	}
	otherGenesis, err := f.states.CreateInitialState(ctx, states.CreateInitialStateParams{
		ProjectID:       other.ID,
		ManifestVersion: "v1",
	})
	if err != nil {
		t.Fatalf("create other genesis: %v", err)
	}
	foreign, err := f.branches.Create(ctx, branches.CreateBranchParams{
		ProjectID:   other.ID,
		Name:        "exp/foreign",
		BaseStateID: otherGenesis.ID,
		CreatedBy:   f.alice.ID,
	})
	if err != nil {
		t.Fatalf("create foreign branch: %v", err)
	}
	if _, err := f.branches.Get(ctx, f.project.ID, foreign.ID); !errors.Is(err, branches.ErrBranchNotFound) {
		t.Errorf("Get(foreign) error = %v, want ErrBranchNotFound", err)
	}
	if _, err := f.branches.GetHead(ctx, "99999999-9999-9999-9999-999999999999"); !errors.Is(err, branches.ErrBranchNotFound) {
		t.Errorf("GetHead(unknown) error = %v, want ErrBranchNotFound", err)
	}
}

// countRows returns the total row count of the four state tables — the
// closed-branch commit probe compares it before/after.
func (f *branchFixture) countRows(t *testing.T, ctx context.Context) int {
	t.Helper()
	var total int
	for _, table := range []string{"project_states", "state_commits", "scientific_object_versions", "relation_versions"} {
		var n int
		if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		total += n
	}
	return total
}
