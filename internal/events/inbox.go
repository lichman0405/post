package events

import (
	"fmt"
	"time"
)

// The research inbox (T1003, docs/18 §3-4): the WEB channel's read model
// over the delivery rows the subscription fan-out writes (T1002).
//
// A delivery row is the notification — T1002 made exactly one row per
// (subscription, event, channel) and called the web channel's row
// 'delivered' the moment it exists, "the inbox row IS the delivery". What
// this file adds is the two things the rows alone do not answer:
//
//  1. AGGREGATION. "默认避免通知每个低级 commit" (docs/18 §3) is a rule
//     about granularity, not about dropping events: a commit is worth
//     knowing about, and fifty commits in one hour are worth knowing about
//     ONCE. An inbox ENTRY is therefore the deliveries of one target and
//     one event type inside one fixed window of InboxAggregationWindow,
//     rendered as one row with a count — not fifty rows, and not a
//     summarization that loses the identity of what happened. The entry
//     keeps the newest delivery's event id so the surface can still link
//     to the event itself, and the count so it can say "and 49 more".
//
//     The window is fixed clock time, not a gap between deliveries, and
//     that is deliberate: entry membership has to be a pure function of
//     the delivery rows, because the read is a GROUP BY over them rather
//     than a stored object (see migration 00079 for why read state lives
//     on the delivery). A gap-based rule would make an entry's boundaries
//     depend on the order rows were inserted, so a delivery that arrived
//     late — or one the revocation path cancelled — could silently move
//     earlier deliveries into a different entry than the one a subscriber
//     already read.
//
//  2. READ/UNREAD. read_at on the delivery row; an entry is unread while
//     any of its deliveries is. Marking read is scoped to what was SHOWN:
//     the caller names the delivery it read and everything older in that
//     same entry is marked, so a notification that arrives a second later
//     is not swept into a read it never had.
//
//  3. AUDIENCE. The read is fail-closed on the caller's CURRENT relation
//     to each target: a target that resolves to AudienceNone (a project
//     they were removed from and made private, a deactivated account)
//     takes its entries out of the inbox entirely — out of both views and
//     out of the badge. The audience is resolved over the WHOLE inbox
//     before any page is cut, so the row a caller sees and the number
//     they see are computed from the same set (inboxVisibleTargets). A
//     delivery row is not deleted by this and not rewritten: it is the
//     caller's own record that something was delivered, and the rule is
//     about what may still be rendered from it.
//
//     The WRITE obeys the same rule, in the same resolution: read-all
//     marks exactly the rows the read would serve (MarkInboxAllRead), so a
//     row the caller cannot see is never marked read on their behalf. What
//     a caller may be shown and what may be marked read are two halves of
//     one question, and answering them from one inboxVisibleTargets is
//     what keeps the badge, the page and the button from disagreeing.

// InboxAggregationWindow is how much wall-clock time one inbox entry
// spans: deliveries of the same (target, event type) inside one window of
// this size are ONE entry.
//
// One hour is the L1 choice, and it is a compromise between the two ways
// an inbox goes wrong. Too small and a working session's worth of commits
// is still a list (the bombardment docs/18 §3 forbids); too large and
// genuinely separate pieces of work — a morning's commits and an
// afternoon's — arrive as one row that cannot be told apart. Within the
// window nothing is lost: the entry carries its delivery count, the
// window's start and end, and the newest event's identity.
const InboxAggregationWindow = time.Hour

// The inbox views. "unread" is the default: the inbox is a to-do list
// first, and the rows already read are history it can still show on
// request.
const (
	// InboxFilterUnread lists entries with at least one unread delivery.
	InboxFilterUnread = "unread"
	// InboxFilterAll lists every entry, read ones included.
	InboxFilterAll = "all"
)

// Inbox read bounds. The default is what one page renders; the maximum is
// what a caller may ask for. A limit is not a horizon — nothing is
// excluded from the read by it, only from the page.
const (
	DefaultInboxLimit = 50
	MaxInboxLimit     = 200
)

// ErrInboxFilter: the view name is not one of the inbox's.
var ErrInboxFilter = fmt.Errorf("events: invalid inbox filter")

// ErrInboxDeliveryNotFound: no delivered web delivery with that id belongs
// to the caller. "not yours" and "does not exist" answer identically, the
// same existence-hiding rule the subscription reads keep.
var ErrInboxDeliveryNotFound = fmt.Errorf("events: inbox delivery not found")

// InboxEntry is one aggregated inbox row: the deliveries of one user, one
// target and one event type inside one window (see the file comment).
//
// Its identity is the tuple (TargetType, TargetID, EventType,
// WindowStart) — that is what a caller is looking at, and it is stable
// across reads, because it is derived from the deliveries rather than
// assigned. LatestDeliveryID is the newest delivery IN the entry and is
// the anchor a mark-read call names; it moves as new deliveries arrive,
// which is the point: reading marks what was shown.
type InboxEntry struct {
	TargetType string
	TargetID   string
	// TargetLabel is the target's display identity — a project name, an
	// asset title, a person's handle, an organization's name — read at
	// render time rather than copied into the delivery when it was fanned
	// out. It is "" only for a target type that has no naming query yet
	// (see inboxLabelQueries); a target the caller may no longer read
	// produces no entry at all, so its name never reaches this struct.
	TargetLabel string
	EventType   string
	WindowStart time.Time
	// Count is how many deliveries the entry aggregates.
	Count int
	// Unread is how many of them are unread. Every delivery counted here
	// is a delivered web row (status 'delivered') whose target the caller
	// can still read: a cancelled row is not in the entry at all, and a
	// target whose audience no longer includes the caller takes its whole
	// entry out of the inbox rather than shrinking it
	// (inboxVisibleTargets, decision 3).
	Unread  int
	FirstAt time.Time
	LastAt  time.Time
	// LatestEventID is the research event of the newest delivery — the
	// identity a surface links to the event itself with.
	LatestEventID string
	// LatestDeliveryID is the newest delivery row, and the anchor for
	// marking this entry read.
	LatestDeliveryID string
}

// Read reports whether the entry has been read. An entry with no unread
// deliveries is read — including an entry that was read and then grew no
// further.
func (e InboxEntry) Read() bool { return e.Unread == 0 }

// Target renders the entry's aggregation key minus the window.
func (e InboxEntry) Target() Target {
	return Target{Type: e.TargetType, ID: e.TargetID}
}

// ValidateInboxFilter checks an inbox view name at the boundary, so an
// unknown view answers a validation error rather than silently serving a
// different list than the caller asked for.
func ValidateInboxFilter(filter string) error {
	switch filter {
	case InboxFilterUnread, InboxFilterAll:
		return nil
	}
	return fmt.Errorf("%w: unknown inbox filter %q (want %q or %q)",
		ErrInboxFilter, filter, InboxFilterUnread, InboxFilterAll)
}

// InboxLimit clamps a requested page size into the legal range: a
// non-positive request means the default (a caller that names no limit
// wants a page, not zero rows), and anything above the maximum is capped
// rather than refused, because the cap is the server's protection and not
// a client error.
func InboxLimit(requested int) int {
	if requested <= 0 {
		return DefaultInboxLimit
	}
	if requested > MaxInboxLimit {
		return MaxInboxLimit
	}
	return requested
}
