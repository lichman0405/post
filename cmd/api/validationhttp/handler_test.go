package validationhttp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/validation"
	"github.com/lichman0405/post/internal/domain"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// fakeValidator records the service call and returns a canned report.
type fakeValidator struct {
	gotProjectID string
	gotBranchID  string
	gotGate      rsgvalidation.Gate
	report       rsgvalidation.Report
	err          error
}

func (f *fakeValidator) ValidateBranch(_ context.Context, projectID, branchID string, gate rsgvalidation.Gate) (rsgvalidation.Report, error) {
	f.gotProjectID, f.gotBranchID, f.gotGate = projectID, branchID, gate
	return f.report, f.err
}

// fakeProjectGate is the ProjectReader the handler runs before validation:
// a canned project or error.
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

func newTestServer(v BranchValidator, gate *fakeProjectGate) *httptest.Server {
	mux := http.NewServeMux()
	New(Deps{Validator: v, Projects: gate}).Register(mux)
	return httptest.NewServer(mux)
}

// allowGate is the default project gate for the wire-shape tests: every
// project readable.
func allowGate() *fakeProjectGate { return &fakeProjectGate{} }

func postValidate(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestHandleValidateReturnsReport(t *testing.T) {
	report := rsgvalidation.Report{
		Gate:        rsgvalidation.GatePR,
		Verdict:     rsgvalidation.VerdictPassWithWarn,
		Results:     []rsgvalidation.Result{{Check: "branch_empty", Severity: "warning", Passed: false, Detail: "the branch has no states to validate", Why: "w"}},
		Explanation: "gate pr: pass_with_warnings",
	}
	fake := &fakeValidator{report: report}
	ts := newTestServer(fake, allowGate())
	defer ts.Close()

	resp := postValidate(t, ts.URL+"/api/v1/projects/proj-1/branches/branch-1:validate", `{"gate":"pr"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got rsgvalidation.Report
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if got.Gate != rsgvalidation.GatePR || got.Verdict != rsgvalidation.VerdictPassWithWarn {
		t.Errorf("report = %+v, want the canned pr report", got)
	}
	if fake.gotProjectID != "proj-1" || fake.gotBranchID != "branch-1" || fake.gotGate != rsgvalidation.GatePR {
		t.Errorf("service called with (%q, %q, %q), want (proj-1, branch-1, pr)", fake.gotProjectID, fake.gotBranchID, fake.gotGate)
	}
}

func TestHandleValidateGateEnum(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{"main gate", `{"gate":"main"}`, http.StatusOK},
		{"release gate", `{"gate":"release"}`, http.StatusOK},
		{"asset gate", `{"gate":"asset"}`, http.StatusOK},
		{"draft is not an endpoint gate", `{"gate":"draft"}`, http.StatusBadRequest},
		{"unknown gate", `{"gate":"gold"}`, http.StatusBadRequest},
		{"missing gate", `{}`, http.StatusBadRequest},
		{"malformed body", `{`, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(&fakeValidator{}, allowGate())
			defer ts.Close()
			resp := postValidate(t, ts.URL+"/api/v1/projects/proj-1/branches/branch-1:validate", tc.body)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
		})
	}
}

func TestHandleValidateRequiresSuffix(t *testing.T) {
	ts := newTestServer(&fakeValidator{}, allowGate())
	defer ts.Close()
	// Without the ":validate" suffix the remainder wildcard still routes
	// the request here; the handler must answer 404, not validate.
	resp := postValidate(t, ts.URL+"/api/v1/projects/proj-1/branches/branch-1", `{"gate":"pr"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHandleValidateMapsErrors(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"branch not found", validation.ErrBranchNotFound, http.StatusNotFound, "BRANCH_NOT_FOUND"},
		{"validation failure", validation.ErrValidation, http.StatusBadRequest, "VALIDATION_FAILED"},
		{"store failure", validation.ErrStore, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(&fakeValidator{err: tc.err}, allowGate())
			defer ts.Close()
			resp := postValidate(t, ts.URL+"/api/v1/projects/proj-1/branches/branch-1:validate", `{"gate":"pr"}`)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			var envelope struct {
				Code string `json:"code"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
				t.Fatalf("decode envelope: %v", err)
			}
			if envelope.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", envelope.Code, tc.wantCode)
			}
		})
	}
}

func TestHandleValidateProjectGate(t *testing.T) {
	tests := []struct {
		name        string
		gateErr     error
		wantStatus  int
		wantCode    string
		wantService bool // the validator must only run when the project read passed
	}{
		{"unreadable project answers existence hiding", projects.ErrProjectNotFound, http.StatusNotFound, "PROJECT_NOT_FOUND", false},
		{"project read failure is unavailable", projects.ErrStore, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", false},
		{"readable project proceeds", nil, http.StatusOK, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gate := &fakeProjectGate{err: tc.gateErr}
			fake := &fakeValidator{report: rsgvalidation.Report{Gate: rsgvalidation.GatePR, Verdict: rsgvalidation.VerdictPass}}
			ts := newTestServer(fake, gate)
			defer ts.Close()

			resp := postValidate(t, ts.URL+"/api/v1/projects/proj-1/branches/branch-1:validate", `{"gate":"pr"}`)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if tc.wantCode != "" {
				var envelope struct {
					Code string `json:"code"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
					t.Fatalf("decode envelope: %v", err)
				}
				if envelope.Code != tc.wantCode {
					t.Errorf("code = %q, want %q", envelope.Code, tc.wantCode)
				}
			}
			if gate.gotID != "proj-1" {
				t.Errorf("project gate called with %q, want proj-1", gate.gotID)
			}
			if (fake.gotProjectID != "") != tc.wantService {
				t.Errorf("validator called = %v, want %v (no validation may run behind a denied project read)",
					fake.gotProjectID != "", tc.wantService)
			}
		})
	}
}

func TestHandleValidateFailsClosedWithoutProjectGate(t *testing.T) {
	// A missing visibility gate must fail the route closed — an ungated
	// report would disclose a private project's contents.
	mux := http.NewServeMux()
	New(Deps{Validator: &fakeValidator{}}).Register(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp := postValidate(t, ts.URL+"/api/v1/projects/proj-1/branches/branch-1:validate", `{"gate":"pr"}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (fail closed, never ungated)", resp.StatusCode)
	}
	var envelope struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.Code != "SERVICE_UNAVAILABLE" {
		t.Errorf("code = %q, want SERVICE_UNAVAILABLE", envelope.Code)
	}
}
