// Task T0906 — required test "answer grounding tests".
//
// Every fixture under testdata/ is one whole answer case: what the ranking
// returned, what the retrieval reported about its own signals, what the
// provider replied, and the exact bytes the answer layer produces. The test
// drives the REAL generator over a deterministic fake provider
// (internal/search/answer/answertest), so what is pinned includes the schema
// check, the grounding guard, the derived limitations and conflicts, the
// source addresses and the rendering — not just the struct.
//
// The two assertions that make this the acceptance test rather than a
// snapshot test:
//
//  1. a fixture that declares `forbidden` tokens must not have them anywhere
//     in the produced answer — the hallucinated identity has to disappear
//     completely, not merely fail to be cited; and
//  2. the fixture set as a whole must exercise every refusal reason, because a
//     golden suite that only ever produced answered answers would pass while
//     pinning nothing about the guard.

package answer

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/search/answer"
	"github.com/lichman0405/post/internal/search/answer/answertest"
	"github.com/lichman0405/post/internal/search/ranking"
	"github.com/lichman0405/post/internal/search/retrieval"
)

// update rewrites the expected_answer half of every fixture.
var update = flag.Bool("update", false, "regenerate the fixtures' expected_answer bytes")

const fixtureDir = "testdata"

// --------------------------------------------------------------------------
// The fixture format

// fixture is one golden case. Every nested document is decoded strictly
// (TestFixtureVocabularyIsClosed), so a field nobody taught this loader is an
// error rather than a value that silently never reaches the answer.
type fixture struct {
	Name string `json:"name"`
	// Note says what the case pins. A golden nobody can explain is a golden
	// nobody can review.
	Note string `json:"note"`
	// Query is the question the ranking was run for.
	Query string `json:"query"`
	// Signals is the retrieval's own report, including the signals that did
	// not run — which is what the answer's coverage limitations are made of.
	Signals []signal `json:"signals,omitempty"`
	// Sources are the ranked candidates, in rank order, in the fixture's own
	// vocabulary: exactly what the answer may cite.
	Sources []source `json:"sources,omitempty"`
	// Provider is what the answer model does with the request.
	Provider provider `json:"provider"`
	// ProviderTimeoutMS bounds the provider call; 0 means the generator's
	// default. A fixture that pins the timeout path sets it small.
	ProviderTimeoutMS int `json:"provider_timeout_ms,omitempty"`
	// Forbidden are identities the provider wrote that retrieval never
	// returned. They must appear NOWHERE in the answer the platform would
	// publish — not in the summary, not in the citations, not in a source and
	// not in a derived statement.
	Forbidden []string `json:"forbidden,omitempty"`
	// ExpectedAnswer is the answer's canonical rendering.
	ExpectedAnswer json.RawMessage `json:"expected_answer"`

	path string
}

// signal is one retrieval signal, in both places the pipeline reports them:
// the RESULT's report (Ran/Skipped/Hits — what the answer's coverage
// limitations are made of) and a candidate's own recall hits (Rank/Score —
// what the query/scope factor is read from). One type for both, because they
// are the same signal seen twice, and a fixture that wrote them differently
// would be describing a state the retrieval cannot produce.
type signal struct {
	Signal  string  `json:"signal"`
	Ran     bool    `json:"ran,omitempty"`
	Skipped string  `json:"skipped,omitempty"`
	Hits    int     `json:"hits,omitempty"`
	Rank    int     `json:"rank,omitempty"`
	Score   float64 `json:"score,omitempty"`
}

// provider is the scripted answer model.
type provider struct {
	// Absent means no provider is configured at all — the supported state of
	// a deployment that has not answered whether content may go to a
	// third-party service.
	Absent bool `json:"absent,omitempty"`
	// Document is the reply, verbatim and unvalidated.
	Document string `json:"document,omitempty"`
	// Error is returned instead of a document.
	Error string `json:"error,omitempty"`
	// DelayMS postpones the reply, bounded by the request's context.
	DelayMS int `json:"delay_ms,omitempty"`
}

