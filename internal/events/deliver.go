package events

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Deliverer turns fanned-out delivery rows into signed HTTP requests and
// drives the delivery log (T1006, docs/18 §4 / docs/22 §9): at-least-once
// delivery, exponential-backoff retries, and the disable-failing-endpoint
// policy. It never loses a delivery — a crash mid-request leaves the row
// pending under its lease, and the next pass (or the next worker) retries
// it; a duplicate a consumer sees carries the SAME X-POST-Delivery id, so
// an idempotent consumer dedupes on it.
//
// Delivery classification:
//
//   - 2xx                delivered (terminal, counter resets)
//   - 4xx                failed (terminal — retrying a refused request
//     cannot help; it still counts toward the disable policy)
//   - 5xx / network err  retry with backoff (1s, 5s, 25s, 2m, 10m, 30m,
//     then 1h), counted toward the disable policy
//   - other statuses (1xx/3xx, anything outside 2xx/4xx/5xx) retry with
//     the same backoff but do NOT burn the consecutive-failure streak:
//     an informational status or a redirect is not evidence the endpoint
//     is failing
//   - DefaultConsecutiveFailureLimit consecutive failures disable the
//     endpoint (the store emits webhook.delivery_failed)
//
// Every request is signed (Sign) with the endpoint's live secret and a
// fresh timestamp; the destination is the fan-out-time URL snapshot, so
// the delivery log always names where the request actually went.
type Deliverer struct {
	pool         *pgxpool.Pool
	store        *WebhookStore
	log          *slog.Logger
	client       *http.Client
	batchSize    int
	failureLimit int
	// dialContext is the connection dialer; the default refuses private /
	// loopback / link-local addresses (SSRF guard). Tests replace it to
	// reach their httptest servers.
	dialContext func(ctx context.Context, network, addr string) (net.Conn, error)
}

// DefaultDeliveryBatchSize is how many deliveries one RunOnce attempts.
const DefaultDeliveryBatchSize = 100

// DefaultDeliveryPollInterval is the pause between RunOnce passes in Run
// (mirrors DefaultPollInterval — low-latency but not a hot loop).
const DefaultDeliveryPollInterval = time.Second

// deliveryTimeout bounds ONE HTTP attempt. The lease (deliveryLease) must
// comfortably exceed it.
const deliveryTimeout = 10 * time.Second

// DelivererOption tunes a Deliverer.
type DelivererOption func(*Deliverer)

// WithDelivererLogger sets the logger (default slog.Default()).
func WithDelivererLogger(log *slog.Logger) DelivererOption {
	return func(d *Deliverer) { d.log = log }
}

// WithDelivererBatchSize sets the deliveries one RunOnce attempts
// (default DefaultDeliveryBatchSize).
func WithDelivererBatchSize(n int) DelivererOption {
	return func(d *Deliverer) { d.batchSize = n }
}

// WithDelivererFailureLimit sets the consecutive-failure disable
// threshold (default DefaultConsecutiveFailureLimit).
func WithDelivererFailureLimit(n int) DelivererOption {
	return func(d *Deliverer) { d.failureLimit = n }
}

// WithDelivererDialContext replaces the connection dialer. The default is
// the SSRF guard; tests use a plain net.Dialer to reach httptest servers.
// Production callers should not replace it.
func WithDelivererDialContext(dial func(ctx context.Context, network, addr string) (net.Conn, error)) DelivererOption {
	return func(d *Deliverer) { d.dialContext = dial }
}

// NewDeliverer builds the deliverer on pool.
func NewDeliverer(pool *pgxpool.Pool, opts ...DelivererOption) *Deliverer {
	d := &Deliverer{
		pool:         pool,
		store:        NewWebhookStore(pool),
		log:          slog.Default(),
		batchSize:    DefaultDeliveryBatchSize,
		failureLimit: DefaultConsecutiveFailureLimit,
		dialContext:  GuardedDialContext,
	}
	for _, opt := range opts {
		opt(d)
	}
	d.client = &http.Client{
		Timeout: deliveryTimeout,
		// Redirects are never followed: a 3xx is the receiver's answer to
		// the request that was actually signed, so it flows into the
		// classification table as outcomeRetryNoStreak. Following one would
		// let another host's 200 be recorded as "delivered" and carry the
		// signature headers to a destination the endpoint never registered.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			DialContext:           d.dialContext,
			MaxIdleConns:          16,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: deliveryTimeout,
		},
	}
	return d
}

// DeliveryAttempt is one claimed delivery row as the HTTP step needs it.
type DeliveryAttempt struct {
	DeliveryID    string
	EndpointID    string
	EndpointURL   string
	Secret        string
	Attempts      int
	EventID       string
	EventType     string
	OccurredAt    time.Time
	ActorID       *string
	ProjectID     *string
	CorrelationID string
	Visibility    string
	Payload       []byte
}

