// Package merge is the Semantic Merge use case (T0406): the one path that
// advances a project's frozen main. It merges a Research PR whose review
// machine has reached merge_ready, by executing the three-way plan the RSG
// merge engine (internal/rsg/merge) derives from the PR's base/proposed/target
// states plus the human decisions recorded for that triple
// (internal/application/resolutions, T0407), and by classifying every change
// through the detector this platform already has (internal/rsg/conflict,
// T0405) — no second, competing conflict judgement lives here.
//
// # What a merge is allowed to be, from the specifications
//
// docs/09 §7 fixes the automatic-merge boundary, and it is the engine's
// (internal/rsg/merge doc.go), not a policy this package may widen: different
// objects, disjoint non-conflicting fields of one object, a mergeable relation
// set and append-only evidence may merge without a human; a scientific
// conclusion's winner, an averaged or folded protocol value, a causal claim,
// a private → public move and a rights conflict may not. Anything on the wrong
// side of that line is BLOCKED until a human decides it
// (CLAUDE.md invariant 11: agents resolve structural conflicts, humans resolve
// scientific ones). This package never recomputes a classification and never
// turns a blocked change into an applied one.
//
// docs/09 §8 names the seven resolution actions — Accept A, Accept B, Keep
// both versions, Explicit coexistence, Create validation branch, Request more
// evidence, Abort proposed change — plus the unresolved state. They are
// expressible end to end: the vocabulary is stored by T0407/migration 00069's
// widened CHECK, the engine has a defined effect for each (apply_source,
// keep_target, carry_both, abort_proposed, blocked), and two of them are
// deliberately NOT a resolution: keep-both, explicit coexistence and
// unresolved carry the conflict into the accepted state instead of collapsing
// it. A carried conflict writes no version at all — neither side wins — and is
// recorded as a row of the accepted state naming both versions and the human
// who decided to keep them open. "No winner" survives the merge; nothing here
// dissolves a contested conflict to make a merge succeed.
//
// docs/09 §9 draws the line between merging and publishing. A merge changes
// the accepted state and nothing else: visibility is not part of what this
// package writes, and where the source is private and the target public the
// engine withholds every change rather than publishing it. That publication
// decision belongs to the independent Publication Gate (T0704/T0705), and a
// private branch's history is never published as a side effect of merging.
//
// docs/09 §3 makes main protected/frozen: the only thing that may advance it
// is a Research PR merge. This package is that path and adds no way around it
// — it opens no new write surface on main, and the state transition it commits
// goes through the same states.Service + validation gate ladder every other
// state write uses (main's commits are held to GateMain inside the
// transaction). The transition ALSO declares itself
// (states.CommitParams.ResearchPRMerge): since T0601 (Freeze Main
// Governance) the states adapter refuses any commit onto a frozen main
// that does not carry that declaration, with
// MAIN_FROZEN_DIRECT_WRITE_FORBIDDEN — so the rule this package states in
// prose is now the rule the store enforces, this package is the only
// caller that sets the flag, and the freeze forbids the bypass without
// forbidding evolution. This package still leaves the rule as it found it
// (it adds no bypass) and now names the one place the rule is applied.
//
// # The transaction, and the window the T0402 review asked about
//
// The database half of a merge is ONE transaction: the accepted state, the
// version rows the plan materializes, the merge record with its carried
// conflicts, the PR's merge_ready → merged transition and the source branch's
// active → merged close either all land or none do. The Git half is a saga
// beside it (docs/08: Git is the repository/file truth next to PostgreSQL's
// semantic truth): the ref update follows the commit, is recorded on the merge
// row as pending/updated/failed/skipped, and a failed step stays retryable and
// visible instead of leaving the two truths silently divergent. Without a
// provider adapter (T0409 owns it) the step is recorded `pending` with the
// reason — the record says the Git half has not happened rather than claiming
// it did.
//
// T0402's review minor #2 asked the merge to either lock the branch lifecycle
// it reads or to accept the window in the specification. It is closed by
// layers, and the layers are honest about their order:
//
//   - The plan is computed from unlocked reads (the PR, the two branches, the
//     version counters). It is a function of the three pinned states and the
//     recorded decisions, so a state that moved does not make it stale in a
//     way that can be patched: it makes it a different triple, which nobody
//     decided.
//   - The write transaction re-reads the merge's rows under ROW LOCKS
//     (LockMergeScope: both branches FOR UPDATE in ascending id order, then the
//     PR) and compares them against the plan's inputs (verifyScope). A
//     concurrent merge therefore cannot slip between the read and the write:
//     the second one blocks on the lock and then sees the first one's result
//     and refuses deterministically.
//   - The commit's own compare-and-swap on the target branch head is the
//     second layer, and it is the one that closes the simplest race: it only
//     fires while the branch row still equals the head the plan was computed
//     over, so a target that advanced in the window fails the commit before
//     any write happens. verifyScope runs from inside the commit's write
//     callback, i.e. after that CAS, and therefore checks the locked target
//     head against the state being created (see verifyScope) — locking one
//     branch and committing on another is the mistake left for it to catch.
//   - The version counters are optimistic: the operation summary names
//     {entity, version_no} pairs predicted from an unlocked read and
//     re-verified under the same row locks the writes take. A counter that
//     moved rolls the whole merge back and is re-read (bounded by
//     MaxAttempts, default 3). A moved STATE is not retried — it is
//     *StaleMergeError: the human decided a different triple.
//
// # L1 decisions recorded here
//
//   - The accepted state only ever APPENDS versions. A three-way diff can only
//     report an object the database already has versions of, so the merge
//     copies the source version's content into a NEW version row in the
//     accepted state; it never creates a container, never re-points the
//     source's own rows, and never edits one (CLAUDE.md invariant 8: nothing
//     disappears, state only evolves).
//   - The merge record stores the executed plan's canonical bytes as JSONB
//     beside their sha256 (plan_digest). The digest is over the canonical
//     bytes, so the audit check is: recompute the plan from the recorded
//     triple and the recorded decisions and hash it. The JSONB column
//     normalizes whitespace and key order, so the stored JSON is the same
//     VALUE as those bytes and not the same text — the tag match is the plan
//     version plus the digest, never the byte string.
//   - The merge is authorized as ActionMergeMain (maintainer and above), with
//     the membership resolved before any read so a refusal never discloses
//     whether the PR or the branch exists.
//   - The source branch is closed (docs/43: active → merged) only when the
//     locked source head still IS the state this merge accepted. A branch that
//     moved on keeps its active lifecycle: freezing it would make history
//     immutable over work that never merged. The result reports which of the
//     two happened instead of choosing silently.
//   - A merge that would write nothing is refused (CodeNothingToMerge): a PR
//     whose every change is carried, withheld, aborted or blocked does not get
//     a state commit that claims it was merged.
//
// # Gaps this task does NOT close (named, not invented here)
//
//   - Non-push-source-head semantic completeness (issue #189). Migration 00042
//     defaults the flag to semantic_complete and only the push ingestion path
//     ever writes that table. Nothing in this package reasons about it: the
//     merge does not read, write or infer completeness for a state whose head
//     came from anything other than a push, and it does not invent a value.
//     That question is the owner's to decide; this task implements only the
//     part that is independent of the ruling.
//   - The provider-side Git merge (T0409). GitPort has no PR-merge operation
//     and no adapter performs one, so the saga records `pending` and retries
//     are observable rather than pretended (see GitMerger in ports.go).
//   - Freeze-main enforcement (T0601), as described above.
//   - Relation changes whose endpoints are held back are withheld with the
//     rest of the plan rather than written against a version the accepted
//     state does not carry (the engine's rule; see internal/rsg/merge).
package merge
