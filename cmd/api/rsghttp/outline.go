package rsghttp

import (
	_ "embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/rsg"
)

// The Research page (T0211): the HTML representation of the project's
// research outline, served on GET /api/v1/projects/{projectId}/research
// when the caller asks for text/html (a browser navigation). Non-HTML
// clients get the JSON outline contract on the same route — the same
// content negotiation the object detail route uses, so the agent/MCP
// surface and the future research map (T1102) read the outline as data.
//
// The outline is aggregated by research question (a tree through
// parent_question_id) and by finding — never an object type or files
// dump (acceptance: 默认不是 files/type dump). Counts are auxiliary
// annotations. Each question drills down to its object detail page
// (acceptance: 可从 question drill-down), and the whole page is a
// semantic HTML outline — the accessible list fallback the graph view
// (docs/06 §10) must have; the canvas itself arrives with the research
// map milestone (T1102).
//
// The page is exactly as visible as its project: the service runs the
// same requireRead entry gate as the RSG query, so a denied reader gets
// the same existence-hidden neutral not-found page. Read-only by
// construction: no form, no input, no mutation affordance (the web
// surface stays browsable; scientific writes go through the agent/API).

//go:embed research.html
var researchTemplateSrc string

var researchTemplate = template.Must(template.New("research").Parse(researchTemplateSrc))

// researchPayload is the JSON contract for non-HTML clients.
type researchPayload struct {
	ProjectID   string                    `json:"project_id"`
	ProjectSlug string                    `json:"project_slug"`
	ProjectName string                    `json:"project_name"`
	Counts      researchCountsPayload     `json:"counts"`
	Questions   []researchQuestionPayload `json:"questions"`
	Findings    []researchFindingPayload  `json:"findings"`
	Unassigned  []researchRefPayload      `json:"unassigned"`
}

type researchCountsPayload struct {
	Questions    int `json:"questions"`
	Findings     int `json:"findings"`
	Hypotheses   int `json:"hypotheses"`
	Claims       int `json:"claims"`
	OtherObjects int `json:"other_objects"`
}

type researchQuestionPayload struct {
	ObjectID         string                    `json:"object_id"`
	VersionID        string                    `json:"version_id"`
	Title            string                    `json:"title"`
	Statement        string                    `json:"statement"`
	QuestionState    string                    `json:"question_state"`
	ParentQuestionID string                    `json:"parent_question_id"`
	Children         []researchQuestionPayload `json:"children"`
	Hypotheses       []researchRefPayload      `json:"hypotheses"`
	Findings         []researchRefPayload      `json:"findings"`
	OtherObjects     []researchRefPayload      `json:"other_objects"`
}

type researchFindingPayload struct {
	ObjectID    string                 `json:"object_id"`
	VersionID   string                 `json:"version_id"`
	Title       string                 `json:"title"`
	Statement   string                 `json:"statement"`
	FindingType string                 `json:"finding_type"`
	Assessment  string                 `json:"assessment"`
	Claims      []researchClaimPayload `json:"claims"`
	Questions   []researchRefPayload   `json:"questions"`
}

type researchClaimPayload struct {
	ObjectID  string `json:"object_id,omitempty"`
	VersionID string `json:"version_id"`
	Title     string `json:"title"`
	Resolved  bool   `json:"resolved"`
}

type researchRefPayload struct {
	ObjectID   string `json:"object_id"`
	ObjectType string `json:"object_type"`
	Title      string `json:"title"`
}

// handleResearch: GET /api/v1/projects/{projectId}/research — the
// outline as JSON, or the page for browser navigations.
func (h *handlers) handleResearch(w http.ResponseWriter, r *http.Request) {
	if wantsHTML(r) {
		h.handleResearchPage(w, r)
		return
	}
	// Both representations share one URL — key shared caches on Accept so
	// the page and the JSON contract never cross-serve (the object detail
	// route predates this rule; the new route does not copy that bug).
	// Set here rather than at the route top: the HTML error path renders
	// through the shared error page, which adds its own Vary.
	w.Header().Add("Vary", "Accept")
	outline, err := h.svc.ResearchOutline(r.Context(), reader(r), r.PathValue("projectId"))
	if err != nil {
		rsgError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, researchPayloadFrom(outline))
}

