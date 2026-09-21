-- +goose Up
-- Private Evidence / Public Attestation, the storage model (T0812).
--
-- # What an attestation is
--
-- A minimal public statement a project makes about a PUBLIC object version
-- it did not author: "we validated this, this way, and the result was
-- this". It is the one thing a private project may publish about its own
-- private work — docs/12 §2 逐字: "Private Project：project/RSG/private
-- blobs 默认不可见；可显式 Publish Asset/Knowledge/Attestation" — and
-- docs/22 §28 lists create attestation among the high-risk commands that
-- need server-side authorization plus policy validation.
--
-- # Attestation != evidence, and this table is where that is structural
--
-- `evidence_assertions` (00007, guards in 00058) is the evidence model: an
-- assertion pins TWO versions (target and evidence), and carries the
-- substance — directness, inference_nature, scope, reasoning_note. An
-- attestation pins ONE version and carries none of those. It has no
-- evidence_object_version_id column at all, and no free-text field a
-- private detail could be smuggled through: the public half of a row is
-- exactly the four enumerated values below plus the target pin.
--
-- That is the whole privacy argument, and it is STRUCTURAL rather than
-- procedural: there is no column on this table that names the underlying
-- private evidence or its content. What the row does record of the
-- private side — which project attested, which of that project's states
-- the attestation rests on, which internal review authorised it — is
-- cited by id, never by value, and is never projected publicly (see
-- internal/application/attestations.Present, which has no field for it).
--
-- # The private side, cited and not disclosed
--
--   * attesting_project_id  — the project that attests. A project id is
--                             not a public identity: nothing renders it.
--   * basis_state_id        — the attesting project's OWN state that the
--                             attestation rests on (the private evidence
--                             lives in that state's manifest). This is the
--                             "underlying private evidence" the task
--                             requires not to be exposed.
--   * internal_review_id    — the review that authorised the attestation,
--                             on a pull request of the attesting project
--                             and of exactly that state.
--
-- The two triggers below make "the cited private facts are the attester's
-- own" unconditional for ANY write path (application code, psql, a leaked
-- credential), in the 00014/00015/00028 discipline: the domain layer
-- validates on the happy path, the database makes the invariant
-- unconditional. A row whose basis state or whose review belongs to some
-- other project would be an attestation resting on somebody else's private
-- work — the one shape this table must not be able to hold.
--
-- # The target: exactly one pin, and no stored kind
--
-- A target is pinned to a scientific object version (the requirement's
-- Protocol/Claim) or to a research asset version (its Asset). Exactly one
-- of the two columns is set — the CHECK below — and the KIND is DERIVED
-- from which one it is, never stored. A stored target_kind column could
-- disagree with the row it points at (a 'protocol' label on a claim
-- version); the joined row cannot. This is the same decision
-- internal/application/evidencenetwork records for the evidence classes
-- ("the classes are computed, never stored").
--
-- # The public half: four enumerated values
--
--   * validation_type   — what kind of validation was performed
--   * validation_result — confirmed / refuted / inconclusive
--   * org_visibility    — whether the attesting organization is named
--
-- validation_result is deliberately three-valued and NOT numeric: no
-- weight, no score, nothing derivable (CLAUDE.md §9.13, docs/10 §4 "V1 不
-- 自动赋数值权重"). Two attestations on one version keep their own
-- results side by side, exactly as two contradictory evidence assertions
-- do.
--
-- # org_visibility AND the organization's standing setting
--
-- organizations.attestation_attribution is the organization's own standing
-- answer to "may we be named"; the attestation's org_visibility is the
-- choice made for THIS statement. The public projection names the
-- organization only when BOTH say so, and it re-reads the setting on
-- every read (internal/application/attestations.Present). Two independent
-- gates, each with its own failure mode:
--
--   * the recorded value is a PROMISE. An attestation issued while the
--     organization was anonymous stays anonymous even after the setting
--     flips to named — a standing setting must not retroactively break a
--     promise made under it.
--   * the setting is a FLOOR. An organization that flips to anonymous
--     stops being named on every attestation it ever issued — otherwise a
--     setting would be unable to retract a disclosure it exists to
--     control.
--
-- DEFAULT 'anonymous' is the conservative direction, and it is the
-- direction docs/12 §3 requires: "任何 private→public … 都要求有权限的人
-- 显式确认". Being named is a widening; an organization that has not
-- chosen it is not named.
--
-- # What is deliberately NOT here
--
--   * No uniqueness on (attesting_project_id, target_object_version_id,
--     validation_type). A second row with the same shape has a legitimate
--     reading — a re-validation after a first inconclusive result is a
--     new judgment about the same pair — and refusing it in the schema
--     would refuse the honest case to prevent the dishonest one. Nothing
--     sums these rows either (CLAUDE.md §9.13), so a duplicate buys
--     nothing; anti-gaming is a separate concern (docs/13 §6).
--   * No visibility column. An attestation is written already-published:
--     there is no draft state, and a "private attestation" would be a
--     second kind of row on a table whose whole point is that what is here
--     is public.
--   * No revocation. V1 does not retract (the same conservative default
--     the knowledge publication took); history is not deleted anyway
--     (CLAUDE.md §9.8), so a retraction would be a new row that the
--     projection reads — a later task's design, not this one's guess.

ALTER TABLE organizations
  ADD COLUMN attestation_attribution text NOT NULL DEFAULT 'anonymous'
    CHECK (attestation_attribution IN ('anonymous', 'named'));

COMMENT ON COLUMN organizations.attestation_attribution IS
  'standing answer to "may this organization be named on an attestation it issues": anonymous (default) or named. The attestation records its own org_visibility as well; the public projection names the organization only when both say named (migration 00120, T0812)';

CREATE TABLE attestations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  -- pid is the attestation's public identity: the id a client addresses and
  -- cites. Same 26-character lowercase Crockford base32 vocabulary and the
  -- same CHECK as asset pids (00064) and knowledge publication pids
  -- (00083), so one predicate in Go (assets.ValidPID) reads all three.
  --
  -- The DEFAULT stays for rows written before the command could mint one;
  -- the command passes the pid it minted explicitly, because a persistent
  -- identifier that two writers derive differently is not a persistent
  -- identity (00064 records the same decision).
  pid text NOT NULL DEFAULT substr(replace(gen_random_uuid()::text, '-', ''), 1, 26),

  -- The public target: exactly one of these two, enforced below.
  target_object_version_id uuid REFERENCES scientific_object_versions(id) ON DELETE RESTRICT,
  target_asset_version_id  uuid REFERENCES research_asset_versions(id) ON DELETE RESTRICT,

  -- The private side. Cited by id; never projected.
  attesting_project_id      uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  attesting_organization_id uuid REFERENCES organizations(id) ON DELETE RESTRICT,
  basis_state_id            uuid NOT NULL REFERENCES project_states(id) ON DELETE RESTRICT,
  internal_review_id        uuid NOT NULL REFERENCES reviews(id) ON DELETE RESTRICT,

  -- The public half.
  validation_type text NOT NULL,
  validation_result text NOT NULL,
  org_visibility text NOT NULL,

  created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),

  CONSTRAINT attestations_validation_type_check
    CHECK (validation_type IN ('reproduction', 'method_validation', 'data_audit', 'rights_review')),
  CONSTRAINT attestations_validation_result_check
    CHECK (validation_result IN ('confirmed', 'refuted', 'inconclusive')),
  CONSTRAINT attestations_org_visibility_check
    CHECK (org_visibility IN ('anonymous', 'named')),
  CONSTRAINT attestations_pid_format
    CHECK (pid ~ '^[0-9a-hjkmnp-tv-z]{26}$'),
  -- Exactly one target pin. Both NULL is an attestation about nothing; both
  -- set is an attestation about two things, and no read could say which one
  -- the validation_result is about. `<>` on two booleans, not OR: the
  -- columns are nullable, so the two IS NULL tests are the only total form.
  CONSTRAINT attestations_exactly_one_target
    CHECK ((target_object_version_id IS NULL) <> (target_asset_version_id IS NULL))
);

