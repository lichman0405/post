// Task T0905 — required test "ranking golden".
//
// The unit suite (internal/search/ranking/rank_test.go) proves the ranking's
// RULES over a fake store it builds in Go. This file pins the CONTRACT: a
// fixture is a complete case in JSON — the request, the candidates, the fact
// sheet, which versions the store refuses to answer for, and the exact bytes
// that come out — so the diff a reviewer sees on a vocabulary change is the
// change itself and not a summary of it.
//
// What the fixture vocabulary is closed against matters more here than
// anywhere else in the pipeline, and it is tested rather than promised: the
// acceptance criterion for this task is "不按 star/prestige 主排序"
// (docs/14 §3). A ranking cannot order by popularity if popularity has no way
// in, so the loader refuses an unknown field (TestFixtureVocabularyIsClosed,
// which proves it by feeding it one) and the fact sheet's groups are the six
// factors' own inputs and nothing else.

package ranking

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/search"
	"github.com/lichman0405/post/internal/search/ranking"
	"github.com/lichman0405/post/internal/search/retrieval"
)

// update rewrites the expected_ranking half of every fixture.
var update = flag.Bool("update", false, "regenerate the fixtures' expected_ranking bytes")

const (
	fixtureDir = "testdata"

	// fixtureUser is the actor the ranking is resolved for. It is in EVERY
	// fixture on purpose: the ranker refuses an unresolved scope, so a case
	// that could be replayed without an actor would be pinning a state the
	// production path never reaches.
	fixtureUser = "9f0c1b2a-3d4e-4f50-8a91-b2c3d4e5f607"
	// fixtureProject is the one project that actor is a member of. A
	// candidate whose version lives anywhere else is the out-of-scope case.
	fixtureProject = "1a2b3c4d-5e6f-4708-9a1b-2c3d4e5f6071"
)

// --------------------------------------------------------------------------
// The fixture format

// fixture is one golden case. Every nested document is a RAW message so the
// file reads as the case it is and so a fixture cannot be "fixed up" by the
// Go zero value of a field it did not write.
type fixture struct {
	Name string `json:"name"`
	// Note says what the case pins. A golden nobody can explain is a golden
	// nobody can review.
	Note string `json:"note"`
	// Request is the retrieval request the candidates came from — the
	// narrowing the query/scope factor reports.
	Request fixtureRequest `json:"request"`
	// Candidates are the retrieval's output, in ITS order. The order is the
	// input: a full tie keeps it.
	Candidates []fixtureCandidate `json:"candidates"`
	// Facts is the fact sheet the store answers with, one entry per version
	// it admits. A version a candidate resolves to and that is NOT here is
	// the out-of-scope case, and it has to be declared below.
	Facts []fixtureFacts `json:"facts"`
	// Publications maps a knowledge document's pid to the object version it
	// pins — the store's VersionsForPids answer.
	Publications map[string]string `json:"publications,omitempty"`
	// OutOfScope names the versions the store answers NOTHING for, because
	// the caller's scope does not cover them. Declaring them is the point:
	// an absence in `facts` is otherwise indistinguishable from a fixture
	// that forgot to write one, and the ranking reports the two the same
	// way (`unknown`), which is exactly why the fixture must not.
	OutOfScope []string `json:"out_of_scope,omitempty"`
	// UnresolvedPids names the knowledge documents whose publication the
	// store cannot map to a version at all. Same argument as OutOfScope: a
	// pid missing from `publications` could as easily be an omission, and
	// the two produce the same `unknown` row in the output.
	UnresolvedPids []string `json:"unresolved_pids,omitempty"`
	// ExpectedRanking is the ranking's canonical rendering.
	ExpectedRanking json.RawMessage `json:"expected_ranking"`

	// path is where the fixture was read from; used to keep -update honest.
	path string
}

type fixtureRequest struct {
	Query       string   `json:"query"`
	EntityTypes []string `json:"entity_types,omitempty"`
	PublicOnly  bool     `json:"public_only,omitempty"`
	Facets      string   `json:"facets,omitempty"`
}

