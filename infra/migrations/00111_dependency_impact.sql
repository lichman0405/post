-- +goose Up
-- T1007. ONE index: the idempotency of the dependency-impact alert. No
-- table, no column, and — measured, not assumed — no second index. The
-- reason is the task's own subject.
--
-- docs/18 §5 (Dependency Watch) asks the platform to analyse the downstream
-- of an upstream change and create an alert, "只标记 review required，不自动
-- 改科学结论". An alert is therefore a MESSAGE, not a record with a
-- lifecycle: it has no acknowledgement to store, no assignee, no state to
-- transition. The message carrier already exists and is already registered
-- — `dependency.impact_detected` (specs/events/event-types.yaml:29) — and
-- the delivery pipeline that carries messages is built: the outbox (00012,
-- 00046), the fan-outs (00078) and the digest. A dependency_impacts table
-- would be a second copy of "what the log already says", with its own
-- write path, its own drift and its own retention question, and nothing in
-- the product would read it that cannot read the event log instead.
--
-- The idempotency, however, is the analysis's own problem and the schema is
-- where it has to be solved.

-- The analysis is a consumer of the event log, and it is the one consumer
-- whose output is derived from its input rather than willed by a caller:
-- for one trigger event it emits one `dependency.impact_detected` per
-- affected downstream, and the same upstream change replayed — a restarted
-- worker, a re-scan, a second process over the same log — must not produce
-- a second alert. The dedupe belongs in the DATABASE for the reason the
-- ledger projection's does (00087: "no lock, no claim, no bookkeeping
-- row"): the log is append-only, the analysis holds no mutable state, and
-- the only thing that has to agree between two concurrent analyses is the
-- row they write.
--
-- The key is (trigger event, affected entity): one alert per pair, ever.
-- It is read out of the payload — trigger_event_id names the research_events
-- row that caused the analysis, affected_kind/affected_id name the entity
-- the alert is about — because those three fields ARE the identity of the
-- alert (the payload's own definition, pinned by a test in
-- internal/application/dependencyimpact). A conflict on them is the replay,
-- and the insert's ON CONFLICT DO NOTHING turns it into a no-op rather than
-- an error.
--
-- The index is PARTIAL on the event type: a bare unique index over
-- payload->>'…' would constrain every other producer's outbox rows against
-- keys they do not carry (three NULLs are never a conflict in a unique
-- index, but a future producer that happened to carry these keys under a
-- different meaning would be silently deduplicated against an alert it has
-- nothing to do with).
CREATE UNIQUE INDEX outbox_events_dependency_impact_uniq
  ON outbox_events (
    event_type,
    (payload->>'trigger_event_id'),
    (payload->>'affected_kind'),
    (payload->>'affected_id')
  )
  WHERE event_type = 'dependency.impact_detected';

COMMENT ON INDEX outbox_events_dependency_impact_uniq IS
  'One dependency.impact_detected per (trigger event, affected entity), ever: the analysis is derived from the event log, so replaying it must be a no-op (T1007, docs/18 §5).';

-- What this migration deliberately does NOT add, and how that was checked.
--
-- The analysis reads two tables. Both directions already have the index the
-- read needs, and a second one would be a write cost for nothing:
--
--   * THE OBJECT SIDE. The walk is a recursive CTE over relation_versions,
--     which carries relation_versions_source_idx
--     (source_object_version_id, relation_type) and
--     relation_versions_target_idx (target_object_version_id,
--     relation_type) from 00006/00008 — exactly the two directions the
--     walk follows, and the second column is the relation-type filter
--     itself.
--
--   * THE ASSET SIDE. The analysis reads asset_dependencies in the
--     direction its primary key does not serve — asset_dependencies' PK is
--     (project_id, asset_version_id, dependency_type), which leads with the
--     project, so "which projects depend on this fixed version" cannot use
--     it. But that direction was already indexed: 00101 (T0808) added
--     asset_dependencies_version_idx ON asset_dependencies
--     (asset_version_id) for the asset page's used_by block, which asks
--     this very question. The analysis's read is the same probe over the
--     same column, so it uses the same index, and a covering variant
--     (asset_version_id, dependency_type) would buy nothing at this
--     cardinality while adding a second B-tree to every write to a table
--     the publisher writes on every publish. It was written, then removed
--     once 00101 turned out to be sitting there — which is what
--     TestFreshInstallCatalog's explicit-index inventory would have caught
--     either way.

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