// source is one ranked candidate, in the answer's own citation vocabulary.
//
// It is deliberately NOT ranking.Ranked with JSON tags: the fixture format
// should be readable as the case it is, and the fields below are the ones the
// answer layer actually reads. A candidate field this format has no slot for
// is a field the answer layer does not use — which is worth being able to see.
type source struct {
	Rank            int          `json:"rank"`
	Ref             string       `json:"ref"`
	Kind            string       `json:"kind"`
	EntityType      string       `json:"entity_type,omitempty"`
	ObjectType      string       `json:"object_type,omitempty"`
	Identity        string       `json:"identity"`
	Version         string       `json:"version,omitempty"`
	Title           string       `json:"title,omitempty"`
	ProjectID       string       `json:"project_id,omitempty"`
	ObjectID        string       `json:"object_id,omitempty"`
	ObjectVersionID string       `json:"object_version_id,omitempty"`
	Labels          []string     `json:"labels,omitempty"`
	Factors         []factor     `json:"factors"`
	Signals         []signal     `json:"signals,omitempty"`
	Hops            []fixtureHop `json:"hops,omitempty"`
}

// fixtureHop is one traversal step. It is written out because the query/scope
// factor's reason names it, and a fixture whose reason talked about a path the
// fixture did not describe would be pinning a sentence nobody can check.
type fixtureHop struct {
	RelationType string `json:"relation_type"`
	Direction    string `json:"direction"`
	FromRef      string `json:"from_ref"`
	Depth        int    `json:"depth"`
}

// factor is one ranking assessment, verbatim as the ranking renders it.
type factor struct {
	Factor    string         `json:"factor"`
	Level     string         `json:"level"`
	LevelRank int            `json:"level_rank"`
	Reason    string         `json:"reason"`
	Facts     map[string]int `json:"facts,omitempty"`
}

func (s source) ranked() ranking.Ranked {
	factors := make([]ranking.Assessment, 0, len(s.Factors))
	for _, f := range s.Factors {
		factors = append(factors, ranking.Assessment{
			Factor: f.Factor, Level: f.Level, LevelRank: f.LevelRank, Reason: f.Reason, Facts: f.Facts,
		})
	}
	hits := make([]retrieval.SignalHit, 0, len(s.Signals))
	for _, sig := range s.Signals {
		hits = append(hits, retrieval.SignalHit{Signal: sig.Signal, Rank: sig.Rank, Score: sig.Score})
	}
	hops := make([]retrieval.Hop, 0, len(s.Hops))
	for _, h := range s.Hops {
		hops = append(hops, retrieval.Hop{RelationType: h.RelationType, Direction: h.Direction, FromRef: h.FromRef, Depth: h.Depth})
	}
	return ranking.Ranked{
		Rank: s.Rank, Ref: s.Ref, Kind: s.Kind, EntityType: s.EntityType, ObjectType: s.ObjectType,
		Title: s.Title, Version: s.Version, ObjectVersionID: s.ObjectVersionID,
		Labels: s.Labels, Factors: factors,
		Candidate: retrieval.Candidate{
			Ref: s.Ref, Kind: s.Kind, EntityType: s.EntityType, Identity: s.Identity,
			Version: s.Version, Title: s.Title, Visibility: "public", ProjectID: s.ProjectID,
			ObjectType: s.ObjectType, ObjectID: s.ObjectID, ObjectVersionID: s.ObjectVersionID,
			Hops: hops, Signals: hits,
		},
	}
}

// --------------------------------------------------------------------------
// Running a fixture

// generatorFor builds the generator a fixture describes, and the provider it
// should have been given.
func generatorFor(t *testing.T, f fixture) (*answer.Generator, *answertest.Provider) {
	t.Helper()
	deps := answer.Deps{Timeout: time.Duration(f.ProviderTimeoutMS) * time.Millisecond}
	var provider *answertest.Provider
	switch {
	case f.Provider.Absent:
		// No provider at all: deps.Provider stays nil.
	case f.Provider.Error != "":
		provider = answertest.Failing(fmt.Errorf("%s", f.Provider.Error))
		deps.Provider = provider
	case f.Provider.DelayMS > 0:
		provider = answertest.Slow(time.Duration(f.Provider.DelayMS)*time.Millisecond, f.Provider.Document)
		deps.Provider = provider
	default:
		provider = answertest.Reply(f.Provider.Document)
		deps.Provider = provider
	}
	g, err := answer.New(deps)
	if err != nil {
		t.Fatalf("answer.New: %v", err)
	}
	return g, provider
}

