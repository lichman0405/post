-- +goose Up
-- T0902. search_documents gains the PROVENANCE of its embedding, and
-- nothing else about the table changes.
--
-- 00013 created search_documents.embedding vector(1536) and left it NULL
-- forever: no query, no worker and no migration in this tree ever read or
-- wrote it. Filling it needs a second fact on the same row, because an
-- embedding is only valid for the model that produced it — a vector is a
-- pair (model identity, vector), not a number list. Without the identity a
-- row cannot answer "is this vector current?", which is the question every
-- recompute pass asks and the question "recompute after a model change"
-- turns into a job.
--
-- # Why columns on this table rather than a table of its own
--
-- search_documents is a PROJECTION, not history: 00014's append-only guard
-- deliberately excludes it, 00015 leaves it out of the truncate guard, and
-- tests/integration/append_only_truncate_test.go pins that TRUNCATE
-- search_documents succeeds. A projection's rows may be thrown away and
-- derived again, so adding columns to it raises NO backfill question:
-- existing rows are simply rows whose vector has not been computed yet
-- (NULL, below), and a rebuild re-derives them. A separate row-per-version
-- table would instead introduce exactly the consistency problem this table
-- is valuable for NOT having: every read of a document would have to decide
-- which of its version rows is the one it means, and a rebuild would have to
-- keep the two in step.
--
-- # Why the columns are nullable
--
-- NULL is a real state, not a missing one: it says "no vector has been
-- computed for this row under any model" (every row, until T0902's batch
-- job runs). This is the same stance internal/rights/usage.go takes on an
-- unspecified usage — an omitted fact is the answer "not specified", and
-- inventing a placeholder value for it would be inventing a fact. There is
-- deliberately no DEFAULT: a default provider would claim every unembedded
-- row had been embedded by it.
--
-- # Why not structured jsonb
--
-- search_documents.structured is an open bag of a document's own facets. A
-- field that decides "must this row be recomputed?" is a predicate the
-- recompute pass compares against the configured model on every scan, and
-- inside an open bag its absence is indistinguishable from any other key's
-- absence while its type is whatever the last writer put there. A tracked
-- fact belongs in a typed column (docs/52: an external provider's identity
-- never becomes a domain primary identity — here it is a recorded attribute
-- of a derived row, addressed by the entity_ref the row is already keyed
-- by).
--
-- # What the three columns are
--
--   embedding_provider  which implementation computed the vector
--   embedding_model     which model of it (the provider's own name)
--   embedding_version   which revision of that model
--
-- All three are text and none is parsed by POST: the provider owns what it
-- calls its revision, and POST's job is to store it verbatim so that a
-- change is detectable. The complete triple, not just the version, is what
-- the recompute pass compares — a different implementation can ship the
-- same version label.
--
-- # No index, deliberately
--
-- pgvector supports ivfflat/hnsw indexes; ADR-005 sets no threshold and
-- docs/68 pins only "PostgreSQL + pgvector". An index is a measurable
-- performance decision, not a semantic one, and at V1 scale the exact scan
-- it would replace is exact (an approximate index can be less accurate than
-- the scan, never more). The recompute pass reads search_documents
-- unordered by embedding_provider anyway, and the retrieval query does not
-- exist yet (T0904/T0905). Left out as a decision, not by omission.

ALTER TABLE search_documents
  ADD COLUMN embedding_provider text,
  ADD COLUMN embedding_model    text,
  ADD COLUMN embedding_version  text;

COMMENT ON COLUMN search_documents.embedding IS
  'The document''s embedding, 1536 wide (00013). NULL means no vector has been computed for this row.';
COMMENT ON COLUMN search_documents.embedding_provider IS
  'Who computed embedding. NULL means no vector has been computed for this row — never "unknown provider".';
COMMENT ON COLUMN search_documents.embedding_model IS
  'The provider''s own name for the model that produced embedding.';
COMMENT ON COLUMN search_documents.embedding_version IS
  'The provider''s own revision of embedding_model. Stored verbatim: POST does not renumber an external identity.';

-- The vector and its provenance travel together, and the database says so.
-- A row with a vector and no provenance would be re-embedded by every
-- recompute pass forever (its metadata never matches any model), and a row
-- with provenance and no vector would be a row that claims an embedding it
-- does not have. Both are unrepresentable here rather than merely
-- unwritten: the batch job has one write path, and this is the second line
-- for a session that writes SQL directly (the same reason 00078's target
-- CHECK repeats what the application already validates).
ALTER TABLE search_documents
  ADD CONSTRAINT search_documents_embedding_complete CHECK (
    (embedding IS NULL) = (embedding_provider IS NULL)
    AND (embedding IS NULL) = (embedding_model IS NULL)
    AND (embedding IS NULL) = (embedding_version IS NULL)
  );

-- +goose Down
-- Nothing here is history: the columns carry a derived fact about a
-- projection row, so dropping them drops derived state and nothing else.
-- (Forward-only in practice — 00014's rule is that a published migration is
-- never edited; this exists so a rollback of the migration set is total.)
ALTER TABLE search_documents
  DROP CONSTRAINT search_documents_embedding_complete;

ALTER TABLE search_documents
  DROP COLUMN embedding_version,
  DROP COLUMN embedding_model,
  DROP COLUMN embedding_provider;
