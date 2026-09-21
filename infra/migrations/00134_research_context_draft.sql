-- +goose Up
-- The Draft Research Context (T0908): the candidate a search answer becomes
-- before it is a project.
--
-- docs/14:25 is the whole specification of this row, verbatim: "Start
-- Research Project 创建 Draft Research Context：Research Question、referenced
-- knowledge、dependencies、candidate materials、known uncertainties、
-- agent-suggested hypotheses。用户确认后才形成 initial state。" docs/31:36
-- lists the flow as Gate E ("Answer → Draft Research Context → new Project
-- 流程可用"). The columns below are those six things and nothing else.
--
-- # Why a table, and why the draft is not an object
--
-- docs/07:5 defines the RSG as a project's full state graph AT a state
-- version, and docs/14:25 puts the initial state AFTER confirmation. So a
-- draft is not an RSG node, not a scientific object, and not a state: it is
-- the candidate those are built from. Nothing here is versioned, nothing
-- here is a node in a graph, and no scientific_object_version, project_state
-- or state_commit row exists for a project that has only a draft — which is
-- exactly the property T0908 is asked to make checkable ("an agent answer
-- never writes main by itself") rather than asserted.
--
-- It is a row rather than a document because the two things that must hold
-- about it are relational: ONE search starts at most ONE draft (the unique
-- key below), and every ref a draft carries is a ref that search returned
-- (the trigger below). Neither is a statement about a JSON blob.
--
-- # Idempotency: one key, one draft, and one draft per search
--
-- Both routes of the flow carry a contract-required Idempotency-Key
-- (specs/api/openapi.yaml, components.parameters.IdempotencyKey: minLength
-- 8). This table holds the start route's key; the confirm route's key lives
-- in its own column below, because the two calls are two different
-- creations and a key burned by one must not replay the other.
--
--   * UNIQUE (search_id) is the strong one: a search is answered once, and
--     the draft built from it exists once, under ANY key. It is what makes
--     "a replay resumes the same draft instead of opening a second project"
--     true against a writer that lost the read-then-write race, and it is
--     why a DIFFERENT key for a search that was already started is refused
--     instead of silently opening a second project.
--   * UNIQUE (created_by, idempotency_key) is the client's own key
--     discipline: the same actor presenting the same key for a different
--     search is a request that cannot be a replay, and 00072/00083's
--     creation ledgers refuse that shape (IDEMPOTENCY_CONFLICT) rather than
--     letting one key name two creations.
--
-- Both keys are stored, never hashed and never truncated: the key IS the
-- record the replay is compared against, and a platform that stored a digest
-- could not tell the caller which key it had already used.
--
-- # The refs are text[], and the answer's set is the boundary
--
-- referenced/dependency/candidate refs are the caller's KEEP/DROP/
-- RECLASSIFY decisions over the search record's selected_refs, so they are
-- sets of refs and are stored as sets. The invariant — every ref a draft
-- carries was returned by the search it was built from — cannot be a CHECK:
-- PostgreSQL CHECK constraints cannot read another table. It is enforced by
-- research_context_draft_refs_guard() below, the database-side refusal every
-- writer meets (the write path's own pre-check refuses earlier and names the
-- offending ref, exactly the guarding pair internal/search/answer keeps
-- between its in-Go guard and 00121's CHECK).
--
-- Nothing is derived from the search record's ANSWER here beyond that
-- containment: the caller may keep, drop or reclassify, and docs/14:5 gives
-- the draft its own six fields. In particular `hypotheses` and
-- `uncertainties` have no other home in V1 — no object type and no schema
-- field takes them (docs/14:25 names them as parts of the draft and no spec
-- promotes them), so they live, and stay, in these two columns.
--
-- # No index
--
-- Reads are by primary key (the confirm route) and by search_id (the replay
-- read-back), and UNIQUE (search_id) is the second one's index. A third
-- index for a read the contract does not have is a write cost on every
-- start-project for nothing (the argument 00121 records for its own table).
CREATE TABLE research_context_drafts (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  -- The project this draft is for. It exists from the moment the draft does:
  -- docs/14:5 creates the project in planning together with the draft, and
  -- the draft is what the project's initial state will be built from. A
  -- project row cannot be deleted (nothing disappears, CLAUDE.md §9.8), so a
  -- draft naming one always names a live project.
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  -- The answered search this draft was built from. It is NOT NULL because
  -- the draft's whole substrate is that search: its selected_refs are the
  -- set the draft's refs are drawn from (the trigger), and its actor is who
  -- the draft belongs to. RESTRICT like every other reference to the record:
  -- a search that a draft was built from is part of that draft's evidence
  -- and cannot be removed from under it.
  search_id uuid NOT NULL REFERENCES search_records(id) ON DELETE RESTRICT,
  created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  -- The research question, verbatim from the caller. Non-blank (the CHECK):
  -- a draft whose question is empty is a project with no question, which is
  -- not what docs/14:5 creates. Length and shape are the research_question
  -- schema's business at confirm time (minLength 3), not this table's.
  research_question text NOT NULL CHECK (btrim(research_question) <> ''),
  -- The three ref lists of docs/14:5: the knowledge the draft references,
  -- the dependencies it declares, the candidate materials it proposes. All
  -- three are subsets of the search's selected_refs (the trigger below);
  -- empty is a real state (a caller may keep none of them) and is stored as
  -- an empty array rather than as NULL.
  referenced_refs text[] NOT NULL,
  dependency_refs text[] NOT NULL,
  candidate_refs text[] NOT NULL,
  -- docs/14:5's "known uncertainties" and "agent-suggested hypotheses". Free
  -- text on purpose: no schema in specs/schemas defines an uncertainty or a
  -- draft hypothesis document, and inventing one here would be inventing the
  -- product's vocabulary (CLAUDE.md §5: L3). They stay on the draft — see
  -- the header for why that is the honest home, not a missing one.
  uncertainties text[] NOT NULL,
  hypotheses text[] NOT NULL,
  -- draft -> confirmed, once (the confirmation guard below). There is no
  -- 'abandoned': a draft the caller walks away from is a draft nobody
  -- confirmed, and CLAUDE.md §9.8's "nothing disappears" means the row stays
  -- readable rather than being moved to a terminal state to say so.
  status text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'confirmed')),
  -- The start route's key (see the header). Non-blank and bounded below by
  -- the contract's minLength 8: a stored key is a deliberate token, never a
  -- stray empty string.
  idempotency_key text NOT NULL CHECK (length(idempotency_key) >= 8),
  -- The confirm route's key, NULL until the confirmation happens. Separate
  -- from idempotency_key on purpose: the two calls are two creations, and a
  -- key that started a project must not be replayable as a confirmation of
  -- it (nor the reverse).
  confirm_idempotency_key text CHECK (confirm_idempotency_key IS NULL
                                      OR length(confirm_idempotency_key) >= 8),
  -- The confirmation record: who confirmed, when, and what the confirmation
  -- created. Every column is NULL for a draft that has not been confirmed
  -- and NOT NULL once it has (the all-or-nothing CHECK below), and each one
  -- is a row id a reader can follow: the branch, the state the transition
  -- produced, the state commit that names the transition (docs/09 §2), and
  -- the research_question object version the transition created. The ids are
  -- recorded rather than left derivable because the confirm route's replay
  -- must answer the FIRST call's result (specs/api/openapi.yaml: a replay
  -- resumes the same confirmation), and a response reconstructed from a
  -- later read would be a claim about what the platform would say now.
  confirmed_at timestamptz,
  confirmed_by uuid REFERENCES users(id) ON DELETE RESTRICT,
  initial_branch_id uuid REFERENCES branches(id) ON DELETE RESTRICT,
  initial_state_id uuid REFERENCES project_states(id) ON DELETE RESTRICT,
  initial_commit_id uuid REFERENCES state_commits(id) ON DELETE RESTRICT,
  question_object_id uuid REFERENCES scientific_objects(id) ON DELETE RESTRICT,
  question_version_id uuid REFERENCES scientific_object_versions(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),
  -- One draft per search, under any key (the header).
  CONSTRAINT research_context_drafts_search_key UNIQUE (search_id),
  -- One draft per key, per actor (the header).
  CONSTRAINT research_context_drafts_start_key UNIQUE (created_by, idempotency_key),
  -- The confirmation is all-or-nothing, and it is tied to the status it
  -- describes: a row with a confirmation record is a row whose status IS
  -- 'confirmed', and a confirmed row carries the whole record. A confirmed
  -- draft whose branch or commit were missing would be a confirmation
  -- nobody can trace (the acceptance criterion "确认后状态可追溯"), and the
  -- CHECK is what makes that state unstorable rather than merely unwritten.
  CONSTRAINT research_context_drafts_confirmation_shape CHECK (
    (status = 'confirmed') = (confirmed_at IS NOT NULL)
    AND (status = 'confirmed') = (confirmed_by IS NOT NULL)
    AND (status = 'confirmed') = (confirm_idempotency_key IS NOT NULL)
    AND (status = 'confirmed') = (initial_branch_id IS NOT NULL)
    AND (status = 'confirmed') = (initial_state_id IS NOT NULL)
    AND (status = 'confirmed') = (initial_commit_id IS NOT NULL)
    AND (status = 'confirmed') = (question_object_id IS NOT NULL)
    AND (status = 'confirmed') = (question_version_id IS NOT NULL)
  )
);

