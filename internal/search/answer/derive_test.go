package answer_test

import (
	"context"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/search/answer"
	"github.com/lichman0405/post/internal/search/ranking"
	"github.com/lichman0405/post/internal/search/retrieval"
)

// answerOver answers a ranking with no provider (so every field of the
// structured result is exercised without a script) and returns the answer.
func answerOver(t *testing.T, signals []retrieval.SignalReport, ranked ...ranking.Ranked) answer.Answer {
	t.Helper()
	got, err := newGenerator(t, answer.Deps{}).Answer(context.Background(), answer.Input{
		Result:  resultFor(ranked...),
		Signals: signals,
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	return got
}

// statementWith returns the first statement whose text contains needle.
func statementWith(t *testing.T, statements []answer.Statement, needle string) answer.Statement {
	t.Helper()
	for _, s := range statements {
		if strings.Contains(s.Text, needle) {
			return s
		}
	}
	texts := make([]string, 0, len(statements))
	for _, s := range statements {
		texts = append(texts, s.Text)
	}
	t.Fatalf("no statement contains %q; got:\n- %s", needle, strings.Join(texts, "\n- "))
	return answer.Statement{}
}

// TestLimitationsNameEverySignalThatDidNotRun: a missing signal is a
// limitation of the answer's coverage, and the two absences mean opposite
// things — a vector signal skipped for want of an embedder has not told us the
// corpus disagrees.
func TestLimitationsNameEverySignalThatDidNotRun(t *testing.T) {
	got := answerOver(t, signalsFor(), rankedAsset(assetRef, assetPID, cleanFactors()))

	vector := statementWith(t, got.Limitations, "vector signal did not run")
	if !strings.Contains(vector.Text, "no embedder is configured") {
		t.Fatalf("the vector limitation does not name the cause: %q", vector.Text)
	}
	facets := statementWith(t, got.Limitations, "facet signal did not run")
	if !strings.Contains(facets.Text, "no structured filter") {
		t.Fatalf("the facet limitation does not name the cause: %q", facets.Text)
	}
	for _, s := range got.Limitations {
		if strings.Contains(s.Text, "full_text signal") || strings.Contains(s.Text, "graph signal") {
			t.Fatalf("a signal that ran is reported as missing: %q", s.Text)
		}
		// A signal is a property of the search, not of any source, so a
		// statement about one names no refs.
		if strings.Contains(s.Text, "signal did not run") && len(s.Refs) != 0 {
			t.Fatalf("a signal limitation names sources: %v", s.Refs)
		}
	}
}

// TestLimitationsGroupPerSourceRules: forty sources must not become forty
// near-identical lines, and the group's refs are exactly the sources the rule
// matched.
func TestLimitationsGroupPerSourceRules(t *testing.T) {
	noEvidence := factorsWith(ranking.FactorEvidence, ranking.LevelNoEvidence,
		"no evidence assertion targets this version")
	got := answerOver(t, nil,
		rankedAsset(assetRef, assetPID, cleanFactors()),
		rankedKnowledge(2, knownRef, knownPID, noEvidence),
		rankedRelease(3),
	)

	stmt := statementWith(t, got.Limitations, "no evidence assertion has been recorded")
	if len(stmt.Refs) != 1 || stmt.Refs[0] != knownRef {
		t.Fatalf("the group's refs = %v, want [%s]", stmt.Refs, knownRef)
	}
	if !strings.Contains(stmt.Text, "1 source's version") {
		t.Fatalf("the singular form is not used for one source: %q", stmt.Text)
	}

	// Two matching sources take the plural form and the count.
	both := factorsWith(ranking.FactorReproduction, ranking.LevelNoReproduction, "no reproduction is recorded")
	got = answerOver(t, nil,
		rankedAsset(assetRef, assetPID, both),
		rankedKnowledge(2, knownRef, knownPID, both),
		rankedRelease(3),
	)
	stmt = statementWith(t, got.Limitations, "no reproduction has been recorded")
	if len(stmt.Refs) != 2 {
		t.Fatalf("the group's refs = %v, want two", stmt.Refs)
	}
	if !strings.Contains(stmt.Text, "2 of the 3 sources") {
		t.Fatalf("the plural form does not carry the counts: %q", stmt.Text)
	}
}

// TestUnversionedIsNotReportedAsUnread: a document that resolves to no version
// has `unknown` on every database-backed factor, and reporting it twice would
// imply a scope refusal that did not happen.
func TestUnversionedIsNotReportedAsUnread(t *testing.T) {
	state := rankedState(1)
	state.Factors = append(state.Factors,
		assessment(ranking.FactorEvidence, ranking.LevelUnknown,
			"the candidate resolves to no scientific object version, so there are no facts to read"),
		assessment(ranking.FactorConflict, ranking.LevelUnknown,
			"the candidate resolves to no scientific object version, so there are no facts to read"),
	)
	got := answerOver(t, nil, state)

	if stmt := statementWith(t, got.Limitations, "pins no scientific object version"); len(stmt.Refs) != 1 || stmt.Refs[0] != stateRef {
		t.Fatalf("the unversioned statement's refs = %v", stmt.Refs)
	}
	for _, s := range got.Limitations {
		if strings.Contains(s.Text, "outside the project scope of this search") {
			t.Fatalf("an unversioned source is reported as outside the scope: %q", s.Text)
		}
	}
}

// TestUnknownFactorsAreReportedAsUnread: the fail-closed direction. A version
// the platform could not read is reported as unread, never as a version with
// nothing recorded about it.
func TestUnknownFactorsAreReportedAsUnread(t *testing.T) {
	const unreadReason = "the version this candidate resolves to is not in the caller's project scope, so no facts about it were read"
	unread := cleanFactors()
	for i := range unread {
		if unread[i].Factor == ranking.FactorEvidence || unread[i].Factor == ranking.FactorConflict {
			unread[i].Level = ranking.LevelUnknown
			unread[i].Reason = unreadReason
		}
	}
	got := answerOver(t, nil, rankedKnowledge(1, knownRef, knownPID, unread))

	stmt := statementWith(t, got.Limitations, "outside the project scope of this search")
	if len(stmt.Refs) != 1 || stmt.Refs[0] != knownRef {
		t.Fatalf("the unread statement's refs = %v", stmt.Refs)
	}
	for _, s := range got.Limitations {
		if strings.Contains(s.Text, "no evidence assertion has been recorded") {
			t.Fatalf("an unread version is reported as having no evidence: %q", s.Text)
		}
	}
	// And the conflict section says it could not be read rather than that
	// nothing is contested.
	conflict := statementWith(t, got.Conflicts, "could not read the conflict facts")
	if len(conflict.Refs) != 1 || conflict.Refs[0] != knownRef {
		t.Fatalf("the conflict statement's refs = %v", conflict.Refs)
	}
}

// TestConflictStatementsQuoteTheRankingVerbatim: the answer restates the
// ranking's own sentence for a contradicted version, so the two accounts of
// one row cannot drift apart (provider.go).
func TestConflictStatementsQuoteTheRankingVerbatim(t *testing.T) {
	const reason = "2 contradictory evidence assertions and 1 contradicts relation touch this version"
	got := answerOver(t, nil,
		rankedAsset(assetRef, assetPID, factorsWith(ranking.FactorConflict, ranking.LevelContested, reason)),
	)
	stmt := statementWith(t, got.Conflicts, reason)
	if len(stmt.Refs) != 1 || stmt.Refs[0] != assetRef {
		t.Fatalf("the conflict statement's refs = %v", stmt.Refs)
	}
	if stmt.Origin != "platform" {
		t.Fatalf("origin = %q, want platform", stmt.Origin)
	}
	if strings.Contains(stmt.Text, "nothing on the platform contradicts") {
		t.Fatal("a contested source also got the all-clear statement")
	}
}

// TestDisputedReproductionIsAConflict: a recorded failure to reproduce
// disputes a claim to reproducibility, so it belongs in the conflict section
// even though the ranking reports it on the reproduction factor.
func TestDisputedReproductionIsAConflict(t *testing.T) {
	const reason = "2 reproductions recorded, 1 from another project, and 1 recorded failure to reproduce"
	got := answerOver(t, nil,
		rankedKnowledge(1, knownRef, knownPID,
			factorsWith(ranking.FactorReproduction, ranking.LevelDisputedReproduction, reason)),
	)
	if stmt := statementWith(t, got.Conflicts, reason); len(stmt.Refs) != 1 || stmt.Refs[0] != knownRef {
		t.Fatalf("the disputed-reproduction statement's refs = %v", stmt.Refs)
	}
	if !strings.Contains(got.Conflicts[0].Text, reason) {
		t.Fatalf("the conflict section does not lead with the dispute: %q", got.Conflicts[0].Text)
	}
}

// TestNoContradictionIsAStatement: an empty conflict list cannot distinguish
// "we read every source and found none" from "we read none of them", so the
// all-clear case is written out.
func TestNoContradictionIsAStatement(t *testing.T) {
	got := answerOver(t, nil,
		rankedAsset(assetRef, assetPID, cleanFactors()),
		rankedKnowledge(2, knownRef, knownPID, cleanFactors()),
	)
	if len(got.Conflicts) != 1 {
		t.Fatalf("conflicts = %v, want the single all-clear statement", got.Conflicts)
	}
	stmt := got.Conflicts[0]
	if !strings.Contains(stmt.Text, "nothing on the platform contradicts any of the 2 sources") {
		t.Fatalf("all-clear text = %q", stmt.Text)
	}
	if len(stmt.Refs) != 2 {
		t.Fatalf("the all-clear statement's refs = %v, want both sources", stmt.Refs)
	}
}

// TestNoLimitationIsAStatement: same reasoning for limitations. The sentence
// is only reachable when no rule matched, and the rules are closed — a source
// that matches none of them has had every fact on it read.
func TestNoLimitationIsAStatement(t *testing.T) {
	// No provider, so the fallback sentence is there too; the point is that
	// the ONLY per-source statement is the no-limitation one.
	got := answerOver(t, nil, rankedAsset(assetRef, assetPID, cleanFactors()))

	perSource := 0
	for _, s := range got.Limitations {
		if len(s.Refs) == 0 {
			continue
		}
		perSource++
		if !strings.Contains(s.Text, "derived no limitation") {
			t.Fatalf("a per-source statement other than the no-limitation one was derived: %q", s.Text)
		}
		if len(s.Refs) != 1 || s.Refs[0] != assetRef {
			t.Fatalf("the no-limitation statement's refs = %v", s.Refs)
		}
	}
	if perSource != 1 {
		t.Fatalf("per-source limitations = %d, want exactly the no-limitation statement", perSource)
	}
}

// TestStatementsOnlyEverPointAtSourcesOfTheSameAnswer: a limitation or a
// conflict that named an entity outside the source list would be exactly the
// defect this package exists to prevent — a claim the reader cannot check.
func TestStatementsOnlyEverPointAtSourcesOfTheSameAnswer(t *testing.T) {
	relevant := factorsWith(ranking.FactorConflict, ranking.LevelContested,
		"1 contradictory evidence assertion touches this version")
	got := answerOver(t, signalsFor(),
		rankedAsset(assetRef, assetPID, relevant),
		rankedKnowledge(2, knownRef, knownPID,
			factorsWith(ranking.FactorReproduction, ranking.LevelNoReproduction, "no reproduction is recorded")),
		rankedState(3),
	)

	refs := map[string]bool{}
	for _, s := range got.Sources {
		refs[s.Ref] = true
	}
	for _, section := range [][]answer.Statement{got.Limitations, got.Conflicts} {
		for _, stmt := range section {
			for _, ref := range stmt.Refs {
				if !refs[ref] {
					t.Fatalf("statement %q names %q, which is not a source of this answer", stmt.Text, ref)
				}
			}
		}
	}
	// Every source is accounted for: the sections together mention each of
	// them at least once, so no source is silently outside the analysis.
	for _, s := range got.Sources {
		found := false
		for _, section := range [][]answer.Statement{got.Limitations, got.Conflicts} {
			for _, stmt := range section {
				for _, ref := range stmt.Refs {
					found = found || ref == s.Ref
				}
			}
		}
		if !found {
			t.Fatalf("source %s appears in neither section", s.Ref)
		}
	}
}
