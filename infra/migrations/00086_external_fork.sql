-- +goose Up
-- External fork lineage and the cross-project pull request (T0804).
--
-- docs/04 §2 states the rule this migration records structurally: "Public
-- Project 的非成员用户不是 Contributor role，但可 Fork 并从自己的空间发起外部
-- PR" — a non-member forks a public project into their OWN space and
-- proposes from there. specs/policies/permissions-matrix.csv writes the same
-- rule as three cells for authenticated_nonmember (create_branch =
-- external_fork_only, write_scientific_state = own_fork_only, open_pr =
-- allow_from_fork); this migration creates the two facts those conditions
-- are resolved against, so the application layer resolves a condition rather
-- than guessing one.
--
--   1. project_forks: the lineage record. One row per fork project. The
--      relation name is the canonical vocabulary's — 'forked_from', the
--      lineage/network type of internal/rsg/relationcatalog (docs/44) and a
--      member of asset_lineage's CHECK (00010); the CHECK here pins it so a
--      second, invented relation name cannot appear in the column, and the
--      column exists at all so the lineage's vocabulary is a stored fact
--      rather than an implication of the table's name.
--
--      A fork is a research-space relationship between two PROJECTS: the
--      fork project (child) and the project it was forked from (parent).
--      docs/31 Gate C requires "Asset PID/version/lineage/…/fork 工作"; the
--      project-level half of that lineage is what this table carries. The
--      asset-level half stays asset_lineage's (00010) — the two are
--      different subjects and this table does not touch that one.
--
--      The row is append-only except for ONE cell: forked_sha, the parent
--      commit the import landed in the fork's repository, is written once,
--      by the import, from NULL to a value. That one-shot update is the
--      compare-and-swap the import's idempotency is built on (the same
--      shape internal/persistence/main_freeze_store.go uses: an atomic
--      UPDATE, zero rows meaning "already done", then one read to explain
--      the outcome). Everything else — which projects, which branches, who
--      forked — is identity and never moves; the guard below enforces it
--      for ANY update path, application code and psql alike.
--
--      UNIQUE (parent_project_id, forked_by) is what makes a repeated fork
--      request idempotent at the database level: the second request's
--      INSERT hits the constraint, the service reads the existing row back
--      and returns it, and no second lineage row, audit row or event is
--      written. Two concurrent requests can never both win.
--
--   2. pull_request_fork_gate: the database half of "an external PR comes
--      from a fork". pull_requests has always carried source_branch_id and
--      target_branch_id with no constraint tying them to the PR's project —
--      the same-project rule lived only in the store — so an external PR is
--      representable. This gate makes the representation HONEST: a PR whose
--      source branch belongs to another project is refused unless that
--      project is a fork of the PR's project forked by the PR's creator.
--      It is a canonical-store-level constraint in the same discipline as
--      00042's semantic gates: it holds for any INSERT path, and it is what
--      keeps "allow_from_fork" from degrading into "allow from anywhere".
--
--      It sits BEFORE INSERT on pull_requests, beside 00042's
--      pull_request_semantic_gate — which checks the SOURCE branch's
--      semantic completeness. The two gates are complementary and both are
--      needed for the fork path: this one decides whether the source branch
--      may propose to this project at all, that one decides whether the
--      source branch's recorded content is understood well enough to open a
--      formal PR.
--
-- No existing constraint is changed or dropped.

CREATE TABLE project_forks (
  -- The fork project: the project created in the forker's own space. One
  -- fork project has one parent (PRIMARY KEY), so a fork can never claim
  -- two origins.
  fork_project_id uuid PRIMARY KEY REFERENCES projects(id) ON DELETE RESTRICT,
  -- The project the fork was taken from.
  parent_project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  -- The actor who forked. Resolution of the matrix's fork conditions is
  -- about THIS actor's own fork (docs/04 §2: "从自己的空间"), so the row
  -- names them rather than inferring ownership from a membership role that
  -- can change under the row.
  forked_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  -- The canonical lineage relation name (internal/rsg/relationcatalog,
  -- docs/44). A fork is a 'forked_from' edge and nothing else in V1.
  relation_type text NOT NULL DEFAULT 'forked_from'
    CHECK (relation_type = 'forked_from'),
  -- The parent branch the content was imported from, and the fork branch
  -- that received it. Both are the branches the import binds together; the
  -- lineage is not a bare project-to-project edge, it names the research
  -- paths the content travelled along.
  source_branch_id uuid NOT NULL REFERENCES branches(id) ON DELETE RESTRICT,
  fork_branch_id uuid NOT NULL REFERENCES branches(id) ON DELETE RESTRICT,
  -- The parent commit the import landed in the fork's repository. NULL
  -- until the import has run; the import's compare-and-swap writes it
  -- exactly once (see the guard below). It is a recorded observation of the
  -- provider-side fact, which is why it is nullable and not derived.
  forked_sha text,
  created_at timestamptz NOT NULL DEFAULT now(),
  -- A project cannot be a fork of itself: the row would be a cycle of one.
  CHECK (fork_project_id <> parent_project_id),
  -- The two branches are different research paths in different projects.
  CHECK (source_branch_id <> fork_branch_id),
  -- The idempotency key: one fork of a parent per actor. A repeated fork
  -- request collides here instead of creating a second project, lineage
  -- row, audit row or event; two concurrent requests cannot both win. The
  -- store's claim infers this index by its columns
  -- (ON CONFLICT (parent_project_id, forked_by) DO NOTHING) — the name is
  -- pinned so a later migration that moved the key could not leave that
  -- clause silently inferring a different index.
  CONSTRAINT project_forks_parent_actor_key UNIQUE (parent_project_id, forked_by)
);

