package contribution

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LedgerStore is the Contribution Ledger projection's PostgreSQL adapter
// (docs/13 §1): it reads the append-only domain event log (research_events)
// and writes the ledger rows (contribution_events) the mapping table in
// ledger.go describes.
//
// Plain pgx, like OpportunityStore and for the same reason: these columns
// belong to this subsystem, and sqlc queries would put them on the shared
// persistence surface. The mapping decisions are NOT here — this file
// executes them and owns the two things a decision table cannot express:
// which events are candidates, and how the affiliation is resolved.
//
// # Idempotency is the database's, not this code's
//
// Every insert pins its source event and carries
// ON CONFLICT (research_event_id) WHERE research_event_id IS NOT NULL DO
// NOTHING against migration 00087's partial unique index — the shape 00046
// gave the outbox publish step. That is what makes re-running the
// projection harmless: two concurrent projectors, a restart mid-batch, a
// backfill over an already-projected log and a re-run by hand all converge
// on exactly one ledger row per event, and the loser of a race reports the
// event as a duplicate rather than failing. No lock, no claim, no
// bookkeeping row: the log is append-only and nothing here ever updates or
// deletes, so there is no shared mutable state to serialize on.
//
// # The candidate scan and the backlog count cannot disagree
//
// Both are built from ledgerCandidatePredicate — one definition of "this
// event owes a ledger row and has none" — so the number the report calls
// Pending is exactly the set the next pass would pick up. The scan also
// restricts itself to the types the mapping table names: their complement
// is not skipped but COUNTED (Unmapped), because an anti-join over the
// whole log would starve forever on the permanently unmappable events.
//
// # The affiliation is the one at event time
//
// docs/13 §1: a contribution event carries "affiliation at time", and
// 00011 named the column for exactly that (organization_id_at_time). It is
// resolved IN THE INSERT, from the membership rows that cover
// occurred_at's UTC date — an affiliation that starts later or ended
// earlier does not count, NULL start is "always was", NULL end is "still
// is" — with organization_id as the deterministic tie-break when a person
// is in several organizations at once. Because the insert is
// ON CONFLICT DO NOTHING, a re-run can never rewrite an existing row's
// affiliation: joining a new organization today cannot retroactively move
// a contribution made last year. That is the property the acceptance asks
// for, and it holds by construction rather than by a rule someone must
// remember.
//
// # No aggregate is computed here, and none may be
//
// The store counts EVENTS — candidates, duplicates, unmapped, pending —
// because the projection has to report its own coverage. It computes
// nothing about a person: there is no per-actor counter, no weight, no
// rank, and no derived score anywhere in this file (docs/13 §4, §6;
// CLAUDE.md §9 invariant 13). The counts it does report are properties of
// one projection pass over the log, not of anybody's reputation.
type LedgerStore struct {
	pool *pgxpool.Pool
}

// NewLedgerStore builds the projection's store on pool. The pool may be
// lazy (persistence.OpenLazy): the API keeps starting while PostgreSQL is
// down.
func NewLedgerStore(pool *pgxpool.Pool) *LedgerStore {
	return &LedgerStore{pool: pool}
}

// LedgerEventCount is one event type's count in a projection report.
type LedgerEventCount struct {
	// EventType is the event name (specs/events/event-types.yaml).
	EventType string
	// Count is how many such events the log holds.
	Count int64
}

// LedgerBatch is one projection pass's outcome.
//
// The four counts are cumulative over the WHOLE log, not deltas of this
// pass: research_events is append-only, so "how much of the log does the
// ledger not record" is a standing fact about the projection rather than a
// per-run number, and a delta would read as zero on a quiet run while the
// gap was still there.
type LedgerBatch struct {
	// Candidates is how many unprojected events the scan returned (at most
	// the requested limit).
	Candidates int
	// Projected is how many ledger rows this pass inserted.
	Projected int
	// Duplicates is how many candidates turned out to already have a row —
	// a concurrent projector, a re-run, or a hand-written row. It is not an
	// error: the ledger is still exactly one row per event.
	Duplicates int
	// Unmapped counts, per event type, the log's events whose type the
	// mapping table has no ledger row for. These are the events the
	// projection can never record, which is why they are reported rather
	// than left to be discovered by their absence.
	Unmapped []LedgerEventCount
	// Unattributable counts, per event type, the MAPPED events that carry no
	// actor and therefore cannot become ledger rows (actor_id is NOT NULL:
	// an unattributed act is not somebody's contribution).
	Unattributable []LedgerEventCount
	// Pending is how many mapped, attributable events still have no ledger
	// row after this pass — zero means the projection has caught up.
	Pending int64
}

