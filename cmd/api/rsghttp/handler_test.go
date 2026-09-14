package rsghttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/relations"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/sciobjects"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/rsg/semantics"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// The handler tests prove the transport wiring through the REAL guard:
// real signup issues the session, writes additionally require the
// session-bound CSRF token, and the routes pass the parsed path values to
// the rsg service. The stub service records what the handlers called it
// with and answers canned outcomes, so route parsing, wire shapes and
// error mapping are testable without a database. The rsg service rules
// themselves (authorization order, CAS, semantic checks) are covered by
// internal/application/rsg/service_test.go.

const rsgTestProjectID = "11111111-2222-4333-8444-555555555555"

// stubService is a canned Service: every command records its inputs and
// answers the configured result or error.
type stubService struct {
	branch   domain.Branch
	object   rsg.ObjectResult
	version  rsg.ObjectVersionResult
	relation rsg.RelationResult
	err      error

	createBranchCalls        int
	createObjectCalls        int
	createObjectVersionCalls int
	createRelationCalls      int
	getObjectCalls           int

	lastProjectID string
	lastBranchID  string
	lastObjectID  string
	lastInput     any
}

func (s *stubService) CreateBranch(_ context.Context, _ domain.User, projectID string, in rsg.CreateBranchInput) (domain.Branch, error) {
	s.createBranchCalls++
	s.lastProjectID, s.lastInput = projectID, in
	return s.branch, s.err
}

func (s *stubService) CreateObject(_ context.Context, _ domain.User, projectID, branchID string, in rsg.CreateObjectInput) (rsg.ObjectResult, error) {
	s.createObjectCalls++
	s.lastProjectID, s.lastBranchID, s.lastInput = projectID, branchID, in
	return s.object, s.err
}

func (s *stubService) CreateObjectVersion(_ context.Context, _ domain.User, projectID, branchID, objectID string, in rsg.CreateObjectVersionInput) (rsg.ObjectVersionResult, error) {
	s.createObjectVersionCalls++
	s.lastProjectID, s.lastBranchID, s.lastObjectID, s.lastInput = projectID, branchID, objectID, in
	return s.version, s.err
}

func (s *stubService) CreateRelation(_ context.Context, _ domain.User, projectID, branchID string, in rsg.CreateRelationInput) (rsg.RelationResult, error) {
	s.createRelationCalls++
	s.lastProjectID, s.lastBranchID, s.lastInput = projectID, branchID, in
	return s.relation, s.err
}

func (s *stubService) GetObject(_ context.Context, _ projects.Reader, projectID, branchID, objectID string) (rsg.ObjectResult, error) {
	s.getObjectCalls++
	s.lastProjectID, s.lastBranchID, s.lastObjectID = projectID, branchID, objectID
	return s.object, s.err
}

func cannedBranch() domain.Branch {
	return domain.Branch{
		ID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", ProjectID: rsgTestProjectID, Name: "main",
		Visibility: domain.BranchVisibilityPrivate, GitRef: "refs/heads/main",
		BaseStateID: strPtr("state-1"), Lifecycle: domain.BranchLifecycleActive,
		CreatedBy: "user-1", CreatedAt: time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC),
	}
}

func cannedObject() rsg.ObjectResult {
	return rsg.ObjectResult{
		Object: domain.ScientificObject{
			ID: "obj-1", ProjectID: rsgTestProjectID, ObjectType: "material",
			CurrentVersionNo: 1, CreatedBy: "user-1", CreatedAt: time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC),
		},
		Version: domain.ScientificObjectVersion{
			ID: "ver-1", ObjectID: "obj-1", VersionNo: 1, StateID: "state-1",
			SchemaID: "https://open-rd.example/schemas/material.schema.json", SchemaVersion: "1",
			Title: "MOF-5", LifecycleState: domain.LifecycleActive,
			Payload: json.RawMessage(`{"name":"MOF-5"}`),
		},
		Hints: []semantics.Hint{{Code: semantics.HintClaimCompound, Message: "a claim may be compound"}},
	}
}

