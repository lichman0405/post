package gitprovider_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/lichman0405/post/internal/gitprovider"
	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

// The push webhook receiver (T0305): transport behavior only — the raw
// body is read once and signed against the provisioned secret, and the
// ingester's verdict maps onto the status code the provider's redelivery
// policy consumes. Inspection/classification live in the ingester tests
// above; the full loop (real push → receiver → canonical rows) lives in the
// integration suite.

const (
	hookPath       = "/api/v1/git/hooks/gitea"
	hookTestSecret = "webhook-secret"
)

// hookBody builds a delivery-shaped push payload for the receiver tests.
func hookBody(ref, before, after string, repoID int64) string {
	return `{"ref":"` + ref + `","before":"` + before + `","after":"` + after + `",
	  "repository":{"id":` + strconv.FormatInt(repoID, 10) + `,"name":"p-abc","owner":{"login":"post-git-svc"}},
	  "pusher":{"login":"post-git-svc"},"total_commits":1,"commits":[]}`
}

// signHook returns the provider's signature header value for a body.
func signHook(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

// hookRequest builds a delivery request against the receiver with the
// scripted port and store, returning the recorder and the store.
func hookRequest(t *testing.T, body, signature, deliveryID, event string, port gitprovider.GitPort, store *fakeIngestStore) *httptest.ResponseRecorder {
	t.Helper()
	reg := testRegistry(t)
	handler := gitprovider.NewPushWebhookHandler(gitprovider.NewPushIngester(port, store, reg), store)
	req := httptest.NewRequest(http.MethodPost, hookPath, strings.NewReader(body))
	req.Header.Set("X-Gitea-Event", event)
	if signature != "" {
		req.Header.Set("X-Gitea-Signature", signature)
	}
	if deliveryID != "" {
		req.Header.Set("X-Gitea-Delivery", deliveryID)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestPushWebhookIngestsVerifiedDelivery(t *testing.T) {
	body := hookBody("refs/heads/semantic", testSHA, testSHA2, 1)
	store := &fakeIngestStore{secret: hookTestSecret, inserted: true}
	rec := hookRequest(t, body, signHook(hookTestSecret, body), "d-1", "push", &fakePort{}, store)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body %s", rec.Code, rec.Body.String())
	}
	if len(store.params) != 1 {
		t.Fatalf("ingest handoffs = %d, want 1", len(store.params))
	}
	ev := store.params[0].Event
	if ev.Ref != "refs/heads/semantic" || ev.After != testSHA2 || ev.DeliveryID != "d-1" {
		t.Errorf("ingested event = %+v", ev)
	}
}

func TestPushWebhookDuplicateStillNoContent(t *testing.T) {
	// A redelivered webhook ingests nothing (the store's dedupe verdict)
	// and still answers 204: the delivery succeeded, there is nothing to
	// redo, and an error would redeliver a no-op forever.
	body := hookBody("refs/heads/semantic", testSHA, testSHA2, 1)
	store := &fakeIngestStore{secret: hookTestSecret, inserted: false}
	rec := hookRequest(t, body, signHook(hookTestSecret, body), "d-2", "push", &fakePort{}, store)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if len(store.params) != 1 {
		t.Errorf("ingest handoffs = %d, want 1 (the duplicate verdict)", len(store.params))
	}
}

func TestPushWebhookRejectsBadSignature(t *testing.T) {
	body := hookBody("refs/heads/semantic", testSHA, testSHA2, 1)
	store := &fakeIngestStore{secret: hookTestSecret}
	rec := hookRequest(t, body, signHook("wrong-secret", body), "d-1", "push", &fakePort{}, store)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(store.params) != 0 {
		t.Error("bad signature reached the ingester")
	}
}

func TestPushWebhookRejectsMissingSignature(t *testing.T) {
	body := hookBody("refs/heads/semantic", testSHA, testSHA2, 1)
	store := &fakeIngestStore{secret: hookTestSecret}
	rec := hookRequest(t, body, "", "d-1", "push", &fakePort{}, store)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(store.params) != 0 {
		t.Error("missing signature reached the ingester")
	}
}

// denialValue reads post_permission_denials_total{surface,decision} off the
// process registry's exposition. The counter lives on
// observability.Default() — the same process-wide instance the receiver
// increments — so a before/after delta is the receiver's own contribution.
func denialValue(t *testing.T, surface, decision string) float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	promhttp.HandlerFor(observability.Default().Gatherer(), promhttp.HandlerOpts{}).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	want := `post_permission_denials_total{decision="` + decision + `",surface="` + surface + `"} `
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, want) {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, want)), 64)
		if err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		return v
	}
	return 0
}

