package notifications

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/lichman0405/post/internal/events"
)

// Sender is the email digest sender (T1005, docs/18 §4): the process that
// turns the pending email deliveries the subscription fan-out writes (00078)
// into messages. It runs in the worker, next to the outbox dispatcher and
// the two fan-outs, and polls.
//
// One pass does four things, in this order:
//
//  1. withdraws the deliveries that have used up their attempts
//     (events.MaxEmailDeliveryAttempts), so a delivery the transport keeps
//     refusing leaves the queue with a recorded reason instead of blocking
//     its subscriber's head-of-line forever;
//
//  2. claims the due pending email rows under a lease
//     (DigestStore.ClaimEmailDeliveries) — due per the subscriber's cadence
//     (events.DigestDue, applied in the claim), leased so a send outside a
//     transaction cannot be started twice;
//
//  3. re-authorizes EVERY claimed delivery against live state, and
//     withdraws the ones that no longer pass. This is the send-time gate,
//     and it is the whole reason the sender exists next to the fan-out: the
//     fan-out decides at fan-out time and can only revisit a target when a
//     NEW event for it arrives, so a delivery queued for a subscriber whose
//     access is revoked in between would otherwise sit pending until it was
//     sent. A queued email is a copy of a notification that has not left
//     yet, and this is the last moment it can be stopped;
//
//  4. renders one digest per subscriber (Render) and hands it to the
//     transport (Mailer), then records what happened: rows that were in the
//     message become 'delivered', and the digest's send time becomes the
//     subscriber's next interval anchor.
//
// Failures are per subscriber, never per pass: one subscriber's broken
// delivery does not stop the others, and a transport failure leaves the rows
// pending under their lease so the next pass retries them (at-least-once —
// a crash after the send costs a duplicate email, never a lost one).
type Sender struct {
	store     DigestStore
	mailer    Mailer
	log       *slog.Logger
	batchSize int
	baseURL   string
	now       func() time.Time
}

// DefaultSenderBatchSize is how many pending email deliveries one pass
// claims.
const DefaultSenderBatchSize = 200

// DefaultSenderPollInterval is the pause between passes. "Immediate" means
// "on the next pass", so the interval is what immediate costs: five seconds
// is prompt enough to read as immediate and keeps the claim scan (one
// partial index over pending email rows) off the hot path.
const DefaultSenderPollInterval = 5 * time.Second

// SenderOption tunes a Sender.
type SenderOption func(*Sender)

// WithSenderLogger sets the logger (default slog.Default()).
func WithSenderLogger(log *slog.Logger) SenderOption {
	return func(s *Sender) { s.log = log }
}

// WithSenderBatchSize sets the rows one pass claims (default
// DefaultSenderBatchSize).
func WithSenderBatchSize(n int) SenderOption {
	return func(s *Sender) { s.batchSize = n }
}

// WithSenderBaseURL sets the web origin the digest's links are built on
// (POST_WEB_ORIGIN). Empty means links are omitted — never guessed.
func WithSenderBaseURL(u string) SenderOption {
	return func(s *Sender) { s.baseURL = u }
}

// WithSenderClock replaces the clock (tests; the interval anchor is
// stamped from it).
func WithSenderClock(now func() time.Time) SenderOption {
	return func(s *Sender) { s.now = now }
}

