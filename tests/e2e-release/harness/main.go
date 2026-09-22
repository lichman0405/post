// Command harness is the server behind tests/e2e-release: the real POST
// HTTP surface — the routes cmd/api mounts, on the real handlers, the real
// stores, a real PostgreSQL — that a real Chromium drives through the built
// web app.
//
// It is NOT a mock. Every step of the flow the browser takes is an HTTP
// request against a mounted product route: PUT /projects/{id}/policy,
// POST /branches, POST /branches/{id}/objects, POST /pull-requests, POST
// /pull-requests/{prId}/reviews, POST /pull-requests/{n}:merge, POST
// /projects/{id}/releases (through the app's own Releases form), POST
// /projects/{id}/assets:publish and POST
// /projects/{id}/objects/{objectId}:abort-proposal. The browser holds a
// real session cookie and echoes the real session-bound CSRF token; every
// write carries a real Idempotency-Key and therefore a real CORS
// preflight.
//
// # What is fixture, and why that is not a shortcut around the machine
//
//   - The organization and project rows, the memberships and the Research
//     Owners routing rules. The rules and the memberships have no creation
//     route in this build (the membership API changes an existing role but
//     cannot add one; nothing mounts the rules), so they are seeded through
//     the same stores the API writes them with. They are project
//     CONFIGURATION, not flow state: no proposal, review, merge, release or
//     abort state is ever written directly.
//   - The asset publish candidate (GET /harness/asset-candidate): the
//     manifest document, its canonical bytes and the sha256 the integrity
//     hash carries. This is the CONTENT a publisher authors — the document
//     that says what the version is — and it is computed with the
//     production internal/assets model (Manifest.CanonicalJSON/Hash) so the
//     bytes the browser posts are the bytes the product's own gate
//     re-derives. Nothing about the publication is decided here; the
//     publish route validates, authorizes and stores it.
//   - The project policy is NOT fixture: the flow sets
//     main_protected/release_min_reviewers through the product's own
//     PUT /projects/{id}/policy route, because the policy is the first leg
//     of the chain under test.
//
// # The two /harness/ endpoints, and what they are for
//
//   - POST /harness/pull-requests/{number}/request-review — docs/43's
//     open → review_required transition. specs/api/openapi.yaml declares no
//     request-review operation and specs/policies/permissions-matrix.csv
//     has no request_review cell, so there is nothing a browser could call.
//     The handler invokes the PRODUCTION command
//     (pullrequests.Service.RequestReview — validation, compare-and-swap,
//     audit) exactly as the integration suite's fixture does, and never
//     touches the state column.
//   - GET /harness/rows — READ-ONLY row snapshots (SELECT to_jsonb(row)),
//     the observation instrument for the flow's last assertion. "The
//     stored release row and the stored asset version row are
//     byte-identical before and after the abort" is not a question any
//     product route answers: every product read renders the row for a
//     caller, and a rendering is not the row. This endpoint reads the two
//     rows verbatim and hands them back as text, so the browser can compare
//     the BYTES. It writes nothing.
//   - GET /harness/object-rows — the same instrument over the version log
//     of one scientific object. The flow reads it before and after the
//     abort and REQUIRES it to differ: an instrument that cannot report a
//     change cannot report the absence of one either, and this is the
//     control that proves the release/asset probes above were measuring
//     something.
//   - GET /harness/requests — what the server was asked, including the
//     preflights. Playwright does not surface preflight requests.
//
// # No git provider
//
// This flow writes no git content: it pushes no commit and merges no refs.
// The merge is wired exactly as tests/integration/abort_e2e_test.go and
// tests/integration/release_e2e_test.go wire it — Git absent — which the
// merge service documents as "the Git half has not happened; the database
// truth commits anyway". The git half of the product is covered by
// tests/e2e-pr-flows, which runs against real Gitea; duplicating that here
// would add a provider dependency to a flow that never touches one.
//
// The process prints one JSON line ("READY {...}") on stdout once every
// route is serving and the fixture is seeded; run.sh waits for it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/aborthttp"
	"github.com/lichman0405/post/cmd/api/assetshttp"
	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/mergehttp"
	"github.com/lichman0405/post/cmd/api/policyhttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/cmd/api/pullrequestshttp"
	"github.com/lichman0405/post/cmd/api/releasehttp"
	"github.com/lichman0405/post/cmd/api/reviewhttp"
	"github.com/lichman0405/post/cmd/api/rsghttp"

	"github.com/lichman0405/post/internal/application/aborts"
	"github.com/lichman0405/post/internal/application/assetpublish"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/diffs"
	"github.com/lichman0405/post/internal/application/forks"
	"github.com/lichman0405/post/internal/application/manifests"
	"github.com/lichman0405/post/internal/application/merge"
	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/prchecks"
	"github.com/lichman0405/post/internal/application/prdiff"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/application/releases"
	"github.com/lichman0405/post/internal/application/resolutions"
	"github.com/lichman0405/post/internal/application/responsibilities"
	"github.com/lichman0405/post/internal/application/reviews"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/states"
	appvalidation "github.com/lichman0405/post/internal/application/validation"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rights"
	"github.com/lichman0405/post/internal/rsg/integrity"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// harnessTaskID names the database namespace (test_T0608_...), the same
