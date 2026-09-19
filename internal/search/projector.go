package search

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// Projector is the search projection's consumer (T0901, docs/14): the third
// consumer of the transactional outbox (ADR-013), beside the webhook fan-out
// and the subscription fan-out. One pass claims a batch of published outbox
// rows no projector has consumed, turns each into at most one
// search_documents row, and marks the outbox row consumed — all in one
// transaction, each row inside its own savepoint.
//
// The three properties the architecture requires of an outbox consumer, and
// where each one lives here:
//
//   - IDEMPOTENT. The document write is the canonical UpsertSearchDocument
//     (internal/persistence/queries/search.sql:5-15, ON CONFLICT (entity_ref)
//     DO UPDATE), and the cursor write is ON CONFLICT DO NOTHING. A crash
//     between them re-runs both harmlessly on the next pass: the projection
//     is keyed by the entity, not by the event, so replaying an event
//     rewrites the entity's current row rather than accumulating one.
//
//   - PASSTHROUGH-SAFE. The cursor row is written inside the same savepoint
//     as the document, so a row that fails is left unmarked and re-claimed
//     (and re-logged) instead of being dropped — the T1001 lesson, applied
//     exactly as the webhook fan-out (webhook_fanned_out_at) and the
//     subscription fan-out (subscription_fanned_events) apply it. The
//     cursor is this consumer's own table (search_projected_events, 00090):
//     no consumer's progress is visible to another's.
//
//   - COVERAGE-VISIBLE. An event whose type has no projection rule, whose
//     payload names no entity of the right shape, or whose entity is not in
//     the source tables projects nothing — and every one of those is
//     counted in the pass report and logged once, never silently skipped.
//     That is the T0807 pattern for the ledger's coverage gap, and it is
//     what keeps "search is not indexing this yet" a standing fact.
type Projector struct {
	pool      *pgxpool.Pool
	queries   *sqlc.Queries
	log       *slog.Logger
	batchSize int
	interval  time.Duration

	// mu guards warned, the set of coverage facts already reported. Run is a
	// single goroutine; RunOnce is exported and a caller may drive it from
	// several.
	mu     sync.Mutex
	warned map[string]bool
}

// DefaultBatchSize is how many published outbox rows one pass consumes.
const DefaultBatchSize = 100

// DefaultInterval is the pause between passes in Run. The projection is a
// consumer of the event log, not a hot loop: it is expected to run behind
// the outbox dispatcher, and the same second the other two consumers use
// keeps the indexed state within a poll of the committed state.
const DefaultInterval = time.Second

// ProjectorOption tunes a Projector.
type ProjectorOption func(*Projector)

// WithLogger sets the projector's logger (default slog.Default()).
func WithLogger(log *slog.Logger) ProjectorOption {
	return func(p *Projector) { p.log = log }
}

// WithBatchSize sets the rows one pass claims (default DefaultBatchSize).
func WithBatchSize(n int) ProjectorOption {
	return func(p *Projector) { p.batchSize = n }
}

// WithInterval sets the pause between passes in Run (default
// DefaultInterval). It has no effect on RunOnce, which the caller drives.
func WithInterval(d time.Duration) ProjectorOption {
	return func(p *Projector) { p.interval = d }
}