type fixtureCandidate struct {
	Ref             string   `json:"ref"`
	Kind            string   `json:"kind"`
	EntityType      string   `json:"entity_type,omitempty"`
	Identity        string   `json:"identity"`
	Version         string   `json:"version,omitempty"`
	Title           string   `json:"title,omitempty"`
	ObjectType      string   `json:"object_type,omitempty"`
	ObjectID        string   `json:"object_id,omitempty"`
	ObjectVersionID string   `json:"object_version_id,omitempty"`
	VersionNo       int      `json:"version_no,omitempty"`
	Signals         []signal `json:"signals,omitempty"`
	Hops            int      `json:"hops,omitempty"`
	Score           float64  `json:"score,omitempty"`
}

// signal is one recall signal. Rank and Score are both written because they
// mean different things (retrieval.SignalHit) — and because the ranking must
// be indifferent to both: see TestGoldenOrderIgnoresRecallOrderingInputs.
type signal struct {
	Signal string  `json:"signal"`
	Rank   int     `json:"rank"`
	Score  float64 `json:"score,omitempty"`
}

// fixtureFacts is the fact sheet in the fixture's own vocabulary: counts
// grouped by the factor they answer. It is deliberately NOT
// ranking.VersionFacts with JSON tags — the port's field names are Go's, and
// a wire format invented for a fixture should live with the fixture.
//
// The groups are closed. There is no `author`, no `organization`, no
// `popularity`, no `stars` and no free-form map, and the loader refuses one
// (TestFixtureVocabularyIsClosed) — so "the ranking has no prestige input" is
// a property of the fixture format a reviewer can see, not a promise.
type fixtureFacts struct {
	ObjectVersionID string `json:"object_version_id"`
	ObjectID        string `json:"object_id"`
	VersionNo       int    `json:"version_no"`
	NewestVersionNo int    `json:"newest_version_no"`
	LifecycleState  string `json:"lifecycle_state"`
	IsNewest        bool   `json:"is_newest"`

	Evidence struct {
		Assertions    int `json:"assertions"`
		Reviewed      int `json:"reviewed"`
		Rejected      int `json:"rejected"`
		Direct        int `json:"direct"`
		Supporting    int `json:"supporting"`
		Contradicting int `json:"contradicting"`
	} `json:"evidence"`

	Reproduction struct {
		Reproduces  int `json:"reproduces"`
		Independent int `json:"independent"`
		Failed      int `json:"failed"`
	} `json:"reproduction"`

	Reviews struct {
		Scientific       int `json:"scientific"`
		Approved         int `json:"approved"`
		ChangesRequested int `json:"changes_requested"`
	} `json:"reviews"`

	Relations struct {
		Contradicting int `json:"contradicting"`
	} `json:"relations"`
}

func (f fixtureFacts) versionFacts() ranking.VersionFacts {
	return ranking.VersionFacts{
		ObjectVersionID:         f.ObjectVersionID,
		ObjectID:                f.ObjectID,
		VersionNo:               f.VersionNo,
		NewestVersionNo:         f.NewestVersionNo,
		LifecycleState:          f.LifecycleState,
		IsNewest:                f.IsNewest,
		EvidenceAssertions:      f.Evidence.Assertions,
		EvidenceReviewed:        f.Evidence.Reviewed,
		EvidenceRejected:        f.Evidence.Rejected,
		EvidenceDirect:          f.Evidence.Direct,
		EvidenceSupporting:      f.Evidence.Supporting,
		EvidenceContradicting:   f.Evidence.Contradicting,
		Reproduces:              f.Reproduction.Reproduces,
		ReproducesIndependent:   f.Reproduction.Independent,
		FailsToReproduce:        f.Reproduction.Failed,
		ReviewsScientific:       f.Reviews.Scientific,
		ReviewsApproved:         f.Reviews.Approved,
		ReviewsChangesRequested: f.Reviews.ChangesRequested,
		ContradictingRelations:  f.Relations.Contradicting,
	}
}

