// Task T1002 — required test "subscription tests".
//
// The unit suites pin the vocabulary and the pure rules (internal/events:
// target/filter/channel shape, Delivers, SubscribesTo, EventTargets;
// internal/application/subscriptions: the service's error mapping and
// owner scoping). This file pins what only a real PostgreSQL can settle:
// the audience rule against live rows, and the two things this task's
// acceptance criteria are about —
//
//  1. 取消订阅生效 — an unsubscribe stops the flow NOW, not at the next
//     fan-out pass. TestUnsubscribeTakesEffect proves the withdraw is
//     inside the unsubscribe's own transaction rather than a follow-up
//     statement: a second session holds the delivery row locked, so the
//     unsubscribe blocks mid-transaction while that session reads the
//     subscription row and finds deleted_at still NULL. A two-statement
//     implementation (or an autocommit one) would have committed the
//     soft delete by then, and the same read would say otherwise.
//
//  2. private target 权限变化后不继续泄漏 — a permission change takes
//     effect without anyone touching the subscription.
//     TestRevokedAccessStopsDelivery walks both directions over a real
//     permission change (a project membership removed, a public project
//     turned private) and asserts the two halves that matter: no NEW
//     delivery rows for the revoked subscriber, and the rows already in
//     flight for them WITHDRAWN. It also asserts the discriminating half —
//     the member who kept access still receives — so "stop the leak"
//     cannot be satisfied by stopping everything.
//
// The composition is the production one: events.SubscriptionStore,
// subscriptions.Service and events.SubscriptionFanOut over the same pool,
// driven exactly as cmd/worker drives them (dispatcher RunOnce → fan-out
// RunOnce). TestSubscriptionHTTPAuth composes the real guarded route the
// way cmd/api/main.go does.

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/subscriptionshttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/subscriptions"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

// subscriptionTaskID namespaces this task's test databases
// (test_T1002_<run_id>).
const subscriptionTaskID = "T1002"

// silentLogger keeps the fan-out's per-row logging out of the test output
// without silencing the assertions (the fan-out logs, it does not decide).
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --------------------------------------------------------------------------
// Fixture

// subscriptionFixture is one real database with the production pipeline
// wired over it, plus the rows the cases move around: one organization with
// a member and a non-member, a PRIVATE project both alice and bob belong
// to, a PUBLIC project neither is a member of, alice's own private
// project, and one published asset in each.
type subscriptionFixture struct {
	ctx        context.Context
	pool       *pgxpool.Pool
	store      *events.SubscriptionStore
	svc        *subscriptions.Service
	dispatcher *events.Dispatcher
	fanout     *events.SubscriptionFanOut

	alice, bob, carol domain.User
	orgID             string

	sharedPrivate string // private; alice and bob are members
	publicProject string // public; nobody is a member
	alicePrivate  string // private; alice only
	carolPrivate  string // private; carol only (the target alice must not be able to follow)

	sharedAssetPID string // published asset in sharedPrivate
	publicAssetPID string // published asset in publicProject
	carolAssetPID  string // published asset in carolPrivate
}

func newSubscriptionFixture(t *testing.T, ctx context.Context) *subscriptionFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), subscriptionTaskID)
	creds := persistence.NewCredentialStore(pool)
	seed := func(email, handle string) domain.User {
		u, err := creds.CreateWithPassword(ctx, email, "hash", handle, strings.ToUpper(handle[:1])+handle[1:])
		if err != nil {
			t.Fatalf("seed %s: %v", handle, err)
		}
		return u
	}
	f := &subscriptionFixture{
		ctx:        ctx,
		pool:       pool,
		store:      events.NewSubscriptionStore(pool),
		dispatcher: events.NewDispatcher(pool, events.WithLogger(silentLogger())),
		fanout:     events.NewSubscriptionFanOut(pool, events.WithSubscriptionFanOutLogger(silentLogger())),
		alice:      seed("sub-alice@example.com", "alice"),
		bob:        seed("sub-bob@example.com", "bob"),
		carol:      seed("sub-carol@example.com", "carol"),
	}
	f.svc = subscriptions.NewService(f.store)

	f.orgID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO organizations (slug, name) VALUES ('sub-lab', 'Subscription Lab') RETURNING id`)
	// alice is an ACTIVE member, bob is not a member at all.
	if _, err := pool.Exec(ctx,
		`INSERT INTO organization_memberships (organization_id, user_id, role, verified)
		 VALUES ($1, $2, 'maintainer', true)`, f.orgID, f.alice.ID); err != nil {
		t.Fatalf("seed organization membership: %v", err)
	}

	f.sharedPrivate = f.seedProject(t, "sub-shared-private", "private", f.alice.ID, f.bob.ID)
	f.publicProject = f.seedProject(t, "sub-public", "public")
	f.alicePrivate = f.seedProject(t, "sub-alice-private", "private", f.alice.ID)
	f.carolPrivate = f.seedProject(t, "sub-carol-private", "private", f.carol.ID)

	f.sharedAssetPID = f.seedAsset(t, f.sharedPrivate, "shared-asset",
		"01j9z6k3m4n5p6q7r8s9t0v1a1", "Shared asset")
	f.publicAssetPID = f.seedAsset(t, f.publicProject, "public-asset",
		"01j9z6k3m4n5p6q7r8s9t0v1a2", "Public asset")
	f.carolAssetPID = f.seedAsset(t, f.carolPrivate, "carol-asset",
		"01j9z6k3m4n5p6q7r8s9t0v1a3", "Carol's asset")
	return f
}

// seedProject inserts one project, optionally attaching members. The
// membership rows are what the project read gate reads (docs/12 §2), so
// this is also how a "permission change" is undone later.
func (f *subscriptionFixture) seedProject(t *testing.T, slug, visibility string, members ...string) string {
	t.Helper()
	id := mustQueryUUID(t, f.ctx, f.pool,
		`INSERT INTO projects (slug, name, purpose, visibility, created_by)
		 VALUES ($1, $2, 'T1002 subscription fixture', $3, $4) RETURNING id`,
		slug, slug, visibility, f.alice.ID)
	for _, m := range members {
		if _, err := f.pool.Exec(f.ctx,
			`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, 'owner')`,
			id, m); err != nil {
			t.Fatalf("seed membership on %s: %v", slug, err)
		}
	}
	return id
}

// seedAsset inserts one research asset and returns its pid (the identity a
// subscription addresses it by).
func (f *subscriptionFixture) seedAsset(t *testing.T, projectID, slug, pid, title string) string {
	t.Helper()
	mustQueryUUID(t, f.ctx, f.pool,
		`INSERT INTO research_assets (asset_type, slug, title, origin_project_id, pid)
		 VALUES ('dataset', $1, $2, $3, $4) RETURNING id`, slug, title, projectID, pid)
	return pid
}

// setMembership grants or removes one project membership — the app-layer
// permission change (project membership revocation is the change the
// product supports; a project's own visibility is flipped by
// setVisibility below, which is the same state change the read gate sees,
// written directly because the product refuses visibility edits by design
// — projects.ErrVisibilityChangeNotSupported — not because the pipeline
// reads anything but this column).
func (f *subscriptionFixture) setMembership(t *testing.T, projectID, userID string, member bool) {
	t.Helper()
	if member {
		_, err := f.pool.Exec(f.ctx,
			`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, 'contributor')
			 ON CONFLICT DO NOTHING`, projectID, userID)
		if err != nil {
			t.Fatalf("grant membership: %v", err)
		}
		return
	}
	if _, err := f.pool.Exec(f.ctx,
		`DELETE FROM project_memberships WHERE project_id = $1 AND user_id = $2`, projectID, userID); err != nil {
		t.Fatalf("revoke membership: %v", err)
	}
}

// setVisibility flips projects.visibility — the raw state change the
// audience rule reads.
func (f *subscriptionFixture) setVisibility(t *testing.T, projectID, visibility string) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE projects SET visibility = $2 WHERE id = $1`, projectID, visibility); err != nil {
		t.Fatalf("set visibility: %v", err)
	}
}

