package events

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// The pure delivery pieces (T1006 unit surface): the retry schedule, the
// canonical envelope, the SSRF dial guard's classification, webhook URL
// validation, secret generation and the HTTP outcome classification. The
// database-backed pipeline lives in the integration suite
// (tests/integration/webhook_test.go), where a real receiver answers
// real requests.

func TestBackoffDelaySchedule(t *testing.T) {
	want := []struct {
		attempts int
		delay    time.Duration
	}{
		{1, time.Second},
		{2, 5 * time.Second},
		{3, 25 * time.Second},
		{4, 2 * time.Minute},
		{5, 10 * time.Minute},
		{6, 30 * time.Minute},
		{7, time.Hour},
		{10, time.Hour},
	}
	for _, w := range want {
		if got := backoffDelay(w.attempts); got != w.delay {
			t.Errorf("backoffDelay(%d) = %v, want %v", w.attempts, got, w.delay)
		}
	}
}

func TestWebhookBodyEnvelope(t *testing.T) {
	occurred := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	actor := "actor-1"
	a := DeliveryAttempt{
		DeliveryID:    "delivery-1",
		EndpointID:    "endpoint-1",
		EndpointURL:   "https://example.com/hook",
		Secret:        "secret",
		Attempts:      0,
		EventID:       "event-1",
		EventType:     "state.committed",
		OccurredAt:    occurred,
		ActorID:       &actor,
		CorrelationID: "corr-1",
		Visibility:    VisibilityPublic,
		Payload:       []byte(`{"state_id":"s1","payload_version":"1"}`),
	}
	body, err := webhookBody(a)
	if err != nil {
		t.Fatalf("webhookBody: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("envelope is not JSON: %v", err)
	}
	for _, field := range []string{"event_id", "event_type", "occurred_at", "actor_id", "correlation_id", "visibility", "payload"} {
		if _, ok := env[field]; !ok {
			t.Errorf("envelope lacks required field %q: %s", field, body)
		}
	}
	if env["event_id"] != "event-1" || env["event_type"] != "state.committed" {
		t.Errorf("envelope identity mismatch: %s", body)
	}
	payload, ok := env["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload is not an object: %s", body)
	}
	if payload["state_id"] != "s1" || payload["payload_version"] != "1" {
		t.Errorf("payload not embedded verbatim: %s", body)
	}
}

func TestWebhookBodyNullPayloadBecomesEmptyObject(t *testing.T) {
	body, err := webhookBody(DeliveryAttempt{Payload: nil})
	if err != nil {
		t.Fatalf("webhookBody: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("envelope is not JSON: %v", err)
	}
	if _, ok := env["payload"].(map[string]any); !ok {
		t.Fatalf("nil payload must render as {}: %s", body)
	}
}

func TestBlockedIPClassification(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.8.8.8", // loopback
		"10.0.0.1", "172.16.0.1", "192.168.1.1", // RFC1918
		"169.254.169.254",              // link-local (cloud metadata)
		"0.0.0.0",                      // unspecified
		"224.0.0.1", "239.255.255.250", // multicast
		"255.255.255.255", // broadcast
		// IANA special-purpose ranges beyond Go's IsPrivate:
		"100.64.0.1", "100.100.100.100", "100.127.255.254", // CGNAT 100.64.0.0/10
		"192.0.0.1", "192.0.0.9", // IETF protocol assignments 192.0.0.0/24
		"198.18.0.1", "198.19.255.254", // benchmarking 198.18.0.0/15
		"240.0.0.1", "255.255.255.254", // class E 240.0.0.0/4
		"::1",                     // IPv6 loopback
		"fe80::1",                 // IPv6 link-local
		"fc00::1", "fd12:3456::1", // IPv6 ULA
		"::ffff:127.0.0.1",       // IPv4-mapped loopback
		"::ffff:169.254.169.254", // IPv4-mapped link-local
		"::ffff:100.64.0.1",      // IPv4-mapped CGNAT
	}
	for _, ip := range blocked {
		if !blockedIP(net.ParseIP(ip)) {
			t.Errorf("blockedIP(%q) = false, want true", ip)
		}
	}
	public := []string{
		"93.184.216.34", "1.1.1.1", "2606:4700:4700::1111",
		// The public neighbours of the reserved ranges above:
		"100.0.0.1", "100.128.0.1", // just outside CGNAT
		"192.0.1.1",                // just above 192.0.0.0/24
		"198.17.0.1", "198.20.0.1", // just outside 198.18.0.0/15
		"223.255.255.254", // public space below the class E boundary
	}
	for _, ip := range public {
		if blockedIP(net.ParseIP(ip)) {
			t.Errorf("blockedIP(%q) = true, want false", ip)
		}
	}
}

