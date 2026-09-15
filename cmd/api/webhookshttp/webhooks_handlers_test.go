package webhookshttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/webhooks"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/persistence/memstore"
)

// The webhook surface's wire mapping, tested through the REAL guard (a
// real signup issues the session, the real CSRF check protects writes)
// and the real service over a stub store. The assertions are about the
// handler's decisions — route patterns, the once-only secret render, the
// error-code mapping — while the store and service policies have their
// own suites.

const testWebhookID = "11111111-2222-4333-8444-555555555555"

// stubWebhookStore implements webhooks.Store in memory: endpoints are
// owner-scoped (a foreign userID answers ErrWebhookNotFound, the orgs
// disclosure rule) and every method has a failure injection field.
type stubWebhookStore struct {
	mu         sync.Mutex
	ownerID    string
	endpoints  map[string]events.WebhookEndpoint
	deliveries map[string][]events.Delivery

	createErr, listErr, getErr, updateErr, rotateErr, deleteErr, listDeliveriesErr, redeliverErr error
}

func newStubWebhookStore(ownerID string) *stubWebhookStore {
	return &stubWebhookStore{ownerID: ownerID, endpoints: map[string]events.WebhookEndpoint{}, deliveries: map[string][]events.Delivery{}}
}

func (s *stubWebhookStore) owned(userID string) error {
	if userID != s.ownerID {
		return events.ErrWebhookNotFound
	}
	return nil
}

func (s *stubWebhookStore) CreateEndpoint(_ context.Context, userID, url, secret string, filters []string) (events.WebhookEndpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createErr != nil {
		return events.WebhookEndpoint{}, s.createErr
	}
	e := events.WebhookEndpoint{
		ID: "00000000-0000-4000-8000-" + strings.Repeat("1", 12), UserID: userID, URL: url,
		Secret: secret, EventFilters: filters, Enabled: true, CreatedAt: time.Now(),
	}
	s.endpoints[e.ID] = e
	return e, nil
}

func (s *stubWebhookStore) ListEndpoints(_ context.Context, userID string) ([]events.WebhookEndpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	if err := s.owned(userID); err != nil {
		return nil, err
	}
	var out []events.WebhookEndpoint
	for _, e := range s.endpoints {
		out = append(out, e)
	}
	return out, nil
}

func (s *stubWebhookStore) GetEndpoint(_ context.Context, userID, id string) (events.WebhookEndpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return events.WebhookEndpoint{}, s.getErr
	}
	if err := s.owned(userID); err != nil {
		return events.WebhookEndpoint{}, err
	}
	e, ok := s.endpoints[id]
	if !ok {
		return events.WebhookEndpoint{}, events.ErrWebhookNotFound
	}
	return e, nil
}

func (s *stubWebhookStore) UpdateEndpoint(_ context.Context, userID, id string, url *string, filters []string, enabled *bool) (events.WebhookEndpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.updateErr != nil {
		return events.WebhookEndpoint{}, s.updateErr
	}
	if err := s.owned(userID); err != nil {
		return events.WebhookEndpoint{}, err
	}
	e, ok := s.endpoints[id]
	if !ok {
		return events.WebhookEndpoint{}, events.ErrWebhookNotFound
	}
	if url != nil {
		e.URL = *url
	}
	if enabled != nil {
		e.Enabled = *enabled
	}
	s.endpoints[id] = e
	return e, nil
}

func (s *stubWebhookStore) RegenerateSecret(_ context.Context, userID, id, secret string) (events.WebhookEndpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rotateErr != nil {
		return events.WebhookEndpoint{}, s.rotateErr
	}
	if err := s.owned(userID); err != nil {
		return events.WebhookEndpoint{}, err
	}
	e, ok := s.endpoints[id]
	if !ok {
		return events.WebhookEndpoint{}, events.ErrWebhookNotFound
	}
	e.Secret = secret
	s.endpoints[id] = e
	return e, nil
}

func (s *stubWebhookStore) DeleteEndpoint(_ context.Context, userID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteErr != nil {
		return s.deleteErr
	}
	if err := s.owned(userID); err != nil {
		return err
	}
	if _, ok := s.endpoints[id]; !ok {
		return events.ErrWebhookNotFound
	}
	delete(s.endpoints, id)
	return nil
}

