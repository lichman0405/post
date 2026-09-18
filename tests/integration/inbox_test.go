// Task T1003 — the required test "inbox e2e".
//
// The research inbox driven end to end over the production composition:
// real PostgreSQL, the real transactional outbox (events.Record → the
// dispatcher → the subscription fan-out), the real HTTP surface
// (cmd/api/inboxhttp behind the real authhttp session + CSRF guard, with
// cmd/api/subscriptionshttp mounted beside it exactly as cmd/api/main.go
// mounts them), and real accounts created over the wire.
//
// What this file pins, requirement by requirement:
//
//  1. 聚合 meaningful events — the deliveries of one target and one event
//     type inside one window are ONE entry carrying a count, and the
//     events are all still there underneath it. The "forty per-commit
//     deliveries are one entry" case asserts the forty delivered rows and
//     the single entry in the same breath: the difference between
//     aggregating and dropping is the whole point.
//
//  2. read/unread — an entry is read or unread, marking is scoped to what
//     was SHOWN, and read-all is idempotent.
//
//  3. deep links — every entry carries the route of its subject
//     (/projects/{id}/activity, /assets/{pid} for the asset the event
//     names only through its payload).
//
// The acceptance criterion — 低级每 commit 不默认轰炸用户 — as the
// observable claim it is: a subscriber who follows a project and receives
// forty commits is shown ONE entry, not forty, without one delivery being
// suppressed (T1002's subscription promised those events, the fan-out wrote
// them, and the inbox aggregates them rather than hiding what was asked
// for).
//
// The failure modes this suite is built to catch, and where:
//
//   - the aggregation disappearing (one entry per delivery — the
//     bombardment): the count-40 assertions.
//   - the window meaning nothing (deliveries of one target and type
//     collapsing into one entry whatever their age):
//     TestInboxWindowAndAnchorSplitTheMark.
//   - read state leaking (a mark that marks more than it was shown):
//     TestInboxWindowAndAnchorSplitTheMark's two phases — the older
//     window's rows and the deliveries newer than the anchor must both
//     stay unread.
//   - an entry served for a target the caller may no longer read, or a
//     badge that counts one: TestInboxIsOwnerScopedAndHidesWhatItMayNotName
//     (the entry must be gone) and TestInboxBadgeCountsOnlyEntriesItServes
//     (its count must be gone too, while a surviving entry still reports).
//   - a read-all that reaches past what the read serves (read state
//     written onto a target the caller may no longer read, invisible when
//     it happens and already-read when the target returns):
//     TestInboxReadAllMarksOnlyWhatItServes.
//   - another subscriber's rows entering the caller's entry counts:
//     TestInboxBadgeCountsOnlyEntriesItServes (bob follows the target alice
//     is counting, so his two rows would have to show up as four).
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/inboxhttp"
	"github.com/lichman0405/post/cmd/api/subscriptionshttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/inbox"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

// inboxTaskID namespaces this task's test databases (test_T1003_<run_id>).
const inboxTaskID = "T1003"

// The canonical event types this suite publishes
// (specs/events/event-types.yaml).
const (
	eventTypeStateCommitted = "state.committed"
	eventTypePRMerged       = "pull_request.merged"
	eventTypePROpened       = "pull_request.opened"
	eventTypeRelease        = "release.published"
)

// --------------------------------------------------------------------------
// Fixture

// inboxFixture is one real database with the production notification
// pipeline wired over it, plus the rows the cases move around: one public
// project with one published asset, an account that follows them, and a
// second account that follows nothing.
type inboxFixture struct {
	ctx        context.Context
	pool       *pgxpool.Pool
	store      *events.SubscriptionStore
	dispatcher *events.Dispatcher
	fanout     *events.SubscriptionFanOut
	server     string

	alice, bob             domain.User
	aliceClient, bobClient *testUserClient

	projectID   string
	projectName string
	assetPID    string
	assetTitle  string
}