// NewProjector builds the projector on pool. The pool may be lazy
// (persistence.OpenLazy): like every other consumer, the projector retries
// forever while PostgreSQL is down.
func NewProjector(pool *pgxpool.Pool, opts ...ProjectorOption) *Projector {
	p := &Projector{
		pool:      pool,
		queries:   sqlc.New(pool),
		log:       slog.Default(),
		batchSize: DefaultBatchSize,
		interval:  DefaultInterval,
		warned:    map[string]bool{},
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// EventTypeCount is one coverage fact: an event type and how many consumed
// events carried it.
type EventTypeCount struct {
	EventType string
	Count     int
}

// PassReport is what one pass did, so a caller (a test, an operator's
// one-shot command) can assert on the outcome rather than scrape the log.
type PassReport struct {
	// Candidates is how many published outbox rows the pass claimed.
	Candidates int
	// Projected is how many documents the pass wrote. One candidate writes
	// at most one document (one entity, one row), and a candidate whose
	// entity is already indexed rewrites that row rather than adding one.
	Projected int
	// Unmapped counts consumed events whose type the projection's table does
	// not cover.
	Unmapped []EventTypeCount
	// Unaddressed counts events that have a rule whose payload named no
	// entity of the entity type's own identity shape.
	Unaddressed []EventTypeCount
	// Void counts events whose named entity is not in the source tables.
	Void []EventTypeCount
	// Failed is how many rows were left unmarked for retry.
	Failed int
}

// Run consumes the projection backlog until ctx is cancelled: one pass
// immediately, then one pass per interval. Like the dispatcher and the two
// fan-outs, the loop never exits on a transient failure — a down database is
// retried; only ctx cancellation ends it.
func (p *Projector) Run(ctx context.Context) error {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		if _, err := p.RunOnce(ctx); err != nil && ctx.Err() == nil {
			p.log.Error("search: projection pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunOnce consumes one batch of published outbox rows no projector has
// consumed, and returns what the pass did. Each row is processed inside its
// OWN savepoint, so a broken row rolls back to its savepoint and the batch
// continues, leaving that row unmarked for the next pass. The whole pass
// commits in one transaction.
func (p *Projector) RunOnce(ctx context.Context) (PassReport, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return PassReport{}, fmt.Errorf("search: projection: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	claimed, err := p.claim(ctx, tx)
	if err != nil {
		return PassReport{}, err
	}

	report := PassReport{Candidates: len(claimed)}
	unmapped, unaddressed, void := map[string]int{}, map[string]int{}, map[string]int{}
	for _, ev := range claimed {
		log := p.log.With("outbox_id", ev.outboxID, "event_id", ev.eventID,
			"event_type", ev.eventType, "correlation_id", ev.correlationID)
		outcome, err := p.projectOne(ctx, tx, ev, log)
		if err != nil {
			report.Failed++
			continue
		}
		switch outcome {
		case outcomeProjected:
			report.Projected++
		case outcomeUnmapped:
			unmapped[ev.eventType]++
		case outcomeUnaddressed:
			unaddressed[ev.eventType]++
		case outcomeVoid:
			void[ev.eventType]++
		}
	}
	report.Unmapped = countsSorted(unmapped)
	report.Unaddressed = countsSorted(unaddressed)
	report.Void = countsSorted(void)

	if err := tx.Commit(ctx); err != nil {
		return PassReport{}, fmt.Errorf("search: projection: commit: %w", err)
	}
	p.report(report)
	return report, nil
}

// claimProjectable selects the published outbox rows this consumer has not
// consumed yet, locking them so a concurrent pass skips them (FOR UPDATE
// SKIP LOCKED — every consumer's pattern).
//
// Only published rows are claimed: a row whose research event does not exist
// yet has no event to project, and the outbox dispatcher owns that step.
const claimProjectable = `
SELECT oe.id, re.id, re.event_type, re.payload, re.correlation_id
FROM outbox_events oe
JOIN research_events re ON re.outbox_event_id = oe.id
WHERE oe.published_at IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM search_projected_events s WHERE s.outbox_event_id = oe.id)
ORDER BY oe.created_at, oe.id
LIMIT $1
FOR UPDATE OF oe SKIP LOCKED`

// markProjected writes the consumer's cursor row. ON CONFLICT DO NOTHING
// makes a replayed pass a no-op rather than an error.
const markProjected = `
INSERT INTO search_projected_events (outbox_event_id) VALUES ($1)
ON CONFLICT (outbox_event_id) DO NOTHING`

// projectableEvent is one claimed outbox row with its published research
// event, as the projector needs it.
type projectableEvent struct {
	outboxID      string
	eventID       string
	eventType     string
	payload       []byte
	correlationID string
}

func (p *Projector) claim(ctx context.Context, tx pgx.Tx) ([]projectableEvent, error) {
	rows, err := tx.Query(ctx, claimProjectable, p.batchSize)
	if err != nil {
		return nil, fmt.Errorf("search: projection: claim: %w", err)
	}
	defer rows.Close()
	var claimed []projectableEvent
	for rows.Next() {
		var (
			oeID, reID pgtype.UUID
			ev         projectableEvent
		)
		if err := rows.Scan(&oeID, &reID, &ev.eventType, &ev.payload, &ev.correlationID); err != nil {
			return nil, fmt.Errorf("search: projection: scan: %w", err)
		}
		ev.outboxID = uuidText(oeID)
		ev.eventID = uuidText(reID)
		claimed = append(claimed, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search: projection: iterate: %w", err)
	}
	return claimed, nil
}

// outcomeKind is what happened to one claimed row.
type outcomeKind int

const (
	// outcomeNone is returned alongside a non-nil error: the outcome of a
	// failed row is not one of the four facts below, it is "could not look".
	outcomeNone outcomeKind = iota
	outcomeProjected
	outcomeUnmapped
	outcomeUnaddressed
	outcomeVoid
)

// projectOne processes one claimed row inside its own savepoint: the
// document upsert (when the row projects one) and the cursor mark either
// both land or both roll back, so a crash between them re-runs an idempotent
// pair instead of losing an event. The returned error means the row was left
// unmarked and will be re-claimed; the caller counts it.
func (p *Projector) projectOne(ctx context.Context, tx pgx.Tx, ev projectableEvent, log *slog.Logger) (outcomeKind, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		log.Error("search: projection: savepoint begin failed; row stays unmarked for retry", "error", err)
		return outcomeNone, err
	}
	defer func() { _ = sp.Rollback(context.WithoutCancel(ctx)) }() // no-op once committed

	// The outcome is only meaningful when the error is nil: an error means the
	// row was left unmarked and no statement in this savepoint was kept.
	kind, err := p.project(ctx, sp, ev, log)
	if err != nil {
		log.Error("search: projection: projecting the event failed; row stays unmarked for retry", "error", err)
		return outcomeNone, err
	}
	if _, err := sp.Exec(ctx, markProjected, ev.outboxID); err != nil {
		log.Error("search: projection: marking consumed failed; row stays unmarked for retry", "error", err)
		return outcomeNone, err
	}
	if err := sp.Commit(ctx); err != nil {
		log.Error("search: projection: savepoint commit failed; row stays unmarked for retry", "error", err)
		return outcomeNone, err
	}
	return kind, nil
}

// project writes the document one event produces, on db. It returns which of
// the four outcomes happened; an error means "could not look", which is
// never treated as "there is nothing to index".
func (p *Projector) project(ctx context.Context, db pgx.Tx, ev projectableEvent, log *slog.Logger) (outcomeKind, error) {
	rule, ok := RuleFor(ev.eventType)
	if !ok {
		return outcomeUnmapped, nil
	}
	identity := rule.payloadIdentityFor(ev.payload)
	if identity == "" {
		// The event's payload does not name the entity it would index. The
		// event is consumed (the backlog must drain) and the gap is counted:
		// a producer that forgot its identity key shows up as a number, not
		// as an index that quietly misses a class of objects.
		return outcomeUnaddressed, nil
	}
	src, err := sourceFor(rule.EntityType)
	if err != nil {
		return outcomeNone, err
	}
	doc, err := readDocument(ctx, db, src, identity)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The entity the event names is not in the source tables. Nothing
			// is indexed and nothing is deleted: a projection row is dropped
			// by a rebuild, never by a failed read of the thing it projects.
			return outcomeVoid, nil
		}
		return outcomeNone, err
	}
	projectID, err := uuidParam(doc.ProjectID)
	if err != nil {
		return outcomeNone, fmt.Errorf("search: projection: %s: %w", doc.EntityRef, err)
	}
	err = p.queries.WithTx(db).UpsertSearchDocument(ctx, sqlc.UpsertSearchDocumentParams{
		EntityRef:  doc.EntityRef,
		EntityType: doc.EntityType,
		Visibility: doc.Visibility,
		ProjectID:  projectID,
		Title:      doc.Title,
		Content:    doc.Content,
		Structured: doc.Structured,
	})
	if err != nil {
		return outcomeNone, fmt.Errorf("search: projection: upsert %s: %w", doc.EntityRef, err)
	}
	if doc.Visibility != VisibilityPublic {
		// The fail-closed answer is the normal one, not an anomaly: saying it
		// at INFO for every private document would drown the log. It is said
		// at DEBUG so "why is this not searchable publicly" is answerable.
		log.Debug("search: projected a non-public document", "entity_ref", doc.EntityRef,
			"visibility", doc.Visibility)
	}
	return outcomeProjected, nil
}

// readDocument runs a source reader for one identity. db is the caller's
// savepoint, so the read sees exactly the state the projection commits with.
func readDocument(ctx context.Context, db pgx.Tx, src documentSource, identity string) (Document, error) {
	return src.scan(db.QueryRow(ctx, src.byKey, identity))
}

// report logs one pass: the numbers first, then the coverage facts that are
// the reason this consumer counts them at all. A quiet pass says nothing —
// repeating the standing state every interval would bury the passes that did
// something.
func (p *Projector) report(r PassReport) {
	for _, c := range r.Unmapped {
		if p.firstTime("unmapped:" + c.EventType) {
			p.log.Warn("search: event type has no projection rule — consumed, never indexed",
				"event_type", c.EventType, "events_in_log", c.Count,
				"projection_table", "internal/search/projection.go")
		}
	}
	for _, c := range r.Unaddressed {
		if p.firstTime("unaddressed:" + c.EventType) {
			p.log.Warn("search: event carries no identity for the entity it would index — consumed, never indexed",
				"event_type", c.EventType, "events_in_log", c.Count)
		}
	}
	for _, c := range r.Void {
		if p.firstTime("void:" + c.EventType) {
			p.log.Warn("search: the entity an event names is not in the source tables — consumed, nothing indexed",
				"event_type", c.EventType, "events_in_log", c.Count)
		}
	}
	if r.Candidates == 0 {
		return
	}
	p.log.Info("search: projection pass",
		"candidates", r.Candidates, "projected", r.Projected, "failed", r.Failed,
		"unmapped_types", len(r.Unmapped), "unaddressed_types", len(r.Unaddressed),
		"void_types", len(r.Void))
}

// firstTime reports whether key is being reported for the first time.
func (p *Projector) firstTime(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.warned[key] {
		return false
	}
	p.warned[key] = true
	return true
}

// countsSorted renders a per-event-type tally as a stable slice.
func countsSorted(counts map[string]int) []EventTypeCount {
	if len(counts) == 0 {
		return nil
	}
	out := make([]EventTypeCount, 0, len(counts))
	for eventType, n := range counts {
		out = append(out, EventTypeCount{EventType: eventType, Count: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EventType < out[j].EventType })
	return out
}

// uuidText renders a uuid column value as text ("" for NULL).
func uuidText(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", u.Bytes[0:4], u.Bytes[4:6], u.Bytes[6:8], u.Bytes[8:10], u.Bytes[10:16])
}

// uuidParam parses a uuid text form into the pgx type — the same helper
// shape internal/persistence uses (textUUID), copied rather than shared
// because it is six lines and the alternative is exporting it from a
// package whose scope is the store's.
func uuidParam(s string) (pgtype.UUID, error) {
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		return pgtype.UUID{}, fmt.Errorf("invalid uuid %q: %w", s, err)
	}
	return u, nil
}
