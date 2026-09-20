package ranking

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/search"
	"github.com/lichman0405/post/internal/search/retrieval"
)

// The ranking's unit suite. It drives a fake FactorStore, so everything it
// proves is about the ranking's own rules — which level a fact sheet maps to,
// what the order does with a tie, what is refused before a read is issued.
// What it CANNOT prove is that the facts are read under the right scope: that
// is the SQL's property and is pinned by tests/integration/ranking_test.go.

const (
	actorID   = "11111111-1111-4111-8111-111111111111"
	projMine  = "22222222-2222-4222-8222-222222222222"
	projOther = "99999999-9999-4999-8999-999999999999"

	versionA = "aaaaaaaa-0000-4000-8000-000000000001"
	versionB = "bbbbbbbb-0000-4000-8000-000000000002"
	versionC = "cccccccc-0000-4000-8000-000000000003"
	versionD = "dddddddd-0000-4000-8000-000000000004"
)

// scopeReader is the smallest ProjectScopeReader, used to build a real
// resolved scope: Scope's fields are unexported and ResolveScope is its only
// constructor, so a test cannot (and should not) fabricate one.
type scopeReader struct{ projects []domain.Project }

func (r scopeReader) ListProjectsForUser(context.Context, string) ([]domain.Project, error) {
	return r.projects, nil
}

func scopeIn(t *testing.T, ids ...string) search.Scope {
	t.Helper()
	projects := make([]domain.Project, 0, len(ids))
	for _, id := range ids {
		projects = append(projects, domain.Project{ID: id})
	}
	scope, err := search.ResolveScope(context.Background(), scopeReader{projects: projects}, actorID)
	if err != nil {
		t.Fatalf("ResolveScope: %v", err)
	}
	return scope
}

// fakeStore is a FactorStore whose answers each test states, recording every
// call so a test can assert whether the read was issued at all.
type fakeStore struct {
	versions map[string]string
	refused  map[string]string
	facts    map[string]VersionFacts

	errVersions error
	errFacts    error

	gotPids     [][]string
	gotVersions [][]string
}

func (f *fakeStore) VersionsForPids(_ context.Context, _ search.Scope, pids []string) (map[string]string, map[string]string, error) {
	f.gotPids = append(f.gotPids, pids)
	if f.errVersions != nil {
		return nil, nil, f.errVersions
	}
	admitted := map[string]string{}
	refused := map[string]string{}
	for _, pid := range pids {
		if v, ok := f.versions[pid]; ok {
			admitted[pid] = v
		}
		if v, ok := f.refused[pid]; ok {
			refused[pid] = v
		}
	}
	return admitted, refused, nil
}

func (f *fakeStore) Factors(_ context.Context, _ search.Scope, ids []string) ([]VersionFacts, error) {
	f.gotVersions = append(f.gotVersions, ids)
	if f.errFacts != nil {
		return nil, f.errFacts
	}
	out := make([]VersionFacts, 0, len(ids))
	for _, id := range ids {
		if v, ok := f.facts[id]; ok {
			out = append(out, v)
		}
	}
	return out, nil
}

func quietRanker(t *testing.T, store FactorStore) *Ranker {
	t.Helper()
	r, err := NewRanker(store, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("NewRanker: %v", err)
	}
	return r
}

// doc builds a document candidate recalled by the named signals.
func doc(entityType, identity string, signals ...string) retrieval.Candidate {
	hits := make([]retrieval.SignalHit, 0, len(signals))
	for i, s := range signals {
		hits = append(hits, retrieval.SignalHit{Signal: s, Rank: i + 1})
	}
	return retrieval.Candidate{
		Ref:        search.EntityRef(entityType, identity),
		Kind:       retrieval.KindDocument,
		EntityType: entityType,
		Identity:   identity,
		Title:      identity,
		ProjectID:  projMine,
		Signals:    hits,
	}
}

// node builds a graph candidate: an object version reached by traversal.
func node(versionID, objectID string, versionNo int) retrieval.Candidate {
	ref := retrieval.RefKindObjectVersion + ":" + objectID
	if versionNo > 0 {
		ref = fmt.Sprintf("%s@%d", ref, versionNo)
	}
	return retrieval.Candidate{
		Ref:             ref,
		Kind:            retrieval.KindObjectVersion,
		Identity:        objectID,
		ProjectID:       projMine,
		ObjectID:        objectID,
		ObjectVersionID: versionID,
		VersionNo:       versionNo,
		Signals:         []retrieval.SignalHit{{Signal: retrieval.SignalGraph, Rank: 1}},
	}
}

// req builds the request the candidates "came from". It is NOT normalized
// (retrieval.Request's normalizer is unexported and lives where the limits
// are): the ranking reads only the three narrowing fields, and a ranker that
// needed a normalized request would be re-validating a retrieval that
// already ran.
func req(query string) retrieval.Request { return retrieval.Request{Query: query} }

// --------------------------------------------------------------------------
// The factor vocabulary

// TestFactorVocabularyIsClosed pins docs/14 §3's list verbatim. It is the
// mechanical half of the acceptance criterion "不按 star/prestige 主排序": a
// seventh factor added to the ladder would have to be added here too, in a
// diff a reviewer sees, and the test below names the words it may not be.
func TestFactorVocabularyIsClosed(t *testing.T) {
	want := []string{"query_scope_match", "evidence", "review", "reproduction", "conflict", "version"}
	if got := Factors(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Factors() = %v, want docs/14 §3's six in its own order: %v", got, want)
	}
	if len(levelsForFactor) != len(want) {
		t.Errorf("%d factors have ladders, want one per factor (%d)", len(levelsForFactor), len(want))
	}
}

