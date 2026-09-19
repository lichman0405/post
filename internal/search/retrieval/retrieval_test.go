package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/search"
	"github.com/lichman0405/post/internal/search/embedding"
	"github.com/lichman0405/post/internal/search/planner"
)

// The retrieval's unit suite. It drives a fake Store, so everything it proves
// is about the pipeline's own behaviour: which signal runs when, what the
// fusion prefers, how a hop attaches to a candidate, what is refused before a
// read is issued. What it CANNOT prove is that a read is authorized — that is
// the SQL's property, and it is pinned by the integration suite
// (tests/integration/retrieval_test.go) against a real corpus.

const (
	projMine  = "22222222-2222-4222-8222-222222222222"
	projOther = "99999999-9999-4999-8999-999999999999"
	actorID   = "11111111-1111-4111-8111-111111111111"
)

// scopeReader is the smallest ProjectScopeReader, used to build a real
// resolved scope: Scope's fields are unexported and ResolveScope is its only
// constructor, so a test cannot (and should not) fabricate one.
type scopeReader struct{ projects []domain.Project }

func (r scopeReader) ListProjectsForUser(context.Context, string) ([]domain.Project, error) {
	return r.projects, nil
}

// scopeIn resolves a scope over the given projects.
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

// fakeStore is a Store whose behaviour each test states: which rows each
// signal returns, and what the traversal sees. Every call is recorded, so a
// test can assert not only what came back but whether the read was issued at
// all (a refused request must not reach the store).
type fakeStore struct {
	fullText []DocumentHit
	vector   []DocumentHit
	facets   []DocumentHit
	edges    []GraphEdge
	nodes    map[string]GraphObject
	seeds    map[string]SeedVersion

	errFullText error
	errVector   error
	errFacets   error
	errEdges    error
	errNodes    error
	errSeeds    error

	gotFullText []DocumentQuery
	gotVector   []VectorQuery
	gotFacets   []DocumentQuery
	gotEdges    [][]string
	gotNodes    [][]string
	gotSeeds    [][]string
}

func (f *fakeStore) FullText(_ context.Context, q DocumentQuery) ([]DocumentHit, error) {
	f.gotFullText = append(f.gotFullText, q)
	return f.fullText, f.errFullText
}

func (f *fakeStore) Vector(_ context.Context, q VectorQuery) ([]DocumentHit, error) {
	f.gotVector = append(f.gotVector, q)
	return f.vector, f.errVector
}

func (f *fakeStore) Facets(_ context.Context, q DocumentQuery) ([]DocumentHit, error) {
	f.gotFacets = append(f.gotFacets, q)
	return f.facets, f.errFacets
}

func (f *fakeStore) ObjectVersions(_ context.Context, _ search.Scope, ids []string) ([]GraphObject, error) {
	f.gotNodes = append(f.gotNodes, ids)
	if f.errNodes != nil {
		return nil, f.errNodes
	}
	out := make([]GraphObject, 0, len(ids))
	for _, id := range ids {
		if node, ok := f.nodes[id]; ok {
			out = append(out, node)
		}
	}
	return out, nil
}

func (f *fakeStore) SeedObjectVersions(_ context.Context, _ search.Scope, pids []string) ([]SeedVersion, error) {
	f.gotSeeds = append(f.gotSeeds, pids)
	if f.errSeeds != nil {
		return nil, f.errSeeds
	}
	out := make([]SeedVersion, 0, len(pids))
	for _, pid := range pids {
		if seed, ok := f.seeds[pid]; ok {
			out = append(out, seed)
		}
	}
	return out, nil
}

func (f *fakeStore) AdjacentRelations(_ context.Context, _ search.Scope, ids []string) ([]GraphEdge, error) {
	f.gotEdges = append(f.gotEdges, ids)
	return f.edges, f.errEdges
}

// fakeEmbedder is an embedding.Embedder with a stated model and vectors, so a
// test can assert the provenance that reaches the store.
type fakeEmbedder struct {
	model   embedding.Model
	vectors [][]float32
	err     error
	texts   []string
}

func (e *fakeEmbedder) Model() embedding.Model { return e.model }

func (e *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.texts = append(e.texts, texts...)
	if e.err != nil {
		return nil, e.err
	}
	if e.vectors != nil {
		return e.vectors, nil
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		v := make([]float32, embedding.Dimensions)
		v[0] = 1
		out[i] = v
	}
	return out, nil
}

var testModel = embedding.Model{Provider: "post-local", Name: "sha256-bag", Version: "v1"}

// doc is one projected row, with the facets the projection writes.
func doc(ref, entityType, projectID, title string, facets map[string]string) DocumentHit {
	structured := map[string]any{"entity_type": entityType}
	for k, v := range facets {
		structured[k] = v
	}
	raw, err := json.Marshal(structured)
	if err != nil {
		panic(err)
	}
	return DocumentHit{
		Ref:        ref,
		EntityType: entityType,
		Visibility: search.VisibilityPublic,
		ProjectID:  projectID,
		Title:      title,
		Structured: raw,
	}
}

func newTestRetriever(t *testing.T, store Store, emb embedding.Embedder) *Retriever {
	t.Helper()
	r, err := NewRetriever(store, emb, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("NewRetriever: %v", err)
	}
	return r
}

func refs(cands []Candidate) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Ref)
	}
	return out
}

func reportFor(t *testing.T, res Result, signal string) SignalReport {
	t.Helper()
	for _, r := range res.Signals {
		if r.Signal == signal {
			return r
		}
	}
	t.Fatalf("no report for signal %q in %+v", signal, res.Signals)
	return SignalReport{}
}

