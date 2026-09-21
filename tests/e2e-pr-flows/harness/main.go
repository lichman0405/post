// Command harness is the server behind tests/e2e-pr-flows: the real POST
// HTTP surface (the routes cmd/api mounts, on the real handlers, the real
// stores, a real PostgreSQL and a real Gitea) that a real Chromium drives
// through the built web app.
//
// It is NOT a mock. Every flow step the browser takes is an HTTP request
// against a mounted product route — POST /branches, POST
// /branches/{id}/objects, POST /pull-requests, POST
// /pull-requests/{prId}/reviews, POST /pull-requests/{n}:merge, PUT
// /resolutions — served by the same handler packages cmd/api registers,
// over the same persistence adapters, against a fresh migrated database
// and a freshly provisioned Gitea repository. The browser holds a real
// session cookie and echoes the real session-bound CSRF token.
//
// What is fixture, and why that is not a shortcut around the machine:
//
//   - The organization and project rows, the reviewer memberships and the
//     Research Owners routing rules. The rules and the memberships have no
//     creation route in this build (the membership API changes an existing
//     role but cannot add one; nothing mounts the rules), so they are
//     seeded through the same stores the API writes them with. They are
//     project CONFIGURATION, not flow state: no proposal, review or merge
//     state is ever written directly.
//   - The repository: provisioned by the production provisioner
//     (gitprovider.NewProvisioner) — the call the API's provisioning job
//     makes.
//   - The branch refs: a background loop runs the production
//     BranchRefSyncer over every branch with an unsynced ref, standing in
//     for cmd/api's Redis-backed provisioning loop (this process has no
//     Redis).
//
// Two steps have no product route at all, and are served under /harness/
// rather than pretended into the API surface:
//
//   - POST /harness/pull-requests/{number}/request-review — docs/43's
//     open → review_required transition. specs/api/openapi.yaml declares
//     no request-review operation and specs/policies/permissions-matrix.csv
//     has no request_review cell, so there is nothing a browser could
//     call. The handler invokes the PRODUCTION command
//     (pullrequests.Service.RequestReview — validation, compare-and-swap,
//     audit) exactly as the integration suite's fixture does. It never
//     pokes the state column.
//   - POST /harness/git/push — a scientist's `git push` into a research
//     branch. A browser cannot run git, so this handler runs the real git
//     CLI against the real Gitea remote — the same push the integration
//     suite performs — which is what makes the branch carry Git content.
//
// The process prints one JSON line ("READY {...}") on stdout once every
// route is serving and the fixture is seeded; run.sh waits for it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	"github.com/lichman0405/post/internal/application/validation"
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
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// harnessTaskID names the database namespace (test_T0410_...), the same
// convention the integration suite uses: a run-scoped database nothing
// else can collide with, dropped again on exit.
const harnessTaskID = "T0410"

// reviewerPassword is the password every seeded human is signed up with.
// The browser logs in with it through the real sign-in page.
const reviewerPassword = "long-enough-password-1"

// human is one seeded account: the role it holds in the project and the
// one reviewer-responsibility label it holds (so the label a review is
// recorded under is never ambiguous).
type human struct {
	Email  string `json:"email"`
	Handle string `json:"handle"`
	Role   string `json:"role"`
	Label  string `json:"label"`
}

var humans = []human{
	{Email: "flow-owner@example.com", Handle: "flow-owner", Role: "owner"},
	{Email: "flow-maintainer@example.com", Handle: "flow-maintainer", Role: "maintainer", Label: "Experimental Reviewer"},
	{Email: "flow-reviewer@example.com", Handle: "flow-reviewer", Role: "viewer", Label: "Data Reviewer"},
}

func main() {
	adminURL := flag.String("admin-url", envDefault("POSTGRES_TEST_ADMIN_URL", "postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post"),
		"PostgreSQL maintenance URL; a run-scoped database is created on it")
	giteaURL := flag.String("gitea-url", envDefault("POST_GITEA_BASE_URL", "http://127.0.0.1:3000"),
		"Gitea base URL (the GitProvider the flows run against)")
	addr := flag.String("addr", envDefault("PR_FLOWS_API_ADDR", "127.0.0.1:18081"), "listen address for the API")
	webOrigin := flag.String("web-origin", envDefault("PR_FLOWS_WEB_ORIGIN", "http://127.0.0.1:31160"),
		"the web app's origin (the API's CORS/CSRF counterpart)")
	flag.Parse()

	if err := run(*adminURL, *giteaURL, *addr, *webOrigin); err != nil {
		fmt.Fprintf(os.Stderr, "harness: %v\n", err)
		os.Exit(1)
	}
}