// researchPayloadFrom renders the outline as the wire contract (children
// nested, same shape as the page reads).
func researchPayloadFrom(out rsg.ResearchOutline) researchPayload {
	p := researchPayload{
		ProjectID:   out.ProjectID,
		ProjectSlug: out.ProjectSlug,
		ProjectName: out.ProjectName,
		Counts:      researchCountsPayload(out.Counts),
		Questions:   make([]researchQuestionPayload, 0, len(out.Questions)),
		Findings:    make([]researchFindingPayload, 0, len(out.Findings)),
		Unassigned:  make([]researchRefPayload, 0, len(out.Unassigned)),
	}
	for _, q := range out.Questions {
		p.Questions = append(p.Questions, researchQuestionPayloadFrom(q))
	}
	for _, f := range out.Findings {
		p.Findings = append(p.Findings, researchFindingPayloadFrom(f))
	}
	for _, ref := range out.Unassigned {
		p.Unassigned = append(p.Unassigned, researchRefPayloadFrom(ref))
	}
	return p
}

func researchQuestionPayloadFrom(q rsg.OutlineQuestion) researchQuestionPayload {
	p := researchQuestionPayload{
		ObjectID:         q.ObjectID,
		VersionID:        q.VersionID,
		Title:            q.Title,
		Statement:        q.Statement,
		QuestionState:    q.QuestionState,
		ParentQuestionID: q.ParentQuestionID,
		Children:         make([]researchQuestionPayload, 0, len(q.Children)),
		Hypotheses:       make([]researchRefPayload, 0, len(q.Hypotheses)),
		Findings:         make([]researchRefPayload, 0, len(q.Findings)),
		OtherObjects:     make([]researchRefPayload, 0, len(q.OtherObjects)),
	}
	for _, c := range q.Children {
		p.Children = append(p.Children, researchQuestionPayloadFrom(c))
	}
	for _, ref := range q.Hypotheses {
		p.Hypotheses = append(p.Hypotheses, researchRefPayloadFrom(ref))
	}
	for _, ref := range q.Findings {
		p.Findings = append(p.Findings, researchRefPayloadFrom(ref))
	}
	for _, ref := range q.OtherObjects {
		p.OtherObjects = append(p.OtherObjects, researchRefPayloadFrom(ref))
	}
	return p
}

func researchFindingPayloadFrom(f rsg.OutlineFinding) researchFindingPayload {
	p := researchFindingPayload{
		ObjectID:    f.ObjectID,
		VersionID:   f.VersionID,
		Title:       f.Title,
		Statement:   f.Statement,
		FindingType: f.FindingType,
		Assessment:  f.Assessment,
		Claims:      make([]researchClaimPayload, 0, len(f.Claims)),
		Questions:   make([]researchRefPayload, 0, len(f.Questions)),
	}
	for _, c := range f.Claims {
		p.Claims = append(p.Claims, researchClaimPayload{
			ObjectID: c.ObjectID, VersionID: c.VersionID, Title: c.Title, Resolved: c.Resolved,
		})
	}
	for _, ref := range f.Questions {
		p.Questions = append(p.Questions, researchRefPayloadFrom(ref))
	}
	return p
}

func researchRefPayloadFrom(ref rsg.OutlineObjectRef) researchRefPayload {
	return researchRefPayload{ObjectID: ref.ObjectID, ObjectType: ref.ObjectType, Title: ref.Title}
}

// handleResearchPage renders the research page for browser requests.
func (h *handlers) handleResearchPage(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectId")
	outline, err := h.svc.ResearchOutline(r.Context(), reader(r), projectID)
	if err != nil {
		status, code, message := rsgErrorOutcome(err)
		renderObjectPageError(w, r, status, code, message)
		return
	}
	writeDocumentHeaders(w)
	if err := researchTemplate.Execute(w, researchPageModelFrom(r, outline)); err != nil {
		// The template parses at init and the model is plain data — an
		// execute failure is a programming error, and the response is
		// already partially written.
		slog.Error("rsghttp: research page render failed", "error", err)
	}
}

