package inbox

import (
	"context"

	"github.com/lichman0405/post/internal/events"
)

// Store is the inbox persistence port, implemented by
// events.SubscriptionStore over subscription_deliveries. Every read and
// write is owner-scoped by userID: the store never returns another user's
// delivery, and an anchor that is not the caller's answers
// events.ErrInboxDeliveryNotFound rather than disclosing that it exists.
type Store interface {
	// InboxEntries returns one page of the caller's inbox entries, newest
	// first, plus the number of entries holding anything unread (computed
	// before the page is cut). filter is events.InboxFilterUnread or
	// events.InboxFilterAll; limit is the page size.
	InboxEntries(ctx context.Context, userID, filter string, limit int) ([]events.InboxEntry, int, error)
	// MarkInboxRead marks one entry's unread deliveries read, anchored at
	// the given deliveries, and returns how many rows it marked. An anchor
	// that is not the caller's answers events.ErrInboxDeliveryNotFound.
	MarkInboxRead(ctx context.Context, userID string, deliveryIDs []string) (int, error)
	// MarkInboxAllRead marks every unread delivery in the caller's inbox
	// read and returns how many rows it marked.
	MarkInboxAllRead(ctx context.Context, userID string) (int, error)
}
