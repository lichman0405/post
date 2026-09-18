-- +goose Up
-- Scientific responsibility and Research Owners routing (task T0604,
-- docs/04 §3 "Scientific Responsibility").
--
-- docs/04 §3 is two sentences and both are normative here:
--
--   「不与 Access Role 混合。项目可配置：Experimental Reviewer、
--     Computational Reviewer、Data Reviewer、Project Lead、IP Reviewer
--     等责任标签。责任用于 Review routing，不自动赋予更高访问权限。」
--   「类似 CODEOWNERS 的 Research Owners 规则可按对象类型/Schema/领域
--     匹配 reviewer。」
--
-- So the responsibility LABELS and the routing RULES are PROJECT DATA,
-- not code: the label set is open ("等" — "etc."), and which label owns
-- which change is a per-project mapping. Two tables carry that data, and
-- neither of them is a permission table:
--
--   1. research_owner_rules — the CODEOWNERS-like mapping: a change to
--      objects matched by object_type, by schema (schema_ref.id: a
--      canonical type schema or a project schema profile, T0213) or by
--      the object payload's `domain` field (the protocol schema's
--      subject-area field, docs/04 §3's 「领域」) is routed to a
--      responsibility label. Several rules may match one change; the
--      change then requires every matched label (union, never
--      first-match-wins: a rule is a requirement, and dropping one
--      because another matched first would relax the routing).
--
--   2. responsibility_assignments — who holds which label in which
--      project. UNIQUE(project_id, user_id, responsibility): holding a
--      label is one fact, and re-assigning it is idempotent rather than
--      a second row.
--
-- NEITHER table takes part in any authorization decision. docs/04 §3:
-- 「责任用于 Review routing，不自动赋予更高访问权限」. Access is decided
-- by the permission matrix (specs/policies/permissions-matrix.csv,
-- internal/authz) alone; internal/authz does not read these tables at
-- all. What a label buys its holder is exactly one thing: the
-- right to submit a review where the matrix says 'conditional'
-- (submit_scientific_review for viewer/contributor/authenticated
-- non-member), and the attribution of that review to the responsibility
-- it was signed under.
--
-- Both tables are ordinary project configuration, not governance
-- history: a rule that no longer applies is DELETED (the project owner's
-- decision), which is why neither carries the append-only guards of
-- 00014/00015. Every write is audited (the application writes its
-- audit_log row in the same transaction — T0110's discipline), so the
-- history of the mapping survives in the audit log, not in the table.
-- (What the tables DO share with the rest of the schema is the FK
-- discipline: every reference is ON DELETE RESTRICT. Nothing in POST
-- hard-deletes a project or a user — docs/09's 「Nothing disappears」
-- — so a cascade would only ever be a silent data-loss path.)
--
-- Bounds are enforced here as well as in the domain layer (the same
-- two-layer discipline as migration 00061's reviews columns): a label or
-- a match value is 1..200 characters and not blank, and a match value
-- must equal its trimmed self (a rule whose key carries invisible
-- whitespace would match nothing and silently stop routing).

CREATE TABLE research_owner_rules (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  match_kind text NOT NULL,
  match_value text NOT NULL,
  responsibility text NOT NULL,
  created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT research_owner_rules_match_kind_check
    CHECK (match_kind IN ('object_type', 'schema', 'domain')),
  CONSTRAINT research_owner_rules_match_value_check
    CHECK (char_length(match_value) BETWEEN 1 AND 200 AND match_value = btrim(match_value)),
  CONSTRAINT research_owner_rules_responsibility_check
    CHECK (char_length(responsibility) BETWEEN 1 AND 200 AND responsibility = btrim(responsibility)),
  -- One rule row per (project, kind, value, label): the same mapping
  -- written twice is the same requirement twice, which would make the
  -- required-review projection count it twice. A different LABEL for the
  -- same match is a different row on purpose — that is how one change
  -- acquires two responsible reviewers.
  UNIQUE (project_id, match_kind, match_value, responsibility)
);

CREATE INDEX research_owner_rules_project_idx ON research_owner_rules (project_id);

CREATE TABLE responsibility_assignments (
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  responsibility text NOT NULL,
  created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT responsibility_assignments_responsibility_check
    CHECK (char_length(responsibility) BETWEEN 1 AND 200 AND responsibility = btrim(responsibility)),
  PRIMARY KEY (project_id, user_id, responsibility)
);

-- The routing resolver's read: "which labels does this user hold here?"
-- (the reviewer-responsibility hook) and "who holds this label?"
-- (the assignment list a project owner reads).
CREATE INDEX responsibility_assignments_project_idx
  ON responsibility_assignments (project_id, responsibility);

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
