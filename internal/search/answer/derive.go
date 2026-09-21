package answer

import (
	"strconv"
	"strings"

	"github.com/lichman0405/post/internal/search/ranking"
	"github.com/lichman0405/post/internal/search/retrieval"
)

// Limitations and conflicts, derived from the platform's own facts.
//
// docs/14 §4 asks an answer for "conflicting/limited evidence" beside its
// sources, and these two functions are where that comes from. They read the
// factors the ranking already computed for each source, and the signals the
// retrieval already reported, and they write nothing of their own: every
// sentence below is a restatement of a level a reader can see on the same
// source, which is what makes the sections checkable rather than authoritative
// (docs/10 §8's rule for labels — a derived description, never a new judgement
// — applies to these sentences too).
//
// The model is not asked to author either section, for the reason schema.go
// gives: "these versions contradict each other" is a claim about the
// platform's graph, and docs/14 §4 forbids the answer layer from creating one.

// deriveLimitations returns the limitations of an answer, in a fixed order:
//
//  1. the fallback cause, when there is one (a fallback's first limitation is
//     that it is a fallback);
//  2. one statement per signal that did not run;
//  3. the per-source limitations, in the order of limitationRules below,
//     grouped: one statement per rule with the matching sources' refs, so a
//     result of forty sources does not become forty near-identical lines;
//  4. when no per-source rule applies, the single statement that none does —
//     so an empty section never means "we did not look" (the rule set is
//     closed, and a source matches none of it only by the platform having read
//     every fact on it).
func deriveLimitations(sources []Source, signals []retrieval.SignalReport, reason Reason) []Statement {
	out := make([]Statement, 0, len(limitationRules)+len(signals)+1)
	if reason != "" {
		out = append(out, Statement{Text: fallbackText(reason), Origin: originPlatform})
	}
	for _, s := range signals {
		if !s.Ran {
			out = append(out, Statement{Text: signalNotRunText(s), Origin: originPlatform})
		}
	}
	if len(sources) == 0 {
		// A result with no source is already explained — by the fallback
		// cause when there is one (ReasonNoSources is the only way to reach
		// this), and there is no other way to reach it: the generator does
		// not call a provider with an empty vocabulary, so an ANSWERED answer
		// always has at least one source.
		return out
	}
	matched := false
	for _, rule := range limitationRules {
		refs := make([]string, 0, len(sources))
		for _, src := range sources {
			if rule.match(src) {
				refs = append(refs, src.Ref)
			}
		}
		if len(refs) == 0 {
			continue
		}
		matched = true
		text := rule.one
		if len(refs) > 1 {
			text = rule.some(len(refs), len(sources))
		}
		out = append(out, Statement{Text: text, Refs: refs, Origin: originPlatform})
	}
	if !matched {
		out = append(out, Statement{Text: noLimitationText(len(sources)), Refs: refsOf(sources), Origin: originPlatform})
	}
	return out
}