func (s *stubWebhookStore) ListDeliveries(_ context.Context, userID, endpointID string, _ *time.Time, _ string, _ int) ([]events.Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listDeliveriesErr != nil {
		return nil, s.listDeliveriesErr
	}
	// The real store's join answers an EMPTY page for a foreign endpoint
	// (the service documents this: no existence disclosure, and the
	// sub-resource read is fine with it).
	if err := s.owned(userID); err != nil {
		return []events.Delivery{}, nil
	}
	if _, ok := s.endpoints[endpointID]; !ok {
		return []events.Delivery{}, nil
	}
	return s.deliveries[endpointID], nil
}

func (s *stubWebhookStore) Redeliver(_ context.Context, userID, endpointID, deliveryID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.redeliverErr != nil {
		return s.redeliverErr
	}
	if err := s.owned(userID); err != nil {
		return err
	}
	if _, ok := s.endpoints[endpointID]; !ok {
		return events.ErrWebhookNotFound
	}
	if deliveryID == "pending-delivery" {
		return events.ErrWebhookNotRedeliverable
	}
	return nil
}

// webhookTestServer composes the guarded surface the same way
// cmd/api/main.go does: real auth (memstore), the webhook API mounted at
// /api/v1/webhooks and /api/v1/webhooks/, the whole thing behind
// authAPI.Guard.
type webhookTestServer struct {
	ts     *httptest.Server
	client *http.Client
	csrf   string
	userID string
	store  *stubWebhookStore
}

func newWebhookTestServer(t *testing.T, st webhooks.Store) *webhookTestServer {
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
	api := New(Deps{Store: st})
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	mux.Handle("/api/v1/webhooks", api.Routes())
	mux.Handle("/api/v1/webhooks/", api.Routes())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(observability.Middleware(logger)(authAPI.Guard(mux)))
	t.Cleanup(ts.Close)

	client, csrf, userID := signupWebhookUser(t, ts, "webhook-alice@example.com", "webhook-alice", "Alice")
	return &webhookTestServer{ts: ts, client: client, csrf: csrf, userID: userID, store: st.(*stubWebhookStore)}
}

