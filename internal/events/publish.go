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

	"github.com/lichman0405/post/internal/observability"
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
	// poll is the pause between successful passes; backoffMin/backoffMax
	// bound the failure path. All three are options so the tuning surface
	// is uniform — before T1109 batch size had a With* and the interval
	// did not (T1001 review item 5).
	poll       time.Duration
	backoffMin time.Duration
	backoffMax time.Duration
	// publish is the per-row publish step, replaceable in tests to force a
	// deterministic failure (the savepoint isolation keeps one broken row
	// from blocking the rest of the batch).
	publish func(ctx context.Context, tx pgx.Tx, p PendingEvent) error
}

// DefaultBatchSize is how many pending outbox rows one RunOnce claims.
const DefaultBatchSize = 100

// DefaultPollInterval is the pause between RunOnce passes in Run while the
// passes are succeeding. The outbox is a low-latency path (the event should
// exist shortly after the transaction commits) but not a hot loop.
const DefaultPollInterval = time.Second

// The retry ladder used while passes are FAILING. Before T1109 the loop
// logged one ERROR per second for as long as PostgreSQL was down — a
// database outage of an hour produced 3600 identical lines and told an
// operator nothing the first line had not (tasks/decisions.md L1-20260914-73,
// T1001 review items 2 and 5). The failure path now backs off from
// DefaultBackoffMin, doubling to DefaultBackoffMax, and resets to the
// normal cadence on the first successful pass — so recovery is immediate
// and an outage is legible: the gap between lines IS the retry rhythm.
//
// The values are the ones internal/worker already uses for job retries
// (worker.go backoff/backoffCap), so the platform has one retry vocabulary
// rather than two.
const (
	// DefaultBackoffMin is the pause after the first failed pass.
	DefaultBackoffMin = time.Second
	// DefaultBackoffMax caps the failure-path pause.
	DefaultBackoffMax = 30 * time.Second
)

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

// WithPollInterval sets the pause between successful RunOnce passes
// (default DefaultPollInterval). Non-positive values are ignored, like
// worker.RedisQueue.WithPollTimeout and the other tuning options.
func WithPollInterval(d time.Duration) DispatcherOption {
	return func(dis *Dispatcher) {
		if d > 0 {
			dis.poll = d
		}
	}
}

// WithBackoff sets the failure-path retry ladder's first step and cap
// (defaults DefaultBackoffMin / DefaultBackoffMax). Non-positive values are
// ignored, like WithPollInterval; the two are resolved together, against the
// values already in place, so the ladder this installs never descends: the
// pause may grow as an outage continues, never shrink.
//
// A cap below the first step therefore RAISES the cap to the first step. Both
// other readings are wrong. Lowering the first step would silently discard the
// knob the caller chose. Leaving a cap under the first step is what the
// previous version did, and it inverted the ladder: WithBackoff(60s, 45s) set
// min=60s, then refused 45s (it is below the new min) and kept the DEFAULT cap
// of 30s — so Run, which waits on the current step and only then climbs, spent
// 60s, 30s, 30s, 30s… with the pause collapsing the moment the outage proved
// serious. TestWithBackoffNeverInstallsADescendingLadder is the assertion.
func WithBackoff(min, max time.Duration) DispatcherOption {
	return func(dis *Dispatcher) {
		nextMin := dis.backoffMin
		if min > 0 {
			nextMin = min
		}
		nextMax := dis.backoffMax
		if max > 0 && max >= nextMin {
			nextMax = max
		}
		if nextMax < nextMin {
			nextMax = nextMin
		}
		dis.backoffMin, dis.backoffMax = nextMin, nextMax
	}
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
		pool:       pool,
		log:        slog.Default(),
		batchSize:  DefaultBatchSize,
		poll:       DefaultPollInterval,
		backoffMin: DefaultBackoffMin,
		backoffMax: DefaultBackoffMax,
		publish:    PublishEvent,
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
	// Via is the channel the write path recorded on the outbox row
	// (domain.StateVia's vocabulary), nil when it recorded none. The
	// publish step COPIES it into the research event; it is never
	// re-derived from the payload — the envelope rule 00046 states for the
	// other envelope columns, which exists because a payload is
	// producer-written data about the event while an envelope column is
	// what the write path recorded.
	Via        *string
	OccurredAt time.Time
}

