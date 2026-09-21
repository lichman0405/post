// Package integration — T0211 "research page e2e": the research outline
// page exercised end to end over REAL PostgreSQL, the real auth guard,
// the real RSG service and the real HTTP surface — signup, project
// create, branch create, question/hypothesis/finding/claim/relation
// creates, then the browser read of the research page itself. The
// acceptance criteria ride on the negatives first: the default view must
// be the question-organized outline (never a files/type dump), a question
// must drill down to its detail page over the wire, an invisible
// project's outline must answer the existence-hidden not-found page with
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

	"github.com/jackc/pgx/v5/pgxpool"
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

const researchPageTaskID = "T0211"

// researchPageFixture is the full production composition (the same wiring
// cmd/api/main.go builds) plus the seeded research state: alice owns a
// private project with a two-level question tree, a hypothesis and a
// finding addressing the root question, a claim the finding references, a
// material addressing the sub-question and a leftover dataset; the public
// project carries one question whose statement is hostile markup. bob is
// a member of nothing.
type researchPageFixture struct {
	ts *httptest.Server
	// pool is the fixture's database. The HTTP surface is the subject of
	// every test built on this fixture; the pool is here for the fixtures
	// that must place state no HTTP call can place cheaply (T1102's scale
	// project: the API's per-write cost grows with the project's size, so
	// 1000 objects go in directly and the read path under test stays real).
	pool *pgxpool.Pool

	alice *testUserClient
	bob   *testUserClient
	anon  *testUserClient

	privateProjectID string
	privateBranchID  string
	rootQuestionID   string
	childQuestionID  string
	findingID        string
	hypothesisID     string
	claimID          string
	materialID       string

	publicProjectID string
	publicBranchID  string
	publicQuestion  string
}

