// The search API's unit tests: the pipeline's order, what it records, and
// which failures it answers the caller for.
//
// They are white-box on purpose, like the other transports' tests: the route
// is guard-protected in production, so a handler cannot be driven past the
// guard from outside the package, and the pipeline itself takes a scope
// (search.Scope, constructible only by search.ResolveScope) that a test
// outside the package cannot build without a database. What needs a real
// session or a real table is covered by tests/integration.
//
// The fakes below are scripted, and the answers one of them returns are REAL
// where it matters: TestSearchRecordsWhatTheAnswerLayerSays runs the actual
// answer generator (internal/search/answer) over a scripted provider, so the
// refusal of a hallucinated citation is exercised through this transport
// rather than simulated here.
package searchhttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/search"
	"github.com/lichman0405/post/internal/search/answer"
	"github.com/lichman0405/post/internal/search/planner"
	"github.com/lichman0405/post/internal/search/ranking"
	"github.com/lichman0405/post/internal/search/retrieval"
)

const (
	actorID    = "6a1f0d3e-1c2b-4a5d-8e9f-0a1b2c3d4e5f"
	assetRef   = "asset:AST-0001@2"
	knownRef   = "knowledge:KNW-0007@1"
	projectID  = "3f2a5b7c-1d4e-4a6b-8c9d-0e1f2a3b4c5d"
	objectUUID = "1d2c3b4a-5e6f-4071-8293-a4b5c6d7e8f9"
)

// ---------------------------------------------------------------------------
// Fakes

type fakeScope struct {
	err   error
	gotID string
	calls int
}

func (f *fakeScope) ListProjectsForUser(_ context.Context, userID string) ([]domain.Project, error) {
	f.calls++
	f.gotID = userID
	if f.err != nil {
		return nil, f.err
	}
	return []domain.Project{{ID: projectID, Slug: "p", Name: "P", Visibility: domain.VisibilityPrivate}}, nil
}

type fakePlanner struct {
	plan planner.Plan
	err  error
	got  planner.Request
}

func (f *fakePlanner) Plan(_ context.Context, _ search.Scope, req planner.Request) (planner.Plan, error) {
	f.got = req
	if f.err != nil {
		return planner.Plan{}, f.err
	}
	return f.plan, nil
}

type fakeRetriever struct {
	result retrieval.Result
	err    error
	got    retrieval.Request
	calls  int
}

func (f *fakeRetriever) Retrieve(_ context.Context, _ search.Scope, req retrieval.Request) (retrieval.Result, error) {
	f.calls++
	f.got = req
	if f.err != nil {
		return retrieval.Result{}, f.err
	}
	return f.result, nil
}

type fakeRanker struct {
	result ranking.Result
	err    error
	got    retrieval.Result
	calls  int
}

func (f *fakeRanker) Rank(_ context.Context, _ search.Scope, _ retrieval.Request, res retrieval.Result) (ranking.Result, error) {
	f.calls++
	f.got = res
	if f.err != nil {
		return ranking.Result{}, f.err
	}
	return f.result, nil
}

type fakeAnswerer struct {
	ans   answer.Answer
	err   error
	got   answer.Input
	calls int
}

func (f *fakeAnswerer) Answer(_ context.Context, in answer.Input) (answer.Answer, error) {
	f.calls++
	f.got = in
	if f.err != nil {
		return answer.Answer{}, f.err
	}
	return f.ans, nil
}

type fakeRecords struct {
	id  string
	err error
	got persistence.SearchRecord
}

func (f *fakeRecords) Save(_ context.Context, rec persistence.SearchRecord) (string, error) {
	f.got = rec
	if f.err != nil {
		return "", f.err
	}
	return f.id, nil
}

// wireError is the platform's error envelope (internal: cmd/api/authhttp,
// docs/22 §5) as a test sees it.
type wireError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// ---------------------------------------------------------------------------
// Fixtures

// rankedAsset is one ranked candidate: an asset whose version is pinned, with
// the six factors the answer layer reads.
func rankedAsset() ranking.Ranked {
	return ranking.Ranked{
		Rank: 1, Ref: assetRef, Kind: retrieval.KindDocument, EntityType: "asset",
		Title: "Mg-MOF-74 CO2 uptake", Version: "2",
		Factors: cleanFactors(),
		Candidate: retrieval.Candidate{
			Ref: assetRef, Kind: retrieval.KindDocument, EntityType: "asset",
			Identity: "AST-0001", Version: "2", Title: "Mg-MOF-74 CO2 uptake",
			Visibility: "public", ProjectID: projectID, ObjectType: "asset",
		},
	}
}

