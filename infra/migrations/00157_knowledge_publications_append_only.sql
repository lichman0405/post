-- +goose Up
-- The publication record is append-only (T1218, issue #228).
--
-- knowledge_publications has been in the schema since 00010 as the record
-- that one scientific object version was published to the network:
-- published_by and published_at (the act, not an attribute of the object),
-- the rights_json the publisher published UNDER, and UNIQUE(object_version_id,
-- public_version). It is history, not current state: a correction appends a
-- new public_version rather than rewriting what an earlier publication said.
--
-- Nothing enforced that. 00083 (T0805) built the publish path's
-- Idempotency-Key ledger beside it and wrote the premise into its own header
-- — the ledger is "Append-only, like the row it points at and like the other
-- ledgers" — then took BOTH halves of the guard for the LEDGER
-- (knowledge_publication_creations_append_only at :118,
-- knowledge_publication_creations_no_truncate at :122) and never touched the
-- row it points at. 00014's row-level census never included
-- knowledge_publications either: T0013's exemption list (tasks/results/
-- T0013/RESULT.json) filed it with the current-state tables (users,
-- projects), a grouping that contradicts both the table's own columns and
-- 00083's premise. Until now the immutability lived only in that comment, and
-- a comment is not a mechanism: the refusal is.
--
-- The disclosure is issue #228. Its body also states that
-- PublishKnowledgePublication had no caller outside the generated sqlc code
-- and that adding a trigger "would break no code" — true when written, not
-- true now: T0805 (fc78310) landed the publish command, and
-- internal/persistence/knowledge_publish_store.go calls it today. The risk
-- was assessed against the tree instead: that path only INSERTs
-- (internal/persistence/queries/knowledge_publish.sql), so a BEFORE
-- UPDATE OR DELETE / BEFORE TRUNCATE refusal cannot reach a legitimate
-- publish, and tests/integration/knowledge_publish_test.go — the command's
-- end-to-end suite — is what proves it, publishing through the store with
-- this migration applied.
--
-- Both halves, and the same append_only_guard() 00014 created rather than a
-- second judgement of what immutable means: a ledger that can be truncated is
-- not append-only, TRUNCATE fires no row trigger, and an operator reading the
-- error gets the identical 'table % is append-only: % is forbidden' every
-- other guarded table raises (TG_TABLE_NAME / TG_OP).
--
-- No constraint is added, changed or dropped, and the application rule that
-- one version is published once (owner ruling L3-20260916-1 #3) stays where
-- 00083 put it: knowledgepublish.Judge refuses it, not an index.

CREATE TRIGGER knowledge_publications_append_only
  BEFORE UPDATE OR DELETE ON knowledge_publications
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER knowledge_publications_no_truncate
  BEFORE TRUNCATE ON knowledge_publications FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
