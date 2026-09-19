-- +goose Up
-- Pull-request creation idempotency (T0410).
--
-- specs/api/openapi.yaml declares the Open-pull-request route with
-- components.parameters.IdempotencyKey — `required: true`, `minLength: 8`.
-- A required header the server demands and then discards is worse than no
-- header at all: the client believes a retry is safe, the server creates a
-- second proposal, and the contract's own promise is the thing that broke.
-- The four writes that already carry this parameter solved it the same way
-- (merge 00070, release 00053, milestone 00063, asset publish 00072,
-- knowledge publish 00083): a UNIQUE key on the project, with the second
-- request returning the row the first one produced.
--
-- Those five are ledgers, because the entity they create is not the row the
-- request is addressed to (a merge record, a release, a publication). A pull
-- request is different: the created entity IS the pull_requests row, so the
-- key's home is that row and no second table is needed. One key names one
-- proposal per project, which is exactly the uniqueness the contract asks
-- for and exactly what UNIQUE(project_id, number) already is for the number.
--
-- The index is PARTIAL on `creation_key <> ''`:
--
--   * '' is the absent key — every row written before this migration and
--     every creation that sends no key at all (the domain service's own
--     callers, the store's tests, the fork service's import). Those must not
--     collide with each other, and a plain UNIQUE would let at most one of
--     them exist per project.
--   * column default '' rather than NULL keeps the wire/adapter types plain
--     (a go string, not a nullable one) without making "no key" a value a
--     caller could store by accident.
--
-- Forward-only and additive: no existing row changes, no column is dropped,
-- and a database that never sends a key behaves exactly as before.
ALTER TABLE pull_requests ADD COLUMN creation_key text NOT NULL DEFAULT '';

CREATE UNIQUE INDEX pull_requests_creation_key_idx
  ON pull_requests (project_id, creation_key)
  WHERE creation_key <> '';

COMMENT ON COLUMN pull_requests.creation_key IS
  'The Idempotency-Key the creation request carried (specs/api/openapi.yaml components.parameters.IdempotencyKey); empty when the request sent none. UNIQUE per project among non-empty keys: a repeated request returns the proposal the first one opened instead of opening a second.';
