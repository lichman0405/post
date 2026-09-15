package webhooks

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
)

// Service orchestrates the webhook endpoint use cases (T1006). All policy
// lives here (docs/52):
//
//   - endpoints belong to one user; every operation is owner-scoped and a
//     miss answers ErrNotFound without disclosing existence (the orgs
//     rule);
//   - the signing secret is generated here (crypto/rand), returned
//     exactly once at creation/rotation, and never again — reads answer
//     without it by construction (the store does not select it);
//   - the URL and filter shapes are validated here (the store trusts the
//     service, the service trusts nothing);
//   - re-enabling an endpoint the disable policy tripped resets its
//     failure streak (the owner's decision to retry deserves a clean
//     slate).
type Service struct {
	store Store
	// secretSource generates signing secrets; replaceable in tests only.
	secretSource func() (string, error)
}

// NewService wires the service on the store port.
func NewService(store Store) *Service {
	return &Service{store: store, secretSource: events.GenerateSecret}
}

// CreatedEndpoint carries an endpoint plus the one-time secret of the
// create/rotation call that returned it.
type CreatedEndpoint struct {
	events.WebhookEndpoint
	Secret string
}

// Create registers a new endpoint for the actor and returns it with its
// generated secret — the only time the secret is shown. max filters per
// endpoint and endpoints per user are the events package's policy
// constants.
func (s *Service) Create(ctx context.Context, actor domain.User, rawURL string, filters []string) (CreatedEndpoint, error) {
	if err := validateEndpointInput(rawURL, filters); err != nil {
		return CreatedEndpoint{}, err
	}
	secret, err := s.secretSource()
	if err != nil {
		return CreatedEndpoint{}, fmt.Errorf("%w: generating secret: %v", ErrStore, err)
	}
	e, err := s.store.CreateEndpoint(ctx, actor.ID, rawURL, secret, filters)
	if err != nil {
		return CreatedEndpoint{}, mapStoreError(err)
	}
	e.Secret = "" // the secret travels in the dedicated field only
	return CreatedEndpoint{WebhookEndpoint: e, Secret: secret}, nil
}

// List returns the actor's endpoints, oldest first.
func (s *Service) List(ctx context.Context, actor domain.User) ([]events.WebhookEndpoint, error) {
	list, err := s.store.ListEndpoints(ctx, actor.ID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStore, err)
	}
	return list, nil
}

// Get returns one owned endpoint.
func (s *Service) Get(ctx context.Context, actor domain.User, id string) (events.WebhookEndpoint, error) {
	e, err := s.store.GetEndpoint(ctx, actor.ID, id)
	if err != nil {
		return events.WebhookEndpoint{}, mapStoreError(err)
	}
	return e, nil
}

// Update applies a PATCH-shaped update: url and enabled are pointers
// (nil = unchanged — a filters-only PATCH must not touch them), filters
// is a full replacement. Re-enabling resets the disable-policy streak
// (store-side, with disabled_at cleared).
func (s *Service) Update(ctx context.Context, actor domain.User, id string, rawURL *string, filters []string, enabled *bool) (events.WebhookEndpoint, error) {
	if rawURL != nil {
		if err := events.ValidateWebhookURL(*rawURL); err != nil {
			return events.WebhookEndpoint{}, fmt.Errorf("%w: %v", ErrValidation, err)
		}
	}
	if err := events.ValidateEventFilters(filters); err != nil {
		return events.WebhookEndpoint{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	e, err := s.store.UpdateEndpoint(ctx, actor.ID, id, rawURL, filters, enabled)
	if err != nil {
		return events.WebhookEndpoint{}, mapStoreError(err)
	}
	return e, nil
}

// RegenerateSecret replaces the endpoint's signing secret and returns the
// new one — the only other time a secret is shown. Pending deliveries are
// signed with the new secret from here on (the credential is resolved at
// delivery time).
func (s *Service) RegenerateSecret(ctx context.Context, actor domain.User, id string) (CreatedEndpoint, error) {
	secret, err := s.secretSource()
	if err != nil {
		return CreatedEndpoint{}, fmt.Errorf("%w: generating secret: %v", ErrStore, err)
	}
	e, err := s.store.RegenerateSecret(ctx, actor.ID, id, secret)
	if err != nil {
		return CreatedEndpoint{}, mapStoreError(err)
	}
	e.Secret = ""
	return CreatedEndpoint{WebhookEndpoint: e, Secret: secret}, nil
}

// Delete removes one owned endpoint. Its pending deliveries are cancelled
// in the same transaction; finished delivery-log rows stay (nothing
// disappears — docs/04 §6).
func (s *Service) Delete(ctx context.Context, actor domain.User, id string) error {
	if err := s.store.DeleteEndpoint(ctx, actor.ID, id); err != nil {
		return mapStoreError(err)
	}
	return nil
}

// ListDeliveries is the endpoint's delivery log, newest first. The
// keyset pair (beforeTS, beforeID) pages; empty means "from the top".
func (s *Service) ListDeliveries(ctx context.Context, actor domain.User, endpointID string, beforeTS *time.Time, beforeID string) ([]events.Delivery, error) {
	// Validate the pagination key at the boundary: a non-UUID before id
	// must answer a 400, not a store error deep in the query cast.
	if beforeID != "" {
		var id pgtype.UUID
		if err := id.Scan(beforeID); err != nil {
			return nil, fmt.Errorf("%w: before id must be a UUID", ErrValidation)
		}
	}
	// Ownership is checked by the store's join; a foreign endpoint id
	// answers an empty page, which is fine for a sub-resource read.
	list, err := s.store.ListDeliveries(ctx, actor.ID, endpointID, beforeTS, beforeID, deliveryPageLimit)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStore, err)
	}
	return list, nil
}

// deliveryPageLimit bounds one delivery-log page.
const deliveryPageLimit = 50

// Redeliver re-queues one finished delivery (failed/cancelled/delivered)
// for a new attempt — the platform-side manual retry docs/22 §9
// promises. The delivery id stays the same, so an idempotent consumer
// recognizes the duplicate.
func (s *Service) Redeliver(ctx context.Context, actor domain.User, endpointID, deliveryID string) error {
	if err := s.store.Redeliver(ctx, actor.ID, endpointID, deliveryID); err != nil {
		return mapStoreError(err)
	}
	return nil
}

// validateEndpointInput checks the create-time shape: URL and filters
// travel the same validators the update path uses.
func validateEndpointInput(rawURL string, filters []string) error {
	if err := events.ValidateWebhookURL(rawURL); err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if err := events.ValidateEventFilters(filters); err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	return nil
}

// mapStoreError translates the store's sentinels into this package's.
func mapStoreError(err error) error {
	switch {
	case errors.Is(err, events.ErrWebhookNotFound):
		return ErrNotFound
	case errors.Is(err, events.ErrWebhookLimit):
		return ErrLimit
	case errors.Is(err, events.ErrWebhookNotRedeliverable):
		return ErrNotRedeliverable
	default:
		return fmt.Errorf("%w: %v", ErrStore, err)
	}
}