// TestRefusesAnUnresolvedScopeWithoutReading: the zero Scope is not a scope,
// and the refusal must happen BEFORE any read. This is the fail-open shape
// the whole design exists to prevent — a store handed an empty scope reads
// the public corpus and reports success, so a request that never resolved a
// scope must not reach it.
func TestRefusesAnUnresolvedScopeWithoutReading(t *testing.T) {
	store := &fakeStore{}
	r := newTestRetriever(t, store, nil)

	_, err := r.Retrieve(context.Background(), search.Scope{}, Request{Query: "mof"})
	if !errors.Is(err, ErrNoScope) {
		t.Fatalf("err = %v, want ErrNoScope", err)
	}
	if !errors.Is(err, search.ErrNoActor) {
		t.Errorf("err = %v, want it to wrap search.ErrNoActor: the scope layer's refusal and the retrieval's are one reason", err)
	}
	if len(store.gotFullText)+len(store.gotVector)+len(store.gotFacets)+len(store.gotSeeds)+len(store.gotEdges)+len(store.gotNodes) != 0 {
		t.Errorf("the store was read under an unresolved scope: %+v", store)
	}
}

// TestSignalsReportWhatDidNotRun: a signal that did not run is reported with
// its reason, in pipeline order, and the report distinguishes "no embedder"
// from "the corpus matched nothing". A naive implementation omits the entry,
// and an answer layer then reads a missing signal as an empty one.
func TestSignalsReportWhatDidNotRun(t *testing.T) {
	store := &fakeStore{fullText: []DocumentHit{doc("asset:RA-1", search.EntityAsset, projMine, "MOF water stability", map[string]string{"version": "3"})}}
	r := newTestRetriever(t, store, nil)

	res, err := r.Retrieve(context.Background(), scopeIn(t, projMine), Request{Query: "mof"})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(res.Signals) != 4 {
		t.Fatalf("signals = %+v, want 4 reports (a missing signal is indistinguishable from an absent one)", res.Signals)
	}
	want := []struct {
		signal  string
		ran     bool
		skipped string
	}{
		{SignalFullText, true, ""},
		{SignalVector, false, SkippedNoEmbedder},
		{SignalFacets, false, SkippedNoFacets},
		{SignalGraph, false, SkippedNoSeeds},
	}
	for i, w := range want {
		got := res.Signals[i]
		if got.Signal != w.signal || got.Ran != w.ran || got.Skipped != w.skipped {
			t.Errorf("signals[%d] = %+v, want signal=%s ran=%v skipped=%q", i, got, w.signal, w.ran, w.skipped)
		}
	}
	if len(store.gotVector) != 0 || len(store.gotFacets) != 0 {
		t.Errorf("a signal that reported itself skipped was issued anyway: vector=%d facets=%d",
			len(store.gotVector), len(store.gotFacets))
	}
}

// TestVectorSignalCarriesTheEmbeddersProvenance: the query vector and the
// model identity must reach the store together and unaltered. A stored vector
// from another model is not a weak match, it is not a match, so the identity
// the read filters on has to be the identity of the embedder that produced
// THIS query vector — not a configured constant that could drift from it.
func TestVectorSignalCarriesTheEmbeddersProvenance(t *testing.T) {
	store := &fakeStore{}
	emb := &fakeEmbedder{model: testModel}
	r := newTestRetriever(t, store, emb)

	if _, err := r.Retrieve(context.Background(), scopeIn(t, projMine), Request{Query: "mof"}); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(store.gotVector) != 1 {
		t.Fatalf("vector reads = %d, want 1", len(store.gotVector))
	}
	got := store.gotVector[0]
	if got.Model != testModel {
		t.Errorf("model = %+v, want %+v", got.Model, testModel)
	}
	if got.Embedding == "" || !strings.HasPrefix(got.Embedding, "[") {
		t.Errorf("embedding = %q, want pgvector text input syntax", got.Embedding)
	}
	if len(emb.texts) != 1 || emb.texts[0] != "mof" {
		t.Errorf("embedded %q, want the question verbatim", emb.texts)
	}
}

// TestEmbeddingFailureFailsTheSearch: an embedder that errors must fail the
// request, not silently degrade to keyword-only. The two are different
// answers — "nothing matched" and "we could not look" — and a caller cannot
// tell them apart in a result list.
func TestEmbeddingFailureFailsTheSearch(t *testing.T) {
	store := &fakeStore{}
	r := newTestRetriever(t, store, &fakeEmbedder{model: testModel, err: errors.New("provider down")})

	res, err := r.Retrieve(context.Background(), scopeIn(t, projMine), Request{Query: "mof"})
	if !errors.Is(err, ErrEmbedding) {
		t.Fatalf("err = %v, want ErrEmbedding", err)
	}
	if len(res.Candidates) != 0 {
		t.Errorf("candidates = %v returned alongside an error; a partial candidate set is not an answer", refs(res.Candidates))
	}
}

// TestWrongWidthVectorRefused: a provider that is not the model the column
// was built for returns a vector of the wrong width. Refusing it here names
// the cause; letting it through makes the server raise a type error several
// layers down.
func TestWrongWidthVectorRefused(t *testing.T) {
	store := &fakeStore{}
	emb := &fakeEmbedder{model: testModel, vectors: [][]float32{make([]float32, embedding.Dimensions-1)}}
	r := newTestRetriever(t, store, emb)

	if _, err := r.Retrieve(context.Background(), scopeIn(t, projMine), Request{Query: "mof"}); !errors.Is(err, ErrEmbedding) {
		t.Fatalf("err = %v, want ErrEmbedding", err)
	}
	if len(store.gotVector) != 0 {
		t.Errorf("a wrong-width vector was sent to the store anyway: %q", store.gotVector[0].Embedding)
	}
}

