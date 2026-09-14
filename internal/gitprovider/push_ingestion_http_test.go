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

	"github.com/lichman0405/post/internal/gitprovider"
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
