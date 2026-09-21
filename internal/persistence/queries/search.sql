-- Search projection (canonical table: search_documents). Rebuildable by
-- construction (docs/14: Postgres FTS + structured filters, and since T0902
-- pgvector embeddings as well; OpenSearch explicitly post-V1).
--
-- Two writers, one row each, and they do not overlap: T0901's projection
-- owns the document (entity_type, visibility, project_id, title, content,
-- structured), T0902's batch embedding job owns the vector and its
-- provenance (embedding, embedding_provider, embedding_model,
-- embedding_version). The visibility filter below is T0901's and is
-- untouched by the embedding work: a vector is an attribute of a row that
-- was already access-filtered, not a second way in.

-- name: UpsertSearchDocument :exec
INSERT INTO search_documents (entity_ref, entity_type, visibility, project_id, title, content, structured)
VALUES (@entity_ref, @entity_type, @visibility, @project_id, @title, @content, @structured)
ON CONFLICT (entity_ref) DO UPDATE SET
    entity_type = EXCLUDED.entity_type,
    visibility  = EXCLUDED.visibility,
    project_id  = EXCLUDED.project_id,
    title       = EXCLUDED.title,
    content     = EXCLUDED.content,
    structured  = EXCLUDED.structured,
    updated_at  = now();

-- name: SearchDocuments :many
--
-- Access control is enforced HERE, not delegated to a caller.
--
-- The query returns a row only if it is public, or if its project is
-- explicitly listed in allowed_project_ids. Passing an empty array therefore
-- yields public rows only — never the whole table. That property is the point:
-- a query that cannot even accept an actor's scope cannot enforce one, and the
-- previous unfiltered form returned every matching row regardless of
-- visibility.
--
-- docs/54 ranks "private project/branch content appearing in Search" as its
-- top-severity scenario, and docs/23 §5 requires tenant/project/object policy
-- filtering on every query, search, export and download. Master Gate E ("Search
-- 无 private leakage") holds this invariant too.
SELECT entity_ref, entity_type, visibility, project_id, title, content, structured, updated_at,
       ts_rank(to_tsvector('simple', title || ' ' || content), plainto_tsquery('simple', @query)) AS rank
FROM search_documents
WHERE to_tsvector('simple', title || ' ' || content) @@ plainto_tsquery('simple', @query)
  AND (visibility = 'public' OR project_id = ANY(@allowed_project_ids::uuid[]))
ORDER BY rank DESC, entity_ref
LIMIT @page_size OFFSET @page_offset;

-- name: SearchDocumentsNeedingEmbedding :many
--
-- The embedding backlog: the documents whose stored vector is not the one
-- the CURRENT model would produce. That is exactly two states, and they are
-- one predicate — the vector is missing, or its provenance is not this
-- model's (T0902, internal/search/embedding). A stable ORDER BY makes a
-- batch resumable and a run reproducible: two passes over the same backlog
-- select the same rows, so "the same input twice" is not a coincidence of
-- the planner.
--
-- This is a READ of rows that are about to be written, not a claim on them:
-- no row lock is taken and none is held while the embedder runs (see the
-- batch job for why). Two batch jobs running at once may therefore select
-- the same rows and write the same values — the write is idempotent, which
-- is the same at-least-once/ idempotent pairing the queue itself is built
-- on.
SELECT entity_ref, title, content
FROM search_documents
WHERE embedding IS NULL
   OR embedding_provider IS DISTINCT FROM @embedding_provider::text
   OR embedding_model    IS DISTINCT FROM @embedding_model::text
   OR embedding_version  IS DISTINCT FROM @embedding_version::text
ORDER BY entity_ref
LIMIT @batch_size;

-- name: UpdateSearchDocumentEmbedding :execrows
--
-- The projection's SECOND write path, and deliberately not the first: this
-- updates the vector and its provenance and touches nothing else. T0901's
-- UpsertSearchDocument stays the single writer of a document's derived
-- content and visibility, so re-running the projection does not erase a
-- vector while re-embedding does not rewrite a document (and the two can
-- run concurrently without fighting). The split is what makes the pair
-- idempotent in both directions: each writer owns its columns.
--
-- embedding is assigned from a text parameter holding pgvector's input
-- syntax ("[0.1,0.2,...]"). The cast is explicit because the column's type
-- is not one the driver knows; the server does the parsing, which is how
-- this repository keeps a pgvector Go dependency out of the module (see
-- sqlc.yaml's vector override for the generation half of the same fact).
--
-- :execrows, not :exec — the row count is not decoration, it is what makes
-- the recompute loop bounded. The batch job loop terminates because a row
-- it has written stops matching the backlog predicate; if a write ever
-- lands nowhere (a BEFORE UPDATE trigger that returns NULL, a changed
-- WHERE, a renamed column), the row stays in the backlog and the job would
-- select it forever. Reporting the count lets the job say "this write did
-- not land" and fail loudly instead of spinning, which matters because the
-- same code runs as a resident consumer where a spin is invisible.
UPDATE search_documents
SET embedding          = @embedding::vector,
    embedding_provider = @embedding_provider::text,
    embedding_model    = @embedding_model::text,
    embedding_version  = @embedding_version::text
WHERE entity_ref = @entity_ref;

-- ---------------------------------------------------------------------------
-- T0904: the retrieval surface.
--
-- docs/14 §2 fixes the pipeline as "structured filters + FTS + semantic
-- candidate retrieval + graph traversal + scientific ranking", and
-- ADR-005 keeps all four in PostgreSQL for V1. T0901 filled the projection,
-- T0902 filled the vector, T0903 wrote the plan; these three reads are what
-- turns a plan into candidates, and they are the FTS, the vector and the
-- structured-filter recall signals respectively (the graph traversal is
-- ListScopeAdjacentRelationVersions / ListScopeObjectVersions below).
--
-- # Why these are new queries and not a widened SearchDocuments
--
-- The canonical read (SearchDocuments, above) stays exactly as it is: it is
-- the access-control regression test's subject (tests/integration/
-- search_access_test.go) and the surface T0905/T0906 will serve. What the
-- retrieval adds is NARROWING that has to happen in SQL rather than after
-- the fact, because a narrowing applied after LIMIT is not a narrowing: a
-- question whose plan names knowledge documents would otherwise take the
-- page of best-matching rows across every entity type and then throw most
-- of it away, reporting "no knowledge answer" for a corpus that has one.
--
-- The access predicate is therefore repeated VERBATIM in each of them —
-- `(visibility = 'public' OR project_id = ANY(@allowed_project_ids::uuid[]))`
-- — and NOT re-derived. A second implementation of that rule is how two
-- answers to "who may see this row" start to disagree, so the integration
-- suite pins that this copy and the canonical query return the same rows for
-- the same input, and that the fail-closed property (an empty scope yields
-- public rows only, never the table) holds for each of them independently of
-- the other. A query that could not accept a scope could not enforce one.
--
-- # public_only
--
-- The plan's `visibility` item is a NARROWING HINT and never a grant
-- (planner.VisibilityPublic / VisibilityAccessible). It arrives here as a
-- boolean that can only ever REMOVE rows: 'public' means "the rows the read
-- query returns to anybody", and 'accessible' means "whatever the scope
-- already allows", which is the predicate below unchanged. There is
-- deliberately no value of this flag that widens anything.
--
-- # entity_types / structured_filter
--
-- entity_types is the plan's target_object vocabulary (the projection's own
-- entity types). structured_filter is the caller's facet filter
-- (specs/api/openapi.yaml, POST /search: `filters`), matched as jsonb
-- containment so a caller can ask for one facet — {"object_type":"claim"} is
-- how "Claims" is recalled — without this query knowing any facet's name.
-- Both are ANDed onto the access predicate, so neither can be used to reach
-- a row the predicate refuses.

