// Package merge is the Semantic Merge Engine (T0406): the pure planner
// that turns one three-way Research State Diff (internal/rsg/diff, T0401),
// its Semantic Conflict report (internal/rsg/conflict, T0405) and a human
// resolution plan (docs/09 §8) into the deterministic, executable plan of
// what a Research PR merge writes into the accepted state.
//
// It plans; it does not write. The application layer
// (internal/application/merge) runs the plan inside the state commit's
// transaction, with the target gate (docs/22 §7), and records the saga
// that follows. Keeping the decision here pure is what makes the merge
// reproducible: the same three states and the same decisions always
// produce the same plan bytes (CanonicalJSON), so a merge can be reviewed,
// replayed and audited without the database.
//
// The auto-merge boundary is docs/09 §7, read literally and taken from the
// T0405 detector rather than re-derived here: a change the detector marked
// auto_mergeable is merged with no human decision (different objects, the
// same object's different non-conflicting fields, append-only evidence); a
// change it marked conflicted is NEVER merged without one. This package
// re-classifies nothing — it consumes conflict.Report and switches on it
// (task requirement: 复用 T0405 的语义冲突检测结果，不另立一套判定).
//
// Every conflicted change must carry a decision whose classifier key
// matches a conflict the detector actually reported; an undecided conflict
// blocks the plan. Nothing is invented for the undecided case: "the merge
// would be convenient" is not a decision (CLAUDE.md invariant 11).
//
// The decision vocabulary is domain.ResolutionKind — the docs/09 §8
// actions as one vocabulary, shared with the resolution UI (T0407). Their
// structural effects:
//
//	accept_source          -> apply: the source content lands in the accepted state
//	accept_target          -> keep_target: the source change does not land
//	keep_both              -> carry_both: both sides survive; the conflict is carried
//	explicit_coexistence   -> carry_both: as above, recorded as an affirmed coexistence
//	validation_branch      -> blocked: the combined content must be validated on its own branch first
//	request_evidence       -> blocked: the conflict waits for evidence
//	abort_change           -> abort_proposed_change: the proposal is abandoned, not merged
//	unresolved             -> carry_both: main keeps the conflict contested
//
// "carry_both" is deliberately NOT a content merge, and that is the
// load-bearing decision of this package:
//
//   - The engine does not compute a third value. Averaging, folding or
//     re-wording two scientific values is exactly what docs/09 §7 forbids
//     (Protocol 冲突数值折中禁止自动) and no decision kind asks for it.
//   - Writing the source version into the same object's version chain
//     would create a chain whose head is the "later" version — an implicit
//     winner decided by whichever side was written second. The acceptance
//     criterion forbids collapsing a contested conflict into a winner, so
//     a carry_both change materializes nothing and instead names BOTH
//     versions (source and target) in the plan's carried-conflict record,
//     which the merge persists against the merged state.
//   - Nothing is lost by that: version rows are immutable and never
//     deleted (invariant 8: nothing disappears; state only evolves). Both
//     versions still exist, and after the merge the accepted state says so
//     explicitly instead of quietly preferring one.
//
// Publication is not this package's business and never happens as a side
// effect of merging (docs/09 §9: merge 只改变 accepted state; private →
// public goes through the independent Publication Gate, docs/23 §4). A
// change whose content would leave the source's visibility behind — the
// source branch is private and the target branch is public — is marked
// Withheld: it is not materialized at all, and the plan names it so the
// Publication Gate has the list of what still has to be published. The
// merge therefore never widens visibility, and private branch history is
// never published by being merged (docs/09 §9).
//
// A change is also withheld when it cannot be written faithfully. Relations
// are version-pinned edges, so a relation whose endpoint is a version the
// accepted state will not contain cannot land: writing it would put a
// dangling edge into the merged state. The plan is explicit about the
// difference between the two reasons (WithholdReason): the Publication Gate
// owns the first, and the second means the change has to wait for the
// object it depends on. A relation whose endpoint IS one of the versions
// this merge writes is not dropped — its pin is rewritten onto the object
// (Change.EndpointRewrites), which the executor resolves to the version row
// it just wrote.
//
// Two decisions describe a change that cannot be carried out and stop the
// merge instead of being approximated: a conflict nobody decided, and a
// change whose conflicts were decided to contradicting effects (Accept A on
// one conflict and Accept B on another of the same version row). The second
// case is refused rather than resolved by precedence: picking which decision
// wins for which field would synthesize a row no human chose.
//
// The engine is a pure function of its inputs: the report is recomputed
// from the same three states (conflict.Detect), decisions are matched by
// their classifier key, and every output list follows the report's
// canonical order (objects by object id, then relations by relation id,
// then blockers). Two runs over the same inputs marshal to the same bytes.
package merge
