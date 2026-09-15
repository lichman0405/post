// Package resolutions is the Scientific Conflict Resolution use case
// (T0407): the human decisions over the conflicts the Semantic Conflict
// Detector (internal/rsg/conflict, T0405) classified, and the read view
// the resolution UI renders. docs/09 §8 names the decision vocabulary —
// Accept A, Accept B, Keep both versions, Create validation branch, and
// contested/unresolved — and docs/09 §7 names what no machine may decide:
// no winner is ever picked, no protocol value is ever averaged or
// folded. This package enforces exactly that: the decision kinds are the
// ones docs/09 §8 names — Accept A, Accept B, Keep both versions,
// Explicit coexistence, Create validation branch, Request more evidence,
// Abort proposed change, plus the contested/unresolved state — and a
// decision is only accepted when it names a conflict the detector
// actually classified for the same base/source/target triple.
//
// Save is the write command. It authorizes the actor through the project
// surface + policy engine with the same require shape the RSG write path
// uses (ActionWriteScientificState — L1 decision: recording a human
// scientific decision is scientific-state work, contributor or above; the
// merge itself stays ActionMergeMain's maintainer gate, and the audit row
// names the human who decided), recomputes the conflict report for the
// pinned triple (states are immutable, so the report a decision was made
// against never changes), verifies every decision against that report and
// stores the plan in one transaction together with one audit entry —
// docs/60: scientific conflict final resolution must be human-governed.
// A decision that names a conflict the report does not contain is refused
// (ErrConflictNotFound): the merge engine (T0406) consumes only plans
// whose every item the detector vouched for.
//
// Plan and View are the read surfaces the resolution UI renders: the
// decisions recorded for a triple, and the conflict report plus per-side
// evidence context plus those decisions. Like the diffs package they
// perform no authorization of their own — the HTTP surface resolves
// visibility through the project read gate before calling them.
//
// L1 decisions recorded here because the consumers (the resolution UI,
// the merge engine T0406) build on them:
//
//   - Resolution identity: a decision is keyed by the detector report's
//     full classifier key (target kind + id, conflict code, fields,
//     payload keys, paired object) plus the pinned triple. Two conflicts
//     can never share a decision, and a decision can never be misread as
//     one about a different conflict.
//   - The agent explanation (the detector's Detail text, and any future
//     agent-authored suggestion) is advisory-only: nothing in this
//     package, the HTTP surface or the UI derives a decision from it. The
//     only outcomes are the five human-chosen kinds; there is no computed,
//     averaged or auto-selected kind.
//   - "unresolved" is an explicit decision (keep the conflict contested in
//     main, docs/09 §8), not the absence of one: a conflict without a row
//     is simply undecided.
//   - Decisions are overwritten in place (latest wins, decided_by/
//     decided_at move); the overwrite is audit-logged in the same
//     transaction. There is no delete path.
//
// Persistence note (allowed_scope, L1): this task's allowed_scope does not
// include internal/persistence (the canonical adapter directory), so the
// pgx adapter lives here in store_pg.go — raw SQL against the migration
// 00054 table, no sqlc. The Supervisor may relocate it once a task with
// internal/persistence in scope can carry it.
package resolutions
