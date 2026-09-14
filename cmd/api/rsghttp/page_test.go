package rsghttp

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/domain"
)

// The object detail page tests (T0210): the HTML representation of the
// object read route. The negative paths are the point — the page must
// never render an edit form, never leak raw payload markup, never render
// chrome for a denied read, and never answer a version number the service
// did not receive.

// cannedDetail is a two-version material with one outgoing relation.
func cannedDetail() rsg.ObjectDetail {
	v1 := domain.ScientificObjectVersion{
		ID: "ver-1", ObjectID: "obj-1", VersionNo: 1, StateID: "state-1",
		SchemaID: "https://open-rd.example/schemas/material.schema.json", SchemaVersion: "1",
		Title: "MOF-5", LifecycleState: domain.LifecycleActive,
		Payload:       json.RawMessage(`{"name":"MOF-5"}`),
		IntegrityHash: "hash-1", CreatedBy: "user-1",
		CreatedAt: time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC),
	}
	v2 := v1
	v2.ID = "ver-2"
	v2.VersionNo = 2
	v2.Title = "MOF-5 refined"
	v2.Payload = json.RawMessage(`{"name":"MOF-5","formula":"Zn4O(BDC)3"}`)
	return rsg.ObjectDetail{
		Project: domain.Project{ID: rsgTestProjectID, Slug: "rsg-project", Name: "RSG Project", Visibility: domain.VisibilityPublic},
		Branch:  domain.Branch{ID: "branch-1", ProjectID: rsgTestProjectID, Name: "main"},
		Object: domain.ScientificObject{
			ID: "obj-1", ProjectID: rsgTestProjectID, ObjectType: "material",
			CurrentVersionNo: 2, CreatedBy: "user-1",
			CreatedAt: time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC),
		},
		Versions: []domain.ScientificObjectVersion{v1, v2},
		Selected: v2,
		Relations: []rsg.ObjectRelationVersion{
			{
				Relation: domain.RelationVersion{
					ID: "rel-1", RelationID: "rel-1", VersionNo: 1, StateID: "state-1",
					RelationType:          "derived_from",
					SourceObjectVersionID: "ver-2", TargetObjectVersionID: "ver-9",
					CreatedBy: "user-1", CreatedAt: time.Date(2026, 9, 14, 11, 0, 0, 0, time.UTC),
				},
				Source: rsg.ObjectRelationEndpoint{ObjectID: "obj-1", ObjectType: "material", Title: "MOF-5 refined"},
				Target: rsg.ObjectRelationEndpoint{ObjectID: "obj-9", ObjectType: "dataset", Title: "isotherm series"},
			},
		},
		Creators: map[string]string{"user-1": "alice"},
	}
}

// getPage fetches the object route with a browser Accept header.
func getPage(t *testing.T, svc Service, path string) *http.Response {
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

func TestObjectDetailPageRenders(t *testing.T) {
	stub := &stubService{detail: cannedDetail()}
	resp := getPage(t, stub, objectsBase+"/objects/obj-1")
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
		// header: type / id / version / state (the task's required facts)
		`<span class="type-badge">material</span>`,
		`data-object-id="obj-1"`,
		`<summary data-version-summary>Version <strong>2</strong> of 2</summary>`,
		`data-lifecycle-state="active"`,
		"MOF-5 refined",
		// tabs: all five, metadata active — and each tab link carries its
		// own tab key, so switching tabs actually switches tabs
		`data-tab="metadata"`, `data-tab="relations"`, `data-tab="history"`, `data-tab="files"`, `data-tab="evidence"`,
		`tab=relations`, `tab=history`, `tab=files`, `tab=evidence`,
		// Work with Agent CTA and the pre-filled tool references (quotes
		// render escaped in a text node: &#34;name&#34;)
		`data-agent-menu`, `data-agent-summary`,
		"object.get", "object.create_version", "object_id",
		// version switch links
		"version=1", "version=2",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if stub.getObjectDetailCalls != 1 || stub.lastProjectID != rsgTestProjectID || stub.lastBranchID != "branch-1" || stub.lastObjectID != "obj-1" {
		t.Fatalf("calls = %d, projectID = %q, branchID = %q, objectID = %q",
			stub.getObjectDetailCalls, stub.lastProjectID, stub.lastBranchID, stub.lastObjectID)
	}
	if stub.lastVersionNo != nil {
		t.Errorf("version = %v, want nil (latest)", *stub.lastVersionNo)
	}
}

// TestObjectDetailPageHasNoEditForm is the acceptance criterion "无 web
// scientific edit form": the page carries no form, no input, no textarea
// and no button — nothing that could submit a scientific mutation.
func TestObjectDetailPageHasNoEditForm(t *testing.T) {
	stub := &stubService{detail: cannedDetail()}
	resp := getPage(t, stub, objectsBase+"/objects/obj-1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s)", resp.StatusCode, respBody(resp))
	}
	body := respBody(resp)
	for _, forbidden := range []string{"<form", "<input", "<textarea", "<button"} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Errorf("page contains %q — the detail page must have no edit form", forbidden)
		}
	}
	if !strings.Contains(body, "there is no scientific edit form on the web surface") {
		t.Errorf("page lacks the read-only footer note")
	}
}