// cleanFactors is a source that matches none of the answer layer's limitation
// rules, so a fixture's limitations are the fallback cause and nothing else.
func cleanFactors() []ranking.Assessment {
	return []ranking.Assessment{
		{Factor: ranking.FactorQueryScopeMatch, Level: ranking.LevelQuestion, LevelRank: 1, Reason: "recalled by the question"},
		{Factor: ranking.FactorEvidence, Level: ranking.LevelAsserted, LevelRank: 2, Reason: "1 evidence assertion is recorded"},
		{Factor: ranking.FactorReview, Level: ranking.LevelReviewed, LevelRank: 2, Reason: "the state is reviewed"},
		{Factor: ranking.FactorReproduction, Level: ranking.LevelIndependent, LevelRank: 1, Reason: "1 reproduction from another project"},
		{Factor: ranking.FactorConflict, Level: ranking.LevelUncontested, LevelRank: 1, Reason: "nothing contradicts this version"},
		{Factor: ranking.FactorVersion, Level: ranking.LevelCurrent, LevelRank: 1, Reason: "this is the object's newest version"},
	}
}

// signalsFor is the retrieval report of a deployment with no embedder.
func signalsFor() []retrieval.SignalReport {
	return []retrieval.SignalReport{
		{Signal: retrieval.SignalFullText, Ran: true, Hits: 1},
		{Signal: retrieval.SignalVector, Ran: false, Skipped: retrieval.SkippedNoEmbedder},
		{Signal: retrieval.SignalFacets, Ran: false, Skipped: retrieval.SkippedNoFacets},
		{Signal: retrieval.SignalGraph, Ran: true, Hits: 0},
	}
}

// newPipeline wires a service over the fakes, with the REAL answer generator
// unless the caller passes its own.
func newPipeline(t *testing.T, deps Deps) *service {
	t.Helper()
	if deps.Scope == nil {
		deps.Scope = &fakeScope{}
	}
	if deps.Retriever == nil {
		deps.Retriever = &fakeRetriever{result: retrieval.Result{Query: "q", Signals: signalsFor()}}
	}
	if deps.Ranker == nil {
		deps.Ranker = &fakeRanker{result: ranking.Result{Query: "q", Factors: ranking.Factors(), Ranked: []ranking.Ranked{rankedAsset()}}}
	}
	if deps.Answerer == nil {
		a, err := answer.New(answer.Deps{})
		if err != nil {
			t.Fatalf("answer.New: %v", err)
		}
		deps.Answerer = a
	}
	if deps.Records == nil {
		deps.Records = &fakeRecords{id: "9f8e7d6c-5b4a-4392-8170-6f5e4d3c2b1a"}
	}
	return newService(deps)
}

// ---------------------------------------------------------------------------
// The pipeline

