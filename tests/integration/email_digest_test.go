// Task T1005 — required test "email tests".
//
// The unit suites pin the pieces (internal/events: the cadence vocabulary
// and the due rule; internal/application/notifications: the sender's gate
// branches, the template, the dev sink, the configuration). This file pins
// what only the real pipeline over a real PostgreSQL can settle — and it is
// built around this task's acceptance criterion:
//
//	测试邮件不含 unauthorized private content
//
// The shape of the proof is deliberate. "No message contains private
// content" is trivially satisfied by sending nothing, so every case carries
// both halves:
//
//  1. a POSITIVE control — an authorized member's digest DOES name the
//     private target, which is what makes the marker string findable in the
//     sink at all;
//  2. the NEGATIVE assertion — no message addressed to anyone else contains
//     that marker, or the private target's id, anywhere in its bytes;
//  3. the DISCRIMINATING control — the subscriber who kept access still
//     receives, so "stop the leak" cannot be satisfied by stopping
//     everything.
//
// The composition is the production one: events.Dispatcher →
// events.SubscriptionFanOut → notifications.Sender → notifications.DevSink,
// driven exactly as cmd/worker drives them (RunOnce in sequence), over the
// same real tables the API writes.

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/notificationshttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/notifications"
	"github.com/lichman0405/post/internal/application/subscriptions"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

// emailTaskID namespaces this task's test databases (test_T1005_<run_id>).
const emailTaskID = "T1005"

// The markers. Deliberately unique strings so "is this content in a message"
// is a string search over the message's bytes rather than a rendering
// question — and long enough that a substring collision with a uuid or a
// timestamp is impossible.
const (
	markerProjectName = "Zephyrleak Confidential Project"
	markerAssetTitle  = "Zephyrleak Confidential Dataset"
)

// --------------------------------------------------------------------------
// Fixture

// emailFixture is one real database with the whole email pipeline wired over
// it: the outbox dispatcher, the subscription fan-out (T1002), the digest
// sender and the development mail sink (T1005) writing into a temporary
// directory. Plus the rows the cases move around: a private project two of
// the three accounts can see and one cannot, and a public project.
type emailFixture struct {
	ctx        context.Context
	pool       *pgxpool.Pool
	store      *events.NotificationStore
	svc        *notifications.Service
	subSvc     *subscriptions.Service
	dispatcher *events.Dispatcher
	fanout     *events.SubscriptionFanOut
	sender     *notifications.Sender
	sinkDir    string

	alice, bob, carol domain.User

	privateProject string // private; alice and bob are members
	publicProject  string // public; nobody is a member
	privateAsset   string // published asset inside privateProject
	publicAsset    string // published asset inside publicProject
}

// emailFixtureOptions: how the sender is built (the transport is the one
// thing the cases vary — the exhaustion case brings a transport that always
// refuses).
type emailFixtureOptions struct {
	mailer notifications.Mailer
}

func newEmailFixture(t *testing.T, ctx context.Context) *emailFixture {
	t.Helper()
	return newEmailFixtureWith(t, ctx, emailFixtureOptions{})
}

func newEmailFixtureWith(t *testing.T, ctx context.Context, opts emailFixtureOptions) *emailFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), emailTaskID)
	creds := persistence.NewCredentialStore(pool)
	seed := func(email, handle string) domain.User {
		u, err := creds.CreateWithPassword(ctx, email, "hash", handle, strings.ToUpper(handle[:1])+handle[1:])
		if err != nil {
			t.Fatalf("seed %s: %v", handle, err)
		}
		return u
	}
	f := &emailFixture{
		ctx:        ctx,
		pool:       pool,
		store:      events.NewNotificationStore(pool),
		dispatcher: events.NewDispatcher(pool, events.WithLogger(silentLogger())),
		fanout:     events.NewSubscriptionFanOut(pool, events.WithSubscriptionFanOutLogger(silentLogger())),
		alice:      seed("digest-alice@example.com", "alice"),
		bob:        seed("digest-bob@example.com", "bob"),
		carol:      seed("digest-carol@example.com", "carol"),
		sinkDir:    t.TempDir(),
	}
	f.subSvc = subscriptions.NewService(events.NewSubscriptionStore(pool))
	f.svc = notifications.NewService(f.store)

	sink, err := notifications.NewDevSink(f.sinkDir, notifications.WithDevSinkLogger(silentLogger()))
	if err != nil {
		t.Fatalf("dev mail sink: %v", err)
	}
	mailer := opts.mailer
	if mailer == nil {
		mailer = sink
	}
	f.sender = notifications.NewSender(f.store, mailer,
		notifications.WithSenderLogger(silentLogger()),
		notifications.WithSenderBaseURL("http://web.test"))

	f.privateProject = f.seedProject(t, "digest-private", markerProjectName, "private", f.alice.ID, f.bob.ID)
	f.publicProject = f.seedProject(t, "digest-public", "Public Project", "public")
	f.privateAsset = f.seedAsset(t, f.privateProject, "zephyrleak-dataset", "01j9z6k3m4n5p6q7r8s9t0v1b1", markerAssetTitle)
	f.publicAsset = f.seedAsset(t, f.publicProject, "public-dataset", "01j9z6k3m4n5p6q7r8s9t0v1b2", "Public Dataset")
	return f
}

func (f *emailFixture) seedProject(t *testing.T, slug, name, visibility string, members ...string) string {
	t.Helper()
	id := mustQueryUUID(t, f.ctx, f.pool,
		`INSERT INTO projects (slug, name, purpose, visibility, created_by)
		 VALUES ($1, $2, 'T1005 email digest fixture', $3, $4) RETURNING id`,
		slug, name, visibility, f.alice.ID)
	for _, m := range members {
		if _, err := f.pool.Exec(f.ctx,
			`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, 'owner')`,
			id, m); err != nil {
			t.Fatalf("seed membership on %s: %v", slug, err)
		}
	}
	return id
}

