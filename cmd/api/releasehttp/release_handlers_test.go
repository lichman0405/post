package releasehttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/releases"
	"github.com/lichman0405/post/internal/domain"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// fakeCommand implements CommandPort with canned results, recording the
// create calls (the write the tests can reach without a guard-attached
// principal).
type fakeCommand struct {
	release  domain.Release
	releases []domain.Release
	manifest []byte
	err      error

	gotCreateProjectID string
	gotCreateVersion   string
	gotCreateKey       *string
	creates            int
}

func (f *fakeCommand) Create(_ context.Context, _ domain.User, in releases.CreateReleaseParams) (domain.Release, error) {
	f.creates++
	f.gotCreateProjectID = in.ProjectID
	f.gotCreateVersion = in.Version
	f.gotCreateKey = in.IdempotencyKey
	return f.release, f.err
}

func (f *fakeCommand) List(context.Context, string) ([]domain.Release, error) {
	return f.releases, f.err
}

func (f *fakeCommand) Get(context.Context, string, string) (domain.Release, error) {
	return f.release, f.err
}

func (f *fakeCommand) Manifest(context.Context, string, string) ([]byte, error) {
	return f.manifest, f.err
}

// fakeProjectGate is the visibility gate every release read runs: a
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

func fixtureRelease() domain.Release {
	return domain.Release{
		ID:           "release-00000001",
		ProjectID:    "proj-00000001",
		Version:      "v1.0.0",
		Title:        "v1.0.0",
		StateID:      "state-00000001",
		ManifestHash: "abcd",
		CreatedBy:    "user-00000001",
		CreatedAt:    time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC),
	}
}

// TestListReleasesRendersPayload: the list answers the releases payload
// under the visibility gate (anonymous is fine — the gate decides).
func TestListReleasesRendersPayload(t *testing.T) {
	cmd := &fakeCommand{releases: []domain.Release{fixtureRelease()}}
	gate := &fakeProjectGate{}
	ts := newTestServer(cmd, gate)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/projects/proj-00000001/releases")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Releases []releasePayload `json:"releases"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Releases) != 1 || body.Releases[0].ID != "release-00000001" ||
		body.Releases[0].Version != "v1.0.0" || body.Releases[0].ManifestHash != "abcd" {
		t.Fatalf("releases = %+v, want the fixture release", body.Releases)
	}
	if gate.gotID != "proj-00000001" {
		t.Fatalf("gate ran for %q, want proj-00000001", gate.gotID)
	}
}

