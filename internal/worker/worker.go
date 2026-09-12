// Package worker implements the POST background job loop (docs/52 §17,
// ADR-013): jobs are consumed from a Redis list queue with the
// reliable-queue pattern, dispatched by type, and completed with the
// idempotency/retry/backoff/dead-letter shape the architecture requires.
//
// Job payloads carry identity/version/reference only — never bulk sensitive
// data (docs/52 §17). Every job carries a correlation id so a job can be
// traced across API -> queue -> worker (docs/26 §2).
//
// Delivery semantics: at-least-once. The idempotency mark is written only
// after a job completes, so a redelivered job is skipped only when its
// previous attempt finished successfully; failed attempts retry until the
// cap and then dead-letter. Handlers must therefore be idempotent (the job
// ID is their idempotency key), which is exactly what ADR-013 requires of
// outbox consumers.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// Job is one queued unit of work. ID is the idempotency key (docs/52 §17):
// a job whose ID already completed is skipped, so retries and duplicate
// enqueues never double-execute.
type Job struct {
	ID            string          `json:"id"`
	Type          string          `json:"type"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload,omitempty"`
	Attempts      int             `json:"attempts"`
	Error         string          `json:"error,omitempty"` // set on dead-letter
}

// Handler processes one job type. Returning an error schedules a retry
// (until MaxAttempts, then the job is dead-lettered).
type Handler func(ctx context.Context, job Job) error

// Loop consumes jobs from a RedisQueue and dispatches them to registered
// handlers.
type Loop struct {
	queue       *RedisQueue
	handlers    map[string]Handler
	timeouts    map[string]time.Duration
	timeout     time.Duration
	maxAttempts int
	backoff     time.Duration
	backoffCap  time.Duration
	seenTTL     time.Duration
	log         *slog.Logger
}

// Defaults for the minimal V1 loop; retuned once the SLO work lands.
const (
	DefaultMaxAttempts = 3
	DefaultTimeout     = 30 * time.Second
	DefaultBackoff     = time.Second
	DefaultBackoffCap  = 30 * time.Second
	DefaultSeenTTL     = 24 * time.Hour
)

