// Task T0813: the Contribution Ledger is wired into production and its
// `via` column has a real source — over a REAL PostgreSQL, the real stores
// and, for the wiring half, the real cmd/worker process.
//
// T0807 delivered the ledger projection (internal/contribution/ledger.go's
// mapping table, the store, the application projector) and said in its own
// result that two things were missing and outside its write scope: nothing
// drove the projector in a running process, and contribution_events.via
// was NULL on every row — the leg that would set it lives in
// internal/events, and 00087's "HONEST STATE OF THE CHAIN" comment says so
// verbatim. This file is the test of those two gaps closed:
//
//   - via travels the WHOLE envelope chain and is copied verbatim at every
//     hop: events.Record(via=…, payload says something else) →
//     outbox_events.via → (dispatcher.RunOnce) research_events.via →
//     (projector.RunOnce) contribution_events.via. The event's payload
//     deliberately carries a DIFFERENT channel, so a hop that re-derived
//     the value from the payload would be caught rather than pass;
//   - the control case: an event with no channel records NULL at all three
//     hops — the chain is not propped up by a default, and the assertion
//     above can therefore report the opposite result;
//   - a channel outside domain.StateVia's six is refused by the recorder
//     and writes NO row (fail closed), so a bad value cannot enter the
//     chain at all;
//   - the producer paths that know their channel now declare it: an object
//     created through the real RSG service lands a ledger row whose via is
//     'api' without the test naming a channel anywhere;
//   - and the mount itself: a real cmd/worker process, with the test
//     database seeded by the fixture, both PUBLISHES the recorded events
//     (its dispatcher) and PROJECTS them into the ledger (its
//     contribution projector) — the row appearing in the database is the
//     evidence, not the existence of a Run method — while an event type
//     with no mapping is reported at runtime in the same process's log.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
)

// t0813Correlation names the one outbox row a chain test is following, so
// the hops are joined by an id this test controls rather than by
// event_type (which the fixture's own setup also produces).
const t0813Correlation = "T0813-via-chain"

// t0813ObjectID is the (well-formed, and otherwise unused) object id the
// chain tests' payloads name: the ledger copies refs out of the payload
// verbatim, and a malformed id there would make the assertion about the
// channel carry a second, unrelated uncertainty.
const t0813ObjectID = "0193b0f0-0000-7000-8000-000000000813"

// viaChain is one event's channel at each hop of the envelope chain, as the
// three tables store it (nil = NULL, the honest "no channel recorded").
type viaChain struct {
	outbox     *string
	research   *string
	ledger     *string
	ledgerRows int
}

// recordViaChainEvent records ONE event into the outbox on the fixture's
// pool with the given envelope channel, and returns the outbox row's id.
//
// The payload deliberately claims a channel of its own ("web") that differs
// from the envelope: the envelope is what the write path recorded, and a
// hop that read payload->>'via' would answer "web" — the value that must
// never appear in the ledger.
func recordViaChainEvent(t *testing.T, ctx context.Context, f *ledgerFixture, eventType string, via domain.StateVia) string {
	t.Helper()
	// A raw channel is passed through as-is on purpose: the fail-closed
	// case needs to reach the recorder, and a typed call could not spell
	// a value the vocabulary refuses.
	payload, err := json.Marshal(map[string]any{
		"object_id":   t0813ObjectID,
		"object_type": "hypothesis",
		"statement":   "the envelope, not the payload, carries the channel",
		"via":         "web",
	})
	if err != nil {
		t.Fatalf("render the event payload: %v", err)
	}
	if err := events.Record(ctx, f.pool, events.Event{
		EventType:     eventType,
		ActorID:       f.alice.ID,
		ProjectID:     f.project.ID,
		Visibility:    events.VisibilityPrivate,
		CorrelationID: t0813Correlation,
		Via:           via,
		Payload:       payload,
	}); err != nil {
		t.Fatalf("record the %s event (via=%q): %v", eventType, via, err)
	}
	var outboxID string
	if err := f.pool.QueryRow(ctx,
		`SELECT id::text FROM outbox_events WHERE correlation_id = $1`, t0813Correlation,
	).Scan(&outboxID); err != nil {
		t.Fatalf("read the recorded outbox row: %v", err)
	}
	return outboxID
}

