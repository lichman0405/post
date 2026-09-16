package subscriptions

import (
	"context"

	"github.com/lichman0405/post/internal/events"
)

// Store is the persistence port for subscriptions and the audience rule,
// implemented by events.SubscriptionStore. Every read and write is
// owner-scoped by userID: the store never returns another user's
// subscription, and an id that is not the caller's answers
// events.ErrSubscriptionNotFound rather than disclosing that it exists.
type Store interface {
	// CreateSubscription registers one live subscription, or answers
	// events.ErrSubscriptionExists / events.ErrSubscriptionLimit.
	CreateSubscription(ctx context.Context, userID string, target events.Target, filters, channels []string) (events.Subscription, error)
	// ListSubscriptions returns the actor's live subscriptions, newest
	// first; a non-empty targetType/targetID filters to one target.
	ListSubscriptions(ctx context.Context, userID, targetType, targetID string) ([]events.Subscription, error)
	// GetSubscription returns one owned live subscription or
	// events.ErrSubscriptionNotFound.
	GetSubscription(ctx context.Context, userID, id string) (events.Subscription, error)
	// UpdateSubscription replaces filters and channels (a full
	// replacement), or answers events.ErrSubscriptionNotFound.
	UpdateSubscription(ctx context.Context, userID, id string, filters, channels []string) (events.Subscription, error)
	// DeleteSubscription unsubscribes: the row is soft-deleted and every
	// delivery still in flight is cancelled in the same transaction, or
	// events.ErrSubscriptionNotFound.
	DeleteSubscription(ctx context.Context, userID, id string) error
	// TargetAudienceFor resolves how userID is related to target right
	// now — the rule the subscribe path and the fan-out share.
	TargetAudienceFor(ctx context.Context, target events.Target, userID string) (events.AudienceLevel, error)
}
