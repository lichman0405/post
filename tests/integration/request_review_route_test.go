// Package integration — T0411 "request review route": the product path that
// moves a proposal into review (docs/43's open -> review_required, and the
// review loop's re-entry changes_requested -> review_required once the author
// has answered the requested changes).
//
// Everything below runs over REAL PostgreSQL and the routes cmd/api mounts,
// behind the real auth guard: the session cookie and the session-bound CSRF
// token are the ones the signup endpoint minted, the proposal is opened by the
// contract's own POST, sent to review by the route this task adds, and reviewed
// through the review-submission route. No test-side principal is injected and
// no service is called directly where a criterion names an endpoint; the state
// every assertion reads is the ROW in pull_requests, not a return value.
//
// The acceptance criteria map onto TestRequestReviewRouteEndToEnd:
//
//	(1) the GAP: a proposal created through the product path cannot reach
//	    merge_ready before this step — the reviews are submitted, the state
//	    does not move, and the database's own transition map refuses
//	    open -> merged. The assertion is one a wired-up step turns red (the
//	    mutation is recorded in RESULT.json, not here).
//	(2) the move itself, through the route, asserted on the stored row; and
//	    the same call replayed by an identity the matrix denies, refused.
//	(3) both edges: open -> review_required AND changes_requested ->
//	    review_required, each with its own case (the second one re-entered
//	    after the author answered a changes_requested review).
//	(4) the per-cell authorization of the ruled shape (A: the existing
//	    open_pr cell), cell by cell, including the non-member whose fork
//	    proposal MUST succeed and the agent column.
//	(5) the rest of the machine unchanged: the database still refuses
//	    open -> merged, and a merged/closed/aborted proposal never moves.
//	(6) idempotency and concurrency: a replay of the same Idempotency-Key
//	    answers the same proposal and writes exactly one audit row; two
//	    concurrent calls let exactly one of them take effect.
//	(7) refusals leak no existence: for a proposal that is not there, the
//	    unauthorized caller's answer is indistinguishable from the answer to
//	    a proposal that IS there and is forbidden.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/mergehttp"
	"github.com/lichman0405/post/cmd/api/policyhttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/cmd/api/pullrequestshttp"
	"github.com/lichman0405/post/cmd/api/reviewhttp"
	"github.com/lichman0405/post/cmd/api/rsghttp"

	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/diffs"
	"github.com/lichman0405/post/internal/application/forks"
	"github.com/lichman0405/post/internal/application/merge"
	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/prchecks"
	"github.com/lichman0405/post/internal/application/prdiff"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/application/resolutions"
	"github.com/lichman0405/post/internal/application/responsibilities"
	"github.com/lichman0405/post/internal/application/reviews"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/gitprovider"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rsg/integrity"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

const requestReviewTaskID = "T0411"

// ---------------------------------------------------------------- the wire

// reviewPRWire is the pull-request document the route answers with
// (cmd/api/pullrequestshttp's prPayload), reduced to the fields this test
// asserts on.
type reviewPRWire struct {
	ID              string `json:"id"`
	ProjectID       string `json:"project_id"`
	Number          int64  `json:"number"`
	State           string `json:"state"`
	SourceBranchID  string `json:"source_branch_id"`
	TargetBranchID  string `json:"target_branch_id"`
	ProposedStateID string `json:"proposed_state_id"`
	Title           string `json:"title"`
	CreatedBy       string `json:"created_by"`
}

// reviewErrorWire is the transport's error envelope (cmd/api/authhttp). The
// request_id is deliberately NOT part of any comparison below: it is the
// per-request correlation id, and two refusals that must be indistinguishable
// still carry their own.
type reviewErrorWire struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

func reviewCollectionPath(projectID string) string {
	return "/api/v1/projects/" + projectID + "/pull-requests"
}

// reviewPath is the CONTRACT's route
// (specs/api/openapi.yaml: /projects/{projectId}/pull-requests/{prId}:request-review),
// spelled with the verb inside the last segment.
func reviewPath(projectID string, number int64) string {
	return reviewCollectionPath(projectID) + "/" + strconv.FormatInt(number, 10) + ":request-review"
}

func reviewSubmissionsPath(projectID string, number int64) string {
	return reviewCollectionPath(projectID) + "/" + strconv.FormatInt(number, 10) + "/reviews"
}

// ------------------------------------------------------------------ stubs

// reviewStubProvisioner stands in for the provisioner while the fixture builds
// the non-member's fork. The fork's Git side is T0814's subject and is covered
// end to end there (tests/integration/external_fork_http_e2e_test.go, against a
// real Gitea); here the fork exists only to give a non-member a proposal the
// open_pr cell's allow_from_fork condition can resolve against, so the provider
// round-trips are the one thing this fixture does not reproduce.
type reviewStubProvisioner struct{}

func (reviewStubProvisioner) Provision(context.Context, string) error { return nil }

// reviewStubImporter is the same substitution for the content copy: it records
// a fork point without a real repository.
type reviewStubImporter struct{}

func (reviewStubImporter) Import(_ context.Context, in gitprovider.ForkImportRequest) (gitprovider.ForkImportResult, error) {
	sha := "sha256:fixture-fork-point"
	return gitprovider.ForkImportResult{SourceSHA: sha, TargetHeadSHA: sha, Inserted: true}, nil
}

// --------------------------------------------------------------- fixture

// reviewFixture is the platform graph for this file's criteria: one private
// project with a membership in every human role, one public project for the
// external contributor, and the routes cmd/api mounts — including the merge
// surface, which is where the request-review verb is registered (a prefix has
// ONE remainder owner, and that is the merge route's pattern).
type reviewFixture struct {
	ctx     context.Context
	pool    *pgxpool.Pool
	server  string
	project domain.Project
	// public is the project an external contributor forks. A non-member
	// reaches the open_pr cell's allow_from_fork condition only through a
	// fork of a PUBLIC project (the read gate of a private one refuses them
	// first).
	public         domain.Project
	mainBranch     domain.Branch
	research       domain.Branch
	publicMain     domain.Branch
	publicResearch domain.Branch
	policyStore    *persistence.PolicyStore
	projectStore   *persistence.ProjectStore
	prs            *pullrequests.Service
	forks          *forks.Service

	owner       *testUserClient
	maintainer  *testUserClient
	contributor *testUserClient
	viewer      *testUserClient
	// outsider is an authenticated caller with NO membership in either
	// project: the authenticated_nonmember column.
	outsider *testUserClient
	// external forks the public project: a non-member whose proposal comes
	// from their own fork.
	external *testUserClient

	id map[string]string
}