// readViaChain reads the channel at each hop for one outbox row. The
// research event is found by its outbox id (the publish step's idempotency
// key) and the ledger row by its research event id (the projection's), so
// every hop is the row the previous hop produced, not a row that merely
// looks like it.
func readViaChain(t *testing.T, ctx context.Context, f *ledgerFixture, outboxID string) viaChain {
	t.Helper()
	var c viaChain
	if err := f.pool.QueryRow(ctx,
		`SELECT via FROM outbox_events WHERE id = $1`, outboxID,
	).Scan(&c.outbox); err != nil {
		t.Fatalf("read the outbox hop: %v", err)
	}
	var researchID *string
	err := f.pool.QueryRow(ctx,
		`SELECT id::text, via FROM research_events WHERE outbox_event_id = $1`, outboxID,
	).Scan(&researchID, &c.research)
	if err == pgx.ErrNoRows {
		return c // not published yet: the caller's assertion says so
	}
	if err != nil {
		t.Fatalf("read the research-event hop: %v", err)
	}
	if researchID == nil {
		return c
	}
	rows, err := f.pool.Query(ctx,
		`SELECT via FROM contribution_events WHERE research_event_id = $1`, *researchID)
	if err != nil {
		t.Fatalf("read the ledger hop: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v *string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan the ledger hop: %v", err)
		}
		c.ledger = v
		c.ledgerRows++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the ledger hop: %v", err)
	}
	return c
}

// viaOrNull renders one hop for a message: the channel, or "<null>" when the
// hop is NULL. Used where a broken chain must be REPORTED rather than
// dereferenced.
func viaOrNull(v *string) string {
	if v == nil {
		return "<null>"
	}
	return *v
}

// TestContributionLedgerViaEnvelopeChainCopiesVerbatim is the required
// "via envelope chain" test: three hops, one value, copied and never
// re-derived.
func TestContributionLedgerViaEnvelopeChainCopiesVerbatim(t *testing.T) {
	ctx := testCtx(t)
	f := newLedgerFixture(t, ctx)

	outboxID := recordViaChainEvent(t, ctx, f, "scientific_object.version_created", domain.ViaAPI)

	// Hop 1: the write path's declaration reached the outbox column.
	if got := readViaChain(t, ctx, f, outboxID); got.outbox == nil || *got.outbox != string(domain.ViaAPI) {
		t.Fatalf("outbox_events.via = %v, want %q: the envelope column must carry what the write path declared",
			got.outbox, domain.ViaAPI)
	}

	// Hop 2: the publisher copies it. The payload says "web"; a publisher
	// that looked at the payload would land "web" here.
	published, err := f.dispatcher.RunOnce(ctx)
	if err != nil {
		t.Fatalf("publish pass: %v", err)
	}
	if published == 0 {
		t.Fatal("the publish pass published nothing — the chain's second hop never ran")
	}
	got := readViaChain(t, ctx, f, outboxID)
	if got.research == nil {
		t.Fatalf("research_events.via for the published row = NULL, want %q (the payload's \"web\" is not the channel)",
			domain.ViaAPI)
	}
	if *got.research != string(domain.ViaAPI) {
		t.Errorf("research_events.via = %q, want %q copied from the outbox row", *got.research, domain.ViaAPI)
	}

	// Hop 3: the projection copies it into the ledger row it writes.
	batch, err := f.projector.RunOnce(ctx)
	if err != nil {
		t.Fatalf("projection pass: %v", err)
	}
	if batch.Projected == 0 {
		t.Fatal("the projection pass projected nothing — the chain's third hop never ran")
	}
	got = readViaChain(t, ctx, f, outboxID)
	if got.ledgerRows != 1 {
		t.Fatalf("contribution_events rows for the event = %d, want exactly 1", got.ledgerRows)
	}
	if got.ledger == nil || *got.ledger != string(domain.ViaAPI) {
		t.Errorf("contribution_events.via = %v, want %q (docs/13 §1's seventh field, copied from the event envelope)",
			got.ledger, domain.ViaAPI)
	}

	// The three hops are the same string, and it is the one the write path
	// declared — asserted as one equality so no hop can drift alone. NULL
	// reads as "<null>" here rather than panicking, so a break in any hop
	// reports the whole chain instead of aborting the test.
	if viaOrNull(got.outbox) != viaOrNull(got.research) || viaOrNull(got.research) != viaOrNull(got.ledger) {
		t.Errorf("the chain is not one value: outbox=%s research=%s ledger=%s",
			viaOrNull(got.outbox), viaOrNull(got.research), viaOrNull(got.ledger))
	}
}

