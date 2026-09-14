// Package integration — T0212 "overview e2e": the project overview page
// exercised end to end over REAL PostgreSQL, the real auth guard, the
// real RSG service and the real HTTP surface — signup, project create,
// branch create, question/hypothesis/finding/claim/relation creates, then
// the browser read of the overview page itself. The acceptance criteria
// ride on the negatives first: the first screen must explain what the
// project is doing (purpose + key questions/findings — never a files
// tree), questions must drill down to their detail pages, the attention
// list must carry the graph's own state signals, the current-main section
// must show the accepted state, the empty state must guide the
// agent/project start with pre-filled tool references, an invisible
// project's overview must answer the existence-hidden not-found page with
// no chrome, and hostile payload text must render escaped.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/cmd/api/rsghttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/application/validation"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

const overviewPageTaskID = "T0212"

// overviewPageFixture is the full production composition (the same wiring
// cmd/api/main.go builds) plus the seeded research state: alice owns a
// private project with a two-level question tree, an accepted finding, a
// contested finding, a hypothesis, a claim and a second active branch;
// the empty project carries nothing at all (the start-guide case); the
// public project carries a hostile purpose. bob is a member of nothing.
type overviewPageFixture struct {
	ts *httptest.Server

	alice *testUserClient
	bob   *testUserClient
	anon  *testUserClient

	privateProjectID  string
	privateBranchID   string
	rootQuestionID    string
	childQuestionID   string
	findingID         string
	contestedID       string
	featureBranchID   string
	featureBranchName string

	emptyProjectID  string
	publicProjectID string
}

// page fetches a route with a browser Accept header.
func (f *overviewPageFixture) page(t *testing.T, uc *testUserClient, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/html")
	resp, err := uc.client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (f *overviewPageFixture) overviewPath(projectID string) string {
	return "/api/v1/projects/" + projectID + "/overview"
}

type overviewObjResponse struct {
	ID        string `json:"id"`
	VersionID string `json:"version_id"`
	VersionNo int    `json:"version_no"`
}

// createObject creates one object over the wire and returns its identity.
func (f *overviewPageFixture) createObject(t *testing.T, projectID, branchID, objectType, payload string) overviewObjResponse {
	t.Helper()
	resp := f.alice.do(t, http.MethodPost,
		"/api/v1/projects/"+projectID+"/branches/"+branchID+"/objects",
		`{"object_type":"`+objectType+`","payload":`+payload+`}`)
	mustStatus(t, resp, http.StatusCreated)
	var obj overviewObjResponse
	if err := json.NewDecoder(resp.Body).Decode(&obj); err != nil {
		t.Fatalf("create %s payload: %v", objectType, err)
	}
	return obj
}

// createBranch creates one branch over the wire and returns its identity.
func (f *overviewPageFixture) createBranch(t *testing.T, projectID, name, purpose string) string {
	t.Helper()
	body := `{"name":"` + name + `","base_ref":"","visibility":"private"`
	if purpose != "" {
		body += `,"purpose":"` + purpose + `"`
	}
	body += `}`
	resp := f.alice.do(t, http.MethodPost, "/api/v1/projects/"+projectID+"/branches", body)
	mustStatus(t, resp, http.StatusCreated)
	var b struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		t.Fatalf("create branch payload: %v", err)
	}
	return b.ID
}