// ProjectBatch runs one projection pass: it reads up to limit unprojected
// mapped events, matches each against the mapping table, inserts the ledger
// row it produces (idempotently), and reports the log's coverage.
//
// The pass is ONE transaction: the rows either all land or none do, and a
// retry re-reads the same candidates (nothing was marked as seen). A batch
// limit is a batching decision, never a correctness one — every event is
// projected by some pass, and the report's Pending says whether more
// remain.
func (s *LedgerStore) ProjectBatch(ctx context.Context, limit int) (LedgerBatch, error) {
	if limit <= 0 {
		return LedgerBatch{}, fmt.Errorf("%w: batch limit must be positive", ErrValidation)
	}
	mapped := MappedEventTypes()
	var batch LedgerBatch
	err := withTx(ctx, s.pool, func(tx pgx.Tx) error {
		candidates, err := ledgerCandidates(ctx, tx, mapped, limit)
		if err != nil {
			return err
		}
		batch.Candidates = len(candidates)
		for _, src := range candidates {
			row, ok := ProjectEvent(src)
			if !ok {
				// Unreachable: the scan is parameterised by the mapping
				// table. If it ever happens, the event stays a candidate and
				// the next pass reports it again — it is never dropped
				// silently.
				return fmt.Errorf("%w: %s has no ledger mapping but was scanned as a candidate",
					ErrStore, describeSource(src))
			}
			inserted, err := ledgerInsertRow(ctx, tx, row)
			if err != nil {
				return err
			}
			batch.Projected += inserted
			batch.Duplicates += 1 - inserted
		}
		if batch.Unmapped, err = ledgerUnmappedCounts(ctx, tx, mapped); err != nil {
			return err
		}
		if batch.Unattributable, err = ledgerUnattributableCounts(ctx, tx, mapped); err != nil {
			return err
		}
		return tx.QueryRow(ctx, ledgerPendingQuery, mapped).Scan(&batch.Pending)
	})
	if err != nil {
		return LedgerBatch{}, err
	}
	return batch, nil
}

// ledgerCandidatePredicate is the single definition of "this event owes a
// ledger row and has none": mapped type, an actor to credit, no row yet.
// The candidate scan and the pending count are both built from it, so the
// projection's backlog number cannot drift from what the projection would
// actually pick up.
const ledgerCandidatePredicate = `
  FROM research_events r
 WHERE r.event_type = ANY($1)
   AND r.actor_id IS NOT NULL
   AND NOT EXISTS (
         SELECT 1 FROM contribution_events c
          WHERE c.research_event_id = r.id)`

// ledgerCandidatesQuery reads the next batch of unprojected events, oldest
// first, so a backfill projects the log in the order the work happened
// (occurred_at, then id as the deterministic tie-break).
const ledgerCandidatesQuery = `
SELECT r.id, r.event_type, r.actor_id, r.project_id, r.via, r.occurred_at, r.payload` +
	ledgerCandidatePredicate + `
 ORDER BY r.occurred_at, r.id
 LIMIT $2`

// ledgerPendingQuery counts what the projection still owes.
const ledgerPendingQuery = `SELECT count(*)` + ledgerCandidatePredicate

// ledgerUnmappedQuery counts the log by event type for the events the
// mapping table does not cover — the visibility the projection owes the
// vocabulary (an event nobody projects must be a number someone can read,
// never a hole).
const ledgerUnmappedQuery = `
SELECT r.event_type, count(*)
  FROM research_events r
 WHERE NOT (r.event_type = ANY($1))
 GROUP BY r.event_type
 ORDER BY r.event_type`

// ledgerUnattributableQuery counts the mapped events that carry no actor:
// not projectable (contribution_events.actor_id is NOT NULL) and reported
// rather than skipped.
const ledgerUnattributableQuery = `
SELECT r.event_type, count(*)
  FROM research_events r
 WHERE r.event_type = ANY($1)
   AND r.actor_id IS NULL
 GROUP BY r.event_type
 ORDER BY r.event_type`

// ledgerInsertRow writes one ledger row, resolving the affiliation at
// event time in the same statement (see the store's header). It reports
// how many rows it inserted: 0 means the event already had a row (the
// partial unique index's conflict, absorbed by DO NOTHING).
//
// $2 is occurred_at's UTC date, computed in Go from the event's instant
// rather than from now(): the affiliation belongs to when the act
// happened, and a projection run must not be an input to its own output.
const ledgerInsertRowQuery = `
INSERT INTO contribution_events
    (actor_id, organization_id_at_time, project_id, event_type, role_codes,
     object_refs, accepted_context, released_context, occurred_at, via,
     research_event_id)
SELECT $1, (
         SELECT m.organization_id
           FROM organization_memberships m
          WHERE m.user_id = $1
            AND (m.affiliation_start IS NULL OR m.affiliation_start <= $2::date)
            AND (m.affiliation_end IS NULL OR m.affiliation_end >= $2::date)
          ORDER BY m.affiliation_start DESC NULLS LAST, m.organization_id
          LIMIT 1
       ), $3, $4, $5, $6, $7, $8, $9, $10, $11
ON CONFLICT (research_event_id) WHERE research_event_id IS NOT NULL DO NOTHING`