func run(adminURL, giteaURL, addr, webOrigin string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := giteaReachable(ctx, giteaURL); err != nil {
		return fmt.Errorf("gitea at %s is not usable (%v): this suite runs against the real dev stack "+
			"(docker compose up postgres gitea); there is no mock to fall back on", giteaURL, err)
	}

	// ---- a fresh, migrated database: the flow's own namespace, dropped on exit.
	name := testdb.DatabaseName(harnessTaskID)
	if err := createDatabase(ctx, adminURL, name); err != nil {
		return fmt.Errorf("create database %s: %w", name, err)
	}
	defer dropDatabase(adminURL, name)
	dbURL, err := testdb.WithDatabase(adminURL, name)
	if err != nil {
		return err
	}
	if _, err := persistence.Migrate(ctx, dbURL); err != nil {
		return fmt.Errorf("migrate %s: %w", name, err)
	}
	pool, err := persistence.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	token, err := serviceToken(ctx, giteaURL)
	if err != nil {
		return fmt.Errorf("resolve the Gitea service token: %w", err)
	}
	cfg := gitprovider.Config{
		BaseURL: giteaURL,
		Token:   config.Secret(token),
		// No webhook receiver is mounted here (this process has no interest
		// in push deliveries); the URL is required by the provisioner's
		// contract and is never called back.
		WebhookURL: "http://host.invalid/api/v1/git/hooks/gitea",
	}
	adapter := gitprovider.NewGiteaAdapter(cfg)
	owner, err := adapter.Owner(ctx)
	if err != nil {
		return fmt.Errorf("resolve the Gitea service identity: %w", err)
	}

	api, err := buildAPI(ctx, pool, adapter, cfg, token, owner, webOrigin)
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	server := &http.Server{Handler: api.guarded, ReadHeaderTimeout: 15 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutCtx)
	}()

	base := "http://" + listener.Addr().String()

	// ---- the three humans, created through the real signup endpoint.
	ids := make([]string, len(humans))
	for i, h := range humans {
		id, err := signup(ctx, base, h.Email, h.Handle)
		if err != nil {
			return err
		}
		ids[i] = id
	}
	fixture, err := api.seed(ctx, ids)
	if err != nil {
		return err
	}
	defer fixture.cleanup(ctx)

	// The ref-sync loop: cmd/api runs this off Redis; here a ticker over the
	// same production syncer keeps Gitea's refs consistent with the branch
	// rows the browser creates.
	go syncLoop(ctx, gitprovider.NewBranchRefSyncer(adapter, gitprovider.NewBranchRefStore(pool)), pool)

	line, err := json.Marshal(map[string]any{
		"ready":        true,
		"api_base":     base,
		"web_origin":   webOrigin,
		"project_id":   fixture.projectID,
		"project_name": fixture.projectName,
		"repo":         fixture.repoName,
		"gitea":        giteaURL,
		"users":        humans,
		"password":     reviewerPassword,
	})
	if err != nil {
		return err
	}
	fmt.Println("READY " + string(line))

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	return nil
}

// serverLog records what the SERVER was asked, in the order it was asked.
// The interesting entries are the ones no Go client ever produces: a
// browser sends a CORS preflight before any write that carries a custom
// header, and whether the API's allow-list admits that header decides
// whether the write leaves the browser at all. Playwright's own network
// events do not report preflights, so the suite reads them back from here.
type serverLog struct {
	mu      sync.Mutex
	entries []serverLogEntry
}

type serverLogEntry struct {
	Method    string `json:"method"`
	Path      string `json:"path"`
	Status    int    `json:"status"`
	Origin    string `json:"origin,omitempty"`
	Asked     string `json:"asked_headers,omitempty"`
	Allowed   string `json:"allowed_headers,omitempty"`
	AllowOrig string `json:"allowed_origin,omitempty"`
}

func (l *serverLog) add(e serverLogEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, e)
}

func (l *serverLog) list() []serverLogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]serverLogEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

