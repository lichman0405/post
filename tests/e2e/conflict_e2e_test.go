// The Scientific Conflict Resolution e2e (T0407): the resolution journey
// the way a browser/API client drives it — real HTTP servers and clients,
// the production guard + middleware + handlers (cmd/api/authhttp,
// cmd/api/projectshttp, cmd/api/conflicthttp), real PostgreSQL (the
// per-run namespaced test database, migrated to head including 00054) and
// real stores throughout: the conflict report, the evidence read and the
// resolution plan all run on the production graph, never on mocks. Only
// Redis is in-process (miniredis speaking the real protocol, the same
// trade the auth e2e makes).
//
// PostgreSQL is a requirement this one file adds to the package: when no
// database is reachable the test skips loudly (the same infra-conditional
// pattern as the gitea integration tests) so the infra-free unit gate
// stays green, and runs the full journey wherever PostgreSQL is up.
//
// T0407-TEST "conflict e2e" (blocking):
//
//	go test ./tests/e2e -run TestE2EConflictResolution -count=1
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/conflicthttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/diffs"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/resolutions"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/states"
	appvalidation "github.com/lichman0405/post/internal/application/validation"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

const conflictTaskID = "T0407"

// conflictAdminURL resolves the PostgreSQL maintenance database for the
// namespaced test database, the same variable + default the integration
// suite and `make test-integration` use.
func conflictAdminURL() string {
	if u := os.Getenv("POSTGRES_TEST_ADMIN_URL"); u != "" {
		return u
	}
	return "postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post"
}

// pgReachable probes the maintenance database; the skip is the
// infra-conditional pattern of the gitea integration tests.
func pgReachable(u string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, u)
	if err != nil {
		return false
	}
	defer pool.Close()
	return pool.Ping(ctx) == nil
}

// conflictEnv is one composed deployment: guarded API server (real
// handlers, real PostgreSQL) + Redis, seeded with one private project
// whose two branches diverged on a protocol — a real scientific conflict
// the surface resolves (the T0405 acceptance shape).
type conflictEnv struct {
	api                  *httptest.Server
	client               *http.Client // cookie jar holds alice's session
	users                *memstore.Users
	pool                 *pgxpool.Pool
	alice                domain.User
	project              domain.Project
	base, source, target string
	objectID             string
	csrf                 string
}

