package observability

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// seriesLines returns the exposition lines whose METRIC NAME is exactly name.
// Substring matching is not good enough here: post_metrics_collector_errors_total
// carries a collector= label, so a failed read of post_probe_down puts the
// string post_probe_down into the exposition without putting a post_probe_down
// SAMPLE into it. The two are different facts and two different tests.
func seriesLines(body, name string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line == name || strings.HasPrefix(line, name+"{") || strings.HasPrefix(line, name+" ") {
			out = append(out, line)
		}
	}
	return out
}

// seriesValue reads the value of a family that is expected to have exactly one
// series. Values are parsed rather than string-matched so an assertion cannot
// be satisfied by a different number that happens to be a prefix.
func seriesValue(t *testing.T, body, name string) float64 {
	t.Helper()
	lines := seriesLines(body, name)
	if len(lines) != 1 {
		t.Fatalf("%s has %d series, want 1:\n%s", name, len(lines), body)
	}
	i := strings.LastIndexByte(lines[0], ' ')
	v, err := strconv.ParseFloat(strings.TrimSpace(lines[0][i+1:]), 64)
	if err != nil {
		t.Fatalf("parsing %q: %v", lines[0], err)
	}
	return v
}

// scrape renders the exposition exactly as the endpoint serves it, rather
// than through a hand-rolled formatter. A formatter written here could agree
// with a wrong handler; going through the real one means the assertions are
// about the bytes a scraper receives.
func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

// The endpoint's content is the deliverable, so what it exposes has to be
// the declared set and nothing else. A dedicated registry is what makes that
// true; this test is what keeps it true — if a collector ever registers on
// prometheus.DefaultRegisterer, or the runtime collectors get wired in by
// accident, the exposition grows families nobody enumerated here.
func TestExpositionCarriesOnlyDeclaredFamilies(t *testing.T) {
	m := NewMetrics()
	// Touch every producer so nothing is missing merely because a vector is
	// not materialised until its first observation.
	m.ObserveHTTP(http.MethodGet, "/healthz", http.StatusOK, 0.001)
	m.ObservePermissionDenial("api", DecisionForbidden)
	m.ObserveQueueError("read")
	m.ObserveQueueJob("completed")
	m.ObserveOutboxPublishFailure()
	m.ObserveWebhookDelivery("delivered")
	m.ObserveSearchQuery("ok", 0.01)
	m.ObservePlannerPlanned()
	m.ObservePlannerFallback("no_model")
	m.ObserveReconciliation(ReconcileOutcomeOK, 1, 0, 0, 0)
	m.CollectorError("probe")

	body := scrape(t, m)
	for _, want := range []string{
		"post_http_requests_total", "post_http_request_duration_seconds",
		"post_permission_denials_total", "post_queue_errors_total",
		"post_queue_jobs_total", "post_outbox_publish_failures_total",
		"post_webhook_deliveries_total", "post_search_query_duration_seconds",
		"post_planner_runs_total", "post_planner_fallbacks_total",
		"post_rsg_reconciliation_passes_total",
		"post_rsg_reconciliation_open_findings",
		"post_metrics_collector_errors_total",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition is missing %s after its producer ran:\n%s", want, body)
		}
	}
	// The Go runtime's own collectors are exactly the noise a dedicated
	// registry is chosen to avoid; post_ is the platform's namespace, so
	// anything without it is not ours.
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name := line
		if i := strings.IndexAny(name, "{ "); i >= 0 {
			name = name[:i]
		}
		if !strings.HasPrefix(name, "post_") {
			t.Errorf("exposition carries a family outside the post_ namespace: %q", line)
		}
	}
}

