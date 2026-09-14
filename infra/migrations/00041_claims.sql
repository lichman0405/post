-- +goose Up
-- Claim structured projection (task T0502): the queryable, structured
-- face of a claim object version — claim type, subject/property/value,
-- scope, causal basis and assessment as columns instead of payload
-- digging (docs/08 §Claim, docs/21 §7, docs/10 §5, docs/43).
--
-- The source of truth stays scientific_object_versions.payload; this row
-- is a REBUILDABLE projection (docs/21 §5: projections can be rebuilt and
-- are not the historical source of truth), so it deliberately carries no
-- append-only trigger (00014/00015 guard the version logs, not their
-- projections).
--
-- One row per claim VERSION (version_id is the PK): a Finding pins fixed
-- claim versions, so a new claim version is a new row and can never
-- silently rewrite what an older Finding saw (T0503 acceptance).
--
-- scope is the structured claim scope (docs/21 §7: structured JSONB);
-- scope_conditions is its normalized common-fields subset (temperature,
-- pressure, ...) kept as a separate array so projections and indexes
-- reach the conditions without parsing the whole scope.
--
-- basis is the declared causal basis (docs/10 §5: controlled
-- intervention, temporal ordering, confounders considered, dose-response,
-- mechanistic characterization, computational intervention), one
-- {"type","detail"} entry per basis. Causal/mechanistic claims must
-- declare at least one — the semantic check warns when they do not
-- (T0502 acceptance: causal 无 basis 产生 validation warning).
CREATE TABLE claims (
  version_id uuid PRIMARY KEY REFERENCES scientific_object_versions(id) ON DELETE RESTRICT,
  object_id uuid NOT NULL REFERENCES scientific_objects(id) ON DELETE RESTRICT,
  claim_type text NOT NULL CHECK (claim_type IN ('descriptive','quantitative','comparative','associational','predictive','causal','mechanistic')),
  subject_ref text,
  property text,
  value jsonb,
  scope jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(scope) = 'object'),
  scope_conditions jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(scope_conditions) = 'array'),
  basis jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(basis) = 'array'),
  assessment text CHECK (assessment IN ('preliminary','supported','contested','unresolved','superseded','aborted')),
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX claims_object_idx ON claims(object_id);
CREATE INDEX claims_type_idx ON claims(claim_type);
CREATE INDEX claims_subject_idx ON claims(subject_ref) WHERE subject_ref IS NOT NULL;
CREATE INDEX claims_scope_conditions_gin ON claims USING gin(scope_conditions);
CREATE INDEX claims_basis_gin ON claims USING gin(basis);

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
