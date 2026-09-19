// Package evidencegraph is the evidence-graph read service (T0506,
// docs/10_EVIDENCE_PROVENANCE.md): the assertions a project's knowledge
// objects carry, grouped by the exact target object version each one pins,
// with support and contradiction side by side and the hypothesis page's two
// sections kept apart.
//
// # What this package is, and what it is not
//
// It is a READ side and it writes nothing: the assertions themselves live in
// evidence_assertions, written by the RSG service's evidence command on the
// same state-commit machinery every scientific-state write uses (T0806), and
// this package has no store method that could write a row.
//
// It is NOT the provenance graph (CLAUDE.md §9 invariant 10, docs/10 §1:
// Provenance Graph != Evidence Graph). Provenance answers "where did this
// come from" and is served by the rebuildable provenance_edges projection;
// evidence answers "why does this bear on that proposition" and is served
// from evidence_assertions. The two surfaces share no route prefix, no store
// and no relation semantics, and cmd/api/evidencehttp carries a test that
// says so in as many words.
//
// It is NOT the network evidence read either (T0806,
// internal/application/evidencenetwork): that one renders a PUBLISHED
// object's evidence to an audience and classifies it Origin / Reviewed
// External / Unreviewed External. This one is the owning project's read of
// its own objects and classes nothing.
//
// # Grouping, not union
//
// A group is one pinned target version (internal/evidence.GroupAll), and
// the hypothesis page does not fold its two sections together. Grouping is
// the requirement's own word; a merged, counted or netted view would be the
// net-position arithmetic docs/10 §4 and CLAUDE.md §9.13 forbid.
//
// # Fail closed
//
// Every rule here answers "less" rather than "more" when the facts are
// unclear: an object outside the path project answers exactly what a
// nonexistent object answers (docs/45: no existence oracle), a store
// failure is an error and never an empty page, and a relation the stance
// rule does not know is rendered without a stance label rather than guessed
// into one (internal/evidence.Group).
package evidencegraph