// publishAndFanOut records one event and runs both production passes once:
// the dispatcher publishes the outbox row into research_events, the
// subscription fan-out consumes it.
func (f *subscriptionFixture) publishAndFanOut(t *testing.T, e events.Event) string {
	t.Helper()
	if err := events.Record(f.ctx, f.pool, e); err != nil {
		t.Fatalf("record event %s: %v", e.EventType, err)
	}
	if _, err := f.dispatcher.RunOnce(f.ctx); err != nil {
		t.Fatalf("dispatcher RunOnce: %v", err)
	}
	if _, err := f.fanout.RunOnce(f.ctx); err != nil {
		t.Fatalf("fanout RunOnce: %v", err)
	}
	var eventID string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT id FROM research_events WHERE event_type = $1
		 ORDER BY occurred_at DESC, id DESC LIMIT 1`, e.EventType).Scan(&eventID); err != nil {
		t.Fatalf("read the published event id: %v", err)
	}
	return eventID
}

// fanOutOnly runs the fan-out again over whatever is already published —
// the second pass of an idempotency check.
func (f *subscriptionFixture) fanOutOnly(t *testing.T) {
	t.Helper()
	if _, err := f.fanout.RunOnce(f.ctx); err != nil {
		t.Fatalf("fanout RunOnce: %v", err)
	}
}

// deliveries returns the delivery rows of one subscriber for one target,
// oldest first, read with raw SQL rather than through the store so the
// assertions do not depend on the code under test's own reader.
func (f *subscriptionFixture) deliveries(t *testing.T, userID, targetType, targetID string) []events.SubscriptionDelivery {
	t.Helper()
	rows, err := f.pool.Query(f.ctx, `
		SELECT id, subscription_id, user_id, event_id, channel, event_type,
		       target_type, target_id, status, created_at, delivered_at, cancelled_at
		FROM subscription_deliveries
		WHERE user_id = $1 AND target_type = $2 AND target_id = $3
		ORDER BY created_at, id`, userID, targetType, targetID)
	if err != nil {
		t.Fatalf("query deliveries: %v", err)
	}
	defer rows.Close()
	var out []events.SubscriptionDelivery
	for rows.Next() {
		var d events.SubscriptionDelivery
		if err := rows.Scan(&d.ID, &d.SubscriptionID, &d.UserID, &d.EventID, &d.Channel, &d.EventType,
			&d.TargetType, &d.TargetID, &d.Status, &d.CreatedAt, &d.DeliveredAt, &d.CancelledAt); err != nil {
			t.Fatalf("scan delivery: %v", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate deliveries: %v", err)
	}
	return out
}

// statuses counts the delivery rows of one subscriber on one target by
// channel and status, e.g. {"email/pending": 1, "web/delivered": 1}.
func (f *subscriptionFixture) statuses(t *testing.T, userID, targetType, targetID string) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, d := range f.deliveries(t, userID, targetType, targetID) {
		out[d.Channel+"/"+d.Status]++
	}
	return out
}

// mustSubscribe subscribes through the SERVICE (the production subscribe
// path, audience check included) and fails the test when it refuses.
func (f *subscriptionFixture) mustSubscribe(t *testing.T, actor domain.User, target events.Target, filters, channels []string) events.Subscription {
	t.Helper()
	sub, err := f.svc.Subscribe(f.ctx, actor, target, filters, channels)
	if err != nil {
		t.Fatalf("subscribe %s to %s %s: %v", actor.Handle, target.Type, target.ID, err)
	}
	return sub
}

// aliceEvent is the event most cases publish: alice acting inside the
// shared private project.
func (f *subscriptionFixture) aliceEvent(eventType, visibility string, payload string) events.Event {
	return events.Event{
		EventType:  eventType,
		ActorID:    f.alice.ID,
		ProjectID:  f.sharedPrivate,
		Visibility: visibility,
		Payload:    json.RawMessage(payload),
	}
}

// --------------------------------------------------------------------------
// 1. Targets, the audience rule, and owner scoping (public/private auth)

// TestSubscriptionTargetsAndAudience covers the five target types the model
// declares and the authorization half of "public/private auth": a
// subscription may only be created for a target the actor may CURRENTLY
// see, and a target the actor may not see answers exactly what an unknown
// target answers.
func TestSubscriptionTargetsAndAudience(t *testing.T) {
	ctx := testCtx(t)
	f := newSubscriptionFixture(t, ctx)

	t.Run("every target type is followable at the level its audience allows", func(t *testing.T) {
		cases := []struct {
			name   string
			actor  domain.User
			target events.Target
			want   bool
		}{
			{"private project as a member", f.alice, events.Target{Type: events.TargetTypeProject, ID: f.sharedPrivate}, true},
			{"private project as a non-member", f.bob, events.Target{Type: events.TargetTypeProject, ID: f.carolPrivate}, false},
			{"public project as a non-member", f.bob, events.Target{Type: events.TargetTypeProject, ID: f.publicProject}, true},
			{"private asset as a member", f.alice, events.Target{Type: events.TargetTypeAsset, ID: f.sharedAssetPID}, true},
			{"private asset as a non-member", f.bob, events.Target{Type: events.TargetTypeAsset, ID: f.carolAssetPID}, false},
			{"public asset as a non-member", f.bob, events.Target{Type: events.TargetTypeAsset, ID: f.publicAssetPID}, true},
			{"organization as an active member", f.alice, events.Target{Type: events.TargetTypeOrganization, ID: f.orgID}, true},
			{"organization as a non-member", f.bob, events.Target{Type: events.TargetTypeOrganization, ID: f.orgID}, true},
			{"a person's own profile", f.carol, events.Target{Type: events.TargetTypeUser, ID: f.alice.ID}, true},
			{"an unknown project id", f.alice, events.Target{Type: events.TargetTypeProject, ID: "11111111-2222-3333-4444-555555555555"}, false},
			{"an unknown asset pid", f.alice, events.Target{Type: events.TargetTypeAsset, ID: "01j9z6k3m4n5p6q7r8s9t0v9z9"}, false},
			{"an unknown organization id", f.alice, events.Target{Type: events.TargetTypeOrganization, ID: "11111111-2222-3333-4444-555555555556"}, false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := f.svc.Subscribe(ctx, tc.actor, tc.target, nil, []string{events.ChannelWeb})
				switch {
				case tc.want && err != nil:
					t.Fatalf("subscribe to a visible target = %v, want nil", err)
				case !tc.want && !errors.Is(err, subscriptions.ErrNotFound):
					t.Fatalf("subscribe to an unseen target = %v, want ErrNotFound", err)
				}
			})
		}
	})

	t.Run("an existing subscription is not readable, editable or endable by another account", func(t *testing.T) {
		sub := f.mustSubscribe(t, f.alice, events.Target{Type: events.TargetTypeProject, ID: f.publicProject},
			nil, []string{events.ChannelWeb})

		// Carol may not see the target at all, and bob may see it but does
		// not own the subscription: both must answer "not found", which is
		// also the answer for an id that does not exist.
		if _, err := f.svc.Get(ctx, f.bob, sub.ID); !errors.Is(err, subscriptions.ErrNotFound) {
			t.Errorf("Get by a foreign account = %v, want ErrNotFound", err)
		}
		if _, err := f.svc.Update(ctx, f.bob, sub.ID, nil, []string{events.ChannelEmail}); !errors.Is(err, subscriptions.ErrNotFound) {
			t.Errorf("Update by a foreign account = %v, want ErrNotFound", err)
		}
		if err := f.svc.Unsubscribe(ctx, f.bob, sub.ID); !errors.Is(err, subscriptions.ErrNotFound) {
			t.Errorf("Unsubscribe by a foreign account = %v, want ErrNotFound", err)
		}
		if _, err := f.svc.Get(ctx, f.bob, "11111111-2222-3333-4444-555555555557"); !errors.Is(err, subscriptions.ErrNotFound) {
			t.Errorf("Get of an unknown id = %v, want ErrNotFound", err)
		}

		// The owner's row is untouched by any of it.
		still, err := f.svc.Get(ctx, f.alice, sub.ID)
		if err != nil {
			t.Fatalf("the owner's subscription disappeared: %v", err)
		}
		if len(still.Channels) != 1 || still.Channels[0] != events.ChannelWeb {
			t.Errorf("the foreign PATCH reached the row: channels = %v", still.Channels)
		}

		// Listing is owner-scoped too: bob's own follow of the same target
		// is the only row his watch-state read returns.
		if list, err := f.svc.List(ctx, f.bob, events.TargetTypeProject, f.publicProject); err != nil {
			t.Fatalf("List for bob: %v", err)
		} else if len(list) != 1 || list[0].UserID != f.bob.ID {
			t.Errorf("bob's list = %d rows, want 0", len(list))
		}
	})
}

// TestSubscriptionValidation covers the shape rules the service enforces
// before anything reaches the store — the boundary that keeps a malformed
// target from becoming a query error, and a malformed channel list from
// becoming a subscription that can never deliver.
func TestSubscriptionValidation(t *testing.T) {
	ctx := testCtx(t)
	f := newSubscriptionFixture(t, ctx)
	ok := events.Target{Type: events.TargetTypeProject, ID: f.publicProject}

	cases := []struct {
		name     string
		target   events.Target
		filters  []string
		channels []string
	}{
		{"unknown target type", events.Target{Type: "planet", ID: f.publicProject}, nil, []string{events.ChannelWeb}},
		{"project id that is not a uuid", events.Target{Type: events.TargetTypeProject, ID: "not-a-uuid"}, nil, []string{events.ChannelWeb}},
		{"asset id that is not a pid", events.Target{Type: events.TargetTypeAsset, ID: f.publicProject}, nil, []string{events.ChannelWeb}},
		{"empty target id", events.Target{Type: events.TargetTypeProject, ID: ""}, nil, []string{events.ChannelWeb}},
		{"no channels", ok, nil, nil},
		{"empty channel list", ok, nil, []string{}},
		{"unknown channel", ok, nil, []string{"carrier-pigeon"}},
		{"duplicate channel", ok, nil, []string{events.ChannelWeb, events.ChannelWeb}},
		{"filter with invalid characters", ok, []string{"state committed"}, []string{events.ChannelWeb}},
		{"empty filter", ok, []string{""}, []string{events.ChannelWeb}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.svc.Subscribe(ctx, f.alice, tc.target, tc.filters, tc.channels); !errors.Is(err, subscriptions.ErrValidation) {
				t.Fatalf("Subscribe = %v, want ErrValidation", err)
			}
		})
	}

	// The negative control: the same shape checks accept the well-formed
	// input the cases above are mutations of. Without it, "every case
	// failed" would also be satisfied by a service that refuses everything.
	if _, err := f.svc.Subscribe(ctx, f.alice, ok, []string{"state.committed"}, []string{events.ChannelWeb, events.ChannelEmail}); err != nil {
		t.Fatalf("Subscribe with valid input = %v, want nil", err)
	}

	sub := f.mustSubscribe(t, f.bob, events.Target{Type: events.TargetTypeProject, ID: f.publicProject}, nil, []string{events.ChannelWeb})
	for _, id := range []string{"not-a-uuid", ""} {
		if _, err := f.svc.Get(ctx, f.bob, id); !errors.Is(err, subscriptions.ErrValidation) {
			t.Errorf("Get(%q) = %v, want ErrValidation", id, err)
		}
		if err := f.svc.Unsubscribe(ctx, f.bob, id); !errors.Is(err, subscriptions.ErrValidation) {
			t.Errorf("Unsubscribe(%q) = %v, want ErrValidation", id, err)
		}
	}
	if _, err := f.svc.Get(ctx, f.bob, sub.ID); err != nil {
		t.Errorf("Get of a real id = %v, want nil", err)
	}
}

// TestSubscriptionLifecycle covers the parts of the write path the HTTP
// surface exposes: one live subscription per (user, target), a re-follow
// after unsubscribing that does not resurrect the old row, a PATCH that
// replaces both lists, and the per-user cap.
func TestSubscriptionLifecycle(t *testing.T) {
	ctx := testCtx(t)
	f := newSubscriptionFixture(t, ctx)
	target := events.Target{Type: events.TargetTypeProject, ID: f.publicProject}

	sub := f.mustSubscribe(t, f.alice, target, nil, []string{events.ChannelWeb})
	if _, err := f.svc.Subscribe(ctx, f.alice, target, nil, []string{events.ChannelEmail}); !errors.Is(err, subscriptions.ErrExists) {
		t.Fatalf("second live subscribe = %v, want ErrExists", err)
	}

	updated, err := f.svc.Update(ctx, f.alice, sub.ID, []string{"state.committed"}, []string{events.ChannelWeb, events.ChannelEmail})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(updated.EventFilters) != 1 || updated.EventFilters[0] != "state.committed" {
		t.Errorf("filters after PATCH = %v", updated.EventFilters)
	}
	if len(updated.Channels) != 2 {
		t.Errorf("channels after PATCH = %v", updated.Channels)
	}

	// The replacement is a FULL replacement: patching back to "no filters"
	// must actually clear them, not leave the old list in place.
	cleared, err := f.svc.Update(ctx, f.alice, sub.ID, nil, []string{events.ChannelWeb})
	if err != nil {
		t.Fatalf("Update to empty filters: %v", err)
	}
	if len(cleared.EventFilters) != 0 {
		t.Errorf("filters after clearing = %v, want empty (empty means every event type)", cleared.EventFilters)
	}

	if err := f.svc.Unsubscribe(ctx, f.alice, sub.ID); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	if _, err := f.svc.Get(ctx, f.alice, sub.ID); !errors.Is(err, subscriptions.ErrNotFound) {
		t.Errorf("Get after unsubscribe = %v, want ErrNotFound", err)
	}
	if err := f.svc.Unsubscribe(ctx, f.alice, sub.ID); !errors.Is(err, subscriptions.ErrNotFound) {
		t.Errorf("second Unsubscribe = %v, want ErrNotFound", err)
	}

	// Nothing disappears: the ended row is still there, stamped.
	var deletedAt *time.Time
	if err := f.pool.QueryRow(ctx,
		`SELECT deleted_at FROM subscriptions WHERE id = $1`, sub.ID).Scan(&deletedAt); err != nil {
		t.Fatalf("the ended subscription row is gone: %v (nothing disappears — CLAUDE.md §9)", err)
	}
	if deletedAt == nil {
		t.Fatal("the ended subscription is not stamped deleted_at")
	}

	// Re-following inserts a NEW row: the old one and its deliveries stay
	// as history.
	again := f.mustSubscribe(t, f.alice, target, nil, []string{events.ChannelWeb})
	if again.ID == sub.ID {
		t.Fatalf("re-following resurrected the ended row %s", sub.ID)
	}
	if list, err := f.svc.List(ctx, f.alice, events.TargetTypeProject, f.publicProject); err != nil {
		t.Fatalf("List: %v", err)
	} else if len(list) != 1 || list[0].ID != again.ID {
		t.Errorf("the live list after re-following = %+v, want exactly the new row", list)
	}
}

// TestSubscriptionLimit drives the per-user cap with real rows: the
// MaxSubscriptionsPerUser'th subscription is accepted and the next one is
// refused. The cap is what keeps one account from making the fan-out's
// per-event candidate scan unbounded, so it is asserted against the
// constant rather than a copy of it.
func TestSubscriptionLimit(t *testing.T) {
	ctx := testCtx(t)
	f := newSubscriptionFixture(t, ctx)

	// Distinct public projects to follow, seeded in one statement.
	var ids []string
	rows, err := f.pool.Query(ctx, `
		INSERT INTO projects (slug, name, purpose, visibility, created_by)
		SELECT 'sub-cap-' || g, 'cap ' || g, 'T1002 cap fixture', 'public', $1
		FROM generate_series(1, $2) AS g
		RETURNING id`, f.alice.ID, events.MaxSubscriptionsPerUser)
	if err != nil {
		t.Fatalf("seed cap projects: %v", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan project id: %v", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate project ids: %v", err)
	}

	var first events.Subscription
	for i, id := range ids {
		sub, err := f.svc.Subscribe(ctx, f.alice,
			events.Target{Type: events.TargetTypeProject, ID: id}, nil, []string{events.ChannelWeb})
		if err != nil {
			t.Fatalf("subscribe %d of %d = %v, want nil", i+1, events.MaxSubscriptionsPerUser, err)
		}
		if i == 0 {
			first = sub
		}
	}
	if _, err := f.svc.Subscribe(ctx, f.alice,
		events.Target{Type: events.TargetTypeProject, ID: f.publicProject}, nil, []string{events.ChannelWeb}); !errors.Is(err, subscriptions.ErrLimit) {
		t.Fatalf("subscribe past the cap = %v, want ErrLimit", err)
	}
	// The cap counts LIVE subscriptions only: ending one frees a slot.
	if err := f.svc.Unsubscribe(ctx, f.alice, first.ID); err != nil {
		t.Fatalf("unsubscribe one of the cap rows: %v", err)
	}
	if _, err := f.svc.Subscribe(ctx, f.alice,
		events.Target{Type: events.TargetTypeProject, ID: f.publicProject}, nil, []string{events.ChannelWeb}); err != nil {
		t.Fatalf("subscribe after freeing a slot = %v, want nil", err)
	}
}

// --------------------------------------------------------------------------
// 2. The fan-out: filters, channels, and the visibility rule

// TestSubscriptionFanOutCoversFiltersChannelsAndVisibility walks the
// delivery decision over real events: which event types a subscription's
// filters admit, which channel each delivery row is born on, and the rule
// that an event is never more visible than its subject.
func TestSubscriptionFanOutCoversFiltersChannelsAndVisibility(t *testing.T) {
	ctx := testCtx(t)
	f := newSubscriptionFixture(t, ctx)
	target := events.Target{Type: events.TargetTypeProject, ID: f.sharedPrivate}

	// alice: everything, both channels. bob: only state.committed, web only.
	f.mustSubscribe(t, f.alice, target, nil, []string{events.ChannelWeb, events.ChannelEmail})
	f.mustSubscribe(t, f.bob, target, []string{"state.committed"}, []string{events.ChannelWeb})

	f.publishAndFanOut(t, f.aliceEvent("state.committed", events.VisibilityPrivate, `{}`))

	want := map[string]int{"web/delivered": 1, "email/pending": 1}
	if got := f.statuses(t, f.alice.ID, events.TargetTypeProject, f.sharedPrivate); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("alice's deliveries = %v, want %v (a member receives a private event on both channels; "+
			"web is born delivered, email is born pending for the digest sender)", got, want)
	}
	if got := f.statuses(t, f.bob.ID, events.TargetTypeProject, f.sharedPrivate); fmt.Sprint(got) != fmt.Sprint(map[string]int{"web/delivered": 1}) {
		t.Fatalf("bob's deliveries = %v, want one web row (his filter admits state.committed)", got)
	}

	// A second event type: alice's unfiltered subscription takes it, bob's
	// filter does not admit it and must produce nothing.
	f.publishAndFanOut(t, f.aliceEvent("branch.created", events.VisibilityPrivate, `{}`))
	if n := len(f.deliveries(t, f.bob.ID, events.TargetTypeProject, f.sharedPrivate)); n != 1 {
		t.Errorf("bob has %d rows after branch.created, want still 1 (his filter is state.committed)", n)
	}
	if n := len(f.deliveries(t, f.alice.ID, events.TargetTypeProject, f.sharedPrivate)); n != 4 {
		t.Errorf("alice has %d rows after two events on two channels, want 4", n)
	}

	// Idempotency: the fan-out's cursor and the (subscription, event,
	// channel) unique index both refuse a second pass.
	f.fanOutOnly(t)
	f.fanOutOnly(t)
	if n := len(f.deliveries(t, f.alice.ID, events.TargetTypeProject, f.sharedPrivate)); n != 4 {
		t.Errorf("re-running the fan-out produced %d rows, want the same 4", n)
	}
}

// TestSubscriptionVisibilityRule separates the two halves of the audience
// rule with one event: a PRIVATE event in a PUBLIC project reaches the
// project's members and not its public followers. A subscription is not a
// licence to see more than the subject's own read gate allows.
func TestSubscriptionVisibilityRule(t *testing.T) {
	ctx := testCtx(t)
	f := newSubscriptionFixture(t, ctx)

	// carol is a member of the public project; bob only follows it.
	f.setMembership(t, f.publicProject, f.carol.ID, true)
	target := events.Target{Type: events.TargetTypeProject, ID: f.publicProject}
	f.mustSubscribe(t, f.bob, target, nil, []string{events.ChannelWeb, events.ChannelEmail})
	f.mustSubscribe(t, f.carol, target, nil, []string{events.ChannelWeb, events.ChannelEmail})

	f.publishAndFanOut(t, events.Event{
		EventType:  "state.committed",
		ActorID:    f.alice.ID,
		ProjectID:  f.publicProject,
		Visibility: events.VisibilityPrivate,
		Payload:    json.RawMessage(`{}`),
	})
	if n := len(f.deliveries(t, f.bob.ID, events.TargetTypeProject, f.publicProject)); n != 0 {
		t.Errorf("a public-level follower of a PUBLIC project received %d rows for a PRIVATE event, want 0", n)
	}
	if got := f.statuses(t, f.carol.ID, events.TargetTypeProject, f.publicProject); len(got) != 2 {
		t.Errorf("the project's member received %v, want both channels", got)
	}

	// The control: the same follower DOES receive the public event, so the
	// assertion above is about the event's visibility and not about a
	// pipeline that delivers nothing.
	f.publishAndFanOut(t, events.Event{
		EventType:  "state.committed",
		ActorID:    f.alice.ID,
		ProjectID:  f.publicProject,
		Visibility: events.VisibilityPublic,
		Payload:    json.RawMessage(`{}`),
	})
	if got := f.statuses(t, f.bob.ID, events.TargetTypeProject, f.publicProject); len(got) != 2 {
		t.Errorf("the public follower received %v for a PUBLIC event, want both channels", got)
	}
}

// TestSubscriptionAssetTargetDelivery covers the asset target: the pid a
// publish event carries in its payload addresses the asset, and an asset
// subscription's audience is its origin project's.
func TestSubscriptionAssetTargetDelivery(t *testing.T) {
	ctx := testCtx(t)
	f := newSubscriptionFixture(t, ctx)

	f.mustSubscribe(t, f.alice, events.Target{Type: events.TargetTypeAsset, ID: f.sharedAssetPID}, nil,
		[]string{events.ChannelWeb})
	// carol may not see the asset's origin project, so she cannot follow
	// the asset either — the refusal is asserted here rather than assumed,
	// because "she has no deliveries" below would otherwise be satisfied by
	// a subscription that was never created for a different reason.
	if _, err := f.svc.Subscribe(ctx, f.carol,
		events.Target{Type: events.TargetTypeAsset, ID: f.sharedAssetPID}, nil, []string{events.ChannelWeb}); !errors.Is(err, subscriptions.ErrNotFound) {
		t.Fatalf("a non-member following a private project's asset = %v, want ErrNotFound", err)
	}
	// bob follows the asset's project but NOT the asset itself: the two
	// targets are separate subscriptions, and this event addresses both.
	f.mustSubscribe(t, f.bob, events.Target{Type: events.TargetTypeProject, ID: f.sharedPrivate}, nil,
		[]string{events.ChannelWeb})

	f.publishAndFanOut(t, events.Event{
		EventType:  events.EventTypeAssetVersionPublished,
		ActorID:    f.alice.ID,
		ProjectID:  f.sharedPrivate,
		Visibility: events.VisibilityPrivate,
		Payload:    json.RawMessage(`{"asset_id":"` + f.sharedAssetPID + `"}`),
	})

	if n := len(f.deliveries(t, f.alice.ID, events.TargetTypeAsset, f.sharedAssetPID)); n != 1 {
		t.Errorf("the asset subscriber got %d rows, want 1", n)
	}
	if n := len(f.deliveries(t, f.bob.ID, events.TargetTypeProject, f.sharedPrivate)); n != 1 {
		t.Errorf("the project subscriber got %d rows, want 1 (the event addresses both targets)", n)
	}
	// carol could not see the private project, so the subscribe itself was
	// refused and there is nothing to deliver to — and no oracle either.
	if n := len(f.deliveries(t, f.carol.ID, events.TargetTypeAsset, f.sharedAssetPID)); n != 0 {
		t.Errorf("a non-member received %d rows for a private project's asset, want 0", n)
	}
}

// TestSubscriptionOrganizationTargetDelivery covers the organization
// target the fan-out resolves from the project's owner: an active member
// receives the project's events, a non-member follow is at the public
// level and receives only the public ones.
func TestSubscriptionOrganizationTargetDelivery(t *testing.T) {
	ctx := testCtx(t)
	f := newSubscriptionFixture(t, ctx)
	if _, err := f.pool.Exec(ctx, `UPDATE projects SET organization_id = $2 WHERE id = $1`,
		f.publicProject, f.orgID); err != nil {
		t.Fatalf("attach the project to the organization: %v", err)
	}

	org := events.Target{Type: events.TargetTypeOrganization, ID: f.orgID}
	f.mustSubscribe(t, f.alice, org, nil, []string{events.ChannelWeb, events.ChannelEmail})
	f.mustSubscribe(t, f.bob, org, nil, []string{events.ChannelWeb, events.ChannelEmail})

	f.publishAndFanOut(t, events.Event{
		EventType:  "state.committed",
		ActorID:    f.alice.ID,
		ProjectID:  f.publicProject,
		Visibility: events.VisibilityPrivate,
		Payload:    json.RawMessage(`{}`),
	})
	if got := f.statuses(t, f.alice.ID, events.TargetTypeOrganization, f.orgID); len(got) != 2 {
		t.Errorf("the active organization member received %v, want both channels", got)
	}
	if n := len(f.deliveries(t, f.bob.ID, events.TargetTypeOrganization, f.orgID)); n != 0 {
		t.Errorf("the non-member organization follower received %d rows for a private event, want 0", n)
	}
}

// --------------------------------------------------------------------------
// 3. Acceptance criterion 1: 取消订阅生效

// TestUnsubscribeTakesEffect is the first acceptance criterion. It asserts
// three things, in increasing strength:
//
//  1. after Unsubscribe returns, none of the subscription's in-flight
//     (pending) deliveries remain pending — the unsubscribe reaches rows
//     that were fanned out BEFORE it, not just future ones;
//  2. the next event produces no rows for the ended subscription;
//  3. the withdrawal happens inside the unsubscribe's OWN transaction. (3)
//     is the one a two-statement implementation would fail, so it is
//     proved rather than asserted: a second session takes the delivery row
//     FOR UPDATE, which makes the unsubscribe block inside its
//     transaction, and that session then reads the subscription row and
//     must still see deleted_at IS NULL — the soft delete cannot have
//     committed yet. When the lock is released both land together.
func TestUnsubscribeTakesEffect(t *testing.T) {
	ctx := testCtx(t)
	f := newSubscriptionFixture(t, ctx)
	target := events.Target{Type: events.TargetTypeProject, ID: f.sharedPrivate}

	sub := f.mustSubscribe(t, f.alice, target, nil, []string{events.ChannelWeb, events.ChannelEmail})
	f.mustSubscribe(t, f.bob, target, nil, []string{events.ChannelWeb, events.ChannelEmail})
	f.publishAndFanOut(t, f.aliceEvent("state.committed", events.VisibilityPrivate, `{}`))

	if got := f.statuses(t, f.alice.ID, events.TargetTypeProject, f.sharedPrivate); fmt.Sprint(got) != fmt.Sprint(map[string]int{"web/delivered": 1, "email/pending": 1}) {
		t.Fatalf("before the unsubscribe alice holds %v, want one delivered web row and one pending email row", got)
	}

	// --- (3) the atomicity probe, before the ordinary unsubscribe --------
	blocker, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire the blocking session: %v", err)
	}
	defer blocker.Release()
	tx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the blocking transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var heldID string
	if err := tx.QueryRow(ctx, `
		SELECT id FROM subscription_deliveries
		WHERE subscription_id = $1 AND status = 'pending'
		ORDER BY id LIMIT 1 FOR UPDATE`, sub.ID).Scan(&heldID); err != nil {
		t.Fatalf("lock a pending delivery row: %v", err)
	}

	unsubDone := make(chan error, 1)
	go func() { unsubDone <- f.svc.Unsubscribe(ctx, f.alice, sub.ID) }()

	if !f.waitForLockWait(t, "subscription_deliveries") {
		t.Fatalf("the unsubscribe never blocked on the locked delivery row: the withdraw is not " +
			"part of the same transaction as the soft delete (or it does not touch pending rows at all)")
	}
	// The unsubscribe is now mid-transaction, waiting on the row this
	// session holds. If the soft delete were a separate (or autocommit)
	// statement it would already be visible here.
	var deletedAt *time.Time
	if err := f.pool.QueryRow(ctx,
		`SELECT deleted_at FROM subscriptions WHERE id = $1`, sub.ID).Scan(&deletedAt); err != nil {
		t.Fatalf("read the subscription while the unsubscribe is blocked: %v", err)
	}
	if deletedAt != nil {
		t.Errorf("deleted_at is already set while the withdraw is still blocked: the unsubscribe commits " +
			"its soft delete separately from the withdraw, so a crash between them leaves queued email for a subscription the user ended")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("release the lock: %v", err)
	}
	if err := <-unsubDone; err != nil {
		t.Fatalf("Unsubscribe = %v, want nil", err)
	}

	// --- (1) nothing is left in flight, immediately ---------------------
	if got := f.statuses(t, f.alice.ID, events.TargetTypeProject, f.sharedPrivate); fmt.Sprint(got) != fmt.Sprint(map[string]int{"web/delivered": 1, "email/cancelled": 1}) {
		t.Fatalf("after the unsubscribe alice holds %v, want the pending email withdrawn and the "+
			"delivered web row untouched (rows already delivered stay — nothing disappears)", got)
	}

	// --- (2) the ended subscription receives nothing more ----------------
	before := len(f.deliveries(t, f.alice.ID, events.TargetTypeProject, f.sharedPrivate))
	f.publishAndFanOut(t, f.aliceEvent("state.committed", events.VisibilityPrivate, `{}`))
	if after := len(f.deliveries(t, f.alice.ID, events.TargetTypeProject, f.sharedPrivate)); after != before {
		t.Errorf("the ended subscription gained %d rows on the next event, want 0", after-before)
	}

	// The ended subscription's rows are still readable — the withdraw is a
	// status change, not a delete (nothing disappears).
	kept, err := f.store.Deliveries(ctx, f.pool, f.alice.ID, "", 100)
	if err != nil {
		t.Fatalf("read alice's deliveries: %v", err)
	}
	if len(kept) != 2 {
		t.Fatalf("alice's delivery log holds %d rows after the unsubscribe, want both (one delivered, one withdrawn)", len(kept))
	}

	// The other subscriber is unaffected: the unsubscribe is one account's.
	if n := len(f.deliveries(t, f.bob.ID, events.TargetTypeProject, f.sharedPrivate)); n != 4 {
		t.Errorf("bob has %d rows, want 4 (two events on two channels): another account's unsubscribe "+
			"must not touch his", n)
	}
}

// waitForLockWait reports whether some other backend is currently BLOCKED
// on a lock while running a statement mentioning the given relation — the
// observable signature of the unsubscribe stalling inside its transaction.
func (f *subscriptionFixture) waitForLockWait(t *testing.T, relation string) bool {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := f.pool.QueryRow(f.ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database()
			  AND pid <> pg_backend_pid()
			  AND state = 'active'
			  AND wait_event_type = 'Lock'
			  AND query LIKE '%' || $1 || '%'`, relation).Scan(&n); err != nil {
			t.Fatalf("probe pg_stat_activity: %v", err)
		}
		if n > 0 {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// --------------------------------------------------------------------------
// 4. Acceptance criterion 2: private target 权限变化后不继续泄漏

// TestRevokedAccessStopsDelivery is the second acceptance criterion. Alice
// and bob both follow a private project and both receive its events; the
// permission then changes in the two ways it can change, and each time the
// assertions are the same three, plus the control that says the leak
// stopped without stopping the flow:
//
//	(a) the revoked subscriber gets NO new delivery rows;
//	(b) the rows already in flight for them are WITHDRAWN (cancelled), not
//	    left to arrive later;
//	(c) the subscriber who kept access still receives, and their in-flight
//	    rows stay pending.
//
// Nobody touches the subscription in between: the fan-out re-resolves the
// audience before every delivery, which is the whole mechanism.
func TestRevokedAccessStopsDelivery(t *testing.T) {
	ctx := testCtx(t)
	target := events.Target{Type: events.TargetTypeProject, ID: "PLACEHOLDER"}

	t.Run("a project membership removed", func(t *testing.T) {
		f := newSubscriptionFixture(t, ctx)
		target.ID = f.sharedPrivate
		f.mustSubscribe(t, f.alice, target, nil, []string{events.ChannelWeb, events.ChannelEmail})
		f.mustSubscribe(t, f.bob, target, nil, []string{events.ChannelWeb, events.ChannelEmail})
		f.publishAndFanOut(t, f.aliceEvent("state.committed", events.VisibilityPrivate, `{}`))

		if got := f.statuses(t, f.bob.ID, events.TargetTypeProject, f.sharedPrivate); fmt.Sprint(got) != fmt.Sprint(map[string]int{"web/delivered": 1, "email/pending": 1}) {
			t.Fatalf("bob held %v before the revocation, want one delivered and one pending row", got)
		}

		// The permission change: bob is removed from the private project.
		f.setMembership(t, f.sharedPrivate, f.bob.ID, false)

		// The next event is the moment the change must take effect.
		f.publishAndFanOut(t, f.aliceEvent("state.committed", events.VisibilityPrivate, `{}`))

		got := f.statuses(t, f.bob.ID, events.TargetTypeProject, f.sharedPrivate)
		want := map[string]int{"web/delivered": 1, "email/cancelled": 1}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("after the revocation bob holds %v, want %v: no new row for the second event, and "+
				"the pending email withdrawn rather than left to arrive", got, want)
		}

		// (b) more precisely: the row that was pending is the one that was
		// cancelled, not a new one.
		for _, d := range f.deliveries(t, f.bob.ID, events.TargetTypeProject, f.sharedPrivate) {
			if d.Channel == events.ChannelEmail {
				if d.Status != events.SubscriptionDeliveryCancelled || d.CancelledAt == nil {
					t.Errorf("the in-flight email row = %s (cancelled_at %v), want cancelled with a timestamp", d.Status, d.CancelledAt)
				}
			}
		}

		// (c) the control: alice kept access and is unaffected.
		if got := f.statuses(t, f.alice.ID, events.TargetTypeProject, f.sharedPrivate); fmt.Sprint(got) != fmt.Sprint(map[string]int{"web/delivered": 2, "email/pending": 2}) {
			t.Errorf("alice holds %v, want two of each — the revocation of ONE subscriber must not "+
				"interrupt the flow to the others", got)
		}

		// Nobody edited the subscription: it is still live, still there.
		subs, err := f.store.LiveSubscriptionsForTarget(ctx, f.pool, target)
		if err != nil {
			t.Fatalf("read the live subscriptions: %v", err)
		}
		if len(subs) != 2 {
			t.Fatalf("live subscriptions on the target = %d, want 2 (the revocation must act through "+
				"the audience rule, not by deleting rows)", len(subs))
		}
	})

	t.Run("a public project turned private", func(t *testing.T) {
		f := newSubscriptionFixture(t, ctx)
		target := events.Target{Type: events.TargetTypeProject, ID: f.publicProject}
		// bob is a plain follower at the public level; carol is a member.
		f.setMembership(t, f.publicProject, f.carol.ID, true)
		f.mustSubscribe(t, f.bob, target, nil, []string{events.ChannelWeb, events.ChannelEmail})
		f.mustSubscribe(t, f.carol, target, nil, []string{events.ChannelWeb, events.ChannelEmail})

		pubEvent := func() events.Event {
			return events.Event{
				EventType:  "state.committed",
				ActorID:    f.alice.ID,
				ProjectID:  f.publicProject,
				Visibility: events.VisibilityPublic,
				Payload:    json.RawMessage(`{}`),
			}
		}
		f.publishAndFanOut(t, pubEvent())
		if got := f.statuses(t, f.bob.ID, events.TargetTypeProject, f.publicProject); len(got) != 2 {
			t.Fatalf("bob held %v while the project was public, want both channels", got)
		}

		// The permission change: the project stops being public. The
		// product refuses this edit through its API by design
		// (projects.ErrVisibilityChangeNotSupported); the column is what
		// the read gate AND this pipeline read, so the state change is
		// written directly here.
		f.setVisibility(t, f.publicProject, "private")

		f.publishAndFanOut(t, pubEvent())
		got := f.statuses(t, f.bob.ID, events.TargetTypeProject, f.publicProject)
		want := map[string]int{"web/delivered": 1, "email/cancelled": 1}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("after the project turned private bob holds %v, want %v: a follower at the public "+
				"level must receive nothing more and lose the queued email", got, want)
		}
		if got := f.statuses(t, f.carol.ID, events.TargetTypeProject, f.publicProject); fmt.Sprint(got) != fmt.Sprint(map[string]int{"web/delivered": 2, "email/pending": 2}) {
			t.Errorf("the member holds %v, want two of each — the project turning private is not a "+
				"reason to stop delivering to the people who can still see it", got)
		}

		// And back: the flip is tracked in both directions, so the
		// mechanism is a live read and not a one-way latch.
		f.setVisibility(t, f.publicProject, "public")
		f.publishAndFanOut(t, pubEvent())
		if got := f.statuses(t, f.bob.ID, events.TargetTypeProject, f.publicProject); fmt.Sprint(got) != fmt.Sprint(map[string]int{"web/delivered": 2, "email/pending": 1, "email/cancelled": 1}) {
			t.Errorf("after the project became public again bob holds %v, want the new event delivered "+
				"beside the withdrawn one", got)
		}
	})

	t.Run("a membership removed from a project that stays public", func(t *testing.T) {
		f := newSubscriptionFixture(t, ctx)
		target := events.Target{Type: events.TargetTypeProject, ID: f.publicProject}
		// carol is a member, so she was at the member level and received
		// the project's PRIVATE events; bob is a plain public follower.
		f.setMembership(t, f.publicProject, f.carol.ID, true)
		f.mustSubscribe(t, f.carol, target, nil, []string{events.ChannelWeb, events.ChannelEmail})
		f.mustSubscribe(t, f.bob, target, nil, []string{events.ChannelWeb, events.ChannelEmail})
		event := func(visibility string) events.Event {
			return events.Event{
				EventType: "state.committed", ActorID: f.alice.ID, ProjectID: f.publicProject,
				Visibility: visibility, Payload: json.RawMessage(`{}`),
			}
		}
		f.publishAndFanOut(t, event(events.VisibilityPublic))
		f.publishAndFanOut(t, event(events.VisibilityPrivate))
		if got := f.statuses(t, f.carol.ID, events.TargetTypeProject, f.publicProject); fmt.Sprint(got) != fmt.Sprint(map[string]int{"web/delivered": 2, "email/pending": 2}) {
			t.Fatalf("the member held %v, want two events on two channels", got)
		}

		// The permission change: carol leaves the project, which STAYS
		// public. Her level drops from member to public — it does not
		// vanish — so only the private event's queued notification is no
		// longer hers to receive.
		f.setMembership(t, f.publicProject, f.carol.ID, false)
		f.publishAndFanOut(t, event(events.VisibilityPublic))

		for _, d := range f.deliveries(t, f.carol.ID, events.TargetTypeProject, f.publicProject) {
			if d.Channel != events.ChannelEmail {
				continue
			}
			// Read the visibility of the event each pending row points at:
			// the assertion is about which ROW was withdrawn, and that is
			// decided by the event's own visibility.
			var visibility string
			if err := f.pool.QueryRow(ctx, `SELECT visibility FROM research_events WHERE id = $1`, d.EventID).Scan(&visibility); err != nil {
				t.Fatalf("read the event of delivery %s: %v", d.ID, err)
			}
			switch visibility {
			case events.VisibilityPrivate:
				if d.Status != events.SubscriptionDeliveryCancelled {
					t.Errorf("the queued PRIVATE event is %s after the demotion, want cancelled: a level that "+
						"dropped must not keep delivering what only a member may see", d.Status)
				}
			case events.VisibilityPublic:
				if d.Status != events.SubscriptionDeliveryPending {
					t.Errorf("the queued PUBLIC event is %s, want it still pending (an email row is born "+
						"pending for the digest sender — the point is that it was NOT withdrawn): the "+
						"subscriber may still see the project, and only what the new level forbids may "+
						"be withdrawn", d.Status)
				}
			}
		}
		// The discriminating control: bob, who was always at the public
		// level, was never sent the private event in the first place.
		for _, d := range f.deliveries(t, f.bob.ID, events.TargetTypeProject, f.publicProject) {
			var visibility string
			if err := f.pool.QueryRow(ctx, `SELECT visibility FROM research_events WHERE id = $1`, d.EventID).Scan(&visibility); err != nil {
				t.Fatalf("read the event of delivery %s: %v", d.ID, err)
			}
			if visibility != events.VisibilityPublic {
				t.Errorf("a public-level follower was sent a %s event (delivery %s): the subscription pipeline "+
					"is what must not route it, not the withdrawal that catches up afterwards", visibility, d.ID)
			}
		}
	})

	t.Run("an account disabled", func(t *testing.T) {
		f := newSubscriptionFixture(t, ctx)
		// bob follows alice (a person). Disabling alice's account must stop
		// the stream: a disabled account is not a public profile.
		f.mustSubscribe(t, f.bob, events.Target{Type: events.TargetTypeUser, ID: f.alice.ID}, nil,
			[]string{events.ChannelWeb, events.ChannelEmail})
		f.publishAndFanOut(t, f.aliceEvent("state.committed", events.VisibilityPublic, `{}`))
		if got := f.statuses(t, f.bob.ID, events.TargetTypeUser, f.alice.ID); len(got) != 2 {
			t.Fatalf("the follower of a live profile held %v, want both channels", got)
		}

		if _, err := f.pool.Exec(ctx, `UPDATE users SET disabled_at = now() WHERE id = $1`, f.alice.ID); err != nil {
			t.Fatalf("disable the account: %v", err)
		}
		f.publishAndFanOut(t, f.aliceEvent("state.committed", events.VisibilityPublic, `{}`))
		got := f.statuses(t, f.bob.ID, events.TargetTypeUser, f.alice.ID)
		want := map[string]int{"web/delivered": 1, "email/cancelled": 1}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("after the account was disabled the follower holds %v, want %v", got, want)
		}
	})

	t.Run("an organization membership that ended", func(t *testing.T) {
		f := newSubscriptionFixture(t, ctx)
		if _, err := f.pool.Exec(ctx, `UPDATE projects SET organization_id = $2 WHERE id = $1`,
			f.publicProject, f.orgID); err != nil {
			t.Fatalf("attach the project to the organization: %v", err)
		}
		target := events.Target{Type: events.TargetTypeOrganization, ID: f.orgID}
		f.mustSubscribe(t, f.alice, target, nil, []string{events.ChannelWeb, events.ChannelEmail})
		f.publishAndFanOut(t, events.Event{
			EventType: "state.committed", ActorID: f.alice.ID, ProjectID: f.publicProject,
			Visibility: events.VisibilityPrivate, Payload: json.RawMessage(`{}`),
		})
		if n := len(f.deliveries(t, f.alice.ID, events.TargetTypeOrganization, f.orgID)); n != 2 {
			t.Fatalf("the active member held %d rows, want 2", n)
		}

		// The affiliation ENDED: the row stays (nothing disappears), the
		// membership stops being active (domain.OrganizationMembership.Active).
		if _, err := f.pool.Exec(ctx,
			`UPDATE organization_memberships SET affiliation_end = current_date - 1
			 WHERE organization_id = $1 AND user_id = $2`, f.orgID, f.alice.ID); err != nil {
			t.Fatalf("end the affiliation: %v", err)
		}
		f.publishAndFanOut(t, events.Event{
			EventType: "state.committed", ActorID: f.alice.ID, ProjectID: f.publicProject,
			Visibility: events.VisibilityPrivate, Payload: json.RawMessage(`{}`),
		})
		got := f.statuses(t, f.alice.ID, events.TargetTypeOrganization, f.orgID)
		want := map[string]int{"web/delivered": 1, "email/cancelled": 1}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("after the affiliation ended the former member holds %v, want %v", got, want)
		}
	})
}

