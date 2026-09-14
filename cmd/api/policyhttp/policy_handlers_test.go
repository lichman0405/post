package policyhttp

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/policy"
)

// writePolicyError is the transport error gate (docs/45): dependency
// failures answer a generic 503 naming nothing internal, and an unmapped
// error is logged server-side and answers a generic 500. The raw error
// text never reaches the envelope, so an authenticated caller cannot
// harvest driver detail (host names, credentials, constraint names) by
// hitting the surface during an outage. Same shape as the sibling
// surfaces (orgshttp/projectshttp/audithttp).
func TestWritePolicyErrorDoesNotLeakRawErrors(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		secret     string
	}{
		{
			name:       "store failure answers a generic 503",
			err:        fmt.Errorf("%w: %v", policy.ErrStore, errors.New("failed to connect to host=secret-db user=admin database=secret")),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   policy.CodeServiceUnavailable,
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
			req := httptest.NewRequest(http.MethodGet, "/api/v1/organizations/org/policy", nil)
			writePolicyError(rec, req, tc.err)
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

// The generic branches must not swallow the sentinel cases: domain errors
// keep their specific mapping (regression pin).
func TestWritePolicyErrorKeepsDomainMappings(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/organizations/org/policy", nil)
	writePolicyError(rec, req, policy.ErrPolicyNotFound)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if !strings.Contains(rec.Body.String(), policy.CodePolicyNotFound) {
		t.Fatalf("body %q does not carry code %q", rec.Body.String(), policy.CodePolicyNotFound)
	}
}