// TestContributionLedgerViaChainControlCaseIsNull is the control for the
// test above: an event whose write path recorded no channel is NULL at all
// three hops.
//
// Without it, "via is api" could be produced by a default somewhere in the
// chain rather than by the write path's declaration, and the assertion
// above could not fail. It also pins what 00087's comments define NULL to
// mean: "the source event carried no channel", never a fabricated one.
func TestContributionLedgerViaChainControlCaseIsNull(t *testing.T) {
	ctx := testCtx(t)
	f := newLedgerFixture(t, ctx)

	outboxID := recordViaChainEvent(t, ctx, f, "scientific_object.version_created", "")

	if got := readViaChain(t, ctx, f, outboxID); got.outbox != nil {
		t.Fatalf("outbox_events.via for an undeclared channel = %q, want NULL", *got.outbox)
	}
	if _, err := f.dispatcher.RunOnce(ctx); err != nil {
		t.Fatalf("publish pass: %v", err)
	}
	if _, err := f.projector.RunOnce(ctx); err != nil {
		t.Fatalf("projection pass: %v", err)
	}
	got := readViaChain(t, ctx, f, outboxID)
	if got.ledgerRows != 1 {
		t.Fatalf("contribution_events rows for the event = %d, want exactly 1 (the row lands; only the channel is absent)",
			got.ledgerRows)
	}
	if got.research != nil || got.ledger != nil {
		t.Errorf("an event with no declared channel produced research via=%v ledger via=%v, want NULL at both hops",
			got.research, got.ledger)
	}
}

