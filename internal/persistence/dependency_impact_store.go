package persistence

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/dependencyimpact"
	"github.com/lichman0405/post/internal/events"
)

// DependencyImpactStore is the dependency impact analysis's PostgreSQL
// adapter: the candidate scan, the dependency walk and the idempotent alert
// write. It implements dependencyimpact.Store.
//
// Plain pgx, not sqlc, for the same reason internal/persistence/queries/
// rsg_query.sql's header records for ListStateLineage: sqlc v1.31.1's
// analyzer cannot resolve a recursive CTE's self-reference and rejects the
// valid PostgreSQL. The scan and the two subject lookups are plain pgx too,
// because they are parameterised by arrays the relation catalog produces at
// runtime (relation_type = ANY($n)) and by jsonb payload fields, neither of
// which a generated query would express better.
//
// # Where the decisions are
//
// Not here. Which event types trigger an analysis
// (dependencyimpact.ResolveSubject), which edges are walked
// (dependencyimpact.DependentTypes, read from the relation catalog), what a
// change reaches (dependencyimpact.AnalyzeSubject, run below over a
// transaction-bound analyzer) and what an alert says
// (dependencyimpact.AlertEvent) are all in the application package. This file
// executes them: it owns the candidate predicate, the walk's SQL and the
// write's transaction, and nothing else.
//
// # The candidate predicate is shared, and it drains
//
// The scan, the pending count and the no-dependents count are built from ONE
// predicate (dependencyImpactCandidatePredicate), so the backlog the report
// prints cannot drift from the set the next pass would pick up — the rule
// internal/contribution/ledger_store.go states for the ledger's cursor-free
// projection.
//
// That predicate carries a condition the ledger's does not, and it is the one
// thing about this scan that is subtle: a trigger event is a candidate only if
// its subject HAS AT LEAST ONE DEPENDENT. Without it, a trigger that yields no
// alerts would stay a candidate forever — it owes no alert row, so nothing
// removes it from the window — and a project's ordinary
// `scientific_object.version_created` events (nobody depends on most objects)
// would fill the batch window and starve every newer trigger. The condition is
// monotone: relation_versions is append-only and asset_dependencies rows are
// never deleted, so once a subject has a dependent it keeps having one, and a
// subject that has none yet becomes eligible the moment one appears. What it
// excludes is therefore not lost work but work with no downstream to analyse —
// and it is COUNTED, never silently dropped
// (dependencyImpactNoDependentQuery).
//
// # Idempotency is the database's, not this code's
//
// Alerts are written through events.RecordIdempotent, whose INSERT carries ON
// CONFLICT DO NOTHING against migration 00111's partial unique index on
// (event_type, payload->>'trigger_event_id', payload->>'affected_kind',
// payload->>'affected_id'). Two concurrent analyses, a restart mid-batch and a
// re-run by hand therefore converge on exactly one alert per (trigger,
// affected) pair, and the loser reports the pair as a duplicate rather than
// failing. No lock, no claim, no bookkeeping row: the log is append-only and
// nothing here ever updates or deletes, so there is no shared mutable state to
// serialize on.
//
// # Nothing here writes anything but an alert
//
// The store has no statement that touches scientific_objects,
// scientific_object_versions, project_states, releases, projects or reviews —
// which is what docs/11 §7's 「impact 是提示，不是状态迁移」 means in practice,
// and what the acceptance's read-back assertion checks from the other side.
type DependencyImpactStore struct {
	pool *pgxpool.Pool
}

// NewDependencyImpactStore builds the analysis's store on pool. The pool may
// be lazy (persistence.OpenLazy): the worker keeps starting while PostgreSQL
// is down, and the pass reports the outage per attempt.
func NewDependencyImpactStore(pool *pgxpool.Pool) *DependencyImpactStore {
	return &DependencyImpactStore{pool: pool}
}

var _ dependencyimpact.Store = (*DependencyImpactStore)(nil)

