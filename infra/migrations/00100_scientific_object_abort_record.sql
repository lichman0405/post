-- +goose Up
-- The abort record on the append-only object version log (T0602).
--
-- docs/46 (Abort、Retention 与"不删除"原则) fixes what an abort must record,
-- verbatim: "Abort 必须记录 actor、time、reason code、human explanation、
-- replacement/superseding ref(optional)、review/approval if main object."
-- and how a main-line abort travels: "main 中对象 Abort 必须 branch → PR →
-- merge". docs/43's object lifecycle is "active → aborted → reopened →
-- active；可 superseded…但不物理删除", and the migration that installed the
-- append-only triggers (00014) names abort as the canonical example of a
-- correction that APPENDS a row instead of mutating one.
--
-- # Where the record lives, and why not in `payload`
--
-- The six columns below are added to scientific_object_versions — the table
-- that already carries the lifecycle_state the abort moves. They are NOT
-- folded into `payload`, and that is the load-bearing decision of this
-- migration: `payload` is the schema-governed SCIENTIFIC content of the
-- version, and its sha256 is the version's integrity_hash (docs/23 §3,
-- docs/21 §10 — "accepted state is content-addressed"). An abort is a
-- GOVERNANCE act about a version, not a change to what the version says:
-- aborted versions store the aborted version's payload byte-for-byte, so the
-- aborted row's content hash equals the content hash it had, and the
-- lifecycle move is the only thing the row changes. Folding the record into
-- the payload would make every abort a content change that invalidates the
-- hash it was accepted under, and would force every per-type schema in
-- packages/schemas/ to tolerate governance keys inside scientific content.
--
-- # Nullable, and grouped by one constraint
--
-- Every column is NULL for every version that is not an abort (and for every
-- row written before this migration — there are none on main, but the log is
-- append-only and the column set must not pretend otherwise). The
-- abort_record_shape CHECK keeps the record all-or-nothing: a row either
-- carries the whole record (reason code + explanation + deciding actor +
-- deciding time; the replacement ref and the request key stay optional) or
-- none of it. A half-written record is refused by the database, not by
-- convention.
--
-- aborted_by / aborted_at are deliberately NOT the row's created_by /
-- created_at. For the version the abort command writes, the two pairs agree.
-- For the version a Research PR MERGE writes onto main, they do not: the
-- merge materializes the source version's content into a new row whose
-- created_by is the MERGING actor (internal/application/merge materialize —
-- the merge is the write). docs/46:7's "actor, time" are facts about the
-- abort DECISION, so they get their own columns and survive the merge with
-- their original values.
--
-- # The request key replaces a ledger table
--
-- The contract's Idempotency-Key is honored on the state itself, exactly as
-- migration 00089 did for pull-request creation: the entity the request
-- produces IS a scientific_object_versions row, so the key's home is that
-- row and no second bookkeeping table is needed. The index is PARTIAL and
-- scoped to the object (one key names one abort of one object; NULL is the
-- absent key, and NULLs never collide in a unique index). A repeated request
-- therefore reads the row the first one wrote instead of appending a second
-- — no second audit row, no second scientific_object.aborted event.
--
-- Forward-only and additive: no existing column, constraint or row is
-- touched; a database that never aborts an object behaves exactly as before
-- and every existing query keeps working (the new columns are simply absent
-- from their projections).
ALTER TABLE scientific_object_versions
  ADD COLUMN abort_reason_code text,
  ADD COLUMN abort_explanation text,
  ADD COLUMN abort_replacement_ref text,
  ADD COLUMN aborted_by uuid REFERENCES users(id) ON DELETE RESTRICT,
  ADD COLUMN aborted_at timestamptz,
  ADD COLUMN abort_request_key text;

-- The reason code is an OPEN string in V1 (the T0602 ruling). Both specs
-- that require the field name it and neither enumerates values: docs/46:7
-- says the abort MUST record a reason code, and specs/mcp/tools.json's
-- object.abort_proposal passes `reason_code` in as a CALLER ARGUMENT. So the
-- column stores whatever token the caller used, and this CHECK only fixes
-- the SHAPE — a bounded lowercase token — never a vocabulary. No list of
-- accepted codes exists in this migration, in Go, or anywhere else; adding
-- one later is a normal narrowing, and the cost of not having one is named
-- in the T0602 result: V1 cannot break aborts down by reason.
ALTER TABLE scientific_object_versions
  ADD CONSTRAINT scientific_object_versions_abort_reason_code_shape
  CHECK (abort_reason_code IS NULL OR abort_reason_code ~ '^[a-z0-9_]{1,64}$');