// deriveConflicts returns the recorded contradictions touching a result's
// sources, in a fixed order: one statement per contested source, then one per
// source whose reproduction is disputed, then — when nothing is contested —
// the statement that says so.
//
// The "nothing is contested" case is a STATEMENT rather than an empty list
// because the two absences differ: a client that renders an empty array
// cannot tell "the platform read every source's conflict facts and found none"
// from "the platform could not read them". The conflict factor's levels
// separate those cases on the source itself, and this list says which one it
// is in a sentence.
func deriveConflicts(sources []Source) []Statement {
	out := make([]Statement, 0, 4)
	if len(sources) == 0 {
		return out
	}
	for _, src := range sources {
		if level, ok := factorLevel(src, ranking.FactorConflict); ok && level == ranking.LevelContested {
			out = append(out, Statement{
				Text:   factorReason(src, ranking.FactorConflict),
				Refs:   []string{src.Ref},
				Origin: originPlatform,
			})
		}
	}
	for _, src := range sources {
		// A recorded failure to reproduce disputes a claim to
		// reproducibility, so it belongs in this section even though the
		// ranking reports it on the reproduction factor (docs/10 §4's
		// `fails_to_reproduce`).
		if level, ok := factorLevel(src, ranking.FactorReproduction); ok && level == ranking.LevelDisputedReproduction {
			out = append(out, Statement{
				Text:   factorReason(src, ranking.FactorReproduction),
				Refs:   []string{src.Ref},
				Origin: originPlatform,
			})
		}
	}
	if len(out) > 0 {
		return out
	}
	// Nothing contested. Say so only if every source's conflict fact was
	// actually read; otherwise name the ones it was not read for, because
	// "no conflict is recorded" about a version nobody read would be the
	// fail-open version of this whole package.
	unread := make([]string, 0, len(sources))
	for _, src := range sources {
		if level, ok := factorLevel(src, ranking.FactorConflict); !ok || level == ranking.LevelUnknown {
			unread = append(unread, src.Ref)
		}
	}
	if len(unread) > 0 {
		text := "the platform could not read the conflict facts of 1 of the " + strconv.Itoa(len(sources)) +
			" sources, so no statement is made about whether it is contradicted"
		if len(unread) > 1 {
			text = "the platform could not read the conflict facts of " + strconv.Itoa(len(unread)) + " of the " +
				strconv.Itoa(len(sources)) + " sources, so no statement is made about whether they are contradicted"
		}
		return []Statement{{Text: text, Refs: unread, Origin: originPlatform}}
	}
	text := "nothing on the platform contradicts the 1 source of this result: no contradictory evidence assertion and no contradicts relation touches it"
	if len(sources) > 1 {
		text = "nothing on the platform contradicts any of the " + strconv.Itoa(len(sources)) +
			" sources of this result: no contradictory evidence assertion and no contradicts relation touches them"
	}
	return []Statement{{Text: text, Refs: refsOf(sources), Origin: originPlatform}}
}

// limitationRule is one derived limitation: when it applies to a source, and
// what it says. The two texts are written rather than templated because the
// verb has to agree with the subject — "1 source pins" and "3 sources pin" are
// not the same sentence with a number substituted, and a sentence that reads
// as a template is a sentence a reader trusts less (the same reasoning
// ranking.assessEvidence gives for its own clauses).
type limitationRule struct {
	// name is the rule's identity, for the tests that check the set is closed
	// and for a reader of a failure message.
	name  string
	match func(Source) bool
	// one is the sentence for exactly one matching source.
	one string
	// some is the sentence for more than one: how many matched, of how many.
	some func(n, total int) string
}

