// Package validation owns the progressive validation gates of the RSG:
// five named checkpoints — draft, pr, main, release, asset — that hold a
// branch's research state to increasingly strict standards as it moves
// toward publication (T0207, docs/22 §7).
//
// The ladder is the point. A draft may lack domain fields (docs/08
// §CoreScientificObject: "Draft 可缺部分 domain field"); proposing a PR
// makes the type schema's required fields blocking; merging to frozen main
// adds provenance completeness; a release additionally requires the review
// and rights record; publishing a research asset additionally requires the
// full docs/11 §4 checklist. The same check that merely warns at one gate
// blocks at the next — GateSpecs is the declarative record of which check
// runs where, at which severity, and why, so every verdict is explainable
// from the spec alone (Spec.Why).
//
// The package is pure: it validates a Snapshot of facts (states, commits,
// object/relation versions, release/asset facts) against the schema
// registry (internal/rsg/schemareg) and returns a Report. It never reads
// or writes storage — the application layer (internal/application/validation)
// assembles snapshots from the persistence layer and decides what a
// blocked report means for the command that asked.
package validation