// ledgerCandidates reads the next batch of unprojected events.
func ledgerCandidates(ctx context.Context, tx pgx.Tx, mapped []string, limit int) ([]LedgerSource, error) {
	rows, err := tx.Query(ctx, ledgerCandidatesQuery, mapped, limit)
	if err != nil {
		return nil, fmt.Errorf("%w: scan ledger candidates: %v", ErrStore, err)
	}
	defer rows.Close()
	var out []LedgerSource
	for rows.Next() {
		var (
			src  LedgerSource
			id   pgtype.UUID
			act  pgtype.UUID
			proj pgtype.UUID
			via  *string
		)
		if err := rows.Scan(&id, &src.EventType, &act, &proj, &via, &src.OccurredAt, &src.Payload); err != nil {
			return nil, fmt.Errorf("%w: read ledger candidate: %v", ErrStore, err)
		}
		src.EventID = uuidText(id)
		// The predicate guarantees an actor; a NULL here would mean the
		// projection wrote a row naming nobody, which actor_id refuses.
		if !act.Valid {
			return nil, fmt.Errorf("%w: event %s is a candidate with no actor", ErrStore, src.EventID)
		}
		src.ActorID = uuidText(act)
		src.ProjectID = uuidTextOrEmpty(proj)
		if via != nil {
			src.Via = *via
		}
		out = append(out, src)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: iterate ledger candidates: %v", ErrStore, err)
	}
	return out, nil
}

// ledgerInsertRow inserts one projected row through the caller's
// transaction; it returns 1 when the row landed and 0 when the event
// already had one.
func ledgerInsertRow(ctx context.Context, tx pgx.Tx, row LedgerRow) (int, error) {
	actor, err := textUUID(row.ActorID)
	if err != nil {
		return 0, fmt.Errorf("%w: ledger actor: %v", ErrStore, err)
	}
	projectID, err := nullableUUID(row.ProjectID)
	if err != nil {
		return 0, fmt.Errorf("%w: ledger project: %v", ErrStore, err)
	}
	// object_refs is a jsonb array of "kind:value" strings. json.Marshal of
	// an empty slice would render "null" — RefTexts returns an empty
	// non-nil slice, which renders "[]", the column's own default and the
	// honest rendering of "this event named no entity".
	refs, err := json.Marshal(RefTexts(row.Refs))
	if err != nil {
		return 0, fmt.Errorf("%w: ledger object refs: %v", ErrStore, err)
	}
	sourceEvent, err := textUUID(row.SourceEventID)
	if err != nil {
		return 0, fmt.Errorf("%w: ledger source event: %v", ErrStore, err)
	}
	// The affiliation date is the event's UTC date: memberships carry
	// dates, the ledger carries instants, and choosing one zone and stating
	// it is the only way the two can be compared at all.
	affiliationDate := row.OccurredAt.UTC().Format("2006-01-02")
	tag, err := tx.Exec(ctx, ledgerInsertRowQuery,
		actor, affiliationDate, projectID, row.EventType, RoleCodes(row.RoleCodes),
		refs, row.AcceptedContext, row.ReleasedContext, row.OccurredAt,
		nullableText(row.Via), sourceEvent)
	if err != nil {
		return 0, fmt.Errorf("%w: insert ledger row for %s: %v", ErrStore, row.SourceEventID, err)
	}
	return int(tag.RowsAffected()), nil
}

// ledgerUnmappedCounts reads the per-type counts for the types the mapping
// table does not cover.
func ledgerUnmappedCounts(ctx context.Context, tx pgx.Tx, mapped []string) ([]LedgerEventCount, error) {
	return ledgerCounts(ctx, tx, ledgerUnmappedQuery, mapped)
}

// ledgerUnattributableCounts reads the per-type counts for mapped events
// with no actor.
func ledgerUnattributableCounts(ctx context.Context, tx pgx.Tx, mapped []string) ([]LedgerEventCount, error) {
	return ledgerCounts(ctx, tx, ledgerUnattributableQuery, mapped)
}

// ledgerCounts runs one of the two per-type count queries.
func ledgerCounts(ctx context.Context, tx pgx.Tx, query string, mapped []string) ([]LedgerEventCount, error) {
	rows, err := tx.Query(ctx, query, mapped)
	if err != nil {
		return nil, fmt.Errorf("%w: count ledger coverage: %v", ErrStore, err)
	}
	defer rows.Close()
	out := []LedgerEventCount{}
	for rows.Next() {
		var c LedgerEventCount
		if err := rows.Scan(&c.EventType, &c.Count); err != nil {
			return nil, fmt.Errorf("%w: read ledger coverage count: %v", ErrStore, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: iterate ledger coverage counts: %v", ErrStore, err)
	}
	return out, nil
}

// uuidTextOrEmpty renders a nullable scanned uuid ("" when absent).
func uuidTextOrEmpty(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuidText(u)
}