// TestFanOutFailClosedOnUnresolvableAudience is the counterpart of the
// revocation tests: "could not look" must not be mistaken for "no
// relationship". The row's audience query is broken from under the
// fan-out (the relation it reads is renamed away inside a transaction the
// fan-out can see), and the fan-out must leave the event UNMARKED for
// retry rather than treat the error as a revocation and cancel the
// subscriber's in-flight rows.
func TestFanOutFailClosedOnUnresolvableAudience(t *testing.T) {
	ctx := testCtx(t)
	f := newSubscriptionFixture(t, ctx)
	target := events.Target{Type: events.TargetTypeProject, ID: f.sharedPrivate}

	f.mustSubscribe(t, f.alice, target, nil, []string{events.ChannelWeb, events.ChannelEmail})
	f.publishAndFanOut(t, f.aliceEvent("state.committed", events.VisibilityPrivate, `{}`))
	before := f.statuses(t, f.alice.ID, events.TargetTypeProject, f.sharedPrivate)
	if fmt.Sprint(before) != fmt.Sprint(map[string]int{"web/delivered": 1, "email/pending": 1}) {
		t.Fatalf("alice holds %v before the probe, want one delivered and one pending row", before)
	}

	// Break the audience resolution: inside a transaction, rename the
	// relation the audience query joins. The rename is transactional and
	// invisible to the fan-out only if it never commits — so this probe
	// instead drops a column the query reads, in a transaction that stays
	// open, which makes every concurrent read of it fail.
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the probe transaction: %v", err)
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE project_memberships RENAME COLUMN user_id TO probe_user_id`); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("break the audience query: %v", err)
	}
	// The ALTER takes an ACCESS EXCLUSIVE lock, so the fan-out's own read
	// blocks rather than failing — release it and let the fan-out run
	// against the broken schema by committing the rename.
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the probe transaction: %v", err)
	}
	var restoreOnce sync.Once
	restored := func() {
		restoreOnce.Do(func() {
			if _, err := f.pool.Exec(context.Background(),
				`ALTER TABLE project_memberships RENAME COLUMN probe_user_id TO user_id`); err != nil {
				t.Errorf("restore project_memberships.user_id: %v", err)
			}
		})
	}
	defer restored()

	// A new event: the fan-out cannot resolve the audience for it.
	if err := events.Record(ctx, f.pool, f.aliceEvent("state.committed", events.VisibilityPrivate, `{}`)); err != nil {
		t.Fatalf("record event: %v", err)
	}
	if _, err := f.dispatcher.RunOnce(ctx); err != nil {
		t.Fatalf("dispatcher RunOnce: %v", err)
	}
	if _, err := f.fanout.RunOnce(ctx); err != nil {
		t.Fatalf("fanout RunOnce over a broken audience query = %v, want a pass that survives it", err)
	}

	// The pending row is still pending: an error is not a revocation.
	got := f.statuses(t, f.alice.ID, events.TargetTypeProject, f.sharedPrivate)
	if got["email/pending"] != 1 || got["email/cancelled"] != 0 {
		t.Errorf("after an unresolvable audience the subscriber holds %v: a store failure must never be "+
			"treated as \"no relationship\" and cancel a delivery", got)
	}
	// The row was left unmarked, so the work is retried rather than lost.
	var fanned int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM subscription_fanned_events`).Scan(&fanned); err != nil {
		t.Fatalf("count the consumed outbox rows: %v", err)
	}
	if fanned != 1 {
		t.Errorf("the fan-out marked %d outbox rows as consumed, want 1 (the first, successful pass) — "+
			"the failing row must stay unmarked for retry, not be dropped", fanned)
	}

	// With the schema restored the retry succeeds and the row lands: the
	// failure deferred the delivery instead of destroying it.
	restored()
	f.fanOutOnly(t)
	got = f.statuses(t, f.alice.ID, events.TargetTypeProject, f.sharedPrivate)
	if got["email/pending"] != 2 {
		t.Errorf("after the retry the subscriber holds %v, want both events pending on email", got)
	}
}

