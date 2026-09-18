package events

import (
	"fmt"
	"time"
)

// The email notification model (T1005, docs/18 §4: "Email immediate/digest
// abstraction"; docs/20 §9 lists "notification digest" among the worker's
// jobs). A subscription decides WHICH channels an event travels over (00078:
// subscription_deliveries rows with channel = 'email' are born 'pending' for
// exactly this sender); this file owns the two things that decide WHEN the
// email leaves: the account's cadence, and the pure rule for whether a
// digest of that cadence is due.
//
// It sits in the events package, next to the subscription model it extends
// and the Delivers rule the send and the fan-out share, because all of them
// are one authorization vocabulary: a cadence is a property of the same
// pipeline that decides what may be delivered at all.

// Cadence is how often ONE account receives its email notifications.
const (
	// CadenceImmediate: every event on its own, as soon as it is fanned
	// out. The sender polls, so "immediate" means the next pass, not a
	// push.
	CadenceImmediate = "immediate"
	// CadenceDaily: the events of a day in one digest.
	CadenceDaily = "daily"
	// CadenceWeekly: the events of a week in one digest.
	CadenceWeekly = "weekly"
)

// DefaultCadence is what an account that has never set a cadence gets.
//
// Daily, not immediate, and the reason is the one docs/18 §3 states for the
// whole notification surface — "默认避免通知每个低级 commit": email is the
// channel that leaves the building, and a platform whose default is one
// message per research event trains its users to filter it out. A
// subscriber who wants each event on its own asks for it (immediate); the
// default is the calm one.
const DefaultCadence = CadenceDaily

// The digest sender's retry policy (see NotificationStore.
// ClaimEmailDeliveries). MaxEmailDeliveryAttempts bounds how often one
// delivery row may be claimed before it is withdrawn: an email that cannot
// be handed to the transport after this many tries is withdrawn with the
// reason recorded rather than retried forever, and the row stops occupying
// the head of its subscriber's queue.
const (
	MaxEmailDeliveryAttempts = 5
	// EmailDeliveryLease is the base claim lease: how long a claimed row
	// stays unclaimable by another sender. It must comfortably exceed one
	// transport submission (the claim is committed before the send, so the
	// lease is the send's budget, not its duration).
	EmailDeliveryLease = 5 * time.Minute
	// EmailDeliveryLeaseCap is the ceiling the lease backs off to, so a
	// failing delivery is retried at a bounded rate instead of hot-looping.
	EmailDeliveryLeaseCap = time.Hour
)

// NotificationPreferences is one account's email cadence and the state the
// sender's schedule needs. A missing row is NOT "no email": it means
// DefaultCadence (see ValidateCadence and the migration's comment), so
// every account has exactly one defined behaviour.
type NotificationPreferences struct {
	UserID string
	// Cadence is one of CadenceImmediate / CadenceDaily / CadenceWeekly.
	Cadence string
	// LastDigestAt is when this account's last digest was handed to the
	// transport (nil = none sent yet, which is due immediately).
	LastDigestAt *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
	// Stored reports whether a row exists. Absence is a normal state (the
	// account never opened the setting), not an error.
	Stored bool
}

// EffectiveCadence is the cadence the pipeline acts on: the stored one, or
// DefaultCadence when no row exists. Every caller that decides something
// with a cadence reads it through here rather than falling back itself, so
// there is one answer to "what does an account without a preference row
// get".
func (p NotificationPreferences) EffectiveCadence() string {
	if p.Cadence == "" {
		return DefaultCadence
	}
	return p.Cadence
}

// ValidCadence reports whether c is one of the three cadences.
func ValidCadence(c string) bool {
	switch c {
	case CadenceImmediate, CadenceDaily, CadenceWeekly:
		return true
	}
	return false
}

// ValidateCadence checks a cadence a caller supplied. The same three values
// are a CHECK on notification_preferences.cadence — that is the second
// line, for a session that writes SQL directly (the split 00059 and 00078
// use for their vocabularies).
func ValidateCadence(c string) error {
	if !ValidCadence(c) {
		return fmt.Errorf("events: unknown email cadence %q", c)
	}
	return nil
}

// DigestWindow is the minimum interval between two digests of a cadence.
// Immediate has no window: every event is its own digest, which is why the
// sender treats it as always due.
func DigestWindow(cadence string) time.Duration {
	switch cadence {
	case CadenceDaily:
		return 24 * time.Hour
	case CadenceWeekly:
		return 7 * 24 * time.Hour
	default: // immediate, and anything ValidateCadence would have refused
		return 0
	}
}

// DigestDue reports whether a subscriber with this cadence and this last
// digest time should receive a digest at now.
//
// An account that has never had a digest (last == nil) is due, whatever the
// cadence: waiting a full interval before the FIRST digest would mean a new
// daily subscriber hears nothing for a day. After that, the anchor is the
// last actual send, so a delayed digest does not push the next one out.
//
// The live claim (NotificationStore.ClaimEmailDeliveries) applies the same
// rule in SQL — it has to, because it is what decides which rows are
// claimed — and it is parameterized with the windows from THIS function, so
// the durations have one source.
func DigestDue(cadence string, last *time.Time, now time.Time) bool {
	window := DigestWindow(cadence)
	if window <= 0 {
		return true
	}
	if last == nil {
		return true
	}
	return !now.Before(last.Add(window))
}

// EmailDelivery is one claimed pending email row, as the digest sender
// needs it — IDENTITY ONLY. It deliberately does not carry the event's
// visibility, the target's audience or the subscription's liveness: those
// are the state the send-time gate re-reads (EmailDeliveryState), and a
// value carried from claim time would be a snapshot the gate must then
// remember to distrust. Past its ID the claim is a promise that the row is
// this sender's to resolve, not a statement about whether it may be sent.
type EmailDelivery struct {
	ID             string
	UserID         string
	SubscriptionID string
	EventID        string
	EventType      string
	Target         Target
	CreatedAt      time.Time
	// Attempts is how often this row has been claimed, INCLUDING this
	// claim (the claim increments it).
	Attempts int
	// Email is the recipient address (users.email), "" when the account
	// has none — there is then no transport that could ever deliver it.
	Email string
}

// EmailDeliveryState is the live state of one claimed delivery, read at
// send time. The digest sender decides on these values and never on
// anything the claim returned.
type EmailDeliveryState struct {
	// Found is false when the row no longer exists (nothing disappears
	// here, so this is a defensive case, not an expected one).
	Found bool
	// Status is the row's status NOW. Only SubscriptionDeliveryPending
	// may be sent: 'cancelled' means the owner unsubscribed or the fan-out
	// withdrew it between the claim and now, 'delivered' means another
	// sender finished it.
	Status string
	// SubscriptionLive is whether the row's subscription is still live
	// (subscriptions.deleted_at IS NULL).
	SubscriptionLive bool
	// EventVisibility is the event's own visibility (public/private), read
	// from the append-only research event.
	EventVisibility string
	// OccurredAt is when the event happened (research_events.occurred_at).
	// It is read here, with the visibility, because both are columns of the
	// same immutable row — and because a digest that dated its items by
	// when it happened to be built would misdate a notification that sat
	// behind a daily window.
	OccurredAt time.Time
}
