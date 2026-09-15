package integration

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/mergegit"
	"github.com/lichman0405/post/cmd/api/mergehttp"
	"github.com/lichman0405/post/cmd/api/policyhttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"

	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/diffs"
	"github.com/lichman0405/post/internal/application/merge"
	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/prchecks"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/application/resolutions"
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

// T0409's required test — label: `merge governance e2e`.
//
// One merge, end to end, over REAL PostgreSQL and a REAL Gitea: a project
// provisioned by the production Provisioner, its branch refs maintained by
// the production branch-ref syncer, a research commit pushed over the git
// protocol, the proposal driven through the real review machine to
// merge_ready, and the merge itself issued as an HTTP request against the
// production route — mergehttp.New -> Register -> the real auth guard, over
// the real merge command with the production provider adapter behind it.
// Nothing on the platform side is a stub — the only test-owned code is the
// assertions.
//
// The merge goes through the ENDPOINT and not around it: the request carries
// the session and CSRF token the real signup endpoint minted, the
// Idempotency-Key header the contract requires, and the assertions below read
// the response the handler produced (git_state, git_sha, state_id,
// plan_digest, replayed). The replay is a second request to the same route.
// Calling merge.Service.Merge directly would leave the route, the guard, the
// project read gate, the contract-required header and the wire mapping
// unexercised, which is the gap this test exists to close.
//
// The in-memory adapters are the session store and the rate limiter
// (memstore); every store behind the guard is the real PostgreSQL one — the
// same exception release_e2e_test.go records, for the same reason (Redis
// session semantics are orthogonal and covered elsewhere).
//
// The four criteria the test names:
//
//	(a) the PR becomes merged — on the platform (pull_requests.state) AND on
//	    the provider (the provider pull request for the ref pair is merged);
//	(b) main's head state IS the accepted state the merge committed, and the
//	    provider's main ref is at the commit the saga recorded — main moved,
//	    and it moved to exactly what the merge accepted;
//	(c) the merge's domain event and its audit row exist (outbox_events /
//	    audit_log), carrying the merge id, the PR and the accepted state;
//	(d) a direct push to main is STILL refused after the merge — by the
//	    provider's own protection, for both the merge service identity (the
//	    one identity allowed to merge) and the instance admin.
//
// Plus the contract this task owns: the same Idempotency-Key replays — the
// same merge comes back, and nothing is written or merged a second time.
//
// What is NOT exercised here, and why it does not weaken the above: the
// provider's main ref and the platform's `project_states.git_commit_sha` are
// independent records in this build. Push ingestion (T0305) is what pins a
// platform state to a provider commit, and it runs on the webhook path; a
// fixture-built state carries no sha, so the merge plans with EMPTY pins and
// the adapter's pin checks are vacuous here (its own unit and integration
// tests cover them, and the bridge's wire-through is pinned in
// cmd/api/mergegit). The merge still names the exact ref pair, and the
// assertion below is that the provider advanced THAT pair — the pair is
// checked, the commits are not.

const mergeGovernanceTaskID = "T0409"

// govPR is the provider's pull-request shape, the subset these assertions
// need (the adapter's own type is unexported and this test reads the
// provider directly, as an outside observer would).
type govPR struct {
	Number   int64  `json:"number"`
	State    string `json:"state"`
	Merged   bool   `json:"merged"`
	MergeSHA string `json:"merge_commit_sha"`
	MergedBy *struct {
		Login string `json:"login"`
	} `json:"merged_by"`
	Head struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
}

func (pr govPR) mergedBy() string {
	if pr.MergedBy == nil {
		return ""
	}
	return pr.MergedBy.Login
}

