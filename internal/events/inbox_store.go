package events

import (
	"context"
	"fmt"

	"github.com/lichman0405/post/internal/observability"
)

// The inbox reads and writes (T1003). They live on SubscriptionStore
// because that type already owns subscription_deliveries — the fan-out's
// writes and the audience rule — and a second owner of the same table is
// how the rows the inbox lists and the rows the fan-out withdraws drift
// apart.
//
// Every statement here is scoped by user_id first, for the same reason the
// subscription reads are: an inbox is one person's, and a foreign id must
// answer the same thing an unknown one does.

// inboxTargetsQuery lists the distinct targets the caller has a delivered
// web row for: the whole inbox, NOT one page of it.
//
// Decision 3 resolves the audience over this list rather than over the
// page about to be served, because the badge is the inbox's and not the
// page's: a hidden target has to be out of scope before the entries are
// grouped, or an entry that is never served would still be counted in
// unread_entries.
//
// The list is NOT bounded by what the caller follows today, and no schema
// limit bounds it: MaxSubscriptionsPerUser counts LIVE subscriptions
// (subscription_store.go: "WHERE user_id = $1 AND deleted_at IS NULL"),
// while an unsubscribe is a soft delete that keeps the row and, with it,
// every delivery it produced. Follow / unfollow / follow again therefore
// accumulates distinct targets, so this list — and the audience pass over
// it — is proportional to the caller's delivery HISTORY, not to their live
// subscriptions. It cannot be capped for free either: the badge is
// computed over exactly this list (decision 3), so a cap here would be a
// cap on the badge, which is the one number this query exists to make
// honest.
const inboxTargetsQuery = `
SELECT DISTINCT d.target_type, d.target_id
FROM subscription_deliveries d
WHERE d.user_id = $1::uuid
  AND d.channel = 'web'
  AND d.status = 'delivered'`

// inboxEntriesQuery aggregates one subscriber's web deliveries into inbox
// entries (see the file comment in inbox.go for what an entry is).
//
// The scope is the delivered WEB rows of targets the caller can still
// read: a 'pending' row is an email the digest sender has not sent, and a
// 'cancelled' row is one the T1002 revocation path withdrew — neither was
// ever in the inbox, so neither may be counted in an entry or kept alive
// as a stale one. Because the aggregation is a GROUP BY over live rows
// rather than a stored entry, a delivery the fan-out cancels stops
// counting toward its entry on the next read — and an entry with no rows
// left is not served at all, which is also what the audience scope below
// does to every entry of a target the caller may no longer read.
//
// $5 and $6 are the other half of that scope: the targets InboxEntries
// resolved as visible, as two parallel arrays. The filter is applied to
// the ROWS the entries are grouped from, not to the entries afterwards,
// which is what makes decision 3 hold in both directions at once — a
// hidden target contributes no entry to either view AND no entry to the
// badge, so the page and the badge cannot disagree about it.
//
// $2 is the view (InboxFilterUnread / InboxFilterAll) and filters the
// ENTRIES, not the deliveries the entries are computed from: an entry's
// count must be the same in both views, so the unread view cannot be
// answered by hiding the read half of a half-read entry.
//
// $3 is the page size and $4 the aggregation window in seconds — the
// window is a parameter so InboxAggregationWindow stays the one place it
// is defined.
//
// unread_entries is the badge: the number of visible entries with
// something unread in them, computed over every visible entry (not just
// the page), so a truncated page still reports the true count.
const inboxEntriesQuery = `
WITH scoped AS (
  SELECT d.id, d.event_id, d.event_type, d.target_type, d.target_id, d.created_at, d.read_at,
         date_bin(make_interval(secs => $4::int), d.created_at, timestamptz 'epoch') AS window_start
  FROM subscription_deliveries d
  WHERE d.user_id = $1::uuid
    AND d.channel = 'web'
    AND d.status = 'delivered'
    AND (d.target_type, d.target_id) IN (
      SELECT v.target_type, v.target_id
      FROM unnest($5::text[], $6::text[]) AS v(target_type, target_id)
    )
),
entries AS (
  SELECT target_type, target_id, event_type, window_start,
         count(*) AS n,
         count(*) FILTER (WHERE read_at IS NULL) AS unread,
         min(created_at) AS first_at,
         max(created_at) AS last_at,
         (array_agg(event_id ORDER BY created_at DESC, id DESC))[1] AS latest_event_id,
         (array_agg(id ORDER BY created_at DESC, id DESC))[1] AS latest_delivery_id
  FROM scoped
  GROUP BY target_type, target_id, event_type, window_start
),
totals AS (
  SELECT count(*) FILTER (WHERE unread > 0) AS unread_entries FROM entries
)
SELECT e.target_type, e.target_id, e.event_type, e.window_start,
       e.n, e.unread, e.first_at, e.last_at, e.latest_event_id, e.latest_delivery_id,
       t.unread_entries
FROM entries e
CROSS JOIN totals t
WHERE $2 = 'all' OR e.unread > 0
ORDER BY e.last_at DESC, e.latest_delivery_id DESC
LIMIT $3`