// safeUUIDExpr renders the SQL that reads a uuid out of the payload field
// `field` of the research_events row aliased `alias`, without ever raising on
// a malformed value: casting junk text to uuid is an ERROR in PostgreSQL, and
// one unparseable payload must not abort the whole scan (it is one event, not
// the query).
//
// pg_input_is_valid is PostgreSQL 16's (docker-compose.yml and CI both run
// pgvector/pgvector:0.8.6-pg16), which is why the guard is a function call
// rather than a regular expression that would also have to guess which uuid
// spellings the server accepts. A NULL result means "this payload names no
// such id", which is what every caller below tests for — and it is what keeps
// the scans' behaviour identical to the Go side's
// dependencyimpact.ResolveSubject, which rejects the same payloads.
func safeUUIDExpr(alias, field string) string {
	return "(CASE WHEN pg_input_is_valid(" + alias + ".payload->>'" + field + "', 'uuid')" +
		" THEN (" + alias + ".payload->>'" + field + "')::uuid END)"
}

// dependencyEdgeSQL is the dependency edge, at OBJECT level: every pair
// (dependent object, dependency object) that a dependency-marked relation
// version establishes.
//
// Both ends are resolved through scientific_object_versions because the stored
// edge is version-pinned while the walk is over objects (the application
// package's comment records why: docs/18 §5's "new version" trigger could
// never fire on a version-pinned walk). The two branches are the two
// directions the catalog declares, written as two SELECTs rather than one CASE
// so that each uses its own index — relation_versions_target_idx for the
// first, relation_versions_source_idx for the second (both from 00006/00008).
// A CASE over the whole table would read every dependency row to answer a
// question about one object.
//
// $2 is the types whose DEPENDENT end is the edge's source, $3 the types whose
// dependent end is the target (dependencyimpact.DependentTypes, read from the
// relation catalog). No relation type name is spelled here.
const dependencyEdgeSQL = `
  SELECT sv.object_id AS dependent_object, tv.object_id AS dependency_object
    FROM relation_versions rv
    JOIN scientific_object_versions sv ON sv.id = rv.source_object_version_id
    JOIN scientific_object_versions tv ON tv.id = rv.target_object_version_id
   WHERE rv.relation_type = ANY($2)
  UNION ALL
  SELECT tv.object_id, sv.object_id
    FROM relation_versions rv
    JOIN scientific_object_versions sv ON sv.id = rv.source_object_version_id
    JOIN scientific_object_versions tv ON tv.id = rv.target_object_version_id
   WHERE rv.relation_type = ANY($3)`

// dependencyWalkSQL is the walk: the object-level closure of one upstream
// object, with each dependent's hop count and the truncation flag.
//
// UNION, not UNION ALL, in the recursive CTE, and the difference is a
// correctness property as well as a cost one. UNION ALL would expand every
// simple path through the graph, which in a densely connected project is
// exponential in the depth cap; UNION collapses duplicate (object, hops) rows,
// so the walk can hold at most |objects| × MaxHops rows however tangled the
// graph is — and a cycle (two protocols that depend on each other, which is
// legal in this repository) is absorbed by it rather than needing a path
// array. min(hops) then gives each dependent its true shortest distance, which
// is the definition of direct/indirect, and bool_or(hops >= $4) reports that
// some path reached the cap: a truncated answer says so.
//
// The subject itself is excluded from the result. It is the thing that
// CHANGED, not a thing the change reaches; in a cycle it would otherwise come
// back as its own dependent.
//
// The join to scientific_objects is what makes the answer renderable — an
// object_type and the project that owns each dependent — and it is an inner
// join on purpose: a scientific_object_versions row whose object is gone
// (which the append-only model does not produce) names a dependent nothing can
// route an alert about, so it is not reported as one.
const dependencyWalkSQL = `
WITH RECURSIVE walk(object_id, hops) AS (
  SELECT e.dependent_object, 1
    FROM (` + dependencyEdgeSQL + `) e
   WHERE e.dependency_object = $1
  UNION
  SELECT e.dependent_object, w.hops + 1
    FROM walk w
    JOIN (` + dependencyEdgeSQL + `) e ON e.dependency_object = w.object_id
   WHERE w.hops < $4
)
SELECT w.object_id, min(w.hops), bool_or(w.hops >= $4),
       so.object_type, so.project_id
  FROM walk w
  JOIN scientific_objects so ON so.id = w.object_id
 WHERE w.object_id <> $1
 GROUP BY w.object_id, so.object_type, so.project_id
 ORDER BY so.project_id, w.object_id`

