package answer

import (
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/search"
	"github.com/lichman0405/post/internal/search/ranking"
	"github.com/lichman0405/post/internal/search/retrieval"
)

// The ranked result the guard's tests are about: one asset document and one
// knowledge document, the two citation shapes the platform produces.
func testRanked() []ranking.Ranked {
	return []ranking.Ranked{
		{
			Rank: 1, Ref: "asset:AST-0001@2", Kind: retrieval.KindDocument,
			EntityType: "asset", Title: "Mg-MOF-74 CO2 uptake at 298 K", Version: "2",
			Candidate: retrieval.Candidate{
				Ref: "asset:AST-0001@2", Kind: retrieval.KindDocument, EntityType: "asset",
				Identity: "AST-0001", Version: "2", ProjectID: "3f2a5b7c-1d4e-4a6b-8c9d-0e1f2a3b4c5d",
				ObjectID: "1d2c3b4a-5e6f-4071-8293-a4b5c6d7e8f9",
			},
		},
		{
			Rank: 2, Ref: "knowledge:KNW-0007@1", Kind: retrieval.KindDocument,
			EntityType: "knowledge", Title: "The uptake is reversible", Version: "1",
			ObjectVersionID: "9f8e7d6c-5b4a-4392-8170-6f5e4d3c2b1a",
			Candidate: retrieval.Candidate{
				Ref: "knowledge:KNW-0007@1", Kind: retrieval.KindDocument, EntityType: "knowledge",
				Identity: "KNW-0007", Version: "1", ProjectID: "3f2a5b7c-1d4e-4a6b-8c9d-0e1f2a3b4c5d",
				ObjectID:        "1d2c3b4a-5e6f-4071-8293-a4b5c6d7e8f9",
				ObjectVersionID: "9f8e7d6c-5b4a-4392-8170-6f5e4d3c2b1a",
			},
		},
	}
}

// TestGuardAcceptsOnlyRetrievedRefs is the acceptance criterion of T0906:
// "构造 hallucination test：模型不可引用不存在 id". The table is the criterion
// in both directions — every document that invents an entity is refused, and
// every document that cites what retrieval returned is accepted, because a
// guard that refused everything would satisfy the first half alone.
func TestGuardAcceptsOnlyRetrievedRefs(t *testing.T) {
	vocab := vocabularyFor(testRanked())

	cases := []struct {
		name       string
		summary    string
		citations  []string
		violations int
		// want is a substring the first violation must contain, when the
		// document is refused.
		want string
	}{
		{
			name:      "both refs cited and named",
			summary:   "The uptake is reversible (knowledge:KNW-0007@1); it was measured on a Mg-MOF-74 sample (asset:AST-0001@2).",
			citations: []string{"asset:AST-0001@2", "knowledge:KNW-0007@1"},
		},
		{
			name:       "a ref the search returned in a different version",
			summary:    "The uptake is 3.2 mmol/g.",
			citations:  []string{"asset:AST-0001@1"},
			violations: 1,
			want:       `citation "asset:AST-0001@1"`,
		},
		{
			name:       "a completely invented pid",
			summary:    "The uptake is 3.2 mmol/g.",
			citations:  []string{"asset:AST-9999@2"},
			violations: 1,
			want:       `citation "asset:AST-9999@2"`,
		},
		{
			name:       "a ref of an entity kind the search never recalled",
			summary:    "The uptake is 3.2 mmol/g.",
			citations:  []string{"release:00000000-0000-4000-8000-000000000000"},
			violations: 1,
			want:       "citation",
		},
		{
			name:       "an id invented in the summary rather than cited",
			summary:    "The claim claim:CLM-0001@3 supports the measurement.",
			citations:  []string{"asset:AST-0001@2"},
			violations: 1,
			want:       `token "claim:CLM-0001@3"`,
		},
		{
			name:       "a uuid invented in the summary",
			summary:    "The version 0f0e0d0c-0b0a-4988-8776-655443322110 was reproduced.",
			citations:  []string{"asset:AST-0001@2"},
			violations: 1,
			want:       `token "0f0e0d0c-0b0a-4988-8776-655443322110"`,
		},
		{
			name:       "a source outside the platform",
			summary:    "See https://example.org/paper/1234 for the measurement.",
			citations:  []string{"asset:AST-0001@2"},
			violations: 1,
			want:       "https://example.org/paper/1234",
		},
		{
			name: "an identity retrieval returned, named in passing",
			// The pid and the object-version uuid both travelled with the
			// candidates, so naming them is naming what the search found.
			summary:   "Publication KNW-0007 pins version 9f8e7d6c-5b4a-4392-8170-6f5e4d3c2b1a.",
			citations: []string{"knowledge:KNW-0007@1"},
		},
		{
			name: "ordinary prose",
			// Chemical formulas, units, ratios, temperatures and a DOI
			// without a scheme are what the answers this pipeline exists to
			// produce are made of.
			summary: "Mg-MOF-74 takes up 3.2 mmol/g of CO2 at 298 K (1:2 molar ratio, 70% of the theoretical maximum); " +
				"doi:10.1021/ja00001a001 records it.",
			citations: []string{"asset:AST-0001@2"},
		},
		{
			name:      "an empty summary is not this guard's business",
			summary:   "",
			citations: []string{"asset:AST-0001@2"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := document{AnswerVersion: AnswerVersion, Summary: tc.summary, Citations: tc.citations}
			got := checkDocument(doc, vocab)
			if len(got) != tc.violations {
				t.Fatalf("checkDocument returned %d violations (%s), want %d:\nsummary %q\ncitations %v",
					len(got), violationsError(got), tc.violations, tc.summary, tc.citations)
			}
			if tc.violations > 0 && !strings.Contains(got[0].String(), tc.want) {
				t.Fatalf("first violation is %q, want it to contain %q", got[0].String(), tc.want)
			}
		})
	}
}

