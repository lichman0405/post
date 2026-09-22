package observability

import (
	"net/http"
	"strconv"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics is the process's metric registry (docs/26 §3).
//
// # Why a registry of our own, and not prometheus.DefaultRegisterer
//
// The endpoint's content is the deliverable, so it is enumerated here rather
// than accumulated by whichever import happens to register a collector first.
// A dedicated registry means /metrics answers exactly the families this file
// declares — no process/go runtime collectors, nothing a transitive dependency
// registered — so "which families does POST expose" is answerable by reading
// one file, and a family that appears in production but not here is a bug
// rather than a default.
//
// # The families, and which docs/26 §3 item each one is
//
//	post_http_requests_total                     HTTP error
//	post_http_request_duration_seconds           HTTP latency
//	post_permission_denials_total                permission denied rates
//	post_queue_errors_total                      queue failures
//	post_queue_jobs_total                        queue failures (outcome split)
//	post_queue_depth                             queue depth     (scrape-time)
//	post_outbox_publish_failures_total           outbox lag      (failure half)
//	post_outbox_pending_events                   outbox lag      (scrape-time)
//	post_outbox_oldest_pending_seconds           outbox lag      (scrape-time)
//	post_webhook_deliveries_total                webhook success
//	post_search_query_duration_seconds           search latency
//	post_planner_runs_total                      LLM planner failures
//	post_planner_fallbacks_total                 LLM planner failures (per cause)
//	post_rsg_reconciliation_*                    RSG reconciliation drift
//	post_db_pool_* / post_db_up                  DB pool/query
//	post_metrics_collector_errors_total          self-observation
//
// Two §3 items have no family here because they have no source anywhere in
// this tree: "Git ingestion lag" and "blob upload/failure". Their absence is
// deliberate and is recorded in ops/observability/alerts.yml beside the rules
// — inventing a counter with nothing behind it would make the endpoint look
// complete while measuring nothing.
//
// # Naming and cardinality
//
// Everything is prefixed post_ (the platform's namespace) and every label's
// value comes from a closed set defined in code: HTTP status classes are
// numbers the server chose, routes are Go 1.22+ ServeMux patterns (never
// r.URL.Path — a project id in a label is unbounded cardinality and a leak of
// which ids exist), the HTTP method is folded to httpMethodLabel's set (the
// one producer whose input does arrive from the wire — see below), planner
// fallback reasons are planner.Reason's closed set.
//
// # The method label, and why it is folded rather than passed through
//
// Every other label here is produced by the server choosing a value. `method`
// is the exception: its input is http.Request.Method, the raw request-line
// token, and RFC 9110 makes ANY valid token a legal method — a client sending
// `FOO /x HTTP/1.1` with a fresh token per request reaches the handler and the
// middleware with `Method: "FOO"`, no routing required. Passing that straight
// into WithLabelValues would let one anonymous caller grow both HTTP families
// without bound, one series per request, for as long as it kept sending.
//
// So the value is not the request's method; it is the request's method IF it
// is one of the nine the standard names, and "OTHER" otherwise
// (httpMethodLabel). The label still answers everything the rules and panels
// ask of it — "what kind of request was this, and how much of it was a write"
// — because those questions are about the standard verbs; a request whose
// verb is not one of them is one bucket, however it is spelled. What it no
// longer does is carry an attacker-chosen string, which is the property
// TestHTTPMethodLabelIsAClosedSet asserts over a real socket.
type Metrics struct {
	reg *prometheus.Registry

	// httpRequests counts completed requests. Status is the class the
	// handler actually wrote ("200", "403", "503"), not a bucketed class:
	// docs/26 §3 asks for "HTTP latency/error" and §6 alerts on specific
	// conditions, and collapsing 4xx/5xx would throw away the distinction
	// the rules need.
	httpRequests *prometheus.CounterVec
	// httpDuration is the latency histogram. Buckets are
	// prometheus.DefBuckets (5ms…10s): no numeric SLO is stated in the
	// specs (docs/27 §SLO names the budget qualitatively), so the honest
	// ladder is the one every Prometheus deployment already understands
	// rather than a threshold this task invented.
	httpDuration *prometheus.HistogramVec

	// permissionDenials counts refusals by the surface that produced them
	// and the decision class. See cmd/api/authhttp/envelope.go for the
	// boundary tier and internal/events/inbox_store.go for the silent-filter
	// tier — the two places the verdict is consumed.
	permissionDenials *prometheus.CounterVec

	// queueErrors counts queue operations that failed (the dependency is
	// down, or the reply was unusable). This is the family that moves when
	// Redis disappears, and it is therefore what the queue alert fires on.
	queueErrors *prometheus.CounterVec
	// queueJobs counts terminal job outcomes.
	queueJobs *prometheus.CounterVec

	// outboxPublishFailures counts dispatcher passes that could not complete
	// — the database being unreachable is the common cause, which is why the
	// database alert reads it as well as post_db_up.
	outboxPublishFailures prometheus.Counter

	// webhookDeliveries counts one delivery attempt by outcome, using the
	// classification internal/events/deliver.go already makes.
	webhookDeliveries *prometheus.CounterVec

	// searchQueries is the query-latency histogram.
	searchQueries *prometheus.HistogramVec

	// plannerRuns / plannerFallbacks are split so the fallback RATE is
	// fallbacks/runs, and the per-cause counter is the one
	// planner.Reason's closed set was declared for (plan.go:70-72).
	plannerRuns      *prometheus.CounterVec
	plannerFallbacks *prometheus.CounterVec

	// Reconciliation (docs/16 §5 — any drift is a high-severity alert).
	reconcilePasses           *prometheus.CounterVec
	reconcileFindingsOpened   prometheus.Counter
	reconcileFindingsResolved prometheus.Counter
	reconcileOpenFindings     prometheus.Gauge
	reconcileRefsChecked      prometheus.Counter

	// collectorErrors counts scrape-time source reads that failed. A
	// collector that cannot reach its source emits no samples, and silence
	// is indistinguishable from zero — this family is what makes that
	// difference visible.
	collectorErrors *prometheus.CounterVec
}

// defaultMetrics is the process-wide instance. It mirrors slog.SetDefault:
// the producers that emit metrics (the worker loop, the outbox dispatcher,
// the planner) are reached through code paths that have no constructor of
// their own to thread a registry through, exactly as they reach their logger
// through slog.Default().
var (
	defaultOnce    sync.Once
	defaultMetrics *Metrics
)

// Default returns the process's metrics. Never nil.
func Default() *Metrics {
	defaultOnce.Do(func() { defaultMetrics = NewMetrics() })
	return defaultMetrics
}

// NewMetrics builds a registry with the docs/26 §3 families registered on it.
func NewMetrics() *Metrics {
	m := &Metrics{reg: prometheus.NewRegistry()}

	m.httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "post_http_requests_total",
		Help: "Completed HTTP requests by method, matched route pattern and response status (docs/26 §3 HTTP error).",
	}, []string{"method", "route", "status"})

	m.httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "post_http_request_duration_seconds",
		Help:    "HTTP request latency by method and matched route pattern (docs/26 §3 HTTP latency).",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "route"})

	m.permissionDenials = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "post_permission_denials_total",
		Help: "Refusals by surface and decision class (docs/26 §3 permission denied rates).",
	}, []string{"surface", "decision"})

	m.queueErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "post_queue_errors_total",
		Help: "Failed job-queue operations by operation (docs/26 §3 queue failures).",
	}, []string{"operation"})

	m.queueJobs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "post_queue_jobs_total",
		Help: "Terminal job outcomes by outcome (docs/26 §3 queue failures).",
	}, []string{"outcome"})

	m.outboxPublishFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "post_outbox_publish_failures_total",
		Help: "Outbox dispatcher passes that could not complete (docs/26 §3 outbox lag).",
	})

	m.webhookDeliveries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "post_webhook_deliveries_total",
		Help: "Webhook delivery attempts by outcome (docs/26 §3 webhook success).",
	}, []string{"outcome"})

	m.searchQueries = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "post_search_query_duration_seconds",
		Help:    "Search query latency by outcome (docs/26 §3 search latency).",
		Buckets: prometheus.DefBuckets,
	}, []string{"outcome"})

	m.plannerRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "post_planner_runs_total",
		Help: "Search planner runs by status (docs/26 §3 LLM planner failures).",
	}, []string{"status"})

	m.plannerFallbacks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "post_planner_fallbacks_total",
		Help: "Search planner fallbacks by cause (docs/26 §3 LLM planner failures).",
	}, []string{"reason"})

	m.reconcilePasses = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "post_rsg_reconciliation_passes_total",
		Help: "Git/RSG reconciliation passes by outcome (docs/26 §3 RSG reconciliation drift).",
	}, []string{"outcome"})

	m.reconcileFindingsOpened = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "post_rsg_reconciliation_findings_opened_total",
		Help: "Git/RSG drift findings opened (docs/16 §5, docs/26 §3/§6 P1 RSG/Git drift).",
	})

	m.reconcileFindingsResolved = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "post_rsg_reconciliation_findings_resolved_total",
		Help: "Git/RSG drift findings resolved by re-verification.",
	})

	m.reconcileOpenFindings = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "post_rsg_reconciliation_open_findings",
		Help: "Git/RSG drift findings open as of the last completed pass.",
	})

	m.reconcileRefsChecked = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "post_rsg_reconciliation_refs_checked_total",
		Help: "Branch refs verified by the Git/RSG reconciliation loop.",
	})

	m.collectorErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "post_metrics_collector_errors_total",
		Help: "Scrape-time collector source reads that failed, by collector.",
	}, []string{"collector"})

	m.reg.MustRegister(
		m.httpRequests, m.httpDuration, m.permissionDenials,
		m.queueErrors, m.queueJobs, m.outboxPublishFailures,
		m.webhookDeliveries, m.searchQueries,
		m.plannerRuns, m.plannerFallbacks,
		m.reconcilePasses, m.reconcileFindingsOpened, m.reconcileFindingsResolved,
		m.reconcileOpenFindings, m.reconcileRefsChecked,
		m.collectorErrors,
	)
	return m
}