// assetDependentsSQL reads the projects that pin one asset version, with the
// visibility of each declaration. $2 is the dependency types whose catalog
// flag says an upstream change triggers re-analysis — so an asset_dependencies
// row of a type that does NOT (a citation) is not read at all, which is the
// flag deciding rather than the table's name.
//
// The read is a probe by asset_version_id, which asset_dependencies' primary
// key (project_id, asset_version_id, dependency_type) cannot serve — it leads
// with the project — and asset_dependencies_version_idx (00101) does. No index
// of this task's own is needed for it, and 00111 adds none.
const assetDependentsSQL = `
SELECT ad.project_id, ad.visibility_of_usage
  FROM asset_dependencies ad
 WHERE ad.asset_version_id = $1
   AND ad.dependency_type = ANY($2)
 ORDER BY ad.project_id`

// relationTypes renders a catalog-produced relation-type list for a
// `relation_type = ANY($n)` parameter.
//
// It is []string, and it is NOT textUUIDs — which is what this file called
// until an integration test showed every walk returning an empty closure.
// textUUIDs parses its input as uuids and SKIPS whatever it cannot parse
// (rsg_query_store.go: "unparseable ids can never match a row"), which is
// right for the id lists it was written for and silently wrong here: these
// values are relation TYPE NAMES ("depends_on", "used_by"), so every entry
// was dropped, the parameter arrived as an empty array, `= ANY('{}')` matched
// no row, and the walk answered "nothing depends on this" for a graph full of
// dependents — with no error anywhere, because an empty array is a perfectly
// valid thing to compare against.
//
// The non-nil return is the same trap one step further out: a nil slice
// travels as NULL, and `relation_type = ANY(NULL)` is NULL rather than true,
// which filters every row just as quietly. An empty array is an answer; NULL
// is not.
func relationTypes(types []string) []string {
	if types == nil {
		return []string{}
	}
	return types
}

// subjectObjectSQL resolves an object subject to its project and that
// project's visibility. The object's project is the read gate that decides
// whether a caller may be told anything about it at all.
const subjectObjectSQL = `
SELECT so.project_id, p.visibility
  FROM scientific_objects so
  JOIN projects p ON p.id = so.project_id
 WHERE so.id = $1`

// subjectAssetSQL resolves an asset-version subject to the ORIGIN project's id
// and visibility (research_assets.origin_project_id, NOT NULL since 00010).
// The origin project is the right gate because it is the project whose read
// gate serves the asset's own page — the rule assets.mayLinkVersion states as
// "the version itself must be public, and its asset's project must be public
// too".
const subjectAssetSQL = `
SELECT ra.origin_project_id, p.visibility
  FROM research_asset_versions rav
  JOIN research_assets ra ON ra.id = rav.asset_id
  JOIN projects p ON p.id = ra.origin_project_id
 WHERE rav.id = $1`

// subjectQueries is the per-kind resolution above, keyed by subject kind. The
// resolution is driven by it, and the package's own test pins its keys to
// dependencyimpact.KnownSubjectKinds: a trigger whose subject kind has no
// statement here would be a trigger the analysis can never resolve — the same
// hole the trigger table's own test guards from the other side.
var subjectQueries = map[dependencyimpact.SubjectKind]string{
	dependencyimpact.SubjectObject:       subjectObjectSQL,
	dependencyimpact.SubjectAssetVersion: subjectAssetSQL,
}

