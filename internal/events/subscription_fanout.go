package events

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SubscriptionFanOut turns published research events into per-subscriber
// notifications (T1002): the bridge between the outbox publisher (T1001)
// and the channels that read delivery rows (the research inbox, T1003;
// the digest sender, T1005). One pass claims a batch of published outbox
// rows no subscription fan-out has consumed, resolves each event's
// targets, and for every live subscription on those targets makes TWO
// decisions:
//
//  1. Authorization, against CURRENT state. The subscriber's level on the
//     target is resolved from live rows at this moment (TargetAudience) —
//     not from anything remembered when they subscribed. A subscription
//     whose owner lost access produces nothing, and that is what stops a
//     private target's events from continuing to flow after a permission
//     change (the task's second acceptance criterion). It is also why the
//     level is re-resolved per event rather than cached per pass: the
//     whole point is that it tracks the database, not the subscription.
//
//  2. Delivery, per channel. A level that permits the event's own
//     visibility (Delivers) inserts one delivery row per channel; the
//     unique index makes a retried pass a no-op.
//
// A candidate whose level does not deliver everything is not merely
// skipped: the fan-out CANCELS the deliveries that subscription still has
// in flight for that target and that the new level no longer permits.
// Skipping alone would leave a queued email for a target its recipient can
// no longer see — a leak that happens later rather than one that does not
// happen — and "no longer permits" is per event visibility, so a member
// demoted to public keeps the queued public notifications and loses the
// queued private ones. Rows already delivered are left alone: they were
// delivered.
//
// Every row is processed inside its own savepoint, so one broken row (a
// poison payload, a target that cannot be resolved) rolls back to its
// savepoint, is logged with its identity, and is retried on the next pass
// without stalling the batch — the T1001 lesson, applied by the webhook
// fan-out first (webhook_fanned_out_at) and here (subscription_fanned_events).
type SubscriptionFanOut struct {
	pool      *pgxpool.Pool
	store     *SubscriptionStore
	log       *slog.Logger
	batchSize int
}

// DefaultSubscriptionFanOutBatchSize is how many published outbox rows one
// RunOnce consumes.
const DefaultSubscriptionFanOutBatchSize = 100

// SubscriptionFanOutOption tunes a SubscriptionFanOut.
type SubscriptionFanOutOption func(*SubscriptionFanOut)

// WithSubscriptionFanOutLogger sets the logger (default slog.Default()).
func WithSubscriptionFanOutLogger(log *slog.Logger) SubscriptionFanOutOption {
	return func(f *SubscriptionFanOut) { f.log = log }
}

// WithSubscriptionFanOutBatchSize sets the rows one RunOnce claims
// (default DefaultSubscriptionFanOutBatchSize).
func WithSubscriptionFanOutBatchSize(n int) SubscriptionFanOutOption {
	return func(f *SubscriptionFanOut) { f.batchSize = n }
}