// The permission family's control: a refusal is counted, an ordinary
// response is not. Without the second half, a counter that increments on
// every request would pass — and would report an "elevated denial rate" for
// a platform with no denials at all.
func TestObserveRefusalCountsRefusalsAndNothingElse(t *testing.T) {
	m := NewMetrics()
	for _, status := range []int{
		http.StatusForbidden, http.StatusUnauthorized,
		http.StatusOK, http.StatusCreated, http.StatusNoContent,
		http.StatusNotFound, // this platform ALSO uses 404 as a refusal, and
		// ObserveRefusal's own doc says it cannot tell that apart from a
		// genuine miss. If a future change starts counting 404s, this case
		// is where the decision has to be made deliberately.
		http.StatusInternalServerError,
	} {
		m.ObserveRefusal("api", status)
	}

	body := scrape(t, m)
	if !strings.Contains(body, `post_permission_denials_total{decision="forbidden",surface="api"} 1`) {
		t.Errorf("one 403 did not produce exactly one forbidden denial:\n%s", body)
	}
	if !strings.Contains(body, `post_permission_denials_total{decision="unauthenticated",surface="api"} 1`) {
		t.Errorf("one 401 did not produce exactly one unauthenticated denial:\n%s", body)
	}
	if got := strings.Count(body, "post_permission_denials_total{"); got != 2 {
		t.Errorf("the family has %d series, want 2 (200/201/204/404/500 must not be counted):\n%s", got, body)
	}
}

// The route label has to be the ServeMux pattern, not the path. A path label
// would be unbounded cardinality AND would publish which ids exist; both are
// reasons the middleware reads http.Request.Pattern instead. This drives two
// different project ids through a real mux and asserts one series.
func TestHTTPMetricsCarryTheRoutePatternNotThePath(t *testing.T) {
	m := NewMetrics()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects/{projectId}", func(w http.ResponseWriter, r *http.Request) {
		m.ObserveHTTP(r.Method, resolveRoute("", r), http.StatusOK, 0.01)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, id := range []string{"prj_alpha", "prj_beta"} {
		resp, err := http.Get(srv.URL + "/api/v1/projects/" + id)
		if err != nil {
			t.Fatalf("GET %s: %v", id, err)
		}
		resp.Body.Close()
	}

	body := scrape(t, m)
	if !strings.Contains(body, `route="/api/v1/projects/{projectId}"`) {
		t.Errorf("route label is not the matching pattern:\n%s", body)
	}
	if got := strings.Count(body, "post_http_requests_total{"); got != 1 {
		t.Errorf("two ids produced %d series, want 1 — the label is not cardinality-bounded:\n%s", got, body)
	}
	for _, id := range []string{"prj_alpha", "prj_beta"} {
		if strings.Contains(body, id) {
			t.Errorf("the exposition publishes the requested id %q:\n%s", id, body)
		}
	}
}

// familyLabelValues reads the set of values a label takes across every series
// of family, out of the exposition text rather than out of the registry.
// Histograms expose name_bucket/name_sum/name_count, so a family is matched at
// a separator boundary rather than by equality.
func familyLabelValues(body, family, label string) map[string]bool {
	key := label + `="`
	values := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name := line
		if i := strings.IndexAny(name, "{ "); i >= 0 {
			name = name[:i]
		}
		if name != family && !strings.HasPrefix(name, family+"_") {
			continue
		}
		i := strings.Index(line, key)
		if i < 0 {
			continue
		}
		rest := line[i+len(key):]
		if j := strings.IndexByte(rest, '"'); j >= 0 {
			values[rest[:j]] = true
		}
	}
	return values
}

// httpRequestsSum adds up post_http_requests_total. The middleware records a
// request AFTER its handler returns, so a scrape taken the moment the response
// arrives can miss the newest one; the tests below wait on this sum reaching
// the number of requests they sent. That wait is independent of how the method
// label is spelled — the counter increments either way — so it cannot turn a
// label defect into a timeout.
func httpRequestsSum(t *testing.T, m *Metrics) float64 {
	t.Helper()
	total := 0.0
	for _, line := range seriesLines(scrape(t, m), "post_http_requests_total") {
		i := strings.LastIndexByte(line, ' ')
		v, err := strconv.ParseFloat(strings.TrimSpace(line[i+1:]), 64)
		if err != nil {
			t.Fatalf("parsing %q: %v", line, err)
		}
		total += v
	}
	return total
}

func waitForHTTPRequests(t *testing.T, m *Metrics, want float64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := httpRequestsSum(t, m); got >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited 5s for post_http_requests_total to reach %v; it stands at %v",
				want, httpRequestsSum(t, m))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// The method label is the one label whose value arrives from the wire, so it is