// TestSearchRunsThePipelineInOrder pins the wiring the transport owns: each
// step receives what the PREVIOUS step produced, and the plan is what the
// retrieval is built from.
func TestSearchRunsThePipelineInOrder(t *testing.T) {
	plannerFake := &fakePlanner{plan: planner.Plan{
		Status: planner.StatusPlanned, Query: "co2 uptake",
		Document: &planner.Document{
			PlanVersion:  planner.PlanVersion,
			Intent:       planner.IntentAnswer,
			TargetObject: planner.TargetObject{EntityTypes: []string{"asset"}},
			Visibility:   planner.VisibilityPublic,
		},
	}}
	retriever := &fakeRetriever{result: retrieval.Result{Query: "co2 uptake", Signals: signalsFor()}}
	ranker := &fakeRanker{result: ranking.Result{Query: "co2 uptake", Factors: ranking.Factors(), Ranked: []ranking.Ranked{rankedAsset()}}}
	answerer := &fakeAnswerer{ans: answer.Answer{Version: answer.AnswerVersion, Status: answer.StatusFallback, Reason: answer.ReasonNoProvider, Query: "co2 uptake"}}
	records := &fakeRecords{id: "11111111-2222-4333-8444-555555555555"}
	svc := newPipeline(t, Deps{
		Planner: plannerFake, Retriever: retriever, Ranker: ranker, Answerer: answerer, Records: records,
	})

	filters := []byte(`{"material":"MOF"}`)
	if _, _, err := svc.search(context.Background(), actorID, "co2 uptake", filters); err != nil {
		t.Fatalf("search: %v", err)
	}

	if plannerFake.got.Query != "co2 uptake" {
		t.Fatalf("the planner was asked %q", plannerFake.got.Query)
	}
	// The retrieval's request is the PLAN's, plus the caller's filters
	// (retrieval.RequestFromPlan) — the plan narrows, the caller's filters
	// pass through untouched.
	if got := retriever.got.EntityTypes; len(got) != 1 || got[0] != "asset" {
		t.Fatalf("the retrieval was narrowed to %v, want the plan's entity types", got)
	}
	if !retriever.got.PublicOnly {
		t.Fatal("the retrieval was not narrowed to public rows, which the plan asked for")
	}
	if string(retriever.got.Facets) != string(filters) {
		t.Fatalf("the retrieval was given the filters %s, want %s", retriever.got.Facets, filters)
	}
	// The ranker is given the retrieval's own result, and the answerer the
	// ranker's — the two halves of the result cannot be mixed.
	if len(ranker.got.Candidates) != len(retriever.result.Candidates) {
		t.Fatal("the ranker was not given the retrieval's result")
	}
	if len(answerer.got.Result.Ranked) != 1 || answerer.got.Result.Ranked[0].Ref != assetRef {
		t.Fatalf("the answerer was given %+v, want the ranked result", answerer.got.Result.Ranked)
	}
	if len(answerer.got.Signals) != len(signalsFor()) {
		t.Fatalf("the answerer was given %d signals, want the retrieval's %d", len(answerer.got.Signals), len(signalsFor()))
	}
	if retriever.calls != 1 || ranker.calls != 1 || answerer.calls != 1 {
		t.Fatalf("a step ran more than once: retrieval=%d ranker=%d answer=%d", retriever.calls, ranker.calls, answerer.calls)
	}
}

// TestSearchRecordsWhatThePipelineProduced: the row docs/22 §8 requires
// carries the pipeline's own values — the refs the ranking returned, the
// citations the answer made, the signals the retrieval reported — in the
// spellings their producers use.
func TestSearchRecordsWhatThePipelineProduced(t *testing.T) {
	records := &fakeRecords{id: "11111111-2222-4333-8444-555555555555"}
	plannerFake := &fakePlanner{plan: planner.Plan{
		Status: planner.StatusPlanned, Query: "co2 uptake",
		Document: &planner.Document{
			PlanVersion: planner.PlanVersion, Intent: planner.IntentAnswer,
			TargetObject: planner.TargetObject{EntityTypes: []string{"asset"}},
		},
	}}
	svc := newPipeline(t, Deps{Planner: plannerFake, Records: records})

	filters := []byte(`{"material":"MOF"}`)
	_, id, err := svc.search(context.Background(), actorID, "co2 uptake", filters)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if id != records.id {
		t.Fatalf("search id = %q, want the recorded id %q", id, records.id)
	}
	rec := records.got
	if rec.ActorID != actorID || rec.Query != "co2 uptake" {
		t.Fatalf("record = actor %q query %q", rec.ActorID, rec.Query)
	}
	if string(rec.Filters) != string(filters) {
		t.Fatalf("recorded filters = %s, want the caller's bytes %s", rec.Filters, filters)
	}
	if len(rec.SelectedRefs) != 1 || rec.SelectedRefs[0] != assetRef {
		t.Fatalf("recorded refs = %v, want the ranking's refs", rec.SelectedRefs)
	}
	// The plan column holds the planner's OWN canonical rendering, so a
	// reader of the row sees the plan's status and reason and not just a
	// document.
	wantPlan, err := plannerFake.plan.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if string(rec.Plan) != string(wantPlan) {
		t.Fatalf("recorded plan = %s, want %s", rec.Plan, wantPlan)
	}
	var signals []retrieval.SignalReport
	if err := json.Unmarshal(rec.Signals, &signals); err != nil {
		t.Fatalf("the recorded signals are not a SignalReport[]: %v", err)
	}
	if len(signals) != len(signalsFor()) {
		t.Fatalf("recorded %d signals, want %d", len(signals), len(signalsFor()))
	}
	if !json.Valid(rec.Answer) {
		t.Fatalf("the recorded answer is not JSON: %s", rec.Answer)
	}
	if strings.Contains(string(rec.Answer), "no answer model is configured") == false {
		t.Fatalf("the recorded answer is not the structured fallback the generator produced: %s", rec.Answer)
	}
}

