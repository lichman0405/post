package answer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/search/answer"
	"github.com/lichman0405/post/internal/search/answer/answertest"
	"github.com/lichman0405/post/internal/search/ranking"
	"github.com/lichman0405/post/internal/search/retrieval"
)

const question = "Which MOF materials show CO2 uptake above 3 mmol/g at 298 K?"

// groundedDocument is the provider document the acceptance paths use: a
// summary that cites the asset the ranking returned and names nothing else.
const groundedDocument = `{
  "answer_version": "1",
  "summary": "Mg-MOF-74 takes up 3.2 mmol/g of CO2 at 298 K (asset:AST-0001@2).",
  "citations": ["asset:AST-0001@2"]
}`

// resultFor builds the ranking a test answers from.
func resultFor(ranked ...ranking.Ranked) ranking.Result {
	return ranking.Result{Query: question, Factors: ranking.Factors(), Ranked: ranked}
}

// signalsFor is a retrieval signal report: the text and graph signals ran, the
// vector signal did not (no embedder is configured) and the facet signal did
// not (the question carried no filter). It is the report a deployment without
// an embedder produces, and it is what puts a limitation about coverage on
// every answer in these tests.
func signalsFor() []retrieval.SignalReport {
	return []retrieval.SignalReport{
		{Signal: retrieval.SignalFullText, Ran: true, Hits: 3},
		{Signal: retrieval.SignalVector, Ran: false, Skipped: retrieval.SkippedNoEmbedder},
		{Signal: retrieval.SignalFacets, Ran: false, Skipped: retrieval.SkippedNoFacets},
		{Signal: retrieval.SignalGraph, Ran: true, Hits: 2},
	}
}

// newGenerator builds a generator, or fails the test.
func newGenerator(t *testing.T, deps answer.Deps) *answer.Generator {
	t.Helper()
	g, err := answer.New(deps)
	if err != nil {
		t.Fatalf("answer.New: %v", err)
	}
	return g
}

// answerFor runs one answer over the given provider and ranking.
func answerFor(t *testing.T, deps answer.Deps, in answer.Input) answer.Answer {
	t.Helper()
	got, err := newGenerator(t, deps).Answer(context.Background(), in)
	if err != nil {
		t.Fatalf("Answer returned an error: %v", err)
	}
	return got
}

// TestAnswerCarriesGroundedSummary is the acceptance path: a document that
// cites what retrieval returned is ACCEPTED, carried as a View, and marked on
// the source it names. A generator that fell back unconditionally would
// satisfy every refusal test in this file on its own.
func TestAnswerCarriesGroundedSummary(t *testing.T) {
	provider := answertest.Reply(groundedDocument)
	got := answerFor(t, answer.Deps{Provider: provider}, answer.Input{
		Result:  resultFor(rankedAsset(assetRef, assetPID, cleanFactors())),
		Signals: signalsFor(),
	})

	if got.Status != answer.StatusAnswered {
		t.Fatalf("status = %s, want %s", got.String(), answer.StatusAnswered)
	}
	if got.Reason != "" {
		t.Fatalf("an answered answer carries no reason, got %q", got.Reason)
	}
	if !got.AnswerView {
		t.Fatal("AnswerView is false on a model-written summary; docs/14 §4 requires the label")
	}
	if got.Summary != "Mg-MOF-74 takes up 3.2 mmol/g of CO2 at 298 K (asset:AST-0001@2)." {
		t.Fatalf("summary = %q", got.Summary)
	}
	if len(got.Citations) != 1 || got.Citations[0] != assetRef {
		t.Fatalf("citations = %v, want [%s]", got.Citations, assetRef)
	}
	if len(got.Sources) != 1 || !got.Sources[0].Cited {
		t.Fatalf("the cited source is not marked: %+v", got.Sources)
	}
	if got.Version != answer.AnswerVersion {
		t.Fatalf("answer version = %q, want %q", got.Version, answer.AnswerVersion)
	}
}

