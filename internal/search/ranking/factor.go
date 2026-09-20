package ranking

import "sort"

// The six factors, named exactly as docs/14 §3 names them and in its order.
//
// The names are the wire vocabulary: they appear in every Assessment and in
// the canonical rendering the golden fixtures pin (tests/ranking), so a
// consumer can key off them without knowing which revision of this package
// produced the result. They are a closed set — see Factors.
const (
	// FactorQueryScopeMatch is how the candidate matched the question, and
	// under which narrowing. It is the only factor that reads no database
	// row: it is a property of the retrieval itself (which signal recalled
	// the candidate, and what the request narrowed to).
	FactorQueryScopeMatch = "query_scope_match"
	// FactorEvidence is the evidence profile: what evidence has been
	// ASSERTED about the version, and whether a human has reviewed it
	// (docs/10 §3).
	FactorEvidence = "evidence"
	// FactorReview is the review state: the scientific review decision
	// recorded on the project state the version was created in (docs/09 §5).
	FactorReview = "review"
	// FactorReproduction is independent reproduction: whether a project
	// other than the one that owns the version has reproduced it (docs/10
	// §4's `reproduces`).
	FactorReproduction = "reproduction"
	// FactorConflict is contradictory evidence: assertions and relation
	// edges that contradict this version (docs/10 §4's `contradicts`,
	// `inconsistent_with`, `challenges`, `fails_to_reproduce`).
	FactorConflict = "conflict"
	// FactorVersion is version/freshness: whether the candidate is the
	// object's newest live version, an earlier one, or a withdrawn one.
	FactorVersion = "version"
)

// factorOrder is the priority order, highest priority first. It is
// docs/14 §3's own enumeration order, which is the order the document lists
// the factors in and therefore the one a reader of the spec expects; nothing
// in this package may reorder it without a spec change.
var factorOrder = []string{
	FactorQueryScopeMatch,
	FactorEvidence,
	FactorReview,
	FactorReproduction,
	FactorConflict,
	FactorVersion,
}

// Factors returns the ranking's factor vocabulary in priority order.
//
// It is exported because "the vocabulary is closed" is a claim callers and
// tests need to be able to state: a consumer rendering a candidate's
// explanation iterates this list, and a change to it is a contract change
// that the golden fixtures make visible.
func Factors() []string {
	out := make([]string, len(factorOrder))
	copy(out, factorOrder)
	return out
}

// Levels. Every factor's ladder is a small, named, ordered set; lower is
// better, and LevelRank is that position. The names are the second half of
// the vocabulary a consumer keys off, so they are declared here rather than
// spelled at the point of use.
//
// A level is always reached by a rule a reader can check against the reason
// on the same Assessment — no level is set from a quantity nobody can see.
const (
	// Scope-match levels.
	//
	// LevelQuestion: a text or vector signal recalled the candidate, so the
	// question's own words or meaning matched its content.
	LevelQuestion = "question"
	// LevelConditions: only the caller's structured filter recalled it — the
	// question's stated conditions matched, its wording did not.
	LevelConditions = "conditions"
	// LevelRelated: only the traversal reached it — the candidate is related
	// to something that matched, not something that matched itself.
	LevelRelated = "related"
	// LevelUnattributed: no recall signal is recorded on the candidate. It is
	// unreachable through Retrieve (every candidate enters through a signal)
	// and exists so that a hand-built candidate is reported rather than
	// silently ranked as if it had matched.
	LevelUnattributed = "unattributed"

	// Evidence-profile levels.
	//
	// LevelReviewed: at least one assertion about this version has been
	// through human review (docs/10 §3's review state = 'reviewed').
	LevelReviewed = "reviewed"
	// LevelAsserted: evidence has been asserted, none of it reviewed.
	LevelAsserted = "asserted"
	// LevelRejected: assertions exist and every one that carries a review
	// decision was rejected — a human looked at this version's evidence and
	// did not accept it. It ranks below LevelAsserted deliberately: "nobody
	// has checked" is a weaker negative fact than "somebody checked and
	// said no".
	LevelRejected = "rejected"
	// LevelNoEvidence: nothing has been asserted about this version at all.
	LevelNoEvidence = "none"

	// Review-state levels.
	//
	// LevelApproved: a scientific review approved the state this version was
	// created in.
	LevelApproved = "approved"
	// LevelReviewedState: a scientific review exists on that state and none
	// approved it (comments, or a change request).
	LevelReviewedState = "reviewed"
	// LevelUnreviewed: no scientific review has been recorded on that state.
	LevelUnreviewed = "unreviewed"

	// Reproduction levels.
	//
	// LevelIndependent: a project other than the version's own recorded a
	// reproduction, and NO failure to reproduce is recorded by anyone. The
	// failure count is not split by origin: a recorded failure blocks this
	// level whoever recorded it, the version's own project included
	// (assessReproduction).
	LevelIndependent = "independent"
	// LevelDisputedReproduction: an independent reproduction AND at least one
	// recorded failure to reproduce both exist. The failure is counted
	// whoever recorded it — it does not have to come from another project —
	// because a failure assertion disputes the claim to reproducibility from
	// anywhere. This is the strongest reason to read the sources rather than
	// the rank.
	LevelDisputedReproduction = "disputed"
	// LevelSelfReproduced: the only reproductions are from the version's own
	// project, so the platform cannot tell a replication from a reuse.
	LevelSelfReproduced = "self"
	// LevelNoReproduction: no reproduction has been recorded either way.
	LevelNoReproduction = "none"

	// Conflict levels.
	//
	// LevelUncontested: nothing on the platform contradicts this version.
	LevelUncontested = "uncontested"
	// LevelContested: at least one contradictory evidence assertion or
	// contradicts relation touches it (docs/10 §8's "actively contested").
	LevelContested = "contested"

	// Version/freshness levels.
	//
	// LevelCurrent: the candidate pins the object's newest version and that
	// version is live.
	LevelCurrent = "current"
	// LevelHistorical: the candidate pins a real version of the object that
	// is not its newest — the object has moved on.
	LevelHistorical = "historical"
	// LevelUnversioned: the candidate pins no scientific object version at
	// all (an asset, release or state document, or a publication that
	// resolved to nothing), so there is no freshness to state.
	LevelUnversioned = "unversioned"
	// LevelAborted: the version was withdrawn. Nothing disappears
	// (CLAUDE.md §9.8) and an aborted version is still returned — last.
	LevelAborted = "aborted"

	// LevelUnknown: the factor could not be read. It is the level every
	// database-backed factor takes when the candidate's version is outside
	// the caller's project scope (store.go), and it is deliberately NOT the
	// factor's benign level — `none` says "we looked and there is nothing",
	// which is a claim about another project's corpus that this platform is
	// not entitled to make without reading it. It ranks after every level
	// that reports a fact and before `aborted` on the freshness ladder,
	// because a withdrawn version is a fact and this is the absence of one.
	//
	// `unknown` is one name shared by the five database-backed factors: the
	// situation is the same one situation, and five spellings of it would be
	// five chances for a consumer to miss one.
	LevelUnknown = "unknown"
)

