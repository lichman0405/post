// Task T0906 — the database half of the answer's grounding guarantee.
//
// The Go tests around this file prove the guard that runs in memory
// (internal/search/answer/grounding.go and its golden fixtures) and the
// transport that records what the pipeline produced
// (cmd/api/searchhttp/grounding_test.go). Neither of them can show what
// migration 00121 adds: a SECOND refusal that does not depend on any Go code
// having been run — the table's CHECK (citations <@ selected_refs).
//
// That distinction is the whole point of this file. The guard can be
// bypassed by a writer that never calls it (a future store, a migration
// script, an operator at a psql prompt), and docs/54 ranks a fabricated or
// unauthorized citation as a top-severity scenario. A rule that lives in one
// process is a rule that holds while that process is the only writer; the
// CHECK holds for every writer.
//
// The unit test cannot stand in for this: an in-memory fake accepts any
// record, so "the store refuses an ungrounded citation" is a statement about
// PostgreSQL, and this is the only place it can be observed.
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/searchhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/search/answer"
	"github.com/lichman0405/post/internal/search/answer/answertest"
	"github.com/lichman0405/post/internal/search/ranking"
	"github.com/lichman0405/post/internal/search/retrieval"
)

// searchAnswerTaskID namespaces this file's test databases (docs/66 §3).
const searchAnswerTaskID = "T0906"

// The refs of one search. The citation vocabulary is the answer layer's
// ("<kind>:<identity>@<version>", internal/search/retrieval/candidate.go).
const (
	answerRefAsset   = "asset:AST-0001@2"
	answerRefKnows   = "knowledge:KNW-0007@1"
	answerRefOther   = "asset:AST-0001@3"
	answerRefInvent  = "asset:AST-9999@7"
	answerRefSignals = `[{"signal":"full_text","ran":true,"hits":2}]`
)

// The two documents a search records. The plan is stored as the planner's own
// canonical rendering and the answer as the answer layer's, so both are shown
// here in those shapes rather than as shapes this test invented.
const (
	answerPlanJSON   = `{"status":"planned","query":"co2 uptake","document":{"plan_version":"1","intent":"answer"}}`
	answerAnswerJSON = `{"answer_version":"1","query":"co2 uptake","status":"fallback","reason":"no_provider","summary":"","citations":[],"sources":[]}`
)

