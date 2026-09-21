// Package assetmetadata is the research asset metadata revision use case
// (T0706): the governance write that revises an asset's description,
// keywords, contact, documentation, title or slug — docs/11 §4's
// "Asset Metadata 可独立 revision，保留 audit".
//
// # What it is not, and why that is the whole design
//
// A revision here produces NO new scientific version and touches no
// existing one. docs/11 §4 is one sentence and the second half of it
// ("不产生新的 scientific version") is a prohibition, not a remark: the
// metadata is a value of the ASSET row (research_assets, migration
// 00125), and the versions live in research_asset_versions, which this
// command never reads and never writes. The two surfaces already know
// about each other and say so in the code: publishing a version of an
// existing asset REFUSES title and slug, because "publishing a version is
// not an asset-metadata revision (docs/11 §4 gives metadata its own,
// separately audited surface)"
// (internal/application/assetpublish/command.go:197) — this package is
// that surface, and it refuses the mirror-image case by taking no version
// argument at all.
//
// # The two shapes it copies, and why both
//
//  1. internal/application/projects/settings.go — the write shape. A
//     governed metadata edit is an IN-PLACE update of the mutable columns
//     plus ONE audit_log row in the SAME transaction
//     (settings.go:35-43), carrying the actor, the via, the action, the
//     target, the project and the request's correlation id, with the
//     before and after values in audit_log.before_summary /
//     .after_summary (00012:52-53 — the two columns exist for exactly
//     this). UpdateSettings (:132) is the same shape one surface over:
//     nil fields stay unchanged, the audit row commits with the update,
//     the before/after pair is filled from the row.
//
//     This is deliberately NOT "append a revision row". docs/11 §4 makes
//     the metadata revisable and makes the version stream immutable; a
//     per-revision row would be a second version stream for the metadata,
//     which is the thing the sentence forbids. The audit row is the
//     record, and it is the record docs/11 §4 asks for by name (保留
//     audit).
//
//  2. internal/application/assetrights — the asset-side governance
//     shape. The project id is a REQUIRED field of the request rather
//     than something derived from the asset, and it is resolved BEFORE
//     the asset is looked up, so a denial never discloses whether the pid
//     exists. The store then verifies the asset's origin project is the
//     one the caller named, so a caller authorized in project A cannot
//     write metadata for an asset of project B.
//
// # Who may revise, and the gap this leaves
//
// The permission matrix has NO asset-metadata row
// (specs/policies/permissions-matrix.csv — grep for metadata returns
// nothing), and specs/ is outside this task's scope. The rule is
// therefore the default-deny server-side ROLE gate
// projects/settings.go took for the same situation (:29-33): the actor's
// resolved membership must be maintainer or above, and the refusal is the
// same value for "not a member" and for "role too low" so the check
// discloses nothing about the project. The missing matrix row is recorded
// as a follow-up for the Supervisor rather than invented here — inventing
// one would be writing policy, which is the specification's to write.
package assetmetadata