COMMENT ON TABLE project_forks IS
  'External fork lineage (T0804): the fork project created in a non-member''s own space, the parent project it was forked from, the actor who forked, and the branch pair the content was imported along. relation_type is the canonical vocabulary''s ''forked_from'' (docs/44). Append-only except forked_sha, which the import writes once under a compare-and-swap.';

COMMENT ON COLUMN project_forks.forked_sha IS
  'The parent commit the fork import landed in the fork project''s GitProvider repository. NULL until the import has run; written once (NULL -> value) by the import''s compare-and-swap, never updated again.';

-- The parent side of the lineage read (a parent's forks, newest first).
CREATE INDEX project_forks_parent_idx
  ON project_forks (parent_project_id, created_at DESC, fork_project_id);

-- +goose StatementBegin
CREATE FUNCTION project_fork_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'project_forks is append-only: DELETE is forbidden (a fork is lineage; nothing disappears, state only evolves)'
      USING ERRCODE = 'P0001';
  END IF;

  -- Identity is immutable: which projects, which branches, who forked, the
  -- relation name and when the fork happened never move after the insert.
  IF NEW.fork_project_id IS DISTINCT FROM OLD.fork_project_id
     OR NEW.parent_project_id IS DISTINCT FROM OLD.parent_project_id
     OR NEW.forked_by IS DISTINCT FROM OLD.forked_by
     OR NEW.relation_type IS DISTINCT FROM OLD.relation_type
     OR NEW.source_branch_id IS DISTINCT FROM OLD.source_branch_id
     OR NEW.fork_branch_id IS DISTINCT FROM OLD.fork_branch_id
     OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
    RAISE EXCEPTION 'project_forks identity is immutable: only forked_sha may be set, once, from NULL'
      USING ERRCODE = 'P0001';
  END IF;

  -- The one-shot compare-and-swap cell. A second write to it is refused
  -- whatever it would say — the import that lost the race reads the winner's
  -- value instead of overwriting it.
  IF OLD.forked_sha IS NOT NULL AND NEW.forked_sha IS DISTINCT FROM OLD.forked_sha THEN
    RAISE EXCEPTION 'project_forks.forked_sha is write-once: it is already % and cannot move to %',
      OLD.forked_sha, NEW.forked_sha
      USING ERRCODE = 'P0001';
  END IF;

  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER project_forks_guard
  BEFORE UPDATE OR DELETE ON project_forks
  FOR EACH ROW EXECUTE FUNCTION project_fork_guard();

-- Row-level triggers do not fire on TRUNCATE (00015): the immutability
-- above could still be bypassed wholesale with `TRUNCATE ... CASCADE`. The
-- statement-level half reuses the shared append_only_guard() function.
CREATE TRIGGER project_forks_no_truncate
  BEFORE TRUNCATE ON project_forks FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

-- pull_request_fork_gate: an external pull request comes from a fork of the
-- project it proposes to, forked by the person opening it.
--
-- The same-project case is the ordinary one and passes untouched. For a
-- source branch in another project the gate demands a project_forks row
-- binding exactly (fork project = the source branch's project, parent
-- project = the PR's project, forked_by = the PR's creator) — so the
-- matrix's allow_from_fork is backed by a stored fact, and a PR can never
-- be opened across two unrelated projects, from a third party's fork, or
-- with the fork's other members' branches as its source.
--
-- A row that names a branch the gate cannot resolve (impossible through the
-- FK, kept as a fail-open only for the same-project case which is decided
-- before the lookup) is left to the application layer; the gate only ever
-- REFUSES a cross-project source it cannot justify.
-- +goose StatementBegin
CREATE FUNCTION pull_request_fork_gate() RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  source_project uuid;
BEGIN
  SELECT b.project_id INTO source_project
    FROM branches b
   WHERE b.id = NEW.source_branch_id;

  -- Same project: the ordinary internal pull request. Nothing to say.
  IF source_project IS NULL OR source_project = NEW.project_id THEN
    RETURN NEW;
  END IF;

  IF NOT EXISTS (
    SELECT 1
      FROM project_forks f
     WHERE f.fork_project_id = source_project
       AND f.parent_project_id = NEW.project_id
       AND f.forked_by = NEW.created_by
  ) THEN
    RAISE EXCEPTION 'pull request on project % cannot take its source branch from project %: a cross-project pull request must come from the opener''s own fork of this project (docs/04 §2, open_pr = allow_from_fork)',
      NEW.project_id, source_project
      USING ERRCODE = 'P0001';
  END IF;

  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER pull_request_fork_gate_trigger
  BEFORE INSERT ON pull_requests
  FOR EACH ROW EXECUTE FUNCTION pull_request_fork_gate();

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