// TestFacetsOnlyWhenAFilterIsGiven: the facet signal without a filter is "the
// first N rows of the index", which is not an answer to anything. With a
// filter it runs, and the text signals stand down when there is no question.
func TestFacetsOnlyWhenAFilterIsGiven(t *testing.T) {
	store := &fakeStore{facets: []DocumentHit{doc("asset:RA-2", search.EntityAsset, projMine, "CO2 uptake", map[string]string{"version": "1"})}}
	r := newTestRetriever(t, store, nil)

	res, err := r.Retrieve(context.Background(), scopeIn(t, projMine), Request{
		Query:  "",
		Facets: []byte(`{"object_type":"claim"}`),
	})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(store.gotFacets) != 1 {
		t.Fatalf("facet reads = %d, want 1", len(store.gotFacets))
	}
	if got := store.gotFacets[0].StructuredFilter; string(got) != `{"object_type":"claim"}` {
		t.Errorf("filter = %q, want the caller's filter verbatim", got)
	}
	if len(store.gotFullText) != 0 {
		t.Errorf("the full-text signal ran with an empty query: %+v", store.gotFullText)
	}
	if got := reportFor(t, res, SignalFullText).Skipped; got == "" {
		t.Errorf("the full-text signal is not reported as skipped: %+v", res.Signals)
	}
	if got := reportFor(t, res, SignalFacets); !got.Ran || got.Hits != 1 {
		t.Errorf("facets report = %+v, want ran with 1 hit", got)
	}
}

// TestFusionPrefersAgreementOverOneConfidentSignal is the property RRF exists
// for: a candidate two signals both placed second outranks candidates a
// single signal placed first. Anything that summed the signals' own scores
// could not promise this, because ts_rank and cosine similarity are not
// commensurable.
func TestFusionPrefersAgreementOverOneConfidentSignal(t *testing.T) {
	agree := doc("knowledge:KP-AGREE", search.EntityKnowledge, projMine, "agreed", map[string]string{"public_version": "1"})
	textTop := doc("asset:RA-TEXT", search.EntityAsset, projMine, "text top", map[string]string{"version": "1"})
	vectorTop := doc("asset:RA-VEC", search.EntityAsset, projMine, "vector top", map[string]string{"version": "1"})

	store := &fakeStore{
		fullText: []DocumentHit{textTop, agree},
		vector:   []DocumentHit{vectorTop, agree},
	}
	r := newTestRetriever(t, store, &fakeEmbedder{model: testModel})

	res, err := r.Retrieve(context.Background(), scopeIn(t, projMine), Request{Query: "mof"})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if got := refs(res.Candidates); len(got) != 3 || got[0] != "knowledge:KP-AGREE@1" {
		t.Fatalf("candidates = %v, want the two-signal candidate first", got)
	}
	// The two singletons tie exactly (1/61 each) and neither has more
	// signals, so the ref breaks it — and that tie-break has to be total, or
	// the same query returns a different order run to run.
	if got := refs(res.Candidates); got[1] != "asset:RA-TEXT@1" || got[2] != "asset:RA-VEC@1" {
		t.Errorf("candidates = %v, want the tied pair ordered by ref", got)
	}
	best := res.Candidates[0]
	if len(best.Signals) != 2 {
		t.Fatalf("signals on the best candidate = %+v, want both", best.Signals)
	}
	if best.Signals[0].Signal != SignalFullText || best.Signals[0].Rank != 2 {
		t.Errorf("signals = %+v, want the full-text placement first", best.Signals)
	}
}

// TestSameResultEveryRun: the pipeline's order must not depend on map
// iteration or on the order a store happens to return equal rows in. T0905's
// ranking is measured against these candidates, and an answer's
// reproducibility claim rests on this.
func TestSameResultEveryRun(t *testing.T) {
	store := &fakeStore{
		fullText: []DocumentHit{
			doc("asset:RA-B", search.EntityAsset, projMine, "b", map[string]string{"version": "1"}),
			doc("asset:RA-A", search.EntityAsset, projMine, "a", map[string]string{"version": "1"}),
		},
		facets: []DocumentHit{
			doc("asset:RA-C", search.EntityAsset, projMine, "c", map[string]string{"version": "1"}),
			doc("asset:RA-A", search.EntityAsset, projMine, "a", map[string]string{"version": "1"}),
		},
	}
	r := newTestRetriever(t, store, nil)
	req := Request{Query: "mof", Facets: []byte(`{"entity_type":"asset"}`)}

	first, err := r.Retrieve(context.Background(), scopeIn(t, projMine), req)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := r.Retrieve(context.Background(), scopeIn(t, projMine), req)
		if err != nil {
			t.Fatalf("Retrieve: %v", err)
		}
		if fmt.Sprint(refs(again.Candidates)) != fmt.Sprint(refs(first.Candidates)) {
			t.Fatalf("run %d = %v, first run = %v", i, refs(again.Candidates), refs(first.Candidates))
		}
	}
}

