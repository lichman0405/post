// Task T0807: Contribution Ledger projection (docs/13 §1) — over a REAL
// PostgreSQL, with the real store, the real event log and (for the end-to-
// end half) the real producers.
//
// docs/13 §1 says a contribution event carries seven things: actor person
// id、affiliation at time、project/object refs、role tags、via agent/client、
// timestamp、accepted/released context. contribution_events has had a place
// for six of them since 00011 and no writer at all; this file is the test of
// the writer. It proves, by trying to make each of them fail:
//
//   - the projection runs over the REAL log: an object created through the
//     rsg service records its events in the outbox, the dispatcher
//     publishes them into research_events, and the projection turns the one
//     event that is a contribution act into exactly one ledger row — the
//     `state.committed` event of the same commit produces none, and is
//     REPORTED as unmapped rather than dropped;
//   - the seven fields land where docs/13 §1 says, including the two that
//     are decisions rather than copies (role tags from the object type, the
//     contexts from the event's own identity);
//   - the affiliation is the one at EVENT time, resolved from the
//     membership windows rather than from the person's current
//     organization: the same actor projects to different organizations for
//     two events at two instants, to NULL when no membership covered the
//     instant, and an organization change moves NEW rows only — re-running
//     the projection can never rewrite an existing row;
//   - re-running is a no-op: the candidate scan skips projected events
//     (Candidates 0), the partial unique index refuses a duplicate source
//     event even for a hand-written or racing second writer, and eight
//     concurrent passes over one backlog still produce exactly one row per
//     event;
//   - the append-only guard covers the new write path: the rows the
//     projection writes cannot be UPDATE-d, DELETE-d or TRUNCATE-d away;
//   - the ledger carries no score, weight, rank or aggregate column at all,
//     and nothing can be read out of role_codes as a judgment;
//   - the mapping table and the role vocabulary cannot drift from their
//     sources (specs/events/event-types.yaml, docs/04 §4).
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	yaml "go.yaml.in/yaml/v3"

	"github.com/lichman0405/post/internal/application/branches"
	appcontribution "github.com/lichman0405/post/internal/application/contribution"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/contribution"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

const ledgerTaskID = "T0807"

// utcDate renders the UTC calendar date n days before now in the text form
// the date columns take; utcDay is the same date as a time.Time, for the one
// call that takes a time (OrgStore.CreateOrganization dates the creator's
// owner membership with the value it is handed — see the fixture).
//
// The membership windows these tests control are dated in UTC ON PURPOSE:
// the projection resolves "affiliation at time" at the event instant's UTC
// date (documented in the store), and the org service dates memberships by
// the LOCAL calendar date (internal/application/orgs dateOnly/today — for a
// UTC node the two coincide). A fixture that mixed a local-dated membership
// into a UTC-ruled projection would make these assertions depend on the
// machine's clock, which is exactly the kind of test that passes here and
// fails in CI. It bit: todayUTC() (tests/integration/org_permission_test.go)
// reads the LOCAL year/month/day and stamps it UTC, so a membership dated
// "today" with it already covers an event at the event's own UTC date
// whenever the local calendar has not rolled over yet — on CI (UTC) always,
// and on a UTC+8 machine from 08:00 local on. T0807 went red on exactly that.
// utcDay is the UTC-labelled opposite: built from time.Now().UTC(), so the
// date it names is the UTC date in every timezone.
func utcDate(daysAgo int) string {
	return utcDay(daysAgo).Format("2006-01-02")
}

