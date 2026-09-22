package observability

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// This file is the scrape-time half of the metric surface.
//
// Counters and histograms are pushed by the code path that does the work
// (internal/worker, internal/events, the HTTP middleware). Some of the
// docs/26 §3 families are not like that: a queue's depth, a pool's connection
// counts and the outbox's backlog are PROPERTIES OF A DEPENDENCY at the moment
// of the scrape, not events. They are read on demand.
//
// They are read here rather than cached by a background poller for the reason
// the specs' own alert classes imply: a stale gauge under-reports a backlog
// exactly when the process is too busy to refresh it, which is when the alert
// matters. Reading at scrape time means the number in /metrics is the number
// at that instant, and a source that cannot be read is visibly a source that
// cannot be read (CollectorError) rather than a plausible-looking zero.

// CollectTimeout bounds one scrape-time source read. It is well under
// Prometheus's own 10s scrape timeout so a stuck dependency produces the
// collector-error family rather than a scrape that never returns.
const CollectTimeout = 3 * time.Second

// Sample is one scrape-time observation. Labels must cover exactly the
// labelNames the collector was declared with; a sample whose labels do not
// is dropped and counted, never emitted with a wrong shape.
type Sample struct {
	Labels prometheus.Labels
	Value  float64
}

// NewFuncCollector builds a gauge collector that calls fn on every scrape.
//
// fn returning an error is the normal case for a down dependency: the
// collector then emits NO samples for that scrape and increments
// post_metrics_collector_errors_total{collector=name}. Silence plus an error
// count is the honest answer — substituting 0 would say "the queue is empty"
// when the truth is "the queue could not be read".
func NewFuncCollector(m *Metrics, name, help string, labelNames []string, fn func(ctx context.Context) ([]Sample, error)) prometheus.Collector {
	return newFuncCollector(m, prometheus.GaugeValue, name, help, labelNames, fn)
}

// NewCounterCollector is NewFuncCollector for a scrape-time source whose
// value is a monotonic event count rather than a level — pgxpool's
// AcquireCount and friends are the case this exists for. The distinction is
// not cosmetic: the name of such a family ends in _total, and a _total
// series that the exposition declares `gauge` is a promise the endpoint does
// not keep — an operator reading the type trusts counter semantics
// (resets are handled, rate() is the natural query) that a gauge does not
// have.
//
// The value must be monotonic within the process's lifetime only, which is
// what a counter is: the source resets with the process, and Prometheus
// detects that as every other counter reset.
func NewCounterCollector(m *Metrics, name, help string, labelNames []string, fn func(ctx context.Context) ([]Sample, error)) prometheus.Collector {
	return newFuncCollector(m, prometheus.CounterValue, name, help, labelNames, fn)
}

func newFuncCollector(m *Metrics, valueType prometheus.ValueType, name, help string, labelNames []string, fn func(ctx context.Context) ([]Sample, error)) prometheus.Collector {
	return &funcCollector{
		name:       name,
		help:       help,
		labelNames: labelNames,
		desc:       prometheus.NewDesc(name, help, labelNames, nil),
		valueType:  valueType,
		fn:         fn,
		metrics:    m,
	}
}

type funcCollector struct {
	name       string
	help       string
	labelNames []string
	desc       *prometheus.Desc
	valueType  prometheus.ValueType
	fn         func(ctx context.Context) ([]Sample, error)
	metrics    *Metrics
}

func (c *funcCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *funcCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), CollectTimeout)
	defer cancel()

	samples, err := c.fn(ctx)
	if err != nil {
		if c.metrics != nil {
			c.metrics.CollectorError(c.name)
		}
		return
	}
	for _, s := range samples {
		values := make([]string, len(c.labelNames))
		ok := true
		for i, n := range c.labelNames {
			v, present := s.Labels[n]
			if !present {
				ok = false
				break
			}
			values[i] = v
		}
		m, err := prometheus.NewConstMetric(c.desc, c.valueType, s.Value, values...)
		if err != nil || !ok {
			if c.metrics != nil {
				c.metrics.CollectorError(c.name)
			}
			continue
		}
		ch <- m
	}
}
