package integration

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/aborthttp"
	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/mergehttp"
	"github.com/lichman0405/post/cmd/api/policyhttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/cmd/api/pullrequestshttp"
	"github.com/lichman0405/post/cmd/api/reviewhttp"
	"github.com/lichman0405/post/cmd/api/rsghttp"

	"github.com/lichman0405/post/internal/application/aborts"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/diffs"
	"github.com/lichman0405/post/internal/application/merge"
	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/prchecks"
	"github.com/lichman0405/post/internal/application/prdiff"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/application/reopens"
	"github.com/lichman0405/post/internal/application/resolutions"
	"github.com/lichman0405/post/internal/application/responsibilities"
	"github.com/lichman0405/post/internal/application/reviews"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rsg/integrity"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

// T0610's required test — label: `reopen transition`.
//
// The reopen half of docs/43:10 / docs/46:11, end to end over REAL PostgreSQL:
// an object carried to 'aborted' ON MAIN by T0602's abort (over its contract
// route) plus a real Research PR merge, then carried to 'reopened' by this
// task's command plus a real Research PR merge, with every claim read back out
// of STORAGE column by column.
//
// # Where the reopen is driven from, and why it is not an HTTP route
//
// The abort half of this pair runs over its contract route (abortPath:
// specs/api/openapi.yaml's POST
// /projects/{projectId}/objects/{objectId}:abort-proposal). The reopen has no
// route to drive: `grep -n reopen specs/api/openapi.yaml` returns NOTHING — the
// contract declares one object-level proposal path and it is the abort's — and
// this task's scope note (tasks/packages/T0610.json) says the reopen contract
// route "需要在裁定之后确认" and that the Worker must report back rather than
// add a path to the contract; specs/api/** is forbidden scope besides. The
// owner's ruling of 2026-09-21 decided the permission row and said nothing
// about a route.
//
// The reopen is therefore driven through the COMMAND — constructed here
// exactly as cmd/api/main.go constructs the abort command, over the same
// production stores and the same authz.MatrixEngine — while everything after it
// (the reviews that walk the PR to merge_ready, the merge that lands the
// transition on main) runs over the REAL routes. This is the precedent the
// merge-governance suite set for a transition with no contract operation
// (merge_governance_e2e_test.go:511-514: "docs/43's open → review_required
// transition has no route in this build ... inventing one would be inventing a
// product action"), and the RESULT names it as a fail-closed judgement rather
// than letting it pass silently.
//
// # What is asserted, and from where
//
// Nothing below reads a service's return value as evidence of a stored fact:
// every claim is a SELECT against scientific_object_versions,
// scientific_objects, audit_log or outbox_events. The states are produced by
// the product — v1 by the RSG route, the abort by the abort route plus a merge
// over the merge route, the reopen by the reopen command plus a merge over the
// merge route. No lifecycle is written into storage by this test, which is the
// anti-example the task book names (freeze_main_e2e_test.go:849-853,
// pullrequest_test.go:331-336).

const reopenTaskID = "T0610"

// reopenReason and reopenExplanation are the two record fields this command
// requires (see internal/application/reopens's package doc: no sentence in docs/
// or specs/ says what a reopen records, so the shape is T0602's abort record
// minus the abort-only replacement ref). abortReason and abortExplanation are
// the abort's own record, so the row a reopen starts from carries a record this
// task's subject can be told apart from.
const (
	reopenReason      = "new_evidence_received"
	reopenExplanation = "The oxidation that invalidated this claim was traced to a faulty sample holder, and the 2026-09-19 re-run on the replaced holder reproduces the original band gap."
	abortReason       = "superseded_by_better_evidence"
	abortExplanation  = "The 2026-08-14 re-run of the sorption measurement contradicts this claim's band gap."
)

// ------------------------------------------------------------------ driving

// tryReopen issues one reopen through the command and hands back exactly what
// it answered — result and error both, so a refusal can be asserted rather than
// fatal to the test.
func (f *reopenFixture) tryReopen(ctx context.Context, actor reopens.Actor, objectID, versionID, key, reason, explanation string) (reopens.Result, error) {
	return f.reopens.ReopenProposal(ctx, actor, reopens.Input{
		ProjectID:        f.project.ID,
		ObjectID:         objectID,
		ObjectVersionRef: versionID,
		ReasonCode:       reason,
		Explanation:      explanation,
		IdempotencyKey:   key,
	})
}

// reopen runs one reopen proposal and fails the test if it is refused.
func (f *reopenFixture) reopen(t *testing.T, ctx context.Context, actor reopens.Actor, objectID, versionID, key string) reopens.Result {
	t.Helper()
	res, err := f.tryReopen(ctx, actor, objectID, versionID, key, reopenReason, reopenExplanation)
	if err != nil {
		t.Fatalf("reopen %s: %v", versionID, err)
	}
	return res
}

// ----------------------------------------------------------------- fixture

// reopenFixture is the platform graph, wired the way cmd/api wires it, over
// real PostgreSQL and behind the real auth guard: the abort fixture's graph plus
// this task's command, with the reopen reader wired into the merge exactly as
// cmd/api wires it.
type reopenFixture struct {
	pool          *pgxpool.Pool
	server        string
	project       domain.Project
	org           domain.Organization
	mainBranch    domain.Branch
	owner         *testUserClient
	ownerID       string
	maintainerID  string
	viewerID      string
	contributorID string
	strangerID    string
	// clients holds every acting browser by the email it signed up with.
	clients map[string]*testUserClient
	prs     *pullrequests.Service
	merges  *merge.Service
	aborts  *aborts.Service
	reopens *reopens.Service
}

