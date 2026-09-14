// Package conflict is the Semantic Conflict Detector (T0405): the pure
// classifier that reads one three-way Research State Diff (internal/rsg/diff,
// T0401) and marks each source-side change auto_mergeable or conflicted,
// with every conflict classified into the taxonomy of docs/09 §6 —
// attribute, identity, relation, schema, knowledge, rights, dependency —
// plus the scientific category that docs/09 §7/§8 name explicitly.
//
// The auto-merge boundary is docs/09 §7, read literally:
//
//   - Auto (no conflict): different objects, the same object's different
//     non-conflicting fields, converged same-field changes (both branches
//     made the same change), and append-only list changes (append-only
//     evidence) — a base list that both sides only extended is mergeable.
//   - Never auto (conflict): every other same-field divergence. Scientific
//     conflicts — any diverging field of a protocol object — are never
//     auto-resolved (docs/09 §7: Protocol 冲突数值折中禁止自动); no winner
//     is ever picked (docs/07 §8).
//
// Classification rules (L1 decisions, recorded here because the consumers
// — the merge engine T0406 and the resolution UI T0407 — build on them):
//
//   - attribute: title or lifecycle diverged on both sides for a
//     non-protocol, non-knowledge object, or payload diverged on an
//     object whose scientific type is not knowledge-shaped.
//   - identity: two objects created on opposite sides of the fork with the
//     same object_type and the same trimmed title — a suspected duplicate
//     (docs/09 §6: 疑似同对象/不同对象). The detector flags it; a human
//     decides merge-into-one versus keep-both.
//   - relation: a relation's relation_type or payload diverged on both
//     sides (for knowledge-category edge types the payload divergence is
//     knowledge instead — the edge's payload is evidence metadata).
//   - schema: schema_ref diverged on both sides.
//   - knowledge: payload diverged on a knowledge object (claim,
//     hypothesis, research_question, finding — docs/09 §6: claim
//     assessment/evidence conflicts), or on a knowledge-category relation.
//   - rights: visibility_policy_id diverged on both sides (a rights
//     conflict, never auto-resolved). A source-side visibility move that
//     the target did not diverge on is also never auto: visibility
//     expansion requires explicit authorized confirmation with an audit
//     event (docs/12 §3), and this detector cannot tell an expansion from
//     a narrowing — so every visibility move is deferred to a human.
//   - dependency: a relation's endpoint pins (source/target object
//     version) diverged on both sides — upstream re-analysis semantics
//     (docs/19 §3).
//   - scientific: any diverging content field (title, payload,
//     lifecycle_state) of a protocol object — same field, different
//     values on the two branches of a protocol is a scientific conflict
//     (task acceptance criterion). Protocol content changes are never
//     auto-resolved, appends included: no branch validated the combined
//     steps.
//
// Payload divergence is judged at top-level key granularity: only a key
// both sides changed counts, a converged key (same resulting value, or
// both sides removing the key — agreement on absence) is not a conflict,
// and a key whose base value is an array that both sides only extended is
// a mergeable append. A JSON null is NOT an empty array: null != [], so a
// null base value is no anchor list to extend and a null head value is no
// extension — a key whose base is null that both sides replace with
// different values is a conflict. This is what keeps "append evidence"
// from being misjudged as a text conflict (task acceptance criterion):
// two branches appending different evidence to the same list merge.
//
// The detector is a pure function of the three states' recorded content
// (the same purity invariant as the diff engine): Detect takes the same
// Inputs as diff.Compute, computes the diff itself, and returns it inside
// the Report so consumers get the change list and the verdicts from one
// call. Nothing mutable is read; verdicts are derived, never stored.
//
// Determinism: the report marshals to the same bytes for the same inputs.
// Verdicts follow the diff's identity-sorted change order; conflicts on
// one change follow the canonical field order; identity conflicts sort by
// the target-side object id; payload keys sort by name. The golden file
// under testdata/ pins the bytes.
package conflict
