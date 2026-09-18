// Package assetrights is the asset rights-holder governance use case
// (T0711): changing who holds a research asset, as an append-only
// governance event.
//
// # What it implements, in the words of the specs
//
// docs/11 §6 is the whole requirement: "分离 Rights Holder、Custodian、
// Maintainer、Creator、Contributor、Originating Project。Ownership transfer
// 是 append-only governance event；Creator/history 永久保留。" docs/26 §5 lists
// ownership transfer among the HIGHEST-risk audited actions — beside
// visibility, rights, policy, merge, publish and abort/reopen — and
// requires the audit to be append-only. This package is the command half
// of both sentences; migration 00082 is the storage half
// (asset_rights_holder_events, append-only by trigger).
//
// # The four properties the command is built around
//
//  1. A transfer APPENDS. It never updates a row, and it never touches a
//     version: the previous holder stays readable in the chain, and the
//     asset's id, pid, origin project, versions, origin refs and credited
//     creators are all exactly what they were (the acceptance of T0711).
//     A creator is a signature, not an ownership stake — changing who
//     holds an asset does not change who wrote it (CLAUDE.md invariant 8,
//     "Nothing disappears; state only evolves").
//
//  2. Only the OWNER. specs/policies/permissions-matrix.csv:13 is
//     `change_rights_holder,deny,deny,deny,deny,deny,allow,deny` — every
//     column deny except owner, and this command reads it through
//     authz.ActionChangeRightsHolder. Note that the matrix's maintainer
//     cell here is `deny`, unlike the publish row's `conditional`: there
//     is no condition to interpret and no judgement to make.
//
//  3. Fail-closed, in both directions. A membership that cannot be
//     resolved, an engine that cannot answer, a class the matrix denies —
//     every one of them is a refusal, never a default. An unknown actor
//     class (authz.ClassOf's empty string for a role outside the four) is
//     denied by the engine's own table, not defaulted here.
//
//  4. The party is a person OR an organization, and the record says
//     which. owner ruling L3-20260916-1 (tasks/decisions.md): 权利人可以是
//     组织；人和组织分开记. The request names a domain.Party (kind + id),
//     the store resolves that pair to a real row of the table its kind
//     names, and a pair that resolves to nothing is refused rather than
//     stored as a dangling reference. A project is not a holder: docs/11
//     §6 lists Originating Project as its own role, and the kind check
//     (domain.PartyKind.IsIdentity) refuses it here as 00082's CHECK
//     refuses it in the database.
//
// # What this package deliberately does NOT do
//
//   - It does not fix the design of a first holder. An asset that has
//     never had one is not secretly owned by its publisher, its creator or
//     its origin project: the first event of such an asset records a NULL
//     previous holder (00082), which is the honest statement that there
//     was none. Deriving an initial holder from published_by would be a
//     product rule (who owns an asset) that no spec states and that this
//     task may not invent.
//   - It does not treat a credit as a claim of ownership, or ownership as
//     a credit: the creator rows live in asset_version_parties and this
//     command cannot write them.
//   - It does not record a credit dispute. docs/26 §5 lists credit dispute
//     beside ownership transfer as a SEPARATE high-risk action, and it is
//     not this task's.
//
// # The surface
//
// The command is delivered without an HTTP route: specs/api/openapi.yaml
// is the API contract (docs/22, OpenAPI-first) and is not in this task's
// writable scope, so a route added here would be an endpoint the contract
// does not describe. The transport is a follow-up (see the task's
// RESULT); what is complete and tested here is the command, its
// authorization, its transaction and its audit — driven in the
// integration suite exactly as cmd/api would drive it, over the real
// PostgreSQL and the real permission matrix.
package assetrights
