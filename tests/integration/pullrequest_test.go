package integration

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

// Task T0402: Pull Request Domain — over a REAL PostgreSQL, with the same
// store composition the API uses. Proves the three requirements and both
// acceptance criteria end to end:
//
//   - base/proposed state fixed + "PR base 不随 main 漂移": the base is
//     pinned to the target branch's head at creation and stays there when
//     main advances — and the database itself rejects any write path that
//     would move it (migration 00051 fixity guard, SQLSTATE asserted);
//   - head update 可显式 refresh: the proposed state moves ONLY through
//     the explicit refresh — the one path setting the transaction-scoped
//     flag the guard requires — and never on a terminal PR;
//   - open/close/review state: the docs/43 machine runs through the
//     service, illegal transitions are refused by the service AND by the
//     database trigger for raw writes;
//   - number per project: allocation is per-project and race-free
//     (serialized on the project row), a second project starts at #1.

const prTaskID = "T0402"

// prFixture seeds one private project owned by alice and wires the rsg
// service (the branch/commit write path) plus the pullrequests service
// over the real stores — the composition cmd/api/main.go uses.
type prFixture struct {
	prsvc     *pullrequests.Service
	svc       *rsg.Service
	statesSvc *states.Service
	branches  *branches.Service
	pool      *pgxpool.Pool
	alice     domain.User
	project   domain.Project
	main      domain.Branch
}

