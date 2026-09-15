-- +goose Up
-- Finding projection (task T0503): the structured face of a finding object
-- version — finding type, assessment, and the claim versions it pins.
--
-- A Finding is the human-confirmed formal object that aggregates claim
-- VERSIONS (docs/08 §Finding: "contained claim versions"; the schema's
-- claim_version_refs, required, minItems 1). It pins VERSION ids, never
-- object ids: an edit of a claim is a NEW version row in the append-only
-- log (00014/00015), so a later claim version can never silently rewrite
-- what an older Finding aggregated (T0503 acceptance criterion).
--
-- findings is the per-version projection (one row per finding version,
-- the same 00041 shape). Like every projection it is REBUILDABLE
-- (docs/21 §5): the payload in scientific_object_versions stays the
-- historical source of truth, so it deliberately carries no append-only
-- trigger. finding_claim_versions is the projection's materialization of
-- the claim_version_refs array — one row per pinned version, position
-- carrying the array order. It is the references' queryable face:
-- reverse lookups (which findings pin this claim version — the impact
-- analysis input, docs/19 §3) are an index scan here, not a GIN array
-- search.
--
-- The guards enforce the mechanically decidable half of the model for
-- every ROW-LEVEL write path — INSERT, UPDATE and DELETE, whether from
-- application code, psql or a leaked credential — in the
-- 00014/00015/00028/00040 discipline. TRUNCATE is deliberately NOT
-- covered (see "Deliberately NOT enforced here").
--
-- The write paths in question are the ones that reach these two tables:
-- every INSERT, UPDATE and DELETE of a findings row or a ref row is
-- judged, and the judgement is made on the state at COMMIT (below). A
-- write to some OTHER table that could erode an invariant here — moving a
-- referenced claim object to another project, say — is not visible to any
-- trigger in this file and is that table's discipline, the same boundary
-- every cross-table guard in this schema (00040's, 00047's) draws.
--
-- What they enforce:
--
--   * every pinned ref must name a scientific object VERSION whose object
--     is a claim, in the finding's own project — a fixed claim version,
--     not a foreign object or another project's claim;
--   * every findings row must pin at least one claim version (the
--     schema's claim_version_refs minItems 1), and a ref row may only
--     exist together with its findings row — no ref-less findings rows,
--     no orphan refs;
--   * every findings row must name the object its OWN version belongs to:
--     object_id is the denormalized owner of version_id, so a row that
--     paired another object's id with this version would mis-attribute
--     the finding in every object_id-keyed lookup (findings_object_idx
--     first among them);
--   * a finding can never silently lose what it aggregates. Three writes
--     would do it and all three are refused: deleting the last pinned ref,
--     deleting the findings row while refs remain, and RE-POINTING either
--     identity column — finding_claim_versions.finding_version_id or
--     findings.version_id — away from rows that still exist. The last one
--     is why those two guards carry an UPDATE OF <identity column> event:
--     a re-point is a move, and the refs of the version the row left
--     behind would otherwise be stranded (nothing else looks at OLD).
--
-- The triggers are DEFERRABLE INITIALLY DEFERRED constraint triggers:
-- they fire once, at COMMIT, when every row of the transaction is
-- visible, so a projection writer may write the findings row and its
-- refs in one transaction in any order (the 00040 parent/child shape).
-- The two UPDATE OF events are scoped to the identity column on purpose:
-- that column is the ONLY one their invariant depends on, so every UPDATE
-- that could break it is checked, and an UPDATE that does not name it
-- (finding_type, assessment, position, …) cannot orphan a ref. This is
-- also why they are not spelled as a bare UPDATE: a bare UPDATE event
-- would re-run the guard on a row that keeps its identity and refuse a
-- perfectly legal rewrite of it.
--
-- Deliberately NOT enforced here:
--
--   * TRUNCATE. A row-level trigger cannot see it, so the guards above
--     cover INSERT/UPDATE/DELETE and not truncation — and that is the
--     intended scope, not a hole to plug: truncate-then-repopulate is
--     exactly how a REBUILDABLE projection is rebuilt (docs/21 §5). The
--     claims projection (00041) carries no truncate guard either; the
--     append-only logs are the tables that do (00015);
--   * finding_type/assessment VALUES are CHECK-pinned (they are the
--     schema enums of finding.schema.json), but TRANSITIONS between them
--     are not: like claim assessment, docs/43 makes these positions, not
--     a linear ladder, and a new version is the legitimate way to change
--     one (00040's reasoning);
--   * negative_finding carries no extra constraint: what a negative
--     finding must link to (the contradicts/fails_to_reproduce edges of
--     docs/44) is a scientific judgement the relations validate per
--     write, not a projection-column rule.

CREATE TABLE findings (
  version_id uuid PRIMARY KEY REFERENCES scientific_object_versions(id) ON DELETE RESTRICT,
  object_id uuid NOT NULL REFERENCES scientific_objects(id) ON DELETE RESTRICT,
  -- finding_type is NULLABLE because the finding schema leaves it
  -- optional: finding.schema.json's required list names statement and
  -- claim_version_refs, not finding_type (only its enum is pinned). A
  -- projection row must be able to materialize every schema-legal
  -- version, so the column mirrors the schema's requiredness — the same
  -- rule that makes assessment nullable below. The claims table (00041)
  -- holds claim_type NOT NULL for the same rule in the other direction:
  -- claim.schema.json DOES require claim_type. The CHECK still refuses
  -- every value outside the enum; NULL means "no type stated".
  finding_type text CHECK (finding_type IN ('observation','comparison','trend','mechanistic_interpretation','negative_finding','integrated_conclusion')),
  assessment text CHECK (assessment IN ('preliminary','accepted','contested','unresolved','superseded','aborted')),
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE finding_claim_versions (
  finding_version_id uuid NOT NULL REFERENCES scientific_object_versions(id) ON DELETE RESTRICT,
  claim_version_id uuid NOT NULL REFERENCES scientific_object_versions(id) ON DELETE RESTRICT,
  position int NOT NULL DEFAULT 0 CHECK (position >= 0),
  PRIMARY KEY (finding_version_id, claim_version_id),
  UNIQUE (finding_version_id, position)
);

CREATE INDEX findings_object_idx ON findings(object_id);
CREATE INDEX findings_type_idx ON findings(finding_type);
CREATE INDEX finding_claim_versions_claim_idx ON finding_claim_versions(claim_version_id);

-- +goose StatementBegin
CREATE FUNCTION finding_claim_refs_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  finding_obj_type text;
  finding_project uuid;
  claim_obj_type text;
  claim_project uuid;
BEGIN
  SELECT so.object_type, so.project_id INTO finding_obj_type, finding_project
    FROM scientific_object_versions sov
    JOIN scientific_objects so ON so.id = sov.object_id
   WHERE sov.id = NEW.finding_version_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'finding claim ref: finding version % does not exist',
      NEW.finding_version_id
      USING ERRCODE = 'P0001';
  END IF;
  IF finding_obj_type <> 'finding' THEN
    RAISE EXCEPTION 'finding claim ref: version % is a %, not a finding version',
      NEW.finding_version_id, finding_obj_type
      USING ERRCODE = 'P0001';
  END IF;
  IF NOT EXISTS (SELECT 1 FROM findings WHERE version_id = NEW.finding_version_id) THEN
    RAISE EXCEPTION 'finding claim ref: no findings projection row for version %',
      NEW.finding_version_id
      USING ERRCODE = 'P0001';
  END IF;

  SELECT so.object_type, so.project_id INTO claim_obj_type, claim_project
    FROM scientific_object_versions sov
    JOIN scientific_objects so ON so.id = sov.object_id
   WHERE sov.id = NEW.claim_version_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'finding claim ref: claim version % does not exist',
      NEW.claim_version_id
      USING ERRCODE = 'P0001';
  END IF;
  IF claim_obj_type <> 'claim' THEN
    RAISE EXCEPTION 'finding claim ref: version % is a %, not a claim version',
      NEW.claim_version_id, claim_obj_type
      USING ERRCODE = 'P0001';
  END IF;
  IF claim_project IS DISTINCT FROM finding_project THEN
    RAISE EXCEPTION 'finding claim ref: claim version % belongs to a different project',
      NEW.claim_version_id
      USING ERRCODE = 'P0001';
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER finding_claim_versions_ref_guard
  AFTER INSERT OR UPDATE ON finding_claim_versions
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION finding_claim_refs_guard();

-- +goose StatementBegin
CREATE FUNCTION finding_refs_present_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM finding_claim_versions
                  WHERE finding_version_id = NEW.version_id) THEN
    RAISE EXCEPTION 'finding %: a finding must pin at least one claim version (claim_version_refs)',
      NEW.version_id
      USING ERRCODE = 'P0001';
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER findings_refs_present
  AFTER INSERT OR UPDATE ON findings
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION finding_refs_present_guard();

-- +goose StatementBegin
CREATE FUNCTION finding_version_object_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM scientific_object_versions
                  WHERE id = NEW.version_id AND object_id = NEW.object_id) THEN
    RAISE EXCEPTION 'finding %: object_id % is not the object of version %',
      NEW.version_id, NEW.object_id, NEW.version_id
      USING ERRCODE = 'P0001';
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER findings_version_object_pairing
  AFTER INSERT OR UPDATE ON findings
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION finding_version_object_guard();

-- +goose StatementBegin
CREATE FUNCTION finding_no_orphan_refs_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  -- Fires on DELETE and on UPDATE OF version_id. On UPDATE the guard only
  -- has something to say if the identity column actually moved: a rewrite
  -- that keeps it (an upsert echoing version_id = EXCLUDED.version_id, a
  -- no-op assignment) strands nothing and must not be refused.
  IF TG_OP = 'UPDATE' THEN
    IF OLD.version_id IS NOT DISTINCT FROM NEW.version_id THEN
      RETURN NEW;
    END IF;
  END IF;
  IF EXISTS (SELECT 1 FROM finding_claim_versions
              WHERE finding_version_id = OLD.version_id) THEN
    RAISE EXCEPTION 'finding %: its claim version refs must be removed before the projection row that names it goes away or is re-pointed',
      OLD.version_id
      USING ERRCODE = 'P0001';
  END IF;
  RETURN OLD;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER findings_no_orphan_refs
  AFTER DELETE OR UPDATE OF version_id ON findings
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION finding_no_orphan_refs_guard();

-- +goose StatementBegin
CREATE FUNCTION finding_refs_remain_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  -- Fires on DELETE and on UPDATE OF finding_version_id. On UPDATE the ref
  -- must have been moved to a DIFFERENT finding to be able to strand the
  -- one it left (an upsert echoing finding_version_id stranded nothing).
  IF TG_OP = 'UPDATE' THEN
    IF OLD.finding_version_id IS NOT DISTINCT FROM NEW.finding_version_id THEN
      RETURN NEW;
    END IF;
  END IF;
  IF EXISTS (SELECT 1 FROM findings WHERE version_id = OLD.finding_version_id)
     AND NOT EXISTS (SELECT 1 FROM finding_claim_versions
                      WHERE finding_version_id = OLD.finding_version_id) THEN
    RAISE EXCEPTION 'finding %: removing the last claim version ref leaves the finding without claim refs',
      OLD.finding_version_id
      USING ERRCODE = 'P0001';
  END IF;
  RETURN OLD;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER finding_claim_versions_refs_remain
  AFTER DELETE OR UPDATE OF finding_version_id ON finding_claim_versions
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION finding_refs_remain_guard();

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
