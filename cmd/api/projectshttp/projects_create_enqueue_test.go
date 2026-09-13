package projectshttp

import (
	"context"
	"encoding/json"
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
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/gitprovider"
	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/worker"
)

// The T0301 create-side wiring: a successful project create enqueues one
// project-provision job carrying the project id and the request's
// correlation id. The job is best effort — a missing or failing sink never
// fails the create (the row is the truth; the API's startup sweep
// back-fills).

const createProjectID = "c9f0f895-8b6d-4b16-9c0e-1f2a3b4c5d6e"

// createStubStore reuses the shared stub (membership/settings reads) and
// answers CreateProject with a fixed provision-pending project.
type createStubStore struct {
	stubProjectStore
}

func (s *createStubStore) CreateProject(_ context.Context, p domain.Project, creator string) (domain.Project, domain.ProjectMembership, error) {
	created := domain.Project{
		ID:              createProjectID,
		Slug:            p.Slug,
		Name:            p.Name,
		Purpose:         p.Purpose,
		ActivityStatus:  "planning",
		Visibility:      p.Visibility,
		ProvisionStatus: domain.ProvisionPending,
		CreatedAt:       time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC),
	}
	return created, domain.ProjectMembership{
		ProjectID: createProjectID,
		UserID:    creator,
		Role:      domain.ProjectRoleOwner,
		CreatedAt: time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC),
	}, nil
}

// recordingSink captures every enqueued job; err, when set, fails the
// enqueue (the create must still succeed).
type recordingSink struct {
	jobs []worker.Job
	err  error
}

func (s *recordingSink) Enqueue(_ context.Context, job worker.Job) error {
	s.jobs = append(s.jobs, job)
	return s.err
}

// newCreateTestServer composes the auth surface + project routes through
// the real guard, with the provisioning sink attached — and returns the
// server, the authed client (session cookie), and the CSRF token the
// create write must echo.
func newCreateTestServer(t *testing.T, store projects.ProjectStore, sink JobSink) (*httptest.Server, *http.Client, string) {
	t.Helper()
	authAPI := authhttp.New(authhttp.Deps{
		Users:      memstore.NewUsers(),
		Sessions:   memstore.NewSessions(),
		Limiter:    memstore.NewLimiter(),
		OIDCClient: nil,
		Cfg: authn.Config{
			WebOrigin:          "http://web.test",
			SessionTTL:         time.Hour,
			LoginLimitPerEmail: 1000,
			LoginLimitPerIP:    10000,
			LoginWindow:        time.Minute,
			SignupLimitPerIP:   10000,
		},
		Secure: false,
	})
	projectAPI := New(Deps{
		Store:         store,
		Orgs:          &stubOrgGate{},
		Authz:         stubEngine{},
		ProvisionJobs: sink,
	})
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	// The bare path is registered alongside the subtree, as in
	// cmd/api/main.go — otherwise POST /api/v1/projects (create) would be
	// 307-redirected to the slash form.
	mux.Handle("/api/v1/projects", projectAPI.Routes())
	mux.Handle("/api/v1/projects/", projectAPI.Routes())
	ts := httptest.NewServer(observability.Middleware(nil)(authAPI.Guard(mux)))
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	authed := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := authed.Post(ts.URL+"/api/v1/auth/signup", "application/json",
		strings.NewReader(`{"email":"create-handler@example.com","password":"long-enough-password-1","handle":"create-handler","display_name":"Create Handler"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("signup = %d (body %s)", resp.StatusCode, readAll(resp))
	}
	var payload struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	return ts, authed, payload.CSRFToken
}

func readAll(resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// TestCreateEnqueuesProvisioningJob: the acceptance-criterion wiring — the
// created project comes back provision-pending and exactly one
// project-provision job carrying the project id and the request's
// correlation id lands on the sink.
func TestCreateEnqueuesProvisioningJob(t *testing.T) {
	store := &createStubStore{}
	sink := &recordingSink{}
	ts, authed, csrf := newCreateTestServer(t, store, sink)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/projects",
		strings.NewReader(`{"slug":"battery-lab","name":"Battery Lab","purpose":"provisioning wiring","visibility":"private"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := authed.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d (body %s)", resp.StatusCode, readAll(resp))
	}
	var body struct {
		Project struct {
			ID              string `json:"id"`
			ProvisionStatus string `json:"provision_status"`
		} `json:"project"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Project.ID != createProjectID {
		t.Errorf("created project id = %q, want %q", body.Project.ID, createProjectID)
	}
	if body.Project.ProvisionStatus != "pending" {
		t.Errorf("created provision_status = %q, want pending", body.Project.ProvisionStatus)
	}

	if len(sink.jobs) != 1 {
		t.Fatalf("enqueued jobs = %d, want exactly 1", len(sink.jobs))
	}
	job := sink.jobs[0]
	if job.Type != gitprovider.ProvisionJobType {
		t.Errorf("job type = %q, want %q", job.Type, gitprovider.ProvisionJobType)
	}
	if job.ID == "" {
		t.Error("job id is empty")
	}
	var payload gitprovider.ProvisionJobPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatalf("job payload: %v", err)
	}
	if payload.ProjectID != createProjectID {
		t.Errorf("job payload project_id = %q, want %q", payload.ProjectID, createProjectID)
	}
	if cid := resp.Header.Get(observability.HeaderCorrelationID); cid == "" || job.CorrelationID != cid {
		t.Errorf("job correlation id = %q, want the request's %q", job.CorrelationID, cid)
	}
}

// TestCreateWithoutSinkStillSucceeds: the sink is optional (unit
// composition) — the create succeeds and the project stays pending for the
// startup sweep.
func TestCreateWithoutSinkStillSucceeds(t *testing.T) {
	store := &createStubStore{}
	ts, authed, csrf := newCreateTestServer(t, store, nil)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/projects",
		strings.NewReader(`{"slug":"no-sink","name":"No Sink","purpose":"x","visibility":"private"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := authed.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d (body %s)", resp.StatusCode, readAll(resp))
	}
}

// TestCreateSurvivesEnqueueFailure: a dead queue never fails the create —
// the project row exists and the sweep back-fills.
func TestCreateSurvivesEnqueueFailure(t *testing.T) {
	store := &createStubStore{}
	sink := &recordingSink{err: context.Canceled}
	ts, authed, csrf := newCreateTestServer(t, store, sink)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/projects",
		strings.NewReader(`{"slug":"dead-queue","name":"Dead Queue","purpose":"x","visibility":"private"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := authed.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create with dead queue = %d (body %s), want 201", resp.StatusCode, readAll(resp))
	}
	if len(sink.jobs) != 1 {
		t.Errorf("enqueue attempts = %d, want 1 (the failure is logged, not swallowed)", len(sink.jobs))
	}
}