// TestCandidatesAreVersionPinned: every candidate names a version, which is
// what ADR-010 needs from retrieval — the answer may cite only a
// platform-determined version of an entity.
func TestCandidatesAreVersionPinned(t *testing.T) {
	store := &fakeStore{fullText: []DocumentHit{
		doc("knowledge:KP-1", search.EntityKnowledge, projMine, "a claim", map[string]string{"public_version": "2", "object_type": "claim"}),
		doc("asset:RA-1", search.EntityAsset, projMine, "an asset", map[string]string{"version": "7"}),
		doc("release:3f8a1c62-9b4d-4f1e-8a77-0c2d5e6f7a80", search.EntityRelease, projMine, "a release", map[string]string{"version": "1.2.0"}),
	}}
	r := newTestRetriever(t, store, nil)

	res, err := r.Retrieve(context.Background(), scopeIn(t, projMine), Request{Query: "mof"})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(res.Candidates) != 3 {
		t.Fatalf("candidates = %v, want 3", refs(res.Candidates))
	}
	for _, c := range res.Candidates {
		if !c.Pinned() {
			t.Errorf("candidate %q is not version-pinned", c.Ref)
		}
	}
	if got := res.Candidates[0].ObjectType; got != "claim" {
		t.Errorf("object_type = %q, want the facet the projection wrote", got)
	}
}

// TestStateIsAVersionlessEntityThatStillPins: a project state has no version
// label, and the candidate says so by rendering without a "@" rather than by
// inventing one. Its row id is the pin (the content is commit-fixed).
func TestStateIsAVersionlessEntityThatStillPins(t *testing.T) {
	const stateID = "5c1d0f2e-1111-4222-8333-444455556666"
	store := &fakeStore{fullText: []DocumentHit{
		doc(search.EntityRef(search.EntityState, stateID), search.EntityState, projMine, "state", nil),
	}}
	r := newTestRetriever(t, store, nil)

	res, err := r.Retrieve(context.Background(), scopeIn(t, projMine), Request{Query: "mof"})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	c := res.Candidates[0]
	if c.Ref != search.EntityRef(search.EntityState, stateID) {
		t.Errorf("ref = %q, want no invented version suffix", c.Ref)
	}
	if c.Version != "" {
		t.Errorf("version = %q, want empty", c.Version)
	}
	if !c.Pinned() {
		t.Error("a state candidate reports itself unpinned; its row id is the pin")
	}
}

// TestGraphExpansionPinsTheReachedNode: the traversal's output is a version
// (an object and its ordinal) and a hop that says where it came from, and the
// document it expanded from is enriched with the version it pins rather than
// duplicated beside it.
func TestGraphExpansionPinsTheReachedNode(t *testing.T) {
	const (
		claimPID = "CLM-DEMO-001"
		claimOV  = "aaaa1111-1111-4111-8111-111111111111"
		matOV    = "bbbb2222-2222-4222-8222-222222222222"
		claimOID = "cccc3333-3333-4333-8333-333333333333"
		matOID   = "dddd4444-4444-4444-8444-444444444444"
	)
	store := &fakeStore{
		fullText: []DocumentHit{
			doc(search.EntityRef(search.EntityKnowledge, claimPID), search.EntityKnowledge, projMine,
				"A claim about MOF stability", map[string]string{"public_version": "2", "object_type": "claim"}),
		},
		seeds: map[string]SeedVersion{
			claimPID: {Pid: claimPID, GraphObject: GraphObject{
				ObjectVersionID: claimOV, ObjectID: claimOID, VersionNo: 2, ObjectType: "claim", ProjectID: projMine,
			}},
		},
		edges: []GraphEdge{{
			RelationType:    "about_material",
			SourceVersionID: claimOV,
			TargetVersionID: matOV,
			SourceProjectID: projMine,
			TargetProjectID: projMine,
		}},
		nodes: map[string]GraphObject{
			matOV: {ObjectVersionID: matOV, ObjectID: matOID, VersionNo: 3, Title: "ZIF-8", ObjectType: "material", ProjectID: projMine},
		},
	}
	r := newTestRetriever(t, store, nil)

	res, err := r.Retrieve(context.Background(), scopeIn(t, projMine), Request{Query: "mof stability"})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(res.Candidates) != 2 {
		t.Fatalf("candidates = %v, want the document and the material it reaches", refs(res.Candidates))
	}

	var document, material *Candidate
	for i := range res.Candidates {
		switch res.Candidates[i].Kind {
		case KindDocument:
			document = &res.Candidates[i]
		case KindObjectVersion:
			material = &res.Candidates[i]
		}
	}
	if document == nil || material == nil {
		t.Fatalf("candidates = %v, want one of each kind", refs(res.Candidates))
	}
	if document.ObjectVersionID != claimOV || document.ObjectID != claimOID || document.VersionNo != 2 {
		t.Errorf("the document was not pinned to the version its publication published: %+v", document)
	}
	if material.Ref != search.EntityRef(RefKindObjectVersion, matOID)+"@3" {
		t.Errorf("material ref = %q, want object@version", material.Ref)
	}
	if material.ObjectType != "material" || !material.Pinned() {
		t.Errorf("material = %+v, want a pinned material", material)
	}
	if len(material.Hops) != 1 {
		t.Fatalf("hops = %+v, want one", material.Hops)
	}
	hop := material.Hops[0]
	wantHop := Hop{RelationType: "about_material", Direction: DirectionOut, FromRef: document.Ref, Depth: 1}
	if hop != wantHop {
		t.Errorf("hop = %+v, want %+v", hop, wantHop)
	}
	if got := reportFor(t, res, SignalGraph); !got.Ran || got.Hits != 1 {
		t.Errorf("graph report = %+v, want ran with 1 hit", got)
	}
}

