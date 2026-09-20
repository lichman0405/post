// Package discussions is the discussion use case (T0811): a conversation
// attached to a project, a published knowledge object or a research pull
// request, and the promotion of one of its comments into a proposed
// research object (an Issue, a Hypothesis, or an external-evidence
// proposal).
//
// # A discussion does not move scientific state
//
// This is the property the package is built around, and it is structural
// rather than a promise. A thread and a comment are version-less member
// rows (internal/domain/discussion.go): they carry no version, no state id
// and no branch, they take no part in a state commit, and no append-only
// ledger registers them. The three tables of 00104 are the only tables any
// port here writes, and the ports do not include a state-commit port at all
// — so "a comment opens with no state transition and writes no
// contribution_events row" is a fact about what this package can reach, not
// a rule it remembers to follow. See ports.go for the argument in full.
//
// # A promotion does, and does it through the surface that owns the object
//
// Promoting a comment is the deliberate opposite: it creates a row in
// another surface's table, records where it came from in
// discussion_promotions, and audits the act (domain.ActionDiscussionPromoted
// — a promotion under write_scientific_state is exactly the governance
// action the audit trail exists for). The created object is made by the
// command that owns it:
//
//   - an Issue, by the promotion store, in one transaction with the
//     promotion and audit rows;
//   - a Hypothesis, by internal/application/rsg's CreateObject — a real
//     state commit, with the schema the object type's canonical schema
//     resolves to and the title the RSG write path derives from the
//     statement;
//   - an external-evidence proposal, by rsg's CreateEvidenceAssertion —
//     one evidence_assertions row in its default review_state
//     'unreviewed', which IS the pending proposal. No proposal table is
//     created for it (the task's ruling: reviewing it later is a legitimate
//     in-place transition, 00058).
//
// # Authorization
//
// Every method runs the project read gate (T0106's resolution, the same one
// every project-scoped surface runs): a discussion is exactly as visible as
// the project carrying it. Writes additionally need an authenticated actor,
// and a promotion evaluates the matrix cell of write_scientific_state for
// the actor's class — the RSG writes' own action, reused rather than
// duplicated, with the own_fork_only conditional cell resolved against the
// fork lineage exactly as the RSG write sites resolve it. The hidden actor
// is an agent or a service: this command has no agent path, and the class
// it evaluates is authz.ClassOf(authenticated, role, false), the same shape
// the other application commands use.
//
// The feature adds NO action to the matrix: specs/policies is not this
// task's to change, and the ruling is that a promotion is
// write_scientific_state while a comment needs nothing beyond reading the
// target.
//
// # Provenance
//
// A promotion's origin is a ROW, not a relation: relation_versions endpoints
// are foreign keys to scientific_object_versions and a discussion has no
// version, so the relation graph cannot carry it (domain invariant 10's
// separation is beside the point — the shape simply does not fit). The
// discussion_promotions table names the comment and the created object, and
// the reverse read (PromotionsForRef) answers "which discussion proposed
// this" for an object of any of the three kinds. There is deliberately no
// second mechanism: no origin_discussion_id column was added to any of the
// three object tables, and the promoted object carries no back-reference of
// its own.
//
// # What the specifications leave open
//
// Recorded rather than decided (the task's standing instruction): who may
// withdraw a comment (author-only here); whether an Issue is a research
// object at all (docs/03 names Issue once, in a sentence about Research
// Questions); what the issue_type vocabulary is (free text in 00009, and
// this flow refuses a blank one rather than choosing a word); and whether
// the same comment may be promoted twice into the same kind (nothing
// forbids it, and a second promotion creates a second object). The task
// result lists these for the Supervisor.
package discussions