// TestGuardRefusesTheWholeDocumentNotTheCitation states the policy the package
// doc argues for: the refusal is TOTAL. Pruning the offending citation would
// publish a summary whose evidence was deleted from under it, and would
// publish it as an ANSWERED answer.
func TestGuardRefusesTheWholeDocumentNotTheCitation(t *testing.T) {
	vocab := vocabularyFor(testRanked())
	doc := document{
		AnswerVersion: AnswerVersion,
		Summary:       "The uptake is reversible.",
		Citations:     []string{"asset:AST-0001@2", "knowledge:KNW-9999@1"},
	}
	got := checkDocument(doc, vocab)
	if len(got) != 1 {
		t.Fatalf("violations = %s, want exactly the one invented citation", violationsError(got))
	}
	if got[0].Kind != violationCitation {
		t.Fatalf("violation kind = %q, want %q", got[0].Kind, violationCitation)
	}
}

// TestGuardReportsEveryViolation: the list is what the log line names, so a
// document with three invented entities must report three, in a deterministic
// order (citations first, then the summary's tokens in the order written).
func TestGuardReportsEveryViolation(t *testing.T) {
	vocab := vocabularyFor(testRanked())
	doc := document{
		AnswerVersion: AnswerVersion,
		Summary:       "asset:AST-0001@2 was reproduced by 11111111-1111-4111-8111-111111111111 and asset:AST-0002@1.",
		Citations:     []string{"asset:AST-0001@2", "asset:AST-0002@1"},
	}
	got := checkDocument(doc, vocab)
	want := []string{
		`citation "asset:AST-0002@1"`,
		`token "11111111-1111-4111-8111-111111111111"`,
		`token "asset:AST-0002@1"`,
	}
	if len(got) != len(want) {
		t.Fatalf("violations = %s, want %v", violationsError(got), want)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Fatalf("violation %d = %q, want %q", i, got[i].String(), want[i])
		}
	}
}

// TestRefKindPrefixesCoverTheProjection is the closure of the guard's
// vocabulary: every entity type the projection can write is a prefix the guard
// recognises, so a ref-shaped token of a kind this platform produces can never
// pass as prose. A projection that gains an entity type fails here until the
// guard is taught it.
func TestRefKindPrefixesCoverTheProjection(t *testing.T) {
	vocab := vocabularyFor(testRanked())
	kinds := append(search.EntityTypes(), retrieval.RefKindObjectVersion)
	if len(kinds) < 2 {
		t.Fatalf("the projection reports %d entity types; the guard's closure cannot be checked", len(kinds))
	}
	for _, kind := range kinds {
		token := kind + ":INVENTED-0001@1"
		if vocab.holds(token) {
			t.Fatalf("the vocabulary holds %q, which no candidate carries", token)
		}
		if !namesEntity(token) {
			t.Fatalf("token %q is not recognised as a ref of kind %q", token, kind)
		}
		if got := vocab.ungrounded(token); len(got) != 1 {
			t.Fatalf("ungrounded(%q) = %v, want the token itself", token, got)
		}
	}
}

// TestGuardVocabularyIsExactAboutVersions: ADR-010's citation is
// version-pinned, so a citation the platform did not return is refused even
// when it names a real entity. The refusal is what makes "the answer cites the
// version it read" a property of the design rather than of the provider's
// diligence.
func TestGuardVocabularyIsExactAboutVersions(t *testing.T) {
	vocab := vocabularyFor(testRanked())
	for _, ref := range []string{"asset:AST-0001@2", "knowledge:KNW-0007@1"} {
		if !vocab.refs[ref] {
			t.Fatalf("ref %q from the ranking is not in the vocabulary", ref)
		}
	}
	for _, ref := range []string{"asset:AST-0001", "asset:AST-0001@1", "asset:AST-0001@3", "prefix:asset:AST-0001@2"} {
		if vocab.refs[ref] {
			t.Fatalf("ref %q is in the vocabulary; the ranking never returned it", ref)
		}
	}
}

// TestGuardVocabularyCarriesTheCandidatesIdentities: a summary may name an
// entity it is citing by its own identity (the pid, the object uuid), which is
// what makes a legitimate sentence about a returned source pass. The
// identities come from the candidates, not from the summary.
func TestGuardVocabularyCarriesTheCandidatesIdentities(t *testing.T) {
	vocab := vocabularyFor(testRanked())
	for _, token := range []string{
		"AST-0001",
		"KNW-0007",
		"1d2c3b4a-5e6f-4071-8293-a4b5c6d7e8f9",
		"9f8e7d6c-5b4a-4392-8170-6f5e4d3c2b1a",
	} {
		if !vocab.holds(token) {
			t.Fatalf("token %q travels with a candidate but is not in the vocabulary", token)
		}
	}
}