// TestGraphDoesNotDuplicateADocument: a node the traversal reaches that was
// ALSO recalled as a document is one thing. It must appear once — with both
// of its reasons — because two candidates with different refs for one entity
// would let an answer cite it twice and make the candidate count a lie.
func TestGraphDoesNotDuplicateADocument(t *testing.T) {
	const (
		claimPID = "CLM-DEMO-002"
		matPID   = "MAT-DEMO-X"
		claimOV  = "aaaa1111-1111-4111-8111-111111111111"
		matOV    = "bbbb2222-2222-4222-8222-222222222222"
		matOID   = "dddd4444-4444-4444-8444-444444444444"
	)
	store := &fakeStore{
		fullText: []DocumentHit{
			doc(search.EntityRef(search.EntityKnowledge, claimPID), search.EntityKnowledge, projMine,
				"claim", map[string]string{"public_version": "1", "object_type": "claim"}),
			doc(search.EntityRef(search.EntityKnowledge, matPID), search.EntityKnowledge, projMine,
				"ZIF-8", map[string]string{"public_version": "3", "object_type": "material"}),
		},
		seeds: map[string]SeedVersion{
			claimPID: {Pid: claimPID, GraphObject: GraphObject{ObjectVersionID: claimOV, ObjectID: "cccc3333-3333-4333-8333-333333333333", VersionNo: 1, ProjectID: projMine}},
			matPID:   {Pid: matPID, GraphObject: GraphObject{ObjectVersionID: matOV, ObjectID: matOID, VersionNo: 3, ProjectID: projMine}},
		},
		edges: []GraphEdge{{
			RelationType:    "about_material",
			SourceVersionID: claimOV,
			TargetVersionID: matOV,
			SourceProjectID: projMine,
			TargetProjectID: projMine,
		}},
		nodes: map[string]GraphObject{
			matOV: {ObjectVersionID: matOV, ObjectID: matOID, VersionNo: 3, Title: "ZIF-8", ObjectType: "material", ProjectID: projMine},
		},
	}
	r := newTestRetriever(t, store, nil)

	res, err := r.Retrieve(context.Background(), scopeIn(t, projMine), Request{Query: "zif-8"})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(res.Candidates) != 2 {
		t.Fatalf("candidates = %v, want the two documents once each", refs(res.Candidates))
	}
	var material Candidate
	for _, c := range res.Candidates {
		if strings.HasPrefix(c.Ref, search.EntityRef(search.EntityKnowledge, matPID)) {
			material = c
		}
	}
	if material.Ref == "" {
		t.Fatalf("the material document is gone: %v", refs(res.Candidates))
	}
	if !strings.HasPrefix(material.Ref, "knowledge:MAT-DEMO-X") {
		t.Errorf("the material was re-addressed as %q; a document keeps the identity it was recalled by", material.Ref)
	}
	if len(material.Hops) != 1 || material.Hops[0].RelationType != "about_material" {
		t.Errorf("hops = %+v, want the hop recorded on the document", material.Hops)
	}
	if len(material.Signals) != 2 {
		t.Errorf("signals = %+v, want both the text recall and the traversal", material.Signals)
	}
}

// TestGraphTerminatesOnACycle: the relation graph is full of cycles
// (supports/contradicts run both ways between the same two claims), and a
// traversal that did not remember where it had been would re-emit nodes until
// the depth ran out, or forever.
func TestGraphTerminatesOnACycle(t *testing.T) {
	const (
		pidA = "CLM-A"
		ovA  = "aaaa1111-1111-4111-8111-111111111111"
		ovB  = "bbbb2222-2222-4222-8222-222222222222"
		oidB = "dddd4444-4444-4444-8444-444444444444"
	)
	store := &fakeStore{
		fullText: []DocumentHit{
			doc(search.EntityRef(search.EntityKnowledge, pidA), search.EntityKnowledge, projMine,
				"a", map[string]string{"public_version": "1", "object_type": "claim"}),
		},
		seeds: map[string]SeedVersion{
			pidA: {Pid: pidA, GraphObject: GraphObject{ObjectVersionID: ovA, ObjectID: "cccc3333-3333-4333-8333-333333333333", VersionNo: 1, ProjectID: projMine}},
		},
		// Both directions of the same relation: a cycle through the seed.
		edges: []GraphEdge{
			{RelationType: "contradicts", SourceVersionID: ovA, TargetVersionID: ovB, SourceProjectID: projMine, TargetProjectID: projMine},
			{RelationType: "contradicts", SourceVersionID: ovB, TargetVersionID: ovA, SourceProjectID: projMine, TargetProjectID: projMine},
		},
		nodes: map[string]GraphObject{
			ovB: {ObjectVersionID: ovB, ObjectID: oidB, VersionNo: 1, Title: "b", ObjectType: "claim", ProjectID: projMine},
		},
	}
	r := newTestRetriever(t, store, nil)

	res, err := r.Retrieve(context.Background(), scopeIn(t, projMine), Request{Query: "a", Limits: Limits{Depth: 2}})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(res.Candidates) != 2 {
		t.Fatalf("candidates = %v, want the document and the contradicted claim once", refs(res.Candidates))
	}
	// The walk must not re-emit: one node read, in one level, for a graph
	// that comes straight back to where it started.
	if len(store.gotNodes) != 1 || len(store.gotNodes[0]) != 1 {
		t.Errorf("node reads = %v, want one level of one new node", store.gotNodes)
	}
	for _, c := range res.Candidates {
		seenHops := map[Hop]bool{}
		for _, h := range c.Hops {
			if seenHops[h] {
				t.Errorf("candidate %q recorded the hop %+v twice", c.Ref, h)
			}
			seenHops[h] = true
		}
	}
	// The seed DOES carry hops — the two directions of the relation are two
	// relation versions, and both genuinely connect it to the other claim —
	// and they are distinguishable, which is what keeps the list a set.
	var document Candidate
	for _, c := range res.Candidates {
		if c.Kind == KindDocument {
			document = c
		}
	}
	if len(document.Hops) != 2 {
		t.Fatalf("the seed carries %d hops (%+v), want the two directions of the cycle", len(document.Hops), document.Hops)
	}
	if document.Hops[0].Direction == document.Hops[1].Direction {
		t.Errorf("hops = %+v, want one in each direction", document.Hops)
	}
}