// reviewActor is one acting browser under the role name the matrix column
// carries, for the table-driven criterion (4).
type reviewActor struct {
	name   string
	client *testUserClient
}

func newReviewFixture(t *testing.T, ctx context.Context) *reviewFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), requestReviewTaskID)

	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	orgStore := persistence.NewOrgStore(pool)
	projectStore := persistence.NewProjectStore(pool)
	policyStore := persistence.NewPolicyStore(pool)
	stateStore := persistence.NewStateStore(pool)
	branchStore := persistence.NewBranchStore(pool)
	prStore := persistence.NewPullRequestStore(pool)
	objects := persistence.NewScientificObjectStore(pool)

	projectAPI := projectshttp.New(projectshttp.Deps{Store: projectStore, Orgs: orgStore, Authz: authz.NewMatrixEngine()})
	projectSvc := projectAPI.Service()
	policyAPI := policyhttp.New(policyhttp.Deps{Store: policyStore, Orgs: orgStore, Projects: projectStore})
	branchSvc := branches.NewService(branchStore)
	statesSvc := states.NewService(stateStore, newCommitGuard(t))
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
	diffSvc := diffs.NewService(stateStore, persistence.NewManifestStore(pool), prStore)
	resolutionSvc := resolutions.NewService(diffSvc, resolutions.NewPGStore(pool), projectSvc, authz.NewMatrixEngine())
	prSvc := pullrequests.NewService(prStore)
	checksSvc := prchecks.NewService(prchecks.Deps{
		PRs:      prStore,
		Projects: projectStore,
		States:   stateStore,
		Branches: branchStore,
		Manifest: persistence.NewManifestStore(pool),
		Policies: policyStore,
		Engine:   integrity.New(reg),
	})
	// The open_pr cell's two commands: Create is the external-PR path
	// (OpenExternalPR), Review is this task's (RequestReview). Both resolve the
	// SAME open_pr cell, which is the whole of the ruling, and the review port
	// is the pull-request service itself — the production value
	// (internal/application/forks/ports.go asserts the assignment compiles).
	forksSvc := forks.NewService(forks.Deps{
		Projects:     projectSvc,
		Branches:     branchSvc,
		BranchWriter: rsgSvc,
		Forks:        persistence.NewForkStore(pool),
		Repos:        reviewStubProvisioner{},
		Imports:      reviewStubImporter{},
		PullRequests: prSvc,
		Reviews:      prSvc,
		Authz:        authz.NewMatrixEngine(),
	})
	// The review machine the proposal walks once it IS in review: the routing
	// the required-review calculation reads, and the submission command. The
	// gap below is asserted against this stack, so "the reviews did not move
	// it" is a statement about a machine that could have.
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
		Aborts:    objects,
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
			// The suite signs up several users per fixture against one
			// loopback address; the signup limiter is production's, and
			// leaving it at its production value would rate-limit the
			// fixture rather than the product.
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
	prAPI := pullrequestshttp.New(pullrequestshttp.Deps{
		PullRequests: prSvc,
		Create:       forksSvc,
		Review:       forksSvc,
		Checks:       checksSvc,
		Diff:         prdiff.NewService(prStore, branchStore, diffSvc),
		Projects:     projectSvc,
	})
	prAPI.Register(apiMux)
	reviewhttp.New(reviewhttp.Deps{Service: reviewsSvc, Projects: projectSvc}).Register(apiMux)
	// The composition the route is actually served by: the merge surface owns
	// the collection's remainder pattern and dispatches ":request-review" to
	// the pull-request surface's handler — the very call cmd/api/main.go makes.
	mergehttp.New(mergehttp.Deps{
		Command:       mergeSvc,
		Projects:      projectSvc,
		ReviewRequest: prAPI.RequestReviewHandler(),
	}).Register(apiMux)
	ts := httptest.NewServer(authAPI.Guard(apiMux))
	t.Cleanup(ts.Close)

	f := &reviewFixture{
		ctx: ctx, pool: pool, server: ts.URL,
		policyStore: policyStore, projectStore: projectStore,
		prs: prSvc, forks: forksSvc,
		id: map[string]string{},
	}
	// Every acting browser is created through the real signup endpoint.
	f.owner, f.id["owner"] = signup(t, ts.URL, "review-owner@example.com", "review-owner")
	f.maintainer, f.id["maintainer"] = signup(t, ts.URL, "review-maintainer@example.com", "review-maintainer")
	f.contributor, f.id["contributor"] = signup(t, ts.URL, "review-contributor@example.com", "review-contributor")
	f.viewer, f.id["viewer"] = signup(t, ts.URL, "review-viewer@example.com", "review-viewer")
	f.outsider, f.id["outsider"] = signup(t, ts.URL, "review-outsider@example.com", "review-outsider")
	f.external, f.id["external"] = signup(t, ts.URL, "review-external@example.com", "review-external")

	ownerUser := domain.User{ID: f.id["owner"]}
	org, _, err := orgStore.CreateOrganization(ctx, domain.Organization{
		Slug: "review-gov", Name: "Review Governance",
	}, f.id["owner"], todayUTC())
	if err != nil {
		t.Fatalf("create the fixture organization: %v", err)
	}
	f.project, _, err = projectStore.CreateProject(ctx, domain.Project{
		OrganizationID:  &org.ID,
		Slug:            "review-gov",
		Name:            "Review Governance",
		Purpose:         "T0411 request review route e2e",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, f.id["owner"])
	if err != nil {
		t.Fatalf("create the fixture project: %v", err)
	}
	// The public sibling: what a non-member can read, and therefore the only
	// project whose open_pr cell a non-member can reach at all.
	f.public, _, err = projectStore.CreateProject(ctx, domain.Project{
		OrganizationID:  &org.ID,
		Slug:            "review-gov-public",
		Name:            "Review Governance (public)",
		Purpose:         "T0411 external contribution cell",
		Visibility:      domain.VisibilityPublic,
		ProvisionStatus: domain.ProvisionPending,
	}, f.id["owner"])
	if err != nil {
		t.Fatalf("create the public fixture project: %v", err)
	}
	for _, seed := range []struct{ who, role string }{
		{"maintainer", "maintainer"},
		{"contributor", "contributor"},
		{"viewer", "viewer"},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, $3)`,
			f.project.ID, f.id[seed.who], seed.role); err != nil {
			t.Fatalf("seed the %s membership: %v", seed.role, err)
		}
	}
	seedProjectPolicy(t, ctx, policyStore, f.project, f.id["owner"])
	seedProjectPolicy(t, ctx, policyStore, f.public, f.id["owner"])

	f.mainBranch, err = rsgSvc.CreateBranch(ctx, ownerUser, f.project.ID, rsg.CreateBranchInput{
		Name: "main", BaseRef: "", Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create the main branch: %v", err)
	}
	f.research, err = rsgSvc.CreateBranch(ctx, ownerUser, f.project.ID, rsg.CreateBranchInput{
		Name: "review-target", Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create the research branch: %v", err)
	}
	f.publicMain, err = rsgSvc.CreateBranch(ctx, ownerUser, f.public.ID, rsg.CreateBranchInput{
		Name: "main", BaseRef: "", Visibility: domain.BranchVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("create the public project's main branch: %v", err)
	}
	// A line of the public project that is NOT anybody's fork: the branch the
	// allow_from_fork condition has to refuse, held by the same non-member who
	// is allowed on their own fork.
	f.publicResearch, err = rsgSvc.CreateBranch(ctx, ownerUser, f.public.ID, rsg.CreateBranchInput{
		Name: "upstream-research", Visibility: domain.BranchVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("create the public project's research branch: %v", err)
	}
	// The research line carries content: the proposal's source branch must be
	// readable as changes for the proposed/target pair to be a diff at all,
	// and migration 00042's pull_request_semantic_gate refuses a formal PR
	// from an unparseable line.
	f.writeClaim(t, f.owner, f.project.ID, f.research.ID, "the review route fixture's claim")

	// The required-review configuration the calculation runs on: the claim
	// changes the branch carries are routed to one responsibility, and BOTH
	// reviewers below hold it. The scientific requirement is signed under the
	// routed responsibility, and the whole-proposal integrity requirement
	// accepts only a review recorded under a label the routing resolved
	// (domain.EvaluateRequiredReviews). Reviewed and approved: the machine is
	// armed, so "the state did not move" is a fact about the route, not about
	// a stack that could not have moved it.
	for _, who := range []string{"viewer", "owner"} {
		if _, err := routingSvc.Assign(ctx, ownerUser, f.project.ID, f.id[who], "Data Reviewer"); err != nil {
			t.Fatalf("assign the reviewer responsibility to %s: %v", who, err)
		}
	}
	if _, err := routingSvc.AddRule(ctx, ownerUser, f.project.ID, responsibilities.AddRuleInput{
		MatchKind:      domain.ResearchOwnerMatchObjectType,
		MatchValue:     "claim",
		Responsibility: "Data Reviewer",
	}); err != nil {
		t.Fatalf("route claim changes to the reviewer: %v", err)
	}
	return f
}

// writeClaim writes one main-gate-complete claim through the RSG route.
func (f *reviewFixture) writeClaim(t *testing.T, uc *testUserClient, projectID, branchID, statement string) {
	t.Helper()
	status, body := wirePost(t, uc, branchObjectsPath(projectID, branchID),
		fmt.Sprintf(`{"object_type":"claim","payload":%s}`, mergeMainGateClaim(statement)))
	if status != http.StatusCreated {
		t.Fatalf("write the fixture claim = %d: %s", status, body)
	}
}

// ---------------------------------------------------------------- actions

// openPR opens a proposal through the contract's own route
// (POST /projects/{projectId}/pull-requests) and returns its per-project
// number. The key must be unique per project (migration 00089), so callers
// pass one; the source branch may belong to the caller's fork of the project.
func (f *reviewFixture) openPR(t *testing.T, uc *testUserClient, projectID, sourceBranchID, targetBranchID, title, key string) int64 {
	t.Helper()
	body := fmt.Sprintf(`{"source_branch_id":%q,"target_branch_id":%q,"title":%q,"body":"T0411"}`,
		sourceBranchID, targetBranchID, title)
	resp := uc.doKeyed(t, http.MethodPost, reviewCollectionPath(projectID), body, key)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("open the proposal %q = %d: %s", title, resp.StatusCode, readAll(t, resp))
	}
	var out reviewPRWire
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode the opened proposal: %v", err)
	}
	if out.Number < 1 {
		t.Fatalf("the open route answered without a number: %+v", out)
	}
	return out.Number
}

// sendToReview issues ONE request-review call over the route, with the given
// Idempotency-Key ("" sends none).
func (f *reviewFixture) sendToReview(t *testing.T, uc *testUserClient, projectID string, number int64, key string) (*http.Response, reviewPRWire) {
	t.Helper()
	resp := uc.doKeyed(t, http.MethodPost, reviewPath(projectID, number), "", key)
	var out reviewPRWire
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode the request-review answer: %v", err)
		}
	}
	return resp, out
}

// submitReview submits one review over the review-submission route.
func (f *reviewFixture) submitReview(t *testing.T, uc *testUserClient, projectID string, number int64, kind, decision string) {
	t.Helper()
	body := fmt.Sprintf(`{"kind":%q,"decision":%q,"body":"reviewed for T0411"}`, kind, decision)
	resp := uc.do(t, http.MethodPost, reviewSubmissionsPath(projectID, number), body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("submit the %s %s review of PR %d = %d: %s",
			kind, decision, number, resp.StatusCode, readAll(t, resp))
	}
}

// approveWholeProposal submits the two judgments the arming configuration asks
// for: the routed scientific one, and the proposal-wide integrity one.
func (f *reviewFixture) approveWholeProposal(t *testing.T, projectID string, number int64) {
	t.Helper()
	f.submitReview(t, f.viewer, projectID, number, "scientific", "approved")
	f.submitReview(t, f.owner, projectID, number, "integrity", "approved")
}

// ------------------------------------------------------------ the storage

// prRow is what the assertions read: the stored row, never a service's return
// value.
type prRow struct {
	id    string
	state string
}

func (f *reviewFixture) pr(t *testing.T, projectID string, number int64) prRow {
	t.Helper()
	var row prRow
	if err := f.pool.QueryRow(f.ctx,
		`SELECT id::text, state FROM pull_requests WHERE project_id = $1::uuid AND number = $2`,
		projectID, number).Scan(&row.id, &row.state); err != nil {
		t.Fatalf("read PR %d of %s: %v", number, projectID, err)
	}
	return row
}

// stateOf is the stored state, the one fact every criterion below turns on.
func (f *reviewFixture) stateOf(t *testing.T, projectID string, number int64) string {
	t.Helper()
	return f.pr(t, projectID, number).state
}

// auditRows counts this transition's audit rows for one proposal.
func (f *reviewFixture) auditRows(t *testing.T, prID string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM audit_log WHERE action = $1 AND target_ref = $2`,
		domain.ActionPullRequestReviewRequested, "pull_request:"+prID).Scan(&n); err != nil {
		t.Fatalf("count the request-review audit rows of %s: %v", prID, err)
	}
	return n
}

