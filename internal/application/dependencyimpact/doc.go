// Package dependencyimpact is the dependency impact analysis of docs/19 §3
// and docs/18 §5: when an upstream thing a project depends on really
// changes, work out which downstream entities that change reaches, and say
// so — without changing any of them.
//
// The requirement is one sentence, quoted verbatim: docs/19
// §3 「`depends_on`：实际输入/方法/复现依赖；上游变更触发 impact analysis。」 The
// five characters 「上游变更触发」 are the whole of this package. The trigger
// is not a button a user presses: it is an upstream state change that has
// already happened and already been recorded as a domain event, so this is
// an event-driven analysis chain (AnalysisProjector, mounted in cmd/worker)
// and not an endpoint. Nothing here adds a way to ask for an analysis by
// hand, and a test asserts that no such route exists.
//
// docs/18 §5 (Dependency Watch) adds the other half — 「当上游 dependency
// abort/supersede/new version/rights restriction 时，分析受影响下游，并创建
// alert。系统只标记 review required，不自动改科学结论。」 — and this package
// implements both halves: it creates the alert as the already-registered
// `dependency.impact_detected` event carrying review_required, and it
// changes nothing (an assertion reads every affected row back out of
// storage byte for byte).
//
// # Which edges count is read, never re-judged
//
// The question "may a change here reach there" has ONE answer in this tree
// and it is not this package's to give: relationcatalog.Entry.
// DependencyInference (internal/rsg/relationcatalog/catalog.go) is the flag
// docs/19 §3 drew, `depends_on` carries it and `references` does not —
// "references the target as background knowledge, not as an input
// dependency". Every walk in this package is parameterised by
// relationcatalog.DependentTypes, and the catalog's DependencyEnd/
// DependentEnd fields say which END of each flagged edge is the downstream
// one (depends_on names its dependency as the TARGET, used_by names it as
// the SOURCE, and a walk that assumed one direction would report the wrong
// objects for the other). The asset side is the same flag read through
// assets.DependencyType.TriggersImpactAnalysis; nothing here spells a
// relation or dependency type name.
//
// # The graph is over OBJECTS, the evidence is over VERSIONS
//
// A dependency edge is stored version-pinned (relation_versions.source_
// object_version_id → target_object_version_id), but the walk here is over
// OBJECTS: it resolves both ends through scientific_object_versions and
// treats "object E depends on object P" as the edge.
//
// The reason is the trigger set. docs/18 §5 names "new version" beside
// abort and supersede, and a walk that stayed version-pinned could not
// honour it: a new version is a version nothing points at yet, so the
// analysis would run and find nobody — a trigger that can never fire is
// the same as no trigger. Object-level edges make all four triggers mean
// the same thing — "something about this object changed; everyone
// downstream of it should look" — which is what docs/19 §3 asks for.
//
// The cost of that choice, stated rather than hidden: relation_versions is
// append-only (invariant 8, "Nothing disappears; state only evolves"), so
// an edge that a later version dropped still counts. A dependent whose
// newer version no longer uses the thing is still reported. That is the
// fail-safe direction — the alert is a HINT ("只标记 review required"), a
// false hint costs a reviewer's minute, a missed one costs a wrong
// scientific conclusion — and it is re-derivable, since nothing is stored.
//
// # Direct and indirect are in the data
//
// Each impact carries its own hop count and a Directness value derived
// from it (Direct = one hop, Indirect = more). Both are in the returned
// data, not computed at render time: a flattened boolean would make the
// one-hop/two-hop distinction unavailable to the page that has to render
// it, which is the same as not having it.
//
// # Read-only, and it says so in the type system
//
// Nothing in this package writes a scientific object, a state, a release,
// a visibility or a review. The only thing it writes is an alert — one
// outbox row per (trigger, affected) pair, ending up in the append-only
// event log. docs/11 §7 is the rule ("Published Asset Version 不删除。若出现
// 问题，追加 abort/supersede state/event 并指向 replacement"): the platform's
// answer to a bad upstream is a new state, never a rewrite — and this
// package does not even add the state. It only tells the people who own
// the downstream work that they have something to look at.
//
// # One walk, two audiences
//
// Service.Analyze is the walk with no reader (the worker's view, and the
// test's). Service.Read is the same walk through a reader: a caller may
// only be told about entities they could go and read themselves, and the
// ones they may not are DROPPED without a trace — no placeholder, no
// number, no total. That is docs/23 §5 ("no hidden private dependency
// leak") and the rule T0707 already implemented for the other direction of
// the same table (internal/assets/project_dependency.go: "a dropped row
// leaves no entry, no placeholder and no number, so a reader cannot infer
// how many were dropped").
package dependencyimpact