func newPRFixture(t *testing.T, ctx context.Context) *prFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), prTaskID)
	alice, err := persistence.NewCredentialStore(pool).CreateWithPassword(
		ctx, "pr-alice@example.com", "hash", "pr-alice", "Alice")
	if err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	orgStore := persistence.NewOrgStore(pool)
	org, _, err := orgStore.CreateOrganization(ctx, domain.Organization{
		Slug: "pr-fixture", Name: "PR Fixture",
	}, alice.ID, todayUTC())
	if err != nil {
		t.Fatalf("create fixture org: %v", err)
	}
	projectStore := persistence.NewProjectStore(pool)
	project, _, err := projectStore.CreateProject(ctx, domain.Project{
		OrganizationID:  &org.ID,
		Slug:            "pr-project",
		Name:            "PR Project",
		Purpose:         "fixture purpose",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, alice.ID)
	if err != nil {
		t.Fatalf("create fixture project: %v", err)
	}

	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	stateStore := persistence.NewStateStore(pool)
	statesSvc := states.NewService(stateStore, newCommitGuard(t))
	svc := rsg.NewService(rsg.Deps{
		Projects:  projects.NewService(projectStore, orgStore, authz.NewMatrixEngine()),
		Branches:  branches.NewService(persistence.NewBranchStore(pool)),
		States:    statesSvc,
		Latest:    stateStore,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
		Events:    events.Recorder{},
	})
	main, err := svc.CreateBranch(ctx, alice, project.ID, rsg.CreateBranchInput{
		Name:       "main",
		BaseRef:    "",
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	return &prFixture{
		prsvc:     pullrequests.NewService(persistence.NewPullRequestStore(pool)),
		svc:       svc,
		statesSvc: statesSvc,
		branches:  branches.NewService(persistence.NewBranchStore(pool)),
		pool:      pool,
		alice:     alice,
		project:   project,
		main:      main,
	}
}

// commit commits one object on the branch through the rsg service and
// returns the branch's new head state id.
func (f *prFixture) commit(t *testing.T, ctx context.Context, branch, objectType, payload string) string {
	t.Helper()
	if _, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, branch, rsg.CreateObjectInput{
		ObjectType: objectType,
		Payload:    json.RawMessage(payload),
	}); err != nil {
		t.Fatalf("CreateObject on %s: %v", branch, err)
	}
	return *f.head(t, ctx, branch)
}

// head returns the branch's current head state id.
func (f *prFixture) head(t *testing.T, ctx context.Context, branch string) *string {
	t.Helper()
	state, err := f.statesSvc.GetBranchHead(ctx, branch)
	if err != nil {
		t.Fatalf("GetBranchHead: %v", err)
	}
	return &state.ID
}

// newFeature forks the named branch from main's current head.
func (f *prFixture) newFeature(t *testing.T, ctx context.Context, name string) domain.Branch {
	t.Helper()
	feature, err := f.svc.CreateBranch(ctx, f.alice, f.project.ID, rsg.CreateBranchInput{
		Name:       name,
		BaseRef:    *f.head(t, ctx, f.main.ID),
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create branch %s: %v", name, err)
	}
	return feature
}

// openPR opens a PR feature → main through the service.
func (f *prFixture) openPR(t *testing.T, ctx context.Context, featureID, title string) domain.PullRequest {
	t.Helper()
	pr, err := f.prsvc.Create(ctx, pullrequests.CreatePullRequestParams{
		ProjectID:      f.project.ID,
		SourceBranchID: featureID,
		TargetBranchID: f.main.ID,
		Title:          title,
		Body:           "proposal context",
		CreatedBy:      f.alice.ID,
	})
	if err != nil {
		t.Fatalf("CreatePullRequest: %v", err)
	}
	return pr
}

// sqlState asserts err is a Postgres error and returns its SQLSTATE.
func sqlState(t *testing.T, err error) string {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a Postgres error, got %v", err)
	}
	return pgErr.Code
}

// TestPullRequestPinsBaseNoDriftAndNumbersPerProject is the first
// acceptance criterion over the real stack: the base state is pinned at
// creation and does NOT drift when main advances; the number is per
// project.
func TestPullRequestPinsBaseNoDriftAndNumbersPerProject(t *testing.T) {
	ctx := testCtx(t)
	f := newPRFixture(t, ctx)

	mainHead := f.commit(t, ctx, f.main.ID, "claim", `{"statement":"alpha"}`)
	feature := f.newFeature(t, ctx, "feature")
	featureHead := f.commit(t, ctx, feature.ID, "hypothesis", `{"question_ref":"q1"}`)

	pr1 := f.openPR(t, ctx, feature.ID, "Propose the hypothesis")
	if pr1.Number != 1 {
		t.Fatalf("first PR number = %d, want 1", pr1.Number)
	}
	if pr1.BaseStateID != mainHead {
		t.Fatalf("PR base = %s, want main's head at creation %s", pr1.BaseStateID, mainHead)
	}
	if pr1.ProposedStateID != featureHead {
		t.Fatalf("PR proposed = %s, want feature's head at creation %s", pr1.ProposedStateID, featureHead)
	}
	if pr1.State != domain.PullRequestStateOpen || pr1.SourceBranchID != feature.ID || pr1.TargetBranchID != f.main.ID {
		t.Fatalf("PR shape = %+v, want open, feature -> main", pr1)
	}

	// main advances AFTER the PR exists: the PR's base must not drift.
	mainHead2 := f.commit(t, ctx, f.main.ID, "claim", `{"statement":"beta"}`)
	if mainHead2 == mainHead {
		t.Fatal("fixture: main did not advance")
	}
	got, err := f.prsvc.Get(ctx, f.project.ID, 1)
	if err != nil {
		t.Fatalf("Get PR 1: %v", err)
	}
	if got.BaseStateID != mainHead {
		t.Fatalf("PR base drifted with main: %s -> %s (acceptance: PR base 不随 main 漂移)", mainHead, got.BaseStateID)
	}
	if got.ProposedStateID != featureHead {
		t.Fatalf("PR proposed moved without an explicit refresh: %s -> %s", featureHead, got.ProposedStateID)
	}

	// The second PR continues the per-project sequence; its base pins
	// main's head at ITS creation (the newer state).
	pr2 := f.openPR(t, ctx, feature.ID, "Second proposal")
	if pr2.Number != 2 {
		t.Fatalf("second PR number = %d, want 2", pr2.Number)
	}
	if pr2.BaseStateID != mainHead2 {
		t.Fatalf("PR 2 base = %s, want main's head at its creation %s", pr2.BaseStateID, mainHead2)
	}

	list, err := f.prsvc.List(ctx, f.project.ID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].Number != 1 || list[1].Number != 2 {
		t.Fatalf("List = %+v, want PRs 1 and 2 in number order", list)
	}
}

// TestPullRequestHeadRefreshExplicit is the second acceptance criterion:
// the proposed head moves ONLY through the explicit refresh — the one
// path carrying the transaction-scoped flag — and never on a terminal PR.
func TestPullRequestHeadRefreshExplicit(t *testing.T) {
	ctx := testCtx(t)
	f := newPRFixture(t, ctx)

	f.commit(t, ctx, f.main.ID, "claim", `{"statement":"alpha"}`)
	feature := f.newFeature(t, ctx, "feature")
	featureHead := f.commit(t, ctx, feature.ID, "hypothesis", `{"question_ref":"q1"}`)
	pr := f.openPR(t, ctx, feature.ID, "Propose the hypothesis")

	// The feature branch advances; the PR's proposed state stays pinned.
	// The finding payload carries a well-formed pinned claim version: the
	// T0503 semantics check requires every finding to pin at least one
	// (existence is not checked on this path, only the version-id shape).
	featureHead2 := f.commit(t, ctx, feature.ID, "finding",
		`{"summary":"found","claim_version_refs":["cccccccc-cccc-4ccc-8ccc-cccccccccccc"]}`)
	got, err := f.prsvc.Get(ctx, f.project.ID, pr.Number)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ProposedStateID != featureHead {
		t.Fatalf("proposed moved without refresh: %s -> %s", featureHead, got.ProposedStateID)
	}

	// The explicit refresh re-pins to the source branch's CURRENT head.
	refreshed, err := f.prsvc.RefreshProposed(ctx, f.project.ID, pr.Number)
	if err != nil {
		t.Fatalf("RefreshProposed: %v", err)
	}
	if refreshed.ProposedStateID != featureHead2 {
		t.Fatalf("refresh proposed = %s, want feature's current head %s", refreshed.ProposedStateID, featureHead2)
	}

	// Raw writes: the fixity guard refuses base/proposed movement without
	// the flag (P0001), for ANY path — psql included.
	_, err = f.pool.Exec(ctx, `UPDATE pull_requests SET base_state_id = $1 WHERE project_id = $2 AND number = $3`,
		featureHead2, f.project.ID, pr.Number)
	if got := sqlState(t, err); got != "P0001" {
		t.Fatalf("raw base update SQLSTATE = %s, want P0001 (base is fixed, docs/09 §4)", got)
	}
	_, err = f.pool.Exec(ctx, `UPDATE pull_requests SET proposed_state_id = $1 WHERE project_id = $2 AND number = $3`,
		featureHead, f.project.ID, pr.Number)
	if got := sqlState(t, err); got != "P0001" {
		t.Fatalf("raw un-flagged proposed update SQLSTATE = %s, want P0001 (head moves only through the explicit refresh)", got)
	}
	_, err = f.pool.Exec(ctx, `UPDATE pull_requests SET number = 99 WHERE project_id = $1 AND number = $2`,
		f.project.ID, pr.Number)
	if got := sqlState(t, err); got != "P0001" {
		t.Fatalf("raw number update SQLSTATE = %s, want P0001 (number is identity)", got)
	}
	_, err = f.pool.Exec(ctx, `UPDATE pull_requests SET state = 'bogus' WHERE project_id = $1 AND number = $2`,
		f.project.ID, pr.Number)
	if got := sqlState(t, err); got != "P0001" {
		// The fixity/transition guard fires first (BEFORE trigger); the
		// state CHECK itself is asserted in the fresh-install catalog
		// test (checks: ["state = ANY"]).
		t.Fatalf("raw bogus state SQLSTATE = %s, want P0001 (guard transition map; state CHECK asserted in the catalog test)", got)
	}

	// The sanctioned raw path: the transaction-scoped flag opens the one
	// door the guard checks for.
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('post.pr_head_refresh', 'on', true)`); err != nil {
		t.Fatalf("set_config: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE pull_requests SET proposed_state_id = $1 WHERE project_id = $2 AND number = $3`,
		featureHead, f.project.ID, pr.Number); err != nil {
		t.Fatalf("flagged raw proposed update: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Drive the PR to merged: the terminal state refuses the refresh.
	if _, err := f.prsvc.RequestReview(ctx, f.project.ID, pr.Number); err != nil {
		t.Fatalf("RequestReview: %v", err)
	}
	if _, err := f.prsvc.SetState(ctx, f.project.ID, pr.Number, domain.PullRequestStateApproved); err != nil {
		t.Fatalf("approved: %v", err)
	}
	if _, err := f.prsvc.SetState(ctx, f.project.ID, pr.Number, domain.PullRequestStateMergeReady); err != nil {
		t.Fatalf("merge_ready: %v", err)
	}
	merged, err := f.prsvc.MarkMerged(ctx, f.project.ID, pr.Number)
	if err != nil {
		t.Fatalf("MarkMerged: %v", err)
	}
	if merged.MergedAt == nil {
		t.Fatal("merged PR carries no merged_at")
	}
	_, err = f.prsvc.RefreshProposed(ctx, f.project.ID, pr.Number)
	var te *pullrequests.TerminalError
	if !errors.As(err, &te) {
		t.Fatalf("refresh on merged PR error = %v, want TerminalError", err)
	}
	// Even the flagged raw path cannot move a terminal PR (P0001).
	tx, err = f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('post.pr_head_refresh', 'on', true)`); err != nil {
		t.Fatalf("set_config: %v", err)
	}
	_, err = tx.Exec(ctx, `UPDATE pull_requests SET proposed_state_id = $1 WHERE project_id = $2 AND number = $3`,
		featureHead2, f.project.ID, pr.Number)
	if got := sqlState(t, err); got != "P0001" {
		t.Fatalf("flagged raw refresh on merged PR SQLSTATE = %s, want P0001 (terminal PRs never move)", got)
	}
}

// TestPullRequestStateMachine proves the docs/43 machine through the
// service and the trigger backstop for raw writes.
func TestPullRequestStateMachine(t *testing.T) {
	ctx := testCtx(t)
	f := newPRFixture(t, ctx)
	f.commit(t, ctx, f.main.ID, "claim", `{"statement":"alpha"}`)
	feature := f.newFeature(t, ctx, "feature")
	f.commit(t, ctx, feature.ID, "hypothesis", `{"question_ref":"q1"}`)

	// The full happy chain: open → review_required → approved →
	// merge_ready → merged, checking the state after every move.
	pr := f.openPR(t, ctx, feature.ID, "Happy path")
	wantState := func(want domain.PullRequestState) {
		t.Helper()
		got, err := f.prsvc.Get(ctx, f.project.ID, pr.Number)
		if err != nil || got.State != want {
			t.Fatalf("PR state = %s, %v — want %s", got.State, err, want)
		}
	}
	wantState(domain.PullRequestStateOpen)
	if _, err := f.prsvc.RequestReview(ctx, f.project.ID, pr.Number); err != nil {
		t.Fatalf("RequestReview: %v", err)
	}
	wantState(domain.PullRequestStateReviewRequired)
	if _, err := f.prsvc.SetState(ctx, f.project.ID, pr.Number, domain.PullRequestStateApproved); err != nil {
		t.Fatalf("approved: %v", err)
	}
	wantState(domain.PullRequestStateApproved)
	if _, err := f.prsvc.SetState(ctx, f.project.ID, pr.Number, domain.PullRequestStateMergeReady); err != nil {
		t.Fatalf("merge_ready: %v", err)
	}
	wantState(domain.PullRequestStateMergeReady)
	if _, err := f.prsvc.MarkMerged(ctx, f.project.ID, pr.Number); err != nil {
		t.Fatalf("MarkMerged: %v", err)
	}
	wantState(domain.PullRequestStateMerged)
	// A terminal PR refuses every further move — service side.
	_, err := f.prsvc.SetState(ctx, f.project.ID, pr.Number, domain.PullRequestStateClosed)
	var transErr *pullrequests.TransitionError
	if !errors.As(err, &transErr) {
		t.Fatalf("merged -> closed error = %v, want TransitionError", err)
	}
	// ... and database side (P0001), even for a raw write.
	_, err = f.pool.Exec(ctx, `UPDATE pull_requests SET state = 'open' WHERE project_id = $1 AND number = $2`,
		f.project.ID, pr.Number)
	if got := sqlState(t, err); got != "P0001" {
		t.Fatalf("raw merged -> open SQLSTATE = %s, want P0001", got)
	}

	// The review loop: changes_requested → review_required (re-review).
	pr2 := f.openPR(t, ctx, feature.ID, "Review loop")
	if _, err := f.prsvc.RequestReview(ctx, f.project.ID, pr2.Number); err != nil {
		t.Fatalf("RequestReview 2: %v", err)
	}
	if _, err := f.prsvc.SetState(ctx, f.project.ID, pr2.Number, domain.PullRequestStateChangesRequested); err != nil {
		t.Fatalf("changes_requested: %v", err)
	}
	if _, err := f.prsvc.RequestReview(ctx, f.project.ID, pr2.Number); err != nil {
		t.Fatalf("re-request review: %v", err)
	}
	// Skipping the machine is refused — service side and trigger side.
	if _, err := f.prsvc.MarkMerged(ctx, f.project.ID, pr2.Number); !errors.As(err, &transErr) {
		t.Fatalf("review_required -> merged error = %v, want TransitionError", err)
	}
	_, err = f.pool.Exec(ctx, `UPDATE pull_requests SET state = 'merged' WHERE project_id = $1 AND number = $2`,
		f.project.ID, pr2.Number)
	if got := sqlState(t, err); got != "P0001" {
		t.Fatalf("raw review_required -> merged SQLSTATE = %s, want P0001", got)
	}

	// closed and aborted are terminal alternatives, reachable from open.
	pr3 := f.openPR(t, ctx, feature.ID, "Closed path")
	if _, err := f.prsvc.Close(ctx, f.project.ID, pr3.Number); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := f.prsvc.RequestReview(ctx, f.project.ID, pr3.Number); !errors.As(err, &transErr) {
		t.Fatalf("closed -> review_required error = %v, want TransitionError", err)
	}
	pr4 := f.openPR(t, ctx, feature.ID, "Aborted path")
	if _, err := f.prsvc.Abort(ctx, f.project.ID, pr4.Number); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	got, err := f.prsvc.Get(ctx, f.project.ID, pr4.Number)
	if err != nil || got.State != domain.PullRequestStateAborted || got.MergedAt != nil {
		t.Fatalf("aborted PR = %+v, %v — want aborted, no merged_at", got, err)
	}
}

// TestPullRequestIsolationAndValidation pins the boundary: per-project
// numbering, foreign-entity non-leakage, shape validation and the
// merged_at consistency the trigger enforces on raw inserts.
func TestPullRequestIsolationAndValidation(t *testing.T) {
	ctx := testCtx(t)
	f := newPRFixture(t, ctx)
	f.commit(t, ctx, f.main.ID, "claim", `{"statement":"alpha"}`)
	feature := f.newFeature(t, ctx, "feature")
	f.commit(t, ctx, feature.ID, "hypothesis", `{"question_ref":"q1"}`)
	pr := f.openPR(t, ctx, feature.ID, "Proposal")

	// Shape validation at the service boundary.
	if _, err := f.prsvc.Create(ctx, pullrequests.CreatePullRequestParams{
		ProjectID: f.project.ID, SourceBranchID: feature.ID,
		TargetBranchID: feature.ID, Title: "same branch", CreatedBy: f.alice.ID,
	}); !errors.Is(err, pullrequests.ErrValidation) {
		t.Fatalf("source == target error = %v, want ErrValidation", err)
	}
	if _, err := f.prsvc.Create(ctx, pullrequests.CreatePullRequestParams{
		ProjectID: f.project.ID, SourceBranchID: "not-a-branch-id",
		TargetBranchID: f.main.ID, Title: "x", CreatedBy: f.alice.ID,
	}); !errors.Is(err, pullrequests.ErrValidation) {
		t.Fatalf("malformed branch id error = %v, want ErrValidation", err)
	}

	// A second project's PRs number from 1 — number is PER PROJECT — and
	// a foreign number reads as not found (no existence leak).
	bob, err := persistence.NewCredentialStore(f.pool).CreateWithPassword(
		ctx, "pr-bob@example.com", "hash", "pr-bob", "Bob")
	if err != nil {
		t.Fatalf("seed bob: %v", err)
	}
	orgStore := persistence.NewOrgStore(f.pool)
	org2, _, err := orgStore.CreateOrganization(ctx, domain.Organization{
		Slug: "pr-fixture-2", Name: "PR Fixture 2",
	}, bob.ID, todayUTC())
	if err != nil {
		t.Fatalf("create org 2: %v", err)
	}
	project2, _, err := persistence.NewProjectStore(f.pool).CreateProject(ctx, domain.Project{
		OrganizationID:  &org2.ID,
		Slug:            "pr-project-2",
		Name:            "PR Project 2",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, bob.ID)
	if err != nil {
		t.Fatalf("create project 2: %v", err)
	}
	main2, err := f.svc.CreateBranch(ctx, bob, project2.ID, rsg.CreateBranchInput{
		Name:       "main",
		BaseRef:    "",
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create branch 2: %v", err)
	}
	feature2, err := f.svc.CreateBranch(ctx, bob, project2.ID, rsg.CreateBranchInput{
		Name:       "feature",
		BaseRef:    *f.head(t, ctx, main2.ID),
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create feature 2: %v", err)
	}
	prSvc2 := pullrequests.NewService(persistence.NewPullRequestStore(f.pool))
	prOther, err := prSvc2.Create(ctx, pullrequests.CreatePullRequestParams{
		ProjectID:      project2.ID,
		SourceBranchID: feature2.ID,
		TargetBranchID: main2.ID,
		Title:          "Other project's first PR",
		CreatedBy:      bob.ID,
	})
	if err != nil {
		t.Fatalf("create PR in project 2: %v", err)
	}
	if prOther.Number != 1 {
		t.Fatalf("project 2's first PR number = %d, want 1 (number per project)", prOther.Number)
	}
	// A second PR in project 2 carries #2 there — number 2 does not exist
	// in project 1 (which has only its #1), so the read reports the same
	// not-found outcome as any foreign number (never leak another
	// project's entity existence, docs/45).
	prOther2, err := prSvc2.Create(ctx, pullrequests.CreatePullRequestParams{
		ProjectID:      project2.ID,
		SourceBranchID: feature2.ID,
		TargetBranchID: main2.ID,
		Title:          "Other project's second PR",
		CreatedBy:      bob.ID,
	})
	if err != nil || prOther2.Number != 2 {
		t.Fatalf("project 2's second PR = %+v, %v — want number 2", prOther2, err)
	}
	if _, err := f.prsvc.Get(ctx, f.project.ID, prOther2.Number); !errors.Is(err, pullrequests.ErrPullRequestNotFound) {
		t.Fatalf("foreign number read error = %v, want ErrPullRequestNotFound", err)
	}
	if got, err := prSvc2.Get(ctx, project2.ID, prOther2.Number); err != nil || got.Number != 2 {
		t.Fatalf("own project's #2 read = %+v, %v", got, err)
	}

	// A branch of another project reports the same not-found outcome.
	if _, err := f.prsvc.Create(ctx, pullrequests.CreatePullRequestParams{
		ProjectID:      f.project.ID,
		SourceBranchID: feature2.ID,
		TargetBranchID: f.main.ID,
		Title:          "foreign branch",
		CreatedBy:      f.alice.ID,
	}); !errors.Is(err, pullrequests.ErrBranchNotFound) {
		t.Fatalf("foreign branch error = %v, want ErrBranchNotFound", err)
	}

	// A merged branch refuses proposals (docs/43: closed paths).
	mergedBranch, err := f.svc.CreateBranch(ctx, f.alice, f.project.ID, rsg.CreateBranchInput{
		Name:       "done",
		BaseRef:    *f.head(t, ctx, f.main.ID),
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create done branch: %v", err)
	}
	if _, err := f.branches.Merge(ctx, f.project.ID, mergedBranch.ID); err != nil {
		t.Fatalf("merge done branch: %v", err)
	}
	_, err = f.prsvc.Create(ctx, pullrequests.CreatePullRequestParams{
		ProjectID:      f.project.ID,
		SourceBranchID: mergedBranch.ID,
		TargetBranchID: f.main.ID,
		Title:          "from a merged branch",
		CreatedBy:      f.alice.ID,
	})
	var bnae *pullrequests.BranchNotActiveError
	if !errors.As(err, &bnae) {
		t.Fatalf("merged source branch error = %v, want BranchNotActiveError", err)
	}

	// The trigger stamps merged_at on raw INSERTs reaching merged and
	// refuses merged_at on non-merged rows — consistency for ANY path.
	var rawID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO pull_requests (project_id, number, source_branch_id, target_branch_id,
		                           base_state_id, proposed_state_id, title, state, created_by)
		VALUES ($1, 77, $2, $3, $4, $5, 'raw merged', 'merged', $6) RETURNING id`,
		f.project.ID, feature.ID, f.main.ID, pr.BaseStateID, pr.ProposedStateID, f.alice.ID).Scan(&rawID); err != nil {
		t.Fatalf("raw merged insert: %v", err)
	}
	var mergedSet bool
	if err := f.pool.QueryRow(ctx, `SELECT merged_at IS NOT NULL FROM pull_requests WHERE id = $1`, rawID).Scan(&mergedSet); err != nil {
		t.Fatalf("read merged_at: %v", err)
	}
	if !mergedSet {
		t.Fatalf("raw merged insert left merged_at NULL — want the trigger to stamp it")
	}
	_, err = f.pool.Exec(ctx, `
		INSERT INTO pull_requests (project_id, number, source_branch_id, target_branch_id,
		                           base_state_id, proposed_state_id, title, state, created_by, merged_at)
		VALUES ($1, 78, $2, $3, $4, $5, 'raw open with merged_at', 'open', $6, now())`,
		f.project.ID, feature.ID, f.main.ID, pr.BaseStateID, pr.ProposedStateID, f.alice.ID)
	if got := sqlState(t, err); got != "P0001" {
		t.Fatalf("raw open-with-merged_at insert SQLSTATE = %s, want P0001", got)
	}
}