// auditFromStates is the set of states the recorded moves left FROM — the
// evidence of WHICH edge was travelled (open vs changes_requested), which the
// audit row's before_summary records by design.
func (f *reviewFixture) auditFromStates(t *testing.T, prID string) map[string]int {
	t.Helper()
	rows, err := f.pool.Query(f.ctx,
		`SELECT COALESCE(before_summary->>'state', '') FROM audit_log WHERE action = $1 AND target_ref = $2`,
		domain.ActionPullRequestReviewRequested, "pull_request:"+prID)
	if err != nil {
		t.Fatalf("read the request-review audit rows of %s: %v", prID, err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var state string
		if err := rows.Scan(&state); err != nil {
			t.Fatalf("scan the audit before_summary: %v", err)
		}
		out[state]++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the audit rows: %v", err)
	}
	return out
}

// outboxRows counts a project's outbox rows, by name and in total.
func (f *reviewFixture) outboxRows(t *testing.T, projectID, eventType string) (named, total int) {
	t.Helper()
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FILTER (WHERE event_type = $2), count(*) FROM outbox_events WHERE project_id = $1::uuid`,
		projectID, eventType).Scan(&named, &total); err != nil {
		t.Fatalf("count the outbox rows of %s: %v", projectID, err)
	}
	return named, total
}

// errorOf decodes the transport's error envelope.
func errorOf(t *testing.T, resp *http.Response) reviewErrorWire {
	t.Helper()
	var out reviewErrorWire
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode the error envelope: %v", err)
	}
	return out
}

// setStateDirectly walks a proposal's row along a path of LEGAL edges. It is
// fixture construction for criterion (5), and it is labelled as such: the
// subject of that criterion is what the ROUTE does with a row that is already
// merged/closed/aborted, and this build has no route to those states (no close
// route, no PR abort command, and a merge needs the real merge engine over the
// real provider). The walk goes through the database's own transition map one
// edge at a time, so the guard is exercised rather than bypassed.
func (f *reviewFixture) setStateDirectly(t *testing.T, projectID string, number int64, path ...domain.PullRequestState) {
	t.Helper()
	for _, next := range path {
		tag, err := f.pool.Exec(f.ctx,
			`UPDATE pull_requests SET state = $1 WHERE project_id = $2::uuid AND number = $3`,
			string(next), projectID, number)
		if err != nil {
			t.Fatalf("walk PR %d to %s: %v", number, next, err)
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("walk PR %d to %s: %d rows", number, next, tag.RowsAffected())
		}
	}
}

// assertGuardRefuses proves the DATABASE refuses one state move on the real
// row. The application's compare-and-swap is the normal path; migration
// 00051's pull_request_guard is the backstop no code path can skip, so the
// refusal is provoked with a bare UPDATE — the write that no application layer
// stands in front of.
func (f *reviewFixture) assertGuardRefuses(t *testing.T, projectID string, number int64, to domain.PullRequestState) {
	t.Helper()
	before := f.stateOf(t, projectID, number)
	_, err := f.pool.Exec(f.ctx,
		`UPDATE pull_requests SET state = $1 WHERE project_id = $2::uuid AND number = $3`,
		string(to), projectID, number)
	if err == nil {
		t.Fatalf("the database accepted %s -> %s; docs/43's transition map (migration 00051) is the backstop and must refuse it",
			before, to)
	}
	if !strings.Contains(err.Error(), "illegal pull request state transition") || !strings.Contains(err.Error(), "P0001") {
		t.Fatalf("the database refused %s -> %s with an unexpected error: %v", before, to, err)
	}
}

// walkTo places a proposal's row in a terminal state. merged is reachable only
// from merge_ready, and merge_ready only from approved, which only
// review_required reaches — so the walk runs the machine's own edges where it
// can (sending to review through the ROUTE, then satisfying the reviews), and
// steps onto the terminal state directly only where no route exists.
func (f *reviewFixture) walkTo(t *testing.T, projectID string, number int64, terminal domain.PullRequestState) {
	t.Helper()
	switch terminal {
	case domain.PullRequestStateClosed:
		f.setStateDirectly(t, projectID, number, domain.PullRequestStateClosed)
	case domain.PullRequestStateAborted:
		f.setStateDirectly(t, projectID, number, domain.PullRequestStateAborted)
	case domain.PullRequestStateMerged:
		if _, _ = f.sendToReview(t, f.owner, projectID, number, "review-walk-key-000001"); f.stateOf(t, projectID, number) != string(domain.PullRequestStateReviewRequired) {
			t.Fatalf("the walk could not put PR %d in review: %q", number, f.stateOf(t, projectID, number))
		}
		f.approveWholeProposal(t, projectID, number)
		if got := f.stateOf(t, projectID, number); got != string(domain.PullRequestStateMergeReady) {
			t.Fatalf("the walk left PR %d in %q, want %q", number, got, domain.PullRequestStateMergeReady)
		}
		f.setStateDirectly(t, projectID, number, domain.PullRequestStateMerged)
	default:
		t.Fatalf("walkTo does not know %q", terminal)
	}
}

// ---------------------------------------------------------------- helpers

// reviewAttempt is one call's outcome, collectable from a goroutine (where
// only the test goroutine may fail the test).
type reviewAttempt struct {
	status int
	body   string
	err    error
}

// reviewOnce issues one request-review call without any testing helper on the
// way out.
func reviewOnce(client *http.Client, server, projectID string, number int64, csrf, key string) reviewAttempt {
	req, err := http.NewRequest(http.MethodPost, server+reviewPath(projectID, number), nil)
	if err != nil {
		return reviewAttempt{err: err}
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return reviewAttempt{err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return reviewAttempt{err: err}
	}
	return reviewAttempt{status: resp.StatusCode, body: string(body)}
}

// assertIndistinguishable is criterion (7): the two answers differ in nothing a
// caller can act on. The request id is excluded and said so — it is the
// transport's per-request correlation id, and it is deliberately unique.
func assertIndistinguishable(t *testing.T, present, absent reviewAttempt) {
	t.Helper()
	if present.err != nil || absent.err != nil {
		t.Fatalf("a refusal failed at the transport: %v / %v", present.err, absent.err)
	}
	if present.status != absent.status {
		t.Fatalf("status %d for a proposal that exists vs %d for one that does not: the refusal leaks existence",
			present.status, absent.status)
	}
	var p, a reviewErrorWire
	if err := json.Unmarshal([]byte(present.body), &p); err != nil {
		t.Fatalf("decode the refusal: %v (%s)", err, present.body)
	}
	if err := json.Unmarshal([]byte(absent.body), &a); err != nil {
		t.Fatalf("decode the refusal: %v (%s)", err, absent.body)
	}
	if p.Code != a.Code || p.Message != a.Message {
		t.Fatalf("the refusals differ where they must not:\n exists:  %s / %s\n missing: %s / %s",
			p.Code, p.Message, a.Code, a.Message)
	}
}

// forkedProject is the fork the external contributor's proposal comes from.
type forkedProject struct {
	projectID string
	branchID  string
}

// forkPublic runs the production fork path for the external contributor
// against the public project and writes a claim on the fork's branch, so the
// proposal they open from it is an ordinary proposal of the parent.
func (f *reviewFixture) forkPublic(t *testing.T) forkedProject {
	t.Helper()
	result, err := f.forks.Fork(f.ctx, domain.User{ID: f.id["external"]}, forks.ForkRequest{
		ProjectID:      f.public.ID,
		SourceBranchID: f.publicMain.ID,
	})
	if err != nil {
		t.Fatalf("fork the public project: %v", err)
	}
	if result.Project.ID == "" || result.Branch.ID == "" {
		t.Fatalf("the fork answered no project or branch: %+v", result)
	}
	f.writeClaim(t, f.external, result.Project.ID, result.Branch.ID, "the external contribution")
	return forkedProject{projectID: result.Project.ID, branchID: result.Branch.ID}
}

// ---------------------------------------------------------------- the test

// TestRequestReviewRouteEndToEnd is T0411's required test (label:
// `request review route`).
func TestRequestReviewRouteEndToEnd(t *testing.T) {
	ctx := testCtx(t)
	f := newReviewFixture(t, ctx)

	// ================================================================= (1)
	// THE GAP. A proposal created through the product path cannot reach
	// merge_ready while this step does not exist: reviews are submitted
	// through the review route, the machine that would advance the proposal
	// is wired and armed — and the state does not move, because the
	// submission's own move (review_required -> approved) is a
	// compare-and-swap on a state the proposal never entered.
	//
	// This is the assertion that a wired-up step turns red. With the
	// send-to-review call inserted where a caller would put it before
	// reviews (the mutation recorded in RESULT.json), the two submissions
	// below walk the proposal to merge_ready and this assertion fails on
	// "state = merge_ready, want open".
	gap := f.openPR(t, f.owner, f.project.ID, f.research.ID, f.mainBranch.ID, "the gap", "review-gap-key-000001")
	if got := f.stateOf(t, f.project.ID, gap); got != string(domain.PullRequestStateOpen) {
		t.Fatalf("(1) the proposal was opened as %q, want %q", got, domain.PullRequestStateOpen)
	}
	f.approveWholeProposal(t, f.project.ID, gap)
	gapRow := f.pr(t, f.project.ID, gap)
	if gapRow.state == string(domain.PullRequestStateMergeReady) {
		t.Fatal("(1) the proposal is merge_ready without ever being sent to review")
	}
	if gapRow.state != string(domain.PullRequestStateOpen) {
		t.Fatalf("(1) the proposal is %q after both required reviews, want %q: a proposal the product created reached a reviewed state without being sent to review",
			gapRow.state, domain.PullRequestStateOpen)
	}
	// And the state it is parked in cannot be turned into the merged state by
	// anybody: the proposal can only leave 'open' by going into review (or
	// being closed/aborted), and the database is what says so (criterion 5's
	// first half, asserted where it belongs — on the row the whole machine
	// moves through).
	f.assertGuardRefuses(t, f.project.ID, gap, domain.PullRequestStateMerged)
	if got := f.stateOf(t, f.project.ID, gap); got != string(domain.PullRequestStateOpen) {
		t.Fatalf("(1) the refused write still moved the proposal: state = %q", got)
	}

	// ================================================================= (2)
	// THE MOVE, through the real route, asserted on the stored row.
	main := f.openPR(t, f.owner, f.project.ID, f.research.ID, f.mainBranch.ID, "the main journey", "review-main-key-00001")
	resp, payload := f.sendToReview(t, f.owner, f.project.ID, main, "review-send-key-00001")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("(2) sending PR %d to review = %d: %s", main, resp.StatusCode, readAll(t, resp))
	}
	if payload.Number != main || payload.State != string(domain.PullRequestStateReviewRequired) {
		t.Fatalf("(2) the route answered %+v, want PR %d in review", payload, main)
	}
	mainRow := f.pr(t, f.project.ID, main)
	if mainRow.state != string(domain.PullRequestStateReviewRequired) {
		t.Fatalf("(2) the stored row is %q, want %q", mainRow.state, domain.PullRequestStateReviewRequired)
	}
	if mainRow.id != payload.ID {
		t.Fatalf("(2) the row the route answered for (%s) is not the row that moved (%s)", payload.ID, mainRow.id)
	}
	if n := f.auditRows(t, mainRow.id); n != 1 {
		t.Fatalf("(2) the move wrote %d audit rows, want 1", n)
	}
	if from := f.auditFromStates(t, mainRow.id); from[string(domain.PullRequestStateOpen)] != 1 {
		t.Fatalf("(2) the audit row does not report the open edge: %v", from)
	}

	// The replay by an identity the matrix denies: the viewer's open_pr cell
	// is deny (specs/policies/permissions-matrix.csv, the viewer column). The
	// refusal is 403 and the proposal does not move.
	denied := f.viewer.doKeyed(t, http.MethodPost, reviewPath(f.project.ID, main), "", "review-send-key-00002")
	if denied.StatusCode != http.StatusForbidden {
		t.Fatalf("(2) an unauthorized replay = %d, want 403: %s", denied.StatusCode, readAll(t, denied))
	}
	if got := errorOf(t, denied); got.Code == "" {
		t.Fatal("(2) the refusal carried no wire code")
	}
	if got := f.stateOf(t, f.project.ID, main); got != string(domain.PullRequestStateReviewRequired) {
		t.Fatalf("(2) the refused replay moved the proposal: state = %q", got)
	}
	if n := f.auditRows(t, mainRow.id); n != 1 {
		t.Fatalf("(2) the refused replay wrote an audit row: %d rows", n)
	}

	// ================================================================= (3)
	// BOTH EDGES. The second one is the review loop: a reviewer asked for
	// changes, and the author re-enters review after answering.
	loop := f.openPR(t, f.owner, f.project.ID, f.research.ID, f.mainBranch.ID, "the review loop", "review-loop-key-00001")
	if _, _ = f.sendToReview(t, f.owner, f.project.ID, loop, "review-loop-key-00002"); f.stateOf(t, f.project.ID, loop) != string(domain.PullRequestStateReviewRequired) {
		t.Fatalf("(3) the first submission did not reach review: %q", f.stateOf(t, f.project.ID, loop))
	}
	f.submitReview(t, f.viewer, f.project.ID, loop, "scientific", "changes_requested")
	if got := f.stateOf(t, f.project.ID, loop); got != string(domain.PullRequestStateChangesRequested) {
		t.Fatalf("(3) a changes_requested review left the proposal %q, want %q", got, domain.PullRequestStateChangesRequested)
	}
	resp, payload = f.sendToReview(t, f.owner, f.project.ID, loop, "review-loop-key-00003")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("(3) re-entering review = %d: %s", resp.StatusCode, readAll(t, resp))
	}
	if payload.State != string(domain.PullRequestStateReviewRequired) {
		t.Fatalf("(3) the route answered %q, want the re-entry", payload.State)
	}
	loopRow := f.pr(t, f.project.ID, loop)
	if loopRow.state != string(domain.PullRequestStateReviewRequired) {
		t.Fatalf("(3) the stored row is %q, want %q", loopRow.state, domain.PullRequestStateReviewRequired)
	}
	from := f.auditFromStates(t, loopRow.id)
	if from[string(domain.PullRequestStateOpen)] != 1 || from[string(domain.PullRequestStateChangesRequested)] != 1 {
		t.Fatalf("(3) the two edges are not both recorded: %v (want one open and one changes_requested)", from)
	}

	// ================================================================= (4)
	// THE CELLS, one by one, over the ruled shape (A: the existing open_pr
	// cell, no new CSV row and no new action).
	//
	//	open_pr = deny,allow_from_fork,deny,allow,allow,allow,allow
	//	         (anon, non-member, viewer, contributor, maintainer, owner, agent)

	// contributor, maintainer, owner: allow. The contributor's case is
	// deliberately a proposal SOMEBODY ELSE authored — the capability the
	// cell grants under this shape, asserted rather than left implicit (see
	// RESULT.json: it is reported to the Supervisor as a consequence of the
	// ruling, not silently widened or narrowed).
	for _, tc := range []reviewActor{
		{"contributor", f.contributor},
		{"maintainer", f.maintainer},
		{"owner", f.owner},
	} {
		number := f.openPR(t, f.owner, f.project.ID, f.research.ID, f.mainBranch.ID, "cell "+tc.name, "review-cell-"+tc.name+"-0001")
		resp, _ := f.sendToReview(t, tc.client, f.project.ID, number, "review-cell-"+tc.name+"-0002")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("(4) %s sending a proposal it did not author to review = %d, want 200 (open_pr is allow for that column): %s",
				tc.name, resp.StatusCode, readAll(t, resp))
		}
		if got := f.stateOf(t, f.project.ID, number); got != string(domain.PullRequestStateReviewRequired) {
			t.Fatalf("(4) %s's accepted call left the proposal %q", tc.name, got)
		}
	}

	// viewer: deny. Not a member's proposal, and not their own either (a
	// viewer cannot open one), so the refusal is the cell's.
	viewerPR := f.openPR(t, f.owner, f.project.ID, f.research.ID, f.mainBranch.ID, "cell viewer", "review-cell-viewer-0001")
	resp, _ = f.sendToReview(t, f.viewer, f.project.ID, viewerPR, "review-cell-viewer-0002")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("(4) a viewer sending a proposal to review = %d, want 403: %s", resp.StatusCode, readAll(t, resp))
	}
	if got := f.stateOf(t, f.project.ID, viewerPR); got != string(domain.PullRequestStateOpen) {
		t.Fatalf("(4) the viewer's refused call moved the proposal: %q", got)
	}

	// anonymous: deny — and the refusal is structural (the guard answers
	// before any routing).
	anon := reviewOnce(http.DefaultClient, f.server, f.project.ID, viewerPR, "", "review-cell-anon-000001")
	if anon.err != nil {
		t.Fatalf("(4) the anonymous call failed at the transport: %v", anon.err)
	}
	if anon.status != http.StatusUnauthorized {
		t.Fatalf("(4) an anonymous caller = %d, want 401", anon.status)
	}

	// non-member on a PRIVATE project: the read gate answers the
	// existence-hiding not-found before any cell is resolved. (Criterion (7)
	// asserts the same answer for a proposal that is not there.)
	resp, _ = f.sendToReview(t, f.outsider, f.project.ID, viewerPR, "review-cell-outsider-0001")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("(4) a non-member on a private project = %d, want 404: %s", resp.StatusCode, readAll(t, resp))
	}

	// non-member WITH a fork: allow_from_fork, and the criterion asks for this
	// one to be asserted as a SUCCESS. The fork runs the production fork path;
	// only its provider round-trips are the fixture's stubs (see the stub
	// types above).
	forked := f.forkPublic(t)
	forkPR := f.openPR(t, f.external, f.public.ID, forked.branchID, f.publicMain.ID, "external contribution", "review-fork-key-000001")
	if got := f.stateOf(t, f.public.ID, forkPR); got != string(domain.PullRequestStateOpen) {
		t.Fatalf("(4) the external proposal was opened as %q", got)
	}
	resp, _ = f.sendToReview(t, f.external, f.public.ID, forkPR, "review-fork-key-000002")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("(4) a non-member sending their OWN fork's proposal to review = %d, want 200 (open_pr is allow_from_fork for that column): %s",
			resp.StatusCode, readAll(t, resp))
	}
	if got := f.stateOf(t, f.public.ID, forkPR); got != string(domain.PullRequestStateReviewRequired) {
		t.Fatalf("(4) the external contribution is %q after being sent to review, want %q", got, domain.PullRequestStateReviewRequired)
	}

	// The same non-member on a proposal of the SAME project that is not from
	// their fork: the condition fails, and the answer is the cell's own 403.
	otherForkPR := f.openPR(t, f.owner, f.public.ID, f.publicResearch.ID, f.publicMain.ID, "an upstream proposal", "review-pub-key-000001")
	resp, _ = f.sendToReview(t, f.external, f.public.ID, otherForkPR, "review-pub-key-000002")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("(4) a non-member on a proposal that is not from their fork = %d, want 403: %s", resp.StatusCode, readAll(t, resp))
	}
	if got := f.stateOf(t, f.public.ID, otherForkPR); got != string(domain.PullRequestStateOpen) {
		t.Fatalf("(4) the refused foreign-line call moved the proposal: %q", got)
	}

	// agent: allow. No route in this build carries an agent identity (the
	// session edge resolves no agent flag, so the handler tells the command
	// IsAgent=false as a statement about THIS BUILD), so the cell is asserted
	// where it is resolved — the command, with the actor the platform would
	// pass for an agent.
	//
	// The actor is a non-member on the PUBLIC project, and that is not a
	// detail: the class the agent axis decides is the matrix column, and the
	// project READ gate runs before it for every class alike — an agent whose
	// user cannot read a private project is refused by the gate, exactly as a
	// human non-member is (the same gate the (4) 404 above shows). What this
	// case isolates is the agent COLUMN: a non-member's own cell there is
	// allow_from_fork, and the agent's is an unconditional allow, so the
	// proposal the same user could not touch as themselves is one an agent
	// may send.
	agentPR := f.openPR(t, f.owner, f.public.ID, f.publicResearch.ID, f.publicMain.ID, "cell agent", "review-cell-agent-0001")
	if _, err := f.forks.RequestReview(ctx, forks.Actor{User: domain.User{ID: f.id["outsider"]}, IsAgent: true},
		forks.RequestReviewRequest{ProjectID: f.public.ID, Number: agentPR, IdempotencyKey: "review-cell-agent-0002"}); err != nil {
		t.Fatalf("(4) the agent column of open_pr is allow, and an agent's request was refused: %v", err)
	}
	if got := f.stateOf(t, f.public.ID, agentPR); got != string(domain.PullRequestStateReviewRequired) {
		t.Fatalf("(4) the agent's accepted call left the proposal %q", got)
	}
	// The same actor, the same proposal, as a HUMAN rather than an agent: the
	// non-member cell, whose condition this proposal does not meet. The two
	// answers side by side are the agent column's evidence.
	if _, err := f.forks.RequestReview(ctx, forks.Actor{User: domain.User{ID: f.id["outsider"]}},
		forks.RequestReviewRequest{ProjectID: f.public.ID, Number: agentPR, IdempotencyKey: "review-cell-human-0001"}); err == nil {
		t.Fatal("(4) a non-member sent somebody else's upstream proposal to review as a human: the open_pr condition is not resolved")
	}

	// ================================================================= (5)
	// THE REST OF THE MACHINE. open -> merged stays refused by the database
	// (asserted in (1) on the real row); a terminal proposal cannot be moved
	// by this route at all.
	for _, terminal := range []domain.PullRequestState{
		domain.PullRequestStateMerged,
		domain.PullRequestStateClosed,
		domain.PullRequestStateAborted,
	} {
		number := f.openPR(t, f.owner, f.project.ID, f.research.ID, f.mainBranch.ID, "terminal "+string(terminal), "review-term-"+string(terminal)+"-1")
		f.walkTo(t, f.project.ID, number, terminal)
		resp, _ := f.sendToReview(t, f.owner, f.project.ID, number, "review-term-"+string(terminal)+"-2")
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("(5) sending a %s proposal to review = %d, want 409: %s", terminal, resp.StatusCode, readAll(t, resp))
		}
		if got := f.stateOf(t, f.project.ID, number); got != string(terminal) {
			t.Fatalf("(5) a %s proposal is now %q: a terminal proposal moved", terminal, got)
		}
	}

	// ================================================================= (6)
	// IDEMPOTENCY AND CONCURRENCY.
	idem := f.openPR(t, f.owner, f.project.ID, f.research.ID, f.mainBranch.ID, "idempotency", "review-idem-key-00001")
	idemRow := f.pr(t, f.project.ID, idem)
	_, outboxBefore := f.outboxRows(t, f.project.ID, "pull_request.review_requested")
	first, firstBody := f.sendToReview(t, f.owner, f.project.ID, idem, "review-idem-key-00002")
	if first.StatusCode != http.StatusOK {
		t.Fatalf("(6) the first call = %d: %s", first.StatusCode, readAll(t, first))
	}
	second, secondBody := f.sendToReview(t, f.owner, f.project.ID, idem, "review-idem-key-00002")
	if second.StatusCode != http.StatusOK {
		t.Fatalf("(6) the replay = %d: %s", second.StatusCode, readAll(t, second))
	}
	if firstBody != secondBody {
		t.Fatalf("(6) the replay answered a different proposal:\n first: %+v\nsecond: %+v", firstBody, secondBody)
	}
	if n := f.auditRows(t, idemRow.id); n != 1 {
		t.Fatalf("(6) the replay wrote a second audit row: %d rows for one proposal sent to review once", n)
	}
	// The event half of the criterion: this transition emits NO domain event,
	// and that is a recorded deviation rather than an oversight. The canonical
	// event list (specs/events/event-types.yaml, which docs/18 §2 makes
	// authoritative) is exactly project/branch/state, pull_request.opened /
	// reviewed / merged, and nothing else — a review REQUEST is none of them.
	// Emitting `pull_request.reviewed` for a request that carries no judgment
	// would tell every subscriber of that event that a judgment exists when
	// none does. So the counts asserted here are: one audit row (above) and a
	// project outbox the two calls did not grow at all.
	named, after := f.outboxRows(t, f.project.ID, "pull_request.review_requested")
	if named != 0 {
		t.Fatalf("(6) %d rows claim an event name that is not in the canonical list", named)
	}
	if after != outboxBefore {
		t.Fatalf("(6) the send-to-review and its replay wrote %d outbox rows of any type, want 0", after-outboxBefore)
	}

	// The race: two calls at once with DIFFERENT keys, so nothing but the
	// compare-and-swap can decide it. Exactly one audit row and one state
	// change is the evidence that the move is a conditional update rather
	// than a read-then-write.
	race := f.openPR(t, f.owner, f.project.ID, f.research.ID, f.mainBranch.ID, "concurrency", "review-race-key-00001")
	raceRow := f.pr(t, f.project.ID, race)
	results := make([]reviewAttempt, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = reviewOnce(f.owner.client, f.server, f.project.ID, race, f.owner.csrf,
				fmt.Sprintf("review-race-key-0001%d", i))
		}(i)
	}
	wg.Wait()
	for _, r := range results {
		if r.err != nil {
			t.Fatalf("(6) a concurrent call failed at the transport: %v", r.err)
		}
		if r.status != http.StatusOK && r.status != http.StatusConflict {
			t.Fatalf("(6) a concurrent call answered %d: %s", r.status, r.body)
		}
	}
	if got := f.stateOf(t, f.project.ID, race); got != string(domain.PullRequestStateReviewRequired) {
		t.Fatalf("(6) the raced proposal is %q, want %q", got, domain.PullRequestStateReviewRequired)
	}
	if n := f.auditRows(t, raceRow.id); n != 1 {
		t.Fatalf("(6) %d audit rows for one raced move: the transition is not a single conditional update", n)
	}

	// ================================================================= (7)
	// NO EXISTENCE LEAK. Callers who may not send a proposal to review, each
	// asked about a proposal that IS there and one that is NOT: the answers
	// must be the same code and the same message, because authorization
	// resolves before any target query.
	const missing = int64(999999)
	t.Run("a member the cell denies", func(t *testing.T) {
		present := reviewOnce(f.viewer.client, f.server, f.project.ID, viewerPR, f.viewer.csrf, "review-hide-key-000001")
		absent := reviewOnce(f.viewer.client, f.server, f.project.ID, missing, f.viewer.csrf, "review-hide-key-000002")
		assertIndistinguishable(t, present, absent)
		if present.status != http.StatusForbidden {
			t.Fatalf("the viewer's answer = %d, want the cell's 403", present.status)
		}
	})
	t.Run("a non-member of the project", func(t *testing.T) {
		present := reviewOnce(f.outsider.client, f.server, f.project.ID, viewerPR, f.outsider.csrf, "review-hide-key-000003")
		absent := reviewOnce(f.outsider.client, f.server, f.project.ID, missing, f.outsider.csrf, "review-hide-key-000004")
		assertIndistinguishable(t, present, absent)
		if present.status != http.StatusNotFound {
			t.Fatalf("the non-member's answer = %d, want the read gate's 404", present.status)
		}
	})
	t.Run("a non-member whose fork condition fails", func(t *testing.T) {
		present := reviewOnce(f.external.client, f.server, f.public.ID, otherForkPR, f.external.csrf, "review-hide-key-000005")
		absent := reviewOnce(f.external.client, f.server, f.public.ID, missing, f.external.csrf, "review-hide-key-000006")
		assertIndistinguishable(t, present, absent)
		if present.status != http.StatusForbidden {
			t.Fatalf("the foreign-line answer = %d, want the condition's 403", present.status)
		}
	})

	// ================================================================= (8)
	// The route refuses a request that cannot carry a key, in place: the
	// contract makes Idempotency-Key required (components.parameters,
	// minLength 8), the same parameter :merge carries.
	keyless := f.openPR(t, f.owner, f.project.ID, f.research.ID, f.mainBranch.ID, "keyless", "review-keyless-key-01")
	resp, _ = f.sendToReview(t, f.owner, f.project.ID, keyless, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("(8) a keyless request = %d, want 400: %s", resp.StatusCode, readAll(t, resp))
	}
	if got := f.stateOf(t, f.project.ID, keyless); got != string(domain.PullRequestStateOpen) {
		t.Fatalf("(8) a keyless request moved the proposal: %q", got)
	}
	short := "short"
	resp, _ = f.sendToReview(t, f.owner, f.project.ID, keyless, short)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("(8) a key below the contract's minLength = %d, want 400: %s", resp.StatusCode, readAll(t, resp))
	}
	if got := f.stateOf(t, f.project.ID, keyless); got != string(domain.PullRequestStateOpen) {
		t.Fatalf("(8) a refused key moved the proposal: %q", got)
	}
}
