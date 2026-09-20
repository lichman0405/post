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
// External / Unreviewed External. This one renders the assertions pinned to
// an object of the path project and classes nothing.
//
// # The read carries its reader (ADR-024)
//
// Every read here takes the caller as an explicit input (projects.Reader —
// the identity the transports already resolve for every other
// visibility-aware read, not a second model of who a caller is), and the
// reader decides WHICH ROWS are rendered: an assertion's own
// visibility = 'public', or the reader being a member of the asserting
// project, or of the project that owns the target version. The predicate
// lives in the store query (internal/persistence/queries/evidence.sql,
// ListEvidenceAssertionsForTarget), not in a filter over a full result set,
// so both transports that serve this read — the JSON routes and the object
// detail page's evidence tab — get one rule and cannot disagree.
//
// This is the axis the PLATFORM read needs and is deliberately not folded
// into the public read's `visibility = 'public'`: that predicate belongs to
// the anonymous surfaces (GET /knowledge/{knowledgeId} and the research
// profile), and applying it here would take from the two parties rows they
// see today. The repository has both rules on purpose; see
// internal/persistence/queries/research_profile.sql's warning against
// "tidying" the axes into one.
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
// failure is an error and never an empty page, a relation the stance
// rule does not know is rendered without a stance label rather than guessed
// into one (internal/evidence.Group), and a reader the audience rule cannot
// resolve to a user id is answered the PUBLIC rows rather than every row —
// the same direction as the column's own DEFAULT 'private'.
package evidencegraph