// utcDay is utcDate as a time.Time: UTC midnight of that date. The store
// renders the date from the value's own location, so a UTC-labelled time
// yields the UTC calendar date whatever TZ the test process runs under.
func utcDay(daysAgo int) time.Time {
	now := time.Now().UTC().AddDate(0, 0, -daysAgo)
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// ledgerFixture seeds a project owned by orgA with alice as its owner, plus
// the affiliation windows the at-event-time test needs, and wires the real
// projection composition: the subsystem's store, the application
// projector, and the outbox dispatcher that publishes the events the rsg
// service records.
//
// alice's own membership is deliberately UNBOUNDED (start 2025-01-01, no
// end): she is the actor of the end-to-end half, where the write has to
// pass the real authorization matrix, and an ended membership is not a
// thing that test should be mixing into its setup. bob carries the
// membership windows instead — his events are written directly into the
// log, which needs no permission at all.
type ledgerFixture struct {
	svc        *rsg.Service
	pool       *pgxpool.Pool
	store      *contribution.LedgerStore
	projector  *appcontribution.LedgerProjector
	dispatcher *events.Dispatcher
	alice      domain.User
	bob        domain.User
	orgA       string
	orgB       string
	project    domain.Project
	branch     string
}

func newLedgerFixture(t *testing.T, ctx context.Context) *ledgerFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), ledgerTaskID)
	cred := persistence.NewCredentialStore(pool)
	seedUser := func(email, handle, name string) domain.User {
		t.Helper()
		u, err := cred.CreateWithPassword(ctx, email, "hash", handle, name)
		if err != nil {
			t.Fatalf("seed user %s: %v", handle, err)
		}
		return u
	}
	alice := seedUser("ledger-alice@example.com", "ledger-alice", "Alice")
	bob := seedUser("ledger-bob@example.com", "ledger-bob", "Bob")

	orgStore := persistence.NewOrgStore(pool)
	// alice owns orgA for good (the end-to-end write path authorizes
	// through this membership); bob's window ENDED at the start of 2026.
	// CreateOrganization always writes its creator an owner membership dated
	// with the value passed here, so alice's is re-dated to that unbounded
	// window right after, in UTC text like every other window in this
	// fixture.
	orgA, _, err := orgStore.CreateOrganization(ctx, domain.Organization{
		Slug: "ledger-fixture-a", Name: "Ledger Fixture A",
	}, alice.ID, utcDay(0))
	if err != nil {
		t.Fatalf("create fixture org A: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE organization_memberships
		SET affiliation_start = '2025-01-01', affiliation_end = NULL
		WHERE organization_id = $1 AND user_id = $2`, orgA.ID, alice.ID); err != nil {
		t.Fatalf("set alice's org A affiliation: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO organization_memberships
		(organization_id, user_id, role, affiliation_start, affiliation_end, verified)
		VALUES ($1, $2, 'contributor', '2025-01-01', '2026-01-01', false)`, orgA.ID, bob.ID); err != nil {
		t.Fatalf("seed bob's ended org A membership: %v", err)
	}
	// orgB is BOB's organization and bob is its creator — that is the point
	// of the line below, not a detail. CreateOrganization ALWAYS writes its
	// caller an owner membership, so creating orgB with alice (as this
	// fixture first did) leaves her with a SECOND membership starting at the
	// date passed here, and the projection resolves an actor's organization
	// by taking the LATEST-starting membership that covers the event
	// instant. An orgB membership dated "today" therefore outranks orgA for
	// an event happening today, and the end-to-end assertion below would
	// depend on where the machine's clock sits — which is the bug this
	// fixture was fixed for (see utcDate): it was green here and red in CI,
	// for the same code.
	//
	// bob is in orgB as of yesterday (UTC) — the CURRENT affiliation his
	// newer events must resolve to. The start is deliberately in the past
	// rather than "today": the projection compares the event's UTC date
	// against the membership's dates, and a membership starting today would
	// be a boundary case in the fixture instead of in the assertion.
	orgB, _, err := orgStore.CreateOrganization(ctx, domain.Organization{
		Slug: "ledger-fixture-b", Name: "Ledger Fixture B",
	}, bob.ID, utcDay(1))
	if err != nil {
		t.Fatalf("create fixture org B: %v", err)
	}

	projectStore := persistence.NewProjectStore(pool)
	project, _, err := projectStore.CreateProject(ctx, domain.Project{
		OrganizationID:  &orgA.ID,
		Slug:            "ledger-project",
		Name:            "Ledger Project",
		Purpose:         "fixture purpose",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, alice.ID)
	if err != nil {
		t.Fatalf("create fixture project: %v", err)
	}

	stateStore := persistence.NewStateStore(pool)
	svc := rsg.NewService(rsg.Deps{
		Projects:  projects.NewService(projectStore, orgStore, authz.NewMatrixEngine()),
		Branches:  branches.NewService(persistence.NewBranchStore(pool)),
		States:    states.NewService(stateStore, newCommitGuard(t)),
		Latest:    stateStore,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Authz:     authz.NewMatrixEngine(),
		Schemas:   mustRegistry(t),
		Events:    events.Recorder{},
	})
	// The first branch bootstraps the genesis root through the real
	// service, exactly as the rsg fixture does.
	branch, err := svc.CreateBranch(ctx, alice, project.ID, rsg.CreateBranchInput{
		Name:       "main",
		BaseRef:    "",
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	store := contribution.NewLedgerStore(pool)
	return &ledgerFixture{
		svc:        svc,
		pool:       pool,
		store:      store,
		projector:  appcontribution.NewLedgerProjector(store, appcontribution.WithLedgerLogger(outboxTestLogger())),
		dispatcher: events.NewDispatcher(pool, events.WithLogger(outboxTestLogger())),
		alice:      alice,
		bob:        bob,
		orgA:       orgA.ID,
		orgB:       orgB.ID,
		project:    project,
		branch:     branch.ID,
	}
}

// ledgerRow is one contribution_events row as these tests inspect it.
type ledgerRow struct {
	ID              string
	ActorID         string
	OrgAtTime       *string
	ProjectID       *string
	EventType       string
	RoleCodes       []string
	ObjectRefs      []string
	AcceptedContext bool
	ReleasedContext bool
	OccurredAt      time.Time
	Via             *string
	SourceEventID   *string
}

// ledgerRowColumns is the ledger's complete column list (00011's columns
// plus 00087's two). It is asserted EXACTLY by
// TestContributionLedgerCarriesNoScoreSurface: a new column on this table
// has to be a deliberate edit of this list, which is what makes "no
// aggregate on a ledger row" (docs/13 §4/§6, CLAUDE.md §9 invariant 13) a
// property of the schema rather than a promise.
var ledgerRowColumns = []string{
	"id", "actor_id", "organization_id_at_time", "project_id", "event_type",
	"role_codes", "object_refs", "accepted_context", "released_context",
	"occurred_at", "research_event_id", "via",
}

func (f *ledgerFixture) rowFor(t *testing.T, ctx context.Context, sourceEventID string) (ledgerRow, bool) {
	t.Helper()
	var (
		r    ledgerRow
		refs []byte
	)
	err := f.pool.QueryRow(ctx, `
		SELECT id, actor_id, organization_id_at_time, project_id, event_type,
		       role_codes, object_refs, accepted_context, released_context,
		       occurred_at, via, research_event_id
		  FROM contribution_events
		 WHERE research_event_id = $1`, sourceEventID).
		Scan(&r.ID, &r.ActorID, &r.OrgAtTime, &r.ProjectID, &r.EventType,
			&r.RoleCodes, &refs, &r.AcceptedContext, &r.ReleasedContext,
			&r.OccurredAt, &r.Via, &r.SourceEventID)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return ledgerRow{}, false
		}
		t.Fatalf("read ledger row for event %s: %v", sourceEventID, err)
	}
	if err := json.Unmarshal(refs, &r.ObjectRefs); err != nil {
		t.Fatalf("decode object_refs %s: %v", refs, err)
	}
	return r, true
}

func (f *ledgerFixture) mustRowFor(t *testing.T, ctx context.Context, sourceEventID string) ledgerRow {
	t.Helper()
	r, ok := f.rowFor(t, ctx, sourceEventID)
	if !ok {
		t.Fatalf("no ledger row for source event %s — the projection did not record it", sourceEventID)
	}
	return r
}

func (f *ledgerFixture) rowCount(t *testing.T, ctx context.Context) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM contribution_events`).Scan(&n); err != nil {
		t.Fatalf("count ledger rows: %v", err)
	}
	return n
}

// ledgerEvent is one research_events row these tests write directly: the
// projection's input, constructed exactly as the log stores it (envelope
// columns and payload apart).
type ledgerEvent struct {
	eventType  string
	actorID    string
	projectID  string
	occurredAt time.Time
	via        *string
	payload    string
}

func (f *ledgerFixture) recordEvent(t *testing.T, ctx context.Context, e ledgerEvent) string {
	t.Helper()
	if e.payload == "" {
		e.payload = "{}"
	}
	var id string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO research_events
		    (event_type, actor_id, project_id, visibility, payload, correlation_id, occurred_at, via)
		VALUES ($1, $2, $3, 'private', $4::jsonb, 'ledger-test', $5, $6)
		RETURNING id`,
		e.eventType, e.actorID, e.projectID, e.payload, e.occurredAt, e.via).Scan(&id); err != nil {
		t.Fatalf("record %s event: %v", e.eventType, err)
	}
	return id
}

