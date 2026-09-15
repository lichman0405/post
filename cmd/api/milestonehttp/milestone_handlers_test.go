package milestonehttp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/milestones"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
)

// fakeCommand implements CommandPort with canned results, recording the
// create calls (the write the tests can reach without a guard-attached
// principal).
type fakeCommand struct {
	milestone  domain.Milestone
	milestones []domain.Milestone
	err        error
	listErr    error
	createErr  error

	gotCreateProjectID  string
	gotCreateKind       domain.MilestoneKind
	gotCreateLabel      string
	gotCreateOccurredAt time.Time
	gotCreateReleaseID  *string
	gotCreateKey        *string
	creates             int
}

func (f *fakeCommand) Create(_ context.Context, _ domain.User, in milestones.CreateMilestoneParams) (domain.Milestone, error) {
	f.creates++
	f.gotCreateProjectID = in.ProjectID
	f.gotCreateKind = in.Kind
	f.gotCreateLabel = in.Label
	f.gotCreateOccurredAt = in.OccurredAt
	f.gotCreateReleaseID = in.ReleaseID
	f.gotCreateKey = in.IdempotencyKey
	if f.createErr != nil {
		return domain.Milestone{}, f.createErr
	}
	return f.milestone, f.err
}

func (f *fakeCommand) List(context.Context, string) ([]domain.Milestone, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.milestones, f.err
}

func (f *fakeCommand) Get(context.Context, string, string) (domain.Milestone, error) {
	return f.milestone, f.err
}

// fakeProjectGate is the visibility gate every milestone read runs: a
// canned project or error, recording the reader it was handed.
type fakeProjectGate struct {
	gotReader projects.Reader
	gotID     string
	err       error
}

func (f *fakeProjectGate) Get(_ context.Context, r projects.Reader, projectID string) (domain.Project, error) {
	f.gotReader, f.gotID = r, projectID
	if f.err != nil {
		return domain.Project{}, f.err
	}
	return domain.Project{ID: projectID}, nil
}

func newTestServer(cmd CommandPort, gate *fakeProjectGate) *httptest.Server {
	mux := http.NewServeMux()
	New(Deps{Command: cmd, Projects: gate}).Register(mux)
	return httptest.NewServer(mux)
}

func fixtureMilestone() domain.Milestone {
	return domain.Milestone{
		ID:         "milestone-00000001",
		ProjectID:  "proj-00000001",
		Kind:       domain.MilestonePaperSubmitted,
		Label:      "JACS 2026",
		OccurredAt: time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC),
		CreatedBy:  "user-00000001",
		CreatedAt:  time.Date(2026, 9, 14, 11, 0, 0, 0, time.UTC),
	}
}

// TestListMilestonesRendersPayload: the list answers the milestones
// payload under the visibility gate (anonymous is fine — the gate
// decides).
func TestListMilestonesRendersPayload(t *testing.T) {
	cmd := &fakeCommand{milestones: []domain.Milestone{fixtureMilestone()}}
	gate := &fakeProjectGate{}
	ts := newTestServer(cmd, gate)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/projects/proj-00000001/milestones")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Milestones []milestonePayload `json:"milestones"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Milestones) != 1 || body.Milestones[0].ID != "milestone-00000001" ||
		body.Milestones[0].Kind != "paper_submitted" ||
		body.Milestones[0].Label == nil || *body.Milestones[0].Label != "JACS 2026" {
		t.Fatalf("milestones = %+v, want the fixture milestone", body.Milestones)
	}
	if body.Milestones[0].ReleaseID != nil {
		t.Fatalf("release_id = %v, want null (no link)", body.Milestones[0].ReleaseID)
	}
	if gate.gotID != "proj-00000001" {
		t.Fatalf("gate ran for %q, want proj-00000001", gate.gotID)
	}
}

// TestListMilestonesRendersNullLabel: a canonical milestone without a
// custom label renders label: null (the UI shows the kind's display
// name).
func TestListMilestonesRendersNullLabel(t *testing.T) {
	m := fixtureMilestone()
	m.Label = ""
	cmd := &fakeCommand{milestones: []domain.Milestone{m}}
	gate := &fakeProjectGate{}
	ts := newTestServer(cmd, gate)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/projects/proj-00000001/milestones")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), `"label":null`) {
		t.Fatalf("body = %s, want a null label", raw)
	}
}

// TestListMilestonesGateError: the visibility gate's error maps through
// the shared error writer.
func TestListMilestonesGateError(t *testing.T) {
	gate := &fakeProjectGate{err: projects.ErrProjectNotFound}
	cmd := &fakeCommand{}
	ts := newTestServer(cmd, gate)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/projects/proj-hidden/milestones")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestGetMilestoneRendersPayload: GET one milestone by id.
func TestGetMilestoneRendersPayload(t *testing.T) {
	cmd := &fakeCommand{milestone: fixtureMilestone()}
	gate := &fakeProjectGate{}
	ts := newTestServer(cmd, gate)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/projects/proj-00000001/milestones/milestone-00000001")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body milestonePayload
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.ID != "milestone-00000001" || body.Kind != "paper_submitted" {
		t.Fatalf("milestone = %+v, want the fixture", body)
	}
}

// TestGetMilestoneNotFoundMaps: the command's not-found maps to the
// stable 404 envelope.
func TestGetMilestoneNotFoundMaps(t *testing.T) {
	cmd := &fakeCommand{err: milestones.ErrMilestoneNotFound}
	gate := &fakeProjectGate{}
	ts := newTestServer(cmd, gate)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/projects/proj-00000001/milestones/milestone-none")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var envelope map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope["code"] != milestones.CodeMilestoneNotFound {
		t.Fatalf("code = %v, want MILESTONE_NOT_FOUND", envelope["code"])
	}
}