-- The human explanation is the part a machine cannot reconstruct, so it must
-- be present and non-blank whenever a record exists. The upper bound is the
-- same 4 KiB order as the other free-text fields the platform stores; it
-- keeps one row from carrying an unbounded document.
ALTER TABLE scientific_object_versions
  ADD CONSTRAINT scientific_object_versions_abort_explanation_shape
  CHECK (abort_explanation IS NULL
         OR (length(btrim(abort_explanation)) BETWEEN 1 AND 4096));

-- The optional pointers (docs/46:7's "replacement/superseding ref"): absent
-- is NULL, never ''. An empty string would make "no replacement given" and
-- "a replacement given as nothing" indistinguishable on read.
ALTER TABLE scientific_object_versions
  ADD CONSTRAINT scientific_object_versions_abort_replacement_ref_shape
  CHECK (abort_replacement_ref IS NULL
         OR (length(btrim(abort_replacement_ref)) BETWEEN 1 AND 512));

-- The idempotency key follows the contract's own bound
-- (specs/api/openapi.yaml, components.parameters.IdempotencyKey: minLength
-- 8): a stored key is a deliberate token, never a stray empty string.
ALTER TABLE scientific_object_versions
  ADD CONSTRAINT scientific_object_versions_abort_request_key_shape
  CHECK (abort_request_key IS NULL OR length(abort_request_key) >= 8);

-- All-or-nothing, and tied to the state it describes: a row carrying abort
-- metadata is a row whose lifecycle_state IS 'aborted'. The record can
-- therefore never be attached to an active version, and an aborted version
-- written without a record (a payload-only lifecycle move, e.g. a fixture)
-- stays legal — this migration constrains the record's shape, it does not
-- mandate that every aborted row has one.
ALTER TABLE scientific_object_versions
  ADD CONSTRAINT scientific_object_versions_abort_record_shape
  CHECK (
    NOT (abort_reason_code IS NULL) = NOT (abort_explanation IS NULL)
    AND NOT (abort_reason_code IS NULL) = NOT (aborted_by IS NULL)
    AND NOT (abort_reason_code IS NULL) = NOT (aborted_at IS NULL)
    AND (abort_reason_code IS NULL OR lifecycle_state = 'aborted')
  );

CREATE UNIQUE INDEX scientific_object_versions_abort_request_key_idx
  ON scientific_object_versions (object_id, abort_request_key)
  WHERE abort_request_key IS NOT NULL;

COMMENT ON COLUMN scientific_object_versions.abort_reason_code IS
  'The abort''s reason code (docs/46:7). An OPEN caller-supplied token in V1 — no vocabulary is defined by any spec, and this platform does not invent one; the column CHECKs the shape (^[a-z0-9_]{1,64}$) and records the value as given.';
COMMENT ON COLUMN scientific_object_versions.abort_explanation IS
  'The human explanation the abort recorded (docs/46:7) — the part no machine can reconstruct. Non-blank when the record exists.';
COMMENT ON COLUMN scientific_object_versions.abort_replacement_ref IS
  'The optional replacement/superseding ref (docs/46:7). NULL means none was given; it is never stored as an empty string.';
COMMENT ON COLUMN scientific_object_versions.aborted_by IS
  'The actor who decided the abort (docs/46:7 "actor"). Not the row''s created_by: after a Research PR merge materializes the abort onto main, created_by is the merging actor while this stays the aborting one.';
COMMENT ON COLUMN scientific_object_versions.aborted_at IS
  'Server-derived time of the abort decision (docs/46:7 "time"); never caller-supplied. Travels with the record when a merge materializes the abort onto main.';
COMMENT ON COLUMN scientific_object_versions.abort_request_key IS
  'The Idempotency-Key the abort request carried (specs/api/openapi.yaml components.parameters.IdempotencyKey); NULL when none. UNIQUE per object among non-NULL keys, so a repeated request reads the row the first one wrote instead of appending a second (the migration-00089 pattern). Not carried onto main by a merge: it names a request, not history.';

-- +goose Down
DROP INDEX IF EXISTS scientific_object_versions_abort_request_key_idx;
ALTER TABLE scientific_object_versions
  DROP CONSTRAINT IF EXISTS scientific_object_versions_abort_record_shape,
  DROP CONSTRAINT IF EXISTS scientific_object_versions_abort_request_key_shape,
  DROP CONSTRAINT IF EXISTS scientific_object_versions_abort_replacement_ref_shape,
  DROP CONSTRAINT IF EXISTS scientific_object_versions_abort_explanation_shape,
  DROP CONSTRAINT IF EXISTS scientific_object_versions_abort_reason_code_shape,
  DROP COLUMN IF EXISTS abort_request_key,
  DROP COLUMN IF EXISTS aborted_at,
  DROP COLUMN IF EXISTS aborted_by,
  DROP COLUMN IF EXISTS abort_replacement_ref,
  DROP COLUMN IF EXISTS abort_explanation,
  DROP COLUMN IF EXISTS abort_reason_code;