// Handler serves the registry in the Prometheus text exposition format
// (OpenMetrics when the scraper asks for it via Accept).
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{EnableOpenMetrics: true})
}

// Register adds a scrape-time collector (see NewFuncCollector) to the
// registry. It is intended to be called during wiring, before the endpoint
// serves.
func (m *Metrics) Register(c prometheus.Collector) error { return m.reg.Register(c) }

// Gatherer exposes the registry for tests that assert on the exposition.
func (m *Metrics) Gatherer() prometheus.Gatherer { return m.reg }

// CollectorError records a failed scrape-time source read. Collectors built
// by NewFuncCollector call it themselves; it is exported for the rare
// collector that needs a source-specific counter instead.
func (m *Metrics) CollectorError(name string) { m.collectorErrors.WithLabelValues(name).Inc() }

// ---------------------------------------------------------------------------
// Accessors used by the producers. One method per family keeps the label
// values' closed sets in this file rather than at each call site.
// ---------------------------------------------------------------------------

// httpMethodOther is what a request whose method is not one of the nine the
// standard library names is labelled with. One bucket for all of them.
const httpMethodOther = "OTHER"

// httpMethodLabel maps a request's method onto the metric label's closed set.
// The set is the nine methods net/http names; everything else — including
// syntactically valid extensions such as PROPFIND or a fresh random token per
// request — is httpMethodOther.
//
// This is the ONLY producer whose input arrives from the wire, so it is the
// only one that needs folding; every other label value is a constant this
// package declares or a string the server built. The switch is exhaustive
// over http.Method* rather than a map, so a reader can count the set in one
// screen and a future net/http addition lands in OTHER until someone teaches
// it here on purpose.
func httpMethodLabel(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodConnect,
		http.MethodOptions, http.MethodTrace:
		return method
	default:
		return httpMethodOther
	}
}