// TestObjectDetailPageEscapesPayload: a hostile payload renders as text,
// never as markup (the page must be safe for anonymous readers of public
// projects).
func TestObjectDetailPageEscapesPayload(t *testing.T) {
	d := cannedDetail()
	d.Selected.Payload = json.RawMessage(`{"name":"<script>alert('xss')</script>","formula":"<img src=x onerror=alert(1)>"}`)
	d.Selected.Title = "<b>bold claim</b>"
	d.Versions[1] = d.Selected
	d.Relations[0].Target.Title = `"><svg onload=alert(1)>`
	stub := &stubService{detail: d}
	resp := getPage(t, stub, objectsBase+"/objects/obj-1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s)", resp.StatusCode, respBody(resp))
	}
	body := respBody(resp)
	for _, forbidden := range []string{"<script>alert", "<img src=x", `<svg onload`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("page renders raw markup %q — payload must be escaped", forbidden)
		}
	}
	if !strings.Contains(body, "&lt;script&gt;alert") {
		t.Errorf("page lacks the escaped payload text")
	}
}

func TestObjectDetailPageVersionSwitch(t *testing.T) {
	// The service answers the requested version; the page renders it.
	d := cannedDetail()
	d.Selected = d.Versions[0]
	stub := &stubService{detail: d}
	resp := getPage(t, stub, objectsBase+"/objects/obj-1?version=1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s)", resp.StatusCode, respBody(resp))
	}
	if stub.lastVersionNo == nil || *stub.lastVersionNo != 1 {
		t.Errorf("version = %v, want 1", stub.lastVersionNo)
	}
	if !strings.Contains(respBody(resp), `Version <strong>1</strong> of 2`) {
		t.Errorf("page does not show the selected version 1")
	}
}

func TestObjectDetailPageRelationsTab(t *testing.T) {
	stub := &stubService{detail: cannedDetail()}
	resp := getPage(t, stub, objectsBase+"/objects/obj-1?tab=relations")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s)", resp.StatusCode, respBody(resp))
	}
	body := respBody(resp)
	for _, want := range []string{
		`data-tab-panel="relations"`,
		"derived_from",    // the relation row's type
		"isotherm series", // the other endpoint's title
		"→",               // direction: this object is the source
		"v1",              // relation version
		"/objects/obj-9",  // link to the other object's page
		"alice",           // creator resolved through profiles
	} {
		if !strings.Contains(body, want) {
			t.Errorf("relations tab lacks %q", want)
		}
	}
	if strings.Contains(body, "Payload") {
		t.Errorf("relations tab renders metadata content")
	}
}

func TestObjectDetailPageVersionParamRejected(t *testing.T) {
	stub := &stubService{detail: cannedDetail()}
	for _, param := range []string{"version=0", "version=-3", "version=abc", "version=1.5"} {
		resp := getPage(t, stub, objectsBase+"/objects/obj-1?"+param)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body %s)", param, resp.StatusCode, respBody(resp))
		}
	}
	if stub.getObjectDetailCalls != 0 {
		t.Errorf("service called %d times for bad version params, want 0", stub.getObjectDetailCalls)
	}
}

// TestObjectDetailPageVersionPassesThroughUntouched: a positive value
// beyond the int4 range must reach the service exactly as typed — the
// page must neither truncate it nor answer in place (the store guards the
// int32 bound and answers not-found, after the visibility gate).
func TestObjectDetailPageVersionPassesThroughUntouched(t *testing.T) {
	stub := &stubService{detail: cannedDetail()}
	resp := getPage(t, stub, objectsBase+"/objects/obj-1?version=4294967301") // 2^32+5
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s)", resp.StatusCode, respBody(resp))
	}
	if stub.lastVersionNo == nil || *stub.lastVersionNo != 4294967301 {
		t.Errorf("version = %v, want 4294967301 passed through untouched", stub.lastVersionNo)
	}
}

func TestObjectDetailPageNotFound(t *testing.T) {
	stub := &stubService{err: projects.ErrProjectNotFound}
	resp := getPage(t, stub, objectsBase+"/objects/obj-1")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", resp.StatusCode, respBody(resp))
	}
	body := respBody(resp)
	if strings.Contains(body, "version-menu") || strings.Contains(body, "Work with Agent") {
		t.Errorf("denied read renders page chrome — existence must stay hidden")
	}
	if !strings.Contains(body, "Not found") {
		t.Errorf("error page lacks the neutral not-found title")
	}
}

func TestObjectDetailPageJSONContractUnchanged(t *testing.T) {
	stub := &stubService{object: cannedObject()}
	ts, _, _, _ := newRSGTestServer(t, stub)
	// A JSON client (no Accept) still gets the envelope, not HTML.
	resp, err := http.Get(ts.URL + objectsBase + "/objects/obj-1")
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
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["id"] != "obj-1" || payload["version_id"] != "ver-1" {
		t.Errorf("payload = %v", payload)
	}
	if stub.getObjectDetailCalls != 0 {
		t.Errorf("JSON read routed through the page model (%d detail calls)", stub.getObjectDetailCalls)
	}
}

func TestObjectDetailPageUnknownTabFallsBackToMetadata(t *testing.T) {
	stub := &stubService{detail: cannedDetail()}
	resp := getPage(t, stub, objectsBase+"/objects/obj-1?tab=bogus")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s)", resp.StatusCode, respBody(resp))
	}
	body := respBody(resp)
	if !strings.Contains(body, `data-tab-panel="metadata"`) {
		t.Errorf("unknown tab did not fall back to metadata")
	}
	if !strings.Contains(body, "Payload") {
		t.Errorf("metadata tab content missing")
	}
}

func TestWantsHTML(t *testing.T) {
	cases := []struct {
		accept string
		want   bool
	}{
		{"", false},
		{"application/json", false},
		{"text/html", true},
		{"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8", true},
		{"application/xhtml+xml", true},
		{"application/json, text/plain", false},
	}
	for _, tt := range cases {
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		if tt.accept != "" {
			req.Header.Set("Accept", tt.accept)
		}
		if got := wantsHTML(req); got != tt.want {
			t.Errorf("wantsHTML(%q) = %v, want %v", tt.accept, got, tt.want)
		}
	}
}