func (c fixtureCandidate) candidate() retrieval.Candidate {
	hits := make([]retrieval.SignalHit, 0, len(c.Signals))
	for _, s := range c.Signals {
		hits = append(hits, retrieval.SignalHit{Signal: s.Signal, Rank: s.Rank, Score: s.Score})
	}
	out := retrieval.Candidate{
		Ref:             c.Ref,
		Kind:            c.Kind,
		EntityType:      c.EntityType,
		Identity:        c.Identity,
		Version:         c.Version,
		Title:           c.Title,
		ProjectID:       fixtureProject,
		ObjectType:      c.ObjectType,
		ObjectID:        c.ObjectID,
		ObjectVersionID: c.ObjectVersionID,
		VersionNo:       c.VersionNo,
		Signals:         hits,
		Score:           c.Score,
	}
	for i := 0; i < c.Hops; i++ {
		out.Hops = append(out.Hops, retrieval.Hop{
			RelationType: "supports", Direction: retrieval.DirectionIn,
			FromRef: c.Ref, Depth: i + 1,
		})
	}
	return out
}

func (f fixture) request() retrieval.Request {
	req := retrieval.Request{Query: f.Request.Query, EntityTypes: f.Request.EntityTypes, PublicOnly: f.Request.PublicOnly}
	if f.Request.Facets != "" {
		req.Facets = []byte(f.Request.Facets)
	}
	return req
}

// --------------------------------------------------------------------------
// The fake store

// fixtureStore answers exactly what a fixture declares and never more. Its
// two answers are the two halves of the FactorStore contract, and both are
// enforced here rather than assumed:
//
//   - a version in `out_of_scope` is ABSENT from Factors, never present with
//     zeros — the distinction the ranking reports as `unknown` vs `none` and
//     the one thing a store could get wrong without any test noticing;
//   - a pid pinning an out-of-scope version is REFUSED by VersionsForPids
//     (the version exists; the caller may not read it), while a pid in no
//     `publications` entry resolves to nothing.
type fixtureStore struct {
	facts        map[string]ranking.VersionFacts
	outOfScope   map[string]bool
	publications map[string]string
}

func newFixtureStore(f fixture) *fixtureStore {
	s := &fixtureStore{
		facts:        make(map[string]ranking.VersionFacts, len(f.Facts)),
		outOfScope:   make(map[string]bool, len(f.OutOfScope)),
		publications: f.Publications,
	}
	for _, ff := range f.Facts {
		s.facts[ff.ObjectVersionID] = ff.versionFacts()
	}
	for _, id := range f.OutOfScope {
		s.outOfScope[id] = true
	}
	return s
}

func (s *fixtureStore) VersionsForPids(_ context.Context, _ search.Scope, pids []string) (map[string]string, map[string]string, error) {
	admitted := map[string]string{}
	refused := map[string]string{}
	for _, pid := range pids {
		v, ok := s.publications[pid]
		if !ok {
			continue
		}
		if s.outOfScope[v] {
			refused[pid] = v
		} else {
			admitted[pid] = v
		}
	}
	return admitted, refused, nil
}

func (s *fixtureStore) Factors(_ context.Context, _ search.Scope, versionIDs []string) ([]ranking.VersionFacts, error) {
	out := make([]ranking.VersionFacts, 0, len(versionIDs))
	for _, id := range versionIDs {
		if s.outOfScope[id] {
			continue
		}
		if v, ok := s.facts[id]; ok {
			out = append(out, v)
		}
	}
	return out, nil
}

// --------------------------------------------------------------------------
// Running a fixture

// scopeReader resolves the fixture actor's membership the only way a Scope
// can be built (search.ResolveScope is its sole constructor).
type scopeReader struct{}

func (scopeReader) ListProjectsForUser(context.Context, string) ([]domain.Project, error) {
	return []domain.Project{{ID: fixtureProject, Slug: "fixture-lab", Name: "Fixture Lab"}}, nil
}

func fixtureScope(t *testing.T) search.Scope {
	t.Helper()
	scope, err := search.ResolveScope(context.Background(), scopeReader{}, fixtureUser)
	if err != nil {
		t.Fatalf("ResolveScope: %v", err)
	}
	return scope
}

// runFixture drives one fixture through the real ranker.
func runFixture(t *testing.T, f fixture) ranking.Result {
	t.Helper()
	r, err := ranking.NewRanker(newFixtureStore(f))
	if err != nil {
		t.Fatalf("NewRanker: %v", err)
	}
	cands := make([]retrieval.Candidate, 0, len(f.Candidates))
	for _, c := range f.Candidates {
		cands = append(cands, c.candidate())
	}
	got, err := r.Rank(context.Background(), fixtureScope(t), f.request(), retrieval.Result{
		Query: f.Request.Query, Candidates: cands,
	})
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	return got
}