// objectClosure implements dependencyimpact.SubjectAnalyzer over any
// transaction surface, so the worker's pass runs the very walk a reader does.
func (s *DependencyImpactStore) objectClosure(ctx context.Context, q DBTX, objectID string) (dependencyimpact.Analysis, error) {
	subject := dependencyimpact.Subject{Kind: dependencyimpact.SubjectObject, ID: objectID}
	id, err := textUUID(objectID)
	if err != nil {
		// An unparseable id names no object, which is the same answer as an
		// object with no dependents: an empty closure, never an error (the
		// lineage walk's rule for the same situation).
		return dependencyimpact.Analysis{Subject: subject}, nil
	}
	rows, err := q.Query(ctx, dependencyWalkSQL, id,
		relationTypes(dependencyimpact.DependentTypes(dependencyimpact.EndpointSource)),
		relationTypes(dependencyimpact.DependentTypes(dependencyimpact.EndpointTarget)),
		dependencyimpact.MaxHops)
	if err != nil {
		return dependencyimpact.Analysis{}, fmt.Errorf("%w: dependency walk: %v", dependencyimpact.ErrStore, err)
	}
	defer rows.Close()
	out := dependencyimpact.Analysis{Subject: subject}
	for rows.Next() {
		var (
			objID      pgtype.UUID
			hops       int
			hitCap     bool
			objectType string
			projectID  pgtype.UUID
		)
		if err := rows.Scan(&objID, &hops, &hitCap, &objectType, &projectID); err != nil {
			return dependencyimpact.Analysis{}, fmt.Errorf("%w: read dependency walk row: %v", dependencyimpact.ErrStore, err)
		}
		text := pgUUIDToText(objID)
		out.Impacts = append(out.Impacts, dependencyimpact.Impact{
			Kind:       dependencyimpact.AffectedObject,
			ID:         text,
			ProjectID:  pgUUIDToText(projectID),
			ObjectID:   text,
			ObjectType: objectType,
			Hops:       hops,
			Directness: dependencyimpact.DirectnessOf(hops),
		})
		out.HitDepthCap = out.HitDepthCap || hitCap
	}
	if err := rows.Err(); err != nil {
		return dependencyimpact.Analysis{}, fmt.Errorf("%w: iterate dependency walk: %v", dependencyimpact.ErrStore, err)
	}
	return out, nil
}

// ObjectClosure implements dependencyimpact.Store on the pool.
func (s *DependencyImpactStore) ObjectClosure(ctx context.Context, objectID string) (dependencyimpact.Analysis, error) {
	return s.objectClosure(ctx, s.pool, objectID)
}