// wrap records every request after it is served, so the recorded status and
// CORS headers are the ones the handler wrote.
func (l *serverLog) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		e := serverLogEntry{Method: r.Method, Path: r.URL.Path, Status: sw.status}
		if r.Method == http.MethodOptions {
			e.Origin = r.Header.Get("Origin")
			e.Asked = r.Header.Get("Access-Control-Request-Headers")
			e.Allowed = sw.Header().Get("Access-Control-Allow-Headers")
			e.AllowOrig = sw.Header().Get("Access-Control-Allow-Origin")
		}
		l.add(e)
	})
}

// statusWriter is the minimal status-capturing wrapper (the header map is
// the real one, so a CORS header set by the guard is readable afterwards).
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// api is the composed surface plus the state the harness routes need.
type api struct {
	mux     *http.ServeMux
	guarded http.Handler
	pool    *pgxpool.Pool
	log     *serverLog

	orgStore     *persistence.OrgStore
	projectStore *persistence.ProjectStore
	policyStore  *persistence.PolicyStore
	routingSvc   *responsibilities.Service
	prSvc        *pullrequests.Service
	adapter      *gitprovider.GiteaAdapter

	// base is the GitProvider the git helper pushes to; token and owner
	// are the resolved service identity.
	base       string
	webhookURL string
	token      string
	owner      string
	repoName   string
}

// buildAPI composes the real handlers over the real stores — the same
// composition cmd/api performs for these routes.
func buildAPI(ctx context.Context, pool *pgxpool.Pool, adapter *gitprovider.GiteaAdapter,
	cfg gitprovider.Config, token, owner, webOrigin string) (*api, error) {
	reg, err := schemareg.New()
	if err != nil {
		return nil, err
	}
	orgStore := persistence.NewOrgStore(pool)
	projectStore := persistence.NewProjectStore(pool)
	policyStore := persistence.NewPolicyStore(pool)
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: projectStore,
		Orgs:  orgStore,
		Authz: authz.NewMatrixEngine(),
	})
	projectSvc := projectAPI.Service()
	policySvc := policyhttp.New(policyhttp.Deps{
		Store:    policyStore,
		Orgs:     orgStore,
		Projects: projectStore,
	}).Service()
	stateStore := persistence.NewStateStore(pool)
	branchStore := persistence.NewBranchStore(pool)
	branchSvc := branches.NewService(branchStore)
	statesSvc := states.NewService(stateStore,
		validation.NewGuard(rsgvalidation.NewValidator(reg), persistence.NewValidationTxProbe()))
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
	diffSvc := diffs.NewService(stateStore, persistence.NewManifestStore(pool), prStore)
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
	mergeSvc := merge.NewService(merge.Deps{
		Store:     persistence.NewSemanticMergeStore(pool),
		Diffs:     diffSvc,
		Plans:     resolutionSvc,
		Commits:   statesSvc,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Projects:  projectSvc,
		Authz:     authz.NewMatrixEngine(),
		Checks:    checksSvc,
		// The fork lineage (T0817), wired as cmd/api wires it: a source branch
		// outside the PR's project is admissible only when the lineage says it
		// is this PR author's fork of this project. These flows merge within
		// one project, so it is never consulted — and leaving it nil would
		// make a legitimate external proposal refuse.
		Forks:    persistence.NewForkStore(pool),
		Policies: policySvc,
		Rules:    policy.NewRuleEvaluator(),
		Events:   events.Recorder{},
		Git:      mergegit.New(adapter, gitprovider.NewUserAccessStore(pool)),
		RefGuard: gitprovider.RefGuard{MergeService: owner},
	})
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
			WebOrigin:          webOrigin,
			SessionTTL:         time.Hour,
			LoginLimitPerEmail: 1000,
			LoginLimitPerIP:    10000,
			LoginWindow:        time.Minute,
			SignupLimitPerIP:   10000,
		},
		Secure: false,
		Audit:  persistence.NewAuditStore(pool),
	})

	mux := http.NewServeMux()
	authAPI.Register(mux)
	mux.Handle("/api/v1/projects", projectAPI.Routes())
	mux.Handle("/api/v1/projects/", projectAPI.Routes())
	rsghttp.New(rsghttp.Deps{Service: rsgSvc}).Register(mux)
	pullrequestshttp.New(pullrequestshttp.Deps{
		PullRequests: prSvc,
		Create:       forksSvc,
		Checks:       checksSvc,
		Diff:         diffOfPR,
		Projects:     projectSvc,
	}).Register(mux)
	reviewhttp.New(reviewhttp.Deps{Service: reviewsSvc, Projects: projectSvc}).Register(mux)
	mergehttp.New(mergehttp.Deps{Command: mergeSvc, Projects: projectSvc}).Register(mux)
	conflicthttp.New(conflicthttp.Deps{Viewer: resolutionSvc, Saver: resolutionSvc, Projects: projectSvc}).Register(mux)

	a := &api{
		mux: mux, pool: pool,
		orgStore: orgStore, projectStore: projectStore, policyStore: policyStore,
		routingSvc: routingSvc, prSvc: prSvc, adapter: adapter,
		base: cfg.BaseURL, webhookURL: cfg.WebhookURL, token: token, owner: owner,
	}
	a.harnessRoutes(mux)
	a.log = &serverLog{}
	a.guarded = a.log.wrap(authAPI.Guard(mux))
	return a, nil
}