// TestNoPrestigeFactor is the other half of the same acceptance criterion:
// the vocabulary a ranking exposes to a caller must contain no term that
// orders candidates by popularity or standing.
//
// It is a vocabulary test and not a source grep on purpose. A source grep for
// "star" would pass on this repository today and would keep passing if
// someone added a `contributors` field to VersionFacts — what a reviewer
// needs to be told is which NAMES reached the output, and that is what this
// reads. The behavioural half (two corpora that differ only in prestige
// proxies, and the ranking does not move) is
// tests/integration/ranking_test.go, where the proxies are real columns.
func TestNoPrestigeFactor(t *testing.T) {
	// Two lists, because one would have to be either too loose or too
	// tight: `phrase` matches a whole name, `word` matches one
	// underscore-separated segment. Checking names for SUBSTRINGS is what
	// makes a naive version of this test fail on `reviewed` (it contains
	// `view`), which is a false alarm about the word that carries the
	// factor's whole meaning.
	phrase := []string{"citation_count", "contributor_count", "member_count",
		"organization_size", "org_size", "h_index", "impact_factor", "star_count"}
	word := []string{"popular", "popularity", "stars", "star", "starred", "prestige",
		"prestigious", "reputation", "follower", "followers", "watch", "watchers",
		"views", "view", "citation", "citations", "contributor", "contributors",
		"member", "members", "size", "trust", "trusted", "quality", "score", "rank"}
	vocabulary := map[string][]string{"factor": Factors()}
	for _, factor := range Factors() {
		vocabulary["level:"+factor] = Levels(factor)
	}
	vocabulary["label"] = labelOrder
	for kind, names := range vocabulary {
		for _, name := range names {
			lower := strings.ToLower(name)
			for _, p := range phrase {
				if lower == p {
					t.Errorf("%s %q names a popularity/prestige property (%q); docs/14 §3 forbids ranking by one",
						kind, name, p)
				}
			}
			for _, segment := range strings.FieldsFunc(lower, func(r rune) bool { return r == '_' || r == '-' }) {
				for _, w := range word {
					if segment == w {
						t.Errorf("%s %q names a popularity/prestige property (%q); docs/14 §3 forbids ranking by one",
							kind, name, w)
					}
				}
			}
		}
	}
}

// TestLevelLaddersAreWellFormed checks that every declared level has exactly
// one position on exactly one factor's ladder — the property that makes an
// order explainable. A level with no position would panic at assessment time
// (assessment), and two levels sharing one would silently mean "equal".
func TestLevelLaddersAreWellFormed(t *testing.T) {
	for _, factor := range Factors() {
		ladder := Levels(factor)
		if len(ladder) < 2 {
			t.Errorf("factor %s has a ladder of %d levels, want at least 2", factor, len(ladder))
		}
		for i, level := range ladder {
			got, ok := levelRank(factor, level)
			if !ok {
				t.Errorf("factor %s: level %q has no position", factor, level)
				continue
			}
			if got != i {
				t.Errorf("factor %s: level %q is at index %d but ranks %d", factor, level, i, got)
			}
		}
	}
	if _, ok := levelRank(FactorEvidence, LevelContested); ok {
		t.Error("`contested` is a conflict level; it must not resolve on the evidence ladder")
	}
	if _, ok := levelRank("not_a_factor", LevelContested); ok {
		t.Error("an unknown factor must resolve no level")
	}
}

// TestUnknownRanksAfterEveryReadableLevel pins the fail-closed placement of
// `unknown` on each ladder: it must not outrank a level that reports a fact,
// and on the freshness ladder it must not outrank `aborted` (a withdrawal is
// a fact, and this is the absence of one).
func TestUnknownRanksAfterEveryReadableLevel(t *testing.T) {
	for _, factor := range []string{FactorEvidence, FactorReview, FactorReproduction, FactorConflict, FactorVersion} {
		ladder := Levels(factor)
		unknown, _ := levelRank(factor, LevelUnknown)
		if unknown != len(ladder)-1 && factor != FactorVersion {
			t.Errorf("factor %s: `unknown` is at %d of %d, want last", factor, unknown, len(ladder))
		}
	}
	if u, _ := levelRank(FactorVersion, LevelUnknown); u > mustRank(t, FactorVersion, LevelAborted) {
		t.Error("`unknown` must outrank `aborted`: a withdrawn version is a fact and an unread one is not")
	}
}

func mustRank(t *testing.T, factor, level string) int {
	t.Helper()
	rank, ok := levelRank(factor, level)
	if !ok {
		t.Fatalf("factor %s has no level %q", factor, level)
	}
	return rank
}

// --------------------------------------------------------------------------
// Refusals

func TestRankRefusesAnUnresolvedScope(t *testing.T) {
	store := &fakeStore{}
	r := quietRanker(t, store)
	_, err := r.Rank(context.Background(), search.Scope{}, req("q"), retrieval.Result{})
	if !errors.Is(err, ErrNoScope) || !errors.Is(err, search.ErrNoActor) {
		t.Fatalf("Rank with the zero Scope: %v, want ErrNoScope wrapping search.ErrNoActor", err)
	}
	if len(store.gotPids) != 0 || len(store.gotVersions) != 0 {
		t.Error("an unauthenticated ranking must not reach the store at all")
	}
}

func TestNewRankerRefusesANilStore(t *testing.T) {
	if _, err := NewRanker(nil); err == nil {
		t.Fatal("NewRanker(nil) succeeded; a ranker with no factor store must be refused, not degraded")
	}
}

func TestRankPropagatesStoreFailures(t *testing.T) {
	boom := errors.New("boom")
	store := &fakeStore{errFacts: boom}
	r := quietRanker(t, store)
	_, err := r.Rank(context.Background(), scopeIn(t, projMine), req("q"),
		retrieval.Result{Candidates: []retrieval.Candidate{node(versionA, "obj-a", 1)}})
	if !errors.Is(err, ErrStore) || !errors.Is(err, boom) {
		t.Fatalf("Rank with a failing store: %v, want ErrStore wrapping the cause", err)
	}

	store = &fakeStore{errVersions: boom}
	r = quietRanker(t, store)
	_, err = r.Rank(context.Background(), scopeIn(t, projMine), req("q"),
		retrieval.Result{Candidates: []retrieval.Candidate{doc(search.EntityKnowledge, "KP-1", retrieval.SignalFullText)}})
	if !errors.Is(err, ErrStore) || !errors.Is(err, boom) {
		t.Fatalf("Rank with a failing pid resolution: %v, want ErrStore wrapping the cause", err)
	}
}