// --------------------------------------------------------------------------
// The golden test

// TestGoldenRankings runs every fixture through the real ranker and compares
// the exact bytes.
func TestGoldenRankings(t *testing.T) {
	fixtures := loadFixtures(t)
	if len(fixtures) == 0 {
		t.Fatal("no fixtures found: a golden test with no goldens passes without testing anything")
	}
	var levels = map[string]int{}
	var labelHits = map[string]int{}

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
				f.ExpectedRanking = canonical
				writeFixture(t, f)
				return
			}

			want := compact(t, "expected_ranking", f.ExpectedRanking)
			have := compact(t, "the ranking the ranker produced", canonical)
			if !bytes.Equal(want, have) {
				t.Errorf("the ranking does not match the golden.\n--- want ---\n%s\n--- got ---\n%s",
					indent(want), indent(have))
			}

			if got.Query != f.Request.Query {
				t.Errorf("the ranking carries query %q, want the question asked (%q)", got.Query, f.Request.Query)
			}
			if !reflect.DeepEqual(got.Factors, literalFactorNames) {
				t.Errorf("the ranking carries factor vocabulary %v, want %v", got.Factors, literalFactorNames)
			}
			// Ranks are 1..n in slice order: a result whose Rank column and
			// slice order could disagree is a result a caller can render
			// wrongly without noticing.
			for i, rc := range got.Ranked {
				if rc.Rank != i+1 {
					t.Errorf("rank %d of %d reports Rank = %d", i+1, len(got.Ranked), rc.Rank)
				}
				for _, a := range rc.Factors {
					levels[a.Factor+"/"+a.Level]++
				}
				for _, l := range rc.Labels {
					labelHits[l]++
				}
			}
		})
	}
	if *update {
		return
	}

	// The set has to pin what the ranking claims to do. A golden set that
	// only ever produced `question`/`reviewed`/`current` would pass while
	// pinning nothing about the ladder's lower half, and the two levels a
	// searcher most needs to trust — `unknown` (nothing was read) and
	// `aborted` (the version was withdrawn) — are exactly the ones a happy
	// corpus never produces.
	for _, want := range []string{
		"query_scope_match/question", "query_scope_match/conditions", "query_scope_match/related",
		"evidence/reviewed", "evidence/asserted", "evidence/rejected", "evidence/none",
		"review/approved", "review/reviewed", "review/unreviewed",
		"reproduction/independent", "reproduction/disputed", "reproduction/self", "reproduction/none",
		"conflict/uncontested", "conflict/contested",
		"version/current", "version/historical", "version/unversioned", "version/aborted",
		"version/unknown",
	} {
		if levels[want] == 0 {
			t.Errorf("no fixture produces the level %s: that rung of the ladder is untested here", want)
		}
	}
	// Every level a database-backed factor takes when nothing was read. Five
	// factors, one name — the count is the number of factors, not of
	// situations.
	if got := levels["evidence/unknown"]; got == 0 {
		t.Error("no fixture pins a version whose facts were not read: the fail-closed path is untested here")
	}
	for _, label := range []string{
		"limited_evidence", "mixed_evidence", "actively_contested", "independently_reproduced",
	} {
		if labelHits[label] == 0 {
			t.Errorf("no fixture carries the label %q", label)
		}
	}
}

// literalFactorNames is the factor vocabulary written out here, in
// docs/14 §3's order, rather than read from ranking.Factors(). A golden test
// that took the vocabulary from the code under test would agree with any
// vocabulary at all; this list is the spec's, and a change to it is a change
// to the spec that has to show up in a review.
var literalFactorNames = []string{
	"query_scope_match", "evidence", "review", "reproduction", "conflict", "version",
}