// inputFor is the ranking and signal report a fixture describes.
func inputFor(f fixture) answer.Input {
	ranked := make([]ranking.Ranked, 0, len(f.Sources))
	for _, s := range f.Sources {
		ranked = append(ranked, s.ranked())
	}
	signals := make([]retrieval.SignalReport, 0, len(f.Signals))
	for _, s := range f.Signals {
		signals = append(signals, retrieval.SignalReport{Signal: s.Signal, Ran: s.Ran, Skipped: s.Skipped, Hits: s.Hits})
	}
	return answer.Input{
		Result:  ranking.Result{Query: f.Query, Factors: ranking.Factors(), Ranked: ranked},
		Signals: signals,
	}
}

// runFixture drives one fixture through the real generator.
func runFixture(t *testing.T, f fixture) answer.Answer {
	t.Helper()
	g, _ := generatorFor(t, f)
	got, err := g.Answer(context.Background(), inputFor(f))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	return got
}

// --------------------------------------------------------------------------
// The golden test

// TestGoldenAnswerGrounding runs every fixture through the real generator and
// compares the exact bytes.
func TestGoldenAnswerGrounding(t *testing.T) {
	fixtures := loadFixtures(t)
	if len(fixtures) == 0 {
		t.Fatal("no fixtures found: a golden test with no goldens passes without testing anything")
	}

	statuses := map[answer.Status]int{}
	reasons := map[answer.Reason]int{}
	kinds := map[string]int{}

	for _, f := range fixtures {
		t.Run(f.Name, func(t *testing.T) {
			got := runFixture(t, f)
			canonical, err := got.CanonicalJSON()
			if err != nil {
				t.Fatalf("CanonicalJSON: %v", err)
			}
			if !bytes.HasSuffix(canonical, []byte("\n")) {
				t.Errorf("the canonical rendering does not end in a newline: %q", canonical[len(canonical)-1:])
			}

			if *update {
				f.ExpectedAnswer = canonical
				writeFixture(t, f)
			} else {
				want := compact(t, "expected_answer", f.ExpectedAnswer)
				have := compact(t, "the answer the generator produced", canonical)
				if !bytes.Equal(want, have) {
					t.Errorf("the answer does not match the golden.\n--- want ---\n%s\n--- got ---\n%s",
						want, have)
				}
			}

			// The criterion: an identity the provider invented appears
			// NOWHERE in what the platform would publish.
			for _, forbidden := range f.Forbidden {
				if bytes.Contains(canonical, []byte(forbidden)) {
					t.Errorf("the invented identity %q reached the answer document:\n%s", forbidden, canonical)
				}
			}

			// The citations are sources of this answer, and every source the
			// answer marks as cited is one of them.
			byRef := map[string]bool{}
			for _, s := range got.Sources {
				byRef[s.Ref] = true
			}
			for _, ref := range got.Citations {
				if !byRef[ref] {
					t.Errorf("the answer cites %q, which is not one of its sources", ref)
				}
			}
			for _, s := range got.Sources {
				if s.Cited && !contains(got.Citations, s.Ref) {
					t.Errorf("source %q is marked cited but is not in the citation list", s.Ref)
				}
			}

			// The sections are always there (docs/14 §4), and the conflict
			// section is empty in exactly one situation: no sources at all.
			if len(got.Limitations) == 0 {
				t.Error("limitations are empty")
			}
			if len(got.Sources) > 0 && len(got.Conflicts) == 0 {
				t.Error("conflicts are empty over a result that has sources")
			}
		})
	}
	if *update {
		return
	}

	// Coverage: the fixture set has to exercise the vocabulary, or the
	// golden bytes above pin a fraction of the behaviour. This is the same
	// argument tests/ranking makes for its ladders.
	for _, f := range fixtures {
		got := runFixture(t, f)
		statuses[got.Status]++
		if got.Reason != "" {
			reasons[got.Reason]++
		}
		for _, s := range got.Sources {
			if s.Href != "" {
				kinds[s.EntityType]++
			} else {
				kinds["unaddressable:"+s.EntityType]++
			}
		}
	}
	for _, want := range []answer.Reason{
		answer.ReasonNoProvider, answer.ReasonNoSources, answer.ReasonProviderError,
		answer.ReasonTimeout, answer.ReasonInvalidAnswer, answer.ReasonUngroundedCitation,
	} {
		if reasons[want] == 0 {
			t.Errorf("no fixture produces the fallback reason %q: that path is untested here", want)
		}
	}
	if statuses[answer.StatusAnswered] == 0 {
		t.Error("no fixture produces an answered answer: an implementation that always fell back would pass")
	}
	for _, want := range []string{"asset", "knowledge", "release", "unaddressable:state", "unaddressable:"} {
		if kinds[want] == 0 {
			t.Errorf("no fixture covers the source address %q", want)
		}
	}
}

