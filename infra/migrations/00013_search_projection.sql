-- +goose Up
-- Search projection (rebuildable; canonical lines 410-423).
CREATE TABLE search_documents (
  entity_ref text PRIMARY KEY,
  entity_type text NOT NULL,
  visibility text NOT NULL,
  project_id uuid REFERENCES projects(id) ON DELETE RESTRICT,
  title text NOT NULL,
  content text NOT NULL,
  structured jsonb NOT NULL DEFAULT '{}'::jsonb,
  embedding vector(1536),
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX search_documents_fts_idx ON search_documents USING gin(to_tsvector('simple', title || ' ' || content));
CREATE INDEX search_documents_structured_gin ON search_documents USING gin(structured);
