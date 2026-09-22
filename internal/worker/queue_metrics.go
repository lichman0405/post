package worker

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/lichman0405/post/internal/observability"
)

// The metric adapter for RedisQueue, in its own file on purpose: queue.go is
// listed in ops/ci/staticcheck-baseline.txt by line number for two
// grandfathered SA1019 findings (BRPopLPush/RPopLPush). Adding imports to it
// shifts those lines and un-baselines them, and the gate's own fixture
// (scripts/tests/staticcheck-unit-test.sh) pins the same line numbers, so a
// shift reddens make check. Keeping the adapter here leaves the baselined file
// byte-identical. The metrics surface being separate from the queue mechanics
// is the shape internal/observability already uses (metrics.go vs collect.go).

// CollectDepth is Depth as a metric: one sample per list, read on every
// scrape (docs/26 §3 "queue depth"). It is the production caller Depth did
// not have — before this the method was reachable only from tests, so the
// dead-letter list was written in production and readable only by a test.
//
// Failure is passed through, not softened: the gauge collector that wraps
// this emits no sample when the read fails, and counts the failure in
// post_metrics_collector_errors_total. A Redis outage therefore shows up as
// the read-failure counter climbing (and post_queue_errors_total with it),
// never as a queue that looks empty.
func (q *RedisQueue) CollectDepth(ctx context.Context) ([]observability.Sample, error) {
	jobs, processing, dead, err := q.Depth(ctx)
	if err != nil {
		return nil, err
	}
	// The queue label is the namespace prefix, which is a deployment
	// constant ("post", "post-provisioning"), not a job or request value.
	sample := func(state string, v int64) observability.Sample {
		return observability.Sample{
			Labels: prometheus.Labels{"queue": q.prefix, "state": state},
			Value:  float64(v),
		}
	}
	return []observability.Sample{
		sample("jobs", jobs),
		sample("processing", processing),
		sample("dead", dead),
	}, nil
}

// QueueName is the namespace prefix this queue's keys live under. It is the
// value the post_queue_depth gauge labels its samples with.
func (q *RedisQueue) QueueName() string { return q.prefix }