// TestFixtureVocabularyIsClosed proves the loader refuses a field nobody
// taught it. Without it, a fixture could carry "expected_citations": [...] and
// the test would pass while ignoring it.
func TestFixtureVocabularyIsClosed(t *testing.T) {
	dec := json.NewDecoder(strings.NewReader(`{
	  "name": "x", "note": "x", "query": "x", "provider": {"absent": true},
	  "expected_answer": {}, "expected_status": "answered"
	}`))
	dec.DisallowUnknownFields()
	var f fixture
	if err := dec.Decode(&f); err == nil {
		t.Fatal("the fixture loader accepted an unknown field")
	}
}

// TestForbiddenTokensAreChecked proves the guard's own instrument works: a
// fixture whose forbidden token IS in the answer must fail. It runs the same
// assertion the golden test runs, over a case built to trip it.
func TestForbiddenTokensAreChecked(t *testing.T) {
	f := fixture{
		Name: "instrument", Query: "q",
		Provider: provider{Document: `{"answer_version":"1","summary":"x","citations":["asset:AST-0001@2"]}`},
		Sources: []source{{
			Rank: 1, Ref: "asset:AST-0001@2", Kind: retrieval.KindDocument, EntityType: "asset",
			Identity: "AST-0001", Version: "2", ProjectID: "3f2a5b7c-1d4e-4a6b-8c9d-0e1f2a3b4c5d",
			Factors: []factor{{Factor: ranking.FactorVersion, Level: ranking.LevelUnversioned, LevelRank: 2, Reason: "r"}},
		}},
		// "AST-0001" IS in the answer: the assertion must say so.
		Forbidden: []string{"AST-0001"},
	}
	got := runFixture(t, f)
	canonical, err := got.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if !bytes.Contains(canonical, []byte("AST-0001")) {
		t.Fatal("the instrument is broken: this case should contain the forbidden token")
	}
}

// TestCitationOnlyHallucinationIsRefusedByTheCitationCheck isolates the half of
// the guard that a fixture with a polluted summary cannot isolate, and states
// its own premise as an experiment rather than as an inspection.
//
// hallucinated-citation.json commits the same error TWICE — the invented ref is
// in citations[] and written into the summary — so the summary's token refuses
// it even when only the citation check is disabled, and pinning it says nothing
// about the citation check on its own. The fixture used here has a summary that
// names no entity at all, so nothing but citations[] can refuse it. The control
// is what makes "clean" a measurement rather than a claim: the SAME prose, with
// the invented ref swapped for one retrieval returned, is an ANSWERED answer —
// false the moment this case's prose names anything the search did not return.
func TestCitationOnlyHallucinationIsRefusedByTheCitationCheck(t *testing.T) {
	f := fixtureNamed(t, "hallucinated-citation-clean-summary")
	if len(f.Sources) == 0 {
		t.Fatal("the fixture has no source to cite")
	}

	got := runFixture(t, f)
	if got.Status != answer.StatusFallback || got.Reason != answer.ReasonUngroundedCitation {
		t.Fatalf("status/reason = %s, want %s (%s)",
			got, answer.StatusFallback, answer.ReasonUngroundedCitation)
	}
	// The refusal is total, not a pruning of the offending citation: nothing
	// of the refused document survives into the answer the platform publishes.
	if got.Summary != "" {
		t.Errorf("the refused summary was kept: %q", got.Summary)
	}
	if len(got.Citations) != 0 {
		t.Errorf("the refused citations were kept: %v", got.Citations)
	}
	for _, s := range got.Sources {
		if s.Cited {
			t.Errorf("source %q is marked cited in an answer whose document was refused", s.Ref)
		}
	}

	// The premise, measured: the same document over a citation retrieval DID
	// return is accepted — same prose, one ref different.
	grounded := f
	grounded.Provider.Document = withCitation(t, f.Provider.Document, f.Sources[0].Ref)
	ctrl := runFixture(t, grounded)
	if ctrl.Status != answer.StatusAnswered {
		t.Fatalf("the same summary over a retrieved citation is %s, want an answered answer: this case is only about the citation if its prose is clean",
			ctrl)
	}
	if ctrl.Summary == "" || len(ctrl.Citations) != 1 || ctrl.Citations[0] != f.Sources[0].Ref {
		t.Fatalf("the control answered with summary %q and citations %v, want the fixture's own prose over %q",
			ctrl.Summary, ctrl.Citations, f.Sources[0].Ref)
	}
}

