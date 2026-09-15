package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WebhookStore is the webhook persistence: the endpoint registry CRUD the
// application service drives, and the delivery-log / delivery-attempt
// mutations the worker deliverer drives. Raw SQL like the dispatcher —
// the webhook tables belong to the events domain and the sqlc surface
// (internal/persistence) is not part of this task's scope. Reads never
// select the secret; only Create and RegenerateSecret return it, once.
type WebhookStore struct {
	pool *pgxpool.Pool
}

// NewWebhookStore builds the store on pool. The pool may be lazy
// (persistence.OpenLazy): like the dispatcher, callers retry while
// PostgreSQL is down.
func NewWebhookStore(pool *pgxpool.Pool) *WebhookStore {
	return &WebhookStore{pool: pool}
}

// Sentinel errors. The application service maps these to its own wire
// codes; the worker only ever sees the deliverer's paths.
var (
	// ErrWebhookNotFound: no endpoint with that id owned by that user
	// (the owner scope is part of the query — "exists but not yours" and
	// "does not exist" answer identically, the orgs disclosure rule).
	ErrWebhookNotFound = errors.New("events: webhook endpoint not found")
	// ErrWebhookLimit: the owner already has MaxEndpointsPerUser
	// endpoints.
	ErrWebhookLimit = errors.New("events: webhook endpoint limit reached")
	// ErrWebhookNotRedeliverable: the delivery is pending (or unknown) —
	// only finished deliveries can be redelivered.
	ErrWebhookNotRedeliverable = errors.New("events: delivery not redeliverable")
)

// webhookCols selects every column — Create and RegenerateSecret only
// (they return the secret once). webhookReadCols is everything else:
// reads never select the secret, so no accidental render can leak it.
const webhookCols = `id, user_id, url, secret, event_filters, enabled, consecutive_failures, disabled_at, created_at`

const webhookReadCols = `id, user_id, url, event_filters, enabled, consecutive_failures, disabled_at, created_at`