// unmappedCount reads one type's cumulative count out of a projection
// report.
func unmappedCount(batch contribution.LedgerBatch, eventType string) int64 {
	for _, c := range batch.Unmapped {
		if c.EventType == eventType {
			return c.Count
		}
	}
	return 0
}

// textOrNull renders a nullable text column for a test message. The message
// has to carry the VALUE: printing the *string itself with %v prints the
// pointer (0x...), which is unreadable exactly where it matters — a CI log.
func textOrNull(v *string) string {
	if v == nil {
		return "<NULL>"
	}
	return *v
}

// ---------------------------------------------------------------------------
// The required test: "contribution projection".

// TestContributionLedgerProjectionEndToEnd runs the whole chain over a real
// database: rsg service → outbox → dispatcher → research_events →
// projection → contribution_events.
func TestContributionLedgerProjectionEndToEnd(t *testing.T) {
	ctx := testCtx(t)
	f := newLedgerFixture(t, ctx)

	// --- the producers write the log ------------------------------------
	// A research question, then a hypothesis that pins it (the reference
	// guard needs a real question_id), both through the real service: each
	// commit records state.committed + scientific_object.version_created
	// into the outbox inside the commit's own transaction.
	question, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, f.branch, rsg.CreateObjectInput{
		ObjectType: "research_question",
		Payload:    json.RawMessage(`{"statement":"What MOFs maximize CO2 uptake at 298 K?","purpose":"screen MOFs","question_state":"open"}`),
	})
	if err != nil {
		t.Fatalf("CreateObject research_question: %v", err)
	}
	hypothesis, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, f.branch, rsg.CreateObjectInput{
		ObjectType: "hypothesis",
		Payload: json.RawMessage(fmt.Sprintf(
			`{"statement":"MOF-5 outperforms ZIF-8 at low pressure","question_id":%q}`, question.Object.ID)),
	})
	if err != nil {
		t.Fatalf("CreateObject hypothesis: %v", err)
	}

	published, err := f.dispatcher.RunOnce(ctx)
	if err != nil {
		t.Fatalf("dispatcher pass: %v", err)
	}
	if published != 4 {
		t.Fatalf("dispatcher published %d events, want 4 (two commits x state.committed + version_created)", published)
	}
	// The log holds the four events with their envelope columns; the
	// payload of each version_created names the object it created.
	type loggedEvent struct {
		id        string
		actorID   *string
		projectID *string
		payload   []byte
	}
	var logged []loggedEvent
	rows, err := f.pool.Query(ctx, `SELECT id, actor_id, project_id, payload FROM research_events
		WHERE event_type = 'scientific_object.version_created' ORDER BY occurred_at, id`)
	if err != nil {
		t.Fatalf("read the version_created events: %v", err)
	}
	for rows.Next() {
		var e loggedEvent
		if err := rows.Scan(&e.id, &e.actorID, &e.projectID, &e.payload); err != nil {
			t.Fatalf("scan the version_created event: %v", err)
		}
		logged = append(logged, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the version_created events: %v", err)
	}
	if len(logged) != 2 {
		t.Fatalf("version_created events in the log = %d, want 2", len(logged))
	}
	var hypothesisEventID, questionEventID string
	for _, e := range logged {
		var payload struct {
			ObjectID string `json:"object_id"`
		}
		if err := json.Unmarshal(e.payload, &payload); err != nil {
			t.Fatalf("decode version_created payload: %v", err)
		}
		switch payload.ObjectID {
		case hypothesis.Object.ID:
			hypothesisEventID = e.id
		case question.Object.ID:
			questionEventID = e.id
		}
	}
	if hypothesisEventID == "" || questionEventID == "" {
		t.Fatalf("version_created events (%v) do not name the two objects the service created", logged)
	}

	// --- one projection pass --------------------------------------------
	batch, err := f.projector.RunOnce(ctx)
	if err != nil {
		t.Fatalf("projection pass: %v", err)
	}
	// Four events are in the log and two of them are contribution acts: the
	// scan is restricted to the MAPPED types (two candidates), and the two
	// state.committed events are not scanned but COUNTED as unmapped —
	// an anti-join over the whole log would re-read the permanently
	// unmappable events forever.
	if batch.Candidates != 2 || batch.Projected != 2 {
		t.Errorf("pass 1: candidates/projected = %d/%d, want 2/2 (the two version_created events are the ledger rows)",
			batch.Candidates, batch.Projected)
	}
	if got := unmappedCount(batch, "state.committed"); got != 2 {
		t.Errorf("unmapped state.committed = %d, want 2: an event with no ledger mapping must be REPORTED, never dropped", got)
	}
	if batch.Pending != 0 {
		t.Errorf("pending after the pass = %d, want 0", batch.Pending)
	}
	if n := f.rowCount(t, ctx); n != 2 {
		t.Fatalf("ledger rows = %d, want 2", n)
	}

	// --- docs/13 §1's seven fields, on the hypothesis row ----------------
	row := f.mustRowFor(t, ctx, hypothesisEventID)
	if row.ActorID != f.alice.ID {
		t.Errorf("actor_id = %s, want %s (the event's actor)", row.ActorID, f.alice.ID)
	}
	if row.OrgAtTime == nil || *row.OrgAtTime != f.orgA {
		t.Errorf("organization_id_at_time = %s, want org A %s (alice's membership at the event instant)",
			textOrNull(row.OrgAtTime), f.orgA)
	}
	if row.ProjectID == nil || *row.ProjectID != f.project.ID {
		t.Errorf("project_id = %s, want %s", textOrNull(row.ProjectID), f.project.ID)
	}
	if row.EventType != "scientific_object.version_created" {
		t.Errorf("event_type = %q, want the source event's name", row.EventType)
	}
	if len(row.RoleCodes) != 1 || row.RoleCodes[0] != string(contribution.RoleHypothesisProposal) {
		t.Errorf("role_codes = %v, want [%s]: the role comes from the object type the event carries",
			row.RoleCodes, contribution.RoleHypothesisProposal)
	}
	if len(row.ObjectRefs) != 1 || row.ObjectRefs[0] != "object:"+hypothesis.Object.ID {
		t.Errorf("object_refs = %v, want [object:%s]", row.ObjectRefs, hypothesis.Object.ID)
	}
	if row.AcceptedContext || row.ReleasedContext {
		t.Errorf("contexts = (accepted %v, released %v), want both false: this event is neither",
			row.AcceptedContext, row.ReleasedContext)
	}
	if row.SourceEventID == nil || *row.SourceEventID != hypothesisEventID {
		t.Errorf("research_event_id = %s, want %s", textOrNull(row.SourceEventID), hypothesisEventID)
	}
	// via: the source event carries no channel, so the row carries none.
	// NOT a default: the alternative — writing 'api' — would read exactly
	// like a recorded fact.
	if row.Via != nil {
		t.Errorf("via = %q, want NULL: an event whose envelope carries no channel must not be given one", *row.Via)
	}
	// The timestamp is the EVENT's, never the projection run's.
	var eventOccurredAt time.Time
	if err := f.pool.QueryRow(ctx, `SELECT occurred_at FROM research_events WHERE id = $1`, hypothesisEventID).
		Scan(&eventOccurredAt); err != nil {
		t.Fatalf("read the source event's occurred_at: %v", err)
	}
	if !row.OccurredAt.Equal(eventOccurredAt) {
		t.Errorf("occurred_at = %s, want the event's own %s", row.OccurredAt, eventOccurredAt)
	}

	// The other object type lands as the other role: the tags are read from
	// the event, not assumed.
	qRow := f.mustRowFor(t, ctx, questionEventID)
	if len(qRow.RoleCodes) != 1 || qRow.RoleCodes[0] != string(contribution.RoleResearchQuestionProposal) {
		t.Errorf("role_codes for the research question = %v, want [%s]",
			qRow.RoleCodes, contribution.RoleResearchQuestionProposal)
	}

	// The two state.committed events produced NO ledger row — they are the
	// reported gap, not a phantom ledger entry.
	var stateRows int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM contribution_events c
		JOIN research_events r ON r.id = c.research_event_id
		WHERE r.event_type = 'state.committed'`).Scan(&stateRows); err != nil {
		t.Fatalf("count ledger rows for state.committed: %v", err)
	}
	if stateRows != 0 {
		t.Errorf("ledger rows for state.committed = %d, want 0 (it has no mapping)", stateRows)
	}

	// --- re-running changes nothing --------------------------------------
	before := f.mustRowFor(t, ctx, hypothesisEventID)
	again, err := f.projector.RunOnce(ctx)
	if err != nil {
		t.Fatalf("second projection pass: %v", err)
	}
	if again.Candidates != 0 || again.Projected != 0 || again.Duplicates != 0 {
		t.Errorf("second pass: candidates/projected/duplicates = %d/%d/%d, want 0/0/0 (every event already has its row)",
			again.Candidates, again.Projected, again.Duplicates)
	}
	if n := f.rowCount(t, ctx); n != 2 {
		t.Errorf("ledger rows after re-running = %d, want 2", n)
	}
	after := f.mustRowFor(t, ctx, hypothesisEventID)
	if !reflect.DeepEqual(after, before) {
		t.Errorf("the re-run changed the row:\n before %+v\n after  %+v", before, after)
	}
}

// TestContributionLedgerAffiliationIsTheOneAtEventTime proves docs/13 §1's
// "affiliation at time" against real membership windows, and that changing
// an affiliation moves NEW rows only.
func TestContributionLedgerAffiliationIsTheOneAtEventTime(t *testing.T) {
	ctx := testCtx(t)
	f := newLedgerFixture(t, ctx)

	// bob: orgA from 2025-01-01 to 2026-01-01, orgB from today.
	old := f.recordEvent(t, ctx, ledgerEvent{
		eventType: "contribution.accepted", actorID: f.bob.ID, projectID: f.project.ID,
		occurredAt: time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC),
	})
	current := f.recordEvent(t, ctx, ledgerEvent{
		eventType: "contribution.accepted", actorID: f.bob.ID, projectID: f.project.ID,
		occurredAt: time.Now().UTC(),
	})
	// Before bob joined ANY organization: no membership covers the instant,
	// so the row records none — it does not fall back to his latest one.
	before := f.recordEvent(t, ctx, ledgerEvent{
		eventType: "contribution.accepted", actorID: f.bob.ID, projectID: f.project.ID,
		occurredAt: time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC),
	})

	batch, err := f.projector.RunOnce(ctx)
	if err != nil {
		t.Fatalf("projection pass: %v", err)
	}
	if batch.Projected != 3 {
		t.Fatalf("projected = %d, want 3", batch.Projected)
	}

	// The SAME actor, three instants, three different answers: an
	// implementation that read the current organization (or the first
	// membership row) would give orgB three times.
	if r := f.mustRowFor(t, ctx, old); r.OrgAtTime == nil || *r.OrgAtTime != f.orgA {
		t.Errorf("organization at 2025-06-01 = %s, want org A %s (bob's membership then)", textOrNull(r.OrgAtTime), f.orgA)
	}
	if r := f.mustRowFor(t, ctx, current); r.OrgAtTime == nil || *r.OrgAtTime != f.orgB {
		t.Errorf("organization today = %s, want org B %s (his membership now)", textOrNull(r.OrgAtTime), f.orgB)
	}
	if r := f.mustRowFor(t, ctx, before); r.OrgAtTime != nil {
		t.Errorf("organization at 2024-06-01 = %s, want NULL (no membership covered the instant)", textOrNull(r.OrgAtTime))
	}
	// acceptance: the two contribution.accepted events carry the accepted
	// context — the docs/13 §4 dimension that has an event of its own.
	if r := f.mustRowFor(t, ctx, old); !r.AcceptedContext {
		t.Error("accepted_context = false for contribution.accepted, want true")
	}

	// --- the affiliation changes; history does not ------------------------
	// bob joins orgC as of today (UTC) while STILL a member of orgB: from
	// now on two memberships cover the instant, and the later start wins —
	// the tie-break is deterministic, not whichever row the planner read
	// first. The window is written as UTC text after the create, so no
	// rendering of a time.Time can shift it.
	orgC, _, err := persistence.NewOrgStore(f.pool).CreateOrganization(ctx, domain.Organization{
		Slug: "ledger-fixture-c", Name: "Ledger Fixture C",
	}, f.bob.ID, utcDay(0))
	if err != nil {
		t.Fatalf("create fixture org C: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE organization_memberships
		SET affiliation_start = $3 WHERE organization_id = $1 AND user_id = $2`,
		orgC.ID, f.bob.ID, utcDate(0)); err != nil {
		t.Fatalf("date bob's org C affiliation in UTC: %v", err)
	}

	moved := f.recordEvent(t, ctx, ledgerEvent{
		eventType: "contribution.accepted", actorID: f.bob.ID, projectID: f.project.ID,
		occurredAt: time.Now().UTC(),
	})
	reRun, err := f.projector.RunOnce(ctx)
	if err != nil {
		t.Fatalf("projection pass after the affiliation change: %v", err)
	}
	// Only the new event is a candidate: an already-projected row is not
	// even re-read, which is the first of the two layers that keep a
	// re-run from rewriting history (the second is the unique index).
	if reRun.Candidates != 1 || reRun.Projected != 1 {
		t.Errorf("pass after the change: candidates/projected = %d/%d, want 1/1", reRun.Candidates, reRun.Projected)
	}
	if r := f.mustRowFor(t, ctx, moved); r.OrgAtTime == nil || *r.OrgAtTime != orgC.ID {
		t.Errorf("organization for the new event = %s, want org C %s (the latest affiliation covering ITS instant)",
			textOrNull(r.OrgAtTime), orgC.ID)
	}
	if r := f.mustRowFor(t, ctx, old); r.OrgAtTime == nil || *r.OrgAtTime != f.orgA {
		t.Errorf("organization of the 2025-06-01 row after the change = %s, want org A %s unchanged",
			textOrNull(r.OrgAtTime), f.orgA)
	}
	if r := f.mustRowFor(t, ctx, current); r.OrgAtTime == nil || *r.OrgAtTime != f.orgB {
		t.Errorf("organization of today's earlier row after the change = %s, want org B %s unchanged",
			textOrNull(r.OrgAtTime), f.orgB)
	}
}

