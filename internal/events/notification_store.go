package events

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NotificationStore is the email-notification persistence (T1005): the
// account's cadence, the digest sender's claim over the pending email rows,
// and the reads the send-time gate makes. Raw SQL like the subscription
// store next to it, and for the same reason — subscription tables are the
// events domain's own, not the sqlc-modeled ones.
//
// It owns no policy. Whether a claimed delivery may be sent is decided by
// the caller from the values this store returns (EmailDeliveryState plus
// TargetAudienceFor) and the ONE rule the whole pipeline shares (Delivers);
// the store only reads live rows and applies the transitions it is asked
// for.
type NotificationStore struct {
	pool *pgxpool.Pool
	subs *SubscriptionStore
}

// NewNotificationStore builds the store on pool. The pool may be lazy
// (persistence.OpenLazy): the sender retries while PostgreSQL is down, like
// every other worker dependency outage.
func NewNotificationStore(pool *pgxpool.Pool) *NotificationStore {
	return &NotificationStore{pool: pool, subs: NewSubscriptionStore(pool)}
}

// preferenceCols is the preference row as every read returns it.
const preferenceCols = `user_id, cadence, last_digest_at, created_at, updated_at`

// GetPreferences returns userID's email cadence, or the zero preferences
// (Stored false, Cadence "") when the account has never set one — absence
// is a state, not an error, and the caller resolves it through
// NotificationPreferences.EffectiveCadence.
func (s *NotificationStore) GetPreferences(ctx context.Context, userID string) (NotificationPreferences, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+preferenceCols+` FROM notification_preferences WHERE user_id = $1`, userID)
	prefs, err := scanPreferences(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return NotificationPreferences{UserID: userID}, nil
		}
		return NotificationPreferences{}, fmt.Errorf("events: read notification preferences: %w", err)
	}
	return prefs, nil
}

// SetCadence records the account's email cadence, creating the row on first
// use. It is an upsert rather than an update: the setting has a default
// (DefaultCadence) and the row exists only to record the departure from it,
// so "set my cadence to daily" for an account that never had a row must
// work. The cadence is validated by the caller (the application service);
// the column's CHECK is the second line.
func (s *NotificationStore) SetCadence(ctx context.Context, userID, cadence string) (NotificationPreferences, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO notification_preferences (user_id, cadence)
		VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE SET cadence = EXCLUDED.cadence, updated_at = now()
		RETURNING `+preferenceCols, userID, cadence)
	prefs, err := scanPreferences(row)
	if err != nil {
		return NotificationPreferences{}, fmt.Errorf("events: set notification cadence %q: %w", cadence, err)
	}
	return prefs, nil
}

// RecordDigestSent stamps the interval anchor after a digest was handed to
// the transport: this is what makes a daily/weekly cadence wait a full
// window before the next one. It never touches cadence — the two writes
// (the setting and the schedule) are independent, and a digest must not be
// able to rewrite what the account asked for.
func (s *NotificationStore) RecordDigestSent(ctx context.Context, userID string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO notification_preferences (user_id, last_digest_at)
		VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE SET last_digest_at = EXCLUDED.last_digest_at, updated_at = now()`,
		userID, at)
	if err != nil {
		return fmt.Errorf("events: record digest for %s: %w", userID, err)
	}
	return nil
}

// TargetAudienceFor is TargetAudience on the store's own pool. It exists so
// the digest sender resolves the subscriber's relationship through the SAME
// implementation the fan-out and the subscribe path use — a second copy of
// that rule is exactly how a revoked membership keeps receiving private
// events (see SubscriptionStore's type comment). The sender calls it at
// SEND time, which is the point: the audience is re-resolved against live
// rows for every digest, not remembered from fan-out time.
func (s *NotificationStore) TargetAudienceFor(ctx context.Context, target Target, userID string) (AudienceLevel, error) {
	return s.subs.TargetAudienceFor(ctx, target, userID)
}

// TargetLabel returns the human label of a target — the subject's own name,
// for the digest's rendering.
//
// It is a READ, not a gate: it answers "what is this called", never "may
// this caller see it". The digest sender calls it only for deliveries that
// have already passed the audience gate (Sender.build), and a caller that
// used it to decide what to show would be reading past that gate. An
// unknown target, a target type with no label, or an empty label is "" —
// the renderer falls back to the target id, never to a guess.
func (s *NotificationStore) TargetLabel(ctx context.Context, target Target) (string, error) {
	query, ok := subjectLabelQueries[target.Type]
	if !ok {
		return "", nil
	}
	var label string
	if err := s.pool.QueryRow(ctx, query, target.ID).Scan(&label); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("events: read the label of %s %s: %w", target.Type, target.ID, err)
	}
	return label, nil
}