// TestGoldenOrderIgnoresRecallOrderingInputs is the behavioural half of "the
// ranking is not the recall": the fusion SCORE and the candidate TITLE are
// rewritten under every fixture, and the order and the six assessments must
// not move.
//
// The rewrite is asserted to have taken effect (the titles in the output
// differ), so the test cannot pass by doing nothing — which is the failure
// mode of every "this input does not matter" test written carelessly. What
// this pins is the boundary between T0904 and T0905: relevance decides which
// candidates exist, the six factors decide their order, and the second one
// does not quietly read the first one's numbers.
func TestGoldenOrderIgnoresRecallOrderingInputs(t *testing.T) {
	fixtures := loadFixtures(t)
	for _, f := range fixtures {
		t.Run(f.Name, func(t *testing.T) {
			base := runFixture(t, f)

			// Rewrite the recall-facing metadata: titles, and every
			// signal's rank and score. Fusion Score is recomputed from the
			// ranks the same way retrieval.fuse orders by them, so the
			// rewrite is a plausible alternative recall.
			shadowed := f
			shadowed.Candidates = make([]fixtureCandidate, 0, len(f.Candidates))
			for i, c := range f.Candidates {
				c.Title = fmt.Sprintf("shadow #%d: %s", i, c.Title)
				c.Score = 100 - float64(i)
				sigs := make([]signal, 0, len(c.Signals))
				for j, s := range c.Signals {
					sigs = append(sigs, signal{Signal: s.Signal, Rank: len(f.Candidates) - i + j, Score: 1.0 / float64(i+j+1)})
				}
				c.Signals = sigs
				shadowed.Candidates = append(shadowed.Candidates, c)
			}
			// The candidate ORDER inside the retrieval result is not
			// rewritten: it is the last tie-break, and pretending
			// otherwise would be testing a rule this file does not claim.
			got := runFixture(t, shadowed)

			if len(got.Ranked) != len(base.Ranked) {
				t.Fatalf("%d ranked after the rewrite, want %d", len(got.Ranked), len(base.Ranked))
			}
			titlesMoved := false
			for i := range got.Ranked {
				if got.Ranked[i].Ref != base.Ranked[i].Ref {
					t.Fatalf("rank %d is %s after the recall metadata was rewritten, was %s: a fusion score or a title decided the order",
						i+1, got.Ranked[i].Ref, base.Ranked[i].Ref)
				}
				if !reflect.DeepEqual(got.Ranked[i].Factors, base.Ranked[i].Factors) {
					t.Errorf("%s's assessments changed when the recall metadata did:\n%+v\nvs\n%+v",
						got.Ranked[i].Ref, got.Ranked[i].Factors, base.Ranked[i].Factors)
				}
				if !reflect.DeepEqual(got.Ranked[i].Labels, base.Ranked[i].Labels) {
					t.Errorf("%s's labels changed when the recall metadata did: %v vs %v",
						got.Ranked[i].Ref, got.Ranked[i].Labels, base.Ranked[i].Labels)
				}
				if got.Ranked[i].Title != base.Ranked[i].Title {
					titlesMoved = true
				}
			}
			if !titlesMoved {
				t.Fatal("the rewrite did not reach the output: this test proved nothing")
			}
		})
	}
}

// TestGoldenRankingsAreDeterministic ranks each fixture 25 times and requires
// byte-identical output on all of them.
//
// Two probes, and which one applies is COMPUTED from the baseline rather than
// declared by the fixture:
//
//   - a fixture whose candidates all differ on at least one factor is ranked
//     again over a ROTATED candidate list — the factors separate everything,
//     so the retrieval's order cannot legitimately reach the output;
//   - a fixture that contains a full tie (two candidates identical on all six
//     factors) is ranked again with the list AS IS. Rotating it would be a
//     false alarm: the tie-break IS the retrieval's order, and moving that
//     order is supposed to move the answer.
//
// Both probes are still worth running, and for the same reason: Go randomises
// map iteration per run, so a ranking that read a map without sorting it —
// the version ids, the fact sheet — would drift across repeats regardless of
// the candidate order. At least one fixture must be fully ordered, or the
// stronger probe never ran.
func TestGoldenRankingsAreDeterministic(t *testing.T) {
	rotated := 0
	for _, f := range loadFixtures(t) {
		t.Run(f.Name, func(t *testing.T) {
			base := runFixture(t, f)
			want, err := base.CanonicalJSON()
			if err != nil {
				t.Fatalf("CanonicalJSON: %v", err)
			}
			separating := !hasFullTie(base)
			if separating {
				rotated++
			}
			for run := 1; run <= 25; run++ {
				probe := f
				if separating {
					probe.Candidates = rotate(f.Candidates, run)
				}
				got, err := runFixture(t, probe).CanonicalJSON()
				if err != nil {
					t.Fatalf("CanonicalJSON (run %d): %v", run, err)
				}
				if !bytes.Equal(compact(t, "golden", want), compact(t, "run", got)) {
					what := "depends on something other than its inputs"
					if separating {
						what = "depends on the order the retrieval returned candidates in, " +
							"although the factors separate every pair"
					}
					t.Fatalf("the ranking %s (run %d):\n%s\n---\n%s", what, run, want, got)
				}
			}
		})
	}
	if rotated == 0 {
		t.Fatal("no fixture has a fully separating factor vector: the rotation probe never ran")
	}
}

