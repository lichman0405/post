-- Search projection (canonical table: search_documents). Rebuildable by
-- construction (docs/14: Postgres FTS + structured filters; embeddings are a
-- later concern, OpenSearch explicitly post-V1).

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
