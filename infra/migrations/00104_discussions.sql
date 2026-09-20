-- +goose Up
-- Discussion threads, comments and promotions (T0811).
--
-- docs/09 §4 and docs/42 §PR list "discussion" among a pull request's
-- parts, and the tree had no table, store, route or component for it: the
-- only hits for comment/discussion in infra/migrations before this file
-- are DDL `COMMENT ON` statements and 00009's review DECISION vocabulary
-- ('commented' → 00061's 'comment'), which is a reviewer's verdict on a
-- proposed head, not a conversation. This migration is the table set.
--
-- # What a discussion is, structurally: a version-less member row
--
-- A thread or a comment carries NO version number, NO state_id and no
-- branch. It is not a scientific object, it never takes part in a state
-- commit (internal/application/states.Service.Commit — 00004's
-- project_states + the version logs), and nothing here is registered in an
-- append-only ledger:
--
--   * nothing in this file writes scientific_object_versions (00005),
--     relation_versions (00006) or contribution_events (00011, guarded
--     append-only by 00014/00015). Commenting therefore cannot change
--     scientific state — the property is structural, not conventional:
--     there is no column, trigger or path here that reaches those tables;
--   * the repo's provenance representation is a RELATION (relation_versions
--     with a provenance relation type, docs/44), and its endpoints are
--     foreign keys to scientific_object_versions. A discussion has no
--     version, so it cannot be an endpoint of a relation — not by
--     preference but because the column would not accept it. Promoting a
--     discussion therefore records its provenance in an explicit
--     promotion row (discussion_promotions below), never as a relation.
--
-- # target_type / target_id: a discussion hangs on three surfaces
--
-- A thread names what it is about. Three target kinds exist because three
-- surfaces have a discussion today (the task's requirement): a project, a
-- published knowledge object, and a pull request. target_id is TEXT and
-- holds the target's identity IN ITS OWN ADDRESSING SCHEME — the project
-- uuid, the publication's pid, the pull request's per-project number —
-- because that is the identity the surface that carries the discussion
-- already shows a caller: the knowledge page addresses a publication by
-- pid (00083), the PR page by number (00009), the project page by its
-- uuid. A polymorphic uuid column would have forced every client to
-- translate what it holds into an internal row id first.
--
-- The target is NOT a foreign key: one column cannot reference three
-- tables. The command resolves and verifies the target inside the thread's
-- project before any row is written (fail closed, docs/45 — an unknown or
-- foreign target is one not-found answer), and the project_id column is
-- what every read, authorization and scope check uses. The CHECK below
-- pins the one case that CAN be pinned in the schema: a thread whose
-- target is its own project must carry that project's uuid — the column
-- pair cannot disagree no matter which write path inserts it.
--
-- # Comments: never physically deleted
--
-- A comment carries deleted_at/deleted_by, NULL while it stands. CLAUDE.md
-- §9.8 ("nothing disappears; state only evolves") is why there is no
-- physical delete: the row, its author and its time survive, the body is
-- simply withheld from the wire after deletion, and a promotion that
-- points at a since-deleted comment still resolves to the author who made
-- the proposal. The pair moves together or not at all (the CHECK below).
--
-- # Promotions: the explicit provenance chain
--
-- One row per promotion: which comment was proposed, what it became, who
-- promoted it and when. The promoted object is named by promoted_ref in
-- the same shape as audit_log.target_ref ("<kind>:<uuid>"), so the REF is
-- derived from the kind exactly once (the CHECK) and a promotion whose
-- kind and ref disagree cannot exist. The reverse read — the promoted
-- object answering "which discussion, whose comment" — is a query on this
-- table joined to discussion_comments (the author column there), which is
-- how an Issue, a promoted Hypothesis and an external-evidence proposal
-- all answer the same question the same way.
--
-- No append-only trigger guards this table: a promotion is a member row
-- like a milestone, not a version log (00014/00015 guard the logs). No
-- update/delete surface exists in V1 either, so the rows only accumulate.

CREATE TABLE discussion_threads (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  target_type text NOT NULL CHECK (target_type IN ('project','knowledge','pull_request')),
  target_id text NOT NULL CHECK (btrim(target_id) <> ''),
  created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),
  -- A thread on the project it lives in names that project, spelled the
  -- same way projects.id renders as text. Any other kind of target is
  -- resolved by the command (the CHECK can only see this one pair).
  CHECK (target_type <> 'project' OR target_id = project_id::text)
);

-- The target scan: every thread of one (project, target), oldest first.
CREATE INDEX discussion_threads_target_idx
  ON discussion_threads(project_id, target_type, target_id, created_at, id);

CREATE TABLE discussion_comments (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  thread_id uuid NOT NULL REFERENCES discussion_threads(id) ON DELETE RESTRICT,
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  body text NOT NULL CHECK (btrim(body) <> ''),
  created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),
  deleted_at timestamptz,
  deleted_by uuid REFERENCES users(id) ON DELETE RESTRICT,
  -- Deletion is recorded as a pair: a tombstone names when and by whom, or
  -- it is no tombstone at all.
  CHECK ((deleted_at IS NULL) = (deleted_by IS NULL))
);

-- The conversation read: one thread's comments in creation order. The
-- creation order (created_at, id) is a total order, so the rendering never
-- depends on insertion timing ties.
CREATE INDEX discussion_comments_thread_idx
  ON discussion_comments(thread_id, created_at, id);

CREATE TABLE discussion_promotions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  thread_id uuid NOT NULL REFERENCES discussion_threads(id) ON DELETE RESTRICT,
  comment_id uuid NOT NULL REFERENCES discussion_comments(id) ON DELETE RESTRICT,
  promoted_kind text NOT NULL CHECK (promoted_kind IN ('issue','hypothesis','external_evidence')),
  promoted_ref text NOT NULL,
  promoted_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  promoted_at timestamptz NOT NULL DEFAULT now(),
  -- The ref IS "<kind>:<uuid>": the prefix is the kind's own name and what
  -- follows it is not empty. A promotion cannot name a kind it is not, and
  -- cannot name nothing.
  CHECK (promoted_ref = promoted_kind || ':' ||
         substring(promoted_ref FROM length(promoted_kind) + 2))
);

-- The reverse read: which discussion proposed the object an id names.
CREATE INDEX discussion_promotions_ref_idx
  ON discussion_promotions(project_id, promoted_kind, promoted_ref, promoted_at, id);

-- And the forward one: what one comment has become so far.
CREATE INDEX discussion_promotions_comment_idx
  ON discussion_promotions(comment_id, promoted_at, id);