// TestSearchRecordRefusesAnUngroundedCitation is the criterion "模型不可引用
// 不存在 id" at the database: a citation the search did not return cannot be
// stored, whatever wrote it.
func TestSearchRecordRefusesAnUngroundedCitation(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), searchAnswerTaskID)
	actorID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO users (handle, display_name) VALUES ('ans-author','Author') RETURNING id`)
	store := persistence.NewSearchRecordStore(pool)

	base := persistence.SearchRecord{
		ActorID:      actorID,
		Query:        "co2 uptake",
		Plan:         []byte(answerPlanJSON),
		Signals:      []byte(answerRefSignals),
		SelectedRefs: []string{answerRefAsset, answerRefKnows},
		Answer:       []byte(answerAnswerJSON),
	}

	cases := []struct {
		name      string
		citations []string
		why       string
	}{
		{
			name:      "an invented identity",
			citations: []string{answerRefInvent},
			why:       "AST-9999 was never returned by any retrieval",
		},
		{
			name:      "an invented identity beside a real one",
			citations: []string{answerRefAsset, answerRefInvent},
			why:       "the real citation must not make the fabricated one storable",
		},
		{
			name:      "the same identity at another version",
			citations: []string{answerRefOther},
			why:       "the ref is a VERSION: citing @3 of an object the search returned at @2 is citing something the search did not return",
		},
		{
			name:      "an identity of an unreturned kind",
			citations: []string{"release:REL-0001@1"},
			why:       "the search returned an asset and a knowledge object, not a release",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := base
			rec.Citations = tc.citations
			_, err := store.Save(ctx, rec)
			if err == nil {
				t.Fatalf("the record was saved: %s (%s)", strings.Join(tc.citations, ", "), tc.why)
			}
			assertCheckViolation(t, err)
		})
	}
}

// TestSearchRecordAcceptsWhatTheSearchReturned is the other direction, and it
// is what stops the test above from being satisfied by a table that refuses
// everything: a citation of a returned ref is stored, and the row reads back
// with exactly the values the pipeline produced.
func TestSearchRecordAcceptsWhatTheSearchReturned(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), searchAnswerTaskID)
	actorID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO users (handle, display_name) VALUES ('ans-author','Author') RETURNING id`)
	store := persistence.NewSearchRecordStore(pool)

	filters := []byte(`{"material":"MOF"}`)
	id, err := store.Save(ctx, persistence.SearchRecord{
		ActorID:      actorID,
		Query:        "co2 uptake",
		Filters:      filters,
		Plan:         []byte(answerPlanJSON),
		Signals:      []byte(answerRefSignals),
		SelectedRefs: []string{answerRefAsset, answerRefKnows},
		Citations:    []string{answerRefAsset},
		Answer:       []byte(answerAnswerJSON),
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if id == "" {
		t.Fatal("Save returned no id: the contract addresses the search by it")
	}

	var (
		gotActor     string
		gotQuery     string
		gotFilters   []byte
		gotPlan      []byte
		gotSignals   []byte
		gotRefs      []string
		gotCitations []string
		gotAnswer    []byte
	)
	if err := pool.QueryRow(ctx, `
		SELECT actor_id::text, query, filters, plan, signals, selected_refs, citations, answer
		FROM search_records WHERE id = $1::uuid`, id).
		Scan(&gotActor, &gotQuery, &gotFilters, &gotPlan, &gotSignals, &gotRefs, &gotCitations, &gotAnswer); err != nil {
		t.Fatalf("read the record back: %v", err)
	}
	if gotActor != actorID {
		t.Errorf("actor = %s, want %s", gotActor, actorID)
	}
	if gotQuery != "co2 uptake" {
		t.Errorf("query = %q", gotQuery)
	}
	// The filters are stored as the CALLER sent them, so a later reader of
	// the row can see what was asked rather than a re-encoding of it.
	if !json.Valid(gotFilters) || !strings.Contains(string(gotFilters), `"MOF"`) {
		t.Errorf("filters = %s, want the caller's document", gotFilters)
	}
	if len(gotRefs) != 2 || gotRefs[0] != answerRefAsset || gotRefs[1] != answerRefKnows {
		t.Errorf("selected_refs = %v, want the ranked refs in order", gotRefs)
	}
	if len(gotCitations) != 1 || gotCitations[0] != answerRefAsset {
		t.Errorf("citations = %v, want [%s]", gotCitations, answerRefAsset)
	}
	// jsonb does not preserve key order or whitespace, which is why the
	// columns are compared as DOCUMENTS: what is stored is the document a
	// reader parses, not the byte string a writer produced.
	for _, tc := range []struct {
		name string
		got  []byte
		want string
	}{{"plan", gotPlan, answerPlanJSON}, {"signals", gotSignals, answerRefSignals}, {"answer", gotAnswer, answerAnswerJSON}} {
		if !sameJSON(t, tc.got, tc.want) {
			t.Errorf("%s = %s, want %s", tc.name, tc.got, tc.want)
		}
	}
}

// TestSearchRecordAcceptsAFallbackWithNoCitation: an answer that cited nothing
// is a real state (every structured fallback), and it must be storable — a
// table that required a citation would forbid exactly the answers the platform
// produces when it has no model.
func TestSearchRecordAcceptsAFallbackWithNoCitation(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), searchAnswerTaskID)
	actorID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO users (handle, display_name) VALUES ('ans-author','Author') RETURNING id`)
	store := persistence.NewSearchRecordStore(pool)

	if _, err := store.Save(ctx, persistence.SearchRecord{
		ActorID:      actorID,
		Query:        "co2 uptake",
		Signals:      []byte(answerRefSignals),
		SelectedRefs: []string{answerRefAsset},
		Answer:       []byte(answerAnswerJSON),
	}); err != nil {
		t.Fatalf("a fallback with no citation was refused: %v", err)
	}

	// And a search that selected NOTHING is storable too — that is what a
	// query with no hits records, and the refs column is an empty list
	// rather than an absent one (the column is NOT NULL).
	if _, err := store.Save(ctx, persistence.SearchRecord{
		ActorID: actorID,
		Query:   "nothing matches this",
		Signals: []byte(answerRefSignals),
		Answer:  []byte(answerAnswerJSON),
	}); err != nil {
		t.Fatalf("a search with no refs was refused: %v", err)
	}
}

// TestSearchRecordRefusesWhatIsNotASearch: the store's own refusals, which
// are the caller's mistakes and are knowable before the write.
func TestSearchRecordRefusesWhatIsNotASearch(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), searchAnswerTaskID)
	actorID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO users (handle, display_name) VALUES ('ans-author','Author') RETURNING id`)
	store := persistence.NewSearchRecordStore(pool)

	cases := []struct {
		name string
		rec  persistence.SearchRecord
		why  string
	}{
		{
			name: "no actor",
			rec:  persistence.SearchRecord{Query: "q", Signals: []byte(`[]`), Answer: []byte(`{}`)},
			why:  "a search nobody ran under has no scope to have been filtered by",
		},
		{
			name: "no query",
			rec:  persistence.SearchRecord{ActorID: actorID, Signals: []byte(`[]`), Answer: []byte(`{}`)},
			why:  "docs/22 §8 records the question that was asked",
		},
		{
			name: "an actor that is not a uuid",
			rec:  persistence.SearchRecord{ActorID: "alice", Query: "q", Signals: []byte(`[]`), Answer: []byte(`{}`)},
			why:  "the column is a uuid with a foreign key to users",
		},
		{
			name: "an answer that is not JSON",
			rec:  persistence.SearchRecord{ActorID: actorID, Query: "q", Signals: []byte(`[]`), Answer: []byte(`not json`)},
			why:  "the column is jsonb",
		},
		{
			name: "no signal report",
			rec:  persistence.SearchRecord{ActorID: actorID, Query: "q", Answer: []byte(`{}`)},
			why:  "the answer's coverage limitations are derived from it, so a search without one cannot be audited",
		},
		{
			name: "no answer document",
			rec:  persistence.SearchRecord{ActorID: actorID, Query: "q", Signals: []byte(`[]`)},
			why:  "a search that ran always produced one, even if only as a fallback",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.Save(ctx, tc.rec)
			if err == nil {
				t.Fatalf("the record was saved: %s", tc.why)
			}
			// The class is the store's own where the mistake is knowable
			// before the write; the database's where it is not (a NOT NULL
			// column, the foreign key).
			if !errors.Is(err, persistence.ErrSearchRecord) && !isConstraintViolation(err) {
				t.Fatalf("err = %v, want a caller-shaped refusal", err)
			}
		})
	}

	// An actor the platform does not have is refused by the foreign key, not
	// by the format check: the record belongs to a person who exists.
	_, err := store.Save(ctx, persistence.SearchRecord{
		ActorID: "00000000-0000-4000-8000-000000000000",
		Query:   "q", Signals: []byte(`[]`), Answer: []byte(`{}`),
	})
	if err == nil {
		t.Fatal("a record was saved for a user that does not exist")
	}
	if !isForeignKeyViolation(err) {
		t.Fatalf("err = %v, want a foreign-key violation", err)
	}
}