// newConflictEnv mirrors cmd/api main.go's wiring: same Deps construction,
// same guard + subtree composition — the storage is the real PostgreSQL
// plus miniredis for sessions. The memstore users are seeded with the SAME
// ids the PostgreSQL users carry, so the authenticated principal
// references a real users row (the resolutions store's decided_by FK).
func newConflictEnv(t *testing.T, ctx context.Context) *conflictEnv {
	t.Helper()
	if !pgReachable(conflictAdminURL()) {
		t.Skipf("conflict e2e: PostgreSQL unreachable at %s — the resolution journey needs a real database; "+
			"start the stack (make infra-up) or set POSTGRES_TEST_ADMIN_URL and the test runs", conflictAdminURL())
	}
	pool, _ := testdb.Setup(t, ctx, conflictAdminURL(), conflictTaskID)

	// alice: the project owner, seeded in PostgreSQL and mirrored into the
	// auth memstore under the same id.
	password := "long-enough-password-1"
	hash, err := authn.HashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	alice, err := persistence.NewCredentialStore(pool).CreateWithPassword(
		ctx, "conflict-alice@example.com", hash, "conflict-alice", "Alice")
	if err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	users := memstore.NewUsers()
	users.Seed(authn.UserRecord{User: alice, PasswordHash: hash})

	mr := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	authAPI := authhttp.New(authhttp.Deps{
		Users:      users,
		Sessions:   persistence.NewRedisSessionStore(redisClient),
		Limiter:    persistence.NewRedisRateLimiter(redisClient),
		OIDCClient: nil,
		Cfg:        defaultCfg(),
		Secure:     false,
	})

	// The project over the real stores, the rsg write path that seeds the
	// diverged branches, and the conflict surface over the real graph —
	// the composition cmd/api/main.go uses.
	orgStore := persistence.NewOrgStore(pool)
	org, _, err := orgStore.CreateOrganization(ctx, domain.Organization{
		Slug: "conflict-e2e", Name: "Conflict E2E",
	}, alice.ID, affiliationToday())
	if err != nil {
		t.Fatalf("create fixture org: %v", err)
	}
	projectStore := persistence.NewProjectStore(pool)
	project, _, err := projectStore.CreateProject(ctx, domain.Project{
		OrganizationID:  &org.ID,
		Slug:            "conflict-project",
		Name:            "Conflict Project",
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
	statesSvc := states.NewService(stateStore,
		appvalidation.NewGuard(rsgvalidation.NewValidator(reg), persistence.NewValidationTxProbe()))
	projectsSvc := projects.NewService(projectStore, orgStore, authz.NewMatrixEngine())
	rsgSvc := rsg.NewService(rsg.Deps{
		Projects:  projectsSvc,
		Branches:  branches.NewService(persistence.NewBranchStore(pool)),
		States:    statesSvc,
		Latest:    stateStore,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
		Events:    events.Recorder{},
	})

	// Seed the conflict: main carries a protocol, feature forks it, both
	// branches move the same step to different temperatures — the
	// SCIENTIFIC_FIELD_DIVERGES verdict, never auto-mergeable.
	main, err := rsgSvc.CreateBranch(ctx, alice, project.ID, rsg.CreateBranchInput{
		Name:       "main",
		BaseRef:    "",
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create main: %v", err)
	}
	pObject, err := rsgSvc.CreateObject(ctx, alice, project.ID, main.ID, rsg.CreateObjectInput{
		ObjectType: "protocol",
		Payload:    json.RawMessage(`{"purpose":"synthesize","steps":[{"id":"s1","temperature":300}]}`),
	})
	if err != nil {
		t.Fatalf("create protocol: %v", err)
	}
	// feature forks main's CURRENT head (the state carrying the protocol)
	// — that state is the merge base, like the T0405 fixture.
	mainHead, err := statesSvc.GetBranchHead(ctx, main.ID)
	if err != nil {
		t.Fatalf("main head before fork: %v", err)
	}
	feature, err := rsgSvc.CreateBranch(ctx, alice, project.ID, rsg.CreateBranchInput{
		Name:       "feature",
		BaseRef:    mainHead.ID,
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create feature: %v", err)
	}
	base := *feature.BaseStateID
	if _, err := rsgSvc.CreateObjectVersion(ctx, alice, project.ID, feature.ID, pObject.Object.ID, rsg.CreateObjectVersionInput{
		ExpectedVersion: 1,
		Patch:           json.RawMessage(`{"purpose":"synthesize","steps":[{"id":"s1","temperature":350}]}`),
	}); err != nil {
		t.Fatalf("feature protocol update: %v", err)
	}
	if _, err := rsgSvc.CreateObjectVersion(ctx, alice, project.ID, main.ID, pObject.Object.ID, rsg.CreateObjectVersionInput{
		ExpectedVersion: 2,
		Patch:           json.RawMessage(`{"purpose":"synthesize","steps":[{"id":"s1","temperature":400}]}`),
	}); err != nil {
		t.Fatalf("main protocol update: %v", err)
	}
	sourceHead, err := statesSvc.GetBranchHead(ctx, feature.ID)
	if err != nil {
		t.Fatalf("feature head: %v", err)
	}
	targetHead, err := statesSvc.GetBranchHead(ctx, main.ID)
	if err != nil {
		t.Fatalf("main head: %v", err)
	}

	// The guarded v1 subtree: projects + conflicts, exactly as main.go
	// composes them.
	resolutionSvc := resolutions.NewService(
		diffs.NewService(stateStore, persistence.NewManifestStore(pool), persistence.NewPullRequestStore(pool)),
		resolutions.NewPGStore(pool),
		projectsSvc,
		authz.NewMatrixEngine(),
	)
	v1 := http.NewServeMux()
	authAPI.Register(v1) // auth routes ride the guarded subtree, as in main.go
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store:         projectStore,
		Orgs:          orgStore,
		Authz:         authz.NewMatrixEngine(),
		ProvisionJobs: nil,
	})
	v1.Handle("/api/v1/projects", projectAPI.Routes())
	v1.Handle("/api/v1/projects/", projectAPI.Routes())
	conflictAPI := conflicthttp.New(conflicthttp.Deps{
		Viewer:   resolutionSvc,
		Saver:    resolutionSvc,
		Projects: projectsSvc,
	})
	conflictAPI.Register(v1)

	mux := http.NewServeMux()
	mux.Handle("/api/v1/", authAPI.Guard(v1))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	client := &http.Client{Jar: jar}

	env := &conflictEnv{
		api: server, client: client, users: users, pool: pool,
		alice: alice, project: project,
		base: base, source: sourceHead.ID, target: targetHead.ID,
		objectID: pObject.Object.ID,
	}

	// Sign in through the real login route, then read the session-bound
	// CSRF token the guard demands on writes.
	env.login(t, "conflict-alice@example.com", password)
	env.refreshCSRF(t)
	return env
}

// affiliationToday is the day the fixtures' memberships start on: "today"
// asked of the one place that defines it (domain.AffiliationDay).
//
// It used to read time.Now()'s LOCAL year/month/day and label it UTC. On a
// UTC+8 host from 00:00 to 08:00 local that day is still the FUTURE in UTC, so
// the creator's membership read as not-yet-active and the fixture's project
// creation was refused ("projects: not allowed") — the date-convention bug
// T0816 fixed in production (internal/domain/affiliation.go, orgs.today) and in
// the integration fixtures, missed in this package. Green on CI (UTC) and after
// 08:00 local; red in the first hours of a UTC+8 morning.
func affiliationToday() time.Time { return domain.AffiliationDay(time.Now()) }

// do issues one request through the env's cookie jar (JSON content type
// implied by a non-empty body, like the auth e2e client).
func (e *conflictEnv) do(t *testing.T, method, path, body string, headers map[string]string) *http.Response {
	t.Helper()
	return doWith(t, e.client, e.api.URL+path, method, body, headers)
}

// doWith issues one request through the given client.
func doWith(t *testing.T, client *http.Client, fullURL, method, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, fullURL, strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, fullURL, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// login performs the real sign-in; the cookie lands in the env's jar.
func (e *conflictEnv) login(t *testing.T, email, password string) {
	t.Helper()
	resp := e.do(t, http.MethodPost, "/api/v1/auth/login",
		fmt.Sprintf(`{"email":%q,"password":%q}`, email, password), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d: %s", resp.StatusCode, bodyBytes(t, resp))
	}
}

// refreshCSRF reads the session endpoint for the CSRF token.
func (e *conflictEnv) refreshCSRF(t *testing.T) {
	t.Helper()
	resp := e.do(t, http.MethodGet, "/api/v1/auth/session", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session = %d: %s", resp.StatusCode, bodyBytes(t, resp))
	}
	var sess struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(bodyBytes(t, resp), &sess); err != nil || sess.CSRFToken == "" {
		t.Fatalf("session payload: %s", bodyBytes(t, resp))
	}
	e.csrf = sess.CSRFToken
}

// conflictsPath renders the GET conflicts URL for the seeded triple.
func (e *conflictEnv) conflictsPath() string {
	return fmt.Sprintf("/api/v1/projects/%s/conflicts?base_state_id=%s&source_state_id=%s&target_state_id=%s",
		e.project.ID, e.base, e.source, e.target)
}

// conflictView is the wire shape of the GET conflicts answer (the fields
// the test asserts on).
type conflictView struct {
	Report struct {
		AutoMergeable  bool `json:"auto_mergeable"`
		ObjectVerdicts []struct {
			ObjectID      string `json:"object_id"`
			AutoMergeable bool   `json:"auto_mergeable"`
			Conflicts     []struct {
				Category    string   `json:"category"`
				Code        string   `json:"code"`
				Fields      []string `json:"fields"`
				PayloadKeys []string `json:"payload_keys"`
				Detail      string   `json:"detail"`
			} `json:"conflicts"`
		} `json:"object_verdicts"`
	} `json:"report"`
	Evidence []struct {
		ObjectID       string `json:"object_id"`
		SourceEvidence []struct {
			EvidenceObjectID string `json:"evidence_object_id"`
		} `json:"source_evidence"`
		TargetEvidence []struct {
			EvidenceObjectID string `json:"evidence_object_id"`
		} `json:"target_evidence"`
	} `json:"evidence"`
	Resolutions []struct {
		TargetID  string `json:"target_id"`
		Code      string `json:"code"`
		Kind      string `json:"kind"`
		Note      string `json:"note"`
		DecidedBy string `json:"decided_by"`
	} `json:"resolutions"`
}

// getConflicts reads the view through the HTTP surface.
func (e *conflictEnv) getConflicts(t *testing.T) (int, conflictView) {
	t.Helper()
	resp := e.do(t, http.MethodGet, e.conflictsPath(), "", nil)
	var view conflictView
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(bodyBytes(t, resp), &view); err != nil {
			t.Fatalf("decode conflicts: %v", err)
		}
	}
	return resp.StatusCode, view
}

// resolutionBody renders a PUT resolutions body for one decision.
func (e *conflictEnv) resolutionBody(kind, note string) string {
	return fmt.Sprintf(`{
		"base_state_id": %q, "source_state_id": %q, "target_state_id": %q,
		"resolutions": [{
			"target_kind": "object", "target_id": %q,
			"code": "SCIENTIFIC_FIELD_DIVERGES",
			"fields": ["payload"], "payload_keys": ["steps"],
			"other_object_id": null,
			"kind": %q, "note": %q
		}]
	}`, e.base, e.source, e.target, e.objectID, kind, note)
}

// saveResolutions PUTs a body as alice and returns the response.
func (e *conflictEnv) saveResolutions(t *testing.T, body string) *http.Response {
	t.Helper()
	return e.do(t, http.MethodPut, "/api/v1/projects/"+e.project.ID+"/resolutions", body,
		map[string]string{"X-CSRF-Token": e.csrf})
}

// TestE2EConflictResolution is the required "conflict e2e": the full
// resolution journey over the production graph —
//
//  1. the conflict view renders the scientific conflict (category
//     scientific, code SCIENTIFIC_FIELD_DIVERGES, payload key steps) with
//     the advisory detail text and the per-object evidence context;
//  2. a human decision (accept_source) is recorded through the guarded
//     PUT, lands in PostgreSQL, and the view carries it afterwards; an
//     overwrite replaces it in place (one row, latest wins);
//  3. a decision naming a conflict the report does not contain is
//     refused with CONFLICT_NOT_FOUND;
//  4. a kind outside the five human decisions ("average") is refused —
//     there is no computed outcome anywhere on the wire;
//  5. the guard refuses the write without the session-bound CSRF token,
//     and refuses anonymous writes with 401;
//  6. an anonymous caller cannot read the private project's conflicts
//     (existence-hiding 404), and a missing triple is 400;
//  7. a viewer's write is refused (AUTH_FORBIDDEN) while the viewer's
//     read flows;
//  8. the audit log records each saved plan naming the human who decided.
func TestE2EConflictResolution(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	e := newConflictEnv(t, ctx)

	// 1. The view renders the conflict.
	status, view := e.getConflicts(t)
	if status != http.StatusOK {
		t.Fatalf("GET conflicts = %d: %s", status, bodyBytes(t, e.do(t, http.MethodGet, e.conflictsPath(), "", nil)))
	}
	if view.Report.AutoMergeable {
		t.Fatalf("report auto_mergeable = true, verdicts = %+v", view.Report.ObjectVerdicts)
	}
	if len(view.Report.ObjectVerdicts) != 1 || len(view.Report.ObjectVerdicts[0].Conflicts) != 1 {
		t.Fatalf("object verdicts = %+v, want exactly one conflict", view.Report.ObjectVerdicts)
	}
	c := view.Report.ObjectVerdicts[0].Conflicts[0]
	if c.Category != "scientific" || c.Code != "SCIENTIFIC_FIELD_DIVERGES" {
		t.Fatalf("conflict = %+v, want scientific/SCIENTIFIC_FIELD_DIVERGES", c)
	}
	if len(c.PayloadKeys) != 1 || c.PayloadKeys[0] != "steps" {
		t.Fatalf("payload_keys = %v, want [steps]", c.PayloadKeys)
	}
	if c.Detail == "" {
		t.Fatal("the advisory detail text must render with the conflict")
	}
	if len(view.Evidence) != 1 || view.Evidence[0].ObjectID != e.objectID {
		t.Fatalf("evidence context = %+v, want exactly the conflicted object", view.Evidence)
	}
	if len(view.Resolutions) != 0 {
		t.Fatalf("resolutions = %+v, want none before any decision", view.Resolutions)
	}

	// A missing triple is refused, not guessed.
	resp := e.do(t, http.MethodGet, "/api/v1/projects/"+e.project.ID+"/conflicts", "", nil)
	if resp.StatusCode != http.StatusBadRequest || decodeEnvelope(t, resp).Code != resolutions.CodeValidation {
		t.Fatalf("GET without triple = %d (%s), want 400 VALIDATION_FAILED", resp.StatusCode, bodyBytes(t, resp))
	}

	// 2. The human decision records through the guarded PUT.
	resp = e.saveResolutions(t, e.resolutionBody("accept_source", "accept the feature branch protocol"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT resolutions = %d: %s", resp.StatusCode, bodyBytes(t, resp))
	}
	var plan struct {
		Resolutions []struct {
			Kind      string `json:"kind"`
			TargetID  string `json:"target_id"`
			DecidedBy string `json:"decided_by"`
		} `json:"resolutions"`
	}
	if err := json.Unmarshal(bodyBytes(t, resp), &plan); err != nil {
		t.Fatalf("decode plan: %v", err)
	}
	if len(plan.Resolutions) != 1 || plan.Resolutions[0].Kind != "accept_source" ||
		plan.Resolutions[0].TargetID != e.objectID || plan.Resolutions[0].DecidedBy != e.alice.ID {
		t.Fatalf("plan = %+v, want one accept_source decision by alice", plan.Resolutions)
	}

	// The view now carries the decision.
	_, view = e.getConflicts(t)
	if len(view.Resolutions) != 1 || view.Resolutions[0].Kind != "accept_source" {
		t.Fatalf("view resolutions after save = %+v", view.Resolutions)
	}

	// An overwrite replaces the decision in place (one row, latest wins).
	resp = e.saveResolutions(t, e.resolutionBody("unresolved", ""))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT overwrite = %d: %s", resp.StatusCode, bodyBytes(t, resp))
	}
	_, view = e.getConflicts(t)
	if len(view.Resolutions) != 1 || view.Resolutions[0].Kind != "unresolved" {
		t.Fatalf("view resolutions after overwrite = %+v", view.Resolutions)
	}
	var count int
	if err := e.pool.QueryRow(ctx,
		"SELECT count(*) FROM conflict_resolutions WHERE project_id = $1", e.project.ID).Scan(&count); err != nil {
		t.Fatalf("count resolutions: %v", err)
	}
	if count != 1 {
		t.Fatalf("resolution rows = %d, want 1 (overwrite, not append)", count)
	}

	// 3. A decision naming a conflict the report does not contain: 409.
	unknown := fmt.Sprintf(`{
		"base_state_id": %q, "source_state_id": %q, "target_state_id": %q,
		"resolutions": [{
			"target_kind": "object", "target_id": "00000000-0000-4000-8000-000000000999",
			"code": "SCIENTIFIC_FIELD_DIVERGES", "fields": ["payload"], "payload_keys": ["steps"],
			"other_object_id": null, "kind": "accept_target", "note": ""
		}]
	}`, e.base, e.source, e.target)
	resp = e.saveResolutions(t, unknown)
	if resp.StatusCode != http.StatusConflict || decodeEnvelope(t, resp).Code != resolutions.CodeConflictNotFound {
		t.Fatalf("PUT unknown conflict = %d (%s), want 409 CONFLICT_NOT_FOUND", resp.StatusCode, bodyBytes(t, resp))
	}

	// 4. No computed outcome: a kind outside the five human decisions is
	// refused as validation, never stored.
	resp = e.saveResolutions(t, e.resolutionBody("average", ""))
	if resp.StatusCode != http.StatusBadRequest || decodeEnvelope(t, resp).Code != resolutions.CodeValidation {
		t.Fatalf("PUT average kind = %d (%s), want 400 VALIDATION_FAILED", resp.StatusCode, bodyBytes(t, resp))
	}

	// 5. The guard refuses writes without the CSRF token and anonymous
	// writes.
	resp = e.do(t, http.MethodPut, "/api/v1/projects/"+e.project.ID+"/resolutions",
		e.resolutionBody("accept_source", ""), nil)
	if resp.StatusCode != http.StatusForbidden || decodeEnvelope(t, resp).Code != authn.CodeCSRFFailed {
		t.Fatalf("PUT without CSRF = %d (%s), want 403 CSRF_FAILED", resp.StatusCode, bodyBytes(t, resp))
	}
	anonClient := &http.Client{}
	anonResp := doWith(t, anonClient, e.api.URL+"/api/v1/projects/"+e.project.ID+"/resolutions",
		http.MethodPut, e.resolutionBody("accept_source", ""), nil)
	if anonResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous PUT = %d, want 401", anonResp.StatusCode)
	}

	// 6. Anonymous read of the private project: existence-hiding 404.
	anonResp = doWith(t, anonClient, e.api.URL+e.conflictsPath(), http.MethodGet, "", nil)
	if anonResp.StatusCode != http.StatusNotFound {
		t.Fatalf("anonymous GET conflicts = %d, want 404", anonResp.StatusCode)
	}

	// 7. A viewer may read but not decide.
	viewerPassword := "viewer-password-123"
	viewerHash, err := authn.HashPassword(viewerPassword)
	if err != nil {
		t.Fatalf("hash viewer password: %v", err)
	}
	viewer, err := persistence.NewCredentialStore(e.pool).CreateWithPassword(
		ctx, "conflict-bob@example.com", viewerHash, "conflict-bob", "Bob")
	if err != nil {
		t.Fatalf("seed bob: %v", err)
	}
	if _, err := e.pool.Exec(ctx,
		"INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, 'viewer')",
		e.project.ID, viewer.ID); err != nil {
		t.Fatalf("seed bob membership: %v", err)
	}
	e.users.Seed(authn.UserRecord{User: viewer, PasswordHash: viewerHash})
	bobJar, _ := cookiejar.New(nil)
	bobClient := &http.Client{Jar: bobJar}
	bobLogin := doWith(t, bobClient, e.api.URL+"/api/v1/auth/login", http.MethodPost,
		`{"email":"conflict-bob@example.com","password":"viewer-password-123"}`, nil)
	if bobLogin.StatusCode != http.StatusOK {
		t.Fatalf("bob login = %d: %s", bobLogin.StatusCode, bodyBytes(t, bobLogin))
	}
	bobSession := doWith(t, bobClient, e.api.URL+"/api/v1/auth/session", http.MethodGet, "", nil)
	var bobSess struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(bodyBytes(t, bobSession), &bobSess); err != nil || bobSess.CSRFToken == "" {
		t.Fatalf("bob session payload: %s", bodyBytes(t, bobSession))
	}
	bobRead := doWith(t, bobClient, e.api.URL+e.conflictsPath(), http.MethodGet, "", nil)
	if bobRead.StatusCode != http.StatusOK {
		t.Fatalf("viewer GET conflicts = %d, want 200", bobRead.StatusCode)
	}
	bobWrite := doWith(t, bobClient, e.api.URL+"/api/v1/projects/"+e.project.ID+"/resolutions",
		http.MethodPut, e.resolutionBody("accept_target", ""),
		map[string]string{"X-CSRF-Token": bobSess.CSRFToken})
	if bobWrite.StatusCode != http.StatusForbidden || decodeEnvelope(t, bobWrite).Code != resolutions.CodeForbidden {
		t.Fatalf("viewer PUT = %d (%s), want 403 AUTH_FORBIDDEN", bobWrite.StatusCode, bodyBytes(t, bobWrite))
	}

	// 8. The audit log records each saved plan naming the human who
	// decided (docs/60: the record that a human, not an agent, decided).
	var auditCount int
	if err := e.pool.QueryRow(ctx,
		"SELECT count(*) FROM audit_log WHERE action = 'conflict.resolution_saved' AND project_id = $1",
		e.project.ID).Scan(&auditCount); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if auditCount != 2 { // the accept_source save and the unresolved overwrite
		t.Fatalf("audit rows = %d, want 2 (one per save)", auditCount)
	}
	var auditActor string
	if err := e.pool.QueryRow(ctx,
		"SELECT actor_id::text FROM audit_log WHERE action = 'conflict.resolution_saved' AND project_id = $1 ORDER BY occurred_at DESC LIMIT 1",
		e.project.ID).Scan(&auditActor); err != nil {
		t.Fatalf("read audit actor: %v", err)
	}
	if auditActor != e.alice.ID {
		t.Fatalf("audit actor = %s, want alice %s (human-governed)", auditActor, e.alice.ID)
	}
}