-- The confirm route's key discipline, the same rule as the start key's
-- constraint above and for the same reason: one key names one confirmation.
-- It is scoped to the CONFIRMING actor (confirmed_by, not created_by — the
-- drafter and the confirmer are often different people, and a draft's creator
-- is not necessarily its project's maintainer) and PARTIAL on the key being
-- set, because a draft that has not been confirmed carries no confirmation
-- key and must not be constrained by one.
--
-- Two confirmations of TWO different drafts under one key are therefore
-- refused rather than silently performed: a client that reuses a key believes
-- it is retrying, and the answer to a retry is the confirmation it already
-- made, never a second one. A repeat of the same key on the SAME draft never
-- reaches this index — it is answered as a replay by the write path.
CREATE UNIQUE INDEX research_context_drafts_confirm_key_uniq
  ON research_context_drafts (confirmed_by, confirm_idempotency_key)
  WHERE confirm_idempotency_key IS NOT NULL;

-- +goose StatementBegin
-- The containment invariant, in the database.
--
-- Every ref a draft carries must be one the search record returned
-- (search_records.selected_refs, 00121 — the set the answer was allowed to
-- cite). The caller may keep, drop or reclassify refs WITHIN that set and
-- may not add one: a draft that named a ref the search never returned would
-- be an ungrounded citation wearing a draft's clothes, which is the
-- top-severity scenario docs/54 ranks fabricated or unauthorized citations
-- as.
--
-- It is a trigger because the rule is a statement about TWO tables and
-- PostgreSQL's CHECK cannot read another row. ERRCODE P0001 (raise
-- exception) is the vocabulary the repository's other cross-table guards use
-- (00028, 00086, 00040), so a caller sees one class of refusal for "the
-- database refused this on a rule of its own".
CREATE FUNCTION research_context_draft_refs_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  selected text[];
  stranger text;