// limitationRules is the closed set of per-source limitations, in the order
// they are reported: version first (whether there IS a version), then what was
// read about it, then how it was found.
var limitationRules = []limitationRule{
	{
		name:  "unversioned",
		match: func(s Source) bool { return hasLevel(s, ranking.FactorVersion, ranking.LevelUnversioned) },
		one:   "1 source pins no scientific object version, so no freshness and no evidence profile can be stated for it",
		some: func(n, total int) string {
			return strconv.Itoa(n) + " of the " + strconv.Itoa(total) +
				" sources pin no scientific object version, so no freshness and no evidence profile can be stated for them"
		},
	},
	{
		name:  "factors-unread",
		match: unreadFactors,
		one: "1 source resolves to a version outside the project scope of this search, so no facts about it were read " +
			"(its factors are `unknown`, which is not the same as a factor reporting nothing)",
		some: func(n, total int) string {
			return strconv.Itoa(n) + " of the " + strconv.Itoa(total) +
				" sources resolve to versions outside the project scope of this search, so no facts about them were read " +
				"(their factors are `unknown`, which is not the same as a factor reporting nothing)"
		},
	},
	{
		name:  "no-evidence",
		match: func(s Source) bool { return hasLevel(s, ranking.FactorEvidence, ranking.LevelNoEvidence) },
		one:   "no evidence assertion has been recorded about 1 source's version, so nothing is asserted about it either way",
		some: func(n, total int) string {
			return "no evidence assertion has been recorded about the versions of " + strconv.Itoa(n) + " of the " +
				strconv.Itoa(total) + " sources, so nothing is asserted about them either way"
		},
	},
	{
		name:  "evidence-rejected",
		match: func(s Source) bool { return hasLevel(s, ranking.FactorEvidence, ranking.LevelRejected) },
		one:   "every evidence assertion about 1 source's version that carries a review decision was rejected",
		some: func(n, total int) string {
			return "every evidence assertion carrying a review decision about the versions of " + strconv.Itoa(n) +
				" of the " + strconv.Itoa(total) + " sources was rejected"
		},
	},
	{
		name:  "no-scientific-review",
		match: func(s Source) bool { return hasLevel(s, ranking.FactorReview, ranking.LevelUnreviewed) },
		one:   "no scientific review has been recorded on the state 1 source's version was created in",
		some: func(n, total int) string {
			return "no scientific review has been recorded on the states the versions of " + strconv.Itoa(n) +
				" of the " + strconv.Itoa(total) + " sources were created in"
		},
	},
	{
		name:  "reproduction-self-only",
		match: func(s Source) bool { return hasLevel(s, ranking.FactorReproduction, ranking.LevelSelfReproduced) },
		one: "the only reproduction recorded for 1 source's version is from its own project, so the platform cannot " +
			"tell a replication from a reuse",
		some: func(n, total int) string {
			return "the only reproductions recorded for the versions of " + strconv.Itoa(n) + " of the " +
				strconv.Itoa(total) + " sources are from their own projects, so the platform cannot tell a replication from a reuse"
		},
	},
	{
		name:  "no-reproduction",
		match: func(s Source) bool { return hasLevel(s, ranking.FactorReproduction, ranking.LevelNoReproduction) },
		one:   "no reproduction has been recorded for 1 source's version, by its own project or any other",
		some: func(n, total int) string {
			return "no reproduction has been recorded for the versions of " + strconv.Itoa(n) + " of the " +
				strconv.Itoa(total) + " sources, by their own projects or any other"
		},
	},
	{
		name:  "version-historical",
		match: func(s Source) bool { return hasLevel(s, ranking.FactorVersion, ranking.LevelHistorical) },
		one:   "1 source pins a version that is not its object's newest: the object has moved on since",
		some: func(n, total int) string {
			return strconv.Itoa(n) + " of the " + strconv.Itoa(total) +
				" sources pin a version that is not their object's newest: those objects have moved on since"
		},
	},
	{
		name:  "version-aborted",
		match: func(s Source) bool { return hasLevel(s, ranking.FactorVersion, ranking.LevelAborted) },
		one:   "1 source is a version that was withdrawn; it is still returned, because state on this platform only evolves",
		some: func(n, total int) string {
			return strconv.Itoa(n) + " of the " + strconv.Itoa(total) +
				" sources are versions that were withdrawn; they are still returned, because state on this platform only evolves"
		},
	},
	{
		name: "traversal-only",
		match: func(s Source) bool {
			return hasLevel(s, ranking.FactorQueryScopeMatch, ranking.LevelRelated) ||
				hasLevel(s, ranking.FactorQueryScopeMatch, ranking.LevelUnattributed)
		},
		one: "1 source was reached by relation traversal rather than recalled by the question itself, so its place in " +
			"this result is its relation to a source that matched",
		some: func(n, total int) string {
			return strconv.Itoa(n) + " of the " + strconv.Itoa(total) +
				" sources were reached by relation traversal rather than recalled by the question, so their place in " +
				"this result is their relation to a source that matched"
		},
	},
}

// unreadFactors reports whether the source pins a version and at least one of
// its factors could not be read.
//
// The unversioned case is excluded deliberately: a document that resolves to
// no version has `unknown` on every database-backed factor (ranking's
// unreadAssessment, the versionID == "" branch), and reporting it twice — once
// as "no version", once as "not read" — would say the same thing twice and
// imply a scope refusal that did not happen.
func unreadFactors(s Source) bool {
	if hasLevel(s, ranking.FactorVersion, ranking.LevelUnversioned) {
		return false
	}
	for _, factor := range ranking.Factors() {
		if hasLevel(s, factor, ranking.LevelUnknown) {
			return true
		}
	}
	return false
}

