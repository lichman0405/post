// The API's half of T0906's acceptance criteria.
//
// The answer layer is tested where it lives (internal/search/answer and the
// golden fixtures in tests/answer). What is tested HERE is the thing only the
// transport can show: that a hallucinated citation does not reach the caller
// and does not reach the RECORD — the row docs/22 §8 requires the server to
// save and the contract addresses by search id afterwards.
//
// The answer generator under test is the real one, driven by a scripted
// provider (internal/search/answer/answertest), so the refusal is produced by
// the production guard rather than simulated. The store is a fake: what the
// real store's writer adds to this is a SQL INSERT, and what the real
// DATABASE adds to it is a CHECK (citations <@ selected_refs) that this test
// cannot exercise without PostgreSQL — tests/integration/search_answer_test.go
// is where that half lives.
package searchhttp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/search/answer"
	"github.com/lichman0405/post/internal/search/answer/answertest"
	"github.com/lichman0405/post/internal/search/ranking"
	"github.com/lichman0405/post/internal/search/retrieval"
)

// groundedAnswer is a document the provider may write: it cites the one ref
// retrieval returned, and nothing else.
const groundedAnswer = `{
  "answer_version": "1",
  "summary": "Mg-MOF-74 takes up CO2 at 298 K [asset:AST-0001@2].",
  "citations": ["asset:AST-0001@2"]
}`

// hallucinatedAnswer is the same answer with an invented source: AST-9999 was
// never recalled, so the platform has said nothing about it.
const hallucinatedAnswer = `{
  "answer_version": "1",
  "summary": "Mg-MOF-74 takes up 3.2 mmol/g [asset:AST-9999@7], better than the earlier report [asset:AST-0001@2].",
  "citations": ["asset:AST-9999@7", "asset:AST-0001@2"]
}`