// TestContributionLedgerDedupeAndAppendOnly tries to make the ledger take a
// second row for one event, and to take one away.
func TestContributionLedgerDedupeAndAppendOnly(t *testing.T) {
	ctx := testCtx(t)
	f := newLedgerFixture(t, ctx)

	eventID := f.recordEvent(t, ctx, ledgerEvent{
		eventType: "scientific_object.version_created", actorID: f.alice.ID, projectID: f.project.ID,
		occurredAt: time.Now().UTC(),
		payload:    `{"object_id":"11111111-1111-4111-8111-111111111111","object_type":"dataset"}`,
	})
	if _, err := f.projector.RunOnce(ctx); err != nil {
		t.Fatalf("projection pass: %v", err)
	}
	row := f.mustRowFor(t, ctx, eventID)
	if len(row.RoleCodes) != 1 || row.RoleCodes[0] != string(contribution.RoleDataCuration) {
		t.Fatalf("role_codes = %v, want [%s]", row.RoleCodes, contribution.RoleDataCuration)
	}

	// (1) A second row for the same event is refused by the partial unique
	// index — the projection's dedupe is the DATABASE's, so it holds for a
	// hand-written or racing second writer too.
	_, err := f.pool.Exec(ctx, `INSERT INTO contribution_events
		(actor_id, project_id, event_type, role_codes, object_refs, occurred_at, research_event_id)
		VALUES ($1, $2, 'scientific_object.version_created', '{}', '[]', now(), $3)`,
		f.alice.ID, f.project.ID, eventID)
	wantPGState(t, "INSERT a second ledger row for the same source event", err, "23505")

	// (2) The projection's own shape — ON CONFLICT DO NOTHING — reports the
	// conflict as a no-op rather than an error, which is what makes two
	// concurrent projectors and a re-run safe.
	tag, err := f.pool.Exec(ctx, `INSERT INTO contribution_events
		(actor_id, project_id, event_type, role_codes, object_refs, occurred_at, research_event_id)
		VALUES ($1, $2, 'scientific_object.version_created', '{}', '[]', now(), $3)
		ON CONFLICT (research_event_id) WHERE research_event_id IS NOT NULL DO NOTHING`,
		f.alice.ID, f.project.ID, eventID)
	if err != nil {
		t.Fatalf("INSERT ... ON CONFLICT DO NOTHING: %v", err)
	}
	if n := tag.RowsAffected(); n != 0 {
		t.Errorf("ON CONFLICT insert affected %d rows, want 0", n)
	}
	// The index is PARTIAL: it constrains projected rows only, so a ledger
	// row written directly with no source event stays legal (twice) — that
	// is the property 00046's shape was copied for.
	for i := 0; i < 2; i++ {
		if _, err := f.pool.Exec(ctx, `INSERT INTO contribution_events
			(actor_id, event_type, role_codes, object_refs, occurred_at)
			VALUES ($1, 'scientific_object.version_created', '{}', '[]', now())`, f.alice.ID); err != nil {
			t.Fatalf("INSERT a sourceless ledger row #%d: %v", i+1, err)
		}
	}
	// A row cannot cite an event that does not exist (23503: the FK).
	_, err = f.pool.Exec(ctx, `INSERT INTO contribution_events
		(actor_id, event_type, role_codes, object_refs, occurred_at, research_event_id)
		VALUES ($1, 'scientific_object.version_created', '{}', '[]', now(), gen_random_uuid())`, f.alice.ID)
	wantPGState(t, "INSERT a ledger row citing a fabricated source event", err, "23503")

	// (3) The append-only guard covers the projection's write path: the
	// rows it writes are as immutable as the rest of the ledger.
	_, err = f.pool.Exec(ctx, `UPDATE contribution_events SET role_codes = '{analysis}' WHERE id = $1`, row.ID)
	wantGuardErr(t, "UPDATE a projected ledger row", err, "append-only")
	_, err = f.pool.Exec(ctx, `DELETE FROM contribution_events WHERE id = $1`, row.ID)
	wantGuardErr(t, "DELETE a projected ledger row", err, "append-only")
	_, err = f.pool.Exec(ctx, `TRUNCATE contribution_events`)
	wantGuardErr(t, "TRUNCATE the ledger", err, "append-only")
	// And the row is still there, unchanged.
	if got := f.mustRowFor(t, ctx, eventID); !reflect.DeepEqual(got, row) {
		t.Errorf("the row changed:\n before %+v\n after  %+v", row, got)
	}
}

