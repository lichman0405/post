package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/sqlc"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

// Task T0204: Project State 与 State Commit — the transaction boundary
// that turns semantic writes into traceable RSG state transitions, over a
// REAL PostgreSQL. Proves the four requirements and both acceptance
// criteria:
//
//   - state snapshots/projections: the branch head pointer advances with
//     every commit (branches.base_state_id, docs/21 §5), and a state's
//     direct members are enumerable from its id (ListStateObjectVersions /
//     ListStateRelationVersions, indexes from migration 00026);
//   - state commit actor/via/message/operations: every commit row carries
//     all four, and the operation summary round-trips as canonical JSON;
//   - parent state: result states chain through parent_state_id /
//     base_state_id, and a commit built on a superseded head fails with
//     *states.StateConflictError (BRANCH_STATE_CONFLICT);
//   - transaction boundary: the state insert, the head compare-and-swap,
//     the caller's semantic writes and the commit row are one transaction
//     — a failing callback or a lost CAS leaves no state, no commit, none
//     of the callback's rows, and an unmoved head (no half state).
//
// Branches are seeded with direct SQL: the branch domain is the parallel
// T0205 delivery, and this task must stay verifiable on its own baseline.
const stateTaskID = "T0204"

// stateFixture seeds alice, an organization, a project with its genesis
// state (through the service — the genesis root is part of this task) and
// one branch per helper call. The raw pool is kept for direct-SQL probes
// against the same database the store writes.
type stateFixture struct {
	service *states.Service
	pool    *pgxpool.Pool
	alice   domain.User
	project domain.Project
	genesis domain.ProjectState
	branch  string
}

func newStateFixture(t *testing.T, ctx context.Context) *stateFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), stateTaskID)
	alice, err := persistence.NewCredentialStore(pool).CreateWithPassword(
		ctx, "state-alice@example.com", "hash", "state-alice", "Alice")
	if err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	org, _, err := persistence.NewOrgStore(pool).CreateOrganization(ctx, domain.Organization{
		Slug: "state-fixture", Name: "State Fixture",
	}, alice.ID, todayUTC())
	if err != nil {
		t.Fatalf("create fixture org: %v", err)
	}
	orgID := org.ID
	project, _, err := persistence.NewProjectStore(pool).CreateProject(ctx, domain.Project{
		OrganizationID:  &orgID,
		Slug:            "state-project",
		Name:            "State Project",
		Purpose:         "fixture purpose",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, alice.ID)
	if err != nil {
		t.Fatalf("create fixture project: %v", err)
	}
	svc := states.NewService(persistence.NewStateStore(pool))
	genesis, err := svc.CreateInitialState(ctx, states.CreateInitialStateParams{
		ProjectID:       project.ID,
		ManifestVersion: "v1",
	})
	if err != nil {
		t.Fatalf("create genesis state: %v", err)
	}
	if genesis.ParentStateID != nil || genesis.BranchID != nil {
		t.Fatalf("genesis has parent %v / branch %v, want both nil", genesis.ParentStateID, genesis.BranchID)
	}
	f := &stateFixture{
		service: svc,
		pool:    pool,
		alice:   alice,
		project: project,
		genesis: genesis,
	}
	f.branch = f.newBranch(t, ctx, genesis.ID)
	return f
}