// InboxEntries returns one page of the caller's inbox, newest entry
// first, plus the total number of entries holding anything unread (the
// badge, computed before the page is cut).
//
// Before the entries are read, the caller's WHOLE inbox is resolved
// through the audience rule (inboxVisibleTargets, decision 3): a target
// they may no longer read is out of scope, so its entries are not served
// in either view and not counted in the badge. Resolving the page instead
// would let a hidden entry keep a place in the badge it no longer has in
// the list.
//
// The labels of the page's targets are filled in as well. Every one of
// them passed that same rule a statement ago, so a name is never resolved
// for a target that was just hidden — the audience is decided once, in
// one place, and naming is what is left.
func (s *SubscriptionStore) InboxEntries(ctx context.Context, userID, filter string, limit int) ([]InboxEntry, int, error) {
	targetTypes, targetIDs, err := s.inboxVisibleTargets(ctx, userID)
	if err != nil {
		return nil, 0, err
	}
	rows, err := s.pool.Query(ctx, inboxEntriesQuery, userID, filter, limit,
		int(InboxAggregationWindow.Seconds()), targetTypes, targetIDs)
	if err != nil {
		return nil, 0, fmt.Errorf("events: inbox entries: %w", err)
	}
	defer rows.Close()

	out := []InboxEntry{}
	unreadEntries := 0
	for rows.Next() {
		var e InboxEntry
		if err := rows.Scan(&e.TargetType, &e.TargetID, &e.EventType, &e.WindowStart,
			&e.Count, &e.Unread, &e.FirstAt, &e.LastAt, &e.LatestEventID,
			&e.LatestDeliveryID, &unreadEntries); err != nil {
			return nil, 0, fmt.Errorf("events: inbox entries: scan: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("events: inbox entries: iterate: %w", err)
	}
	if err := inboxLabels(ctx, s.pool, out); err != nil {
		return nil, 0, err
	}
	return out, unreadEntries, nil
}

// inboxVisibleTargets resolves every target in the caller's inbox through
// the audience rule and returns the ones still visible to them as two
// parallel arrays for inboxEntriesQuery.
//
// This is decision 3, and it is fail-closed: a target whose audience is
// AudienceNone is not returned, so it takes no part in the read. The two
// weaker alternatives are both worse. Keeping the entry and blanking its
// name leaves a row whose whole content is an id — it still says a target
// is there, and still sits in the badge, which is the leak with a smaller
// font. Dropping only the page's targets leaves the badge counting
// entries nobody will be served.
//
// The rule is not restated here: each distinct target goes through
// TargetAudience, the one rule the subscribe path and the fan-out share.
// The cost is one audience lookup per distinct target the caller has been
// delivered something for — over their whole delivery history, not over
// their live subscriptions, because an unsubscribe is a soft delete (see
// inboxTargetsQuery) — rather than one lookup per delivery or per page. A
// batch copy of the rule is the second implementation T1002 warns about.
//
// A lookup that FAILS is not an audience of none, for the same reason the
// fan-out does not treat it as one: "no relationship" is a decision,
// "could not look" is not. The error is returned and the read fails,
// rather than an inbox that quietly empties itself on a hiccup.
func (s *SubscriptionStore) inboxVisibleTargets(ctx context.Context, userID string) ([]string, []string, error) {
	rows, err := s.pool.Query(ctx, inboxTargetsQuery, userID)
	if err != nil {
		return nil, nil, fmt.Errorf("events: inbox targets: %w", err)
	}
	targets := []Target{}
	for rows.Next() {
		var t Target
		if err := rows.Scan(&t.Type, &t.ID); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("events: inbox targets: scan: %w", err)
		}
		targets = append(targets, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, fmt.Errorf("events: inbox targets: iterate: %w", err)
	}

	targetTypes := []string{}
	targetIDs := []string{}
	filtered := false
	for _, t := range targets {
		level, err := s.TargetAudience(ctx, s.pool, t, userID)
		if err != nil {
			return nil, nil, fmt.Errorf("events: inbox audience: %w", err)
		}
		if level == AudienceNone {
			// The refusal no HTTP status can carry: the target is dropped
			// and the request still answers 200 with a shorter list, so a
			// reader — and the edge — cannot tell this from "nothing there".
			// Recorded here because here is the only place the decision is
			// visible (docs/26 §3 permission denied rates). See
			// observability.DecisionFiltered.
			filtered = true
			continue
		}
		targetTypes = append(targetTypes, t.Type)
		targetIDs = append(targetIDs, t.ID)
	}
	if filtered {
		// ONE increment per shortened read, not one per dropped target. The
		// family's other two decisions are counted once per refused REQUEST,
		// and a per-target increment here would make `sum by (decision)` add
		// requests to targets — a read that hid three rows would weigh three
		// times as much as a read that hid one, which is not a rate of
		// anything. The count this answers is "how many answers came back
		// shorter than they should", and that is one per read.
		observability.Default().ObservePermissionDenial("inbox", observability.DecisionFiltered)
	}
	return targetTypes, targetIDs, nil
}

// inboxLabels fills in the display identity of the entries' targets, in
// place. It answers for the targets of ONE page (the caller's entries),
// and it is the caller's job to pass only entries whose targets are the
// caller's own AND visible to them: the audience decision is not made
// here, it is made once for the whole inbox in inboxVisibleTargets, so a
// page can never be labelled under a different rule than it was selected
// with.
//
// What is left here is the naming, and it is a read of the target rather
// than a copy taken when the delivery was fanned out: a delivery is a
// pointer to a target the subscriber could see THEN, and the target's
// present name is not part of it. A target with no label (a type with no
// surface yet) is left "" — which is why this cannot be the audience
// decision: "" must keep meaning "nobody asked for this name", not "you
// may not see it".
func inboxLabels(ctx context.Context, db DBTX, entries []InboxEntry) error {
	if len(entries) == 0 {
		return nil
	}
	distinct := make([]Target, 0, len(entries))
	seen := map[Target]bool{}
	for _, e := range entries {
		t := e.Target()
		if seen[t] {
			continue
		}
		seen[t] = true
		distinct = append(distinct, t)
	}
	if len(distinct) == 0 {
		return nil
	}

	labels := map[Target]string{}
	for _, targetType := range []string{TargetTypeProject, TargetTypeAsset, TargetTypeUser, TargetTypeOrganization} {
		ids := []string{}
		for _, t := range distinct {
			if t.Type == targetType {
				ids = append(ids, t.ID)
			}
		}
		if len(ids) == 0 {
			continue
		}
		query, ok := inboxLabelQueries[targetType]
		if !ok {
			continue
		}
		rows, err := db.Query(ctx, query, ids)
		if err != nil {
			return fmt.Errorf("events: inbox labels for %s: %w", targetType, err)
		}
		for rows.Next() {
			var id, label string
			if err := rows.Scan(&id, &label); err != nil {
				rows.Close()
				return fmt.Errorf("events: inbox labels for %s: scan: %w", targetType, err)
			}
			labels[Target{Type: targetType, ID: id}] = label
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("events: inbox labels for %s: iterate: %w", targetType, err)
		}
	}

	for i := range entries {
		entries[i].TargetLabel = labels[entries[i].Target()]
	}
	return nil
}

// inboxLabelQueries is the display name of one target id, per target type.
//
// A target type with no surface (knowledge, until T0805 publishes one) has
// no entry here, and no label is a truthful answer for it: the caller gets
// the id the delivery named. Each query falls back to the target's stable
// slug rather than answering with nothing, because "" means "the caller
// may not see this target" and an empty column is not that.
var inboxLabelQueries = map[string]string{
	TargetTypeProject: `SELECT id::text, coalesce(nullif(name, ''), slug) FROM projects WHERE id = ANY($1::uuid[])`,
	TargetTypeAsset:   `SELECT pid, coalesce(nullif(title, ''), slug) FROM research_assets WHERE pid = ANY($1::text[])`,
	TargetTypeUser:    `SELECT id::text, handle FROM users WHERE id = ANY($1::uuid[])`,
	TargetTypeOrganization: `SELECT id::text, coalesce(nullif(name, ''), slug) FROM organizations
	                          WHERE id = ANY($1::uuid[])`,
}

// inboxAnchors selects the deliveries a mark-read call named, owner-scoped
// and limited to the rows that can be read at all (the delivered web
// rows — the same scope the entries are computed from, so an anchor can
// never name a row that is not in an entry).
const inboxAnchors = `
SELECT id::text FROM subscription_deliveries
WHERE id = ANY($1::uuid[]) AND user_id = $2::uuid
  AND channel = 'web' AND status = 'delivered'`

// markInboxReadAnchored marks every unread delivery of the anchors'
// entries that is no older than the anchor itself.
//
// The bound is what makes "read" mean "shown": the caller names the newest
// delivery it saw, and everything up to it in the same entry is marked
// while a delivery that arrived after the read started is not. Marking the
// whole entry unconditionally would be one line shorter and would silently
// mark unseen notifications as read.
//
// The entry a row belongs to is compared as the same tuple plus the same
// window bin — the aggregation's own key, computed by the same expression
// as the read (date_bin with the same stride and origin), so the two agree
// by construction rather than by two implementations of one rule.
const markInboxReadAnchored = `
WITH anchors AS (
  SELECT d.target_type, d.target_id, d.event_type, d.created_at, d.id,
         date_bin(make_interval(secs => $3::int), d.created_at, timestamptz 'epoch') AS window_start
  FROM subscription_deliveries d
  WHERE d.id = ANY($1::uuid[]) AND d.user_id = $2::uuid
    AND d.channel = 'web' AND d.status = 'delivered'
)
UPDATE subscription_deliveries d
SET read_at = now()
FROM anchors a
WHERE d.user_id = $2::uuid
  AND d.channel = 'web' AND d.status = 'delivered' AND d.read_at IS NULL
  AND d.target_type = a.target_type
  AND d.target_id = a.target_id
  AND d.event_type = a.event_type
  AND date_bin(make_interval(secs => $3::int), d.created_at, timestamptz 'epoch') = a.window_start
  AND (d.created_at, d.id) <= (a.created_at, a.id)`

// markInboxAllRead marks one subscriber's unread delivered web rows read —
// the rows of the targets they are still served, and only those.
//
// It is deliberately not "the entries of the current page": the caller
// asked for their inbox, and a mark that left rows the badge still counts
// unread would make the button a lie. What bounds it is not the page but
// the audience rule: read-all clears exactly what the read serves
// (inboxVisibleTargets, decision 3), because the badge it is clearing is
// computed over exactly those entries.
//
// Marking wider than that is not a harmless over-delivery — it is a
// one-way write onto rows the caller cannot see. read_at has no inverse,
// and a delivery under a hidden target is invisible by the same rule that
// keeps it out of both views and out of the badge, so a mark reaching it
// is a state change the caller can neither see nor undo: the badge would
// not move, and when the target came back the entry would arrive already
// read although it was never shown. This is the write-side half of
// inbox.go rule 2, whose reason is the same sentence: reading marks what
// was shown.
//
// $2/$3 are the visible targets as two parallel arrays, exactly as
// inboxEntriesQuery takes them, so the write and the read are scoped by
// one resolution instead of two that could drift. A caller with nothing
// visible marks 0 rows and succeeds: empty arrays make the filter match
// nothing, which is the answer the read gives the same caller.
const markInboxAllRead = `
UPDATE subscription_deliveries d
SET read_at = now()
WHERE d.user_id = $1::uuid
  AND d.channel = 'web' AND d.status = 'delivered' AND d.read_at IS NULL
  AND (d.target_type, d.target_id) IN (
    SELECT v.target_type, v.target_id
    FROM unnest($2::text[], $3::text[]) AS v(target_type, target_id)
  )`

// MarkInboxRead marks one entry's unread deliveries read, anchored at the
// deliveries the caller names, and returns how many rows it marked.
//
// An anchor that is not the caller's (or does not exist, or is not a
// delivered web row) fails the whole call with ErrInboxDeliveryNotFound
// before anything is written — the two answers are the same answer, as
// everywhere else in this package. Repeating a call is a no-op that
// reports 0: the rows are already read, which is the state the caller
// asked for.
func (s *SubscriptionStore) MarkInboxRead(ctx context.Context, userID string, deliveryIDs []string) (int, error) {
	if len(deliveryIDs) == 0 {
		return 0, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("events: mark inbox read: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	rows, err := tx.Query(ctx, inboxAnchors, deliveryIDs, userID)
	if err != nil {
		return 0, fmt.Errorf("events: mark inbox read: resolve anchors: %w", err)
	}
	found := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("events: mark inbox read: scan anchor: %w", err)
		}
		found[id] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, fmt.Errorf("events: mark inbox read: iterate anchors: %w", err)
	}
	for _, id := range deliveryIDs {
		if !found[id] {
			return 0, ErrInboxDeliveryNotFound
		}
	}

	tag, err := tx.Exec(ctx, markInboxReadAnchored, deliveryIDs, userID, int(InboxAggregationWindow.Seconds()))
	if err != nil {
		return 0, fmt.Errorf("events: mark inbox read: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("events: mark inbox read: commit: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// MarkInboxAllRead marks the caller's whole served unread inbox read and
// returns how many rows it marked.
//
// The audience is resolved BEFORE the update and a failure is returned
// rather than swallowed: a lookup that fails writes nothing at all, which
// is the same rule the read keeps (inboxVisibleTargets: "could not look"
// is not "no relationship"). Nothing is marked on a guess.
func (s *SubscriptionStore) MarkInboxAllRead(ctx context.Context, userID string) (int, error) {
	targetTypes, targetIDs, err := s.inboxVisibleTargets(ctx, userID)
	if err != nil {
		return 0, err
	}
	tag, err := s.pool.Exec(ctx, markInboxAllRead, userID, targetTypes, targetIDs)
	if err != nil {
		return 0, fmt.Errorf("events: mark inbox read: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