// Run attempts the due backlog until ctx is cancelled: one pass
// immediately, then one per DefaultDeliveryPollInterval. Transient
// failures never end the loop — a down database is retried like every
// other dependency outage; only ctx cancellation ends it.
func (d *Deliverer) Run(ctx context.Context) error {
	ticker := time.NewTicker(DefaultDeliveryPollInterval)
	defer ticker.Stop()
	for {
		if _, _, err := d.RunOnce(ctx); err != nil && ctx.Err() == nil {
			d.log.Error("events: webhook delivery pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunOnce attempts one batch of due deliveries and returns (delivered,
// failed) counts. Each attempt is independent: one broken delivery
// records its failure and moves on — a poison row must not stall the
// backlog (the T1001 lesson).
func (d *Deliverer) RunOnce(ctx context.Context) (int, int, error) {
	attempts, err := d.store.ClaimDueAttempts(ctx, d.batchSize)
	if err != nil {
		return 0, 0, err
	}
	delivered, failed := 0, 0
	for _, a := range attempts {
		log := d.log.With("delivery_id", a.DeliveryID, "endpoint_id", a.EndpointID,
			"event_id", a.EventID, "event_type", a.EventType, "attempt", a.Attempts+1)
		outcome, code, msg := d.try(ctx, a)
		switch outcome {
		case outcomeDelivered:
			if err := d.store.RecordDelivered(ctx, a.DeliveryID, *code); err != nil {
				log.Error("events: recording delivered delivery failed; row stays leased for retry", "error", err)
				continue
			}
			delivered++
			log.Info("events: webhook delivery delivered", "response_code", *code)
		case outcomeTerminal:
			disabled, err := d.store.RecordDeliveryFailure(ctx, a.DeliveryID, code, msg, nil, d.failureLimit)
			if err != nil {
				log.Error("events: recording failed delivery failed; row stays leased for retry", "error", err)
				continue
			}
			failed++
			log.Error("events: webhook delivery permanently failed", "response_code", code, "reason", msg,
				"endpoint_disabled", disabled)
		case outcomeRetry:
			retryAt := time.Now().Add(backoffDelay(a.Attempts + 1))
			disabled, err := d.store.RecordDeliveryFailure(ctx, a.DeliveryID, code, msg, &retryAt, d.failureLimit)
			if err != nil {
				log.Error("events: recording failed delivery failed; row stays leased for retry", "error", err)
				continue
			}
			failed++
			log.Warn("events: webhook delivery failed; retry scheduled", "response_code", code,
				"reason", msg, "retry_at", retryAt, "endpoint_disabled", disabled)
		case outcomeRetryNoStreak:
			retryAt := time.Now().Add(backoffDelay(a.Attempts + 1))
			if err := d.store.RecordDeliveryRetryable(ctx, a.DeliveryID, code, msg, retryAt); err != nil {
				log.Error("events: recording unclassified delivery failed; row stays leased for retry", "error", err)
				continue
			}
			failed++
			log.Warn("events: webhook delivery unclassified status; retry scheduled without burning the streak",
				"response_code", code, "reason", msg, "retry_at", retryAt)
		}
	}
	return delivered, failed, nil
}

// attempt outcomes of one HTTP try.
const (
	outcomeDelivered = iota
	outcomeTerminal
	outcomeRetry
	outcomeRetryNoStreak
)

// try posts one signed delivery and classifies the outcome. It never
// returns an error — every transport problem is a retryable outcome, and
// the store write is what can fail (handled by the caller). code is the
// HTTP response code, or nil when no response ever arrived (transport
// error, unbuildable body): the log column stays NULL so "no response"
// is never confused with a real HTTP status.
func (d *Deliverer) try(ctx context.Context, a DeliveryAttempt) (outcome int, code *int, msg string) {
	body, err := webhookBody(a)
	if err != nil {
		// An unbuildable envelope is a programming error; treat it as a
		// terminal failure so the row surfaces in the log instead of
		// retrying forever.
		return outcomeTerminal, nil, fmt.Sprintf("building delivery body: %v", err)
	}
	timestamp := time.Now().Unix()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.EndpointURL, bytes.NewReader(body))
	if err != nil {
		return outcomeTerminal, nil, fmt.Sprintf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(EventHeader, a.EventType)
	req.Header.Set(EventIDHeader, a.EventID)
	req.Header.Set(DeliveryIDHeader, a.DeliveryID)
	req.Header.Set(TimestampHeader, strconv.FormatInt(timestamp, 10))
	req.Header.Set(SignatureHeader, Sign([]byte(a.Secret), timestamp, body))

	resp, err := d.client.Do(req)
	if err != nil {
		return outcomeRetry, nil, truncateError(err.Error(), maxErrorLen)
	}
	defer resp.Body.Close()
	code = &resp.StatusCode
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return outcomeDelivered, code, ""
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return outcomeTerminal, code, fmt.Sprintf("http status %d", resp.StatusCode)
	}
	if resp.StatusCode >= 500 {
		return outcomeRetry, code, fmt.Sprintf("http status %d", resp.StatusCode)
	}
	// 1xx/3xx — or any status outside the classified bands — gets a retry,
	// but without evidence against the endpoint: the streak is untouched.
	return outcomeRetryNoStreak, code, fmt.Sprintf("http status %d", resp.StatusCode)
}

// webhookBody builds the canonical delivery envelope (specs/events/
// event-types.yaml required_envelope_fields): the research event's
// envelope columns plus its payload object, which already carries
// payload_version (T1001's injection).
func webhookBody(a DeliveryAttempt) ([]byte, error) {
	payload := json.RawMessage(a.Payload)
	if len(payload) == 0 || string(payload) == "null" {
		payload = json.RawMessage(`{}`)
	}
	out, err := json.Marshal(struct {
		EventID       string          `json:"event_id"`
		EventType     string          `json:"event_type"`
		OccurredAt    time.Time       `json:"occurred_at"`
		ActorID       *string         `json:"actor_id"`
		ProjectID     *string         `json:"project_id"`
		CorrelationID string          `json:"correlation_id"`
		Visibility    string          `json:"visibility"`
		Payload       json.RawMessage `json:"payload"`
	}{
		EventID:       a.EventID,
		EventType:     a.EventType,
		OccurredAt:    a.OccurredAt,
		ActorID:       a.ActorID,
		ProjectID:     a.ProjectID,
		CorrelationID: a.CorrelationID,
		Visibility:    a.Visibility,
		Payload:       payload,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding envelope: %w", err)
	}
	return out, nil
}

// backoffDelay maps the just-failed attempt number (1 = first failure) to
// the wait before the next try: 1s, 5s, 25s, 2m, 10m, 30m, then 1h
// capped. With the default failure limit of 10, an endpoint gets roughly
// five hours of chances before the disable policy trips.
func backoffDelay(attempts int) time.Duration {
	switch attempts {
	case 1:
		return time.Second
	case 2:
		return 5 * time.Second
	case 3:
		return 25 * time.Second
	case 4:
		return 2 * time.Minute
	case 5:
		return 10 * time.Minute
	case 6:
		return 30 * time.Minute
	default:
		return time.Hour
	}
}

// GuardedDialContext is the SSRF guard every production delivery dials
// through: it resolves the target host itself and refuses to connect to
// loopback, private, link-local, unspecified, multicast or IANA
// special-purpose (reserved) addresses. Resolving at dial time (not
// registration time) is what closes the DNS-rebinding window — the address
// actually used is the one checked. Tests replace this with a plain dialer
// to reach httptest servers; the guard itself is exercised by the unit
// tests.
func GuardedDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("events: webhook dial: bad address %q: %w", addr, err)
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("events: webhook dial: resolve %q: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("events: webhook dial: %q resolved to no addresses", host)
	}
	for _, ip := range ips {
		if blockedIP(ip.IP) {
			return nil, fmt.Errorf("events: webhook dial: refusing to connect to non-public address %s", ip.IP)
		}
	}
	// Dial the first address directly: the checked addresses are the ones
	// used, never the resolver's second opinion.
	return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
}

// blockedIP reports whether ip must never be dialed by the deliverer.
func blockedIP(ip net.IP) bool {
	v4 := ip.To4()
	if v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() ||
		ip.IsInterfaceLocalMulticast() || ip.Equal(net.IPv4bcast) {
		return true
	}
	for _, r := range reservedV4 {
		if r.Contains(ip) {
			return true
		}
	}
	return false
}

// reservedV4 are the IANA special-purpose IPv4 ranges Go's IsPrivate does
// not cover but the deliverer must still refuse: CGNAT, the IETF protocol
// assignments, the benchmarking ranges and the whole class E space
// (240.0.0.0/4 — which includes the broadcast address, so the explicit
// IPv4bcast check above is belt-and-braces).
var reservedV4 = []*net.IPNet{
	mustCIDR("100.64.0.0/10"), // RFC 6598 shared address space (CGNAT)
	mustCIDR("192.0.0.0/24"),  // RFC 6890 IETF protocol assignments
	mustCIDR("198.18.0.0/15"), // RFC 2544 benchmarking
	mustCIDR("240.0.0.0/4"),   // RFC 1112 class E / future use
}

// mustCIDR parses a CIDR literal; the literals above are compile-time
// constants, so a panic here is a programming error.
func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(fmt.Sprintf("events: bad reserved CIDR %q: %v", s, err))
	}
	return n
}
