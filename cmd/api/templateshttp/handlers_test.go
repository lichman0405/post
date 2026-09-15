package templateshttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/templates"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/memstore"
)

// The template wire mapping through the REAL guard: real signup issues the
// session, the instantiate write additionally requires the session-bound
// CSRF token, and the catalog reads flow unauthenticated (reads decide
// their own visibility). The stub service exercises the transport shape
// end to end — the orchestration itself is covered by the service unit
// tests and the integration suite.

// stubService pins canned answers and records what the wire passed through.
type stubService struct {
	list   []domain.ProjectTemplate
	get    map[string]domain.ProjectTemplate
	getErr error

	instantiateOut templates.Instantiation
	instantiateErr error
	lastActor      domain.User
	lastInput      templates.InstantiateInput

	rec         domain.TemplateInstantiation
	recErr      error
	lastReader  projects.Reader
	lastProject string
}

func (s *stubService) List() ([]domain.ProjectTemplate, error) {
	return s.list, s.getErr
}

func (s *stubService) Get(id, version string) (domain.ProjectTemplate, error) {
	if s.getErr != nil {
		return domain.ProjectTemplate{}, s.getErr
	}
	key := id + "@" + version
	if t, ok := s.get[key]; ok {
		return t, nil
	}
	return domain.ProjectTemplate{}, templates.ErrTemplateNotFound
}

func (s *stubService) Instantiate(_ context.Context, actor domain.User, in templates.InstantiateInput) (templates.Instantiation, error) {
	s.lastActor = actor
	s.lastInput = in
	if s.instantiateErr != nil {
		return templates.Instantiation{}, s.instantiateErr
	}
	return s.instantiateOut, nil
}

func (s *stubService) GetInstantiation(_ context.Context, r projects.Reader, projectID string) (domain.TemplateInstantiation, error) {
	s.lastReader = r
	s.lastProject = projectID
	if s.recErr != nil {
		return domain.TemplateInstantiation{}, s.recErr
	}
	return s.rec, nil
}

// cannedTemplate is one minimal template the renderers walk.
func cannedTemplate() domain.ProjectTemplate {
	return domain.ProjectTemplate{
		ID:          "materials-discovery",
		Version:     "v1",
		Name:        "Materials Discovery",
		Description: "Canned template for wire tests.",
		Project: domain.TemplateProjectDefaults{
			Name: "Materials Discovery", Purpose: "Find a better anode material.",
			Visibility: domain.VisibilityPrivate,
		},
		Schemas: []domain.TemplateSchemaProfile{
			{Name: "material_ext", Base: domain.SchemaRef{ID: "https://open-rd.example/schemas/material", Version: "1"},
				Properties: map[string]any{"target_band_gap": map[string]any{"type": "number"}}, Required: []string{"target_band_gap"}},
		},
		Review: domain.TemplateReviewDefaults{Rules: map[string]json.RawMessage{
			"main_protected": json.RawMessage(`true`),
		}},
		Map: []domain.TemplateQuestion{{Statement: "Which family to screen?", Purpose: "scope"}},
	}
}

func cannedInstantiation() templates.Instantiation {
	tmpl := cannedTemplate()
	return templates.Instantiation{
		Template: tmpl,
		Project: domain.Project{
			ID: "11111111-2222-4333-8444-555555555555", Slug: "wire-lab", Name: "Materials Discovery",
			Purpose: "Find a better anode material.", ActivityStatus: "planning",
			Visibility: domain.VisibilityPrivate, ProvisionStatus: domain.ProvisionPending,
			CreatedBy: "user-1", CreatedAt: time.Date(2026, 9, 14, 8, 0, 0, 0, time.UTC),
		},
		Membership: domain.ProjectMembership{
			ProjectID: "11111111-2222-4333-8444-555555555555", UserID: "user-1",
			Role: domain.ProjectRoleOwner,
		},
		Instantiation: domain.TemplateInstantiation{
			ID: "rec-1", ProjectID: "11111111-2222-4333-8444-555555555555",
			TemplateID: tmpl.ID, TemplateVersion: tmpl.Version, TemplateName: tmpl.Name,
			CreatedBy: "user-1", CreatedAt: time.Date(2026, 9, 14, 8, 0, 0, 0, time.UTC),
		},
		Applied: templates.AppliedReport{
			Recorded: &templates.StepOutcome{},
			Profiles: []templates.AppliedProfile{
				{StepOutcome: templates.StepOutcome{}, SchemaID: "project:11111111-2222-4333-8444-555555555555:material_ext", Version: "1"},
			},
			Policy: &templates.StepOutcome{},
			Map: &templates.AppliedMap{
				StepOutcome: templates.StepOutcome{},
				BranchID:    "branch-1",
				Questions:   []templates.AppliedQuestion{{StepOutcome: templates.StepOutcome{}, ObjectID: "obj-1", Statement: "Which family to screen?"}},
			},
		},
	}
}