// newBranch seeds a branch with the given base state via direct SQL (the
// branch domain belongs to T0205).
func (f *stateFixture) newBranch(t *testing.T, ctx context.Context, baseStateID string) string {
	t.Helper()
	var branchID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO branches (project_id, name, visibility, git_ref, base_state_id, created_by)
		VALUES ($1, $2, 'private', $3, $4, $5) RETURNING id`,
		f.project.ID, "main", "refs/heads/main", baseStateID, f.alice.ID).Scan(&branchID); err != nil {
		t.Fatalf("seed branch: %v", err)
	}
	return branchID
}

// memberCounts returns the raw row counts the no-half-state probes compare
// before/after: states, commits, object versions and relation versions.
func (f *stateFixture) memberCounts(t *testing.T, ctx context.Context) (states, commits, objectVersions, relationVersions int) {
	t.Helper()
	for _, q := range []struct {
		sql   string
		count *int
	}{
		{"SELECT count(*) FROM project_states", &states},
		{"SELECT count(*) FROM state_commits", &commits},
		{"SELECT count(*) FROM scientific_object_versions", &objectVersions},
		{"SELECT count(*) FROM relation_versions", &relationVersions},
	} {
		if err := f.pool.QueryRow(ctx, q.sql).Scan(q.count); err != nil {
			t.Fatalf("count rows: %v", err)
		}
	}
	return
}

// writeObjectVersion is a WriteFunc that writes one scientific object +
// version 1 inside the commit transaction — the same shape T0208's domain
// services will produce. It proves states.Transaction is the sqlc write
// surface (assignable to the generated query runner).
func (f *stateFixture) writeObjectVersion(title string, record *string) states.WriteFunc {
	return func(ctx context.Context, tx states.Transaction, stateID string) error {
		q := sqlc.New(tx)
		stateUUID := parseUUIDOrDie(stateID)
		obj, err := q.CreateScientificObject(ctx, sqlc.CreateScientificObjectParams{
			ProjectID:  parseUUIDOrDie(f.project.ID),
			ObjectType: "hypothesis",
			CreatedBy:  parseUUIDOrDie(f.alice.ID),
		})
		if err != nil {
			return err
		}
		version, err := q.CreateScientificObjectVersion(ctx, sqlc.CreateScientificObjectVersionParams{
			ObjectID:       obj.ID,
			VersionNo:      1,
			StateID:        stateUUID,
			SchemaID:       "https://open-rd.example/schemas/hypothesis.schema.json",
			SchemaVersion:  "1",
			Title:          title,
			LifecycleState: "active",
			Payload:        []byte(`{"statement":"probe"}`),
			IntegrityHash:  integrityHashOf(`{"statement":"probe"}`),
			CreatedBy:      parseUUIDOrDie(f.alice.ID),
		})
		if err != nil {
			return err
		}
		if record != nil {
			*record = pgUUIDTextTest(version.ID)
		}
		return nil
	}
}

// TestStateCommitCreatesTraceableTransition proves the first acceptance
// criterion end to end: one semantic write yields exactly one state
// transition, and every fact of that transition — actor, via, message,
// operations, base, result, members — is readable back.
func TestStateCommitCreatesTraceableTransition(t *testing.T) {
	ctx := testCtx(t)
	f := newStateFixture(t, ctx)

	var writtenVersion string
	// The operation summary is built before the write runs, so the object
	// id does not exist yet; the callback records the real one and the
	// traceability assertions below read the stored summary back.
	state, commit, err := f.service.Commit(ctx, states.CommitParams{
		ProjectID: f.project.ID,
		BranchID:  f.branch,
		ActorID:   f.alice.ID,
		Via:       domain.ViaWeb,
		Message:   "propose the first hypothesis",
		Operations: []domain.StateOperation{
			{Kind: domain.OperationObjectVersionCreated, EntityID: "object-created-in-callback", VersionNo: 1},
		},
		BaseStateID:     &f.genesis.ID,
		ManifestVersion: "v1",
	}, f.writeObjectVersion("First hypothesis", &writtenVersion))
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if writtenVersion == "" {
		t.Fatal("callback did not write a version row")
	}

	// The result state is the traceable node: parent, branch, content hash.
	if state.ParentStateID == nil || *state.ParentStateID != f.genesis.ID {
		t.Errorf("state parent = %v, want genesis %s", state.ParentStateID, f.genesis.ID)
	}
	if state.BranchID == nil || *state.BranchID != f.branch {
		t.Errorf("state branch = %v, want %s", state.BranchID, f.branch)
	}
	wantHash, err := domain.ComputeStateHash(&f.genesis.ID, commitOperations(t, commit))
	if err != nil {
		t.Fatalf("ComputeStateHash: %v", err)
	}
	if state.StateHash != wantHash {
		t.Errorf("state hash = %q, want %q", state.StateHash, wantHash)
	}

	// The commit record carries actor/via/message/operations and the
	// base → result edge.
	if commit.ActorID != f.alice.ID {
		t.Errorf("commit actor = %q, want %q", commit.ActorID, f.alice.ID)
	}
	if commit.Via != domain.ViaWeb {
		t.Errorf("commit via = %q, want web", commit.Via)
	}
	if commit.Message != "propose the first hypothesis" {
		t.Errorf("commit message = %q", commit.Message)
	}
	if commit.BaseStateID == nil || *commit.BaseStateID != f.genesis.ID {
		t.Errorf("commit base = %v, want genesis %s", commit.BaseStateID, f.genesis.ID)
	}
	if commit.ResultStateID != state.ID {
		t.Errorf("commit result = %q, want state %q", commit.ResultStateID, state.ID)
	}

	// The branch head projection advanced to the new state.
	head, err := f.service.GetBranchHead(ctx, f.branch)
	if err != nil {
		t.Fatalf("GetBranchHead: %v", err)
	}
	if head.ID != state.ID {
		t.Errorf("branch head = %q, want %q", head.ID, state.ID)
	}

	// The commit is readable back with its operation summary intact.
	got, err := f.service.GetCommit(ctx, commit.ID)
	if err != nil {
		t.Fatalf("GetCommit: %v", err)
	}
	if got.ID != commit.ID || got.Message != commit.Message {
		t.Errorf("GetCommit roundtrip mismatch: %+v vs %+v", got, commit)
	}

	// The snapshot projection lists the rows the transition created, and
	// each member names the state it belongs to.
	versions, err := f.service.ListStateObjectVersions(ctx, state.ID)
	if err != nil {
		t.Fatalf("ListStateObjectVersions: %v", err)
	}
	if len(versions) != 1 || versions[0].ID != writtenVersion {
		t.Fatalf("state object versions = %+v, want exactly the callback's version %s", versions, writtenVersion)
	}
	if versions[0].StateID != state.ID {
		t.Errorf("member StateID = %q, want the result state %q", versions[0].StateID, state.ID)
	}

	// The state chain on the branch contains exactly the committed state
	// (the genesis has no branch).
	chain, err := f.service.ListStates(ctx, f.branch)
	if err != nil {
		t.Fatalf("ListStates: %v", err)
	}
	if len(chain) != 1 || chain[0].ID != state.ID {
		t.Errorf("branch state chain = %+v, want [%s]", chain, state.ID)
	}
	commits, err := f.service.ListCommits(ctx, f.branch)
	if err != nil {
		t.Fatalf("ListCommits: %v", err)
	}
	if len(commits) != 1 || commits[0].ID != commit.ID {
		t.Errorf("branch commits = %+v, want [%s]", commits, commit.ID)
	}

	// A second commit chains on the first: parent = previous result, and
	// the chain/commit lists grow oldest-first.
	state2, commit2, err := f.service.Commit(ctx, states.CommitParams{
		ProjectID: f.project.ID,
		BranchID:  f.branch,
		ActorID:   f.alice.ID,
		Via:       domain.ViaAPI,
		Message:   "add the second hypothesis",
		Operations: []domain.StateOperation{
			{Kind: domain.OperationObjectVersionCreated, EntityID: "obj-2", VersionNo: 1},
		},
		BaseStateID:     &state.ID,
		ManifestVersion: "v1",
	}, f.writeObjectVersion("Second hypothesis", nil))
	if err != nil {
		t.Fatalf("second Commit: %v", err)
	}
	if state2.ParentStateID == nil || *state2.ParentStateID != state.ID {
		t.Errorf("second state parent = %v, want first state %s", state2.ParentStateID, state.ID)
	}
	head, err = f.service.GetBranchHead(ctx, f.branch)
	if err != nil {
		t.Fatalf("GetBranchHead after second commit: %v", err)
	}
	if head.ID != state2.ID {
		t.Errorf("branch head = %q, want %q", head.ID, state2.ID)
	}
	chain, err = f.service.ListStates(ctx, f.branch)
	if err != nil {
		t.Fatalf("ListStates: %v", err)
	}
	if len(chain) != 2 || chain[0].ID != state.ID || chain[1].ID != state2.ID {
		t.Errorf("chain after second commit = %v", stateIDs(chain))
	}
	commits, err = f.service.ListCommits(ctx, f.branch)
	if err != nil {
		t.Fatalf("ListCommits: %v", err)
	}
	if len(commits) != 2 || commits[0].ID != commit.ID || commits[1].ID != commit2.ID {
		t.Errorf("commits after second commit = %v", commitIDs(commits))
	}
	// The second state's snapshot holds exactly its own member, not the
	// first state's.
	v2, err := f.service.ListStateObjectVersions(ctx, state2.ID)
	if err != nil {
		t.Fatalf("ListStateObjectVersions(second): %v", err)
	}
	if len(v2) != 1 || v2[0].Title != "Second hypothesis" {
		t.Errorf("second state members = %+v, want exactly the second version", v2)
	}
}

// TestStateCommitFailedTransactionLeavesNoHalfState proves the second
// acceptance criterion: a callback failure rolls the whole transition
// back — no state, no commit, none of the callback's rows, unmoved head.
func TestStateCommitFailedTransactionLeavesNoHalfState(t *testing.T) {
	ctx := testCtx(t)
	f := newStateFixture(t, ctx)
	beforeStates, beforeCommits, beforeObjects, beforeRelations := f.memberCounts(t, ctx)

	boom := errors.New("payload rejected by the domain service")
	_, _, err := f.service.Commit(ctx, states.CommitParams{
		ProjectID: f.project.ID,
		BranchID:  f.branch,
		ActorID:   f.alice.ID,
		Via:       domain.ViaAPI,
		Message:   "this transition must not land",
		Operations: []domain.StateOperation{
			{Kind: domain.OperationObjectVersionCreated, EntityID: "obj", VersionNo: 1},
		},
		BaseStateID:     &f.genesis.ID,
		ManifestVersion: "v1",
	}, func(ctx context.Context, tx states.Transaction, stateID string) error {
		// Write a row FIRST — the rollback must take it back too.
		if err := f.writeObjectVersion("half-written", nil)(ctx, tx, stateID); err != nil {
			return err
		}
		return boom
	})
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("Commit error = %v, want the callback's own error", err)
	}

	afterStates, afterCommits, afterObjects, afterRelations := f.memberCounts(t, ctx)
	if afterStates != beforeStates || afterCommits != beforeCommits ||
		afterObjects != beforeObjects || afterRelations != beforeRelations {
		t.Fatalf("failed commit left rows behind: states %d→%d, commits %d→%d, object versions %d→%d, relation versions %d→%d",
			beforeStates, afterStates, beforeCommits, afterCommits, beforeObjects, afterObjects, beforeRelations, afterRelations)
	}
	head, err := f.service.GetBranchHead(ctx, f.branch)
	if err != nil {
		t.Fatalf("GetBranchHead: %v", err)
	}
	if head.ID != f.genesis.ID {
		t.Errorf("branch head = %q after failed commit, want the untouched genesis %q", head.ID, f.genesis.ID)
	}
}

// TestStateCommitHeadConflict proves the compare-and-swap: a commit built
// on a superseded head fails with the stable BRANCH_STATE_CONFLICT and
// writes nothing — the branch chain stays linear.
func TestStateCommitHeadConflict(t *testing.T) {
	ctx := testCtx(t)
	f := newStateFixture(t, ctx)

	winner, _, err := f.service.Commit(ctx, states.CommitParams{
		ProjectID: f.project.ID,
		BranchID:  f.branch,
		ActorID:   f.alice.ID,
		Via:       domain.ViaSystem,
		Message:   "the winning transition",
		Operations: []domain.StateOperation{
			{Kind: domain.OperationObjectVersionCreated, EntityID: "obj-a", VersionNo: 1},
		},
		BaseStateID:     &f.genesis.ID,
		ManifestVersion: "v1",
	}, f.writeObjectVersion("Winner", nil))
	if err != nil {
		t.Fatalf("winning Commit: %v", err)
	}

	beforeStates, beforeCommits, beforeObjects, _ := f.memberCounts(t, ctx)
	_, _, err = f.service.Commit(ctx, states.CommitParams{
		ProjectID: f.project.ID,
		BranchID:  f.branch,
		ActorID:   f.alice.ID,
		Via:       domain.ViaSystem,
		Message:   "the losing transition",
		Operations: []domain.StateOperation{
			{Kind: domain.OperationObjectVersionCreated, EntityID: "obj-b", VersionNo: 1},
		},
		BaseStateID:     &f.genesis.ID, // stale: the winner moved the head
		ManifestVersion: "v1",
	}, f.writeObjectVersion("Loser", nil))
	var conflict *states.StateConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("losing Commit error = %v, want *states.StateConflictError", err)
	}
	if conflict.BranchID != f.branch {
		t.Errorf("conflict branch = %q, want %q", conflict.BranchID, f.branch)
	}
	if conflict.Expected == nil || *conflict.Expected != f.genesis.ID {
		t.Errorf("conflict expected = %v, want genesis %s", conflict.Expected, f.genesis.ID)
	}
	if conflict.Actual == nil || *conflict.Actual != winner.ID {
		t.Errorf("conflict actual = %v, want the winner's state %s", conflict.Actual, winner.ID)
	}
	if conflict.Code() != "BRANCH_STATE_CONFLICT" {
		t.Errorf("conflict code = %q, want BRANCH_STATE_CONFLICT", conflict.Code())
	}
	afterStates, afterCommits, afterObjects, _ := f.memberCounts(t, ctx)
	if afterStates != beforeStates || afterCommits != beforeCommits || afterObjects != beforeObjects {
		t.Fatalf("losing commit wrote rows: states %d→%d, commits %d→%d, objects %d→%d",
			beforeStates, afterStates, beforeCommits, afterCommits, beforeObjects, afterObjects)
	}
	head, err := f.service.GetBranchHead(ctx, f.branch)
	if err != nil {
		t.Fatalf("GetBranchHead: %v", err)
	}
	if head.ID != winner.ID {
		t.Errorf("branch head = %q after the losing commit, want the winner %q", head.ID, winner.ID)
	}
}

// TestStateCommitContentAddressCollision: the identical transition
// (same parent, same operations) derives the same state hash, collides on
// UNIQUE(project_id, state_hash) and is reported as ErrStateExists with
// nothing written — a retried transition can never duplicate a state.
func TestStateCommitContentAddressCollision(t *testing.T) {
	ctx := testCtx(t)
	f := newStateFixture(t, ctx)
	ops := []domain.StateOperation{
		{Kind: domain.OperationBlobAttached, EntityID: "11111111-2222-3333-4444-555555555555"},
	}
	noop := func(context.Context, states.Transaction, string) error { return nil }
	first, _, err := f.service.Commit(ctx, states.CommitParams{
		ProjectID:       f.project.ID,
		BranchID:        f.branch,
		ActorID:         f.alice.ID,
		Via:             domain.ViaGitCompat,
		Message:         "attach blob",
		Operations:      ops,
		BaseStateID:     &f.genesis.ID,
		ManifestVersion: "v1",
	}, noop)
	if err != nil {
		t.Fatalf("first Commit: %v", err)
	}
	// Advance the head is impossible here (the collision fires on the
	// state insert before the CAS), so a second, IDENTICAL transition —
	// same base, same operations — from the current head is the retry
	// case.
	head, err := f.service.GetBranchHead(ctx, f.branch)
	if err != nil {
		t.Fatalf("GetBranchHead: %v", err)
	}
	if head.ID != first.ID {
		t.Fatalf("head = %q, want %q", head.ID, first.ID)
	}
	// An identical transition from the FIRST base already collides: it is
	// the same (parent, operations) content, committed or not.
	beforeStates, beforeCommits, _, _ := f.memberCounts(t, ctx)
	_, _, err = f.service.Commit(ctx, states.CommitParams{
		ProjectID:       f.project.ID,
		BranchID:        f.branch,
		ActorID:         f.alice.ID,
		Via:             domain.ViaGitCompat,
		Message:         "attach blob",
		Operations:      ops,
		BaseStateID:     &f.genesis.ID,
		ManifestVersion: "v1",
	}, noop)
	if !errors.Is(err, states.ErrStateExists) {
		t.Fatalf("identical Commit error = %v, want ErrStateExists", err)
	}
	afterStates, afterCommits, _, _ := f.memberCounts(t, ctx)
	if afterStates != beforeStates || afterCommits != beforeCommits {
		t.Fatalf("colliding commit wrote rows: states %d→%d, commits %d→%d", beforeStates, afterStates, beforeCommits, afterCommits)
	}
	// The original state is still readable by its hash.
	byHash, err := f.service.GetStateByHash(ctx, f.project.ID, first.StateHash)
	if err != nil {
		t.Fatalf("GetStateByHash: %v", err)
	}
	if byHash.ID != first.ID {
		t.Errorf("GetStateByHash = %q, want %q", byHash.ID, first.ID)
	}
}

// TestStateCommitUnknownBranch: a commit naming a missing branch — or a
// branch of ANOTHER project, which reports the same outcome without
// leaking the foreign entity — fails with ErrBranchNotFound and writes
// nothing.
func TestStateCommitUnknownBranch(t *testing.T) {
	ctx := testCtx(t)
	f := newStateFixture(t, ctx)

	// A second project with its own branch: project A must not be able to
	// commit on project B's branch.
	other, _, err := persistence.NewProjectStore(f.pool).CreateProject(ctx, domain.Project{
		OrganizationID:  f.project.OrganizationID,
		Slug:            "state-other-project",
		Name:            "Other Project",
		Purpose:         "fixture purpose",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, f.alice.ID)
	if err != nil {
		t.Fatalf("create other project: %v", err)
	}
	var otherBranch string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO branches (project_id, name, visibility, git_ref, base_state_id, created_by)
		VALUES ($1, 'main', 'private', 'refs/heads/main', $2, $3) RETURNING id`,
		other.ID, f.genesis.ID, f.alice.ID).Scan(&otherBranch); err != nil {
		t.Fatalf("seed other branch: %v", err)
	}

	beforeStates, beforeCommits, _, _ := f.memberCounts(t, ctx)
	for _, tc := range []struct {
		name     string
		branchID string
	}{
		{"missing branch", "99999999-9999-9999-9999-999999999999"},
		{"foreign project's branch", otherBranch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := f.service.Commit(ctx, states.CommitParams{
				ProjectID: f.project.ID,
				BranchID:  tc.branchID,
				ActorID:   f.alice.ID,
				Via:       domain.ViaAPI,
				Message:   "must not land",
				Operations: []domain.StateOperation{
					{Kind: domain.OperationObjectVersionCreated, EntityID: "obj", VersionNo: 1},
				},
				BaseStateID:     &f.genesis.ID,
				ManifestVersion: "v1",
			}, func(context.Context, states.Transaction, string) error { return nil })
			if !errors.Is(err, states.ErrBranchNotFound) {
				t.Fatalf("Commit error = %v, want ErrBranchNotFound", err)
			}
		})
	}
	afterStates, afterCommits, _, _ := f.memberCounts(t, ctx)
	if afterStates != beforeStates || afterCommits != beforeCommits {
		t.Fatalf("unknown-branch commits wrote rows: states %d→%d, commits %d→%d", beforeStates, afterStates, beforeCommits, afterCommits)
	}
}

