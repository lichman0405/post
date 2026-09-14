package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// Task T1001: Transactional Outbox — over a REAL PostgreSQL. The required
// test "outbox integration" plus the acceptance criterion in its own words:
// crash/retry 不丢事件不重复副作用 (a crash or retry loses no event and
// duplicates no side effect).
//
//   - TestOutboxWriteCommitAtomic: a domain write records its events in
//     the SAME transaction as the state change — two outbox rows, nothing
//     published until the worker runs; one dispatcher pass publishes both
//     with the envelope copied verbatim and occurred_at pinned to the
//     outbox row's created_at (event time = state-change time).
//   - TestOutboxWriteRollsBackWithState: a commit whose callback records
//     an event and then fails persists NEITHER the commit NOR the event.
//   - TestOutboxPublishIdempotentAfterCrash: a crash between "research
//     event inserted" and "outbox row marked published" retries as a no-op
//     on the event and still lands the mark — no loss, no duplicate.
//   - TestOutboxConcurrentPublishNoDuplicates: concurrent dispatcher
//     passes claim disjoint batches (FOR UPDATE SKIP LOCKED) and every
//     outbox row yields exactly one research event.
//   - TestOutboxPublishFailureIsolatedAndRetried: one broken row records
//     its failure and stays pending while the rest of the batch publishes
//     (savepoint isolation), and the next pass drains it — a poison row
//     must not stall the backlog.

const outboxTaskID = "T1001"

// outboxFixture seeds alice, a private project, its genesis state and a
// private branch, and wires the rsg service over the real stores — the
// same composition cmd/api/main.go uses, events.Recorder included.
type outboxFixture struct {
	svc     *rsg.Service
	states  *states.Service
	pool    *pgxpool.Pool
	alice   domain.User
	project domain.Project
	branch  string
}

