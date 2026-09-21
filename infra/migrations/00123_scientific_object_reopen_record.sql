-- +goose Up
-- The reopen record on the append-only object version log (T0610).
--
-- docs/46:11 is the whole spec sentence for reopen: "Reopen 创建新 transition，
-- 保留历史 abort。" docs/43:10 gives the edge ("active → aborted → reopened →
-- active；可 superseded…但不物理删除"), docs/03 §6 defines the state
-- ("在 abort 之后通过新 transition 恢复继续研究"), and 00014's append-only trigger
-- names reopen beside abort as the canonical correction that APPENDS a row.
-- Nothing in the repository asks for a reopen column set, and neither does
-- this migration: the columns below are migration 00100's abort record,
-- applied to the transition 00100 itself flags as its sibling.
--
-- # Why this migration exists at all, given the lifecycle bit was already here
--
-- `'reopened'` has been a legal `lifecycle_state` since 00005:20, so the
-- STATE transition needs no schema change. Two things the T0610 acceptance
-- criteria require do:
--
--   - Idempotency. The abort command's replay works because the request's
--     Idempotency-Key is stored on the row the request produced (00100's
--     abort_request_key, unique per object), so the state itself is the
--     idempotency record and a repeat writes no second version, no second
--     audit row and no second event. A reopen must replay the same way, and
--     a key with no home cannot be read back. It cannot borrow the abort
--     column: GetVersionByAbortRequestKey's read is shared with the abort
--     command's replay path, so a key written by a reopen would answer an
--     ABORT request with a reopened row.
--   - The governance record. docs/26 lists "abort/reopen" together among the
--     highest-risk audited actions, and the merge materializes the version a
--     proposal carried onto main under the MERGING actor's created_by — so
--     who decided the reopen and when would otherwise not exist on the row
--     that reaches main (the argument 00100 records verbatim for
--     aborted_by/aborted_at).
--
-- # Shape: 00100's, field for field
--
-- Same placement (columns on scientific_object_versions, NOT folded into
-- `payload`: the payload is the schema-governed scientific content and its
-- sha256 is the version's integrity_hash, so a governance act about a
-- version must not change what the version says). Same nullability (every
-- column NULL for every version that is not a reopen). Same all-or-nothing
-- CHECK. Same partial unique index scoped to the object for the request
-- key. Same open-string reason code (shape only, no vocabulary — none
-- exists in any spec for abort and none exists for reopen).
--
-- One deliberate asymmetry with 00100, named here because it is a
-- difference a reader will otherwise have to infer: 00100's record has an
-- abort_replacement_ref column and this one has no reopen counterpart.
-- docs/46:7's "replacement/superseding ref(optional)" is a field of an
-- ABORT record — the thing that replaces what was aborted. A reopen has no
-- replacement; it is itself the return to use. Adding a column no spec
-- names and no code would write is how a schema accumulates guesses.
--
-- Forward-only and additive: no existing column, constraint or row is
-- touched; a database that never reopens an object behaves exactly as
-- before, and every existing query keeps working (the new columns are
-- simply absent from their projections).
ALTER TABLE scientific_object_versions
  ADD COLUMN reopen_reason_code text,
  ADD COLUMN reopen_explanation text,
  ADD COLUMN reopened_by uuid REFERENCES users(id) ON DELETE RESTRICT,
  ADD COLUMN reopened_at timestamptz,
  ADD COLUMN reopen_request_key text;

-- The reason code is an OPEN string, exactly as 00100's abort_reason_code is:
-- no specification names reopen reason codes at all, so this CHECK fixes the
-- SHAPE (a bounded lowercase token) and never a vocabulary. No list of
-- accepted codes exists in this migration, in Go, or anywhere else.
ALTER TABLE scientific_object_versions
  ADD CONSTRAINT scientific_object_versions_reopen_reason_code_shape
  CHECK (reopen_reason_code IS NULL OR reopen_reason_code ~ '^[a-z0-9_]{1,64}$');