// TestAnswerAlwaysCarriesLimitationsConflictsAndSources is the second
// requirement of T0906 ("回答含 limitations/conflicts/sources") stated as an
// invariant across every outcome this package can produce.
func TestAnswerAlwaysCarriesLimitationsConflictsAndSources(t *testing.T) {
	cases := []struct {
		name     string
		deps     answer.Deps
		input    answer.Input
		sources  int
		wantKeep string
	}{
		{
			name:    "answered",
			deps:    answer.Deps{Provider: answertest.Reply(groundedDocument)},
			input:   answer.Input{Result: resultFor(rankedAsset(assetRef, assetPID, cleanFactors())), Signals: signalsFor()},
			sources: 1,
		},
		{
			name:    "no provider",
			deps:    answer.Deps{},
			input:   answer.Input{Result: resultFor(rankedAsset(assetRef, assetPID, cleanFactors())), Signals: signalsFor()},
			sources: 1,
		},
		{
			name:    "no sources",
			deps:    answer.Deps{Provider: answertest.Reply(groundedDocument)},
			input:   answer.Input{Result: resultFor(), Signals: signalsFor()},
			sources: 0,
		},
		{
			name:    "hallucinated",
			deps:    answer.Deps{Provider: answertest.Reply(`{"answer_version":"1","summary":"x","citations":["asset:AST-9999@2"]}`)},
			input:   answer.Input{Result: resultFor(rankedAsset(assetRef, assetPID, cleanFactors())), Signals: signalsFor()},
			sources: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := answerFor(t, tc.deps, tc.input)
			if len(got.Sources) != tc.sources {
				t.Fatalf("sources = %d, want %d", len(got.Sources), tc.sources)
			}
			if len(got.Limitations) == 0 {
				t.Fatal("limitations are empty; the sections are never optional (docs/14 §4)")
			}
			// Conflicts is empty in exactly one case: a result with no source
			// at all, where there is nothing whose conflicts could be read
			// (derive.go). Every other answer states either the
			// contradictions it found or that it found none.
			if len(got.Sources) > 0 && len(got.Conflicts) == 0 {
				t.Fatal("conflicts are empty over a result that has sources")
			}
			if len(got.Sources) == 0 && len(got.Conflicts) != 0 {
				t.Fatal("conflicts are stated about a result with no source")
			}
			for _, l := range got.Limitations {
				if l.Origin != "platform" {
					t.Fatalf("limitation origin = %q, want platform: every statement this package writes is derived", l.Origin)
				}
				if strings.TrimSpace(l.Text) == "" {
					t.Fatal("a limitation carries no sentence")
				}
			}
			if _, err := got.CanonicalJSON(); err != nil {
				t.Fatalf("CanonicalJSON: %v", err)
			}
		})
	}
}

// TestFallbackWithoutProviderNamesTheDeployment is the supported-state test:
// a deployment that has answered nothing about third-party models runs every
// search to a structured answer, and the answer says why.
func TestFallbackWithoutProviderNamesTheDeployment(t *testing.T) {
	got := answerFor(t, answer.Deps{}, answer.Input{
		Result:  resultFor(rankedAsset(assetRef, assetPID, cleanFactors())),
		Signals: signalsFor(),
	})
	if got.Status != answer.StatusFallback || got.Reason != answer.ReasonNoProvider {
		t.Fatalf("status = %s, want fallback (no_provider)", got.String())
	}
	if got.Summary != "" || got.AnswerView {
		t.Fatalf("a fallback carries no summary and is not a View: %+v", got)
	}
	if !strings.Contains(got.Limitations[0].Text, "no answer model is configured") {
		t.Fatalf("first limitation = %q, want the deployment fact", got.Limitations[0].Text)
	}
	if got.Sources[0].Cited {
		t.Fatal("a fallback marks no source as cited")
	}
}

