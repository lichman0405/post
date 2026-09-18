package inbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
)

// MaxReadAnchors bounds how many entries one mark-read call may name. The
// statement it feeds is bounded by it (an array parameter), and a client
// has no reason to send more than a page — the cap is the server's
// protection, not a product rule, so exceeding it is a validation error
// rather than a silent truncation.
const MaxReadAnchors = 200

// Service orchestrates the research inbox use cases (T1003).
type Service struct {
	store Store
}

// NewService wires the service on the store port.
func NewService(store Store) *Service {
	return &Service{store: store}
}

// Entry is one inbox entry as the surface reads it: the aggregated
// delivery counters plus the link to the subject's page.
type Entry struct {
	events.InboxEntry
	// URL is the web app's route for the entry's target — the deep link
	// from the notification to what it is about. It is "" for a target
	// type with no route yet (see EntryURL).
	URL string
}

// View is one page of the inbox: the entries, the badge, and the view the
// page was answered for.
type View struct {
	Entries []Entry
	// UnreadEntries is the number of entries holding anything unread —
	// the badge — computed over the whole inbox, not over this page.
	UnreadEntries int
	// Filter echoes the view that was served (the default when the caller
	// named none), so a client cannot mistake a filtered page for the
	// whole inbox.
	Filter string
}

// List returns one page of the actor's inbox, newest entry first.
//
// An empty filter is the default view (unread). limit is clamped by
// events.InboxLimit: a caller that names none gets a page, and one that
// asks for more than the maximum gets the maximum rather than an error.
func (s *Service) List(ctx context.Context, actor domain.User, filter string, limit int) (View, error) {
	if filter == "" {
		filter = events.InboxFilterUnread
	}
	if err := events.ValidateInboxFilter(filter); err != nil {
		return View{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	entries, unread, err := s.store.InboxEntries(ctx, actor.ID, filter, events.InboxLimit(limit))
	if err != nil {
		return View{}, fmt.Errorf("%w: %v", ErrStore, err)
	}
	view := View{Entries: make([]Entry, 0, len(entries)), UnreadEntries: unread, Filter: filter}
	for _, e := range entries {
		view.Entries = append(view.Entries, Entry{InboxEntry: e, URL: EntryURL(e.TargetType, e.TargetID)})
	}
	return view, nil
}

// MarkRead marks the entries the caller read, named by the newest delivery
// of each. It returns how many deliveries were marked; marking an entry
// that is already read is a no-op that reports 0 rather than an error —
// the state the caller asked for is the state it is in.
//
// The anchor's SHAPE is checked here (a malformed id answers
// ErrValidation rather than reaching a query that would cast it); whether
// it is the caller's is the store's answer.
func (s *Service) MarkRead(ctx context.Context, actor domain.User, deliveryIDs []string) (int, error) {
	if len(deliveryIDs) == 0 {
		return 0, nil
	}
	if len(deliveryIDs) > MaxReadAnchors {
		return 0, fmt.Errorf("%w: at most %d entries per call", ErrValidation, MaxReadAnchors)
	}
	for _, id := range deliveryIDs {
		if err := validateAnchor(id); err != nil {
			return 0, err
		}
	}
	n, err := s.store.MarkInboxRead(ctx, actor.ID, deliveryIDs)
	if err != nil {
		if errors.Is(err, events.ErrInboxDeliveryNotFound) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("%w: %v", ErrStore, err)
	}
	return n, nil
}

// MarkAllRead marks the actor's whole unread inbox read — what the inbox
// is SERVING them, not every row they own — and returns how many deliveries
// were marked.
//
// The audience is the store's to resolve (MarkInboxAllRead), not this
// layer's: the rule that decides what may be shown decides what may be
// marked, and a second copy of it here is the drift T1002 warns about. A
// caller whose targets are all hidden marks 0 rows and is told so.
func (s *Service) MarkAllRead(ctx context.Context, actor domain.User) (int, error) {
	n, err := s.store.MarkInboxAllRead(ctx, actor.ID)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrStore, err)
	}
	return n, nil
}

// validateAnchor checks the delivery id shape at the boundary, so a
// non-UUID anchor answers a validation error rather than a store error
// deep in a query cast — the same shape check the subscription service
// makes on its ids.
func validateAnchor(id string) error {
	var parsed pgtype.UUID
	if err := parsed.Scan(id); err != nil {
		return fmt.Errorf("%w: delivery id must be a UUID", ErrValidation)
	}
	return nil
}

// EntryURL is the web app route an entry links to: the deep link from a
// notification to the thing it is about.
//
//   - a project's events belong to the project's activity tab, which is
//     where state transitions and contribution events read
//     (apps/web/app/(main)/projects/[id]/activity);
//   - an asset is addressed by its pid, never by a slug or its owning
//     project (internal/assets.URL — the pid is the persistent identity);
//   - a person is addressed by their user id
//     (apps/web/app/(main)/users/[id], T0801).
//
// The two remaining target types answer "" — no href at all — because the
// route does not exist yet: knowledge objects are T0805's surface and
// organizations are T0808's, and a link to a route that does not exist is
// a lie in the shape of an href (the rule internal/application/explore
// states for its own sections). An entry for one of them still carries its
// target type and id, so the surface renders identity instead of a dead
// link.
func EntryURL(targetType, targetID string) string {
	switch targetType {
	case events.TargetTypeProject:
		return "/projects/" + targetID + "/activity"
	case events.TargetTypeAsset:
		return "/assets/" + targetID
	case events.TargetTypeUser:
		return "/users/" + targetID
	default:
		return ""
	}
}