// TestDepthBoundsTheWalk: the limit is a real bound, not decoration — one
// hop reaches a seed's neighbours and stops, and the next hop is only walked
// when the caller asked for it. A retrieval that ignored the bound would
// return the seed's whole neighbourhood, which is what "depth" is for.
func TestDepthBoundsTheWalk(t *testing.T) {
	const (
		pidA = "CLM-A"
		ovA  = "aaaa1111-1111-4111-8111-111111111111"
		ovB  = "bbbb2222-2222-4222-8222-222222222222"
		ovC  = "cccc3333-3333-4333-8333-333333333333"
		oidB = "dddd4444-4444-4444-8444-444444444444"
		oidC = "eeee5555-5555-4555-8555-555555555555"
	)
	base := func() *fakeStore {
		return &fakeStore{
			fullText: []DocumentHit{doc(search.EntityRef(search.EntityKnowledge, pidA), search.EntityKnowledge, projMine,
				"a", map[string]string{"public_version": "1", "object_type": "claim"})},
			seeds: map[string]SeedVersion{
				pidA: {Pid: pidA, GraphObject: GraphObject{ObjectVersionID: ovA, ObjectID: "ffff6666-6666-4666-8666-666666666666", VersionNo: 1, ProjectID: projMine}},
			},
			edges: []GraphEdge{
				{RelationType: "about_material", SourceVersionID: ovA, TargetVersionID: ovB, SourceProjectID: projMine, TargetProjectID: projMine},
				{RelationType: "supports", SourceVersionID: ovB, TargetVersionID: ovC, SourceProjectID: projMine, TargetProjectID: projMine},
			},
			nodes: map[string]GraphObject{
				ovB: {ObjectVersionID: ovB, ObjectID: oidB, VersionNo: 1, Title: "b", ObjectType: "material", ProjectID: projMine},
				ovC: {ObjectVersionID: ovC, ObjectID: oidC, VersionNo: 1, Title: "c", ObjectType: "finding", ProjectID: projMine},
			},
		}
	}

	one := base()
	r := newTestRetriever(t, one, nil)
	res, err := r.Retrieve(context.Background(), scopeIn(t, projMine), Request{Query: "a", Limits: Limits{Depth: 1}})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(res.Candidates) != 2 {
		t.Fatalf("depth 1 candidates = %v, want the document and one neighbour", refs(res.Candidates))
	}

	two := base()
	r = newTestRetriever(t, two, nil)
	res, err = r.Retrieve(context.Background(), scopeIn(t, projMine), Request{Query: "a", Limits: Limits{Depth: 2}})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(res.Candidates) != 3 {
		t.Fatalf("depth 2 candidates = %v, want the two-hop node as well", refs(res.Candidates))
	}
	deeper := res.Candidates[2]
	if deeper.ObjectType != "finding" || len(deeper.Hops) != 1 || deeper.Hops[0].Depth != 2 {
		t.Errorf("two-hop candidate = %+v, want a finding reached at depth 2", deeper)
	}
	if deeper.Hops[0].FromRef == res.Candidates[0].Ref {
		t.Errorf("the depth-2 hop claims to start at the seed (%q); it starts at the depth-1 node", deeper.Hops[0].FromRef)
	}
}

// TestSeedInAnotherProjectIsRefused: a publication pid is a public identity,
// and the version behind it is only a traversal origin if it belongs to the
// project the recalled document does. The traversal must not start from a row
// the searcher's scope does not cover — even if a store returned one.
func TestSeedInAnotherProjectIsRefused(t *testing.T) {
	store := &fakeStore{
		fullText: []DocumentHit{doc("knowledge:KP-PUBLIC", search.EntityKnowledge, "",
			"a public claim", map[string]string{"public_version": "1", "object_type": "claim"})},
		seeds: map[string]SeedVersion{
			"KP-PUBLIC": {Pid: "KP-PUBLIC", GraphObject: GraphObject{
				ObjectVersionID: "aaaa1111-1111-4111-8111-111111111111", ProjectID: projOther,
			}},
		},
	}
	r := newTestRetriever(t, store, nil)

	res, err := r.Retrieve(context.Background(), scopeIn(t, projMine), Request{Query: "claim"})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(store.gotEdges) != 0 {
		t.Errorf("the traversal started from another project's version: %+v", store.gotEdges)
	}
	if got := reportFor(t, res, SignalGraph); got.Ran || got.Skipped != SkippedNoSeeds {
		t.Errorf("graph report = %+v, want skipped for want of seeds", got)
	}
}

