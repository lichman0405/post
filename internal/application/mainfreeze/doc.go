// Package mainfreeze is the Freeze Main governance action (T0601): the
// command that sets a project's main_frozen flag, and the half of the
// frozen-main guarantee that was missing until it existed.
//
// # What it closes, in the words of the code that named the hole
//
// internal/application/merge/doc.go states the rule docs/09 §3 fixes —
// "main protected/frozen: the only thing that may advance it is a Research
// PR merge" — and then recorded what the platform did NOT have: "no code
// refuses a direct semantic write to main today (projects.main_frozen is
// reported, not enforced)". This package is both halves of closing it:
//
//   - the ACTION. Until this command existed nothing in the tree ever
//     wrote the column (internal/persistence/queries carried no statement
//     that set it, so main_frozen was constantly false). Freeze is the
//     first writer.
//   - the ENFORCEMENT it turns on. The refusal lives in the states
//     adapter's commit transaction (persistence.StateStore.CommitState →
//     MAIN_FROZEN_DIRECT_WRITE_FORBIDDEN), with the Git-side half in the
//     push ingestion (gitprovider.PushIngester) — both keyed off the flag
//     this command sets. A freeze that nothing read would be the "reported,
//     not enforced" state this task was dispatched to end.
//
// # The order of the steps, and why it is the order
//
//  1. shape      — validate the request, before anything is read
//  2. agent      — the domain backstop, before anything is read
//  3. authorize  — membership class vs the permission matrix, before
//     anything the project owns is read
//  4. policy     — the governance policy in force (org floor + project)
//  5. freeze     — the store's transaction: the compare-and-swap, the
//     audit row and the domain event, all or nothing
//
// Steps 2 and 3 come before every lookup on purpose: a refusal there must
// not disclose whether the project exists (the releases.ErrForbidden rule
// — "resolved before any target lookup, so the denial never discloses
// whether the project exists"). An unknown project id resolves to "no
// membership", which is a permission-class refusal, never a 404 — that is
// the same existence-hiding the project read gate provides elsewhere, and
// it is stricter here because there is no read gate in front of the
// command.
//
// # Two lines of defence against an agent
//
// The matrix's freeze_main row denies agents (internal/authz/matrix.go,
// specs/policies/permissions-matrix.csv:10 — and that row is the SPEC for
// this action: docs/60's list of human-governed operations does not spell
// freeze out by name, and specs/mcp/tools.json's forbidden_default_agent_actions
// does not carry it either; nothing in this package claims otherwise). An
// agent freezing main is refused twice over, and both refusals are
// deliberate:
//
//   - the DOMAIN backstop (step 2, AgentNotPermittedError). It reads the
//     actor value alone, before any read, and it holds even if the matrix
//     were ever configured to permit an agent. Shape copied from
//     internal/application/contribution/service.go and
//     internal/application/assetpublish, which refuse the same kind of
//     human governance action the same way.
//   - the MATRIX (step 3). The same request is refused again by the
//     authorization every freeze runs.
//
// Neither is redundant: the matrix is a policy document a governance
// change may edit, and the backstop is the product rule that the edit
// cannot waive.
//
// # Idempotency is the state, not a ledger
//
// There is no freeze ledger table and no second migration: the flag IS the
// record. The transaction's compare-and-swap
// (WHERE id = @id AND main_frozen = false) decides the outcome, so of two
// concurrent freezes exactly one wins and only the winner writes the audit
// row and the event; the loser returns the same answer without writing
// anything. A repeated request — same Idempotency-Key or a different one —
// therefore produces one audit row and one event forever, by construction
// rather than by remembering keys. That is why the Idempotency-Key is
// required on the route and consumed by the state: it makes the caller's
// retry explicit, and the retry is then answered by the state instead of a
// table this task may not create.
//
// # One direction only, and that is the specification's decision
//
// V1 has no unfreeze. docs/09 §3 (docs/09_VERSION_CONTROL.md:9-10): the
// frozen state is lifted by no endpoint — "Emergency unfreeze 不在 V1
// 提供，避免形成绕过路径；必要维护用 admin break-glass runbook，并记录审计"
// — and specs/api/openapi.yaml carries POST /projects/{projectId}/main:freeze
// and no :unfreeze. This package therefore has no Unfreeze method, no
// parameter that clears the flag, and internal/persistence/queries/
// projects.sql carries no statement that sets main_frozen back to false.
// Maintenance is an operations runbook, not code this task writes.
//
// # Freeze is project-level, main-only, and does not forbid evolution
//
// projects.main_frozen is a flag on the PROJECT, not on a branch: it
// refuses direct semantic writes to main (and direct Git pushes to it)
// and leaves every research branch untouched. It does not stop main from
// advancing — the Research PR merge still lands, which is exactly the
// guarantee docs/09 §3 states ("即便 Owner 也只能经 PR merge") and the
// reason the enforcement is role-independent: once frozen, a direct write
// is refused for everyone, owner included, because a rule an owner can
// bypass is not a frozen main.
package mainfreeze
