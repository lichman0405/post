package subscriptions

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
)

// Service orchestrates the follow/watch use cases (T1002). All policy
// lives here (docs/52):
//
//   - a subscription belongs to one user; every operation is owner-scoped
//     and a miss answers ErrNotFound without disclosing existence (the
//     orgs rule);
//   - the target type, target id, filter and channel shapes are validated
//     here — the store trusts the service, the service trusts nothing;
//   - you may only follow what you may SEE: the subscribe path resolves
//     the actor's relationship to the target right now, and no
//     relationship answers ErrNotFound, the same existence-hiding answer
//     an unreadable project gives. Anything else would make the subscribe
//     endpoint an existence oracle for private targets.
//
// The complementary half — a subscription that already exists when the
// actor's access is revoked — is not enforced here, because a run of this
// service does not happen at the moment permission changes. It is enforced
// per event by events.SubscriptionFanOut, which re-resolves the same rule
// against live state before any delivery row is written, and withdraws the
// deliveries that are already in flight. Both call
// events.SubscriptionStore.TargetAudience.
type Service struct {
	store Store
}

// NewService wires the service on the store port.
func NewService(store Store) *Service {
	return &Service{store: store}
}

// Subscribe registers the actor's follow of one target, optionally
// narrowed to an event-type filter list and always over at least one
// channel. It answers ErrNotFound when the target does not exist or the
// actor may not see it (indistinguishably), ErrExists when the actor
// already follows it, ErrLimit at the per-user cap, and ErrValidation for
// any shape rule.
func (s *Service) Subscribe(ctx context.Context, actor domain.User, target events.Target, filters, channels []string) (events.Subscription, error) {
	if err := validateTarget(target); err != nil {
		return events.Subscription{}, err
	}
	if err := validateFilters(filters); err != nil {
		return events.Subscription{}, err
	}
	if err := validateChannels(channels); err != nil {
		return events.Subscription{}, err
	}
	// The authorization decision. Fail-closed: an unresolvable target is
	// AudienceNone, which is the same answer as "not yours".
	level, err := s.store.TargetAudienceFor(ctx, target, actor.ID)
	if err != nil {
		return events.Subscription{}, fmt.Errorf("%w: %v", ErrStore, err)
	}
	if level == events.AudienceNone {
		return events.Subscription{}, ErrNotFound
	}
	sub, err := s.store.CreateSubscription(ctx, actor.ID, target, filters, channels)
	if err != nil {
		return events.Subscription{}, mapStoreError(err)
	}
	return sub, nil
}

// List returns the actor's live subscriptions, newest first. A non-empty
// targetType (optionally with targetID) narrows to one target — the
// watch-state read a surface makes before rendering a follow control.
func (s *Service) List(ctx context.Context, actor domain.User, targetType, targetID string) ([]events.Subscription, error) {
	if targetType != "" {
		if !events.ValidateTargetType(targetType) {
			return nil, fmt.Errorf("%w: unknown target type %q", ErrValidation, targetType)
		}
		if targetID != "" {
			if err := events.ValidateTargetID(targetType, targetID); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrValidation, err)
			}
		}
	} else if targetID != "" {
		// A target id without its type cannot be validated (the shape is
		// per type), so it is rejected rather than guessed at.
		return nil, fmt.Errorf("%w: target_type is required with target_id", ErrValidation)
	}
	list, err := s.store.ListSubscriptions(ctx, actor.ID, targetType, targetID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStore, err)
	}
	return list, nil
}

// Get returns one owned subscription.
func (s *Service) Get(ctx context.Context, actor domain.User, id string) (events.Subscription, error) {
	if err := validateID(id); err != nil {
		return events.Subscription{}, err
	}
	sub, err := s.store.GetSubscription(ctx, actor.ID, id)
	if err != nil {
		return events.Subscription{}, mapStoreError(err)
	}
	return sub, nil
}

