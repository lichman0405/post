// Package pullrequests is the Pull Request domain use case (T0402): the
// proposal row of docs/09 §4 — source/target branch, the FIXED base and
// proposed states, the docs/43 lifecycle (open → review_required →
// changes_requested/approved → merge_ready → merged; closed/aborted),
// and the per-project number.
//
// Two acceptance criteria shape the surface:
//
//   - "PR base 不随 main 漂移": the base state is pinned at creation to
//     the target branch's head and never re-derived afterwards — the
//     database rejects any write path that would move it (migration
//     00051). Re-evaluating against the target's current head is the
//     merge flow's job (T0406), not this row's.
//   - "head update 可显式 refresh": the proposed state is pinned at
//     creation to the source branch's head and moves ONLY through
//     RefreshProposed — the one adapter path that sets the
//     transaction-scoped flag migration 00051's fixity guard requires.
//
// The service owns input validation (the port trusts, the service
// verifies); it does NOT authorize — PR creation and lifecycle moves are
// project-governance actions whose membership/role checks belong to the
// consuming API task, which passes resolved identities in. Review
// records (the reviews table) are T0404's Scientific Review model — this
// package owns the PR row itself and the state machine the reviews
// project onto.
package pullrequests