// seedFixture is the seeded substrate the browser starts from: the
// project the flows run on, and the repository it is bound to.
type seedFixture struct {
	projectID   string
	projectName string
	repoName    string
	base        string
	token       string
	owner       string
}

func (f *seedFixture) cleanup(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	deleteRepo(ctx, f.base, f.token, f.owner, f.repoName)
}

// seed writes the project substrate — organization, project, repository,
// memberships, routing rules, policy — and nothing else. No branch, no
// object, no pull request, no review, no merge: those are the browser's,
// and each one is an HTTP request against a product route.
func (a *api) seed(ctx context.Context, ids []string) (*seedFixture, error) {
	org, _, err := a.orgStore.CreateOrganization(ctx, domain.Organization{
		Slug: "mof-humidity", Name: "MOF Humidity",
	}, ids[0], todayUTC())
	if err != nil {
		return nil, fmt.Errorf("seed the organization: %w", err)
	}
	project, _, err := a.projectStore.CreateProject(ctx, domain.Project{
		OrganizationID:  &org.ID,
		Slug:            "demo-mof-humidity-separation",
		Name:            "MOF Gas Separation (demo)",
		Purpose:         "verify MOF-X C2H4/C2H6 separation performance at 40-70% RH (T0410 browser e2e)",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, ids[0])
	if err != nil {
		return nil, fmt.Errorf("seed the project: %w", err)
	}
	if err := gitprovider.NewProvisioner(a.adapter, gitprovider.NewProvisionStore(a.pool), a.webhookURL).
		Provision(ctx, project.ID); err != nil {
		return nil, fmt.Errorf("provision the repository: %w", err)
	}
	a.repoName = gitprovider.RepositoryName(project.ID)

	actor := domain.User{ID: ids[0], Email: humans[0].Email, Handle: humans[0].Handle}
	for i, h := range humans[1:] {
		user := domain.User{ID: ids[i+1], Email: h.Email, Handle: h.Handle}
		if _, err := a.pool.Exec(ctx,
			`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, $3)`,
			project.ID, user.ID, h.Role); err != nil {
			return nil, fmt.Errorf("seed the %s membership: %w", h.Role, err)
		}
		if _, err := a.routingSvc.Assign(ctx, actor, project.ID, user.ID, h.Label); err != nil {
			return nil, fmt.Errorf("assign %s: %w", h.Label, err)
		}
	}
	// One rule per object type these flows move: the protocol change is the
	// scientific one (docs/09 §7's "Protocol 冲突数值折中") and the claim is
	// the knowledge one. A change no rule routes makes the proposal
	// unsatisfiable, which is why the rules are fixture rather than a
	// convenience.
	for _, rule := range []struct{ objectType, label string }{
		{"protocol", "Experimental Reviewer"},
		{"claim", "Data Reviewer"},
	} {
		if _, err := a.routingSvc.AddRule(ctx, actor, project.ID, responsibilities.AddRuleInput{
			MatchKind:      domain.ResearchOwnerMatchObjectType,
			MatchValue:     rule.objectType,
			Responsibility: rule.label,
		}); err != nil {
			return nil, fmt.Errorf("route %s changes: %w", rule.objectType, err)
		}
	}
	if _, err := a.policyStore.CreateProjectVersion(ctx, domain.PolicyVersion{
		Scope:     domain.PolicyScope{ProjectID: project.ID},
		Version:   "v1",
		Policy:    domain.Policy{Rules: map[string]json.RawMessage{domain.RuleMainProtected: json.RawMessage("true")}},
		CreatedBy: ids[0],
	}, project.OrganizationID, func(*domain.Policy) error { return nil },
		domain.AuditEntry{
			ActorID: ids[0], Via: domain.ViaSession, Action: "policy.project_version_created",
			ProjectID: project.ID,
		}); err != nil {
		return nil, fmt.Errorf("seed the project's main_protected policy: %w", err)
	}
	return &seedFixture{
		projectID: project.ID, projectName: project.Name, repoName: a.repoName,
		base: a.base, token: a.token, owner: a.owner,
	}, nil
}

// harnessRoutes mounts the two steps no product route serves. Both live
// under /harness/ so they can never be mistaken for API surface.
func (a *api) harnessRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /harness/pull-requests/{number}/request-review", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ProjectID string `json:"project_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "the request body is not JSON")
			return
		}
		number, err := strconv.ParseInt(r.PathValue("number"), 10, 64)
		if err != nil || number < 1 {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "the path names no pull request")
			return
		}
		pr, err := a.prSvc.RequestReview(r.Context(), in.ProjectID, number)
		if err != nil {
			slog.Warn("harness: request review failed", "error", err)
			writeError(w, http.StatusConflict, "REQUEST_REVIEW_FAILED", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"number": pr.Number, "state": string(pr.State)})
	})
	mux.HandleFunc("POST /harness/git/push", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			BranchID string `json:"branch_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "the request body is not JSON")
			return
		}
		var ref string
		if err := a.pool.QueryRow(r.Context(),
			`SELECT git_ref FROM branches WHERE id = $1`, in.BranchID).Scan(&ref); err != nil {
			writeError(w, http.StatusNotFound, "BRANCH_UNKNOWN", "no branch with that id")
			return
		}
		if ref == "" {
			writeError(w, http.StatusConflict, "BRANCH_NOT_SYNCED", "the branch has no git ref yet")
			return
		}
		if err := pushCommit(r.Context(), a.base, a.token, a.owner, a.repoName, ref); err != nil {
			slog.Warn("harness: git push failed", "error", err)
			writeError(w, http.StatusConflict, "GIT_PUSH_FAILED", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ref": ref})
	})
	// GET /harness/requests — what the server was asked, including the
	// preflights the browser sent. The suite reads the CORS half of this
	// back: Playwright does not surface preflight requests.
	mux.HandleFunc("GET /harness/requests", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"requests": a.log.list()})
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}

// ---- helpers

func giteaReachable(ctx context.Context, base string) error {
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, base+"/api/v1/version", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /api/v1/version = %d", resp.StatusCode)
	}
	return nil
}

func createDatabase(ctx context.Context, adminURL, name string) error {
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		return err
	}
	defer admin.Close()
	_, err = admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %q`, name))
	return err
}