// NewLoop builds a job loop over queue.
func NewLoop(queue *RedisQueue, opts ...Option) *Loop {
	l := &Loop{
		queue:       queue,
		handlers:    map[string]Handler{},
		timeouts:    map[string]time.Duration{},
		timeout:     DefaultTimeout,
		maxAttempts: DefaultMaxAttempts,
		backoff:     DefaultBackoff,
		backoffCap:  DefaultBackoffCap,
		seenTTL:     DefaultSeenTTL,
		log:         slog.Default(),
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// Option tunes a Loop.
type Option func(*Loop)

// WithLogger sets the loop logger (default slog.Default()).
func WithLogger(log *slog.Logger) Option { return func(l *Loop) { l.log = log } }

// WithMaxAttempts sets the retry cap before dead-lettering (default 3).
func WithMaxAttempts(n int) Option { return func(l *Loop) { l.maxAttempts = n } }

// WithTimeout sets the default per-job timeout (default 30s).
func WithTimeout(d time.Duration) Option { return func(l *Loop) { l.timeout = d } }

// WithTypeTimeout sets the timeout for one job type (docs/52 §17: every job
// type has its own timeout).
func WithTypeTimeout(jobType string, d time.Duration) Option {
	return func(l *Loop) { l.timeouts[jobType] = d }
}

// Register wires one job type to its handler.
func (l *Loop) Register(jobType string, h Handler) {
	l.handlers[jobType] = h
}

// Run consumes jobs until ctx is cancelled. It starts by sweeping stale
// in-flight entries back onto the queue (a crash mid-processing must not
// lose a job), then polls forever. Redis outages are retried with a short
// pause — the loop never exits because a dependency is down.
func (l *Loop) Run(ctx context.Context) error {
	if err := l.queue.Recover(ctx); err != nil {
		l.log.Error("worker: processing-list recovery failed", "error", err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		job, raw, err := l.queue.Next(ctx)
		if err != nil {
			if errors.Is(err, redis.Nil) {
				continue // no job within the poll window; check ctx again
			}
			// Redis unreachable: keep the process alive and retry.
			l.log.Error("worker: queue read failed", "error", err)
			if !sleep(ctx, time.Second) {
				return nil
			}
			continue
		}
		l.process(ctx, job, raw)
	}
}

// process handles one dequeued job: completed IDs are skipped (idempotency),
// failures retry with capped exponential backoff, and jobs that exhaust
// their attempts are dead-lettered with the error recorded.
func (l *Loop) process(ctx context.Context, job Job, raw string) {
	log := l.log.With("job_id", job.ID, "job_type", job.Type,
		"correlation_id", job.CorrelationID, "attempt", job.Attempts+1)

	// Idempotency (docs/52 §17): the mark exists only for completed jobs,
	// so a redelivered completed job is acknowledged without re-execution
	// while a redelivered failed job runs again.
	seen, err := l.queue.IsSeen(ctx, job.ID)
	if err != nil {
		// Cannot verify idempotency: leave the job in the processing list
		// (Recover sweeps it back once Redis answers again) and pause.
		log.Error("worker: idempotency check failed; job stays in processing", "error", err)
		sleep(ctx, time.Second)
		return
	}
	if seen {
		log.Info("worker: duplicate job skipped (already completed)")
		l.ack(ctx, job, raw)
		return
	}

	handler, ok := l.handlers[job.Type]
	if !ok {
		log.Error("worker: unknown job type; dead-lettering")
		job.Attempts++
		job.Error = "unknown job type: no handler registered"
		l.deadLetter(ctx, job, raw)
		return
	}

	timeout := l.timeout
	if d, ok := l.timeouts[job.Type]; ok {
		timeout = d
	}
	jobCtx, cancel := context.WithTimeout(ctx, timeout)
	err = handler(jobCtx, job)
	cancel()

	if err == nil {
		if err := l.queue.MarkSeen(ctx, job.ID, l.seenTTL); err != nil {
			// The job completed; if the mark fails the entry may be
			// redelivered and re-executed — handlers are idempotent.
			log.Error("worker: idempotency mark failed after completion", "error", err)
		}
		log.Info("worker: job completed")
		l.ack(ctx, job, raw)
		return
	}
	// Attempts counts executions; the dead-letter entry records how many
	// times the job actually ran.
	job.Attempts++
	if job.Attempts >= l.maxAttempts {
		log.Error("worker: job failed permanently; dead-lettering", "error", err)
		job.Error = err.Error()
		l.deadLetter(ctx, job, raw)
		return
	}
	retryIn := l.backoffFor(job.Attempts)
	log.Warn("worker: job failed; scheduling retry", "error", err, "retry_in", retryIn)
	if !sleep(ctx, retryIn) {
		// Shutting down: put the job back untouched so it is not lost.
		l.requeue(ctx, job, raw)
		return
	}
	l.requeue(ctx, job, raw)
}

// backoffFor is the capped exponential retry delay for the next attempt.
func (l *Loop) backoffFor(attempt int) time.Duration {
	d := l.backoff
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= l.backoffCap {
			return l.backoffCap
		}
	}
	return d
}

// requeue puts the job back on the queue and removes the in-flight copy.
// The in-flight copy is removed on every terminal path, so the processing
// list holds only jobs actually being worked on.
func (l *Loop) requeue(ctx context.Context, job Job, raw string) {
	if err := l.queue.Enqueue(ctx, job); err != nil {
		l.log.Error("worker: requeue failed (job stays in processing)",
			"job_id", job.ID, "error", err)
		return
	}
	l.ack(ctx, job, raw)
}

// deadLetter parks a permanently failed job with its error recorded.
func (l *Loop) deadLetter(ctx context.Context, job Job, raw string) {
	if err := l.queue.DeadLetter(ctx, job); err != nil {
		l.log.Error("worker: dead-letter failed", "job_id", job.ID, "error", err)
	}
	l.ack(ctx, job, raw)
}

// ack removes the in-flight copy from the processing list; a transient
// failure here is logged but the job is never lost — it stays in the
// processing list and Recover sweeps it back.
func (l *Loop) ack(ctx context.Context, job Job, raw string) {
	if err := l.queue.Ack(ctx, raw); err != nil {
		l.log.Error("worker: ack failed (job stays in processing)",
			"job_id", job.ID, "error", err)
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
