package main

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Measurement is one operation's observed cost over one corpus.
//
// It carries the sample COUNT next to every percentile on purpose. A p95 of 3
// samples is the maximum with a decimal point, and a report that printed
// "p95 420ms" without saying it came from 3 calls would be inviting the reader
// to compare noise against a budget. The struct is serialized into the report
// as it stands, so the count cannot get separated from the number.
type Measurement struct {
	// Samples is how many timed calls the percentiles were computed over.
	Samples int
	// Errors counts timed calls that returned an error. A measurement with
	// errors is not a measurement: the harness refuses to print percentiles
	// over a set that contains failures (see measure).
	Errors int
	// FirstError is the first error observed, verbatim.
	FirstError string

	Min time.Duration
	P50 time.Duration
	P95 time.Duration
	P99 time.Duration
	Max time.Duration

	// Cold is one call on a workload wired fresh over a new pool — plan cache,
	// buffer cache and connection all cold. One observation, so it is reported
	// as an observation and never as a percentile.
	Cold    time.Duration
	ColdErr string

	// Statements are the statements the LAST timed call issued, captured by
	// the pool's tracer. They are what the gate EXPLAINs, and they are also
	// the evidence that the operation really ran the query it claims to: an
	// operation that silently read nothing shows an empty or unexpected list
	// here.
	Statements []CapturedStatement
}

// measure runs one operation: warmup, one cold sample, then the timed samples.
//
// # Failures are not samples
//
// A timed call that returns an error aborts the measurement. The alternative —
// counting a 5ms failed call as a sample — would let an operation that is
// broken look faster than one that works, which is the single most misleading
// number this harness could print. The error is carried out and the report
// shows the operation as unmeasured with its error.
func measure(ctx context.Context, w *Workload, op *Operation) (Measurement, error) {
	var m Measurement

	for i := 0; i < op.Warmup; i++ {
		if err := op.Run(ctx); err != nil {
			return m, fmt.Errorf("%s: warmup call %d: %w", op.Name, i+1, err)
		}
	}

	if cold, err := coldSample(ctx, w, op); err != nil {
		m.ColdErr = err.Error()
	} else {
		m.Cold = cold
	}

	// A cold sample of a WRITING operation has just appended a version through
	// the cold workload's own counter, so this workload's counter is now one
	// behind the object and the first timed call would fail the optimistic
	// concurrency check instead of being timed. Rebasing is a read that happens
	// outside every timed region.
	if op.Writes {
		if err := w.RebaseWriteVersion(ctx); err != nil {
			return m, err
		}
	}

	samples := make([]time.Duration, 0, op.Samples)
	for i := 0; i < op.Samples; i++ {
		w.tracer.reset()
		start := time.Now()
		err := op.Run(ctx)
		elapsed := time.Since(start)
		if err != nil {
			m.Errors++
			if m.FirstError == "" {
				m.FirstError = err.Error()
			}
			return m, fmt.Errorf("%s: timed call %d of %d: %w", op.Name, i+1, op.Samples, err)
		}
		samples = append(samples, elapsed)
	}
	m.Samples = len(samples)
	m.Statements = w.tracer.snapshot()

	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	m.Min = sorted[0]
	m.P50 = percentile(sorted, 0.50)
	m.P95 = percentile(sorted, 0.95)
	m.P99 = percentile(sorted, 0.99)
	m.Max = sorted[len(sorted)-1]
	return m, nil
}

// coldSample times one call on a workload built from scratch: a new pool, new
// service graph, new prepared-statement cache. It is what the first request
// after a deploy costs, and it is deliberately NOT part of the percentile set —
// mixing one cold call into 50 hot ones would produce a p95 that depends on
// where the cold call landed in the order.
func coldSample(ctx context.Context, w *Workload, op *Operation) (time.Duration, error) {
	cold, err := openWorkload(ctx, w.benchURL, w.Corpus)
	if err != nil {
		return 0, fmt.Errorf("wire a cold workload: %w", err)
	}
	defer cold.Close()
	// Resolve the same operation on the cold workload: Run closes over the
	// workload it was built on, so the cold call has to run the one built here.
	coldOp := findOperation(cold.operations(), op.Name)
	if coldOp == nil {
		coldOp = findOperation(cold.probes(), op.Name)
	}
	if coldOp == nil {
		return 0, fmt.Errorf("no operation named %q on a cold workload", op.Name)
	}
	start := time.Now()
	if err := coldOp.Run(ctx); err != nil {
		return 0, err
	}
	return time.Since(start), nil
}

func findOperation(ops []*Operation, name string) *Operation {
	for _, op := range ops {
		if op.Name == name {
			return op
		}
	}
	return nil
}

// percentile returns the nearest-rank percentile of an ascending slice: the
// smallest value at or above which p of the samples lie.
//
// Nearest-rank rather than an interpolated estimator, because every number this
// harness prints can then be pointed at a real observed call. An interpolated
// p95 can be a duration no call ever took, which is a strange thing to hand
// somebody as evidence.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(float64(len(sorted))*p + 0.5)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}
