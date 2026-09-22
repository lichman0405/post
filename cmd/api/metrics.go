package main

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/worker"
)

// The API process's metric sources (T1109, docs/26 §3).
//
// Two kinds of family leave this binary. The pushed ones are incremented by
// the code that does the work — the HTTP middleware and refusal counter in
// internal/observability, authhttp.WriteError, runReconciliationSweep — and
// need no wiring here because they already reach observability.Default().
// The pulled ones below are properties of a dependency at the moment of the
// scrape, so they are read here, on demand.
//
// One collector per metric NAME: the registry refuses two collectors that
// describe the same family, so a family fed by several queues is one
// collector that loops, not one collector per queue.

// metricsPingTimeout bounds the PostgreSQL liveness probe one scrape runs.
// It is shorter than observability.CollectTimeout because the scrape's total
// budget is 10s and the ping shares it with the other sources.
const metricsPingTimeout = 2 * time.Second

// registerAPIMetrics adds the API process's scrape-time collectors to m.
//
// It returns an error rather than logging and continuing: a /metrics
// endpoint that silently lost a family would answer 200 with the family
// absent, which is the failure mode this task exists to remove.
func registerAPIMetrics(m *observability.Metrics, pool *pgxpool.Pool, queues ...*worker.RedisQueue) error {
	// Occupancy (docs/26 §3 "DB pool/query").
	if err := m.Register(observability.NewFuncCollector(m,
		"post_db_pool_connections",
		"PostgreSQL pool connections by state (docs/26 §3 DB pool).",
		[]string{"state"},
		func(context.Context) ([]observability.Sample, error) {
			st := pool.Stat()
			return []observability.Sample{
				{Labels: map[string]string{"state": "total"}, Value: float64(st.TotalConns())},
				{Labels: map[string]string{"state": "acquired"}, Value: float64(st.AcquiredConns())},
				{Labels: map[string]string{"state": "idle"}, Value: float64(st.IdleConns())},
				{Labels: map[string]string{"state": "constructing"}, Value: float64(st.ConstructingConns())},
			}, nil
		})); err != nil {
		return err
	}

	if err := m.Register(observability.NewFuncCollector(m,
		"post_db_pool_max_connections",
		"The pool's configured connection ceiling (docs/26 §3 DB pool).",
		nil,
		func(context.Context) ([]observability.Sample, error) {
			return []observability.Sample{{Value: float64(pool.Stat().MaxConns())}}, nil
		})); err != nil {
		return err
	}

	// Acquire pressure: these move only when callers contend for a
	// connection (empty) or give up waiting (canceled) — the two numbers
	// that mean "the pool is the bottleneck" rather than "the pool is busy".
	// acquired is carried alongside so the pressure can be read as a ratio.
	//
	// NewCounterCollector, not NewFuncCollector: all three are pgxpool's
	// monotonic process-lifetime totals, and the name ends in _total. Emitting
	// them as gauges made the exposition say `gauge` about a _total series —
	// the type an operator reads before deciding whether rate() or `>` is the
	// right query.
	if err := m.Register(observability.NewCounterCollector(m,
		"post_db_pool_acquires_total",
		"PostgreSQL pool acquire outcomes by kind (docs/26 §3 DB pool).",
		[]string{"kind"},
		func(context.Context) ([]observability.Sample, error) {
			st := pool.Stat()
			return []observability.Sample{
				{Labels: map[string]string{"kind": "acquired"}, Value: float64(st.AcquireCount())},
				{Labels: map[string]string{"kind": "empty"}, Value: float64(st.EmptyAcquireCount())},
				{Labels: map[string]string{"kind": "canceled"}, Value: float64(st.CanceledAcquireCount())},
			}, nil
		})); err != nil {
		return err
	}

	// post_db_up is the "database unavailable" signal of docs/26 §6 P1: an
	// actual connection attempt rather than an inference from a counter that
	// may simply not have moved yet. 1 when a ping completes inside
	// metricsPingTimeout, 0 otherwise — a down database is 0, never a
	// missing sample, because an absent series leaves the alert with nothing
	// to evaluate and silence would read as a healthy scrape.
	if err := m.Register(observability.NewFuncCollector(m,
		"post_db_up",
		"1 when a PostgreSQL ping succeeded within the scrape's probe budget, 0 otherwise (docs/26 §6 P1 database unavailable).",
		nil,
		func(ctx context.Context) ([]observability.Sample, error) {
			pingCtx, cancel := context.WithTimeout(ctx, metricsPingTimeout)
			defer cancel()
			up := 0.0
			if err := pool.Ping(pingCtx); err == nil {
				up = 1
			}
			return []observability.Sample{{Value: up}}, nil
		})); err != nil {
		return err
	}

	// Queue depth (docs/26 §3 "queue depth"). RedisQueue.Depth has existed
	// since the queue was written and never had a production caller; this is
	// it. The API owns the two queues it enqueues onto, which are the same
	// Redis lists cmd/worker consumes.
	if err := m.Register(observability.NewFuncCollector(m,
		"post_queue_depth",
		"Job queue lengths by queue and state (docs/26 §3 queue depth).",
		[]string{"queue", "state"},
		func(ctx context.Context) ([]observability.Sample, error) {
			var out []observability.Sample
			for _, q := range queues {
				samples, err := q.CollectDepth(ctx)
				if err != nil {
					return nil, err
				}
				out = append(out, samples...)
			}
			return out, nil
		})); err != nil {
		return err
	}
	return nil
}

// metricsHandler mounts the endpoint. It is served from its own mux, OUTSIDE
// observability.Middleware, on purpose: a scrape is not product traffic, and
// counting it in post_http_requests_total would put the monitoring system's
// own request rate into the numbers its alerts are computed from — an alert
// on request rate would then partially measure the scraper.
func metricsHandler(m *observability.Metrics) http.Handler { return m.Handler() }