// TestAnswerDoesNotAskWithoutSources: the provider is not called when the
// search returned nothing. A model asked to answer with an empty citation
// vocabulary has one material to work from, and it is not the platform's.
func TestAnswerDoesNotAskWithoutSources(t *testing.T) {
	provider := answertest.Reply(groundedDocument)
	got := answerFor(t, answer.Deps{Provider: provider}, answer.Input{Result: resultFor(), Signals: signalsFor()})
	if got.Status != answer.StatusFallback || got.Reason != answer.ReasonNoSources {
		t.Fatalf("status = %s, want fallback (no_sources)", got.String())
	}
	if provider.Calls() != 0 {
		t.Fatalf("the provider was called %d times for a search with no source", provider.Calls())
	}
	if !strings.Contains(got.Limitations[0].Text, "returned no source") {
		t.Fatalf("first limitation = %q", got.Limitations[0].Text)
	}
}

// TestAnswerFallsBackOnProviderFailures covers the provider-shaped failures
// docs/32's mitigation names ("fallback structured results"): each one costs
// the summary and never the sources.
func TestAnswerFallsBackOnProviderFailures(t *testing.T) {
	in := answer.Input{Result: resultFor(rankedAsset(assetRef, assetPID, cleanFactors())), Signals: signalsFor()}

	t.Run("error", func(t *testing.T) {
		got := answerFor(t, answer.Deps{Provider: answertest.Failing(errors.New("upstream 503"))}, in)
		if got.Status != answer.StatusFallback || got.Reason != answer.ReasonProviderError {
			t.Fatalf("status = %s, want fallback (provider_error)", got.String())
		}
		if len(got.Sources) != 1 {
			t.Fatal("the sources are lost on a provider error")
		}
	})

	t.Run("timeout", func(t *testing.T) {
		provider := answertest.Slow(2*time.Second, groundedDocument)
		got := answerFor(t, answer.Deps{Provider: provider, Timeout: 20 * time.Millisecond}, in)
		if got.Status != answer.StatusFallback || got.Reason != answer.ReasonTimeout {
			t.Fatalf("status = %s, want fallback (provider_timeout)", got.String())
		}
		if !strings.Contains(got.Limitations[0].Text, "within the time allowed") {
			t.Fatalf("first limitation = %q, want the deadline fact", got.Limitations[0].Text)
		}
	})

	t.Run("not json", func(t *testing.T) {
		got := answerFor(t, answer.Deps{Provider: answertest.Reply("I think the answer is 3.2 mmol/g.")}, in)
		if got.Status != answer.StatusFallback || got.Reason != answer.ReasonInvalidAnswer {
			t.Fatalf("status = %s, want fallback (invalid_answer)", got.String())
		}
	})
}

// TestAnswerFallsBackOnSchemaViolations: the schema is the first half of the
// guard, and each of these documents is refused by it rather than by the
// grounding check.
func TestAnswerFallsBackOnSchemaViolations(t *testing.T) {
	in := answer.Input{Result: resultFor(rankedAsset(assetRef, assetPID, cleanFactors())), Signals: signalsFor()}
	docs := []struct {
		name string
		doc  string
	}{
		{"an unknown field", `{"answer_version":"1","summary":"x","citations":["asset:AST-0001@2"],"confidence":0.9}`},
		{"no citations at all", `{"answer_version":"1","summary":"the evidence shows it is 3.2 mmol/g"}`},
		{"an empty citation list", `{"answer_version":"1","summary":"x","citations":[]}`},
		{"the wrong version", `{"answer_version":"2","summary":"x","citations":["asset:AST-0001@2"]}`},
		{"a duplicated citation", `{"answer_version":"1","summary":"x","citations":["asset:AST-0001@2","asset:AST-0001@2"]}`},
		{"a non-string citation", `{"answer_version":"1","summary":"x","citations":[42]}`},
		{"an empty summary", `{"answer_version":"1","summary":"","citations":["asset:AST-0001@2"]}`},
	}
	for _, tc := range docs {
		t.Run(tc.name, func(t *testing.T) {
			got := answerFor(t, answer.Deps{Provider: answertest.Reply(tc.doc)}, in)
			if got.Status != answer.StatusFallback || got.Reason != answer.ReasonInvalidAnswer {
				t.Fatalf("status = %s, want fallback (invalid_answer)", got.String())
			}
		})
	}
}