func signupWebhookUser(t *testing.T, ts *httptest.Server, email, handle, name string) (*http.Client, string, string) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Post(ts.URL+"/api/v1/auth/signup", "application/json",
		strings.NewReader(`{"email":"`+email+`","password":"long-enough-password-1","handle":"`+handle+`","display_name":"`+name+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("signup = %d (body %s)", resp.StatusCode, webhookBodyOf(resp))
	}
	var payload struct {
		CSRFToken string `json:"csrf_token"`
		User      struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	return client, payload.CSRFToken, payload.User.ID
}

func webhookBodyOf(resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// request performs one request with the session cookie and, for
// non-GET methods, the CSRF header (unless withCSRF is false).
func (s *webhookTestServer) request(t *testing.T, method, path, body string, withCSRF bool) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, s.ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet && withCSRF {
		req.Header.Set("X-CSRF-Token", s.csrf)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (s *webhookTestServer) get(t *testing.T, path string) *http.Response {
	t.Helper()
	return s.request(t, http.MethodGet, path, "", true)
}

// decodeEnvelope unmarshals one JSON response into a map.
func decodeEnvelope(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("response body is not JSON: %v", err)
	}
	return payload
}

func seedEndpoint(t *testing.T, s *webhookTestServer, id, url string) {
	t.Helper()
	s.store.endpoints[id] = events.WebhookEndpoint{
		ID: id, UserID: s.userID, URL: url, EventFilters: []string{},
		Enabled: true, CreatedAt: time.Now(),
	}
}

// TestWebhookRoutesRespondOnRegisteredPatterns: every route answers on its
// method+path pattern, a wrong method answers 405, and an unknown
// sub-path 404 — the mount mirrors cmd/api/main.go.
func TestWebhookRoutesRespondOnRegisteredPatterns(t *testing.T) {
	st := newStubWebhookStore("")
	srv := newWebhookTestServer(t, st)
	st.ownerID = srv.userID
	seedEndpoint(t, srv, testWebhookID, "https://example.com/hook")
	st.deliveries[testWebhookID] = []events.Delivery{{
		ID: "d1", EndpointID: testWebhookID, EventID: "ev1", EventType: "state.committed",
		Status: events.DeliveryDelivered,
	}}

	cases := []struct {
		method string
		path   string
		body   string
		status int
	}{
		{http.MethodPost, "/api/v1/webhooks", `{"url":"https://example.com/new"}`, http.StatusCreated},
		{http.MethodGet, "/api/v1/webhooks", "", http.StatusOK},
		{http.MethodGet, "/api/v1/webhooks/" + testWebhookID, "", http.StatusOK},
		{http.MethodPatch, "/api/v1/webhooks/" + testWebhookID, `{"url":"https://example.com/patched"}`, http.StatusOK},
		{http.MethodPost, "/api/v1/webhooks/" + testWebhookID + "/secret", "", http.StatusOK},
		{http.MethodGet, "/api/v1/webhooks/" + testWebhookID + "/deliveries", "", http.StatusOK},
		{http.MethodPost, "/api/v1/webhooks/" + testWebhookID + "/deliveries/d1/retry", "", http.StatusNoContent},
		{http.MethodDelete, "/api/v1/webhooks/" + testWebhookID, "", http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			resp := srv.request(t, tc.method, tc.path, tc.body, true)
			if resp.StatusCode != tc.status {
				t.Fatalf("%s %s = %d, want %d (body %s)", tc.method, tc.path, resp.StatusCode, tc.status, webhookBodyOf(resp))
			}
		})
	}

	// A registered path with a wrong method answers 405.
	resp := srv.request(t, http.MethodPut, "/api/v1/webhooks", "", true)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /api/v1/webhooks = %d, want 405", resp.StatusCode)
	}
	// An unknown sub-path answers 404, not a panic or a 405.
	resp = srv.get(t, "/api/v1/webhooks/"+testWebhookID+"/nope")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown sub-path = %d, want 404", resp.StatusCode)
	}
}

// TestWebhookCreateRendersSecretExactlyOnce: the create response carries
// the secret in the dedicated field and NOWHERE inside the webhook object.
func TestWebhookCreateRendersSecretExactlyOnce(t *testing.T) {
	st := newStubWebhookStore("")
	srv := newWebhookTestServer(t, st)
	st.ownerID = srv.userID

	resp := srv.request(t, http.MethodPost, "/api/v1/webhooks",
		`{"url":"https://example.com/hook","event_filters":["state.committed"]}`, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d (body %s)", resp.StatusCode, webhookBodyOf(resp))
	}
	payload := decodeEnvelope(t, resp)
	secret, ok := payload["secret"].(string)
	if !ok || secret == "" {
		t.Fatalf("create response must carry the one-time secret: %v", payload)
	}
	hook, ok := payload["webhook"].(map[string]any)
	if !ok {
		t.Fatalf("create response lacks the webhook object: %v", payload)
	}
	if _, leak := hook["secret"]; leak {
		t.Fatalf("the webhook object leaks the secret: %v", hook)
	}
	if hook["url"] != "https://example.com/hook" || hook["enabled"] != true {
		t.Errorf("webhook object = %v", hook)
	}
}

// TestWebhookReadsNeverReturnTheSecret: list, get, update and rotate all
// render the endpoint without any secret key; only the dedicated
// create/rotate response fields carry it.
func TestWebhookReadsNeverReturnTheSecret(t *testing.T) {
	st := newStubWebhookStore("")
	srv := newWebhookTestServer(t, st)
	st.ownerID = srv.userID
	seedEndpoint(t, srv, testWebhookID, "https://example.com/hook")
	base := "/api/v1/webhooks/" + testWebhookID

	checks := []struct {
		name string
		resp *http.Response
	}{
		{"list", srv.get(t, "/api/v1/webhooks")},
		{"get", srv.get(t, base)},
		{"update", srv.request(t, http.MethodPatch, base, `{"url":"https://example.com/patched"}`, true)},
	}
	for _, c := range checks {
		body := webhookBodyOf(c.resp)
		if strings.Contains(body, `"secret"`) {
			t.Fatalf("%s response leaks a secret key: %s", c.name, body)
		}
	}

	// The rotate response carries the NEW secret once, in its own field —
	// and still not inside the webhook object.
	resp := srv.request(t, http.MethodPost, base+"/secret", "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate = %d", resp.StatusCode)
	}
	payload := decodeEnvelope(t, resp)
	if secret, _ := payload["secret"].(string); secret == "" {
		t.Fatalf("rotate response must carry the new secret: %v", payload)
	}
	if hook, ok := payload["webhook"].(map[string]any); ok {
		if _, leak := hook["secret"]; leak {
			t.Fatalf("rotate webhook object leaks the secret: %v", hook)
		}
	}
}