// assertCheckViolation reports whether err is PostgreSQL's refusal of a CHECK.
// It matches the SQLSTATE and not the constraint's NAME: the names are
// PostgreSQL's own (<table>_check, <table>_<column>_check), so a test pinned
// to a name would fail the day a constraint is renamed for a reason that has
// nothing to do with this behaviour.
func assertCheckViolation(t *testing.T, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("err = %v (%T), want a PostgreSQL check violation", err, err)
	}
	if pgErr.Code != checkViolation {
		t.Fatalf("SQLSTATE = %s (%s), want %s (check_violation)", pgErr.Code, pgErr.ConstraintName, checkViolation)
	}
	if pgErr.ConstraintName == "" {
		t.Fatal("the refusal names no constraint: the caller cannot tell which rule refused the row")
	}
}

// checkViolation and foreignKeyViolation are the two SQLSTATE classes this
// file reads. They are spelled out rather than imported so the test states
// what it expects from the database.
const (
	checkViolation      = "23514"
	foreignKeyViolation = "23503"
)

func isConstraintViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == checkViolation || pgErr.Code == "23502")
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation
}

// sameJSON compares two documents by value. jsonb normalizes what it stores
// (key order, whitespace, duplicate keys), so comparing bytes would test the
// server's rendering rather than what was recorded.
func sameJSON(t *testing.T, got []byte, want string) bool {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("the stored document is not JSON: %s: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatalf("the expected document is not JSON: %s: %v", want, err)
	}
	left, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	right, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(left) == string(right)
}