// TestAnswerRefusesHallucinatedCitations is the acceptance criterion:
// "构造 hallucination test：模型不可引用不存在 id". The assertion is not only
// that the answer falls back — it is that the invented identity appears
// NOWHERE in the published document. A refusal that leaked the id into a
// limitation, a log-facing field or a source would be the defect it is
// supposed to prevent.
func TestAnswerRefusesHallucinatedCitations(t *testing.T) {
	const invented = "asset:AST-9999@7"
	doc := `{"answer_version":"1","summary":"The uptake is 3.2 mmol/g (` + invented + `).","citations":["` + invented + `"]}`

	got := answerFor(t, answer.Deps{Provider: answertest.Reply(doc)}, answer.Input{
		Result:  resultFor(rankedAsset(assetRef, assetPID, cleanFactors())),
		Signals: signalsFor(),
	})
	if got.Status != answer.StatusFallback || got.Reason != answer.ReasonUngroundedCitation {
		t.Fatalf("status = %s, want fallback (ungrounded_citation)", got.String())
	}
	if got.Summary != "" || len(got.Citations) != 0 {
		t.Fatalf("the refused document reached the answer: summary %q, citations %v", got.Summary, got.Citations)
	}
	rendered, err := got.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if bytes.Contains(rendered, []byte("AST-9999")) {
		t.Fatalf("the invented identity reached the answer document:\n%s", rendered)
	}
	if !strings.Contains(got.Limitations[0].Text, "cited or named an entity the search did not return") {
		t.Fatalf("first limitation = %q, want the refusal named", got.Limitations[0].Text)
	}
	if len(got.Sources) != 1 || got.Sources[0].Ref != assetRef {
		t.Fatalf("the real source is gone: %+v", got.Sources)
	}
}

// TestAnswerRefusesAnEntityNamedOnlyInTheSummary: the citation list is the
// easy half. A summary that names an entity it does not cite is the same
// defect written in prose, and the guard refuses the whole document rather
// than pruning (grounding.go).
func TestAnswerRefusesAnEntityNamedOnlyInTheSummary(t *testing.T) {
	const invented = "0f0e0d0c-0b0a-4988-8776-655443322110"
	doc := `{"answer_version":"1","summary":"Version ` + invented + ` shows the same uptake.","citations":["asset:AST-0001@2"]}`

	got := answerFor(t, answer.Deps{Provider: answertest.Reply(doc)}, answer.Input{
		Result:  resultFor(rankedAsset(assetRef, assetPID, cleanFactors())),
		Signals: signalsFor(),
	})
	if got.Status != answer.StatusFallback || got.Reason != answer.ReasonUngroundedCitation {
		t.Fatalf("status = %s, want fallback (ungrounded_citation)", got.String())
	}
	if bytes.Contains(mustJSON(t, got), []byte(invented)) {
		t.Fatal("the invented identity reached the answer document")
	}
}

// TestAnswerRefusesTheWholeDocumentNotTheCitation: a document with one honest
// citation and one invented one is REFUSED WHOLE. Pruning would publish a
// summary whose evidence was deleted from under it, marked as grounded.
func TestAnswerRefusesTheWholeDocumentNotTheCitation(t *testing.T) {
	doc := `{"answer_version":"1","summary":"The uptake is reversible (knowledge:KNW-0007@1).","citations":["asset:AST-0001@2","knowledge:KNW-0007@1"]}`

	got := answerFor(t, answer.Deps{Provider: answertest.Reply(doc)}, answer.Input{
		Result:  resultFor(rankedAsset(assetRef, assetPID, cleanFactors())),
		Signals: signalsFor(),
	})
	if got.Status != answer.StatusFallback || got.Reason != answer.ReasonUngroundedCitation {
		t.Fatalf("status = %s, want fallback (ungrounded_citation)", got.String())
	}
	if got.Summary != "" || len(got.Citations) != 0 {
		t.Fatalf("the refused document was partially kept: %q %v", got.Summary, got.Citations)
	}
	if bytes.Contains(mustJSON(t, got), []byte(knownRef)) {
		t.Fatal("the citation the document also carried survived the refusal")
	}
}