BEGIN
  SELECT sr.selected_refs INTO selected
    FROM search_records sr
   WHERE sr.id = NEW.search_id;

  -- The foreign key guarantees the row exists; this branch exists so the
  -- function says what it means rather than comparing against NULL (which
  -- would make every ref a stranger and raise the wrong message).
  IF selected IS NULL THEN
    RAISE EXCEPTION 'research_context_drafts.search_id % names no search record', NEW.search_id
      USING ERRCODE = 'P0001';
  END IF;

  SELECT ref INTO stranger
    FROM unnest(NEW.referenced_refs || NEW.dependency_refs || NEW.candidate_refs) AS ref
   WHERE NOT (ref = ANY(selected))
   LIMIT 1;

  IF stranger IS NOT NULL THEN
    RAISE EXCEPTION 'research context draft ref % was not returned by search % (refs may only be chosen from the search''s selected_refs)', stranger, NEW.search_id
      USING ERRCODE = 'P0001';
  END IF;

  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER research_context_drafts_refs_guard
  BEFORE INSERT OR UPDATE ON research_context_drafts
  FOR EACH ROW EXECUTE FUNCTION research_context_draft_refs_guard();

-- +goose StatementBegin
-- The confirmation is a one-shot transition, and the draft's own facts are
-- immutable.
--
-- Two rules, one function, because both are about the same row and both are
-- "this is a record, not a scratch pad":
--
--   1. draft -> confirmed, once. A confirmed draft cannot be confirmed
--      again (the record states what the confirmation created, and a second
--      write would rewrite history to say something else), and it cannot go
--      back to 'draft' (there is no un-confirm: the initial state exists
--      from the moment of confirmation and CLAUDE.md §9.8's "state only
--      evolves" is the same rule here). The write path's compare-and-swap
--      (UPDATE ... WHERE status = 'draft') is what serializes two
--      confirmations; this is the second refusal, for a writer that does not
--      use it.
--   2. The draft's identity and content are frozen at insert: the project,
--      the search it came from, who created it, the research question, the
--      refs and the two free-text lists never move. A draft whose content
--      could be edited would make the confirmation a confirmation of
--      something else than what the caller read.
--
-- DELETE is refused for the same reason 00086 refuses it on fork lineage: a
-- draft is the record of how a project began, and "nothing disappears; state
-- only evolves" (CLAUDE.md §9.8) applies to it as much as to a scientific
-- object. A draft nobody confirmed stays a draft.
CREATE FUNCTION research_context_draft_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'research_context_drafts is append-only: DELETE is forbidden (a draft is the record of how a project began)'
      USING ERRCODE = 'P0001';
  END IF;

  IF NEW.project_id IS DISTINCT FROM OLD.project_id
     OR NEW.search_id IS DISTINCT FROM OLD.search_id
     OR NEW.created_by IS DISTINCT FROM OLD.created_by
     OR NEW.research_question IS DISTINCT FROM OLD.research_question
     OR NEW.referenced_refs IS DISTINCT FROM OLD.referenced_refs
     OR NEW.dependency_refs IS DISTINCT FROM OLD.dependency_refs
     OR NEW.candidate_refs IS DISTINCT FROM OLD.candidate_refs
     OR NEW.uncertainties IS DISTINCT FROM OLD.uncertainties
     OR NEW.hypotheses IS DISTINCT FROM OLD.hypotheses
     OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
     OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
    RAISE EXCEPTION 'research_context_drafts identity and content are immutable: only the confirmation may be written, once'
      USING ERRCODE = 'P0001';
  END IF;

  IF OLD.status = 'confirmed' AND NEW.status IS DISTINCT FROM OLD.status THEN
    RAISE EXCEPTION 'a confirmed research context draft cannot leave the confirmed status (there is no un-confirm)'
      USING ERRCODE = 'P0001';
  END IF;

  IF OLD.status = 'confirmed' THEN
    RAISE EXCEPTION 'research context draft % is already confirmed', OLD.id
      USING ERRCODE = 'P0001';
  END IF;

  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER research_context_drafts_guard
  BEFORE UPDATE OR DELETE ON research_context_drafts
  FOR EACH ROW EXECUTE FUNCTION research_context_draft_guard();

COMMENT ON TABLE research_context_drafts IS
  'Draft Research Context (T0908, docs/14:25): the candidate — research question, referenced knowledge, dependencies, candidate materials, uncertainties and agent-suggested hypotheses — that a search answer becomes when the reader clicks "Start Research Project". It is NOT an RSG node: the project''s initial state forms only on confirmation, so a project with a draft and no confirmation has no branch, no state and no scientific object.';
COMMENT ON COLUMN research_context_drafts.search_id IS
  'The answered search the draft was built from (search_records, 00121). Its selected_refs are the only refs the draft may carry (research_context_draft_refs_guard), so the draft cannot name a source the answer did not have.';
COMMENT ON COLUMN research_context_drafts.confirm_idempotency_key IS
  'The confirm route''s Idempotency-Key, written once at confirmation. A repeat of the same key answers the confirmation it names; a different key on an already-confirmed draft is refused (the draft is not in a state that can be confirmed).';
COMMENT ON COLUMN research_context_drafts.initial_commit_id IS
  'The state commit that names the confirmation''s transition (docs/09 §2). From it a reader reaches the initial branch, the initial state and the research_question object version the transition created — the traceability the confirmation owes.';
COMMENT ON COLUMN research_context_drafts.question_version_id IS
  'The research_question object version the confirmation created (version 1 of a new object, through the ordinary RSG write path). The object is created at confirmation and never before: an unanswered question is a draft, a question in the project''s state is a decision.';