// The outline's two view keys. They are named because the research map
// (T1102) reads the same coordinate to decide which axis it draws; one
// spelling per view everywhere.
const (
	researchViewQuestions = "questions"
	researchViewFindings  = "findings"
)

// researchViews are the outline views, in display order (docs/42: By
// Question / By Finding). Unknown values fall back to questions — a
// navigation is never an error.
var researchViews = []struct{ Key, Label string }{
	{researchViewQuestions, "By Question"},
	{researchViewFindings, "By Finding"},
}

// researchView normalizes the ?view= value.
func researchView(r *http.Request) string {
	view := r.URL.Query().Get("view")
	for _, v := range researchViews {
		if view == v.Key {
			return v.Key
		}
	}
	return researchViewQuestions
}

// researchPageModel is the template's data shape.
type researchPageModel struct {
	ProjectSlug string
	ProjectName string
	View        string
	ViewLinks   []researchPageViewLink
	CountsLine  string
	// Map is the Research Map (T1102): the coarse aggregated picture, its
	// accessible table and the selected node's detail. It is built from the
	// SAME outline the list columns render — the map adds no read.
	Map        researchMap
	Questions  []researchPageQuestion
	Findings   []researchPageFinding
	Unassigned []researchPageRef
}

type researchPageViewLink struct {
	Key    string
	Label  string
	Href   string
	Active bool
}

type researchPageQuestion struct {
	ObjectID      string
	Title         string
	Statement     string
	QuestionState string
	StateClass    string
	Href          string
	CountsLine    string
	Children      []researchPageQuestion
	Hypotheses    []researchPageRef
	Findings      []researchPageRef
	OtherObjects  []researchPageRef
}

type researchPageFinding struct {
	ObjectID    string
	Title       string
	Statement   string
	FindingType string
	Assessment  string
	Href        string
	Claims      []researchPageClaim
	Questions   []researchPageRef
}

type researchPageClaim struct {
	Title     string
	VersionID string
	Resolved  bool
	Href      string
}

type researchPageRef struct {
	ObjectType string
	Title      string
	Href       string
}

// researchPageModelFrom builds the template model. Every href is built
// from the outline's own branch coordinates (a row whose version carries
// no branch renders without a link — never a fabricated coordinate), and
// every dynamic value is plain data — html/template escapes it all.
func researchPageModelFrom(r *http.Request, out rsg.ResearchOutline) researchPageModel {
	view := researchView(r)
	base := r.URL.Path // the research page's canonical URL

	model := researchPageModel{
		ProjectSlug: out.ProjectSlug,
		ProjectName: out.ProjectName,
		View:        view,
		CountsLine:  researchCountsLine(out.Counts),
		Map:         buildResearchMap(out, view, researchMapPath(r), researchMapLinks{basePath: base, projectID: out.ProjectID}),
		Questions:   make([]researchPageQuestion, 0, len(out.Questions)),
		Findings:    make([]researchPageFinding, 0, len(out.Findings)),
		Unassigned:  make([]researchPageRef, 0, len(out.Unassigned)),
	}

	for _, v := range researchViews {
		query := url.Values{}
		query.Set("view", v.Key)
		model.ViewLinks = append(model.ViewLinks, researchPageViewLink{
			Key:    v.Key,
			Label:  v.Label,
			Href:   base + "?" + query.Encode(),
			Active: view == v.Key,
		})
	}

	for _, q := range out.Questions {
		model.Questions = append(model.Questions, researchPageQuestionFrom(out.ProjectID, q))
	}
	for _, f := range out.Findings {
		model.Findings = append(model.Findings, researchPageFindingFrom(out.ProjectID, f))
	}
	for _, ref := range out.Unassigned {
		model.Unassigned = append(model.Unassigned, researchPageRefFrom(out.ProjectID, ref))
	}
	return model
}

