// Package branches is the Research Branch Domain application layer (task
// T0205): the branch row's use cases — fork a branch from a project state,
// its public/private visibility, its lifecycle (docs/43: active → merged |
// aborted, terminal) and its current head state.
//
// The model (docs/03 §2, docs/09 §1): a branch is one research evolution
// path of a project, not a personal workspace. It is created from a base
// state — that state becomes the branch's initial head (branches.
// base_state_id, the docs/21 §5 "branch current state" projection) while
// it keeps the branch it was committed on, so main's chain and the
// branch's chain never mix: the shared fork point stays visible only on
// the branch that created it, and every later commit is one branch's
// history alone. Each branch head then evolves independently through
// states.Service.Commit (T0204), whose compare-and-swap moves exactly one
// head per commit and never a merged/aborted one.
//
// Visibility (docs/09 §1, docs/12 §2-3): a new branch defaults to the
// project's preset (public project → public, private project → private);
// an explicit value may only stay within the preset — a private project
// cannot host a public branch, because visibility widening goes through
// the explicit, audited publication flow, never through branch creation.
// The private→public change of an existing branch is the same publication
// flow (P6/P7), deliberately absent from this surface.
//
// Lifecycle: Merge and Abort close an active branch (terminal per docs/43);
// main is protected — its lifecycle is the project's, not one research
// path's, and frozen-main write protection is T0601's gate. Closed
// branches accept no commits and their heads no longer move; the database
// enforces both on any update path (migration 00028).
//
// The package validates and orchestrates; it does not authorize — the
// consuming API task resolves membership/role (internal/authz already
// carries ActionCreateBranch) and passes identities in. The persistence
// adapter lives in internal/persistence (BranchStore).
package branches