func newInboxFixture(t *testing.T, ctx context.Context) *inboxFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), inboxTaskID)

	authAPI := authhttp.New(authhttp.Deps{
		Users:    persistence.NewCredentialStore(pool),
		Sessions: memstore.NewSessions(),
		Limiter:  memstore.NewLimiter(),
		Cfg: authn.Config{
			WebOrigin:          "http://web.test",
			SessionTTL:         time.Hour,
			LoginLimitPerEmail: 1000,
			LoginLimitPerIP:    10000,
			LoginWindow:        time.Minute,
			SignupLimitPerIP:   10000,
		},
	})
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	// The graph cmd/api/main.go builds: one subscription store shared by the
	// subscription surface and the inbox surface.
	store := events.NewSubscriptionStore(pool)
	subsAPI := subscriptionshttp.New(subscriptionshttp.Deps{Store: store})
	mux.Handle("/api/v1/subscriptions", subsAPI.Routes())
	mux.Handle("/api/v1/subscriptions/", subsAPI.Routes())
	inboxAPI := inboxhttp.New(inboxhttp.Deps{Store: store})
	mux.Handle("/api/v1/inbox", inboxAPI.Routes())
	mux.Handle("/api/v1/inbox/", inboxAPI.Routes())
	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)

	f := &inboxFixture{
		ctx:         ctx,
		pool:        pool,
		store:       store,
		dispatcher:  events.NewDispatcher(pool, events.WithLogger(silentLogger())),
		fanout:      events.NewSubscriptionFanOut(pool, events.WithSubscriptionFanOutLogger(silentLogger())),
		server:      ts.URL,
		projectName: "MOF Screening",
		assetPID:    "01j9z6k3m4n5p6q7r8s9t0v1w2",
		assetTitle:  "MOF dataset",
	}
	f.aliceClient, f.alice = f.signup(t, "inbox-alice@example.com", "inboxalice")
	f.bobClient, f.bob = f.signup(t, "inbox-bob@example.com", "inboxbob")

	f.projectID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO projects (slug, name, purpose, visibility, created_by)
		 VALUES ('inbox-mof', $1, 'T1003 inbox fixture', 'public', $2) RETURNING id`,
		f.projectName, f.alice.ID)
	if _, err := pool.Exec(ctx,
		`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, 'owner')`,
		f.projectID, f.alice.ID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	mustQueryUUID(t, ctx, pool,
		`INSERT INTO research_assets (asset_type, slug, title, origin_project_id, pid)
		 VALUES ('dataset', 'inbox-ds', $1, $2, $3) RETURNING id`,
		f.assetTitle, f.projectID, f.assetPID)
	return f
}

// signup creates one account through the real signup endpoint (a real users
// row, a session in the in-memory store) and returns the browser and the
// user behind it.
func (f *inboxFixture) signup(t *testing.T, email, handle string) (*testUserClient, domain.User) {
	t.Helper()
	uc, id := signup(t, f.server, email, handle)
	return uc, domain.User{ID: id, Handle: handle, Email: email, DisplayName: handle}
}

// follow subscribes one browser to one target over the real HTTP surface —
// the only way a subscription is created in production.
func (f *inboxFixture) follow(t *testing.T, uc *testUserClient, targetType, targetID string, channels ...string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"target_type": targetType, "target_id": targetID, "channels": channels,
	})
	if err != nil {
		t.Fatalf("marshal subscribe body: %v", err)
	}
	resp := uc.do(t, http.MethodPost, "/api/v1/subscriptions", string(body))
	mustStatus(t, resp, http.StatusCreated)
}

// record writes one event into the transactional outbox.
func (f *inboxFixture) record(t *testing.T, e events.Event) {
	t.Helper()
	if err := events.Record(f.ctx, f.pool, e); err != nil {
		t.Fatalf("record %s: %v", e.EventType, err)
	}
}

// publish runs both production passes once: the dispatcher publishes the
// outbox rows into research_events, the fan-out consumes them into delivery
// rows. Both claim up to 100 rows per pass (their defaults), and no case
// here publishes more than that.
func (f *inboxFixture) publish(t *testing.T) {
	t.Helper()
	if _, err := f.dispatcher.RunOnce(f.ctx); err != nil {
		t.Fatalf("dispatcher RunOnce: %v", err)
	}
	if _, err := f.fanout.RunOnce(f.ctx); err != nil {
		t.Fatalf("fanout RunOnce: %v", err)
	}
}

// commits records and publishes n state.committed events on the fixture
// project — the per-commit traffic docs/18 §3 is about.
func (f *inboxFixture) commits(t *testing.T, n int) {
	t.Helper()
	for i := range n {
		f.record(t, events.Event{
			EventType:     eventTypeStateCommitted,
			ActorID:       f.alice.ID,
			ProjectID:     f.projectID,
			Visibility:    events.VisibilityPublic,
			CorrelationID: fmt.Sprintf("commit-%d", i),
			Payload:       json.RawMessage(`{"commit_sha":"0ff1ce"}`),
		})
	}
	f.publish(t)
}

// openings records and publishes n pull_request.opened events on the fixture
// project: a second event type, so a case can move one event type's
// deliveries without touching another's.
func (f *inboxFixture) openings(t *testing.T, n int) {
	t.Helper()
	for i := range n {
		f.record(t, events.Event{
			EventType:     eventTypePROpened,
			ActorID:       f.alice.ID,
			ProjectID:     f.projectID,
			Visibility:    events.VisibilityPublic,
			CorrelationID: fmt.Sprintf("pr-%d", i),
		})
	}
	f.publish(t)
}

// pinWindow places one subscriber's delivered web rows of one event type
// inside a single clock hour, keeping their relative order.
//
// The aggregation window is wall-clock time, so without this the number of
// entries a case sees would depend on when the suite runs: a pass that
// straddles an hour boundary would split one batch of commits into two
// entries. The fixture therefore puts the deliveries at a KNOWN point of one
// hour (ten minutes in, one second apart) — a property of the test's setup,
// not of the rule under test, which is what the boundary cases below do.
//
// Calling it again re-places every row of that event type into one window
// (the rows keep their present order, so rows already pinned stay in
// sequence and new ones are placed after them), which is how the "a delivery
// after the read" case keeps its new delivery inside the window it is about.
func (f *inboxFixture) pinWindow(t *testing.T, userID, eventType string) {
	t.Helper()
	tag, err := f.pool.Exec(f.ctx, `
		UPDATE subscription_deliveries d
		SET created_at = date_trunc('hour', now()) + interval '10 minutes'
		                 + make_interval(secs => s.rn::int)
		FROM (
		  SELECT id, row_number() OVER (ORDER BY created_at, id) AS rn
		  FROM subscription_deliveries
		  WHERE user_id = $1::uuid AND channel = 'web' AND status = 'delivered'
		    AND event_type = $2
		) s
		WHERE d.id = s.id`, userID, eventType)
	if err != nil {
		t.Fatalf("pin %s deliveries: %v", eventType, err)
	}
	if tag.RowsAffected() == 0 {
		t.Fatalf("pinned 0 %s deliveries — the fixture has nothing to aggregate", eventType)
	}
}

// backdate moves the n oldest deliveries of one event type into an earlier
// clock hour — the only way to test the window boundary without waiting for
// it.
func (f *inboxFixture) backdate(t *testing.T, userID, eventType string, n int, d time.Duration) {
	t.Helper()
	tag, err := f.pool.Exec(f.ctx, `
		UPDATE subscription_deliveries
		SET created_at = created_at - make_interval(secs => $3::int)
		WHERE id IN (
		  SELECT id FROM subscription_deliveries
		  WHERE user_id = $1::uuid AND channel = 'web' AND status = 'delivered'
		    AND event_type = $2
		  ORDER BY created_at, id LIMIT $4)`,
		userID, eventType, int(d.Seconds()), n)
	if err != nil {
		t.Fatalf("backdate %s deliveries: %v", eventType, err)
	}
	if int(tag.RowsAffected()) != n {
		t.Fatalf("backdated %d %s deliveries, want %d", tag.RowsAffected(), eventType, n)
	}
}

// deliveryIDs lists one subscriber's delivered web rows of one event type,
// oldest first.
func (f *inboxFixture) deliveryIDs(t *testing.T, userID, eventType string) []string {
	t.Helper()
	rows, err := f.pool.Query(f.ctx, `
		SELECT id::text FROM subscription_deliveries
		WHERE user_id = $1::uuid AND channel = 'web' AND status = 'delivered'
		  AND event_type = $2
		ORDER BY created_at, id`, userID, eventType)
	if err != nil {
		t.Fatalf("list %s deliveries: %v", eventType, err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan delivery id: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate delivery ids: %v", err)
	}
	return out
}

// deliveryCreatedAt reads one delivery's arrival time.
func (f *inboxFixture) deliveryCreatedAt(t *testing.T, id string) time.Time {
	t.Helper()
	var at time.Time
	if err := f.pool.QueryRow(f.ctx,
		`SELECT created_at FROM subscription_deliveries WHERE id = $1::uuid`, id).Scan(&at); err != nil {
		t.Fatalf("read delivery %s: %v", id, err)
	}
	return at
}

// placeAfter rewrites the given deliveries to arrive just after a moment in
// time — deliveries that land in the same window as the entry the caller
// already saw, but newer than the anchor it saw it with.
func (f *inboxFixture) placeAfter(t *testing.T, ids []string, after time.Time) {
	t.Helper()
	tag, err := f.pool.Exec(f.ctx, `
		UPDATE subscription_deliveries d
		SET created_at = $2::timestamptz + make_interval(secs => s.rn::int)
		FROM (
		  SELECT t.id::uuid AS id, row_number() OVER (ORDER BY t.ord) AS rn
		  FROM unnest($1::text[]) WITH ORDINALITY AS t(id, ord)
		) s
		WHERE d.id = s.id`, ids, after)
	if err != nil {
		t.Fatalf("place deliveries after %s: %v", after, err)
	}
	if int(tag.RowsAffected()) != len(ids) {
		t.Fatalf("placed %d deliveries, want %d", tag.RowsAffected(), len(ids))
	}
}

// webDeliveries counts the delivered web rows behind one subscriber's
// entries of one event type. It is raw SQL on purpose: the "the events are
// all still there" half of aggregation must not be read through the code
// under test.
func (f *inboxFixture) webDeliveries(t *testing.T, userID, eventType string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `
		SELECT count(*) FROM subscription_deliveries
		WHERE user_id = $1::uuid AND channel = 'web' AND status = 'delivered'
		  AND event_type = $2`, userID, eventType).Scan(&n); err != nil {
		t.Fatalf("count %s deliveries: %v", eventType, err)
	}
	return n
}

// unreadWebRows counts one subscriber's unread delivered web rows.
func (f *inboxFixture) unreadWebRows(t *testing.T, userID string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `
		SELECT count(*) FROM subscription_deliveries
		WHERE user_id = $1::uuid AND channel = 'web' AND status = 'delivered'
		  AND read_at IS NULL`, userID).Scan(&n); err != nil {
		t.Fatalf("count unread rows: %v", err)
	}
	return n
}

// unreadInWindow counts the unread deliveries that fall in one aggregation
// window, using the same date_bin the read model uses — so a mark that
// reached across a window boundary is visible here.
func (f *inboxFixture) unreadInWindow(t *testing.T, userID string, windowStart time.Time) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `
		SELECT count(*) FROM subscription_deliveries
		WHERE user_id = $1::uuid AND channel = 'web' AND status = 'delivered'
		  AND read_at IS NULL
		  AND date_bin(make_interval(secs => $2::int), created_at, timestamptz 'epoch')
		      = $3::timestamptz`,
		userID, int(events.InboxAggregationWindow.Seconds()), windowStart).Scan(&n); err != nil {
		t.Fatalf("count unread rows in the window at %s: %v", windowStart, err)
	}
	return n
}

// --------------------------------------------------------------------------
// The inbox as the wire renders it

type inboxEntryJSON struct {
	TargetType       string    `json:"target_type"`
	TargetID         string    `json:"target_id"`
	TargetLabel      string    `json:"target_label"`
	EventType        string    `json:"event_type"`
	WindowStart      time.Time `json:"window_start"`
	Count            int       `json:"count"`
	Unread           int       `json:"unread"`
	Read             bool      `json:"read"`
	FirstAt          time.Time `json:"first_at"`
	LastAt           time.Time `json:"last_at"`
	LatestEventID    string    `json:"latest_event_id"`
	LatestDeliveryID string    `json:"latest_delivery_id"`
	URL              string    `json:"url"`
}

type inboxJSON struct {
	Entries     []inboxEntryJSON `json:"entries"`
	UnreadCount int              `json:"unread_count"`
	Filter      string           `json:"filter"`
}

// inbox reads one page of the caller's inbox over HTTP.
func (f *inboxFixture) inbox(t *testing.T, uc *testUserClient, query string) inboxJSON {
	t.Helper()
	path := "/api/v1/inbox"
	if query != "" {
		path += "?" + query
	}
	resp := uc.do(t, http.MethodGet, path, "")
	mustStatus(t, resp, http.StatusOK)
	var page inboxJSON
	if err := json.Unmarshal([]byte(readAll(t, resp)), &page); err != nil {
		t.Fatalf("decode inbox: %v", err)
	}
	return page
}

// entriesOf returns every entry of one event type.
func (p inboxJSON) entriesOf(eventType string) []inboxEntryJSON {
	out := []inboxEntryJSON{}
	for _, e := range p.Entries {
		if e.EventType == eventType {
			out = append(out, e)
		}
	}
	return out
}

// entry returns the single entry of one event type, or fails — a case that
// expects one entry should not pass silently on two.
func (p inboxJSON) entry(t *testing.T, eventType string) inboxEntryJSON {
	t.Helper()
	found := p.entriesOf(eventType)
	if len(found) != 1 {
		t.Fatalf("entries of type %s = %d, want exactly 1 (page: %+v)", eventType, len(found), p.Entries)
	}
	return found[0]
}

// targetEntry returns the single entry of one target and event type.
func (p inboxJSON) targetEntry(t *testing.T, targetType, targetID, eventType string) inboxEntryJSON {
	t.Helper()
	found := []inboxEntryJSON{}
	for _, e := range p.entriesOf(eventType) {
		if e.TargetType == targetType && e.TargetID == targetID {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("entries of %s/%s %s = %d, want exactly 1 (page: %+v)",
			targetType, targetID, eventType, len(found), p.Entries)
	}
	return found[0]
}

// unreadRows sums the unread deliveries across the page's entries — what a
// read-all would have to mark.
func (p inboxJSON) unreadRows() int {
	n := 0
	for _, e := range p.Entries {
		n += e.Unread
	}
	return n
}

// markRead marks the entries behind the given anchor deliveries read.
func (f *inboxFixture) markRead(t *testing.T, uc *testUserClient, deliveryIDs ...string) int {
	t.Helper()
	body, err := json.Marshal(map[string]any{"deliveries": deliveryIDs})
	if err != nil {
		t.Fatalf("marshal mark-read body: %v", err)
	}
	resp := uc.do(t, http.MethodPost, "/api/v1/inbox/read", string(body))
	mustStatus(t, resp, http.StatusOK)
	return readCount(t, resp)
}

// markAllRead marks the caller's whole inbox read.
func (f *inboxFixture) markAllRead(t *testing.T, uc *testUserClient) int {
	t.Helper()
	resp := uc.do(t, http.MethodPost, "/api/v1/inbox/read-all", "")
	mustStatus(t, resp, http.StatusOK)
	return readCount(t, resp)
}

func readCount(t *testing.T, resp *http.Response) int {
	t.Helper()
	var payload struct {
		Read int `json:"read"`
	}
	if err := json.Unmarshal([]byte(readAll(t, resp)), &payload); err != nil {
		t.Fatalf("decode the read count: %v", err)
	}
	return payload.Read
}

// --------------------------------------------------------------------------
// 1. The acceptance criterion, and everything it rests on

func TestInboxAggregatesMarkReadAndLink(t *testing.T) {
	ctx := testCtx(t)
	f := newInboxFixture(t, ctx)

	// The subscriber: the project on both channels (email is a different
	// output — its rows must not appear in the web inbox), the asset on the
	// web channel.
	f.follow(t, f.aliceClient, events.TargetTypeProject, f.projectID,
		events.ChannelWeb, events.ChannelEmail)
	f.follow(t, f.aliceClient, events.TargetTypeAsset, f.assetPID, events.ChannelWeb)

	// Forty commits — one delivery per commit, nothing suppressed — then
	// three events a subscriber would call meaningful. They land in the same
	// clock hour as the commits: what separates them in the inbox is their
	// type and their target, not their age.
	f.commits(t, 40)
	f.pinWindow(t, f.alice.ID, eventTypeStateCommitted)
	f.record(t, events.Event{EventType: eventTypePRMerged, ActorID: f.alice.ID,
		ProjectID: f.projectID, Visibility: events.VisibilityPublic, CorrelationID: "pr-1"})
	f.record(t, events.Event{EventType: eventTypeRelease, ActorID: f.alice.ID,
		ProjectID: f.projectID, Visibility: events.VisibilityPublic, CorrelationID: "rel-1"})
	f.record(t, events.Event{EventType: events.EventTypeAssetVersionPublished, ActorID: f.alice.ID,
		ProjectID: f.projectID, Visibility: events.VisibilityPublic, CorrelationID: "asset-1",
		Payload: json.RawMessage(fmt.Sprintf(`{"asset_id":%q,"version":"v1"}`, f.assetPID))})
	f.publish(t)

	// The page a subscriber who has read nothing sees: five entries.
	//
	//   project  state.committed                x 40   one entry
	//   project  pull_request.merged            x 1
	//   project  release.published              x 1
	//   project  research_asset.version_publ.   x 1   (the project the asset belongs to)
	//   asset    research_asset.version_publ.   x 1   (the asset the payload names)
	//
	// The asset event reaches two targets and the inbox keeps them apart: an
	// entry is (target, type, window), not (type, window).

	t.Run("forty per-commit deliveries are one entry, not forty", func(t *testing.T) {
		// The deliveries are the pipeline's own count: forty events went in,
		// forty web rows came out. Aggregation is a read model over them.
		if got := f.webDeliveries(t, f.alice.ID, eventTypeStateCommitted); got != 40 {
			t.Fatalf("state.committed web deliveries = %d, want 40 — the fan-out must have delivered "+
				"every commit for this case to say anything about aggregation", got)
		}
		page := f.inbox(t, f.aliceClient, "")
		commit := page.entry(t, eventTypeStateCommitted)
		if commit.Count != 40 {
			t.Errorf("the commit entry aggregates %d deliveries, want 40 — a subscriber who follows a "+
				"project and gets forty commits must see ONE entry, not a list of forty", commit.Count)
		}
		if commit.Unread != 40 {
			t.Errorf("unread in the commit entry = %d, want 40 (nothing has been read yet)", commit.Unread)
		}
		if page.UnreadCount != 5 {
			t.Errorf("unread_count = %d, want 5 entries — the email channel is a different output, "+
				"so the forty email rows behind the same commits are not a second, fifty-entry inbox",
				page.UnreadCount)
		}
	})

	t.Run("each entry links to its subject, and names it", func(t *testing.T) {
		page := f.inbox(t, f.aliceClient, "")
		commit := page.entry(t, eventTypeStateCommitted)
		if want := "/projects/" + f.projectID + "/activity"; commit.URL != want {
			t.Errorf("the project entry's deep link = %q, want %q", commit.URL, want)
		}
		if commit.TargetLabel != f.projectName {
			t.Errorf("the project entry's label = %q, want %q", commit.TargetLabel, f.projectName)
		}
		if commit.LatestEventID == "" || commit.LatestDeliveryID == "" {
			t.Errorf("the entry carries no event/delivery identity: %+v", commit)
		}
		if commit.WindowStart.IsZero() || commit.FirstAt.After(commit.LastAt) {
			t.Errorf("the entry's window/interval is not readable: %+v", commit)
		}

		// The asset target is named only by the event's payload (events.
		// EventTargets reads asset_id), and its pid is the identity the
		// /assets/{pid} route is built from — stable across renames.
		asset := page.targetEntry(t, events.TargetTypeAsset, f.assetPID, events.EventTypeAssetVersionPublished)
		if want := "/assets/" + f.assetPID; asset.URL != want {
			t.Errorf("the asset entry's deep link = %q, want %q", asset.URL, want)
		}
		if asset.TargetLabel != f.assetTitle {
			t.Errorf("the asset entry's label = %q, want %q", asset.TargetLabel, f.assetTitle)
		}
		// The same event, the other target: the project entry keeps the
		// project's identity and name, not the asset's.
		viaProject := page.targetEntry(t, events.TargetTypeProject, f.projectID, events.EventTypeAssetVersionPublished)
		if want := "/projects/" + f.projectID + "/activity"; viaProject.URL != want {
			t.Errorf("the project entry of the asset event links to %q, want %q", viaProject.URL, want)
		}
		if viaProject.TargetLabel != f.projectName {
			t.Errorf("the project entry of the asset event is labelled %q, want %q",
				viaProject.TargetLabel, f.projectName)
		}
	})

	t.Run("an anonymous caller reads nothing and marks nothing", func(t *testing.T) {
		unread := f.unreadWebRows(t, f.alice.ID)
		if unread != 44 {
			t.Fatalf("alice holds %d unread web rows before the anonymous calls, want 44 "+
				"(40 commits, PR, release, and the asset event on its two targets)", unread)
		}
		anon := newTestUserClient(f.server)
		resp := anon.do(t, http.MethodGet, "/api/v1/inbox", "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("anonymous GET /api/v1/inbox = %d, want 401", resp.StatusCode)
		}
		mustEnvelope(t, resp, authn.CodeUnauthenticated)

		// The write is refused by the guard (CSRF is bound to a session an
		// anonymous caller does not have); which of the two refusals comes
		// first is the guard's business, so either is accepted here — what
		// matters is the refusal and that nothing was written.
		resp = anon.do(t, http.MethodPost, "/api/v1/inbox/read-all", "")
		if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
			t.Errorf("anonymous read-all = %d, want a refusal (401 or 403)", resp.StatusCode)
		}
		if got := f.unreadWebRows(t, f.alice.ID); got != unread {
			t.Errorf("the anonymous calls changed alice's read state: %d unread rows, want %d", got, unread)
		}
	})

	var commitAnchor string
	t.Run("marking an entry read clears exactly its deliveries", func(t *testing.T) {
		page := f.inbox(t, f.aliceClient, "")
		commitAnchor = page.entry(t, eventTypeStateCommitted).LatestDeliveryID

		if n := f.markRead(t, f.aliceClient, commitAnchor); n != 40 {
			t.Errorf("marking the commit entry read marked %d rows, want 40", n)
		}
		page = f.inbox(t, f.aliceClient, "")
		if page.UnreadCount != 4 {
			t.Errorf("unread_count = %d, want 4 after reading the commit entry "+
				"(PR, release, and the asset event on its two targets)", page.UnreadCount)
		}
		page = f.inbox(t, f.aliceClient, "filter=all")
		commit := page.entry(t, eventTypeStateCommitted)
		if !commit.Read || commit.Unread != 0 {
			t.Errorf("the commit entry is still unread after marking: %+v", commit)
		}
		if commit.Count != 40 {
			t.Errorf("reading changed the count to %d, want the aggregation untouched at 40", commit.Count)
		}

		// Idempotent: the state the caller asked for is the state it is in.
		if n := f.markRead(t, f.aliceClient, commitAnchor); n != 0 {
			t.Errorf("re-marking the same entry marked %d more rows, want 0", n)
		}
	})

	t.Run("a delivery after the read is unread again, in the same entry", func(t *testing.T) {
		f.commits(t, 1)
		f.pinWindow(t, f.alice.ID, eventTypeStateCommitted)
		page := f.inbox(t, f.aliceClient, "")
		commit := page.entry(t, eventTypeStateCommitted)
		if commit.Count != 41 || commit.Unread != 1 {
			t.Errorf("after one more commit the entry = count %d / unread %d, want 41/1 — the window "+
				"holds the new delivery next to the forty read ones", commit.Count, commit.Unread)
		}
		if page.UnreadCount != 5 {
			t.Errorf("unread_count = %d, want 5 (the commit entry is unread again, plus the other four)",
				page.UnreadCount)
		}
	})

	t.Run("read-all empties the badge and is idempotent", func(t *testing.T) {
		page := f.inbox(t, f.aliceClient, "")
		want := page.unreadRows()
		if want != 5 {
			t.Fatalf("the page holds %d unread deliveries, want 5 (the new commit + the four "+
				"untouched entries)", want)
		}
		if n := f.markAllRead(t, f.aliceClient); n != want {
			t.Errorf("read-all marked %d rows, want %d — the button must clear the badge it was "+
				"shown beside", n, want)
		}
		page = f.inbox(t, f.aliceClient, "")
		if page.UnreadCount != 0 || len(page.Entries) != 0 {
			t.Errorf("the unread view after read-all = %d entries / badge %d, want 0/0",
				len(page.Entries), page.UnreadCount)
		}
		page = f.inbox(t, f.aliceClient, "filter=all")
		if len(page.Entries) != 5 {
			t.Errorf("the all view after read-all = %d entries, want the 5 read ones", len(page.Entries))
		}
		for _, e := range page.Entries {
			if !e.Read {
				t.Errorf("entry %s/%s %s is still unread after read-all", e.TargetType, e.TargetID, e.EventType)
			}
		}
		if n := f.markAllRead(t, f.aliceClient); n != 0 {
			t.Errorf("a second read-all marked %d rows, want 0", n)
		}
	})
}

// --------------------------------------------------------------------------
// 2. The window, and the anchor

// TestInboxWindowAndAnchorSplitTheMark walks the two halves that make "read"
// mean "shown": the window decides which deliveries belong to the entry that
// was read, and the anchor decides how far into that entry the mark reaches.
//
// Both phases are built so an implementation with the wrong rule reports a
// DIFFERENT NUMBER, not merely a different shape:
//
//   - phase one: a mark that ignored the window would mark 5 rows instead of
//     3, and so would one that marked the whole entry regardless of the
//     anchor.
//   - phase two: an ignored anchor (the whole entry marked) reports 3 marked
//     and 0 unread instead of 1 and 2, and an anchor marking everything
//     NEWER than itself reports 2 marked and 1 unread.
func TestInboxWindowAndAnchorSplitTheMark(t *testing.T) {
	ctx := testCtx(t)
	f := newInboxFixture(t, ctx)
	f.follow(t, f.aliceClient, events.TargetTypeProject, f.projectID, events.ChannelWeb)

	t.Run("the window decides which deliveries an entry holds", func(t *testing.T) {
		f.commits(t, 3)
		f.commits(t, 2)
		f.pinWindow(t, f.alice.ID, eventTypeStateCommitted)
		// Two of the five move into an earlier clock hour: same subscriber,
		// same target, same event type — a different entry.
		f.backdate(t, f.alice.ID, eventTypeStateCommitted, 2, 90*time.Minute)

		page := f.inbox(t, f.aliceClient, "")
		entries := page.entriesOf(eventTypeStateCommitted)
		if len(entries) != 2 {
			t.Fatalf("commit entries = %d, want 2 — the window split the deliveries: %+v",
				len(entries), page.Entries)
		}
		current, older := entries[0], entries[1]
		if current.Count != 3 || older.Count != 2 {
			t.Fatalf("the windows hold %d/%d deliveries, want 3 in the current hour and 2 in the older one",
				current.Count, older.Count)
		}
		if !current.WindowStart.After(older.WindowStart) {
			t.Errorf("the newer entry's window (%s) does not start after the older one's (%s)",
				current.WindowStart, older.WindowStart)
		}

		if n := f.markRead(t, f.aliceClient, current.LatestDeliveryID); n != 3 {
			t.Errorf("marking the current window read marked %d rows, want 3 — its own deliveries "+
				"only; the older window is a different entry", n)
		}
		if got := f.unreadInWindow(t, f.alice.ID, current.WindowStart); got != 0 {
			t.Errorf("the current window still holds %d unread rows, want 0", got)
		}
		if got := f.unreadInWindow(t, f.alice.ID, older.WindowStart); got != 2 {
			t.Errorf("the older window holds %d unread rows, want 2 — read state leaked across the "+
				"aggregation boundary", got)
		}
		// The two entries now disagree about read state, which is the point:
		// the mark reached the entry the caller read and stopped at that
		// entry's window.
		page = f.inbox(t, f.aliceClient, "filter=all")
		seenWindows := 0
		for _, e := range page.entriesOf(eventTypeStateCommitted) {
			switch {
			case e.WindowStart.Equal(current.WindowStart):
				seenWindows++
				if !e.Read || e.Unread != 0 {
					t.Errorf("the entry that was marked is not read: %+v", e)
				}
			case e.WindowStart.Equal(older.WindowStart):
				seenWindows++
				if e.Read || e.Unread != 2 {
					t.Errorf("the older window's entry reports read %v / unread %d, want false/2 — "+
						"an entry the caller never read", e.Read, e.Unread)
				}
			default:
				t.Errorf("unexpected commit entry: %+v", e)
			}
		}
		if seenWindows != 2 {
			t.Errorf("the all view holds %d of the two commit entries", seenWindows)
		}
	})

	t.Run("the anchor decides how far into the entry the mark reaches", func(t *testing.T) {
		// A second event type, so phase one's rows (and their read state)
		// cannot be touched here.
		f.openings(t, 1)
		f.pinWindow(t, f.alice.ID, eventTypePROpened)

		page := f.inbox(t, f.aliceClient, "")
		entry := page.entry(t, eventTypePROpened)
		anchor := entry.LatestDeliveryID
		anchorAt := f.deliveryCreatedAt(t, anchor)

		// Two more deliveries arrive in the SAME window while the caller is
		// still looking at the page it read the anchor from.
		before := f.deliveryIDs(t, f.alice.ID, eventTypePROpened)
		f.openings(t, 2)
		seen := map[string]bool{}
		for _, id := range before {
			seen[id] = true
		}
		fresh := []string{}
		for _, id := range f.deliveryIDs(t, f.alice.ID, eventTypePROpened) {
			if !seen[id] {
				fresh = append(fresh, id)
			}
		}
		if len(fresh) != 2 {
			t.Fatalf("the second batch produced %d new deliveries, want 2", len(fresh))
		}
		f.placeAfter(t, fresh, anchorAt)

		page = f.inbox(t, f.aliceClient, "")
		entry = page.entry(t, eventTypePROpened)
		if entry.Count != 3 || entry.Unread != 3 {
			t.Fatalf("the entry = count %d / unread %d, want 3/3 before the mark — the three "+
				"deliveries must share one window for this case to mean anything",
				entry.Count, entry.Unread)
		}

		if n := f.markRead(t, f.aliceClient, anchor); n != 1 {
			t.Errorf("marking with the anchor the caller saw marked %d rows, want 1 — the two "+
				"deliveries that arrived after the page was read were never shown", n)
		}
		page = f.inbox(t, f.aliceClient, "")
		entry = page.entry(t, eventTypePROpened)
		if entry.Unread != 2 || entry.Read {
			t.Errorf("after the mark the entry = unread %d / read %v, want 2 unread — a read is "+
				"what was shown, not the whole entry", entry.Unread, entry.Read)
		}
	})
}

// --------------------------------------------------------------------------
// 3. Owner scoping and the disclosure boundary

// TestInboxIsOwnerScopedAndHidesWhatItMayNotName walks the two boundaries a
// notification list has to keep: the rows are one subscriber's (another
// subscriber reads none of them, and a mark aimed at them writes nothing),
// and an entry is served only while the caller may still read its target —
// a target they no longer may is not named, and not rendered either.
func TestInboxIsOwnerScopedAndHidesWhatItMayNotName(t *testing.T) {
	ctx := testCtx(t)
	f := newInboxFixture(t, ctx)
	f.follow(t, f.aliceClient, events.TargetTypeProject, f.projectID, events.ChannelWeb)
	f.commits(t, 3)
	f.pinWindow(t, f.alice.ID, eventTypeStateCommitted)

	t.Run("another subscriber sees an empty inbox and cannot mark", func(t *testing.T) {
		bobPage := f.inbox(t, f.bobClient, "filter=all")
		if len(bobPage.Entries) != 0 || bobPage.UnreadCount != 0 {
			t.Errorf("bob's inbox = %d entries / badge %d, want 0/0 — bob follows nothing: %+v",
				len(bobPage.Entries), bobPage.UnreadCount, bobPage.Entries)
		}

		// An empty inbox is an empty LIST, not a missing one. Decoded into
		// a slice the two are indistinguishable (len(nil) == len([]T{}) ==
		// 0), so the count above cannot see the difference — and the
		// difference is client-visible: apps/web/lib/inbox.ts refuses a
		// page whose entries are not an array, so a null here turns "you
		// have no notifications" into an error banner. This reads the raw
		// body, which is the only place the distinction exists.
		resp := f.bobClient.do(t, http.MethodGet, "/api/v1/inbox?filter=all", "")
		mustStatus(t, resp, http.StatusOK)
		if body := readAll(t, resp); !strings.Contains(body, `"entries":[]`) {
			t.Errorf("an empty inbox answered %s, want \"entries\":[] — null is not an empty list", body)
		}

		aliceAnchor := f.inbox(t, f.aliceClient, "").entry(t, eventTypeStateCommitted).LatestDeliveryID
		resp = f.bobClient.do(t, http.MethodPost, "/api/v1/inbox/read",
			fmt.Sprintf(`{"deliveries":[%q]}`, aliceAnchor))
		mustStatus(t, resp, http.StatusNotFound)
		mustEnvelope(t, resp, inbox.CodeNotFound)

		if got := f.unreadWebRows(t, f.alice.ID); got != 3 {
			t.Errorf("bob's refused mark changed alice's rows: %d unread, want 3", got)
		}
	})

	t.Run("a foreign anchor and an unknown one answer the same", func(t *testing.T) {
		resp := f.aliceClient.do(t, http.MethodPost, "/api/v1/inbox/read",
			`{"deliveries":["11111111-2222-3333-4444-555555555555"]}`)
		mustStatus(t, resp, http.StatusNotFound)
		mustEnvelope(t, resp, inbox.CodeNotFound)

		// A malformed id is a client error, not a missing row: it cannot be
		// looked up at all.
		resp = f.aliceClient.do(t, http.MethodPost, "/api/v1/inbox/read", `{"deliveries":["not-a-uuid"]}`)
		mustStatus(t, resp, http.StatusBadRequest)
		mustEnvelope(t, resp, inbox.CodeValidationFailed)

		resp = f.aliceClient.do(t, http.MethodGet, "/api/v1/inbox?filter=everything", "")
		mustStatus(t, resp, http.StatusBadRequest)
		mustEnvelope(t, resp, inbox.CodeValidationFailed)

		resp = f.aliceClient.do(t, http.MethodGet, "/api/v1/inbox?limit=nope", "")
		mustStatus(t, resp, http.StatusBadRequest)
		mustEnvelope(t, resp, inbox.CodeValidationFailed)
	})

	t.Run("a target the caller may no longer read takes its entry out of the inbox", func(t *testing.T) {
		// Decision 3, fail-closed. The delivery rows stay — T1002 cancels
		// only what is still in flight, and these are alice's own rows —
		// but an entry whose target she may no longer read is not rendered
		// at all: out of both views, and out of the badge. The assertions
		// are about the whole page rather than about the label, because an
		// entry that kept its place with a blanked name would pass a
		// label-only check and still tell the caller the target is there.
		before := f.inbox(t, f.aliceClient, "filter=all")
		if got := len(before.entriesOf(eventTypeStateCommitted)); got != 1 {
			t.Fatalf("before the revocation alice has %d state.committed entries, want 1: %+v",
				got, before.Entries)
		}
		if before.UnreadCount != 1 {
			t.Fatalf("before the revocation the badge = %d, want 1 — a badge that is already 0 "+
				"cannot show the drop this case is about", before.UnreadCount)
		}

		if _, err := f.pool.Exec(ctx,
			`DELETE FROM project_memberships WHERE project_id = $1 AND user_id = $2`,
			f.projectID, f.alice.ID); err != nil {
			t.Fatalf("revoke alice's membership: %v", err)
		}
		if _, err := f.pool.Exec(ctx,
			`UPDATE projects SET visibility = 'private' WHERE id = $1`, f.projectID); err != nil {
			t.Fatalf("make the project private: %v", err)
		}

		// Both views, and the badge, in the same pass: the badge is the
		// whole inbox's, so it has to count the entries that are actually
		// served and nothing else.
		for _, view := range []string{"filter=all", "filter=unread"} {
			page := f.inbox(t, f.aliceClient, view)
			if len(page.Entries) != 0 {
				t.Errorf("%s still serves %d entries for a target alice may no longer read: %+v",
					view, len(page.Entries), page.Entries)
			}
			if page.UnreadCount != 0 {
				t.Errorf("%s reports a badge of %d after alice's only readable target was revoked, "+
					"want 0 — an entry that is not served must not be counted either",
					view, page.UnreadCount)
			}
		}

		// The rows are still hers: this is a rule about what may be
		// rendered from them, not a withdrawal of the deliveries.
		if got := f.unreadWebRows(t, f.alice.ID); got != 3 {
			t.Errorf("the revoked read changed the delivery rows: %d unread web rows, want the 3 "+
				"that were already delivered — nothing is deleted by a permission change", got)
		}

		// Regaining access restores it: the rule reads current state, it is
		// not a flag frozen when the notification was fanned out.
		if _, err := f.pool.Exec(ctx,
			`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, 'contributor')`,
			f.projectID, f.alice.ID); err != nil {
			t.Fatalf("restore alice's membership: %v", err)
		}
		page := f.inbox(t, f.aliceClient, "filter=all")
		commit := page.entry(t, eventTypeStateCommitted)
		if commit.TargetLabel != f.projectName {
			t.Errorf("the label after regaining access = %q, want %q", commit.TargetLabel, f.projectName)
		}
		if commit.TargetID != f.projectID || commit.URL == "" || commit.Count != 3 {
			t.Errorf("the entry alice got back = %+v, want the same 3 delivered rows, the project id "+
				"and its link — the rows were never withdrawn, so nothing is lost by hiding it",
				commit)
		}
		if page.UnreadCount != 1 {
			t.Errorf("the badge after regaining access = %d, want 1", page.UnreadCount)
		}
	})
}

// TestInboxBadgeCountsOnlyEntriesItServes is the other half of decision 3,
// and the half that only a survivor can show: the badge is the WHOLE
// inbox's, not the page's, so when one of the caller's targets is revoked
// the entry that is still served must carry the number of entries still
// served.
//
// The single-entry case next door cannot see this. Its revoked target is
// the caller's entire inbox, so a badge that wrongly counted the hidden
// entry would have no row left to be reported on and would read as 0
// anyway — the assertion would pass while measuring nothing. Here the
// caller follows TWO projects: one is revoked, the other stays, and the
// surviving entry is the carrier that makes "1, not 2" observable.
//
// The second half of the fixture is bob, who follows the SAME project
// alice's surviving entry is about. An inbox is one person's, and with a
// single subscriber "my count is my rows" cannot be observed at all: a
// read that dropped the owner predicate would answer exactly the numbers
// this test expects. Bob's two deliveries on the shared project are
// another subscriber's rows of the same target and the same event type,
// so an unscoped read has to report 4 where the test says 2 — on alice's
// page and on bob's.
func TestInboxBadgeCountsOnlyEntriesItServes(t *testing.T) {
	ctx := testCtx(t)
	f := newInboxFixture(t, ctx)

	// A second public project, plus a second subscription. Two targets, one
	// window, five delivered rows for alice and two for bob.
	otherID := mustQueryUUID(t, ctx, f.pool,
		`INSERT INTO projects (slug, name, purpose, visibility, created_by)
		 VALUES ('inbox-badge-other', 'Other Project', 'T1003 badge fixture', 'public', $1) RETURNING id`,
		f.alice.ID)
	f.follow(t, f.aliceClient, events.TargetTypeProject, f.projectID, events.ChannelWeb)
	f.follow(t, f.aliceClient, events.TargetTypeProject, otherID, events.ChannelWeb)
	// The other subscriber of the SAME target. He is subscribed before the
	// events are recorded, so the fan-out writes him his own rows.
	f.follow(t, f.bobClient, events.TargetTypeProject, otherID, events.ChannelWeb)
	f.commits(t, 3)
	for i := range 2 {
		f.record(t, events.Event{
			EventType:     eventTypeStateCommitted,
			ActorID:       f.alice.ID,
			ProjectID:     otherID,
			Visibility:    events.VisibilityPublic,
			CorrelationID: fmt.Sprintf("other-%d", i),
			Payload:       json.RawMessage(`{"commit_sha":"0ff1ce"}`),
		})
	}
	f.publish(t)
	f.pinWindow(t, f.alice.ID, eventTypeStateCommitted)
	f.pinWindow(t, f.bob.ID, eventTypeStateCommitted)

	// The fixture's own guard, read through raw SQL: bob really holds rows
	// on the shared target. Without them every assertion below would pass
	// for the wrong reason — a page that leaked nothing reads exactly like
	// one that had nothing to leak.
	if got := f.webDeliveries(t, f.bob.ID, eventTypeStateCommitted); got != 2 {
		t.Fatalf("bob holds %d delivered web rows on the shared project, want 2 — another "+
			"subscriber's rows are what make the owner scope observable here", got)
	}

	for _, view := range []string{"filter=all", "filter=unread"} {
		page := f.inbox(t, f.aliceClient, view)
		if len(page.Entries) != 2 || page.UnreadCount != 2 {
			t.Fatalf("before the revocation %s = %d entries / badge %d, want 2/2: %+v",
				view, len(page.Entries), page.UnreadCount, page.Entries)
		}
		// Counts are per subscriber: bob's two rows of the same target and
		// the same event type must not appear in alice's entry, and her
		// three must not appear in his.
		if e := page.targetEntry(t, events.TargetTypeProject, f.projectID, eventTypeStateCommitted); e.Count != 3 {
			t.Errorf("%s: alice's own project counts %d deliveries, want her 3", view, e.Count)
		}
		if e := page.targetEntry(t, events.TargetTypeProject, otherID, eventTypeStateCommitted); e.Count != 2 {
			t.Errorf("%s: the shared project counts %d deliveries, want alice's 2 — bob's two rows on "+
				"the same target are his, and an inbox is one person's", view, e.Count)
		}
	}

	// And from bob's side, whose inbox has only the shared target: one
	// entry, holding his own two rows.
	{
		page := f.inbox(t, f.bobClient, "filter=all")
		if len(page.Entries) != 1 || page.UnreadCount != 1 {
			t.Fatalf("bob's inbox = %d entries / badge %d, want 1/1 — he follows one project: %+v",
				len(page.Entries), page.UnreadCount, page.Entries)
		}
		if page.Entries[0].TargetID != otherID || page.Entries[0].Count != 2 {
			t.Errorf("bob's entry = %s with %d deliveries, want the shared project's 2 rows of his own",
				page.Entries[0].TargetID, page.Entries[0].Count)
		}
	}

	// One of the two targets goes away. The other does not, and neither do
	// the delivery rows: the entry is withheld from the read, not deleted.
	if _, err := f.pool.Exec(ctx,
		`DELETE FROM project_memberships WHERE project_id = $1 AND user_id = $2`,
		f.projectID, f.alice.ID); err != nil {
		t.Fatalf("revoke alice's membership: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE projects SET visibility = 'private' WHERE id = $1`, f.projectID); err != nil {
		t.Fatalf("make the project private: %v", err)
	}

	for _, view := range []string{"filter=all", "filter=unread"} {
		page := f.inbox(t, f.aliceClient, view)
		if len(page.Entries) != 1 || page.Entries[0].TargetID != otherID {
			t.Errorf("%s serves %+v, want only the surviving project's entry", view, page.Entries)
		} else if page.Entries[0].Count != 2 {
			t.Errorf("%s: the surviving entry counts %d deliveries, want alice's own 2 — revoking one "+
				"target moves neither the rows nor the counts of another", view, page.Entries[0].Count)
		}
		if page.UnreadCount != 1 {
			t.Errorf("%s reports a badge of %d after one of the caller's two entries stopped being "+
				"served, want 1 — the badge counts the entries the caller is SERVED, not the rows "+
				"they own, and it is the whole inbox's rather than the page's",
				view, page.UnreadCount)
		}
	}
	if got := f.webDeliveries(t, f.alice.ID, eventTypeStateCommitted); got != 5 {
		t.Errorf("the deliveries = %d, want the 5 that were delivered — a revoked target loses its "+
			"entry, not its rows", got)
	}

	// Regaining access restores exactly the entry that was hidden, and with
	// it the badge: 2 again, not 3.
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, 'contributor')`,
		f.projectID, f.alice.ID); err != nil {
		t.Fatalf("restore alice's membership: %v", err)
	}
	page := f.inbox(t, f.aliceClient, "filter=all")
	if len(page.Entries) != 2 || page.UnreadCount != 2 {
		t.Errorf("after regaining access: %d entries / badge %d, want 2/2: %+v",
			len(page.Entries), page.UnreadCount, page.Entries)
	}
}

// TestInboxReadAllMarksOnlyWhatItServes is decision 5: the mass mark is
// scoped by the same audience resolution as the read, so the rows it marks
// are exactly the rows the inbox is serving.
//
// The two rules internal/events/inbox.go states have to hold at once — the
// read is fail-closed on the caller's CURRENT relation to each target
// (rule 3), and a mark is bounded by what was shown (rule 2) — and an
// unscoped read-all sets them against each other: it writes a one-way
// read_at onto rows rule 3 hides from the caller entirely. None of that
// write is observable to the person who caused it: the badge does not move
// (it counts served entries), neither view changes, and there is no unread
// mark anywhere. The only trace appears later, when the target comes back
// and its entry arrives already read although it was never shown.
//
// The three consequences are asserted as three different facts, not as
// restatements of one:
//
//   - what read-all RETURNED: 2, the surviving target's rows, not the 5
//     rows alice owns;
//   - what it WROTE: the hidden target's 3 rows are still unread, read
//     back through raw SQL that does not go through the code under test;
//   - what comes BACK: after the revocation is undone the entry is served
//     with unread 3 — the review's counter-example, pinned from this side.
//
// The control at the end is what keeps the other three from passing for
// the wrong reason: with the target served again, read-all marks those
// same 3 rows. The rule is about what is served, not about read-all having
// become a no-op.
func TestInboxReadAllMarksOnlyWhatItServes(t *testing.T) {
	ctx := testCtx(t)
	f := newInboxFixture(t, ctx)

	// A second public project, so something stays served while the first
	// one is revoked.
	otherID := mustQueryUUID(t, ctx, f.pool,
		`INSERT INTO projects (slug, name, purpose, visibility, created_by)
		 VALUES ('inbox-readall-other', 'Other Project', 'T1003 read-all fixture', 'public', $1) RETURNING id`,
		f.alice.ID)
	f.follow(t, f.aliceClient, events.TargetTypeProject, f.projectID, events.ChannelWeb)
	f.follow(t, f.aliceClient, events.TargetTypeProject, otherID, events.ChannelWeb)
	f.commits(t, 3)
	for i := range 2 {
		f.record(t, events.Event{
			EventType:     eventTypeStateCommitted,
			ActorID:       f.alice.ID,
			ProjectID:     otherID,
			Visibility:    events.VisibilityPublic,
			CorrelationID: fmt.Sprintf("readall-%d", i),
			Payload:       json.RawMessage(`{"commit_sha":"0ff1ce"}`),
		})
	}
	f.publish(t)
	f.pinWindow(t, f.alice.ID, eventTypeStateCommitted)

	// Two entries, five unread rows behind them: 3 on the project that
	// will be revoked, 2 on the one that stays.
	page := f.inbox(t, f.aliceClient, "")
	if len(page.Entries) != 2 || page.UnreadCount != 2 || page.unreadRows() != 5 {
		t.Fatalf("before the revocation the inbox = %d entries / badge %d / %d unread rows, "+
			"want 2/2/5: %+v", len(page.Entries), page.UnreadCount, page.unreadRows(), page.Entries)
	}

	// The first project goes private without alice: her 3 rows stop being
	// served. They are still hers and still unread — the state read-all
	// has to leave alone.
	if _, err := f.pool.Exec(ctx,
		`DELETE FROM project_memberships WHERE project_id = $1 AND user_id = $2`,
		f.projectID, f.alice.ID); err != nil {
		t.Fatalf("revoke alice's membership: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE projects SET visibility = 'private' WHERE id = $1`, f.projectID); err != nil {
		t.Fatalf("make the project private: %v", err)
	}
	page = f.inbox(t, f.aliceClient, "")
	if len(page.Entries) != 1 || page.UnreadCount != 1 {
		t.Fatalf("after the revocation the inbox serves %d entries / badge %d, want 1/1 — the "+
			"surviving target's: %+v", len(page.Entries), page.UnreadCount, page.Entries)
	}
	if e := page.Entries[0]; e.TargetID != otherID || e.Unread != 2 {
		t.Fatalf("the served entry = %s with %d unread, want the surviving project's 2", e.TargetID, e.Unread)
	}
	if got := f.unreadWebRows(t, f.alice.ID); got != 5 {
		t.Fatalf("alice holds %d unread web rows before read-all, want 5 — the hidden rows have to "+
			"still be unread for this case to say anything", got)
	}

	// read-all marks the served entry and nothing else.
	if n := f.markAllRead(t, f.aliceClient); n != 2 {
		t.Errorf("read-all marked %d rows, want the 2 the badge was counting — the hidden target's 3 "+
			"rows were never served to anyone and must not be swept into a read", n)
	}
	if got := f.unreadWebRows(t, f.alice.ID); got != 3 {
		t.Errorf("after read-all alice holds %d unread web rows, want the hidden target's 3 — a mark "+
			"may only reach what the inbox serves, or it writes read state nobody can see", got)
	}
	page = f.inbox(t, f.aliceClient, "")
	if len(page.Entries) != 0 || page.UnreadCount != 0 {
		t.Errorf("the unread view after read-all = %d entries / badge %d, want 0/0",
			len(page.Entries), page.UnreadCount)
	}
	all := f.inbox(t, f.aliceClient, "filter=all")
	if len(all.Entries) != 1 || !all.Entries[0].Read {
		t.Errorf("the all view after read-all = %+v, want the surviving entry, read", all.Entries)
	}

	// Nothing served at all, with 3 unread rows still in the table: the
	// empty case of the audience filter. read-all answers 0 and succeeds —
	// an inbox with nothing to serve is not an error.
	if _, err := f.pool.Exec(ctx,
		`UPDATE projects SET visibility = 'private' WHERE id = $1`, otherID); err != nil {
		t.Fatalf("make the second project private: %v", err)
	}
	all = f.inbox(t, f.aliceClient, "filter=all")
	if len(all.Entries) != 0 || all.UnreadCount != 0 {
		t.Fatalf("with both targets hidden the all view still serves %d entries / badge %d: %+v",
			len(all.Entries), all.UnreadCount, all.Entries)
	}
	if n := f.markAllRead(t, f.aliceClient); n != 0 {
		t.Errorf("read-all with nothing served marked %d rows, want 0 — it may mark only what the "+
			"inbox is serving", n)
	}
	if got := f.unreadWebRows(t, f.alice.ID); got != 3 {
		t.Errorf("read-all with nothing served left %d unread web rows, want the same 3", got)
	}

	// Access comes back: the entry returns with its rows STILL UNREAD. This
	// is the counter-example read from the failing side — under a mark that
	// ignored the audience it would arrive read, having never been shown.
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, 'contributor')`,
		f.projectID, f.alice.ID); err != nil {
		t.Fatalf("restore alice's membership: %v", err)
	}
	page = f.inbox(t, f.aliceClient, "")
	entry := page.entry(t, eventTypeStateCommitted)
	if entry.Unread != 3 || entry.Read {
		t.Errorf("the entry that comes back = unread %d / read %v, want 3 unread — it was never "+
			"shown, and a read it never had is a state change its owner cannot undo",
			entry.Unread, entry.Read)
	}
	if page.UnreadCount != 1 {
		t.Errorf("the badge after access returns = %d, want 1 (the entry, unread again)", page.UnreadCount)
	}

	// The control: with the target served again, read-all marks exactly
	// those 3 rows.
	if n := f.markAllRead(t, f.aliceClient); n != 3 {
		t.Errorf("read-all with the entry served again marked %d rows, want 3", n)
	}
	if got := f.unreadWebRows(t, f.alice.ID); got != 0 {
		t.Errorf("after the control read-all %d unread web rows remain, want 0", got)
	}
}

// --------------------------------------------------------------------------
// 4. The page size

// TestInboxLimitIsClampedNotObeyed pins the two ends of the page bound: a
// caller may not ask for an unbounded page, and a caller who asks for a page
// of one gets exactly one entry while the BADGE still reports the whole
// inbox — a truncated page must not look like an empty one.
func TestInboxLimitIsClampedNotObeyed(t *testing.T) {
	ctx := testCtx(t)
	f := newInboxFixture(t, ctx)
	f.follow(t, f.aliceClient, events.TargetTypeProject, f.projectID, events.ChannelWeb)
	f.commits(t, 1)
	f.record(t, events.Event{EventType: eventTypePRMerged, ActorID: f.alice.ID,
		ProjectID: f.projectID, Visibility: events.VisibilityPublic, CorrelationID: "pr-1"})
	f.record(t, events.Event{EventType: eventTypeRelease, ActorID: f.alice.ID,
		ProjectID: f.projectID, Visibility: events.VisibilityPublic, CorrelationID: "rel-1"})
	f.publish(t)

	page := f.inbox(t, f.aliceClient, "limit=1")
	if len(page.Entries) != 1 {
		t.Errorf("limit=1 returned %d entries, want 1", len(page.Entries))
	}
	if page.UnreadCount != 3 {
		t.Errorf("unread_count = %d with limit=1, want 3 — the badge is the inbox's, not the page's",
			page.UnreadCount)
	}
	if page.Filter != events.InboxFilterUnread {
		t.Errorf("the default view echoed %q, want %q", page.Filter, events.InboxFilterUnread)
	}

	page = f.inbox(t, f.aliceClient, "filter=all&limit=1000")
	if len(page.Entries) != 3 {
		t.Errorf("filter=all returned %d entries, want 3 — a page above the maximum is capped, "+
			"not refused", len(page.Entries))
	}
	if page.Filter != events.InboxFilterAll {
		t.Errorf("the all view echoed %q, want %q", page.Filter, events.InboxFilterAll)
	}

	// A caller that names no page size gets the default page, not an empty
	// one: limit=0 is a caller saying "I named no limit", and refusing it
	// would make an absent query parameter a trap.
	resp := f.aliceClient.do(t, http.MethodGet, "/api/v1/inbox?limit=0", "")
	mustStatus(t, resp, http.StatusOK)
	var zero inboxJSON
	if err := json.Unmarshal([]byte(readAll(t, resp)), &zero); err != nil {
		t.Fatalf("decode the default page: %v", err)
	}
	if len(zero.Entries) != 3 {
		t.Errorf("limit=0 returned %d entries, want the default page of 3", len(zero.Entries))
	}
}