// TestWebhookAuthGuard: unauthenticated requests answer 401 on reads AND
// writes; an authenticated write without the CSRF header answers 403.
func TestWebhookAuthGuard(t *testing.T) {
	st := newStubWebhookStore("")
	srv := newWebhookTestServer(t, st)
	st.ownerID = srv.userID
	seedEndpoint(t, srv, testWebhookID, "https://example.com/hook")

	anon := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	do := func(method, path string, csrf bool) *http.Response {
		req, err := http.NewRequest(method, srv.ts.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if csrf {
			req.Header.Set("X-CSRF-Token", srv.csrf)
		}
		resp, err := anon.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/webhooks"},
		{http.MethodGet, "/api/v1/webhooks/" + testWebhookID},
		{http.MethodPost, "/api/v1/webhooks"},
	} {
		resp := do(tc.method, tc.path, true)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s %s = %d, want 401", tc.method, tc.path, resp.StatusCode)
		}
		var envelope struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Code != authn.CodeUnauthenticated {
			t.Errorf("code = %q, want %q", envelope.Code, authn.CodeUnauthenticated)
		}
	}

	// A write without the CSRF header is refused even with a session.
	resp := srv.request(t, http.MethodPost, "/api/v1/webhooks", `{"url":"https://example.com/x"}`, false)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("write without CSRF = %d, want 403", resp.StatusCode)
	}
	var envelope struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Code != authn.CodeCSRFFailed {
		t.Errorf("code = %q, want %q", envelope.Code, authn.CodeCSRFFailed)
	}
}

// TestWebhookCrossUserAnswersNotFound: a second authenticated user sees
// the first user's endpoint as not-found (no existence disclosure) on
// every route.
func TestWebhookCrossUserAnswersNotFound(t *testing.T) {
	st := newStubWebhookStore("")
	srv := newWebhookTestServer(t, st)
	st.ownerID = srv.userID
	seedEndpoint(t, srv, testWebhookID, "https://example.com/hook")

	// A second, independent user.
	bob, bobCSRF, _ := signupWebhookUser(t, srv.ts, "webhook-bob@example.com", "webhook-bob", "Bob")
	do := func(method, path, body string) *http.Response {
		req, err := http.NewRequest(method, srv.ts.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-CSRF-Token", bobCSRF)
		resp, err := bob.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/api/v1/webhooks/" + testWebhookID, ""},
		{http.MethodPatch, "/api/v1/webhooks/" + testWebhookID, `{}`},
		{http.MethodDelete, "/api/v1/webhooks/" + testWebhookID, ""},
	} {
		resp := do(tc.method, tc.path, tc.body)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("cross-user %s %s = %d, want 404", tc.method, tc.path, resp.StatusCode)
		}
		var envelope struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Code != webhooks.CodeEndpointNotFound {
			t.Errorf("code = %q, want %q", envelope.Code, webhooks.CodeEndpointNotFound)
		}
	}

	// The delivery log is the one sub-resource read whose contract is an
	// EMPTY page for a foreign endpoint (the store's join, no disclosure) —
	// 200, not 404.
	resp := do(http.MethodGet, "/api/v1/webhooks/"+testWebhookID+"/deliveries", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cross-user deliveries = %d, want 200 (empty page)", resp.StatusCode)
	}
	payload := decodeEnvelope(t, resp)
	if list, _ := payload["deliveries"].([]any); list == nil || len(list) != 0 {
		t.Fatalf("cross-user deliveries = %v, want an empty page", payload["deliveries"])
	}
}