func newReopenFixture(t *testing.T, ctx context.Context) *reopenFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), reopenTaskID)

	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	orgStore := persistence.NewOrgStore(pool)
	projectStore := persistence.NewProjectStore(pool)
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: projectStore,
		Orgs:  orgStore,
		Authz: authz.NewMatrixEngine(),
	})
	projectSvc := projectAPI.Service()
	policyStore := persistence.NewPolicyStore(pool)
	policyAPI := policyhttp.New(policyhttp.Deps{
		Store:    policyStore,
		Orgs:     orgStore,
		Projects: projectStore,
	})
	stateStore := persistence.NewStateStore(pool)
	branchSvc := branches.NewService(persistence.NewBranchStore(pool))
	statesSvc := states.NewService(stateStore, newCommitGuard(t))
	objects := persistence.NewScientificObjectStore(pool)
	rsgSvc := rsg.NewService(rsg.Deps{
		Projects:  projectSvc,
		Branches:  branchSvc,
		States:    statesSvc,
		Latest:    stateStore,
		Objects:   objects,
		Relations: persistence.NewRelationStore(pool),
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
		Events:    events.Recorder{},
	})
	// The proposals port is T0817's addition: the diff reads a PR's own row for
	// the external-fork case, where the proposal and the target state live in
	// different projects. Passed the same way every other integration fixture
	// passes it (abort_e2e_test.go:400 and friends).
	diffSvc := diffs.NewService(stateStore, persistence.NewManifestStore(pool), persistence.NewPullRequestStore(pool))
	resolutionSvc := resolutions.NewService(diffSvc, resolutions.NewPGStore(pool), projectSvc, authz.NewMatrixEngine())
	branchStore := persistence.NewBranchStore(pool)
	prStore := persistence.NewPullRequestStore(pool)
	prSvc := pullrequests.NewService(prStore)
	checksSvc := prchecks.NewService(prchecks.Deps{
		PRs:      persistence.NewPullRequestStore(pool),
		Projects: projectStore,
		States:   stateStore,
		Branches: persistence.NewBranchStore(pool),
		Manifest: persistence.NewManifestStore(pool),
		Policies: policyStore,
		Engine:   integrity.New(reg),
	})
	// The Research Owners stack and the review submission command, wired as
	// cmd/api wires them: the reviews that walk a proposal to merge_ready have
	// to be the ones the routing resolved, not a state this test assigns.
	routingSvc := responsibilities.NewService(responsibilities.Deps{
		Rules:     persistence.NewResponsibilityStore(pool),
		Projects:  projectStore,
		Members:   projectSvc,
		PRs:       prStore,
		Branches:  branchStore,
		Diffs:     prdiff.NewService(prStore, branchStore, diffSvc),
		Policies:  policyStore,
		Evaluator: policy.NewRuleEvaluator(),
	})
	reviewsSvc := reviews.NewService(reviews.Deps{
		Repo:           persistence.NewReviewStore(pool),
		Projects:       projectSvc,
		Authz:          authz.NewMatrixEngine(),
		Responsibility: routingSvc,
		Routing:        routingSvc,
	})
	mergeSvc := merge.NewService(merge.Deps{
		Store:     persistence.NewSemanticMergeStore(pool),
		Diffs:     diffSvc,
		Plans:     resolutionSvc,
		Commits:   statesSvc,
		Objects:   objects,
		Relations: persistence.NewRelationStore(pool),
		Projects:  projectSvc,
		Authz:     authz.NewMatrixEngine(),
		Checks:    checksSvc,
		Policies:  policyAPI.Service(),
		Rules:     policy.NewRuleEvaluator(),
		Events:    events.Recorder{},
		// Both readers are wired exactly as cmd/api wires them: the merge
		// copies docs/46:7's abort record AND this task's reopen record onto
		// the row it lands on main. Without the reopen reader the merge would
		// materialize a 'reopened' version whose deciding actor and time exist
		// nowhere — see internal/application/merge/ports.go.
		Aborts:  objects,
		Reopens: objects,
	})
	abortSvc := aborts.NewService(aborts.Deps{
		Members:      persistence.NewProjectStore(pool),
		Authz:        authz.NewMatrixEngine(),
		Objects:      objects,
		Branches:     branchSvc,
		PullRequests: prSvc,
		Commits:      statesSvc,
		Events:       events.Recorder{},
	})
	// The command under test, wired from the same components cmd/api/main.go
	// wires it from.
	reopenSvc := reopens.NewService(reopens.Deps{
		Members:      persistence.NewProjectStore(pool),
		Authz:        authz.NewMatrixEngine(),
		Objects:      objects,
		Branches:     branchSvc,
		PullRequests: prSvc,
		Commits:      statesSvc,
		Events:       events.Recorder{},
	})

	authAPI := authhttp.New(authhttp.Deps{
		Users:    persistence.NewCredentialStore(pool),
		Sessions: memstore.NewSessions(),
		Limiter:  memstore.NewLimiter(),
		Cfg: authn.Config{
			WebOrigin:          "http://web.test",
			SessionTTL:         time.Hour,
			LoginLimitPerEmail: 1000,
			LoginLimitPerIP:    10000,
			// The suite signs up several users per fixture against one loopback
			// address; the signup limiter is production's, and leaving it at
			// its production value would rate-limit the fixture rather than
			// the product.
			SignupLimitPerIP: 10000,
			LoginWindow:      time.Minute,
		},
		Secure: false,
		Audit:  persistence.NewAuditStore(pool),
	})
	apiMux := http.NewServeMux()
	authAPI.Register(apiMux)
	apiMux.Handle("/api/v1/projects", projectAPI.Routes())
	apiMux.Handle("/api/v1/projects/", projectAPI.Routes())
	rsghttp.New(rsghttp.Deps{Service: rsgSvc}).Register(apiMux)
	pullrequestshttp.New(pullrequestshttp.Deps{
		PullRequests: prSvc,
		Checks:       checksSvc,
	}).Register(apiMux)
	mergehttp.New(mergehttp.Deps{Command: mergeSvc, Projects: projectSvc}).Register(apiMux)
	aborthttp.New(aborthttp.Deps{Command: abortSvc}).Register(apiMux)
	reviewhttp.New(reviewhttp.Deps{Service: reviewsSvc, Projects: projectSvc}).Register(apiMux)
	ts := httptest.NewServer(authAPI.Guard(apiMux))
	t.Cleanup(ts.Close)

	// Every acting browser is created through the real signup endpoint: the
	// cookies and CSRF tokens below are the ones that endpoint minted.
	owner, ownerID := signup(t, ts.URL, "reopen-owner@example.com", "reopen-owner")
	maintainer, maintainerID := signup(t, ts.URL, "reopen-maintainer@example.com", "reopen-maintainer")
	viewer, viewerID := signup(t, ts.URL, "reopen-viewer@example.com", "reopen-viewer")
	contrib, contribID := signup(t, ts.URL, "reopen-contrib@example.com", "reopen-contrib")
	stranger, strangerID := signup(t, ts.URL, "reopen-stranger@example.com", "reopen-stranger")

	org, _, err := orgStore.CreateOrganization(ctx, domain.Organization{
		Slug: "reopen-gov", Name: "Reopen Governance",
	}, ownerID, todayUTC())
	if err != nil {
		t.Fatalf("create the fixture organization: %v", err)
	}
	project, _, err := projectStore.CreateProject(ctx, domain.Project{
		OrganizationID:  &org.ID,
		Slug:            "reopen-gov",
		Name:            "Reopen Governance",
		Purpose:         "T0610 reopen main object e2e",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, ownerID)
	if err != nil {
		t.Fatalf("create the fixture project: %v", err)
	}
	// The three roles the permission criterion walks; the stranger gets no
	// membership at all, which is what makes the non-member cell the cell under
	// test. The memberships are seeded directly — no route in this build
	// changes a role — while every ACTOR below is a real session.
	for _, seed := range []struct{ userID, role string }{
		{maintainerID, "maintainer"}, {viewerID, "viewer"}, {contribID, "contributor"},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, $3)`,
			project.ID, seed.userID, seed.role); err != nil {
			t.Fatalf("seed the %s membership: %v", seed.role, err)
		}
	}
	// The routing data the required-review calculation runs on: the changes
	// these proposals carry are claims, and both reviewers hold the label the
	// rule routes them to (domain.EvaluateRequiredReviews accepts no empty
	// label, so a reviewer holding no responsibility cannot sign under one).
	// Fixture data — the REVIEWS themselves are HTTP submissions.
	ownerUser := domain.User{ID: ownerID}
	for _, userID := range []string{viewerID, ownerID} {
		if _, err := routingSvc.Assign(ctx, ownerUser, project.ID, userID, "Data Reviewer"); err != nil {
			t.Fatalf("assign the reviewer responsibility to %s: %v", userID, err)
		}
	}
	if _, err := routingSvc.AddRule(ctx, ownerUser, project.ID, responsibilities.AddRuleInput{
		MatchKind:      domain.ResearchOwnerMatchObjectType,
		MatchValue:     "claim",
		Responsibility: "Data Reviewer",
	}); err != nil {
		t.Fatalf("route claim changes to the reviewer: %v", err)
	}
	// The merge's governance check reads main_protected from the policy in
	// force and refuses a policy that does not mention it.
	seedProjectPolicy(t, ctx, policyStore, project, ownerID)

	mainBranch, err := rsgSvc.CreateBranch(ctx, domain.User{ID: ownerID}, project.ID, rsg.CreateBranchInput{
		Name: "main", BaseRef: "", Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create the main branch: %v", err)
	}

	return &reopenFixture{
		pool:       pool,
		server:     ts.URL,
		project:    project,
		org:        org,
		mainBranch: mainBranch,
		owner:      owner,
		ownerID:    ownerID,
		clients: map[string]*testUserClient{
			"reopen-owner@example.com":      owner,
			"reopen-maintainer@example.com": maintainer,
			"reopen-viewer@example.com":     viewer,
			"reopen-contrib@example.com":    contrib,
			"reopen-stranger@example.com":   stranger,
		},
		maintainerID:  maintainerID,
		viewerID:      viewerID,
		contributorID: contribID,
		strangerID:    strangerID,
		prs:           prSvc,
		merges:        mergeSvc,
		aborts:        abortSvc,
		reopens:       reopenSvc,
	}
}

func (f *reopenFixture) clientFor(t *testing.T, email string) *testUserClient {
	t.Helper()
	c, ok := f.clients[email]
	if !ok {
		t.Fatalf("no client for %s", email)
	}
	return c
}

// viewer is the fixture's non-owner client that driveToMergeReady (below)
// drives a review with: it is read back out of the fixture by the email it
// signed up with (clientFor), not re-signed-up, so the review comes from the
// same session the membership was seeded for.
//
// It is the only typed accessor, because it is the only non-owner session a
// request below is issued WITH. The other classes the permission criterion
// walks (contributor, maintainer, non-member) are exercised by IDENTITY —
// reopens.Actor{User: domain.User{ID: ...}} over the ids those sessions signed
// up as — because the reopen command is deliberately mounted on no route in
// this build (the header's fail-closed note: the contract has no reopen
// operation, so there is no endpoint to send their sessions to). Accessors for
// them would be unreachable code, which is what staticcheck's U1000 said of
// them.
func (f *reopenFixture) viewer(t *testing.T) *testUserClient {
	t.Helper()
	return f.clientFor(t, "reopen-viewer@example.com")
}

// activeMainObject writes an object onto main through the production RSG route
// — the state every scenario here starts from — and returns its id and version
// id.
func (f *reopenFixture) activeMainObject(t *testing.T, ctx context.Context, statement string) (objectID, versionID string) {
	t.Helper()
	status, body := wirePost(t, f.owner,
		branchObjectsPath(f.project.ID, f.mainBranch.ID),
		fmt.Sprintf(`{"object_type":"claim","payload":%s}`, mergeMainGateClaim(statement)))
	if status != http.StatusCreated {
		t.Fatalf("write the fixture object onto main = %d: %s", status, body)
	}
	var obj struct {
		ID        string `json:"id"`
		VersionID string `json:"version_id"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode the object payload: %v (%s)", err, body)
	}
	if obj.ID == "" || obj.VersionID == "" {
		t.Fatalf("the object route answered without an id or a version id: %s", body)
	}
	_ = ctx
	return obj.ID, obj.VersionID
}

