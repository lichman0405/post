package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/conflicthttp"
	"github.com/lichman0405/post/cmd/api/mergegit"
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
	"github.com/lichman0405/post/internal/config"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/gitprovider"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rsg/integrity"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

// T0410's end-to-end flows: the branch/PR journey of the seed project
// (docs/34), driven over the PRODUCT PATH — the routes cmd/api mounts, in
// front of the real auth guard, over real PostgreSQL, a real Gitea and a
// real git push.
//
// Two flows, and neither of them constructs state:
//
//	flow 1 (TestPullRequestFlowEndToEnd) — batch create → objects → PR →
//	    review → merge, ending in a merge that lands. The PR is opened by
//	    the POST the contract defines (specs/api/openapi.yaml:172-184),
//	    and the same Idempotency-Key replays onto the same proposal.
//	flow 2 (TestScientificConflictFlowEndToEnd) — the protocol scientific
//	    conflict: both sides move the same protocol parameter, the merge
//	    is REFUSED with no automatic winner, and the flow only advances
//	    when a human decides. The decision is submitted through the
//	    resolution route and carried into main.
//
// What is a fixture here, and why that is not a shortcut around the
// machine: the organization and project rows plus the Gitea repository
// (provisioning is a background job in production, and the rows are the
// substrate every flow runs ON, not a state either flow produces), the
// three memberships (alice owner, bob maintainer, carol viewer — the
// member-management API cannot add a member, only change an existing
// one's role), the routing rules and the reviewer labels (no route exists
// for either in this build), and the git push (the transport, not the
// platform's state).
//
// The ONE direct service call left in these flows is
// pullrequests.Service.RequestReview, and it is called out at every site:
// docs/43's open → review_required transition has no route — the contract
// declares no request-review operation and specs/policies/
// permissions-matrix.csv has no request_review cell — so there is nothing
// to issue an HTTP request TO. Adding one would be inventing a product
// action, which is the Supervisor's call, not this task's. Everything
// after it (review submission, the approved → merge_ready projection, the
// merge) is HTTP, and the assertions below prove the walk really happened
// by reading the events the projections wrote.

const flowTaskID = "T0410"

// ---- the wire shapes this file reads. They are declared here, not
// imported: the handlers' payload structs are unexported, and a test that
// reads the wire the way a client does is the point.

type flowBranch struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	GitRef      string  `json:"git_ref"`
	BaseStateID *string `json:"base_state_id"`
	Lifecycle   string  `json:"lifecycle_state"`
}

type flowObject struct {
	ID             string          `json:"id"`
	ObjectType     string          `json:"object_type"`
	VersionID      string          `json:"version_id"`
	VersionNo      int             `json:"version_no"`
	StateID        string          `json:"state_id"`
	Title          string          `json:"title"`
	CurrentVersion int             `json:"current_version"`
	Payload        json.RawMessage `json:"payload"`
}

type flowPR struct {
	ID              string `json:"id"`
	Number          int64  `json:"number"`
	Title           string `json:"title"`
	State           string `json:"state"`
	SourceBranchID  string `json:"source_branch_id"`
	TargetBranchID  string `json:"target_branch_id"`
	BaseStateID     string `json:"base_state_id"`
	ProposedStateID string `json:"proposed_state_id"`
	CreatedBy       string `json:"created_by"`
}

type flowReview struct {
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	Decision        string `json:"decision"`
	Responsibility  string `json:"responsibility"`
	ReviewerID      string `json:"reviewer_id"`
	ReviewedStateID string `json:"reviewed_state_id"`
}

type flowMerge struct {
	Number          int64   `json:"number"`
	MergeID         string  `json:"merge_id"`
	StateID         string  `json:"state_id"`
	PlanDigest      string  `json:"plan_digest"`
	TargetBranchID  string  `json:"target_branch_id"`
	GitState        string  `json:"git_state"`
	GitSHA          *string `json:"git_sha"`
	GitError        string  `json:"git_error"`
	Applied         int     `json:"applied"`
	Carried         int     `json:"carried"`
	Withheld        int     `json:"withheld"`
	Replayed        bool    `json:"replayed"`
	WrittenVersions []struct {
		TargetKind string `json:"target_kind"`
		TargetID   string `json:"target_id"`
		VersionID  string `json:"version_id"`
		VersionNo  int    `json:"version_no"`
	} `json:"written_versions"`
}

// flowConflict is one classified conflict as the report carries it.
type flowConflict struct {
	Category      string   `json:"category"`
	Code          string   `json:"code"`
	Fields        []string `json:"fields"`
	PayloadKeys   []string `json:"payload_keys"`
	OtherObjectID string   `json:"other_object_id"`
	Detail        string   `json:"detail"`
}

type flowView struct {
	Report struct {
		AutoMergeable  bool `json:"auto_mergeable"`
		ObjectVerdicts []struct {
			ObjectID      string         `json:"object_id"`
			ObjectType    string         `json:"object_type"`
			Kind          string         `json:"kind"`
			AutoMergeable bool           `json:"auto_mergeable"`
			Conflicts     []flowConflict `json:"conflicts"`
		} `json:"object_verdicts"`
		Summary struct {
			ObjectsAutoMergeable int `json:"objects_auto_mergeable"`
			ObjectsConflicted    int `json:"objects_conflicted"`
			ConflictsByCategory  []struct {
				Category string `json:"category"`
				Count    int    `json:"count"`
			} `json:"conflicts_by_category"`
		} `json:"summary"`
	} `json:"report"`
	Resolutions []struct {
		ID        string `json:"id"`
		TargetID  string `json:"target_id"`
		Code      string `json:"code"`
		Kind      string `json:"kind"`
		DecidedBy string `json:"decided_by"`
		Note      string `json:"note"`
	} `json:"resolutions"`
}

// flowDecision is one decision on the wire: submitted, it carries the
// classifier key the report gave plus the human's choice; answered, it
// carries the recorded row (the decider included).
type flowDecision struct {
	TargetKind    string   `json:"target_kind"`
	TargetID      string   `json:"target_id"`
	Code          string   `json:"code"`
	Fields        []string `json:"fields"`
	PayloadKeys   []string `json:"payload_keys"`
	OtherObjectID *string  `json:"other_object_id"`
	Kind          string   `json:"kind"`
	Note          string   `json:"note"`
	DecidedBy     string   `json:"decided_by"`
}

