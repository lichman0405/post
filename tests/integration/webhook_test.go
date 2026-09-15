package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

// Task T1006: Signed Webhooks — over a REAL PostgreSQL plus real HTTP
// receivers (httptest), the whole pipeline: outbox -> research_events ->
// fan-out -> signed delivery -> receiver verification, with the delivery
// log, retries and the disable policy. Required test "webhook security"
// (replay / bad signature) and the acceptance criterion "duplicate event
// consumer safe" are asserted here against the bytes a real receiver gets.

const webhookTaskID = "T1006"

// webhookFixture seeds alice and wires the production pipeline pieces over
// one real database: the outbox dispatcher, the fan-out and the
// deliverer. The deliverer dials permissively — the SSRF guard is unit
// tested in the events package, and the httptest receivers live on
// loopback by construction.
type webhookFixture struct {
	pool       *pgxpool.Pool
	alice      domain.User
	store      *events.WebhookStore
	dispatcher *events.Dispatcher
	fanout     *events.FanOut
}

func newWebhookFixture(t *testing.T, ctx context.Context) *webhookFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), webhookTaskID)
	alice, err := persistence.NewCredentialStore(pool).CreateWithPassword(
		ctx, "webhook-alice@example.com", "hash", "webhook-alice", "Alice")
	if err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &webhookFixture{
		pool:       pool,
		alice:      alice,
		store:      events.NewWebhookStore(pool),
		dispatcher: events.NewDispatcher(pool, events.WithLogger(log)),
		fanout:     events.NewFanOut(pool, events.WithFanOutLogger(log)),
	}
}

func (f *webhookFixture) newDeliverer(opts ...events.DelivererOption) *events.Deliverer {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts = append([]events.DelivererOption{
		events.WithDelivererLogger(log),
		// Permissive dial: the receivers are httptest servers on loopback
		// (the guard itself is unit-tested in the events package).
		events.WithDelivererDialContext((&net.Dialer{}).DialContext),
	}, opts...)
	return events.NewDeliverer(f.pool, opts...)
}

// recordEvent writes one outbox row directly — the upstream half
// (domain write -> outbox) is T1001's covered path; the webhook pipeline
// starts at the published event.
func (f *webhookFixture) recordEvent(t *testing.T, ctx context.Context, e events.Event) {
	t.Helper()
	if err := events.Record(ctx, f.pool, e); err != nil {
		t.Fatalf("record event %s: %v", e.EventType, err)
	}
}

// publishAndFanOut runs the dispatcher and fan-out passes once.
func (f *webhookFixture) publishAndFanOut(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := f.dispatcher.RunOnce(ctx); err != nil {
		t.Fatalf("dispatcher RunOnce: %v", err)
	}
	if _, err := f.fanout.RunOnce(ctx); err != nil {
		t.Fatalf("fanout RunOnce: %v", err)
	}
}

// deliveries returns the delivery log rows, oldest first.
func (f *webhookFixture) deliveries(t *testing.T, ctx context.Context) []events.Delivery {
	t.Helper()
	rows, err := f.pool.Query(ctx, `
		SELECT d.id, d.endpoint_id, d.event_id, d.event_type, d.endpoint, d.status,
		       d.response_code, d.attempts, d.last_attempt_at, d.next_retry_at,
		       d.last_error, d.delivered_at, d.created_at
		FROM webhook_deliveries d ORDER BY d.created_at, d.id`)
	if err != nil {
		t.Fatalf("query deliveries: %v", err)
	}
	defer rows.Close()
	var out []events.Delivery
	for rows.Next() {
		var d events.Delivery
		var endpointID *string
		if err := rows.Scan(&d.ID, &endpointID, &d.EventID, &d.EventType, &d.EndpointURL, &d.Status,
			&d.ResponseCode, &d.Attempts, &d.LastAttemptAt, &d.NextRetryAt,
			&d.LastError, &d.DeliveredAt, &d.CreatedAt); err != nil {
			t.Fatalf("scan delivery: %v", err)
		}
		if endpointID != nil {
			d.EndpointID = *endpointID
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate deliveries: %v", err)
	}
	return out
}

// dueNow pulls every delivery's retry time into the past so the next
// deliverer pass claims it (the test accelerator — real deployments wait
// the backoff).
func (f *webhookFixture) dueNow(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := f.pool.Exec(ctx, `
		UPDATE webhook_deliveries SET next_retry_at = now() - interval '1 second'
		WHERE status = 'pending'`); err != nil {
		t.Fatalf("pull retries due: %v", err)
	}
}

// receivedDelivery is one request a test receiver captured.
type receivedDelivery struct {
	Headers http.Header
	Body    []byte
}

// verifyingReceiver is an httptest handler that records deliveries and
// answers a fixed status.
type verifyingReceiver struct {
	mu     sync.Mutex
	status int
	got    []receivedDelivery
}

func (r *verifyingReceiver) handler(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.got = append(r.got, receivedDelivery{Headers: req.Header.Clone(), Body: body})
	r.mu.Unlock()
	w.WriteHeader(r.status)
}

func (r *verifyingReceiver) received() []receivedDelivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]receivedDelivery, len(r.got))
	copy(out, r.got)
	return out
}

