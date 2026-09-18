// Package forks implements the external contribution path (T0804): a user
// who is not a member of a public project forks it into their own space,
// writes there, and proposes from there.
//
// docs/04 §2 states the rule in prose ("Public Project 的非成员用户不是
// Contributor role，但可 Fork 并从自己的空间发起外部 PR") and
// specs/policies/permissions-matrix.csv writes it as three cells of the
// authenticated_nonmember column:
//
//	create_branch          external_fork_only
//	write_scientific_state own_fork_only
//	open_pr                allow_from_fork
//
// authz.Verdict.Permits() passes only 'allow' (internal/authz/verdict.go),
// so all three cells refuse a non-member until an enforcement site
// resolves the condition the cell names. This package is the site for two
// of them, and creates the lineage fact the third is resolved against:
//
//   - external_fork_only — resolved by Service.Fork. The project being
//     forked must be public, and the fork is created as the actor's OWN
//     personal project (organization_id NULL, owned by the actor), which
//     is what "从自己的空间" means structurally: the fork is not a
//     membership in somebody else's project and grants nothing there.
//   - allow_from_fork — resolved by Service.OpenExternalPR. The source
//     branch of the proposal must belong to a fork of the target project
//     that THIS actor forked — the same fact the database's
//     pull_request_fork_gate re-checks on any insert path (00086).
//   - own_fork_only — resolved where scientific-state writes are
//     authorized (internal/application/rsg.requireWrite), against the
//     same lineage row through an OwnedFork read; not in this package,
//     because the write does not happen here.
//
// Nothing is widened. The package adds no action and no verdict, and a
// fork grants its owner exactly what they already have in a project of
// their own: the copy is Git content imported through the same push
// inspection every other arrival runs (internal/gitprovider/forkimport.go,
// docs/16 §4.1), never a copy of rows and never a path to restricted blob
// bytes (CLAUDE.md invariant 7). The lineage is recorded with the existing
// relation vocabulary — 'forked_from' (project_forks.relation_type) — and
// is readable through Service.Lineage.
package forks