// govPRs lists the repository's pull requests in one state ("open" or "all").
func govPRs(t *testing.T, base, token, owner, name, state string) []govPR {
	t.Helper()
	code, raw := giteaCall(t, http.MethodGet, base, token,
		"/api/v1/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+"/pulls?state="+state+"&limit=50")
	if code != http.StatusOK {
		t.Fatalf("gitea integration: list pull requests = %d (body %s)", code, raw)
	}
	var prs []govPR
	if err := json.Unmarshal(raw, &prs); err != nil {
		t.Fatalf("gitea integration: decode pull requests: %v", err)
	}
	return prs
}

// govPRFor finds the provider pull request for one (head, base) ref pair.
func govPRFor(t *testing.T, base, token, owner, name, head, baseRef string) (govPR, bool) {
	t.Helper()
	for _, pr := range govPRs(t, base, token, owner, name, "all") {
		if pr.Head.Ref == head && pr.Base.Ref == baseRef {
			return pr, true
		}
	}
	return govPR{}, false
}

// govPRByNumber reads one provider pull request. The LIST endpoint does not
// fill `merged_by` (checked against the running instance), so any assertion
// about WHO merged reads the pull request itself — the same single read the
// production adapter makes before it hands the identity to the ref guard.
func govPRByNumber(t *testing.T, base, token, owner, name string, number int64) govPR {
	t.Helper()
	code, raw := giteaCall(t, http.MethodGet, base, token,
		"/api/v1/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+"/pulls/"+strconv.FormatInt(number, 10))
	if code != http.StatusOK {
		t.Fatalf("gitea integration: read pull request %d = %d (body %s)", number, code, raw)
	}
	var pr govPR
	if err := json.Unmarshal(raw, &pr); err != nil {
		t.Fatalf("gitea integration: decode pull request %d: %v", number, err)
	}
	return pr
}

// mergeE2EPayload is the wire shape of one merge (mergehttp's own
// mergePayload records it): the records the merge produced, named so the
// caller can read them back through the ordinary routes.
type mergeE2EPayload struct {
	Number         int64   `json:"number"`
	MergeID        string  `json:"merge_id"`
	StateID        string  `json:"state_id"`
	PlanDigest     string  `json:"plan_digest"`
	TargetBranchID string  `json:"target_branch_id"`
	GitState       string  `json:"git_state"`
	GitSHA         *string `json:"git_sha"`
	GitError       string  `json:"git_error"`
	Replayed       bool    `json:"replayed"`
}

// mergePath is the contract's merge route: POST
// /projects/{projectId}/pull-requests/{number}:merge (specs/api/openapi.yaml).
func mergePath(projectID string, number int64) string {
	return "/api/v1/projects/" + projectID + "/pull-requests/" + strconv.FormatInt(number, 10) + ":merge"
}

// mergeThroughTheEndpoint issues one merge exactly as the contract defines it —
// a POST to the merge route with the required Idempotency-Key, over the real
// guard (the session's cookie and CSRF token ride along) — and returns the
// handler's own response. The merge service is never called directly by this
// test: the route, the guard, the project read gate, the header contract and
// the wire mapping are what the request exercises.
func mergeThroughTheEndpoint(t *testing.T, uc *testUserClient, projectID string, number int64, key string) mergeE2EPayload {
	t.Helper()
	resp := uc.doKeyed(t, http.MethodPost, mergePath(projectID, number), "", key)
	mustStatus(t, resp, http.StatusOK)
	var p mergeE2EPayload
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("merge payload: %v", err)
	}
	return p
}

// TestMergeGovernanceEndToEnd is T0409's required e2e: label
// `merge governance e2e`.
func TestMergeGovernanceEndToEnd(t *testing.T) {
	ctx := testCtx(t)
	requireGit(t)
	base := requireGitea(t)
	token := giteaServiceToken(t, base)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), mergeGovernanceTaskID)

	// ---- The provider side: the real adapter and the identity the provider's
	// own merge whitelist names. Nothing is created by hand on the provider
	// side — the repository below is the production provisioner's work.
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

	// ---- The platform side: the graph cmd/api builds, over the same real pool
	// and through the same production constructors — including the route
	// assemblies, so the merge request further down is served by the wiring
	// that ships.
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
	statesSvc := states.NewService(stateStore, newCommitGuard(t))
	rsgSvc := rsg.NewService(rsg.Deps{
		Projects:  projectSvc,
		Branches:  branches.NewService(branchStore),
		States:    statesSvc,
		Latest:    stateStore,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
		Events:    events.Recorder{},
	})
	diffSvc := diffs.NewService(stateStore, persistence.NewManifestStore(pool))
	resolutionSvc := resolutions.NewService(diffSvc, resolutions.NewPGStore(pool), projectSvc, authz.NewMatrixEngine())
	// The merge command, wired exactly as cmd/api wires it: the Git port is the
	// real bridge over the real adapter, and the ref guard accepts the identity
	// the provider's own merge whitelist names.
	mergeSvc := merge.NewService(merge.Deps{
		Store:     persistence.NewSemanticMergeStore(pool),
		Diffs:     diffSvc,
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
			Branches: branchStore,
			Manifest: persistence.NewManifestStore(pool),
			Policies: policyStore,
			Engine:   integrity.New(reg),
		}),
		Policies: policyAPI.Service(),
		Rules:    policy.NewRuleEvaluator(),
		Events:   events.Recorder{},
		Git:      mergegit.New(adapter, gitprovider.NewUserAccessStore(pool)),
		RefGuard: gitprovider.RefGuard{MergeService: owner},
	})

	// ---- The HTTP server: the production guard in front of a mux the
	// production wiring populated, in cmd/api/main.go's own order
	// (authAPI.Register, the project routes, mergeAPI.Register, one guard over
	// the whole /api/v1 subtree). Only the session store and the rate limiter
	// are the in-memory implementations — Redis session semantics are
	// orthogonal and covered elsewhere (release_e2e_test.go records the same
	// exception); every store behind the guard, and the provider adapter behind
	// the command, are the production ones.
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
	mergeAPI := mergehttp.New(mergehttp.Deps{Command: mergeSvc, Projects: projectSvc})
	mergeAPI.Register(apiMux)
	ts := httptest.NewServer(authAPI.Guard(apiMux))
	t.Cleanup(ts.Close)

	// ---- The acting user, created through the real signup endpoint: the
	// session cookie and the CSRF token the merge request further down carries
	// are the ones this endpoint minted. No test-side principal injection is
	// possible (or wanted) — the principal the handler reads is the guard's.
	uc, aliceID := signup(t, ts.URL, "merge-gov@example.com", "merge-gov")
	alice := domain.User{ID: aliceID}

	// ---- The project, provisioned for real: repository, bootstrap main,
	// webhook and T0302's protection rule, all applied by the production
	// provisioner.
	org, _, err := orgStore.CreateOrganization(ctx, domain.Organization{
		Slug: "merge-gov", Name: "Merge Governance",
	}, alice.ID, todayUTC())
	if err != nil {
		t.Fatalf("create the fixture organization: %v", err)
	}
	project, _, err := projectStore.CreateProject(ctx, domain.Project{
		OrganizationID:  &org.ID,
		Slug:            "merge-gov",
		Name:            "Merge Governance",
		Purpose:         "T0409 merge governance e2e",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, alice.ID)
	if err != nil {
		t.Fatalf("create the fixture project: %v", err)
	}
	if err := gitprovider.NewProvisioner(adapter, gitprovider.NewProvisionStore(pool), cfg.WebhookURL).
		Provision(ctx, project.ID); err != nil {
		t.Fatalf("gitea integration: provision the project: %v", err)
	}
	repoName := gitprovider.RepositoryName(project.ID)
	t.Cleanup(func() { deleteGiteaRepo(t, base, token, owner, repoName) })

	// The identity the provider's own merge whitelist contains — read from
	// the rule provisioning applied, not assumed: the merge service login the
	// platform's RefGuard must judge the update by is the instance's answer to
	// "who may merge here", so a provider configuration change surfaces as a
	// test failure rather than as a merge the guard silently refuses.
	prot, err := adapter.GetMainProtection(ctx, gitprovider.Repository{Owner: owner, Name: repoName})
	if err != nil {
		t.Fatalf("gitea integration: read main protection: %v", err)
	}
	if !prot.Canonical([]string{owner}) {
		t.Fatalf("gitea integration: main protection = %+v, want the canonical rule for %s", prot, owner)
	}

	mainBranch, err := rsgSvc.CreateBranch(ctx, alice, project.ID, rsg.CreateBranchInput{
		Name: "main", BaseRef: "", Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create the main branch: %v", err)
	}
	mainHeadState, err := statesSvc.GetBranchHead(ctx, mainBranch.ID)
	if err != nil {
		t.Fatalf("read main's head state: %v", err)
	}
	researchBranch, err := rsgSvc.CreateBranch(ctx, alice, project.ID, rsg.CreateBranchInput{
		Name: "governance-r1", BaseRef: mainHeadState.ID, Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create the research branch: %v", err)
	}

	// ---- The provider refs, maintained by the production syncer (T0303): the
	// research branch forks the provider's default branch (no push has been
	// ingested, so no state carries a commit sha yet).
	syncer := gitprovider.NewBranchRefSyncer(adapter, gitprovider.NewBranchRefStore(pool))
	for _, b := range []domain.Branch{mainBranch, researchBranch} {
		if err := syncer.Sync(ctx, b.ID); err != nil {
			t.Fatalf("gitea integration: sync the %s branch ref: %v", b.Name, err)
		}
	}
	if got := mainHead(t, base, token, owner, repoName); got == "" {
		t.Fatal("gitea integration: the provisioned repository has no main ref")
	}

	// ---- A real research commit, pushed over the git protocol. The provider
	// will not open a pull request for a pair with nothing to merge, so the
	// proposal has to carry one; this is the path every research-branch push
	// takes.
	pushResearchBranch(t, base, token, owner, repoName, researchBranch.Name, "governed research line")
	before := mainHead(t, base, token, owner, repoName)

	// ---- The semantic content the merge will accept: one main-gate-complete
	// claim, created through the RSG service on the research branch.
	if _, err := rsgSvc.CreateObject(ctx, alice, project.ID, researchBranch.ID, rsg.CreateObjectInput{
		ObjectType: "claim",
		Payload:    json.RawMessage(mergeMainGateClaim("governed claim")),
	}); err != nil {
		t.Fatalf("create the research claim: %v", err)
	}

	// ---- The proposal, driven through the real review machine to merge_ready.
	// docs/43's later transitions have no HTTP route in this build (T0408 owns
	// the UI), so the service is called directly; the state machine, its
	// preconditions and its rows are the production ones.
	prSvc := pullrequests.NewService(persistence.NewPullRequestStore(pool))
	pr, err := prSvc.Create(ctx, pullrequests.CreatePullRequestParams{
		ProjectID:      project.ID,
		SourceBranchID: researchBranch.ID,
		TargetBranchID: mainBranch.ID,
		Title:          "governed merge",
		CreatedBy:      alice.ID,
	})
	if err != nil {
		t.Fatalf("create the pull request: %v", err)
	}
	if _, err := prSvc.RequestReview(ctx, project.ID, pr.Number); err != nil {
		t.Fatalf("request review: %v", err)
	}
	if _, err := prSvc.SetState(ctx, project.ID, pr.Number, domain.PullRequestStateApproved); err != nil {
		t.Fatalf("approve the pull request: %v", err)
	}
	if _, err := prSvc.SetState(ctx, project.ID, pr.Number, domain.PullRequestStateMergeReady); err != nil {
		t.Fatalf("mark the pull request merge_ready: %v", err)
	}

	// ---- The governance policy in force: main_protected true, on the
	// project's own scope, written through the real store.
	seedProjectPolicy(t, ctx, policyStore, project, alice.ID)

	// ---- The merge: one HTTP request against the production route, carrying
	// the contract's required Idempotency-Key and the session the signup
	// endpoint minted. The service is never called directly — the route, the
	// guard, the project read gate, the header contract and the wire mapping
	// are all part of what this request exercises.
	key := "merge-governance-e2e-00000001"
	mergeURL := mergePath(project.ID, pr.Number)

	// The guard refuses an anonymous write before routing: the merge route is
	// exactly as reachable as the session, and a caller without one never
	// reaches the command.
	anonResp := newTestUserClient(ts.URL).do(t, http.MethodPost, mergeURL, "")
	mustStatus(t, anonResp, http.StatusUnauthorized)
	mustEnvelope(t, anonResp, "AUTH_UNAUTHENTICATED")

	// The key is required on this route (specs/api/openapi.yaml,
	// components.parameters.IdempotencyKey): a session without one is refused
	// by the handler, before the command — a request that could not be replayed
	// onto its own merge does not advance main either.
	unkeyedResp := uc.do(t, http.MethodPost, mergeURL, "")
	mustStatus(t, unkeyedResp, http.StatusBadRequest)
	mustEnvelope(t, unkeyedResp, merge.CodeValidation)

	// The merge itself.
	merged := mergeThroughTheEndpoint(t, uc, project.ID, pr.Number, key)
	if merged.Replayed {
		t.Fatal("the first merge reports itself as a replay — the ledger answered for a key that was never used")
	}
	if merged.Number != pr.Number {
		t.Errorf("the merge answered for pull request %d, the request named %d", merged.Number, pr.Number)
	}

	// ---- The Git half really ran: the response reports the saga's step as an
	// update, not as pending or failed. Without this the assertions below would
	// be satisfied by a merge that never touched the provider.
	if merged.GitState != string(domain.GitStateUpdated) {
		t.Fatalf("merge git state = %q (error %q), want %q — the provider merge did not land",
			merged.GitState, merged.GitError, domain.GitStateUpdated)
	}
	if merged.GitSHA == nil || *merged.GitSHA == "" {
		t.Fatal("the merge answered without a commit sha: an updated Git step must name the commit it produced")
	}
	mergedSHA := *merged.GitSHA

	// (a) The proposal is merged on the platform...
	mergedPR, err := prSvc.Get(ctx, project.ID, pr.Number)
	if err != nil {
		t.Fatalf("read the merged pull request: %v", err)
	}
	if mergedPR.State != domain.PullRequestStateMerged {
		t.Errorf("(a) pull request state = %q, want %q", mergedPR.State, domain.PullRequestStateMerged)
	}
	if mergedPR.MergedAt == nil {
		t.Error("(a) the merged pull request has no merged_at — the transition was recorded without it")
	}

	// ...and on the provider, whose own pull request for the ref pair is
	// merged, by the controlled merge service, at the commit the saga read.
	providerPR, found := govPRFor(t, base, token, owner, repoName, researchBranch.Name, "main")
	if !found {
		t.Fatalf("(a) the provider has no pull request for %s -> main: the merge path never opened one", researchBranch.Name)
	}
	if !providerPR.Merged || providerPR.State != "closed" {
		t.Errorf("(a) provider pull request %d = state %q merged %v, want a merged pull request",
			providerPR.Number, providerPR.State, providerPR.Merged)
	}
	if providerPR.MergeSHA != mergedSHA {
		t.Errorf("(a) provider merge commit = %q, the saga recorded %q — the platform is not recording the provider's own commit",
			providerPR.MergeSHA, mergedSHA)
	}
	mergedBy := govPRByNumber(t, base, token, owner, repoName, providerPR.Number).mergedBy()
	if mergedBy != owner {
		t.Errorf("(a) provider pull request merged by %q, want the controlled merge identity %q — the ref guard judges this identity",
			mergedBy, owner)
	}

	// (b) main's head state IS the accepted state the merge answered with, and
	// the provider's main ref is at the commit the merge recorded. The state
	// row is read back from the store — the wire names it, the platform's own
	// truth is what has to agree.
	if merged.StateID == "" {
		t.Fatal("(b) the merge answered without a state id — nothing names what main advanced to")
	}
	headAfter, err := statesSvc.GetBranchHead(ctx, mainBranch.ID)
	if err != nil {
		t.Fatalf("(b) read main's head state after the merge: %v", err)
	}
	if headAfter.ID != merged.StateID {
		t.Errorf("(b) main's head state = %s, the merge accepted %s — main advanced by something other than this merge", headAfter.ID, merged.StateID)
	}
	accepted, err := statesSvc.GetState(ctx, merged.StateID)
	if err != nil {
		t.Fatalf("(b) read the accepted state %s: %v", merged.StateID, err)
	}
	if accepted.BranchID == nil || *accepted.BranchID != mainBranch.ID {
		t.Errorf("(b) the accepted state belongs to branch %v, want main %s", accepted.BranchID, mainBranch.ID)
	}
	if merged.TargetBranchID != mainBranch.ID {
		t.Errorf("(b) the merge answered with target branch %q, want main %s", merged.TargetBranchID, mainBranch.ID)
	}
	after := mainHead(t, base, token, owner, repoName)
	if after != mergedSHA {
		t.Errorf("(b) the provider's main ref is at %s, the merge recorded %s", after, mergedSHA)
	}
	if after == before {
		t.Errorf("(b) the provider's main ref did not move (%s)", after)
	}
	if readme := mainREADME(t, base, token, owner, repoName); !strings.Contains(readme, "governed research line") {
		t.Errorf("(b) main does not carry the research commit's content: %q", readme)
	}

	// (c) The domain event and the audit row. The event is the outbox row the
	// merge wrote in its own transaction; it locates the merge without
	// re-deriving anything (project, PR id and number, accepted state).
	var eventPayload []byte
	if err := pool.QueryRow(ctx,
		`SELECT payload FROM outbox_events WHERE project_id = $1 AND event_type = $2`,
		project.ID, "pull_request.merged").Scan(&eventPayload); err != nil {
		t.Fatalf("(c) read the merge's domain event: %v", err)
	}
	var event struct {
		MergeID           string `json:"merge_id"`
		PullRequestID     string `json:"pull_request_id"`
		PullRequestNumber int64  `json:"pull_request_number"`
		StateID           string `json:"state_id"`
		TargetBranchID    string `json:"target_branch_id"`
	}
	if err := json.Unmarshal(eventPayload, &event); err != nil {
		t.Fatalf("(c) decode the merge's domain event payload (%s): %v", eventPayload, err)
	}
	if event.MergeID != merged.MergeID || event.PullRequestID != pr.ID ||
		event.PullRequestNumber != pr.Number || event.StateID != merged.StateID {
		t.Errorf("(c) the event payload = %+v, want merge %s / PR %s (%d) / state %s",
			event, merged.MergeID, pr.ID, pr.Number, merged.StateID)
	}
	if event.TargetBranchID != mainBranch.ID {
		t.Errorf("(c) the event names target branch %s, want main %s", event.TargetBranchID, mainBranch.ID)
	}

	var auditSummary []byte
	if err := pool.QueryRow(ctx,
		`SELECT after_summary FROM audit_log WHERE project_id = $1 AND action = $2`,
		project.ID, domain.ActionPullRequestMerged).Scan(&auditSummary); err != nil {
		t.Fatalf("(c) read the merge's audit row: %v", err)
	}
	var audit struct {
		PullRequestID string `json:"pull_request_id"`
		StateID       string `json:"state_id"`
		PlanDigest    string `json:"plan_digest"`
		TargetBranch  string `json:"target_branch_id"`
	}
	if err := json.Unmarshal(auditSummary, &audit); err != nil {
		t.Fatalf("(c) decode the audit summary (%s): %v", auditSummary, err)
	}
	if audit.PullRequestID != pr.ID || audit.StateID != merged.StateID || audit.TargetBranch != mainBranch.ID {
		t.Errorf("(c) the audit summary = %+v, want PR %s / state %s / branch %s", audit, pr.ID, merged.StateID, mainBranch.ID)
	}
	if audit.PlanDigest != merged.PlanDigest {
		t.Errorf("(c) the audit summary records plan digest %q, the merge answered %q", audit.PlanDigest, merged.PlanDigest)
	}

	// (d) A direct push to main is STILL refused after the merge, by the
	// provider's own protection — for the merge service identity (the one
	// identity the whitelist allows to MERGE) and for the instance admin. The
	// probe branches from the post-merge main, so the push is a fast-forward
	// and the refusal has to come from protection rather than from a
	// non-fast-forward.
	svcAuth := "Authorization: token " + token
	adminUser, adminPass := giteaAdmin()
	adminAuth := "Authorization: Basic " + base64Authorization(adminUser, adminPass)
	probe := researchDir(t, base, token, owner, repoName, "governance-probe", "should not land")
	expectMainRefusal(t, base, token, owner, repoName, after, probe, svcAuth, "HEAD:main")
	expectMainRefusal(t, base, token, owner, repoName, after, probe, adminAuth, "HEAD:main")

	// ---- The Idempotency-Key contract: the same key replays the same merge —
	// the same request again, over the same route — and writes nothing, merges
	// nothing, and moves nothing.
	replay := mergeThroughTheEndpoint(t, uc, project.ID, pr.Number, key)
	if !replay.Replayed {
		t.Error("the repeated Idempotency-Key did not report a replay")
	}
	if replay.MergeID != merged.MergeID || replay.StateID != merged.StateID {
		t.Errorf("the replay answered with merge %s/state %s, want the first call's %s/%s",
			replay.MergeID, replay.StateID, merged.MergeID, merged.StateID)
	}
	if replay.GitSHA == nil || *replay.GitSHA != mergedSHA {
		t.Errorf("the replay answered with git sha %v, the first call merged %s", replay.GitSHA, mergedSHA)
	}

	var merges, creations, audits, outbox int
	counts := []struct {
		query string
		args  []any
		into  *int
		what  string
	}{
		{`SELECT count(*) FROM semantic_merges WHERE pull_request_id = $1`, []any{pr.ID}, &merges, "merge rows"},
		{`SELECT count(*) FROM merge_creations WHERE project_id = $1 AND idempotency_key = $2`, []any{project.ID, key}, &creations, "ledger entries"},
		{`SELECT count(*) FROM audit_log WHERE project_id = $1 AND action = $2`, []any{project.ID, domain.ActionPullRequestMerged}, &audits, "audit rows"},
		{`SELECT count(*) FROM outbox_events WHERE project_id = $1 AND event_type = $2`, []any{project.ID, "pull_request.merged"}, &outbox, "domain events"},
	}
	for _, c := range counts {
		if err := pool.QueryRow(ctx, c.query, c.args...).Scan(c.into); err != nil {
			t.Fatalf("count %s: %v", c.what, err)
		}
	}
	if merges != 1 || creations != 1 || audits != 1 || outbox != 1 {
		t.Errorf("after the replay: merges=%d ledger=%d audits=%d events=%d, want one of each — a replay is a read",
			merges, creations, audits, outbox)
	}
	replayedHead, err := statesSvc.GetBranchHead(ctx, mainBranch.ID)
	if err != nil {
		t.Fatalf("read main's head state after the replay: %v", err)
	}
	if replayedHead.ID != merged.StateID {
		t.Errorf("the replay moved main to %s, the first request accepted %s", replayedHead.ID, merged.StateID)
	}
	if got := mainHead(t, base, token, owner, repoName); got != after {
		t.Errorf("the replay moved the provider's main ref %s -> %s", after, got)
	}
	if replayed := govPRs(t, base, token, owner, repoName, "all"); len(replayed) != 1 {
		t.Errorf("the provider holds %d pull requests for the pair after the replay, want the one the first call merged", len(replayed))
	}
}

// base64Authorization renders `user:pass` for a Basic Authorization header.
func base64Authorization(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}