// TestWebhookDeliveryEndToEnd is the happy path with the security
// checks ON: a public event flows outbox -> research_events -> delivery
// row -> a real HTTP POST whose signature and timestamp a real receiver
// verifies, and the delivery log records the outcome.
func TestWebhookDeliveryEndToEnd(t *testing.T) {
	ctx := testCtx(t)
	f := newWebhookFixture(t, ctx)

	recv := &verifyingReceiver{status: http.StatusOK}
	srv := httptest.NewServer(http.HandlerFunc(recv.handler))
	defer srv.Close()

	endpoint, err := f.store.CreateEndpoint(ctx, f.alice.ID, srv.URL+"/hook", "test-secret", nil)
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}

	f.recordEvent(t, ctx, events.Event{
		EventType:  "state.committed",
		ActorID:    f.alice.ID,
		Visibility: events.VisibilityPublic,
		Payload:    json.RawMessage(`{"state_id":"s1"}`),
	})
	f.publishAndFanOut(t, ctx)

	rows := f.deliveries(t, ctx)
	if len(rows) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(rows))
	}
	if rows[0].Status != events.DeliveryPending || rows[0].EndpointID != endpoint.ID {
		t.Fatalf("delivery row = %+v, want pending for endpoint %s", rows[0], endpoint.ID)
	}
	if rows[0].EndpointURL != srv.URL+"/hook" {
		t.Fatalf("delivery endpoint snapshot = %q", rows[0].EndpointURL)
	}

	deliverer := f.newDeliverer()
	if delivered, failed, err := deliverer.RunOnce(ctx); err != nil || delivered != 1 || failed != 0 {
		t.Fatalf("RunOnce = (%d, %d, %v), want (1, 0, nil)", delivered, failed, err)
	}

	// The receiver got exactly one signed request, and it verifies.
	got := recv.received()
	if len(got) != 1 {
		t.Fatalf("receiver got %d requests, want 1", len(got))
	}
	req := got[0]
	ts, err := strconv.ParseInt(req.Headers.Get(events.TimestampHeader), 10, 64)
	if err != nil {
		t.Fatalf("timestamp header: %v", err)
	}
	if err := events.VerifySignature([]byte("test-secret"), ts, req.Body,
		req.Headers.Get(events.SignatureHeader), events.DefaultMaxTimestampSkew); err != nil {
		t.Fatalf("receiver-side verification failed: %v", err)
	}
	if req.Headers.Get(events.EventHeader) != "state.committed" {
		t.Errorf("event header = %q", req.Headers.Get(events.EventHeader))
	}
	if req.Headers.Get(events.EventIDHeader) != rows[0].EventID {
		t.Errorf("event id header = %q, want %q", req.Headers.Get(events.EventIDHeader), rows[0].EventID)
	}
	if req.Headers.Get(events.DeliveryIDHeader) != rows[0].ID {
		t.Errorf("delivery id header = %q, want %q", req.Headers.Get(events.DeliveryIDHeader), rows[0].ID)
	}
	var env map[string]any
	if err := json.Unmarshal(req.Body, &env); err != nil {
		t.Fatalf("delivery body is not JSON: %v", err)
	}
	if env["event_type"] != "state.committed" || env["visibility"] != events.VisibilityPublic {
		t.Errorf("envelope mismatch: %s", req.Body)
	}

	// The log row flipped to delivered with the response recorded.
	rows = f.deliveries(t, ctx)
	if rows[0].Status != events.DeliveryDelivered || rows[0].Attempts != 1 ||
		rows[0].ResponseCode == nil || *rows[0].ResponseCode != 200 || rows[0].DeliveredAt == nil {
		t.Fatalf("delivery log after success = %+v", rows[0])
	}
}

