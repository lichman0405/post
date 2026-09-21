package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/diffs"
	"github.com/lichman0405/post/internal/application/merge"
	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/prchecks"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/application/resolutions"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/states"
	appvalidation "github.com/lichman0405/post/internal/application/validation"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/sqlc"
	"github.com/lichman0405/post/internal/rsg/integrity"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// Task T0611: the release's review record must cover the states that
// entered main by MERGE.
//
// docs/09 §3/§4: main advances only through a Research PR merge, and the
// merge commits a NEW state whose parent is the target head the merge ran
// on (migration 00069 pins the triple; internal/application/merge writes
// BaseStateID = target head). So the proposal state P is never an ancestor
// of the merged state M, and T0605's read — "reviews of the PRs whose
// proposed state is in the released state's lineage" — returns nothing for
// exactly the states that got into main the only legitimate way. The same
// record feeds the release gate (releases.Service.ReleaseFacts →
// rsgvalidation.ReleaseFacts.ReviewApproved, docs/11 §1), so a release of a
// merged state was refused by its own gate.
//
// The fix makes the merge edge readable (semantic_merges.result_state_id →
// pull_request_id) without dropping the proposed-state edge T0605 wrote.
//
// This file proves both halves over REAL PostgreSQL: the chain below is the
// production one (branch → commit → PR → reviews → merge service → main),
// and the release is created over the wire against the real release API.

// releaseMergeFixture is the release server (the T0606 composition: real
// auth, org, project, policy and release routes over one migrated
// database) plus the research/merge stack the fix's chain needs, wired
// exactly as cmd/api wires it.
type releaseMergeFixture struct {
	pool       *pgxpool.Pool
	alice      *testUserClient
	aliceID    string
	bobID      string
	projectID  string
	mainBranch string
	genesis    string
	svc        *rsg.Service
	states     *persistence.StateStore
	prs        *pullrequests.Service
	merges     *merge.Service
	store      *persistence.ReleaseStore
}