// Update replaces the subscription's event filters and channels (a full
// replacement: the PATCH body carries the new lists). The target is not
// editable — following something else is a different subscription, and
// re-pointing an existing one would move the meaning of deliveries already
// fanned out under it.
//
// Access is re-checked, for the same reason Subscribe checks it: a
// subscription to a target the actor can no longer see must not be
// silently editable into a working one, and answering ErrNotFound keeps
// the existence-hiding rule uniform across the write paths.
func (s *Service) Update(ctx context.Context, actor domain.User, id string, filters, channels []string) (events.Subscription, error) {
	if err := validateID(id); err != nil {
		return events.Subscription{}, err
	}
	if err := validateFilters(filters); err != nil {
		return events.Subscription{}, err
	}
	if err := validateChannels(channels); err != nil {
		return events.Subscription{}, err
	}
	current, err := s.store.GetSubscription(ctx, actor.ID, id)
	if err != nil {
		return events.Subscription{}, mapStoreError(err)
	}
	level, err := s.store.TargetAudienceFor(ctx, current.Target(), actor.ID)
	if err != nil {
		return events.Subscription{}, fmt.Errorf("%w: %v", ErrStore, err)
	}
	if level == events.AudienceNone {
		return events.Subscription{}, ErrNotFound
	}
	sub, err := s.store.UpdateSubscription(ctx, actor.ID, id, filters, channels)
	if err != nil {
		return events.Subscription{}, mapStoreError(err)
	}
	return sub, nil
}

// Unsubscribe ends the actor's follow. It answers ErrNotFound for an id
// that is not the actor's or is already ended. The store cancels the
// deliveries still in flight in the same transaction, which is what makes
// the unsubscribe take effect now rather than at the next fan-out pass.
//
// The audience is deliberately NOT re-checked here: ending a follow must
// work exactly when access has been lost, otherwise a user who has been
// removed from a private project could not clean up the dead subscription
// — and the store's owner scoping already makes a foreign id answer
// ErrNotFound.
func (s *Service) Unsubscribe(ctx context.Context, actor domain.User, id string) error {
	if err := validateID(id); err != nil {
		return err
	}
	if err := s.store.DeleteSubscription(ctx, actor.ID, id); err != nil {
		return mapStoreError(err)
	}
	return nil
}

// validateID checks the subscription id shape at the boundary, so a
// non-UUID path segment answers 400/404 rather than a store error deep in
// a query cast.
func validateID(id string) error {
	var parsed pgtype.UUID
	if err := parsed.Scan(id); err != nil {
		return fmt.Errorf("%w: subscription id must be a UUID", ErrValidation)
	}
	return nil
}

// validateTarget checks the target's type and id shape. The audience is
// resolved separately.
func validateTarget(target events.Target) error {
	if err := events.ValidateTarget(target); err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	return nil
}

// validateFilters checks the event-type filter list with the same
// validator the webhook registry uses: the filter namespace is the
// canonical event-type namespace, and two validators for it would be two
// answers to the same question. Like the webhook registry, a filter naming
// an event type nothing produces is not an error — it is a filter that
// matches nothing (events.SubscribesTo), and rejecting it would make this
// endpoint a place where an event type has to be registered twice.
func validateFilters(filters []string) error {
	if err := events.ValidateEventFilters(filters); err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	return nil
}

// validateChannels checks the channel list against the closed vocabulary
// the schema enforces.
func validateChannels(channels []string) error {
	if err := events.ValidateChannels(channels); err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	return nil
}

// mapStoreError translates the store's sentinels into this package's.
func mapStoreError(err error) error {
	switch {
	case errors.Is(err, events.ErrSubscriptionNotFound):
		return ErrNotFound
	case errors.Is(err, events.ErrSubscriptionExists):
		return ErrExists
	case errors.Is(err, events.ErrSubscriptionLimit):
		return ErrLimit
	default:
		return fmt.Errorf("%w: %v", ErrStore, err)
	}
}
