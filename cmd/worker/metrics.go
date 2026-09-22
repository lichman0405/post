package main

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/worker"
)

// The worker process's scrape-time metric sources (T1109, docs/26 §3).
//
// Everything else this process reports is pushed: the loop increments the
// queue families as it works, the dispatcher increments the outbox failure
// counter, the deliverer increments the webhook family, and the fan-outs
// increment the permission family. Only these three are properties of a
// dependency at the moment of the scrape.
func registerWorkerMetrics(m *observability.Metrics, pool *pgxpool.Pool, queue *worker.RedisQueue, dispatcher *events.Dispatcher) error {
	// Queue depth (docs/26 §3). Depth() had no production caller before
	// this task — the dead-letter list was written here and readable only
	// from a test.
	if err := m.Register(observability.NewFuncCollector(m,
		"post_queue_depth",
		"Job queue lengths by queue and state (docs/26 §3 queue depth).",
		[]string{"queue", "state"},
		queue.CollectDepth)); err != nil {
		return err
	}

	// Outbox lag (docs/26 §3), the two halves of it: how much is waiting and
	// how long the oldest piece has waited. The depth alone cannot tell a
	// busy outbox from a stuck one.
	if err := m.Register(observability.NewFuncCollector(m,
		"post_outbox_pending_events",
		"Outbox rows not yet published (docs/26 §3 outbox lag, §6 P2 event/outbox backlog).",
		nil,
		func(ctx context.Context) ([]observability.Sample, error) {
			b, err := dispatcher.Backlog(ctx)
			if err != nil {
				return nil, err
			}
			return []observability.Sample{{Value: float64(b.Pending)}}, nil
		})); err != nil {
		return err
	}
	if err := m.Register(observability.NewFuncCollector(m,
		"post_outbox_oldest_pending_seconds",
		"Age of the oldest unpublished outbox row, 0 when none is pending (docs/26 §3 outbox lag).",
		nil,
		func(ctx context.Context) ([]observability.Sample, error) {
			b, err := dispatcher.Backlog(ctx)
			if err != nil {
				return nil, err
			}
			return []observability.Sample{{Value: b.OldestPending.Seconds()}}, nil
		})); err != nil {
		return err
	}

	// Database liveness, the worker's half of the docs/26 §6 P1 "database
	// unavailable" signal. cmd/api probes the same thing on its own endpoint
	// because the two processes can lose the database independently — the
	// worker's dispatcher failing while the API still has a pooled
	// connection is a real, distinguishable state.
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
	return nil
}

// metricsPingTimeout bounds one liveness probe; cmd/api/metrics.go carries
// the same constant for its own endpoint. It is declared in each binary
// rather than shared because neither binary imports the other.
const metricsPingTimeout = 2 * time.Second
