package webhooks

import (
	"context"
	"time"

	"github.com/lichman0405/post/internal/events"
)

// Store is the persistence port for webhook endpoints and the delivery
// log, implemented by events.WebhookStore. Everything is owner-scoped by
// userID: the store never leaks another user's endpoint or delivery.
type Store interface {
	// CreateEndpoint registers one endpoint for the owner and returns it
	// with the secret populated exactly once (reads never select it). It
	// fails with events.ErrWebhookLimit past the per-user limit.
	CreateEndpoint(ctx context.Context, userID, url, secret string, filters []string) (events.WebhookEndpoint, error)
	// ListEndpoints returns the owner's endpoints, oldest first.
	ListEndpoints(ctx context.Context, userID string) ([]events.WebhookEndpoint, error)
	// GetEndpoint returns one owned endpoint or
	// events.ErrWebhookNotFound.
	GetEndpoint(ctx context.Context, userID, id string) (events.WebhookEndpoint, error)
	// UpdateEndpoint applies a PATCH-shaped update (url/enabled are
	// pointers, filters is a full replacement) or
	// events.ErrWebhookNotFound.
	UpdateEndpoint(ctx context.Context, userID, id string, url *string, filters []string, enabled *bool) (events.WebhookEndpoint, error)
	// RegenerateSecret replaces the signing secret and returns the
	// endpoint with the new secret populated exactly once.
	RegenerateSecret(ctx context.Context, userID, id, secret string) (events.WebhookEndpoint, error)
	// DeleteEndpoint removes one owned endpoint (cancelling its pending
	// deliveries in the same transaction) or events.ErrWebhookNotFound.
	DeleteEndpoint(ctx context.Context, userID, id string) error
	// ListDeliveries is the endpoint's delivery log, newest first,
	// keyset-paginated on (created_at, id).
	ListDeliveries(ctx context.Context, userID, endpointID string, beforeTS *time.Time, beforeID string, limit int) ([]events.Delivery, error)
	// Redeliver puts a finished delivery back to pending, or
	// events.ErrWebhookNotRedeliverable / events.ErrWebhookNotFound.
	Redeliver(ctx context.Context, userID, endpointID, deliveryID string) error
}