// ---------------------------------------------------------------------------
// The endpoint, over the wire.
//
// The tests above exercise the store and the database's CHECK. These drive
// POST /api/v1/search as a browser does: through the real auth guard, over a
// real session, against real PostgreSQL, with the pipeline cmd/api/main.go
// composes — the real retrieval, the real ranking and the real answer
// generator, no fakes anywhere.
//
// The distinction is not ceremony. The unit tests in cmd/api/searchhttp run
// the pipeline over fakes, and a fake accepts any request it is handed; the
// whole class of defect where the transport hands the pipeline the wrong
// thing is invisible there by construction. Here the retrieval decides for
// itself whether the request is answerable, so a request the transport built
// wrong fails as the caller would see it.

// The corpus the endpoint searches: ONE public projected document. Public
// matters — the access predicate in SearchDocuments grants a row to any
// caller when visibility is 'public', so the search recalls it without a
// project fixture, and the caller's scope (membership in nothing) is
// untouched by the seeding.
//
// The row is written the way T0901's projection writes one (the idiom
// ranking_test.go uses), including the structured facets: 'version' is the
// facet search.PinnedVersion reads for an asset, so the candidate this row
// becomes is cited as asset:AST-0001@2 — a ref, not a bare identity.
const (
	answerCorpusRef     = "asset:AST-0001"
	answerCorpusRefV2   = "asset:AST-0001@2"
	answerCorpusTitle   = "Mg-MOF-74 CO2 uptake"
	answerCorpusContent = "Mg-MOF-74 takes up 3.2 mmol/g of CO2 at 298 K."
	answerQuery         = "CO2 uptake"
)

