-- +goose Up
-- Evidence assertion guards (task T0504).
--
-- Migration 00007 scaffolded the evidence_assertions table with the
-- relation and review-state enums checked; this migration completes the
-- guard surface so the table enforces the whole evidence-assertion
-- schema vocabulary — and the model's one non-enum structural rule, the
-- directed-edge pins — for ANY write path (application code, psql, a
-- leaked credential), in the 00014/00015/00028 discipline: the domain
-- layer validates on the happy path (internal/domain.EvidenceAssertion
-- and internal/rsg/semantics.CheckEvidenceAssertion), the database makes
-- the invariant unconditional.
--
-- Added here, mirroring the schema enums (specs/schemas/evidence-
-- assertion.schema.json) value for value:
--
--   * evidence_type  — experimental, computational, dataset, literature,
--     external_attestation, other;
--   * directness     — direct, indirect, unknown;
--   * inference_nature — observational, associational, causal,
--     mechanistic, predictive, descriptive, unknown;
--   * scope          — a JSON object (jsonb_typeof(scope) = 'object');
--     the schema leaves scope's CONTENT free-form, so "is an object" is
--     the only mechanical check there is.
--
-- Plus one rule the enums do not cover, mirrored from the domain model:
--
--   * the two version pins must DIFFER (target_object_version_id <>
--     evidence_object_version_id) — an assertion is a directed edge
--     between two versions, and domain.EvidenceAssertion.Validate
--     already rejects the self-assertion, so the database makes the
--     same rule unconditional here (this migration's layering: the
--     happy path validates, the schema guarantees).
--
-- The relation and review-state enums are already checked by 00007; the
-- new checks deliberately cover only the rules that lacked them.
--
-- Deliberately NOT constrained here:
--
--   * (target_object_version_id, evidence_object_version_id) uniqueness.
--     The pair is NOT unique BY DESIGN (task acceptance criterion:
--     支持与反驳可同时存在): one evidence version may carry a supports
--     AND a contradicts assertion on the same target version, side by
--     side, each reviewed on its own. A unique constraint on the pair
--     would collapse the two stances into one row and force a winner —
--     exactly the net-position arithmetic the domain forbids.
--   * any weight/score column. docs/10 §4: V1 不自动赋数值权重 — no
--     numeric weight exists, and none is derivable here (CLAUDE.md
--     §9.13: no Truth Score). Stance labels (supporting/contesting/
--     neutral) are computed by a transparent rule in the domain layer,
--     never stored as numbers.
--   * append-only DELETE guards. Reviewing an assertion is a legitimate
--     in-place state change (unreviewed → reviewed → rejected), like the
--     other version-less member rows; the append-only triggers (00014/
--     00015) guard the version LOGS, not member rows.
--
-- Version pinning (the task's version-pin requirement) was already
-- structural in 00007: both endpoints are uuid foreign keys to
-- scientific_object_versions with ON DELETE RESTRICT, so a pinned
-- version cannot disappear while an assertion points at it, and the
-- assertion keeps its meaning when either object moves on. This
-- migration adds the indexes that make the pinned queries fast.
--
-- The indexes serve the two natural query paths:
--   * listing assertions against one target version (the
--     ListEvidenceAssertionsForTarget shape: target, then created
--     order);
--   * finding every assertion a given evidence version backs.

ALTER TABLE evidence_assertions
  ADD CONSTRAINT evidence_assertions_evidence_type_check
    CHECK (evidence_type IN ('experimental', 'computational', 'dataset', 'literature', 'external_attestation', 'other')),
  ADD CONSTRAINT evidence_assertions_directness_check
    CHECK (directness IN ('direct', 'indirect', 'unknown')),
  ADD CONSTRAINT evidence_assertions_inference_nature_check
    CHECK (inference_nature IN ('observational', 'associational', 'causal', 'mechanistic', 'predictive', 'descriptive', 'unknown')),
  ADD CONSTRAINT evidence_assertions_scope_check
    CHECK (jsonb_typeof(scope) = 'object'),
  -- The self-assertion rule, mirrored from domain.EvidenceAssertion.Validate
  -- (target_version_ref == evidence_version_ref is a hard failure there):
  -- an assertion is a DIRECTED edge between two versions, so a row naming
  -- the same version on both ends is meaningless. The domain rejects it on
  -- the happy path; this CHECK makes the rule unconditional for ANY write
  -- path, which is the point of this migration's layering.
  ADD CONSTRAINT evidence_assertions_pins_differ_check
    CHECK (target_object_version_id <> evidence_object_version_id);

CREATE INDEX evidence_assertions_target_idx
  ON evidence_assertions (target_object_version_id, created_at, id);

CREATE INDEX evidence_assertions_evidence_idx
  ON evidence_assertions (evidence_object_version_id);

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