// TestStoreFailureFailsTheSearch: a read that failed is an error, never an
// empty result. "We could not read the corpus" and "the corpus has nothing"
// are different answers and a searcher cannot tell them apart in a list.
func TestStoreFailureFailsTheSearch(t *testing.T) {
	for name, store := range map[string]*fakeStore{
		"full text": {errFullText: errors.New("boom")},
		"vector":    {errVector: errors.New("boom")},
		"facets":    {errFacets: errors.New("boom")},
		"edges": {
			fullText: []DocumentHit{doc("knowledge:KP-1", search.EntityKnowledge, projMine, "a", map[string]string{"public_version": "1"})},
			seeds:    map[string]SeedVersion{"KP-1": {Pid: "KP-1", GraphObject: GraphObject{ObjectVersionID: "aaaa1111-1111-4111-8111-111111111111", ProjectID: projMine}}},
			errEdges: errors.New("boom"),
		},
		"nodes": {
			fullText: []DocumentHit{doc("knowledge:KP-1", search.EntityKnowledge, projMine, "a", map[string]string{"public_version": "1"})},
			seeds:    map[string]SeedVersion{"KP-1": {Pid: "KP-1", GraphObject: GraphObject{ObjectVersionID: "aaaa1111-1111-4111-8111-111111111111", ProjectID: projMine}}},
			edges: []GraphEdge{{
				RelationType: "about_material", SourceVersionID: "aaaa1111-1111-4111-8111-111111111111",
				TargetVersionID: "bbbb2222-2222-4222-8222-222222222222",
				SourceProjectID: projMine, TargetProjectID: projMine,
			}},
			errNodes: errors.New("boom"),
		},
		"seeds": {
			fullText: []DocumentHit{doc("knowledge:KP-1", search.EntityKnowledge, projMine, "a", map[string]string{"public_version": "1"})},
			errSeeds: errors.New("boom"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newTestRetriever(t, store, &fakeEmbedder{model: testModel})
			req := Request{Query: "a", Facets: []byte(`{"entity_type":"knowledge"}`)}
			res, err := r.Retrieve(context.Background(), scopeIn(t, projMine), req)
			if !errors.Is(err, ErrStore) {
				t.Fatalf("err = %v, want ErrStore", err)
			}
			if len(res.Candidates) != 0 {
				t.Errorf("candidates = %v returned alongside an error", refs(res.Candidates))
			}
		})
	}
}

// TestRefusesRequestsItCannotHonour: a request the pipeline cannot answer is
// refused before any read, and the refusal names the reason. Passing it to
// SQL would turn "the plan named a kind that does not exist" into "nothing
// matched", which hides the defect where it happened.
func TestRefusesRequestsItCannotHonour(t *testing.T) {
	long := strings.Repeat("x", 2001)
	cases := []struct {
		name string
		req  Request
		want error
	}{
		{"no query and no filter", Request{}, ErrNoQuery},
		{"blank query", Request{Query: "   "}, ErrNoQuery},
		{"unknown entity type", Request{Query: "a", EntityTypes: []string{"compound"}}, ErrUnknownEntityType},
		{"filter is not an object", Request{Query: "a", Facets: []byte(`["claim"]`)}, ErrBadFilter},
		{"filter is null", Request{Query: "a", Facets: []byte(`null`)}, ErrBadFilter},
		{"filter is not json", Request{Query: "a", Facets: []byte(`{`)}, ErrBadFilter},
		{"query too long", Request{Query: long}, ErrTooLong},
		{"depth above the cap", Request{Query: "a", Limits: Limits{Depth: MaxDepth + 1}}, ErrTooLong},
		{"negative depth", Request{Query: "a", Limits: Limits{Depth: -1}}, ErrTooLong},
		{"page size above the cap", Request{Query: "a", Limits: Limits{PerSignal: MaxPerSignal + 1}}, ErrTooLong},
		{"too many candidates", Request{Query: "a", Limits: Limits{MaxCandidates: MaxCandidates + 1}}, ErrTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{}
			r := newTestRetriever(t, store, nil)
			if _, err := r.Retrieve(context.Background(), scopeIn(t, projMine), tc.req); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if len(store.gotFullText) != 0 {
				t.Errorf("a refused request was issued to the store: %+v", store.gotFullText)
			}
		})
	}
}

// TestEntityTypesAreANarrowingSet: the types the plan named reach the store
// as a set, deduplicated and sorted, so the SQL parameter does not depend on
// the plan's argument order.
func TestEntityTypesAreANarrowingSet(t *testing.T) {
	store := &fakeStore{}
	r := newTestRetriever(t, store, nil)

	_, err := r.Retrieve(context.Background(), scopeIn(t, projMine), Request{
		Query:       "mof",
		EntityTypes: []string{search.EntityKnowledge, search.EntityAsset, search.EntityKnowledge},
	})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	got := store.gotFullText[0].EntityTypes
	if fmt.Sprint(got) != fmt.Sprint([]string{search.EntityAsset, search.EntityKnowledge}) {
		t.Errorf("entity types = %v, want the deduplicated set in one order", got)
	}
}

// TestMaxCandidatesCuts: the candidate budget is honoured from the head of
// the fused order — the best-fused candidates are the ones that survive.
func TestMaxCandidatesCuts(t *testing.T) {
	rows := make([]DocumentHit, 0, 10)
	for i := 0; i < 10; i++ {
		ref := fmt.Sprintf("asset:RA-%02d", i)
		rows = append(rows, doc(ref, search.EntityAsset, projMine, ref, map[string]string{"version": "1"}))
	}
	store := &fakeStore{fullText: rows}
	r := newTestRetriever(t, store, nil)

	all, err := r.Retrieve(context.Background(), scopeIn(t, projMine), Request{Query: "mof"})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	cut, err := r.Retrieve(context.Background(), scopeIn(t, projMine), Request{Query: "mof", Limits: Limits{MaxCandidates: 3}})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(cut.Candidates) != 3 {
		t.Fatalf("candidates = %v, want 3", refs(cut.Candidates))
	}
	for i, c := range cut.Candidates {
		if c.Ref != all.Candidates[i].Ref {
			t.Errorf("cut[%d] = %q, want the head of the full order (%q)", i, c.Ref, all.Candidates[i].Ref)
		}
	}
}

