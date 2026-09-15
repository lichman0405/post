-- +goose Up
-- Contribution opportunities (task T0803, docs/02 "Open Network — Open
-- Contribution Opportunity").
--
-- A maintainer marks an issue or a research need (a research_question
-- scientific object) as open for contribution, with difficulty and
-- capability metadata; a suggestion from an agent or a member lands as
-- state 'suggested' and becomes real only through a maintainer's
-- approval. The row's lifecycle and its visibility are two separate
-- machines:
--
--   1. state: suggested → open → closed. 'suggested' is the creation
--      state of a suggestion (suggested_by set); 'open' is the creation
--      state of a maintainer mark (suggested_by NULL — a marked row is
--      not a suggestion, and approved_by only exists on rows that were
--      suggested); 'closed' is terminal (a rejected suggestion, or a
--      fulfilled/withdrawn opportunity — the row stays as history,
--      nothing is deleted; a re-mark creates a new row).
--   2. visibility: internal → public. A new row is ALWAYS internal;
--      publicizing is a docs/12 §3 visibility widening ("任何
--      private→public … 都要求有权限的人显式确认，并产生 audit/event。
--      Agent/MCP 不得自动执行此类扩大操作"): it is a separate,
--      explicit action, stamped with publicized_by/publicized_at, and
--      enforced here for ANY write path — a row becomes public only
--      through the one adapter path that sets the transaction-scoped
--      session flag post.co_publicize='on' (the same sanctioned-path
--      discipline as 00051's PR head refresh), and public is terminal
--      (no un-publish in V1).
--
-- A publicized row is frozen except for its state: metadata (title,
-- description, difficulty, capabilities) and the publicize stamps are
-- immutable once public — the open network may index the row, so its
-- content must not drift underneath it. Metadata updates while internal
-- are allowed on any non-closed row.
--
-- The target is polymorphic (an issue row or a research_question
-- scientific object), so there is no FK on target_id; the deferred
-- constraint trigger below pins target existence project-scoped for any
-- write path (the 00040 discipline: it fires once, at COMMIT, when
-- every row of the transaction is visible). One active (non-closed)
-- opportunity per target is the partial unique index — the key the
-- creation insert conflicts on, so a target can never carry two open or
-- two suggested opportunities, while closed history rows coexist.
--
-- Scope of the publicize flag, stated precisely (the same honesty as
-- 00051's header): the flag blocks IMPLICIT or accidental writes by
-- application code — a rogue UPDATE without the flag fails, and no
-- query in this schema sets the flag. It is NOT a defense against
-- hostile or leaked SQL credentials: any session with UPDATE privilege
-- on contribution_opportunities can run
-- set_config('post.co_publicize','on',true) itself. Everything else in
-- this migration (the CHECKs, the state map, the identity/metadata
-- fixity, the target guard, the partial unique index) is unconditional
-- and holds even against such a session.

CREATE TABLE contribution_opportunities (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  target_type text NOT NULL CHECK (target_type IN ('issue','research_question')),
  target_id uuid NOT NULL,
  title text NOT NULL,
  description text NOT NULL DEFAULT '',
  difficulty text NOT NULL CHECK (difficulty IN ('beginner','intermediate','advanced')),
  required_capabilities text[] NOT NULL DEFAULT '{}' CHECK (cardinality(required_capabilities) <= 10),
  state text NOT NULL DEFAULT 'suggested' CHECK (state IN ('suggested','open','closed')),
  visibility text NOT NULL DEFAULT 'internal' CHECK (visibility IN ('internal','public')),
  created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  suggested_by uuid REFERENCES users(id) ON DELETE RESTRICT,
  approved_by uuid REFERENCES users(id) ON DELETE RESTRICT,
  publicized_by uuid REFERENCES users(id) ON DELETE RESTRICT,
  publicized_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

-- One active (suggested or open) opportunity per target; closed rows
-- stay as history (invariant 8: nothing disappears, state only evolves).
CREATE UNIQUE INDEX contribution_opportunities_target_active_idx
  ON contribution_opportunities (target_type, target_id)
  WHERE state IN ('suggested','open');

-- +goose StatementBegin
CREATE FUNCTION contribution_opportunity_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  valid_transition boolean;
  publicize_allowed boolean;
BEGIN
  IF TG_OP = 'INSERT' THEN
    -- A row is born internal, never public: publicizing is the separate
    -- explicit action below, and no INSERT may pre-stamp it.
    IF NEW.visibility <> 'internal' THEN
      RAISE EXCEPTION 'a contribution opportunity is created internal; publicizing is a separate, explicit action (docs/12 §3)'
        USING ERRCODE = 'P0001';
    END IF;
    IF NEW.publicized_by IS NOT NULL OR NEW.publicized_at IS NOT NULL
       OR NEW.approved_by IS NOT NULL THEN
      RAISE EXCEPTION 'publicized/approved stamps are earned by transitions, never set at creation'
        USING ERRCODE = 'P0001';
    END IF;
    -- A suggested row names its suggester; a maintainer mark is not a
    -- suggestion.
    IF NEW.state = 'suggested' AND NEW.suggested_by IS NULL THEN
      RAISE EXCEPTION 'a suggested opportunity names its suggester'
        USING ERRCODE = 'P0001';
    END IF;
    IF NEW.state = 'open' AND NEW.suggested_by IS NOT NULL THEN
      RAISE EXCEPTION 'a maintainer-marked opportunity is not a suggestion'
        USING ERRCODE = 'P0001';
    END IF;
  END IF;

  IF TG_OP = 'UPDATE' THEN
    -- Identity fixity: project, target and creators never move.
    IF NEW.project_id IS DISTINCT FROM OLD.project_id
       OR NEW.target_type IS DISTINCT FROM OLD.target_type
       OR NEW.target_id IS DISTINCT FROM OLD.target_id
       OR NEW.created_by IS DISTINCT FROM OLD.created_by
       OR NEW.suggested_by IS DISTINCT FROM OLD.suggested_by THEN
      RAISE EXCEPTION 'contribution opportunity identity is fixed: project, target and creators are immutable'
        USING ERRCODE = 'P0001';
    END IF;

    -- Approval is a one-way stamp earned by the suggested -> open move.
    IF NEW.approved_by IS DISTINCT FROM OLD.approved_by THEN
      IF OLD.approved_by IS NOT NULL THEN
        RAISE EXCEPTION 'approved_by is immutable once set'
          USING ERRCODE = 'P0001';
      END IF;
      IF NOT (OLD.state = 'suggested' AND NEW.state = 'open') THEN
        RAISE EXCEPTION 'approved_by is earned only by the suggested -> open approval'
          USING ERRCODE = 'P0001';
      END IF;
    END IF;

    -- The state machine: no skipping, closed is terminal.
    IF NEW.state IS DISTINCT FROM OLD.state THEN
      valid_transition :=
        (OLD.state = 'suggested' AND NEW.state IN ('open','closed'))
        OR (OLD.state = 'open' AND NEW.state = 'closed');
      IF NOT valid_transition THEN
        RAISE EXCEPTION 'illegal contribution opportunity state transition: % -> %',
          OLD.state, NEW.state
          USING ERRCODE = 'P0001';
      END IF;
    END IF;

    -- Visibility: only the explicit, audited internal -> public
    -- publicize, and only for an open row. State changes never touch
    -- visibility and public is terminal.
    IF NEW.visibility IS DISTINCT FROM OLD.visibility THEN
      publicize_allowed :=
        OLD.visibility = 'internal'
        AND NEW.visibility = 'public'
        AND OLD.state = 'open'
        AND coalesce(current_setting('post.co_publicize', true), '') = 'on'
        AND NEW.publicized_by IS NOT NULL
        AND NEW.publicized_at IS NOT NULL;
      IF NOT publicize_allowed THEN
        RAISE EXCEPTION 'a contribution opportunity is publicized only through the explicit publicize action (docs/12 §3: visibility widening is never automatic)'
          USING ERRCODE = 'P0001';
      END IF;
    END IF;

    -- Publicized rows are frozen for the open network: metadata and the
    -- publicize stamps are immutable once public (state may still move
    -- open -> closed, and the row stays public as history).
    IF OLD.visibility = 'public' THEN
      IF NEW.title IS DISTINCT FROM OLD.title
         OR NEW.description IS DISTINCT FROM OLD.description
         OR NEW.difficulty IS DISTINCT FROM OLD.difficulty
         OR NEW.required_capabilities IS DISTINCT FROM OLD.required_capabilities
         OR NEW.publicized_by IS DISTINCT FROM OLD.publicized_by
         OR NEW.publicized_at IS DISTINCT FROM OLD.publicized_at THEN
        RAISE EXCEPTION 'a publicized contribution opportunity is frozen: metadata and publicize stamps are immutable'
          USING ERRCODE = 'P0001';
      END IF;
    END IF;
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER contribution_opportunity_guard_trigger
  BEFORE INSERT OR UPDATE ON contribution_opportunities
  FOR EACH ROW EXECUTE FUNCTION contribution_opportunity_guard();

-- +goose StatementBegin
CREATE FUNCTION contribution_opportunity_target_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  found boolean;
BEGIN
  IF NEW.target_type = 'issue' THEN
    SELECT EXISTS (
      SELECT 1 FROM issues i
       WHERE i.id = NEW.target_id AND i.project_id = NEW.project_id
    ) INTO found;
  ELSE
    SELECT EXISTS (
      SELECT 1 FROM scientific_objects o
       WHERE o.id = NEW.target_id
         AND o.object_type = 'research_question'
         AND o.project_id = NEW.project_id
    ) INTO found;
  END IF;
  IF NOT found THEN
    RAISE EXCEPTION 'contribution opportunity target does not exist in the project (target_type %, project %, target %)',
      NEW.target_type, NEW.project_id, NEW.target_id
      USING ERRCODE = 'P0001';
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- Deferred constraint trigger (the 00040 discipline): the check fires
-- once, at COMMIT, when every row of the transaction is visible, so the
-- target and the opportunity may be written in one transaction in any
-- order. Because the identity guard makes the target immutable, only
-- INSERTs can ever trip it in practice.
CREATE CONSTRAINT TRIGGER contribution_opportunity_target_trigger
  AFTER INSERT OR UPDATE ON contribution_opportunities
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION contribution_opportunity_target_guard();

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