// researchPage fetches the research route with a browser Accept header.
func (f *researchPageFixture) page(t *testing.T, uc *testUserClient, path string) *http.Response {
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

func (f *researchPageFixture) researchPath(projectID string) string {
	return "/api/v1/projects/" + projectID + "/research"
}

type createObjResponse struct {
	ID        string `json:"id"`
	VersionID string `json:"version_id"`
	VersionNo int    `json:"version_no"`
}

// createObject creates one object over the wire and returns its identity.
func (f *researchPageFixture) createObject(t *testing.T, projectID, branchID, objectType, payload string) createObjResponse {
	t.Helper()
	resp := f.alice.do(t, http.MethodPost,
		"/api/v1/projects/"+projectID+"/branches/"+branchID+"/objects",
		`{"object_type":"`+objectType+`","payload":`+payload+`}`)
	mustStatus(t, resp, http.StatusCreated)
	var obj createObjResponse
	if err := json.NewDecoder(resp.Body).Decode(&obj); err != nil {
		t.Fatalf("create %s payload: %v", objectType, err)
	}
	return obj
}

func newResearchPageFixture(t *testing.T, ctx context.Context) *researchPageFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), researchPageTaskID)

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

	alice, _ := signup(t, ts.URL, "research-alice@example.com", "research-alice")
	bob, _ := signup(t, ts.URL, "research-bob@example.com", "research-bob")
	anon := newTestUserClient(ts.URL)

	f := &researchPageFixture{ts: ts, alice: alice, bob: bob, anon: anon, pool: pool}

	// --- the private project and its research state ---
	resp := alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"research-lab","name":"Research Lab","purpose":"exercise the research page","visibility":"private"}`)
	mustStatus(t, resp, http.StatusCreated)
	var privateProject projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&privateProject); err != nil {
		t.Fatalf("create private project payload: %v", err)
	}
	f.privateProjectID = privateProject.Project.ID

	resp = alice.do(t, http.MethodPost, "/api/v1/projects/"+f.privateProjectID+"/branches",
		`{"name":"main","base_ref":"","visibility":"private"}`)
	mustStatus(t, resp, http.StatusCreated)
	var branchResp struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&branchResp); err != nil {
		t.Fatalf("create private branch payload: %v", err)
	}
	f.privateBranchID = branchResp.ID

	// The question tree: a root and a sub-question naming it as parent.
	rootQ := f.createObject(t, f.privateProjectID, f.privateBranchID, "research_question",
		`{"statement":"Does the material adsorb CO2?","question_state":"open"}`)
	f.rootQuestionID = rootQ.ID
	childQ := f.createObject(t, f.privateProjectID, f.privateBranchID, "research_question",
		`{"statement":"At which pressure does uptake peak?","question_state":"partially_answered","parent_question_id":"`+rootQ.ID+`"}`)
	f.childQuestionID = childQ.ID

	// A hypothesis and a finding addressing the root question, a claim the
	// finding references, a material addressing the sub-question, and a
	// leftover dataset addressing nothing.
	hyp := f.createObject(t, f.privateProjectID, f.privateBranchID, "hypothesis",
		`{"statement":"Uptake scales with pressure","question_id":"`+rootQ.ID+`"}`)
	f.hypothesisID = hyp.ID
	claim := f.createObject(t, f.privateProjectID, f.privateBranchID, "claim",
		`{"statement":"Capacity is 2 mmol/g","claim_type":"quantitative"}`)
	f.claimID = claim.ID
	finding := f.createObject(t, f.privateProjectID, f.privateBranchID, "finding",
		`{"statement":"Uptake peaks at 30 bar","finding_type":"trend","assessment":"accepted","claim_version_refs":["`+claim.VersionID+`","deadbeef-0000-4000-8000-000000000000"]}`)
	f.findingID = finding.ID
	mat := f.createObject(t, f.privateProjectID, f.privateBranchID, "material",
		`{"name":"MOF-5"}`)
	f.materialID = mat.ID
	f.createObject(t, f.privateProjectID, f.privateBranchID, "dataset",
		`{"name":"isotherm series"}`)

	// The addresses_question edges (the outline's linkage).
	for _, edge := range []struct{ source, target string }{
		{hyp.VersionID, rootQ.VersionID},
		{finding.VersionID, rootQ.VersionID},
		{mat.VersionID, childQ.VersionID},
	} {
		resp = alice.do(t, http.MethodPost, "/api/v1/projects/"+f.privateProjectID+"/branches/"+f.privateBranchID+"/relations",
			`{"relation_type":"addresses_question","source_object_version_id":"`+edge.source+`","target_object_version_id":"`+edge.target+`"}`)
		mustStatus(t, resp, http.StatusCreated)
	}

	// --- the public project: one hostile question anyone may read ---
	resp = alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"research-public","name":"Research Public","purpose":"public research fixture","visibility":"public"}`)
	mustStatus(t, resp, http.StatusCreated)
	var publicProject projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&publicProject); err != nil {
		t.Fatalf("create public project payload: %v", err)
	}
	f.publicProjectID = publicProject.Project.ID

	resp = alice.do(t, http.MethodPost, "/api/v1/projects/"+f.publicProjectID+"/branches",
		`{"name":"main","base_ref":"","visibility":"private"}`)
	mustStatus(t, resp, http.StatusCreated)
	if err := json.NewDecoder(resp.Body).Decode(&branchResp); err != nil {
		t.Fatalf("create public branch payload: %v", err)
	}
	f.publicBranchID = branchResp.ID

	publicQ := f.createObject(t, f.publicProjectID, f.publicBranchID, "research_question",
		`{"statement":"Does <script>alert('xss')</script> adsorb anything?","question_state":"unresolved"}`)
	f.publicQuestion = publicQ.ID

	return f
}

