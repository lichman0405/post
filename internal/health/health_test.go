package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, h http.Handler, path string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("%s: Content-Type = %q, want application/json", path, ct)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: response is not valid JSON: %v (body %q)", path, err, rec.Body.String())
	}
	return rec.Code, body
}

func upProbe() Probe {
	return Probe{Name: "postgresql", Check: func(context.Context) error { return nil }}
}

func TestHealthzIsLivenessOnly(t *testing.T) {
	// A failing readiness probe must never affect /healthz.
	h := NewHandler("api", Probe{Name: "postgresql", Check: func(context.Context) error {
		return errors.New("boom")
	}})

	code, body := get(t, h, "/healthz")
	if code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", code)
	}
	if body["status"] != "ok" || body["service"] != "api" {
		t.Errorf("healthz body = %v", body)
	}
	if _, ok := body["checks"]; ok {
		t.Errorf("healthz must not contain dependency checks: %v", body)
	}
}

func TestReadyzAllUpAnswers200(t *testing.T) {
	h := NewHandler("api", upProbe(), Probe{Name: "redis", Check: func(context.Context) error { return nil }})

	code, body := get(t, h, "/readyz")
	if code != http.StatusOK {
		t.Fatalf("readyz status = %d, want 200 (body %v)", code, body)
	}
	if body["status"] != "ready" {
		t.Errorf("readyz status field = %v, want ready", body["status"])
	}
	checks, _ := body["checks"].(map[string]any)
	for _, name := range []string{"postgresql", "redis"} {
		c, _ := checks[name].(map[string]any)
		if c["status"] != "up" {
			t.Errorf("check %s = %v, want up", name, c)
		}
	}
}

func TestReadyzReportsDownDependencyAs503NotReady(t *testing.T) {
	h := NewHandler("api", upProbe(), Probe{Name: "postgresql",
		Check: func(context.Context) error {
			return errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
		}})

	code, body := get(t, h, "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503 (body %v)", code, body)
	}
	if body["status"] != "not_ready" {
		t.Errorf("readyz status field = %v, want not_ready", body["status"])
	}
	raw := string(mustJSON(t, body))
	if !strings.Contains(raw, "connection refused") {
		t.Errorf("readyz must carry the dependency error detail: %s", raw)
	}
}

func TestReadyzWithNoProbesIsReady(t *testing.T) {
	// A service with no wired dependencies is truthfully ready.
	code, body := get(t, NewHandler("mcp-server"), "/readyz")
	if code != http.StatusOK || body["status"] != "ready" {
		t.Errorf("no-probe readyz = %d %v, want 200 ready", code, body)
	}
}

func TestWrongMethodAndUnknownPath(t *testing.T) {
	h := NewHandler("api", upProbe())
	for _, path := range []string{"/healthz", "/readyz"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d, want 405", path, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/nope", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /nope = %d, want 404", rec.Code)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