func TestRankOfNoCandidates(t *testing.T) {
	r := quietRanker(t, &fakeStore{})
	got, err := r.Rank(context.Background(), scopeIn(t, projMine), req("q"), retrieval.Result{Query: "q"})
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	if got.Ranked == nil || len(got.Ranked) != 0 {
		t.Fatalf("Ranked = %#v, want an empty non-nil list", got.Ranked)
	}
	if !reflect.DeepEqual(got.Factors, Factors()) {
		t.Errorf("the result does not carry its factor vocabulary: %v", got.Factors)
	}
	// An empty result still renders, and renders an empty list rather than
	// a null: a consumer must not have to special-case "no candidates".
	raw, err := got.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"ranked": []`)) {
		t.Errorf("canonical JSON renders no candidates as something other than an empty list:\n%s", raw)
	}
}

// --------------------------------------------------------------------------
// Which level a fact sheet maps to

func TestEvidenceLevels(t *testing.T) {
	cases := []struct {
		name  string
		facts VersionFacts
		want  string
		sub   string // a phrase the reason must contain
	}{
		{"none", VersionFacts{ObjectVersionID: versionA}, LevelNoEvidence, "no evidence assertion"},
		{"asserted", VersionFacts{ObjectVersionID: versionA, EvidenceAssertions: 2}, LevelAsserted, "none reviewed"},
		// The verb agrees with the count. The reasons are read by people and
		// they are pinned in the golden fixtures, so "1 evidence assertion
		// target this version" is a defect a reviewer would have to read
		// past on every result.
		{"one-assertion-agrees", VersionFacts{ObjectVersionID: versionA, EvidenceAssertions: 1, EvidenceReviewed: 1},
			LevelReviewed, "1 evidence assertion targets this version"},
		{"many-assertions-agree", VersionFacts{ObjectVersionID: versionA, EvidenceAssertions: 2, EvidenceReviewed: 1},
			LevelReviewed, "2 evidence assertions target this version"},
		{"reviewed", VersionFacts{ObjectVersionID: versionA, EvidenceAssertions: 2, EvidenceReviewed: 1, EvidenceDirect: 2},
			LevelReviewed, "1 reviewed"},
		{"rejected", VersionFacts{ObjectVersionID: versionA, EvidenceAssertions: 1, EvidenceRejected: 1},
			LevelRejected, "1 rejected"},
		{"rejected-beats-asserted", VersionFacts{ObjectVersionID: versionA, EvidenceAssertions: 3, EvidenceRejected: 3},
			LevelRejected, "none reviewed and 3 rejected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := assessEvidence(versionA, tc.facts, true)
			if got.Level != tc.want {
				t.Errorf("level = %q, want %q (reason: %s)", got.Level, tc.want, got.Reason)
			}
			if !strings.Contains(got.Reason, tc.sub) {
				t.Errorf("reason %q does not contain %q", got.Reason, tc.sub)
			}
			if got.Facts["assertions"] != tc.facts.EvidenceAssertions {
				t.Errorf("Facts[assertions] = %d, want %d", got.Facts["assertions"], tc.facts.EvidenceAssertions)
			}
		})
	}
}

// TestReviewedEvidenceBeatsRejectedEvidence pins the order between the two
// levels a "has any assertion" predicate would collapse: one reviewed
// assertion is a stronger fact than three rejected ones.
func TestReviewedEvidenceBeatsRejectedEvidence(t *testing.T) {
	reviewed := assessEvidence(versionA, VersionFacts{
		ObjectVersionID: versionA, EvidenceAssertions: 3, EvidenceReviewed: 1, EvidenceRejected: 2}, true)
	rejected := assessEvidence(versionA, VersionFacts{
		ObjectVersionID: versionA, EvidenceAssertions: 3, EvidenceRejected: 3}, true)
	if reviewed.LevelRank >= rejected.LevelRank {
		t.Fatalf("reviewed evidence does not outrank rejected evidence (%d vs %d)",
			reviewed.LevelRank, rejected.LevelRank)
	}
}

func TestReviewLevels(t *testing.T) {
	cases := []struct {
		name  string
		facts VersionFacts
		want  string
	}{
		{"unreviewed", VersionFacts{ObjectVersionID: versionA}, LevelUnreviewed},
		{"approved", VersionFacts{ObjectVersionID: versionA, ReviewsScientific: 1, ReviewsApproved: 1}, LevelApproved},
		{"changes-requested", VersionFacts{ObjectVersionID: versionA, ReviewsScientific: 1, ReviewsChangesRequested: 1}, LevelReviewedState},
		{"approved-and-changes", VersionFacts{ObjectVersionID: versionA, ReviewsScientific: 2, ReviewsApproved: 1, ReviewsChangesRequested: 1}, LevelApproved},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := assessReview(versionA, tc.facts, true); got.Level != tc.want {
				t.Errorf("level = %q, want %q (reason: %s)", got.Level, tc.want, got.Reason)
			}
		})
	}
}

func TestReproductionLevels(t *testing.T) {
	cases := []struct {
		name  string
		facts VersionFacts
		want  string
	}{
		{"none", VersionFacts{ObjectVersionID: versionA}, LevelNoReproduction},
		{"self", VersionFacts{ObjectVersionID: versionA, Reproduces: 1}, LevelSelfReproduced},
		{"independent", VersionFacts{ObjectVersionID: versionA, Reproduces: 2, ReproducesIndependent: 2}, LevelIndependent},
		{"independent-and-failed", VersionFacts{ObjectVersionID: versionA, Reproduces: 1, ReproducesIndependent: 1, FailsToReproduce: 1},
			LevelDisputedReproduction},
		{"only-a-failure", VersionFacts{ObjectVersionID: versionA, FailsToReproduce: 1}, LevelNoReproduction},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := assessReproduction(versionA, tc.facts, true)
			if got.Level != tc.want {
				t.Errorf("level = %q, want %q (reason: %s)", got.Level, tc.want, got.Reason)
			}
			if tc.facts.FailsToReproduce > 0 && !strings.Contains(got.Reason, "failure") {
				t.Errorf("reason %q does not mention the recorded failure", got.Reason)
			}
		})
	}
}

// TestIndependentReproductionOutranksSelfReproduction and the disputed case
// below pin the two rules the reproduction factor exists for.
func TestIndependentReproductionOutranksSelfReproduction(t *testing.T) {
	independent := assessReproduction(versionA, VersionFacts{
		ObjectVersionID: versionA, Reproduces: 1, ReproducesIndependent: 1}, true)
	self := assessReproduction(versionA, VersionFacts{ObjectVersionID: versionA, Reproduces: 9}, true)
	if independent.LevelRank >= self.LevelRank {
		t.Fatalf("an independent reproduction does not outrank nine self-reproductions (%d vs %d): "+
			"the factor would be a counter, not an independence test", independent.LevelRank, self.LevelRank)
	}
}

func TestDisputedReproductionRanksBelowAnUncontestedOne(t *testing.T) {
	disputed := assessReproduction(versionA, VersionFacts{
		ObjectVersionID: versionA, Reproduces: 1, ReproducesIndependent: 1, FailsToReproduce: 1}, true)
	clean := assessReproduction(versionA, VersionFacts{
		ObjectVersionID: versionA, Reproduces: 1, ReproducesIndependent: 1}, true)
	if disputed.LevelRank <= clean.LevelRank {
		t.Fatalf("a disputed reproduction (%d) does not rank below a clean one (%d)",
			disputed.LevelRank, clean.LevelRank)
	}
}

func TestConflictLevels(t *testing.T) {
	uncontested := assessConflict(versionA, VersionFacts{ObjectVersionID: versionA, EvidenceSupporting: 5}, true)
	contested := assessConflict(versionA, VersionFacts{ObjectVersionID: versionA, ContradictingRelations: 1}, true)
	if uncontested.Level != LevelUncontested || contested.Level != LevelContested {
		t.Fatalf("levels = %q/%q, want %q/%q", uncontested.Level, contested.Level, LevelUncontested, LevelContested)
	}
	if contested.LevelRank <= uncontested.LevelRank {
		t.Error("a contested version must rank below an uncontested one")
	}
}

// TestConflictReasonsNameOnlyNonZeroHalves pins the reason shape: a finding
// is rendered as the facts it is about, not as a template that reports its
// own zeroes ("and 0 contradicts relations") on every contested result.
func TestConflictReasonsNameOnlyNonZeroHalves(t *testing.T) {
	cases := []struct {
		name  string
		facts VersionFacts
		want  string
		gone  []string
	}{
		{"both halves", VersionFacts{ObjectVersionID: versionA, EvidenceContradicting: 2, ContradictingRelations: 1},
			"2 contradictory evidence assertions and 1 contradicts relation touch this version", nil},
		{"one assertion", VersionFacts{ObjectVersionID: versionA, EvidenceContradicting: 1},
			"1 contradictory evidence assertion touches this version", []string{"relation", "and"}},
		{"relations only", VersionFacts{ObjectVersionID: versionA, ContradictingRelations: 3},
			"3 contradicts relations touch this version", []string{"evidence assertion", "and"}},
		{"neither", VersionFacts{ObjectVersionID: versionA},
			"nothing on the platform contradicts this version", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := assessConflict(versionA, tc.facts, true)
			if !strings.Contains(got.Reason, tc.want) {
				t.Errorf("reason = %q, want it to contain %q", got.Reason, tc.want)
			}
			for _, gone := range tc.gone {
				if strings.Contains(got.Reason, gone) {
					t.Errorf("reason %q names %q, which is not a fact about this version", got.Reason, gone)
				}
			}
		})
	}
}

func TestVersionLevels(t *testing.T) {
	cases := []struct {
		name     string
		version  string
		facts    VersionFacts
		have     bool
		want     string
		reasonIn string
	}{
		{"current", versionA, VersionFacts{ObjectVersionID: versionA, VersionNo: 2, NewestVersionNo: 2, IsNewest: true, LifecycleState: "active"}, true, LevelCurrent, "newest version"},
		{"historical", versionA, VersionFacts{ObjectVersionID: versionA, VersionNo: 1, NewestVersionNo: 2}, true, LevelHistorical, "newest is 2"},
		{"superseded-but-newest", versionA, VersionFacts{ObjectVersionID: versionA, VersionNo: 2, NewestVersionNo: 2, IsNewest: true, LifecycleState: "superseded"}, true, LevelHistorical, "superseded"},
		{"aborted", versionA, VersionFacts{ObjectVersionID: versionA, VersionNo: 2, NewestVersionNo: 2, IsNewest: true, LifecycleState: "aborted"}, true, LevelAborted, "aborted"},
		{"unversioned", "", VersionFacts{}, false, LevelUnversioned, "does not resolve to a scientific object version"},
		{"unknown", versionA, VersionFacts{}, false, LevelUnknown, "not in the caller's project scope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := assessVersion(tc.version, tc.facts, tc.have)
			if got.Level != tc.want {
				t.Errorf("level = %q, want %q (reason: %s)", got.Level, tc.want, got.Reason)
			}
			if !strings.Contains(got.Reason, tc.reasonIn) {
				t.Errorf("reason %q does not contain %q", got.Reason, tc.reasonIn)
			}
		})
	}
	// An aborted version ranks below every other freshness level: nothing
	// disappears (CLAUDE.md §9.8), so it is returned, and last.
	if a, b := mustRank(t, FactorVersion, LevelAborted), mustRank(t, FactorVersion, LevelUnknown); a <= b {
		t.Error("an aborted version must rank below an unread one")
	}
}

// TestUnreadFactsAreUnknownAndNeverNone is the fail-closed rule at the level
// it is decided: a version whose facts were not read must not be reported as
// one with no evidence, no review and no reproduction — those are claims
// about someone else's corpus made without looking at it.
func TestUnreadFactsAreUnknownAndNeverNone(t *testing.T) {
	for _, tc := range []struct {
		factor string
		assess func(string, VersionFacts, bool) Assessment
		benign string
	}{
		{FactorEvidence, assessEvidence, LevelNoEvidence},
		{FactorReview, assessReview, LevelUnreviewed},
		{FactorReproduction, assessReproduction, LevelNoReproduction},
		{FactorConflict, assessConflict, LevelUncontested},
	} {
		got := tc.assess(versionA, VersionFacts{}, false)
		if got.Level != LevelUnknown {
			t.Errorf("factor %s with unread facts = %q, want %q", tc.factor, got.Level, LevelUnknown)
		}
		if got.Level == tc.benign {
			t.Errorf("factor %s reports unread facts as its benign level %q", tc.factor, tc.benign)
		}
		if got.Facts != nil {
			t.Errorf("factor %s reports facts it did not read: %v", tc.factor, got.Facts)
		}
	}
}

func TestQueryScopeLevels(t *testing.T) {
	cases := []struct {
		name   string
		cand   retrieval.Candidate
		want   string
		reason string
	}{
		{"text", doc(search.EntityKnowledge, "KP-1", retrieval.SignalFullText), LevelQuestion, "full_text"},
		{"vector", doc(search.EntityKnowledge, "KP-1", retrieval.SignalVector), LevelQuestion, "vector"},
		{"text-and-vector", doc(search.EntityKnowledge, "KP-1", retrieval.SignalFullText, retrieval.SignalVector),
			LevelQuestion, "full_text, vector"},
		{"facets-only", doc(search.EntityKnowledge, "KP-1", retrieval.SignalFacets), LevelConditions, "structured filter"},
		{"graph-only", node(versionA, "obj-a", 1), LevelRelated, "traversal"},
		{"nothing", retrieval.Candidate{Ref: "knowledge:KP-1"}, LevelUnattributed, "no recall signal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := assessQueryScope(tc.cand, req("q"))
			if got.Level != tc.want {
				t.Errorf("level = %q, want %q (reason: %s)", got.Level, tc.want, got.Reason)
			}
			if !strings.Contains(got.Reason, tc.reason) {
				t.Errorf("reason %q does not contain %q", got.Reason, tc.reason)
			}
		})
	}
}

// TestScopeMatchNamesTheNarrowing pins that the one factor that reads no row
// still explains the request it was ranked under.
func TestScopeMatchNamesTheNarrowing(t *testing.T) {
	narrowed := req("q")
	narrowed.EntityTypes = []string{search.EntityKnowledge}
	narrowed.PublicOnly = true
	narrowed.Facets = []byte(`{"object_type":"claim"}`)
	got := assessQueryScope(doc(search.EntityKnowledge, "KP-1", retrieval.SignalFullText), narrowed)
	for _, want := range []string{"narrowed to entity types knowledge", "public rows only", "a structured filter was applied"} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("reason %q does not mention %q", got.Reason, want)
		}
	}
	open := assessQueryScope(doc(search.EntityKnowledge, "KP-1", retrieval.SignalFullText), req("q"))
	if !strings.Contains(open.Reason, "imposed no narrowing") {
		t.Errorf("an unnarrowed result's reason does not say so: %q", open.Reason)
	}
}

// --------------------------------------------------------------------------
// The order

// TestRankOrdersByTheFirstFactorThatDiffers is the ordering rule stated as a
// table: each case has two candidates differing in exactly one factor, and
// the one with the better level must come first — whatever their fusion
// scores are.
func TestRankOrdersByTheFirstFactorThatDiffers(t *testing.T) {
	strong := VersionFacts{ObjectVersionID: versionA, VersionNo: 1, NewestVersionNo: 1, IsNewest: true,
		LifecycleState: "active", EvidenceAssertions: 1, EvidenceReviewed: 1}
	weak := VersionFacts{ObjectVersionID: versionB, VersionNo: 1, NewestVersionNo: 1, IsNewest: true,
		LifecycleState: "active"}

	a := node(versionA, "aaaa", 1)
	a.Score = 0.01 // a LOW fusion score: relevance is not the ordering input here
	b := node(versionB, "bbbb", 1)
	b.Score = 9.99 // a HIGH one, as if the recall signal loved it

	r := quietRanker(t, &fakeStore{facts: map[string]VersionFacts{versionA: strong, versionB: weak}})
	got, err := r.Rank(context.Background(), scopeIn(t, projMine), req("q"),
		retrieval.Result{Candidates: []retrieval.Candidate{b, a}})
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	if got.Ranked[0].ObjectVersionID != versionA {
		t.Fatalf("rank 1 is %s, want the better-evidenced %s: a recall score decided the scientific ranking",
			got.Ranked[0].ObjectVersionID, versionA)
	}
	if got.Ranked[0].Rank != 1 || got.Ranked[1].Rank != 2 {
		t.Errorf("ranks are %d,%d, want 1,2", got.Ranked[0].Rank, got.Ranked[1].Rank)
	}
}

// TestFullTieKeepsTheRetrievalOrder pins the last tie-break. The ranking
// cannot separate two candidates identical on all six factors, and it does
// not pretend to: it keeps the fusion's order rather than inventing one from
// the ref's spelling, which is what makes "we could not tell these apart"
// visible instead of hidden behind an alphabetical accident.
func TestFullTieKeepsTheRetrievalOrder(t *testing.T) {
	facts := map[string]VersionFacts{
		versionA: {ObjectVersionID: versionA, VersionNo: 1, NewestVersionNo: 1, IsNewest: true, LifecycleState: "active"},
		versionB: {ObjectVersionID: versionB, VersionNo: 1, NewestVersionNo: 1, IsNewest: true, LifecycleState: "active"},
	}
	// "zzz" sorts after "aaa", so the input order is the ONLY thing that can
	// put it first.
	a := node(versionA, "aaa", 1)
	z := node(versionB, "zzz", 1)
	r := quietRanker(t, &fakeStore{facts: facts})

	first, err := r.Rank(context.Background(), scopeIn(t, projMine), req("q"),
		retrieval.Result{Candidates: []retrieval.Candidate{z, a}})
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	if first.Ranked[0].ObjectVersionID != versionB {
		t.Fatalf("rank 1 is %s, want the candidate the retrieval ranked first (%s)",
			first.Ranked[0].ObjectVersionID, versionB)
	}

	second, err := r.Rank(context.Background(), scopeIn(t, projMine), req("q"),
		retrieval.Result{Candidates: []retrieval.Candidate{a, z}})
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	if second.Ranked[0].ObjectVersionID != versionA {
		t.Fatalf("rank 1 is %s, want the candidate the retrieval ranked first (%s)",
			second.Ranked[0].ObjectVersionID, versionA)
	}
}

// TestRankIsIndependentOfTheOrderItIsGiven proves the property the tie-break
// above is written around: when the factors DO separate two candidates, the
// order they arrive in does not matter.
func TestRankIsIndependentOfTheOrderItIsGiven(t *testing.T) {
	facts := map[string]VersionFacts{
		versionA: {ObjectVersionID: versionA, VersionNo: 1, NewestVersionNo: 1, IsNewest: true, LifecycleState: "active",
			EvidenceAssertions: 1, EvidenceReviewed: 1},
		versionB: {ObjectVersionID: versionB, VersionNo: 1, NewestVersionNo: 2, LifecycleState: "active"},
		versionC: {ObjectVersionID: versionC, VersionNo: 1, NewestVersionNo: 1, IsNewest: true, LifecycleState: "aborted"},
		versionD: {ObjectVersionID: versionD, VersionNo: 1, NewestVersionNo: 1, IsNewest: true, LifecycleState: "active",
			EvidenceAssertions: 2, EvidenceContradicting: 2, ContradictingRelations: 1},
	}
	a, b := node(versionA, "aaaa", 1), node(versionB, "bbbb", 1)
	c, d := node(versionC, "cccc", 1), node(versionD, "dddd", 1)
	r := quietRanker(t, &fakeStore{facts: facts})

	want := ""
	for _, order := range [][]retrieval.Candidate{
		{a, b, c, d}, {d, c, b, a}, {b, d, a, c}, {c, a, d, b},
	} {
		got, err := r.Rank(context.Background(), scopeIn(t, projMine), req("q"),
			retrieval.Result{Candidates: order})
		if err != nil {
			t.Fatalf("Rank: %v", err)
		}
		refs := make([]string, 0, len(got.Ranked))
		for _, rc := range got.Ranked {
			refs = append(refs, rc.Ref)
		}
		rendering := strings.Join(refs, "|")
		if want == "" {
			want = rendering
			continue
		}
		if rendering != want {
			t.Fatalf("the order depends on the input order:\n%s\nvs\n%s", want, rendering)
		}
	}
	if !strings.Contains(want, "aaaa") {
		t.Fatalf("sanity: the best-evidenced candidate is not in the order at all: %s", want)
	}
}

// TestRankIsRepeatable runs the same ranking many times: the sort must not
// consult map iteration, which Go randomises per run.
func TestRankIsRepeatable(t *testing.T) {
	facts := map[string]VersionFacts{}
	cands := make([]retrieval.Candidate, 0, 12)
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("%08d-0000-4000-8000-00000000000%d", i, i%10)
		object := fmt.Sprintf("obj-%02d", i)
		facts[id] = VersionFacts{ObjectVersionID: id, VersionNo: 1, NewestVersionNo: 1 + i%2,
			IsNewest: i%2 == 0, LifecycleState: "active", EvidenceAssertions: i % 3}
		cands = append(cands, node(id, object, 1))
	}
	r := quietRanker(t, &fakeStore{facts: facts})
	first, err := r.Rank(context.Background(), scopeIn(t, projMine), req("q"), retrieval.Result{Candidates: cands})
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	want, err := first.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	for i := 0; i < 50; i++ {
		got, err := r.Rank(context.Background(), scopeIn(t, projMine), req("q"), retrieval.Result{Candidates: cands})
		if err != nil {
			t.Fatalf("Rank (run %d): %v", i, err)
		}
		raw, err := got.CanonicalJSON()
		if err != nil {
			t.Fatalf("CanonicalJSON (run %d): %v", i, err)
		}
		if !bytes.Equal(want, raw) {
			t.Fatalf("run %d differs from run 0:\n%s\n---\n%s", i, want, raw)
		}
	}
}

// --------------------------------------------------------------------------
// Explanation and shape

// TestEveryAssessmentExplainsItself pins "透明 explanation fields" as a
// property of the output rather than a style: no assessment may carry an
// empty reason, a level with no position, or a reason that is just the level
// name repeated.
func TestEveryAssessmentExplainsItself(t *testing.T) {
	facts := map[string]VersionFacts{
		versionA: {ObjectVersionID: versionA, VersionNo: 2, NewestVersionNo: 2, IsNewest: true, LifecycleState: "active",
			EvidenceAssertions: 3, EvidenceReviewed: 1, EvidenceDirect: 2, EvidenceSupporting: 2, EvidenceContradicting: 1,
			Reproduces: 2, ReproducesIndependent: 1, FailsToReproduce: 1, ReviewsScientific: 2, ReviewsApproved: 1,
			ReviewsChangesRequested: 1, ContradictingRelations: 1},
	}
	cands := []retrieval.Candidate{
		doc(search.EntityKnowledge, "KP-1", retrieval.SignalFullText, retrieval.SignalGraph),
		doc(search.EntityAsset, "asset-1", retrieval.SignalFacets),
		node(versionA, "obj-a", 2),
	}
	r := quietRanker(t, &fakeStore{
		versions: map[string]string{"KP-1": versionA},
		facts:    facts,
	})
	got, err := r.Rank(context.Background(), scopeIn(t, projMine), req("q"), retrieval.Result{Query: "q", Candidates: cands})
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	if len(got.Ranked) != len(cands) {
		t.Fatalf("%d ranked, want %d: re-ranking must not add or drop candidates", len(got.Ranked), len(cands))
	}
	for _, rc := range got.Ranked {
		if len(rc.Factors) != len(Factors()) {
			t.Errorf("%s carries %d assessments, want %d", rc.Ref, len(rc.Factors), len(Factors()))
		}
		for i, a := range rc.Factors {
			if a.Factor != Factors()[i] {
				t.Errorf("%s assessment %d is %q, want %q (priority order)", rc.Ref, i, a.Factor, Factors()[i])
			}
			if strings.TrimSpace(a.Reason) == "" {
				t.Errorf("%s: factor %s has no reason", rc.Ref, a.Factor)
			}
			if strings.EqualFold(strings.TrimSpace(a.Reason), a.Level) {
				t.Errorf("%s: factor %s's reason is just its level name (%q)", rc.Ref, a.Factor, a.Reason)
			}
			if _, ok := levelRank(a.Factor, a.Level); !ok {
				t.Errorf("%s: factor %s has level %q, which is not on its ladder", rc.Ref, a.Factor, a.Level)
			}
		}
		if rc.Labels == nil {
			t.Errorf("%s renders no label list; want an empty list rather than null", rc.Ref)
		}
		if rc.Candidate.Ref != rc.Ref {
			t.Errorf("%s does not carry its candidate (candidate.Ref = %q)", rc.Ref, rc.Candidate.Ref)
		}
	}
}

// TestLabelsFollowTheFacts pins docs/10 §8's four descriptive labels, and
// that a well-evidenced version carries none of them: a label is a warning or
// a distinction, not a badge every candidate gets.
func TestLabelsFollowTheFacts(t *testing.T) {
	cases := []struct {
		name  string
		facts VersionFacts
		want  []string
	}{
		{"nothing asserted", VersionFacts{ObjectVersionID: versionA}, []string{LabelLimitedEvidence}},
		{"mixed and contested", VersionFacts{ObjectVersionID: versionA, EvidenceAssertions: 3,
			EvidenceSupporting: 2, EvidenceContradicting: 1, ContradictingRelations: 2},
			[]string{LabelMixedEvidence, LabelActivelyContested}},
		{"independent", VersionFacts{ObjectVersionID: versionA, EvidenceAssertions: 1,
			EvidenceSupporting: 1, Reproduces: 1, ReproducesIndependent: 1},
			[]string{LabelIndependentlyReproduced}},
		{"reviewed and clean", VersionFacts{ObjectVersionID: versionA, EvidenceAssertions: 2,
			EvidenceReviewed: 2, EvidenceSupporting: 2}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := labelsFor(tc.facts, true); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("labels = %v, want %v", got, tc.want)
			}
		})
	}
	if got := labelsFor(VersionFacts{}, false); len(got) != 0 {
		t.Errorf("an unread version carries labels: %v — labels are claims about facts, and none was read", got)
	}
}

// TestCanonicalJSONDoesNotEscapeHTML pins the rendering choice the golden
// fixtures depend on: a question about a reaction arrow must survive the
// round trip.
func TestCanonicalJSONDoesNotEscapeHTML(t *testing.T) {
	got, err := Result{Query: "CO2 -> CH4 <catalyst>", Factors: Factors(), Ranked: []Ranked{}}.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if !bytes.Contains(got, []byte("CO2 -> CH4 <catalyst>")) {
		t.Errorf("the canonical rendering escaped its input:\n%s", got)
	}
	if !bytes.HasSuffix(got, []byte("\n")) {
		t.Error("the canonical rendering is not newline-terminated")
	}
}

// TestResolveVersionsUsesTheCandidatesOwnPin proves the ranking does not
// re-derive a version the traversal already pinned, and that it resolves the
// ones it did not — the seed budget (retrieval.Limits.Seeds) is a candidate
// budget, and inheriting it here would rank the candidates past it as "no
// evidence".
func TestResolveVersionsUsesTheCandidatesOwnPin(t *testing.T) {
	pinned := node(versionA, "obj-a", 1)
	unpinned := doc(search.EntityKnowledge, "KP-1", retrieval.SignalFullText)
	asset := doc(search.EntityAsset, "asset-1", retrieval.SignalFullText)
	store := &fakeStore{versions: map[string]string{"KP-1": versionB}}
	r := quietRanker(t, store)
	got, err := r.Rank(context.Background(), scopeIn(t, projMine), req("q"),
		retrieval.Result{Candidates: []retrieval.Candidate{pinned, unpinned, asset}})
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	if len(store.gotPids) != 1 || !reflect.DeepEqual(store.gotPids[0], []string{"KP-1"}) {
		t.Fatalf("pid resolution asked for %v, want exactly the unpinned knowledge document", store.gotPids)
	}
	byRef := map[string]Ranked{}
	for _, rc := range got.Ranked {
		byRef[rc.Ref] = rc
	}
	if got := byRef[pinned.Ref].ObjectVersionID; got != versionA {
		t.Errorf("the already-pinned candidate resolved to %q, want its own %q", got, versionA)
	}
	if got := byRef[unpinned.Ref].ObjectVersionID; got != versionB {
		t.Errorf("the knowledge document resolved to %q, want %q", got, versionB)
	}
	if got := byRef[asset.Ref].ObjectVersionID; got != "" {
		t.Errorf("an asset resolved to object version %q; an asset is not a version-pinned object", got)
	}
}

// TestScopeRefusedVersionIsUnknownNotUnversioned pins the answer the blocking
// review demanded, at the level it is decided: a knowledge document whose
// publication pins a version OUTSIDE the caller's scope is not "unversioned"
// — the version exists, and the ranking must say it could not be read, on
// every database-backed factor, with the scope as the reason. The version
// factor is the sharp half: `unversioned` outranks `unknown` on its ladder,
// so collapsing the two does not just misdescribe the candidate, it promotes
// it. The facts for such a version must never even be requested.
func TestScopeRefusedVersionIsUnknownNotUnversioned(t *testing.T) {
	store := &fakeStore{
		versions: map[string]string{"KP-MINE": versionA},
		refused:  map[string]string{"KP-EXT": versionC},
		facts: map[string]VersionFacts{
			versionA: {ObjectVersionID: versionA, VersionNo: 1, NewestVersionNo: 1, IsNewest: true, LifecycleState: "active"},
		},
	}
	r := quietRanker(t, store)
	mine := doc(search.EntityKnowledge, "KP-MINE", retrieval.SignalFullText)
	ext := doc(search.EntityKnowledge, "KP-EXT", retrieval.SignalFullText)
	none := doc(search.EntityKnowledge, "KP-NONE", retrieval.SignalFullText)
	got, err := r.Rank(context.Background(), scopeIn(t, projMine), req("q"),
		retrieval.Result{Candidates: []retrieval.Candidate{mine, ext, none}})
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	byRef := map[string]Ranked{}
	for _, rc := range got.Ranked {
		byRef[rc.Ref] = rc
	}

	refused := byRef[ext.Ref]
	if refused.ObjectVersionID != versionC {
		t.Errorf("the refused candidate resolved to %q, want the version its publication pins (%q): "+
			"the version exists; it is only unreadable", refused.ObjectVersionID, versionC)
	}
	for _, a := range refused.Factors {
		if a.Factor == FactorQueryScopeMatch {
			continue
		}
		if a.Level != LevelUnknown {
			t.Errorf("factor %s = %q for a scope-refused version, want %q", a.Factor, a.Level, LevelUnknown)
		}
		if a.Factor == FactorVersion && a.Level == LevelUnversioned {
			t.Error("the version factor reports a scope-refused version as `unversioned`: " +
				"that claims no version exists, and it outranks `unknown`")
		}
		if !strings.Contains(a.Reason, "not in the caller's project scope") {
			t.Errorf("factor %s's reason does not name the scope refusal: %q", a.Factor, a.Reason)
		}
	}
	if len(refused.Labels) != 0 {
		t.Errorf("an unread version carries labels: %v", refused.Labels)
	}
	// The facts read must not ASK about the refused version: the store would
	// refuse it, and the ranking does not delegate the refusal to the SQL.
	for _, ids := range store.gotVersions {
		for _, id := range ids {
			if id == versionC {
				t.Errorf("the facts read asked about the scope-refused version %s", versionC)
			}
		}
	}

	// The contrast case, in the same ranking: a pid that names no publication
	// at all is genuinely unversioned, and the reasons must say so. The two
	// absences are different facts and rank differently.
	unresolved := byRef[none.Ref]
	if unresolved.ObjectVersionID != "" {
		t.Errorf("an unpublished pid resolved to %q, want no version", unresolved.ObjectVersionID)
	}
	for _, a := range unresolved.Factors {
		switch {
		case a.Factor == FactorVersion:
			if a.Level != LevelUnversioned || !strings.Contains(a.Reason, "does not resolve to a scientific object version") {
				t.Errorf("an unresolved pid's version factor = %q (%q), want `unversioned` saying so", a.Level, a.Reason)
			}
		case a.Factor != FactorQueryScopeMatch:
			if a.Level != LevelUnknown || !strings.Contains(a.Reason, "resolves to no scientific object version") {
				t.Errorf("an unresolved pid's %s factor = %q (%q), want `unknown` saying there is nothing to read",
					a.Factor, a.Level, a.Reason)
			}
		}
	}
}

// TestRankNeverWidens pins the property the ranking's authorization story
// rests on: the returned refs are exactly the input's, whatever the facts
// say. A candidate the store knows nothing about is still returned — ranked
// by what could be read — never dropped and never joined by a new one.
func TestRankNeverWidens(t *testing.T) {
	cands := []retrieval.Candidate{
		node(versionA, "obj-a", 1),
		doc(search.EntityKnowledge, "KP-1", retrieval.SignalFullText),
		doc(search.EntityRelease, "rel-1", retrieval.SignalFacets),
	}
	r := quietRanker(t, &fakeStore{facts: map[string]VersionFacts{
		versionA: {ObjectVersionID: versionA, VersionNo: 1, NewestVersionNo: 1, IsNewest: true, LifecycleState: "active"}}})
	got, err := r.Rank(context.Background(), scopeIn(t, projMine), req("q"), retrieval.Result{Candidates: cands})
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	in := map[string]bool{}
	for _, c := range cands {
		in[c.Ref] = true
	}
	if len(got.Ranked) != len(in) {
		t.Fatalf("%d ranked, want the %d it was given", len(got.Ranked), len(in))
	}
	for _, rc := range got.Ranked {
		if !in[rc.Ref] {
			t.Errorf("%s is in the result but was not a candidate", rc.Ref)
		}
		delete(in, rc.Ref)
	}
	if len(in) != 0 {
		t.Errorf("candidates missing from the result: %v", in)
	}
}
