package notifications

import (
	"context"
	"time"

	"github.com/lichman0405/post/internal/events"
)

// Store is the persistence port for the account's email cadence,
// implemented by events.NotificationStore. Every read and write is scoped
// by userID: the setting is the account's own, and no call can name another
// account's row.
type Store interface {
	// GetPreferences returns the account's cadence, or the zero
	// preferences when the account has never set one (absence is a state,
	// not an error — events.NotificationPreferences.EffectiveCadence
	// resolves it to the default).
	GetPreferences(ctx context.Context, userID string) (events.NotificationPreferences, error)
	// SetCadence records the account's cadence, creating the row on first
	// use.
	SetCadence(ctx context.Context, userID, cadence string) (events.NotificationPreferences, error)
}

// DigestStore is what the digest sender drives, implemented by
// events.NotificationStore. It is the claim/gate/mark cycle of one send:
// claim the due rows under a lease, read back the live state the gate
// decides on, and record the outcome — delivered, or withdrawn.
type DigestStore interface {
	// WithdrawExhaustedEmailDeliveries cancels pending email rows that
	// have used up their attempts and returns how many.
	WithdrawExhaustedEmailDeliveries(ctx context.Context, maxAttempts int) (int, error)
	// ClaimEmailDeliveries leases up to limit due pending email rows and
	// returns them oldest first.
	ClaimEmailDeliveries(ctx context.Context, limit, maxAttempts int) ([]events.EmailDelivery, error)
	// EmailDeliveryState reads one claimed row's live state — status,
	// subscription liveness, event visibility. The sender decides on this,
	// never on a value carried from claim time.
	EmailDeliveryState(ctx context.Context, id string) (events.EmailDeliveryState, error)
	// TargetAudienceFor resolves how the subscriber is related to the
	// target RIGHT NOW. It is the fan-out's own rule (see the port's
	// implementation), called at send time rather than at fan-out time.
	TargetAudienceFor(ctx context.Context, target events.Target, userID string) (events.AudienceLevel, error)
	// TargetLabel returns the target's human label, for rendering. A read,
	// not a gate: the sender calls it only after the audience gate
	// permitted the item.
	TargetLabel(ctx context.Context, target events.Target) (string, error)
	// MarkEmailDelivered records that the digest carrying these rows
	// reached the transport, and returns how many rows it moved.
	MarkEmailDelivered(ctx context.Context, ids []string) (int, error)
	// WithdrawEmailDelivery cancels one claimed delivery the sender
	// decided not to send.
	WithdrawEmailDelivery(ctx context.Context, id string) error
	// RecordDigestSent stamps the interval anchor after a digest was
	// handed to the transport.
	RecordDigestSent(ctx context.Context, userID string, at time.Time) error
}

// Mail is one rendered message, as the transport port carries it.
type Mail struct {
	To      string
	Subject string
	// Text and HTML are the two bodies of the same message (text/plain and
	// text/html). Both are rendered from the same digest so they cannot
	// disagree about what the message says.
	Text string
	HTML string
}

// Mailer is the mail transport port (docs/52: email goes through a port,
// like Gitea, S3 and Redis — no domain code knows a provider). V1 ships the
// development implementation (DevSink); production SMTP is a transport this
// port is waiting for, not a reason to leak one into the sender.
//
// Send's contract: an error means "not handed over", so the caller leaves
// the deliveries pending and they are retried; a nil error means the
// message left the sender. At-least-once, like every delivery path in this
// domain — a crash after Send returns costs a duplicate email, never a
// lost one.
type Mailer interface {
	Send(ctx context.Context, m Mail) error
}