// levelsForFactor is each factor's ladder in order, best first — the one
// table that defines an ordering, with the position being the index.
//
// There is deliberately no second table mapping level names to numbers: the
// position IS the index in this list, so a level added without a position, or
// two levels sharing one, is not representable. `unknown` appears on five
// ladders because the five database-backed factors share one name for one
// situation, and each ladder decides for itself where it sits (see
// LevelUnknown, and LevelVersion's ladder, where it is above `aborted`).
var levelsForFactor = map[string][]string{
	FactorQueryScopeMatch: {LevelQuestion, LevelConditions, LevelRelated, LevelUnattributed},
	FactorEvidence:        {LevelReviewed, LevelAsserted, LevelRejected, LevelNoEvidence, LevelUnknown},
	FactorReview:          {LevelApproved, LevelReviewedState, LevelUnreviewed, LevelUnknown},
	FactorReproduction:    {LevelIndependent, LevelDisputedReproduction, LevelSelfReproduced, LevelNoReproduction, LevelUnknown},
	FactorConflict:        {LevelUncontested, LevelContested, LevelUnknown},
	FactorVersion:         {LevelCurrent, LevelHistorical, LevelUnversioned, LevelUnknown, LevelAborted},
}

// levelRank looks a level's position up on its factor's ladder. The bool is
// false when the factor or the level is not one this package declares, which
// is a bug at the call site rather than a state to rank.
func levelRank(factor, level string) (int, bool) {
	for i, name := range levelsForFactor[factor] {
		if name == level {
			return i, true
		}
	}
	return 0, false
}

// Levels returns one factor's ladder, best first. It is what a caller needs
// to explain a ranking it did not compute ("why is `historical` below
// `current`?"), and what the unit suite checks the rank table against.
func Levels(factor string) []string {
	ladder := levelsForFactor[factor]
	out := make([]string, len(ladder))
	copy(out, ladder)
	return out
}

// Labels are the descriptive labels docs/10 §8 permits the platform to
// derive: "limited evidence、mixed evidence、actively contested、
// independently reproduced". They are descriptive and not a score — the
// document's next sentence rules out an implicit Truth Score — and each one
// is a restatement of a factor level a reader can already see, never a new
// judgement.
const (
	// LabelLimitedEvidence: nothing has been asserted about the version.
	LabelLimitedEvidence = "limited_evidence"
	// LabelMixedEvidence: the version has both supporting and contradicting
	// evidence assertions.
	LabelMixedEvidence = "mixed_evidence"
	// LabelActivelyContested: the version is contradicted by an assertion or
	// a relation.
	LabelActivelyContested = "actively_contested"
	// LabelIndependentlyReproduced: a project other than the version's own
	// recorded a reproduction.
	LabelIndependentlyReproduced = "independently_reproduced"
)

// labelOrder fixes the order labels are rendered in, so two runs (and two
// fixtures) list them the same way whatever order they were derived in.
var labelOrder = []string{
	LabelLimitedEvidence,
	LabelMixedEvidence,
	LabelActivelyContested,
	LabelIndependentlyReproduced,
}

// sortLabels sorts a label set into labelOrder and drops duplicates.
func sortLabels(labels []string) []string {
	seen := make(map[string]bool, len(labels))
	out := make([]string, 0, len(labels))
	for _, name := range labelOrder {
		for _, l := range labels {
			if l == name && !seen[l] {
				seen[l] = true
				out = append(out, l)
			}
		}
	}
	// A label not in labelOrder is a bug (a new label without a position),
	// and appending it sorted keeps the rendering deterministic rather than
	// dropping a fact on the floor.
	extra := make([]string, 0)
	for _, l := range labels {
		if !seen[l] {
			seen[l] = true
			extra = append(extra, l)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}