// TestContributionLedgerConcurrentProjectors proves the dedupe under the
// condition it exists for: several passes over the same backlog.
func TestContributionLedgerConcurrentProjectors(t *testing.T) {
	ctx := testCtx(t)
	f := newLedgerFixture(t, ctx)

	const events = 12
	for i := 0; i < events; i++ {
		f.recordEvent(t, ctx, ledgerEvent{
			eventType: "scientific_object.version_created", actorID: f.alice.ID, projectID: f.project.ID,
			occurredAt: time.Now().UTC().Add(time.Duration(i) * time.Second),
			payload: fmt.Sprintf(
				`{"object_id":"22222222-2222-4222-8222-22222222222%d","object_type":"dataset"}`, i%10),
		})
	}

	// Eight projectors race over one backlog. Whichever of them wins a row,
	// the outcome is the same: no error, and exactly one ledger row per
	// event (the loser's insert is absorbed by ON CONFLICT DO NOTHING).
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	projected := make(chan int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			batch, err := f.store.ProjectBatch(ctx, events)
			if err != nil {
				errs <- err
				return
			}
			projected <- batch.Projected
		}()
	}
	wg.Wait()
	close(errs)
	close(projected)
	for err := range errs {
		t.Errorf("concurrent projection pass: %v", err)
	}
	total := 0
	for n := range projected {
		total += n
	}
	if total != events {
		t.Errorf("projected across all passes = %d, want %d: the sum, not the ledger, is the interesting number — "+
			"and the ledger below is what must be exact", total, events)
	}
	if n := f.rowCount(t, ctx); n != events {
		t.Errorf("ledger rows = %d, want %d (one per event, whatever the race did)", n, events)
	}
	var duplicates int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM (
		SELECT research_event_id FROM contribution_events
		WHERE research_event_id IS NOT NULL
		GROUP BY research_event_id HAVING count(*) > 1) d`).Scan(&duplicates); err != nil {
		t.Fatalf("look for duplicated source events: %v", err)
	}
	if duplicates != 0 {
		t.Errorf("%d source events produced more than one ledger row", duplicates)
	}
}

// TestContributionLedgerViaIsCopiedNotGuessed proves the envelope rule
// (00046) on the one field docs/13 §1 names that no producer carries yet:
// the channel travels as a COLUMN and is copied verbatim, so a payload
// cannot claim a channel and an absent one stays absent.
func TestContributionLedgerViaIsCopiedNotGuessed(t *testing.T) {
	ctx := testCtx(t)
	f := newLedgerFixture(t, ctx)

	channel := "mcp"
	withChannel := f.recordEvent(t, ctx, ledgerEvent{
		eventType: "contribution.accepted", actorID: f.alice.ID, projectID: f.project.ID,
		occurredAt: time.Now().UTC(), via: &channel,
		// The payload claims a DIFFERENT channel: a ledger that read the
		// payload would record claude_code here.
		payload: `{"via":"claude_code"}`,
	})
	withoutChannel := f.recordEvent(t, ctx, ledgerEvent{
		eventType: "contribution.accepted", actorID: f.alice.ID, projectID: f.project.ID,
		occurredAt: time.Now().UTC(),
	})

	if _, err := f.projector.RunOnce(ctx); err != nil {
		t.Fatalf("projection pass: %v", err)
	}
	if r := f.mustRowFor(t, ctx, withChannel); r.Via == nil || *r.Via != channel {
		t.Errorf("via = %s, want %q: the channel is copied from the event's envelope column", textOrNull(r.Via), channel)
	}
	if r := f.mustRowFor(t, ctx, withoutChannel); r.Via != nil {
		t.Errorf("via = %q, want NULL: an event with no channel must not be given one", *r.Via)
	}
}

// TestContributionLedgerRefsComeFromThePayload proves the refs are read out
// of the event (never resolved from today's state) and that a key the event
// does not carry contributes no ref.
func TestContributionLedgerRefsComeFromThePayload(t *testing.T) {
	ctx := testCtx(t)
	f := newLedgerFixture(t, ctx)

	pr := "33333333-3333-4333-8333-333333333333"
	merge := "44444444-4444-4444-8444-444444444444"
	state := "55555555-5555-4555-8555-555555555555"
	full := f.recordEvent(t, ctx, ledgerEvent{
		eventType: "pull_request.merged", actorID: f.alice.ID, projectID: f.project.ID,
		occurredAt: time.Now().UTC(),
		payload: fmt.Sprintf(
			`{"pull_request_id":%q,"merge_id":%q,"state_id":%q,"pull_request_number":7,"plan_digest":"aa"}`,
			pr, merge, state),
	})
	// The same event with two of the three keys missing: the refs it does
	// carry are recorded, the ones it does not are simply absent.
	partial := f.recordEvent(t, ctx, ledgerEvent{
		eventType: "pull_request.merged", actorID: f.alice.ID, projectID: f.project.ID,
		occurredAt: time.Now().UTC(),
		payload:    fmt.Sprintf(`{"pull_request_id":%q}`, pr),
	})
	// A payload that is not even an object: the act, the actor and the
	// project are envelope facts and are still recorded.
	broken := f.recordEvent(t, ctx, ledgerEvent{
		eventType: "pull_request.merged", actorID: f.alice.ID, projectID: f.project.ID,
		occurredAt: time.Now().UTC(),
		payload:    `[1,2,3]`,
	})

	batch, err := f.projector.RunOnce(ctx)
	if err != nil {
		t.Fatalf("projection pass: %v", err)
	}
	if batch.Projected != 3 {
		t.Fatalf("projected = %d, want 3", batch.Projected)
	}
	want := []string{"pull_request:" + pr, "merge:" + merge, "state:" + state}
	sort.Strings(want)
	got := f.mustRowFor(t, ctx, full).ObjectRefs
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("object_refs = %v, want %v", got, want)
	}
	if got := f.mustRowFor(t, ctx, partial).ObjectRefs; len(got) != 1 || got[0] != "pull_request:"+pr {
		t.Errorf("object_refs for the partial payload = %v, want just the ref the event carried", got)
	}
	if got := f.mustRowFor(t, ctx, broken).ObjectRefs; len(got) != 0 {
		t.Errorf("object_refs for a non-object payload = %v, want none", got)
	}
	if r := f.mustRowFor(t, ctx, broken); r.EventType != "pull_request.merged" || r.ActorID != f.alice.ID {
		t.Errorf("the row for a broken payload lost its envelope facts: %+v", r)
	}
	// pull_request.merged is neither an accepted-contribution nor a release
	// fact; docs/13 §4's dimensions are decided by the event, not assumed.
	if r := f.mustRowFor(t, ctx, full); r.AcceptedContext || r.ReleasedContext {
		t.Errorf("contexts = (accepted %v, released %v) for pull_request.merged, want both false",
			r.AcceptedContext, r.ReleasedContext)
	}
}

// TestContributionLedgerCarriesNoScoreSurface is the schema half of
// docs/13 §4 (禁止单一分数) and CLAUDE.md §9 invariant 13: not only is
// there no score column, there is no column a score could be smuggled
// into without editing this list.
func TestContributionLedgerCarriesNoScoreSurface(t *testing.T) {
	ctx := testCtx(t)
	f := newLedgerFixture(t, ctx)

	rows, err := f.pool.Query(ctx, `SELECT column_name FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'contribution_events'`)
	if err != nil {
		t.Fatalf("read the ledger's columns: %v", err)
	}
	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan a column: %v", err)
		}
		got = append(got, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the ledger's columns: %v", err)
	}
	sort.Strings(got)
	want := append([]string(nil), ledgerRowColumns...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("contribution_events columns = %v, want exactly %v — a new column is a deliberate edit, "+
			"and 00087 adds none beyond research_event_id and via", got, want)
	}
	// And the negative control: the scan can see a forbidden column when
	// there is one. The pattern is the one evidence_assertion_test.go uses
	// for the whole catalog (a score/weight column anywhere fails there);
	// here it is run against the ledger's own row shape.
	for _, col := range got {
		lower := strings.ToLower(col)
		for _, banned := range []string{"score", "weight", "rank", "reputation", "rating"} {
			if strings.Contains(lower, banned) {
				t.Errorf("contribution_events.%s carries a %q surface — the ledger records facts, "+
					"and a reputation Profile derives dimensions from them elsewhere (docs/13 §4)", col, banned)
			}
		}
	}
	// The instrument's own control, so the loop above is not vacuous: the
	// same predicate matches a column list that DOES carry one.
	control := append(append([]string(nil), got...), "contribution_score")
	offenders := 0
	for _, col := range control {
		if strings.Contains(strings.ToLower(col), "score") {
			offenders++
		}
	}
	if offenders != 1 {
		t.Fatalf("the score predicate matched %d of %v — this check cannot fail, so it proves nothing", offenders, control)
	}
}

// ---------------------------------------------------------------------------
// Vocabulary drift: the mapping table and the thirteen roles.

// TestContributionLedgerMappingMatchesTheEventVocabulary pins the mapping
// table to specs/events/event-types.yaml (no invented event names, and no
// silent growth) and reports the events the ledger deliberately does not
// record.
func TestContributionLedgerMappingMatchesTheEventVocabulary(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "specs", "events", "event-types.yaml"))
	if err != nil {
		t.Fatalf("read the event vocabulary: %v", err)
	}
	var spec struct {
		Events []string `yaml:"events"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse the event vocabulary: %v", err)
	}
	if len(spec.Events) == 0 {
		t.Fatal("the event vocabulary parsed empty — this check would pass on anything")
	}
	specSet := map[string]bool{}
	for _, e := range spec.Events {
		specSet[e] = true
	}

	mapped := contribution.MappedEventTypes()
	for _, e := range mapped {
		if !specSet[e] {
			t.Errorf("the mapping table names %q, which specs/events/event-types.yaml does not define", e)
		}
		m, ok := contribution.LedgerMappingFor(e)
		if !ok {
			t.Fatalf("MappedEventTypes lists %q but LedgerMappingFor does not find it", e)
		}
		if strings.TrimSpace(m.Why) == "" {
			t.Errorf("mapping %q carries no citation — every row of the table must say why it is a contribution act", e)
		}
		if !contribution.ValidContributionRoles(m.Roles) {
			t.Errorf("mapping %q carries roles outside docs/04 §4: %v", e, m.Roles)
		}
		for objectType, roles := range m.ObjectTypeRoles {
			if !contribution.ValidContributionRoles(roles) {
				t.Errorf("mapping %q's object type %q carries roles outside docs/04 §4: %v", e, objectType, roles)
			}
		}
	}

	// The mapped set is pinned EXACTLY. Growing it (a new event becomes a
	// contribution act) or shrinking it (one stops being recorded) is a
	// decision someone must make here, in the open, not a side effect of a
	// producer landing.
	wantMapped := []string{
		"contribution.accepted",
		"credit.dispute_opened",
		"credit.dispute_resolved",
		"evidence_assertion.created",
		"knowledge.version_published",
		"pull_request.merged",
		"pull_request.reviewed",
		"release.published",
		"research_asset.version_published",
		"scientific_object.version_created",
	}
	sort.Strings(wantMapped)
	if strings.Join(mapped, ",") != strings.Join(wantMapped, ",") {
		t.Errorf("mapped event types = %v, want %v", mapped, wantMapped)
	}

	// The events the ledger does NOT record, named: they are counted in
	// every projection report (Report.Unmapped), so this list is the
	// vocabulary's visible remainder rather than a silent gap.
	var unmapped []string
	for _, e := range spec.Events {
		if _, ok := contribution.LedgerMappingFor(e); !ok {
			unmapped = append(unmapped, e)
		}
	}
	if len(unmapped) != len(spec.Events)-len(wantMapped) {
		t.Errorf("the mapping covers %d of %d event types, and the reported remainder is %v",
			len(wantMapped), len(spec.Events), unmapped)
	}
	t.Logf("the ledger does not record these %d event types (each one is counted, never dropped): %v",
		len(unmapped), unmapped)

	// The instrument can say no: an event outside the table is not
	// projected, and says so.
	if _, ok := contribution.LedgerMappingFor("state.committed"); ok {
		t.Error("state.committed has a mapping — either the table moved or this check is vacuous")
	}
	if _, ok := contribution.ProjectEvent(contribution.LedgerSource{
		EventID: "66666666-6666-4666-8666-666666666666", EventType: "state.committed",
		ActorID: "77777777-7777-4777-8777-777777777777", OccurredAt: time.Now().UTC(),
	}); ok {
		t.Error("ProjectEvent produced a ledger row for an unmapped event type")
	}
}

