package rsghttp

import (
	"net/http"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
)

// The Research page tests (T0211): the HTML representation of the outline
// route. The point of the negatives: the default view must be the
// question-organized outline (never a type dump), questions must
// drill down to their detail pages, a denied read must render no page
// chrome, and nothing may render raw payload markup.

const researchBase = "/api/v1/projects/" + rsgTestProjectID + "/research"

// cannedOutline is the standard outline: one root question with a child,
// a hypothesis and a finding addressing the root, a claim referenced by
// the finding, and one unassigned dataset.
func cannedOutline() rsg.ResearchOutline {
	return rsg.ResearchOutline{
		ProjectID:   rsgTestProjectID,
		ProjectSlug: "rsg-project",
		ProjectName: "RSG Project",
		Questions: []rsg.OutlineQuestion{
			{
				ObjectID: "q-1", VersionID: "q-1-v1", BranchID: "branch-1",
				Title: "Does the material adsorb CO2?", Statement: "Does it, under pressure?",
				QuestionState: "open",
				Hypotheses:    []rsg.OutlineObjectRef{{ObjectID: "h-1", ObjectType: "hypothesis", Title: "Uptake scales with pressure", BranchID: "branch-1"}},
				Findings:      []rsg.OutlineObjectRef{{ObjectID: "f-1", ObjectType: "finding", Title: "Uptake peaks at 30 bar", BranchID: "branch-1"}},
				OtherObjects:  []rsg.OutlineObjectRef{{ObjectID: "m-1", ObjectType: "material", Title: "MOF-5", BranchID: "branch-1"}},
				Children: []rsg.OutlineQuestion{
					{ObjectID: "q-2", VersionID: "q-2-v1", BranchID: "branch-1", Title: "At what pressure?", QuestionState: "partially_answered"},
				},
			},
		},
		Findings: []rsg.OutlineFinding{
			{
				ObjectID: "f-1", VersionID: "f-1-v1", BranchID: "branch-1",
				Title: "Uptake peaks at 30 bar", Statement: "Peaks at 30 bar.",
				FindingType: "trend", Assessment: "accepted",
				Claims: []rsg.OutlineClaimRef{
					{ObjectID: "c-1", VersionID: "c-1-v1", Title: "Capacity is 2 mmol/g", BranchID: "branch-1", Resolved: true},
					{VersionID: "c-ext-v9", Resolved: false},
				},
				Questions: []rsg.OutlineObjectRef{{ObjectID: "q-1", ObjectType: "research_question", Title: "Does the material adsorb CO2?", BranchID: "branch-1"}},
			},
		},
		Unassigned: []rsg.OutlineObjectRef{{ObjectID: "d-1", ObjectType: "dataset", Title: "isotherm series", BranchID: "branch-1"}},
		Counts:     rsg.OutlineCounts{Questions: 2, Findings: 1, Hypotheses: 1, Claims: 1, OtherObjects: 2},
	}
}

func getResearchPage(t *testing.T, svc Service, path string) *http.Response {
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

// TestResearchPageRendersByQuestion: the default view is the
// question-organized outline with drill-down links — the two acceptance
// criteria at once (not a type dump; drill down from a question).
func TestResearchPageRendersByQuestion(t *testing.T) {
	stub := &stubService{outline: cannedOutline()}
	resp := getResearchPage(t, stub, researchBase)
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
		// header: project breadcrumb + auxiliary counts
		"rsg-project",
		`data-research-counts`,
		"2 questions · 1 finding · 1 hypothesis · 1 claim · 2 other objects",
		// the default view is the question outline
		`data-research-view="questions"`,
		`data-outline="questions"`,
		`data-question-id="q-1"`,
		// question state rendered as a text badge, not color alone
		`data-question-state="open"`, "open",
		// the question statement is part of the outline body
		"Does it, under pressure?",
		// drill-down: the question title links to its object detail page
		`/api/v1/projects/` + rsgTestProjectID + `/branches/branch-1/objects/q-1`,
		// the addressed-by lists are role-grouped
		"hypothesis", "finding",
		"Uptake scales with pressure", "Uptake peaks at 30 bar", "MOF-5",
		// the nested sub-question
		`data-question-id="q-2"`, "At what pressure?",
		// the unassigned remainder carries type badges, not a type grouping
		`data-outline="unassigned"`, "isotherm series",
		// view switch links
		`data-view="questions"`, `data-view="findings"`, "view=findings",
		// the read-only footer
		"The page is read-only",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if stub.researchOutlineCalls != 1 || stub.lastProjectID != rsgTestProjectID {
		t.Errorf("calls = %d, projectID = %q", stub.researchOutlineCalls, stub.lastProjectID)
	}
}

// TestResearchPageDefaultIsNotTypeDump: the outline body precedes the
// unassigned remainder, the primary structure is questions, and no type
// grouping opens the page.
func TestResearchPageDefaultIsNotTypeDump(t *testing.T) {
	stub := &stubService{outline: cannedOutline()}
	resp := getResearchPage(t, stub, researchBase)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s)", resp.StatusCode, respBody(resp))
	}
	body := respBody(resp)
	questionsAt := strings.Index(body, `data-outline="questions"`)
	unassignedAt := strings.Index(body, `data-outline="unassigned"`)
	if questionsAt < 0 || unassignedAt < 0 {
		t.Fatalf("outline sections missing: questions=%d unassigned=%d", questionsAt, unassignedAt)
	}
	if unassignedAt < questionsAt {
		t.Errorf("unassigned section opens the page — the default view must be the question outline")
	}
	// The page body must not open with a per-type object listing (the
	// files-dump shape): no "Materials" / "Datasets" type-grouped headers.
	for _, forbidden := range []string{">Materials<", ">Datasets<", ">Objects by type<"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("page contains a type-grouped section %q", forbidden)
		}
	}
}