// TestWebhookSignatureSecurityReplayAndTampering is the required "webhook
// security" test on the REAL delivered bytes: a replayed delivery (old
// timestamp) and a tampered body both fail receiver-side verification,
// while the original passes — the property a consumer relies on.
func TestWebhookSignatureSecurityReplayAndTampering(t *testing.T) {
	ctx := testCtx(t)
	f := newWebhookFixture(t, ctx)

	recv := &verifyingReceiver{status: http.StatusOK}
	srv := httptest.NewServer(http.HandlerFunc(recv.handler))
	defer srv.Close()

	if _, err := f.store.CreateEndpoint(ctx, f.alice.ID, srv.URL+"/hook", "test-secret", nil); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	f.recordEvent(t, ctx, events.Event{
		EventType:  "release.published",
		Visibility: events.VisibilityPublic,
		Payload:    json.RawMessage(`{"release_id":"r1"}`),
	})
	f.publishAndFanOut(t, ctx)
	if _, _, err := f.newDeliverer().RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	got := recv.received()
	if len(got) != 1 {
		t.Fatalf("receiver got %d requests, want 1", len(got))
	}
	req := got[0]
	ts, _ := strconv.ParseInt(req.Headers.Get(events.TimestampHeader), 10, 64)
	sig := req.Headers.Get(events.SignatureHeader)

	// The original verifies.
	if err := events.VerifySignature([]byte("test-secret"), ts, req.Body, sig, events.DefaultMaxTimestampSkew); err != nil {
		t.Fatalf("original delivery must verify: %v", err)
	}

	// A replay: the same request re-sent 10 minutes later — the timestamp
	// is outside the window and verification fails closed.
	replayedAt := time.Now().Add(-10 * time.Minute).Unix()
	oldSig := events.Sign([]byte("test-secret"), replayedAt, req.Body)
	err := events.VerifySignature([]byte("test-secret"), replayedAt, req.Body, oldSig, events.DefaultMaxTimestampSkew)
	if !errors.Is(err, events.ErrReplay) {
		t.Fatalf("replayed delivery = %v, want ErrReplay", err)
	}

	// A tampered body: any change to the payload must break the HMAC.
	tampered := append(append([]byte{}, req.Body...), ' ')
	if err := events.VerifySignature([]byte("test-secret"), ts, tampered, sig, events.DefaultMaxTimestampSkew); !errors.Is(err, events.ErrBadSignature) {
		t.Fatalf("tampered body = %v, want ErrBadSignature", err)
	}

	// A wrong secret (a consumer misconfigured, or an attacker guessing).
	if err := events.VerifySignature([]byte("wrong-secret"), ts, req.Body, sig, events.DefaultMaxTimestampSkew); !errors.Is(err, events.ErrBadSignature) {
		t.Fatalf("wrong secret = %v, want ErrBadSignature", err)
	}
}

// TestWebhookDuplicateDeliveryConsumerSafe is the acceptance criterion
// "duplicate event consumer safe": the fan-out is exactly-once per
// (endpoint, event) — re-running it adds nothing — and a manual
// redelivery re-sends the SAME delivery id and event id, so an
// idempotent consumer can dedupe. The platform side stays consistent
// (one row, attempts counted, log truthful).
func TestWebhookDuplicateDeliveryConsumerSafe(t *testing.T) {
	ctx := testCtx(t)
	f := newWebhookFixture(t, ctx)

	recv := &verifyingReceiver{status: http.StatusOK}
	srv := httptest.NewServer(http.HandlerFunc(recv.handler))
	defer srv.Close()

	if _, err := f.store.CreateEndpoint(ctx, f.alice.ID, srv.URL+"/hook", "test-secret", nil); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	f.recordEvent(t, ctx, events.Event{
		EventType:  "state.committed",
		Visibility: events.VisibilityPublic,
		Payload:    json.RawMessage(`{"state_id":"dup"}`),
	})
	f.publishAndFanOut(t, ctx)

	// Re-running the fan-out must not create a second delivery row (the
	// partial unique index + the fanned-out cursor make it a no-op).
	if _, err := f.fanout.RunOnce(ctx); err != nil {
		t.Fatalf("fanout rerun: %v", err)
	}
	if rows := f.deliveries(t, ctx); len(rows) != 1 {
		t.Fatalf("deliveries after fan-out rerun = %d, want 1", len(rows))
	}

	deliverer := f.newDeliverer()
	if _, _, err := deliverer.RunOnce(ctx); err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	row := f.deliveries(t, ctx)[0]
	firstDeliveryID, firstEventID := row.ID, row.EventID

	// Manual redelivery (docs/22 §9 retry) re-queues the SAME delivery.
	if err := f.store.Redeliver(ctx, f.alice.ID, row.EndpointID, row.ID); err != nil {
		t.Fatalf("Redeliver: %v", err)
	}
	if _, _, err := deliverer.RunOnce(ctx); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}

	got := recv.received()
	if len(got) != 2 {
		t.Fatalf("consumer got %d requests, want 2 (original + redelivery)", len(got))
	}
	for i, g := range got {
		if g.Headers.Get(events.DeliveryIDHeader) != firstDeliveryID {
			t.Errorf("request %d delivery id = %q, want the SAME %q (the consumer's dedupe key)",
				i, g.Headers.Get(events.DeliveryIDHeader), firstDeliveryID)
		}
		if g.Headers.Get(events.EventIDHeader) != firstEventID {
			t.Errorf("request %d event id = %q, want %q", i, g.Headers.Get(events.EventIDHeader), firstEventID)
		}
		ts, _ := strconv.ParseInt(g.Headers.Get(events.TimestampHeader), 10, 64)
		if err := events.VerifySignature([]byte("test-secret"), ts, g.Body,
			g.Headers.Get(events.SignatureHeader), events.DefaultMaxTimestampSkew); err != nil {
			t.Errorf("request %d verification: %v", i, err)
		}
	}

	// The log still has exactly one row, now with two attempts.
	rows := f.deliveries(t, ctx)
	if len(rows) != 1 || rows[0].Status != events.DeliveryDelivered || rows[0].Attempts != 2 {
		t.Fatalf("delivery log after redelivery = %+v", rows)
	}
}