-- The human explanation is the part a machine cannot reconstruct, so it must
-- be present and non-blank whenever a record exists. The bound is 00100's.
ALTER TABLE scientific_object_versions
  ADD CONSTRAINT scientific_object_versions_reopen_explanation_shape
  CHECK (reopen_explanation IS NULL
         OR (length(btrim(reopen_explanation)) BETWEEN 1 AND 4096));

-- The idempotency key follows the contract's own bound
-- (specs/api/openapi.yaml, components.parameters.IdempotencyKey: minLength 8):
-- a stored key is a deliberate token, never a stray empty string.
ALTER TABLE scientific_object_versions
  ADD CONSTRAINT scientific_object_versions_reopen_request_key_shape
  CHECK (reopen_request_key IS NULL OR length(reopen_request_key) >= 8);

-- All-or-nothing, and tied to the state it describes: a row carrying reopen
-- metadata is a row whose lifecycle_state IS 'reopened'. The record can
-- therefore never be attached to an active or aborted version, and a
-- reopened version written without a record (a payload-only lifecycle move,
-- e.g. a fixture) stays legal — this migration constrains the record's shape,
-- it does not mandate that every reopened row has one.
ALTER TABLE scientific_object_versions
  ADD CONSTRAINT scientific_object_versions_reopen_record_shape
  CHECK (
    NOT (reopen_reason_code IS NULL) = NOT (reopen_explanation IS NULL)
    AND NOT (reopen_reason_code IS NULL) = NOT (reopened_by IS NULL)
    AND NOT (reopen_reason_code IS NULL) = NOT (reopened_at IS NULL)
    AND (reopen_reason_code IS NULL OR lifecycle_state = 'reopened')
  );

CREATE UNIQUE INDEX scientific_object_versions_reopen_request_key_idx
  ON scientific_object_versions (object_id, reopen_request_key)
  WHERE reopen_request_key IS NOT NULL;

COMMENT ON COLUMN scientific_object_versions.reopen_reason_code IS
  'The reopen''s reason code, in the shape 00100 gave the abort record. An OPEN caller-supplied token in V1 — no spec names reopen reason codes at all, and this platform does not invent a vocabulary; the column CHECKs the shape (^[a-z0-9_]{1,64}$) and records the value as given.';
COMMENT ON COLUMN scientific_object_versions.reopen_explanation IS
  'The human explanation the reopen recorded — the part no machine can reconstruct. Non-blank when the record exists.';
COMMENT ON COLUMN scientific_object_versions.reopened_by IS
  'The actor who decided the reopen (not the row''s created_by: after a Research PR merge materializes the reopen onto main, created_by is the merging actor while this stays the reopening one).';
COMMENT ON COLUMN scientific_object_versions.reopened_at IS
  'Server-derived time of the reopen decision; never caller-supplied. Travels with the record when a merge materializes the reopen onto main.';
COMMENT ON COLUMN scientific_object_versions.reopen_request_key IS
  'The Idempotency-Key the reopen request carried (specs/api/openapi.yaml components.parameters.IdempotencyKey); NULL when none. UNIQUE per object among non-NULL keys, so a repeated request reads the row the first one wrote instead of appending a second (the migration-00100 pattern). Not carried onto main by a merge: it names a request, not history.';

-- +goose Down
DROP INDEX IF EXISTS scientific_object_versions_reopen_request_key_idx;
ALTER TABLE scientific_object_versions
  DROP CONSTRAINT IF EXISTS scientific_object_versions_reopen_record_shape,
  DROP CONSTRAINT IF EXISTS scientific_object_versions_reopen_request_key_shape,
  DROP CONSTRAINT IF EXISTS scientific_object_versions_reopen_explanation_shape,
  DROP CONSTRAINT IF EXISTS scientific_object_versions_reopen_reason_code_shape,
  DROP COLUMN IF EXISTS reopen_request_key,
  DROP COLUMN IF EXISTS reopened_at,
  DROP COLUMN IF EXISTS reopened_by,
  DROP COLUMN IF EXISTS reopen_explanation,
  DROP COLUMN IF EXISTS reopen_reason_code;
