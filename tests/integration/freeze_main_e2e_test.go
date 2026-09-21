package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/freezehttp"
	"github.com/lichman0405/post/cmd/api/mergegit"
	"github.com/lichman0405/post/cmd/api/mergehttp"
	"github.com/lichman0405/post/cmd/api/policyhttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/cmd/api/rsghttp"

	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/diffs"
	"github.com/lichman0405/post/internal/application/mainfreeze"
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

// T0601's required test — label: `freeze e2e`.
//
// Freeze main governance, end to end, over REAL PostgreSQL and a REAL Gitea:
// the production freeze route (freezehttp.New -> Register -> the real auth
// guard -> the real command -> the real store), the production RSG write
// route, the production merge route and the production push-webhook receiver
// all mounted on one guarded mux, provisioned by the production provisioner.
// Nothing platform-side is a stub; the only test-owned code is the assertions
// and the membership seed.
//
// The task's two halves are one rule, and the test is arranged so that neither
// half can pass on its own:
//
//	the WRITE  — POST /projects/{projectId}/main:freeze (specs/api/openapi.yaml
//	             :43-50, the literal path) freezes the project, writing the
//	             flag, one audit row and one project.main_frozen event in ONE
//	             transaction;
//	the REFUSAL — once frozen, every OTHER way of moving main is refused with
//	             MAIN_FROZEN_DIRECT_WRITE_FORBIDDEN (docs/45): a direct
//	             semantic write over the RSG route, and a Git-side direct push
//	             to refs/heads/main arriving as a webhook delivery — refused
//	             BEFORE the provider is read or written, never "push first,
//	             report afterwards".
//
// The same test also holds the two directions apart, which is what makes the
// refusal a governance rule rather than a wall:
//
//	- a Research PR merge onto the frozen main SUCCEEDS (docs/09 §3: frozen
//	  main may only be advanced by a PR merge, even for the owner) — so a
//	  frozen project can still evolve;
//	- while frozen, research-branch writes are untouched.
//
// And the boundary cases the acceptance criteria name: an unfrozen project's
// direct main writes are not blocked; only owner/maintainer may freeze (viewer,
// contributor, non-member and anonymous refused, an agent refused on BOTH
// lines of defence); a repeated freeze — same key and a different key — writes
// nothing a second time; two concurrent freezes have exactly one winner; an
// unknown project id answers a permission-class refusal rather than 404; and
// there is no :unfreeze path at all.
//
// The one thing this test does NOT do is exercise the freeze over a real
// subprocess of cmd/api: the graph below is assembled from the same
// constructors cmd/api/main.go calls, in the same order, over the same real
// pool — the exception release_e2e_test.go and merge_governance_e2e_test.go
// record for the session store and the rate limiter (memstore) applies here
// too, for the same reason.

const freezeMainTaskID = "T0601"

// freezeMainPath is the contract's literal path (specs/api/openapi.yaml:43-50,
// "Freeze main; governance action"). It is written out here rather than
// derived, so the test fails if the surface is ever mounted somewhere else.
func freezeMainPath(projectID string) string {
	return "/api/v1/projects/" + projectID + "/main:freeze"
}

// branchObjectsPath is the RSG write surface: the direct semantic write a
// frozen main must refuse (and, before the freeze, must NOT refuse).
func branchObjectsPath(projectID, branchID string) string {
	return "/api/v1/projects/" + projectID + "/branches/" + branchID + "/objects"
}

// freezeWire is the freeze route's response (freezehttp's own payload).
type freezeWire struct {
	ProjectID     string `json:"project_id"`
	MainFrozen    bool   `json:"main_frozen"`
	AlreadyFrozen bool   `json:"already_frozen"`
}

// freezeOnce issues one freeze request and returns its status, its raw body
// and the decoded payload. It calls no testing helper on the way out — the
// concurrent case below drives it from goroutines, where only the collecting
// goroutine may fail the test — so the error is returned instead.
func freezeOnce(client *http.Client, server, projectID, csrf, key string) (status int, body string, out freezeWire, err error) {
	req, err := http.NewRequest(http.MethodPost, server+freezeMainPath(projectID), nil)
	if err != nil {
		return 0, "", freezeWire{}, err
	}
	req.Header.Set("X-CSRF-Token", csrf)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", freezeWire{}, err
	}
	defer resp.Body.Close()
	raw := make([]byte, 0, 256)
	buf := make([]byte, 256)
	for {
		n, rerr := resp.Body.Read(buf)
		raw = append(raw, buf[:n]...)
		if rerr != nil {
			break
		}
	}
	body = string(raw)
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &out); err != nil {
			return resp.StatusCode, body, freezeWire{}, fmt.Errorf("decode freeze payload: %w (%s)", err, body)
		}
	}
	return resp.StatusCode, body, out, nil
}

// freezeThroughTheEndpoint issues one freeze exactly as the contract defines
// it (POST to the literal path, session cookie + CSRF token, the required
// Idempotency-Key) and asserts the 200.
func freezeThroughTheEndpoint(t *testing.T, uc *testUserClient, projectID, key string) freezeWire {
	t.Helper()
	status, body, out, err := freezeOnce(uc.client, uc.server, projectID, uc.csrf, key)
	if err != nil {
		t.Fatalf("freeze %s: %v", projectID, err)
	}
	if status != http.StatusOK {
		t.Fatalf("freeze %s = %d: %s", projectID, status, body)
	}
	return out
}

// freezeRows counts the freeze's own canonical records for one project: the
// audit rows it appends and the domain events it records.
func freezeRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID string) (audits, outbox int) {
	t.Helper()
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE project_id = $1 AND action = $2`,
		projectID, domain.ActionProjectMainFrozen).Scan(&audits); err != nil {
		t.Fatalf("count the freeze audit rows: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox_events WHERE project_id = $1 AND event_type = $2`,
		projectID, "project.main_frozen").Scan(&outbox); err != nil {
		t.Fatalf("count the project.main_frozen events: %v", err)
	}
	return audits, outbox
}

// freezeRow reads the ONE audit row a freeze appended, as the archive holds
// it.
type freezeRow struct {
	actorID       string
	via           string
	targetRef     string
	correlationID string
	before        []byte
	after         []byte
	metadata      []byte
}