// --------------------------------------------------------------------------
// Loading and writing

func loadFixtures(t *testing.T) []fixture {
	t.Helper()
	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatalf("read %s: %v", fixtureDir, err)
	}
	out := make([]fixture, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(fixtureDir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		var f fixture
		if err := dec.Decode(&f); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		f.path = path
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// fixtureNamed returns the fixture with that name. A golden case that quietly
// stopped being loaded — a rename, a file the loader skips — would otherwise
// turn the test that names it into a no-op that passes.
func fixtureNamed(t *testing.T, name string) fixture {
	t.Helper()
	for _, f := range loadFixtures(t) {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no fixture is named %q", name)
	return fixture{}
}

// withCitation rewrites a provider document's citation list, keeping everything
// else — the summary above all — byte-for-byte. It refuses a document that does
// not cite exactly one ref: the case it serves is the one where a single
// citation is the only thing that could have been refused, and this helper must
// not quietly accept a document where that is not true.
func withCitation(t *testing.T, document string, refs ...string) string {
	t.Helper()
	var doc struct {
		AnswerVersion string   `json:"answer_version"`
		Summary       string   `json:"summary"`
		Citations     []string `json:"citations"`
	}
	if err := json.Unmarshal([]byte(document), &doc); err != nil {
		t.Fatalf("the provider document is not JSON: %v", err)
	}
	if len(doc.Citations) != 1 {
		t.Fatalf("the provider document cites %d refs; this helper is for the case that cites exactly one", len(doc.Citations))
	}
	if doc.Summary == "" {
		t.Fatal("the provider document has no summary: there would be nothing held fixed across the two runs")
	}
	doc.Citations = refs
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal the provider document: %v", err)
	}
	return string(out)
}

// writeFixture rewrites a fixture with the bytes just produced. It keeps the
// file's field order by re-encoding the struct (encoding/json writes struct
// fields in declaration order), so the diff a reviewer reads is the answer,
// not the formatting.
func writeFixture(t *testing.T, f fixture) {
	t.Helper()
	if f.path == "" {
		t.Fatalf("fixture %q has no path", f.Name)
	}
	if len(f.ExpectedAnswer) == 0 {
		t.Fatalf("fixture %q produced no bytes to write", f.Name)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(f); err != nil {
		t.Fatalf("encode %s: %v", f.path, err)
	}
	if err := os.WriteFile(f.path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write %s: %v", f.path, err)
	}
}

// compact renders a JSON document on one line for the diff message.
func compact(t *testing.T, what string, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("%s is not valid JSON: %v", what, err)
	}
	return buf.Bytes()
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// The fixtures must agree with the generator about the factor vocabulary: a
// fixture that invented a factor name would be pinning an answer no ranking
// could produce.
func TestFixtureFactorsAreTheRankingVocabulary(t *testing.T) {
	known := map[string]bool{}
	for _, f := range ranking.Factors() {
		known[f] = true
	}
	for _, fx := range loadFixtures(t) {
		for _, s := range fx.Sources {
			for _, f := range s.Factors {
				if !known[f.Factor] {
					t.Errorf("%s: source %s carries the factor %q, which the ranking does not declare", fx.Name, s.Ref, f.Factor)
				}
			}
		}
	}
}