// flowFixture is the seed project (docs/34) over the real stack, with the
// HTTP surface cmd/api mounts.
type flowFixture struct {
	ctx      context.Context
	pool     *pgxpool.Pool
	ts       *httptest.Server
	adapter  *gitprovider.GiteaAdapter
	base     string
	token    string
	owner    string
	repoName string

	project domain.Project
	alice   domain.User
	bob     domain.User
	carol   domain.User
	aliceC  *testUserClient
	bobC    *testUserClient
	carolC  *testUserClient

	// main and the seed project's batch of research branches (docs/34),
	// all created through POST /branches.
	main     flowBranch
	branches map[string]flowBranch

	// forkPoint is main's head when the research branches were cut — the
	// base every proposal of this fixture is pinned to.
	forkPoint string
	// claimV1 is the version the seed finding pins.
	claimV1 string

	statesSvc  *states.Service
	prSvc      *pullrequests.Service
	mergeStore *persistence.SemanticMergeStore
}

// newFlowFixture builds the whole stack and seeds the demo project. Every
// step below that creates platform state does it over HTTP; the fixture's
// own writes are the substrate (rows of the project, its repository, its
// membership and its review routing), named in the file header.
func newFlowFixture(t *testing.T, ctx context.Context) *flowFixture {
	t.Helper()
	requireGit(t)
	base := requireGitea(t)
	token := giteaServiceToken(t, base)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), flowTaskID)

	cfg := gitprovider.Config{
		BaseURL:    base,
		Token:      config.Secret(token),
		WebhookURL: "http://host.invalid/api/v1/git/hooks/gitea",
	}
	adapter := gitprovider.NewGiteaAdapter(cfg)
	owner, err := adapter.Owner(ctx)
	if err != nil {
		t.Fatalf("gitea integration: resolve the service identity: %v", err)
	}

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
	branchStore := persistence.NewBranchStore(pool)
	branchSvc := branches.NewService(branchStore)
	statesSvc := states.NewService(stateStore, newCommitGuard(t))
	rsgSvc := rsg.NewService(rsg.Deps{
		Projects:  projectSvc,
		Branches:  branchSvc,
		States:    statesSvc,
		Latest:    stateStore,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
		Events:    events.Recorder{},
	})
	prStore := persistence.NewPullRequestStore(pool)
	prSvc := pullrequests.NewService(prStore)
	diffSvc := diffs.NewService(stateStore, persistence.NewManifestStore(pool))
	diffOfPR := prdiff.NewService(prStore, branchStore, diffSvc)
	resolutionSvc := resolutions.NewService(diffSvc, resolutions.NewPGStore(pool), projectSvc, authz.NewMatrixEngine())
	checksSvc := prchecks.NewService(prchecks.Deps{
		PRs:      prStore,
		Projects: projectStore,
		States:   stateStore,
		Branches: branchStore,
		Manifest: persistence.NewManifestStore(pool),
		Policies: policyStore,
		Engine:   integrity.New(reg),
	})
	mergeStore := persistence.NewSemanticMergeStore(pool)
	mergeSvc := merge.NewService(merge.Deps{
		Store:     mergeStore,
		Diffs:     diffSvc,
		Plans:     resolutionSvc,
		Commits:   statesSvc,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Projects:  projectSvc,
		Authz:     authz.NewMatrixEngine(),
		Checks:    checksSvc,
		Policies:  policyAPI.Service(),
		Rules:     policy.NewRuleEvaluator(),
		Events:    events.Recorder{},
		Git:       mergegit.New(adapter, gitprovider.NewUserAccessStore(pool)),
		RefGuard:  gitprovider.RefGuard{MergeService: owner},
	})
	// The open-pull-request command: the forks service, whose OpenExternalPR
	// resolves the open_pr cell against the real membership (and, for a
	// non-member, the fork lineage) and proposes through the same pull-request
	// path. This is the production wiring the POST route calls.
	forksSvc := forks.NewService(forks.Deps{
		Projects:     projectSvc,
		Branches:     branchSvc,
		BranchWriter: rsgSvc,
		Forks:        persistence.NewForkStore(pool),
		PullRequests: prSvc,
		Authz:        authz.NewMatrixEngine(),
	})
	routingSvc := responsibilities.NewService(responsibilities.Deps{
		Rules:     persistence.NewResponsibilityStore(pool),
		Projects:  projectStore,
		Members:   projectSvc,
		PRs:       prStore,
		Branches:  branchStore,
		Diffs:     diffOfPR,
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

	authAPI := authhttp.New(authhttp.Deps{
		Users:    persistence.NewCredentialStore(pool),
		Sessions: memstore.NewSessions(),
		Limiter:  memstore.NewLimiter(),
		Cfg: authn.Config{
			WebOrigin:          "http://web.test",
			SessionTTL:         time.Hour,
			LoginLimitPerEmail: 1000,
			LoginLimitPerIP:    10000,
			LoginWindow:        time.Minute,
			SignupLimitPerIP:   10000,
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
		Create:       forksSvc,
		Checks:       checksSvc,
		Diff:         diffOfPR,
		Projects:     projectSvc,
	}).Register(apiMux)
	reviewhttp.New(reviewhttp.Deps{Service: reviewsSvc, Projects: projectSvc}).Register(apiMux)
	mergehttp.New(mergehttp.Deps{Command: mergeSvc, Projects: projectSvc}).Register(apiMux)
	conflicthttp.New(conflicthttp.Deps{Viewer: resolutionSvc, Saver: resolutionSvc, Projects: projectSvc}).Register(apiMux)
	ts := httptest.NewServer(authAPI.Guard(apiMux))
	t.Cleanup(ts.Close)

	f := &flowFixture{
		ctx: ctx, pool: pool, ts: ts, adapter: adapter, base: base, token: token, owner: owner,
		branches:   map[string]flowBranch{},
		statesSvc:  statesSvc,
		prSvc:      prSvc,
		mergeStore: mergeStore,
	}

	// ---- Three humans, created through the real signup endpoint: the
	// session cookies and CSRF tokens every request below carries are the
	// ones this endpoint minted.
	f.aliceC, f.alice.ID = signup(t, ts.URL, "flow-owner@example.com", "flow-owner")
	f.bobC, f.bob.ID = signup(t, ts.URL, "flow-maintainer@example.com", "flow-maintainer")
	f.carolC, f.carol.ID = signup(t, ts.URL, "flow-reviewer@example.com", "flow-reviewer")

	// ---- The project, provisioned for real (repository, bootstrap main,
	// webhook, T0302's protection rule) by the production provisioner. The
	// rows come first because provisioning is driven by a background job in
	// production.
	org, _, err := orgStore.CreateOrganization(ctx, domain.Organization{
		Slug: "mof-humidity", Name: "MOF Humidity",
	}, f.alice.ID, todayUTC())
	if err != nil {
		t.Fatalf("create the fixture organization: %v", err)
	}
	project, _, err := projectStore.CreateProject(ctx, domain.Project{
		OrganizationID:  &org.ID,
		Slug:            "demo-mof-humidity-separation",
		Name:            "MOF Gas Separation (demo)",
		Purpose:         "verify MOF-X C2H4/C2H6 separation performance at 40-70% RH (T0410 e2e)",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, f.alice.ID)
	if err != nil {
		t.Fatalf("create the fixture project: %v", err)
	}
	f.project = project
	if err := gitprovider.NewProvisioner(adapter, gitprovider.NewProvisionStore(pool), cfg.WebhookURL).
		Provision(ctx, project.ID); err != nil {
		t.Fatalf("gitea integration: provision the project: %v", err)
	}
	f.repoName = gitprovider.RepositoryName(project.ID)
	t.Cleanup(func() { deleteGiteaRepo(t, base, token, owner, f.repoName) })

	// ---- The two other humans' memberships and the review routing. Both
	// are fixture data this build has no route for: the member-management
	// API changes an existing membership's role but cannot add one, and
	// nothing mounts the Research Owners rules.
	for _, m := range []struct {
		user domain.User
		role domain.ProjectRole
		// label is the one responsibility the reviewer holds, so the label
		// a review is recorded under is never ambiguous (the recorded label
		// is the sorted-first held label that the routing asked for).
		label string
	}{
		{f.bob, domain.ProjectRoleMaintainer, "Experimental Reviewer"},
		{f.carol, domain.ProjectRoleViewer, "Data Reviewer"},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, $3)`,
			project.ID, m.user.ID, m.role); err != nil {
			t.Fatalf("seed the %s membership: %v", m.role, err)
		}
		if _, err := routingSvc.Assign(ctx, f.alice, project.ID, m.user.ID, m.label); err != nil {
			t.Fatalf("assign %s to %s: %v", m.label, m.user.ID, err)
		}
	}
	// One rule per object type these flows move: the protocol change is
	// the scientific one (docs/09 §7's "Protocol 冲突数值折中") and the claim
	// is the knowledge one. A change no rule routes makes the proposal
	// unsatisfiable, which is exactly why the rules are part of the fixture
	// rather than a convenience.
	for _, rule := range []struct{ objectType, label string }{
		{"protocol", "Experimental Reviewer"},
		{"claim", "Data Reviewer"},
	} {
		if _, err := routingSvc.AddRule(ctx, f.alice, project.ID, responsibilities.AddRuleInput{
			MatchKind:      domain.ResearchOwnerMatchObjectType,
			MatchValue:     rule.objectType,
			Responsibility: rule.label,
		}); err != nil {
			t.Fatalf("route %s changes to %s: %v", rule.objectType, rule.label, err)
		}
	}
	// The governance policy the merge reads: main_protected true, on the
	// project's own scope, written through the real store.
	seedProjectPolicy(t, ctx, policyStore, project, f.alice.ID)

	f.seedBranchesAndObjects(t)
	return f
}

// seedBranchesAndObjects creates the project's branches and its main-line
// objects — the "batch create → objects" half of docs/34's seed flow.
// Every state this produces is a write on the product path: POST
// /branches and POST /branches/{id}/objects.
func (f *flowFixture) seedBranchesAndObjects(t *testing.T) {
	t.Helper()
	// main, then the seed project's four research branches, in one batch —
	// each cut from main's head at the moment of the batch.
	f.main = f.branchCreate(t, f.aliceC, "main", "")
	syncer := gitprovider.NewBranchRefSyncer(f.adapter, gitprovider.NewBranchRefStore(f.pool))
	if err := syncer.Sync(f.ctx, f.main.ID); err != nil {
		t.Fatalf("gitea integration: sync main's ref: %v", err)
	}

	// The main-line objects: one claim, the protocol, and a finding that
	// pins the claim's first version (the cross-object reference a real
	// project carries).
	claim := f.objectCreate(t, f.aliceC, f.main.ID, "claim", mergeMainGateClaim("MOF-X selectivity holds at 40% RH"))
	f.claimV1 = claim.VersionID
	f.objectCreate(t, f.aliceC, f.main.ID, "protocol", mergeProtocolPayload("300 K"))
	f.objectCreate(t, f.aliceC, f.main.ID, "finding", mergeMainGateFinding("water uptake tracks the selectivity loss", claim.VersionID))

	f.forkPoint = f.head(t, f.main.ID)
	for _, name := range []string{"humidity-40rh", "humidity-70rh", "mechanism-water-binding", "protocol-activation-180c"} {
		branch := f.branchCreate(t, f.aliceC, name, f.forkPoint)
		f.branches[name] = branch
		if err := syncer.Sync(f.ctx, branch.ID); err != nil {
			t.Fatalf("gitea integration: sync %s's ref: %v", name, err)
		}
	}
	if got := mainHead(t, f.base, f.token, f.owner, f.repoName); got == "" {
		t.Fatal("gitea integration: the provisioned repository has no main ref")
	}
}

// ---- request helpers: one request, one checked status, one decoded
// answer. Nothing here talks to a service.

func (f *flowFixture) req(t *testing.T, uc *testUserClient, method, path, body string, want int) *http.Response {
	t.Helper()
	resp := uc.do(t, method, path, body)
	if resp.StatusCode != want {
		t.Fatalf("%s %s = %d, want %d: %s", method, path, resp.StatusCode, want, readAll(t, resp))
	}
	return resp
}

func decodeFlow[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var out T
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %T: %v (body %s)", out, err, raw)
	}
	return out
}

func (f *flowFixture) branchCreate(t *testing.T, uc *testUserClient, name, baseRef string) flowBranch {
	t.Helper()
	body := fmt.Sprintf(`{"name":%q,"base_ref":%q,"visibility":"private"}`, name, baseRef)
	resp := f.req(t, uc, http.MethodPost, "/api/v1/projects/"+f.project.ID+"/branches", body, http.StatusCreated)
	return decodeFlow[flowBranch](t, resp)
}

func (f *flowFixture) objectCreate(t *testing.T, uc *testUserClient, branchID, objectType, payload string) flowObject {
	t.Helper()
	body := fmt.Sprintf(`{"object_type":%q,"payload":%s}`, objectType, payload)
	resp := f.req(t, uc, http.MethodPost,
		"/api/v1/projects/"+f.project.ID+"/branches/"+branchID+"/objects", body, http.StatusCreated)
	return decodeFlow[flowObject](t, resp)
}

func (f *flowFixture) objectUpdate(t *testing.T, uc *testUserClient, branchID, objectID string, expected int, patch string) flowObject {
	t.Helper()
	body := fmt.Sprintf(`{"expected_version":%d,"patch":%s}`, expected, patch)
	resp := f.req(t, uc, http.MethodPost,
		"/api/v1/projects/"+f.project.ID+"/branches/"+branchID+"/objects/"+objectID+":version",
		body, http.StatusCreated)
	return decodeFlow[flowObject](t, resp)
}

func (f *flowFixture) objectRead(t *testing.T, uc *testUserClient, branchID, objectID string) flowObject {
	t.Helper()
	resp := f.req(t, uc, http.MethodGet,
		"/api/v1/projects/"+f.project.ID+"/branches/"+branchID+"/objects/"+objectID, "", http.StatusOK)
	return decodeFlow[flowObject](t, resp)
}

// prOpen issues the contract's open-pull-request request and returns the
// handler's answer with the status, so a test can assert a replay.
func (f *flowFixture) prOpen(t *testing.T, uc *testUserClient, sourceBranchID, title, key string) (flowPR, int) {
	t.Helper()
	body := fmt.Sprintf(`{"source_branch_id":%q,"target_branch_id":%q,"title":%q,"body":"docs/34 seed flow (T0410 e2e)"}`,
		sourceBranchID, f.main.ID, title)
	resp := uc.doKeyed(t, http.MethodPost, "/api/v1/projects/"+f.project.ID+"/pull-requests", body, key)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("open pull request = %d: %s", resp.StatusCode, readAll(t, resp))
	}
	return decodeFlow[flowPR](t, resp), resp.StatusCode
}

func (f *flowFixture) prGet(t *testing.T, uc *testUserClient, number int64) flowPR {
	t.Helper()
	resp := f.req(t, uc, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/pull-requests/%d", f.project.ID, number), "", http.StatusOK)
	return decodeFlow[flowPR](t, resp)
}

func (f *flowFixture) reviewSubmit(t *testing.T, uc *testUserClient, number int64, kind, decision, body string) flowReview {
	t.Helper()
	payload := fmt.Sprintf(`{"kind":%q,"decision":%q,"body":%q}`, kind, decision, body)
	resp := f.req(t, uc, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/pull-requests/%d/reviews", f.project.ID, number),
		payload, http.StatusCreated)
	return decodeFlow[flowReview](t, resp)
}

// mergeThrough issues one merge request and returns the status and the
// handler's payload — the refusal path and the landing path are the same
// request, so a test that expects a refusal can read its envelope.
func (f *flowFixture) mergeThrough(t *testing.T, uc *testUserClient, number int64, key string) (flowMerge, int, string) {
	t.Helper()
	path := mergePath(f.project.ID, number)
	resp := uc.doKeyed(t, http.MethodPost, path, "", key)
	raw := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		var env errorEnvelope
		if err := json.Unmarshal([]byte(raw), &env); err != nil {
			t.Fatalf("decode the merge refusal: %v (body %s)", err, raw)
		}
		return flowMerge{}, resp.StatusCode, env.Code
	}
	var out flowMerge
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode the merge payload: %v (body %s)", err, raw)
	}
	return out, resp.StatusCode, ""
}

// head reads a branch's head state — a READ, the value the next write
// names as its base.
func (f *flowFixture) head(t *testing.T, branchID string) string {
	t.Helper()
	state, err := f.statesSvc.GetBranchHead(f.ctx, branchID)
	if err != nil {
		t.Fatalf("read the head of branch %s: %v", branchID, err)
	}
	return state.ID
}

// walkToMergeReady drives open → review_required → approved → merge_ready.
// The reviews are HTTP submissions; the only direct call is RequestReview,
// the transition with no route (see the file header), and the assertions
// afterwards prove the advance was performed by the review store's own
// projection — review_required → approved → merge_ready, recorded in the
// submission that satisfied the calculation.
func (f *flowFixture) walkToMergeReady(t *testing.T, number int64) {
	t.Helper()
	pr := f.prGet(t, f.carolC, number)
	if pr.State != string(domain.PullRequestStateOpen) {
		t.Fatalf("pull request #%d is %q after the POST that opened it, want %q", number, pr.State, domain.PullRequestStateOpen)
	}

	// open → review_required. The ONLY direct service call of these flows:
	// docs/43's first transition has no HTTP route — the contract declares
	// no request-review operation and the permissions matrix has no
	// request_review cell — so there is nothing to issue a request to.
	if _, err := f.prSvc.RequestReview(f.ctx, f.project.ID, number); err != nil {
		t.Fatalf("request review on #%d: %v", number, err)
	}
	if pr := f.prGet(t, f.carolC, number); pr.State != string(domain.PullRequestStateReviewRequired) {
		t.Fatalf("pull request #%d is %q after RequestReview, want %q", number, pr.State, domain.PullRequestStateReviewRequired)
	}

	// The reviews: the routed scientific judgments and the whole-proposal
	// integrity one. The reviewer of each is the holder of the label its
	// change was routed to, and the recorded label is what the routing
	// asked for (never a label the client named).
	scientific := f.reviewSubmit(t, f.bobC, number, "scientific", "approved", "the protocol's numbers check out")
	if scientific.Responsibility != "Experimental Reviewer" {
		t.Fatalf("the protocol review was recorded under %q, want the routed label", scientific.Responsibility)
	}
	knowledge := f.reviewSubmit(t, f.carolC, number, "scientific", "approved", "the claim follows from the measurements")
	if knowledge.Responsibility != "Data Reviewer" {
		t.Fatalf("the claim review was recorded under %q, want the routed label", knowledge.Responsibility)
	}
	// The second and third reviews do not advance (the calculation is not
	// satisfied yet), which is the machine's own behaviour and worth
	// pinning: a review is a recorded judgment, not a vote that moves.
	if pr := f.prGet(t, f.carolC, number); pr.State != string(domain.PullRequestStateReviewRequired) {
		t.Fatalf("pull request #%d is %q with one requirement still missing, want %q",
			number, pr.State, domain.PullRequestStateReviewRequired)
	}
	integrityReview := f.reviewSubmit(t, f.bobC, number, "integrity", "approved", "sources and pins are sound")
	if integrityReview.Responsibility == "" {
		t.Fatal("the integrity review was recorded with no responsibility: the whole-proposal judgment is satisfied only under a routed label")
	}

	// approved → merge_ready, by the store's projection inside the
	// submission that satisfied the calculation.
	if pr := f.prGet(t, f.carolC, number); pr.State != string(domain.PullRequestStateMergeReady) {
		t.Fatalf("pull request #%d is %q once every requirement is met, want %q",
			number, pr.State, domain.PullRequestStateMergeReady)
	}
}

// reviewWalk reads the proposal's review events, oldest first — the
// record the submission itself wrote, which is what makes the walk above
// a fact rather than an assertion about the current row.
func (f *flowFixture) reviewWalk(t *testing.T, number int64) []reviewEvent {
	t.Helper()
	rows, err := f.pool.Query(f.ctx, `
		SELECT payload->>'responsibility', payload->>'state_before', payload->>'state_after', payload->>'advanced'
		  FROM outbox_events
		 WHERE project_id = $1::uuid AND event_type = 'pull_request.reviewed' AND payload->>'pull_request_number' = $2
		 ORDER BY created_at, id`, f.project.ID, fmt.Sprint(number))
	if err != nil {
		t.Fatalf("read the review events of #%d: %v", number, err)
	}
	defer rows.Close()
	var out []reviewEvent
	for rows.Next() {
		var e reviewEvent
		var advanced string
		if err := rows.Scan(&e.Responsibility, &e.StateBefore, &e.StateAfter, &advanced); err != nil {
			t.Fatalf("scan a review event: %v", err)
		}
		e.Advanced = advanced == "true"
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the review events: %v", err)
	}
	return out
}

// TestPullRequestFlowEndToEnd is docs/34's conflict-free merge over the
// product path: batch create → objects → PR → review → merge.
func TestPullRequestFlowEndToEnd(t *testing.T) {
	ctx := testCtx(t)
	f := newFlowFixture(t, ctx)

	// ---- The branch's work: the seed project's 40% RH line. A real
	// research commit is pushed first (the provider will not merge a pair
	// with nothing to merge), then the two proposed objects are created
	// through POST /objects on the branch.
	branch := f.branches["humidity-40rh"]
	pushResearchBranch(t, f.base, f.token, f.owner, f.repoName, branch.Name, "40% RH campaign")
	proposedClaim := f.objectCreate(t, f.aliceC, branch.ID, "claim", mergeMainGateClaim("selectivity is retained at 40% RH"))
	f.objectCreate(t, f.aliceC, branch.ID, "protocol", flowProtocolPayload("40 RH activation protocol", "320 K"))

	// ---- The proposal, opened by the route the contract defines. The
	// request carries the contract-required Idempotency-Key; the answer is
	// 201 with the proposal it opened.
	const openKey = "T0410-flow-one-open-0001"
	pr, status := f.prOpen(t, f.aliceC, branch.ID, "40% RH campaign merges into main", openKey)
	if status != http.StatusCreated {
		t.Fatalf("open pull request status = %d, want 201", status)
	}
	if pr.Number < 1 {
		t.Fatalf("the opened proposal has number %d, want the project's first number", pr.Number)
	}
	if pr.State != string(domain.PullRequestStateOpen) {
		t.Fatalf("the opened proposal is %q, want %q", pr.State, domain.PullRequestStateOpen)
	}
	if pr.CreatedBy != f.alice.ID {
		t.Fatalf("the proposal records author %q, want the session's %q", pr.CreatedBy, f.alice.ID)
	}
	if pr.ProposedStateID == "" || pr.BaseStateID == "" {
		t.Fatal("the opened proposal pins no states: nothing names what would be merged")
	}
	if pr.BaseStateID != f.forkPoint {
		t.Fatalf("the proposal's base is %s, want main's head at the fork point %s", pr.BaseStateID, f.forkPoint)
	}
	if pr.ProposedStateID != f.head(t, branch.ID) {
		t.Fatalf("the proposal's proposed head is %s, want the source branch's head %s", pr.ProposedStateID, f.head(t, branch.ID))
	}

	// The creation key is a promise: repeating the request returns the
	// proposal the first one opened, and opens no second one (migration
	// 00089's partial unique index is what makes the promise hold under
	// concurrency, not a lock this test can see).
	replay, replayStatus := f.prOpen(t, f.aliceC, branch.ID, "40% RH campaign merges into main", openKey)
	if replayStatus != http.StatusCreated {
		t.Fatalf("the repeated open answered %d, want 201 with the same proposal", replayStatus)
	}
	if replay.ID != pr.ID || replay.Number != pr.Number {
		t.Fatalf("the repeated open answered #%d (%s), want the first proposal #%d (%s)", replay.Number, replay.ID, pr.Number, pr.ID)
	}
	resp := f.req(t, f.aliceC, http.MethodGet, "/api/v1/projects/"+f.project.ID+"/pull-requests", "", http.StatusOK)
	list := decodeFlow[[]flowPR](t, resp)
	if len(list) != 1 {
		t.Fatalf("the project holds %d pull requests after a repeated open, want exactly one", len(list))
	}

	// ---- The review walk, then the merge over the contract's route.
	f.walkToMergeReady(t, pr.Number)

	// The walk is a fact of the record, not of the row: the submission that
	// satisfied the calculation moved review_required → merge_ready itself
	// (two compare-and-swaps inside one transaction), and its event says so.
	// A SetState shortcut writes no such event.
	events := f.reviewWalk(t, pr.Number)
	if len(events) != 3 {
		t.Fatalf("the proposal's review events = %+v, want one per submitted review", events)
	}
	last := events[len(events)-1]
	if last.StateBefore != string(domain.PullRequestStateReviewRequired) ||
		last.StateAfter != string(domain.PullRequestStateMergeReady) || !last.Advanced {
		t.Fatalf("the satisfying submission reported %s → %s (advanced %v), want %s → %s advanced",
			last.StateBefore, last.StateAfter, last.Advanced,
			domain.PullRequestStateReviewRequired, domain.PullRequestStateMergeReady)
	}
	var completed int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM audit_log
		 WHERE action = $1 AND after_summary->>'pull_request_number' = $2`,
		domain.ActionPullRequestReviewCompleted, fmt.Sprint(pr.Number)).Scan(&completed); err != nil {
		t.Fatalf("count the review-completed audit rows: %v", err)
	}
	if completed != 1 {
		t.Fatalf("review-completed audit rows = %d, want exactly one advance", completed)
	}

	before := mainHead(t, f.base, f.token, f.owner, f.repoName)
	merged, status, code := f.mergeThrough(t, f.bobC, pr.Number, "T0410-flow-one-merge-0001")
	if status != http.StatusOK {
		t.Fatalf("the merge answered %d %s, want 200", status, code)
	}
	if merged.Replayed {
		t.Fatal("the first merge reports itself as a replay")
	}
	if merged.GitState != string(domain.GitStateUpdated) {
		t.Fatalf("merge git state = %q (error %q), want %q", merged.GitState, merged.GitError, domain.GitStateUpdated)
	}
	if merged.GitSHA == nil || *merged.GitSHA == "" {
		t.Fatal("the merge produced no commit sha")
	}
	if merged.Applied != 2 {
		t.Fatalf("the merge applied %d change(s), want the two the branch proposed", merged.Applied)
	}
	if merged.StateID == "" {
		t.Fatal("the merge names no accepted state")
	}

	// main advanced: the platform's own head is the accepted state, the
	// provider's ref is at the commit the merge recorded, and the accepted
	// state carries the proposed objects.
	if head := f.head(t, f.main.ID); head != merged.StateID {
		t.Fatalf("main's head is %s, the merge accepted %s", head, merged.StateID)
	}
	if after := mainHead(t, f.base, f.token, f.owner, f.repoName); after != *merged.GitSHA {
		t.Fatalf("the provider's main ref is at %s, the merge recorded %s", after, *merged.GitSHA)
	} else if after == before {
		t.Fatal("the provider's main ref did not move")
	}
	if pr := f.prGet(t, f.carolC, pr.Number); pr.State != string(domain.PullRequestStateMerged) {
		t.Fatalf("the pull request is %q after its merge, want %q", pr.State, domain.PullRequestStateMerged)
	}
	accepted := f.objectRead(t, f.aliceC, f.main.ID, proposedClaim.ID)
	if accepted.VersionNo <= proposedClaim.VersionNo {
		t.Fatalf("the accepted state carries claim v%d, the proposal created v%d", accepted.VersionNo, proposedClaim.VersionNo)
	}
	var body struct {
		Statement string `json:"statement"`
	}
	if err := json.Unmarshal(accepted.Payload, &body); err != nil {
		t.Fatalf("decode the accepted claim: %v", err)
	}
	if body.Statement != "selectivity is retained at 40% RH" {
		t.Fatalf("the accepted claim says %q, want the proposal's statement", body.Statement)
	}
}

// TestScientificConflictFlowEndToEnd is docs/34's protocol conflict over
// the product path: both sides move the same protocol parameter, nothing
// picks a winner, and the flow advances only when a human decides.
func TestScientificConflictFlowEndToEnd(t *testing.T) {
	ctx := testCtx(t)
	f := newFlowFixture(t, ctx)

	// ---- The contested object: the protocol main already carries.
	proto := f.objectRead(t, f.aliceC, f.main.ID, f.protocolOnMain(t))

	// ---- The branch moves the protocol's temperature, and adds a claim
	// the merge will be able to land (a plan with nothing to apply is
	// refused, and the point of this flow is that the CONFLICT is not
	// resolved automatically — not that nothing else may merge).
	branch := f.branches["protocol-activation-180c"]
	pushResearchBranch(t, f.base, f.token, f.owner, f.repoName, branch.Name, "180C activation campaign")
	f.objectUpdate(t, f.aliceC, branch.ID, proto.ID, proto.CurrentVersion,
		`{"parameters":{"temperature":"350 K"}}`)
	appliedClaim := f.objectCreate(t, f.aliceC, branch.ID, "claim", mergeMainGateClaim("activation at 180 C needs a re-run"))

	pr, _ := f.prOpen(t, f.aliceC, branch.ID, "180 C activation protocol", "T0410-flow-two-open-0001")
	f.walkToMergeReady(t, pr.Number)

	// ---- main moves the same parameter to a different value. This is the
	// divergence the merge must not resolve: neither 350 K nor 400 K is
	// the merge's to choose, and an average is a value nobody proposed.
	f.objectUpdate(t, f.aliceC, f.main.ID, proto.ID, 2, `{"parameters":{"temperature":"400 K"}}`)
	targetHead := f.head(t, f.main.ID)
	before := mainHead(t, f.base, f.token, f.owner, f.repoName)

	// ---- The merge, over the route, with no decision recorded: refused.
	refused, status, code := f.mergeThrough(t, f.bobC, pr.Number, "T0410-flow-two-merge-0001")
	if status != http.StatusConflict {
		t.Fatalf("the merge over an undecided scientific conflict answered %d %s, want 409", status, code)
	}
	if code != merge.CodeMergeBlocked {
		t.Fatalf("the refusal code = %q, want %q", code, merge.CodeMergeBlocked)
	}
	if refused.StateID != "" || refused.GitSHA != nil {
		t.Fatalf("the refused merge answered with records: %+v", refused)
	}
	// Nothing was written and nothing moved: main is where it was, on the
	// platform and on the provider, and the proposal is still merge_ready.
	if head := f.head(t, f.main.ID); head != targetHead {
		t.Fatalf("main's head is %s after the refused merge, want %s", head, targetHead)
	}
	if after := mainHead(t, f.base, f.token, f.owner, f.repoName); after != before {
		t.Fatalf("the provider's main ref moved to %s on a refused merge", after)
	}
	if pr := f.prGet(t, f.carolC, pr.Number); pr.State != string(domain.PullRequestStateMergeReady) {
		t.Fatalf("the pull request is %q after the refused merge, want %q", pr.State, domain.PullRequestStateMergeReady)
	}
	var merges int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM semantic_merges`).Scan(&merges); err != nil {
		t.Fatalf("count the merge records: %v", err)
	}
	if merges != 0 {
		t.Fatalf("the refused merge wrote %d merge record(s)", merges)
	}
	// The claim the plan COULD have applied was not materialized either: it
	// exists on the branch that proposed it and in no state of main. (A
	// refused merge is all-or-nothing — it does not land the safe half.)
	if got := f.versionsOnBranch(t, appliedClaim.ID, f.main.ID); got != 0 {
		t.Fatalf("the refused merge materialized %d version(s) of the claim in main's history", got)
	}

	// ---- The conflict, read over its own route: the detector's verdict on
	// the protocol is a scientific conflict, and no decision exists yet.
	view := f.conflicts(t, f.carolC, pr.BaseStateID, pr.ProposedStateID, targetHead)
	if view.Report.Summary.ObjectsConflicted != 1 {
		t.Fatalf("the report counts %d conflicted object(s), want exactly the protocol: %+v",
			view.Report.Summary.ObjectsConflicted, view.Report.Summary)
	}
	conflicts := f.verdictConflicts(t, view, proto.ID)
	scientific := f.conflictWithCode(t, conflicts, "SCIENTIFIC_FIELD_DIVERGES")
	if scientific.Category != "scientific" {
		t.Fatalf("the protocol conflict is categorized %q, want scientific", scientific.Category)
	}
	if len(view.Resolutions) != 0 {
		t.Fatalf("the report already carries %d decision(s) no human made", len(view.Resolutions))
	}
	if view.Report.AutoMergeable {
		t.Fatal("the report calls a proposal with an undecided scientific conflict auto-mergeable")
	}

	// ---- The human decision, submitted through the resolution route. The
	// classifier key comes from the report (the decision addresses exactly
	// the conflict the detector found); the choice is the human's. Keeping
	// both sides is the action that neither picks a winner nor discards
	// one: the disagreement becomes a fact of the accepted state.
	decisions := make([]flowDecision, 0, len(conflicts))
	for _, c := range conflicts {
		decisions = append(decisions, flowDecision{
			TargetKind: "object", TargetID: proto.ID,
			Code: c.Code, Fields: nonNil(c.Fields), PayloadKeys: nonNil(c.PayloadKeys),
			OtherObjectID: nil,
			Kind:          string(domain.ResolutionKeepBoth),
			Note:          "both readings stand until the follow-up experiment reports",
		})
	}
	saved := f.putResolutions(t, f.aliceC, pr.BaseStateID, pr.ProposedStateID, targetHead, decisions)
	if len(saved) != len(decisions) {
		t.Fatalf("the resolution route recorded %d decision(s), %d were submitted", len(saved), len(decisions))
	}
	for _, row := range saved {
		if row.Kind != string(domain.ResolutionKeepBoth) {
			t.Fatalf("a recorded decision is %q, want %q", row.Kind, domain.ResolutionKeepBoth)
		}
		if row.DecidedBy != f.alice.ID {
			t.Fatalf("a recorded decision names %q as the decider, want the session's %q", row.DecidedBy, f.alice.ID)
		}
	}

	// ---- The human action advanced the flow: the same merge now lands,
	// carrying the conflict instead of resolving it.
	landed, status, code := f.mergeThrough(t, f.bobC, pr.Number, "T0410-flow-two-merge-0002")
	if status != http.StatusOK {
		t.Fatalf("the merge after the human decision answered %d %s, want 200", status, code)
	}
	if landed.Carried != 1 {
		t.Fatalf("the merge carried %d conflict(s), want exactly one", landed.Carried)
	}
	if landed.Applied != 1 {
		t.Fatalf("the merge applied %d change(s), want the claim that was safe to apply", landed.Applied)
	}
	for _, v := range landed.WrittenVersions {
		if v.TargetID == proto.ID {
			t.Fatalf("the merge wrote a version of the contested protocol (v%d): the conflict was resolved, not carried", v.VersionNo)
		}
	}
	carried, err := f.mergeStore.ListCarriedConflicts(ctx, landed.MergeID)
	if err != nil {
		t.Fatalf("read the carried conflicts: %v", err)
	}
	if len(carried) != 1 {
		t.Fatalf("the accepted state carries %d conflict(s), want exactly one", len(carried))
	}
	if carried[0].TargetID != proto.ID || carried[0].ResultStateID != landed.StateID {
		t.Fatalf("the carried conflict names target %s in state %s, want %s in %s",
			carried[0].TargetID, carried[0].ResultStateID, proto.ID, landed.StateID)
	}
	if carried[0].DecidedBy != f.alice.ID {
		t.Fatalf("the carried conflict names %q as the decider, want the human %q", carried[0].DecidedBy, f.alice.ID)
	}
	if carried[0].SourceVersionID == "" || carried[0].TargetVersionID == nil || *carried[0].TargetVersionID == "" {
		t.Fatal("the carried conflict does not name both sides")
	}
	if *carried[0].TargetVersionID == carried[0].SourceVersionID {
		t.Fatal("the carried conflict names the same version on both sides")
	}

	// main advanced to the accepted state, and the contested object is
	// still main's own reading: keeping both sides means no winner.
	if head := f.head(t, f.main.ID); head != landed.StateID {
		t.Fatalf("main's head is %s, the merge accepted %s", head, landed.StateID)
	}
	accepted := f.objectRead(t, f.aliceC, f.main.ID, proto.ID)
	var payload struct {
		Parameters struct {
			Temperature string `json:"temperature"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(accepted.Payload, &payload); err != nil {
		t.Fatalf("decode the accepted protocol: %v", err)
	}
	if payload.Parameters.Temperature != "400 K" {
		t.Fatalf("the accepted protocol's temperature is %q, want main's own 400 K — no side may win automatically",
			payload.Parameters.Temperature)
	}
	// The proposal's other work did land, in the same accepted state.
	var claimInState int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM scientific_object_versions WHERE object_id = $1 AND state_id = $2::uuid`,
		appliedClaim.ID, landed.StateID).Scan(&claimInState); err != nil {
		t.Fatalf("read the accepted claim: %v", err)
	}
	if claimInState != 1 {
		t.Fatalf("the accepted state carries %d version(s) of the claim that was safe to apply, want one", claimInState)
	}
}

// ---- remaining flow helpers

// protocolOnMain returns the id of the protocol the fixture seeded on main.
func (f *flowFixture) protocolOnMain(t *testing.T) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(f.ctx, `
		SELECT DISTINCT object_id::text FROM scientific_object_versions v
		  JOIN scientific_objects o ON o.id = v.object_id
		 WHERE o.project_id = $1::uuid AND o.object_type = 'protocol'`, f.project.ID).Scan(&id); err != nil {
		t.Fatalf("find the protocol on main: %v", err)
	}
	return id
}

// conflicts reads the conflict view over its own route.
func (f *flowFixture) conflicts(t *testing.T, uc *testUserClient, base, source, target string) flowView {
	t.Helper()
	path := fmt.Sprintf("/api/v1/projects/%s/conflicts?base_state_id=%s&source_state_id=%s&target_state_id=%s",
		f.project.ID, base, source, target)
	resp := f.req(t, uc, http.MethodGet, path, "", http.StatusOK)
	return decodeFlow[flowView](t, resp)
}

// putResolutions submits one plan through the resolution route.
func (f *flowFixture) putResolutions(t *testing.T, uc *testUserClient, base, source, target string, decisions []flowDecision) []flowDecision {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"base_state_id": base, "source_state_id": source, "target_state_id": target,
		"resolutions": decisions,
	})
	if err != nil {
		t.Fatalf("encode the decisions: %v", err)
	}
	resp := f.req(t, uc, http.MethodPut, "/api/v1/projects/"+f.project.ID+"/resolutions", string(body), http.StatusOK)
	// The route answers the recorded plan as {"resolutions": [...]} — the
	// decisions the service stored, which is what a caller asserts on.
	return decodeFlow[struct {
		Resolutions []flowDecision `json:"resolutions"`
	}](t, resp).Resolutions
}

// verdictConflicts returns the conflicts the report classified for one
// object, failing when the report carries no verdict for it.
func (f *flowFixture) verdictConflicts(t *testing.T, view flowView, objectID string) []flowConflict {
	t.Helper()
	for _, v := range view.Report.ObjectVerdicts {
		if v.ObjectID != objectID {
			continue
		}
		if len(v.Conflicts) == 0 {
			t.Fatalf("the report's verdict for %s carries no conflict: %+v", objectID, v)
		}
		return v.Conflicts
	}
	t.Fatalf("the report has no verdict for object %s", objectID)
	return nil
}

// conflictWithCode picks the conflict whose stable code is want.
func (f *flowFixture) conflictWithCode(t *testing.T, conflicts []flowConflict, want string) flowConflict {
	t.Helper()
	for _, c := range conflicts {
		if c.Code == want {
			return c
		}
	}
	t.Fatalf("no %s conflict among %+v", want, conflicts)
	return flowConflict{}
}

// versionsOnBranch counts the object's versions that live in states of one
// branch — the read that says whether a merge wrote anything into that
// branch's history.
func (f *flowFixture) versionsOnBranch(t *testing.T, objectID, branchID string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `
		SELECT count(*) FROM scientific_object_versions v
		  JOIN project_states s ON s.id = v.state_id
		 WHERE v.object_id = $1 AND s.branch_id = $2::uuid`, objectID, branchID).Scan(&n); err != nil {
		t.Fatalf("count the versions of %s on branch %s: %v", objectID, branchID, err)
	}
	return n
}

// flowProtocolPayload is mergeProtocolPayload with a caller-chosen purpose,
// so a protocol created on a research branch is not the identity twin of
// the one main already carries (docs/09 §6's identity conflict is about
// same-type-same-title creations, and this fixture must not manufacture
// one by accident).
func flowProtocolPayload(purpose, temperature string) string {
	return `{"purpose":"` + purpose + `","domain":"materials",` +
		`"steps":[{"id":"s1","action":"heat"}],` +
		`"parameters":{"temperature":"` + temperature + `"},` +
		`"requirements":["dry glovebox"]}`
}

// nonNil turns a nil slice into an empty one: the wire carries [] for "no
// fields", and a null there would be a second spelling of the same key.
func nonNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