func seedAnswerCorpus(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO search_documents (entity_ref, entity_type, visibility, title, content, structured)
		VALUES ($1, 'asset', 'public', $2, $3, '{"version":"2"}'::jsonb)`,
		answerCorpusRef, answerCorpusTitle, answerCorpusContent); err != nil {
		t.Fatalf("seed %s: %v", answerCorpusRef, err)
	}
}

// searchAnswerServer composes the API exactly as cmd/api/main.go does and
// serves it behind the real auth guard. provider may be nil, which is the
// state main.go wires: a deployment with no answer model.
func searchAnswerServer(t *testing.T, pool *pgxpool.Pool, provider answer.Provider) *httptest.Server {
	t.Helper()
	authAPI := authhttp.New(authhttp.Deps{
		Users:    persistence.NewCredentialStore(pool),
		Sessions: memstore.NewSessions(),
		Limiter:  memstore.NewLimiter(),
		Cfg: authn.Config{
			WebOrigin:          "http://web.test",
			SessionTTL:         time.Hour,
			LoginLimitPerEmail: 1000,
			LoginLimitPerIP:    10000,
			LoginWindow:        time.Minute,
			SignupLimitPerIP:   10000,
		},
	})
	retrievalStore, err := retrieval.NewSQLStore(pool)
	if err != nil {
		t.Fatalf("retrieval.NewSQLStore: %v", err)
	}
	rankingStore, err := ranking.NewSQLStore(pool)
	if err != nil {
		t.Fatalf("ranking.NewSQLStore: %v", err)
	}
	searcher, err := retrieval.NewRetriever(retrievalStore, nil, retrieval.WithLogger(silentLogger()))
	if err != nil {
		t.Fatalf("retrieval.NewRetriever: %v", err)
	}
	ranker, err := ranking.NewRanker(rankingStore)
	if err != nil {
		t.Fatalf("ranking.NewRanker: %v", err)
	}
	answerer, err := answer.New(answer.Deps{Provider: provider, Logger: silentLogger()})
	if err != nil {
		t.Fatalf("answer.New: %v", err)
	}
	searchAPI := searchhttp.New(searchhttp.Deps{
		Scope:     persistence.NewProjectStore(pool),
		Retriever: searcher,
		Ranker:    ranker,
		Answerer:  answerer,
		Records:   persistence.NewSearchRecordStore(pool),
	})
	// The same shape as cmd/api/main.go: the search route is registered on
	// the v1 mux, and the guard wraps the whole of it, so the route is
	// unreachable without a session — not merely unauthenticated inside the
	// handler.
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	searchAPI.Register(mux)
	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)
	return ts
}

// answerDocument is the part of the response this file reads.
type answerDocument struct {
	SearchID string `json:"search_id"`
	Answer   struct {
		Version     string   `json:"answer_version"`
		Status      string   `json:"status"`
		Reason      string   `json:"reason"`
		Query       string   `json:"query"`
		AnswerView  bool     `json:"answer_view"`
		Summary     string   `json:"summary"`
		Citations   []string `json:"citations"`
		Limitations []struct {
			Text   string   `json:"text"`
			Refs   []string `json:"refs"`
			Origin string   `json:"origin"`
		} `json:"limitations"`
		Sources []struct {
			Ref      string `json:"ref"`
			Identity string `json:"identity"`
			Version  string `json:"version"`
			Href     string `json:"href"`
			Title    string `json:"title"`
		} `json:"sources"`
	} `json:"answer"`
}

// searchExchange is one search's response, in the three shapes a test needs
// to talk about: the parsed answer, the answer document's own bytes (what the
// record stores), and the whole body (what the caller received).
//
// They are separate on purpose. The record holds the ANSWER, so comparing it
// against the whole response would compare two different documents — which is
// the mistake this type exists to make impossible.
type searchExchange struct {
	doc    answerDocument
	answer []byte
	body   string
}

// ask POSTs one search from an authenticated browser and returns the response.
func ask(t *testing.T, uc *testUserClient, body string) searchExchange {
	t.Helper()
	resp := uc.do(t, http.MethodPost, "/api/v1/search", body)
	mustStatus(t, resp, http.StatusOK)
	raw := readAll(t, resp)
	var envelope struct {
		SearchID string          `json:"search_id"`
		Answer   json.RawMessage `json:"answer"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("search response is not the contract document: %v: %s", err, raw)
	}
	var doc answerDocument
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("search response is not the contract document: %v: %s", err, raw)
	}
	if len(envelope.Answer) == 0 {
		t.Fatalf("the response carries no answer document: %s", raw)
	}
	return searchExchange{doc: doc, answer: envelope.Answer, body: raw}
}

// recordedSearch is one search_records row, read back with raw SQL.
type recordedSearch struct {
	ActorID      string
	Query        string
	PlanIsNull   bool
	SelectedRefs []string
	Citations    []string
	Answer       []byte
}

func readRecord(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string) recordedSearch {
	t.Helper()
	var rec recordedSearch
	if err := pool.QueryRow(ctx, `
		SELECT actor_id::text, query, plan IS NULL, selected_refs, citations, answer
		FROM search_records WHERE id = $1::uuid`, id).
		Scan(&rec.ActorID, &rec.Query, &rec.PlanIsNull, &rec.SelectedRefs, &rec.Citations, &rec.Answer); err != nil {
		t.Fatalf("read search %s back: %v — the contract addresses a search by this id", id, err)
	}
	return rec
}