// ObserveHTTP records one completed request. route must be the ServeMux
// pattern that matched (http.Request.Pattern), never the raw path; method is
// folded to the closed set above before it becomes a label (see the type
// comment — the input is the client's, so it cannot be a label value
// verbatim).
func (m *Metrics) ObserveHTTP(method, route string, status int, seconds float64) {
	method = httpMethodLabel(method)
	m.httpRequests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	m.httpDuration.WithLabelValues(method, route).Observe(seconds)
}

// ObservePermissionDenial records one refusal. surface names where the
// verdict was consumed; decision is the refusal class. Both are closed sets
// maintained at the call sites (cmd/api/authhttp, internal/events).
func (m *Metrics) ObservePermissionDenial(surface, decision string) {
	m.permissionDenials.WithLabelValues(surface, decision).Inc()
}

// The decision vocabulary. Defined here so "which refusals are counted" is
// one list rather than a convention spread over the call sites.
const (
	// DecisionForbidden: the caller was identified and refused — the 403
	// class. Covers the CSRF/Origin guard and every per-handler authorization
	// refusal.
	DecisionForbidden = "forbidden"
	// DecisionUnauthenticated: no usable session — the 401 class.
	DecisionUnauthenticated = "unauthenticated"
	// DecisionFiltered: the request was granted in form (a 200) but the
	// decision REMOVED items from the answer. This is the tier no HTTP
	// status can show: the caller sees a shorter list, never an error, so
	// it is counted where the filter runs (internal/events/inbox_store.go).
	//
	// One increment per shortened READ, like the other two decisions, which
	// are one per refused request — not one per dropped row. A read that
	// hides three rows is one shorter answer, and a per-row count would make
	// `sum by (decision)` add requests to rows.
	DecisionFiltered = "filtered"
)

