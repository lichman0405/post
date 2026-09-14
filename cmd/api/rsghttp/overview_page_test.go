package rsghttp

import (
	"encoding/json"
	"html"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
)

// The Overview page tests (T0212): the HTML representation of the project
// overview route. The point of the negatives: the first screen must
// explain what the project does (purpose + key questions/findings, never
// a files tree), the attention rows and questions must drill down to
// their object pages, the empty state must guide the agent/project start
// with pre-filled tool references, a denied read must render no page
// chrome, and nothing may render raw payload markup.

const overviewBase = "/api/v1/projects/" + rsgTestProjectID + "/overview"

// cannedOverview is the standard summary: a private frozen project with
// two key questions, one key finding, one active path, one attention row
// and a current main with one commit.
func cannedOverview() rsg.ProjectOverview {
	at := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	return rsg.ProjectOverview{
		ProjectID: rsgTestProjectID, ProjectSlug: "rsg-project", ProjectName: "RSG Project",
		Purpose: "Screen MOFs for CO2 capture", Visibility: "private", MainFrozen: true,
		ActivityStatus: "active",
		Counts:         rsg.OutlineCounts{Questions: 2, Findings: 1, Hypotheses: 1, Claims: 1, OtherObjects: 1},
		KeyQuestions: []rsg.OutlineQuestion{
			{
				ObjectID: "q-1", VersionID: "q-1-v1", BranchID: "branch-1",
				Title: "Does the material adsorb CO2?", Statement: "Does it, under pressure?",
				QuestionState: "open",
			},
			{ObjectID: "q-2", VersionID: "q-2-v1", BranchID: "branch-1", Title: "Does it scale?", QuestionState: "unresolved"},
		},
		TotalQuestions: 2,
		KeyFindings: []rsg.OutlineFinding{
			{
				ObjectID: "f-1", VersionID: "f-1-v1", BranchID: "branch-1",
				Title: "Uptake peaks at 30 bar", Statement: "Peaks at 30 bar.",
				FindingType: "trend", Assessment: "accepted",
			},
		},
		TotalFindings: 1,
		Branches: rsg.OverviewBranches{
			Active: []rsg.OverviewBranch{{
				ID: "feat-1", Name: "feature/screening", Purpose: "screen a new linker set",
				HeadStateID: "state-feat", CreatedAt: at,
			}},
			Merged: 1, Aborted: 1,
		},
		NeedsAttention: []rsg.OverviewAttention{{
			Kind: "finding", ObjectID: "f-2", BranchID: "branch-1",
			Title: "Degrades in humid air", Signal: "contested",
		}},
		AttentionTotal: 1,
		CurrentMain: rsg.OverviewMain{
			BranchID: "main-1", BranchName: "main", HeadStateID: "state-main",
			StateHash: "abc123", GitCommitSHA: "deadbeef", HeadCreatedAt: at,
			LatestCommit: rsg.OverviewCommit{
				Present: true, Message: "recorded screening finding", Via: "mcp",
				ActorID: "alice", CreatedAt: at,
			},
		},
	}
}

func getOverviewPage(t *testing.T, svc Service, path string) *http.Response {
	t.Helper()
	ts, _, _, _ := newRSGTestServer(t, svc)
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/html")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// TestOverviewPageRendersSummary: the first screen explains the project —
// purpose, key questions and findings with drill-down links, active
// paths, attention rows, current main — and is read-only, with no file
// tree anywhere.
func TestOverviewPageRendersSummary(t *testing.T) {
	stub := &stubService{overview: cannedOverview()}
	resp := getOverviewPage(t, stub, overviewBase)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, respBody(resp))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if vary := resp.Header.Get("Vary"); !strings.Contains(vary, "Accept") {
		t.Errorf("Vary = %q, want Accept listed", vary)
	}
	body := respBody(resp)
	for _, want := range []string{
		// breadcrumb + header: the project identity and its governance facts
		"rsg-project",
		`data-overview-meta`,
		`data-visibility="private"`,
		`data-main-frozen`,
		`data-activity-status`,
		`data-overview-counts`,
		// the project tab bar: Overview active, Research linked
		`data-project-tab="overview"`,
		`data-project-tab="research"`,
		`aria-current="page"`,
		`href="` + overviewBase + `"`,
		// the purpose — the first screen's "what is this project about"
		"Screen MOFs for CO2 capture",
		`data-overview-section="purpose"`,
		// key questions with drill-down links into the object pages
		`data-overview-section="questions"`,
		`data-question-id="q-1"`,
		"Does the material adsorb CO2?",
		"/api/v1/projects/" + rsgTestProjectID + "/branches/branch-1/objects/q-1",
		// key findings with their assessment
		`data-overview-section="findings"`,
		`data-finding-id="f-1"`,
		"Uptake peaks at 30 bar",
		`data-assessment="accepted"`,
		// active paths
		`data-overview-section="branches"`,
		`data-branch-name="feature/screening"`,
		"screen a new linker set",
		// needs attention with its signal
		`data-overview-section="attention"`,
		"Degrades in humid air",
		`data-signal="contested"`,
		// current main: the head and its newest commit
		`data-overview-section="main"`,
		`data-main`,
		"state-main",
		"recorded screening finding",
		"via mcp",
		// the footer note
		"read-only",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q", want)
		}
	}
	// The first screen must never be a files/type listing, and the page
	// must carry no edit affordance.
	for _, forbidden := range []string{">Files<", "file tree", "<form", "<input", "<textarea", "<button"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("body contains %q", forbidden)
		}
	}
}