// assetDependents implements dependencyimpact.SubjectAnalyzer over any
// transaction surface.
func (s *DependencyImpactStore) assetDependents(ctx context.Context, q DBTX, assetVersionID string) ([]dependencyimpact.AssetDependent, error) {
	id, err := textUUID(assetVersionID)
	if err != nil {
		return nil, nil
	}
	rows, err := q.Query(ctx, assetDependentsSQL, id, relationTypes(dependencyimpact.AssetDependencyTypes()))
	if err != nil {
		return nil, fmt.Errorf("%w: read asset dependents: %v", dependencyimpact.ErrStore, err)
	}
	defer rows.Close()
	out := []dependencyimpact.AssetDependent{}
	for rows.Next() {
		var (
			projectID pgtype.UUID
			usage     string
		)
		if err := rows.Scan(&projectID, &usage); err != nil {
			return nil, fmt.Errorf("%w: read asset dependent row: %v", dependencyimpact.ErrStore, err)
		}
		out = append(out, dependencyimpact.AssetDependent{
			ProjectID:         pgUUIDToText(projectID),
			VisibilityOfUsage: usage,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: iterate asset dependents: %v", dependencyimpact.ErrStore, err)
	}
	return out, nil
}

// AssetDependents implements dependencyimpact.Store on the pool.
func (s *DependencyImpactStore) AssetDependents(ctx context.Context, assetVersionID string) ([]dependencyimpact.AssetDependent, error) {
	return s.assetDependents(ctx, s.pool, assetVersionID)
}

// subjectInfo implements the subject resolution over any transaction surface.
// A subject that names no row of its kind answers ErrSubjectNotFound; so does
// an id that is not a uuid, because a malformed id names nothing — which is
// the answer the read surface needs and not an adapter failure.
func (s *DependencyImpactStore) subjectInfo(ctx context.Context, q DBTX, subject dependencyimpact.Subject) (dependencyimpact.SubjectInfo, error) {
	query, ok := subjectQueries[subject.Kind]
	if !ok {
		return dependencyimpact.SubjectInfo{}, fmt.Errorf("%w: unknown subject kind %q",
			dependencyimpact.ErrValidation, subject.Kind)
	}
	id, err := textUUID(subject.ID)
	if err != nil {
		return dependencyimpact.SubjectInfo{}, fmt.Errorf("%w: %s", dependencyimpact.ErrSubjectNotFound, subject)
	}
	var (
		projectID  pgtype.UUID
		visibility string
	)
	if err := q.QueryRow(ctx, query, id).Scan(&projectID, &visibility); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return dependencyimpact.SubjectInfo{}, fmt.Errorf("%w: %s", dependencyimpact.ErrSubjectNotFound, subject)
		}
		return dependencyimpact.SubjectInfo{}, fmt.Errorf("%w: resolve subject %s: %v", dependencyimpact.ErrStore, subject, err)
	}
	return dependencyimpact.SubjectInfo{ProjectID: pgUUIDToText(projectID), Visibility: visibility}, nil
}

// SubjectInfo implements dependencyimpact.Store on the pool.
func (s *DependencyImpactStore) SubjectInfo(ctx context.Context, subject dependencyimpact.Subject) (dependencyimpact.SubjectInfo, error) {
	return s.subjectInfo(ctx, s.pool, subject)
}

// txAnalyzer is the walk's read surface inside an open transaction. Both
// methods are the store's, bound to the transaction, so a pass runs exactly
// the walk a reader runs — dependencyimpact.AnalyzeSubject over this type is
// the same call the service makes, which is what keeps an alert and a reader's
// answer from disagreeing about the same change.
type txAnalyzer struct {
	store *DependencyImpactStore
	tx    DBTX
}

func (a txAnalyzer) ObjectClosure(ctx context.Context, objectID string) (dependencyimpact.Analysis, error) {
	return a.store.objectClosure(ctx, a.tx, objectID)
}

func (a txAnalyzer) AssetDependents(ctx context.Context, assetVersionID string) ([]dependencyimpact.AssetDependent, error) {
	return a.store.assetDependents(ctx, a.tx, assetVersionID)
}