func newReleaseMergeFixture(t *testing.T, ctx context.Context) *releaseMergeFixture {
	t.Helper()
	ts, pool := newReleaseServer(t, ctx)
	alice, aliceID := signup(t, ts.URL, "postmerge-alice@example.com", "postmerge-alice")
	_, bobID := signup(t, ts.URL, "postmerge-bob@example.com", "postmerge-bob")

	resp := alice.do(t, http.MethodPost, "/api/v1/organizations",
		`{"slug":"postmerge-labs","name":"Post Merge Labs"}`)
	mustStatus(t, resp, http.StatusCreated)
	var createdOrg orgResponse
	if err := json.NewDecoder(resp.Body).Decode(&createdOrg); err != nil {
		t.Fatalf("org create payload: %v", err)
	}
	orgID := createdOrg.Organization.ID

	resp = alice.do(t, http.MethodPost, "/api/v1/projects",
		fmt.Sprintf(`{"slug":"postmerge-lab","name":"Post Merge Lab","purpose":"release after merge","visibility":"private","organization_id":%q}`, orgID))
	mustStatus(t, resp, http.StatusCreated)
	var createdProj projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&createdProj); err != nil {
		t.Fatalf("project create payload: %v", err)
	}
	projectID := createdProj.Project.ID

	// The rights snapshot the release gate demands (both scopes pinned)
	// and the main_protected rule the merge's fail-closed policy check
	// reads (docs/12 §5).
	for _, path := range []string{
		"/api/v1/organizations/" + orgID + "/policy",
		"/api/v1/projects/" + projectID + "/policy",
	} {
		resp := alice.do(t, http.MethodPut, path,
			`{"version":"v1","policy":{"main_protected":true,"release_min_reviewers":2}}`)
		mustStatus(t, resp, http.StatusCreated)
	}

	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	projectStore := persistence.NewProjectStore(pool)
	orgStore := persistence.NewOrgStore(pool)
	projectSvc := projects.NewService(projectStore, orgStore, authz.NewMatrixEngine())
	stateStore := persistence.NewStateStore(pool)
	statesSvc := states.NewService(stateStore, appvalidation.NewGuard(
		rsgvalidation.NewValidator(reg), persistence.NewValidationTxProbe()))
	svc := rsg.NewService(rsg.Deps{
		Projects:  projectSvc,
		Branches:  branches.NewService(persistence.NewBranchStore(pool)),
		States:    statesSvc,
		Latest:    stateStore,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
		Events:    events.Recorder{},
	})
	aliceUser := domain.User{ID: aliceID}
	mainBranch, err := svc.CreateBranch(ctx, aliceUser, projectID, rsg.CreateBranchInput{
		Name:       "main",
		BaseRef:    "",
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create main branch: %v", err)
	}
	var genesis string
	if err := pool.QueryRow(ctx,
		`SELECT id FROM project_states WHERE project_id = $1 AND parent_state_id IS NULL`, projectID).Scan(&genesis); err != nil {
		t.Fatalf("read genesis state: %v", err)
	}
	seedReleaseMergeAcceptedState(t, ctx, svc, aliceUser, projectID, mainBranch.ID)

	policyStore := persistence.NewPolicyStore(pool)
	diffsSvc := diffs.NewService(stateStore, persistence.NewManifestStore(pool), persistence.NewPullRequestStore(pool))
	resolutionSvc := resolutions.NewService(
		diffsSvc, resolutions.NewPGStore(pool), projectSvc, authz.NewMatrixEngine())
	merges := merge.NewService(merge.Deps{
		Store:     persistence.NewSemanticMergeStore(pool),
		Diffs:     diffsSvc,
		Plans:     resolutionSvc,
		Commits:   statesSvc,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Projects:  projectSvc,
		Authz:     authz.NewMatrixEngine(),
		Checks: prchecks.NewService(prchecks.Deps{
			PRs:      persistence.NewPullRequestStore(pool),
			Projects: projectStore,
			States:   stateStore,
			Branches: persistence.NewBranchStore(pool),
			Manifest: persistence.NewManifestStore(pool),
			Policies: policyStore,
			Engine:   integrity.New(reg),
		}),
		Policies: policy.NewService(policyStore, orgStore, projectStore, nil),
		Rules:    policy.NewRuleEvaluator(),
		Events:   events.Recorder{},
	})

	return &releaseMergeFixture{
		pool:       pool,
		alice:      alice,
		aliceID:    aliceID,
		bobID:      bobID,
		projectID:  projectID,
		mainBranch: mainBranch.ID,
		genesis:    genesis,
		svc:        svc,
		states:     stateStore,
		prs:        pullrequests.NewService(persistence.NewPullRequestStore(pool)),
		merges:     merges,
		store:      persistence.NewReleaseStore(pool),
	}
}

// seedReleaseMergeAcceptedState writes the accepted main state the release
// gate validates: a research question and a hypothesis carrying the docs/08
// domain fields the release ladder makes blocking (the same seed the T0606
// e2e uses — the release gate runs the main-gate member checks plus the
// three release facts).
func seedReleaseMergeAcceptedState(t *testing.T, ctx context.Context, svc *rsg.Service, alice domain.User, projectID, mainBranch string) {
	t.Helper()
	qRes, err := svc.CreateObject(ctx, alice, projectID, mainBranch, rsg.CreateObjectInput{
		ObjectType: "research_question",
		Payload:    json.RawMessage(`{"statement":"does the merge land?","purpose":"prove the release record after merge","question_state":"open"}`),
	})
	if err != nil {
		t.Fatalf("CreateObject research question on main: %v", err)
	}
	if _, err := svc.CreateObject(ctx, alice, projectID, mainBranch, rsg.CreateObjectInput{
		ObjectType: "hypothesis",
		Payload: json.RawMessage(fmt.Sprintf(
			`{"statement":"the merged state is releasable","question_id":%q,"hypothesis_type":"mechanistic","scope":{"detail":"probe"}}`,
			qRes.Object.ID)),
	}); err != nil {
		t.Fatalf("CreateObject hypothesis on main: %v", err)
	}
}