// TestResearchPageFindingsView: ?view=findings renders the findings with
// claims (resolved and pinned-only) and the questions they address.
func TestResearchPageFindingsView(t *testing.T) {
	stub := &stubService{outline: cannedOutline()}
	resp := getResearchPage(t, stub, researchBase+"?view=findings")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s)", resp.StatusCode, respBody(resp))
	}
	body := respBody(resp)
	for _, want := range []string{
		`data-research-view="findings"`,
		`data-outline="findings"`,
		`data-finding-id="f-1"`,
		"Uptake peaks at 30 bar",
		`data-assessment="accepted"`,
		"finding type <code>trend</code>",
		// resolved claim links to its detail page; the unresolved one
		// renders its pinned version id without a link
		"/branches/branch-1/objects/c-1",
		"Capacity is 2 mmol/g",
		"c-ext-v9", "not in this project",
		// the questions this finding addresses
		"Does the material adsorb CO2?",
		`data-outline="unassigned"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("findings view lacks %q", want)
		}
	}
}

// TestResearchPageFindingsViewQuestionWithoutBranch: an addressed question
// whose current version carries no branch renders as plain text in the
// findings view — same rule as the question side's reflink, no fabricated
// empty-href link.
func TestResearchPageFindingsViewQuestionWithoutBranch(t *testing.T) {
	out := cannedOutline()
	out.Findings[0].Questions[0].BranchID = ""
	stub := &stubService{outline: out}
	resp := getResearchPage(t, stub, researchBase+"?view=findings")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s)", resp.StatusCode, respBody(resp))
	}
	body := respBody(resp)
	if strings.Contains(body, `<a href="">Does the material adsorb CO2?</a>`) {
		t.Errorf("findings view renders an empty-href link for a branch-less question")
	}
	if !strings.Contains(body, "Does the material adsorb CO2?") {
		t.Errorf("findings view lost the addressed question title")
	}
}

func TestResearchPageUnknownViewFallsBackToQuestions(t *testing.T) {
	stub := &stubService{outline: cannedOutline()}
	resp := getResearchPage(t, stub, researchBase+"?view=bogus")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s)", resp.StatusCode, respBody(resp))
	}
	body := respBody(resp)
	if !strings.Contains(body, `data-research-view="questions"`) {
		t.Errorf("unknown view did not fall back to questions")
	}
	if strings.Contains(body, `data-outline="findings"`) {
		t.Errorf("unknown view rendered the findings outline")
	}
}

// TestResearchPageEscapesHostileText: statements and titles render as
// text, never as markup — and hostile state values cannot break out of
// their attribute contexts (the badge class and data attributes).
func TestResearchPageEscapesHostileText(t *testing.T) {
	out := cannedOutline()
	out.Questions[0].Title = "<b>bold</b>"
	out.Questions[0].Statement = "<script>alert('xss')</script>"
	out.Questions[0].QuestionState = `open" onmouseover="alert(1)`
	out.Findings[0].Title = "<img src=x onerror=alert(1)>"
	out.Findings[0].Assessment = `accepted" data-injected="1`
	stub := &stubService{outline: out}
	resp := getResearchPage(t, stub, researchBase)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s)", resp.StatusCode, respBody(resp))
	}
	body := respBody(resp)
	for _, forbidden := range []string{"<script>alert", "<img src=x", "<b>bold</b>"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("page renders raw markup %q", forbidden)
		}
	}
	if !strings.Contains(body, "&lt;script&gt;alert") {
		t.Errorf("page lacks the escaped statement text")
	}
	if strings.Contains(body, `data-injected="1"`) || strings.Contains(body, `onmouseover="alert(1)"`) {
		t.Errorf("hostile state value broke out of its attribute context")
	}
}

// TestResearchPageHasNoEditForm: the page is read-only — nothing that
// could submit a scientific mutation.
func TestResearchPageHasNoEditForm(t *testing.T) {
	stub := &stubService{outline: cannedOutline()}
	resp := getResearchPage(t, stub, researchBase)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s)", resp.StatusCode, respBody(resp))
	}
	body := strings.ToLower(respBody(resp))
	for _, forbidden := range []string{"<form", "<input", "<textarea", "<button"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("page contains %q — the research page must have no edit form", forbidden)
		}
	}
}

// TestResearchPageDeniedRead: an invisible project answers the neutral
// not-found page with no outline chrome — existence stays hidden.
func TestResearchPageDeniedRead(t *testing.T) {
	stub := &stubService{err: projects.ErrProjectNotFound}
	resp := getResearchPage(t, stub, researchBase)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", resp.StatusCode, respBody(resp))
	}
	body := respBody(resp)
	if !strings.Contains(body, "Not found") {
		t.Errorf("error page lacks the neutral not-found title")
	}
	// The error page must not double up the Vary header (the route and the
	// shared error renderer would each add it once).
	if got := resp.Header.Values("Vary"); len(got) != 1 || !strings.Contains(got[0], "Accept") {
		t.Errorf("Vary values = %q, want exactly one Accept", got)
	}
	for _, chrome := range []string{"data-research-view", "data-question-id", "Research questions", "Findings"} {
		if strings.Contains(body, chrome) {
			t.Errorf("denied read renders page chrome (%q)", chrome)
		}
	}
}

// TestResearchPageJSONContract: a non-HTML client gets the outline as
// JSON with children nested; the HTML model is not consulted.
func TestResearchPageJSONContract(t *testing.T) {
	stub := &stubService{outline: cannedOutline()}
	ts, _, _, _ := newRSGTestServer(t, stub)
	resp, err := http.Get(ts.URL + researchBase)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s)", resp.StatusCode, respBody(resp))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	// Both representations share one URL — the JSON branch must key shared
	// caches on Accept exactly like the page branch does.
	if vary := resp.Header.Get("Vary"); !strings.Contains(vary, "Accept") {
		t.Errorf("Vary = %q, want Accept listed", vary)
	}
	payload := decodePayload(t, resp)
	if payload["project_id"] != rsgTestProjectID {
		t.Errorf("project_id = %v", payload["project_id"])
	}
	questions, ok := payload["questions"].([]any)
	if !ok || len(questions) != 1 {
		t.Fatalf("questions = %v", payload["questions"])
	}
	q1 := questions[0].(map[string]any)
	if q1["object_id"] != "q-1" || q1["question_state"] != "open" || q1["title"] != "Does the material adsorb CO2?" {
		t.Errorf("q1 = %v", q1)
	}
	children, ok := q1["children"].([]any)
	if !ok || len(children) != 1 {
		t.Errorf("q1 children = %v, want the nested sub-question", q1["children"])
	}
	if hyp, ok := q1["hypotheses"].([]any); !ok || len(hyp) != 1 {
		t.Errorf("q1 hypotheses = %v", q1["hypotheses"])
	}
	findings, ok := payload["findings"].([]any)
	if !ok || len(findings) != 1 {
		t.Fatalf("findings = %v", payload["findings"])
	}
	f1 := findings[0].(map[string]any)
	if f1["assessment"] != "accepted" || f1["finding_type"] != "trend" {
		t.Errorf("f1 = %v", f1)
	}
	counts, ok := payload["counts"].(map[string]any)
	if !ok || counts["questions"] != float64(2) || counts["other_objects"] != float64(2) {
		t.Errorf("counts = %v", payload["counts"])
	}
	if stub.researchOutlineCalls != 1 {
		t.Errorf("calls = %d, want 1", stub.researchOutlineCalls)
	}
}

// TestResearchPageEmptyProject: the empty outline renders the empty
// states, not an error.
func TestResearchPageEmptyProject(t *testing.T) {
	stub := &stubService{outline: rsg.ResearchOutline{ProjectID: rsgTestProjectID, ProjectSlug: "rsg-project", ProjectName: "RSG Project"}}
	resp := getResearchPage(t, stub, researchBase)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s)", resp.StatusCode, respBody(resp))
	}
	body := respBody(resp)
	if !strings.Contains(body, "No research questions yet") {
		t.Errorf("empty page lacks the empty state")
	}
	if strings.Contains(body, "Not linked to a question") {
		t.Errorf("empty page renders an unassigned section")
	}
	if !strings.Contains(body, "empty") {
		t.Errorf("empty page lacks the empty counts annotation")
	}
}

// TestResearchPageNoBranchRendersNoLink: a row whose version carries no
// branch renders as text — the page must not fabricate a branch
// coordinate for the drill-down link.
func TestResearchPageNoBranchRendersNoLink(t *testing.T) {
	out := cannedOutline()
	out.Questions[0].BranchID = ""
	out.Questions[0].Children[0].BranchID = ""
	out.Unassigned[0].BranchID = ""
	stub := &stubService{outline: out}
	resp := getResearchPage(t, stub, researchBase)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s)", resp.StatusCode, respBody(resp))
	}
	body := respBody(resp)
	if strings.Contains(body, "href=\"/api/v1/projects/"+rsgTestProjectID+"/branches//objects/") {
		t.Errorf("page fabricates an empty-branch drill-down link")
	}
	// The question title still renders, now as plain text.
	if !strings.Contains(body, "Does the material adsorb CO2?") {
		t.Errorf("branch-less question title missing")
	}
}
