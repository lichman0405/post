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
SELECT entity_ref, entity_type, visibility, project_id, title, content, structured, updated_at,
       ts_rank(to_tsvector('simple', title || ' ' || content), plainto_tsquery('simple', @query)) AS rank
FROM search_documents
WHERE to_tsvector('simple', title || ' ' || content) @@ plainto_tsquery('simple', @query)
ORDER BY rank DESC, entity_ref
LIMIT @page_size OFFSET @page_offset;
