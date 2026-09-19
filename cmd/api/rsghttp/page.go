package rsghttp

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/rsg/provenance"
)

// The Scientific Object Detail page (T0210): the HTML representation of
// the object read route, served when the caller asks for text/html — a
// browser navigation. The JSON contract on the same route is unchanged,
// and the page is exactly as visible as the object: it runs the same
// service read (project visibility gate + branch + object ownership) as
// the JSON path, so a denied reader gets the same existence-hidden
// outcome (a neutral not-found page, no shell chrome).
//
// The page is deliberately read-only (acceptance: 无 web scientific edit
// form): no form, no input, no mutation affordance of any kind. Object
// mutation is represented by the "Work with Agent" CTA, which shows the
// MCP tool references pre-filled with this object's coordinates — the
// agent is the mutation entry point (docs/06 §9), the web surface stays
// browsable and governable.
//
// Design notes (L1, recorded for the Supervisor):
//   - Server-rendered, zero JavaScript: version switching and tabs are
//     plain links with query parameters (?version=N, ?tab=...), and the
//     switcher is a native <details> dropdown — keyboard reachable by
//     construction.
//   - T0210's allowed scope excludes apps/web (the Next.js shell), so the
//     detail page lives here, on the Go API, at the object's canonical
//     URL. The research outline (T0211) links to these pages.

//go:embed object_detail.html
var objectDetailTemplateSrc string

var objectDetailTemplate = template.Must(template.New("objectDetail").Parse(objectDetailTemplateSrc))