// newTemplatesTestServer composes the auth surface + template routes like
// the sibling wire tests, and returns the server, the authed client, the
// signed-up user id and the CSRF token the writes must echo.
func newTemplatesTestServer(t *testing.T, svc Service) (*httptest.Server, *http.Client, string, string) {
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
		strings.NewReader(`{"email":"templates-handler@example.com","password":"long-enough-password-1","handle":"templates-handler","display_name":"Templates Handler"}`))
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

// templatesWrite performs one JSON write with the CSRF token attached,
// exactly as the web app sends it.
func templatesWrite(t *testing.T, client *http.Client, method, url, csrf, body string) *http.Response {
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
		t.Fatalf("decode %s: %v", resp.Request.URL.Path, err)
	}
	return payload
}

func respBody(resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// TestListTemplatesPublicRead: the catalog read flows without a session
// and renders the newest version per id with its defaults.
func TestListTemplatesPublicRead(t *testing.T) {
	stub := &stubService{list: []domain.ProjectTemplate{cannedTemplate()}}
	ts, _, _, _ := newTemplatesTestServer(t, stub)
	resp, err := http.Get(ts.URL + "/api/v1/templates")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	payload := decodePayload(t, resp)
	list, ok := payload["templates"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("templates = %v, want one entry", payload["templates"])
	}
	entry := list[0].(map[string]any)
	if entry["id"] != "materials-discovery" || entry["version"] != "v1" {
		t.Errorf("entry = %v, want materials-discovery@v1", entry)
	}
	proj := entry["project"].(map[string]any)
	if proj["visibility"] != "private" || proj["purpose"] == "" {
		t.Errorf("project defaults = %v, want the template's presets", proj)
	}
	schemas := entry["schemas"].([]any)
	if len(schemas) != 1 || schemas[0].(map[string]any)["name"] != "material_ext" {
		t.Errorf("schemas = %v, want material_ext", schemas)
	}
	if _, ok := entry["review"].(map[string]any); !ok {
		t.Errorf("review = %v, want a JSON object of the template's rules", entry["review"])
	}
}

// TestGetTemplateVersionPinning: ?version= pins the resolved entry.
func TestGetTemplateVersionPinning(t *testing.T) {
	stub := &stubService{get: map[string]domain.ProjectTemplate{"materials-discovery@v1": cannedTemplate()}}
	ts, _, _, _ := newTemplatesTestServer(t, stub)
	resp, err := http.Get(ts.URL + "/api/v1/templates/materials-discovery?version=v1")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	payload := decodePayload(t, resp)
	if payload["id"] != "materials-discovery" || payload["version"] != "v1" {
		t.Errorf("payload = %v, want the pinned entry", payload)
	}
}

// TestGetTemplateNotFound: an unknown id answers the stable 404 envelope.
func TestGetTemplateNotFound(t *testing.T) {
	stub := &stubService{get: map[string]domain.ProjectTemplate{}}
	ts, _, _, _ := newTemplatesTestServer(t, stub)
	resp, err := http.Get(ts.URL + "/api/v1/templates/unknown")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	body := respBody(resp)
	if !strings.Contains(body, `"code":"`+templates.CodeTemplateNotFound+`"`) {
		t.Fatalf("body %q does not carry TEMPLATE_NOT_FOUND", body)
	}
}

// TestInstantiateHappyPath: the guarded write creates from the template
// and the 201 body answers the project contract + the provenance record +
// the per-default application report.
func TestInstantiateHappyPath(t *testing.T) {
	stub := &stubService{instantiateOut: cannedInstantiation()}
	ts, authed, _, csrf := newTemplatesTestServer(t, stub)
	resp := templatesWrite(t, authed, http.MethodPost,
		ts.URL+"/api/v1/templates/materials-discovery/instantiate", csrf,
		`{"slug":"wire-lab","name":"My Lab","purpose":"My goal.","visibility":"private"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", resp.StatusCode, respBody(resp))
	}
	payload := decodePayload(t, resp)
	if payload["template"].(map[string]any)["id"] != "materials-discovery" {
		t.Errorf("template = %v, want the resolved template", payload["template"])
	}
	proj := payload["project"].(map[string]any)
	if proj["id"] != "11111111-2222-4333-8444-555555555555" {
		t.Errorf("project = %v, want the created project", proj)
	}
	mem := payload["membership"].(map[string]any)
	if mem["role"] != "owner" {
		t.Errorf("membership = %v, want the creator's owner role", mem)
	}
	rec := payload["record"].(map[string]any)
	if rec["template_id"] != "materials-discovery" || rec["template_version"] != "v1" || rec["project_id"] != "11111111-2222-4333-8444-555555555555" {
		t.Errorf("record = %v, want the template id + version recorded", rec)
	}
	applied := payload["applied"].(map[string]any)
	if applied["recorded"].(map[string]any)["error"] != nil && applied["recorded"].(map[string]any)["error"] != "" {
		t.Errorf("applied.recorded = %v, want the record step applied", applied["recorded"])
	}
	if errVal, ok := applied["policy"].(map[string]any)["error"]; ok && errVal != "" {
		t.Errorf("applied.policy = %v, want applied", applied["policy"])
	}
	profiles := applied["profiles"].([]any)
	if len(profiles) != 1 || profiles[0].(map[string]any)["schema_id"] != "project:11111111-2222-4333-8444-555555555555:material_ext" {
		t.Errorf("applied.profiles = %v, want the namespaced profile id", profiles)
	}
	// The wire passed the caller's fields through to the service.
	if stub.lastInput.Slug != "wire-lab" || stub.lastInput.Visibility != domain.VisibilityPrivate {
		t.Errorf("service input = %+v, want the decoded request fields", stub.lastInput)
	}
	if stub.lastActor.ID == "" {
		t.Error("the service received no actor — the guard's principal did not flow")
	}
}

// TestInstantiateWithoutSessionIsRejected: an anonymous write answers 401
// before routing (the guard, not the handler).
func TestInstantiateWithoutSessionIsRejected(t *testing.T) {
	stub := &stubService{instantiateOut: cannedInstantiation()}
	ts, _, _, _ := newTemplatesTestServer(t, stub)
	resp, err := http.Post(ts.URL+"/api/v1/templates/materials-discovery/instantiate",
		"application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// TestInstantiateWithoutCSRFIsRejected: a session without the CSRF token
// answers 403 — the write guard is in force for the template path too.
func TestInstantiateWithoutCSRFIsRejected(t *testing.T) {
	stub := &stubService{instantiateOut: cannedInstantiation()}
	ts, authed, _, _ := newTemplatesTestServer(t, stub)
	resp := templatesWrite(t, authed, http.MethodPost,
		ts.URL+"/api/v1/templates/materials-discovery/instantiate", "",
		`{}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", resp.StatusCode, respBody(resp))
	}
}

// TestInstantiateUnknownTemplate: the resolved-catalog miss answers the
// stable 404 envelope.
func TestInstantiateUnknownTemplate(t *testing.T) {
	stub := &stubService{instantiateErr: templates.ErrTemplateNotFound}
	ts, authed, _, csrf := newTemplatesTestServer(t, stub)
	resp := templatesWrite(t, authed, http.MethodPost,
		ts.URL+"/api/v1/templates/unknown/instantiate", csrf, `{}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if !strings.Contains(respBody(resp), `"code":"`+templates.CodeTemplateNotFound+`"`) {
		t.Fatalf("body %q does not carry TEMPLATE_NOT_FOUND", respBody(resp))
	}
}

// TestInstantiateProjectErrorsReuseProjectCodes: a create-from-template
// answers the SAME stable contract a plain create answers (slug conflict
// → the projects surface's code and message).
func TestInstantiateProjectErrorsReuseProjectCodes(t *testing.T) {
	stub := &stubService{instantiateErr: projects.ErrSlugTaken}
	ts, authed, _, csrf := newTemplatesTestServer(t, stub)
	resp := templatesWrite(t, authed, http.MethodPost,
		ts.URL+"/api/v1/templates/materials-discovery/instantiate", csrf, `{}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	body := respBody(resp)
	if !strings.Contains(body, `"code":"`+projects.CodeProjectSlugTaken+`"`) {
		t.Fatalf("body %q does not carry the projects slug-taken code", body)
	}
}

// TestInstantiateMalformedBody: a broken JSON body answers 400 before the
// service runs.
func TestInstantiateMalformedBody(t *testing.T) {
	stub := &stubService{instantiateOut: cannedInstantiation()}
	ts, authed, _, csrf := newTemplatesTestServer(t, stub)
	resp := templatesWrite(t, authed, http.MethodPost,
		ts.URL+"/api/v1/templates/materials-discovery/instantiate", csrf, `{not json`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestGetInstantiationRecordsTemplateOrigin: the record read answers the
// recorded id + version, and the wire passes the caller's principal to
// the service (the project-visibility gate runs there).
func TestGetInstantiationRecordsTemplateOrigin(t *testing.T) {
	stub := &stubService{rec: cannedInstantiation().Instantiation}
	ts, authed, _, _ := newTemplatesTestServer(t, stub)
	resp, err := authed.Get(ts.URL + "/api/v1/projects/11111111-2222-4333-8444-555555555555/template-instantiation")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	payload := decodePayload(t, resp)
	if payload["template_id"] != "materials-discovery" || payload["template_version"] != "v1" {
		t.Errorf("record = %v, want the recorded id + version", payload)
	}
	if !stub.lastReader.Authenticated || stub.lastReader.UserID == "" {
		t.Errorf("service reader = %+v, want the caller's principal", stub.lastReader)
	}
	if stub.lastProject != "11111111-2222-4333-8444-555555555555" {
		t.Errorf("service project = %s, want the path id", stub.lastProject)
	}
}

// TestGetInstantiationNotFound: no recorded origin answers the stable 404
// (same shape as an unknown project — nothing is disclosed).
func TestGetInstantiationNotFound(t *testing.T) {
	stub := &stubService{recErr: templates.ErrInstantiationNotFound}
	ts, _, _, _ := newTemplatesTestServer(t, stub)
	resp, err := http.Get(ts.URL + "/api/v1/projects/11111111-2222-4333-8444-555555555555/template-instantiation")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestTemplateErrorDoesNotLeakRawErrors is the transport error gate
// (docs/45): dependency failures answer a generic 503 naming nothing
// internal, and an unmapped error answers a generic 500 — the raw error
// text never reaches the envelope.
func TestTemplateErrorDoesNotLeakRawErrors(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		secret     string
	}{
		{
			name:       "store failure answers a generic 503",
			err:        fmt.Errorf("%w: %v", templates.ErrStore, errors.New("failed to connect to host=secret-db user=admin database=secret")),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   templates.CodeUnavailable,
			secret:     "secret-db",
		},
		{
			name:       "unmapped error answers a generic 500",
			err:        errors.New("pgx: failed to connect to host=secret-db user=admin"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   "INTERNAL_ERROR",
			secret:     "secret-db",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/templates", nil)
			(&handlers{}).templateError(rec, req, tc.err)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			body := rec.Body.String()
			if !strings.Contains(body, `"code":"`+tc.wantCode+`"`) {
				t.Fatalf("body %q does not carry code %q", body, tc.wantCode)
			}
			if strings.Contains(body, tc.secret) {
				t.Fatalf("body %q leaks the raw error detail %q", body, tc.secret)
			}
		})
	}
}

// TestTemplateErrorKeepsDomainMappings: the sentinel cases keep their
// specific mapping (regression pin — the generic branches must not
// swallow them).
func TestTemplateErrorKeepsDomainMappings(t *testing.T) {
	cases := []struct {
		err        error
		wantStatus int
		wantCode   string
	}{
		{templates.ErrTemplateNotFound, http.StatusNotFound, templates.CodeTemplateNotFound},
		{templates.ErrInstantiationNotFound, http.StatusNotFound, templates.CodeTemplateNotFound},
		{templates.ErrInstantiationExists, http.StatusConflict, templates.CodeValidation},
		{templates.ErrValidation, http.StatusBadRequest, templates.CodeValidation},
		{projects.ErrValidation, http.StatusBadRequest, templates.CodeValidation},
		{projects.ErrProjectNotFound, http.StatusNotFound, projects.CodeProjectNotFound},
		{projects.ErrMemberNotFound, http.StatusNotFound, projects.CodeProjectMembershipNotFound},
		{projects.ErrForbidden, http.StatusForbidden, projects.CodeProjectForbidden},
		{projects.ErrOrgNotFound, http.StatusNotFound, projects.CodeOrgNotFound},
		{projects.ErrOrgDeactivated, http.StatusConflict, projects.CodeOrgDeactivated},
		{projects.ErrProgramNotFound, http.StatusNotFound, projects.CodeProgramNotFound},
		{projects.ErrProgramOrgMismatch, http.StatusBadRequest, projects.CodeProgramOrgMismatch},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/templates", nil)
		(&handlers{}).templateError(rec, req, tc.err)
		if rec.Code != tc.wantStatus {
			t.Errorf("%v: status = %d, want %d", tc.err, rec.Code, tc.wantStatus)
		}
		if !strings.Contains(rec.Body.String(), `"code":"`+tc.wantCode+`"`) {
			t.Errorf("%v: body %q does not carry code %q", tc.err, rec.Body.String(), tc.wantCode)
		}
	}
}