// NewSender builds the sender over the store port and the transport port.
func NewSender(store DigestStore, mailer Mailer, opts ...SenderOption) *Sender {
	s := &Sender{
		store:     store,
		mailer:    mailer,
		log:       slog.Default(),
		batchSize: DefaultSenderBatchSize,
		now:       time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Run sends until ctx is cancelled: one pass immediately, then one per
// DefaultSenderPollInterval. Transient failures never end the loop — a down
// database or a refused transport is retried like every other dependency
// outage; only ctx cancellation ends it.
func (s *Sender) Run(ctx context.Context) error {
	ticker := time.NewTicker(DefaultSenderPollInterval)
	defer ticker.Stop()
	for {
		if _, err := s.RunOnce(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("notifications: digest pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunOnce runs one pass and returns how many deliveries it marked delivered.
// The error is the joined failure of whatever did not work; deliveries that
// failed are still pending, so a non-nil error here is "retry me", not "the
// work is lost".
func (s *Sender) RunOnce(ctx context.Context) (int, error) {
	if s.mailer == nil {
		// Fail closed rather than pretend: without a transport nothing may
		// be marked delivered. The rows stay pending, so a deployment that
		// configures one later sends them.
		return 0, errors.New("notifications: digest sender has no mail transport")
	}

	withdrawn, err := s.store.WithdrawExhaustedEmailDeliveries(ctx, events.MaxEmailDeliveryAttempts)
	if err != nil {
		return 0, err
	}
	if withdrawn > 0 {
		s.log.Warn("notifications: withdrew email deliveries that exhausted their attempts",
			"withdrawn", withdrawn, "max_attempts", events.MaxEmailDeliveryAttempts)
	}

	claimed, err := s.store.ClaimEmailDeliveries(ctx, s.batchSize, events.MaxEmailDeliveryAttempts)
	if err != nil {
		return 0, err
	}
	if len(claimed) == 0 {
		return 0, nil
	}

	sent := 0
	var errs []error
	for _, group := range groupBySubscriber(claimed) {
		n, err := s.deliver(ctx, group)
		sent += n
		if err != nil {
			errs = append(errs, err)
		}
	}
	return sent, errors.Join(errs...)
}

// groupBySubscriber groups the claimed deliveries by subscriber, keeping the
// claim's order (oldest first) — one digest per subscriber per pass, and the
// groups in the order their first delivery was claimed.
func groupBySubscriber(claimed []events.EmailDelivery) [][]events.EmailDelivery {
	index := map[string]int{}
	groups := [][]events.EmailDelivery{}
	for _, d := range claimed {
		i, ok := index[d.UserID]
		if !ok {
			index[d.UserID] = len(groups)
			groups = append(groups, []events.EmailDelivery{d})
			continue
		}
		groups[i] = append(groups[i], d)
	}
	return groups
}

// deliver sends one subscriber's digest.
func (s *Sender) deliver(ctx context.Context, group []events.EmailDelivery) (int, error) {
	if len(group) == 0 {
		return 0, nil
	}
	userID, to := group[0].UserID, group[0].Email
	var errs []error

	items := make([]Item, 0, len(group))
	ids := make([]string, 0, len(group))
	for _, d := range group {
		if to == "" {
			// No address on the account: no transport could ever deliver
			// this, so the delivery is withdrawn rather than retried
			// forever. (The address is read per delivery from the same
			// claim, so this is the account's state, not one row's.)
			s.log.Warn("notifications: withdrawing an email delivery for an account with no address",
				"delivery_id", d.ID, "user_id", d.UserID)
			if err := s.store.WithdrawEmailDelivery(ctx, d.ID); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		gated, err := s.gate(ctx, d)
		if err != nil {
			// The gate could not be resolved: send nothing for this
			// delivery and leave it pending. "Could not check" is never
			// allowed to read as "allowed" (the fan-out's rule), and it is
			// not a withdrawal either — the next pass re-checks.
			errs = append(errs, fmt.Errorf("delivery %s: %w", d.ID, err))
			continue
		}
		switch gated.verdict {
		case verdictSkip:
			s.log.Info("notifications: delivery resolved elsewhere; nothing to send",
				"delivery_id", d.ID, "reason", gated.reason)
			continue
		case verdictWithdraw:
			s.log.Warn("notifications: withdrawing an undeliverable delivery",
				"delivery_id", d.ID, "user_id", d.UserID,
				"target_type", d.Target.Type, "reason", gated.reason)
			if err := s.store.WithdrawEmailDelivery(ctx, d.ID); err != nil {
				errs = append(errs, err)
			}
			continue
		}

		label, err := s.store.TargetLabel(ctx, d.Target)
		if err != nil {
			// The item is authorized; a label that could not be read is
			// only a worse rendering (the template names the target by its
			// identity instead). Losing the notification over it would be
			// the tail wagging the dog.
			s.log.Warn("notifications: reading the target label failed; rendering the target identity instead",
				"delivery_id", d.ID, "target_type", d.Target.Type, "error", err)
			label = ""
		}
		items = append(items, Item{
			EventType:  d.EventType,
			Target:     d.Target,
			Label:      label,
			OccurredAt: gated.occurredAt,
			Link:       s.link(d.Target),
		})
		ids = append(ids, d.ID)
	}

	if len(items) == 0 {
		// Everything was skipped or withdrawn: no message, and no interval
		// anchor either (nothing was sent).
		return 0, errors.Join(errs...)
	}

	mail := Render(Digest{To: to, Items: items, ManageURL: s.manageURL()})
	if err := s.mailer.Send(ctx, mail); err != nil {
		// The rows stay pending under their lease: the next pass retries
		// this subscriber. Nothing is marked delivered that was not sent.
		errs = append(errs, fmt.Errorf("send digest for user %s: %w", userID, err))
		return 0, errors.Join(errs...)
	}
	moved, err := s.store.MarkEmailDelivered(ctx, ids)
	if err != nil {
		errs = append(errs, err)
	} else if moved != len(ids) {
		// A row the owner withdrew (or another sender finished) between
		// the send and the mark stays as it is; the email has left, so the
		// difference is worth a line but is not an error.
		s.log.Warn("notifications: digest sent, but not every delivery could be marked",
			"user_id", userID, "carried", len(ids), "marked", moved)
	}
	if err := s.store.RecordDigestSent(ctx, userID, s.now()); err != nil {
		errs = append(errs, err)
	}
	s.log.Info("notifications: digest sent", "user_id", userID, "deliveries", len(ids))
	return moved, errors.Join(errs...)
}

// gateResult is the send-time gate's answer: what to do with the delivery,
// why (for the log line), and when the event behind it happened (for the
// rendered item).
type gateResult struct {
	verdict    gateVerdict
	reason     string
	occurredAt time.Time
}

// gateVerdict is what the send-time gate decided about one claimed delivery.
type gateVerdict int

const (
	// verdictSend: the delivery may be sent.
	verdictSend gateVerdict = iota
	// verdictSkip: nothing to do — the row is not pending any more (another
	// sender finished it, or it was withdrawn in the meantime).
	verdictSkip
	// verdictWithdraw: the delivery must be cancelled — the subscriber lost
	// the right to it, or unsubscribed, between the fan-out and now.
	verdictWithdraw
)

// gate decides whether one claimed delivery may still be sent, from the
// LIVE state read at this moment.
//
// The rule it applies is not its own: the audience comes from
// events.SubscriptionStore.TargetAudience (the one implementation the
// subscribe path and the fan-out share) and the decision from
// events.Delivers (the one predicate). What this function adds is WHEN:
// send time, per delivery, never cached — the same reason the fan-out
// re-resolves per event instead of remembering a level.
//
// A level that no longer permits the event's own visibility is a
// WITHDRAWAL, not a skip: the row would otherwise stay pending forever
// (the fan-out already ran) and its content — the target's label, its
// existence, the fact that something happened on it — is exactly what must
// not leave the building.
func (s *Sender) gate(ctx context.Context, d events.EmailDelivery) (gateResult, error) {
	state, err := s.store.EmailDeliveryState(ctx, d.ID)
	if err != nil {
		return gateResult{}, fmt.Errorf("read delivery state: %w", err)
	}
	if !state.Found {
		return gateResult{verdict: verdictSkip, reason: "the delivery row is gone"}, nil
	}
	if state.Status != events.SubscriptionDeliveryPending {
		return gateResult{
			verdict: verdictSkip,
			reason:  "the delivery is no longer pending (" + state.Status + ")",
		}, nil
	}
	if !state.SubscriptionLive {
		return gateResult{verdict: verdictWithdraw, reason: "the subscription was ended"}, nil
	}
	level, err := s.store.TargetAudienceFor(ctx, d.Target, d.UserID)
	if err != nil {
		return gateResult{}, fmt.Errorf("resolve the subscriber's audience: %w", err)
	}
	if !events.Delivers(level, state.EventVisibility) {
		return gateResult{
			verdict: verdictWithdraw,
			reason: fmt.Sprintf(
				"the subscriber's audience on the target (%s) no longer permits a %s event",
				level, state.EventVisibility),
		}, nil
	}
	return gateResult{verdict: verdictSend, occurredAt: state.OccurredAt}, nil
}

// link is the absolute URL of the target's page, or "" when there is no
// page (TargetPath) or no configured origin — a digest never contains a
// guessed URL.
func (s *Sender) link(t events.Target) string {
	path := TargetPath(t)
	if path == "" || s.baseURL == "" {
		return ""
	}
	return s.baseURL + path
}

// manageURL is where the recipient changes their cadence: the notifications
// page on the configured origin (empty when no origin is configured).
func (s *Sender) manageURL() string {
	if s.baseURL == "" {
		return ""
	}
	return s.baseURL + "/notifications"
}