// dependencyImpactCandidatePredicate is the single definition of "this trigger
// event owes an analysis and has not had one": it is a type the trigger table
// covers, no alert names it yet, and its subject has at least one dependent.
//
// $1 trigger types, $2 object trigger types, $3 dependent-is-source dependency
// types, $4 dependent-is-target dependency types, $5 asset trigger types, $6
// asset dependency types.
//
// The shape tests ($2/$5, which payload field names the subject) are the
// trigger table's own declaration read back in SQL
// (dependencyimpact.ObjectTriggerTypes/AssetTriggerTypes), so an event whose
// type says "object" but whose payload carries no object_id is excluded by the
// same rule that would make the Go resolver refuse it — the two halves of the
// analysis agree on what a trigger event looks like rather than the SQL
// guessing at the Go side's shape.
var dependencyImpactCandidatePredicate = `
  FROM research_events re
 WHERE re.event_type = ANY($1)
   AND NOT EXISTS (
         SELECT 1 FROM outbox_events oe
          WHERE oe.event_type = '` + dependencyimpact.EventImpactDetected + `'
            AND oe.payload->>'` + dependencyimpact.AlertFieldTriggerEventID + `' = re.id::text)
   AND (
         (re.event_type = ANY($2) AND (
              EXISTS (
                SELECT 1 FROM relation_versions rv
                  JOIN scientific_object_versions tv ON tv.id = rv.target_object_version_id
                 WHERE rv.relation_type = ANY($3)
                   AND tv.object_id = ` + safeUUIDExpr("re", "object_id") + `)
           OR EXISTS (
                SELECT 1 FROM relation_versions rv
                  JOIN scientific_object_versions sv ON sv.id = rv.source_object_version_id
                 WHERE rv.relation_type = ANY($4)
                   AND sv.object_id = ` + safeUUIDExpr("re", "object_id") + `)))
      OR (re.event_type = ANY($5) AND EXISTS (
              SELECT 1 FROM asset_dependencies ad
               WHERE ad.asset_version_id = ` + safeUUIDExpr("re", "asset_version_id") + `
                 AND ad.dependency_type = ANY($6)))
       )`

// dependencyImpactCandidatesQuery reads the next batch of unanalysed triggers,
// oldest first, so a backlog built while the worker was down is analysed in
// the order the changes happened (occurred_at, then id as the deterministic
// tie-break).
var dependencyImpactCandidatesQuery = `
SELECT re.id, re.event_type, re.payload, re.occurred_at, re.actor_id, re.project_id,
       re.visibility, re.correlation_id` + dependencyImpactCandidatePredicate + `
 ORDER BY re.occurred_at, re.id
 LIMIT $7`

// dependencyImpactPendingQuery counts what the analysis still owes: the same
// predicate without the limit, which is what makes "pending" mean "what the
// next pass will pick up".
var dependencyImpactPendingQuery = `SELECT count(*)` + dependencyImpactCandidatePredicate

// dependencyImpactUnresolvableQuery counts, per event type, the trigger events
// whose payload names no subject this analysis can resolve. They are not
// candidates and never will be, so they are REPORTED rather than left to be
// discovered by their absence — the ledger's unmapped-type rule.
var dependencyImpactUnresolvableQuery = `
SELECT re.event_type, count(*)
  FROM research_events re
 WHERE re.event_type = ANY($1)
   AND NOT (
         (re.event_type = ANY($2) AND pg_input_is_valid(re.payload->>'object_id', 'uuid'))
      OR (re.event_type = ANY($3) AND pg_input_is_valid(re.payload->>'asset_version_id', 'uuid')))
 GROUP BY re.event_type
 ORDER BY re.event_type`