// the one that has to be folded. http.Request.Method is the raw request-line
// token and RFC 9110 §9.1 makes any valid token a legal method: an anonymous
// client sending `FOO /x HTTP/1.1` with a fresh token per request reaches the
// middleware with Method="FOO" — no route, no session, no handler of its own
// required — and an unfolded label would give it one new series per request in
// BOTH HTTP families, forever. That is the defect this test exists to catch.
//
// It drives the real path over a real socket: client → server → Middleware →
// Default().ObserveHTTP, which is why the exposition read here is Default()'s
// rather than a private registry's. The three phases are the three claims: a
// standard verb is still spelled out (the label was not deleted to bound it),
// one non-standard verb lands in ONE bucket and produces no series of its own,
// and thirty more distinct never-seen verbs do not move the set at all.
func TestHTTPMethodLabelIsAClosedSet(t *testing.T) {
	m := Default()

	served := &atomic.Int64{}
	srv := httptest.NewServer(
		Middleware(slog.New(slog.NewTextHandler(io.Discard, nil)))(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				served.Add(1)
				w.WriteHeader(http.StatusOK)
			})))
	defer srv.Close()

	// send drives one request with a client-chosen method. http.Transport
	// writes req.Method verbatim into the request line, so this is the same
	// `VERB /anything HTTP/1.1` a hand-written socket would produce; the 200
	// assertion is what proves the server accepted it and the request really
	// reached the middleware (a request the server refused with 400 never
	// reaches it, and would make this test measure nothing).
	send := func(method string) {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+"/anything", nil)
		if err != nil {
			t.Fatalf("building a %s request: %v", method, err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s request: %v", method, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s /anything = %d, want 200", method, resp.StatusCode)
		}
	}
	methods := func(body, family string) map[string]bool {
		return familyLabelValues(body, family, "method")
	}
	const requests = "post_http_requests_total"
	const duration = "post_http_request_duration_seconds"

	base := httpRequestsSum(t, m)

	// Phase 1 — an ordinary verb. The label still names it, so "no growth" is
	// not achieved by not having a method label at all.
	send(http.MethodGet)
	waitForHTTPRequests(t, m, base+1)
	body := scrape(t, m)
	standard := methods(body, requests)
	if !standard[http.MethodGet] {
		t.Fatalf("after one GET the method values are %v, want GET among them — the label is gone, which bounds the set the other way and is not this fix:\n%s",
			standard, body)
	}
	if len(standard) != 1 {
		t.Fatalf("after one GET the method values are %v, want exactly {GET}:\n%s", standard, body)
	}

	// Phase 2 — one non-standard verb: at most one new value (the single fold
	// bucket), and the verb itself must not be a label value.
	send("FOO")
	waitForHTTPRequests(t, m, base+2)
	body = scrape(t, m)
	afterOne := methods(body, requests)
	if afterOne["FOO"] {
		t.Errorf("the client-chosen verb FOO became a label value (method values: %v):\n%s", afterOne, body)
	}
	if got := len(afterOne); got > 2 {
		t.Errorf("one non-standard verb produced %d method values, want at most 2 (the standard verbs + one bucket): %v\n%s",
			got, afterOne, body)
	}
	if !afterOne[httpMethodOther] {
		t.Errorf("the non-standard verb is counted as %v; it must be counted in the %q bucket — a method the metric silently drops is a request the metric lies about:\n%s",
			afterOne, httpMethodOther, body)
	}
	// The histogram carries the same label and must fold the same way.
	if got := methods(body, duration); got["FOO"] {
		t.Errorf("post_http_request_duration_seconds carries method=%q:\n%s", "FOO", body)
	}

	// Phase 3 — thirty more, each distinct and never sent before. The set must
	// not move AT ALL: this is the unbounded-growth property, asserted as
	// equality against the previous snapshot rather than as a count.
	sent := []string{"FOO"}
	for i := range 30 {
		v := fmt.Sprintf("PROBE%d", i)
		sent = append(sent, v)
		send(v)
	}
	waitForHTTPRequests(t, m, base+2+30)
	// Every one of the 32 requests reached the handler — the server accepted
	// each client-chosen verb rather than refusing it in the parser, which is
	// what makes the middleware above the only thing standing between the
	// verb and a label value. (32 = the GET + FOO + the 30 PROBEn.)
	if got := served.Load(); got != 32 {
		t.Fatalf("the handler served %d requests, want 32: the server refused a client-chosen method before the middleware saw it, so this test measured nothing", got)
	}
	body = scrape(t, m)
	afterMany := methods(body, requests)
	for _, verb := range sent {
		if afterMany[verb] {
			t.Errorf("the client-chosen verb %q became a label value:\n%s", verb, body)
		}
	}
	if len(afterMany) != len(afterOne) {
		t.Errorf("31 distinct non-standard verbs grew the method value set from %v to %v — one anonymous client, one series per request:\n%s",
			afterOne, afterMany, body)
	}
	if got := strings.Count(body, requests+"{"); got > 2 {
		t.Errorf("post_http_requests_total has %d series after 32 requests in 31 distinct verbs, want at most 2 (GET and %q):\n%s",
			got, httpMethodOther, body)
	}
	if got := methods(body, duration); len(got) != len(afterOne) {
		t.Errorf("post_http_request_duration_seconds method values grew with the verbs: %v:\n%s", got, body)
	}
}