// convention the integration suite uses: a run-scoped database nothing
// else can collide with, dropped again on exit.
const harnessTaskID = "T0608"

// reviewerPassword is the password every seeded human is signed up with.
// The browser logs in with it through the real sign-in page.
const reviewerPassword = "long-enough-password-1"

// human is one seeded account: the role it holds in the project and the
// one reviewer-responsibility label it holds. Both reviewers carry the
// SAME label, because the responsibility is a job ("Data Reviewer") several
// people hold — and the policy under test counts PEOPLE (distinct
// reviewers), not labels. A flow that gave the two reviewers different
// labels would prove nothing about the count.
type human struct {
	Email  string `json:"email"`
	Handle string `json:"handle"`
	Role   string `json:"role"`
	Label  string `json:"label"`
}

var humans = []human{
	{Email: "release-owner@example.com", Handle: "release-owner", Role: "owner"},
	{Email: "release-reviewer-a@example.com", Handle: "release-reviewer-a", Role: "maintainer", Label: "Data Reviewer"},
	{Email: "release-reviewer-b@example.com", Handle: "release-reviewer-b", Role: "viewer", Label: "Data Reviewer"},
}

func main() {
	adminURL := flagAdminURL()
	addr := envDefault("RELEASE_API_ADDR", "127.0.0.1:18191")
	webOrigin := envDefault("RELEASE_WEB_ORIGIN", "http://127.0.0.1:31170")
	if err := run(adminURL, addr, webOrigin); err != nil {
		fmt.Fprintf(os.Stderr, "harness: %v\n", err)
		os.Exit(1)
	}
}

func flagAdminURL() string {
	return envDefault("POSTGRES_TEST_ADMIN_URL", "postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post")
}

