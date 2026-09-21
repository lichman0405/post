package security

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A fixed-window fake over the same port the production Redis adapter
// implements. It is deliberately a real counter (not a scripted "yes/no"
// queue), so the tests below exercise the middleware's decisions rather
// than a mock's.
type fakeLimiter struct {
	mu     sync.Mutex
	counts map[string]int
	seen   []string
	err    error
}

func newFakeLimiter() *fakeLimiter { return &fakeLimiter{counts: map[string]int{}} }

func (f *fakeLimiter) Check(_ context.Context, bucket string, limit int, window time.Duration) (bool, time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, bucket)
	if f.err != nil {
		return false, 0, f.err
	}
	f.counts[bucket]++
	if f.counts[bucket] > limit {
		return false, window, nil
	}
	return true, 0, nil
}

func (f *fakeLimiter) buckets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func testConfig() RateLimitConfig {
	return RateLimitConfig{
		Window:                  time.Minute,
		AnonymousPerIP:          3,
		AuthenticatedPerSession: 5,
		CredentialPerIP:         2,
		OutboundPerKey:          1,
		SessionCookie:           "post_session",
		ExemptPaths:             ExemptProbePaths(),
	}
}

func request(t *testing.T, mw func(http.Handler) http.Handler, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	var reached bool
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mw(next).ServeHTTP(rec, r)
	if rec.Code == http.StatusTooManyRequests || rec.Code == http.StatusServiceUnavailable {
		if reached {
			t.Fatalf("a refused request reached the handler (status %d)", rec.Code)
		}
	}
	return rec
}

// The headline behaviour: the budget is a count, and the request after the
// last allowed one is refused with 429 and a Retry-After.
func TestRateLimitRefusesTheRequestAfterTheBudgetWithRetryAfter(t *testing.T) {
	lim := newFakeLimiter()
	mw := RateLimit(lim, testConfig())

	for i := 1; i <= 3; i++ {
		rec := request(t, mw, httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d of 3 = %d, want 200 (budget is 3)", i, rec.Code)
		}
	}
	rec := request(t, mw, httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("4th request = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get(HeaderRetryAfter); got != "60" {
		t.Errorf("Retry-After = %q, want \"60\" (the window, in delta-seconds)", got)
	}
	var body errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("429 body is not the error envelope: %v (%s)", err, rec.Body.String())
	}
	if body.Code != CodeRateLimited {
		t.Errorf("429 code = %q, want %q", body.Code, CodeRateLimited)
	}
	if body.Message != "too many attempts; try again later" {
		t.Errorf("429 message = %q — it must match the authn rate-limit message", body.Message)
	}
	if !body.Retryable {
		t.Error("429 envelope retryable = false; a rate limit is the definition of retryable")
	}
	// A 429 is a JSON document a browser will render if someone navigates
	// straight at it, and it is produced by the limiter rather than by any
	// product handler — so it states its own nosniff like every other byte
	// exit. Asserted here, at the edge, because the exit registry in
	// tests/security scans cmd/api and this writer is not there.
	if got := rec.Header().Get(HeaderContentTypeOptions); got != ValueNosniff {
		t.Errorf("429 X-Content-Type-Options = %q, want %q", got, ValueNosniff)
	}
	if got := rec.Header().Get(HeaderContentType); got != "application/json" {
		t.Errorf("429 Content-Type = %q, want application/json", got)
	}
}

// The Retry-After a sub-second window produces must still be at least 1:
// "Retry-After: 0" tells a client to retry immediately, which is a busy
// loop against the endpoint the limiter just protected.
func TestRateLimitRetryAfterNeverRoundsToZero(t *testing.T) {
	lim := newFakeLimiter()
	cfg := testConfig()
	cfg.Window = 200 * time.Millisecond
	cfg.AnonymousPerIP = 1
	mw := RateLimit(lim, cfg)

	request(t, mw, httptest.NewRequest(http.MethodGet, "/x", nil))
	rec := request(t, mw, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get(HeaderRetryAfter); got != "1" {
		t.Errorf("Retry-After = %q, want \"1\" (0 would invite an immediate retry loop)", got)
	}
}

// Fail closed. An erroring limiter means the policy state is unknowable:
// the request is refused, never waved through. The counter-test that makes
// this mean something is the mutation recorded in RESULT (returning `true`
// on the error path turns this test red).
func TestRateLimitFailsClosedWhenTheLimiterErrors(t *testing.T) {
	lim := newFakeLimiter()
	lim.err = errors.New("redis: connection refused (127.0.0.1:6379)")
	mw := RateLimit(lim, testConfig())

	rec := request(t, mw, httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("request with a broken limiter = %d, want 503 (fail closed)", rec.Code)
	}
	var body errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("503 body is not the error envelope: %v", err)
	}
	if body.Code != CodeServiceUnavailable {
		t.Errorf("503 code = %q, want %q", body.Code, CodeServiceUnavailable)
	}
	// The dependency's own error text names an address; it must not travel.
	if strings.Contains(rec.Body.String(), "6379") || strings.Contains(rec.Body.String(), "redis") {
		t.Errorf("503 body leaks the dependency error: %s", rec.Body.String())
	}
	// Same reason as the 429 above, and the same claim: the refusals this
	// middleware writes are byte exits of their own.
	if got := rec.Header().Get(HeaderContentTypeOptions); got != ValueNosniff {
		t.Errorf("503 X-Content-Type-Options = %q, want %q", got, ValueNosniff)
	}
	if got := rec.Header().Get(HeaderContentType); got != "application/json" {
		t.Errorf("503 Content-Type = %q, want application/json", got)
	}
}

