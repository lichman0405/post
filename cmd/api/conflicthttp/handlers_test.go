package conflicthttp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lichman0405/post/internal/application/authn"
)

// TestHandlePutResolutionsUnauthenticatedCode locks the wire contract of
// the no-principal backstop: the guard answers AUTH_UNAUTHENTICATED (the
// code the web client maps), so the handler's own backstop must answer
// the same code — a caller reaching the route unauthenticated must never
// see a different spelling depending on which layer answered. This is a
// white-box test: the routes are guard-protected in production, so only
// the package itself can drive the handler past the guard.
func TestHandlePutResolutionsUnauthenticatedCode(t *testing.T) {
	h := &handlers{} // all deps nil: the principal check fires first
	req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/p1/resolutions", nil)
	req.SetPathValue("projectId", "p1")
	rec := httptest.NewRecorder()

	h.handlePutResolutions(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	var env struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not the error envelope: %v (body %q)", err, rec.Body.String())
	}
	if env.Code != authn.CodeUnauthenticated {
		t.Fatalf("code = %q, want %q", env.Code, authn.CodeUnauthenticated)
	}
}