// TestCreateInitialState: the genesis root is unique per project by its
// content hash, and a branch without a head reports ErrStateNotFound.
func TestCreateInitialState(t *testing.T) {
	ctx := testCtx(t)
	f := newStateFixture(t, ctx)

	if _, err := f.service.CreateInitialState(ctx, states.CreateInitialStateParams{
		ProjectID:       f.project.ID,
		ManifestVersion: "v1",
	}); !errors.Is(err, states.ErrStateExists) {
		t.Fatalf("second genesis error = %v, want ErrStateExists", err)
	}

	// A genesis for a project that does not exist is a validation outcome
	// (the FK rejects it), never a store failure — and writes nothing.
	if _, err := f.service.CreateInitialState(ctx, states.CreateInitialStateParams{
		ProjectID:       "99999999-9999-9999-9999-999999999999",
		ManifestVersion: "v1",
	}); !errors.Is(err, states.ErrValidation) {
		t.Fatalf("unknown-project genesis error = %v, want ErrValidation", err)
	}

	// A branch with no head yet: GetBranchHead is ErrStateNotFound.
	var headless string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO branches (project_id, name, visibility, git_ref, created_by)
		VALUES ($1, 'headless', 'private', 'refs/heads/headless', $2) RETURNING id`,
		f.project.ID, f.alice.ID).Scan(&headless); err != nil {
		t.Fatalf("seed headless branch: %v", err)
	}
	if _, err := f.service.GetBranchHead(ctx, headless); !errors.Is(err, states.ErrStateNotFound) {
		t.Fatalf("GetBranchHead on headless branch = %v, want ErrStateNotFound", err)
	}

	// A first commit on the headless branch succeeds from a nil base: the
	// CAS matches "no head yet" and the chain starts.
	state, commit, err := f.service.Commit(ctx, states.CommitParams{
		ProjectID: f.project.ID,
		BranchID:  headless,
		ActorID:   f.alice.ID,
		Via:       domain.ViaSystem,
		Message:   "first transition on the headless branch",
		Operations: []domain.StateOperation{
			{Kind: domain.OperationObjectVersionCreated, EntityID: "obj-headless", VersionNo: 1},
		},
		ManifestVersion: "v1",
	}, f.writeObjectVersion("Headless first", nil))
	if err != nil {
		t.Fatalf("nil-base Commit: %v", err)
	}
	if commit.BaseStateID != nil {
		t.Errorf("commit base = %v, want nil (the branch had no head)", commit.BaseStateID)
	}
	if state.ParentStateID != nil {
		t.Errorf("state parent = %v, want nil", state.ParentStateID)
	}
	head, err := f.service.GetBranchHead(ctx, headless)
	if err != nil {
		t.Fatalf("GetBranchHead: %v", err)
	}
	if head.ID != state.ID {
		t.Errorf("head = %q, want %q", head.ID, state.ID)
	}
}

// TestStateCommitConcurrentWriters: two commits racing from the same base
// serialize through the head compare-and-swap into exactly one winner and
// one BRANCH_STATE_CONFLICT loser — never two heads, never a half-written
// loser.
func TestStateCommitConcurrentWriters(t *testing.T) {
	ctx := testCtx(t)
	f := newStateFixture(t, ctx)

	beforeStates, beforeCommits, beforeObjects, _ := f.memberCounts(t, ctx)
	type outcome struct {
		state  domain.ProjectState
		commit domain.StateCommit
		err    error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		go func(i int) {
			<-start
			state, commit, err := f.service.Commit(ctx, states.CommitParams{
				ProjectID: f.project.ID,
				BranchID:  f.branch,
				ActorID:   f.alice.ID,
				Via:       domain.ViaSystem,
				Message:   fmt.Sprintf("racing transition %d", i),
				Operations: []domain.StateOperation{
					{Kind: domain.OperationObjectVersionCreated, EntityID: fmt.Sprintf("obj-race-%d", i), VersionNo: 1},
				},
				BaseStateID:     &f.genesis.ID,
				ManifestVersion: "v1",
			}, f.writeObjectVersion(fmt.Sprintf("Racer %d", i), nil))
			results <- outcome{state, commit, err}
		}(i)
	}
	close(start)
	var winners, losers int
	var winnerID string
	for i := 0; i < 2; i++ {
		r := <-results
		switch {
		case r.err == nil:
			winners++
			winnerID = r.state.ID
		case errors.As(r.err, new(*states.StateConflictError)):
			losers++
		default:
			t.Errorf("racer %d: unexpected error %v", i, r.err)
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("concurrent commits: %d winners, %d conflict losers, want exactly 1/1", winners, losers)
	}
	afterStates, afterCommits, afterObjects, _ := f.memberCounts(t, ctx)
	if afterStates != beforeStates+1 || afterCommits != beforeCommits+1 || afterObjects != beforeObjects+1 {
		t.Fatalf("concurrent commits wrote %d states, %d commits, %d objects; want exactly one of each",
			afterStates-beforeStates, afterCommits-beforeCommits, afterObjects-beforeObjects)
	}
	head, err := f.service.GetBranchHead(ctx, f.branch)
	if err != nil {
		t.Fatalf("GetBranchHead: %v", err)
	}
	if head.ID != winnerID {
		t.Errorf("branch head = %q, want the winner %q", head.ID, winnerID)
	}
}

// commitOperations decodes a commit's operation summary back into the
// operation list — the traceability proof: what is stored is exactly what
// was committed, in order.
func commitOperations(t *testing.T, commit domain.StateCommit) []domain.StateOperation {
	t.Helper()
	var ops []domain.StateOperation
	if err := json.Unmarshal(commit.OperationSummary, &ops); err != nil {
		t.Fatalf("operation summary is not canonical JSON: %v (%s)", err, commit.OperationSummary)
	}
	return ops
}

func parseUUIDOrDie(s string) pgtype.UUID {
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		panic("invalid uuid in test fixture: " + err.Error())
	}
	return u
}

func stateIDs(states []domain.ProjectState) []string {
	out := make([]string, len(states))
	for i, s := range states {
		out[i] = s.ID
	}
	return out
}

func commitIDs(commits []domain.StateCommit) []string {
	out := make([]string, len(commits))
	for i, c := range commits {
		out[i] = c.ID
	}
	return out
}

// integrityHashOf mirrors the store-side hash discipline for seeded rows
// (sha256 of the canonical payload bytes).
func integrityHashOf(payload string) string {
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// pgUUIDTextTest renders a pgx uuid in the canonical 8-4-4-4-12 text form
// (the same rendering the persistence layer uses).
func pgUUIDTextTest(u pgtype.UUID) string {
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