func cannedVersion() rsg.ObjectVersionResult {
	return rsg.ObjectVersionResult{
		Object: domain.ScientificObject{
			ID: "obj-1", ProjectID: rsgTestProjectID, ObjectType: "material",
			CurrentVersionNo: 2, CreatedBy: "user-1", CreatedAt: time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC),
		},
		Version: domain.ScientificObjectVersion{
			ID: "ver-2", ObjectID: "obj-1", VersionNo: 2, StateID: "state-2",
			SchemaID: "https://open-rd.example/schemas/material.schema.json", SchemaVersion: "1",
			Title: "MOF-5", LifecycleState: domain.LifecycleActive,
			Payload: json.RawMessage(`{"name":"MOF-5","formula":"Zn4O(BDC)3"}`),
		},
	}
}

func cannedRelation() rsg.RelationResult {
	return rsg.RelationResult{
		Relation: domain.Relation{
			ID: "rel-1", ProjectID: rsgTestProjectID, CurrentVersionNo: 1,
			CreatedAt: time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC),
		},
		Version: domain.RelationVersion{
			ID: "relver-1", RelationID: "rel-1", VersionNo: 1, StateID: "state-2",
			RelationType: "derived_from", SourceObjectVersionID: "ver-2", TargetObjectVersionID: "ver-1",
			Payload: json.RawMessage(`{}`),
		},
	}
}

func strPtr(s string) *string { return &s }