// A reconciliation pass that fails knows nothing about how much drift is
// open. Zeroing the gauge would report "no drift" during exactly the outage
// that probably caused some — and would clear the P1 drift alert while the
// platform is blind. This is the bug the gauge's two entry points exist to
// prevent, so it is asserted rather than commented.
func TestFailedReconciliationPassLeavesTheDriftGaugeAlone(t *testing.T) {
	m := NewMetrics()
	m.ObserveReconciliation(ReconcileOutcomeOK, 12, 3, 0, 3)

	if !strings.Contains(scrape(t, m), "post_rsg_reconciliation_open_findings 3") {
		t.Fatalf("the gauge did not record the pass's open findings:\n%s", scrape(t, m))
	}

	m.ObserveReconciliationFailure()
	body := scrape(t, m)
	if !strings.Contains(body, "post_rsg_reconciliation_open_findings 3") {
		t.Errorf("a failed pass changed the drift gauge; it must leave the last real value in place:\n%s", body)
	}
	if !strings.Contains(body, `post_rsg_reconciliation_passes_total{outcome="ok"} 1`) ||
		!strings.Contains(body, `post_rsg_reconciliation_passes_total{outcome="failed"} 1`) {
		t.Errorf("the two outcomes are not distinguishable in the pass counter:\n%s", body)
	}
}

// The scrape-time contract, from both sides: a source that cannot be read
// produces NO sample (not a zero) and DOES increment the collector-error
// family. That pair is the whole reason this task added the second family.
func TestCollectorEmitsNoSampleAndCountsTheFailure(t *testing.T) {
	m := NewMetrics()

	down := errors.New("connection refused")
	if err := m.Register(NewFuncCollector(m, "post_probe_down", "a source that is down",
		nil, func(context.Context) ([]Sample, error) { return nil, down })); err != nil {
		t.Fatalf("register: %v", err)
	}
	up := 0.0
	if err := m.Register(NewFuncCollector(m, "post_probe_up", "a source that answers",
		nil, func(context.Context) ([]Sample, error) {
			up++
			return []Sample{{Value: up}}, nil
		})); err != nil {
		t.Fatalf("register: %v", err)
	}

	body := scrape(t, m)
	if lines := seriesLines(body, "post_probe_down"); len(lines) != 0 {
		t.Errorf("the failing collector emitted %v; a fabricated zero is worse than silence:\n%s", lines, body)
	}
	if got := seriesValue(t, body, "post_probe_up"); got != 1 {
		t.Errorf("the working collector's value is %v, want 1:\n%s", got, body)
	}

	// The error counter is asserted on the NEXT scrape, and that is a
	// property of the instrument, not a convenience. client_golang gathers a
	// registry through a worker pool (registry.go, collectWorker), so the
	// counter family and the failing collector are read concurrently: whether
	// this scrape's increment is folded in before the family is read is a
	// race. Measured, not assumed — asserting it on the first scrape failed
	// five runs in five here. What the instrument does guarantee is that the
	// failed read becomes visible by the following scrape, which is all an
	// alert needs: rate() over ten minutes cannot see a 15s lag.
	body2 := scrape(t, m)
	if got := seriesValue(t, body2, `post_metrics_collector_errors_total{collector="post_probe_down"}`); got < 1 {
		t.Errorf("the failed read is not visible by the second scrape (got %v); silence and zero are then the same observation:\n%s", got, body2)
	}

	// The gauge is read per scrape, not cached: a second gather sees the
	// source again (the numbers in /metrics are the numbers at that instant).
	if got := seriesValue(t, body2, "post_probe_up"); got != 2 {
		t.Errorf("the collector did not re-read its source on the second scrape (got %v, want 2):\n%s", got, body2)
	}
}

