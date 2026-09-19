package search

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// Rebuild reconstructs the projection from its sources (T0901).
//
// search_documents is a PROJECTION, not history: migration 00014's
// append-only guard deliberately excludes it, 00015 leaves it out of the
// truncate guard, and the integration suite pins that
// (tests/integration/append_only_truncate_test.go: TRUNCATE search_documents
// must succeed). So "throw it away and derive it again" is a legal
// operation, and it is the only operation that can repair the two things
// the incremental path cannot:
//
//   - an index that missed events while the projector was not running, and
//   - a document whose VISIBILITY inputs changed without an event of its
//     own. The projected visibility is a copy of the source's axes, so a
//     project that became private after a row was projected leaves that row
//     saying public until something re-projects it. Rebuild re-derives every
//     row from current state, which is what makes that repair possible
//     without a second consumer.
//
// # Why it walks the sources rather than the event log
//
// A rebuild that replayed research_events would produce whatever the events
// said at the time they happened — including the visibility the entity had
// THEN, which is exactly what it is meant to repair. It walks the current
// source rows instead, and it walks them through the SAME query and scan
// pairs the incremental path uses (sources.go), so a rebuilt index and an
// incrementally-updated one cannot disagree about what an entity indexes as.
//
// # Idempotence, and the one column that is not
//
// Running Rebuild twice leaves the same rows with the same values: it
// truncates and re-inserts in one transaction, and every document is
// derived from source rows rather than accumulated. updated_at is the
// exception by construction — UpsertSearchDocument stamps now() — which is
// why the acceptance compares the derived columns. embedding is NULL for
// every rebuilt row: the rebuild re-derives what this task projects, and
// vector retrieval (T0902) is the surface that will have to fill it again
// (a rebuild that dropped embeddings silently would be a retrieval
// regression, and it is recorded as a hand-off, not hidden).
//
// The whole rebuild is ONE transaction, so no reader ever sees the index
// emptied: the truncate and every insert commit together.
func (p *Projector) Rebuild(ctx context.Context) (RebuildReport, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return RebuildReport{}, fmt.Errorf("search: rebuild: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var removed int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM search_documents`).Scan(&removed); err != nil {
		return RebuildReport{}, fmt.Errorf("search: rebuild: count projected rows: %w", err)
	}
	if _, err := tx.Exec(ctx, `TRUNCATE search_documents`); err != nil {
		return RebuildReport{}, fmt.Errorf("search: rebuild: truncate: %w", err)
	}

	report := RebuildReport{Removed: removed, ByEntityType: map[string]int{}}
	for _, entityType := range rebuildableEntityTypes() {
		src, err := sourceFor(entityType)
		if err != nil {
			return RebuildReport{}, err
		}
		n, err := p.rebuildEntityType(ctx, tx, entityType, src)
		if err != nil {
			return RebuildReport{}, err
		}
		report.ByEntityType[entityType] = n
		report.Projected += n
	}

	if err := tx.Commit(ctx); err != nil {
		return RebuildReport{}, fmt.Errorf("search: rebuild: commit: %w", err)
	}
	p.log.Info("search: rebuild complete",
		"removed", report.Removed, "projected", report.Projected,
		"by_entity_type", report.EntityTypes())
	return report, nil
}

// RebuildReport is what one rebuild did.
type RebuildReport struct {
	// Removed is how many rows were in the projection before the rebuild.
	Removed int
	// Projected is how many rows were written.
	Projected int
	// ByEntityType is the per-entity-type count of the rows written.
	ByEntityType map[string]int
}

// EntityTypes renders the per-type counts as a stable "type=n" list for a
// log line or a CLI report.
func (r RebuildReport) EntityTypes() []string {
	out := make([]string, 0, len(r.ByEntityType))
	for entityType, n := range r.ByEntityType {
		out = append(out, fmt.Sprintf("%s=%d", entityType, n))
	}
	sort.Strings(out)
	return out
}

// rebuildEntityType projects every entity of one type through the source's
// `every` scan. A row the scan cannot build is an error, not a skip: unlike
// the event path — where an event may legitimately name an entity that is
// gone — a rebuild is reading the entities it is about to index, so a row it
// cannot turn into a document is a defect that must stop the rebuild rather
// than produce an index with a hole in it.
//
// Reading and writing are two separate steps over one entity type, and not
// for style: the whole rebuild is ONE transaction (so no reader ever sees
// the index emptied), and a single connection carries one statement at a
// time — issuing the upsert while the source cursor is still open fails with
// "conn busy". So the source is drained into memory first, and only then
// written. One entity type at a time is the unit that has to fit: the
// alternative, a second connection, would put the inserts outside the
// transaction that must not be observable half-done.
func (p *Projector) rebuildEntityType(ctx context.Context, tx pgx.Tx, entityType string, src documentSource) (int, error) {
	docs, err := readEveryDocument(ctx, tx, entityType, src)
	if err != nil {
		return 0, err
	}
	for _, doc := range docs {
		projectID, err := uuidParam(doc.ProjectID)
		if err != nil {
			return 0, fmt.Errorf("search: rebuild: %s: %w", doc.EntityRef, err)
		}
		if err := p.queries.WithTx(tx).UpsertSearchDocument(ctx, sqlc.UpsertSearchDocumentParams{
			EntityRef:  doc.EntityRef,
			EntityType: doc.EntityType,
			Visibility: doc.Visibility,
			ProjectID:  projectID,
			Title:      doc.Title,
			Content:    doc.Content,
			Structured: doc.Structured,
		}); err != nil {
			return 0, fmt.Errorf("search: rebuild: upsert %s: %w", doc.EntityRef, err)
		}
	}
	return len(docs), nil
}

// readEveryDocument drains one entity type's source scan, closing the cursor
// before the caller writes anything (see rebuildEntityType).
func readEveryDocument(ctx context.Context, tx pgx.Tx, entityType string, src documentSource) ([]Document, error) {
	rows, err := tx.Query(ctx, src.every)
	if err != nil {
		return nil, fmt.Errorf("search: rebuild: scan %s entities: %w", entityType, err)
	}
	defer rows.Close()

	var docs []Document
	for rows.Next() {
		doc, err := src.scan(rows)
		if err != nil {
			return nil, fmt.Errorf("search: rebuild: read a %s document: %w", entityType, err)
		}
		docs = append(docs, doc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search: rebuild: iterate %s entities: %w", entityType, err)
	}
	return docs, nil
}

// rebuildableEntityTypes returns the entity types a rebuild scans, sorted.
// It is derived from the rule table — the entity types of the rules, as a
// set — so a rule added without a source reader is caught by sourceFor
// rather than by a rebuild that silently misses a class of documents. The
// unit suite pins that this set and the readers' keys are the same set.
func rebuildableEntityTypes() []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(projectionRules))
	for _, r := range projectionRules {
		if !seen[r.EntityType] {
			seen[r.EntityType] = true
			out = append(out, r.EntityType)
		}
	}
	sort.Strings(out)
	return out
}
