package events

import (
	"context"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Dispatcher publishes pending outbox rows into research_events (the
// cmd/worker side of the transactional outbox, ADR-013): it claims a batch
// of unpublished rows, inserts the research event for each and marks the
// outbox row published. The insert carries the outbox row's id and is
// guarded by the partial unique index research_events_outbox_event_uniq,
// so the publish step is idempotent — a retry after a crash can never
// duplicate an event, and a crash before the mark only re-runs the same
// idempotent insert (at-least-once delivery, exactly-once effect).
type Dispatcher struct {
	pool      *pgxpool.Pool
	log       *slog.Logger
	batchSize int
	// publish is the per-row publish step, replaceable in tests to force a
	// deterministic failure (the savepoint isolation keeps one broken row
	// from blocking the rest of the batch).
	publish func(ctx context.Context, tx pgx.Tx, p PendingEvent) error
}

// DefaultBatchSize is how many pending outbox rows one RunOnce claims.
const DefaultBatchSize = 100

// DefaultPollInterval is the pause between RunOnce passes in Run. The
// outbox is a low-latency path (the event should exist shortly after the
// transaction commits) but not a hot loop; the next task's SLO work may
// tune this per deployment.
const DefaultPollInterval = time.Second

// DispatcherOption tunes a Dispatcher.
type DispatcherOption func(*Dispatcher)

// WithLogger sets the dispatcher logger (default slog.Default()).
func WithLogger(log *slog.Logger) DispatcherOption {
	return func(d *Dispatcher) { d.log = log }
}

// WithBatchSize sets the rows one RunOnce claims (default
// DefaultBatchSize).
func WithBatchSize(n int) DispatcherOption {
	return func(d *Dispatcher) { d.batchSize = n }
}

// WithPublisher replaces the per-row publish step. The default is
// PublishEvent; the seam exists so tests can force one row to fail
// deterministically (the savepoint isolation keeps that row from blocking
// the rest of the batch) — production callers should not replace it.
func WithPublisher(publish func(ctx context.Context, tx pgx.Tx, p PendingEvent) error) DispatcherOption {
	return func(d *Dispatcher) { d.publish = publish }
}

// NewDispatcher builds the outbox publisher on pool. The pool may be lazy
// (persistence.OpenLazy): the dispatcher retries forever while PostgreSQL
// is down, like every other worker dependency outage.
func NewDispatcher(pool *pgxpool.Pool, opts ...DispatcherOption) *Dispatcher {
	d := &Dispatcher{
		pool:      pool,
		log:       slog.Default(),
		batchSize: DefaultBatchSize,
		publish:   PublishEvent,
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// PendingEvent is one claimed outbox row, as the publish step needs it.
type PendingEvent struct {
	OutboxID      string
	EventType     string
	ActorID       *string
	ProjectID     *string
	Visibility    string
	Payload       []byte
	CorrelationID string
	OccurredAt    time.Time
}

// Run publishes the outbox backlog until ctx is cancelled: one pass
// immediately (a backlog built while the worker was down drains right
// away), then one pass per DefaultPollInterval. The loop never exits on a
// transient failure — a down database is retried like every other
// dependency outage; only ctx cancellation ends it.
func (d *Dispatcher) Run(ctx context.Context) error {
	ticker := time.NewTicker(DefaultPollInterval)
	defer ticker.Stop()
	for {
		if _, err := d.RunOnce(ctx); err != nil && ctx.Err() == nil {
			d.log.Error("events: outbox publish pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunOnce claims one batch of pending outbox rows and publishes each, all
// inside ONE transaction: a crash mid-batch rolls everything back and the
// rows are retried on the next pass — and the retry is a no-op for rows
// already published, so no event is lost and none is duplicated.
//
// Each row publishes inside its own savepoint (pgx nested transaction):
// one broken row records its failure (attempts/last_error) and is retried
// on the next pass without blocking the rest of the batch — a poison row
// must not stall the whole backlog. The claim uses FOR UPDATE SKIP LOCKED,
// so concurrent dispatcher passes never process the same row twice.
//
// It returns the number of rows marked published.
func (d *Dispatcher) RunOnce(ctx context.Context) (int, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("events: begin publish transaction: %w", err)
	}
	// The commit below is the only non-rollback exit; a deferred rollback
	// after commit is a no-op. Rollback runs on a non-cancelled context so
	// a caller cancelling ctx cannot abort the rollback itself.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	pending, err := claimPending(ctx, tx, d.batchSize)
	if err != nil {
		return 0, fmt.Errorf("events: claim pending outbox rows: %w", err)
	}

	published := 0
	for _, p := range pending {
		log := d.log.With("outbox_id", p.OutboxID, "event_type", p.EventType,
			"correlation_id", p.CorrelationID)
		if err := d.publish(ctx, tx, p); err != nil {
			// Record the failure on the row (attempts, last_error) and keep
			// the batch moving; the row stays pending and is retried on
			// the next pass — no event is lost, and the error is the
			// operator's window onto a stuck row (docs/26 §2).
			log.Error("events: outbox publish failed; row stays pending for retry", "error", err)
			if rerr := recordPublishError(ctx, tx, p.OutboxID, err); rerr != nil {
				log.Error("events: recording publish failure failed", "error", rerr)
			}
			continue
		}
		published++
		log.Info("events: outbox event published")
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("events: commit publish transaction: %w", err)
	}
	return published, nil
}

const claimPendingOutbox = `
SELECT id, event_type, actor_id, project_id, visibility, payload, correlation_id, created_at
FROM outbox_events
WHERE published_at IS NULL
ORDER BY created_at, id
LIMIT $1
FOR UPDATE SKIP LOCKED`

// claimPending reads the next batch of unpublished rows, oldest first,
// locking them so a concurrent dispatcher skips them.
func claimPending(ctx context.Context, tx pgx.Tx, batch int) ([]PendingEvent, error) {
	rows, err := tx.Query(ctx, claimPendingOutbox, batch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pending []PendingEvent
	for rows.Next() {
		var (
			id  pgtype.UUID
			act pgtype.UUID
			pro pgtype.UUID
			p   PendingEvent
		)
		if err := rows.Scan(&id, &p.EventType, &act, &pro, &p.Visibility, &p.Payload, &p.CorrelationID, &p.OccurredAt); err != nil {
			return nil, err
		}
		p.OutboxID = uuidString(id)
		p.ActorID = uuidPtr(act)
		p.ProjectID = uuidPtr(pro)
		pending = append(pending, p)
	}
	return pending, rows.Err()
}

const insertResearchEvent = `
INSERT INTO research_events (event_type, actor_id, project_id, visibility, payload, correlation_id, occurred_at, outbox_event_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (outbox_event_id) WHERE outbox_event_id IS NOT NULL DO NOTHING`

const markOutboxPublished = `
UPDATE outbox_events
SET published_at = now(), attempts = attempts + 1, last_error = NULL
WHERE id = $1`

const recordPublishFailure = `
UPDATE outbox_events
SET attempts = attempts + 1, last_error = $2
WHERE id = $1`

// PublishEvent publishes one claimed outbox row inside its own savepoint:
// the research event insert (idempotent — a previous attempt's row makes
// it a no-op) and the published-mark either both land or both roll back,
// so a crash between the two leaves the row pending for a retry that
// cannot duplicate the event. The savepoint keeps the caller's outer
// transaction usable after a failure (one broken row must not abort the
// batch).
func PublishEvent(ctx context.Context, tx pgx.Tx, p PendingEvent) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	// A rolled-back savepoint (publish failure) must not abort the outer
	// transaction; pgx nested transactions handle that — the outer tx
	// stays usable after sp.Rollback.
	if _, err := sp.Exec(ctx, insertResearchEvent,
		p.EventType, nullableText(deref(p.ActorID)), nullableText(deref(p.ProjectID)),
		p.Visibility, p.Payload, p.CorrelationID, p.OccurredAt, p.OutboxID); err != nil {
		_ = sp.Rollback(ctx)
		return fmt.Errorf("insert research event: %w", err)
	}
	if _, err := sp.Exec(ctx, markOutboxPublished, p.OutboxID); err != nil {
		_ = sp.Rollback(ctx)
		return fmt.Errorf("mark outbox row published: %w", err)
	}
	return sp.Commit(ctx)
}

// recordPublishError bumps the row's attempts and pins the failure
// reason. Bounded: a hostile error chain must not store unbounded text.
const maxErrorLen = 2000

func recordPublishError(ctx context.Context, tx pgx.Tx, outboxID string, err error) error {
	msg := truncateError(err.Error(), maxErrorLen)
	_, execErr := tx.Exec(ctx, recordPublishFailure, outboxID, msg)
	return execErr
}

// truncateError bounds msg to max runes, never cutting a multi-byte
// character in half: last_error is read by humans and tools, and invalid
// UTF-8 is neither (T1001 review — the previous byte-slice truncation
// could leave a broken trailing rune).
func truncateError(msg string, max int) string {
	if utf8.RuneCountInString(msg) <= max {
		return msg
	}
	runes := []rune(msg)
	return string(runes[:max])
}

func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", u.Bytes[0:4], u.Bytes[4:6], u.Bytes[6:8], u.Bytes[8:10], u.Bytes[10:16])
}

func uuidPtr(u pgtype.UUID) *string {
	if !u.Valid {
		return nil
	}
	s := uuidString(u)
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