// TestResearchPageE2E is the required "research page e2e" test.
func TestResearchPageE2E(t *testing.T) {
	ctx := testCtx(t)
	f := newResearchPageFixture(t, ctx)

	// --- denial first: the outline is exactly as visible as its project ---
	t.Run("private outline hidden from anonymous", func(t *testing.T) {
		resp := f.page(t, f.anon, f.researchPath(f.privateProjectID))
		mustStatus(t, resp, http.StatusNotFound)
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("Content-Type = %q, want text/html", ct)
		}
		body := readAll(t, resp)
		if !strings.Contains(body, "Not found") {
			t.Errorf("denied page lacks the neutral not-found title")
		}
		for _, chrome := range []string{"data-research-view", "data-question-id", "Research questions", "Findings", "Research Lab"} {
			if strings.Contains(body, chrome) {
				t.Errorf("denied read renders page chrome (%q) — existence must stay hidden", chrome)
			}
		}
	})

	t.Run("private outline hidden from non-member", func(t *testing.T) {
		resp := f.page(t, f.bob, f.researchPath(f.privateProjectID))
		mustStatus(t, resp, http.StatusNotFound)
		if body := readAll(t, resp); strings.Contains(body, "Does the material adsorb CO2?") {
			t.Errorf("non-member outline discloses a question")
		}
	})

	t.Run("denied JSON contract unchanged", func(t *testing.T) {
		resp := f.bob.do(t, http.MethodGet, f.researchPath(f.privateProjectID), "")
		mustStatus(t, resp, http.StatusNotFound)
		mustEnvelope(t, resp, projects.CodeProjectNotFound)
	})

	// --- the page itself, as alice on her private project ---
	t.Run("default view is the question outline, not a type dump", func(t *testing.T) {
		resp := f.page(t, f.alice, f.researchPath(f.privateProjectID))
		mustStatus(t, resp, http.StatusOK)
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("Content-Type = %q, want text/html", ct)
		}
		if vary := resp.Header.Get("Vary"); !strings.Contains(vary, "Accept") {
			t.Errorf("Vary = %q, want Accept listed", vary)
		}
		body := readAll(t, resp)
		for _, want := range []string{
			// breadcrumb + auxiliary counts (never the organization)
			"research-lab",
			`data-research-counts`,
			"2 questions", "1 finding", "1 hypothesis", "1 claim", "2 other objects",
			// default view: the question tree
			`data-research-view="questions"`,
			`data-outline="questions"`,
			`data-question-id="` + f.rootQuestionID + `"`,
			`data-question-id="` + f.childQuestionID + `"`,
			// question statements render inside the outline
			"Does the material adsorb CO2?",
			"At which pressure does uptake peak?",
			// the addressed-by lists are role-grouped, with the hypothesis
			// and finding titles
			"Uptake scales with pressure",
			"Uptake peaks at 30 bar",
			"MOF-5",
			// auxiliary per-question counts
			`data-question-counts="` + f.rootQuestionID + `"`,
			// the unassigned remainder (the dataset) with its type badge
			`data-outline="unassigned"`,
			"isotherm series",
			"dataset",
			// the read-only footer
			"The page is read-only",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("default page lacks %q", want)
			}
		}
		// The outline opens the page; the remainder never does.
		questionsAt := strings.Index(body, `data-outline="questions"`)
		unassignedAt := strings.Index(body, `data-outline="unassigned"`)
		if questionsAt < 0 || unassignedAt < 0 || unassignedAt < questionsAt {
			t.Errorf("outline order wrong: questions=%d unassigned=%d", questionsAt, unassignedAt)
		}
		// No type-grouped section headers — the files-dump shape is off
		// the table.
		for _, forbidden := range []string{">Materials<", ">Datasets<", ">Hypotheses<", ">Objects by type<"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("default page contains a type-grouped section %q", forbidden)
			}
		}
	})

	t.Run("question drills down to its detail page", func(t *testing.T) {
		resp := f.page(t, f.alice, f.researchPath(f.privateProjectID))
		mustStatus(t, resp, http.StatusOK)
		body := readAll(t, resp)
		// The question title is a link to the object detail page; follow
		// it exactly as a browser would and land on the detail page.
		detailPath := fmt.Sprintf("/api/v1/projects/%s/branches/%s/objects/%s",
			f.privateProjectID, f.privateBranchID, f.rootQuestionID)
		if !strings.Contains(body, `href="`+detailPath+`"`) {
			t.Errorf("outline lacks the drill-down link %q", detailPath)
		}
		detail := f.page(t, f.alice, detailPath)
		mustStatus(t, detail, http.StatusOK)
		detailBody := readAll(t, detail)
		if !strings.Contains(detailBody, "Does the material adsorb CO2?") {
			t.Errorf("detail page does not render the question the outline linked to")
		}
	})

	t.Run("findings view aggregates claims and addressed questions", func(t *testing.T) {
		resp := f.page(t, f.alice, f.researchPath(f.privateProjectID)+"?view=findings")
		mustStatus(t, resp, http.StatusOK)
		body := readAll(t, resp)
		for _, want := range []string{
			`data-research-view="findings"`,
			`data-outline="findings"`,
			`data-finding-id="` + f.findingID + `"`,
			"Uptake peaks at 30 bar",
			`data-assessment="accepted"`,
			"finding type <code>trend</code>",
			// the claim resolves to its detail-page link …
			"Capacity is 2 mmol/g",
			"/branches/" + f.privateBranchID + "/objects/" + f.claimID,
			// … the missing claim renders its pinned version id
			"deadbeef-0000-4000-8000-000000000000",
			"not in this project",
			// the question the finding addresses
			"Does the material adsorb CO2?",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("findings view lacks %q", want)
			}
		}
	})

	t.Run("page has no edit form", func(t *testing.T) {
		resp := f.page(t, f.alice, f.researchPath(f.privateProjectID))
		mustStatus(t, resp, http.StatusOK)
		body := strings.ToLower(readAll(t, resp))
		for _, forbidden := range []string{"<form", "<input", "<textarea", "<button"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("page contains %q — the research page must have no edit form", forbidden)
			}
		}
	})

	// The version-pinned edge case over real storage: the root question
	// gains a v2 AFTER the edges exist. The edges stay pinned to v1 —
	// nothing re-pins them — so the outline must resolve them by container
	// object: the hypothesis and finding still address the question, the
	// question renders at v2, and nothing drops to the unassigned remainder.
	t.Run("edges pinned to superseded versions still appear", func(t *testing.T) {
		resp := f.alice.do(t, http.MethodPost,
			"/api/v1/projects/"+f.privateProjectID+"/branches/"+f.privateBranchID+"/objects/"+f.rootQuestionID+":version",
			`{"expected_version":1,"patch":{"statement":"Does the material adsorb CO2? (revised)"}}`)
		mustStatus(t, resp, http.StatusCreated)

		page := f.page(t, f.alice, f.researchPath(f.privateProjectID))
		mustStatus(t, page, http.StatusOK)
		body := readAll(t, page)
		for _, want := range []string{
			// the question now renders its v2 statement …
			"Does the material adsorb CO2? (revised)",
			// … and the v1-pinned edges still attach their sources
			"Uptake scales with pressure",
			"Uptake peaks at 30 bar",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("outline after the question's v2 lacks %q", want)
			}
		}
		// A surviving edge never orphans its source into the remainder.
		unassignedAt := strings.Index(body, `data-outline="unassigned"`)
		if unassignedAt < 0 {
			t.Fatalf("outline lacks the unassigned section")
		}
		for _, orphaned := range []string{"Uptake scales with pressure", "Uptake peaks at 30 bar"} {
			if strings.Index(body, orphaned) > unassignedAt {
				t.Errorf("v1-pinned edge orphaned %q into the unassigned remainder", orphaned)
			}
		}
		// The finding axis keeps its addressed question, at v2.
		findings := f.page(t, f.alice, f.researchPath(f.privateProjectID)+"?view=findings")
		mustStatus(t, findings, http.StatusOK)
		if body := readAll(t, findings); !strings.Contains(body, "Does the material adsorb CO2? (revised)") {
			t.Errorf("findings view lost the addressed question after its v2")
		}
	})

	// The claim-ref case over real storage: the finding's claim_version_refs
	// pins the claim's v1; the claim then gains a v2. The ref must still
	// resolve — the page must not say "not in this project" about a claim
	// object that lives in the project, and the claim must not land in the
	// unassigned remainder at the same time.
	t.Run("claim ref pinned to a superseded version still resolves", func(t *testing.T) {
		resp := f.alice.do(t, http.MethodPost,
			"/api/v1/projects/"+f.privateProjectID+"/branches/"+f.privateBranchID+"/objects/"+f.claimID+":version",
			`{"expected_version":1,"patch":{"statement":"Capacity is 2 mmol/g (revised)"}}`)
		mustStatus(t, resp, http.StatusCreated)

		findings := f.page(t, f.alice, f.researchPath(f.privateProjectID)+"?view=findings")
		mustStatus(t, findings, http.StatusOK)
		body := readAll(t, findings)
		// The pinned v1 resolves with its own title and a drill-down link …
		if !strings.Contains(body, "Capacity is 2 mmol/g") {
			t.Errorf("findings view lost the pinned claim's title")
		}
		if !strings.Contains(body, "/branches/"+f.privateBranchID+"/objects/"+f.claimID) {
			t.Errorf("findings view lacks the claim's drill-down link")
		}
		// … and exactly ONE ref renders "not in this project" — the
		// deadbeef one, not the in-project claim.
		if n := strings.Count(body, "not in this project"); n != 1 {
			t.Errorf("findings view says 'not in this project' %d times, want exactly 1 (only the missing ref)", n)
		}
		// The claim is part of the findings axis: its v2 title must not
		// appear in the questions view's unassigned remainder.
		questions := f.page(t, f.alice, f.researchPath(f.privateProjectID))
		mustStatus(t, questions, http.StatusOK)
		qbody := readAll(t, questions)
		unassignedAt := strings.Index(qbody, `data-outline="unassigned"`)
		if unassignedAt < 0 {
			t.Fatalf("questions view lacks the unassigned section")
		}
		if i := strings.Index(qbody, "Capacity is 2 mmol/g (revised)"); i >= 0 && i > unassignedAt {
			t.Errorf("claim appears in the unassigned remainder while a finding references it")
		}
	})

	// --- anonymously on the public project ---
	t.Run("public outline renders anonymously with escaping", func(t *testing.T) {
		resp := f.page(t, f.anon, f.researchPath(f.publicProjectID))
		mustStatus(t, resp, http.StatusOK)
		body := readAll(t, resp)
		if !strings.Contains(body, "research-public") {
			t.Errorf("public outline lacks the project breadcrumb")
		}
		// The hostile statement renders as text, never as markup.
		if strings.Contains(body, "<script>alert") {
			t.Errorf("public outline renders the hostile script tag")
		}
		if !strings.Contains(body, "&lt;script&gt;alert") {
			t.Errorf("public outline lacks the escaped statement text")
		}
	})

	// --- the JSON contract on the same route ---
	t.Run("JSON outline contract for non-HTML clients", func(t *testing.T) {
		resp := f.anon.do(t, http.MethodGet, f.researchPath(f.publicProjectID), "")
		mustStatus(t, resp, http.StatusOK)
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		var payload struct {
			ProjectID   string `json:"project_id"`
			ProjectSlug string `json:"project_slug"`
			Counts      struct {
				Questions int `json:"questions"`
			} `json:"counts"`
			Questions []struct {
				ObjectID      string `json:"object_id"`
				Title         string `json:"title"`
				QuestionState string `json:"question_state"`
			} `json:"questions"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatalf("JSON outline decode: %v", err)
		}
		if payload.ProjectSlug != "research-public" || payload.Counts.Questions != 1 {
			t.Errorf("JSON outline = %+v", payload)
		}
		if len(payload.Questions) != 1 || payload.Questions[0].ObjectID != f.publicQuestion ||
			payload.Questions[0].QuestionState != "unresolved" {
			t.Errorf("JSON questions = %+v", payload.Questions)
		}
	})
}