// TestWebhookErrorMapping: the sentinel-to-wire table — 400 for validation
// (including a non-UUID pagination key), 404 for unknown, 409 for the
// policy conflicts, 503 for store outages.
func TestWebhookErrorMapping(t *testing.T) {
	t.Run("400 bad url", func(t *testing.T) {
		st := newStubWebhookStore("")
		srv := newWebhookTestServer(t, st)
		st.ownerID = srv.userID
		resp := srv.request(t, http.MethodPost, "/api/v1/webhooks", `{"url":"ftp://example.com/x"}`, true)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("create bad url = %d, want 400", resp.StatusCode)
		}
		assertWebhookErrorCode(t, resp, webhooks.CodeValidationFailed)
	})

	t.Run("400 malformed body", func(t *testing.T) {
		st := newStubWebhookStore("")
		srv := newWebhookTestServer(t, st)
		st.ownerID = srv.userID
		resp := srv.request(t, http.MethodPost, "/api/v1/webhooks", `{"url":`, true)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("malformed body = %d, want 400", resp.StatusCode)
		}
		assertWebhookErrorCode(t, resp, webhooks.CodeValidationFailed)
	})

	t.Run("400 non-uuid before key", func(t *testing.T) {
		st := newStubWebhookStore("")
		srv := newWebhookTestServer(t, st)
		st.ownerID = srv.userID
		seedEndpoint(t, srv, testWebhookID, "https://example.com/hook")
		resp := srv.get(t, "/api/v1/webhooks/"+testWebhookID+"/deliveries?before=2026-09-15T00:00:00Z,not-a-uuid")
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("bad before key = %d, want 400 (body %s)", resp.StatusCode, webhookBodyOf(resp))
		}
		assertWebhookErrorCode(t, resp, webhooks.CodeValidationFailed)
	})

	t.Run("404 unknown endpoint", func(t *testing.T) {
		st := newStubWebhookStore("")
		srv := newWebhookTestServer(t, st)
		st.ownerID = srv.userID
		resp := srv.get(t, "/api/v1/webhooks/99999999-9999-4999-8999-999999999999")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("unknown endpoint = %d, want 404", resp.StatusCode)
		}
		assertWebhookErrorCode(t, resp, webhooks.CodeEndpointNotFound)
	})

	t.Run("409 limit", func(t *testing.T) {
		st := newStubWebhookStore("")
		st.createErr = events.ErrWebhookLimit
		srv := newWebhookTestServer(t, st)
		st.ownerID = srv.userID
		resp := srv.request(t, http.MethodPost, "/api/v1/webhooks", `{"url":"https://example.com/x"}`, true)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("limit = %d, want 409", resp.StatusCode)
		}
		assertWebhookErrorCode(t, resp, webhooks.CodeEndpointLimit)
	})

	t.Run("409 not redeliverable", func(t *testing.T) {
		st := newStubWebhookStore("")
		srv := newWebhookTestServer(t, st)
		st.ownerID = srv.userID
		seedEndpoint(t, srv, testWebhookID, "https://example.com/hook")
		resp := srv.request(t, http.MethodPost, "/api/v1/webhooks/"+testWebhookID+"/deliveries/pending-delivery/retry", "", true)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("not redeliverable = %d, want 409", resp.StatusCode)
		}
		assertWebhookErrorCode(t, resp, webhooks.CodeNotRedeliverable)
	})

	t.Run("503 store outage", func(t *testing.T) {
		st := newStubWebhookStore("")
		st.listErr = errors.New("database is down")
		srv := newWebhookTestServer(t, st)
		st.ownerID = srv.userID
		resp := srv.get(t, "/api/v1/webhooks")
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("store outage = %d, want 503", resp.StatusCode)
		}
		assertWebhookErrorCode(t, resp, webhooks.CodeServiceUnavailable)
	})
}

func assertWebhookErrorCode(t *testing.T, resp *http.Response, want string) {
	t.Helper()
	var envelope struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("error body is not JSON: %v", err)
	}
	if envelope.Code != want {
		t.Errorf("code = %q, want %q", envelope.Code, want)
	}
}