// --------------------------------------------------------------------------
// 5. The HTTP surface: the same rules at the wire

// TestSubscriptionHTTPAuth composes the real guarded route (authhttp's
// session + CSRF guard over cmd/api/subscriptionshttp) exactly as
// cmd/api/main.go does and asserts the transport half of "public/private
// auth": an anonymous caller writes nothing, a caller may only act on
// their own subscriptions, and a target the caller may not see is refused
// identically to a target that does not exist — no existence oracle.
func TestSubscriptionHTTPAuth(t *testing.T) {
	ctx := testCtx(t)
	var pool *pgxpool.Pool
	pool, _ = testdb.Setup(t, ctx, adminURL(t), subscriptionTaskID)

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
	subsAPI := subscriptionshttp.New(subscriptionshttp.Deps{Store: events.NewSubscriptionStore(pool)})
	mux.Handle("/api/v1/subscriptions", subsAPI.Routes())
	mux.Handle("/api/v1/subscriptions/", subsAPI.Routes())
	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)

	alice, aliceID := signup(t, ts.URL, "sub-http-alice@example.com", "subhttpalice")
	bob, _ := signup(t, ts.URL, "sub-http-bob@example.com", "subhttpbob")

	// alice's private project (she is a member) and carol's private project
	// (she is not).
	aliceProject := mustQueryUUID(t, ctx, pool,
		`INSERT INTO projects (slug, name, purpose, visibility, created_by)
		 VALUES ('sub-http-alice', 'Alice private', 'T1002', 'private', $1) RETURNING id`, aliceID)
	if _, err := pool.Exec(ctx,
		`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, 'owner')`,
		aliceProject, aliceID); err != nil {
		t.Fatalf("seed alice's membership: %v", err)
	}
	foreignProject := mustQueryUUID(t, ctx, pool,
		`INSERT INTO projects (slug, name, purpose, visibility, created_by)
		 VALUES ('sub-http-foreign', 'Someone else private', 'T1002', 'private', $1) RETURNING id`, aliceID)

	body := func(targetType, targetID string, channels ...string) string {
		out, err := json.Marshal(map[string]any{
			"target_type": targetType, "target_id": targetID, "channels": channels,
		})
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		return string(out)
	}

	t.Run("anonymous callers are refused and write nothing", func(t *testing.T) {
		anon := newTestUserClient(ts.URL)
		resp := anon.do(t, http.MethodPost, "/api/v1/subscriptions",
			body(events.TargetTypeProject, aliceProject, events.ChannelWeb))
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("anonymous POST = %d, want 401: %s", resp.StatusCode, readAll(t, resp))
		}
		resp = anon.do(t, http.MethodGet, "/api/v1/subscriptions", "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("anonymous GET = %d, want 401", resp.StatusCode)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM subscriptions`).Scan(&n); err != nil {
			t.Fatalf("count subscriptions: %v", err)
		}
		if n != 0 {
			t.Errorf("the anonymous calls wrote %d subscription rows, want 0", n)
		}
	})

	var subID string
	t.Run("a visible target subscribes and comes back in the owner's list", func(t *testing.T) {
		resp := alice.do(t, http.MethodPost, "/api/v1/subscriptions",
			body(events.TargetTypeProject, aliceProject, events.ChannelWeb, events.ChannelEmail))
		mustStatus(t, resp, http.StatusCreated)
		var payload struct {
			Subscription struct {
				ID           string   `json:"id"`
				TargetType   string   `json:"target_type"`
				TargetID     string   `json:"target_id"`
				EventFilters []string `json:"event_filters"`
				Channels     []string `json:"channels"`
			} `json:"subscription"`
		}
		if err := json.Unmarshal([]byte(readAll(t, resp)), &payload); err != nil {
			t.Fatalf("decode subscription: %v", err)
		}
		subID = payload.Subscription.ID
		if subID == "" || payload.Subscription.TargetID != aliceProject {
			t.Fatalf("created subscription = %+v", payload.Subscription)
		}
		if len(payload.Subscription.Channels) != 2 {
			t.Errorf("channels = %v, want both", payload.Subscription.Channels)
		}
		if payload.Subscription.EventFilters == nil {
			t.Error("event_filters is null, want [] (an empty list means every event type)")
		}

		resp = alice.do(t, http.MethodGet, "/api/v1/subscriptions?target_type=project&target_id="+aliceProject, "")
		mustStatus(t, resp, http.StatusOK)
		var list struct {
			Subscriptions []struct {
				ID string `json:"id"`
			} `json:"subscriptions"`
		}
		if err := json.Unmarshal([]byte(readAll(t, resp)), &list); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		if len(list.Subscriptions) != 1 || list.Subscriptions[0].ID != subID {
			t.Errorf("the watch-state read = %+v, want exactly the new subscription", list.Subscriptions)
		}

		// A second live subscription to the same target is a conflict, not
		// a duplicate row.
		resp = alice.do(t, http.MethodPost, "/api/v1/subscriptions",
			body(events.TargetTypeProject, aliceProject, events.ChannelWeb))
		mustStatus(t, resp, http.StatusConflict)
		mustEnvelope(t, resp, subscriptions.CodeExists)
	})

	t.Run("a target the caller may not see answers exactly what an unknown target answers", func(t *testing.T) {
		resp := alice.do(t, http.MethodPost, "/api/v1/subscriptions",
			body(events.TargetTypeProject, foreignProject, events.ChannelWeb))
		mustStatus(t, resp, http.StatusNotFound)
		refused := readAll(t, resp)

		resp = alice.do(t, http.MethodPost, "/api/v1/subscriptions",
			body(events.TargetTypeProject, "11111111-2222-3333-4444-555555555555", events.ChannelWeb))
		mustStatus(t, resp, http.StatusNotFound)
		unknown := readAll(t, resp)

		if refused != unknown {
			t.Errorf("an unreadable project and an unknown project answered differently:\n%s\n%s\n"+
				"(the difference is an existence oracle for private targets)", refused, unknown)
		}
	})

	t.Run("another account cannot read, edit or end the subscription", func(t *testing.T) {
		for _, tc := range []struct {
			method, path, body string
		}{
			{http.MethodGet, "/api/v1/subscriptions/" + subID, ""},
			{http.MethodPatch, "/api/v1/subscriptions/" + subID, `{"channels":["email"]}`},
			{http.MethodDelete, "/api/v1/subscriptions/" + subID, ""},
		} {
			resp := bob.do(t, tc.method, tc.path, tc.body)
			mustStatus(t, resp, http.StatusNotFound)
			mustEnvelope(t, resp, subscriptions.CodeNotFound)
		}
		resp := bob.do(t, http.MethodGet, "/api/v1/subscriptions", "")
		mustStatus(t, resp, http.StatusOK)
		if raw := readAll(t, resp); !strings.Contains(raw, `"subscriptions":[]`) {
			t.Errorf("bob's list = %s, want an empty list", raw)
		}
		// The owner's row survived all of it.
		resp = alice.do(t, http.MethodGet, "/api/v1/subscriptions/"+subID, "")
		mustStatus(t, resp, http.StatusOK)
	})

	t.Run("malformed input is a 400 with a code", func(t *testing.T) {
		for _, tc := range []struct{ name, body string }{
			{"unknown target type", body("planet", aliceProject, events.ChannelWeb)},
			{"no channels", body(events.TargetTypeProject, aliceProject)},
			{"unknown channel", body(events.TargetTypeProject, aliceProject, "carrier-pigeon")},
			{"malformed JSON", `{"target_type":`},
		} {
			resp := alice.do(t, http.MethodPost, "/api/v1/subscriptions", tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%s = %d, want 400: %s", tc.name, resp.StatusCode, readAll(t, resp))
				continue
			}
			mustEnvelope(t, resp, subscriptions.CodeValidationFailed)
		}
		// A non-UUID path segment is a 400, not a 500 from a query cast.
		resp := alice.do(t, http.MethodGet, "/api/v1/subscriptions/not-a-uuid", "")
		mustStatus(t, resp, http.StatusBadRequest)
	})

	t.Run("the owner's unsubscribe takes effect at the wire", func(t *testing.T) {
		resp := alice.do(t, http.MethodDelete, "/api/v1/subscriptions/"+subID, "")
		mustStatus(t, resp, http.StatusNoContent)
		resp = alice.do(t, http.MethodGet, "/api/v1/subscriptions/"+subID, "")
		mustStatus(t, resp, http.StatusNotFound)
		// And the state is real, not just a read-path filter.
		var deleted *time.Time
		if err := pool.QueryRow(ctx, `SELECT deleted_at FROM subscriptions WHERE id = $1`, subID).Scan(&deleted); err != nil {
			t.Fatalf("read the ended subscription: %v", err)
		}
		if deleted == nil {
			t.Error("the DELETE answered 204 but the row is not stamped deleted_at")
		}
	})
}
