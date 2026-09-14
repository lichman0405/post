// Package releases is the release manifest builder use case (T0605): the
// system-facing read that renders one accepted main snapshot as its
// immutable release manifest — the docs/11 §1 document that fixes
// objects/versions, relations, blob hashes, git refs, policy/schema
// versions, the review/approval record and the rights snapshot, all
// content-addressed by the manifest hash.
//
// The release manifest embeds the state's canonical export
// (internal/rsg/manifest, T0206) and pins the release-level additions on
// top of it: the policy versions in force (the organization lower bound
// and the project's stricter overlay, each pinned with its full canonical
// document), the schema versions referenced by the snapshot (pinned by
// id, version and the registry's content hash of the exact document
// bytes), and the review record of the research PRs whose proposed states
// became part of the released main lineage.
//
// Determinism contract (acceptance: manifest deterministic): the manifest
// is a pure function of its recorded inputs — the state's exported
// content, the pinned policy version rows, the registry's immutable
// schema registrations, the stored review rows and the release version
// string. generated_at is the only field that moves between renders of
// the same release, and it is not part of the manifest hash (the same
// convention as internal/rsg/manifest).
//
// There is no HTTP route in this task and the service performs no
// authorization of its own. The release command (T0606) resolves
// visibility and policy first: it names the policy versions to pin
// explicitly — never "the latest" — so a release rebuilt at any later
// time renders the identical document instead of silently re-binding a
// newer policy (docs/12 §5: the release binds the policy version in force
// at release time).
//
// The service refuses states that are not accepted main snapshots: a
// release exists to fix accepted state, and a manifest of anything else
// is not a document at all (ErrNotMainState). It is also the release
// domain service the validation ladder names (internal/rsg/validation):
// ReleaseFacts assembles the three release-gate facts — review/approval
// record, rights/policy snapshot, main-branch origin — the T0606 flow
// hands to the release gate before building.
package releases