// fork creates a research branch off baseState.
func (f *releaseMergeFixture) fork(t *testing.T, ctx context.Context, name, baseState string) domain.Branch {
	t.Helper()
	branch, err := f.svc.CreateBranch(ctx, domain.User{ID: f.aliceID}, f.projectID, rsg.CreateBranchInput{
		Name:       name,
		BaseRef:    baseState,
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create %s branch: %v", name, err)
	}
	return branch
}

// openPR opens one proposal against main and pins its proposed head to the
// source branch's head (the explicit refresh, T0402).
func (f *releaseMergeFixture) openPR(t *testing.T, ctx context.Context, sourceBranchID, title string) domain.PullRequest {
	t.Helper()
	pr, err := f.prs.Create(ctx, pullrequests.CreatePullRequestParams{
		ProjectID:      f.projectID,
		SourceBranchID: sourceBranchID,
		TargetBranchID: f.mainBranch,
		Title:          title,
		CreatedBy:      f.aliceID,
	})
	if err != nil {
		t.Fatalf("create pull request: %v", err)
	}
	pr, err = f.prs.RefreshProposed(ctx, f.projectID, pr.Number)
	if err != nil {
		t.Fatalf("refresh proposed head: %v", err)
	}
	return pr
}

// addReview seeds one review row on a PR through the canonical query.
// reviewedState is the head the review evaluates (migration 00061 pins
// reviewed_state_id to the PR's proposed head).
func (f *releaseMergeFixture) addReview(t *testing.T, ctx context.Context, pr domain.PullRequest, reviewerID, kind, decision string) {
	t.Helper()
	q := sqlc.New(f.pool)
	if _, err := q.CreateReview(ctx, sqlc.CreateReviewParams{
		PullRequestID:   parseUUIDOrDie(pr.ID),
		ReviewerID:      parseUUIDOrDie(reviewerID),
		ReviewKind:      kind,
		Decision:        decision,
		ReviewedStateID: parseUUIDOrDie(pr.ProposedStateID),
		Responsibility:  "",
		Body:            "",
	}); err != nil {
		t.Fatalf("CreateReview: %v", err)
	}
}

// approve drives a PR through the docs/43 review machine to merge_ready —
// the only state the merge engine accepts.
func (f *releaseMergeFixture) approve(t *testing.T, ctx context.Context, number int64) domain.PullRequest {
	t.Helper()
	if _, err := f.prs.RequestReview(ctx, f.projectID, number); err != nil {
		t.Fatalf("request review: %v", err)
	}
	if _, err := f.prs.SetState(ctx, f.projectID, number, domain.PullRequestStateApproved); err != nil {
		t.Fatalf("approve: %v", err)
	}
	ready, err := f.prs.SetState(ctx, f.projectID, number, domain.PullRequestStateMergeReady)
	if err != nil {
		t.Fatalf("mark merge ready: %v", err)
	}
	return ready
}

// mergePR runs one real Research PR merge and returns the accepted state
// it committed on main.
func (f *releaseMergeFixture) mergePR(t *testing.T, ctx context.Context, number int64) string {
	t.Helper()
	res, err := f.merges.Merge(ctx, domain.User{ID: f.aliceID}, merge.Input{
		ProjectID: f.projectID,
		Number:    number,
	})
	if err != nil {
		t.Fatalf("merge PR #%d: %v", number, err)
	}
	if res.State.BranchID == nil || *res.State.BranchID != f.mainBranch {
		t.Fatalf("merge result state %s is not on main (%v)", res.State.ID, res.State.BranchID)
	}
	return res.State.ID
}

// TestReleaseReviewAfterMerge is the fix's end-to-end proof, over the
// production path (docs/09 §3: main advances only through a Research PR
// merge) against REAL PostgreSQL and the REAL release API:
//
//   - after one merge, ListReleaseReviews of the state the merge committed
//     carries the merged PR's reviews (it returned NOTHING before the fix:
//     the proposal is not an ancestor of the state the merge wrote);
//   - POST /releases answers 201 (it answered 409 RELEASE_GATE_BLOCKED
//     before the fix — the gate reads this very record) and the manifest's
//     reviews section is exactly the stored rows, field for field;
//   - after a SECOND merge the later state's record carries both merges'
//     PRs, each once, in PR-number order;
//   - a research head that never merged contributes nothing.
func TestReleaseReviewAfterMerge(t *testing.T) {
	ctx := testCtx(t)
	f := newReleaseMergeFixture(t, ctx)

	// --- merge #1: research-one → main ---
	branch1 := f.fork(t, ctx, "research-one", f.genesis)
	mainBeforeMerge, err := f.states.GetBranchHead(ctx, f.mainBranch)
	if err != nil {
		t.Fatalf("read main head before the merge: %v", err)
	}
	if _, err := f.svc.CreateObject(ctx, domain.User{ID: f.aliceID}, f.projectID, branch1.ID, rsg.CreateObjectInput{
		ObjectType: "claim",
		Payload:    json.RawMessage(mergeMainGateClaim("merged into main first")),
	}); err != nil {
		t.Fatalf("CreateObject claim on research-one: %v", err)
	}
	pr1 := f.openPR(t, ctx, branch1.ID, "first merge")
	f.addReview(t, ctx, pr1, f.bobID, "scientific", "approved")
	f.addReview(t, ctx, pr1, f.bobID, "integrity", "approved")
	f.approve(t, ctx, pr1.Number)
	merged1 := f.mergePR(t, ctx, pr1.Number)

	// The merge is a NEW state on main whose parent is the target head it
	// ran on — not the proposal. That is what makes the proposal invisible
	// to the lineage walk the release record used to rely on alone.
	if merged1 == pr1.ProposedStateID {
		t.Fatalf("merge committed the proposal state itself (%s): the fixture does not exercise the merge edge", merged1)
	}
	var parent string
	if err := f.pool.QueryRow(ctx,
		`SELECT parent_state_id FROM project_states WHERE id = $1`, merged1).Scan(&parent); err != nil {
		t.Fatalf("read the merged state's parent: %v", err)
	}
	if parent != mainBeforeMerge.ID {
		t.Fatalf("merged state's parent = %s, want the pre-merge main head %s", parent, mainBeforeMerge.ID)
	}

	records, err := f.store.ListReleaseReviews(ctx, merged1, f.mainBranch)
	if err != nil {
		t.Fatalf("ListReleaseReviews after merge #1: %v", err)
	}
	// Errorf, not Fatalf: the same defect shows up again at the release gate
	// below, and one run should report both halves of it rather than stopping
	// at the first.
	if len(records) != 1 {
		t.Errorf("after merge #1: records = %+v, want exactly the merged PR #%d (the proposal %s is not an ancestor of %s)",
			records, pr1.Number, pr1.ProposedStateID, merged1)
	} else {
		if records[0].PullRequestNumber != pr1.Number {
			t.Errorf("after merge #1: record = PR #%d, want PR #%d", records[0].PullRequestNumber, pr1.Number)
		}
		if len(records[0].Reviews) != 2 {
			t.Errorf("after merge #1: PR #%d has %d reviews, want the two approved ones: %+v",
				pr1.Number, len(records[0].Reviews), records[0].Reviews)
		}
	}

	// The release of the merged state: the gate reads this record, so it is
	// the same defect seen from the wire.
	resp := createRelease(t, f.alice, f.projectID, "v1.0.0", "First release", "post-merge-key-1")
	mustStatus(t, resp, http.StatusCreated)
	first := decodeRelease(t, resp)
	if first.StateID != merged1 {
		t.Fatalf("release state_id = %s, want the merged main head %s", first.StateID, merged1)
	}
	manifest1 := decodeReleaseManifest(t, f, first.ID)
	if manifest1.StateID != merged1 {
		t.Fatalf("manifest state_id = %s, want %s", manifest1.StateID, merged1)
	}
	assertManifestReviewsMatchDatabase(t, ctx, f, manifest1, []int64{pr1.Number}, "after merge #1")

	// --- merge #2: research-two → main (on top of merge #1's state) ---
	branch2 := f.fork(t, ctx, "research-two", merged1)
	if _, err := f.svc.CreateObject(ctx, domain.User{ID: f.aliceID}, f.projectID, branch2.ID, rsg.CreateObjectInput{
		ObjectType: "claim",
		Payload:    json.RawMessage(mergeMainGateClaim("merged into main second")),
	}); err != nil {
		t.Fatalf("CreateObject claim on research-two: %v", err)
	}
	pr2 := f.openPR(t, ctx, branch2.ID, "second merge")
	f.addReview(t, ctx, pr2, f.bobID, "scientific", "approved")
	f.addReview(t, ctx, pr2, f.bobID, "integrity", "approved")
	f.approve(t, ctx, pr2.Number)
	merged2 := f.mergePR(t, ctx, pr2.Number)

	records2, err := f.store.ListReleaseReviews(ctx, merged2, f.mainBranch)
	if err != nil {
		t.Fatalf("ListReleaseReviews after merge #2: %v", err)
	}
	if len(records2) != 2 {
		t.Fatalf("after merge #2: records = %+v, want both merges' PRs (#%d, #%d)", records2, pr1.Number, pr2.Number)
	}
	// Ordered by PR number, and each PR appears exactly once — the merge
	// edge must not duplicate a record the proposed-state edge also carries.
	if records2[0].PullRequestNumber != pr1.Number || records2[1].PullRequestNumber != pr2.Number {
		t.Fatalf("after merge #2: PR order = #%d, #%d; want #%d, #%d",
			records2[0].PullRequestNumber, records2[1].PullRequestNumber, pr1.Number, pr2.Number)
	}
	for _, rec := range records2 {
		if len(rec.Reviews) != 2 {
			t.Fatalf("PR #%d carries %d reviews, want 2: %+v", rec.PullRequestNumber, len(rec.Reviews), rec.Reviews)
		}
	}
	if records2[0].ProposedStateID != pr1.ProposedStateID || records2[1].ProposedStateID != pr2.ProposedStateID {
		t.Fatalf("record proposed states = %s, %s; want the proposals %s, %s",
			records2[0].ProposedStateID, records2[1].ProposedStateID, pr1.ProposedStateID, pr2.ProposedStateID)
	}

	// The unmerged research path: a third PR, reviewed, never merged. Its
	// reviews are not part of main's acceptance record.
	branch3 := f.fork(t, ctx, "research-three", merged2)
	if _, err := f.svc.CreateObject(ctx, domain.User{ID: f.aliceID}, f.projectID, branch3.ID, rsg.CreateObjectInput{
		ObjectType: "claim",
		Payload:    json.RawMessage(mergeMainGateClaim("still on a research branch")),
	}); err != nil {
		t.Fatalf("CreateObject claim on research-three: %v", err)
	}
	pr3 := f.openPR(t, ctx, branch3.ID, "never merged")
	f.addReview(t, ctx, pr3, f.bobID, "scientific", "approved")
	f.addReview(t, ctx, pr3, f.bobID, "integrity", "approved")

	// The release of the newer merged state, over the wire, with both
	// merges' reviews in its manifest and the unmerged PR absent.
	resp = createRelease(t, f.alice, f.projectID, "v1.1.0", "Second release", "post-merge-key-2")
	mustStatus(t, resp, http.StatusCreated)
	second := decodeRelease(t, resp)
	if second.StateID != merged2 {
		t.Fatalf("release state_id = %s, want the second merged head %s", second.StateID, merged2)
	}
	manifest2 := decodeReleaseManifest(t, f, second.ID)
	assertManifestReviewsMatchDatabase(t, ctx, f, manifest2, []int64{pr1.Number, pr2.Number}, "after merge #2")
	if len(manifest2.Reviews) != 2 {
		t.Fatalf("manifest reviews = %+v, want the two merged PRs", manifest2.Reviews)
	}
	for _, rec := range manifest2.Reviews {
		if rec.PullRequestNumber == pr3.Number {
			t.Fatalf("manifest carries the unmerged PR #%d: %+v", pr3.Number, rec)
		}
	}
}

// TestReleaseReviewsBothEdgesOfOnePRAppearOnce pins the dedup property of
// the read: the same PR can qualify through BOTH edges at once — its
// proposed state is inside the released lineage AND its merge row's result
// state is — and it must still be one record, carrying each review row
// once.
//
// The merge service cannot currently produce that shape (a merge with
// nothing to apply plans no operations, and a state commit with no
// operations is refused), so the shape is seeded directly: it is the row
// pair the QUERY must stay correct over, and the union of two sets is
// exactly where "list the PR twice" would show up.
func TestReleaseReviewsBothEdgesOfOnePRAppearOnce(t *testing.T) {
	ctx := testCtx(t)
	f := newReleaseMergeFixture(t, ctx)

	head, err := f.states.GetBranchHead(ctx, f.mainBranch)
	if err != nil {
		t.Fatalf("read main head: %v", err)
	}
	// PR #1 proposes main's own head against main (T0605's fixture shape:
	// the proposal IS a lineage member) ...
	q := sqlc.New(f.pool)
	row, err := q.CreatePullRequest(ctx, sqlc.CreatePullRequestParams{
		ProjectID:       parseUUIDOrDie(f.projectID),
		Number:          1,
		SourceBranchID:  parseUUIDOrDie(f.mainBranch),
		TargetBranchID:  parseUUIDOrDie(f.mainBranch),
		BaseStateID:     parseUUIDOrDie(f.genesis),
		ProposedStateID: parseUUIDOrDie(head.ID),
		Title:           "proposes a state already on main",
		Body:            "",
		CreatedBy:       parseUUIDOrDie(f.aliceID),
	})
	if err != nil {
		t.Fatalf("CreatePullRequest: %v", err)
	}
	prID := pgUUIDTextTest(row.ID)
	for _, kind := range []string{"scientific", "integrity"} {
		if _, err := q.CreateReview(ctx, sqlc.CreateReviewParams{
			PullRequestID:   parseUUIDOrDie(prID),
			ReviewerID:      parseUUIDOrDie(f.bobID),
			ReviewKind:      kind,
			Decision:        "approved",
			ReviewedStateID: parseUUIDOrDie(head.ID),
			Responsibility:  "",
			Body:            "",
		}); err != nil {
			t.Fatalf("CreateReview: %v", err)
		}
	}
	// ... and its merge row's result state is that same lineage member, so
	// the merge edge matches too.
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO semantic_merges
			(project_id, pull_request_id, source_branch_id, target_branch_id,
			 base_state_id, source_state_id, target_state_id, result_state_id, actor_id,
			 plan_version, plan, plan_digest)
		VALUES ($1, $2, $3, $3, $4, $5, $5, $5, $6, 'v1', '{}'::jsonb, 'seeded-digest')`,
		f.projectID, prID, f.mainBranch, f.genesis, head.ID, f.aliceID); err != nil {
		t.Fatalf("seed the merge edge: %v", err)
	}

	records, err := f.store.ListReleaseReviews(ctx, head.ID, f.mainBranch)
	if err != nil {
		t.Fatalf("ListReleaseReviews: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %+v, want PR #1 exactly once (both edges match the same PR)", records)
	}
	if records[0].PullRequestNumber != 1 || records[0].ProposedStateID != head.ID {
		t.Fatalf("record = %+v, want PR #1 proposing %s", records[0], head.ID)
	}
	if len(records[0].Reviews) != 2 {
		t.Fatalf("PR #1 carries %d reviews, want 2 — one row per stored review, not one per matching edge: %+v",
			len(records[0].Reviews), records[0].Reviews)
	}
	ids := map[string]int{}
	for _, r := range records[0].Reviews {
		ids[r.ID]++
	}
	for id, n := range ids {
		if n != 1 {
			t.Fatalf("review %s appears %d times in one record, want once", id, n)
		}
	}
}

// releaseManifestReviews is the part of the manifest export the release
// contract fixes (docs/11 §1: the manifest pins the review/approval
// record).
type releaseManifestReviews struct {
	StateID string `json:"state_id"`
	Reviews []struct {
		PullRequestNumber int64  `json:"pull_request_number"`
		ProposedStateID   string `json:"proposed_state_id"`
		Reviews           []struct {
			ID         string    `json:"id"`
			ReviewerID string    `json:"reviewer_id"`
			ReviewKind string    `json:"review_kind"`
			Decision   string    `json:"decision"`
			CreatedAt  time.Time `json:"created_at"`
		} `json:"reviews"`
	} `json:"reviews"`
}

// decodeReleaseManifest downloads the stored manifest export and decodes
// the review section of it.
func decodeReleaseManifest(t *testing.T, f *releaseMergeFixture, releaseID string) releaseManifestReviews {
	t.Helper()
	b, hdr := getManifestBytes(t, f.alice, f.projectID, releaseID)
	if ct := hdr.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("manifest content type = %q", ct)
	}
	var doc releaseManifestReviews
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("manifest decode: %v", err)
	}
	return doc
}

// assertManifestReviewsMatchDatabase compares the manifest's review
// record with the reviews table, row for row and field for field: the
// manifest is the pinned record (docs/11 §1), so it must not be a
// re-derivation of it. wantPRs names the pull requests the record is
// expected to cover — the manifest must carry exactly the stored rows of
// those PRs and nothing else.
func assertManifestReviewsMatchDatabase(t *testing.T, ctx context.Context, f *releaseMergeFixture, manifest releaseManifestReviews, wantPRs []int64, when string) {
	t.Helper()
	want := make(map[int64]bool, len(wantPRs))
	for _, n := range wantPRs {
		want[n] = true
	}
	for _, rec := range manifest.Reviews {
		if !want[rec.PullRequestNumber] {
			t.Fatalf("%s: manifest carries PR #%d, which is not part of the record: %+v", when, rec.PullRequestNumber, rec)
		}
	}
	type storedReview struct {
		number     int64
		proposed   string
		id         string
		reviewerID string
		kind       string
		decision   string
		createdAt  time.Time
	}
	rows, err := f.pool.Query(ctx, `
		SELECT pr.number, pr.proposed_state_id, r.id, r.reviewer_id, r.review_kind, r.decision, r.created_at
		FROM reviews r
		JOIN pull_requests pr ON pr.id = r.pull_request_id
		WHERE pr.project_id = $1 AND pr.number = ANY($2)
		ORDER BY pr.number, r.created_at, r.id`, f.projectID, wantPRs)
	if err != nil {
		t.Fatalf("read stored reviews: %v", err)
	}
	defer rows.Close()
	var stored []storedReview
	for rows.Next() {
		var s storedReview
		var id, reviewer, proposed pgtype.UUID
		if err := rows.Scan(&s.number, &proposed, &id, &reviewer, &s.kind, &s.decision, &s.createdAt); err != nil {
			t.Fatalf("scan stored review: %v", err)
		}
		s.proposed = pgUUIDTextTest(proposed)
		s.id = pgUUIDTextTest(id)
		s.reviewerID = pgUUIDTextTest(reviewer)
		stored = append(stored, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read stored reviews: %v", err)
	}

	var manifestRows []storedReview
	for _, rec := range manifest.Reviews {
		for _, r := range rec.Reviews {
			manifestRows = append(manifestRows, storedReview{
				number:     rec.PullRequestNumber,
				proposed:   rec.ProposedStateID,
				id:         r.ID,
				reviewerID: r.ReviewerID,
				kind:       r.ReviewKind,
				decision:   r.Decision,
				createdAt:  r.CreatedAt.UTC(),
			})
		}
	}
	sort.SliceStable(manifestRows, func(i, j int) bool { return manifestRows[i].id < manifestRows[j].id })
	storedByID := make(map[string]storedReview, len(stored))
	for _, s := range stored {
		storedByID[s.id] = s
	}
	if len(manifestRows) != len(stored) {
		t.Fatalf("%s: manifest carries %d reviews, the table holds %d", when, len(manifestRows), len(stored))
	}
	for _, m := range manifestRows {
		s, ok := storedByID[m.id]
		if !ok {
			t.Fatalf("%s: manifest carries review %s, which is not a stored row", when, m.id)
		}
		if m.number != s.number || m.proposed != s.proposed || m.reviewerID != s.reviewerID ||
			m.kind != s.kind || m.decision != s.decision || !m.createdAt.Equal(s.createdAt) {
			t.Fatalf("%s: manifest review %s = %+v, stored row = %+v", when, m.id, m, s)
		}
	}
}