func (f *emailFixture) seedAsset(t *testing.T, projectID, slug, pid, title string) string {
	t.Helper()
	mustQueryUUID(t, f.ctx, f.pool,
		`INSERT INTO research_assets (asset_type, slug, title, origin_project_id, pid)
		 VALUES ('dataset', $1, $2, $3, $4) RETURNING id`, slug, title, projectID, pid)
	return pid
}

// setMembership grants or removes one project membership — the app-layer
// permission change (the same state change TestRevokedAccessStopsDelivery
// uses for T1002's acceptance).
func (f *emailFixture) setMembership(t *testing.T, projectID, userID string, member bool) {
	t.Helper()
	if member {
		if _, err := f.pool.Exec(f.ctx,
			`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, 'contributor')
			 ON CONFLICT DO NOTHING`, projectID, userID); err != nil {
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
// audience rule reads (the product refuses the edit through its API by
// design; the column is what the gate reads).
func (f *emailFixture) setVisibility(t *testing.T, projectID, visibility string) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE projects SET visibility = $2 WHERE id = $1`, projectID, visibility); err != nil {
		t.Fatalf("set visibility: %v", err)
	}
}

// mustSubscribe subscribes through the SERVICE (the production subscribe
// path, audience check included).
func (f *emailFixture) mustSubscribe(t *testing.T, actor domain.User, target events.Target, channels ...string) events.Subscription {
	t.Helper()
	sub, err := f.subSvc.Subscribe(f.ctx, actor, target, nil, channels)
	if err != nil {
		t.Fatalf("subscribe %s to %s %s: %v", actor.Handle, target.Type, target.ID, err)
	}
	return sub
}

// setCadence writes the account's cadence through the production service.
func (f *emailFixture) setCadence(t *testing.T, actor domain.User, cadence string) {
	t.Helper()
	if _, err := f.svc.SetCadence(f.ctx, actor, cadence); err != nil {
		t.Fatalf("set cadence %s for %s: %v", cadence, actor.Handle, err)
	}
}

// publishAndFanOut records one event and runs both production passes once:
// the dispatcher publishes the outbox row, the subscription fan-out turns it
// into delivery rows.
func (f *emailFixture) publishAndFanOut(t *testing.T, e events.Event) {
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
}

// runSender runs one digest pass and fails the test on a sender error (the
// cases that expect a failure call RunOnce themselves).
func (f *emailFixture) runSender(t *testing.T) int {
	t.Helper()
	sent, err := f.sender.RunOnce(f.ctx)
	if err != nil {
		t.Fatalf("sender RunOnce: %v", err)
	}
	return sent
}

// projectEvent is the event every case publishes: alice acting inside a
// project.
func (f *emailFixture) projectEvent(projectID, visibility string) events.Event {
	return events.Event{
		EventType:  "state.committed",
		ActorID:    f.alice.ID,
		ProjectID:  projectID,
		Visibility: visibility,
		Payload:    json.RawMessage(`{}`),
	}
}

// --------------------------------------------------------------------------
// Reading the sink

// sentMessage is one file the dev mail sink wrote, as the test reads it:
// the raw bytes, the recipient header, and the decoded text body. The raw
// bytes are what the leak assertions search — a body that never reached the
// text part is still bytes on disk.
type sentMessage struct {
	path string
	to   string
	raw  string
}

// messages returns every message in the sink, by file name (the sink names
// files by send time, so the order is the send order).
func (f *emailFixture) messages(t *testing.T) []sentMessage {
	t.Helper()
	entries, err := os.ReadDir(f.sinkDir)
	if err != nil {
		t.Fatalf("read the sink directory: %v", err)
	}
	var out []sentMessage
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(f.sinkDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		out = append(out, sentMessage{path: e.Name(), to: recipientOf(string(raw)), raw: string(raw)})
	}
	return out
}

// messagesTo narrows the sink to the messages addressed to one account.
func (f *emailFixture) messagesTo(t *testing.T, addr string) []sentMessage {
	t.Helper()
	var out []sentMessage
	for _, m := range f.messages(t) {
		if m.to == addr {
			out = append(out, m)
		}
	}
	return out
}

func recipientOf(raw string) string {
	for _, line := range strings.Split(raw, "\r\n") {
		if rest, ok := strings.CutPrefix(line, "To: "); ok {
			return rest
		}
	}
	return ""
}

// --------------------------------------------------------------------------
// Reading the rows

// deliveryRow is one subscription_deliveries row, read with raw SQL rather
// than through the store so the assertions do not depend on the code under
// test's own reader.
type deliveryRow struct {
	id          string
	channel     string
	status      string
	attempts    int
	deliveredAt *time.Time
	cancelledAt *time.Time
}

func (f *emailFixture) rows(t *testing.T, userID, targetType, targetID string) []deliveryRow {
	t.Helper()
	rs, err := f.pool.Query(f.ctx, `
		SELECT id, channel, status, attempts, delivered_at, cancelled_at
		FROM subscription_deliveries
		WHERE user_id = $1 AND target_type = $2 AND target_id = $3
		ORDER BY created_at, id`, userID, targetType, targetID)
	if err != nil {
		t.Fatalf("query deliveries: %v", err)
	}
	defer rs.Close()
	var out []deliveryRow
	for rs.Next() {
		var d deliveryRow
		if err := rs.Scan(&d.id, &d.channel, &d.status, &d.attempts, &d.deliveredAt, &d.cancelledAt); err != nil {
			t.Fatalf("scan delivery: %v", err)
		}
		out = append(out, d)
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("iterate deliveries: %v", err)
	}
	return out
}

func (f *emailFixture) emailStatuses(t *testing.T, userID, targetType, targetID string) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, r := range f.rows(t, userID, targetType, targetID) {
		if r.channel == events.ChannelEmail {
			out[r.status]++
		}
	}
	return out
}

// --------------------------------------------------------------------------
// 1. The digest, end to end (positive control)

// TestEmailDigestEndToEnd walks the whole pipeline once with everything
// allowed: subscribe → event → fan-out → digest → file on disk. It is the
// case that proves the marker strings ARE findable in the sink, which is
// what makes every "contains nothing" assertion below mean something.
func TestEmailDigestEndToEnd(t *testing.T) {
	ctx := testCtx(t)
	f := newEmailFixture(t, ctx)

	target := events.Target{Type: events.TargetTypeProject, ID: f.privateProject}
	f.mustSubscribe(t, f.alice, target, events.ChannelEmail)
	f.mustSubscribe(t, f.bob, target, events.ChannelEmail)
	// Both accounts asked for immediate: this case is about the message, not
	// about the window (the cadence cases come later).
	f.setCadence(t, f.alice, events.CadenceImmediate)
	f.setCadence(t, f.bob, events.CadenceImmediate)

	f.publishAndFanOut(t, f.projectEvent(f.privateProject, events.VisibilityPrivate))
	if got := f.emailStatuses(t, f.alice.ID, events.TargetTypeProject, f.privateProject); got["pending"] != 1 {
		t.Fatalf("alice's email rows = %v, want one pending (the fan-out's job)", got)
	}

	if sent := f.runSender(t); sent != 2 {
		t.Errorf("sender moved %d deliveries, want 2", sent)
	}

	msgs := f.messages(t)
	if len(msgs) != 2 {
		t.Fatalf("the sink holds %d messages, want one per subscriber: %v", len(msgs), describeMessages(msgs))
	}
	for _, m := range msgs {
		// The positive control: the authorized content IS in the message.
		if !strings.Contains(m.raw, markerProjectName) {
			t.Errorf("the message to %s does not name the private project it is authorized to report:\n%s", m.to, m.raw)
		}
		if !strings.Contains(m.raw, f.privateProject) {
			t.Errorf("the message to %s carries no link to the target:\n%s", m.to, m.raw)
		}
		if !strings.HasPrefix(m.raw, "From: ") || !strings.Contains(m.raw, "MIME-Version: 1.0") {
			t.Errorf("the message to %s is not a mail message:\n%s", m.to, m.raw)
		}
	}
	for _, addr := range []string{f.alice.Email, f.bob.Email} {
		if len(f.messagesTo(t, addr)) != 1 {
			t.Errorf("the sink holds %d messages for %s, want 1", len(f.messagesTo(t, addr)), addr)
		}
	}

	// And the rows are resolved: nothing is left pending after a successful
	// send, or the next pass would mail it again.
	for _, u := range []domain.User{f.alice, f.bob} {
		got := f.emailStatuses(t, u.ID, events.TargetTypeProject, f.privateProject)
		if got["delivered"] != 1 || got["pending"] != 0 {
			t.Errorf("%s's email rows = %v, want one delivered and none pending", u.Handle, got)
		}
	}
}

func describeMessages(msgs []sentMessage) string {
	var b strings.Builder
	for _, m := range msgs {
		fmt.Fprintf(&b, "%s -> %s; ", m.path, m.to)
	}
	return b.String()
}

// --------------------------------------------------------------------------
// 2. The acceptance criterion: no unauthorized private content

// TestEmailDigestCarriesNoUnauthorizedPrivateContent is this task's
// acceptance test (测试邮件不含 unauthorized private content).
//
// The two cases are the two ways access can disappear between the moment a
// delivery is queued and the moment it would leave the building:
//
//   - a project membership is removed (the subscriber was at the member
//     level);
//   - a public project is turned private (the subscriber was at the public
//     level).
//
// In both, the delivery row already exists — the fan-out queued it while the
// subscriber still had access — and NO new event is published afterwards, so
// the fan-out (which withdraws in-flight rows only when a new event for the
// target arrives, T1002) cannot be what stops it. The only thing standing
// between the queued copy and the subscriber is the digest sender's
// send-time gate, which is what this test is about.
func TestEmailDigestCarriesNoUnauthorizedPrivateContent(t *testing.T) {
	ctx := testCtx(t)

	// revokedCase is one revocation: the fixture with the row already
	// queued and the access already gone, plus the strings that must not
	// reach anyone but the subscriber who kept access.
	type revokedCase struct {
		f          *emailFixture
		revoked    domain.User
		authorized domain.User
		targetID   string
		// markers are the private strings a message may only carry when it
		// is addressed to someone who may see the target.
		markers []string
		// authorizedName is the marker the KEPT subscriber's message must
		// still carry (the positive control).
		authorizedName string
	}

	cases := []struct {
		name  string
		setup func(t *testing.T) revokedCase
	}{
		{
			name: "a membership removed after the delivery was queued",
			setup: func(t *testing.T) revokedCase {
				f := newEmailFixture(t, ctx)
				target := events.Target{Type: events.TargetTypeProject, ID: f.privateProject}
				f.mustSubscribe(t, f.bob, target, events.ChannelEmail)
				f.mustSubscribe(t, f.alice, target, events.ChannelEmail)
				f.publishAndFanOut(t, f.projectEvent(f.privateProject, events.VisibilityPrivate))
				if got := f.emailStatuses(t, f.bob.ID, events.TargetTypeProject, f.privateProject); got["pending"] != 1 {
					t.Fatalf("bob's queued delivery = %v, want one pending row to be withdrawn", got)
				}
				// The revocation. No event follows it: the only thing that
				// can still stop bob's queued email is the send-time gate.
				f.setMembership(t, f.privateProject, f.bob.ID, false)
				return revokedCase{
					f: f, revoked: f.bob, authorized: f.alice, targetID: f.privateProject,
					// The private project's name and id, and the title and
					// pid of the asset inside it: everything a digest about
					// this target could carry.
					markers:        []string{markerProjectName, f.privateProject, markerAssetTitle, f.privateAsset},
					authorizedName: markerProjectName,
				}
			},
		},
		{
			name: "a public project turned private after the delivery was queued",
			setup: func(t *testing.T) revokedCase {
				f := newEmailFixture(t, ctx)
				target := events.Target{Type: events.TargetTypeProject, ID: f.publicProject}
				// carol follows at the PUBLIC level (she is a member of the
				// private project, not this one); alice is a member here.
				f.setMembership(t, f.publicProject, f.alice.ID, true)
				f.mustSubscribe(t, f.carol, target, events.ChannelEmail)
				f.mustSubscribe(t, f.alice, target, events.ChannelEmail)
				f.publishAndFanOut(t, f.projectEvent(f.publicProject, events.VisibilityPublic))
				if got := f.emailStatuses(t, f.carol.ID, events.TargetTypeProject, f.publicProject); got["pending"] != 1 {
					t.Fatalf("carol's queued delivery = %v, want one pending row", got)
				}
				f.setVisibility(t, f.publicProject, "private")
				return revokedCase{
					f: f, revoked: f.carol, authorized: f.alice, targetID: f.publicProject,
					// The project is private now: its name and id are the
					// private content a message to the follower would leak.
					markers:        []string{"Public Project", f.publicProject},
					authorizedName: "Public Project",
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := tc.setup(t)
			f, revoked, authorized, targetID := cs.f, cs.revoked, cs.authorized, cs.targetID

			f.runSender(t)

			// (a) The revoked subscriber received NOTHING — no message is
			// addressed to them at all.
			if msgs := f.messagesTo(t, revoked.Email); len(msgs) != 0 {
				t.Errorf("%s received %d message(s) after losing access:\n%s",
					revoked.Handle, len(msgs), msgs[0].raw)
			}

			// (b) THE ACCEPTANCE ASSERTION: no message went to anyone who
			// may not see it. Every file the sink wrote is inspected, and
			// the private markers (the project's name, the asset's title,
			// the target's id, the asset's pid) may appear only in a message
			// addressed to a subscriber who still has access.
			authorizedAddr := authorized.Email
			for _, m := range f.messages(t) {
				for _, marker := range cs.markers {
					if !strings.Contains(m.raw, marker) {
						continue
					}
					if m.to != authorizedAddr {
						t.Errorf("a message addressed to %s contains %q — unauthorized private content left "+
							"the building:\n%s", m.to, marker, m.raw)
					}
				}
			}

			// (c) The queue reflects what happened: the queued row was
			// WITHDRAWN (with a timestamp), not left pending for a later
			// pass to send.
			got := f.emailStatuses(t, revoked.ID, events.TargetTypeProject, targetID)
			if got["cancelled"] != 1 || got["pending"] != 0 || got["delivered"] != 0 {
				t.Errorf("%s's email rows = %v, want exactly one cancelled", revoked.Handle, got)
			}
			for _, r := range f.rows(t, revoked.ID, events.TargetTypeProject, targetID) {
				if r.channel == events.ChannelEmail && (r.cancelledAt == nil || r.deliveredAt != nil) {
					t.Errorf("the withdrawn row is %s (cancelled_at %v, delivered_at %v), want cancelled with a timestamp",
						r.status, r.cancelledAt, r.deliveredAt)
				}
			}

			// (d) The discriminating control: the subscriber who KEPT access
			// still receives their digest, and it still names the target.
			// Without this, "stopped the leak" would be satisfied by a
			// pipeline that stopped everything.
			kept := f.messagesTo(t, authorizedAddr)
			if len(kept) != 1 {
				t.Fatalf("the subscriber who kept access received %d messages, want 1", len(kept))
			}
			if !strings.Contains(kept[0].raw, cs.authorizedName) {
				t.Errorf("the authorized message no longer names the target:\n%s", kept[0].raw)
			}
			if got := f.emailStatuses(t, authorized.ID, events.TargetTypeProject, targetID); got["delivered"] != 1 {
				t.Errorf("%s's email rows = %v, want one delivered", authorized.Handle, got)
			}

			// (e) And the queued row was not silently dropped either: it is
			// still there, cancelled, one row in and one row out.
			if n := len(f.rows(t, revoked.ID, events.TargetTypeProject, targetID)); n != 1 {
				t.Errorf("%s holds %d rows on the target, want the one queued row (nothing disappears)", revoked.Handle, n)
			}
		})
	}
}

// TestEmailDigestNeverSendsAnUnreadableTarget is the second half of the
// same rule: when access is revoked the message must not simply lose the
// label and go out as a bare event line. The whole item — event type
// included, which is itself information about the target — is gone.
func TestEmailDigestNeverSendsAnUnreadableTarget(t *testing.T) {
	ctx := testCtx(t)
	f := newEmailFixture(t, ctx)
	target := events.Target{Type: events.TargetTypeProject, ID: f.privateProject}

	f.mustSubscribe(t, f.bob, target, events.ChannelEmail)
	f.publishAndFanOut(t, f.projectEvent(f.privateProject, events.VisibilityPrivate))
	f.setMembership(t, f.privateProject, f.bob.ID, false)
	f.runSender(t)

	if msgs := f.messages(t); len(msgs) != 0 {
		t.Fatalf("the sink holds %d message(s) although the only subscriber lost access:\n%s",
			len(msgs), msgs[0].raw)
	}
	for _, leak := range []string{"state.committed", f.privateProject, markerProjectName, "digest-bob@example.com"} {
		for _, m := range f.messages(t) {
			if strings.Contains(m.raw, leak) {
				t.Errorf("message %s contains %q", m.path, leak)
			}
		}
	}
}

// --------------------------------------------------------------------------
// 3. The cadence preference (immediate / daily / weekly)

// TestEmailDigestCadence pins the three preferences against the real
// schedule: what the account asks for is what the sender does. The windows
// are moved by back-dating last_digest_at, which is the row the schedule is
// measured from — not by waiting.
func TestEmailDigestCadence(t *testing.T) {
	ctx := testCtx(t)
	f := newEmailFixture(t, ctx)
	target := events.Target{Type: events.TargetTypeProject, ID: f.publicProject}
	f.mustSubscribe(t, f.alice, target, events.ChannelEmail)

	// The default: an account that never touched the setting is on daily.
	// Nothing about the account is configured here on purpose.
	if prefs, err := f.svc.Preferences(ctx, f.alice); err != nil {
		t.Fatalf("read the default preferences: %v", err)
	} else if prefs.Stored || prefs.EffectiveCadence() != events.DefaultCadence {
		t.Fatalf("default preferences = %+v, want the unstored daily default", prefs)
	}

	// The first digest is never made to wait for the window.
	f.publishAndFanOut(t, f.projectEvent(f.publicProject, events.VisibilityPublic))
	if sent := f.runSender(t); sent != 1 {
		t.Fatalf("the first digest moved %d deliveries, want 1 (a new subscriber must not wait a day)", sent)
	}
	if got := len(f.messages(t)); got != 1 {
		t.Fatalf("messages after the first digest = %d, want 1", got)
	}

	// Daily: a second event an hour later (its row queued now) stays queued
	// — one message a day, which is the point of the default.
	f.publishAndFanOut(t, f.projectEvent(f.publicProject, events.VisibilityPublic))
	if sent := f.runSender(t); sent != 0 {
		t.Errorf("a second digest went out %d deliveries inside the daily window, want 0", sent)
	}
	if got := len(f.messages(t)); got != 1 {
		t.Errorf("messages inside the daily window = %d, want still 1", got)
	}
	if got := f.emailStatuses(t, f.alice.ID, events.TargetTypeProject, f.publicProject); got["pending"] != 1 {
		t.Errorf("alice's email rows = %v, want the second one still pending", got)
	}

	// Past the window, the queued digest goes out (and carries the event
	// that was waiting).
	f.backdateAnchor(t, f.alice, 25*time.Hour)
	if sent := f.runSender(t); sent != 1 {
		t.Errorf("after the window the sender moved %d deliveries, want 1", sent)
	}
	if got := len(f.messages(t)); got != 2 {
		t.Fatalf("messages after the window = %d, want 2", got)
	}
	if last := f.messages(t); len(last) > 0 && !strings.Contains(last[len(last)-1].raw, "POST digest: 1 update") {
		t.Errorf("the queued digest did not carry its event:\n%s", last[len(last)-1].raw)
	}

	// Weekly: same anchor, a much longer wait.
	f.setCadence(t, f.alice, events.CadenceWeekly)
	f.publishAndFanOut(t, f.projectEvent(f.publicProject, events.VisibilityPublic))
	f.backdateAnchor(t, f.alice, 6*24*time.Hour)
	if sent := f.runSender(t); sent != 0 {
		t.Errorf("a weekly subscriber received a digest after 6 days, want 0")
	}
	f.backdateAnchor(t, f.alice, 8*24*time.Hour)
	if sent := f.runSender(t); sent != 1 {
		t.Errorf("a weekly subscriber received nothing after 8 days, want 1")
	}

	// Immediate: every event, on the next pass — no window at all.
	f.setCadence(t, f.alice, events.CadenceImmediate)
	f.publishAndFanOut(t, f.projectEvent(f.publicProject, events.VisibilityPublic))
	if sent := f.runSender(t); sent != 1 {
		t.Errorf("the immediate subscriber's digest moved %d deliveries, want 1", sent)
	}
	f.publishAndFanOut(t, f.projectEvent(f.publicProject, events.VisibilityPublic))
	if sent := f.runSender(t); sent != 1 {
		t.Errorf("the immediate subscriber's second digest moved %d deliveries, want 1 — immediate means "+
			"no window, not one per day", sent)
	}
	// Five messages in total: the first digest, the one behind the daily
	// window, the one behind the weekly window, and the immediate
	// subscriber's two — one per event.
	if got := len(f.messages(t)); got != 5 {
		t.Errorf("messages = %d, want 5 (one per digest sent, and no message for any suppressed pass)", got)
	}
}

// TestEmailDigestEventsOfOnePassArriveAsOneMessage: the digest is a digest.
// Two events fanned out in the same pass reach the subscriber as one
// message with two items, in the order the events happened.
func TestEmailDigestEventsOfOnePassArriveAsOneMessage(t *testing.T) {
	ctx := testCtx(t)
	f := newEmailFixture(t, ctx)
	target := events.Target{Type: events.TargetTypeProject, ID: f.publicProject}
	f.mustSubscribe(t, f.alice, target, events.ChannelEmail)
	f.setCadence(t, f.alice, events.CadenceImmediate)

	f.publishAndFanOut(t, f.projectEvent(f.publicProject, events.VisibilityPublic))
	f.publishAndFanOut(t, f.projectEvent(f.publicProject, events.VisibilityPublic))
	if sent := f.runSender(t); sent != 2 {
		t.Errorf("the digest moved %d deliveries, want both", sent)
	}
	msgs := f.messages(t)
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want one carrying both events", len(msgs))
	}
	if !strings.Contains(msgs[0].raw, "POST digest: 2 updates") {
		t.Errorf("the message does not say how many updates it carries:\n%s", msgs[0].raw)
	}
	// Counted in the text part alone: the same items appear again in the
	// HTML part, and counting both would read one digest as two.
	textPart, _, _ := strings.Cut(msgs[0].raw, "Content-Type: text/html")
	if n := strings.Count(textPart, "\n- 20"); n != 2 {
		t.Errorf("the message carries %d items in its text part, want 2:\n%s", n, msgs[0].raw)
	}
	if got := f.emailStatuses(t, f.alice.ID, events.TargetTypeProject, f.publicProject); got["delivered"] != 2 {
		t.Errorf("email rows = %v, want both delivered", got)
	}
}

// backdateAnchor moves the account's digest anchor back by d — the way a
// test reaches the far side of a 24h or 7d window without waiting for it.
// It writes last_digest_at directly because that is the column the schedule
// reads (the pipeline's own writer only ever stamps "now").
func (f *emailFixture) backdateAnchor(t *testing.T, actor domain.User, d time.Duration) {
	t.Helper()
	// The row is created by the first digest; an account that has never had
	// one is due anyway, so a missing row is a legitimate no-op.
	tag, err := f.pool.Exec(f.ctx,
		`UPDATE notification_preferences SET last_digest_at = now() - $2::interval WHERE user_id = $1`,
		actor.ID, d)
	if err != nil {
		t.Fatalf("back-date the digest anchor: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("back-dated %d preference rows for %s, want 1 (the digest should have created it)",
			tag.RowsAffected(), actor.Handle)
	}
}

// TestCadenceChangeKeepsTheAnchor: changing the frequency must not disturb
// the schedule. A settings write that cleared last_digest_at would let a
// user pull a digest forward by opening the settings page — and it would do
// it silently.
func TestCadenceChangeKeepsTheAnchor(t *testing.T) {
	ctx := testCtx(t)
	f := newEmailFixture(t, ctx)
	target := events.Target{Type: events.TargetTypeProject, ID: f.publicProject}
	f.mustSubscribe(t, f.alice, target, events.ChannelEmail)
	f.publishAndFanOut(t, f.projectEvent(f.publicProject, events.VisibilityPublic))
	f.runSender(t)

	before := f.anchor(t, f.alice)
	if before == nil {
		t.Fatal("the first digest did not record a send time")
	}
	f.setCadence(t, f.alice, events.CadenceWeekly)
	after := f.anchor(t, f.alice)
	if after == nil {
		t.Fatal("changing the cadence cleared last_digest_at")
	}
	if !after.Equal(*before) {
		t.Errorf("last_digest_at moved from %v to %v when the cadence changed", before, after)
	}
}

// anchor reads the account's digest anchor.
func (f *emailFixture) anchor(t *testing.T, actor domain.User) *time.Time {
	t.Helper()
	var at *time.Time
	if err := f.pool.QueryRow(f.ctx,
		`SELECT last_digest_at FROM notification_preferences WHERE user_id = $1`, actor.ID).Scan(&at); err != nil {
		t.Fatalf("read the anchor: %v", err)
	}
	return at
}

// --------------------------------------------------------------------------
// 4. The retry policy: leases and the exhausted-attempt withdrawal

// refusingMailer is a transport that always fails — the development sink's
// stand-in for "the mail relay is down".
type refusingMailer struct{ calls int }

func (m *refusingMailer) Send(context.Context, notifications.Mail) error {
	m.calls++
	return fmt.Errorf("transport refused")
}

// TestEmailDigestRetriesThenWithdraws pins the retry policy end to end: a
// delivery whose transport keeps failing is retried under a lease (not
// hot-looped, not repeated inside it) and is finally WITHDRAWN with a
// timestamp, so it stops occupying its subscriber's queue.
func TestEmailDigestRetriesThenWithdraws(t *testing.T) {
	ctx := testCtx(t)
	mailer := &refusingMailer{}
	f := newEmailFixtureWith(t, ctx, emailFixtureOptions{mailer: mailer})
	target := events.Target{Type: events.TargetTypeProject, ID: f.publicProject}
	f.mustSubscribe(t, f.alice, target, events.ChannelEmail)
	f.publishAndFanOut(t, f.projectEvent(f.publicProject, events.VisibilityPublic))

	// Attempt 1: the transport refuses, the row stays pending, nothing is
	// marked delivered.
	if _, err := f.sender.RunOnce(ctx); err == nil {
		t.Error("RunOnce = nil error although the transport refused, want the failure surfaced")
	}
	if got := f.emailStatuses(t, f.alice.ID, events.TargetTypeProject, f.publicProject); got["pending"] != 1 {
		t.Fatalf("after a refused send alice's email rows = %v, want still pending", got)
	}
	attempts := func() int {
		rows := f.rows(t, f.alice.ID, events.TargetTypeProject, f.publicProject)
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rows))
		}
		return rows[0].attempts
	}
	if got := attempts(); got != 1 {
		t.Errorf("attempts = %d after one pass, want 1", got)
	}

	// Inside the lease: a second pass finds nothing to claim. Without the
	// lease two senders (or two passes) would mail the same row.
	if sent := f.runSender(t); sent != 0 {
		t.Errorf("a second pass inside the lease moved %d deliveries, want 0", sent)
	}
	if got := attempts(); got != 1 {
		t.Errorf("attempts = %d inside the lease, want still 1: the claim is a lease, not a free-for-all", got)
	}

	// Expire the lease and retry up to the policy's limit.
	for i := 0; i < events.MaxEmailDeliveryAttempts; i++ {
		f.expireLease(t)
		_, _ = f.sender.RunOnce(ctx)
		if s := f.emailStatuses(t, f.alice.ID, events.TargetTypeProject, f.publicProject); s["pending"] == 0 {
			break
		}
	}
	rows := f.rows(t, f.alice.ID, events.TargetTypeProject, f.publicProject)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].attempts != events.MaxEmailDeliveryAttempts {
		t.Errorf("attempts = %d, want the policy's limit %d: the retry loop must stop here",
			rows[0].attempts, events.MaxEmailDeliveryAttempts)
	}
	// One more pass: the row is withdrawn rather than retried forever, and
	// it is withdrawn by the sweep before the claim (so it never blocks the
	// deliveries queued behind it).
	f.expireLease(t)
	if _, err := f.sender.RunOnce(ctx); err != nil {
		t.Errorf("the withdrawal pass reported an error: %v", err)
	}
	rows = f.rows(t, f.alice.ID, events.TargetTypeProject, f.publicProject)
	if rows[0].status != events.SubscriptionDeliveryCancelled || rows[0].cancelledAt == nil {
		t.Errorf("an undeliverable row is %s (cancelled_at %v), want cancelled with a timestamp so the "+
			"row says what became of it", rows[0].status, rows[0].cancelledAt)
	}
	if rows[0].deliveredAt != nil {
		t.Error("a delivery the transport refused is stamped delivered_at")
	}
	// Nothing was ever written: the transport refused every attempt.
	if msgs := f.messages(t); len(msgs) != 0 {
		t.Errorf("the sink holds %d message(s) although every send failed", len(msgs))
	}
}

// expireLease makes the claimed rows claimable again by another pass — the
// clock the backoff would otherwise spend minutes waiting for.
func (f *emailFixture) expireLease(t *testing.T) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE subscription_deliveries SET leased_until = now() - interval '1 second'
		 WHERE channel = 'email' AND status = 'pending'`); err != nil {
		t.Fatalf("expire the lease: %v", err)
	}
}

// TestEmailDigestWithdrawsAccountsWithoutAnAddress: an account with no
// address cannot be reached by any transport, so its delivery leaves the
// queue rather than consuming the retry budget.
func TestEmailDigestWithdrawsAccountsWithoutAnAddress(t *testing.T) {
	ctx := testCtx(t)
	f := newEmailFixture(t, ctx)
	target := events.Target{Type: events.TargetTypeProject, ID: f.publicProject}
	f.mustSubscribe(t, f.alice, target, events.ChannelEmail)
	f.publishAndFanOut(t, f.projectEvent(f.publicProject, events.VisibilityPublic))
	if _, err := f.pool.Exec(ctx, `UPDATE users SET email = '' WHERE id = $1`, f.alice.ID); err != nil {
		t.Fatalf("clear the address: %v", err)
	}

	if sent := f.runSender(t); sent != 0 {
		t.Errorf("sent = %d for an account with no address, want 0", sent)
	}
	if msgs := f.messages(t); len(msgs) != 0 {
		t.Errorf("the sink holds %d message(s) for an account with no address", len(msgs))
	}
	rows := f.rows(t, f.alice.ID, events.TargetTypeProject, f.publicProject)
	if len(rows) != 1 || rows[0].status != events.SubscriptionDeliveryCancelled {
		t.Errorf("rows = %+v, want one cancelled", rows)
	}
}

// --------------------------------------------------------------------------
// 5. The settings surface at the wire

// TestNotificationPreferencesHTTPAuth drives the real guarded route the way
// cmd/api/main.go composes it: session + CSRF in front, the service behind,
// the real PostgreSQL under both. The cadence a client writes here is the
// one the sender acts on — which the last block proves by running the
// pipeline.
func TestNotificationPreferencesHTTPAuth(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), emailTaskID)

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
	notifAPI := notificationshttp.New(notificationshttp.Deps{Store: events.NewNotificationStore(pool)})
	mux.Handle("/api/v1/notifications/preferences", notifAPI.Routes())
	mux.Handle("/api/v1/notifications/", notifAPI.Routes())
	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)

	alice, aliceID := signup(t, ts.URL, "digest-http-alice@example.com", "digesthttpalice")
	bob, _ := signup(t, ts.URL, "digest-http-bob@example.com", "digesthttpbob")

	// The wire shape, as the client sees it (cmd/api/notificationshttp's
	// preference payload).
	type preferences struct {
		Cadence      string     `json:"cadence"`
		Options      []string   `json:"options"`
		LastDigestAt *time.Time `json:"last_digest_at"`
		Stored       bool       `json:"stored"`
		UpdatedAt    time.Time  `json:"updated_at"`
	}
	decode := func(t *testing.T, resp *http.Response) preferences {
		t.Helper()
		var payload struct {
			Preferences preferences `json:"preferences"`
		}
		if err := json.Unmarshal([]byte(readAll(t, resp)), &payload); err != nil {
			t.Fatalf("decode preferences: %v", err)
		}
		return payload.Preferences
	}

	t.Run("anonymous callers are refused", func(t *testing.T) {
		anon := newTestUserClient(ts.URL)
		resp := anon.do(t, http.MethodGet, "/api/v1/notifications/preferences", "")
		mustStatus(t, resp, http.StatusUnauthorized)
		resp = anon.do(t, http.MethodPut, "/api/v1/notifications/preferences", `{"cadence":"weekly"}`)
		mustStatus(t, resp, http.StatusUnauthorized)
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM notification_preferences`).Scan(&n); err != nil {
			t.Fatalf("count preferences: %v", err)
		}
		if n != 0 {
			t.Errorf("the anonymous calls wrote %d preference rows, want 0", n)
		}
	})

	t.Run("an account that never set a cadence reads the default", func(t *testing.T) {
		resp := alice.do(t, http.MethodGet, "/api/v1/notifications/preferences", "")
		mustStatus(t, resp, http.StatusOK)
		prefs := decode(t, resp)
		if prefs.Cadence != events.DefaultCadence {
			t.Errorf("cadence = %q, want the default %q", prefs.Cadence, events.DefaultCadence)
		}
		if prefs.Stored {
			t.Error("stored = true for an account with no row: the client cannot tell a default from a choice")
		}
		if len(prefs.Options) != 3 {
			t.Errorf("options = %v, want the three cadences", prefs.Options)
		}
		if prefs.LastDigestAt != nil {
			t.Errorf("last_digest_at = %v, want null before the first digest", prefs.LastDigestAt)
		}
	})

	t.Run("the owner can set each cadence and read it back", func(t *testing.T) {
		for _, cadence := range []string{events.CadenceWeekly, events.CadenceImmediate, events.CadenceDaily} {
			resp := alice.do(t, http.MethodPut, "/api/v1/notifications/preferences",
				fmt.Sprintf(`{"cadence":%q}`, cadence))
			mustStatus(t, resp, http.StatusOK)
			if got := decode(t, resp).Cadence; got != cadence {
				t.Errorf("PUT %s returned cadence %q", cadence, got)
			}
			resp = alice.do(t, http.MethodGet, "/api/v1/notifications/preferences", "")
			mustStatus(t, resp, http.StatusOK)
			prefs := decode(t, resp)
			if prefs.Cadence != cadence || !prefs.Stored {
				t.Errorf("after PUT %s the read is %+v", cadence, prefs)
			}
		}
	})

	t.Run("malformed input is a 400 with a code", func(t *testing.T) {
		for _, body := range []string{
			`{"cadence":"hourly"}`,
			`{"cadence":""}`,
			`{}`,
			`{"cadence":`,
		} {
			resp := alice.do(t, http.MethodPut, "/api/v1/notifications/preferences", body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("PUT %s = %d, want 400: %s", body, resp.StatusCode, readAll(t, resp))
				continue
			}
			mustEnvelope(t, resp, notifications.CodeValidationFailed)
		}
		// The refused writes changed nothing.
		resp := alice.do(t, http.MethodGet, "/api/v1/notifications/preferences", "")
		mustStatus(t, resp, http.StatusOK)
		if got := decode(t, resp).Cadence; got != events.CadenceDaily {
			t.Errorf("cadence = %q after the refused writes, want the last accepted value", got)
		}
	})

	t.Run("one account cannot read or write another's setting", func(t *testing.T) {
		// The routes take no user id at all, so bob's only possible target is
		// himself: whatever he does here leaves alice's row alone.
		resp := bob.do(t, http.MethodPut, "/api/v1/notifications/preferences", `{"cadence":"weekly"}`)
		mustStatus(t, resp, http.StatusOK)
		var cadence string
		if err := pool.QueryRow(ctx,
			`SELECT cadence FROM notification_preferences WHERE user_id = $1`, aliceID).Scan(&cadence); err != nil {
			t.Fatalf("read alice's cadence: %v", err)
		}
		if cadence != events.CadenceDaily {
			t.Errorf("alice's cadence = %q after bob's write, want her own", cadence)
		}
	})

	t.Run("the cadence written at the wire is the one the sender obeys", func(t *testing.T) {
		// alice is now on daily (above). This is the integration that makes
		// the setting more than a stored string: the same row the API wrote
		// is the one the claim filters on.
		projectID := mustQueryUUID(t, ctx, pool,
			`INSERT INTO projects (slug, name, purpose, visibility, created_by)
			 VALUES ('digest-http-project', 'HTTP Preferences Project', 'T1005', 'public', $1) RETURNING id`, aliceID)
		target := events.Target{Type: events.TargetTypeProject, ID: projectID}
		subSvc := subscriptions.NewService(events.NewSubscriptionStore(pool))
		aliceUser := domain.User{ID: aliceID, Handle: "digesthttpalice", Email: "digest-http-alice@example.com"}
		if _, err := subSvc.Subscribe(ctx, aliceUser, target, nil, []string{events.ChannelEmail}); err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		store := events.NewNotificationStore(pool)
		sink, err := notifications.NewDevSink(t.TempDir(), notifications.WithDevSinkLogger(silentLogger()))
		if err != nil {
			t.Fatalf("dev mail sink: %v", err)
		}
		sender := notifications.NewSender(store, sink, notifications.WithSenderLogger(silentLogger()))

		publish := func() {
			if err := events.Record(ctx, pool, events.Event{
				EventType: "state.committed", ActorID: aliceID, ProjectID: projectID,
				Visibility: events.VisibilityPublic, Payload: json.RawMessage(`{}`),
			}); err != nil {
				t.Fatalf("record event: %v", err)
			}
			if _, err := events.NewDispatcher(pool, events.WithLogger(silentLogger())).RunOnce(ctx); err != nil {
				t.Fatalf("dispatcher: %v", err)
			}
			if _, err := events.NewSubscriptionFanOut(pool, events.WithSubscriptionFanOutLogger(silentLogger())).RunOnce(ctx); err != nil {
				t.Fatalf("fan-out: %v", err)
			}
		}
		publish()
		if sent, err := sender.RunOnce(ctx); err != nil || sent != 1 {
			t.Fatalf("first digest = (%d, %v), want 1", sent, err)
		}
		publish()
		if sent, err := sender.RunOnce(ctx); err != nil || sent != 0 {
			t.Fatalf("second digest inside the daily window = (%d, %v), want 0", sent, err)
		}

		// The account switches to immediate through the real route; the very
		// next pass sends the queued event.
		resp := alice.do(t, http.MethodPut, "/api/v1/notifications/preferences", `{"cadence":"immediate"}`)
		mustStatus(t, resp, http.StatusOK)
		if sent, err := sender.RunOnce(ctx); err != nil || sent != 1 {
			t.Errorf("after switching to immediate the queued digest = (%d, %v), want 1", sent, err)
		}
	})
}