func researchPageQuestionFrom(projectID string, q rsg.OutlineQuestion) researchPageQuestion {
	p := researchPageQuestion{
		ObjectID:      q.ObjectID,
		Title:         q.Title,
		Statement:     q.Statement,
		QuestionState: q.QuestionState,
		StateClass:    q.QuestionState,
		Href:          objectHref(projectID, q.BranchID, q.ObjectID),
		CountsLine:    researchQuestionCountsLine(q),
		Children:      make([]researchPageQuestion, 0, len(q.Children)),
		Hypotheses:    make([]researchPageRef, 0, len(q.Hypotheses)),
		Findings:      make([]researchPageRef, 0, len(q.Findings)),
		OtherObjects:  make([]researchPageRef, 0, len(q.OtherObjects)),
	}
	for _, c := range q.Children {
		p.Children = append(p.Children, researchPageQuestionFrom(projectID, c))
	}
	for _, ref := range q.Hypotheses {
		p.Hypotheses = append(p.Hypotheses, researchPageRefFrom(projectID, ref))
	}
	for _, ref := range q.Findings {
		p.Findings = append(p.Findings, researchPageRefFrom(projectID, ref))
	}
	for _, ref := range q.OtherObjects {
		p.OtherObjects = append(p.OtherObjects, researchPageRefFrom(projectID, ref))
	}
	return p
}

func researchPageFindingFrom(projectID string, f rsg.OutlineFinding) researchPageFinding {
	p := researchPageFinding{
		ObjectID:    f.ObjectID,
		Title:       f.Title,
		Statement:   f.Statement,
		FindingType: f.FindingType,
		Assessment:  f.Assessment,
		Href:        objectHref(projectID, f.BranchID, f.ObjectID),
		Claims:      make([]researchPageClaim, 0, len(f.Claims)),
		Questions:   make([]researchPageRef, 0, len(f.Questions)),
	}
	for _, c := range f.Claims {
		p.Claims = append(p.Claims, researchPageClaim{
			Title:     c.Title,
			VersionID: c.VersionID,
			Resolved:  c.Resolved,
			Href:      objectHref(projectID, c.BranchID, c.ObjectID),
		})
	}
	for _, ref := range f.Questions {
		p.Questions = append(p.Questions, researchPageRefFrom(projectID, ref))
	}
	return p
}

func researchPageRefFrom(projectID string, ref rsg.OutlineObjectRef) researchPageRef {
	return researchPageRef{
		ObjectType: ref.ObjectType,
		Title:      ref.Title,
		Href:       objectHref(projectID, ref.BranchID, ref.ObjectID),
	}
}

// researchCountsLine renders the header's auxiliary summary (empty string
// for an empty project).
func researchCountsLine(c rsg.OutlineCounts) string {
	parts := make([]string, 0, 5)
	if c.Questions > 0 {
		parts = append(parts, pluralCount(c.Questions, "question"))
	}
	if c.Findings > 0 {
		parts = append(parts, pluralCount(c.Findings, "finding"))
	}
	if c.Hypotheses > 0 {
		parts = append(parts, pluralCount(c.Hypotheses, "hypothesis", "hypotheses"))
	}
	if c.Claims > 0 {
		parts = append(parts, pluralCount(c.Claims, "claim"))
	}
	if c.OtherObjects > 0 {
		parts = append(parts, pluralCount(c.OtherObjects, "other object"))
	}
	return joinCounts(parts)
}

// researchQuestionCountsLine renders one question's auxiliary annotation
// ("" when nothing addresses the question).
func researchQuestionCountsLine(q rsg.OutlineQuestion) string {
	parts := make([]string, 0, 3)
	if len(q.Findings) > 0 {
		parts = append(parts, pluralCount(len(q.Findings), "finding"))
	}
	if len(q.Hypotheses) > 0 {
		parts = append(parts, pluralCount(len(q.Hypotheses), "hypothesis", "hypotheses"))
	}
	if len(q.OtherObjects) > 0 {
		parts = append(parts, pluralCount(len(q.OtherObjects), "other object"))
	}
	return joinCounts(parts)
}

// pluralCount renders "n label" with the label pluralized when n != 1.
func pluralCount(n int, singular string, plural ...string) string {
	label := singular + "s"
	if len(plural) > 0 {
		label = plural[0]
	}
	if n == 1 {
		label = singular
	}
	return fmt.Sprintf("%d %s", n, label)
}

// joinCounts joins the non-empty count parts with " · ".
func joinCounts(parts []string) string {
	return strings.Join(parts, " · ")
}