// TestSearchWithoutAPlannerRecordsNoPlan: "this deployment planned nothing"
// and "the planner was asked and failed" must not be the same row.
func TestSearchWithoutAPlannerRecordsNoPlan(t *testing.T) {
	records := &fakeRecords{id: "11111111-2222-4333-8444-555555555555"}
	svc := newPipeline(t, Deps{Records: records})

	if _, _, err := svc.search(context.Background(), actorID, "co2 uptake", nil); err != nil {
		t.Fatalf("search: %v", err)
	}
	if records.got.Plan != nil {
		t.Fatalf("a search with no planner recorded a plan: %s", records.got.Plan)
	}
	if records.got.Filters != nil {
		t.Fatalf("a request with no filters recorded filters: %s", records.got.Filters)
	}
}

// TestSearchWithoutAnAnswerModelIsStillASearch: the provider's absence is a
// supported deployment state, not a failure of the route. The answer falls
// back, the record is written, and the caller gets a result.
func TestSearchWithoutAnAnswerModelIsStillASearch(t *testing.T) {
	records := &fakeRecords{id: "11111111-2222-4333-8444-555555555555"}
	svc := newPipeline(t, Deps{Records: records})

	ans, id, err := svc.search(context.Background(), actorID, "co2 uptake", nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if id == "" {
		t.Fatal("the search was not recorded")
	}
	if ans.Status != answer.StatusFallback || ans.Reason != answer.ReasonNoProvider {
		t.Fatalf("answer = %s/%s, want a fallback naming the missing provider", ans.Status, ans.Reason)
	}
	// The structured half is there regardless: this is the "results survive
	// the missing sentence" property the whole task rests on.
	if len(ans.Sources) != 1 || ans.Sources[0].Ref != assetRef {
		t.Fatalf("the fallback carries %d sources, want the ranked candidate", len(ans.Sources))
	}
	if len(ans.Limitations) == 0 {
		t.Fatal("the fallback carries no limitations")
	}
}

// TestSearchRefusesAFailedRecordAndDoesNotAnswer is the decision service.go
// documents: an answer whose search id resolves to nothing is not a partial
// success.
func TestSearchRefusesAFailedRecordAndDoesNotAnswer(t *testing.T) {
	records := &fakeRecords{err: errors.New("connection refused")}
	svc := newPipeline(t, Deps{Records: records})

	ans, id, err := svc.search(context.Background(), actorID, "co2 uptake", nil)
	if !errors.Is(err, ErrRecordFailed) {
		t.Fatalf("err = %v, want ErrRecordFailed", err)
	}
	if id != "" || ans.Status != "" {
		t.Fatalf("an answer was returned with a failed record: id=%q status=%q", id, ans.Status)
	}
}

// TestSearchRefusesWithNoActor: the guard makes this unreachable in
// production, and the backstop is checked rather than assumed because the
// alternative to checking is searching on behalf of nobody.
func TestSearchRefusesWithNoActor(t *testing.T) {
	records := &fakeRecords{id: "x"}
	scope := &fakeScope{}
	svc := newPipeline(t, Deps{Scope: scope, Records: records})

	if _, _, err := svc.search(context.Background(), "", "q", nil); !errors.Is(err, errNoActor) {
		t.Fatalf("err = %v, want errNoActor", err)
	}
	if scope.calls != 0 {
		t.Fatal("a scope was resolved for an actor that does not exist")
	}
	if records.got.ActorID != "" {
		t.Fatal("a record was written for an actor that does not exist")
	}
}

// TestSearchClassifiesEveryFailureShape pins the 400/503 split. It is one
// table because the classification is one switch, and a case added without a
// line here is a case answered by the default (503), which this test would
// not notice.
func TestSearchClassifiesEveryFailureShape(t *testing.T) {
	cases := []struct {
		name string
		dep  func(*Deps)
		err  error
		want error
	}{
		{
			name: "empty query is the caller's",
			dep:  func(d *Deps) { d.Planner = &fakePlanner{err: planner.ErrEmptyQuery} },
			want: ErrInvalidRequest,
		},
		{
			name: "an unusable filter is the caller's",
			dep:  func(d *Deps) { d.Retriever = &fakeRetriever{err: retrieval.ErrBadFilter} },
			want: ErrInvalidRequest,
		},
		{
			name: "an unknown entity type is the caller's",
			dep:  func(d *Deps) { d.Retriever = &fakeRetriever{err: retrieval.ErrUnknownEntityType} },
			want: ErrInvalidRequest,
		},
		{
			name: "a store failure is the platform's",
			dep:  func(d *Deps) { d.Retriever = &fakeRetriever{err: retrieval.ErrStore} },
			want: ErrUnavailable,
		},
		{
			name: "a ranking store failure is the platform's",
			dep:  func(d *Deps) { d.Ranker = &fakeRanker{err: ranking.ErrStore} },
			want: ErrUnavailable,
		},
		{
			name: "a scope read failure is the platform's",
			dep:  func(d *Deps) { d.Scope = &fakeScope{err: errors.New("pool closed")} },
			want: ErrUnavailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := Deps{}
			tc.dep(&deps)
			svc := newPipeline(t, deps)
			if _, _, err := svc.search(context.Background(), actorID, "q", nil); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestSearchStopsAtTheFirstFailure: a failed step must not be followed by the
// ones after it — a provider call or a database read that cannot matter.
func TestSearchStopsAtTheFirstFailure(t *testing.T) {
	retriever := &fakeRetriever{err: retrieval.ErrStore}
	ranker := &fakeRanker{result: ranking.Result{Query: "q"}}
	answerer := &fakeAnswerer{}
	records := &fakeRecords{id: "x"}
	svc := newPipeline(t, Deps{Retriever: retriever, Ranker: ranker, Answerer: answerer, Records: records})

	if _, _, err := svc.search(context.Background(), actorID, "q", nil); err == nil {
		t.Fatal("a failed retrieval did not stop the pipeline")
	}
	if ranker.calls != 0 || answerer.calls != 0 {
		t.Fatalf("steps after the failure ran: ranker=%d answer=%d", ranker.calls, answerer.calls)
	}
	if records.got.Query != "" {
		t.Fatal("a record was written for a search that failed")
	}
}

// TestSearchReturnsTheCallersCancellation: a caller that has stopped waiting
// is told so, and nothing is recorded on its behalf.
func TestSearchReturnsTheCallersCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	retriever := &fakeRetriever{err: context.Canceled}
	records := &fakeRecords{id: "x"}
	svc := newPipeline(t, Deps{Retriever: retriever, Records: records})

	_, _, err := svc.search(ctx, actorID, "q", nil)
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want an unavailable error carrying the cancellation", err)
	}
	if records.got.Query != "" {
		t.Fatal("a cancelled search was recorded")
	}
}

// ---------------------------------------------------------------------------
// The wire

// TestNoPrincipalIsUnauthorized drives the handler itself — the one path a
// unit test can reach past the guard — and pins the code.
func TestNoPrincipalIsUnauthorized(t *testing.T) {
	admin := newPipeline(t, Deps{})
	rec := httptest.NewRecorder()
	admin.handleSearch(rec, httptest.NewRequest(http.MethodPost, "/api/v1/search",
		strings.NewReader(`{"query":"co2"}`)))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	var body wireError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the error envelope is not JSON: %s", rec.Body.String())
	}
	if body.Code != CodeSearchUnauthenticated {
		t.Fatalf("code = %q, want %q", body.Code, CodeSearchUnauthenticated)
	}
	if body.Retryable {
		t.Fatal("a 401 was marked retryable")
	}
}

// TestDecodeSearchRefusesWhatTheContractDoesNotAllow: the transport checks
// the SHAPE the contract declares (query is a string and present, filters is
// an object) and leaves what a request MEANS to the retrieval.
func TestDecodeSearchRefusesWhatTheContractDoesNotAllow(t *testing.T) {
	cases := []struct {
		name string
		body string
		why  string
	}{
		{"no query", `{"filters":{"a":1}}`, "the contract requires `query`"},
		{"blank query", `{"query":"   "}`, "a question of spaces is not a question"},
		{"filters not an object", `{"query":"q","filters":[1,2]}`, "the contract says `filters: {type: object}`"},
		{"filters a string", `{"query":"q","filters":"material=MOF"}`, "the same"},
		{"filters a number", `{"query":"q","filters":3}`, "the same"},
		{"body not an object", `{"query":`, "a truncated body is not JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/search", strings.NewReader(tc.body))
			// decodeSearch writes its own refusal; the caller id comes from
			// the guard and is not what this test is about.
			if _, ok := decodeSearch(rec, req); ok {
				t.Fatalf("the loader accepted %s (%s)", tc.body, tc.why)
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			var body wireError
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("the error envelope is not JSON: %s", rec.Body.String())
			}
			if body.Code != CodeSearchInvalidRequest {
				t.Fatalf("code = %q, want %q", body.Code, CodeSearchInvalidRequest)
			}
			// The message names the field and never the value the caller
			// sent: a refusal must not echo an attacker's payload back.
			if strings.Contains(body.Message, "material=MOF") {
				t.Fatalf("the refusal echoed the caller's value: %q", body.Message)
			}
		})
	}
}