// CreateEndpoint registers one endpoint for owner userID, enforcing the
// per-user limit under the user row's lock (a concurrent create cannot
// race the count). The secret is the caller's freshly generated value —
// returned once here, never selected again. A nil filter list is stored as
// an empty array ({}), never NULL — the column default only applies to
// omitted columns, and pgx renders nil slices as NULL.
func (s *WebhookStore) CreateEndpoint(ctx context.Context, userID, rawURL, secret string, filters []string) (WebhookEndpoint, error) {
	if filters == nil {
		filters = []string{}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WebhookEndpoint{}, fmt.Errorf("events: webhook create: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// Lock the owner's user row: the limit check and the insert must be
	// atomic against concurrent creates by the same owner. Deleted
	// endpoints do not count toward the limit — a delete frees the slot.
	if _, err := tx.Exec(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, userID); err != nil {
		return WebhookEndpoint{}, fmt.Errorf("events: webhook create: lock owner: %w", err)
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM webhook_endpoints WHERE user_id = $1 AND deleted_at IS NULL`, userID).Scan(&count); err != nil {
		return WebhookEndpoint{}, fmt.Errorf("events: webhook create: count: %w", err)
	}
	if count >= MaxEndpointsPerUser {
		return WebhookEndpoint{}, ErrWebhookLimit
	}
	e, err := scanEndpointFull(tx.QueryRow(ctx, `
		INSERT INTO webhook_endpoints (user_id, url, secret, event_filters)
		VALUES ($1, $2, $3, $4)
		RETURNING `+webhookCols, userID, rawURL, secret, filters))
	if err != nil {
		return WebhookEndpoint{}, fmt.Errorf("events: webhook create: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return WebhookEndpoint{}, fmt.Errorf("events: webhook create: commit: %w", err)
	}
	return e, nil
}

// ListEndpoints returns the owner's live endpoints, oldest first. Secrets
// are not selected.
func (s *WebhookStore) ListEndpoints(ctx context.Context, userID string) ([]WebhookEndpoint, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+webhookReadCols+`
		FROM webhook_endpoints WHERE user_id = $1 AND deleted_at IS NULL ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("events: webhook list: %w", err)
	}
	defer rows.Close()
	return scanEndpoints(rows)
}

// GetEndpoint returns one owned, live endpoint, or ErrWebhookNotFound.
func (s *WebhookStore) GetEndpoint(ctx context.Context, userID, id string) (WebhookEndpoint, error) {
	e, err := scanEndpoint(s.pool.QueryRow(ctx, `
		SELECT `+webhookReadCols+`
		FROM webhook_endpoints WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL`, id, userID))
	if err != nil {
		return WebhookEndpoint{}, mapWebhookScanErr(err)
	}
	return e, nil
}

// UpdateEndpoint applies a PATCH-shaped update: url and enabled are
// pointers (nil = unchanged), filters is a full replacement. Re-enabling
// clears the disable-policy residue (consecutive_failures, disabled_at) —
// the owner's re-enable is a fresh start; disabling stamps disabled_at.
func (s *WebhookStore) UpdateEndpoint(ctx context.Context, userID, id string, rawURL *string, filters []string, enabled *bool) (WebhookEndpoint, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE webhook_endpoints SET
			url = COALESCE($3, url),
			event_filters = COALESCE($4, event_filters),
			enabled = COALESCE($5, enabled),
			disabled_at = CASE
				WHEN $5::boolean IS NULL THEN disabled_at
				WHEN $5 THEN NULL
				ELSE now() END,
			consecutive_failures = CASE WHEN $5 IS NOT NULL AND $5 THEN 0 ELSE consecutive_failures END
		WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL
		RETURNING `+webhookReadCols, id, userID, rawURL, filters, enabled)
	e, err := scanEndpoint(row)
	if err != nil {
		return WebhookEndpoint{}, mapWebhookScanErr(err)
	}
	return e, nil
}

// RegenerateSecret replaces the endpoint's signing secret with secret and
// returns it once — the only read path that selects it.
func (s *WebhookStore) RegenerateSecret(ctx context.Context, userID, id, secret string) (WebhookEndpoint, error) {
	e, err := scanEndpointFull(s.pool.QueryRow(ctx, `
		UPDATE webhook_endpoints SET secret = $3
		WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL
		RETURNING `+webhookCols, id, userID, secret))
	if err != nil {
		return WebhookEndpoint{}, mapWebhookScanErr(err)
	}
	return e, nil
}

// DeleteEndpoint removes one owned endpoint (a soft delete — docs/04 §6:
// nothing disappears, state only evolves) and cancels its pending
// deliveries in the same transaction. The row is not removed: deleted_at
// is stamped and enabled flips false, so the fan-out stops targeting it,
// every read answers not-found and re-enable/redeliver are blocked, while
// the delivery log keeps its endpoint_id and stays readable through
// ListDeliveries (the log is history, and history never vanishes).
func (s *WebhookStore) DeleteEndpoint(ctx context.Context, userID, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("events: webhook delete: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `
		UPDATE webhook_deliveries SET status = $3
		WHERE endpoint_id = $1 AND status = $2`, id, DeliveryPending, DeliveryCancelled); err != nil {
		return fmt.Errorf("events: webhook delete: cancel pending: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE webhook_endpoints SET deleted_at = now(), enabled = false
		WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL`, id, userID)
	if err != nil {
		return fmt.Errorf("events: webhook delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrWebhookNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("events: webhook delete: commit: %w", err)
	}
	return nil
}

// ListDeliveries is the delivery log for one owned endpoint: newest
// first, keyset-paginated on (created_at, id). A nil before pair means
// "from the top" (the audit list shape).
func (s *WebhookStore) ListDeliveries(ctx context.Context, userID, endpointID string, beforeTS *time.Time, beforeID string, limit int) ([]Delivery, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT d.id, d.endpoint_id, d.event_id, d.event_type, d.endpoint, d.status,
		       d.response_code, d.attempts, d.last_attempt_at, d.next_retry_at,
		       d.last_error, d.delivered_at, d.created_at
		FROM webhook_deliveries d
		JOIN webhook_endpoints we ON we.id = d.endpoint_id
		WHERE d.endpoint_id = $1 AND we.user_id = $2
		  AND ($3::timestamptz IS NULL OR (d.created_at, d.id) < ($3::timestamptz, $4::uuid))
		ORDER BY d.created_at DESC, d.id DESC
		LIMIT $5`, endpointID, userID, beforeTS, nullableText(beforeID), limit)
	if err != nil {
		return nil, fmt.Errorf("events: webhook deliveries: %w", err)
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events: webhook deliveries: iterate: %w", err)
	}
	return out, nil
}

// Redeliver puts a finished delivery (failed/cancelled/delivered) back to
// pending with next_retry_at = now — the platform-side manual retry
// docs/22 §9 promises. The delivery id is stable across the redelivery,
// so an idempotent consumer sees the duplicate coming (its dedupe key
// matches). Pending deliveries answer ErrWebhookNotRedeliverable.
func (s *WebhookStore) Redeliver(ctx context.Context, userID, endpointID, deliveryID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE webhook_deliveries d SET status = $4, next_retry_at = now(), last_error = NULL
		FROM webhook_endpoints we
		WHERE d.id = $1 AND d.endpoint_id = we.id AND we.user_id = $2 AND d.endpoint_id = $3
		  AND we.deleted_at IS NULL
		  AND d.status IN ($5, $6, $7)`,
		deliveryID, userID, endpointID, DeliveryPending, DeliveryFailed, DeliveryCancelled, DeliveryDelivered)
	if err != nil {
		return fmt.Errorf("events: webhook redeliver: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var status string
		err := s.pool.QueryRow(ctx, `
			SELECT d.status FROM webhook_deliveries d
			JOIN webhook_endpoints we ON we.id = d.endpoint_id
			WHERE d.id = $1 AND we.user_id = $2 AND d.endpoint_id = $3 AND we.deleted_at IS NULL`,
			deliveryID, userID, endpointID).Scan(&status)
		if err != nil {
			return ErrWebhookNotFound
		}
		return ErrWebhookNotRedeliverable
	}
	return nil
}

// deliveryLease is how long a claimed delivery stays reserved while the
// HTTP attempt is in flight. Claiming pushes next_retry_at into the
// future; a crash mid-request releases the row when the lease expires, so
// at-least-once delivery survives crashes without double-claiming between
// live workers (SKIP LOCKED + the lease).
const deliveryLease = time.Minute

// ClaimDueAttempts reserves up to batch due pending deliveries of enabled
// endpoints for this worker pass and returns what to POST. The claim and
// the lease move in ONE transaction (FOR UPDATE SKIP LOCKED — the T1001
// claim shape); the HTTP work happens after the transaction commits, on
// the caller's connection.
func (s *WebhookStore) ClaimDueAttempts(ctx context.Context, batch int) ([]DeliveryAttempt, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("events: webhook claim: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	rows, err := tx.Query(ctx, `
		SELECT d.id, d.endpoint_id, d.endpoint, we.secret, d.attempts,
		       d.event_id, d.event_type, e.payload, e.occurred_at,
		       e.actor_id, e.project_id, e.correlation_id, e.visibility
		FROM webhook_deliveries d
		JOIN webhook_endpoints we ON we.id = d.endpoint_id
		JOIN research_events e ON e.id = d.event_id
		WHERE d.status = $2 AND we.enabled
		  AND (d.next_retry_at IS NULL OR d.next_retry_at <= now())
		ORDER BY d.next_retry_at NULLS FIRST, d.created_at, d.id
		LIMIT $1
		FOR UPDATE OF d SKIP LOCKED`, batch, DeliveryPending)
	if err != nil {
		return nil, fmt.Errorf("events: webhook claim: %w", err)
	}
	var claimed []DeliveryAttempt
	for rows.Next() {
		var (
			a       DeliveryAttempt
			actor   pgtype.UUID
			project pgtype.UUID
		)
		if err := rows.Scan(&a.DeliveryID, &a.EndpointID, &a.EndpointURL, &a.Secret, &a.Attempts,
			&a.EventID, &a.EventType, &a.Payload, &a.OccurredAt,
			&actor, &project, &a.CorrelationID, &a.Visibility); err != nil {
			rows.Close()
			return nil, fmt.Errorf("events: webhook claim: scan: %w", err)
		}
		a.ActorID = uuidPtr(actor)
		a.ProjectID = uuidPtr(project)
		claimed = append(claimed, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events: webhook claim: iterate: %w", err)
	}
	for _, a := range claimed {
		if _, err := tx.Exec(ctx, `
			UPDATE webhook_deliveries SET next_retry_at = now() + make_interval(secs => $2)
			WHERE id = $1`, a.DeliveryID, int64(deliveryLease/time.Second)); err != nil {
			return nil, fmt.Errorf("events: webhook claim: lease: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("events: webhook claim: commit: %w", err)
	}
	return claimed, nil
}

// RecordDelivered finishes one delivery after a 2xx: the log row flips to
// delivered and the endpoint's consecutive-failure counter resets — one
// success forgives the streak (the disable policy counts CONSECUTIVE
// failures).
func (s *WebhookStore) RecordDelivered(ctx context.Context, deliveryID string, code int) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("events: webhook record delivered: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `
		UPDATE webhook_deliveries SET status = $2, response_code = $3,
		       attempts = attempts + 1, last_attempt_at = now(),
		       next_retry_at = NULL, last_error = NULL, delivered_at = now()
		WHERE id = $1`, deliveryID, DeliveryDelivered, code); err != nil {
		return fmt.Errorf("events: webhook record delivered: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE webhook_endpoints we SET consecutive_failures = 0
		FROM webhook_deliveries d
		WHERE d.id = $1 AND we.id = d.endpoint_id AND we.consecutive_failures > 0`, deliveryID); err != nil {
		return fmt.Errorf("events: webhook record delivered: reset counter: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("events: webhook record delivered: commit: %w", err)
	}
	return nil
}

// RecordDeliveryFailure records one failed attempt. retryAt == nil marks
// the delivery terminal (a 4xx — retrying cannot help); a non-nil retryAt
// keeps it pending until then (5xx/network — transient). Either way the
// endpoint's consecutive-failure counter bumps in the SAME transaction,
// and at limit the endpoint is disabled (enabled=false, disabled_at=now)
// plus the canonical webhook.delivery_failed event is recorded into the
// outbox — the disable and its observability event are atomic, so a crash
// can neither lose the disable nor emit it without one.
//
// It returns whether the failure just disabled the endpoint.
func (s *WebhookStore) RecordDeliveryFailure(ctx context.Context, deliveryID string, code *int, msg string, retryAt *time.Time, failureLimit int) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("events: webhook record failure: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if retryAt == nil {
		if _, err := tx.Exec(ctx, `
			UPDATE webhook_deliveries SET status = $2, response_code = $3,
			       attempts = attempts + 1, last_attempt_at = now(),
			       next_retry_at = NULL, last_error = $4
			WHERE id = $1`, deliveryID, DeliveryFailed, code, msg); err != nil {
			return false, fmt.Errorf("events: webhook record failure: %w", err)
		}
	} else {
		if _, err := tx.Exec(ctx, `
			UPDATE webhook_deliveries SET response_code = $2,
			       attempts = attempts + 1, last_attempt_at = now(),
			       next_retry_at = $3, last_error = $4
			WHERE id = $1`, deliveryID, code, *retryAt, msg); err != nil {
			return false, fmt.Errorf("events: webhook record failure: %w", err)
		}
	}

	var endpointID, userID string
	var failures int
	if err := tx.QueryRow(ctx, `
		UPDATE webhook_endpoints we SET consecutive_failures = we.consecutive_failures + 1
		FROM webhook_deliveries d
		WHERE d.id = $1 AND we.id = d.endpoint_id
		RETURNING we.id, we.user_id, we.consecutive_failures`, deliveryID).Scan(&endpointID, &userID, &failures); err != nil {
		return false, fmt.Errorf("events: webhook record failure: bump counter: %w", err)
	}
	if failures >= failureLimit {
		tag, err := tx.Exec(ctx, `
			UPDATE webhook_endpoints SET enabled = false, disabled_at = now()
			WHERE id = $1 AND enabled`, endpointID)
		if err != nil {
			return false, fmt.Errorf("events: webhook record failure: disable: %w", err)
		}
		if tag.RowsAffected() > 0 {
			ev, err := webhookDeliveryFailedEvent(endpointID, userID, failures, msg)
			if err != nil {
				return false, fmt.Errorf("events: webhook record failure: build event: %w", err)
			}
			// Fail-closed like every outbox write: the disable must not
			// be silently un-observable.
			if err := Record(ctx, tx, ev); err != nil {
				return false, fmt.Errorf("events: webhook record failure: record delivery_failed event: %w", err)
			}
			if err := tx.Commit(ctx); err != nil {
				return false, fmt.Errorf("events: webhook record failure: commit: %w", err)
			}
			return true, nil
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("events: webhook record failure: commit: %w", err)
	}
	return false, nil
}

// RecordDeliveryRetryable records one failed attempt that must NOT burn
// the endpoint's consecutive-failure streak: the response was outside
// 2xx/4xx/5xx (an informational status, a redirect) — retrying may help,
// but an unclassified status is not evidence the endpoint is failing. The
// row stays pending until retryAt; the counter is untouched.
func (s *WebhookStore) RecordDeliveryRetryable(ctx context.Context, deliveryID string, code *int, msg string, retryAt time.Time) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE webhook_deliveries SET response_code = $2,
		       attempts = attempts + 1, last_attempt_at = now(),
		       next_retry_at = $3, last_error = $4
		WHERE id = $1`, deliveryID, code, retryAt, msg); err != nil {
		return fmt.Errorf("events: webhook record retryable: %w", err)
	}
	return nil
}

// scanEndpoints drains a rows cursor of endpoints.
func scanEndpoints(rows pgx.Rows) ([]WebhookEndpoint, error) {
	var out []WebhookEndpoint
	for rows.Next() {
		e, err := scanEndpoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events: webhook list: iterate: %w", err)
	}
	return out, nil
}

// scanEndpointFull maps one endpoint row in the webhookCols (10-column,
// secret included) order — Create and RegenerateSecret only.
func scanEndpointFull(row interface{ Scan(dest ...any) error }) (WebhookEndpoint, error) {
	var (
		e        WebhookEndpoint
		disabled pgtype.Timestamptz
		created  time.Time
	)
	err := row.Scan(&e.ID, &e.UserID, &e.URL, &e.Secret, &e.EventFilters, &e.Enabled,
		&e.ConsecutiveFailures, &disabled, &created)
	if err != nil {
		return WebhookEndpoint{}, err
	}
	e.DisabledAt = tsPtr(disabled)
	e.CreatedAt = created
	return e, nil
}

// scanEndpoint maps one endpoint row in the webhookReadCols (9-column,
// no secret) order — every read path.
func scanEndpoint(row interface{ Scan(dest ...any) error }) (WebhookEndpoint, error) {
	var (
		e        WebhookEndpoint
		disabled pgtype.Timestamptz
		created  time.Time
	)
	err := row.Scan(&e.ID, &e.UserID, &e.URL, &e.EventFilters, &e.Enabled,
		&e.ConsecutiveFailures, &disabled, &created)
	if err != nil {
		return WebhookEndpoint{}, err
	}
	e.DisabledAt = tsPtr(disabled)
	e.CreatedAt = created
	return e, nil
}

// scanDelivery maps one delivery-log row in the ListDeliveries order.
func scanDelivery(row interface{ Scan(dest ...any) error }) (Delivery, error) {
	var (
		d           Delivery
		lastAttempt pgtype.Timestamptz
		nextRetry   pgtype.Timestamptz
		deliveredAt pgtype.Timestamptz
		endpointID  pgtype.UUID
	)
	err := row.Scan(&d.ID, &endpointID, &d.EventID, &d.EventType, &d.EndpointURL, &d.Status,
		&d.ResponseCode, &d.Attempts, &lastAttempt, &nextRetry, &d.LastError, &deliveredAt, &d.CreatedAt)
	if err != nil {
		return Delivery{}, fmt.Errorf("events: scan delivery: %w", err)
	}
	d.EndpointID = uuidString(endpointID)
	d.LastAttemptAt = tsPtr(lastAttempt)
	d.NextRetryAt = tsPtr(nextRetry)
	d.DeliveredAt = tsPtr(deliveredAt)
	return d, nil
}

// tsPtr renders a nullable timestamptz.
func tsPtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

// mapWebhookScanErr turns a no-rows scan into ErrWebhookNotFound.
func mapWebhookScanErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrWebhookNotFound
	}
	return fmt.Errorf("events: webhook store: %w", err)
}

// webhookDeliveryFailedEvent builds the canonical webhook.delivery_failed
// outbox event the disable policy emits (specs/events/event-types.yaml):
// identity/reference only — endpoint id, the counter that tripped, and
// the bounded last error. Private: the endpoint and its failures belong
// to the owner (docs/12 — the event is never more visible than its
// subject).
func webhookDeliveryFailedEvent(endpointID, userID string, failures int, lastErr string) (Event, error) {
	payload, err := json.Marshal(struct {
		EndpointID          string `json:"endpoint_id"`
		ConsecutiveFailures int    `json:"consecutive_failures"`
		LastError           string `json:"last_error"`
	}{endpointID, failures, truncateError(lastErr, maxErrorLen)})
	if err != nil {
		return Event{}, err
	}
	return Event{
		EventType:  "webhook.delivery_failed",
		ActorID:    userID,
		Visibility: VisibilityPrivate,
		Payload:    payload,
	}, nil
}