// TestWebhookFailingEndpointDisabledAfterLimit: consecutive failures
// disable the endpoint (the disable policy), stop the attempts, and emit
// the canonical webhook.delivery_failed event into the outbox.
func TestWebhookFailingEndpointDisabledAfterLimit(t *testing.T) {
	ctx := testCtx(t)
	f := newWebhookFixture(t, ctx)

	recv := &verifyingReceiver{status: http.StatusInternalServerError}
	srv := httptest.NewServer(http.HandlerFunc(recv.handler))
	defer srv.Close()

	endpoint, err := f.store.CreateEndpoint(ctx, f.alice.ID, srv.URL+"/hook", "test-secret", nil)
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	f.recordEvent(t, ctx, events.Event{
		EventType:  "state.committed",
		Visibility: events.VisibilityPublic,
		Payload:    json.RawMessage(`{"state_id":"failing"}`),
	})
	f.publishAndFanOut(t, ctx)

	// A lowered limit keeps the test fast; each pass is one failed attempt.
	const limit = 3
	deliverer := f.newDeliverer(events.WithDelivererFailureLimit(limit))
	for i := 1; i <= limit; i++ {
		f.dueNow(t, ctx)
		if delivered, failed, err := deliverer.RunOnce(ctx); err != nil || delivered != 0 || failed != 1 {
			t.Fatalf("attempt %d RunOnce = (%d, %d, %v), want (0, 1, nil)", i, delivered, failed, err)
		}
	}

	// The endpoint is disabled with its streak recorded.
	e, err := f.store.GetEndpoint(ctx, f.alice.ID, endpoint.ID)
	if err != nil {
		t.Fatalf("get endpoint: %v", err)
	}
	if e.Enabled || e.ConsecutiveFailures < limit || e.DisabledAt == nil {
		t.Fatalf("endpoint after %d failures = %+v, want disabled with streak >= %d", limit, e, limit)
	}

	// The delivery stays pending (it never succeeded) but the disabled
	// endpoint claims nothing — no further attempts.
	rows := f.deliveries(t, ctx)
	if rows[0].Status != events.DeliveryPending || rows[0].Attempts != limit {
		t.Fatalf("delivery row after disable = %+v", rows[0])
	}
	f.dueNow(t, ctx)
	if delivered, failed, err := deliverer.RunOnce(ctx); err != nil || delivered != 0 || failed != 0 {
		t.Fatalf("RunOnce against disabled endpoint = (%d, %d, %v), want (0, 0, nil)", delivered, failed, err)
	}

	// The canonical event is in the outbox and publishes.
	var eventType, visibility string
	if err := f.pool.QueryRow(ctx, `
		SELECT event_type, visibility FROM outbox_events
		WHERE event_type = 'webhook.delivery_failed'`).Scan(&eventType, &visibility); err != nil {
		t.Fatalf("webhook.delivery_failed outbox row: %v", err)
	}
	if visibility != events.VisibilityPrivate {
		t.Errorf("webhook.delivery_failed visibility = %q, want private (fail closed)", visibility)
	}
	if _, err := f.dispatcher.RunOnce(ctx); err != nil {
		t.Fatalf("publish delivery_failed: %v", err)
	}
	var published int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM research_events WHERE event_type = 'webhook.delivery_failed'`).Scan(&published); err != nil {
		t.Fatalf("count published delivery_failed: %v", err)
	}
	if published != 1 {
		t.Fatalf("published webhook.delivery_failed = %d, want 1", published)
	}

	// Re-enabling by the owner resets the streak — the owner's decision
	// deserves a clean slate.
	enabled := true
	updated, err := f.store.UpdateEndpoint(ctx, f.alice.ID, endpoint.ID, nil, nil, &enabled)
	if err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if !updated.Enabled || updated.ConsecutiveFailures != 0 || updated.DisabledAt != nil {
		t.Fatalf("re-enabled endpoint = %+v", updated)
	}
}

// TestWebhookPermanentFailureIsTerminal: a 4xx ends the delivery with
// status failed and no retry — retrying a refused request cannot help.
func TestWebhookPermanentFailureIsTerminal(t *testing.T) {
	ctx := testCtx(t)
	f := newWebhookFixture(t, ctx)

	recv := &verifyingReceiver{status: http.StatusBadRequest}
	srv := httptest.NewServer(http.HandlerFunc(recv.handler))
	defer srv.Close()

	if _, err := f.store.CreateEndpoint(ctx, f.alice.ID, srv.URL+"/hook", "test-secret", nil); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	f.recordEvent(t, ctx, events.Event{
		EventType:  "state.committed",
		Visibility: events.VisibilityPublic,
		Payload:    json.RawMessage(`{"state_id":"bad"}`),
	})
	f.publishAndFanOut(t, ctx)

	deliverer := f.newDeliverer()
	if delivered, failed, err := deliverer.RunOnce(ctx); err != nil || delivered != 0 || failed != 1 {
		t.Fatalf("RunOnce = (%d, %d, %v), want (0, 1, nil)", delivered, failed, err)
	}
	row := f.deliveries(t, ctx)[0]
	if row.Status != events.DeliveryFailed || row.ResponseCode == nil || *row.ResponseCode != 400 ||
		row.NextRetryAt != nil || row.LastError == nil {
		t.Fatalf("terminal failure row = %+v", row)
	}
	// No further attempts even after the retry time passes.
	f.dueNow(t, ctx)
	if delivered, failed, err := deliverer.RunOnce(ctx); err != nil || delivered != 0 || failed != 0 {
		t.Fatalf("RunOnce after terminal failure = (%d, %d, %v), want (0, 0, nil)", delivered, failed, err)
	}
}

// TestWebhookFanOutVisibilityAndFilters: private events go nowhere (the
// fail-closed V1 boundary), filters exact-match, and disabled endpoints
// receive nothing.
func TestWebhookFanOutVisibilityAndFilters(t *testing.T) {
	ctx := testCtx(t)
	f := newWebhookFixture(t, ctx)

	recv := &verifyingReceiver{status: http.StatusOK}
	srv := httptest.NewServer(http.HandlerFunc(recv.handler))
	defer srv.Close()

	allEvents, err := f.store.CreateEndpoint(ctx, f.alice.ID, srv.URL+"/all", "s1", nil)
	if err != nil {
		t.Fatalf("create all-events endpoint: %v", err)
	}
	releaseOnly, err := f.store.CreateEndpoint(ctx, f.alice.ID, srv.URL+"/release", "s2", []string{"release.published"})
	if err != nil {
		t.Fatalf("create filtered endpoint: %v", err)
	}

	// A private event: consumed by the fan-out, delivered to NO ONE.
	f.recordEvent(t, ctx, events.Event{
		EventType:  "state.committed",
		Visibility: events.VisibilityPrivate,
		Payload:    json.RawMessage(`{"state_id":"private"}`),
	})
	f.publishAndFanOut(t, ctx)
	if rows := f.deliveries(t, ctx); len(rows) != 0 {
		t.Fatalf("private event produced %d deliveries, want 0", len(rows))
	}

	// A public state.committed: only the unfiltered endpoint receives it.
	f.recordEvent(t, ctx, events.Event{
		EventType:  "state.committed",
		Visibility: events.VisibilityPublic,
		Payload:    json.RawMessage(`{"state_id":"public"}`),
	})
	f.publishAndFanOut(t, ctx)
	rows := f.deliveries(t, ctx)
	if len(rows) != 1 || rows[0].EndpointID != allEvents.ID {
		t.Fatalf("public event deliveries = %+v, want only %s", rows, allEvents.ID)
	}

	// A public release.published: both endpoints receive it.
	f.recordEvent(t, ctx, events.Event{
		EventType:  "release.published",
		Visibility: events.VisibilityPublic,
		Payload:    json.RawMessage(`{"release_id":"r"}`),
	})
	f.publishAndFanOut(t, ctx)
	rows = f.deliveries(t, ctx)
	if len(rows) != 3 {
		t.Fatalf("release event deliveries = %d, want 3", len(rows))
	}
	var hit []string
	for _, r := range rows {
		hit = append(hit, r.EndpointID)
	}
	if !contains(hit, allEvents.ID) || !contains(hit, releaseOnly.ID) {
		t.Fatalf("release event reached %v, want both endpoints", hit)
	}

	// Disable the unfiltered endpoint: new events skip it.
	disabled := false
	if _, err := f.store.UpdateEndpoint(ctx, f.alice.ID, allEvents.ID, nil, nil, &disabled); err != nil {
		t.Fatalf("disable endpoint: %v", err)
	}
	f.recordEvent(t, ctx, events.Event{
		EventType:  "release.published",
		Visibility: events.VisibilityPublic,
		Payload:    json.RawMessage(`{"release_id":"r2"}`),
	})
	f.publishAndFanOut(t, ctx)
	rows = f.deliveries(t, ctx)
	if len(rows) != 4 {
		t.Fatalf("deliveries after disable = %d, want 4 (one new row)", len(rows))
	}
	if rows[3].EndpointID != releaseOnly.ID {
		t.Fatalf("newest delivery went to %s, want only the enabled %s", rows[3].EndpointID, releaseOnly.ID)
	}
}

// TestWebhookEndpointLifecycle: the registry CRUD over the real store —
// the secret is shown exactly once, reads hide it, the limit holds, and
// deletion cancels pending deliveries while keeping the log rows.
func TestWebhookEndpointLifecycle(t *testing.T) {
	ctx := testCtx(t)
	f := newWebhookFixture(t, ctx)

	e, err := f.store.CreateEndpoint(ctx, f.alice.ID, "https://example.com/hook", "created-secret", []string{"state.committed"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if e.Secret != "created-secret" || !e.Enabled || e.ConsecutiveFailures != 0 {
		t.Fatalf("created endpoint = %+v", e)
	}

	// Reads never select the secret.
	got, err := f.store.GetEndpoint(ctx, f.alice.ID, e.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Secret != "" {
		t.Fatalf("GetEndpoint leaked the secret: %+v", got)
	}
	list, err := f.store.ListEndpoints(ctx, f.alice.ID)
	if err != nil || len(list) != 1 || list[0].Secret != "" {
		t.Fatalf("ListEndpoints = %+v (err %v), want one secret-free row", list, err)
	}

	// Foreign ownership answers not-found without disclosure.
	if _, err := f.store.GetEndpoint(ctx, "99999999-9999-4999-8999-999999999999", e.ID); !errors.Is(err, events.ErrWebhookNotFound) {
		t.Fatalf("foreign GetEndpoint = %v, want ErrWebhookNotFound", err)
	}

	// RegenerateSecret is the only other path that returns the secret.
	rotated, err := f.store.RegenerateSecret(ctx, f.alice.ID, e.ID, "rotated-secret")
	if err != nil || rotated.Secret != "rotated-secret" {
		t.Fatalf("regenerate = %+v (err %v)", rotated, err)
	}
	if got, _ := f.store.GetEndpoint(ctx, f.alice.ID, e.ID); got.Secret != "" {
		t.Fatal("secret leaked after regeneration")
	}

	// Update: url/filters/enabled PATCH semantics.
	newURL := "https://example.com/other"
	rotatedURL, err := f.store.UpdateEndpoint(ctx, f.alice.ID, e.ID, &newURL, []string{"release.published"}, nil)
	if err != nil || rotatedURL.URL != newURL || len(rotatedURL.EventFilters) != 1 {
		t.Fatalf("update = %+v (err %v)", rotatedURL, err)
	}

	// The per-user limit.
	for i := 0; i < events.MaxEndpointsPerUser-1; i++ {
		if _, err := f.store.CreateEndpoint(ctx, f.alice.ID, "https://example.com/hook", "s", nil); err != nil {
			t.Fatalf("create #%d: %v", i, err)
		}
	}
	if _, err := f.store.CreateEndpoint(ctx, f.alice.ID, "https://example.com/hook", "s", nil); !errors.Is(err, events.ErrWebhookLimit) {
		t.Fatalf("create past limit = %v, want ErrWebhookLimit", err)
	}

	// Deletion is a soft delete (docs/04 §6): pending deliveries are
	// cancelled, the log rows keep their endpoint_id (nothing disappears),
	// the endpoint answers not-found, and its limit slot is freed.
	f.recordEvent(t, ctx, events.Event{
		EventType:  "release.published",
		Visibility: events.VisibilityPublic,
		Payload:    json.RawMessage(`{"release_id":"del"}`),
	})
	f.publishAndFanOut(t, ctx)
	if err := f.store.DeleteEndpoint(ctx, f.alice.ID, e.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var cancelled, nullEndpoint int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'cancelled'),
		       count(*) FILTER (WHERE endpoint_id IS NULL)
		FROM webhook_deliveries`).Scan(&cancelled, &nullEndpoint); err != nil {
		t.Fatalf("count cancelled: %v", err)
	}
	if cancelled != 1 || nullEndpoint != 0 {
		t.Fatalf("cancelled=%d null_endpoint=%d, want the one pending delivery cancelled and every row keeping its endpoint_id",
			cancelled, nullEndpoint)
	}
	if _, err := f.store.GetEndpoint(ctx, f.alice.ID, e.ID); !errors.Is(err, events.ErrWebhookNotFound) {
		t.Fatalf("GetEndpoint after delete = %v, want ErrWebhookNotFound", err)
	}
	// The deleted endpoint no longer counts toward the limit: the slot it
	// held can be taken by a new endpoint.
	if _, err := f.store.CreateEndpoint(ctx, f.alice.ID, "https://example.com/replacement", "s", nil); err != nil {
		t.Fatalf("create after delete (freed slot): %v", err)
	}
}