// hasFullTie reports whether any two ranked candidates carry the same level on
// all six factors — the one case where the retrieval's own order is the
// tie-break and a rotation legitimately changes the answer.
func hasFullTie(res ranking.Result) bool {
	for i := 0; i < len(res.Ranked); i++ {
		for j := i + 1; j < len(res.Ranked); j++ {
			if reflect.DeepEqual(levelsOf(res.Ranked[i]), levelsOf(res.Ranked[j])) {
				return true
			}
		}
	}
	return false
}

func levelsOf(rc ranking.Ranked) []int {
	out := make([]int, 0, len(rc.Factors))
	for _, a := range rc.Factors {
		out = append(out, a.LevelRank)
	}
	return out
}

func rotate(cands []fixtureCandidate, by int) []fixtureCandidate {
	if len(cands) == 0 {
		return cands
	}
	n := by % len(cands)
	out := make([]fixtureCandidate, 0, len(cands))
	out = append(out, cands[n:]...)
	return append(out, cands[:n]...)
}

// --------------------------------------------------------------------------
// The fixture set kept honest

// TestFixtureVocabularyIsClosed proves the fixture format has no room for a
// popularity or prestige input: the loader refuses an unknown field, and this
// test shows that by feeding it a candidate that carries one.
//
// It is written as a falsification rather than as an assertion about the
// struct because "there is no stars field" is true of almost any struct, and
// the property that actually matters is that a fixture CANNOT smuggle one in.
// If the loader were ever relaxed to ignore unknown fields, this test fails —
// with the ranking's acceptance criterion, not a style rule, as the reason.
func TestFixtureVocabularyIsClosed(t *testing.T) {
	// A candidate as a fixture would write it, plus one field each of the
	// ways "who wrote this" could enter the ranking.
	for _, injected := range []string{
		`"stars": 4821`,
		`"author_followers": 91`,
		`"organization_prestige": "top-10"`,
		`"citation_count": 300`,
		`"contributors": 12`,
		`"popularity": 0.9`,
		`"trust": "high"`,
	} {
		t.Run(injected, func(t *testing.T) {
			doc := `{"name":"x","note":"n","request":{"query":"q"},"candidates":[` +
				`{"ref":"knowledge:pid","kind":"document","identity":"pid",` + injected + `}]}`
			var f fixture
			dec := json.NewDecoder(strings.NewReader(doc))
			dec.DisallowUnknownFields()
			err := dec.Decode(&f)
			if err == nil {
				t.Fatalf("the fixture loader accepted %s: a ranking that can be told who wrote a candidate can be ordered by it, "+
					"and docs/14 §3 forbids ordering by popularity or organization prestige", injected)
			}
			if !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("the loader refused %s for the wrong reason: %v", injected, err)
			}
		})
	}

	// The fact sheet is the same argument one level down: the ranking reads
	// counts of ROWS other people wrote, and a fact sheet that could carry a
	// standing would be a standing the ranking read.
	doc := `{"name":"x","note":"n","request":{"query":"q"},"candidates":[],"facts":[` +
		`{"object_version_id":"v","stars":9}]}`
	var f fixture
	dec := json.NewDecoder(strings.NewReader(doc))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("the fact sheet accepted an unknown field (%v): the six factors' inputs are a closed set", err)
	}
}