// TestAnswerReturnsTheCallersCancellation: a caller that has stopped waiting
// is not served by a fallback nobody will read, and hiding its cancellation
// inside a successful return would tell it a search happened that did not
// (planner.Planner's rule).
func TestAnswerReturnsTheCallersCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := newGenerator(t, answer.Deps{Provider: answertest.Reply(groundedDocument)}).
		Answer(ctx, answer.Input{Result: resultFor(rankedAsset(assetRef, assetPID, cleanFactors())), Signals: signalsFor()})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

// TestAnswerIsDeterministic: the same ranking and the same provider document
// produce byte-identical answers, which is what lets tests/answer pin the
// canonical rendering.
func TestAnswerIsDeterministic(t *testing.T) {
	g := newGenerator(t, answer.Deps{Provider: answertest.Reply(groundedDocument)})
	in := answer.Input{
		Result: resultFor(
			rankedAsset(assetRef, assetPID, cleanFactors()),
			rankedKnowledge(2, knownRef, knownPID, factorsWith(ranking.FactorEvidence, ranking.LevelNoEvidence,
				"no evidence assertion targets this version")),
		),
		Signals: signalsFor(),
	}
	first, err := g.Answer(context.Background(), in)
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	second, err := g.Answer(context.Background(), in)
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	a, err := first.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	b, err := second.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("two runs differ:\n%s\n%s", a, b)
	}
}

// TestProviderRequestCarriesTheQuestionAndTheVocabularyAndNothingElse asserts
// the port's contract from the outside: what the generator SENDS. The scope,
// the actor and the project membership are absent by design (docs/54 #1 and
// #7), and the citation vocabulary is exactly the ranked candidates — so an
// adapter has nothing to cite but what the search found.
func TestProviderRequestCarriesTheQuestionAndTheVocabularyAndNothingElse(t *testing.T) {
	provider := answertest.Reply(groundedDocument)
	answerFor(t, answer.Deps{Provider: provider}, answer.Input{
		Result: resultFor(
			rankedAsset(assetRef, assetPID, cleanFactors()),
			rankedKnowledge(2, knownRef, knownPID, cleanFactors()),
		),
		Signals: signalsFor(),
	})

	requests := provider.Requests()
	if len(requests) != 1 {
		t.Fatalf("the provider was called %d times, want 1", len(requests))
	}
	req := requests[0]
	if req.Query != question {
		t.Fatalf("query = %q, want the question verbatim", req.Query)
	}
	if len(req.Citable) != 2 || req.Citable[0].Ref != assetRef || req.Citable[1].Ref != knownRef {
		t.Fatalf("citable = %+v, want the two ranked refs in rank order", req.Citable)
	}
	if req.Citable[0].Reasons[0] != cleanFactors()[0].Reason {
		t.Fatalf("citable reasons are not the ranking's own: %v", req.Citable[0].Reasons)
	}
	if !bytes.Equal(req.AnswerSchema, answer.AnswerSchema()) {
		t.Fatal("the request does not carry the packaged answer schema")
	}
	// The request type is the port's whole surface: a field added to it must
	// be a fact about the question or about a citable entity, never a fact
	// about the user (provider.go). This is the assertion that a scope or an
	// actor id would fail.
	rendered, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	for _, leak := range []string{projectID, "scope", "actor", "user_id", "principal", "allowed_project"} {
		if bytes.Contains(rendered, []byte(leak)) {
			t.Fatalf("the request to the provider carries %q:\n%s", leak, rendered)
		}
	}
}

// TestAnswerSchemaIsACopy: an adapter that mutates the bytes it was handed
// must not be able to change what this package validates against.
func TestAnswerSchemaIsACopy(t *testing.T) {
	first := answer.AnswerSchema()
	if len(first) == 0 {
		t.Fatal("AnswerSchema is empty")
	}
	first[0] = 'x'
	if second := answer.AnswerSchema(); second[0] != '{' {
		t.Fatal("AnswerSchema returns the package's own bytes, not a copy")
	}
}

func mustJSON(t *testing.T, a answer.Answer) []byte {
	t.Helper()
	out, err := a.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	return out
}