// dependencyImpactNoDependentQuery counts, per event type, the trigger events
// that DO name a resolvable subject and have nothing downstream of it. They are
// not candidates either — there is nothing to analyse — and this count is where
// that is visible: without it, "the analysis never looked at this" and "the
// analysis looked and found nobody" would be the same silence.
//
// It is the candidate predicate's own condition, negated: the same EXISTS
// tests, the same parameter arrays, so the two answer one question from
// opposite sides and cannot disagree about which events they are counting.
var dependencyImpactNoDependentQuery = `
SELECT re.event_type, count(*)
  FROM research_events re
 WHERE re.event_type = ANY($1)
   AND NOT EXISTS (
         SELECT 1 FROM outbox_events oe
          WHERE oe.event_type = '` + dependencyimpact.EventImpactDetected + `'
            AND oe.payload->>'` + dependencyimpact.AlertFieldTriggerEventID + `' = re.id::text)
   AND NOT (
         (re.event_type = ANY($2) AND (
              EXISTS (
                SELECT 1 FROM relation_versions rv
                  JOIN scientific_object_versions tv ON tv.id = rv.target_object_version_id
                 WHERE rv.relation_type = ANY($3)
                   AND tv.object_id = ` + safeUUIDExpr("re", "object_id") + `)
           OR EXISTS (
                SELECT 1 FROM relation_versions rv
                  JOIN scientific_object_versions sv ON sv.id = rv.source_object_version_id
                 WHERE rv.relation_type = ANY($4)
                   AND sv.object_id = ` + safeUUIDExpr("re", "object_id") + `)))
      OR (re.event_type = ANY($5) AND EXISTS (
              SELECT 1 FROM asset_dependencies ad
               WHERE ad.asset_version_id = ` + safeUUIDExpr("re", "asset_version_id") + `
                 AND ad.dependency_type = ANY($6))))
   AND (
         (re.event_type = ANY($2) AND pg_input_is_valid(re.payload->>'object_id', 'uuid'))
      OR (re.event_type = ANY($3) AND pg_input_is_valid(re.payload->>'asset_version_id', 'uuid')))
 GROUP BY re.event_type
 ORDER BY re.event_type`

// AnalyzeBatch implements dependencyimpact.Store: one analysis pass, in ONE
// transaction.
//
// The transaction is what makes a pass atomic against a crash — either every
// alert of the batch is written, or none is and the next pass re-reads the
// same candidates (nothing was marked as seen, so a retry re-derives rather
// than loses). It is not what makes the pass idempotent: 00111's partial unique
// index does that, and it holds across processes and across restarts, which a
// transaction cannot.
//
// The pass walks its subjects with dependencyimpact.AnalyzeSubject over the
// transaction — the same call the service and the read surface make — so an
// alert can never be about a different set of dependents than the answer a
// reader gets for the same change.
func (s *DependencyImpactStore) AnalyzeBatch(ctx context.Context, limit int) (dependencyimpact.Batch, error) {
	if limit <= 0 {
		return dependencyimpact.Batch{}, fmt.Errorf("%w: batch limit must be positive", dependencyimpact.ErrValidation)
	}
	objectDependents := dependencyimpact.DependentTypes(dependencyimpact.EndpointSource)
	objectDependency := dependencyimpact.DependentTypes(dependencyimpact.EndpointTarget)
	assetTypes := dependencyimpact.AssetDependencyTypes()
	var batch dependencyimpact.Batch
	err := WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		triggers, err := s.candidates(ctx, tx, limit)
		if err != nil {
			return err
		}
		batch.Candidates = len(triggers)
		analyzer := txAnalyzer{store: s, tx: tx}
		for _, t := range triggers {
			info, err := s.subjectInfo(ctx, tx, t.Subject)
			if err != nil {
				return err
			}
			analysis, err := dependencyimpact.AnalyzeSubject(ctx, analyzer, t.Subject)
			if err != nil {
				return err
			}
			if analysis.HitDepthCap {
				batch.HitDepthCap++
			}
			for _, imp := range analysis.Impacts {
				ev, err := dependencyimpact.AlertEvent(t, info.ProjectID, imp)
				if err != nil {
					return err
				}
				inserted, err := events.RecordIdempotent(ctx, tx, ev)
				if err != nil {
					return fmt.Errorf("%w: record impact alert: %v", dependencyimpact.ErrStore, err)
				}
				if inserted {
					batch.Emitted++
				} else {
					batch.Duplicates++
				}
			}
		}
		if batch.Unresolvable, err = s.coverageCounts(ctx, tx, dependencyImpactUnresolvableQuery,
			dependencyimpact.TriggerTypes(),
			dependencyimpact.ObjectTriggerTypes(),
			dependencyimpact.AssetTriggerTypes()); err != nil {
			return err
		}
		if batch.NoDependents, err = s.coverageCounts(ctx, tx, dependencyImpactNoDependentQuery,
			dependencyimpact.TriggerTypes(), dependencyimpact.ObjectTriggerTypes(),
			objectDependents, objectDependency, dependencyimpact.AssetTriggerTypes(),
			assetTypes); err != nil {
			return err
		}
		return tx.QueryRow(ctx, dependencyImpactPendingQuery,
			dependencyimpact.TriggerTypes(), dependencyimpact.ObjectTriggerTypes(),
			objectDependents, objectDependency, dependencyimpact.AssetTriggerTypes(),
			assetTypes).Scan(&batch.Pending)
	})
	if err != nil {
		return dependencyimpact.Batch{}, err
	}
	return batch, nil
}