// TestFiltersArePassedThroughVerbatim: the platform stores and matches the
// caller's filters as sent, so decoding them into a map anywhere on the way
// would rewrite them (numbers through float64, key order lost).
func TestFiltersArePassedThroughVerbatim(t *testing.T) {
	body := `{"query":"co2 uptake","filters":{"z":1,"a":[1,2],"big":12345678901234567890,"p":1.50}}`
	rec := httptest.NewRecorder()
	got, ok := decodeSearch(rec, httptest.NewRequest(http.MethodPost, "/api/v1/search", strings.NewReader(body)))
	if !ok {
		t.Fatalf("decodeSearch refused a valid request: %s", rec.Body.String())
	}
	const want = `{"z":1,"a":[1,2],"big":12345678901234567890,"p":1.50}`
	if string(got.Filters) != want {
		t.Fatalf("filters = %s, want the caller's bytes %s", got.Filters, want)
	}
	if got.Query != "co2 uptake" {
		t.Fatalf("query = %q", got.Query)
	}

	// Absent and null both mean "no filter", and neither is the four bytes
	// "null" reaching the retrieval as a filter document.
	for _, body := range []string{`{"query":"q"}`, `{"query":"q","filters":null}`} {
		rec := httptest.NewRecorder()
		got, ok := decodeSearch(rec, httptest.NewRequest(http.MethodPost, "/api/v1/search", strings.NewReader(body)))
		if !ok {
			t.Fatalf("decodeSearch refused %s: %s", body, rec.Body.String())
		}
		if got.Filters != nil {
			t.Fatalf("%s gave filters %s, want none", body, got.Filters)
		}
	}
}