// TestWebhookFanoutSkipsBrokenRowWithoutStallingBatch: one event whose
// delivery-row write necessarily fails (a CHECK constraint planted to
// simulate a poison row) must not abort the fan-out batch — the other
// events in the SAME pass still fan out and get marked, and the broken
// row stays unmarked (re-claimed, re-failing observably next pass)
// instead of silently vanishing.
func TestWebhookFanoutSkipsBrokenRowWithoutStallingBatch(t *testing.T) {
	ctx := testCtx(t)
	f := newWebhookFixture(t, ctx)

	if _, err := f.store.CreateEndpoint(ctx, f.alice.ID, "https://example.com/hook", "test-secret", nil); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}

	// The poison event (its delivery-row insert will necessarily fail)
	// and the healthy event behind it in the same batch.
	f.recordEvent(t, ctx, events.Event{
		EventType:  "test.poison.fanout",
		Visibility: events.VisibilityPublic,
		Payload:    json.RawMessage(`{}`),
	})
	f.recordEvent(t, ctx, events.Event{
		EventType:  "state.committed",
		Visibility: events.VisibilityPublic,
		Payload:    json.RawMessage(`{"state_id":"after-poison"}`),
	})
	if _, err := f.dispatcher.RunOnce(ctx); err != nil {
		t.Fatalf("dispatcher RunOnce: %v", err)
	}

	var poisonEventID, healthyEventID string
	if err := f.pool.QueryRow(ctx, `
		SELECT id FROM research_events WHERE event_type = 'test.poison.fanout'`).Scan(&poisonEventID); err != nil {
		t.Fatalf("poison event id: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `
		SELECT id FROM research_events WHERE event_type = 'state.committed'`).Scan(&healthyEventID); err != nil {
		t.Fatalf("healthy event id: %v", err)
	}

	// Plant the poison: a CHECK constraint that rejects ONLY the poison
	// event's delivery row. (DDL cannot bind parameters — the uuid is a
	// fixed string in this test's namespace.)
	if _, err := f.pool.Exec(ctx, fmt.Sprintf(`
		ALTER TABLE webhook_deliveries ADD CONSTRAINT t1006_poison_probe
		CHECK (event_id <> '%s')`, poisonEventID)); err != nil {
		t.Fatalf("plant poison constraint: %v", err)
	}
	// The claim order is (created_at, id): age the poison row so it is
	// claimed first and the healthy row sits behind it in the batch.
	if _, err := f.pool.Exec(ctx, `
		UPDATE outbox_events SET created_at = now() - interval '1 minute'
		WHERE id = (SELECT outbox_event_id FROM research_events WHERE id = $1)`, poisonEventID); err != nil {
		t.Fatalf("age poison row: %v", err)
	}

	// One pass: the poison insert fails; the healthy row must still fan
	// out and be marked in the same pass.
	fanned, err := f.fanout.RunOnce(ctx)
	if err != nil {
		t.Fatalf("fanout RunOnce: %v", err)
	}
	if fanned != 1 {
		t.Fatalf("fanned = %d, want 1 (only the healthy row)", fanned)
	}
	rows := f.deliveries(t, ctx)
	if len(rows) != 1 || rows[0].EventType != "state.committed" || rows[0].EventID != healthyEventID {
		t.Fatalf("deliveries after poisoned pass = %+v, want only the healthy delivery", rows)
	}
	var healthyFanned bool
	if err := f.pool.QueryRow(ctx, `
		SELECT webhook_fanned_out_at IS NOT NULL FROM outbox_events
		WHERE id = (SELECT outbox_event_id FROM research_events WHERE id = $1)`, healthyEventID).Scan(&healthyFanned); err != nil {
		t.Fatalf("healthy fanned flag: %v", err)
	}
	if !healthyFanned {
		t.Fatal("healthy row not marked fanned — the poison row stalled the batch")
	}

	// The poison row stays unmarked and is re-claimed: the next pass fails
	// it again (observably, via the logged trace) and still completes.
	fanned, err = f.fanout.RunOnce(ctx)
	if err != nil {
		t.Fatalf("fanout second pass: %v", err)
	}
	if fanned != 0 {
		t.Fatalf("second pass fanned = %d, want 0 (the poison row stays unmarked)", fanned)
	}
	if len(f.deliveries(t, ctx)) != 1 {
		t.Fatal("poison re-claim produced a delivery row")
	}
}

// TestWebhookDeletedEndpointHistoryRemainsReadable: after an endpoint is
// deleted its delivery-log rows keep their endpoint_id and stay readable
// through ListDeliveries — nothing disappears (docs/04 §6); the endpoint
// itself answers not-found on every read and redelivery is blocked.
func TestWebhookDeletedEndpointHistoryRemainsReadable(t *testing.T) {
	ctx := testCtx(t)
	f := newWebhookFixture(t, ctx)

	recv := &verifyingReceiver{status: http.StatusOK}
	srv := httptest.NewServer(http.HandlerFunc(recv.handler))
	defer srv.Close()

	endpoint, err := f.store.CreateEndpoint(ctx, f.alice.ID, srv.URL+"/hook", "test-secret", nil)
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	f.recordEvent(t, ctx, events.Event{
		EventType:  "state.committed",
		Visibility: events.VisibilityPublic,
		Payload:    json.RawMessage(`{"state_id":"history"}`),
	})
	f.publishAndFanOut(t, ctx)
	if _, _, err := f.newDeliverer().RunOnce(ctx); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	if err := f.store.DeleteEndpoint(ctx, f.alice.ID, endpoint.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// The history is still readable, with its endpoint_id intact.
	rows, err := f.store.ListDeliveries(ctx, f.alice.ID, endpoint.ID, nil, "", 50)
	if err != nil {
		t.Fatalf("list deliveries after delete: %v", err)
	}
	if len(rows) != 1 || rows[0].EndpointID != endpoint.ID || rows[0].Status != events.DeliveryDelivered {
		t.Fatalf("delivery log after delete = %+v, want the delivered row with its endpoint_id", rows)
	}

	// The endpoint itself is gone from every read, and redelivery of its
	// history is blocked.
	if _, err := f.store.GetEndpoint(ctx, f.alice.ID, endpoint.ID); !errors.Is(err, events.ErrWebhookNotFound) {
		t.Fatalf("GetEndpoint after delete = %v, want ErrWebhookNotFound", err)
	}
	if list, err := f.store.ListEndpoints(ctx, f.alice.ID); err != nil || len(list) != 0 {
		t.Fatalf("ListEndpoints after delete = %+v (err %v), want empty", list, err)
	}
	if err := f.store.Redeliver(ctx, f.alice.ID, endpoint.ID, rows[0].ID); !errors.Is(err, events.ErrWebhookNotFound) {
		t.Fatalf("Redeliver after delete = %v, want ErrWebhookNotFound", err)
	}
}

// TestWebhookUnclassifiedStatusDoesNotBurnStreak: a status outside
// 2xx/4xx/5xx (a redirect) retries WITHOUT counting toward the disable
// policy, while real failures (a transport error) still do — and the
// transport failure records response_code NULL, never a fake 0. Endpoint
// C's redirect CARRIES a Location: it must come back as the delivery's
// own answer (retry, code 302) and never be followed — the target behind
// it fails the test the moment the signed request reaches it.
func TestWebhookUnclassifiedStatusDoesNotBurnStreak(t *testing.T) {
	ctx := testCtx(t)
	f := newWebhookFixture(t, ctx)

	// Endpoint A answers 302 with no Location; endpoint B targets a closed
	// port.
	redirector := &verifyingReceiver{status: http.StatusFound}
	srv := httptest.NewServer(http.HandlerFunc(redirector.handler))
	defer srv.Close()
	deadLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve dead port: %v", err)
	}
	deadURL := "http://" + deadLn.Addr().String() + "/hook"
	deadLn.Close()

	// Endpoint C answers 302 + Location pointing at this target: nobody
	// may ever contact it — C registered cSrv's URL and nothing else (a
	// followed redirect would also carry the signature headers there).
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("deliverer followed the redirect: the signed request reached a host endpoint C never registered")
		w.WriteHeader(http.StatusOK)
	}))
	defer targetSrv.Close()
	cSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", targetSrv.URL+"/moved")
		w.WriteHeader(http.StatusFound)
	}))
	defer cSrv.Close()

	a, err := f.store.CreateEndpoint(ctx, f.alice.ID, srv.URL+"/hook", "sa", nil)
	if err != nil {
		t.Fatalf("create redirect endpoint: %v", err)
	}
	b, err := f.store.CreateEndpoint(ctx, f.alice.ID, deadURL, "sb", nil)
	if err != nil {
		t.Fatalf("create dead endpoint: %v", err)
	}
	c, err := f.store.CreateEndpoint(ctx, f.alice.ID, cSrv.URL+"/hook", "sc", nil)
	if err != nil {
		t.Fatalf("create location-redirect endpoint: %v", err)
	}
	f.recordEvent(t, ctx, events.Event{
		EventType:  "state.committed",
		Visibility: events.VisibilityPublic,
		Payload:    json.RawMessage(`{"state_id":"unclassified"}`),
	})
	f.publishAndFanOut(t, ctx)

	const limit = 3
	deliverer := f.newDeliverer(events.WithDelivererFailureLimit(limit))
	for i := 1; i <= limit; i++ {
		f.dueNow(t, ctx)
		if _, _, err := deliverer.RunOnce(ctx); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}

	// A: three 302s — retries scheduled, streak untouched, still enabled.
	ea, err := f.store.GetEndpoint(ctx, f.alice.ID, a.ID)
	if err != nil {
		t.Fatalf("get redirect endpoint: %v", err)
	}
	if !ea.Enabled || ea.ConsecutiveFailures != 0 {
		t.Fatalf("redirect endpoint = %+v, want enabled with streak 0", ea)
	}
	// B: three transport failures — streak burned, disabled at the limit.
	eb, err := f.store.GetEndpoint(ctx, f.alice.ID, b.ID)
	if err != nil {
		t.Fatalf("get dead endpoint: %v", err)
	}
	if eb.Enabled || eb.ConsecutiveFailures < limit {
		t.Fatalf("dead endpoint = %+v, want disabled with streak >= %d", eb, limit)
	}
	// C: three 302+Location answers — the redirect is the delivery's own
	// answer: retries scheduled, streak untouched, still enabled.
	ec, err := f.store.GetEndpoint(ctx, f.alice.ID, c.ID)
	if err != nil {
		t.Fatalf("get location-redirect endpoint: %v", err)
	}
	if !ec.Enabled || ec.ConsecutiveFailures != 0 {
		t.Fatalf("location-redirect endpoint = %+v, want enabled with streak 0", ec)
	}

	// The delivery rows tell the same story: A keeps its real code (302)
	// and C too — never the 200 that sits behind C's Location; B's
	// response_code is NULL — no response, not a fake 0.
	rows := f.deliveries(t, ctx)
	var rowA, rowB, rowC *events.Delivery
	for i := range rows {
		switch rows[i].EndpointID {
		case a.ID:
			rowA = &rows[i]
		case b.ID:
			rowB = &rows[i]
		case c.ID:
			rowC = &rows[i]
		}
	}
	if rowA == nil || rowA.Status != events.DeliveryPending || rowA.Attempts != limit ||
		rowA.ResponseCode == nil || *rowA.ResponseCode != http.StatusFound {
		t.Fatalf("redirect delivery row = %+v, want pending with %d attempts and code 302", rowA, limit)
	}
	if rowC == nil || rowC.Status != events.DeliveryPending || rowC.Attempts != limit ||
		rowC.ResponseCode == nil || *rowC.ResponseCode != http.StatusFound {
		t.Fatalf("location-redirect delivery row = %+v, want pending with %d attempts and code 302 (not the 200 behind the Location)", rowC, limit)
	}
	if rowB == nil || rowB.Status != events.DeliveryPending || rowB.ResponseCode != nil || rowB.LastError == nil {
		t.Fatalf("dead-port delivery row = %+v, want pending with response_code NULL and last_error set", rowB)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