-- name: SearchDocumentsFullText :many
--
-- The FTS signal. The text semantics are the canonical query's, character
-- for character — the same to_tsvector expression, the same
-- plainto_tsquery, the same ts_rank — so the two cannot disagree about what
-- "matches" means, and the index the projection built
-- (search_documents_fts_idx) serves both.
SELECT entity_ref, entity_type, visibility, project_id, title, content, structured, updated_at,
       ts_rank(to_tsvector('simple', title || ' ' || content), plainto_tsquery('simple', @query)) AS rank
FROM search_documents
WHERE to_tsvector('simple', title || ' ' || content) @@ plainto_tsquery('simple', @query)
  AND (@entity_types::text[] IS NULL OR entity_type = ANY(@entity_types::text[]))
  AND (@structured_filter::jsonb IS NULL OR structured @> @structured_filter::jsonb)
  AND (@public_only::boolean = false OR visibility = 'public')
  AND (visibility = 'public' OR project_id = ANY(@allowed_project_ids::uuid[]))
ORDER BY rank DESC, entity_ref
LIMIT @page_size;

-- name: SearchDocumentsByVector :many
--
-- The vector signal. Two things about it are load-bearing.
--
-- 1. The provenance match. A vector is only meaningful against the model
--    that produced it (internal/search/embedding/port.go, Model), so a row
--    whose stored provider/model/version is not the one that embedded THIS
--    query is not a worse match — it is not a match at all, and comparing
--    against it would produce a confident, meaningless distance. The three
--    columns are compared, not just the version: two implementations can
--    ship the same version label. Rows left behind by a replaced model are
--    simply not recalled here; the batch job is what brings them back
--    (SearchDocumentsNeedingEmbedding selects exactly them).
--
-- 2. The exact scan. There is no ivfflat/hnsw index on the column, and that
--    is 00092's recorded decision, not an omission: an approximate index can
--    be less accurate than the scan, never more, and V1's scale does not
--    require one. Adding one is a measurable performance decision with its
--    own evidence, and this query is where its effect would be felt.
--
-- @embedding arrives in pgvector's text input syntax and is cast explicitly,
-- exactly as UpdateSearchDocumentEmbedding writes it: the column's type is
-- not one the driver knows, and the server parses it (sqlc.yaml's override).
--
-- The ::float8 cast on the score is not decoration: pgvector's `<=>` is an
-- operator over a type sqlc has no mapping for (sqlc.yaml overrides the
-- column, not the operator), and without the cast the generator typed the
-- result as int32 — a real double precision value read through an integer
-- destination, which pgx refuses at scan time. The cast states the type the
-- expression actually has.
SELECT entity_ref, entity_type, visibility, project_id, title, content, structured, updated_at,
       (1 - (embedding <=> @embedding::vector))::float8 AS similarity
FROM search_documents
WHERE embedding IS NOT NULL
  AND embedding_provider = @embedding_provider::text
  AND embedding_model    = @embedding_model::text
  AND embedding_version  = @embedding_version::text
  AND (@entity_types::text[] IS NULL OR entity_type = ANY(@entity_types::text[]))
  AND (@structured_filter::jsonb IS NULL OR structured @> @structured_filter::jsonb)
  AND (@public_only::boolean = false OR visibility = 'public')
  AND (visibility = 'public' OR project_id = ANY(@allowed_project_ids::uuid[]))
ORDER BY embedding <=> @embedding::vector, entity_ref
LIMIT @page_size;

-- name: SearchDocumentsByFacets :many
--
-- The structured-filter signal: recall by facet alone, with no text
-- predicate at all. It is what makes a question like "the claims about CO2
-- uptake" answerable when the wording of the question does not occur in the
-- documents, and it is the one signal whose caller must supply a filter —
-- without one it would be "the first N rows of the index", which is not an
-- answer to anything. The retrieval layer enforces that (a facets-only
-- recall runs only when a facet was given); this query does not need to,
-- because returning rows the caller asked for is exactly its job.
--
-- ORDER BY entity_ref is a stable order rather than a ranking: there is no
-- text to rank against, and inventing a relevance order here would be
-- inventing a score (CLAUDE.md §9.13). The fusion layer treats this signal
-- as an unordered set by ranking it in that order — deterministically, which
-- is what reproducibility needs.
SELECT entity_ref, entity_type, visibility, project_id, title, content, structured, updated_at
FROM search_documents
WHERE (@entity_types::text[] IS NULL OR entity_type = ANY(@entity_types::text[]))
  AND (@structured_filter::jsonb IS NULL OR structured @> @structured_filter::jsonb)
  AND (@public_only::boolean = false OR visibility = 'public')
  AND (visibility = 'public' OR project_id = ANY(@allowed_project_ids::uuid[]))
ORDER BY entity_ref
LIMIT @page_size;

-- ---------------------------------------------------------------------------
-- The graph half.
--
-- A document is not an object version: search_documents indexes the network's
-- readable things (a published knowledge object, an asset version, a
-- release, a state), and the relation graph (relations / relation_versions,
-- 00006) connects scientific object VERSIONS. The mapping from one to the
-- other exists for exactly one entity type and it is a typed column, not a
-- convention: a knowledge publication pins the object version it published
-- (knowledge_publications.object_version_id, 00010/00083 — "what is
-- published, and what a foreign project cites, is one version"). An asset
-- and a release are bundles whose own surfaces (their manifests) are where
-- their content is read from, and a state is a transition; none of the three
-- is a version-pinned object, so none of them seeds an expansion. That is the
-- boundary of this task and not a missing join.

-- name: SearchSeedObjectVersions :many
--
-- The seed mapping: the pinned object version behind each recalled
-- publication pid. The version id is the graph's addressing unit, and the
-- object id + version_no are the citation (docs/21, ADR-010: the answer
-- cites a platform-determined version), which is why they travel together.
SELECT kp.pid,
       sov.id        AS object_version_id,
       sov.object_id,
       sov.version_no,
       sov.title,
       so.object_type,
       so.project_id::text AS project_id
FROM knowledge_publications kp
JOIN scientific_object_versions sov ON sov.id = kp.object_version_id
JOIN scientific_objects so ON so.id = sov.object_id
WHERE kp.pid = ANY(@pids::text[]);

-- name: ListScopeAdjacentRelationVersions :many
--
-- One traversal hop, scope-filtered IN SQL.
--
-- It is the retrieval's shape of 00036's ListAdjacentRelationVersions (the
-- RSG query surface's, T0209), with the same as-of rule — no lineage pin
-- here, so each relation renders at its newest version — and one difference
-- that matters: the RSG query deliberately returns an edge of any project so
-- that its SERVICE can authorize the hop, while a search has no such second
-- gate to run — its authorization is the scope, resolved once
-- (internal/search/scope.go) — so the scope is applied where the rows are
-- read. An edge enters only when the relation's own project AND both
-- endpoint projects are in the caller's scope; a hidden endpoint would
-- otherwise leak its pinned version id through the edge it appears on.
--
-- This is strictly NARROWER than T0209's per-project requireRead, on purpose.
-- T0209's surface is reached with a project in hand and a public project is
-- readable by anyone there; a search is reached with no project at all, and
-- invariant 6 ("Publish controls visibility") means an unpublished object
-- version is not the network's to read. A non-member therefore expands into
-- nothing; the public half of the graph is still searchable, one surface up,
-- because the projection indexes exactly the published things.
SELECT DISTINCT ON (rv.relation_id)
  rv.relation_id,
  rv.relation_type,
  rv.source_object_version_id,
  rv.target_object_version_id,
  so_s.project_id::text AS source_project_id,
  so_t.project_id::text AS target_project_id
FROM relation_versions rv
JOIN relations r ON r.id = rv.relation_id
JOIN scientific_object_versions sov_s ON sov_s.id = rv.source_object_version_id
JOIN scientific_objects so_s ON so_s.id = sov_s.object_id
JOIN scientific_object_versions sov_t ON sov_t.id = rv.target_object_version_id
JOIN scientific_objects so_t ON so_t.id = sov_t.object_id
WHERE (rv.source_object_version_id = ANY(@version_ids::uuid[])
       OR rv.target_object_version_id = ANY(@version_ids::uuid[]))
  AND r.project_id   = ANY(@project_ids::uuid[])
  AND so_s.project_id = ANY(@project_ids::uuid[])
  AND so_t.project_id = ANY(@project_ids::uuid[])
ORDER BY rv.relation_id, rv.version_no DESC;

-- name: ListScopeObjectVersions :many
--
-- The node rows of one traversal level. Scope-filtered in SQL for the same
-- reason as the hop above: the ids come from edges the previous level
-- admitted, and re-stating the scope here means a defect in the hop's filter
-- still cannot return another project's object version. The three columns
-- the retrieval reports beyond the ids are the ones a candidate must carry
-- and nothing else — no payload, no content: a candidate is a citation
-- pointer, and the answer layer reads the object through its own surface.
SELECT sov.id AS object_version_id,
       sov.object_id,
       sov.version_no,
       sov.title,
       so.object_type,
       so.project_id::text AS project_id
FROM scientific_object_versions sov
JOIN scientific_objects so ON so.id = sov.object_id
WHERE sov.id = ANY(@version_ids::uuid[])
  AND so.project_id = ANY(@project_ids::uuid[])
ORDER BY sov.id;

-- ---------------------------------------------------------------------------
-- T0905: the ranking's factor reads.
--
-- docs/14 §3 orders the candidate set by "query/scope match、evidence
-- profile、review state、independent reproduction、contradictory evidence、
-- version/freshness", and "不得主要按 popularity/star/organization
-- prestige". Every one of those six is a READ of rows the platform already
-- keeps — nothing is stored for the ranking, and no table is added for it.
-- That is a property worth stating in the query rather than in prose: a
-- ranking that had its own table would be a second copy of the evidence
-- graph, and the two would drift.
--
-- This query is the WHOLE database side of the ranking: one row per object
-- version, carrying the counts each factor is decided from. The decision
-- itself is internal/search/ranking's, in Go, where it is unit-testable and
-- where a golden fixture can pin it byte for byte.
--
-- # Why aggregate counts and not the rows
--
-- The ranking never renders an assertion, a review or a relation: it renders
-- a LEVEL and a sentence ("2 evidence assertions, of which 1 reviewed"). What
-- it needs from the database is therefore a count per bucket, and shipping
-- the rows would mean reading — and holding — data the ranking has no use
-- for. It is also the shape that keeps the read bounded: the response is one
-- row per requested version, whatever the corpus's density.
--
-- # Scope
--
-- Same boundary as ListScopeObjectVersions, and for the same reason: the
-- ids arrive from candidates the caller has already been authorized to see,
-- so the scope is applied to this read's OUTPUT as well as enforced a second
-- time where the facts live.
--
--   * the version itself must belong to a project in the scope. A candidate
--     whose version is outside it reports NO facts, and the ranking says so
--     (an "unknown" factor), rather than reading another project's evidence
--     to order a row the caller may see.
--   * a contradictory RELATION is admitted by the same three-way rule as
--     the traversal hop (relation project AND both endpoint projects in
--     scope): a contradiction asserted by a project the caller is not in is
--     not the caller's graph's to know about.
--   * an evidence assertion is admitted when its project is in scope or the
--     ASSERTION's own visibility is public (00091's column). That is one
--     conjunct looser than the evidence table's own public-network read
--     (ListPublishedEvidenceForTarget), which additionally requires the
--     asserting project to be public unless it is the target's own. The
--     difference is unreachable through the write path — only a public
--     project can write a public assertion there, and a project's
--     visibility cannot change — so the two predicates agree on every row
--     the platform can produce; this read does not repeat the network
--     read's structural refusal because its boundary is the caller's
--     scope, not the anonymous network's. docs/10 §7 is why public external
--     evidence counts here: on a published Knowledge Object the network is
--     shown Reviewed and Unreviewed External Evidence, and a ranking that
--     could not see it would rank by origin evidence alone.
--
-- # Why counts are ::integer
--
-- So the generated Go type is int32 and matches the aggregate columns'
-- siblings elsewhere in this file (projects.sql's owner_count). A count that
-- could overflow int32 is a corpus nobody has.

-- name: SearchRankingFacts :many
--
-- A FACT SHEET per object version — never a verdict. Every column is a count
-- or a flag read from a row someone else wrote; no column is a score, and
-- nothing here is computed from another column in this result. docs/10 §4
-- ("V1 不自动赋数值权重") is the reason that split matters: the weights would
-- have to live somewhere, and a weight that lived in SQL would be a
-- judgement the platform made about evidence types without a human.
WITH asked AS (
  SELECT sov.id,
         sov.object_id,
         sov.version_no,
         sov.lifecycle_state,
         -- The newest version of the OBJECT, not of the requested set. The
         -- difference is the whole point of the column: the ranking asks
         -- about the versions its candidates pin, and a pinned version is
         -- exactly the one that may have been superseded. A window function
         -- over this CTE would answer "the newest among the rows I asked
         -- about", which is the requested version itself whenever a single
         -- version of an object is asked for — so `historical` would be
         -- unreachable and every stale pin would be reported as current.
         -- The correlated max reads the object's own lineage instead
         -- (scientific_object_versions' UNIQUE(object_id, version_no) is
         -- what makes it a bounded lookup rather than a scan).
         (SELECT max(v.version_no)
            FROM scientific_object_versions v
           WHERE v.object_id = sov.object_id) AS newest_version_no
  FROM scientific_object_versions sov
  WHERE sov.id = ANY(@version_ids::uuid[])
)
SELECT
  a.id                    AS object_version_id,
  a.object_id::text       AS object_id,
  a.version_no,
  a.newest_version_no::integer AS newest_version_no,
  a.lifecycle_state,
  (a.version_no = a.newest_version_no) AS is_newest,
  ev.assertions::integer            AS evidence_assertions,
  ev.reviewed::integer              AS evidence_reviewed,
  ev.rejected::integer              AS evidence_rejected,
  ev.direct::integer                AS evidence_direct,
  ev.supporting::integer            AS evidence_supporting,
  ev.contradicting::integer         AS evidence_contradicting,
  ev.reproduces::integer            AS reproduces,
  ev.reproduces_independent::integer AS reproduces_independent,
  ev.fails_to_reproduce::integer    AS fails_to_reproduce,
  rv.scientific::integer            AS reviews_scientific,
  rv.approved::integer              AS reviews_approved,
  rv.changes_requested::integer     AS reviews_changes_requested,
  cx.contradicting_relations::integer AS contradicting_relations
FROM asked a
JOIN scientific_objects so ON so.id = a.object_id
LEFT JOIN LATERAL (
  SELECT count(*)                                                            AS assertions,
         count(*) FILTER (WHERE ea.review_state = 'reviewed')                AS reviewed,
         count(*) FILTER (WHERE ea.review_state = 'rejected')                AS rejected,
         count(*) FILTER (WHERE ea.directness = 'direct')                    AS direct,
         count(*) FILTER (WHERE ea.relation_type IN
           ('supports','validates','consistent_with','reproduces'))          AS supporting,
         count(*) FILTER (WHERE ea.relation_type IN
           ('contradicts','inconsistent_with','challenges','fails_to_reproduce')) AS contradicting,
         count(*) FILTER (WHERE ea.relation_type = 'reproduces')             AS reproduces,
         -- An INDEPENDENT reproduction is one asserted by a project other
         -- than the one that owns the version it targets. Project is the
         -- platform's research boundary (CLAUDE.md §9.1), so "another
         -- project reproduced it" is the platform's own notion of an
         -- independent party — and it is a fact about the rows, not a trust
         -- judgement about the people in them.
         count(*) FILTER (WHERE ea.relation_type = 'reproduces'
                            AND ea.project_id <> so.project_id)              AS reproduces_independent,
         count(*) FILTER (WHERE ea.relation_type = 'fails_to_reproduce')     AS fails_to_reproduce
  FROM evidence_assertions ea
  WHERE ea.target_object_version_id = a.id
    AND (ea.visibility = 'public' OR ea.project_id = ANY(@project_ids::uuid[]))
) ev ON true
LEFT JOIN LATERAL (
  -- The review state of the STATE this version was created in, and only the
  -- 'scientific' dimension: docs/09 §5 records a review per dimension
  -- precisely so that one dimension's approval does not stand in for
  -- another's, and a ranking that read either kind would flatten exactly
  -- that difference. Integrity review is a different axis (it is about the
  -- artifact's reproducibility, not the science), and it is not this
  -- factor's to read.
  SELECT count(*)                                                       AS scientific,
         count(*) FILTER (WHERE r.decision = 'approved')                AS approved,
         count(*) FILTER (WHERE r.decision = 'changes_requested')       AS changes_requested
  FROM reviews r
  JOIN scientific_object_versions sv ON sv.id = a.id
  JOIN project_states ps ON ps.id = sv.state_id
  WHERE r.reviewed_state_id = sv.state_id
    AND r.review_kind = 'scientific'
    AND ps.project_id = ANY(@project_ids::uuid[])
) rv ON true
LEFT JOIN LATERAL (
  -- The relation half of contradictory evidence: every relation of type
  -- 'contradicts' that touches this version, counted by RELATION (not by
  -- version: a relation that was restated is one contradiction, and
  -- counting its versions would let a re-issued edge weigh more). Admitted
  -- by the traversal hop's three-way project rule.
  --
  -- The type predicate reads EVERY version of the relation, which is a
  -- deliberate difference from the platform's other relation reads — the
  -- traversal hop (ListScopeAdjacentRelationVersions) and the RSG surface
  -- render each relation at its NEWEST version. The two rules diverge only
  -- when a relation's type was changed across its versions: a relation that
  -- once asserted 'contradicts' against this version and was later restated
  -- to another type still counts here. The divergence is the two questions
  -- the reads answer. A traversal walks the graph AS OF now, and an edge
  -- re-typed away is not an edge to follow. This factor answers whether the
  -- version was ever contested, and the contradicting relation_version is
  -- a committed row that still exists (nothing disappears, CLAUDE.md §9.8):
  -- restating the relation adds a version, it does not withdraw the
  -- assertion that was made.
  SELECT count(DISTINCT rv1.relation_id) AS contradicting_relations
  FROM relation_versions rv1
  JOIN relations rel ON rel.id = rv1.relation_id
  JOIN scientific_object_versions sov_s ON sov_s.id = rv1.source_object_version_id
  JOIN scientific_objects so_s ON so_s.id = sov_s.object_id
  JOIN scientific_object_versions sov_t ON sov_t.id = rv1.target_object_version_id
  JOIN scientific_objects so_t ON so_t.id = sov_t.object_id
  WHERE rv1.relation_type = 'contradicts'
    AND (rv1.source_object_version_id = a.id OR rv1.target_object_version_id = a.id)
    AND rel.project_id  = ANY(@project_ids::uuid[])
    AND so_s.project_id = ANY(@project_ids::uuid[])
    AND so_t.project_id = ANY(@project_ids::uuid[])
) cx ON true
WHERE so.project_id = ANY(@project_ids::uuid[])
ORDER BY a.id;

-- ---------------------------------------------------------------------------
-- The search answer record (T0906, migration 00121)
--
-- One row per answered search, written once by the search API. What it is for
-- and what it deliberately is not (a cache) is the migration's header; what
-- belongs here is the write path's own rule.
--
-- Every column is passed as a value the caller already produced, and the
-- INSERT does not compute anything. That is deliberate: the row must record
-- the search that RAN. The citations are the answer's own citation list, the
-- selected refs are the retrieval's ranked refs, and a query that derived
-- either one would be a second producer of the invariant — the generator's
-- guard and the table's CHECK (citations <@ selected_refs) already refuse an
-- ungrounded citation, and a third refusal written in SQL here could only
-- disagree with them.
--
-- There is no UPDATE and no DELETE: docs/22 §8 saves the record as evidence
-- of what was answered, and CLAUDE.md §9.8's "nothing disappears" applies to
-- an answer that was published to a reader as much as to a scientific object.

-- name: InsertSearchRecord :one
INSERT INTO search_records (
    actor_id, query, filters, plan, signals, selected_refs, citations, answer
) VALUES (
    @actor_id, @query, @filters, @plan, @signals, @selected_refs, @citations, @answer
)
RETURNING id, created_at;