// Anonymous traffic is keyed by client IP and authenticated traffic by
// session token — and the token itself must never appear in a key, because
// a Redis key is a value that gets listed, logged and backed up.
func TestRateLimitKeysAnonymousByIPAndAuthenticatedBySession(t *testing.T) {
	lim := newFakeLimiter()
	mw := RateLimit(lim, testConfig())

	anonA := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	anonA.RemoteAddr = "203.0.113.7:41000"
	request(t, mw, anonA)

	anonB := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	anonB.RemoteAddr = "203.0.113.8:41001"
	request(t, mw, anonB)

	const token = "session-token-that-must-not-be-a-key"
	authed := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	authed.RemoteAddr = "203.0.113.7:41002"
	authed.AddCookie(&http.Cookie{Name: "post_session", Value: token})
	request(t, mw, authed)

	got := lim.buckets()
	want := []string{
		"edge:anon:ip:203.0.113.7",
		"edge:anon:ip:203.0.113.8",
		"edge:auth:sess:" + tokenHash(token),
	}
	if len(got) != len(want) {
		t.Fatalf("buckets = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("bucket %d = %q, want %q", i, got[i], want[i])
		}
	}
	for _, b := range got {
		if strings.Contains(b, token) {
			t.Errorf("bucket %q contains the session token verbatim", b)
		}
	}
	// Two different IPs must not share a budget; the two 203.0.113.7
	// requests must not share one either.
	if got[0] == got[1] {
		t.Error("two distinct client IPs landed in one anonymous bucket")
	}
	if got[0] == got[2] {
		t.Error("anonymous and authenticated traffic from one IP share a bucket")
	}
}

// The credential endpoints and the outbound-trigger routes get their own
// budgets, counted separately from — and in addition to — the ordinary
// anonymous budget.
func TestRateLimitClassifiesCredentialAndOutboundRoutes(t *testing.T) {
	lim := newFakeLimiter()
	mw := RateLimit(lim, testConfig())

	post := func(path string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		r.RemoteAddr = "203.0.113.7:42000"
		return r
	}
	request(t, mw, post("/api/v1/auth/login"))
	request(t, mw, post("/api/v1/auth/signup"))
	request(t, mw, post("/api/v1/webhooks/abc/deliveries/def/retry"))
	// Reads of the auth surface are ordinary anonymous traffic, not
	// credential traffic: a session poll must not spend the brute-force
	// budget.
	request(t, mw, httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil))
	request(t, mw, httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/authorize-url", nil))

	want := []string{
		"edge:cred:ip:203.0.113.7",
		"edge:cred:ip:203.0.113.7",
		"edge:out:ip:203.0.113.7",
		"edge:anon:ip:192.0.2.1",
		"edge:anon:ip:192.0.2.1",
	}
	got := lim.buckets()
	if len(got) != len(want) {
		t.Fatalf("buckets = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("bucket %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// The credential budget is real: two login attempts are allowed at the
// test budget, the third is a 429.
func TestRateLimitCredentialBudgetIsEnforced(t *testing.T) {
	lim := newFakeLimiter()
	mw := RateLimit(lim, testConfig())
	post := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader("{}"))
		r.RemoteAddr = "203.0.113.9:43000"
		return request(t, mw, r)
	}
	if got := post().Code; got != http.StatusOK {
		t.Fatalf("1st login = %d, want 200", got)
	}
	if got := post().Code; got != http.StatusOK {
		t.Fatalf("2nd login = %d, want 200", got)
	}
	rec := post()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd login = %d, want 429 (credential budget is 2)", rec.Code)
	}
	if rec.Header().Get(HeaderRetryAfter) != "60" {
		t.Errorf("429 Retry-After = %q, want \"60\"", rec.Header().Get(HeaderRetryAfter))
	}
}

// The liveness and readiness probes are the one exemption, and it is
// exact: a probe path is exempt, a path that merely starts with one is not
// (an exemption implemented as a prefix match is an exemption a caller can
// widen).
func TestRateLimitExemptsOnlyTheProbePaths(t *testing.T) {
	lim := newFakeLimiter()
	lim.err = errors.New("redis down")
	mw := RateLimit(lim, testConfig())

	for _, path := range ExemptProbePaths() {
		rec := request(t, mw, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s with a broken limiter = %d, want 200 (the probe must not depend on Redis)", path, rec.Code)
		}
	}
	if len(lim.buckets()) != 0 {
		t.Errorf("exempt paths were still counted: %v", lim.buckets())
	}
	for _, path := range []string{"/healthz/", "/healthz/extra", "/readyzz", "/api/v1/projects"} {
		rec := request(t, mw, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s with a broken limiter = %d, want 503 (only the probes are exempt)", path, rec.Code)
		}
	}
}

// The envelope this middleware renders must be the API's one envelope
// shape, field for field (docs/45). cmd/api/authhttp is the canonical
// renderer; internal/** cannot import cmd/**, so the two are pinned here.
func TestRateLimitEnvelopeHasTheCanonicalFields(t *testing.T) {
	lim := newFakeLimiter()
	lim.err = errors.New("boom")
	mw := RateLimit(lim, testConfig())
	rec := request(t, mw, httptest.NewRequest(http.MethodGet, "/x", nil))

	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("body: %v", err)
	}
	for _, field := range []string{"code", "message", "request_id", "retryable"} {
		if _, ok := decoded[field]; !ok {
			t.Errorf("envelope is missing field %q: %s", field, rec.Body.String())
		}
	}
	if len(decoded) != 4 {
		t.Errorf("envelope has %d fields, want exactly 4: %s", len(decoded), rec.Body.String())
	}
	if got := rec.Header().Get(HeaderContentType); got != "application/json" {
		t.Errorf("envelope Content-Type = %q, want application/json", got)
	}
}
