package events

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FanOut turns published research events into per-endpoint delivery rows
// (T1006): the bridge between the outbox publisher (T1001) and the HTTP
// deliverer. One pass claims a batch of outbox rows the dispatcher has
// published but no fan-out has consumed, inserts a delivery row for every
// matching enabled endpoint, and marks the outbox row fanned out — all in
// ONE transaction (each row under its own savepoint, so one broken row
// never stalls the batch), so a crash between the inserts and the mark
// re-runs the fan-out on the next pass and the partial unique index
// webhook_deliveries_endpoint_event_uniq turns the re-run into a no-op.
// Exactly-once fan-out per (endpoint, event) pair; at-least-once
// delivery from there.
//
// Delivery eligibility (V1, fail closed — docs/12 §3: an event is never
// more visible than its subject):
//
//   - ONLY public events fan out. A private event goes to no external
//     URL, period; the row is marked fanned (consumed) so the backlog
//     scan never re-reads it. Private-event webhooks are a subscription
//     product decision (T1002) and are deliberately not invented here.
//   - An endpoint's event_filters exact-match the event type; an empty
//     filter list receives everything (public).
//   - Disabled endpoints receive nothing (the disable policy's point).
type FanOut struct {
	pool      *pgxpool.Pool
	log       *slog.Logger
	batchSize int
}

// DefaultFanOutBatchSize is how many published outbox rows one RunOnce
// consumes.
const DefaultFanOutBatchSize = 100

// FanOutOption tunes a FanOut.
type FanOutOption func(*FanOut)

// WithFanOutLogger sets the logger (default slog.Default()).
func WithFanOutLogger(log *slog.Logger) FanOutOption {
	return func(f *FanOut) { f.log = log }
}

// WithFanOutBatchSize sets the rows one RunOnce claims (default
// DefaultFanOutBatchSize).
func WithFanOutBatchSize(n int) FanOutOption {
	return func(f *FanOut) { f.batchSize = n }
}

// NewFanOut builds the fan-out on pool. The pool may be lazy, like the
// dispatcher's.
func NewFanOut(pool *pgxpool.Pool, opts ...FanOutOption) *FanOut {
	f := &FanOut{pool: pool, log: slog.Default(), batchSize: DefaultFanOutBatchSize}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// fannedEvent is one claimed outbox row with its published research event.
type fannedEvent struct {
	outboxID   string
	eventID    string
	eventType  string
	visibility string
}

const claimFannableOutbox = `
SELECT re.id, re.event_type, re.visibility, oe.id
FROM outbox_events oe
JOIN research_events re ON re.outbox_event_id = oe.id
WHERE oe.published_at IS NOT NULL AND oe.webhook_fanned_out_at IS NULL
ORDER BY oe.created_at, oe.id
LIMIT $1
FOR UPDATE OF oe SKIP LOCKED`

const insertDeliveryRow = `
INSERT INTO webhook_deliveries (endpoint_id, endpoint, event_id, event_type)
SELECT we.id, we.url, $1, $2
FROM webhook_endpoints we
WHERE we.enabled
  AND (cardinality(we.event_filters) = 0 OR $2 = ANY(we.event_filters))
ON CONFLICT (endpoint_id, event_id) WHERE endpoint_id IS NOT NULL DO NOTHING`

const markOutboxFannedOut = `
UPDATE outbox_events SET webhook_fanned_out_at = now() WHERE id = $1`

// Run consumes the fan-out backlog until ctx is cancelled: one pass
// immediately, then one per DefaultPollInterval. Like the dispatcher, the
// loop never exits on a transient failure — a down database is retried;
// only ctx cancellation ends it.
func (f *FanOut) Run(ctx context.Context) error {
	ticker := time.NewTicker(DefaultPollInterval)
	defer ticker.Stop()
	for {
		if _, err := f.RunOnce(ctx); err != nil && ctx.Err() == nil {
			f.log.Error("events: fanout pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunOnce consumes one batch of fannable outbox rows and returns how many
// it marked fanned. Each row is processed inside its OWN savepoint: a
// broken row (e.g. a CHECK-violating poison delivery) rolls back to its
// savepoint and the batch continues — the failure is logged with the row
// identity, the row stays unmarked and is re-claimed (and re-fails
// observably) on the next pass, never silently dropped. The whole pass
// still commits in one transaction.
func (f *FanOut) RunOnce(ctx context.Context) (int, error) {
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("events: fanout: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	rows, err := tx.Query(ctx, claimFannableOutbox, f.batchSize)
	if err != nil {
		return 0, fmt.Errorf("events: fanout: claim: %w", err)
	}
	var claimed []fannedEvent
	for rows.Next() {
		var e fannedEvent
		if err := rows.Scan(&e.eventID, &e.eventType, &e.visibility, &e.outboxID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("events: fanout: scan: %w", err)
		}
		claimed = append(claimed, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("events: fanout: iterate: %w", err)
	}

	fanned, skipped := 0, 0
	for _, e := range claimed {
		log := f.log.With("outbox_id", e.outboxID, "event_id", e.eventID, "event_type", e.eventType)
		if fanOutOne(ctx, tx, e, log) {
			fanned++
		} else {
			skipped++
		}
	}
	if skipped > 0 {
		f.log.Warn("events: fanout: pass skipped broken rows", "skipped", skipped, "fanned", fanned)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("events: fanout: commit: %w", err)
	}
	return fanned, nil
}

// fanOutOne processes one claimed row inside its own savepoint: a broken
// row rolls back to the savepoint and the batch continues. Returns
// whether the row was marked fanned; a false return leaves the row
// unmarked so the next pass re-claims it — the failure was logged with
// the row identity, so a poison row re-fails loudly instead of being
// silently dropped (RunOnce's skipped summary adds the batch-level
// trace).
func fanOutOne(ctx context.Context, tx pgx.Tx, e fannedEvent, log *slog.Logger) bool {
	sp, err := tx.Begin(ctx)
	if err != nil {
		log.Error("events: fanout: savepoint begin failed; row stays unmarked for retry", "error", err)
		return false
	}
	defer func() { _ = sp.Rollback(context.WithoutCancel(ctx)) }() // no-op once sp.Commit released it

	if e.visibility != VisibilityPublic {
		// Private events are consumed but never delivered (V1 fail-closed
		// boundary, see the package comment).
		if _, err := sp.Exec(ctx, markOutboxFannedOut, e.outboxID); err != nil {
			log.Error("events: fanout: marking private event fanned failed; row stays unmarked for retry", "error", err)
			return false
		}
	} else {
		tag, err := sp.Exec(ctx, insertDeliveryRow, e.eventID, e.eventType)
		if err != nil {
			log.Error("events: fanout: inserting deliveries failed; row stays unmarked for retry", "error", err)
			return false
		}
		if _, err := sp.Exec(ctx, markOutboxFannedOut, e.outboxID); err != nil {
			log.Error("events: fanout: marking fanned failed; row stays unmarked for retry", "error", err)
			return false
		}
		log.Info("events: fanout: event fanned out to endpoints", "deliveries", tag.RowsAffected())
	}
	if err := sp.Commit(ctx); err != nil {
		log.Error("events: fanout: savepoint commit failed; row stays unmarked for retry", "error", err)
		return false
	}
	return true
}