// TestRegisterMountsTheContractPath: the route is the contract's and it is
// POST only.
func TestRegisterMountsTheContractPath(t *testing.T) {
	v1 := http.NewServeMux()
	New(Deps{
		Scope: &fakeScope{}, Retriever: &fakeRetriever{}, Ranker: &fakeRanker{},
		Answerer: &fakeAnswerer{}, Records: &fakeRecords{},
	}).Register(v1)

	rec := httptest.NewRecorder()
	v1.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/search", strings.NewReader(`{"query":"q"}`)))
	_ = rec
	if rec.Code == http.StatusMethodNotAllowed || rec.Code == http.StatusNotFound {
		t.Fatalf("POST /api/v1/search is not registered: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	v1.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/search", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/v1/search = %d, want 405: there is nothing to read at this path", rec.Code)
	}
}

// TestRequiredDependenciesAreRefused: each of the five is the only producer
// of something the answer or the record must contain, so a nil one is caught
// at wiring time rather than at the first request.
func TestRequiredDependenciesAreRefused(t *testing.T) {
	cases := map[string]Deps{
		"scope":     {Retriever: &fakeRetriever{}, Ranker: &fakeRanker{}, Answerer: &fakeAnswerer{}, Records: &fakeRecords{}},
		"retriever": {Scope: &fakeScope{}, Ranker: &fakeRanker{}, Answerer: &fakeAnswerer{}, Records: &fakeRecords{}},
		"ranker":    {Scope: &fakeScope{}, Retriever: &fakeRetriever{}, Answerer: &fakeAnswerer{}, Records: &fakeRecords{}},
		"answerer":  {Scope: &fakeScope{}, Retriever: &fakeRetriever{}, Ranker: &fakeRanker{}, Records: &fakeRecords{}},
		"records":   {Scope: &fakeScope{}, Retriever: &fakeRetriever{}, Ranker: &fakeRanker{}, Answerer: &fakeAnswerer{}},
	}
	for name, deps := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("a nil %s was accepted", name)
				}
			}()
			newService(deps)
		})
	}
}