// TestOverviewPageEmptyStateGuide: an empty project with no main renders
// the start guide with the pre-filled agent tool references; a project
// whose main exists renders the shorter guide with main's real branch id
// pre-filled.
func TestOverviewPageEmptyStateGuide(t *testing.T) {
	empty := rsg.ProjectOverview{
		ProjectID: rsgTestProjectID, ProjectSlug: "rsg-project", ProjectName: "RSG Project",
		Purpose: "Screen MOFs for CO2 capture", Visibility: "private", Empty: true,
	}

	// No main yet: the guide walks through branch.create first.
	stub := &stubService{overview: empty}
	body := respBody(getOverviewPage(t, stub, overviewBase))
	for _, want := range []string{
		`data-overview-guide`,
		"Start this project",
		"Screen MOFs for CO2 capture",
		`data-agent-tool="branch.create"`,
		`data-agent-tool="object.create"`,
		`data-agent-tool="relation.create"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("empty guide lacks %q", want)
		}
	}
	// The pre-filled tool references render in text nodes with quotes
	// HTML-escaped (&#34;); the decoded text carries the exact JSON.
	unescaped := html.UnescapeString(body)
	for _, want := range []string{
		`"name": "branch.create"`,
		`"project_id": "` + rsgTestProjectID + `"`,
		`"name": "main"`,
		`"object_type": "research_question"`,
		`"branch_id": "<branch_id>"`,
		`"type": "addresses_question"`,
	} {
		if !strings.Contains(unescaped, want) {
			t.Errorf("empty guide's agent JSON lacks %q", want)
		}
	}
	// The empty guide replaces the summary sections.
	for _, forbidden := range []string{`data-overview-section="questions"`, `data-overview-section="main"`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("empty guide renders summary section %q", forbidden)
		}
	}

	// Main exists: no branch.create step, and the object/relation refs
	// carry main's real branch id.
	withMain := empty
	withMain.CurrentMain = rsg.OverviewMain{BranchID: "main-1", BranchName: "main", HeadStateID: "state-main"}
	withMain.Empty = true
	stub = &stubService{overview: withMain}
	body = respBody(getOverviewPage(t, stub, overviewBase))
	for _, want := range []string{
		"No research state yet",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("with-main guide lacks %q", want)
		}
	}
	if strings.Contains(body, `data-agent-tool="branch.create"`) {
		t.Errorf("with-main guide still shows the branch.create step")
	}
	if !strings.Contains(html.UnescapeString(body), `"branch_id": "main-1"`) {
		t.Errorf("with-main guide's agent JSON lacks main's branch id")
	}
}

// TestOverviewPageEscapesHostileText: payload text renders escaped —
// never raw markup.
func TestOverviewPageEscapesHostileText(t *testing.T) {
	ov := cannedOverview()
	ov.Purpose = `Screens <script>alert('xss')</script> material`
	ov.KeyQuestions[0].Title = `Does <img src=x onerror=alert(1)> adsorb?`
	stub := &stubService{overview: ov}
	body := respBody(getOverviewPage(t, stub, overviewBase))
	if strings.Contains(body, "<script>alert('xss')</script>") {
		t.Errorf("purpose renders raw script markup")
	}
	if strings.Contains(body, "<img src=x") {
		t.Errorf("question title renders raw img markup")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("escaped purpose missing")
	}
}

// TestOverviewPageDeniedRead: a denied read renders the neutral
// existence-hidden not-found page with no chrome.
func TestOverviewPageDeniedRead(t *testing.T) {
	stub := &stubService{err: projects.ErrProjectNotFound}
	resp := getOverviewPage(t, stub, overviewBase)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", resp.StatusCode, respBody(resp))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := respBody(resp)
	if !strings.Contains(body, "Not found") {
		t.Errorf("denied page lacks the neutral not-found title")
	}
	for _, chrome := range []string{"data-overview-meta", "data-project-tab", "Screen MOFs", "rsg-project"} {
		if strings.Contains(body, chrome) {
			t.Errorf("denied read renders page chrome (%q)", chrome)
		}
	}
}

// TestOverviewJSONContract: non-HTML clients get the summary JSON on the
// same route — the agent/MCP surface reads the overview as data.
func TestOverviewJSONContract(t *testing.T) {
	stub := &stubService{overview: cannedOverview()}
	ts, authed, _, _ := newRSGTestServer(t, stub)
	resp, err := authed.Get(ts.URL + overviewBase)
	if err != nil {
		t.Fatalf("GET overview JSON: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, respBody(resp))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var p struct {
		ProjectID    string `json:"project_id"`
		ProjectSlug  string `json:"project_slug"`
		Purpose      string `json:"purpose"`
		MainFrozen   bool   `json:"main_frozen"`
		KeyQuestions []struct {
			ObjectID string `json:"object_id"`
			Title    string `json:"title"`
		} `json:"key_questions"`
		TotalQuestions int `json:"total_questions"`
		KeyFindings    []struct {
			ObjectID   string `json:"object_id"`
			Assessment string `json:"assessment"`
		} `json:"key_findings"`
		TotalFindings int `json:"total_findings"`
		Branches      struct {
			Active []struct {
				Name    string `json:"name"`
				Purpose string `json:"purpose"`
			} `json:"active"`
			Merged  int `json:"merged"`
			Aborted int `json:"aborted"`
		} `json:"branches"`
		NeedsAttention []struct {
			Kind   string `json:"kind"`
			Signal string `json:"signal"`
		} `json:"needs_attention"`
		AttentionTotal int `json:"attention_total"`
		CurrentMain    struct {
			BranchID     string `json:"branch_id"`
			HeadStateID  string `json:"head_state_id"`
			LatestCommit *struct {
				Message string `json:"message"`
				Via     string `json:"via"`
				Actor   string `json:"actor"`
			} `json:"latest_commit"`
		} `json:"current_main"`
		Empty bool `json:"empty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("decode overview JSON: %v", err)
	}
	if p.ProjectSlug != "rsg-project" || p.Purpose != "Screen MOFs for CO2 capture" || !p.MainFrozen {
		t.Errorf("project facts = %+v", p)
	}
	if p.TotalQuestions != 2 || len(p.KeyQuestions) != 2 || p.KeyQuestions[0].ObjectID != "q-1" {
		t.Errorf("key questions = %+v", p.KeyQuestions)
	}
	if p.TotalFindings != 1 || len(p.KeyFindings) != 1 || p.KeyFindings[0].Assessment != "accepted" {
		t.Errorf("key findings = %+v", p.KeyFindings)
	}
	if len(p.Branches.Active) != 1 || p.Branches.Active[0].Name != "feature/screening" ||
		p.Branches.Merged != 1 || p.Branches.Aborted != 1 {
		t.Errorf("branches = %+v", p.Branches)
	}
	if p.AttentionTotal != 1 || len(p.NeedsAttention) != 1 || p.NeedsAttention[0].Signal != "contested" {
		t.Errorf("attention = %+v", p.NeedsAttention)
	}
	if p.CurrentMain.BranchID != "main-1" || p.CurrentMain.HeadStateID != "state-main" ||
		p.CurrentMain.LatestCommit == nil || p.CurrentMain.LatestCommit.Message != "recorded screening finding" {
		t.Errorf("current main = %+v", p.CurrentMain)
	}
	if p.Empty {
		t.Errorf("empty = true for a populated overview")
	}
}

// TestOverviewJSONDenied: the denied JSON contract answers the standard
// error envelope — the route's JSON path runs the same gate.
func TestOverviewJSONDenied(t *testing.T) {
	stub := &stubService{err: projects.ErrProjectNotFound}
	ts, authed, _, _ := newRSGTestServer(t, stub)
	resp, err := authed.Get(ts.URL + overviewBase)
	if err != nil {
		t.Fatalf("GET overview JSON: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if envelope := decodeError(t, resp); envelope.Code != projects.CodeProjectNotFound {
		t.Errorf("code = %q, want %q", envelope.Code, projects.CodeProjectNotFound)
	}
}