CREATE UNIQUE INDEX attestations_pid_uniq ON attestations (pid);

-- The two natural target reads: "the attestations about this object
-- version", newest first (the shape a public listing rides), and the same
-- for an asset version. The keyset order is (created_at DESC, id DESC) so
-- the read walks the index rather than sorting.
CREATE INDEX attestations_target_object_idx
  ON attestations (target_object_version_id, created_at DESC, id DESC);
CREATE INDEX attestations_target_asset_idx
  ON attestations (target_asset_version_id, created_at DESC, id DESC);

-- The attesting project's own read (a member listing what this project has
-- attested) — a stable forward walk, not a keyset page.
CREATE INDEX attestations_attesting_project_idx
  ON attestations (attesting_project_id, created_at, id);

-- The private-side guard. Two facts must hold for ANY write path:
--
--   1. the basis state is a state of the ATTESTING project, and
--   2. the internal review is a review on a pull request of the attesting
--      project, of EXACTLY that basis state.
--
-- (2) is the same pinning reviews.reviewed_state_id carries since 00061
-- ("a review is a judgment about one state, not about a moving target"):
-- here it says the review the attestation cites is the review of the state
-- the attestation rests on, not a review of some other state of the same
-- project. Without it, a project could cite an unrelated approved review.
--
-- Cross-table invariants cannot be CHECK constraints, so they are triggers
-- — the same layering 00004/00005/00058 use, and the same reason: the
-- domain layer refuses these on the happy path, and the database refuses
-- them unconditionally.
-- +goose StatementBegin
CREATE FUNCTION attestations_private_side_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM project_states ps
    WHERE ps.id = NEW.basis_state_id
      AND ps.project_id = NEW.attesting_project_id
  ) THEN
    RAISE EXCEPTION 'attestation: basis state % is not a state of the attesting project % (the private evidence an attestation rests on is the attesting project''s own)',
      NEW.basis_state_id, NEW.attesting_project_id
      USING ERRCODE = 'P0001';
  END IF;

  IF NOT EXISTS (
    SELECT 1
    FROM reviews r
    JOIN pull_requests pr ON pr.id = r.pull_request_id
    WHERE r.id = NEW.internal_review_id
      AND pr.project_id = NEW.attesting_project_id
      AND r.reviewed_state_id = NEW.basis_state_id
  ) THEN
    RAISE EXCEPTION 'attestation: internal review % is not a review of basis state % on a pull request of project % (an attestation cites the review that judged the state it rests on)',
      NEW.internal_review_id, NEW.basis_state_id, NEW.attesting_project_id
      USING ERRCODE = 'P0001';
  END IF;

  IF NEW.attesting_organization_id IS NOT NULL THEN
    IF NOT EXISTS (
      SELECT 1 FROM projects p
      WHERE p.id = NEW.attesting_project_id
        AND p.organization_id = NEW.attesting_organization_id
    ) THEN
      RAISE EXCEPTION 'attestation: organization % does not own the attesting project % (an attestation is made in its own organization''s name)',
        NEW.attesting_organization_id, NEW.attesting_project_id
        USING ERRCODE = 'P0001';
    END IF;
  ELSE
    IF EXISTS (
      SELECT 1 FROM projects p
      WHERE p.id = NEW.attesting_project_id
        AND p.organization_id IS NOT NULL
    ) THEN
      RAISE EXCEPTION 'attestation: attesting project % belongs to an organization, so the attestation must record it',
        NEW.attesting_project_id
        USING ERRCODE = 'P0001';
    END IF;
  END IF;

  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER attestations_private_side
  BEFORE INSERT OR UPDATE ON attestations
  FOR EACH ROW EXECUTE FUNCTION attestations_private_side_guard();

-- Append-only. An attestation is a statement made in public: it is never
-- edited and never deleted (CLAUDE.md §9.8 — nothing disappears; state
-- only evolves). The same guard function 00014 installed for the version
-- logs and ledgers, and the same statement-level TRUNCATE closure 00015
-- added, because row triggers do not fire on TRUNCATE.
CREATE TRIGGER attestations_append_only
  BEFORE UPDATE OR DELETE ON attestations
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER attestations_no_truncate
  BEFORE TRUNCATE ON attestations FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