func newOutboxFixture(t *testing.T, ctx context.Context) *outboxFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), outboxTaskID)
	alice, err := persistence.NewCredentialStore(pool).CreateWithPassword(
		ctx, "outbox-alice@example.com", "hash", "outbox-alice", "Alice")
	if err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	org, _, err := persistence.NewOrgStore(pool).CreateOrganization(ctx, domain.Organization{
		Slug: "outbox-fixture", Name: "Outbox Fixture",
	}, alice.ID, todayUTC())
	if err != nil {
		t.Fatalf("create fixture org: %v", err)
	}
	project, _, err := persistence.NewProjectStore(pool).CreateProject(ctx, domain.Project{
		OrganizationID:  &org.ID,
		Slug:            "outbox-project",
		Name:            "Outbox Project",
		Purpose:         "fixture purpose",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, alice.ID)
	if err != nil {
		t.Fatalf("create fixture project: %v", err)
	}
	stateStore := persistence.NewStateStore(pool)
	statesSvc := states.NewService(stateStore, newCommitGuard(t))
	genesis, err := statesSvc.CreateInitialState(ctx, states.CreateInitialStateParams{
		ProjectID:       project.ID,
		ManifestVersion: "v1",
	})
	if err != nil {
		t.Fatalf("create genesis state: %v", err)
	}
	var branchID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO branches (project_id, name, visibility, git_ref, base_state_id, created_by)
		VALUES ($1, $2, 'private', $3, $4, $5) RETURNING id`,
		project.ID, "main", "refs/heads/main", genesis.ID, alice.ID).Scan(&branchID); err != nil {
		t.Fatalf("seed branch: %v", err)
	}
	svc := rsg.NewService(rsg.Deps{
		Projects:  projects.NewService(persistence.NewProjectStore(pool), persistence.NewOrgStore(pool), authz.NewMatrixEngine()),
		Branches:  branches.NewService(persistence.NewBranchStore(pool)),
		States:    statesSvc,
		Latest:    stateStore,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Authz:     authz.NewMatrixEngine(),
		Schemas:   mustRegistry(t),
		Events:    events.Recorder{},
	})
	return &outboxFixture{
		svc:     svc,
		states:  statesSvc,
		pool:    pool,
		alice:   alice,
		project: project,
		branch:  branchID,
	}
}

func outboxTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// mustRegistry loads the canonical schema registry (the same composition
// cmd/api/main.go uses).
func mustRegistry(t *testing.T) *schemareg.Registry {
	t.Helper()
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	return reg
}

func (f *outboxFixture) newDispatcher(opts ...events.DispatcherOption) *events.Dispatcher {
	opts = append([]events.DispatcherOption{events.WithLogger(outboxTestLogger())}, opts...)
	return events.NewDispatcher(f.pool, opts...)
}

// outboxEnvelope is one outbox row as the tests inspect it.
type outboxEnvelope struct {
	ID            string
	EventType     string
	ActorID       *string
	ProjectID     *string
	Visibility    string
	CorrelationID string
	Payload       []byte
	CreatedAt     time.Time
	PublishedAt   *time.Time
	Attempts      int
	LastError     *string
}

func (f *outboxFixture) outboxRows(t *testing.T, ctx context.Context) []outboxEnvelope {
	t.Helper()
	rows, err := f.pool.Query(ctx, `
		SELECT id, event_type, actor_id, project_id, visibility, correlation_id,
		       payload, created_at, published_at, attempts, last_error
		FROM outbox_events ORDER BY created_at, id`)
	if err != nil {
		t.Fatalf("query outbox rows: %v", err)
	}
	defer rows.Close()
	var out []outboxEnvelope
	for rows.Next() {
		var e outboxEnvelope
		if err := rows.Scan(&e.ID, &e.EventType, &e.ActorID, &e.ProjectID, &e.Visibility,
			&e.CorrelationID, &e.Payload, &e.CreatedAt, &e.PublishedAt, &e.Attempts, &e.LastError); err != nil {
			t.Fatalf("scan outbox row: %v", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate outbox rows: %v", err)
	}
	return out
}

// publishedEvent is one research_event as the tests inspect it, joined to
// its source outbox row.
type publishedEvent struct {
	EventType     string
	ActorID       *string
	ProjectID     *string
	Visibility    string
	CorrelationID string
	Payload       []byte
	OutboxEventID string
	OccurredAt    time.Time
	OutboxCreated time.Time
}

func (f *outboxFixture) publishedEvents(t *testing.T, ctx context.Context) []publishedEvent {
	t.Helper()
	rows, err := f.pool.Query(ctx, `
		SELECT r.event_type, r.actor_id, r.project_id, r.visibility, r.correlation_id,
		       r.payload, r.outbox_event_id, r.occurred_at, o.created_at
		FROM research_events r JOIN outbox_events o ON o.id = r.outbox_event_id
		ORDER BY r.occurred_at, r.id`)
	if err != nil {
		t.Fatalf("query published events: %v", err)
	}
	defer rows.Close()
	var out []publishedEvent
	for rows.Next() {
		var e publishedEvent
		if err := rows.Scan(&e.EventType, &e.ActorID, &e.ProjectID, &e.Visibility,
			&e.CorrelationID, &e.Payload, &e.OutboxEventID, &e.OccurredAt, &e.OutboxCreated); err != nil {
			t.Fatalf("scan published event: %v", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate published events: %v", err)
	}
	return out
}

// TestOutboxWriteCommitAtomic is the required "outbox integration" test:
// the domain transaction writes the outbox (same transaction as the state
// change — nothing published until the worker runs) and the worker's
// publish copies the envelope verbatim with the event time pinned to the
// state-change time.
func TestOutboxWriteCommitAtomic(t *testing.T) {
	ctx := testCtx(t)
	f := newOutboxFixture(t, ctx)

	res, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, f.branch, rsg.CreateObjectInput{
		ObjectType: "material", Payload: json.RawMessage(`{"name":"MOF-5"}`),
	})
	if err != nil {
		t.Fatalf("CreateObject: %v", err)
	}
	if res.Version.StateID == "" {
		t.Fatalf("created version carries no state id: %+v", res.Version)
	}

	// The commit wrote the state change AND its two events atomically: two
	// outbox rows, nothing published (the dispatcher has not run yet).
	outbox := f.outboxRows(t, ctx)
	if len(outbox) != 2 {
		t.Fatalf("outbox rows = %d, want 2 (state.committed + scientific_object.version_created)", len(outbox))
	}
	var eventCount int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM research_events`).Scan(&eventCount); err != nil {
		t.Fatalf("count research events: %v", err)
	}
	if eventCount != 0 {
		t.Fatalf("research_events rows before the worker ran = %d, want 0 (the dispatcher is the only publisher)", eventCount)
	}

	// The envelope columns are written by the producer, never derived later.
	types := map[string]bool{}
	for _, e := range outbox {
		types[e.EventType] = true
		if e.ActorID == nil || *e.ActorID != f.alice.ID {
			t.Errorf("outbox actor_id = %v, want %s", e.ActorID, f.alice.ID)
		}
		if e.ProjectID == nil || *e.ProjectID != f.project.ID {
			t.Errorf("outbox project_id = %v, want %s", e.ProjectID, f.project.ID)
		}
		if e.Visibility != events.VisibilityPrivate {
			t.Errorf("outbox visibility = %q, want private (the branch is private)", e.Visibility)
		}
		if e.CorrelationID == "" {
			t.Error("outbox correlation_id is empty (every event carries one)")
		}
		var payload map[string]any
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatalf("outbox payload is not JSON: %v", err)
		}
		if payload["payload_version"] != events.DefaultPayloadVersion {
			t.Errorf("outbox payload lacks payload_version %q: %v", events.DefaultPayloadVersion, payload)
		}
		if e.PublishedAt != nil || e.Attempts != 0 {
			t.Errorf("outbox row pre-publish state = published %v attempts %d, want unpublished/0", e.PublishedAt, e.Attempts)
		}
	}
	if !types["state.committed"] || !types["scientific_object.version_created"] {
		t.Fatalf("outbox event types = %v, want state.committed + scientific_object.version_created", types)
	}

	// One dispatcher pass publishes both rows into research_events.
	dispatcher := f.newDispatcher()
	n, err := dispatcher.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 2 {
		t.Fatalf("RunOnce published %d rows, want 2", n)
	}

	published := f.publishedEvents(t, ctx)
	if len(published) != 2 {
		t.Fatalf("published research_events = %d, want 2", len(published))
	}
	byOutbox := map[string]outboxEnvelope{}
	for _, e := range outbox {
		byOutbox[e.ID] = e
	}
	for _, p := range published {
		src, ok := byOutbox[p.OutboxEventID]
		if !ok {
			t.Fatalf("research_event %s names no outbox row", p.OutboxEventID)
		}
		// The envelope is copied verbatim from the outbox row.
		if p.EventType != src.EventType || p.Visibility != src.Visibility || p.CorrelationID != src.CorrelationID {
			t.Errorf("published envelope %q/%q/%q differs from outbox %q/%q/%q",
				p.EventType, p.Visibility, p.CorrelationID, src.EventType, src.Visibility, src.CorrelationID)
		}
		if (p.ActorID == nil) != (src.ActorID == nil) || (p.ActorID != nil && *p.ActorID != *src.ActorID) {
			t.Errorf("published actor_id %v differs from outbox %v", p.ActorID, src.ActorID)
		}
		if (p.ProjectID == nil) != (src.ProjectID == nil) || (p.ProjectID != nil && *p.ProjectID != *src.ProjectID) {
			t.Errorf("published project_id %v differs from outbox %v", p.ProjectID, src.ProjectID)
		}
		var payload map[string]any
		if err := json.Unmarshal(p.Payload, &payload); err != nil {
			t.Fatalf("published payload is not JSON: %v", err)
		}
		if payload["payload_version"] != events.DefaultPayloadVersion {
			t.Errorf("published payload lacks payload_version: %v", payload)
		}
		// Event time = state-change time, not publish time (docs/52 §17).
		if !p.OccurredAt.Equal(src.CreatedAt) {
			t.Errorf("published occurred_at %v != outbox created_at %v", p.OccurredAt, src.CreatedAt)
		}
	}

	// Every outbox row is marked published exactly once.
	var marked int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox_events
		WHERE published_at IS NOT NULL AND attempts = 1 AND last_error IS NULL`).Scan(&marked); err != nil {
		t.Fatalf("count marked rows: %v", err)
	}
	if marked != 2 {
		t.Fatalf("marked published rows = %d, want 2 (attempts 1, no error)", marked)
	}

	// A second pass is a no-op: nothing pending, nothing duplicated.
	n, err = dispatcher.RunOnce(ctx)
	if err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if n != 0 {
		t.Fatalf("second RunOnce published %d rows, want 0", n)
	}
	if got := len(f.publishedEvents(t, ctx)); got != 2 {
		t.Fatalf("research_events after second pass = %d, want 2", got)
	}
}

// TestOutboxWriteRollsBackWithState: the outbox row shares the domain
// transaction — a failing callback leaves neither the commit nor its
// events (the no-half-state invariant extends to events).
func TestOutboxWriteRollsBackWithState(t *testing.T) {
	ctx := testCtx(t)
	f := newOutboxFixture(t, ctx)

	head, err := f.states.GetBranchHead(ctx, f.branch)
	if err != nil {
		t.Fatalf("GetBranchHead: %v", err)
	}
	_, _, err = f.states.Commit(ctx, states.CommitParams{
		ProjectID:       f.project.ID,
		BranchID:        f.branch,
		ActorID:         f.alice.ID,
		Via:             domain.ViaAPI,
		Message:         "write that records its event and then fails",
		Operations:      []domain.StateOperation{{Kind: domain.OperationEvidenceAsserted, EntityID: "99999999-9999-4999-8999-999999999999", VersionNo: 1}},
		BaseStateID:     &head.ID,
		ManifestVersion: "v1",
		Gate:            rsgvalidation.GateDraft,
	}, func(ctx context.Context, tx states.Transaction, stateID string) error {
		if err := events.Record(ctx, tx, events.Event{
			EventType:  "state.committed",
			ActorID:    f.alice.ID,
			ProjectID:  f.project.ID,
			Visibility: events.VisibilityPrivate,
			Payload:    json.RawMessage(`{}`),
		}); err != nil {
			return err
		}
		return errors.New("injected semantic write failure")
	})
	if err == nil {
		t.Fatal("the failing commit succeeded, want it to fail")
	}

	// Neither the state transition nor the event persisted.
	var outboxCount, commitCount int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events`).Scan(&outboxCount); err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM state_commits`).Scan(&commitCount); err != nil {
		t.Fatalf("count commits: %v", err)
	}
	if outboxCount != 0 {
		t.Fatalf("outbox rows after rollback = %d, want 0 (the event rolled back with the commit)", outboxCount)
	}
	if commitCount != 0 {
		t.Fatalf("state commits after rollback = %d, want 0", commitCount)
	}
	// The head did not move.
	after, err := f.states.GetBranchHead(ctx, f.branch)
	if err != nil {
		t.Fatalf("GetBranchHead after rollback: %v", err)
	}
	if after.ID != head.ID {
		t.Fatalf("branch head moved %s -> %s on a failed commit", head.ID, after.ID)
	}
}

// TestOutboxPublishIdempotentAfterCrash is the acceptance criterion's crash
// half: the process died between "research event inserted" and "outbox row
// marked published". The retry must land the mark without duplicating the
// event — 不丢事件不重复副作用.
func TestOutboxPublishIdempotentAfterCrash(t *testing.T) {
	ctx := testCtx(t)
	f := newOutboxFixture(t, ctx)
	if _, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, f.branch, rsg.CreateObjectInput{
		ObjectType: "material", Payload: json.RawMessage(`{"name":"MOF-5"}`),
	}); err != nil {
		t.Fatalf("CreateObject: %v", err)
	}

	// Crash simulation: the publisher's research-event insert for the FIRST
	// row landed, but the process died before marking the outbox row.
	var crashedID string
	if err := f.pool.QueryRow(ctx, `SELECT id FROM outbox_events ORDER BY created_at, id LIMIT 1`).Scan(&crashedID); err != nil {
		t.Fatalf("pick crash row: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO research_events (event_type, actor_id, project_id, visibility, payload, correlation_id, occurred_at, outbox_event_id)
		SELECT event_type, actor_id, project_id, visibility, payload, correlation_id, created_at, id
		FROM outbox_events WHERE id = $1`, crashedID); err != nil {
		t.Fatalf("simulate crash insert: %v", err)
	}

	// The retry: both outbox rows get their published-mark (the pre-inserted
	// event is a no-op for the crashed row, the other row publishes fully).
	dispatcher := f.newDispatcher()
	n, err := dispatcher.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 2 {
		t.Fatalf("RunOnce published %d rows, want 2 (the crashed row still needs its mark)", n)
	}

	// Exactly one research event per outbox row — nothing duplicated.
	published := f.publishedEvents(t, ctx)
	if len(published) != 2 {
		t.Fatalf("research_events after retry = %d, want 2 (the crashed insert must not duplicate)", len(published))
	}
	var crashedEvents int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM research_events WHERE outbox_event_id = $1`, crashedID).Scan(&crashedEvents); err != nil {
		t.Fatalf("count crashed-row events: %v", err)
	}
	if crashedEvents != 1 {
		t.Fatalf("research_events for the crashed row = %d, want exactly 1", crashedEvents)
	}

	// The retried row is marked published once, with one attempt, no error.
	var marked int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox_events
		WHERE published_at IS NOT NULL AND attempts = 1 AND last_error IS NULL`).Scan(&marked); err != nil {
		t.Fatalf("count marked rows: %v", err)
	}
	if marked != 2 {
		t.Fatalf("marked published rows = %d, want 2", marked)
	}

	// And the drain is stable: a further pass publishes nothing new.
	n, err = dispatcher.RunOnce(ctx)
	if err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if n != 0 || len(f.publishedEvents(t, ctx)) != 2 {
		t.Fatalf("second pass: published %d, events %d; want 0/2", n, len(f.publishedEvents(t, ctx)))
	}
}