// newRSGTestServer composes the auth surface + RSG routes like the
// projectshttp tests, and returns the server, the authed client, the
// signed-up user id and the CSRF token the writes must echo.
func newRSGTestServer(t *testing.T, svc Service) (*httptest.Server, *http.Client, string, string) {
	t.Helper()
	authAPI := authhttp.New(authhttp.Deps{
		Users:    memstore.NewUsers(),
		Sessions: memstore.NewSessions(),
		Limiter:  memstore.NewLimiter(),
		Cfg: authn.Config{
			WebOrigin:          "http://web.test",
			SessionTTL:         time.Hour,
			LoginLimitPerEmail: 1000,
			LoginLimitPerIP:    10000,
			LoginWindow:        time.Minute,
			SignupLimitPerIP:   10000,
		},
	})
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	New(Deps{Service: svc}).Register(mux)
	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	authed := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := authed.Post(ts.URL+"/api/v1/auth/signup", "application/json",
		strings.NewReader(`{"email":"rsg-handler@example.com","password":"long-enough-password-1","handle":"rsg-handler","display_name":"RSG Handler"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("signup = %d", resp.StatusCode)
	}
	var payload struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	return ts, authed, payload.User.ID, payload.CSRFToken
}

// rsgWrite performs one JSON write with the CSRF token attached, exactly
// as the web app sends it.
func rsgWrite(t *testing.T, client *http.Client, method, url, csrf, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodePayload(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode %s: %v (body %s)", resp.Request.URL.Path, err, respBody(resp))
	}
	return payload
}

func respBody(resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

const objectsBase = "/api/v1/projects/" + rsgTestProjectID + "/branches/branch-1"

func TestCreateBranchHappyPath(t *testing.T) {
	stub := &stubService{branch: cannedBranch()}
	ts, authed, _, csrf := newRSGTestServer(t, stub)

	resp := rsgWrite(t, authed, http.MethodPost, ts.URL+"/api/v1/projects/"+rsgTestProjectID+"/branches", csrf,
		`{"name":"main","base_ref":"","visibility":"private"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", resp.StatusCode, respBody(resp))
	}
	payload := decodePayload(t, resp)
	for _, field := range []string{"id", "project_id", "name", "visibility", "git_ref", "base_state_id", "lifecycle_state", "created_by", "created_at"} {
		if _, ok := payload[field]; !ok {
			t.Errorf("payload lacks %q: %v", field, payload)
		}
	}
	if payload["id"] != "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee" || payload["name"] != "main" || payload["visibility"] != "private" || payload["lifecycle_state"] != "active" {
		t.Errorf("payload = %v", payload)
	}
	if stub.createBranchCalls != 1 || stub.lastProjectID != rsgTestProjectID {
		t.Fatalf("calls = %d, projectID = %q", stub.createBranchCalls, stub.lastProjectID)
	}
	in, ok := stub.lastInput.(rsg.CreateBranchInput)
	if !ok || in.Name != "main" || in.BaseRef != "" || in.Visibility != domain.BranchVisibilityPrivate {
		t.Errorf("input = %#v", stub.lastInput)
	}
}

func TestCreateObjectHappyPath(t *testing.T) {
	stub := &stubService{object: cannedObject()}
	ts, authed, _, csrf := newRSGTestServer(t, stub)

	resp := rsgWrite(t, authed, http.MethodPost, ts.URL+objectsBase+"/objects", csrf,
		`{"object_type":"material","payload":{"name":"MOF-5"},"schema_ref":null}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", resp.StatusCode, respBody(resp))
	}
	payload := decodePayload(t, resp)
	// The G3 acceptance contract reads the version facts at the top level.
	if payload["version_id"] != "ver-1" || payload["version_no"] != float64(1) || payload["id"] != "obj-1" {
		t.Errorf("payload = %v", payload)
	}
	hints, ok := payload["hints"].([]any)
	if !ok || len(hints) != 1 || hints[0].(map[string]any)["code"] != semantics.HintClaimCompound {
		t.Errorf("hints = %v", payload["hints"])
	}
	if stub.createObjectCalls != 1 || stub.lastProjectID != rsgTestProjectID || stub.lastBranchID != "branch-1" {
		t.Fatalf("calls = %d, projectID = %q, branchID = %q", stub.createObjectCalls, stub.lastProjectID, stub.lastBranchID)
	}
	in, ok := stub.lastInput.(rsg.CreateObjectInput)
	if !ok || in.ObjectType != "material" || in.SchemaRef != "" {
		t.Errorf("input = %#v", stub.lastInput)
	}
}

func TestCreateObjectVersionRouteParsing(t *testing.T) {
	stub := &stubService{version: cannedVersion()}
	ts, authed, _, csrf := newRSGTestServer(t, stub)

	// The ":version" suffix route: the object id is everything before it.
	resp := rsgWrite(t, authed, http.MethodPost, ts.URL+objectsBase+"/objects/obj-1:version", csrf,
		`{"expected_version":1,"patch":{"formula":"Zn4O(BDC)3"}}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", resp.StatusCode, respBody(resp))
	}
	payload := decodePayload(t, resp)
	if payload["version_id"] != "ver-2" || payload["version_no"] != float64(2) || payload["id"] != "obj-1" {
		t.Errorf("payload = %v", payload)
	}
	if stub.createObjectVersionCalls != 1 || stub.lastObjectID != "obj-1" || stub.lastBranchID != "branch-1" {
		t.Fatalf("calls = %d, objectID = %q, branchID = %q", stub.createObjectVersionCalls, stub.lastObjectID, stub.lastBranchID)
	}
	in, ok := stub.lastInput.(rsg.CreateObjectVersionInput)
	if !ok || in.ExpectedVersion != 1 {
		t.Errorf("input = %#v", stub.lastInput)
	}
}

func TestCreateObjectVersionSuffixRequired(t *testing.T) {
	stub := &stubService{version: cannedVersion()}
	ts, authed, _, csrf := newRSGTestServer(t, stub)

	// No ":version" suffix: not a route.
	resp := rsgWrite(t, authed, http.MethodPost, ts.URL+objectsBase+"/objects/obj-1", csrf, `{"expected_version":1,"patch":{}}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("no-suffix status = %d, want 404", resp.StatusCode)
	}
	// An empty object id ("/objects/:version") is also not an object.
	resp = rsgWrite(t, authed, http.MethodPost, ts.URL+objectsBase+"/objects/:version", csrf, `{"expected_version":1,"patch":{}}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("empty-id status = %d, want 404", resp.StatusCode)
	}
	if stub.createObjectVersionCalls != 0 {
		t.Errorf("service called %d times on non-route paths", stub.createObjectVersionCalls)
	}
}

func TestCreateRelationHappyPath(t *testing.T) {
	stub := &stubService{relation: cannedRelation()}
	ts, authed, _, csrf := newRSGTestServer(t, stub)

	resp := rsgWrite(t, authed, http.MethodPost, ts.URL+objectsBase+"/relations", csrf,
		`{"relation_type":"derived_from","source_object_version_id":"ver-2","target_object_version_id":"ver-1"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", resp.StatusCode, respBody(resp))
	}
	payload := decodePayload(t, resp)
	if payload["version_id"] != "relver-1" || payload["id"] != "rel-1" || payload["relation_type"] != "derived_from" ||
		payload["source_object_version_id"] != "ver-2" || payload["target_object_version_id"] != "ver-1" {
		t.Errorf("payload = %v", payload)
	}
	if stub.createRelationCalls != 1 || stub.lastBranchID != "branch-1" {
		t.Fatalf("calls = %d, branchID = %q", stub.createRelationCalls, stub.lastBranchID)
	}
	in, ok := stub.lastInput.(rsg.CreateRelationInput)
	if !ok || in.RelationType != "derived_from" || in.SourceObjectVersionID != "ver-2" || in.TargetObjectVersionID != "ver-1" {
		t.Errorf("input = %#v", stub.lastInput)
	}
}

func TestGetObjectHappyPath(t *testing.T) {
	stub := &stubService{object: cannedObject()}
	ts, authed, _, _ := newRSGTestServer(t, stub)

	resp, err := authed.Get(ts.URL + objectsBase + "/objects/obj-1")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, respBody(resp))
	}
	payload := decodePayload(t, resp)
	if payload["id"] != "obj-1" || payload["version_id"] != "ver-1" {
		t.Errorf("payload = %v", payload)
	}
	if stub.getObjectCalls != 1 || stub.lastObjectID != "obj-1" {
		t.Fatalf("calls = %d, objectID = %q", stub.getObjectCalls, stub.lastObjectID)
	}
}

func TestWriteWithoutSessionIsRejected(t *testing.T) {
	stub := &stubService{object: cannedObject()}
	ts, _, _, _ := newRSGTestServer(t, stub)
	// A client with no session cookie: the guard answers before routing.
	anon := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp := rsgWrite(t, anon, http.MethodPost, ts.URL+objectsBase+"/objects", "", `{"object_type":"material","payload":{"name":"MOF-5"}}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if stub.createObjectCalls != 0 {
		t.Errorf("service called %d times without a session", stub.createObjectCalls)
	}
}

func TestCreateObjectForbiddenEnvelope(t *testing.T) {
	// The service refusal travels through the full stack as the stable
	// envelope — the same one whether the target exists or not.
	stub := &stubService{err: rsg.ErrForbidden}
	ts, authed, _, csrf := newRSGTestServer(t, stub)

	resp := rsgWrite(t, authed, http.MethodPost, ts.URL+objectsBase+"/objects", csrf,
		`{"object_type":"material","payload":{"name":"MOF-5"}}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	envelope := decodeError(t, resp)
	if envelope.Code != rsg.CodeForbidden {
		t.Errorf("code = %q, want %q", envelope.Code, rsg.CodeForbidden)
	}
}

func TestCreateObjectVersionConflictEnvelope(t *testing.T) {
	stub := &stubService{err: &sciobjects.VersionConflictError{ObjectID: "obj-1", Expected: 1, Actual: 2}}
	ts, authed, _, csrf := newRSGTestServer(t, stub)

	resp := rsgWrite(t, authed, http.MethodPost, ts.URL+objectsBase+"/objects/obj-1:version", csrf,
		`{"expected_version":1,"patch":{}}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	envelope := decodeError(t, resp)
	if envelope.Code != sciobjects.CodeVersionConflict {
		t.Errorf("code = %q, want %q", envelope.Code, sciobjects.CodeVersionConflict)
	}
}

// decodeError reads the docs/45 error envelope.
func decodeError(t *testing.T, resp *http.Response) struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Retryable bool   `json:"retryable"`
} {
	t.Helper()
	defer resp.Body.Close()
	var envelope struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
		Retryable bool   `json:"retryable"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	return envelope
}

// TestRSGErrorMapping covers the wire mapping of every service outcome:
// one envelope shape, stable codes, and a denied write answers the same
// whether the target exists or not (docs/45).
func TestRSGErrorMapping(t *testing.T) {
	schemaBlocked := rsgvalidation.Report{
		Gate:    rsgvalidation.GateDraft,
		Verdict: rsgvalidation.VerdictBlocked,
		Results: []rsgvalidation.Result{
			{Check: rsgvalidation.CheckSchemaTyped, Severity: rsgvalidation.SeverityBlocking, Passed: false, Why: "w"},
		},
	}
	mixedBlocked := rsgvalidation.Report{
		Gate:    rsgvalidation.GatePR,
		Verdict: rsgvalidation.VerdictBlocked,
		Results: []rsgvalidation.Result{
			{Check: rsgvalidation.CheckSchemaTyped, Severity: rsgvalidation.SeverityBlocking, Passed: false, Why: "w"},
			{Check: rsgvalidation.CheckPayloadIntegrity, Severity: rsgvalidation.SeverityBlocking, Passed: false, Why: "w"},
		},
	}
	tests := []struct {
		name      string
		err       error
		status    int
		code      string
		retryable bool
	}{
		{"project not found", projects.ErrProjectNotFound, http.StatusNotFound, projects.CodeProjectNotFound, false},
		{"write denied", rsg.ErrForbidden, http.StatusForbidden, rsg.CodeForbidden, false},
		{"branch not found (branches)", branches.ErrBranchNotFound, http.StatusNotFound, states.CodeBranchNotFound, false},
		{"branch not found (states)", states.ErrBranchNotFound, http.StatusNotFound, states.CodeBranchNotFound, false},
		{"branch name taken", branches.ErrBranchNameTaken, http.StatusConflict, branches.CodeBranchNameTaken, false},
		{"public branch in private project", branches.ErrPublicBranchInPrivateProject, http.StatusForbidden, branches.CodePublicBranchInPrivateProject, false},
		{"base state not found", branches.ErrBaseStateNotFound, http.StatusNotFound, branches.CodeBaseStateNotFound, false},
		{"state not found", states.ErrStateNotFound, http.StatusNotFound, states.CodeStateNotFound, false},
		{"state exists", states.ErrStateExists, http.StatusConflict, states.CodeStateExists, false},
		{"branch state conflict", &states.StateConflictError{BranchID: "b"}, http.StatusConflict, states.CodeBranchStateConflict, false},
		{"branch not active", &states.BranchNotActiveError{BranchID: "b", Lifecycle: "merged"}, http.StatusConflict, states.CodeBranchNotActive, false},
		{"object not found", sciobjects.ErrObjectNotFound, http.StatusNotFound, sciobjects.CodeObjectNotFound, false},
		{"object version not found", sciobjects.ErrVersionNotFound, http.StatusNotFound, sciobjects.CodeVersionNotFound, false},
		{"object version conflict", &sciobjects.VersionConflictError{ObjectID: "o", Expected: 1, Actual: 2}, http.StatusConflict, sciobjects.CodeVersionConflict, false},
		{"relation not found", relations.ErrRelationNotFound, http.StatusNotFound, relations.CodeRelationNotFound, false},
		{"relation version not found", relations.ErrRelationVersionNotFound, http.StatusNotFound, relations.CodeRelationVersionNotFound, false},
		{"referenced version not found", &relations.ReferencedVersionNotFoundError{Side: "source", VersionID: "v"}, http.StatusNotFound, relations.CodeObjectVersionNotFound, false},
		{"unknown relation type", &relations.UnknownRelationTypeError{Type: "x"}, http.StatusBadRequest, relations.CodeRSGValidationFailed, false},
		{"schema gate blocked", &rsgvalidation.GateBlockedError{Report: schemaBlocked}, http.StatusUnprocessableEntity, "SCHEMA_VALIDATION_FAILED", false},
		{"mixed gate blocked", &rsgvalidation.GateBlockedError{Report: mixedBlocked}, http.StatusUnprocessableEntity, "RSG_VALIDATION_FAILED", false},
		{"semantic validation", rsg.ErrValidation, http.StatusBadRequest, rsg.CodeValidation, false},
		{"unknown failure", errors.New("db down"), http.StatusServiceUnavailable, rsg.CodeUnavailable, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p/branches/b/objects", nil)
			rsgError(rec, req, tt.err)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d", rec.Code, tt.status)
			}
			var envelope struct {
				Code      string `json:"code"`
				Message   string `json:"message"`
				RequestID string `json:"request_id"`
				Retryable bool   `json:"retryable"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&envelope); err != nil {
				t.Fatalf("decode envelope: %v", err)
			}
			if envelope.Code != tt.code {
				t.Errorf("code = %q, want %q", envelope.Code, tt.code)
			}
			if envelope.Message == "" {
				t.Error("message is empty")
			}
			if envelope.Retryable != tt.retryable {
				t.Errorf("retryable = %v, want %v", envelope.Retryable, tt.retryable)
			}
		})
	}
}

func TestMalformedBody(t *testing.T) {
	stub := &stubService{object: cannedObject()}
	ts, authed, _, csrf := newRSGTestServer(t, stub)

	resp := rsgWrite(t, authed, http.MethodPost, ts.URL+objectsBase+"/objects", csrf, `{not json`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if envelope := decodeError(t, resp); envelope.Code != rsg.CodeValidation {
		t.Errorf("code = %q, want %q", envelope.Code, rsg.CodeValidation)
	}
	if stub.createObjectCalls != 0 {
		t.Errorf("service called %d times on a malformed body", stub.createObjectCalls)
	}
}