// The reconciliation outcome vocabulary, read by the drift alert. "failed"
// exists so that "could not look" and "looked and found nothing" stay
// distinguishable in the series — see ObserveReconciliationFailure.
const (
	ReconcileOutcomeOK     = "ok"
	ReconcileOutcomeFailed = "failed"
)

// ObserveRefusal records a product refusal by the HTTP status the edge turned
// it into. Only the two statuses that mean "refused for who you are" are
// counted: 403 and 401. Everything else — including 404, which this platform
// also uses as a refusal — is not a denial this metric can distinguish, and
// the family says so rather than guessing.
func (m *Metrics) ObserveRefusal(surface string, status int) {
	switch status {
	case http.StatusForbidden:
		m.ObservePermissionDenial(surface, DecisionForbidden)
	case http.StatusUnauthorized:
		m.ObservePermissionDenial(surface, DecisionUnauthenticated)
	}
}

// ObserveQueueError records one failed queue operation.
func (m *Metrics) ObserveQueueError(operation string) {
	m.queueErrors.WithLabelValues(operation).Inc()
}

// ObserveQueueJob records one terminal job outcome.
func (m *Metrics) ObserveQueueJob(outcome string) {
	m.queueJobs.WithLabelValues(outcome).Inc()
}

// ObserveOutboxPublishFailure records one dispatcher pass that did not
// complete.
func (m *Metrics) ObserveOutboxPublishFailure() { m.outboxPublishFailures.Inc() }

// ObserveWebhookDelivery records one delivery attempt by outcome.
func (m *Metrics) ObserveWebhookDelivery(outcome string) {
	m.webhookDeliveries.WithLabelValues(outcome).Inc()
}

// ObserveSearchQuery records one search query's latency.
func (m *Metrics) ObserveSearchQuery(outcome string, seconds float64) {
	m.searchQueries.WithLabelValues(outcome).Observe(seconds)
}

// The planner status vocabulary: planner.StatusPlanned / StatusFallback.
const (
	PlannerStatusPlanned  = "planned"
	PlannerStatusFallback = "fallback"
)

// ObservePlannerPlanned records a run that produced a usable plan.
func (m *Metrics) ObservePlannerPlanned() {
	m.plannerRuns.WithLabelValues(PlannerStatusPlanned).Inc()
}

// ObservePlannerFallback records a run that degraded to structured results,
// attributed to the cause planner.Reason names. The fallback RATE is
// post_planner_fallbacks_total / post_planner_runs_total.
func (m *Metrics) ObservePlannerFallback(reason string) {
	m.plannerRuns.WithLabelValues(PlannerStatusFallback).Inc()
	m.plannerFallbacks.WithLabelValues(reason).Inc()
}

// ObserveReconciliation records one completed reconciliation pass — one that
// actually read the RSG/Git state and therefore knows how much drift is open.
func (m *Metrics) ObserveReconciliation(outcome string, refsChecked, opened, resolved, open int) {
	m.reconcilePasses.WithLabelValues(outcome).Inc()
	if refsChecked > 0 {
		m.reconcileRefsChecked.Add(float64(refsChecked))
	}
	if opened > 0 {
		m.reconcileFindingsOpened.Add(float64(opened))
	}
	if resolved > 0 {
		m.reconcileFindingsResolved.Add(float64(resolved))
	}
	m.reconcileOpenFindings.Set(float64(open))
}

// ObserveReconciliationFailure records a pass that did not complete. It
// deliberately does NOT touch post_rsg_reconciliation_open_findings.
//
// The open-findings gauge drives the P1 drift alert, and a failed pass has
// read no state at all: it knows nothing about how much drift is open. Setting
// the gauge to 0 would report "no drift" at the exact moment the platform
// cannot tell — silently clearing the drift alert during the outage that is
// most likely to have caused the drift. Leaving the last known value in place
// is stale, not wrong, and the staleness is itself visible: the pass counter's
// outcome="failed" series climbs and post_db_up goes to 0 beside it.
//
// This is the same rule the scrape-time collectors follow (see collect.go):
// on error, emit no sample rather than a fabricated one.
func (m *Metrics) ObserveReconciliationFailure() {
	m.reconcilePasses.WithLabelValues(ReconcileOutcomeFailed).Inc()
}