// objectPageErrorTemplate is the neutral error page HTML requests get
// (same status/code/message the JSON envelope carries — no chrome, no
// shell, nothing that leaks more than the envelope does).
var objectPageErrorTemplate = template.Must(template.New("objectPageError").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} — POST</title>
<style>
  body { margin: 0; background: #ffffff; color: #1f2328;
    font: 14px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", "Noto Sans", Helvetica, Arial, sans-serif; }
  .state { max-width: 1280px; margin: 0 auto; padding: 48px 24px; text-align: center; }
  h1 { font-size: 20px; font-weight: 600; margin: 0 0 8px; }
  p { color: #59636e; margin: 0 0 4px; }
  code { font-size: 12px; color: #59636e; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
</style>
</head>
<body>
<div class="state">
  <h1>{{.Title}}</h1>
  <p>{{.Message}}</p>
  <code>{{.Status}} · {{.Code}}</code>
</div>
</body>
</html>`))

// wantsHTML reports whether the caller asked for the page representation
// (a browser navigation) rather than the JSON contract. The test is a
// simple media-type presence check on the Accept header: browsers send
// text/html on navigation, API clients do not.
func wantsHTML(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		mediaType := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if mediaType == "text/html" || mediaType == "application/xhtml+xml" {
			return true
		}
	}
	return false
}

// pageTabs are the detail page's tab keys, in display order. Unknown tab
// values fall back to metadata (a navigation is never an error).
//
// Provenance and Evidence are TWO tabs, not one tab with two views: docs/42
// §Scientific Object Detail names them as two items of the page's body, and
// the acceptance criterion for T0507 is that a reader does not confuse them
// (docs/10 §1) — a reader who has to change a control inside one tab to see
// the other has already been told they are the same surface. Each tab states
// what it answers and links to the other one by name.
var pageTabs = []struct{ Key, Label string }{
	{"metadata", "Metadata"},
	{"relations", "Relations"},
	{graphProvenance, "Provenance"},
	{graphEvidence, "Evidence"},
	{"history", "History"},
	{"files", "Files"},
}

// pageTab normalizes the ?tab= value to one of the page tabs.
func pageTab(r *http.Request) string {
	tab := r.URL.Query().Get("tab")
	for _, t := range pageTabs {
		if tab == t.Key {
			return t.Key
		}
	}
	return "metadata"
}

// pageVersionParam parses the ?version= value: absent selects the latest
// version, a positive integer selects that version, anything that is not
// a positive integer is answered in place with 400. A positive value is
// deliberately passed through untouched, even beyond the int4 range: the
// version lookup guards the int32 bound and answers "object version not
// found" after the visibility gate — answering it here would skip that
// gate, and truncating it upstream once silently rendered the wrong
// version (2^32+5 as version 5).
func pageVersionParam(w http.ResponseWriter, r *http.Request) (*int, bool) {
	raw := r.URL.Query().Get("version")
	if raw == "" {
		return nil, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		renderObjectPageError(w, r, http.StatusBadRequest, rsg.CodeValidation,
			"version must be a positive integer")
		return nil, false
	}
	return &n, true
}

// handleObjectDetailPage renders the read-only detail page for browser
// requests on the object read route.
func (h *handlers) handleObjectDetailPage(w http.ResponseWriter, r *http.Request) {
	projectID, branchID, objectID := r.PathValue("projectId"), r.PathValue("branchId"), r.PathValue("objectId")
	versionNo, ok := pageVersionParam(w, r)
	if !ok {
		return
	}
	detail, err := h.svc.GetObjectDetail(r.Context(), reader(r), projectID, branchID, objectID, versionNo)
	if err != nil {
		status, code, message := rsgErrorOutcome(err)
		renderObjectPageError(w, r, status, code, message)
		return
	}
	// The graph tabs read through T0505's and T0506's own services, from the
	// version this page resolved — never from the raw ?version= parameter,
	// and never at all on a tab that does not show them: a page load should
	// cost the queries the page renders.
	selected := detail.Selected.VersionNo
	model := objectPageModelFrom(r, detail, versionNo)
	tabHref := pageTabHref(r)
	if model.Tab == graphProvenance {
		model.Provenance = provenancePanelFor(r.Context(), h.provenance, projectID, objectID, &selected,
			pageDirection(r), tabHref)
	}
	if model.Tab == graphEvidence {
		model.Evidence = evidencePanelFor(r.Context(), h.evidence, projectID, objectID, &selected, tabHref)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Add("Vary", "Accept")
	if err := objectDetailTemplate.Execute(w, model); err != nil {
		// The template parses at init and the model is plain data — an
		// execute failure is a programming error, and the response is
		// already partially written.
		slog.Error("rsghttp: object detail page render failed", "error", err)
	}
}

// pageDirection reads the provenance tab's ?direction= parameter: upstream
// (the default) walks the origins, downstream walks the dependents. Anything
// else is treated as the default — a navigation is never an error, the same
// rule pageTab follows.
func pageDirection(r *http.Request) provenance.WalkDirection {
	dir, ok := provenance.ParseWalkDirection(r.URL.Query().Get("direction"))
	if !ok {
		return provenance.WalkUpstream
	}
	return dir
}

// pageTabHref builds a link to another tab of the same object page, keeping
// the selected version and (for the provenance tab) the walk direction. Every
// href on the page is built from the request path, so the page never hard-codes
// the mount prefix.
func pageTabHref(r *http.Request) func(tab, direction string) string {
	base := r.URL.Path
	version := r.URL.Query().Get("version")
	return func(tab, direction string) string {
		query := url.Values{}
		query.Set("tab", tab)
		if version != "" {
			query.Set("version", version)
		}
		if direction != "" {
			query.Set("direction", direction)
		}
		return base + "?" + query.Encode()
	}
}

// renderObjectPageError answers an HTML request with the neutral error
// page carrying the same status/code/message the JSON envelope answers.
func renderObjectPageError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	title := http.StatusText(status)
	if status == http.StatusNotFound {
		title = "Not found"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Add("Vary", "Accept")
	w.WriteHeader(status)
	if err := objectPageErrorTemplate.Execute(w, map[string]string{
		"Title":   title,
		"Message": message,
		"Status":  strconv.Itoa(status),
		"Code":    code,
	}); err != nil {
		slog.Error("rsghttp: object detail error page render failed", "error", err)
	}
}

// objectPageModel is the template's data shape.
type objectPageModel struct {
	ProjectSlug       string
	ProjectName       string
	BranchName        string
	ObjectType        string
	Title             string
	ObjectID          string
	StateID           string
	LifecycleState    string
	LifecycleClass    string
	SelectedVersionNo int
	LatestVersionNo   int
	VersionLinks      []objectPageVersion
	Tab               string
	TabLinks          []objectPageTab
	Facts             []objectPageFact
	PayloadJSON       string
	Relations         []objectPageRelation
	AgentGetJSON      string
	AgentVersionJSON  string
	// The two graph tabs (T0507). At most one is filled: the one the page is
	// showing. A tab the reader did not open costs no read and renders
	// nothing — there is no "other graph" data lurking in this page.
	Provenance provenancePanel
	Evidence   evidencePanel
}

type objectPageVersion struct {
	No      int
	Title   string
	Href    string
	Current bool
}

type objectPageTab struct {
	Key    string
	Label  string
	Href   string
	Active bool
}

type objectPageFact struct {
	Label  string
	Value  string
	IsCode bool
}

type objectPageRelation struct {
	Direction  string // "→" outgoing (this object is the source), "←" incoming
	Type       string
	OtherType  string
	OtherTitle string
	OtherHref  string
	VersionNo  int
	CreatedBy  string
	CreatedAt  string
}

// objectPageModelFrom builds the template model from the service read.
// Every href is built from the request path so the page never hard-codes
// the mount prefix, and every dynamic value is plain data — html/template
// escapes it all.
func objectPageModelFrom(r *http.Request, d rsg.ObjectDetail, versionNo *int) objectPageModel {
	tab := pageTab(r)
	base := r.URL.Path // the object's canonical URL, already escaped

	model := objectPageModel{
		ProjectSlug:       d.Project.Slug,
		ProjectName:       d.Project.Name,
		BranchName:        d.Branch.Name,
		ObjectType:        d.Object.ObjectType,
		Title:             d.Selected.Title,
		ObjectID:          d.Object.ID,
		StateID:           d.Selected.StateID,
		LifecycleState:    string(d.Selected.LifecycleState),
		LifecycleClass:    string(d.Selected.LifecycleState),
		SelectedVersionNo: d.Selected.VersionNo,
		LatestVersionNo:   d.Object.CurrentVersionNo,
		Tab:               tab,
		PayloadJSON:       prettyJSON(d.Selected.Payload),
	}

	for _, v := range d.Versions {
		query := url.Values{}
		query.Set("tab", tab)
		query.Set("version", strconv.Itoa(v.VersionNo))
		model.VersionLinks = append(model.VersionLinks, objectPageVersion{
			No:      v.VersionNo,
			Title:   v.Title,
			Href:    base + "?" + query.Encode(),
			Current: v.VersionNo == d.Selected.VersionNo,
		})
	}

	for _, t := range pageTabs {
		// Each tab link names its own tab (and keeps the selected version)
		// — navigation between tabs is a plain query-parameter link.
		query := url.Values{}
		query.Set("tab", t.Key)
		if versionNo != nil {
			query.Set("version", strconv.Itoa(*versionNo))
		}
		model.TabLinks = append(model.TabLinks, objectPageTab{
			Key:    t.Key,
			Label:  t.Label,
			Href:   base + "?" + query.Encode(),
			Active: tab == t.Key,
		})
	}

	model.Facts = []objectPageFact{
		{Label: "Project", Value: d.Project.Name},
		{Label: "Branch", Value: d.Branch.Name},
		{Label: "Object type", Value: d.Object.ObjectType},
		{Label: "Object ID", Value: d.Object.ID, IsCode: true},
		{Label: "Schema", Value: d.Selected.SchemaID + " @" + d.Selected.SchemaVersion, IsCode: true},
		{Label: "Lifecycle state", Value: string(d.Selected.LifecycleState)},
		{Label: "Created by", Value: creatorLabel(d, d.Selected.CreatedBy)},
		{Label: "Created at", Value: d.Selected.CreatedAt.Format(time.RFC3339)},
		{Label: "Current version", Value: strconv.Itoa(d.Object.CurrentVersionNo), IsCode: true},
		{Label: "State", Value: d.Selected.StateID, IsCode: true},
		{Label: "Integrity hash", Value: d.Selected.IntegrityHash, IsCode: true},
	}

	model.Relations = make([]objectPageRelation, 0, len(d.Relations))
	versionIDs := make(map[string]struct{}, len(d.Versions))
	for _, v := range d.Versions {
		versionIDs[v.ID] = struct{}{}
	}
	for _, rv := range d.Relations {
		_, isSource := versionIDs[rv.Relation.SourceObjectVersionID]
		// Outgoing: this object is the source, the other endpoint is the
		// target. Incoming: the reverse.
		other := rv.Target
		direction := "→"
		if !isSource {
			other = rv.Source
			direction = "←"
		}
		model.Relations = append(model.Relations, objectPageRelation{
			Direction:  direction,
			Type:       rv.Relation.RelationType,
			OtherType:  other.ObjectType,
			OtherTitle: other.Title,
			OtherHref:  objectHref(d.Project.ID, d.Branch.ID, other.ObjectID),
			VersionNo:  rv.Relation.VersionNo,
			CreatedBy:  creatorLabel(d, rv.Relation.CreatedBy),
			CreatedAt:  rv.Relation.CreatedAt.Format(time.RFC3339),
		})
	}

	model.AgentGetJSON = mustIndentJSON(map[string]any{
		"name": "object.get",
		"args": map[string]any{
			"object_id": d.Object.ID,
			"version":   d.Selected.VersionNo,
		},
	})
	model.AgentVersionJSON = mustIndentJSON(map[string]any{
		"name": "object.create_version",
		"args": map[string]any{
			"project_id":       d.Project.ID,
			"branch_id":        d.Branch.ID,
			"object_id":        d.Object.ID,
			"expected_version": d.Selected.VersionNo,
			"patch":            map[string]any{},
		},
	})
	return model
}

// objectHref builds the canonical object URL for a project/branch/object
// triple (the relations tab links to the other endpoint's page). An empty
// coordinate answers "" — a caller without a full triple must render the
// title without a link rather than fabricate a URL (the research outline
// (T0211) links objects whose versions carry no branch this way).
func objectHref(projectID, branchID, objectID string) string {
	if projectID == "" || branchID == "" || objectID == "" {
		return ""
	}
	return fmt.Sprintf("/api/v1/projects/%s/branches/%s/objects/%s",
		url.PathEscape(projectID), url.PathEscape(branchID), url.PathEscape(objectID))
}

// creatorLabel renders a creator: the profile handle when it resolves,
// the raw id otherwise (a creator may predate the profile surface).
func creatorLabel(d rsg.ObjectDetail, userID string) string {
	if handle, ok := d.Creators[userID]; ok && handle != "" {
		return handle
	}
	return userID
}

// prettyJSON indents a payload for the page; a payload that fails to
// re-indent renders verbatim (it round-tripped through jsonb already).
func prettyJSON(raw []byte) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

// mustIndentJSON marshals with indentation; the inputs are plain strings
// and numbers, so a failure is a programming error the page renders as an
// empty object rather than crashing the request. HTML characters are not
// escaped in the JSON itself — the template escapes the text node on
// render, and the browser then shows the JSON with its real characters
// (< reads badly in a tool reference a person copies).
func mustIndentJSON(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return "{}"
	}
	return strings.TrimRight(buf.String(), "\n")
}