// Run publishes the outbox backlog until ctx is cancelled: one pass
// immediately (a backlog built while the worker was down drains right
// away), then one pass per poll interval while passes succeed. The loop
// never exits on a transient failure — a down database is retried like
// every other dependency outage; only ctx cancellation ends it.
//
// Failures back off instead of repeating at the poll interval: see the
// DefaultBackoffMin comment. The pause is per-attempt and resets on the
// first success, so the loop returns to the low-latency cadence the moment
// the database answers again.
func (d *Dispatcher) Run(ctx context.Context) error {
	backoff := d.backoffMin
	for {
		_, err := d.RunOnce(ctx)
		wait := d.poll
		if err != nil && ctx.Err() == nil {
			d.log.Error("events: outbox publish pass failed",
				"error", err, "retry_in", backoff)
			// The counter beside the log line (docs/26 §3 outbox lag): the
			// failure half of the family. It is also what the database
			// alert reads, because a database that cannot be reached is
			// exactly this loop failing every pass.
			observability.Default().ObserveOutboxPublishFailure()
			wait = backoff
			backoff = nextBackoff(backoff, d.backoffMax)
		} else {
			backoff = d.backoffMin
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// nextBackoff doubles current, capped at max. A non-positive max means
// uncapped doubling, which WithBackoff never produces (the defaults always
// apply) but a zero-value Dispatcher built by hand could.
func nextBackoff(current, max time.Duration) time.Duration {
	next := current * 2
	if max > 0 && next > max {
		return max
	}
	return next
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

// OutboxBacklog is the outbox as the metric endpoint reports it.
type OutboxBacklog struct {
	// Pending is how many rows are still unpublished.
	Pending int64
	// OldestPending is how long the oldest unpublished row has been waiting
	// — the LAG docs/26 §3 names, as opposed to the depth. Zero when
	// nothing is pending. A depth says "there is work"; the age says
	// whether it is moving, which is the difference between a busy outbox
	// and a stuck one.
	OldestPending time.Duration
}

// The backlog probe. It is a read-only aggregate over the same partial index
// the claim uses (outbox_events_pending_idx, 00046), so it costs a count
// over exactly the rows the dispatcher is working on. Nothing here writes or
// locks: a metric read must never perturb the thing it measures, and the
// claim's FOR UPDATE SKIP LOCKED is the only lock on this path.
const outboxBacklogQuery = `
SELECT count(*),
       COALESCE(EXTRACT(EPOCH FROM now() - min(created_at)), 0)
FROM outbox_events
WHERE published_at IS NULL`

// Backlog reads the current outbox lag. Errors are returned, not swallowed:
// a database that cannot be reached must not be reported as an empty outbox.
func (d *Dispatcher) Backlog(ctx context.Context) (OutboxBacklog, error) {
	var (
		pending int64
		oldest  float64
	)
	if err := d.pool.QueryRow(ctx, outboxBacklogQuery).Scan(&pending, &oldest); err != nil {
		return OutboxBacklog{}, fmt.Errorf("events: read outbox backlog: %w", err)
	}
	return OutboxBacklog{
		Pending:       pending,
		OldestPending: time.Duration(oldest * float64(time.Second)),
	}, nil
}

const claimPendingOutbox = `
SELECT id, event_type, actor_id, project_id, visibility, payload, correlation_id, via, created_at
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
			via *string
			p   PendingEvent
		)
		if err := rows.Scan(&id, &p.EventType, &act, &pro, &p.Visibility, &p.Payload, &p.CorrelationID, &via, &p.OccurredAt); err != nil {
			return nil, err
		}
		p.Via = via
		p.OutboxID = uuidString(id)
		p.ActorID = uuidPtr(act)
		p.ProjectID = uuidPtr(pro)
		pending = append(pending, p)
	}
	return pending, rows.Err()
}

// via is appended last, beside outbox_event_id: the envelope columns keep
// their positions and the new one is obviously the copy of the outbox
// row's own via ($9).
const insertResearchEvent = `
INSERT INTO research_events (event_type, actor_id, project_id, visibility, payload, correlation_id, occurred_at, outbox_event_id, via)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
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
// every envelope column — actor, project, visibility, correlation id and
// the channel (via) — is copied from the outbox row, never re-derived from
// the payload (00046). The research event insert (idempotent — a previous
// attempt's row makes it a no-op) and the published-mark either both land or both roll back,
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
		p.Visibility, p.Payload, p.CorrelationID, p.OccurredAt, p.OutboxID,
		nullableText(deref(p.Via))); err != nil {
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