// subjectLabelQueries names one target per type, read from the subject's own
// row — the same tables the audience queries read, and the reason a label is
// only ever read behind the gate: these are the titles a private target's
// name lives in.
var subjectLabelQueries = map[string]string{
	TargetTypeProject:      `SELECT name FROM projects WHERE id = $1::uuid`,
	TargetTypeAsset:        `SELECT title FROM research_assets WHERE pid = $1`,
	TargetTypeKnowledge:    `SELECT sov.title FROM knowledge_publications kp JOIN scientific_object_versions sov ON sov.id = kp.object_version_id WHERE kp.pid = $1`,
	TargetTypeUser:         `SELECT display_name FROM users WHERE id = $1::uuid`,
	TargetTypeOrganization: `SELECT name FROM organizations WHERE id = $1::uuid`,
}

// ClaimEmailDeliveries claims up to limit pending email deliveries whose
// subscriber is due for a digest, and returns them oldest first.
//
// The claim is a LEASE, not a transaction boundary: this statement commits
// on its own, stamps leased_until, and the caller sends after it returns.
// Two senders therefore cannot claim the same row (SKIP LOCKED plus the
// lease), and a crash between the claim and the send costs at most one
// duplicate — the row is claimed again once the lease expires, which is the
// at-least-once delivery every other pipeline in this domain has (docs/18
// §1, ADR-013).
//
// The lease grows with the attempts: a delivery whose send keeps failing is
// retried at base × 2^attempts, capped, instead of hot-looping. Rows that
// have reached maxAttempts are not claimable; the caller withdraws them
// (WithdrawExhaustedEmailDeliveries) so they leave the queue with a reason
// instead of blocking their subscriber's head-of-line forever.
//
// The cadence filter is applied HERE because it decides which rows the
// claim may take: a daily subscriber whose last digest was an hour ago is
// not due, and leasing their rows would only make them unclaimable for the
// lease duration. The intervals come from DigestWindow, passed in, so the
// durations have one source (the CASE below only picks between them).
func (s *NotificationStore) ClaimEmailDeliveries(ctx context.Context, limit, maxAttempts int) ([]EmailDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, claimEmailDeliveries,
		maxAttempts, limit, EmailDeliveryLease, DefaultCadence,
		DigestWindow(CadenceDaily), DigestWindow(CadenceWeekly), EmailDeliveryLeaseCap)
	if err != nil {
		return nil, fmt.Errorf("events: claim email deliveries: %w", err)
	}
	defer rows.Close()
	out := []EmailDelivery{}
	for rows.Next() {
		var (
			d     EmailDelivery
			email *string
		)
		if err := rows.Scan(&d.ID, &d.UserID, &email, &d.SubscriptionID, &d.EventID,
			&d.EventType, &d.Target.Type, &d.Target.ID, &d.CreatedAt, &d.Attempts); err != nil {
			return nil, fmt.Errorf("events: claim email deliveries: scan: %w", err)
		}
		if email != nil {
			d.Email = *email
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events: claim email deliveries: iterate: %w", err)
	}
	// The statement's own order is not part of its contract (the UPDATE's
	// RETURNING order is unspecified), so the digest's item order is fixed
	// here: oldest first, id as the tiebreak.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// claimEmailDeliveries claims due pending email rows under a lease.
//
// $1 maxAttempts  $2 limit          $3 lease base       $4 default cadence
// $5 daily window $6 weekly window  $7 lease cap
//
// The LEFT JOIN is what makes the default cadence work: an account with no
// notification_preferences row still has a cadence (DefaultCadence), and an
// inner join would silently stop delivering to every account that never
// opened the setting.
const claimEmailDeliveries = `
WITH due AS (
    SELECT d.id
    FROM subscription_deliveries d
    LEFT JOIN notification_preferences p ON p.user_id = d.user_id
    WHERE d.channel = 'email'
      AND d.status = 'pending'
      AND d.attempts < $1
      AND (d.leased_until IS NULL OR d.leased_until <= now())
      AND (COALESCE(p.cadence, $4) = 'immediate'
           OR p.last_digest_at IS NULL
           OR p.last_digest_at <= now() - CASE COALESCE(p.cadence, $4)
                                            WHEN 'daily' THEN $5::interval
                                            ELSE $6::interval
                                          END)
    ORDER BY d.created_at, d.id
    LIMIT $2
    FOR UPDATE OF d SKIP LOCKED
)
UPDATE subscription_deliveries d
SET attempts = d.attempts + 1,
    leased_until = now() + LEAST($3::interval * power(2, d.attempts), $7::interval)
FROM due, users u
WHERE d.id = due.id AND u.id = d.user_id
RETURNING d.id, d.user_id, u.email, d.subscription_id, d.event_id, d.event_type,
          d.target_type, d.target_id, d.created_at, d.attempts`

// WithdrawExhaustedEmailDeliveries cancels pending email rows that have used
// up their attempts, and returns how many it withdrew. It is the digest
// sender's dead-letter equivalent: a delivery the transport has refused
// MaxEmailDeliveryAttempts times is withdrawn — with cancelled_at recorded,
// so the row says what happened to it — instead of being retried forever
// and holding up the deliveries queued behind it. It touches email rows
// only: a web row is delivered the moment it exists (00078).
func (s *NotificationStore) WithdrawExhaustedEmailDeliveries(ctx context.Context, maxAttempts int) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE subscription_deliveries
		SET status = 'cancelled', cancelled_at = now(), leased_until = NULL
		WHERE channel = 'email' AND status = 'pending' AND attempts >= $1`, maxAttempts)
	if err != nil {
		return 0, fmt.Errorf("events: withdraw exhausted email deliveries: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// EmailDeliveryState reads the live state of one claimed delivery — the
// state the send-time gate decides on. Found is false when the row is
// gone (defensive: nothing here is hard-deleted).
func (s *NotificationStore) EmailDeliveryState(ctx context.Context, id string) (EmailDeliveryState, error) {
	var (
		st     EmailDeliveryState
		live   bool
		status string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT d.status, (sub.deleted_at IS NULL), re.visibility, re.occurred_at
		FROM subscription_deliveries d
		JOIN subscriptions sub ON sub.id = d.subscription_id
		JOIN research_events re ON re.id = d.event_id
		WHERE d.id = $1::uuid`, id).Scan(&status, &live, &st.EventVisibility, &st.OccurredAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return EmailDeliveryState{}, nil
		}
		return EmailDeliveryState{}, fmt.Errorf("events: read email delivery state: %w", err)
	}
	st.Found = true
	st.Status = status
	st.SubscriptionLive = live
	return st, nil
}