// NewSubscriptionFanOut builds the fan-out on pool. The pool may be lazy,
// like the dispatcher's. The store is built here rather than injected so
// the authorization rule cannot be replaced by a caller.
func NewSubscriptionFanOut(pool *pgxpool.Pool, opts ...SubscriptionFanOutOption) *SubscriptionFanOut {
	f := &SubscriptionFanOut{
		pool:      pool,
		store:     NewSubscriptionStore(pool),
		log:       slog.Default(),
		batchSize: DefaultSubscriptionFanOutBatchSize,
	}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// fanEvent is one claimed outbox row with its published research event, as
// the fan-out needs it.
type fanEvent struct {
	outboxID      string
	eventID       string
	eventType     string
	actorID       *string
	projectID     *string
	visibility    string
	payload       []byte
	correlationID string
}

// Run consumes the fan-out backlog until ctx is cancelled: one pass
// immediately, then one per DefaultPollInterval. Like the dispatcher and
// the webhook fan-out, the loop never exits on a transient failure — a
// down database is retried; only ctx cancellation ends it.
func (f *SubscriptionFanOut) Run(ctx context.Context) error {
	ticker := time.NewTicker(DefaultPollInterval)
	defer ticker.Stop()
	for {
		if _, err := f.RunOnce(ctx); err != nil && ctx.Err() == nil {
			f.log.Error("events: subscription fanout pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunOnce consumes one batch of fannable outbox rows and returns how many
// it consumed (marked fanned). Each row is processed inside its OWN
// savepoint: a broken row rolls back to its savepoint and the batch
// continues, leaving the row unmarked so the failure is re-claimed and
// re-logged rather than silently dropped. The whole pass still commits in
// one transaction.
func (f *SubscriptionFanOut) RunOnce(ctx context.Context) (int, error) {
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("events: subscription fanout: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	claimed, err := f.claim(ctx, tx)
	if err != nil {
		return 0, err
	}

	fanned, skipped := 0, 0
	for _, ev := range claimed {
		log := f.log.With("outbox_id", ev.outboxID, "event_id", ev.eventID,
			"event_type", ev.eventType, "correlation_id", ev.correlationID)
		if f.fanOutOne(ctx, tx, ev, log) {
			fanned++
		} else {
			skipped++
		}
	}
	if skipped > 0 {
		f.log.Warn("events: subscription fanout: pass skipped broken rows",
			"skipped", skipped, "fanned", fanned)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("events: subscription fanout: commit: %w", err)
	}
	return fanned, nil
}

// claimSubscriptionFannable selects the published outbox rows this
// consumer has not consumed yet, locking them so a concurrent pass skips
// them (FOR UPDATE SKIP LOCKED — the dispatcher's and the webhook
// fan-out's pattern).
const claimSubscriptionFannable = `
SELECT oe.id, re.id, re.event_type, re.actor_id, re.project_id, re.visibility, re.payload, re.correlation_id
FROM outbox_events oe
JOIN research_events re ON re.outbox_event_id = oe.id
WHERE oe.published_at IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM subscription_fanned_events f WHERE f.outbox_event_id = oe.id)
ORDER BY oe.created_at, oe.id
LIMIT $1
FOR UPDATE OF oe SKIP LOCKED`

// markSubscriptionFanned writes the consumer's cursor row.
const markSubscriptionFanned = `
INSERT INTO subscription_fanned_events (outbox_event_id) VALUES ($1)
ON CONFLICT (outbox_event_id) DO NOTHING`

// cancelUndeliverableDeliveries withdraws the in-flight deliveries of one
// subscription for one target that the subscriber's level no longer
// permits — the revocation half of the authorization decision (see the
// type comment).
//
// $4 and $5 are the caller's answer to "does this level still deliver an
// event at PRIVATE / at PUBLIC visibility", computed by Delivers rather
// than restated here: the rule has one implementation, and this statement
// only applies it to the visibility of the event each pending row points
// at. That is what keeps a level that merely DROPPED from leaking too — a
// member demoted to public keeps their queued public notifications and
// loses the queued private ones, and only a relationship that vanished
// loses everything.
const cancelUndeliverableDeliveries = `
UPDATE subscription_deliveries d
SET status = 'cancelled', cancelled_at = now()
FROM research_events re
WHERE d.event_id = re.id
  AND d.subscription_id = $1 AND d.target_type = $2 AND d.target_id = $3
  AND d.status = 'pending'
  AND (($4 AND re.visibility <> 'public') OR $5)`

func (f *SubscriptionFanOut) claim(ctx context.Context, tx pgx.Tx) ([]fanEvent, error) {
	rows, err := tx.Query(ctx, claimSubscriptionFannable, f.batchSize)
	if err != nil {
		return nil, fmt.Errorf("events: subscription fanout: claim: %w", err)
	}
	defer rows.Close()
	var claimed []fanEvent
	for rows.Next() {
		var (
			oeID, reID pgtype.UUID
			actor      pgtype.UUID
			project    pgtype.UUID
			ev         fanEvent
		)
		if err := rows.Scan(&oeID, &reID, &ev.eventType, &actor, &project,
			&ev.visibility, &ev.payload, &ev.correlationID); err != nil {
			return nil, fmt.Errorf("events: subscription fanout: scan: %w", err)
		}
		ev.outboxID = uuidString(oeID)
		ev.eventID = uuidString(reID)
		ev.actorID = uuidPtr(actor)
		ev.projectID = uuidPtr(project)
		claimed = append(claimed, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events: subscription fanout: iterate: %w", err)
	}
	return claimed, nil
}

// fanOutOne processes one claimed row inside its own savepoint. It returns
// whether the row was marked fanned; a false return leaves it unmarked so
// the next pass re-claims it — the failure was logged with the row
// identity, so a poison row re-fails loudly instead of vanishing.
func (f *SubscriptionFanOut) fanOutOne(ctx context.Context, tx pgx.Tx, ev fanEvent, log *slog.Logger) bool {
	sp, err := tx.Begin(ctx)
	if err != nil {
		log.Error("events: subscription fanout: savepoint begin failed; row stays unmarked for retry", "error", err)
		return false
	}
	defer func() { _ = sp.Rollback(context.WithoutCancel(ctx)) }() // no-op once committed

	targets, err := f.targets(ctx, sp, ev)
	if err != nil {
		log.Error("events: subscription fanout: resolving targets failed; row stays unmarked for retry", "error", err)
		return false
	}

	delivered := 0
	for _, target := range targets {
		candidates, err := f.store.LiveSubscriptionsForTarget(ctx, sp, target)
		if err != nil {
			log.Error("events: subscription fanout: claiming subscriptions failed; row stays unmarked for retry",
				"error", err, "target_type", target.Type, "target_id", target.ID)
			return false
		}
		for _, sub := range candidates {
			level, err := f.store.TargetAudience(ctx, sp, target, sub.UserID)
			if err != nil {
				// An unresolvable audience is NOT treated as AudienceNone:
				// "no relationship" is a decision, "could not look" is not,
				// and silently treating a store failure as a revocation
				// would cancel deliveries on the strength of an error. The
				// whole row is retried instead.
				log.Error("events: subscription fanout: resolving audience failed; row stays unmarked for retry",
					"error", err, "subscription_id", sub.ID, "subscriber", sub.UserID,
					"target_type", target.Type, "target_id", target.ID)
				return false
			}
			// Withdraw whatever this level no longer permits BEFORE
			// deciding about the event in hand: the rows already queued
			// were queued under the level the subscriber had then, and a
			// level that dropped (a membership removed from a project
			// that is still public, an affiliation that ended) would
			// otherwise keep a private event's notification in flight
			// past the permission change. A member has nothing to
			// withdraw, which is why the statement is skipped for them.
			if level != AudienceMember {
				noPrivate := !Delivers(level, VisibilityPrivate)
				noPublic := !Delivers(level, VisibilityPublic)
				if _, err := sp.Exec(ctx, cancelUndeliverableDeliveries,
					sub.ID, target.Type, target.ID, noPrivate, noPublic); err != nil {
					log.Error("events: subscription fanout: withdrawing undeliverable deliveries failed; row stays unmarked for retry",
						"error", err, "subscription_id", sub.ID)
					return false
				}
			}
			if level == AudienceNone {
				// The subscriber may no longer see the target at all.
				log.Info("events: subscription fanout: subscriber no longer sees the target; deliveries withdrawn",
					"subscription_id", sub.ID, "subscriber", sub.UserID,
					"target_type", target.Type, "target_id", target.ID)
				continue
			}
			if !SubscribesTo(sub.EventFilters, ev.eventType) {
				continue
			}
			if !Delivers(level, ev.visibility) {
				continue
			}
			n, err := f.store.InsertDeliveries(ctx, sp, sub, ev.eventID, ev.eventType)
			if err != nil {
				log.Error("events: subscription fanout: inserting deliveries failed; row stays unmarked for retry",
					"error", err, "subscription_id", sub.ID)
				return false
			}
			delivered += n
		}
	}

	if _, err := sp.Exec(ctx, markSubscriptionFanned, ev.outboxID); err != nil {
		log.Error("events: subscription fanout: marking fanned failed; row stays unmarked for retry", "error", err)
		return false
	}
	if err := sp.Commit(ctx); err != nil {
		log.Error("events: subscription fanout: savepoint commit failed; row stays unmarked for retry", "error", err)
		return false
	}
	if delivered > 0 {
		log.Info("events: subscription fanout: event delivered to subscribers", "deliveries", delivered,
			"targets", len(targets))
	}
	return true
}

// targets resolves the subscription targets one event addresses:
// EventTargets' envelope+payload mapping, plus the organization that owns
// the event's project (which no producer writes into a payload, so it is
// read from the schema instead).
func (f *SubscriptionFanOut) targets(ctx context.Context, db DBTX, ev fanEvent) ([]Target, error) {
	targets := EventTargets(ev.eventType, deref(ev.actorID), deref(ev.projectID), ev.payload)
	if projectID := deref(ev.projectID); projectID != "" {
		orgID, err := f.store.ProjectOrganization(ctx, db, projectID)
		if err != nil {
			return nil, err
		}
		if orgID != "" {
			targets = append(targets, Target{Type: TargetTypeOrganization, ID: orgID})
		}
	}
	return targets, nil
}