// TestContributionRoleVocabularyMatchesDocs04 pins the Go vocabulary to
// docs/04 §4 verbatim: the thirteen labels the document lists, each one
// round-tripping to its code and back. A reworded, added or removed role
// fails here rather than shipping as a second, quieter vocabulary.
func TestContributionRoleVocabularyMatchesDocs04(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "docs", "04_USERS_ROLES.md"))
	if err != nil {
		t.Fatalf("read docs/04: %v", err)
	}
	var labels []string
	inSection := false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## 4. Contribution Role") {
			inSection = true
			continue
		}
		if !inSection {
			continue
		}
		if strings.HasPrefix(trimmed, "- ") {
			labels = append(labels, strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")))
			continue
		}
		if trimmed == "" {
			continue
		}
		if len(labels) > 0 {
			break // the list ended at the first prose line after it
		}
	}
	if len(labels) != 13 {
		t.Fatalf("docs/04 §4 lists %d roles (%v), want 13", len(labels), labels)
	}

	roles := contribution.ContributionRoles()
	if len(roles) != len(labels) {
		t.Fatalf("the Go vocabulary holds %d roles, docs/04 §4 lists %d", len(roles), len(labels))
	}
	for _, label := range labels {
		code := strings.ToLower(strings.ReplaceAll(label, " ", "_"))
		role, ok := contribution.ParseContributionRole(code)
		if !ok {
			t.Errorf("docs/04 §4's %q (%s) is not in the Go vocabulary", label, code)
			continue
		}
		if role.Label() != label {
			t.Errorf("role %s is labelled %q in Go and %q in docs/04", code, role.Label(), label)
		}
	}
	// The instrument can say no: a code outside the thirteen is refused.
	if _, ok := contribution.ParseContributionRole("lead_author"); ok {
		t.Error("ParseContributionRole accepted a code docs/04 §4 does not define")
	}
	if contribution.ContributionRole("lead_author").Valid() {
		t.Error("Valid() accepted a code docs/04 §4 does not define")
	}
	if contribution.ValidContributionRoles([]contribution.ContributionRole{
		contribution.RoleAnalysis, contribution.RoleAnalysis,
	}) {
		t.Error("ValidContributionRoles accepted a repeated role")
	}
}