// abortOnMain takes an object from 'active on main' to 'aborted on main' the
// way docs/46:9 requires, and through the PRODUCT: the abort route proposes, the
// proposal's reviews are submitted over the review route, and the merge route
// lands it. It returns the ACCEPTED (aborted, on main) version row's id and
// number — the row a reopen starts from.
func (f *reopenFixture) abortOnMain(t *testing.T, ctx context.Context, objectID, versionID, key string) (string, int) {
	t.Helper()
	resp := f.owner.doKeyed(t, http.MethodPost, abortPath(f.project.ID, objectID),
		abortBody(versionID, abortReason, abortExplanation, ""), key)
	mustStatus(t, resp, http.StatusCreated)
	var wire abortWire
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		t.Fatalf("decode the abort payload: %v", err)
	}
	f.driveToMergeReady(t, ctx, wire.PullRequestNumber, "the abort T0610's reopen starts from")
	f.merge(t, f.owner, wire.PullRequestNumber, key+"-merge")

	rows := versionsOfObject(t, ctx, f.pool, objectID)
	accepted := rows[len(rows)-1]
	if accepted.lifecycleState != string(domain.LifecycleAborted) {
		t.Fatalf("the abort did not land on main: the head version is %q", accepted.lifecycleState)
	}
	if accepted.branchID == nil || *accepted.branchID != f.mainBranch.ID {
		t.Fatalf("the accepted aborted version is not on main: branch_id = %v", accepted.branchID)
	}
	if head := objectHeadVersionNo(t, ctx, f.pool, objectID); head != accepted.versionNo {
		t.Fatalf("the accepted aborted version is v%d but the object's head is v%d", accepted.versionNo, head)
	}
	return accepted.id, accepted.versionNo
}

// driveToMergeReady takes a proposal to the only state the merge engine accepts
// BY SUBMITTING ITS REVIEWS, over the review route — the walk
// merge_governance_e2e_test.go and the abort suite perform. The one direct call
// left is RequestReview, for the reason merge_governance_e2e_test.go:511-514
// records: docs/43's open → review_required transition has no route in this
// build (no contract operation, no request_review cell in the permission
// matrix — inventing one would be inventing a product action).
func (f *reopenFixture) driveToMergeReady(t *testing.T, ctx context.Context, number int64, note string) {
	t.Helper()
	if _, err := f.prs.RequestReview(ctx, f.project.ID, number); err != nil {
		t.Fatalf("request review on PR %d: %v", number, err)
	}
	for _, submission := range []struct {
		client *testUserClient
		kind   string
	}{
		{f.viewer(t), "scientific"},
		{f.owner, "integrity"},
	} {
		body := fmt.Sprintf(`{"kind":%q,"decision":"approved","body":%q}`, submission.kind, "reviewed for the T0610 reopen e2e: "+note)
		resp := submission.client.do(t, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/pull-requests/%d/reviews", f.project.ID, number), body)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("submit the %s review of PR %d = %d: %s", submission.kind, number, resp.StatusCode, readAll(t, resp))
		}
	}
	// The state the merge will read is the store's, not this test's: if the
	// reviews did not satisfy the calculation, the proposal is still parked and
	// the merge below would refuse it. It is read out of the ROW, the way every
	// other assertion here reads its subject.
	var state string
	if err := f.pool.QueryRow(ctx,
		`SELECT state FROM pull_requests WHERE project_id = $1::uuid AND number = $2`,
		f.project.ID, number).Scan(&state); err != nil {
		t.Fatalf("read PR %d after its reviews: %v", number, err)
	}
	if state != string(domain.PullRequestStateMergeReady) {
		t.Fatalf("PR %d is %q after its reviews, want %q: the review submissions did not walk the machine",
			number, state, domain.PullRequestStateMergeReady)
	}
}

// merge lands a proposal through the production merge route.
func (f *reopenFixture) merge(t *testing.T, uc *testUserClient, number int64, key string) mergeE2EPayload {
	t.Helper()
	return mergeThroughTheEndpoint(t, uc, f.project.ID, number, key)
}

// ---------------------------------------------------------------- storage

// xminOf reads the transaction that wrote one row, through PostgreSQL's xmin
// system column. It is how "in the same transaction" is asserted as a FACT
// about the rows rather than as a claim about the code's shape: two rows one
// transaction wrote carry the same xmin, and rows two transactions wrote do
// not. The query is a constant from this file with its own bound parameters.
func xminOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, args ...any) string {
	t.Helper()
	var xmin string
	if err := pool.QueryRow(ctx, query, args...).Scan(&xmin); err != nil {
		t.Fatalf("read the writing transaction of %s: %v", query, err)
	}
	return xmin
}

// objectHeadVersionNo reads the object's version counter — the log's head — so
// a refusal can be attributed to the check that produced it rather than to
// whichever check happens to run first.
func objectHeadVersionNo(t *testing.T, ctx context.Context, pool *pgxpool.Pool, objectID string) int {
	t.Helper()
	var head int
	if err := pool.QueryRow(ctx,
		`SELECT current_version_no FROM scientific_objects WHERE id = $1::uuid`, objectID).Scan(&head); err != nil {
		t.Fatalf("read the version counter of %s: %v", objectID, err)
	}
	return head
}

// reopenCounts counts the reopen command's two records for one object: the
// audit rows it wrote and the domain events it recorded. Both are counts, not
// existence checks, because the idempotency criterion is about how MANY were
// written.
func reopenCounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool, objectID string) (audits, outbox int) {
	t.Helper()
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE action = $1 AND target_ref = $2`,
		domain.ActionScientificObjectReopened, "object:"+objectID).Scan(&audits); err != nil {
		t.Fatalf("count the reopen audit rows: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox_events WHERE event_type = $1 AND payload->>'object_id' = $2`,
		"scientific_object.reopened", objectID).Scan(&outbox); err != nil {
		t.Fatalf("count the scientific_object.reopened events: %v", err)
	}
	return audits, outbox
}

// reopenAuditRow is the one audit row a reopen appended, as the archive holds
// it.
type reopenAuditRow struct {
	actorID string
	target  string
	before  []byte
	after   []byte
}