// hasLevel reports whether a factor on the source carries a level.
//
// A factor that is ABSENT (the ranking returned fewer than six, which only a
// hand-built source can do) reports false for every level, so a rule never
// matches on a factor nobody read.
func hasLevel(s Source, factor, level string) bool {
	got, ok := factorLevel(s, factor)
	return ok && got == level
}

// factorLevel reads one factor's level off a source.
func factorLevel(s Source, factor string) (string, bool) {
	for _, a := range s.Factors {
		if a.Factor == factor {
			return a.Level, true
		}
	}
	return "", false
}

// factorReason reads one factor's sentence off a source. An absent factor
// yields "", which the caller must not print; deriveConflicts only calls it
// for a factor it has already found by level.
func factorReason(s Source, factor string) string {
	for _, a := range s.Factors {
		if a.Factor == factor {
			return a.Reason
		}
	}
	return ""
}

// refsOf returns the sources' refs in rank order. The slice is fresh so a
// caller cannot edit the answer through it.
func refsOf(sources []Source) []string {
	out := make([]string, 0, len(sources))
	for _, s := range sources {
		out = append(out, s.Ref)
	}
	return out
}

// noLimitationText states that no rule applies. It is a sentence rather than
// an empty list so that "the platform read everything and found nothing to
// limit" cannot be confused with a section that was not filled in; the words
// it uses are exactly the negations of the rules above.
func noLimitationText(total int) string {
	n := strconv.Itoa(total)
	if total == 1 {
		return "the platform derived no limitation for this result's 1 source: it pins its object's newest live version, " +
			"with evidence asserted about it and a scientific review state the platform could read"
	}
	return "the platform derived no limitation for this result's " + n +
		" sources: each pins its object's newest live version, with evidence asserted about it and a scientific review state the platform could read"
}

// fallbackText names why an answer carries no written summary. The sentences
// are the user-facing half of the Reason vocabulary: a reader learns what
// happened to their question, not which component failed.
func fallbackText(reason Reason) string {
	switch reason {
	case ReasonNoProvider:
		return "no answer model is configured on this deployment, so this answer is the structured result: the sources " +
			"the search returned, and what the platform has recorded about them"
	case ReasonNoSources:
		return "the search returned no source, so there is nothing a written answer could cite"
	case ReasonProviderError:
		return "the answer model could not be reached, so this answer is the structured result"
	case ReasonTimeout:
		return "the answer model did not answer within the time allowed, so this answer is the structured result"
	case ReasonInvalidAnswer:
		return "the answer model returned a document that does not satisfy the answer schema, so it was refused and this " +
			"answer is the structured result"
	case ReasonUngroundedCitation:
		return "the answer model cited or named an entity the search did not return, so its document was refused and " +
			"this answer is the structured result"
	default:
		// A reason added without a sentence is a compile-clean gap, so the
		// default is a sentence that is true of every case rather than a
		// panic: the answer still falls back and still says so.
		return "no written answer was produced for this search, so this answer is the structured result"
	}
}

// signalNotRunText names a signal that did not run and why, in the words the
// retrieval's own skip reasons mean (internal/search/retrieval/retrieval.go).
// A missing signal is a limitation of the ANSWER, not of the corpus: a vector
// signal skipped for want of an embedder has not told us the corpus disagrees.
func signalNotRunText(s retrieval.SignalReport) string {
	switch s.Skipped {
	case retrieval.SkippedNoEmbedder:
		return "the vector signal did not run: no embedder is configured, so the question could not be embedded and no " +
			"stored vector could be compared against it"
	case retrieval.SkippedNoFacets:
		return "the facet signal did not run: the question carried no structured filter, and the facet signal without one " +
			"would be the newest rows of the index rather than a match"
	case retrieval.SkippedNoSeeds:
		return "the traversal signal did not run: nothing the text signals recalled resolved to a scientific object " +
			"version, so there was nowhere to start walking the relation graph from"
	default:
		if s.Skipped == "" {
			return "the " + s.Signal + " signal did not run, and no reason was reported for it"
		}
		return "the " + s.Signal + " signal did not run: " + strings.TrimSpace(s.Skipped)
	}
}