func newOverviewPageFixture(t *testing.T, ctx context.Context) *overviewPageFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), overviewPageTaskID)

	// --- composition (identical to cmd/api/main.go) ---
	sessions := memstore.NewSessions()
	limiter := memstore.NewLimiter()
	cfg := authn.Config{
		WebOrigin:          "http://web.test",
		SessionTTL:         time.Hour,
		LoginLimitPerEmail: 1000,
		LoginLimitPerIP:    10000,
		LoginWindow:        time.Minute,
		SignupLimitPerIP:   10000,
	}
	authAPI := authhttp.New(authhttp.Deps{
		Users:      persistence.NewCredentialStore(pool),
		Sessions:   sessions,
		Limiter:    limiter,
		OIDCClient: nil,
		Cfg:        cfg,
		Secure:     false,
	})
	orgStore := persistence.NewOrgStore(pool)
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: persistence.NewProjectStore(pool),
		Orgs:  orgStore,
		Authz: authz.NewMatrixEngine(),
	})
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	stateStore := persistence.NewStateStore(pool)
	rsgSvc := rsg.NewService(rsg.Deps{
		Projects:  projectAPI.Service(),
		Branches:  branches.NewService(persistence.NewBranchStore(pool)),
		States:    states.NewService(stateStore, validation.NewGuard(rsgvalidation.NewValidator(reg), persistence.NewValidationTxProbe())),
		Latest:    stateStore,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Queries:   persistence.NewRSGQueryStore(pool),
		Profiles:  persistence.NewProfileStore(pool),
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
		Events:    events.Recorder{},
	})
	rsgAPI := rsghttp.New(rsghttp.Deps{Service: rsgSvc})
	apiMux := http.NewServeMux()
	apiMux.Handle("/api/v1/auth/", authAPI.Routes())
	apiMux.Handle("/api/v1/projects", projectAPI.Routes())
	apiMux.Handle("/api/v1/projects/", projectAPI.Routes())
	rsgAPI.Register(apiMux)
	ts := httptest.NewServer(authAPI.Guard(apiMux))
	t.Cleanup(ts.Close)

	alice, _ := signup(t, ts.URL, "overview-alice@example.com", "overview-alice")
	bob, _ := signup(t, ts.URL, "overview-bob@example.com", "overview-bob")
	anon := newTestUserClient(ts.URL)

	f := &overviewPageFixture{ts: ts, alice: alice, bob: bob, anon: anon}

	// --- the private project and its research state ---
	resp := alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"overview-lab","name":"Overview Lab","purpose":"Screen MOFs for CO2 capture under humid conditions","visibility":"private"}`)
	mustStatus(t, resp, http.StatusCreated)
	var privateProject projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&privateProject); err != nil {
		t.Fatalf("create private project payload: %v", err)
	}
	f.privateProjectID = privateProject.Project.ID

	f.privateBranchID = f.createBranch(t, f.privateProjectID, "main", "")

	// The question tree: a root and an unresolved sub-question.
	rootQ := f.createObject(t, f.privateProjectID, f.privateBranchID, "research_question",
		`{"statement":"Does the material adsorb CO2?","question_state":"open"}`)
	f.rootQuestionID = rootQ.ID
	childQ := f.createObject(t, f.privateProjectID, f.privateBranchID, "research_question",
		`{"statement":"Does it survive humid cycling?","question_state":"unresolved","parent_question_id":"`+rootQ.ID+`"}`)
	f.childQuestionID = childQ.ID

	// A hypothesis and two findings addressing the root question — one
	// accepted, one contested (the attention list's finding signal) — a
	// claim the accepted finding references, and a material addressing
	// the sub-question.
	hyp := f.createObject(t, f.privateProjectID, f.privateBranchID, "hypothesis",
		`{"statement":"Uptake scales with pressure","question_id":"`+rootQ.ID+`"}`)
	claim := f.createObject(t, f.privateProjectID, f.privateBranchID, "claim",
		`{"statement":"Capacity is 2 mmol/g","claim_type":"quantitative"}`)
	finding := f.createObject(t, f.privateProjectID, f.privateBranchID, "finding",
		`{"statement":"Uptake peaks at 30 bar","finding_type":"trend","assessment":"accepted","claim_version_refs":["`+claim.VersionID+`"]}`)
	f.findingID = finding.ID
	contested := f.createObject(t, f.privateProjectID, f.privateBranchID, "finding",
		`{"statement":"Degrades in humid air","finding_type":"trend","assessment":"contested"}`)
	f.contestedID = contested.ID
	mat := f.createObject(t, f.privateProjectID, f.privateBranchID, "material",
		`{"name":"MOF-5"}`)

	for _, edge := range []struct{ source, target string }{
		{hyp.VersionID, rootQ.VersionID},
		{finding.VersionID, rootQ.VersionID},
		{contested.VersionID, rootQ.VersionID},
		{mat.VersionID, childQ.VersionID},
	} {
		resp = alice.do(t, http.MethodPost, "/api/v1/projects/"+f.privateProjectID+"/branches/"+f.privateBranchID+"/relations",
			`{"relation_type":"addresses_question","source_object_version_id":"`+edge.source+`","target_object_version_id":"`+edge.target+`"}`)
		mustStatus(t, resp, http.StatusCreated)
	}

	// A second research path: an active branch with a purpose.
	f.featureBranchID = f.createBranch(t, f.privateProjectID, "feature/humidity", "test humid stability")
	f.featureBranchName = "feature/humidity"

	// --- the empty project (the start-guide case) ---
	resp = alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"overview-empty","name":"Overview Empty","purpose":"A project that has not started","visibility":"private"}`)
	mustStatus(t, resp, http.StatusCreated)
	var emptyProject projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&emptyProject); err != nil {
		t.Fatalf("create empty project payload: %v", err)
	}
	f.emptyProjectID = emptyProject.Project.ID

	// --- the public project: a hostile purpose anyone may read ---
	resp = alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"overview-public","name":"Overview Public","purpose":"Screens <script>alert('xss')</script> materials","visibility":"public"}`)
	mustStatus(t, resp, http.StatusCreated)
	var publicProject projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&publicProject); err != nil {
		t.Fatalf("create public project payload: %v", err)
	}
	f.publicProjectID = publicProject.Project.ID
	publicBranch := f.createBranch(t, f.publicProjectID, "main", "")
	f.createObject(t, f.publicProjectID, publicBranch, "research_question",
		`{"statement":"Does the public material work?","question_state":"open"}`)

	return f
}

// TestOverviewPageE2E is the required "overview e2e" test.
func TestOverviewPageE2E(t *testing.T) {
	ctx := testCtx(t)
	f := newOverviewPageFixture(t, ctx)

	// --- denial first: the overview is exactly as visible as its project ---
	t.Run("private overview hidden from anonymous", func(t *testing.T) {
		resp := f.page(t, f.anon, f.overviewPath(f.privateProjectID))
		mustStatus(t, resp, http.StatusNotFound)
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("Content-Type = %q, want text/html", ct)
		}
		body := readAll(t, resp)
		if !strings.Contains(body, "Not found") {
			t.Errorf("denied page lacks the neutral not-found title")
		}
		for _, chrome := range []string{"data-overview-meta", "data-project-tab", "data-question-id", "Screen MOFs for CO2 capture", "Overview Lab"} {
			if strings.Contains(body, chrome) {
				t.Errorf("denied read renders page chrome (%q) — existence must stay hidden", chrome)
			}
		}
	})

	t.Run("private overview hidden from non-member", func(t *testing.T) {
		resp := f.page(t, f.bob, f.overviewPath(f.privateProjectID))
		mustStatus(t, resp, http.StatusNotFound)
		if body := readAll(t, resp); strings.Contains(body, "Does the material adsorb CO2?") {
			t.Errorf("non-member overview discloses a question")
		}
	})

	t.Run("denied JSON contract unchanged", func(t *testing.T) {
		resp := f.bob.do(t, http.MethodGet, f.overviewPath(f.privateProjectID), "")
		mustStatus(t, resp, http.StatusNotFound)
		mustEnvelope(t, resp, projects.CodeProjectNotFound)
	})

	// --- the first screen, as alice on her private project ---
	t.Run("first screen explains what the project is doing", func(t *testing.T) {
		resp := f.page(t, f.alice, f.overviewPath(f.privateProjectID))
		mustStatus(t, resp, http.StatusOK)
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("Content-Type = %q, want text/html", ct)
		}
		if vary := resp.Header.Get("Vary"); !strings.Contains(vary, "Accept") {
			t.Errorf("Vary = %q, want Accept listed", vary)
		}
		body := readAll(t, resp)
		for _, want := range []string{
			// breadcrumb + header facts
			"overview-lab",
			`data-overview-meta`,
			`data-visibility="private"`,
			`data-overview-counts`,
			// the project tab bar: Overview active, Research linked
			`data-project-tab="overview"`,
			`data-project-tab="research"`,
			// the purpose
			"Screen MOFs for CO2 capture under humid conditions",
			// key questions, with drill-down links
			`data-overview-section="questions"`,
			`data-question-id="` + f.rootQuestionID + `"`,
			"Does the material adsorb CO2?",
			// key findings with their assessment badges
			`data-overview-section="findings"`,
			`data-finding-id="` + f.findingID + `"`,
			"Uptake peaks at 30 bar",
			`data-assessment="accepted"`,
			// the active path
			`data-overview-section="branches"`,
			`data-branch-name="feature/humidity"`,
			"test humid stability",
			// the attention list: the contested finding and the
			// unresolved question — the graph's own signals
			`data-overview-section="attention"`,
			"Degrades in humid air",
			`data-signal="contested"`,
			"Does it survive humid cycling?",
			`data-signal="unresolved"`,
			// current main
			`data-overview-section="main"`,
			`data-main`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("first screen lacks %q", want)
			}
		}
		// The purpose comes before the questions — the first screen leads
		// with what the project is about.
		purposeAt := strings.Index(body, `data-overview-section="purpose"`)
		questionsAt := strings.Index(body, `data-overview-section="questions"`)
		if purposeAt < 0 || questionsAt < 0 || questionsAt < purposeAt {
			t.Errorf("section order wrong: purpose=%d questions=%d", purposeAt, questionsAt)
		}
		// Never a files tree, never a type dump, never an edit form.
		for _, forbidden := range []string{">Files<", "file tree", ">Materials<", ">Datasets<", ">Objects by type<", "<form", "<input", "<textarea", "<button"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("first screen contains %q", forbidden)
			}
		}
	})

	t.Run("key question drills down to its detail page", func(t *testing.T) {
		resp := f.page(t, f.alice, f.overviewPath(f.privateProjectID))
		mustStatus(t, resp, http.StatusOK)
		body := readAll(t, resp)
		detailPath := fmt.Sprintf("/api/v1/projects/%s/branches/%s/objects/%s",
			f.privateProjectID, f.privateBranchID, f.rootQuestionID)
		if !strings.Contains(body, `href="`+detailPath+`"`) {
			t.Errorf("overview lacks the drill-down link %q", detailPath)
		}
		detail := f.page(t, f.alice, detailPath)
		mustStatus(t, detail, http.StatusOK)
		if detailBody := readAll(t, detail); !strings.Contains(detailBody, "Does the material adsorb CO2?") {
			t.Errorf("detail page does not render the question the overview linked to")
		}
	})

	t.Run("current main shows the accepted state", func(t *testing.T) {
		resp := f.page(t, f.alice, f.overviewPath(f.privateProjectID))
		mustStatus(t, resp, http.StatusOK)
		body := readAll(t, resp)
		for _, want := range []string{
			// the newest commit on main is the last relation created
			"create addresses_question relation",
			"via api",
			`data-main-commit`,
			"Head state",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("current main lacks %q", want)
			}
		}
	})

	// --- the empty state: the start guide ---
	t.Run("empty project renders the agent start guide", func(t *testing.T) {
		resp := f.page(t, f.alice, f.overviewPath(f.emptyProjectID))
		mustStatus(t, resp, http.StatusOK)
		body := readAll(t, resp)
		for _, want := range []string{
			`data-overview-guide`,
			"Start this project",
			"A project that has not started",
			`data-agent-tool="branch.create"`,
			`data-agent-tool="object.create"`,
			`data-agent-tool="relation.create"`,
			f.emptyProjectID,
			"research_question",
			"addresses_question",
			// the read-only note
			"read-only",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("start guide lacks %q", want)
			}
		}
		// The guide replaces the summary sections — there is nothing to
		// summarize.
		for _, forbidden := range []string{`data-overview-section="questions"`, `data-overview-section="main"`} {
			if strings.Contains(body, forbidden) {
				t.Errorf("start guide renders summary section %q", forbidden)
			}
		}

		// Once main exists, the guide drops the branch step and pre-fills
		// main's real branch id into the object/relation references.
		mainID := f.createBranch(t, f.emptyProjectID, "main", "")
		after := f.page(t, f.alice, f.overviewPath(f.emptyProjectID))
		mustStatus(t, after, http.StatusOK)
		afterBody := readAll(t, after)
		if !strings.Contains(afterBody, "No research state yet") {
			t.Errorf("with-main guide lacks the no-research-state headline")
		}
		if strings.Contains(afterBody, `data-agent-tool="branch.create"`) {
			t.Errorf("with-main guide still shows the branch.create step")
		}
		if !strings.Contains(afterBody, mainID) {
			t.Errorf("with-main guide does not pre-fill main's branch id %q", mainID)
		}
	})

	// --- anonymously on the public project ---
	t.Run("public overview renders anonymously with escaping", func(t *testing.T) {
		resp := f.page(t, f.anon, f.overviewPath(f.publicProjectID))
		mustStatus(t, resp, http.StatusOK)
		body := readAll(t, resp)
		if !strings.Contains(body, "overview-public") {
			t.Errorf("public overview lacks the project breadcrumb")
		}
		// The hostile purpose renders as text, never as markup.
		if strings.Contains(body, "<script>alert") {
			t.Errorf("public overview renders the hostile script tag")
		}
		if !strings.Contains(body, "&lt;script&gt;alert") {
			t.Errorf("public overview lacks the escaped purpose text")
		}
	})

	// --- the JSON contract on the same route ---
	t.Run("JSON overview contract for non-HTML clients", func(t *testing.T) {
		resp := f.anon.do(t, http.MethodGet, f.overviewPath(f.publicProjectID), "")
		mustStatus(t, resp, http.StatusOK)
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		var payload struct {
			ProjectID    string `json:"project_id"`
			ProjectSlug  string `json:"project_slug"`
			Purpose      string `json:"purpose"`
			KeyQuestions []struct {
				ObjectID string `json:"object_id"`
			} `json:"key_questions"`
			TotalQuestions int `json:"total_questions"`
			CurrentMain    struct {
				BranchID string `json:"branch_id"`
			} `json:"current_main"`
			Empty bool `json:"empty"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatalf("JSON overview decode: %v", err)
		}
		if payload.ProjectSlug != "overview-public" || payload.TotalQuestions != 1 ||
			len(payload.KeyQuestions) != 1 {
			t.Errorf("JSON overview = %+v", payload)
		}
		if payload.CurrentMain.BranchID == "" {
			t.Errorf("JSON overview lacks current main for a project whose main exists")
		}
		if payload.Empty {
			t.Errorf("JSON overview says empty for a project with a question")
		}
	})
}
