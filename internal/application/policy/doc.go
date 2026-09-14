// Package policy orchestrates the organization/project policy engine
// (T0603, docs/12 §5): organization policy is the minimum governance
// requirement; a project policy may only be stricter, never silently
// looser. Policies are versioned — the canonical policy_versions table is
// append-only (migrations 00014/00015), every change is a new row, old
// versions stay queryable forever, and releases pin the version that was
// in force (releases.policy_version_id, T0605).
//
// The engine has three layers:
//
//   - domain.Policy is the policy document (a flat set of named rules
//     with raw JSON values); internal/domain/policy.go owns the rule
//     vocabulary, strictness comparison and the org-lower-bound merge;
//   - Evaluator is the policy evaluate interface: enforcement sites ask
//     it typed questions ("how many reviewers does this policy require?")
//     instead of reading policy_json; the V1 implementation is
//     RuleEvaluator, fail-closed on anything it cannot resolve;
//   - Service orchestrates the use cases against the store and the
//     organization/project gates: set a policy version (owner-only),
//     read any version, compute the effective policy.
//
// L1 decisions recorded for the Supervisor (no product semantics were
// invented — every rule key ships from docs/12 §5's examples):
//
//   - who may write policy: organization owners set org policy; project
//     owners set project policy (policy governs the maintainers' own
//     merge authority, so letting a maintainer lower a governance bound
//     would be privilege escalation — the same L1 gate T0109 used);
//   - who may read policy: any current member of the scope (policy is
//     governance configuration; V1 exposes no policy to non-members);
//   - a project version is validated against the org version in force
//     when it is WRITTEN (snapshot semantics — "Policy 版本化；Release
//     绑定当时 policy version"); evaluation overlays the org bound on
//     top, and the lower bound always wins a conflict;
//   - a new rule key with no registered strictness order compares by
//     identity only: rewriting it at project level is refused
//     (unprovable = refused, default deny). T0604 extends the vocabulary
//     in domain/policy.go when reviewer routing lands.
package policy