// candidates reads the next batch of triggers and resolves each one's subject
// with the same function the rest of the platform uses
// (dependencyimpact.ResolveSubject), so an event the scan selects is an event
// the analysis can act on. A row whose payload the resolver refuses is skipped
// — the scan cannot select one (its shape test is the resolver's own), so
// reaching that branch would mean the two definitions have drifted, and the
// pass's coverage counts are where that would show.
func (s *DependencyImpactStore) candidates(ctx context.Context, q DBTX, limit int) ([]dependencyimpact.Trigger, error) {
	rows, err := q.Query(ctx, dependencyImpactCandidatesQuery,
		dependencyimpact.TriggerTypes(),
		dependencyimpact.ObjectTriggerTypes(),
		dependencyimpact.DependentTypes(dependencyimpact.EndpointSource),
		dependencyimpact.DependentTypes(dependencyimpact.EndpointTarget),
		dependencyimpact.AssetTriggerTypes(),
		dependencyimpact.AssetDependencyTypes(),
		limit)
	if err != nil {
		return nil, fmt.Errorf("%w: read trigger candidates: %v", dependencyimpact.ErrStore, err)
	}
	defer rows.Close()
	var out []dependencyimpact.Trigger
	for rows.Next() {
		var (
			id         pgtype.UUID
			eventType  string
			payload    []byte
			occurredAt pgtype.Timestamptz
			actorID    pgtype.Text
			projectID  pgtype.UUID
			visibility string
			correlID   pgtype.Text
		)
		if err := rows.Scan(&id, &eventType, &payload, &occurredAt, &actorID, &projectID, &visibility, &correlID); err != nil {
			return nil, fmt.Errorf("%w: read trigger candidate row: %v", dependencyimpact.ErrStore, err)
		}
		subject, ok := dependencyimpact.ResolveSubject(eventType, payload)
		if !ok {
			continue
		}
		out = append(out, dependencyimpact.Trigger{
			EventID:       pgUUIDToText(id),
			EventType:     eventType,
			OccurredAt:    occurredAt.Time,
			ActorID:       actorID.String,
			ProjectID:     pgUUIDToText(projectID),
			Visibility:    visibility,
			CorrelationID: correlID.String,
			Subject:       subject,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: iterate trigger candidates: %v", dependencyimpact.ErrStore, err)
	}
	return out, nil
}

// coverageCounts runs one of the two per-type coverage queries: a grouped
// count whose rows are the report's own statement of what the analysis saw and
// did not act on. Both queries share this shape, so neither can grow a
// silently different result shape from the other.
func (s *DependencyImpactStore) coverageCounts(ctx context.Context, q DBTX, query string, args ...any) ([]dependencyimpact.EventCount, error) {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%w: read coverage counts: %v", dependencyimpact.ErrStore, err)
	}
	defer rows.Close()
	var out []dependencyimpact.EventCount
	for rows.Next() {
		var c dependencyimpact.EventCount
		if err := rows.Scan(&c.EventType, &c.Count); err != nil {
			return nil, fmt.Errorf("%w: read coverage count row: %v", dependencyimpact.ErrStore, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: iterate coverage counts: %v", dependencyimpact.ErrStore, err)
	}
	return out, nil
}