// A scrape-time source that reports a monotonic event count is declared a
// counter, so the exposition's `# TYPE` line agrees with the series' own
// `_total` name. This is not cosmetic: the type line is what tells an
// operator whether rate()/increase() apply, and a `_total` series declared
// gauge is the endpoint promising counter semantics it does not have.
func TestCounterCollectorIsDeclaredACounter(t *testing.T) {
	m := NewMetrics()

	acquires := 0.0
	if err := m.Register(NewCounterCollector(m, "post_probe_acquires_total",
		"a monotonic source read at scrape time", []string{"kind"},
		func(context.Context) ([]Sample, error) {
			acquires += 7
			return []Sample{{Labels: prometheus.Labels{"kind": "acquired"}, Value: acquires}}, nil
		})); err != nil {
		t.Fatalf("register: %v", err)
	}
	// The gauge constructor is asserted beside it so the two are shown to be
	// distinguishable rather than both happening to say "counter".
	if err := m.Register(NewFuncCollector(m, "post_probe_depth", "a level read at scrape time",
		nil, func(context.Context) ([]Sample, error) { return []Sample{{Value: 3}}, nil })); err != nil {
		t.Fatalf("register: %v", err)
	}

	body := scrape(t, m)
	if !strings.Contains(body, "# TYPE post_probe_acquires_total counter") {
		t.Errorf("the counter collector is not declared a counter:\n%s", body)
	}
	if !strings.Contains(body, "# TYPE post_probe_depth gauge") {
		t.Errorf("the gauge collector is not declared a gauge:\n%s", body)
	}
	if got := seriesValue(t, body, `post_probe_acquires_total{kind="acquired"}`); got != 7 {
		t.Errorf("value = %v, want 7:\n%s", got, body)
	}
	if got := seriesValue(t, body, "post_probe_depth"); got != 3 {
		t.Errorf("value = %v, want 3:\n%s", got, body)
	}
}

// A sample with the wrong label shape is dropped and counted, never emitted
// under a shape it does not have.
func TestCollectorDropsSamplesWithMissingLabels(t *testing.T) {
	m := NewMetrics()
	if err := m.Register(NewFuncCollector(m, "post_probe_labels", "labelled source",
		[]string{"state"}, func(context.Context) ([]Sample, error) {
			return []Sample{
				{Labels: prometheus.Labels{"state": "jobs"}, Value: 4},
				{Labels: prometheus.Labels{"wrong": "name"}, Value: 9},
			}, nil
		})); err != nil {
		t.Fatalf("register: %v", err)
	}

	body := scrape(t, m)
	if got := seriesValue(t, body, `post_probe_labels{state="jobs"}`); got != 4 {
		t.Errorf("the well-formed sample is %v, want 4:\n%s", got, body)
	}
	if lines := seriesLines(body, "post_probe_labels"); len(lines) != 1 {
		t.Errorf("the family has %d series, want 1 — the malformed sample was emitted under a shape it does not have:\n%s",
			len(lines), body)
	}

	// Same one-scrape lag as TestCollectorEmitsNoSampleAndCountsTheFailure,
	// for the same reason (concurrent gather); see the comment there.
	body2 := scrape(t, m)
	if got := seriesValue(t, body2, `post_metrics_collector_errors_total{collector="post_probe_labels"}`); got < 1 {
		t.Errorf("the dropped sample was not counted by the second scrape (got %v):\n%s", got, body2)
	}
}
