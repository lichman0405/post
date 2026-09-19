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