// TestGoldenFixturesAreWellFormed keeps the golden set honest independently of
// the ranker: a fixture has to say what it is, ask a question, name its
// candidates unambiguously, and account for every version it does not supply
// facts for.
func TestGoldenFixturesAreWellFormed(t *testing.T) {
	for _, f := range loadFixtures(t) {
		t.Run(f.Name, func(t *testing.T) {
			if f.Name == "" || f.Note == "" || f.Request.Query == "" {
				t.Fatalf("fixture is incomplete: %+v", f)
			}
			// -update writes to <name>.json, so a fixture whose name and file
			// disagree would silently duplicate itself on the next update.
			if base := strings.TrimSuffix(filepath.Base(f.path), ".json"); base != f.Name {
				t.Errorf("fixture at %s calls itself %q: -update writes <name>.json", f.path, f.Name)
			}
			if len(f.Candidates) == 0 {
				t.Fatal("the fixture ranks no candidates: an empty case pins nothing")
			}
			if len(f.ExpectedRanking) == 0 {
				t.Fatal("the fixture has no expected_ranking: run go test ./tests/ranking -run TestGolden -update")
			}

			facts := map[string]bool{}
			for _, ff := range f.Facts {
				if ff.ObjectVersionID == "" {
					t.Error("a fact sheet names no version")
				}
				if facts[ff.ObjectVersionID] {
					t.Errorf("version %s has two fact sheets", ff.ObjectVersionID)
				}
				facts[ff.ObjectVersionID] = true
			}
			declared := map[string]bool{}
			for _, id := range f.OutOfScope {
				if declared[id] {
					t.Errorf("version %s is declared out of scope twice", id)
				}
				if facts[id] {
					t.Errorf("version %s is both out of scope and supplied facts: the two states are the whole point", id)
				}
				declared[id] = true
			}
			for pid, version := range f.Publications {
				if facts[version] || declared[version] {
					continue
				}
				t.Errorf("publication %s pins version %s, which is neither in facts nor in out_of_scope", pid, version)
			}

			refs := map[string]bool{}
			unresolved := map[string]bool{}
			for _, pid := range f.UnresolvedPids {
				if _, ok := f.Publications[pid]; ok {
					t.Errorf("pid %s is both published and declared unresolved", pid)
				}
				unresolved[pid] = true
			}
			// Resolved the way the ranker resolves: the candidate's own pin
			// first, then the pid.
			resolved := 0
			for _, c := range f.Candidates {
				if c.Ref == "" || c.Identity == "" || c.Kind == "" {
					t.Errorf("candidate %+v does not identify itself", c)
				}
				if refs[c.Ref] {
					t.Errorf("two candidates share the ref %s: a result keyed by ref could not tell them apart", c.Ref)
				}
				refs[c.Ref] = true
				version := c.ObjectVersionID
				if version == "" && c.EntityType == "knowledge" {
					version = f.Publications[c.Identity]
				}
				if version == "" {
					if c.EntityType == "knowledge" && !unresolved[c.Identity] {
						t.Errorf("candidate %s is a knowledge document whose pid %s is neither published nor declared unresolved — "+
							"a fixture that forgot the publication is indistinguishable from one testing a withdrawn one",
							c.Ref, c.Identity)
					}
					continue
				}
				resolved++
				if !facts[version] && !declared[version] {
					t.Errorf("candidate %s resolves to version %s, which is neither in facts nor in out_of_scope — "+
						"a fixture that forgot the facts is indistinguishable from one testing the fail-closed path",
						c.Ref, version)
				}
			}
			if resolved == 0 && len(f.OutOfScope) == 0 {
				t.Error("no candidate resolves to a version: the case pins no fact-backed level")
			}
			for id := range declared {
				found := false
				for _, c := range f.Candidates {
					if c.ObjectVersionID == id {
						found = true
					}
				}
				for _, v := range f.Publications {
					if v == id {
						found = true
					}
				}
				if !found {
					t.Errorf("version %s is declared out of scope but no candidate resolves to it", id)
				}
			}
		})
	}
}