// searchWithProvider runs one search whose answer comes from a real generator
// over doc, and returns the answer the caller received and the record written.
func searchWithProvider(t *testing.T, doc string) (answer.Answer, persistence.SearchRecord) {
	t.Helper()
	provider := answertest.Reply(doc)
	t.Cleanup(provider.Close)
	generator, err := answer.New(answer.Deps{Provider: provider})
	if err != nil {
		t.Fatalf("answer.New: %v", err)
	}
	records := &fakeRecords{id: "11111111-2222-4333-8444-555555555555"}
	svc := newPipeline(t, Deps{
		Retriever: &fakeRetriever{result: retrieval.Result{Query: "co2 uptake", Signals: signalsFor()}},
		Ranker: &fakeRanker{result: ranking.Result{
			Query: "co2 uptake", Factors: ranking.Factors(), Ranked: []ranking.Ranked{rankedAsset()},
		}},
		Answerer: generator,
		Records:  records,
	})
	ans, id, err := svc.search(context.Background(), actorID, "co2 uptake", nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if id != records.id {
		t.Fatalf("search id = %q, want %q", id, records.id)
	}
	if provider.Calls() != 1 {
		t.Fatalf("the provider was called %d times, want once", provider.Calls())
	}
	return ans, records.got
}

// TestHallucinatedCitationIsRefusedAndLeavesNoTrace is acceptance criterion
// one ("模型不可引用不存在 id"), at the boundary a caller sees.
//
// Three things are asserted, and the third is the one that matters: the
// invented identity must appear NOWHERE — not in the summary, not in the
// citations, not in the record the platform saved. A refusal that answered a
// fallback while storing the model's document would pass a test that only
// looked at the response.
func TestHallucinatedCitationIsRefusedAndLeavesNoTrace(t *testing.T) {
	ans, rec := searchWithProvider(t, hallucinatedAnswer)

	if ans.Status != answer.StatusFallback || ans.Reason != answer.ReasonUngroundedCitation {
		t.Fatalf("answer = %s/%s, want a fallback naming the ungrounded citation", ans.Status, ans.Reason)
	}
	if ans.Summary != "" || len(ans.Citations) != 0 {
		t.Fatalf("the refused document survived in the answer: summary=%q citations=%v", ans.Summary, ans.Citations)
	}
	// The structured result is what the caller gets instead, so the search
	// still answered something.
	if len(ans.Sources) != 1 || ans.Sources[0].Ref != assetRef {
		t.Fatalf("the fallback carries %v, want the ranked candidate", ans.Sources)
	}

	if len(rec.Citations) != 0 {
		t.Fatalf("the record cites %v, want nothing: the document was refused", rec.Citations)
	}
	if len(rec.SelectedRefs) != 1 || rec.SelectedRefs[0] != assetRef {
		t.Fatalf("the record selected %v, want the ranking's refs", rec.SelectedRefs)
	}
	for _, field := range []struct {
		name string
		doc  []byte
	}{{"answer", rec.Answer}, {"plan", rec.Plan}, {"signals", rec.Signals}} {
		if strings.Contains(string(field.doc), "AST-9999") {
			t.Fatalf("the invented identity reached the record's %s: %s", field.name, field.doc)
		}
	}
	// And the one place a fabricated source would be published: the answer
	// document as the caller receives it.
	canonical, err := ans.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if strings.Contains(string(canonical), "AST-9999") {
		t.Fatalf("the invented identity reached the answer document:\n%s", canonical)
	}
}

// TestGroundedCitationIsAnsweredAndRecorded is the other side of the same
// coin, and it is what keeps the test above from being satisfiable by an
// implementation that refuses everything: a compliant document is answered,
// its citation is recorded, and the record's citations are a subset of the
// refs the search selected — the invariant the table's CHECK states.
func TestGroundedCitationIsAnsweredAndRecorded(t *testing.T) {
	ans, rec := searchWithProvider(t, groundedAnswer)

	if ans.Status != answer.StatusAnswered {
		t.Fatalf("answer = %s/%s, want answered", ans.Status, ans.Reason)
	}
	if ans.Summary == "" {
		t.Fatal("the answered summary is empty")
	}
	if len(rec.Citations) != 1 || rec.Citations[0] != assetRef {
		t.Fatalf("the record cites %v, want [%s]", rec.Citations, assetRef)
	}
	selected := map[string]bool{}
	for _, ref := range rec.SelectedRefs {
		selected[ref] = true
	}
	for _, ref := range rec.Citations {
		if !selected[ref] {
			t.Fatalf("the record cites %q, which the search did not select: the CHECK would refuse this row", ref)
		}
	}
	// The recorded answer is the canonical document the caller received, so
	// a reader of the row sees what was published and not a re-rendering.
	canonical, err := ans.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if string(rec.Answer) != string(canonical) {
		t.Fatalf("the recorded answer differs from the published one:\nrecord: %s\npublished: %s", rec.Answer, canonical)
	}
	// Determinism, at the boundary where it is checkable: the same provider
	// reply over the same result produces the same bytes.
	if _, second := searchWithProvider(t, groundedAnswer); string(second.Answer) != string(rec.Answer) {
		t.Fatal("the same search produced two different records")
	}
}

// TestTheRecordedAnswerIsTheContractDocument: the response the caller gets and
// the row the server saves carry the SAME answer bytes, and the response's
// answer is a document (an object with the answer's own version), not a
// re-rendering by the transport.
func TestTheRecordedAnswerIsTheContractDocument(t *testing.T) {
	_, rec := searchWithProvider(t, groundedAnswer)

	var doc struct {
		Version   string   `json:"answer_version"`
		Query     string   `json:"query"`
		Status    string   `json:"status"`
		Sources   []any    `json:"sources"`
		Citations []string `json:"citations"`
	}
	if err := json.Unmarshal(rec.Answer, &doc); err != nil {
		t.Fatalf("the recorded answer is not the answer document: %v", err)
	}
	if doc.Version != answer.AnswerVersion {
		t.Fatalf("the recorded answer pins version %q, want %q", doc.Version, answer.AnswerVersion)
	}
	if doc.Query != "co2 uptake" {
		t.Fatalf("the recorded answer answers the question %q", doc.Query)
	}
	if len(doc.Sources) != 1 {
		t.Fatalf("the recorded answer carries %d sources", len(doc.Sources))
	}
	// The citation is spelled the way the refs are, so a reader can match
	// one to the other without a translation table.
	if len(doc.Citations) != 1 || doc.Citations[0] != rec.SelectedRefs[0] {
		t.Fatalf("citations %v do not match the selected refs %v", doc.Citations, rec.SelectedRefs)
	}
}