// TestDeliveryTryClassification is the outcome table of one HTTP try:
// 2xx delivered, 4xx terminal, 5xx retry, anything outside those bands
// (a 3xx — with or without a Location) retry WITHOUT burning the streak:
// the redirect comes back as the attempt's own answer and is never
// followed — and a transport failure (or an unbuildable request) records
// NO response code, never a fake 0.
func TestDeliveryTryClassification(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d := NewDeliverer(nil, WithDelivererDialContext((&net.Dialer{}).DialContext))

	statusServer := func(status int) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	deadLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve dead port: %v", err)
	}
	deadURL := "http://" + deadLn.Addr().String() + "/hook"
	deadLn.Close()

	// A 3xx that CARRIES a Location: the deliverer must return the
	// redirect itself, never follow it. Following would credit another
	// host's 200 as "delivered" and carry the signature headers to a
	// destination the endpoint never registered.
	var followed atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		followed.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL+"/moved")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	code := func(c int) *int { return &c }
	attempt := func(url string) DeliveryAttempt {
		return DeliveryAttempt{
			DeliveryID:  "d1",
			EndpointID:  "e1",
			EndpointURL: url,
			Secret:      "secret",
			EventID:     "ev1",
			EventType:   "state.committed",
			Payload:     []byte(`{}`),
		}
	}

	cases := []struct {
		name    string
		a       DeliveryAttempt
		outcome int
		code    *int
	}{
		{"2xx delivered", attempt(statusServer(http.StatusOK)), outcomeDelivered, code(200)},
		{"4xx terminal", attempt(statusServer(http.StatusBadRequest)), outcomeTerminal, code(400)},
		{"5xx retry", attempt(statusServer(http.StatusInternalServerError)), outcomeRetry, code(500)},
		{"3xx retry without streak", attempt(statusServer(http.StatusFound)), outcomeRetryNoStreak, code(302)},
		{"3xx with Location retry without streak", attempt(redirector.URL + "/hook"), outcomeRetryNoStreak, code(302)},
		{"transport error: no code", attempt(deadURL), outcomeRetry, nil},
		{"unbuildable request terminal: no code", attempt("http://example.com/\x00"), outcomeTerminal, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outcome, gotCode, msg := d.try(ctx, tc.a)
			if outcome != tc.outcome {
				t.Fatalf("try() outcome = %d, want %d (msg %q)", outcome, tc.outcome, msg)
			}
			if (gotCode == nil) != (tc.code == nil) || (gotCode != nil && *gotCode != *tc.code) {
				t.Fatalf("try() code = %v, want %v", gotCode, tc.code)
			}
		})
	}
	if followed.Load() {
		t.Error("the deliverer followed the redirect: the signed request reached a host the endpoint never registered")
	}
}

func TestGuardedDialRefusesLoopback(t *testing.T) {
	// The guard must refuse BEFORE any connection is attempted — the
	// address is classified from the resolution, and 127.0.0.1 is always
	// loopback regardless of what listens there.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := GuardedDialContext(ctx, "tcp", "127.0.0.1:80")
	if err == nil {
		conn.Close()
		t.Fatal("GuardedDialContext to loopback succeeded, want refusal")
	}
	if conn != nil {
		t.Fatal("refused dial returned a connection")
	}
}

func TestValidateWebhookURL(t *testing.T) {
	valid := []string{
		"https://example.com/hook",
		"http://example.com:8080/path?query=1",
		"https://hooks.example.org/",
	}
	for _, u := range valid {
		if err := ValidateWebhookURL(u); err != nil {
			t.Errorf("ValidateWebhookURL(%q) = %v, want nil", u, err)
		}
	}
	invalid := []string{
		"",
		"not a url",
		"ftp://example.com/hook",
		"https://",                      // no host
		"https://user:pass@example.com", // embedded credentials leak into the delivery log
		"/relative/path",
		"https://" + string(make([]byte, 3000)), // over length (host part)
	}
	for _, u := range invalid {
		if err := ValidateWebhookURL(u); err == nil {
			t.Errorf("ValidateWebhookURL(%q) = nil, want error", u)
		}
	}
}

func TestValidateEventFilters(t *testing.T) {
	if err := ValidateEventFilters(nil); err != nil {
		t.Errorf("ValidateEventFilters(nil) = %v, want nil (empty means all events)", err)
	}
	if err := ValidateEventFilters([]string{"state.committed", "pull_request.opened"}); err != nil {
		t.Errorf("ValidateEventFilters(valid) = %v, want nil", err)
	}
	invalid := [][]string{
		{""},
		{"State.Committed"},                // uppercase
		{"state committed"},                // space
		{"state;committed"},                // charset
		{"x." + string(make([]byte, 120))}, // over length
	}
	for _, f := range invalid {
		if err := ValidateEventFilters(f); err == nil {
			t.Errorf("ValidateEventFilters(%q) = nil, want error", f)
		}
	}
}

func TestGenerateSecretShape(t *testing.T) {
	a, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	b, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	if len(a) != 2*DefaultSecretBytes {
		t.Errorf("secret length = %d, want %d hex chars", len(a), 2*DefaultSecretBytes)
	}
	if a == b {
		t.Error("two generated secrets are identical")
	}
	for _, c := range a {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("secret %q is not lowercase hex", a)
		}
	}
}