// TestSearchAPIAnswersOverTheWire is the whole path a browser takes: sign up,
// ask a question about a public document, receive a structured answer, and
// find the search recorded under the id the response named. No planner and no
// provider are configured — the deployment main.go wires — so the answer is
// the fallback the contract promises when there is no model, and the record's
// plan column is NULL rather than an envelope for a step that never ran.
func TestSearchAPIAnswersOverTheWire(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), searchAnswerTaskID)
	seedAnswerCorpus(t, ctx, pool)
	ts := searchAnswerServer(t, pool, nil)

	alice, aliceID := signup(t, ts.URL, "search-alice@example.com", "search-alice")

	got := ask(t, alice, `{"query":"`+answerQuery+`"}`)
	doc := got.doc
	if doc.SearchID == "" {
		t.Fatalf("the response named no search id: %s", got.body)
	}
	if doc.Answer.Version != "1" {
		t.Errorf("answer_version = %q, want the contract's version", doc.Answer.Version)
	}
	if doc.Answer.Query != answerQuery {
		t.Errorf("the answer answers %q, want %q", doc.Answer.Query, answerQuery)
	}
	if doc.Answer.Status != "fallback" || doc.Answer.Reason != "no_provider" {
		t.Errorf("answer = %s/%s, want the fallback a deployment with no model gives", doc.Answer.Status, doc.Answer.Reason)
	}
	if doc.Answer.Summary != "" || len(doc.Answer.Citations) != 0 {
		t.Errorf("a fallback carries no summary and no citation: %q %v", doc.Answer.Summary, doc.Answer.Citations)
	}
	// The structured result is what the caller gets instead, and it is the
	// document the seeding put in the corpus — recalled by the REAL
	// retrieval, at the version the projection pinned.
	if len(doc.Answer.Sources) != 1 {
		t.Fatalf("the answer carries %d sources, want the one recalled document: %s", len(doc.Answer.Sources), got.body)
	}
	src := doc.Answer.Sources[0]
	if src.Ref != answerCorpusRefV2 {
		t.Errorf("source ref = %q, want %q", src.Ref, answerCorpusRefV2)
	}
	// Acceptance criterion two, from the caller's side: the source it may
	// cite is one it can open.
	if src.Href == "" {
		t.Error("the source carries no href, so a click on it cannot land anywhere")
	}
	if len(doc.Answer.Limitations) == 0 {
		t.Error("the answer states no limitation; even a fallback owes the caller the reason it is one")
	}

	// And the search survives the response: the row is under the id the
	// contract addresses, owned by the session that asked, holding the
	// question verbatim and the very bytes that were published.
	rec := readRecord(t, ctx, pool, doc.SearchID)
	if rec.ActorID != aliceID {
		t.Errorf("the record belongs to %s, want the caller %s", rec.ActorID, aliceID)
	}
	if rec.Query != answerQuery {
		t.Errorf("the record's query = %q, want %q", rec.Query, answerQuery)
	}
	if !rec.PlanIsNull {
		t.Error("the record's plan is not NULL, but no planner is configured: a plan that never ran must not look like one that did")
	}
	if len(rec.SelectedRefs) != 1 || rec.SelectedRefs[0] != answerCorpusRefV2 {
		t.Errorf("the record selected %v, want the ranked ref %s", rec.SelectedRefs, answerCorpusRefV2)
	}
	if !sameJSON(t, rec.Answer, string(got.answer)) {
		t.Errorf("the recorded answer is not the published one:\nrecord: %s\npublished: %s", rec.Answer, got.answer)
	}
}