// TestGoldenFixturesPinTheRendering is the half of "canonical rendering" a
// whitespace-insensitive comparison cannot see.
//
// Go's default HTML escaping rewrites '<' into the six characters \u003c.
// The ranking still round-trips and is still correct, so nothing else here
// would notice — but a result is stored and shown, and a question about
// "CO2 <- CH4" is not a question about escape sequences. The check is written
// so -update cannot launder it: regenerating from an escaping implementation
// writes the escape, and the escape is what this test looks for.
func TestGoldenFixturesPinTheRendering(t *testing.T) {
	fixtures := loadFixtures(t)
	questions := 0
	for _, f := range fixtures {
		if strings.ContainsAny(f.Request.Query, "<>") {
			questions++
		}
		for _, r := range []rune{'<', '>', '&'} {
			escaped := fmt.Sprintf(`\u%04x`, r)
			if bytes.Contains(f.ExpectedRanking, []byte(escaped)) {
				t.Errorf("fixture %s renders %s instead of %q: the ranking is stored and shown, so it must not be HTML-escaped",
					f.Name, escaped, r)
			}
		}
	}
	if questions == 0 {
		t.Fatal("no fixture asks a question containing '<' or '>': the escaping check above is vacuous")
	}
}

// TestGoldenFixturesCarryNoPrestigeVocabulary reads the fixtures as BYTES.
//
// TestFixtureVocabularyIsClosed proves the loader refuses a prestige field;
// this proves no fixture in the set carries one anyway, including in the
// free-form places the loader does not type (a note, a title). The two
// together are the acceptance criterion's paper trail: the ranking has no
// popularity input, and nothing in the corpus that pins it mentions one.
func TestGoldenFixturesCarryNoPrestigeVocabulary(t *testing.T) {
	// Whole-word matches, on the longest first: `reviewed` contains `view`,
	// and the words that carry a factor's meaning must not be read as
	// accidents of spelling.
	banned := []string{
		"popularity", "prestige", "reputation", "followers", "stars", "stargazers",
		"watchers", "citation_count", "h_index", "impact_factor", "contributors",
		"member_count", "trust_score", "rank_score", "quality_score", "upvotes",
	}
	checked := 0
	for _, f := range loadFixtures(t) {
		raw, err := os.ReadFile(f.path)
		if err != nil {
			t.Fatalf("read %s: %v", f.path, err)
		}
		// The tokens the JSON actually contains as KEYS or as values'
		// words: split on everything that is not a letter, a digit or an
		// underscore.
		for _, token := range strings.FieldsFunc(strings.ToLower(string(raw)), func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_')
		}) {
			for _, word := range banned {
				if token == word {
					t.Errorf("fixture %s mentions %q: the ranking has no popularity input, and neither does the corpus that pins it",
						f.Name, word)
				}
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no fixtures were read: this test proved nothing")
	}
}

// --------------------------------------------------------------------------
// Plumbing

func loadFixtures(t *testing.T) []fixture {
	t.Helper()
	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatalf("read %s: %v", fixtureDir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	out := make([]fixture, 0, len(names))
	for _, name := range names {
		path := filepath.Join(fixtureDir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var f fixture
		dec := json.NewDecoder(bytes.NewReader(raw))
		// The closed vocabulary (TestFixtureVocabularyIsClosed) is enforced
		// here, on the real corpus, and not only in that test.
		dec.DisallowUnknownFields()
		if err := dec.Decode(&f); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		f.path = path
		out = append(out, f)
	}
	return out
}

// writeFixture rewrites one fixture in place, preserving every field it did
// not compute.
//
// It uses an Encoder rather than json.MarshalIndent so that HTML escaping is
// OFF. json.Marshal re-escapes an embedded json.RawMessage ('<' becomes the
// six characters \u003c), which would mean -update wrote a query about "<1
// bar" as an escape sequence — and TestGoldenFixturesPinTheRendering, which
// exists to catch exactly that in the ranker's output, cannot tell a fixture
// the RANKER escaped from one the WRITER did. A regeneration tool that
// rewrites the thing under test is how a golden test stops being one.
func writeFixture(t *testing.T, f fixture) {
	t.Helper()
	file, err := os.Create(f.path)
	if err != nil {
		t.Fatalf("create %s: %v", f.path, err)
	}
	defer file.Close()
	enc := json.NewEncoder(file)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(f); err != nil {
		t.Fatalf("render %s: %v", f.Name, err)
	}
}

// compact removes insignificant whitespace without touching key order or
// escaping: the comparison has to be insensitive to how the fixture is
// indented and sensitive to how the ranking is written.
func compact(t *testing.T, what string, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("%s is not valid JSON: %v", what, err)
	}
	return buf.Bytes()
}

func indent(raw []byte) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}