// The receiver counts its own refusals (docs/26 §3 "permission denied
// rates"). It is the one refusal site in the binary outside
// cmd/api/authhttp/envelope.go: the route is registered on the root mux,
// outside the session/CSRF guard, so it answers by writing the status
// directly and the envelope never sees it. Without these counts the frozen
// main 403 — the only signal an operator gets that somebody pushed to a
// frozen main — is missing from the single denial-rate rule in the file.
func TestPushWebhookCountsFrozenMainRefusal(t *testing.T) {
	before := denialValue(t, "gitprovider", "forbidden")
	body := hookBody(gitprovider.MainRef, testSHA, testSHA2, 1)
	store := &fakeIngestStore{secret: hookTestSecret, mainFrozen: true}

	rec := hookRequest(t, body, signHook(hookTestSecret, body), "d-frozen", "push", &fakePort{}, store)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body %s", rec.Code, rec.Body.String())
	}
	if got := denialValue(t, "gitprovider", "forbidden"); got != before+1 {
		t.Errorf(`post_permission_denials_total{decision="forbidden",surface="gitprovider"} = %v, want %v: a frozen-main delivery was answered 403 without being counted as a permission denial`, got, before+1)
	}
}

// ...and the two signature refusals are counted as unauthenticated, so the
// surface is not half-instrumented. Both statuses are counted for the same
// reason: the HMAC over the raw body IS this receiver's authentication, so
// failing it is the 401 class the family already defines.
func TestPushWebhookCountsSignatureRefusals(t *testing.T) {
	before := denialValue(t, "gitprovider", "unauthenticated")
	body := hookBody("refs/heads/semantic", testSHA, testSHA2, 1)
	store := &fakeIngestStore{secret: hookTestSecret}

	for _, tc := range []struct{ name, signature string }{
		{"missing", ""},
		{"bad", signHook("wrong-secret", body)},
	} {
		rec := hookRequest(t, body, tc.signature, "d-"+tc.name, "push", &fakePort{}, store)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s signature: status = %d, want 401", tc.name, rec.Code)
		}
	}
	if got := denialValue(t, "gitprovider", "unauthenticated"); got != before+2 {
		t.Errorf(`post_permission_denials_total{decision="unauthenticated",surface="gitprovider"} = %v, want %v: two refused deliveries were not counted`, got, before+2)
	}
}

func TestPushWebhookIgnoresNonPushEvent(t *testing.T) {
	// A delivery for another event type (the provider registers push only)
	// is a no-op success — an error would redeliver it forever.
	store := &fakeIngestStore{secret: hookTestSecret}
	rec := hookRequest(t, `{}`, "", "", "create", &fakePort{}, store)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if len(store.params) != 0 {
		t.Error("non-push delivery reached the ingester")
	}
}

func TestPushWebhookIgnoresTagPush(t *testing.T) {
	// A tag push is a legal delivery the ingestion has nothing to do for:
	// verified (any push event must be), parsed, and answered 204.
	body := hookBody("refs/tags/v1", testSHA, testSHA2, 1)
	store := &fakeIngestStore{secret: hookTestSecret}
	rec := hookRequest(t, body, signHook(hookTestSecret, body), "d-1", "push", &fakePort{}, store)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if len(store.params) != 0 {
		t.Error("tag push reached the ingester")
	}
	// Without a valid signature even a tag push is 401: unverifiable
	// deliveries are rejected before anything is interpreted.
	rec = hookRequest(t, body, signHook("wrong", body), "d-1", "push", &fakePort{}, store)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned tag push status = %d, want 401", rec.Code)
	}
	if len(store.params) != 0 {
		t.Error("unsigned tag push reached the ingester")
	}
}

func TestPushWebhookRejectsMalformedPayload(t *testing.T) {
	store := &fakeIngestStore{secret: hookTestSecret}
	rec := hookRequest(t, `{"ref":`, signHook(hookTestSecret, `{"ref":`), "d-1", "push", &fakePort{}, store)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(store.params) != 0 {
		t.Error("malformed payload reached the ingester")
	}
}

func TestPushWebhookUnknownRepository(t *testing.T) {
	// A signed delivery for a repository the platform does not own: 404 —
	// retrying cannot fix it, and the provider records the delivery as
	// failed rather than redelivering a no-op forever.
	body := hookBody("refs/heads/semantic", testSHA, testSHA2, 1)
	store := &fakeIngestStore{secretErr: gitprovider.ErrRepoNotProvisioned}
	rec := hookRequest(t, body, signHook(hookTestSecret, body), "d-1", "push", &fakePort{}, store)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if len(store.params) != 0 {
		t.Error("unprovisioned delivery reached the ingester")
	}
}

func TestPushWebhookIngestionFailureIs503(t *testing.T) {
	// The ingestion failed AFTER the delivery was verified: 503 — the
	// provider's redelivery is the bounded retry (the dedupe key makes an
	// already-recorded delivery a no-op on the retry).
	body := hookBody("refs/heads/semantic", testSHA, testSHA2, 1)
	port := &fakePort{changedFilesErr: gitprovider.ErrUnavailable}
	store := &fakeIngestStore{secret: hookTestSecret, inserted: true}
	rec := hookRequest(t, body, signHook(hookTestSecret, body), "d-1", "push", port, store)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// testRegistry loads the canonical schema registry once per test.
func testRegistry(t *testing.T) *schemareg.Registry {
	t.Helper()
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	return reg
}
