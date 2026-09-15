-- +goose Up
-- Project milestones (T0609): research-timeline markers, deliberately a
-- separate object from releases. A release is an immutable snapshot of
-- accepted state (00053); a milestone is a recorded research event —
-- candidate selected, paper submitted, patent filed, external validation,
-- or a custom-labeled marker. A milestone MAY name the release it
-- documents (release_id, nullable — not required); it never depends on
-- one, and a milestone may precede any release entirely.
--
-- Milestones never touch the project lifecycle: no trigger and no column
-- here advances projects.activity_status, which keeps no forced
-- "completed" terminal state (docs/43 §1 — planning → active ↔ paused →
-- archived). Research progress is a timeline of recorded facts, not a
-- lifecycle transition, so no milestone kind is a completion state.
--
-- The kind CHECK carries the canonical vocabulary plus custom; the
-- cross-column CHECK enforces the custom-label rule at the database level
-- (a custom marker without a label has no name — it must not exist, no
-- matter which write path inserts it). A canonical kind may carry a
-- custom display label (e.g. the venue of a paper submission) or none.
--
-- project_milestone_creations is the Idempotency-Key ledger (docs/22: an
-- Idempotency-Key governs create/command/publish/merge/release): a key
-- replays the milestone it created, forever — a retried create is a read,
-- never a duplicate timeline row. Milestones are records, not immutable
-- snapshots, so the pair carries no append-only guards (no update/delete
-- surface exists in V1; a correction surface may add one later).
--
-- The timeline index serves the project timeline scan in research order:
-- occurred_at first (the canonical kinds' natural progression), creation
-- order breaking same-date ties deterministically.

CREATE TABLE project_milestones (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  kind text NOT NULL CHECK(kind IN ('candidate_selected','paper_submitted','patent_filed','external_validation','custom')),
  label text,
  occurred_at timestamptz NOT NULL,
  release_id uuid REFERENCES releases(id) ON DELETE RESTRICT,
  created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK (kind <> 'custom' OR (label IS NOT NULL AND btrim(label) <> ''))
);

CREATE INDEX project_milestones_timeline_idx
  ON project_milestones(project_id, occurred_at, created_at, id);

CREATE TABLE project_milestone_creations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  idempotency_key text NOT NULL,
  milestone_id uuid NOT NULL REFERENCES project_milestones(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(project_id, idempotency_key)
);