func readReopenAudit(t *testing.T, ctx context.Context, pool *pgxpool.Pool, objectID string) reopenAuditRow {
	t.Helper()
	var r reopenAuditRow
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(actor_id::text, ''), COALESCE(target_ref, ''),
		        COALESCE(before_summary, 'null'::jsonb), COALESCE(after_summary, 'null'::jsonb)
		   FROM audit_log WHERE action = $1 AND target_ref = $2`,
		domain.ActionScientificObjectReopened, "object:"+objectID).Scan(&r.actorID, &r.target, &r.before, &r.after); err != nil {
		t.Fatalf("read the reopen audit row: %v", err)
	}
	return r
}

// storedReopens returns every version row of the object that is in the state a
// reopen produces, oldest first. It is how "the refused call produced no
// reopen" is asserted: a count of rows in that state in STORAGE, not the
// absence of a return value.
func storedReopens(t *testing.T, ctx context.Context, pool *pgxpool.Pool, objectID string) []versionRow {
	t.Helper()
	var out []versionRow
	for _, row := range versionsOfObject(t, ctx, pool, objectID) {
		if row.lifecycleState == string(domain.LifecycleReopened) || row.reopenRequestKey != nil {
			out = append(out, row)
		}
	}
	return out
}

// -------------------------------------------------------------------- test

// TestReopenProposalEndToEnd is T0610's required end-to-end test: label
// `reopen transition`.
func TestReopenProposalEndToEnd(t *testing.T) {
	ctx := testCtx(t)
	f := newReopenFixture(t, ctx)
	pool := f.pool

	objectID, v1ID := f.activeMainObject(t, ctx, "a claim that will be aborted and then reopened")
	v1Before := readVersion(t, ctx, pool, v1ID)

	// ---- The state the reopen starts from, reached by the PRODUCT: the abort
	// route proposes and a real merge lands 'aborted' on main.
	abortedID, abortedNo := f.abortOnMain(t, ctx, objectID, v1ID, "t0610-abort-0001")
	abortedBefore := readVersion(t, ctx, pool, abortedID)
	mainHeadBeforeReopen := branchHead(t, ctx, pool, f.mainBranch.ID)

	// The accepted row carries the aborting actor's record and no reopen
	// record: it is the row criterion (2) below compares against afterwards.
	if abortedBefore.abortReasonCode == nil || *abortedBefore.abortReasonCode != abortReason {
		t.Fatalf("the accepted aborted version carries no abort record: %s", abortedBefore.describe())
	}
	if abortedBefore.abortedBy == nil || *abortedBefore.abortedBy != f.ownerID {
		t.Fatalf("the accepted aborted version does not name the aborting actor: %s", abortedBefore.describe())
	}
	if abortedBefore.reopenReasonCode != nil || abortedBefore.reopenedBy != nil {
		t.Fatalf("the aborted version already carries a reopen record: %s", abortedBefore.describe())
	}

	// ---- (1) The reopen proposal, through the command. The lifecycle it
	// reports is the ROW's; main carries nothing until the PR below merges.
	ownerActor := reopens.Actor{User: domain.User{ID: f.ownerID}}
	const reopenKey = "t0610-reopen-0001"
	res := f.reopen(t, ctx, ownerActor, objectID, abortedID, reopenKey)

	if res.Replayed {
		t.Fatal("(1) the first reopen reported itself as a replay")
	}
	if res.LifecycleState != string(domain.LifecycleReopened) {
		t.Fatalf("(1) the proposal's version row is in lifecycle %q, want reopened", res.LifecycleState)
	}
	if res.AbortedVersionID != abortedID || res.AbortedVersionNo != abortedNo {
		t.Fatalf("(1) the proposal names version %s v%d as the one being reopened, want %s v%d",
			res.AbortedVersionID, res.AbortedVersionNo, abortedID, abortedNo)
	}
	if res.VersionID == abortedID || res.VersionNo <= abortedNo {
		t.Fatalf("(1) the reopen did not append: it reported version %s v%d against the aborted %s v%d",
			res.VersionID, res.VersionNo, abortedID, abortedNo)
	}
	if res.BranchID == "" || res.BranchName == "" {
		t.Fatalf("(1) the proposal names no branch: %+v", res)
	}
	if res.PullRequestNumber == 0 {
		t.Fatalf("(1) the proposal opened no Research PR: %+v", res)
	}
	if res.ReasonCode != reopenReason || res.Explanation != reopenExplanation {
		t.Fatalf("(1) the proposal's record = (%q, %q), want the request's", res.ReasonCode, res.Explanation)
	}
	if res.DecidedBy != f.ownerID {
		t.Fatalf("(1) decided_by = %q, want the reopening actor %q", res.DecidedBy, f.ownerID)
	}
	if res.DecidedAt.IsZero() {
		t.Fatal("(1) decided_at is zero: the decision time is server-derived and must be recorded")
	}

	// ---- (1) The same facts as STORED, read column by column.
	stored := readVersion(t, ctx, pool, res.VersionID)
	if stored.lifecycleState != string(domain.LifecycleReopened) {
		t.Fatalf("(1) the stored version's lifecycle_state = %q, want reopened", stored.lifecycleState)
	}
	if stored.reopenReasonCode == nil || *stored.reopenReasonCode != reopenReason {
		t.Fatalf("(1) stored reopen_reason_code = %v, want %q", stored.reopenReasonCode, reopenReason)
	}
	if stored.reopenExplanation == nil || *stored.reopenExplanation != reopenExplanation {
		t.Fatalf("(1) stored reopen_explanation = %v, want the explanation", stored.reopenExplanation)
	}
	if stored.reopenedBy == nil || *stored.reopenedBy != f.ownerID {
		t.Fatalf("(1) stored reopened_by = %v, want the reopening actor %q", stored.reopenedBy, f.ownerID)
	}
	if stored.reopenedAt == nil {
		t.Fatal("(1) stored reopened_at is NULL")
	}
	if stored.reopenRequestKey == nil || *stored.reopenRequestKey != reopenKey {
		t.Fatalf("(1) stored reopen_request_key = %v, want the request's Idempotency-Key %q", stored.reopenRequestKey, reopenKey)
	}
	if stored.abortReasonCode != nil || stored.abortedBy != nil {
		t.Fatalf("(1) the reopened version carries a half-copied ABORT record: %s", stored.describe())
	}
	if stored.branchID == nil || *stored.branchID != res.BranchID {
		t.Fatalf("(1) the stored version is not on the branch the proposal named: %v vs %s", stored.branchID, res.BranchID)
	}
	if !bytes.Equal(stored.payload, v1Before.payload) {
		t.Fatalf("(1) the reopen changed the research content: %s vs %s", stored.payload, v1Before.payload)
	}

	// ---- (1) Nothing reached main yet. docs/09:9-10 is not a formality: main
	// advances by a Research PR merge and by nothing else, so until the PR
	// below merges there is no reopen on main at all.
	if now := branchHead(t, ctx, pool, f.mainBranch.ID); now != mainHeadBeforeReopen {
		t.Fatalf("(1) main's head moved to %s during the reopen proposal; the command must not write to main", now)
	}
	if row := readVersion(t, ctx, pool, abortedID); row.lifecycleState != string(domain.LifecycleAborted) {
		t.Fatalf("(1) the aborted version on main is now %q; the proposal must not change it", row.lifecycleState)
	}

	// ---- (8) One reopen wrote exactly one audit row and exactly one event.
	if audits, outbox := reopenCounts(t, ctx, pool, objectID); audits != 1 || outbox != 1 {
		t.Fatalf("(8) one reopen wrote %d audit rows and %d events, want 1 and 1", audits, outbox)
	}

	// ---- (9) The idempotent replay: the same key answers with the proposal it
	// already created and writes NOTHING — the counts are the evidence.
	replay := f.reopen(t, ctx, ownerActor, objectID, abortedID, reopenKey)
	if !replay.Replayed {
		t.Fatal("(9) the repeated request did not report itself as a replay")
	}
	if replay.VersionID != res.VersionID || replay.VersionNo != res.VersionNo {
		t.Fatalf("(9) the replay answered version %s v%d, want the first answer's %s v%d",
			replay.VersionID, replay.VersionNo, res.VersionID, res.VersionNo)
	}
	if replay.PullRequestNumber != res.PullRequestNumber {
		t.Fatalf("(9) the replay answered PR %d, want the first answer's %d", replay.PullRequestNumber, res.PullRequestNumber)
	}
	if replay.BranchID != res.BranchID || replay.BranchName != res.BranchName {
		t.Fatalf("(9) the replay answered branch %s/%s, want %s/%s", replay.BranchID, replay.BranchName, res.BranchID, res.BranchName)
	}
	if replay.AbortedVersionID != abortedID || replay.ReasonCode != reopenReason ||
		replay.Explanation != reopenExplanation || replay.DecidedBy != f.ownerID {
		t.Fatalf("(9) the replay answered a different record: %+v", replay)
	}
	if audits, outbox := reopenCounts(t, ctx, pool, objectID); audits != 1 || outbox != 1 {
		t.Fatalf("(9) the replay wrote a second record: %d audit rows and %d events, want 1 and 1", audits, outbox)
	}
	if n := len(versionsOfObject(t, ctx, pool, objectID)); n != 4 {
		t.Fatalf("(9) the object has %d version rows after the replay, want 4 (v1 + abort proposal + accepted abort + reopen proposal)", n)
	}

	// ---- (7) The user-visible audit row: docs/26 lists abort/reopen among the
	// highest-risk audited actions and docs/53 makes the row part of the
	// action.
	audit := readReopenAudit(t, ctx, pool, objectID)
	if audit.actorID != f.ownerID {
		t.Fatalf("(7) the audit row's actor = %q, want %q", audit.actorID, f.ownerID)
	}
	var after map[string]any
	if err := json.Unmarshal(audit.after, &after); err != nil {
		t.Fatalf("(7) decode the audit row's after_summary: %v (%s)", err, audit.after)
	}
	if after["reason_code"] != reopenReason || after["explanation"] != reopenExplanation {
		t.Fatalf("(7) the audit row's record = (%v, %v), want the request's", after["reason_code"], after["explanation"])
	}
	if after["lifecycle_state"] != string(domain.LifecycleReopened) {
		t.Fatalf("(7) the audit row's after lifecycle_state = %v, want reopened", after["lifecycle_state"])
	}
	if after["idempotency_key"] != reopenKey {
		t.Fatalf("(7) the audit row does not name the request that produced it: %v", after["idempotency_key"])
	}
	var before map[string]any
	if err := json.Unmarshal(audit.before, &before); err != nil {
		t.Fatalf("(7) decode the audit row's before_summary: %v (%s)", err, audit.before)
	}
	if before["lifecycle_state"] != string(domain.LifecycleAborted) {
		t.Fatalf("(7) the audit row's before lifecycle_state = %v, want aborted", before["lifecycle_state"])
	}
	// ---- (7) The three rows one reopen writes — the version, its audit row and
	// its event — share ONE transaction, and the instrument is shown to be able
	// to tell two transactions apart before it is trusted: the aborted version
	// was written by the merge's transaction and must NOT carry the reopen's
	// xmin. Without that contrast, "all three xmins are equal" would be equally
	// true of a database that assigned one xmin to everything.
	versionTx := xminOf(t, ctx, pool,
		`SELECT xmin::text FROM scientific_object_versions WHERE id = $1::uuid`, res.VersionID)
	auditTx := xminOf(t, ctx, pool,
		`SELECT xmin::text FROM audit_log WHERE action = $1 AND target_ref = $2`,
		domain.ActionScientificObjectReopened, "object:"+objectID)
	eventTx := xminOf(t, ctx, pool,
		`SELECT xmin::text FROM outbox_events WHERE event_type = $1 AND payload->>'object_id' = $2`,
		"scientific_object.reopened", objectID)
	abortedTx := xminOf(t, ctx, pool,
		`SELECT xmin::text FROM scientific_object_versions WHERE id = $1::uuid`, abortedID)
	if versionTx != auditTx || versionTx != eventTx {
		t.Fatalf("(7) the reopen's version (tx %s), audit row (tx %s) and event (tx %s) were NOT written by one transaction",
			versionTx, auditTx, eventTx)
	}
	if abortedTx == versionTx {
		t.Fatalf("(7) the aborted version and the reopen report the same writing transaction (%s): this instrument cannot tell two transactions apart, so the equality above proves nothing",
			abortedTx)
	}

	// ---- (6) The proposal merges onto main, and the reopen takes effect
	// there. docs/43:10's 'aborted → reopened' edge is effective only here.
	f.driveToMergeReady(t, ctx, res.PullRequestNumber, "the reopen proposal")
	merged := f.merge(t, f.owner, res.PullRequestNumber, "t0610-reopen-merge-0001")
	if merged.Replayed {
		t.Fatal("(6) the merge of the reopen proposal reported itself as a replay")
	}

	rows := versionsOfObject(t, ctx, pool, objectID)
	if len(rows) != 5 {
		t.Fatalf("(6) the object has %d version rows after the merge, want 5 (v1 + abort proposal + accepted abort + reopen proposal + accepted reopen)", len(rows))
	}
	accepted := rows[4]
	if accepted.lifecycleState != string(domain.LifecycleReopened) {
		t.Fatalf("(6) the accepted version's lifecycle_state = %q, want reopened", accepted.lifecycleState)
	}
	if accepted.branchID == nil || *accepted.branchID != f.mainBranch.ID {
		t.Fatalf("(6) the accepted version is not on main: branch_id = %v", accepted.branchID)
	}
	if head := objectHeadVersionNo(t, ctx, pool, objectID); head != accepted.versionNo {
		t.Fatalf("(6) the accepted version is v%d but the object's head is v%d", accepted.versionNo, head)
	}
	// The merge COPIES the decision rather than re-deciding it: the row on main
	// carries the reopening actor and time, not the merging one.
	if accepted.reopenedBy == nil || *accepted.reopenedBy != f.ownerID {
		t.Fatalf("(6) the accepted version lost the reopening actor: %v", accepted.reopenedBy)
	}
	if accepted.reopenedAt == nil {
		t.Fatal("(6) the accepted version lost the decision time")
	}
	if accepted.reopenReasonCode == nil || *accepted.reopenReasonCode != reopenReason {
		t.Fatalf("(6) the accepted version lost the reason code: %v", accepted.reopenReasonCode)
	}
	if accepted.reopenExplanation == nil || *accepted.reopenExplanation != reopenExplanation {
		t.Fatalf("(6) the accepted version lost the explanation: %v", accepted.reopenExplanation)
	}
	if accepted.reopenRequestKey != nil {
		t.Fatalf("(6) the accepted version carries the request's Idempotency-Key (%v); the key belongs to the request that was made, not to the row the merge landed",
			*accepted.reopenRequestKey)
	}
	if !bytes.Equal(accepted.payload, v1Before.payload) {
		t.Fatalf("(6) the accepted version's payload is not the claim's own content: %s", accepted.payload)
	}

	// ---- (2) THE ABORT HISTORY IS PRESERVED, BYTE FOR BYTE. The aborted
	// version on main is read back column by column and compared with the
	// snapshot taken before the reopen; the reopen appended a row and edited
	// nothing (docs/46:11).
	if now := readVersion(t, ctx, pool, abortedID); now.describe() != abortedBefore.describe() {
		t.Fatalf("(2) the aborted version was REWRITTEN by the reopen:\n before: %s\n  after: %s",
			abortedBefore.describe(), now.describe())
	}
	// The same claim for the log's first row, plus a count of the rows in the
	// state the reopen produces: two (the proposal's and the accepted one),
	// which is what "a new transition" means.
	if after := versionsOfObject(t, ctx, pool, objectID); after[0].describe() != v1Before.describe() {
		t.Fatalf("(2) the reopen rewrote v1:\n before: %s\n  after: %s", v1Before.describe(), after[0].describe())
	}
	if n := len(storedReopens(t, ctx, pool, objectID)); n != 2 {
		t.Fatalf("(2) the object has %d rows in lifecycle 'reopened', want 2 (the proposal's and the accepted one)", n)
	}

	// ---- (8) The event: exactly one, carrying identity and reference only.
	var rawPayload []byte
	if err := pool.QueryRow(ctx,
		`SELECT payload FROM outbox_events WHERE event_type = $1 AND payload->>'object_id' = $2`,
		"scientific_object.reopened", objectID).Scan(&rawPayload); err != nil {
		t.Fatalf("(8) read the reopen event's payload: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(rawPayload, &payload); err != nil {
		t.Fatalf("(8) decode the reopen event's payload: %v (%s)", err, rawPayload)
	}
	if payload["reason_code"] != reopenReason || payload["object_type"] != "claim" {
		t.Fatalf("(8) the event payload = %v, want the claim and the reason code", payload)
	}
	if payload["reopened_version_no"] != float64(abortedNo) {
		t.Fatalf("(8) the event's reopened_version_no = %v, want the aborted version's number %d", payload["reopened_version_no"], abortedNo)
	}
	if _, present := payload["explanation"]; present {
		t.Fatalf("(8) the event payload carries the free-text explanation; it carries identity and reference only: %v", payload)
	}
	if audits, outbox := reopenCounts(t, ctx, pool, objectID); audits != 1 || outbox != 1 {
		t.Fatalf("(8) after the merge the reopen's records number %d audits and %d events, want 1 and 1", audits, outbox)
	}

	// ---- (3) THE STATE MACHINE HAS ONE ROAD IN. Two refusals, and each one's
	// precondition is asserted first so the refusal is attributed to the check
	// that produced it:
	//
	//   (a) an object that was never aborted is not reopenable;
	//   (b) an object that is already reopened is not reopenable again.
	neverAbortedID, neverAbortedV1 := f.activeMainObject(t, ctx, "a claim nobody ever aborted")
	if head := objectHeadVersionNo(t, ctx, pool, neverAbortedID); head != 1 {
		t.Fatalf("(3a) the never-aborted object's head is v%d, want v1", head)
	}
	if got := readVersion(t, ctx, pool, neverAbortedV1).lifecycleState; got != string(domain.LifecycleActive) {
		t.Fatalf("(3a) the never-aborted object's current version is %q, want active — the refusal below would otherwise come from another check", got)
	}
	if _, err := f.tryReopen(ctx, ownerActor, neverAbortedID, neverAbortedV1, "t0610-reopen-active-0001", reopenReason, reopenExplanation); err == nil {
		t.Fatal("(3a) a reopen of a never-aborted active object was permitted")
	} else if !errors.Is(err, reopens.ErrVersionNotFound) {
		t.Fatalf("(3a) the refusal of a never-aborted object is %v, want %v", err, reopens.ErrVersionNotFound)
	}
	if n := len(storedReopens(t, ctx, pool, neverAbortedID)); n != 0 {
		t.Fatalf("(3a) the refused call left %d reopened rows in storage", n)
	}
	// (b) The accepted version on main is the object's CURRENT version and it
	// is 'reopened': the edge admits only 'aborted' as its starting point, so
	// the lifecycle check is the only one that can refuse this.
	if head := objectHeadVersionNo(t, ctx, pool, objectID); head != accepted.versionNo {
		t.Fatalf("(3b) the accepted version is v%d but the object's head is v%d", accepted.versionNo, head)
	}
	if _, err := f.tryReopen(ctx, ownerActor, objectID, accepted.id, "t0610-reopen-again-0001", reopenReason, reopenExplanation); err == nil {
		t.Fatal("(3b) a second reopen of an already-reopened object was permitted")
	} else if !errors.Is(err, reopens.ErrVersionNotFound) {
		t.Fatalf("(3b) the refusal of an already-reopened object is %v, want %v", err, reopens.ErrVersionNotFound)
	}
	if audits, outbox := reopenCounts(t, ctx, pool, objectID); audits != 1 || outbox != 1 {
		t.Fatalf("(3) the refused calls wrote records: %d audits and %d events, want 1 and 1 (the permitted reopen's)", audits, outbox)
	}
	if got := readVersion(t, ctx, pool, accepted.id).lifecycleState; got != string(domain.LifecycleReopened) {
		t.Fatalf("(3) the accepted version is %q after the refused calls, want reopened", got)
	}
	if got := len(versionsOfObject(t, ctx, pool, objectID)); got != 5 {
		t.Fatalf("(3) the refused calls appended versions: %d rows, want 5", got)
	}
	// And one more version the command must refuse: the ABORT PROPOSAL's row,
	// which is aborted but is not the object's current version (the merge left
	// it on the abort's own branch). A reopen of it would reopen nothing.
	abortProposal := versionsOfObject(t, ctx, pool, objectID)[1]
	if abortProposal.lifecycleState != string(domain.LifecycleAborted) ||
		abortProposal.branchID == nil || *abortProposal.branchID == f.mainBranch.ID {
		t.Fatalf("(3) the abort's proposal row is not what this probe needs: %s", abortProposal.describe())
	}
	if _, err := f.tryReopen(ctx, ownerActor, objectID, abortProposal.id, "t0610-reopen-old-0001", reopenReason, reopenExplanation); err == nil {
		t.Fatal("(3) a reopen of an aborted version that is not the object's current version was permitted")
	}

	// ---- (4) THE PERMISSION ROW, CELL BY CELL. The row the owner's ruling
	// fixed is checked against the canonical CSV first (internal/authz's own
	// drift test asserts the engine's table matches that CSV; this asserts the
	// row the ruling named is the row that is there), then each class is
	// exercised against the command.
	if err := assertRuledReopenRow(); err != nil {
		t.Fatalf("(4) %v", err)
	}
	for _, refuse := range []struct {
		name  string
		actor reopens.Actor
	}{
		{"viewer", reopens.Actor{User: domain.User{ID: f.viewerID}}},
		{"contributor", reopens.Actor{User: domain.User{ID: f.contributorID}}},
		{"non-member", reopens.Actor{User: domain.User{ID: f.strangerID}}},
		{"anonymous", reopens.Actor{}},
	} {
		t.Run("reopen refused for the "+refuse.name, func(t *testing.T) {
			_, err := f.tryReopen(ctx, refuse.actor, objectID, abortedID, "t0610-denied-key-0001", reopenReason, reopenExplanation)
			if err == nil {
				t.Fatalf("the %s reopen was permitted", refuse.name)
			}
			if !errors.Is(err, reopens.ErrForbidden) {
				t.Fatalf("the %s refusal is %v, want %v (the matrix's deny cell for reopen_main_object)", refuse.name, err, reopens.ErrForbidden)
			}
		})
	}
	// And the refusals are not a coincidence of these four callers: the
	// matrix's own answer for the same class is `deny`.
	for _, denied := range []struct {
		name     string
		known    bool
		role     *domain.ProjectRole
		verdict  authz.Verdict
		permitsF bool
	}{
		{"anonymous", false, nil, authz.VerdictDeny, false},
		{"non-member", true, nil, authz.VerdictDeny, false},
		{"viewer", true, rolePtr(domain.ProjectRoleViewer), authz.VerdictDeny, false},
		{"contributor", true, rolePtr(domain.ProjectRoleContributor), authz.VerdictDeny, false},
	} {
		decision, err := authz.NewMatrixEngine().Authorize(ctx, authz.Request{
			Action: authz.ActionReopenMainObject,
			Class:  authz.ClassOf(denied.known, denied.role, false),
		})
		if err != nil {
			t.Fatalf("(4) authorize reopen_main_object for a %s: %v", denied.name, err)
		}
		if decision.Verdict != denied.verdict || decision.Permits() != denied.permitsF {
			t.Fatalf("(4) the matrix's %s cell for reopen_main_object = %q (permits=%v), want %q (permits=%v)",
				denied.name, decision.Verdict, decision.Permits(), denied.verdict, denied.permitsF)
		}
	}
	// maintainer/owner: via_pr — the proposal IS the resolution, and it is
	// resolved only so far. The maintainer's own proposal is created and its PR
	// opened; nothing is effective until a merge, which this test does not
	// perform for this one. Its subject is a DIFFERENT object, so the counts
	// and row counts asserted above stay about the first.
	maintainerObjectID, maintainerV1 := f.activeMainObject(t, ctx, "a claim the maintainer will reopen")
	maintainerAbortedID, _ := f.abortOnMain(t, ctx, maintainerObjectID, maintainerV1, "t0610-abort-0002")
	mainHeadBeforeMaintainer := branchHead(t, ctx, pool, f.mainBranch.ID)
	maintainerRes, err := f.tryReopen(ctx, reopens.Actor{User: domain.User{ID: f.maintainerID}},
		maintainerObjectID, maintainerAbortedID, "t0610-reopen-maintainer-0001", reopenReason, reopenExplanation)
	if err != nil {
		t.Fatalf("(4) a maintainer's reopen was refused: %v — the matrix's cell for a maintainer is via_pr, and this command is the resolution", err)
	}
	if maintainerRes.PullRequestNumber == 0 || maintainerRes.LifecycleState != string(domain.LifecycleReopened) {
		t.Fatalf("(4) the maintainer's proposal is not a proposal: %+v", maintainerRes)
	}
	if now := branchHead(t, ctx, pool, f.mainBranch.ID); now != mainHeadBeforeMaintainer {
		t.Fatalf("(4) the maintainer's proposal moved main to %s; via_pr means the PR, not the write", now)
	}
	if got := readVersion(t, ctx, pool, maintainerAbortedID).lifecycleState; got != string(domain.LifecycleAborted) {
		t.Fatalf("(4) the maintainer's proposal changed the aborted version on main to %q", got)
	}
	// The row it wrote sits on the proposal branch, not on main.
	maintainerReopens := storedReopens(t, ctx, pool, maintainerObjectID)
	if len(maintainerReopens) != 1 {
		t.Fatalf("(4) the maintainer's proposal left %d reopened rows, want 1 (on its proposal branch)", len(maintainerReopens))
	}
	if maintainerReopens[0].branchID == nil || *maintainerReopens[0].branchID == f.mainBranch.ID {
		t.Fatalf("(4) the maintainer's proposal wrote to main: %s", maintainerReopens[0].describe())
	}

	// ---- (5) The agent half, on both lines of defence. The FIRST line is the
	// domain backstop, asked with the project's OWNER as the agent's user — an
	// actor the matrix admits — so the refusal cannot be the matrix's doing.
	// The SECOND is the matrix's own answer for the agent class, asked
	// independently.
	agentObjectID, agentV1 := f.activeMainObject(t, ctx, "a claim an agent will try to reopen")
	agentAbortedID, _ := f.abortOnMain(t, ctx, agentObjectID, agentV1, "t0610-abort-0003")
	agentActor := reopens.Actor{User: domain.User{ID: f.ownerID}, IsAgent: true}
	const agentKey = "t0610-reopen-agent-0001"
	_, err = f.tryReopen(ctx, agentActor, agentObjectID, agentAbortedID, agentKey, reopenReason, reopenExplanation)
	if err == nil {
		t.Fatal("(5) an agent reopened a main object — the domain backstop did not fire")
	}
	var refused *reopens.AgentNotPermittedError
	if !errors.As(err, &refused) {
		t.Fatalf("(5) the agent refusal is %v, want *reopens.AgentNotPermittedError", err)
	}
	if refused.Code() != reopens.CodeAgentDenied {
		t.Fatalf("(5) the agent refusal code = %q, want %q", refused.Code(), reopens.CodeAgentDenied)
	}
	agentDecision, err := authz.NewMatrixEngine().Authorize(ctx, authz.Request{
		Action: authz.ActionReopenMainObject,
		Class:  authz.ClassOf(true, rolePtr(domain.ProjectRoleOwner), true),
	})
	if err != nil {
		t.Fatalf("(5) authorize reopen_main_object for an agent: %v", err)
	}
	if agentDecision.Permits() {
		t.Fatal("(5) the permission matrix permits reopen_main_object for an agent — the second line of defence is gone")
	}
	if agentDecision.Verdict != authz.VerdictProposalOnly {
		t.Fatalf("(5) the matrix's agent cell for reopen_main_object = %q, want %q (the ruling's row)",
			agentDecision.Verdict, authz.VerdictProposalOnly)
	}
	// What the agent emitted is not a reopen: no row of the object is in the
	// state the refused call would have produced, and no record was written.
	if rows := storedReopens(t, ctx, pool, agentObjectID); len(rows) != 0 {
		t.Fatalf("(5) an agent's refused proposal left %d reopened rows in storage: %s", len(rows), rows[0].id)
	}
	for _, row := range versionsOfObject(t, ctx, pool, agentObjectID) {
		if row.reopenRequestKey != nil && *row.reopenRequestKey == agentKey {
			t.Fatalf("(5) an agent's refused proposal left a version row in storage: %s", row.id)
		}
	}
	if audits, outbox := reopenCounts(t, ctx, pool, agentObjectID); audits != 0 || outbox != 0 {
		t.Fatalf("(5) an agent's refused proposal wrote %d audit rows and %d events, want 0 and 0", audits, outbox)
	}

	// ---- (5) Existence hiding: the same unauthorized caller, the same request
	// shape, one object that exists and one that does not. The two answers must
	// be identical — same error — or the refusal would be a lookup wearing a
	// permission's clothes.
	ghost := "00000000-0000-4000-8000-00000000dead"
	viewerActor := reopens.Actor{User: domain.User{ID: f.viewerID}}
	_, realErr := f.tryReopen(ctx, viewerActor, objectID, abortedID, "t0610-hiding-key-0001", reopenReason, reopenExplanation)
	ghostRes, ghostErr := f.tryReopen(ctx, viewerActor, ghost, ghost, "t0610-hiding-key-0001", reopenReason, reopenExplanation)
	if ghostErr == nil || realErr == nil {
		t.Fatalf("(5) the existence-hiding probes were not refused (%v, %v)", realErr, ghostErr)
	}
	if realErr.Error() != ghostErr.Error() {
		t.Fatalf("(5) an unauthorized reopen of a real object (%v) is distinguishable from one of a missing object (%v)", realErr, ghostErr)
	}
	if ghostRes != (reopens.Result{}) {
		t.Fatalf("(5) the refused call answered a result: %+v", ghostRes)
	}
	// The same pair for a caller the matrix ADMITS: an owner reopening a missing
	// object has to answer "not found" rather than a permission refusal, or the
	// refusal above would be reported for the wrong reason.
	if _, err := f.tryReopen(ctx, ownerActor, ghost, ghost, "t0610-hiding-owner-0001", reopenReason, reopenExplanation); !errors.Is(err, reopens.ErrObjectNotFound) {
		t.Fatalf("(5) an owner reopening a missing object answered %v, want %v", err, reopens.ErrObjectNotFound)
	}
	// And an owner naming a version that does not exist, on a real object: "no
	// such version" is the answer — not a permission refusal, and not a
	// reopened object.
	if _, err := f.tryReopen(ctx, ownerActor, objectID, ghost, "t0610-hiding-owner-0002", reopenReason, reopenExplanation); !errors.Is(err, reopens.ErrVersionNotFound) {
		t.Fatalf("(5) an owner naming a missing version answered %v, want %v", err, reopens.ErrVersionNotFound)
	}
}

// TestReopenProposalConcurrency is the concurrent half of the idempotency
// criterion: two requests carrying ONE Idempotency-Key race, and exactly one
// reopen exists afterwards — one version row, one audit row, one event. The
// single winner is the compare-and-swap on the object's version counter, not a
// read-then-write: the loser's append is refused by the store.
//
// The loser's own outcome is not asserted to be a success: it may be answered
// by the replay read (when the winner committed before the loser looked) or
// refused with the conflict code (when it did not). What is asserted is that
// the loser wrote NOTHING, which is the property the criterion names.
func TestReopenProposalConcurrency(t *testing.T) {
	ctx := testCtx(t)
	f := newReopenFixture(t, ctx)
	pool := f.pool

	objectID, v1ID := f.activeMainObject(t, ctx, "a claim two maintainers will reopen at once")
	abortedID, _ := f.abortOnMain(t, ctx, objectID, v1ID, "t0610-conc-abort-0001")
	before := versionsOfObject(t, ctx, pool, objectID)

	const (
		key    = "t0610-concurrent-0001"
		caller = 2
	)
	actor := reopens.Actor{User: domain.User{ID: f.ownerID}}

	type outcome struct {
		res reopens.Result
		err error
	}
	results := make([]outcome, caller)
	// The goroutines share the fixture's pool, not a testing.T: they must not
	// call t.Fatalf from a non-test goroutine.
	driver := f
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < caller; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := driver.tryReopen(context.Background(), actor, objectID, abortedID, key, reopenReason, reopenExplanation)
			results[i] = outcome{res: res, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	// Exactly one reopen: one new version row, one audit row, one event.
	after := versionsOfObject(t, ctx, pool, objectID)
	if len(after) != len(before)+1 {
		t.Fatalf("(9) %d concurrent requests appended %d version rows, want exactly 1 (%d before, %d after)",
			caller, len(after)-len(before), len(before), len(after))
	}
	if audits, outbox := reopenCounts(t, ctx, pool, objectID); audits != 1 || outbox != 1 {
		t.Fatalf("(9) %d concurrent requests wrote %d audit rows and %d events, want 1 and 1", caller, audits, outbox)
	}
	// A caller answered as a success must have been answered the winner's
	// proposal or its replay — both name the same row. A refusal must be the
	// conflict class, which is what "re-read and retry" means.
	winner := ""
	for i, out := range results {
		switch {
		case out.err == nil:
			if out.res.VersionID == "" {
				t.Fatalf("(9) caller %d was answered without a version: %+v", i, out.res)
			}
			if winner == "" {
				winner = out.res.VersionID
			} else if out.res.VersionID != winner {
				t.Fatalf("(9) the two callers were answered different versions: %s and %s", winner, out.res.VersionID)
			}
		case errors.Is(out.err, reopens.ErrConflict):
			// The loser of the race: it neither won nor replayed, and the
			// counts above show it wrote nothing.
		default:
			t.Fatalf("(9) caller %d failed with %v, want either a replay or %v", i, out.err, reopens.ErrConflict)
		}
	}
	if winner == "" {
		t.Fatal("(9) neither concurrent request was answered a proposal")
	}
	// The winning row is in storage, in the state the command reported.
	won := readVersion(t, ctx, pool, winner)
	if won.lifecycleState != string(domain.LifecycleReopened) {
		t.Fatalf("(9) the winning version's lifecycle_state = %q, want reopened", won.lifecycleState)
	}
	if won.reopenRequestKey == nil || *won.reopenRequestKey != key {
		t.Fatalf("(9) the winning version does not carry the request key: %v", won.reopenRequestKey)
	}
}

// TestReopenRefusesAKeyBorrowedFromAnotherObject is the refusal the two key
// indexes' disagreement makes necessary: the reopen's own key index is per
// OBJECT (migration 00123), while the Research PR's creation key index is per
// PROJECT (migration 00089) and is SHARED with the abort command. One key sent
// for two objects therefore finds the first object's proposal in the
// project-wide index, and answering this object's reopen with it would report a
// reopen as proposed when nothing proposes it.
func TestReopenRefusesAKeyBorrowedFromAnotherObject(t *testing.T) {
	ctx := testCtx(t)
	f := newReopenFixture(t, ctx)

	firstID, firstV1 := f.activeMainObject(t, ctx, "the claim that will hold the key")
	firstAbortedID, _ := f.abortOnMain(t, ctx, firstID, firstV1, "t0610-borrow-abort-0001")
	secondID, secondV1 := f.activeMainObject(t, ctx, "the claim that will try to borrow it")
	secondAbortedID, _ := f.abortOnMain(t, ctx, secondID, secondV1, "t0610-borrow-abort-0002")

	actor := reopens.Actor{User: domain.User{ID: f.ownerID}}
	const borrowed = "t0610-borrowed-key-0001"

	if _, err := f.tryReopen(ctx, actor, firstID, firstAbortedID, borrowed, reopenReason, reopenExplanation); err != nil {
		t.Fatalf("the first reopen: %v", err)
	}
	// The same key, one object over. The per-project proposal index already
	// holds a proposal for this key, and that proposal moves the FIRST object.
	_, err := f.tryReopen(ctx, actor, secondID, secondAbortedID, borrowed, reopenReason, reopenExplanation)
	if err == nil {
		t.Fatal("the second object's reopen was answered with another object's proposal")
	}
	if !errors.Is(err, reopens.ErrIdempotencyKeyInUse) {
		t.Fatalf("the borrowed-key refusal is %v, want %v", err, reopens.ErrIdempotencyKeyInUse)
	}
	var held *reopens.IdempotencyKeyInUseError
	if !errors.As(err, &held) || held.Code() != reopens.CodeConflict {
		t.Fatalf("the borrowed-key refusal's wire code is not the conflict class: %v", err)
	}
	// And the second object's state is untouched — not one row, not one record:
	// the refusal is made BEFORE the append (the project-wide proposal index is
	// read ahead of it), so the request that is refused never reaches the point
	// of no return. Without that ordering the refusal would leave a version row
	// on the second object's proposal branch that no proposal explains, and the
	// log is append-only.
	if rows := storedReopens(t, ctx, f.pool, secondID); len(rows) != 0 {
		t.Fatalf("the refused borrowed-key call left %d reopened rows on the second object", len(rows))
	}
	if n := len(versionsOfObject(t, ctx, f.pool, secondID)); n != 3 {
		t.Fatalf("the refused borrowed-key call left the second object with %d version rows, want 3 (v1 + abort proposal + accepted abort)", n)
	}
	if audits, outbox := reopenCounts(t, ctx, f.pool, secondID); audits != 0 || outbox != 0 {
		t.Fatalf("the refused borrowed-key call wrote %d audit rows and %d events, want 0 and 0", audits, outbox)
	}
	// The first object's reopen is unaffected and is still the only one.
	if rows := storedReopens(t, ctx, f.pool, firstID); len(rows) != 1 {
		t.Fatalf("the first object has %d reopened rows, want 1", len(rows))
	}
}

// ------------------------------------------------------------------ helpers

func rolePtr(r domain.ProjectRole) *domain.ProjectRole { return &r }

// assertRuledReopenRow checks the canonical permission CSV against the owner's
// 2026-09-21 ruling for this task, cell by cell: the reopen row must be
// `reopen_main_object,deny,deny,deny,deny,via_pr,via_pr,proposal_only`, and it
// must be cell-for-cell identical to the abort row it mirrors. The ruling, as
// it stands in tasks/packages/T0610.json:
//
//	specs/policies/permissions-matrix.csv 新增一行
//	reopen_main_object,deny,deny,deny,deny,via_pr,via_pr,proposal_only
//	（与 :14 的 abort_main_object 行逐格相同）
//
// The CSV is the canonical policy document; internal/authz's drift test asserts
// the engine's table matches it, so reading the CSV here is reading the row the
// engine evaluates.
func assertRuledReopenRow() error {
	path := filepath.Join("..", "..", "specs", "policies", "permissions-matrix.csv")
	fh, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open the canonical permission matrix: %w", err)
	}
	defer fh.Close()
	records, err := csv.NewReader(fh).ReadAll()
	if err != nil {
		return fmt.Errorf("parse the canonical permission matrix: %w", err)
	}
	if len(records) == 0 {
		return fmt.Errorf("the canonical permission matrix is empty")
	}
	header := records[0]
	want := []string{"reopen_main_object", "deny", "deny", "deny", "deny", "via_pr", "via_pr", "proposal_only"}
	var reopenRow, abortRow []string
	for _, rec := range records[1:] {
		if len(rec) == 0 {
			continue
		}
		switch rec[0] {
		case "reopen_main_object":
			reopenRow = rec
		case "abort_main_object":
			abortRow = rec
		}
	}
	if reopenRow == nil {
		return fmt.Errorf("the canonical permission matrix has no reopen_main_object row (the ruling's row is missing)")
	}
	if len(reopenRow) != len(header) {
		return fmt.Errorf("the reopen_main_object row has %d cells, want %d (%v)", len(reopenRow), len(header), reopenRow)
	}
	for i, cell := range want {
		if reopenRow[i] != cell {
			return fmt.Errorf("the reopen_main_object row's cell %d (%s) is %q, want %q — the owner's ruling is %v",
				i, header[i], reopenRow[i], cell, want)
		}
	}
	if abortRow == nil {
		return fmt.Errorf("the canonical permission matrix has no abort_main_object row to be identical to")
	}
	if len(abortRow) != len(reopenRow) {
		return fmt.Errorf("the abort row has %d cells and the reopen row %d", len(abortRow), len(reopenRow))
	}
	// Every cell but the first is compared: the first is the action key, which
	// is the one cell a reopen row cannot share with an abort row. 逐格相同 is
	// the four verdict columns and the three class columns — the ruling's own
	// row (`reopen_main_object` followed by the abort's seven cells).
	for i := 1; i < len(reopenRow); i++ {
		if reopenRow[i] != abortRow[i] {
			return fmt.Errorf("cell %d (%s) differs from the abort row: reopen %q, abort %q — the ruling is 逐格相同",
				i, header[i], reopenRow[i], abortRow[i])
		}
	}
	return nil
}
