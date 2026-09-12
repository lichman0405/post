// Package authz owns the rights/permission decisions defined in
// docs/12_PERMISSIONS_RIGHTS_POLICY.md and ADR-009 (Rights = license +
// machine metadata + agreement ref). Every public action must pass an
// explicit authorization check (docs/50): the transport layer hides
// nothing by itself — a hidden control is not a permission.
//
// T0105 lands the V1 permission framework here:
//
//   - Action / ActorClass / Verdict mirror the rows, columns and cell
//     values of specs/policies/permissions-matrix.csv (the canonical
//     policy table; matrix.go mirrors it cell for cell and
//     TestPermissionMatrixMatchesCSV keeps the two identical);
//   - Engine is the policy engine interface. Its contract is default
//     deny: an unknown action, an unknown actor class, or any request an
//     implementation cannot resolve is denied, never allowed;
//   - MatrixEngine is the V1 implementation: a static, deterministic
//     lookup over the matrix;
//   - ClassOf resolves the actor's matrix column from a project
//     membership role (owner/maintainer/contributor/viewer), the
//     authenticated-but-not-a-member state, anonymity, or the agent flag.
//
// Enforcement sites live in the application services (docs/52: the
// application layer makes the authz decision); the HTTP layer only maps
// the resulting errors onto wire codes.
package authz