func readFreezeAudit(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID string) freezeRow {
	t.Helper()
	var r freezeRow
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(actor_id::text, ''), via, COALESCE(target_ref, ''), correlation_id,
		        before_summary, after_summary, metadata
		 FROM audit_log WHERE project_id = $1 AND action = $2`,
		projectID, domain.ActionProjectMainFrozen).
		Scan(&r.actorID, &r.via, &r.targetRef, &r.correlationID, &r.before, &r.after, &r.metadata); err != nil {
		t.Fatalf("read the freeze audit row: %v", err)
	}
	return r
}

// stateCount counts the canonical states a project holds, and those on one
// branch: the "nothing was written" evidence a refusal has to leave behind.
func stateCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID, branchID string) (all, onBranch int) {
	t.Helper()
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM project_states WHERE project_id = $1`, projectID).Scan(&all); err != nil {
		t.Fatalf("count the project's states: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM project_states WHERE project_id = $1 AND branch_id = $2`,
		projectID, branchID).Scan(&onBranch); err != nil {
		t.Fatalf("count the branch's states: %v", err)
	}
	return all, onBranch
}

// freezeMainPost performs the freeze over the wire without asserting the
// outcome (the refusals are as interesting as the success).
func freezeMainPost(t *testing.T, uc *testUserClient, projectID, key string) (int, string) {
	t.Helper()
	resp := uc.doKeyed(t, http.MethodPost, freezeMainPath(projectID), "", key)
	return resp.StatusCode, readAll(t, resp)
}

// wirePost sends one JSON write from a session and returns its status and
// body without asserting either.
func wirePost(t *testing.T, uc *testUserClient, path, body string) (int, string) {
	t.Helper()
	resp := uc.do(t, http.MethodPost, path, body)
	return resp.StatusCode, readAll(t, resp)
}

// mainFrozenOf reads the flag the whole task is about, straight from the
// canonical row.
func mainFrozenOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID string) bool {
	t.Helper()
	var frozen bool
	if err := pool.QueryRow(ctx, `SELECT main_frozen FROM projects WHERE id = $1`, projectID).Scan(&frozen); err != nil {
		t.Fatalf("read projects.main_frozen: %v", err)
	}
	return frozen
}

// countIngestions counts the canonical ingestion rows: the evidence that a
// refused delivery wrote nothing at all.
func countIngestions(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM git_push_ingestions`).Scan(&n); err != nil {
		t.Fatalf("count the push ingestions: %v", err)
	}
	return n
}

// recordedMainHead reads the branch ref pointer the ingestion advances and
// guards (git_branch_refs.head_sha): main's head as the PLATFORM has it.
func recordedMainHead(t *testing.T, ctx context.Context, pool *pgxpool.Pool, branchID string) string {
	t.Helper()
	var head string
	if err := pool.QueryRow(ctx,
		`SELECT coalesce(head_sha, '') FROM git_branch_refs WHERE branch_id = $1`, branchID).Scan(&head); err != nil {
		t.Fatalf("read the branch ref's recorded head: %v", err)
	}
	return head
}

// semanticMergeGit reads the Git half of one recorded merge exactly as
// stored. The seam's predicate is these three fields — the ref spelling, the
// saga state and the sha — so a test that only asserts the DELIVERY would
// leave a silent mismatch in any of them looking like a working rule.
func semanticMergeGit(t *testing.T, ctx context.Context, pool *pgxpool.Pool, mergeID string) (ref, sha, state string) {
	t.Helper()
	var r, s, st *string
	if err := pool.QueryRow(ctx,
		`SELECT git_ref, git_sha, git_state FROM semantic_merges WHERE id = $1`, mergeID).Scan(&r, &s, &st); err != nil {
		t.Fatalf("read the recorded merge %s: %v", mergeID, err)
	}
	return textOrEmpty(r), textOrEmpty(s), textOrEmpty(st)
}

func textOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// mainDelivery is the canonical row one accepted main delivery left, as
// stored.
type mainDelivery struct {
	gitRef     string
	before     string
	branchName string
	headSkip   *string
}

// readMainDeliveries reads the ingestion rows one pushed main head wrote and
// how many there are. The count is the evidence that the seam's admitted
// branch really executed against the database — a predicate that silently
// matched nothing (a misspelled git_ref in the record, a broken project
// join, a git_state that never reached 'updated') would still answer the
// webhook with a status, and would still refuse the merge, while leaving
// this table empty.
func readMainDeliveries(t *testing.T, ctx context.Context, pool *pgxpool.Pool, after string) (mainDelivery, int) {
	t.Helper()
	var d mainDelivery
	rows, err := pool.Query(ctx,
		`SELECT git_ref, before_sha, coalesce(branch_name, ''), head_skip_reason
		   FROM git_push_ingestions
		  WHERE git_ref = 'refs/heads/main' AND after_sha = $1`, after)
	if err != nil {
		t.Fatalf("read the main deliveries for %s: %v", after, err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		if err := rows.Scan(&d.gitRef, &d.before, &d.branchName, &d.headSkip); err != nil {
			t.Fatalf("scan a main delivery: %v", err)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the main deliveries for %s: %v", after, err)
	}
	return d, n
}

// countPushedHeadStates counts the pushed-head project states one commit
// produced: the semantic-completion evidence T0306 builds on, which the
// platform's own merge has to keep producing while main is frozen.
func countPushedHeadStates(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID, sha string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM project_states WHERE project_id = $1 AND git_commit_sha = $2`, projectID, sha).Scan(&n); err != nil {
		t.Fatalf("count the pushed-head states for %s: %v", sha, err)
	}
	return n
}

// ownerRole is the matrix column the agent case is asked about: an OWNER with
// the agent flag must still be denied, or the agent denial would be an
// accident of the actor having no role.
var ownerRole = domain.ProjectRoleOwner