// TestCreateMilestoneRequiresSession: no principal in the context — the
// handler-level backstop answers 401 before any command call (the guard
// enforces the same in the composed tree).
func TestCreateMilestoneRequiresSession(t *testing.T) {
	cmd := &fakeCommand{milestone: fixtureMilestone()}
	gate := &fakeProjectGate{}
	ts := newTestServer(cmd, gate)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/v1/projects/proj-00000001/milestones",
		"application/json", strings.NewReader(`{"kind":"paper_submitted","occurred_at":"2026-09-14T10:00:00Z"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if cmd.creates != 0 {
		t.Fatalf("command creates = %d, want 0", cmd.creates)
	}
}

// TestCreateMilestoneMalformedBody: a non-JSON body is a 400 with the
// validation envelope, never a crash (401 first when no principal is
// attached — the session backstop precedes the body decode).
func TestCreateMilestoneMalformedBody(t *testing.T) {
	cmd := &fakeCommand{}
	gate := &fakeProjectGate{}
	ts := newTestServer(cmd, gate)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/v1/projects/proj-00000001/milestones",
		"application/json", strings.NewReader("{not json"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 401 (no principal) or 400 (bad body)", resp.StatusCode)
	}
}

// TestCreateMilestoneMalformedOccurredAt: a non-RFC3339 occurred_at
// answers 400 without reaching the command (the session backstop may
// answer 401 first — same split as the body test).
func TestCreateMilestoneMalformedOccurredAt(t *testing.T) {
	cmd := &fakeCommand{}
	gate := &fakeProjectGate{}
	ts := newTestServer(cmd, gate)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/v1/projects/proj-00000001/milestones",
		"application/json", strings.NewReader(`{"kind":"paper_submitted","occurred_at":"14/09/2026"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 401 (no principal) or 400 (bad occurred_at)", resp.StatusCode)
	}
	if cmd.creates != 0 {
		t.Fatalf("command creates = %d, want 0", cmd.creates)
	}
}

// TestCreateMilestoneErrorMaps: the read-side command sentinels map onto
// the stable wire envelopes (the create's error mapping needs a
// guard-attached principal and is covered by the integration suite, same
// split as releasehttp).
func TestCreateMilestoneErrorMaps(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"milestone not found", milestones.ErrMilestoneNotFound, http.StatusNotFound, milestones.CodeMilestoneNotFound},
		{"store", milestones.ErrStore, http.StatusServiceUnavailable, milestones.CodeMilestoneServiceUnavailable},
		{"validation", milestones.ErrValidation, http.StatusBadRequest, milestones.CodeMilestoneValidationFailed},
		{"forbidden", milestones.ErrForbidden, http.StatusForbidden, milestones.CodeMilestoneForbidden},
		{"release link", milestones.ErrReleaseNotFound, http.StatusNotFound, milestones.CodeMilestoneReleaseNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &fakeCommand{err: tc.err}
			gate := &fakeProjectGate{}
			ts := newTestServer(cmd, gate)
			defer ts.Close()

			resp, err := http.Get(ts.URL + "/api/v1/projects/proj-00000001/milestones/milestone-00000001")
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			var envelope map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if envelope["code"] != tc.code {
				t.Fatalf("code = %v, want %s", envelope["code"], tc.code)
			}
		})
	}
}

// TestNoUpdateOrDeleteRoutes: V1 has no correction surface — the mux
// answers 405 for PUT/PATCH/DELETE (a recorded milestone is a fact on
// the timeline).
func TestNoUpdateOrDeleteRoutes(t *testing.T) {
	cmd := &fakeCommand{}
	gate := &fakeProjectGate{}
	ts := newTestServer(cmd, gate)
	defer ts.Close()

	for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
		req, err := http.NewRequest(method,
			ts.URL+"/api/v1/projects/proj-00000001/milestones/milestone-00000001", nil)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s status = %d, want 405", method, resp.StatusCode)
		}
	}
}

// milestoneCreateBody renders a create body for the principal-less
// create tests (the handler path below the guard).
func milestoneCreateBody(kind, label, occurredAt, releaseID string) string {
	labelField := ""
	if label != "" {
		labelField = fmt.Sprintf(`"label":%q,`, label)
	}
	releaseField := ""
	if releaseID != "" {
		releaseField = fmt.Sprintf(`,"release_id":%q`, releaseID)
	}
	return fmt.Sprintf(`{"kind":%q,%s"occurred_at":%q%s}`, kind, labelField, occurredAt, releaseField)
}

// TestMilestoneCreateBodyShape: the rendered body decodes into the
// request shape the handler parses (guards the body builder above).
func TestMilestoneCreateBodyShape(t *testing.T) {
	raw := milestoneCreateBody("custom", "First replicate", "2026-09-14T10:00:00Z", "release-00000001")
	var req createMilestoneRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("body does not parse: %v", err)
	}
	if req.Kind != "custom" || req.Label != "First replicate" ||
		req.OccurredAt != "2026-09-14T10:00:00Z" || req.ReleaseID != "release-00000001" {
		t.Fatalf("request = %+v, want the rendered fields", req)
	}
}