// TestRequestFromPlanOnlyNarrows is the property docs/54 #7 needs: a plan can
// ask for less and can never ask for more. The scope is not a parameter of
// the translation at all, so there is no shape of plan — including one the
// model was talked into — that widens what the caller may read.
func TestRequestFromPlanOnlyNarrows(t *testing.T) {
	fallback := planner.Plan{Status: planner.StatusFallback, Reason: planner.ReasonTimeout, Query: "is ZIF-8 stable in water?"}
	got := RequestFromPlan(fallback, nil, Limits{})
	if got.Query != fallback.Query || len(got.EntityTypes) != 0 || got.PublicOnly || len(got.Facets) != 0 {
		t.Errorf("a fallback plan produced %+v, want the question alone", got)
	}

	planned := planner.Plan{
		Status: planner.StatusPlanned,
		Query:  "which claims support ZIF-8 stability?",
		Document: &planner.Document{
			PlanVersion:  planner.PlanVersion,
			Intent:       planner.IntentAnswer,
			TargetObject: planner.TargetObject{EntityTypes: []string{search.EntityKnowledge}},
			NetworkScope: planner.NetworkScopePlatform,
			Visibility:   planner.VisibilityPublic,
		},
	}
	got = RequestFromPlan(planned, []byte(`{"object_type":"claim"}`), Limits{PerSignal: 5})
	if fmt.Sprint(got.EntityTypes) != fmt.Sprint([]string{search.EntityKnowledge}) {
		t.Errorf("entity types = %v, want the plan's target object", got.EntityTypes)
	}
	if !got.PublicOnly {
		t.Error("public_only = false, want the plan's public visibility to narrow the read")
	}
	if string(got.Facets) != `{"object_type":"claim"}` {
		t.Errorf("facets = %q, want the caller's filter passed through", got.Facets)
	}
	if got.Limits.PerSignal != 5 {
		t.Errorf("limits = %+v, want the caller's", got.Limits)
	}

	// VisibilityAccessible is the absence of a narrowing hint, not a grant:
	// a plan that asks for "accessible" gets exactly what the scope allows.
	planned.Document.Visibility = planner.VisibilityAccessible
	if RequestFromPlan(planned, nil, Limits{}).PublicOnly {
		t.Error("public_only = true for VisibilityAccessible; the flag can only ever remove rows, so it must be false here")
	}
}

// TestNewRetrieverRefusesAStoreItCannotRead: a retriever without a store is a
// panic waiting for the first search.
func TestNewRetrieverRefusesAStoreItCannotRead(t *testing.T) {
	if _, err := NewRetriever(nil, nil); err == nil {
		t.Fatal("NewRetriever(nil) = nil error, want a refusal")
	}
}

// TestStoreRefusesAnUnresolvedScope: the store is the second copy of the
// refusal, on purpose. The real queries express the scope only as an array
// parameter, and their predicate returns the public rows when that array is
// empty — so a store that accepted an unresolved scope would read the public
// corpus and call it a success.
func TestStoreRefusesAnUnresolvedScope(t *testing.T) {
	store, err := NewSQLStore(nil)
	if err == nil {
		t.Fatal("NewSQLStore(nil) = nil error, want a refusal")
	}
	if store != nil {
		t.Errorf("store = %+v, want nil", store)
	}
	if err := checkScope(search.Scope{}); !errors.Is(err, ErrNoScope) {
		t.Errorf("checkScope(zero) = %v, want ErrNoScope", err)
	}
	if err := checkScope(scopeIn(t, projMine)); err != nil {
		t.Errorf("checkScope(resolved) = %v, want nil", err)
	}
}

// TestUuidConversionRoundTrips: the scope's project list and a node's version
// id cross the boundary as uuid text, and the text form has to be the
// canonical lowercase one — postgres renders ::text that way, and a mismatch
// would make the seed-to-document project comparison always fire.
func TestUuidConversionRoundTrips(t *testing.T) {
	const text = "3f8a1c62-9b4d-4f1e-8a77-0c2d5e6f7a80"
	list, err := uuidList([]string{text})
	if err != nil {
		t.Fatalf("uuidList: %v", err)
	}
	if got := uuidText(list[0]); got != text {
		t.Errorf("round trip = %q, want %q", got, text)
	}
	if got := uuidText(pgtype.UUID{}); got != "" {
		t.Errorf("a NULL uuid rendered as %q, want the empty string", got)
	}
	if _, err := uuidList([]string{"not-a-uuid"}); err == nil {
		t.Error("uuidList(not-a-uuid) = nil error, want a refusal: a dropped id silently shrinks the traversal")
	}
}

// TestLimitsDefaultsAreBounded: the zero Limits is not "no limits" — a caller
// that forgot to set them gets a bounded search, not an unbounded one — and
// zero means the default uniformly, so there is no field whose zero value
// silently means something else.
func TestLimitsDefaultsAreBounded(t *testing.T) {
	got, err := Limits{}.normalize()
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if got.PerSignal != DefaultPerSignal || got.MaxCandidates != DefaultMaxCandidates {
		t.Errorf("defaults = %+v, want %d/%d", got, DefaultPerSignal, DefaultMaxCandidates)
	}
	if got.Depth != DefaultDepth || got.Seeds != DefaultSeeds {
		t.Errorf("defaults = %+v, want depth %d and seeds %d", got, DefaultDepth, DefaultSeeds)
	}
	if DefaultDepth > MaxDepth || DefaultSeeds > MaxSeeds {
		t.Errorf("a default exceeds its cap: depth %d/%d seeds %d/%d",
			DefaultDepth, MaxDepth, DefaultSeeds, MaxSeeds)
	}
	// Every default must survive its own range check, or the zero Limits is
	// a request the pipeline refuses.
	if _, err := (Limits{}).normalize(); err != nil {
		t.Errorf("the zero Limits is refused: %v", err)
	}
}