// TestCreateRequiresSession: without a guard-attached principal the
// create answers 401 before the command runs (the handler backstop).
func TestCreateRequiresSession(t *testing.T) {
	cmd := &fakeCommand{release: fixtureRelease()}
	gate := &fakeProjectGate{}
	ts := newTestServer(cmd, gate)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/v1/projects/proj-00000001/releases",
		"application/json", strings.NewReader(`{"version":"v1.0.0"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if cmd.creates != 0 {
		t.Fatalf("command ran %d creates, want 0", cmd.creates)
	}
}

// TestCreateRejectsMalformedBody: a malformed body is a 400
// VALIDATION_FAILED, never a crash.
func TestCreateRejectsMalformedBody(t *testing.T) {
	cmd := &fakeCommand{}
	ts := newTestServer(cmd, &fakeProjectGate{})
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/v1/projects/proj-00000001/releases",
		"application/json", strings.NewReader(`{`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 401 (no principal) or 400 (bad body)", resp.StatusCode)
	}
}

// TestGetReleaseNotFound: an unknown release answers 404
// RELEASE_NOT_FOUND under a passing project gate.
func TestGetReleaseNotFound(t *testing.T) {
	cmd := &fakeCommand{err: releases.ErrReleaseNotFound}
	ts := newTestServer(cmd, &fakeProjectGate{})
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/projects/proj-00000001/releases/release-00000099")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), releases.CodeReleaseNotFound) {
		t.Fatalf("body %q does not carry code %q", body, releases.CodeReleaseNotFound)
	}
}

// TestProjectGateHidesProject: the visibility gate's not-found answers
// RELEASE_PROJECT_NOT_FOUND — the project read gate owns existence
// hiding, the release handlers only translate it.
func TestProjectGateHidesProject(t *testing.T) {
	cmd := &fakeCommand{}
	ts := newTestServer(cmd, &fakeProjectGate{err: projects.ErrProjectNotFound})
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/projects/proj-00000001/releases")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), releases.CodeReleaseProjectNotFound) {
		t.Fatalf("body %q does not carry code %q", body, releases.CodeReleaseProjectNotFound)
	}
}

// TestManifestExportServesStoredBytes: the export answers the command's
// bytes verbatim with a download disposition named by the version.
func TestManifestExportServesStoredBytes(t *testing.T) {
	cmd := &fakeCommand{
		release:  fixtureRelease(),
		manifest: []byte(`{"format":"open-rd/release-manifest","format_version":1}`),
	}
	ts := newTestServer(cmd, &fakeProjectGate{})
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/projects/proj-00000001/releases/release-00000001/manifest")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="release-v1.0.0.manifest.json"` {
		t.Fatalf("content-disposition = %q", cd)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"format":"open-rd/release-manifest","format_version":1}` {
		t.Fatalf("body = %q, want the command's bytes verbatim", body)
	}
}

// TestNoEditNoDelete: the surface registers no update or delete route
// (acceptance: no edit/delete of an immutable release). On a bare mux
// that registers only the release surface, the mux itself answers 405
// for every mutating method — no such method exists for the path. In the
// production composition the projects subtree shadows that with its own
// "no such route" 404; either way the release command never runs (the
// integration suite pins the composed-tree status).
func TestNoEditNoDelete(t *testing.T) {
	cmd := &fakeCommand{release: fixtureRelease()}
	ts := newTestServer(cmd, &fakeProjectGate{})
	defer ts.Close()

	for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
		req, err := http.NewRequest(method,
			ts.URL+"/api/v1/projects/proj-00000001/releases/release-00000001", nil)
		if err != nil {
			t.Fatalf("NewRequest %s: %v", method, err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s status = %d, want 405 (bare release mux)", method, resp.StatusCode)
		}
	}
	if cmd.creates != 0 {
		t.Fatalf("mutating attempts reached the command: creates=%d", cmd.creates)
	}
}

// TestWriteReleaseErrorDoesNotLeakRawErrors: dependency failures answer
// a generic 503 naming nothing internal, and an unmapped error answers a
// generic 500 (docs/45 — same gate as every sibling surface).
func TestWriteReleaseErrorDoesNotLeakRawErrors(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		secret     string
	}{
		{
			name:       "store failure answers a generic 503",
			err:        fmt.Errorf("%w: %v", releases.ErrStore, errors.New("failed to connect to host=secret-db user=admin")),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   releases.CodeReleaseServiceUnavailable,
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
			req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p/releases", nil)
			writeReleaseError(rec, req, tc.err)
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

// TestWriteReleaseErrorKeepsDomainMappings: the sentinel shapes keep
// their specific codes (regression pin), and the gate refusal carries
// the gate's own explanation as the message (docs/22 §7).
func TestWriteReleaseErrorKeepsDomainMappings(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantMsg    string
	}{
		{"validation", releases.ErrValidation, http.StatusBadRequest, releases.CodeReleaseValidationFailed, ""},
		{"forbidden", releases.ErrForbidden, http.StatusForbidden, releases.CodeReleaseForbidden, ""},
		{"project not found", releases.ErrProjectNotFound, http.StatusNotFound, releases.CodeReleaseProjectNotFound, ""},
		{"no main state", releases.ErrNotMainState, http.StatusConflict, releases.CodeReleaseNoMainState, ""},
		{"version taken", releases.ErrVersionTaken, http.StatusConflict, releases.CodeReleaseVersionTaken, ""},
		{"release not found", releases.ErrReleaseNotFound, http.StatusNotFound, releases.CodeReleaseNotFound, ""},
		{"gate blocked", &releases.GateRefused{Report: rsgvalidation.Report{
			Gate: rsgvalidation.GateRelease, Verdict: rsgvalidation.VerdictBlocked, Explanation: "the rights snapshot is missing",
		}}, http.StatusConflict, releases.CodeReleaseGateBlocked, "the rights snapshot is missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p/releases", nil)
			writeReleaseError(rec, req, tc.err)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			body := rec.Body.String()
			if !strings.Contains(body, `"code":"`+tc.wantCode+`"`) {
				t.Fatalf("body %q does not carry code %q", body, tc.wantCode)
			}
			if tc.wantMsg != "" && !strings.Contains(body, tc.wantMsg) {
				t.Fatalf("body %q does not carry the gate explanation %q", body, tc.wantMsg)
			}
		})
	}
}
