package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newCaptureLogger returns a logger writing JSON lines into buf.
func newCaptureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, nil))
}

func TestNewCorrelationIDIsValidAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := NewCorrelationID()
		if err != nil {
			t.Fatalf("NewCorrelationID: %v", err)
		}
		if _, ok := ParseCorrelationID(string(id)); !ok {
			t.Fatalf("generated id %q does not pass ParseCorrelationID", id)
		}
		if seen[string(id)] {
			t.Fatalf("duplicate generated id %q", id)
		}
		seen[string(id)] = true
	}
}

func TestParseCorrelationID(t *testing.T) {
	valid := []string{
		"0123456789abcdef0123456789abcdef",     // Go hex form
		"3f9c21e5-b8d4-4c0a-9f1e-7d3b2a91c4f8", // web UUID form
		"web-trace-abc123",                     // human test ids
		"a.b_c-1x",                             // minimal 8 chars
		strings.Repeat("a", 64),                // maximal 64 chars
	}
	for _, s := range valid {
		if _, ok := ParseCorrelationID(s); !ok {
			t.Errorf("ParseCorrelationID(%q) = rejected, want accepted", s)
		}
	}
	invalid := []string{
		"",
		"short",                 // < 8 chars
		strings.Repeat("a", 65), // > 64 chars
		"has space",
		"../../etc/passwd",
		"a;log-injection",
		"a\nb",
		"-leading-dash", // first char must be alnum
		"日本国",
	}
	for _, s := range invalid {
		if id, ok := ParseCorrelationID(s); ok {
			t.Errorf("ParseCorrelationID(%q) = %q accepted, want rejected", s, id)
		}
	}
}

func TestMiddlewareCreatesIDEchoesAndLogsCompletion(t *testing.T) {
	var buf bytes.Buffer
	log := newCaptureLogger(&buf)
	var seenFromCtx CorrelationID
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ok bool
		seenFromCtx, ok = FromContext(r.Context())
		if !ok {
			t.Error("correlation id missing from request context")
		}
		LoggerFromContext(r.Context()).Info("handler line")
		w.WriteHeader(http.StatusCreated)
	})
	mw := Middleware(log)(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz?token=topsecret", nil)
	mw.ServeHTTP(rec, req)

	if seenFromCtx == "" {
		t.Fatal("handler saw no correlation id")
	}
	echoed := rec.Header().Get(HeaderCorrelationID)
	if echoed != string(seenFromCtx) {
		t.Errorf("response header %q, context id %q: want equal", echoed, seenFromCtx)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201 (inner handler untouched)", rec.Code)
	}

	out := buf.String()
	// The query string must never reach the log (credential carrier).
	if strings.Contains(out, "topsecret") || strings.Contains(out, "?token") {
		t.Errorf("query string leaked into request log: %s", out)
	}
	for _, want := range []string{
		`"correlation_id":"` + string(seenFromCtx) + `"`,
		`"msg":"http request completed"`,
		`"method":"GET"`,
		`"path":"/healthz"`,
		`"status":201`,
		`"msg":"handler line"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %s:\n%s", want, out)
		}
	}
	// Both the handler line and the completion line carry the same id.
	if got := strings.Count(out, `"correlation_id":"`+string(seenFromCtx)+`"`); got < 2 {
		t.Errorf("correlation id appears %d times, want >= 2 (handler + completion):\n%s", got, out)
	}
}

func TestMiddlewareHonoursValidIncomingID(t *testing.T) {
	var buf bytes.Buffer
	log := newCaptureLogger(&buf)
	mw := Middleware(log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := FromContext(r.Context())
		w.Header().Set("seen", string(id))
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set(HeaderCorrelationID, "web-trace-abc123")
	mw.ServeHTTP(rec, req)

	if got := rec.Header().Get("seen"); got != "web-trace-abc123" {
		t.Errorf("handler saw %q, want incoming id web-trace-abc123", got)
	}
	if got := rec.Header().Get(HeaderCorrelationID); got != "web-trace-abc123" {
		t.Errorf("echoed %q, want incoming id", got)
	}
}

func TestMiddlewareRejectsInvalidIncomingID(t *testing.T) {
	var buf bytes.Buffer
	log := newCaptureLogger(&buf)
	mw := Middleware(log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := FromContext(r.Context())
		w.Header().Set("seen", string(id))
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set(HeaderCorrelationID, "bad id with spaces")
	mw.ServeHTTP(rec, req)

	got := rec.Header().Get("seen")
	if _, ok := ParseCorrelationID(got); !ok {
		t.Errorf("handler saw invalid id %q; a fresh valid id must replace invalid input", got)
	}
	if got == "bad id with spaces" {
		t.Error("invalid incoming id was propagated verbatim")
	}
}

func TestMiddlewareWriterPreservesFlusher(t *testing.T) {
	// The wrapped ResponseWriter must keep optional interfaces usable.
	mw := Middleware(slog.New(slog.DiscardHandler))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The Go 1.20+ contract for wrapped writers: Unwrap() lets
		// http.ResponseController reach the optional interfaces.
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("ResponseController.Flush through the wrapper: %v", err)
		}
	}))
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestRedisLogBridgeFormatsAndRedacts(t *testing.T) {
	var buf bytes.Buffer
	log := newCaptureLogger(&buf)

	bridge := redisLogBridge{log: log.With("component", "go-redis")}
	bridge.Printf(context.Background(), "redis: dial tcp failed for %s", "postgres://user:sekret123@db.internal:5432/post")

	out := buf.String()
	if strings.Contains(out, "sekret123") {
		t.Errorf("raw secret leaked through go-redis bridge: %s", out)
	}
	for _, want := range []string{
		`"level":"WARN"`,
		`"component":"go-redis"`,
		`"msg":"go-redis: redis: dial tcp failed for postgres://user:***@db.internal:5432/post"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("bridge output missing %s:\n%s", want, out)
		}
	}
}

func TestRedisLogBridgeMasksBareTokens(t *testing.T) {
	var buf bytes.Buffer
	log := newCaptureLogger(&buf)
	bridge := redisLogBridge{log: log.With("component", "go-redis")}

	// A GitHub-style token in a non-URL message must be masked by
	// RedactForOutput's token regex.
	bridge.Printf(context.Background(), "redis: auth failed with %s", "ghp_t0007canarytoken00")

	out := buf.String()
	if strings.Contains(out, "ghp_t0007canarytoken00") {
		t.Errorf("token leaked through go-redis bridge: %s", out)
	}
	if !strings.Contains(out, "***") {
		t.Errorf("token was not masked: %s", out)
	}
}

func TestLoggerFromContextFallsBackToDefault(t *testing.T) {
	if LoggerFromContext(context.Background()) == nil {
		t.Fatal("LoggerFromContext on a bare context returned nil")
	}
	var buf bytes.Buffer
	log := newCaptureLogger(&buf)
	ctx := WithLogger(context.Background(), log)
	if LoggerFromContext(ctx) != log {
		t.Error("LoggerFromContext did not return the attached logger")
	}
}

// TestCorrelationIDTravelsInJSON confirms the id survives JSON round-trips
// (it is embedded verbatim in queue job payloads).
func TestCorrelationIDTravelsInJSON(t *testing.T) {
	id, err := NewCorrelationID()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"correlation_id": string(id)})
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]string
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back["correlation_id"] != string(id) {
		t.Errorf("round-tripped id = %q, want %q", back["correlation_id"], id)
	}
}