// TestFreezeMainGovernanceEndToEnd is T0601's required e2e: label
// `freeze e2e`.
func TestFreezeMainGovernanceEndToEnd(t *testing.T) {
	ctx := testCtx(t)
	requireGit(t)
	base := requireGitea(t)
	token := giteaServiceToken(t, base)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), freezeMainTaskID)

	cfg := gitprovider.Config{
		BaseURL:    base,
		Token:      config.Secret(token),
		WebhookURL: "http://host.invalid/api/v1/git/hooks/gitea",
	}
	adapter := gitprovider.NewGiteaAdapter(cfg)
	mergeIdentity, err := adapter.Owner(ctx)
	if err != nil {
		t.Fatalf("gitea integration: resolve the service identity: %v", err)
	}

	// ---- The platform graph: the same constructors cmd/api/main.go calls,
	// over the same real pool.
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
	diffSvc := diffs.NewService(stateStore, persistence.NewManifestStore(pool), persistence.NewPullRequestStore(pool))
	resolutionSvc := resolutions.NewService(diffSvc, resolutions.NewPGStore(pool), projectSvc, authz.NewMatrixEngine())
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
		RefGuard: gitprovider.RefGuard{MergeService: mergeIdentity},
	})
	// The freeze command, wired as cmd/api wires it: the raw project store for
	// membership (it answers "not a member" for a stranger AND for a project
	// that does not exist, which is what the existence-hiding refusal is built
	// on), the policy service for the policy in force, the typed rule surface,
	// the freeze store and the matrix engine.
	freezeSvc := mainfreeze.NewCommand(mainfreeze.Deps{
		Members:  projectStore,
		Policies: policyAPI.Service(),
		Rules:    policy.NewRuleEvaluator(),
		Store:    persistence.NewMainFreezeStore(pool),
		Authz:    authz.NewMatrixEngine(),
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
	mergehttp.New(mergehttp.Deps{Command: mergeSvc, Projects: projectSvc}).Register(apiMux)
	freezehttp.New(freezehttp.Deps{Command: freezeSvc}).Register(apiMux)
	ts := httptest.NewServer(authAPI.Guard(apiMux))
	t.Cleanup(ts.Close)

	// ---- The acting users, all created through the real signup endpoint: the
	// cookies and CSRF tokens below are what that endpoint minted.
	alice, aliceID := signup(t, ts.URL, "freeze-gov@example.com", "freeze-gov")
	bob, bobID := signup(t, ts.URL, "freeze-viewer@example.com", "freeze-viewer")
	carol, carolID := signup(t, ts.URL, "freeze-contrib@example.com", "freeze-contrib")
	stranger, _ := signup(t, ts.URL, "freeze-stranger@example.com", "freeze-stranger")
	aliceUser := domain.User{ID: aliceID}

	// ---- The project, provisioned for real, and the memberships the matrix
	// columns are read from. The member-management API is T0109; T0105's own
	// authorization test seeds canonical membership rows the same way.
	org, _, err := orgStore.CreateOrganization(ctx, domain.Organization{
		Slug: "freeze-gov", Name: "Freeze Governance",
	}, aliceID, todayUTC())
	if err != nil {
		t.Fatalf("create the fixture organization: %v", err)
	}
	project, _, err := projectStore.CreateProject(ctx, domain.Project{
		OrganizationID:  &org.ID,
		Slug:            "freeze-gov",
		Name:            "Freeze Governance",
		Purpose:         "T0601 freeze main governance e2e",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, aliceID)
	if err != nil {
		t.Fatalf("create the fixture project: %v", err)
	}
	for _, seed := range []struct{ userID, role string }{{bobID, "viewer"}, {carolID, "contributor"}} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, $3)`,
			project.ID, seed.userID, seed.role); err != nil {
			t.Fatalf("seed the %s membership: %v", seed.role, err)
		}
	}

	// ---- The provider side: the production provisioner creates the
	// repository, the bootstrap main commit, the webhook and T0302's protection
	// rule. The receiver below is the production one.
	pushes := &pushIngestionGiteaFixture{
		branchRefGiteaFixture: &branchRefGiteaFixture{
			pool:     pool,
			base:     base,
			token:    token,
			user:     aliceUser,
			project:  project,
			branches: branches.NewService(branchStore),
			cfg:      cfg,
		},
		reg: reg,
	}
	owner, repoName, repoID, webhookSecret := pushes.provision(t, ctx)

	// ---- main exists, with the genesis state the platform's own branch
	// creation produces.
	mainBranch, err := rsgSvc.CreateBranch(ctx, aliceUser, project.ID, rsg.CreateBranchInput{
		Name: "main", BaseRef: "", Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create the main branch: %v", err)
	}
	if err := pushes.syncer().Sync(ctx, mainBranch.ID); err != nil {
		t.Fatalf("gitea integration: sync main's ref: %v", err)
	}

	// ---- (4) An UNFROZEN project's direct write to main is NOT blocked. This
	// is also the reading the task requires literally: a new project is
	// unfrozen, its setup writes main directly, and freezing is an explicit
	// action taken afterwards. Without this step the freeze below could be
	// "passing" by refusing everything.
	status, body := wirePost(t, alice,
		branchObjectsPath(project.ID, mainBranch.ID),
		fmt.Sprintf(`{"object_type":"claim","payload":%s}`, mergeMainGateClaim("a setup-time claim written straight into main")))
	if status != http.StatusCreated {
		t.Fatalf("(4) an unfrozen project refused a direct main write = %d: %s", status, body)
	}
	mainHeadBeforeFreeze, err := statesSvc.GetBranchHead(ctx, mainBranch.ID)
	if err != nil {
		t.Fatalf("(4) read main's head state: %v", err)
	}

	// ---- The research branch forks the CURRENT main head (after that direct
	// write), and carries the commit the provider needs to open a PR for.
	researchBranch, err := rsgSvc.CreateBranch(ctx, aliceUser, project.ID, rsg.CreateBranchInput{
		Name: "freeze-r1", BaseRef: mainHeadBeforeFreeze.ID, Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create the research branch: %v", err)
	}
	if err := pushes.syncer().Sync(ctx, researchBranch.ID); err != nil {
		t.Fatalf("gitea integration: sync the research branch ref: %v", err)
	}
	pushResearchBranch(t, base, token, owner, repoName, researchBranch.Name, "frozen-main research line")

	// ---- The Git-side control, BEFORE the freeze: a delivery for
	// refs/heads/main whose shas cannot be resolved by the provider. The
	// ingester reads the provider before it records anything, so an unfrozen
	// project answers 503 (the fetch failed). After the freeze the same
	// delivery answers 403 — which is only meaningful because this control
	// shows the payload itself is not what produces it.
	ghostBefore := "1111111111111111111111111111111111111111"
	ghostAfter := "2222222222222222222222222222222222222222"
	if code := pushes.deliver(t, repoID, owner, repoName, "refs/heads/main", ghostBefore, ghostAfter, 1, nil,
		webhookSecret, "freeze-main-unfrozen-1"); code != http.StatusServiceUnavailable {
		t.Fatalf("the unfrozen control delivery = %d, want 503 (the provider read is what fails before the freeze)", code)
	}

	// ---- (5) Only owner and maintainer may freeze. Over the wire: a viewer,
	// a contributor, a non-member and an anonymous caller are all refused, and
	// the refusals are permission-class.
	for _, tc := range []struct {
		name   string
		client *testUserClient
		want   int
	}{
		{"viewer", bob, http.StatusForbidden},
		{"contributor", carol, http.StatusForbidden},
		{"non-member", stranger, http.StatusForbidden},
		{"anonymous", newTestUserClient(ts.URL), http.StatusUnauthorized},
	} {
		t.Run("freeze refused for the "+tc.name, func(t *testing.T) {
			status, body := freezeMainPost(t, tc.client, project.ID, "freeze-denied-key-0001")
			if status != tc.want {
				t.Fatalf("(5) %s freeze = %d: %s", tc.name, status, body)
			}
			if tc.want == http.StatusForbidden {
				var env errorEnvelope
				if err := json.Unmarshal([]byte(body), &env); err != nil {
					t.Fatalf("(5) decode the refusal: %v (%s)", err, body)
				}
				if env.Code != mainfreeze.CodeForbidden {
					t.Fatalf("(5) %s refusal code = %q, want %q", tc.name, env.Code, mainfreeze.CodeForbidden)
				}
			}
		})
	}
	if audits, outbox := freezeRows(t, ctx, pool, project.ID); audits != 0 || outbox != 0 {
		t.Fatalf("(5) a refused freeze wrote %d audit rows and %d events, want none", audits, outbox)
	}

	// ---- (5b) The agent refusal, on both lines of defence.
	//
	// The SECOND line first: the domain backstop refuses an agent before any
	// lookup, whatever the matrix would say. The actor here is the project's
	// OWNER — an actor the matrix admits — so the refusal cannot be the
	// matrix's doing.
	agentActor := mainfreeze.Actor{User: aliceUser, IsAgent: true}
	if _, err := freezeSvc.Freeze(ctx, agentActor, mainfreeze.Input{
		ProjectID: project.ID, IdempotencyKey: "freeze-agent-key-0001",
	}); err == nil {
		t.Fatal("(5) an agent froze main — the domain backstop did not fire")
	} else {
		var refused *mainfreeze.AgentNotPermittedError
		if !errors.As(err, &refused) {
			t.Fatalf("(5) the agent refusal is %v, want *mainfreeze.AgentNotPermittedError", err)
		}
		if refused.Code() != mainfreeze.CodeAgentFreezeDenied {
			t.Fatalf("(5) the agent refusal code = %q, want %q", refused.Code(), mainfreeze.CodeAgentFreezeDenied)
		}
	}
	// The FIRST line has to deny the same actor independently — otherwise the
	// backstop would be the only thing standing between an agent and a freeze,
	// and the acceptance criterion "agent: deny" would rest on one mechanism.
	// The matrix's own answer, asked with the owner's role and the agent flag:
	decision, err := authz.NewMatrixEngine().Authorize(ctx, authz.Request{
		Action: authz.ActionFreezeMain,
		Class:  authz.ClassOf(true, &ownerRole, true),
	})
	if err != nil {
		t.Fatalf("(5) authorize freeze_main for an agent: %v", err)
	}
	if decision.Permits() {
		t.Fatal("(5) the permission matrix permits freeze_main for an agent — the first line of defence is gone")
	}
	if audits, outbox := freezeRows(t, ctx, pool, project.ID); audits != 0 || outbox != 0 {
		t.Fatalf("(5) a refused agent wrote %d audit rows and %d events, want none", audits, outbox)
	}

	// ---- (6) The freeze transaction is atomic. A freeze whose LAST step
	// fails must leave the project exactly as it was: the audit row is
	// appended after the flag update and before the event, so an audit insert
	// that cannot succeed (an actor id with no users row — audit_log.actor_id
	// is a foreign key) is a deterministic late failure. The flag must be
	// rolled back with it, and no event may exist. This is the same store the
	// route uses, so what it proves holds for the route.
	second, _, err := projectStore.CreateProject(ctx, domain.Project{
		Slug: "freeze-gov-2", Name: "Freeze Governance Two", Purpose: "T0601 concurrency fixture",
		Visibility: domain.VisibilityPrivate, ProvisionStatus: domain.ProvisionPending,
	}, aliceID)
	if err != nil {
		t.Fatalf("create the second fixture project: %v", err)
	}
	ghostActor := "00000000-0000-4000-8000-0000000000ff"
	_, ferr := persistence.NewMainFreezeStore(pool).Freeze(ctx, mainfreeze.FreezeRequest{
		ProjectID:      second.ID,
		ActorID:        ghostActor,
		Audit:          domain.AuditEntry{ActorID: ghostActor, Action: domain.ActionProjectMainFrozen, TargetRef: "project:" + second.ID, ProjectID: second.ID},
		IdempotencyKey: "freeze-rollback-key-0001",
	})
	if ferr == nil {
		t.Fatal("(6) a freeze whose audit row could not be written reported success")
	}
	if got := mainFrozenOf(t, ctx, pool, second.ID); got {
		t.Fatal("(6) the failed freeze left the flag SET: the flag update was not rolled back with the audit row")
	}
	if audits, outbox := freezeRows(t, ctx, pool, second.ID); audits != 0 || outbox != 0 {
		t.Fatalf("(6) the failed freeze left %d audit rows and %d events, want none", audits, outbox)
	}

	// ---- The governance policy in force, written through the real store.
	seedProjectPolicy(t, ctx, policyStore, project, aliceID)

	if status, body := freezeMainPost(t, bob, project.ID, "freeze-viewer-key-0002"); status != http.StatusForbidden {
		t.Fatalf("(5) a viewer froze main under the project's own policy = %d: %s", status, body)
	}

	// ---- (6) + (8) The freeze itself, as the owner, and its exact records.
	key := "freeze-governance-e2e-0001"
	frozen := freezeThroughTheEndpoint(t, alice, project.ID, key)
	if !frozen.MainFrozen || frozen.AlreadyFrozen {
		t.Fatalf("(6) the freeze answered %+v, want a first freeze that reports main frozen", frozen)
	}
	if frozen.ProjectID != project.ID {
		t.Fatalf("(6) the freeze answered for project %q, want %q", frozen.ProjectID, project.ID)
	}
	if got := mainFrozenOf(t, ctx, pool, project.ID); !got {
		t.Fatal("(6) the freeze answered 200 and the flag is not set — the answer is not the state")
	}
	audits, outbox := freezeRows(t, ctx, pool, project.ID)
	if audits != 1 || outbox != 1 {
		t.Fatalf("(6) the freeze wrote %d audit rows and %d events, want exactly one of each", audits, outbox)
	}
	row := readFreezeAudit(t, ctx, pool, project.ID)
	if row.actorID != aliceID {
		t.Errorf("(6) the audit row names actor %q, want the freezing user %q", row.actorID, aliceID)
	}
	if row.targetRef != "project:"+project.ID {
		t.Errorf("(6) the audit row's target is %q, want the project", row.targetRef)
	}
	if row.correlationID == "" {
		t.Error("(6) the audit row has no correlation id — the freeze is untraceable")
	}
	var before, after, meta map[string]any
	for _, dec := range []struct {
		raw  []byte
		into *map[string]any
		what string
	}{
		{row.before, &before, "before_summary"},
		{row.after, &after, "after_summary"},
		{row.metadata, &meta, "metadata"},
	} {
		if err := json.Unmarshal(dec.raw, dec.into); err != nil {
			t.Fatalf("(6) decode the audit %s (%s): %v", dec.what, dec.raw, err)
		}
	}
	if before["main_frozen"] != false || after["main_frozen"] != true {
		t.Errorf("(6) the audit row does not state the transition: before=%v after=%v", before["main_frozen"], after["main_frozen"])
	}
	if meta["idempotency_key"] != key {
		t.Errorf("(6) the audit row does not carry the request's Idempotency-Key: %v", meta)
	}

	// The event, as the outbox holds it: the spec's name, the flag's value,
	// this project, this actor, and a visibility that is NOT public because
	// the project is not (docs/12: an event is never more visible than its
	// subject).
	var (
		eventType, eventActor, eventProject, eventVisibility, eventCorrelation string
		eventPayload                                                           []byte
	)
	if err := pool.QueryRow(ctx,
		`SELECT event_type, COALESCE(actor_id::text, ''), COALESCE(project_id::text, ''),
		        visibility, correlation_id, payload
		 FROM outbox_events WHERE project_id = $1 AND event_type = $2`,
		project.ID, "project.main_frozen").
		Scan(&eventType, &eventActor, &eventProject, &eventVisibility, &eventCorrelation, &eventPayload); err != nil {
		t.Fatalf("(6) read the freeze's domain event: %v", err)
	}
	if eventActor != aliceID || eventProject != project.ID {
		t.Errorf("(6) the event names actor %q / project %q, want %q / %q", eventActor, eventProject, aliceID, project.ID)
	}
	if eventVisibility == events.VisibilityPublic {
		t.Errorf("(6) the event of a private project is published as %q — the visibility is not fail-closed", eventVisibility)
	}
	if eventCorrelation != row.correlationID {
		t.Errorf("(6) the event and the audit row carry different correlation ids (%q / %q): they are not one transaction's records",
			eventCorrelation, row.correlationID)
	}
	var payload struct {
		PayloadVersion int  `json:"payload_version"`
		MainFrozen     bool `json:"main_frozen"`
	}
	if err := json.Unmarshal(eventPayload, &payload); err != nil {
		t.Fatalf("(6) decode the event payload (%s): %v", eventPayload, err)
	}
	if !payload.MainFrozen {
		t.Errorf("(6) the event payload does not say main is frozen: %s", eventPayload)
	}

	// ---- (7) An unknown project id is refused as a PERMISSION outcome, not
	// disclosed as a missing project.
	unknown := "99999999-9999-4999-8999-999999999999"
	status, body = freezeMainPost(t, alice, unknown, "freeze-unknown-key-0001")
	if status == http.StatusNotFound {
		t.Fatalf("(7) an unknown project id answered 404 (it disclosed the project's absence): %s", body)
	}
	if status != http.StatusForbidden {
		t.Fatalf("(7) an unknown project id = %d: %s, want the permission-class refusal", status, body)
	}
	var env errorEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("(7) decode the refusal: %v (%s)", err, body)
	}
	if env.Code != mainfreeze.CodeForbidden || env.Code == mainfreeze.CodeProjectNotFound {
		t.Fatalf("(7) an unknown project id answered code %q — existence was disclosed", env.Code)
	}

	// ---- (1) The frozen main refuses a DIRECT semantic write, over the real
	// RSG route, with the error model's own code — and nothing is written.
	mainHeadFrozen, err := statesSvc.GetBranchHead(ctx, mainBranch.ID)
	if err != nil {
		t.Fatalf("(1) read main's head state: %v", err)
	}
	_, statesOnMainBefore := stateCount(t, ctx, pool, project.ID, mainBranch.ID)
	status, body = wirePost(t, alice,
		branchObjectsPath(project.ID, mainBranch.ID),
		fmt.Sprintf(`{"object_type":"claim","payload":%s}`, mergeMainGateClaim("a direct write into frozen main")))
	if status != http.StatusForbidden {
		t.Fatalf("(1) a direct write into frozen main = %d: %s, want 403", status, body)
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("(1) decode the refusal: %v (%s)", err, body)
	}
	if env.Code != states.CodeMainFrozenDirectWrite {
		t.Fatalf("(1) the refusal code = %q, want %q — a caller cannot tell the freeze blocked it", env.Code, states.CodeMainFrozenDirectWrite)
	}
	mainHeadAfterRefusal, err := statesSvc.GetBranchHead(ctx, mainBranch.ID)
	if err != nil {
		t.Fatalf("(1) re-read main's head state: %v", err)
	}
	if mainHeadAfterRefusal.ID != mainHeadFrozen.ID {
		t.Errorf("(1) the refused write moved main: %s -> %s", mainHeadFrozen.ID, mainHeadAfterRefusal.ID)
	}
	if _, statesOnMainAfter := stateCount(t, ctx, pool, project.ID, mainBranch.ID); statesOnMainAfter != statesOnMainBefore {
		t.Errorf("(1) the refused write left %d states on main, was %d — a refusal wrote something", statesOnMainAfter, statesOnMainBefore)
	}

	// The freeze is PROJECT-level and main-only: the same route on a research
	// branch still writes. A gate that stopped research would be a different
	// rule than the one docs/09 §3 states.
	status, body = wirePost(t, alice,
		branchObjectsPath(project.ID, researchBranch.ID),
		fmt.Sprintf(`{"object_type":"claim","payload":%s}`, mergeMainGateClaim("a research claim written while main is frozen")))
	if status != http.StatusCreated {
		t.Fatalf("(2) a research-branch write while main is frozen = %d: %s, want 201", status, body)
	}

	// ---- (2) THE SAME TEST: the frozen project's Research PR merge still
	// succeeds. This is the whole point of freezing rather than locking — main
	// may only be advanced by a PR merge (docs/09 §3), and that path has to
	// stay open, for the owner included.
	prSvc := pullrequests.NewService(persistence.NewPullRequestStore(pool))
	pr, err := prSvc.Create(ctx, pullrequests.CreatePullRequestParams{
		ProjectID:      project.ID,
		SourceBranchID: researchBranch.ID,
		TargetBranchID: mainBranch.ID,
		Title:          "a research proposal that must still merge into frozen main",
		CreatedBy:      aliceID,
	})
	if err != nil {
		t.Fatalf("(2) create the pull request: %v", err)
	}
	if _, err := prSvc.RequestReview(ctx, project.ID, pr.Number); err != nil {
		t.Fatalf("(2) request review: %v", err)
	}
	if _, err := prSvc.SetState(ctx, project.ID, pr.Number, domain.PullRequestStateApproved); err != nil {
		t.Fatalf("(2) approve the pull request: %v", err)
	}
	if _, err := prSvc.SetState(ctx, project.ID, pr.Number, domain.PullRequestStateMergeReady); err != nil {
		t.Fatalf("(2) mark the pull request merge_ready: %v", err)
	}
	// The governed path's own push: what Gitea will report as `before` when
	// it delivers the merge it is about to perform below.
	mainGitBeforeMerge := mainHead(t, base, token, owner, repoName)
	if mainGitBeforeMerge == "" {
		t.Fatal("(2) the provider's main has no ref")
	}
	merged := mergeThroughTheEndpoint(t, alice, project.ID, pr.Number, "freeze-merge-key-0001")
	if merged.GitState != string(domain.GitStateUpdated) {
		t.Fatalf("(2) the merge into frozen main did not reach the provider: git_state=%q error=%q", merged.GitState, merged.GitError)
	}
	headAfterMerge, err := statesSvc.GetBranchHead(ctx, mainBranch.ID)
	if err != nil {
		t.Fatalf("(2) read main's head after the merge: %v", err)
	}
	if headAfterMerge.ID != merged.StateID {
		t.Fatalf("(2) main's head is %s, the merge accepted %s", headAfterMerge.ID, merged.StateID)
	}
	if headAfterMerge.ID == mainHeadFrozen.ID {
		t.Fatal("(2) the merge reported success and main did not move")
	}

	// ...and the refusal still holds AFTER the merge. A merge that reopened
	// direct writes would be a hole, not a feature.
	status, body = wirePost(t, alice,
		branchObjectsPath(project.ID, mainBranch.ID),
		fmt.Sprintf(`{"object_type":"claim","payload":%s}`, mergeMainGateClaim("a direct write after the merge")))
	if status != http.StatusForbidden {
		t.Fatalf("(1) a direct write into frozen main after the merge = %d: %s, want 403", status, body)
	}

	// ---- (3a) The Git side's SEAM: the platform's own governed merge
	// arrives at this endpoint as an ordinary push. Gitea delivers the merge
	// it just performed as a push of refs/heads/main — before = main's head
	// at the merge, after = the merge commit, no merge marker anywhere in
	// the payload — onto the very receiver the refusal below is tested
	// against. A freeze rule that refused every main delivery while frozen
	// would therefore refuse the governed path too: main's recorded head
	// would never advance, the pushed-head state and the branch's semantic
	// marks would never be written, and T0309's reconciler would report
	// `ref_head_moved` drift the platform caused itself, on every merge, for
	// good.
	//
	// The rule is that while frozen exactly one main delivery is admitted:
	// the one whose after IS the merge commit the platform recorded for this
	// project's main — and specifically the NEWEST such merge, the one main
	// is at. The assertions below are the real chain: two real merges through
	// the contract route, their commits read back from semantic_merges and
	// from the provider, the deliveries posted into the production receiver,
	// and the canonical rows that must follow. They are also what falsifies
	// the four ways this predicate can silently stop matching: the ref
	// spelling in the record, the ordering that picks WHICH recorded merge,
	// the git_state the record must have reached, and the project join that
	// resolves the repository to a project at all (a join that missed would
	// answer "not frozen" and let the foreign push below through).
	if merged.GitSHA == nil {
		t.Fatal("(3a) the merge recorded no Git sha, so there is no seam to test")
	}
	platformMergeSHA := *merged.GitSHA
	if got := mainHead(t, base, token, owner, repoName); got != platformMergeSHA {
		t.Fatalf("(3a) the provider's main is at %q, the merge recorded %q — the delivery's after would not be the platform's own merge", got, platformMergeSHA)
	}
	mergeRef, mergeSHA, mergeState := semanticMergeGit(t, ctx, pool, merged.MergeID)
	if mergeRef != gitprovider.MainRef || mergeState != string(domain.GitStateUpdated) || mergeSHA != platformMergeSHA {
		t.Fatalf("(3a) the recorded merge is ref=%q state=%q sha=%q, want %q/%q/%s — the seam's predicate reads exactly these three fields, and a mismatch in any of them refuses every platform merge",
			mergeRef, mergeState, mergeSHA, gitprovider.MainRef, domain.GitStateUpdated, platformMergeSHA)
	}
	// The pointer the ingestion guards with is still where merge #1 started:
	// the platform has ingested no main push in this project yet.
	mainRecordedBefore := recordedMainHead(t, ctx, pool, mainBranch.ID)
	if mainRecordedBefore != mainGitBeforeMerge {
		t.Fatalf("(3a) main's recorded head is %q while the provider's main ref was %q before the merge: the delivery's before names neither", mainRecordedBefore, mainGitBeforeMerge)
	}

	// ---- (3c) A SECOND governed merge, run before any main push is
	// ingested: the seam is pinned to the merge main is AT, not to every
	// merge the platform ever recorded, and only two recorded merges can tell
	// those apart. The second merge takes the same reviewed path as the
	// first — a fresh research branch, a real push, a claim, the PR flow, the
	// contract's merge route — so what it records is a real record.
	//
	// It runs before the deliveries for a reason worth knowing: ingesting a
	// main push writes a pushed-head project state whose parent T0305 resolves
	// from the delivery's before commit, and main's before commits carry no
	// state of their own, so that state is a chain root beside the semantic
	// one and the NEXT merge's chain_integrity check blocks on it (a
	// pre-existing interaction between the ingestion and the merge gate,
	// independent of the freeze and of this task's rule; it is reported in
	// RESULT rather than worked around here).
	r2Branch, err := rsgSvc.CreateBranch(ctx, aliceUser, project.ID, rsg.CreateBranchInput{
		Name: "freeze-r2", BaseRef: headAfterMerge.ID, Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("(3c) create the second research branch: %v", err)
	}
	if err := pushes.syncer().Sync(ctx, r2Branch.ID); err != nil {
		t.Fatalf("(3c) gitea integration: sync the second research branch ref: %v", err)
	}
	pushResearchBranch(t, base, token, owner, repoName, r2Branch.Name, "second governed research line")
	status, body = wirePost(t, alice,
		branchObjectsPath(project.ID, r2Branch.ID),
		fmt.Sprintf(`{"object_type":"claim","payload":%s}`, mergeMainGateClaim("the second governed proposal")))
	if status != http.StatusCreated {
		t.Fatalf("(3c) a write on the second research branch = %d: %s, want 201", status, body)
	}
	pr2, err := prSvc.Create(ctx, pullrequests.CreatePullRequestParams{
		ProjectID:      project.ID,
		SourceBranchID: r2Branch.ID,
		TargetBranchID: mainBranch.ID,
		Title:          "a second research proposal for the frozen main",
		CreatedBy:      aliceID,
	})
	if err != nil {
		t.Fatalf("(3c) create the second pull request: %v", err)
	}
	if _, err := prSvc.RequestReview(ctx, project.ID, pr2.Number); err != nil {
		t.Fatalf("(3c) request review: %v", err)
	}
	if _, err := prSvc.SetState(ctx, project.ID, pr2.Number, domain.PullRequestStateApproved); err != nil {
		t.Fatalf("(3c) approve the second pull request: %v", err)
	}
	if _, err := prSvc.SetState(ctx, project.ID, pr2.Number, domain.PullRequestStateMergeReady); err != nil {
		t.Fatalf("(3c) mark the second pull request merge_ready: %v", err)
	}
	if got := mainHead(t, base, token, owner, repoName); got != platformMergeSHA {
		t.Fatalf("(3c) the provider's main is %q before the second merge, want the first merge's %s", got, platformMergeSHA)
	}
	merged2 := mergeThroughTheEndpoint(t, alice, project.ID, pr2.Number, "freeze-merge-key-0002")
	if merged2.GitState != string(domain.GitStateUpdated) || merged2.GitSHA == nil {
		t.Fatalf("(3c) the second merge did not reach the provider: git_state=%q git_sha=%v error=%q", merged2.GitState, merged2.GitSHA, merged2.GitError)
	}
	secondMergeSHA := *merged2.GitSHA
	if secondMergeSHA == platformMergeSHA {
		t.Fatal("(3c) the second merge produced the first merge's commit — there is nothing new to tell apart")
	}
	if got := mainHead(t, base, token, owner, repoName); got != secondMergeSHA {
		t.Fatalf("(3c) the provider's main is at %q, the second merge recorded %q", got, secondMergeSHA)
	}
	if ref, sha, state := semanticMergeGit(t, ctx, pool, merged2.MergeID); ref != gitprovider.MainRef || state != string(domain.GitStateUpdated) || sha != secondMergeSHA {
		t.Fatalf("(3c) the second recorded merge is ref=%q state=%q sha=%q, want %q/%q/%s", ref, state, sha, gitprovider.MainRef, domain.GitStateUpdated, secondMergeSHA)
	}
	if got := recordedMainHead(t, ctx, pool, mainBranch.ID); got != mainGitBeforeMerge {
		t.Fatalf("(3c) main's recorded head is %q before any main delivery, want the synced %q", got, mainGitBeforeMerge)
	}

	// ---- (3a, continued) The deliveries, oldest record first.
	//
	// (i) The OLDER recorded merge, delivered onto the newer head. It is a
	// commit the platform itself merged, and under a predicate that admitted
	// any recorded merge sha it would be admitted here — and the head guard
	// would then move main's recorded head BACKWARD onto it (before names the
	// current head), recording a rewind as if it were a governed advance.
	// The seam's read takes the newest row, so it is refused, and nothing is
	// written.
	ingestionsBeforeOlder := countIngestions(t, ctx, pool)
	if code := pushes.deliver(t, repoID, owner, repoName, gitprovider.MainRef, secondMergeSHA, platformMergeSHA, 1, nil,
		webhookSecret, "freeze-main-older-merge-1"); code != http.StatusForbidden {
		t.Fatalf("(3a) a delivery moving frozen main back onto the previous merge commit = %d, want 403", code)
	}
	if got := countIngestions(t, ctx, pool); got != ingestionsBeforeOlder {
		t.Fatalf("(3a) the refused rewind wrote %d ingestion rows, was %d", got, ingestionsBeforeOlder)
	}
	if got := recordedMainHead(t, ctx, pool, mainBranch.ID); got != mainGitBeforeMerge {
		t.Fatalf("(3a) the refused rewind moved main's recorded head to %q, want it left at %q", got, mainGitBeforeMerge)
	}

	// (ii) The platform's own merge push for the merge main is AT: after IS
	// the recorded merge commit, so the frozen project admits it. The
	// delivery's before is the head the PLATFORM has recorded for main —
	// what its head guard advances from, and the only before that can make
	// the advance fire here, because the first main push of a project's life
	// is also its synchronised head. (Gitea's own payload for a merge names
	// the ref's previous value; in a run that had ingested merge #1's push
	// that value would be M1 — the reason it is not ingested here is the
	// chain_integrity interaction recorded above.)
	if code := pushes.deliver(t, repoID, owner, repoName, gitprovider.MainRef, mainRecordedBefore, secondMergeSHA, 1, nil,
		webhookSecret, "freeze-main-platform-merge-2"); code != http.StatusNoContent {
		t.Fatalf("(3a) the platform's own governed merge into frozen main = %d, want 204 — the freeze refused the platform's own merge", code)
	}
	// Admitted means RECORDED: the ingestion row, the advanced head pointer
	// and the pushed-head state are the three canonical facts the governed
	// merge needs and the reason the seam exists at all.
	delivery, deliveries := readMainDeliveries(t, ctx, pool, secondMergeSHA)
	if deliveries != 1 {
		t.Fatalf("(3a) the platform's own merge delivery left %d ingestion rows for %s, want 1 — the admitted branch did not record the delivery", deliveries, secondMergeSHA)
	}
	if delivery.gitRef != gitprovider.MainRef || delivery.before != mainRecordedBefore || delivery.branchName != "main" || delivery.headSkip != nil {
		t.Fatalf("(3a) the recorded delivery is ref=%q before=%q branch=%q head_skip=%v, want %q/%q/main/none — the platform's merge was recorded as something else",
			delivery.gitRef, delivery.before, delivery.branchName, delivery.headSkip, gitprovider.MainRef, mainRecordedBefore)
	}
	if got := recordedMainHead(t, ctx, pool, mainBranch.ID); got != secondMergeSHA {
		t.Fatalf("(3a) main's recorded head is %q after the platform's own merge delivery, want %s — the ref moved and the platform did not record it", got, secondMergeSHA)
	}
	if n := countPushedHeadStates(t, ctx, pool, project.ID, secondMergeSHA); n != 1 {
		t.Fatalf("(3a) the platform's own merge wrote %d pushed-head states for %s, want 1", n, secondMergeSHA)
	}

	// (iii) A redelivery of the SAME merge — the provider's retry, a fresh
	// delivery id, the same pushed head — stays the ingestion's dedupe no-op
	// (T0305's platform-wide rule, unchanged by the freeze): the seam admits
	// it, and nothing new is written for it. That is the "repeat delivery"
	// boundary: not a second head advance, not a second state, not a second
	// row.
	ingestionsBeforeRepeat := countIngestions(t, ctx, pool)
	if code := pushes.deliver(t, repoID, owner, repoName, gitprovider.MainRef, mainRecordedBefore, secondMergeSHA, 1, nil,
		webhookSecret, "freeze-main-platform-merge-2-again"); code != http.StatusNoContent {
		t.Fatalf("(3a) the redelivery of the platform's own merge = %d, want 204 (a duplicate delivery is not a refusal)", code)
	}
	if got := countIngestions(t, ctx, pool); got != ingestionsBeforeRepeat {
		t.Fatalf("(3a) the redelivered merge wrote %d ingestion rows, was %d — a duplicate delivery is not a no-op", got, ingestionsBeforeRepeat)
	}
	if _, n := readMainDeliveries(t, ctx, pool, secondMergeSHA); n != 1 {
		t.Fatalf("(3a) the redelivered merge left %d ingestion rows for %s, want 1", n, secondMergeSHA)
	}
	if got := recordedMainHead(t, ctx, pool, mainBranch.ID); got != secondMergeSHA {
		t.Fatalf("(3a) the redelivered merge moved main's recorded head to %q, want it left at %s", got, secondMergeSHA)
	}

	// (iv) A foreign direct push on the SAME frozen project: main pushed back
	// onto a real commit the platform never merged — the commit this project
	// started from, resolvable by the provider, so nothing but the rule can
	// explain the refusal.
	if code := pushes.deliver(t, repoID, owner, repoName, gitprovider.MainRef, secondMergeSHA, mainGitBeforeMerge, 1, nil,
		webhookSecret, "freeze-main-foreign-1"); code != http.StatusForbidden {
		t.Fatalf("(3a) a foreign direct push into the frozen project = %d, want 403", code)
	}
	if got := recordedMainHead(t, ctx, pool, mainBranch.ID); got != secondMergeSHA {
		t.Fatalf("(3a) the refused foreign push moved main's recorded head to %q, want it left at %s", got, secondMergeSHA)
	}

	// ---- (3b) The Git side: a direct push to main is refused BEFORE the
	// provider action. The delivery below carries shas the provider cannot
	// resolve, so a check that ran after reading the provider would answer 503
	// (the control at the top of this test is exactly that answer, on the same
	// project before it was frozen). The 403 can only come from a decision
	// taken before the read — and nothing was ingested.
	ingestionsBefore := countIngestions(t, ctx, pool)
	if code := pushes.deliver(t, repoID, owner, repoName, "refs/heads/main", ghostBefore, ghostAfter, 1, nil,
		webhookSecret, "freeze-main-frozen-1"); code != http.StatusForbidden {
		t.Fatalf("(3b) a direct main push into a frozen project = %d, want 403", code)
	}
	if got := countIngestions(t, ctx, pool); got != ingestionsBefore {
		t.Fatalf("(3b) the refused delivery wrote %d ingestion rows, was %d — the refusal is not before the provider action", got, ingestionsBefore)
	}
	// The same delivery on a research ref is NOT refused by the freeze: the
	// 503 says the ingester went on to read the provider (and failed there).
	if code := pushes.deliver(t, repoID, owner, repoName, "refs/heads/"+researchBranch.Name, ghostAfter, ghostAfter, 1, nil,
		webhookSecret, "freeze-research-frozen-1"); code != http.StatusServiceUnavailable {
		t.Fatalf("(3b) a delivery for a research ref = %d, want 503 (the freeze is main-only)", code)
	}

	// ---- (8) The idempotency is the STATE, not a ledger: the same key again,
	// and then a different key, both report the state the first call produced
	// and write nothing new.
	for _, replayKey := range []string{key, "freeze-governance-e2e-0002"} {
		replay := freezeThroughTheEndpoint(t, alice, project.ID, replayKey)
		if !replay.MainFrozen || !replay.AlreadyFrozen {
			t.Fatalf("(8) the repeat with key %q answered %+v, want main frozen and already frozen", replayKey, replay)
		}
	}
	if auditsNow, outboxNow := freezeRows(t, ctx, pool, project.ID); auditsNow != 1 || outboxNow != 1 {
		t.Fatalf("(8) after two repeats: %d audit rows and %d events, want the one the first freeze wrote", auditsNow, outboxNow)
	}

	// ---- (8) Two concurrent freezes: the conditional update admits one
	// winner, and only the winner writes. The fixture project is unfrozen
	// (its earlier freeze failed and rolled back), so this starts from a
	// genuinely free project.
	type outcome struct {
		status int
		wire   freezeWire
	}
	results := make([]outcome, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("freeze-concurrent-key-000%d", i+1)
			status, _, wire, err := freezeOnce(alice.client, ts.URL, second.ID, alice.csrf, key)
			if err != nil {
				results[i] = outcome{status: -1, wire: freezeWire{ProjectID: err.Error()}}
				return
			}
			results[i] = outcome{status: status, wire: wire}
		}(i)
	}
	wg.Wait()
	winners := 0
	for i, res := range results {
		if res.status != http.StatusOK {
			t.Fatalf("(8) concurrent freeze %d = %d (%s)", i, res.status, res.wire.ProjectID)
		}
		if !res.wire.AlreadyFrozen {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("(8) %d of the concurrent freezes report themselves as the winner, want exactly 1", winners)
	}
	if auditsNow, outboxNow := freezeRows(t, ctx, pool, second.ID); auditsNow != 1 || outboxNow != 1 {
		t.Fatalf("(8) the concurrent freezes wrote %d audit rows and %d events, want one of each from the single winner", auditsNow, outboxNow)
	}
	if got := mainFrozenOf(t, ctx, pool, second.ID); !got {
		t.Fatal("(8) the concurrent freeze left the project unfrozen")
	}

	// ---- (9) There is no unfreeze path. The contract has one direction
	// (specs/api/openapi.yaml defines :freeze and no :unfreeze; docs/09 §3
	// makes maintenance, not an API, the way a freeze is lifted), and the
	// surface must not offer one — asserted by asking for it.
	// Each probe carries a well-formed Idempotency-Key on purpose: a route
	// that exists must be refused for being a route that must not exist, not
	// for the shape of the request that asked for it: a ghost route would
	// otherwise answer 400 and this loop would pass for the wrong reason.
	for _, suffix := range []string{"/main:unfreeze", "/main/unfreeze", "/main:thaw"} {
		resp := alice.doKeyed(t, http.MethodPost, "/api/v1/projects/"+project.ID+suffix, "", "freeze-probe-key-0001")
		status, body = resp.StatusCode, readAll(t, resp)
		if status != http.StatusNotFound {
			t.Fatalf("(9) POST %s = %d: %s, want 404 — a second direction exists", suffix, status, body)
		}
	}
	if got := mainFrozenOf(t, ctx, pool, project.ID); !got {
		t.Fatal("(9) the project is no longer frozen after probing for an unfreeze route")
	}
}