// TestOutboxConcurrentPublishNoDuplicates: concurrent dispatcher passes
// claim disjoint batches (FOR UPDATE SKIP LOCKED) and the idempotent
// insert keeps every outbox row at exactly one research event — the retry
// half of the acceptance criterion under contention.
func TestOutboxConcurrentPublishNoDuplicates(t *testing.T) {
	ctx := testCtx(t)
	f := newOutboxFixture(t, ctx)
	const total = 12
	for i := 0; i < total; i++ {
		if err := events.Record(ctx, f.pool, events.Event{
			EventType:  "state.committed",
			ActorID:    f.alice.ID,
			ProjectID:  f.project.ID,
			Visibility: events.VisibilityPrivate,
			Payload:    json.RawMessage(fmt.Sprintf(`{"n":%d}`, i)),
		}); err != nil {
			t.Fatalf("seed outbox row %d: %v", i, err)
		}
	}

	dispatcher := f.newDispatcher(events.WithBatchSize(3))
	const workers = 4
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := dispatcher.RunOnce(ctx); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent RunOnce: %v", err)
	}

	var published, events, duplicateEvents int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE published_at IS NOT NULL`).Scan(&published); err != nil {
		t.Fatalf("count published rows: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM research_events`).Scan(&events); err != nil {
		t.Fatalf("count research events: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM research_events r
		WHERE (SELECT count(*) FROM research_events r2 WHERE r2.outbox_event_id = r.outbox_event_id) > 1`).Scan(&duplicateEvents); err != nil {
		t.Fatalf("count duplicated events: %v", err)
	}
	if published != total {
		t.Errorf("published outbox rows = %d, want %d", published, total)
	}
	if events != total {
		t.Errorf("research_events = %d, want %d", events, total)
	}
	if duplicateEvents != 0 {
		t.Errorf("outbox rows with more than one research event = %d, want 0", duplicateEvents)
	}
}

// TestOutboxPublishFailureIsolatedAndRetried: one broken row must not
// stall the backlog — the batch keeps publishing around it, the row
// records its failure (attempts/last_error), and the next pass drains it.
func TestOutboxPublishFailureIsolatedAndRetried(t *testing.T) {
	ctx := testCtx(t)
	f := newOutboxFixture(t, ctx)
	for _, tag := range []string{"a", "poison", "c"} {
		if err := events.Record(ctx, f.pool, events.Event{
			EventType:  "state.committed",
			ProjectID:  f.project.ID,
			Visibility: events.VisibilityPrivate,
			Payload:    json.RawMessage(fmt.Sprintf(`{"tag":%q}`, tag)),
		}); err != nil {
			t.Fatalf("seed outbox row %s: %v", tag, err)
		}
	}

	// The publisher seam fails the poison row exactly once, then behaves
	// like production (PublishEvent).
	var failed atomic.Bool
	dispatcher := f.newDispatcher(events.WithPublisher(func(ctx context.Context, tx pgx.Tx, p events.PendingEvent) error {
		var payload map[string]any
		_ = json.Unmarshal(p.Payload, &payload)
		if payload["tag"] == "poison" && !failed.Swap(true) {
			return errors.New("injected publish failure")
		}
		return events.PublishEvent(ctx, tx, p)
	}))

	n, err := dispatcher.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce with a failing row must keep the batch: %v", err)
	}
	if n != 2 {
		t.Fatalf("first pass published %d rows, want 2 (the poison row stays pending)", n)
	}

	// The poison row recorded its failure and stayed pending.
	outbox := f.outboxRows(t, ctx)
	var poison *outboxEnvelope
	for i := range outbox {
		if strings.Contains(string(outbox[i].Payload), `"poison"`) {
			poison = &outbox[i]
		}
	}
	if poison == nil {
		t.Fatal("poison row not found in outbox")
	}
	if poison.PublishedAt != nil || poison.Attempts != 1 || poison.LastError == nil || !strings.Contains(*poison.LastError, "injected publish failure") {
		t.Fatalf("poison row after failed pass = published %v attempts %d last_error %v; want pending, attempts 1, error pinned",
			poison.PublishedAt, poison.Attempts, poison.LastError)
	}
	var healthy int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE published_at IS NOT NULL`).Scan(&healthy); err != nil {
		t.Fatalf("count published rows: %v", err)
	}
	if healthy != 2 {
		t.Fatalf("published rows after failed pass = %d, want 2 (the healthy rows must not roll back with the poison row)", healthy)
	}

	// The next pass: the injected failure is gone, the real publisher drains
	// the poison row and clears its error.
	n, err = dispatcher.RunOnce(ctx)
	if err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("second pass published %d rows, want 1 (the poison row)", n)
	}
	var attempts int
	var lastError *string
	if err := f.pool.QueryRow(ctx, `
		SELECT attempts, last_error FROM outbox_events WHERE payload->>'tag' = 'poison'`).Scan(&attempts, &lastError); err != nil {
		t.Fatalf("read poison row: %v", err)
	}
	if attempts != 2 || lastError != nil {
		t.Fatalf("drained poison row = attempts %d last_error %v; want attempts 2, error cleared", attempts, lastError)
	}
	var eventsCount int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM research_events`).Scan(&eventsCount); err != nil {
		t.Fatalf("count research events: %v", err)
	}
	if eventsCount != 3 {
		t.Fatalf("research_events = %d, want 3 (every outbox row published exactly once)", eventsCount)
	}
}