// TestSearchAPIRefusesWhatIsNotARequest: the transport's own refusals, over
// the wire. Every one of them is a caller mistake that must not cost a
// database row — and the anonymous case is the guard's, not the handler's.
func TestSearchAPIRefusesWhatIsNotARequest(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), searchAnswerTaskID)
	seedAnswerCorpus(t, ctx, pool)
	ts := searchAnswerServer(t, pool, nil)

	// No session at all: refused by the GUARD, before the handler runs — so
	// the code is the guard's own (AUTH_UNAUTHENTICATED) and not the
	// handler's SEARCH_UNAUTHENTICATED. The distinction is worth asserting:
	// a route registered OUTSIDE the guard would answer the handler's code,
	// which is what makes this the check that the route is behind it.
	anon := newTestUserClient(ts.URL)
	resp, err := anon.client.Post(ts.URL+"/api/v1/search", "application/json",
		strings.NewReader(`{"query":"CO2 uptake"}`))
	if err != nil {
		t.Fatalf("anonymous search: %v", err)
	}
	mustStatus(t, resp, http.StatusUnauthorized)
	mustEnvelope(t, resp, "AUTH_UNAUTHENTICATED")
	_ = resp.Body.Close()

	alice, _ := signup(t, ts.URL, "search-refuse@example.com", "search-refuse")
	for _, tc := range []struct {
		name string
		body string
		code string
	}{
		{"a blank query", `{"query":"   "}`, "SEARCH_INVALID_REQUEST"},
		{"no query field", `{"query":""}`, "SEARCH_INVALID_REQUEST"},
		{"a query that is not a string", `{"query":42}`, "SEARCH_INVALID_REQUEST"},
		{"filters that are not an object", `{"query":"CO2 uptake","filters":[1,2]}`, "SEARCH_INVALID_REQUEST"},
		{"a body that is not JSON", `{not json`, "SEARCH_INVALID_REQUEST"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := alice.do(t, http.MethodPost, "/api/v1/search", tc.body)
			mustStatus(t, resp, http.StatusBadRequest)
			mustEnvelope(t, resp, tc.code)
		})
	}

	// A refused request leaves nothing behind: the record exists so that a
	// search can be cited afterwards, and a search that never ran has no
	// answer to cite.
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM search_records`).Scan(&rows); err != nil {
		t.Fatalf("count the records: %v", err)
	}
	if rows != 0 {
		t.Fatalf("%d records exist after nothing but refused requests", rows)
	}
}

// TestSearchAPIRefusesAHallucinatedCitationOverTheWire is acceptance
// criterion one ("模型不可引用不存在 id") end to end: a real retrieval recalls
// a real document, a provider answers by citing an id nobody fetched, and the
// caller receives the structured fallback instead — with the invented id in
// neither the response nor the row the platform saved.
//
// # The two documents, and why both are here
//
// The guard has two checks — a citation outside the vocabulary, and an entity
// named in the summary outside it (grounding.go) — and a document that
// commits both faults is refused by whichever check runs first. So a test
// that only ever sent such a document would stay green with either check
// broken: it would be measuring the pair, not the two. Each case below
// isolates one fault, which is what makes the pair's coverage real. (Found
// by mutating the citation loop and watching this test pass anyway.)
func TestSearchAPIRefusesAHallucinatedCitationOverTheWire(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), searchAnswerTaskID)
	seedAnswerCorpus(t, ctx, pool)

	cases := []struct {
		name string
		// email and handle are per case: both cases share one database, and
		// an account is unique by email.
		email string
		// doc is what the provider answers with. AST-9999 is in this corpus
		// nowhere, and AST-0001@2 is the ref the retrieval actually returned.
		doc string
		why string
	}{
		{
			name:  "an id invented in the citations",
			email: "search-hallucinate-cite@example.com",
			doc: `{
			  "answer_version": "1",
			  "summary": "Mg-MOF-74 takes up 3.2 mmol/g of CO2 at 298 K.",
			  "citations": ["asset:AST-9999@7", "asset:AST-0001@2"]
			}`,
			why: "the summary names no entity, so only the citation check can refuse this document",
		},
		{
			name:  "an id invented in the summary",
			email: "search-hallucinate-name@example.com",
			doc: `{
			  "answer_version": "1",
			  "summary": "Mg-MOF-74 takes up 3.2 mmol/g [asset:AST-9999@7], better than the earlier report [asset:AST-0001@2].",
			  "citations": ["asset:AST-9999@7", "asset:AST-0001@2"]
			}`,
			why: "the fabricated source is written into the sentence as well as cited",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := answertest.Reply(tc.doc)
			t.Cleanup(provider.Close)
			ts := searchAnswerServer(t, pool, provider)

			alice, _ := signup(t, ts.URL, tc.email, strings.SplitN(tc.email, "@", 2)[0])
			got := ask(t, alice, `{"query":"`+answerQuery+`"}`)
			doc := got.doc

			if provider.Calls() != 1 {
				t.Fatalf("the provider was called %d times, want once: the corpus has one document to answer with", provider.Calls())
			}
			if doc.Answer.Status != "fallback" || doc.Answer.Reason != "ungrounded_citation" {
				t.Fatalf("answer = %s/%s, want the refusal of a citation retrieval never returned (%s): %s",
					doc.Answer.Status, doc.Answer.Reason, tc.why, got.body)
			}
			if doc.Answer.Summary != "" || len(doc.Answer.Citations) != 0 {
				t.Fatalf("the refused document reached the caller: summary=%q citations=%v", doc.Answer.Summary, doc.Answer.Citations)
			}
			if n := strings.Count(got.body, "AST-9999"); n != 0 {
				t.Fatalf("the invented identity appears %d time(s) in the response: %s", n, got.body)
			}
			// What the caller gets instead is the evidence, so the refusal
			// costs the summary and not the search.
			if len(doc.Answer.Sources) != 1 || doc.Answer.Sources[0].Ref != answerCorpusRefV2 {
				t.Fatalf("the fallback carries %v, want the recalled ref", doc.Answer.Sources)
			}

			rec := readRecord(t, ctx, pool, doc.SearchID)
			assertNoInventedCitation(t, rec, got.answer)
		})
	}
}

// assertNoInventedCitation asserts the two halves of "the refusal left no
// trace" that are about the RECORD: nothing fabricated is cited in the row,
// and the row's answer is the document the caller was given.
func assertNoInventedCitation(t *testing.T, rec recordedSearch, published []byte) {
	t.Helper()
	if len(rec.Citations) != 0 {
		t.Errorf("the record cites %v, want nothing: the document was refused", rec.Citations)
	}
	if strings.Contains(string(rec.Answer), "AST-9999") {
		t.Errorf("the invented identity reached the recorded answer: %s", rec.Answer)
	}
	if !sameJSON(t, rec.Answer, string(published)) {
		t.Errorf("the recorded answer is not the published one:\nrecord: %s\npublished: %s", rec.Answer, published)
	}
}

// TestSearchAPIAnswersAGroundedCitationOverTheWire is the other half, and it
// is what stops the test above from passing on an implementation that refuses
// every model answer: the same corpus, the same provider protocol, a document
// that cites what the retrieval returned — and the answer is published, with
// its citation recorded in the row.
func TestSearchAPIAnswersAGroundedCitationOverTheWire(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), searchAnswerTaskID)
	seedAnswerCorpus(t, ctx, pool)

	provider := answertest.Reply(`{
	  "answer_version": "1",
	  "summary": "Mg-MOF-74 takes up 3.2 mmol/g of CO2 at 298 K [asset:AST-0001@2].",
	  "citations": ["asset:AST-0001@2"]
	}`)
	t.Cleanup(provider.Close)
	ts := searchAnswerServer(t, pool, provider)

	alice, _ := signup(t, ts.URL, "search-grounded@example.com", "search-grounded")
	got := ask(t, alice, `{"query":"`+answerQuery+`"}`)
	doc := got.doc

	if doc.Answer.Status != "answered" {
		t.Fatalf("answer = %s/%s, want answered: %s", doc.Answer.Status, doc.Answer.Reason, got.body)
	}
	if doc.Answer.Summary == "" {
		t.Fatal("the answered summary is empty")
	}
	if !doc.Answer.AnswerView {
		t.Error("a model-written summary is not marked as a View (docs/14 §4)")
	}
	if len(doc.Answer.Citations) != 1 || doc.Answer.Citations[0] != answerCorpusRefV2 {
		t.Fatalf("citations = %v, want [%s]", doc.Answer.Citations, answerCorpusRefV2)
	}

	rec := readRecord(t, ctx, pool, doc.SearchID)
	if len(rec.Citations) != 1 || rec.Citations[0] != answerCorpusRefV2 {
		t.Fatalf("the record cites %v, want [%s]", rec.Citations, answerCorpusRefV2)
	}
	// The invariant the table's CHECK states, observed on a row the pipeline
	// wrote rather than on one a test constructed.
	selected := map[string]bool{}
	for _, ref := range rec.SelectedRefs {
		selected[ref] = true
	}
	for _, ref := range rec.Citations {
		if !selected[ref] {
			t.Fatalf("the record cites %q, which the search did not select", ref)
		}
	}
	if !sameJSON(t, rec.Answer, string(got.answer)) {
		t.Errorf("the recorded answer is not the published one:\nrecord: %s\npublished: %s", rec.Answer, got.answer)
	}
}