// MarkEmailDelivered records that the digest carrying these rows was handed
// to the transport. It only touches rows still pending: a row the owner
// withdrew between the send and this write stays withdrawn (the email may
// have left, but the record must not claim a delivery that was cancelled —
// the difference is exactly what an operator needs to see). It returns how
// many rows it moved.
func (s *NotificationStore) MarkEmailDelivered(ctx context.Context, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE subscription_deliveries
		SET status = 'delivered', delivered_at = now(), leased_until = NULL
		WHERE id = ANY($1::uuid[]) AND status = 'pending'`, ids)
	if err != nil {
		return 0, fmt.Errorf("events: mark email deliveries delivered: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// WithdrawEmailDelivery cancels one claimed delivery the sender has decided
// not to send: the owner lost access to the target, unsubscribed, or the
// account has no address to send to. The reason is the caller's to log —
// what the row records is the fact (cancelled_at), which is the same thing
// an unsubscribe stamps, because it is the same outcome.
func (s *NotificationStore) WithdrawEmailDelivery(ctx context.Context, id string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE subscription_deliveries
		SET status = 'cancelled', cancelled_at = now(), leased_until = NULL
		WHERE id = $1::uuid AND status = 'pending'`, id); err != nil {
		return fmt.Errorf("events: withdraw email delivery %s: %w", id, err)
	}
	return nil
}

func scanPreferences(row rowScanner) (NotificationPreferences, error) {
	var p NotificationPreferences
	if err := row.Scan(&p.UserID, &p.Cadence, &p.LastDigestAt, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return NotificationPreferences{}, err
	}
	p.Stored = true
	return p, nil
}
