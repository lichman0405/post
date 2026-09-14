-- +goose Up
-- Research Question ↔ Hypothesis relationship guards (task T0501).
--
-- The question↔hypothesis model rests on four structural references:
--
--   1. hypothesis.question_id names the research question the hypothesis
--      answers (hypothesis.schema.json makes it required; docs/08 gives
--      Hypothesis a question_ref);
--   2. research_question.parent_question_id nests a sub-question under
--      its parent (research_question.schema.json) — this is what makes
--      subquestions structural, not just textual;
--   3. relation "addresses_question" (knowledge category, docs/44): a
--      hypothesis addresses the target research question;
--   4. relation "tests_hypothesis" (knowledge category, docs/44): an
--      experiment or calculation tests the target hypothesis.
--
-- The endpoint-type table declares the endpoint object types the two
-- knowledge edges' NAMES pin (the docs/44 semantics lines): "addresses
-- the target research question" pins addresses_question's target to
-- research_question; "tests the target hypothesis" pins tests_hypothesis's
-- target to hypothesis. Only the end the name states is declared — the
-- source end stays NULL ("unconstrained"), because no spec says who may
-- point at a question or a hypothesis: inventing a source set turned out
-- to contradict the main-line domain model (findings address questions;
-- T0209's rsg_query_test pins that shape). The table mirrors the Go
-- relation catalog (internal/rsg/relationcatalog); an integration test
-- pins the two to each other so the DB declaration can never drift from
-- the catalog. Relations whose type is not declared there are untouched —
-- the catalog itself remains the authority on which relation types
-- exist. A full endpoint-declaration spec for core edges is a separate
-- ADR-level decision (issue #183), not this task's.
--
-- The guards enforce the mechanically decidable half of the model for ANY
-- write path (application code, psql, a leaked credential), in the
-- 00014/00015/00028 discipline: the app services validate on the happy
-- path, the database makes the invariant unconditional.
--
-- Deliberately NOT enforced here:
--
--   * question_state (research_question) and assessment (hypothesis)
--     enums: they are schema-governed scientific content, and they are
--     NOT issue states. An issue row (open → closed) is project
--     coordination; closing an issue never touches a question's or
--     hypothesis's versioned content, so nothing here reads issues.
--     A DB CHECK on the enums would couple every future schema enum
--     change to a migration; the schemareg + gate ladder already validate
--     them per write.
--   * transitions between question_state/assessment values: docs/43 says
--     these assessments are not linear upgrade levels, and an object's
--     lifecycle already allows active → aborted → reopened, so a content
--     regression is a legitimate new version, not an illegal transition.
--
-- The triggers are DEFERRABLE INITIALLY DEFERRED constraint triggers:
-- they fire once, at COMMIT, when every row of the transaction is
-- visible, so a parent and child (or an object and its knowledge
-- relation) may be written in one transaction in any order.

CREATE TABLE knowledge_relation_endpoint_types (
  relation_type text PRIMARY KEY,
  source_object_types text[], -- NULL = the source end is unconstrained
  target_object_types text[] NOT NULL
);

INSERT INTO knowledge_relation_endpoint_types
  (relation_type, source_object_types, target_object_types)
VALUES
  ('addresses_question', NULL, ARRAY['research_question']),
  ('tests_hypothesis', NULL, ARRAY['hypothesis']);

-- +goose StatementBegin
CREATE FUNCTION knowledge_relation_endpoint_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  allowed_src text[];
  allowed_tgt text[];
  rel_project uuid;
  src_type text;
  src_project uuid;
  tgt_type text;
  tgt_project uuid;
BEGIN
  SELECT source_object_types, target_object_types
    INTO allowed_src, allowed_tgt
    FROM knowledge_relation_endpoint_types
   WHERE relation_type = NEW.relation_type;
  IF NOT FOUND THEN
    -- Not a declared knowledge relation: nothing to enforce here.
    RETURN NEW;
  END IF;

  SELECT project_id INTO rel_project FROM relations WHERE id = NEW.relation_id;

  SELECT so.object_type, so.project_id INTO src_type, src_project
    FROM scientific_object_versions sov
    JOIN scientific_objects so ON so.id = sov.object_id
   WHERE sov.id = NEW.source_object_version_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'knowledge relation %: source object version % does not exist',
      NEW.relation_type, NEW.source_object_version_id
      USING ERRCODE = 'P0001';
  END IF;

  SELECT so.object_type, so.project_id INTO tgt_type, tgt_project
    FROM scientific_object_versions sov
    JOIN scientific_objects so ON so.id = sov.object_id
   WHERE sov.id = NEW.target_object_version_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'knowledge relation %: target object version % does not exist',
      NEW.relation_type, NEW.target_object_version_id
      USING ERRCODE = 'P0001';
  END IF;

  -- A knowledge edge is a claim about one project's research: both
  -- endpoints must live in the relation's project.
  IF src_project IS DISTINCT FROM rel_project THEN
    RAISE EXCEPTION 'knowledge relation %: source object version % belongs to a different project',
      NEW.relation_type, NEW.source_object_version_id
      USING ERRCODE = 'P0001';
  END IF;
  IF tgt_project IS DISTINCT FROM rel_project THEN
    RAISE EXCEPTION 'knowledge relation %: target object version % belongs to a different project',
      NEW.relation_type, NEW.target_object_version_id
      USING ERRCODE = 'P0001';
  END IF;

  -- NULL means the end is unconstrained: only the end the edge's NAME
  -- states is ever declared, so the source end is NULL for both edges
  -- and the check below only fires once a declaration exists.
  IF allowed_src IS NOT NULL AND NOT (src_type = ANY(allowed_src)) THEN
    RAISE EXCEPTION 'knowledge relation %: source object type % is not allowed (allowed: %)',
      NEW.relation_type, src_type, allowed_src
      USING ERRCODE = 'P0001';
  END IF;
  IF allowed_tgt IS NOT NULL AND NOT (tgt_type = ANY(allowed_tgt)) THEN
    RAISE EXCEPTION 'knowledge relation %: target object type % is not allowed (allowed: %)',
      NEW.relation_type, tgt_type, allowed_tgt
      USING ERRCODE = 'P0001';
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER relation_versions_knowledge_endpoints
  AFTER INSERT ON relation_versions
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION knowledge_relation_endpoint_guard();

-- +goose StatementBegin
CREATE FUNCTION scientific_object_reference_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  obj_type text;
  obj_project uuid;
  ref text;
  ref_field text;
  target_type text;
  target_project uuid;
  cur text;
  parent_of_cur text;
  visited text[];
  i int;
BEGIN
  SELECT object_type, project_id INTO obj_type, obj_project
    FROM scientific_objects WHERE id = NEW.object_id;
  IF NOT FOUND THEN
    -- Cannot happen through the FK; guard against misleading errors.
    RETURN NEW;
  END IF;

  IF obj_type = 'research_question' THEN
    ref_field := 'parent_question_id';
  ELSIF obj_type = 'hypothesis' THEN
    ref_field := 'question_id';
  ELSE
    RETURN NEW;
  END IF;

  -- Absent (or JSON null, or empty/whitespace) means "no reference": a
  -- draft hypothesis may not know its question yet, a root question has
  -- no parent. The gate ladder decides whether that omission is
  -- acceptable at this point; the guard only polices a reference that IS
  -- given (an explicitly empty string is still a hard error in the
  -- application-level semantic checks, which run before the write).
  ref := NULLIF(btrim(NEW.payload->>ref_field), '');
  IF ref IS NULL THEN
    RETURN NEW;
  END IF;

  IF ref !~ '^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$' THEN
    RAISE EXCEPTION 'scientific object % (%): % reference % is not a uuid',
      NEW.object_id, obj_type, ref_field, ref
      USING ERRCODE = 'P0001';
  END IF;

  SELECT object_type, project_id INTO target_type, target_project
    FROM scientific_objects WHERE id = ref::uuid;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'scientific object % (%): % reference % does not name an existing object',
      NEW.object_id, obj_type, ref_field, ref
      USING ERRCODE = 'P0001';
  END IF;
  IF target_type <> 'research_question' THEN
    RAISE EXCEPTION 'scientific object % (%): % reference % names a % object, not a research_question',
      NEW.object_id, obj_type, ref_field, ref, target_type
      USING ERRCODE = 'P0001';
  END IF;
  IF target_project IS DISTINCT FROM obj_project THEN
    RAISE EXCEPTION 'scientific object % (%): % reference % names a research question of another project',
      NEW.object_id, obj_type, ref_field, ref
      USING ERRCODE = 'P0001';
  END IF;

  -- Parent chains must be acyclic. Walk up from the named parent through
  -- each question's LATEST version row (max(version_no) is the log's own
  -- truth; current_version_no is a projection only the repository
  -- maintains, so a guard that must hold for ANY write path cannot trust
  -- it). A revisit is a cycle and the depth is capped at 64 (a legitimate
  -- question hierarchy is nowhere near that).
  IF obj_type = 'research_question' THEN
    cur := ref;
    visited := ARRAY[NEW.object_id::text];
    FOR i IN 1..64 LOOP
      IF cur = NEW.object_id::text THEN
        RAISE EXCEPTION 'scientific object % (research_question): parent chain forms a cycle through %',
          NEW.object_id, cur
          USING ERRCODE = 'P0001';
      END IF;
      IF cur = ANY(visited) THEN
        RAISE EXCEPTION 'scientific object % (research_question): parent chain forms a cycle at %',
          NEW.object_id, cur
          USING ERRCODE = 'P0001';
      END IF;
      visited := array_append(visited, cur);
      SELECT sov.payload->>'parent_question_id'
        INTO parent_of_cur
        FROM scientific_object_versions sov
       WHERE sov.object_id = cur::uuid
       ORDER BY sov.version_no DESC
       LIMIT 1;
      IF parent_of_cur IS NULL OR btrim(parent_of_cur) = '' THEN
        RETURN NEW; -- the chain ends at a root question
      END IF;
      IF parent_of_cur !~ '^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$' THEN
        -- An ancestor row written before this guard existed may carry a
        -- non-uuid parent; the chain cannot be walked through it, and
        -- refusing every new subquestion over old garbage would be wrong.
        RETURN NEW;
      END IF;
      cur := parent_of_cur;
    END LOOP;
    RAISE EXCEPTION 'scientific object % (research_question): parent chain exceeds the depth cap of 64',
      NEW.object_id
      USING ERRCODE = 'P0001';
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER scientific_object_versions_reference_guard
  AFTER INSERT ON scientific_object_versions
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION scientific_object_reference_guard();

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