// TestContributionLedgerViaOutsideTheVocabularyIsRefused proves the
// recorder is the chain's fail-closed gate: a channel that is not one of
// domain.StateVia's six fails the record AND writes no row, so the value
// cannot reach the outbox, let alone the ledger.
//
// The check would be vacuous if it only asserted the error: the row count
// is what proves nothing was written.
func TestContributionLedgerViaOutsideTheVocabularyIsRefused(t *testing.T) {
	ctx := testCtx(t)
	f := newLedgerFixture(t, ctx)

	var before int64
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events`).Scan(&before); err != nil {
		t.Fatalf("count the outbox: %v", err)
	}

	const badType = "scientific_object.version_created"
	payload := json.RawMessage(`{"object_id":"` + t0813ObjectID + `"}`)
	err := events.Record(ctx, f.pool, events.Event{
		EventType:     badType,
		ActorID:       f.alice.ID,
		ProjectID:     f.project.ID,
		Visibility:    events.VisibilityPrivate,
		CorrelationID: t0813Correlation,
		Via:           domain.StateVia("git"), // near git_compat, and not it
		Payload:       payload,
	})
	if err == nil {
		t.Fatal("the recorder accepted a channel outside the vocabulary — the chain's gate is open")
	}
	if !strings.Contains(err.Error(), "not a canonical channel") {
		t.Errorf("refusal = %v, want it to name the channel vocabulary", err)
	}

	var after int64
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events`).Scan(&after); err != nil {
		t.Fatalf("count the outbox again: %v", err)
	}
	if after != before {
		t.Errorf("outbox_events rows = %d after a refused record, want %d: a refused event must write nothing",
			after, before)
	}
	// And nothing reached the log either (the same statement's other half:
	// a row could have been written by a path that did not check).
	var logged int64
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM research_events WHERE event_type = $1 AND payload->>'object_id' = $2`,
		badType, t0813ObjectID,
	).Scan(&logged); err != nil {
		t.Fatalf("count the research log: %v", err)
	}
	if logged != 0 {
		t.Errorf("research_events rows for the refused event = %d, want 0", logged)
	}
}

// TestContributionLedgerViaComesFromTheWritePath proves the producers fill
// the envelope: an object created through the REAL RSG service — which
// declares its channel once, in its own CommitParams — lands a ledger row
// whose via is 'api', with this test naming no channel anywhere.
//
// That is the difference between "the column can be filled" and "the write
// paths fill it": the value here travelled from
// internal/application/rsg's commit declaration through the outbox, the
// publisher and the projection without any test-side help.
func TestContributionLedgerViaComesFromTheWritePath(t *testing.T) {
	ctx := testCtx(t)
	f := newLedgerFixture(t, ctx)

	obj, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, f.branch, rsg.CreateObjectInput{
		ObjectType: "hypothesis",
		Payload: json.RawMessage(
			`{"statement":"the write path declares its channel","question_id":null}`),
	})
	if err != nil {
		t.Fatalf("CreateObject hypothesis: %v", err)
	}
	if _, err := f.dispatcher.RunOnce(ctx); err != nil {
		t.Fatalf("publish pass: %v", err)
	}
	if _, err := f.projector.RunOnce(ctx); err != nil {
		t.Fatalf("projection pass: %v", err)
	}

	// The ledger row for THIS object's creation: the projection writes it
	// from the version_created event, whose payload names object_id.
	rows, err := f.pool.Query(ctx, `
		SELECT c.via, c.event_type, c.actor_id::text, c.role_codes
		  FROM contribution_events c
		  JOIN research_events r ON r.id = c.research_event_id
		 WHERE r.event_type = 'scientific_object.version_created'
		   AND r.payload->>'object_id' = $1`, obj.Object.ID)
	if err != nil {
		t.Fatalf("read the ledger row: %v", err)
	}
	defer rows.Close()
	var (
		via       *string
		eventType string
		actorID   string
		roles     []string
		found     int
	)
	for rows.Next() {
		if err := rows.Scan(&via, &eventType, &actorID, &roles); err != nil {
			t.Fatalf("scan the ledger row: %v", err)
		}
		found++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the ledger row: %v", err)
	}
	if found != 1 {
		t.Fatalf("ledger rows for the created object = %d, want exactly 1", found)
	}
	if via == nil || *via != string(domain.ViaAPI) {
		t.Errorf("contribution_events.via = %v, want %q: the RSG write path declares its channel and the ledger copies it",
			via, domain.ViaAPI)
	}
	if actorID != f.alice.ID {
		t.Errorf("ledger actor = %s, want the creating actor %s", actorID, f.alice.ID)
	}
	if len(roles) != 1 || roles[0] != "hypothesis_proposal" {
		t.Errorf("ledger roles = %v, want the object type's role [hypothesis_proposal]", roles)
	}
}

// TestContributionLedgerProjectorIsMountedInTheWorker is the required
// "contribution ledger wiring" test. It runs the REAL cmd/worker binary
// against the fixture's database and mini-Redis, and proves two things no
// unit test can:
//
//   - the projection LOOP is driven by the worker process and lands rows:
//     an object created through the service after the worker is up is
//     published by the worker's own dispatcher and projected by the
//     worker's own ledger goroutine, so the contribution_events row that
//     appears could only have been written there (this test never calls
//     RunOnce, and never publishes anything);
//   - the coverage visibility is real at runtime: the state.committed
//     event of that same write has no ledger mapping, and the worker says
//     so in its log, once, with the event type — the standing fact is
//     stated by the running process rather than being a claim in a table.
func TestContributionLedgerProjectorIsMountedInTheWorker(t *testing.T) {
	ctx := testCtx(t)
	f := newLedgerFixture(t, ctx)

	redisServer := startMiniRedis(t)
	worker := startPostWorker(t, buildPostWorker(t), f.dbURL, redisServer.Addr())

	obj, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, f.branch, rsg.CreateObjectInput{
		ObjectType: "hypothesis",
		Payload: json.RawMessage(
			`{"statement":"projected by the worker, not by the test","question_id":null}`),
	})
	if err != nil {
		t.Fatalf("CreateObject hypothesis: %v", err)
	}

	// The row the worker's projection must write. Read through
	// outbox → research_events → contribution_events, so "a ledger row
	// exists" cannot be satisfied by some other event's row.
	waitFor(t, 60*time.Second, func() string {
		var (
			via       *string
			projected *time.Time
		)
		err := f.pool.QueryRow(ctx, `
			SELECT c.via, c.occurred_at
			  FROM contribution_events c
			  JOIN research_events r ON r.id = c.research_event_id
			  JOIN outbox_events o ON o.id = r.outbox_event_id
			 WHERE r.event_type = 'scientific_object.version_created'
			   AND r.payload->>'object_id' = $1`, obj.Object.ID).Scan(&via, &projected)
		if err == pgx.ErrNoRows {
			return "the worker has not written a contribution_events row for the created object yet"
		}
		if err != nil {
			t.Fatalf("read the ledger row: %v", err)
		}
		if via == nil || *via != string(domain.ViaAPI) {
			return fmt.Sprintf("the worker projected via=%v, want %q", via, domain.ViaAPI)
		}
		return ""
	}, func() string { return worker.output() })

	// The unmapped event of the same write is reported by the worker at
	// runtime: state.committed has no ledger row in the mapping table
	// (T0807's decision), and a type nobody projects must be a number
	// someone can read, not a hole.
	waitFor(t, 60*time.Second, func() string {
		log := worker.output()
		if !strings.Contains(log, "event type has no ledger mapping") {
			return "the worker has not reported an unmapped event type"
		}
		if !strings.Contains(log, `"event_type":"state.committed"`) {
			return "the unmapped report does not name state.committed"
		}
		return ""
	}, func() string { return worker.output() })

	// Quote the worker's own line into the test output: the standing fact
	// belongs to the RUNNING process, so a -v run shows what it actually
	// said about the type it could not project, not just that a substring
	// was found.
	for _, line := range strings.Split(worker.output(), "\n") {
		if strings.Contains(line, "event type has no ledger mapping") {
			t.Logf("worker runtime report: %s", line)
			break
		}
	}

	// The mount is stated at startup as well, so an operator can tell "the
	// ledger is running and had nothing to project" from "the ledger is
	// not running" — the projector is silent on a pass with no candidates.
	if !strings.Contains(worker.output(), "research_events -> contribution_events") {
		t.Error("the worker's startup line does not state that the contribution ledger is mounted")
	}
}