func dropDatabase(adminURL, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		return
	}
	defer admin.Close()
	_, _ = admin.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, name))
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func todayUTC() time.Time { return time.Now().UTC() }

// signup creates one account through the real signup endpoint and returns
// its user id.
func signup(ctx context.Context, base, email, handle string) (string, error) {
	body := fmt.Sprintf(`{"email":%q,"password":%q,"handle":%q,"display_name":%q}`,
		email, reviewerPassword, handle, handle)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/auth/signup", strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("signup %s = %d", handle, resp.StatusCode)
	}
	var payload struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	return payload.User.ID, nil
}

// serviceToken mints a run-scoped Gitea token for the service account
// through the admin's basic auth (Gitea only accepts basic auth on its
// token-management API) — the same flow the integration suite uses.
func serviceToken(ctx context.Context, base string) (string, error) {
	adminUser := envDefault("GITEA_ADMIN_USER", "postadmin")
	adminPass := envDefault("GITEA_ADMIN_PASSWORD", "postadmin_dev_pw")
	svcAccount := envDefault("GITEA_SERVICE_ACCOUNT", "post-git-svc")

	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	// Ensure the service account exists; Gitea answers 422 when it already
	// does, which is the expected steady state.
	create := fmt.Sprintf(`{"login_name":%q,"username":%q,"email":%q,"password":%q,"must_change_password":false}`,
		svcAccount, svcAccount, svcAccount+"@post.local", adminPass)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, base+"/api/v1/admin/users", strings.NewReader(create))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(adminUser, adminPass)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()

	tokenBody := fmt.Sprintf(`{"name":%q,"scopes":["write:repository","write:user","write:admin"]}`,
		fmt.Sprintf("t0410-harness-%d", time.Now().UnixNano()))
	req2, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		base+"/api/v1/users/"+url.PathEscape(svcAccount)+"/tokens", strings.NewReader(tokenBody))
	if err != nil {
		return "", err
	}
	req2.Header.Set("Content-Type", "application/json")
	req2.SetBasicAuth(adminUser, adminPass)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		return "", err
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusCreated && resp2.StatusCode != http.StatusOK {
		return "", fmt.Errorf("mint the service token = %d", resp2.StatusCode)
	}
	var minted struct {
		SHA1 string `json:"sha1"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&minted); err != nil {
		return "", err
	}
	if minted.SHA1 == "" {
		return "", errors.New("the provider returned no token")
	}
	return minted.SHA1, nil
}

func syncLoop(ctx context.Context, syncer *gitprovider.BranchRefSyncer, pool *pgxpool.Pool) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ids, err := unsyncedBranches(ctx, pool)
			if err != nil {
				continue
			}
			for _, id := range ids {
				if err := syncer.Sync(ctx, id); err != nil {
					slog.Debug("harness: ref sync", "branch", id, "error", err)
				}
			}
		}
	}
}

func unsyncedBranches(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT b.id::text
		  FROM branches b
		  LEFT JOIN git_branch_refs r ON r.branch_id = b.id
		 WHERE r.branch_id IS NULL AND b.lifecycle = 'active'
		 LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// pushCommit runs the real git CLI against the real Gitea remote — the
// push a scientist's working copy makes.
func pushCommit(ctx context.Context, base, token, owner, repo, ref string) error {
	dir, err := os.MkdirTemp("", "pr-flows-git-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	remote := base + "/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + ".git"
	branch := strings.TrimPrefix(ref, "refs/heads/")
	auth := gitAuthEnv("Authorization: token " + token)
	for _, args := range [][]string{
		{"init", "-q", "-b", "scratch"},
		{"config", "user.email", "harness@post.local"},
		{"config", "user.name", "POST harness"},
		// The commit must sit ON TOP OF main, not on an orphan history:
		// the provider only merges a ref pair that shares an ancestor, so
		// an unrelated root would make every merge fail at the git step
		// with a conflict — a fixture artefact masquerading as a product
		// failure. This mirrors the integration suite's researchDir.
		{"fetch", remote, "main"},
		{"checkout", "-q", "-b", branch, "FETCH_HEAD"},
	} {
		if out, err := runGit(ctx, dir, auth, args...); err != nil {
			return fmt.Errorf("git %s: %v (%s)", strings.Join(args, " "), err, out)
		}
	}
	note := filepath.Join(dir, "research-"+branch+".txt")
	if err := os.WriteFile(note, []byte(branch+"\n"), 0o600); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"add", "."},
		{"commit", "-q", "-m", "research notes for " + branch},
		{"push", remote, "HEAD:refs/heads/" + branch},
	} {
		if out, err := runGit(ctx, dir, auth, args...); err != nil {
			return fmt.Errorf("git %s: %v (%s)", strings.Join(args, " "), err, out)
		}
	}
	return nil
}

// gitAuthEnv is the environment that hands one git invocation its
// Authorization header. The header rides GIT_CONFIG_VALUE_0 and NOT a
// `-c http.extraHeader=...` argument: a `-c` value is an argument, and argv
// is readable by every account on the machine (ps aux, /proc/<pid>/cmdline).
// This is the shape internal/gitprovider/gitea.go uses and the shape the
// integration suite's gitAuthEnv moved to; the harness is a client of the
// same provider, so it follows the same rule.
func gitAuthEnv(header string) []string {
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=" + header,
	}
}

func runGit(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(append(os.Environ(), "HOME="+dir), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func deleteRepo(ctx context.Context, base, token, owner, repo string) {
	if repo == "" {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		base+"/api/v1/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(repo), nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "token "+token)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}