func run(adminURL, addr, webOrigin string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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

	api, err := buildAPI(ctx, pool, webOrigin)
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

	line, err := json.Marshal(map[string]any{
		"ready":        true,
		"api_base":     base,
		"web_origin":   webOrigin,
		"project_id":   fixture.projectID,
		"project_name": fixture.projectName,
		"org_id":       fixture.orgID,
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
	routingSvc   *responsibilities.Service
	prSvc        *pullrequests.Service

	// ownerID is the seeded owner, used to name the asset version's
	// creators (the credit list of the fixture's candidate document).
	ownerID string
}

// buildAPI composes the real handlers over the real stores — the same
// composition cmd/api performs for these routes.
func buildAPI(ctx context.Context, pool *pgxpool.Pool, webOrigin string) (*api, error) {
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
	policyAPI := policyhttp.New(policyhttp.Deps{
		Store:    policyStore,
		Orgs:     orgStore,
		Projects: projectStore,
	})
	stateStore := persistence.NewStateStore(pool)
	branchStore := persistence.NewBranchStore(pool)
	branchSvc := branches.NewService(branchStore)
	statesSvc := states.NewService(stateStore,
		appvalidation.NewGuard(rsgvalidation.NewValidator(reg), persistence.NewValidationTxProbe()))
	objects := persistence.NewScientificObjectStore(pool)
	relations := persistence.NewRelationStore(pool)
	// Queries and Profiles are wired exactly as cmd/api wires them
	// (cmd/api/main.go:568-569). They are not decoration: the state-pinned
	// slice (GET /projects/{id}/query) and main's head state (GET
	// /projects/{id}/overview) are the two reads that answer "has main
	// moved?", and a service constructed without them refuses both with 503
	// by design (query.go:71, fail closed) — which is what a harness that
	// skipped them would report as "the product cannot answer", wrongly.
	rsgSvc := rsg.NewService(rsg.Deps{
		Projects:  projectSvc,
		Branches:  branchSvc,
		States:    statesSvc,
		Latest:    stateStore,
		Objects:   objects,
		Relations: relations,
		Queries:   persistence.NewRSGQueryStore(pool),
		Profiles:  persistence.NewProfileStore(pool),
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
		Events:    events.Recorder{},
	})
	prStore := persistence.NewPullRequestStore(pool)
	prSvc := pullrequests.NewService(prStore)
	// The open route's command (cmd/api wires the same service into
	// pullrequestshttp.Deps.Create). The flow opens its proposals through
	// that route, so the service is required wiring, not an extra.
	forksSvc := forks.NewService(forks.Deps{
		Projects:     projectSvc,
		Branches:     branchSvc,
		BranchWriter: rsgSvc,
		Forks:        persistence.NewForkStore(pool),
		PullRequests: prSvc,
		Authz:        authz.NewMatrixEngine(),
	})
	// The third port is the proposal read (T0817): it is what lets the SOURCE
	// side of a triple live in another project when a pull request of this one
	// proposes it — the external fork's shape. The proposal store is already
	// here (prStore, above) and is the same implementation cmd/api wires.
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
		Objects:   objects,
		Relations: relations,
		Projects:  projectSvc,
		Authz:     authz.NewMatrixEngine(),
		Checks:    checksSvc,
		// The fork lineage (T0817), wired exactly as cmd/api wires it: a
		// source branch outside the PR's project is admissible only when the
		// lineage says it is this PR author's fork of this project. This
		// flow merges within one project, so it is never consulted — and
		// leaving it nil would make a legitimate external proposal refuse.
		Forks:    persistence.NewForkStore(pool),
		Policies: policyAPI.Service(),
		Rules:    policy.NewRuleEvaluator(),
		Events:   events.Recorder{},
		// The abort reader is wired exactly as cmd/api wires it: the merge
		// copies docs/46:7's record onto the row it lands on main.
		Aborts: objects,
		// Git is deliberately absent — see the package doc.
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
	releaseStore := persistence.NewReleaseStore(pool)
	releaseBuilder := releases.NewService(
		stateStore,
		manifests.NewService(stateStore, persistence.NewManifestStore(pool)),
		projectStore,
		branchStore,
		policyStore,
		releaseStore,
		reg,
	)
	releaseCommand := releases.NewCommand(
		releaseBuilder,
		projectStore,
		policyStore,
		branchStore,
		stateStore,
		appvalidation.NewService(
			persistence.NewValidationSnapshotRepository(stateStore),
			rsgvalidation.NewValidator(reg),
		),
		releaseStore,
		authz.NewMatrixEngine(),
	)
	abortSvc := aborts.NewService(aborts.Deps{
		Members:      projectStore,
		Authz:        authz.NewMatrixEngine(),
		Objects:      objects,
		Branches:     branchSvc,
		PullRequests: prSvc,
		Commits:      statesSvc,
		Events:       events.Recorder{},
	})
	publishCommand := assetpublish.NewCommand(assetpublish.Deps{
		Members:  projectStore,
		Policies: policyAPI.Service(),
		Rules:    policy.NewRuleEvaluator(),
		Store:    persistence.NewAssetPublishStore(pool, rsgvalidation.NewValidator(reg)),
		Authz:    authz.NewMatrixEngine(),
	})
	assetsAPI := assetshttp.New(assetshttp.Deps{
		State:        assetshttp.NewPostgresStateStore(pool),
		Projects:     projectSvc,
		Publish:      publishCommand,
		Pages:        persistence.NewAssetPageStore(pool),
		Members:      projectSvc,
		Dependencies: persistence.NewProjectDependencyStore(pool),
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
	policyAPI.Register(mux)
	releasehttp.New(releasehttp.Deps{Command: releaseCommand, Projects: projectSvc}).Register(mux)
	aborthttp.New(aborthttp.Deps{Command: abortSvc}).Register(mux)
	assetsAPI.Register(mux)

	a := &api{
		mux: mux, pool: pool,
		orgStore: orgStore, projectStore: projectStore,
		routingSvc: routingSvc, prSvc: prSvc,
	}
	a.harnessRoutes(mux)
	a.log = &serverLog{}
	a.guarded = a.log.wrap(authAPI.Guard(mux))
	return a, nil
}

// seedFixture is the seeded substrate the browser starts from.
type seedFixture struct {
	projectID   string
	projectName string
	orgID       string
}

// seed writes the project substrate — organization, project, memberships,
// routing rules — and nothing else. No branch, no object, no pull request,
// no review, no merge, no policy, no release, no asset, no abort: those are
// the browser's, and each one is an HTTP request against a product route.
func (a *api) seed(ctx context.Context, ids []string) (*seedFixture, error) {
	org, _, err := a.orgStore.CreateOrganization(ctx, domain.Organization{
		Slug: "mof-release", Name: "MOF Release Study",
	}, ids[0], todayUTC())
	if err != nil {
		return nil, fmt.Errorf("seed the organization: %w", err)
	}
	project, _, err := a.projectStore.CreateProject(ctx, domain.Project{
		OrganizationID:  &org.ID,
		Slug:            "mof-release-rh",
		Name:            "MOF Release Study (demo)",
		Purpose:         "verify the release/abort/policy chain end to end (T0608 browser e2e)",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, ids[0])
	if err != nil {
		return nil, fmt.Errorf("seed the project: %w", err)
	}
	a.ownerID = ids[0]

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
	// One rule: every claim change in this project is the Data Reviewer's to
	// review. A change no rule routes makes the proposal unsatisfiable
	// (BuildRequiredReviews refuses to read a missing rule as "no reviewer
	// needed"), so the rule is fixture rather than a convenience.
	if _, err := a.routingSvc.AddRule(ctx, actor, project.ID, responsibilities.AddRuleInput{
		MatchKind:      domain.ResearchOwnerMatchObjectType,
		MatchValue:     "claim",
		Responsibility: "Data Reviewer",
	}); err != nil {
		return nil, fmt.Errorf("route claim changes: %w", err)
	}
	return &seedFixture{projectID: project.ID, projectName: project.Name, orgID: org.ID}, nil
}

// harnessRoutes mounts what no product route serves: the one missing
// transition, and the read-only observation instruments.
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

	// GET /harness/rows?release_id=&asset_pid=&asset_version= — the stored
	// release row and the stored asset version row, verbatim (to_jsonb), as
	// text. Read-only: this is the instrument the byte-identity assertion
	// needs, and it is the only way to see a ROW rather than a rendering of
	// one.
	mux.HandleFunc("GET /harness/rows", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		out := map[string]any{}
		if id := q.Get("release_id"); id != "" {
			row, err := a.rowText(r, `SELECT to_jsonb(t)::text FROM releases t WHERE t.id = $1::uuid`, id)
			if err != nil {
				writeError(w, http.StatusBadRequest, "ROW_READ_FAILED", err.Error())
				return
			}
			out["release"] = row
		}
		if pid, version := q.Get("asset_pid"), q.Get("asset_version"); pid != "" && version != "" {
			row, err := a.rowText(r, `
				SELECT to_jsonb(v)::text
				  FROM research_asset_versions v
				  JOIN research_assets a ON a.id = v.asset_id
				 WHERE a.pid = $1 AND v.version = $2`, pid, version)
			if err != nil {
				writeError(w, http.StatusBadRequest, "ROW_READ_FAILED", err.Error())
				return
			}
			out["asset_version"] = row
		}
		writeJSON(w, http.StatusOK, out)
	})

	// GET /harness/object-rows?object_id= — every version row of one
	// scientific object, verbatim and in version order. The flow requires
	// THIS probe to report a change across the abort: an instrument that
	// cannot report a difference cannot report sameness either.
	mux.HandleFunc("GET /harness/object-rows", func(w http.ResponseWriter, r *http.Request) {
		objectID := r.URL.Query().Get("object_id")
		if objectID == "" {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "object_id is required")
			return
		}
		rows, err := a.pool.Query(r.Context(), `
			SELECT to_jsonb(v)::text
			  FROM scientific_object_versions v
			 WHERE v.object_id = $1::uuid
			 ORDER BY v.version_no`, objectID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "ROW_READ_FAILED", err.Error())
			return
		}
		defer rows.Close()
		out := []string{}
		for rows.Next() {
			var text string
			if err := rows.Scan(&text); err != nil {
				writeError(w, http.StatusBadRequest, "ROW_READ_FAILED", err.Error())
				return
			}
			out = append(out, text)
		}
		if err := rows.Err(); err != nil {
			writeError(w, http.StatusBadRequest, "ROW_READ_FAILED", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"rows": out, "count": len(out)})
	})

	// GET /harness/asset-candidate — the publish candidate DOCUMENT: the
	// manifest, its canonical bytes, the hash that covers them, the default
	// rights declaration and the provenance pins. This is the content a
	// publisher authors; the product's publish route is what decides whether
	// it may be stored.
	//
	// The three required parameters describe the release and object version
	// the candidate pins. Six optional ones (T1103) let a caller author a
	// DIFFERENT document from the same place, so a suite can reach states the
	// one default document cannot express — and so the hash below is always
	// computed by the production model over the document actually returned:
	//
	//	version          the version label               (default "1.0")
	//	title, slug      the display fields of a NEW asset
	//	visibility       the target visibility           (default "private")
	//	dependency_pins  comma-separated pid@version pins (default: none)
	//	blob_ids         comma-separated blob ids        (default: blob-fixture-0001)
	//	metadata_pad     a metadata value of N characters, for size limits
	//
	// Every default reproduces the document this route has always answered
	// with, so an existing caller (release-e2e.mjs) is unaffected by them.
	mux.HandleFunc("GET /harness/asset-candidate", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		projectID, releaseID, versionID := q.Get("project_id"), q.Get("release_id"), q.Get("object_version_id")
		if projectID == "" || releaseID == "" || versionID == "" {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST",
				"project_id, release_id and object_version_id are required")
			return
		}
		versionLabel := queryDefault(q, "version", "1.0")
		visibility := queryDefault(q, "visibility", "private")
		if !assets.ValidVersionLabel(versionLabel) {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST",
				"version "+strconv.Quote(versionLabel)+" is not a storable version label")
			return
		}
		if v := assets.Visibility(visibility); !v.Valid() {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST",
				"visibility must be public or private, got "+strconv.Quote(visibility))
			return
		}
		metadata := assets.Metadata{
			"purpose":       "the isotherm series behind the released claim",
			"data_type":     "adsorption isotherm",
			"blob_ids":      queryList(q, "blob_ids", []string{"blob-fixture-0001"}),
			"access_level":  "restricted",
			"quality_notes": "instrument drift corrected against the reference run",
		}
		if pad := q.Get("metadata_pad"); pad != "" {
			n, err := strconv.Atoi(pad)
			if err != nil || n < 0 {
				writeError(w, http.StatusBadRequest, "BAD_REQUEST",
					"metadata_pad must be a non-negative integer")
				return
			}
			// A key of its own rather than a padded value of a required one:
			// the manifest is an open map and an extra key is part of the
			// document (and therefore of the hash), which is the point.
			metadata["harness_pad"] = strings.Repeat("x", n)
		}
		pins := make([]assets.DependencyPin, 0, 2)
		for _, pin := range queryList(q, "dependency_pins", nil) {
			pins = append(pins, assets.DependencyPin(pin))
		}
		manifest := assets.Manifest{
			Version:        assets.ManifestFormatVersion,
			AssetType:      assets.TypeDataset,
			Metadata:       metadata,
			DependencyPins: pins,
		}
		raw, err := manifest.CanonicalJSON()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "CANDIDATE_FAILED", err.Error())
			return
		}
		hash, err := manifest.Hash()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "CANDIDATE_FAILED", err.Error())
			return
		}
		rightsDoc, err := json.Marshal(rights.New())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "CANDIDATE_FAILED", err.Error())
			return
		}
		refs := make([]string, 0, 3)
		for _, ref := range []struct {
			kind  assets.OriginKind
			value string
		}{
			{assets.KindProject, projectID},
			{assets.KindRelease, releaseID},
			{assets.KindObjectVersion, versionID},
		} {
			built, ok := assets.NewOriginRef(ref.kind, ref.value)
			if !ok {
				writeError(w, http.StatusBadRequest, "BAD_REQUEST",
					"the "+string(ref.kind)+" pin is not a uuid: "+ref.value)
				return
			}
			refs = append(refs, string(built))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"asset_pid":      "",
			"asset_type":     string(assets.TypeDataset),
			"version":        versionLabel,
			"manifest":       json.RawMessage(raw),
			"rights":         json.RawMessage(rightsDoc),
			"origin_refs":    refs,
			"visibility":     visibility,
			"integrity_hash": hash,
			"creator_ids":    []string{a.ownerID},
			"title":          queryDefault(q, "title", "Isotherm series behind the released claim"),
			"slug":           queryDefault(q, "slug", "isotherm-series-released-claim"),
		})
	})

	// POST /harness/blob — a stored blob, openly attached to one object
	// version, and its id. A fixture, like the organization and the
	// candidate: this build has a file-upload surface but no route that
	// ATTACHES a blob to an object version with an access level, and the
	// preview/publish rules that read an access level are exactly what the
	// publish suite needs a stored blob for (internal/assets/preview.go: a
	// blob blocks when the rights declaration promises open access and the
	// blob is not openly attached). Nothing about a publication is decided
	// here; the row is what the product's own reader sees.
	mux.HandleFunc("POST /harness/blob", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ObjectVersionID string `json:"object_version_id"`
			AccessLevel     string `json:"access_level"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "the request body is not JSON")
			return
		}
		if in.ObjectVersionID == "" {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "object_version_id is required")
			return
		}
		if in.AccessLevel == "" {
			in.AccessLevel = "open"
		}
		var exists bool
		if err := a.pool.QueryRow(r.Context(),
			`SELECT EXISTS(SELECT 1 FROM scientific_object_versions WHERE id = $1::uuid)`,
			in.ObjectVersionID).Scan(&exists); err != nil || !exists {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST",
				"object_version_id does not name a stored object version")
			return
		}
		var blobID string
		if err := a.pool.QueryRow(r.Context(), `
			INSERT INTO blobs (content_hash, size_bytes, media_type, storage_key, integrity_state, created_by)
			VALUES ($1, $2, 'application/octet-stream', $3, 'verified', $4::uuid)
			RETURNING id::text`,
			"harness-"+strconv.FormatInt(time.Now().UnixNano(), 10), int64(1024),
			"harness/blob/"+strconv.FormatInt(time.Now().UnixNano(), 10), a.ownerID,
		).Scan(&blobID); err != nil {
			writeError(w, http.StatusConflict, "BLOB_NOT_CREATED", err.Error())
			return
		}
		// state_id comes from the object version the attachment is made
		// against — the same source migration 00035 backfilled the column
		// from, and the only one that exists here: an attachment is made
		// against a version, and that version's state is what a manifest
		// export enumerates it by (it is NOT NULL since 00035).
		tag, err := a.pool.Exec(r.Context(), `
			INSERT INTO blob_attachments (blob_id, scientific_object_version_id, attachment_role, access_level, state_id)
			SELECT $1::uuid, sov.id, 'data', $3, sov.state_id
			  FROM scientific_object_versions sov
			 WHERE sov.id = $2::uuid`,
			blobID, in.ObjectVersionID, in.AccessLevel)
		if err != nil {
			writeError(w, http.StatusConflict, "BLOB_NOT_ATTACHED", err.Error())
			return
		}
		if tag.RowsAffected() != 1 {
			writeError(w, http.StatusConflict, "BLOB_NOT_ATTACHED",
				"the object version named no row to attach the blob to")
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"blob_id":      blobID,
			"access_level": in.AccessLevel,
		})
	})

	// POST /harness/blob-access — set the access level of every attachment
	// of one blob. It is the one lever the publish suite needs to move the
	// state UNDER a confirmation page that was already previewed: the page's
	// preview is computed, the level changes, and the publish's own
	// re-check inside its transaction is what has to catch it.
	//
	// blob_attachments is not append-only (migration 00014 guards releases,
	// research_asset_versions, asset_lineage, state_commits and
	// policy_versions), and the column's own CHECK admits exactly the two
	// values (00008).
	mux.HandleFunc("POST /harness/blob-access", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			BlobID      string `json:"blob_id"`
			AccessLevel string `json:"access_level"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "the request body is not JSON")
			return
		}
		if in.BlobID == "" || in.AccessLevel == "" {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "blob_id and access_level are required")
			return
		}
		tag, err := a.pool.Exec(r.Context(),
			`UPDATE blob_attachments SET access_level = $2 WHERE blob_id = $1::uuid`,
			in.BlobID, in.AccessLevel)
		if err != nil {
			writeError(w, http.StatusBadRequest, "ACCESS_NOT_SET", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"blob_id":      in.BlobID,
			"access_level": in.AccessLevel,
			"attachments":  tag.RowsAffected(),
		})
	})

	// POST /harness/state — a project state row in a project of the
	// caller's choosing, and the canonical `state:<uuid>` origin ref that
	// names it.
	//
	// A publication must pin a source: internal/assets' gate refuses a
	// candidate with no release or state origin ref
	// (ASSET_UNPINNED_SOURCE, docs/11 §3), and a ref into ANOTHER project's
	// private state is a private dependency that blocks a public
	// publication. So a fixture version stored in a PUBLIC project needs a
	// state in that project, and this route is the substrate for it — the
	// same kind of fixture as the organization, the project rows and the
	// blobs below. Creating one through the product would mean a branch, an
	// object, a research pull request, a review and a merge, which is a
	// chain this suite already runs once and which says nothing about the
	// publish page.
	//
	// project_states is append-only (migration 00014 guards UPDATE and
	// DELETE; 00015 guards TRUNCATE), so the insert below is the whole
	// interaction. branch_id is nullable (00004): a state that no branch
	// pointed at yet is a state the preview resolves all the same
	// (ListPreviewStateRefs joins projects, not branches).
	mux.HandleFunc("POST /harness/state", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ProjectID string `json:"project_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "the request body is not JSON")
			return
		}
		if in.ProjectID == "" {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "project_id is required")
			return
		}
		var exists bool
		if err := a.pool.QueryRow(r.Context(),
			`SELECT EXISTS(SELECT 1 FROM projects WHERE id = $1::uuid)`,
			in.ProjectID).Scan(&exists); err != nil || !exists {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST",
				"project_id does not name a stored project")
			return
		}
		var stateID string
		if err := a.pool.QueryRow(r.Context(), `
			INSERT INTO project_states (project_id, state_hash, manifest_version)
			VALUES ($1::uuid, $2, '1')
			RETURNING id::text`,
			in.ProjectID, "harness-state-"+strconv.FormatInt(time.Now().UnixNano(), 10),
		).Scan(&stateID); err != nil {
			writeError(w, http.StatusConflict, "STATE_NOT_CREATED", err.Error())
			return
		}
		ref, ok := assets.NewOriginRef(assets.KindState, stateID)
		if !ok {
			writeError(w, http.StatusInternalServerError, "REF_NOT_BUILT",
				"the stored state id is not a uuid")
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"state_id": stateID,
			"ref":      string(ref),
		})
	})

	// GET /harness/review-lineage?state_id= — the release gate's review
	// record, as the query behind it computes it: the ancestors of one
	// state along parent_state_id, the research PRs that proposed any of
	// them against main, and the reviews those PRs carry. READ-ONLY, and
	// not an assertion: it exists so a refusal of the release create can be
	// explained by the same facts the API used, in the suite's own log,
	// instead of by reasoning about a database nobody kept. The subset is
	// deliberate — the three questions are the ones the release gate's
	// release_review check turns on.
	mux.HandleFunc("GET /harness/review-lineage", func(w http.ResponseWriter, r *http.Request) {
		stateID := r.URL.Query().Get("state_id")
		if stateID == "" {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "state_id is required")
			return
		}
		var mainBranchID string
		if err := a.pool.QueryRow(r.Context(),
			`SELECT id::text FROM branches WHERE project_id = (SELECT project_id FROM project_states WHERE id = $1::uuid) AND name = 'main'`,
			stateID).Scan(&mainBranchID); err != nil {
			writeError(w, http.StatusBadRequest, "ROW_READ_FAILED", err.Error())
			return
		}
		rows, err := a.pool.Query(r.Context(), `
			WITH RECURSIVE lineage(id) AS (
			  SELECT ps.id FROM project_states ps WHERE ps.id = $1::uuid
			  UNION
			  SELECT ps.parent_state_id FROM project_states ps
			  JOIN lineage l ON ps.id = l.id
			  WHERE ps.parent_state_id IS NOT NULL
			)
			SELECT l.id::text,
			       COALESCE(pr.number, 0),
			       COALESCE(pr.proposed_state_id::text, ''),
			       COALESCE(pr.target_branch_id = $2::uuid, false),
			       COALESCE((SELECT count(*) FROM reviews r WHERE r.pull_request_id = pr.id), 0)
			  FROM lineage l
			  LEFT JOIN pull_requests pr ON pr.proposed_state_id = l.id
			 ORDER BY 2, 1`, stateID, mainBranchID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "ROW_READ_FAILED", err.Error())
			return
		}
		defer rows.Close()
		type ancestor struct {
			StateID       string `json:"state_id"`
			PRNumber      int64  `json:"pull_request_number"`
			ProposedState string `json:"proposed_state_id"`
			TargetsMain   bool   `json:"targets_main"`
			Reviews       int64  `json:"reviews"`
		}
		out := []ancestor{}
		for rows.Next() {
			var x ancestor
			if err := rows.Scan(&x.StateID, &x.PRNumber, &x.ProposedState, &x.TargetsMain, &x.Reviews); err != nil {
				writeError(w, http.StatusBadRequest, "ROW_READ_FAILED", err.Error())
				return
			}
			out = append(out, x)
		}
		if err := rows.Err(); err != nil {
			writeError(w, http.StatusBadRequest, "ROW_READ_FAILED", err.Error())
			return
		}
		// The count the gate's release_review check is computed from: the
		// reviews of the main-targeting PRs whose proposal is an ancestor.
		var matching int64
		for _, x := range out {
			if x.TargetsMain && x.PRNumber > 0 {
				matching += x.Reviews
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"state_id":         stateID,
			"main_branch_id":   mainBranchID,
			"ancestors":        out,
			"matching_reviews": matching,
		})
	})

	// GET /harness/requests — what the server was asked, including the
	// preflights the browser sent.
	mux.HandleFunc("GET /harness/requests", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"requests": a.log.list()})
	})
}

// queryDefault returns the query parameter's value, or def when it is absent
// or empty. It keeps a fixture route's defaults in one place.
func queryDefault(q url.Values, key, def string) string {
	if v := strings.TrimSpace(q.Get(key)); v != "" {
		return v
	}
	return def
}

// queryList splits a comma-separated query parameter, dropping empty entries
// so a caller can pass "" to mean "the default" and "a,b," to mean two items.
func queryList(q url.Values, key string, def []string) []string {
	raw := strings.TrimSpace(q.Get(key))
	if raw == "" {
		return def
	}
	out := make([]string, 0, 2)
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

// rowText reads one row as text; ok is false when no row matched.
func (a *api) rowText(r *http.Request, query string, args ...any) (any, error) {
	var text string
	err := a.pool.QueryRow(r.Context(), query, args...).Scan(&text)
	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, nil
		}
		return nil, err
	}
	return text, nil
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/auth/signup", stringReader(body))
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

func stringReader(s string) io.Reader { return strings.NewReader(s) }
