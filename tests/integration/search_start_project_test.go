// Task T0908 — Search → Draft Research Context, end to end against REAL
// PostgreSQL.
//
// docs/14:23-25 gives the flow what a draft carries and one rule about when it
// becomes state ("初始状态在用户确认后才形成"), and docs/31:36 lists the whole path
// as Gate E. The two halves of that rule are what this file is about, and
// neither is checkable in a unit test:
//
//  1. An agent's answer never writes research state. The draft route creates a
//     project and a draft row and NOTHING else — asserted by reading the four
//     tables a state transition writes (branches, project_states,
//     state_commits, scientific_object_versions) for the new project and
//     finding zero rows in each. A Go-level assertion ("this path holds no
//     state writer") is a statement about the code; this is a statement about
//     the database, which is what an operator would audit.
//
//  2. Confirmation is the only thing that forms the initial state, and it
//     NAMES the transition: from the state commit the draft records, a reader
//     reaches the branch, the state and the research_question object version.
//
// The corpus, the auth guard, the search route and the pipeline are composed
// exactly as cmd/api/main.go composes them: the search this flow consumes is a
// search that really ran, and its searchId is the one the contract addresses.
package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/cmd/api/searchhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/researchcontext"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/application/validation"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
	"github.com/lichman0405/post/internal/search/answer"
	"github.com/lichman0405/post/internal/search/answer/answertest"
	"github.com/lichman0405/post/internal/search/ranking"
	"github.com/lichman0405/post/internal/search/retrieval"
)

// startProjectTaskID namespaces this file's test databases (docs/66 §3).
const startProjectTaskID = "T0908"

const (
	startSlug     = "mof-uptake-repro"
	startQuestion = "Which Mg-MOF-74 samples reproduce the reported 3.2 mmol/g CO2 uptake at 298 K?"
)

// startProjectBody is the start route's contract body: docs/14:25's lists, with
// the ref the search below really returns.
const startProjectBody = `{
	"name": "MOF uptake reproducibility",
	"slug": "` + startSlug + `",
	"visibility": "private",
	"research_question": "` + startQuestion + `",
	"referenced_refs": ["asset:AST-0001@2"],
	"candidate_refs": [],
	"uncertainties": ["the reported uptake is at 1 bar, and the corpus records no pressure for the sample"],
	"hypotheses": ["the discrepancy is a pelletisation artifact rather than a framework difference"]
}`

// startProjectFixture is one composed API over one migrated database.
type startProjectFixture struct {
	ctx         context.Context
	pool        *pgxpool.Pool
	server      *httptest.Server
	provisioned []string
}

// newStartProjectFixture composes the API the way cmd/api/main.go does and
// serves it behind the real auth guard.
func newStartProjectFixture(t *testing.T, provider answer.Provider) *startProjectFixture {
	t.Helper()
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), startProjectTaskID)

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
		Secure: false,
	})
	orgStore := persistence.NewOrgStore(pool)
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: persistence.NewProjectStore(pool),
		Orgs:  orgStore,
		Authz: authz.NewMatrixEngine(),
	})
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	stateStore := persistence.NewStateStore(pool)
	rsgSvc := rsg.NewService(rsg.Deps{
		Projects:  projectAPI.Service(),
		Branches:  branches.NewService(persistence.NewBranchStore(pool)),
		States:    states.NewService(stateStore, validation.NewGuard(rsgvalidation.NewValidator(reg), persistence.NewValidationTxProbe())),
		Latest:    stateStore,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Profiles:  persistence.NewProfileStore(pool),
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
		Events:    events.Recorder{},
	})

	// The pipeline, exactly as cmd/api/main.go builds it. provider may be nil
	// (the supported "no answer model" deployment, where the answer is the
	// structured fallback and the record cites nothing); the acceptance walk
	// below passes a scripted model so the answer CITES the ref the draft
	// carries, which is what makes the two sets comparable element by element.
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

	searchRecords := persistence.NewSearchRecordStore(pool)
	draftStore := persistence.NewResearchContextStore(pool)
	fixture := &startProjectFixture{ctx: ctx, pool: pool}
	flow := researchcontext.New(researchcontext.Deps{
		SearchRecords: searchRecords,
		DraftStore:    draftStore,
		Projects:      projectAPI.Service(),
		StateWriter:   rsgSvc,
		CommitReader:  draftStore,
		Authz:         authz.NewMatrixEngine(),
	})
	searchAPI := searchhttp.New(searchhttp.Deps{
		Scope:     persistence.NewProjectStore(pool),
		Retriever: searcher,
		Ranker:    ranker,
		Answerer:  answerer,
		Records:   searchRecords,
		Drafts:    flow,
		// cmd/api/main.go's hook is newProvisionJob over the Redis queue; a
		// test has neither, and what is asserted here is only that the HOOK
		// fired for the project the route created and not for a replay.
		ProvisionProject: func(_ context.Context, projectID string) error {
			fixture.provisioned = append(fixture.provisioned, projectID)
			return nil
		},
	})

	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	mux.Handle("/api/v1/projects", projectAPI.Routes())
	mux.Handle("/api/v1/projects/", projectAPI.Routes())
	searchAPI.Register(mux)
	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)
	fixture.server = ts
	return fixture
}

// draftWire is the draft document both routes answer with.
type draftWire struct {
	ID                        string   `json:"id"`
	ProjectID                 string   `json:"project_id"`
	SearchID                  string   `json:"search_id"`
	ResearchQuestion          string   `json:"research_question"`
	ReferencedRefs            []string `json:"referenced_refs"`
	DependencyRefs            []string `json:"dependency_refs"`
	CandidateRefs             []string `json:"candidate_refs"`
	Uncertainties             []string `json:"uncertainties"`
	Hypotheses                []string `json:"hypotheses"`
	Status                    string   `json:"status"`
	ConfirmedAt               *string  `json:"confirmed_at"`
	InitialBranchID           *string  `json:"initial_branch_id"`
	InitialStateID            *string  `json:"initial_state_id"`
	InitialCommitID           *string  `json:"initial_commit_id"`
	ResearchQuestionObjectID  *string  `json:"research_question_object_id"`
	ResearchQuestionVersionID *string  `json:"research_question_version_id"`
}

type startProjectWire struct {
	DraftID   string    `json:"draft_id"`
	ProjectID string    `json:"project_id"`
	Draft     draftWire `json:"draft"`
	Replayed  bool      `json:"replayed"`
}

type confirmWire struct {
	DraftID                   string `json:"draft_id"`
	ProjectID                 string `json:"project_id"`
	BranchID                  string `json:"branch_id"`
	StateCommitID             string `json:"state_commit_id"`
	StateID                   string `json:"state_id"`
	ResearchQuestionObjectID  string `json:"research_question_object_id"`
	ResearchQuestionVersionID string `json:"research_question_version_id"`
	Replayed                  bool   `json:"replayed"`
}

// postWithKey sends one POST carrying the Idempotency-Key header, from a
// browser that already holds a session and a CSRF token.
func postWithKey(t *testing.T, uc *testUserClient, path, key, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, uc.server+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", uc.csrf)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := uc.client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp, []byte(readAll(t, resp))
}

func startProjectBodyWithRef(ref string) string {
	return strings.Replace(startProjectBody, "asset:AST-0001@2", ref, 1)
}

// ---------------------------------------------------------------------------
// The acceptance walk

// TestStartResearchProjectE2E — required test "start project e2e".
//
// One answered search becomes a project plus a draft with NO research state;
// the draft's refs are the search's own; confirmation forms the initial state
// and names the transition; both routes are replayable by their key and only
// by it; and the permission boundary answers "no permission" and "cannot see"
// identically.
func TestStartResearchProjectE2E(t *testing.T) {
	provider := answertest.Reply(`{
	  "answer_version": "1",
	  "summary": "Mg-MOF-74 takes up 3.2 mmol/g of CO2 at 298 K [asset:AST-0001@2].",
	  "citations": ["asset:AST-0001@2"]
	}`)
	t.Cleanup(provider.Close)
	fx := newStartProjectFixture(t, provider)
	seedAnswerCorpus(t, fx.ctx, fx.pool)
	seedSecondCorpusDoc(t, fx.ctx, fx.pool)

	alice, aliceID := signup(t, fx.server.URL, "t0908-alice@example.com", "t0908-alice")

	// --- a real search, through the real route -----------------------------
	exchange := ask(t, alice, `{"query":"CO2 uptake"}`)
	searchID := exchange.doc.SearchID
	if searchID == "" {
		t.Fatal("the search route answered no search_id")
	}
	if len(exchange.doc.Answer.Sources) == 0 {
		t.Fatal("the search recalled nothing: there is no corpus for a draft to be built from")
	}
	selectedRefs := selectedRefsOf(t, fx.ctx, fx.pool, searchID)
	if len(selectedRefs) == 0 {
		t.Fatal("the stored search selected no refs: nothing to build a draft from")
	}
	if len(selectedRefs) != 2 {
		t.Fatalf("the search selected %v, want TWO recalled refs: with one, keeping it and dropping "+
			"the other is not a choice the draft can get wrong", selectedRefs)
	}
	// Which ref the caller keeps is the caller's business (the endpoint takes
	// the lists from the request, and a draft may drop refs the search
	// returned). This fixture keeps exactly the one the ANSWER cited, so the
	// draft's refs and the answer's citations can be compared element by
	// element instead of being the same single element by construction.
	groundedRef := "asset:AST-0001@2"
	if !containsRef(selectedRefs, groundedRef) {
		t.Fatalf("the search selected %v, which does not include the cited ref %s — nothing to keep",
			selectedRefs, groundedRef)
	}
	// The answer's OWN citation set, read from the row rather than from the
	// response: the draft's refs are chosen from the search's refs, and the
	// acceptance asks for the two to be comparable element by element.
	rec := readRecord(t, fx.ctx, fx.pool, searchID)
	if len(rec.Citations) != 1 || rec.Citations[0] != groundedRef {
		t.Fatalf("the record cites %v, want [%s]", rec.Citations, groundedRef)
	}

	// --- start: a project and a draft, and NO research state ---------------
	resp, raw := postWithKey(t, alice, "/api/v1/search/"+searchID+":start-project", "start-key-alice-1", startProjectBody)
	mustStatus(t, resp, http.StatusCreated)
	var started startProjectWire
	decodeInto(t, raw, &started)
	if started.DraftID == "" || started.ProjectID == "" {
		t.Fatalf("start-project answered no ids: %s", raw)
	}
	if started.Replayed {
		t.Error("a first start answered replayed=true")
	}
	if started.Draft.Status != "draft" {
		t.Errorf("draft status = %q, want draft", started.Draft.Status)
	}
	if started.Draft.SearchID != searchID {
		t.Errorf("draft search_id = %q, want the search it was started from", started.Draft.SearchID)
	}
	if started.Draft.ResearchQuestion != startQuestion {
		t.Errorf("research_question = %q, want the caller's verbatim", started.Draft.ResearchQuestion)
	}
	if len(started.Draft.Uncertainties) != 1 || len(started.Draft.Hypotheses) != 1 {
		t.Errorf("uncertainties/hypotheses = %v/%v, want the request's own lists",
			started.Draft.Uncertainties, started.Draft.Hypotheses)
	}
	if len(started.Draft.ReferencedRefs) != 1 || started.Draft.ReferencedRefs[0] != groundedRef {
		t.Errorf("referenced_refs = %v, want the caller's choice from the search's refs", started.Draft.ReferencedRefs)
	}
	// The draft's stored ref set, element by element against the answer's
	// citations: every ref the draft carries is a ref the ANSWER cited, and
	// every ref the answer cited is carried or deliberately dropped — the
	// caller kept this one.
	assertRefsMatchTheAnswer(t, fx.ctx, fx.pool, started.DraftID, rec.Citations)
	if len(fx.provisioned) != 1 || fx.provisioned[0] != started.ProjectID {
		t.Errorf("provisioned = %v, want exactly the new project [%s]", fx.provisioned, started.ProjectID)
	}

	// The acceptance rule, read off the four tables a state transition writes.
	for _, c := range projectStateCounts(t, fx.ctx, fx.pool, started.ProjectID) {
		if c.rows != 0 {
			t.Errorf("%s = %d rows after start-project, want 0: an answered search must not write research state", c.table, c.rows)
		}
	}
	// A count of zero is only meaningful beside the rows it is about.
	if n := tableCount(t, fx.ctx, fx.pool, `SELECT count(*) FROM projects WHERE id = $1::uuid`, started.ProjectID); n != 1 {
		t.Fatalf("the project row = %d, want 1", n)
	}
	if n := tableCount(t, fx.ctx, fx.pool, `SELECT count(*) FROM research_context_drafts WHERE id = $1::uuid`, started.DraftID); n != 1 {
		t.Fatalf("the draft row = %d, want 1", n)
	}
	// The project is planning, which is what docs/14:5 creates.
	if got := textColumn(t, fx.ctx, fx.pool, `SELECT activity_status FROM projects WHERE id = $1::uuid`, started.ProjectID); got != "planning" {
		t.Errorf("activity_status = %q, want planning", got)
	}

	// --- the same key replays, body edits and all --------------------------
	// A replay is a RETRY: the house rule for an Idempotency-Key is that the
	// row the first call wrote is the row the caller is answered with, and
	// the request's own fields are not re-read (knowledgepublish's
	// replayedMatches states the same rule). The altered ref below therefore
	// lands nowhere — it is not stored, and it is not refused either, because
	// nothing was written.
	resp, raw = postWithKey(t, alice, "/api/v1/search/"+searchID+":start-project", "start-key-alice-1", startProjectBodyWithRef("asset:AST-9999@7"))
	mustStatus(t, resp, http.StatusCreated)
	var replay startProjectWire
	decodeInto(t, raw, &replay)
	if !replay.Replayed {
		t.Error("the same key and search did not answer as a replay")
	}
	if replay.DraftID != started.DraftID || replay.ProjectID != started.ProjectID {
		t.Errorf("replay ids = %q/%q, want the first call's %q/%q",
			replay.DraftID, replay.ProjectID, started.DraftID, started.ProjectID)
	}
	if len(replay.Draft.ReferencedRefs) != 1 || replay.Draft.ReferencedRefs[0] != groundedRef {
		t.Errorf("the replayed draft's refs = %v, want the stored ones: a replay must not rewrite the row",
			replay.Draft.ReferencedRefs)
	}
	if n := tableCount(t, fx.ctx, fx.pool, `SELECT count(*) FROM projects WHERE slug = $1`, startSlug); n != 1 {
		t.Errorf("projects with the slug = %d, want 1: the replay created a second project", n)
	}
	if len(fx.provisioned) != 1 {
		t.Errorf("provisioned = %v after a replay, want no second enqueue", fx.provisioned)
	}

	// --- the same search under a DIFFERENT key is refused ------------------
	resp, raw = postWithKey(t, alice, "/api/v1/search/"+searchID+":start-project", "start-key-alice-2", startProjectBody)
	mustStatus(t, resp, http.StatusConflict)
	if got := envelopeOf(t, raw); got.Code != "RESEARCH_CONTEXT_ALREADY_STARTED" {
		t.Errorf("code = %q, want RESEARCH_CONTEXT_ALREADY_STARTED", got.Code)
	}
	if n := tableCount(t, fx.ctx, fx.pool, `SELECT count(*) FROM research_context_drafts WHERE search_id = $1::uuid`, searchID); n != 1 {
		t.Fatalf("drafts for the search = %d, want exactly 1", n)
	}

	// --- confirm: the initial state exists now -----------------------------
	resp, raw = postWithKey(t, alice, "/api/v1/research-context-drafts/"+started.DraftID+":confirm", "confirm-key-alice-1", "")
	mustStatus(t, resp, http.StatusCreated)
	var confirmed confirmWire
	decodeInto(t, raw, &confirmed)
	if confirmed.BranchID == "" || confirmed.StateCommitID == "" {
		t.Fatalf("confirm answered no branch/commit: %s", raw)
	}

	// From the state commit, a reader reaches the branch, the state and the
	// research_question object version (docs/09 §2).
	assertTransitionTrace(t, fx.ctx, fx.pool, confirmed, aliceID)

	// The draft's terminal state: confirmed, readable forever, and recording
	// the transition it caused.
	var (
		status   string
		key      string
		branchID string
		commitID string
		objectID string
		versID   string
		question string
	)
	if err := fx.pool.QueryRow(fx.ctx, `
		SELECT status, confirm_idempotency_key, initial_branch_id::text, initial_commit_id::text,
		       question_object_id::text, question_version_id::text, research_question
		FROM research_context_drafts WHERE id = $1::uuid`, started.DraftID).
		Scan(&status, &key, &branchID, &commitID, &objectID, &versID, &question); err != nil {
		t.Fatalf("read the confirmed draft: %v", err)
	}
	if status != "confirmed" || key != "confirm-key-alice-1" {
		t.Errorf("draft = %q under key %q, want confirmed under the confirming key", status, key)
	}
	if question != startQuestion {
		t.Errorf("the stored question = %q, want the caller's verbatim", question)
	}
	if branchID != confirmed.BranchID || commitID != confirmed.StateCommitID ||
		objectID != confirmed.ResearchQuestionObjectID || versID != confirmed.ResearchQuestionVersionID {
		t.Errorf("the draft records %s/%s/%s/%s, want the answer's %s/%s/%s/%s",
			branchID, commitID, objectID, versID,
			confirmed.BranchID, confirmed.StateCommitID, confirmed.ResearchQuestionObjectID, confirmed.ResearchQuestionVersionID)
	}

	// The state is real now, and the counts say exactly how much of it the
	// confirmation made: ONE branch (main), TWO states — the genesis root the
	// branch was created on (docs/07: a branch with no base gets one) and the
	// state the transition produced — ONE commit, because only the transition
	// is a commit, and ONE object version.
	for _, c := range projectStateCounts(t, fx.ctx, fx.pool, started.ProjectID) {
		var want int
		switch c.table {
		case "branches", "state_commits", "scientific_object_versions":
			want = 1
		case "project_states":
			want = 2
		}
		if c.rows != want {
			t.Errorf("%s = %d after confirm, want %d", c.table, c.rows, want)
		}
	}

	// --- confirm replays by its key, and only by it ------------------------
	resp, raw = postWithKey(t, alice, "/api/v1/research-context-drafts/"+started.DraftID+":confirm", "confirm-key-alice-1", "")
	mustStatus(t, resp, http.StatusCreated)
	var confirmReplay confirmWire
	decodeInto(t, raw, &confirmReplay)
	if !confirmReplay.Replayed || confirmReplay.StateCommitID != confirmed.StateCommitID {
		t.Errorf("confirm replay = %+v, want the first call's commit", confirmReplay)
	}
	if n := tableCount(t, fx.ctx, fx.pool, `SELECT count(*) FROM state_commits WHERE project_id = $1::uuid`, started.ProjectID); n != 1 {
		t.Errorf("state_commits = %d after a replayed confirmation, want 1", n)
	}
	resp, raw = postWithKey(t, alice, "/api/v1/research-context-drafts/"+started.DraftID+":confirm", "confirm-key-alice-2", "")
	mustStatus(t, resp, http.StatusConflict)
	if got := envelopeOf(t, raw); got.Code != "RESEARCH_CONTEXT_DRAFT_NOT_CONFIRMABLE" {
		t.Errorf("code = %q, want RESEARCH_CONTEXT_DRAFT_NOT_CONFIRMABLE", got.Code)
	}

	// --- the permission boundary -------------------------------------------
	// Bob is not a member of this private project. "No permission" and "cannot
	// see" are the same answer, and that answer is the one a draft that does
	// not exist gets.
	bob, _ := signup(t, fx.server.URL, "t0908-bob@example.com", "t0908-bob")
	resp, deniedRaw := postWithKey(t, bob, "/api/v1/research-context-drafts/"+started.DraftID+":confirm", "confirm-key-bob-1", "")
	mustStatus(t, resp, http.StatusNotFound)
	resp, missingRaw := postWithKey(t, bob, "/api/v1/research-context-drafts/"+draftIDNotStored+":confirm", "confirm-key-bob-1", "")
	mustStatus(t, resp, http.StatusNotFound)
	deniedEnv, missingEnv := envelopeOf(t, deniedRaw), envelopeOf(t, missingRaw)
	if deniedEnv != missingEnv {
		t.Errorf("a non-member got %+v and a draft that does not exist got %+v: the two answers must be identical",
			deniedEnv, missingEnv)
	}
	if n := tableCount(t, fx.ctx, fx.pool, `SELECT count(*) FROM branches WHERE project_id = $1::uuid`, started.ProjectID); n != 1 {
		t.Errorf("a refused confirmation wrote a branch (branches = %d)", n)
	}

	// An anonymous caller never reaches the flow at all: the guard answers.
	anon := newTestUserClient(fx.server.URL)
	resp, _ = postWithKey(t, anon, "/api/v1/search/"+searchID+":start-project", "start-key-anon-1", startProjectBody)
	mustStatus(t, resp, http.StatusUnauthorized)
	resp, _ = postWithKey(t, anon, "/api/v1/research-context-drafts/"+started.DraftID+":confirm", "confirm-key-anon-1", "")
	mustStatus(t, resp, http.StatusUnauthorized)

	// --- a search that belongs to someone else -----------------------------
	// Bob's own search, started by Alice: the contract's 409 — and the same
	// answer a search id that names nothing gets.
	bobExchange := ask(t, bob, `{"query":"CO2 uptake"}`)
	resp, raw = postWithKey(t, alice, "/api/v1/search/"+bobExchange.doc.SearchID+":start-project", "start-key-alice-9", startProjectBody)
	mustStatus(t, resp, http.StatusConflict)
	otherEnv := envelopeOf(t, raw)
	resp, raw = postWithKey(t, alice, "/api/v1/search/"+searchIDNotStored+":start-project", "start-key-alice-9", startProjectBody)
	mustStatus(t, resp, http.StatusConflict)
	if missingEnv := envelopeOf(t, raw); missingEnv != otherEnv {
		t.Errorf("another actor's search got %+v and an unknown id got %+v: the two answers must be identical",
			otherEnv, missingEnv)
	}
	if n := tableCount(t, fx.ctx, fx.pool, `SELECT count(*) FROM projects WHERE slug = $1`, startSlug); n != 1 {
		t.Errorf("projects with the slug = %d, want 1: a refused start created one", n)
	}
}

const (
	draftIDNotStored  = "00000000-0000-4000-8000-00000000draft"
	searchIDNotStored = "00000000-0000-4000-8000-0000000search"
)

// TestStartProjectRefusesAnUngroundedRefE2E: a ref the search never returned
// is refused, it costs no project, and the DATABASE refuses it too — for any
// writer, including one that never asks the service.
func TestStartProjectRefusesAnUngroundedRefE2E(t *testing.T) {
	fx := newStartProjectFixture(t, nil)
	seedAnswerCorpus(t, fx.ctx, fx.pool)
	alice, aliceID := signup(t, fx.server.URL, "t0908-refs-alice@example.com", "t0908-refs-alice")
	exchange := ask(t, alice, `{"query":"CO2 uptake"}`)

	resp, raw := postWithKey(t, alice, "/api/v1/search/"+exchange.doc.SearchID+":start-project", "start-key-refs-1",
		startProjectBodyWithRef("asset:AST-9999@7"))
	mustStatus(t, resp, http.StatusBadRequest)
	env := envelopeOf(t, raw)
	if env.Code != "VALIDATION_FAILED" || !strings.Contains(env.Message, "asset:AST-9999@7") {
		t.Errorf("refusal = %+v, want VALIDATION_FAILED naming the refused ref", env)
	}
	if n := tableCount(t, fx.ctx, fx.pool, `SELECT count(*) FROM projects WHERE slug = $1`, startSlug); n != 0 {
		t.Errorf("projects with the slug = %d, want 0: an ungrounded ref must cost no project", n)
	}

	// A ref the search DID return, sent by a second caller who is not the
	// search's actor: the answer is the contract's 409, not a draft.
	bob, _ := signup(t, fx.server.URL, "t0908-refs-bob@example.com", "t0908-refs-bob")
	resp, raw = postWithKey(t, bob, "/api/v1/search/"+exchange.doc.SearchID+":start-project", "start-key-refs-2", startProjectBody)
	mustStatus(t, resp, http.StatusConflict)
	if got := envelopeOf(t, raw); got.Code != "SEARCH_NOT_OWNED" {
		t.Errorf("code = %q, want SEARCH_NOT_OWNED", got.Code)
	}

	// The database's own refusal, for a writer that never asked the service
	// (00134's research_context_draft_refs_guard). The project is created
	// through the contract's own route so the row is a real one.
	resp, raw = postWithKey(t, alice, "/api/v1/projects",
		"", `{"slug":"t0908-raw","name":"Raw","purpose":"raw insert","visibility":"private"}`)
	// The project route needs no key; the empty one above is simply absent.
	_ = raw
	mustStatus(t, resp, http.StatusCreated)
	rawProjectID := textColumn(t, fx.ctx, fx.pool, `SELECT id::text FROM projects WHERE slug = 't0908-raw'`)
	_, err := fx.pool.Exec(fx.ctx, `
		INSERT INTO research_context_drafts
			(project_id, search_id, created_by, research_question, referenced_refs,
			 dependency_refs, candidate_refs, uncertainties, hypotheses, idempotency_key)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'Which samples reproduce it?',
		        ARRAY['asset:AST-9999@7'], ARRAY[]::text[], ARRAY[]::text[], ARRAY[]::text[], ARRAY[]::text[],
		        'raw-writer-key')`,
		rawProjectID, exchange.doc.SearchID, aliceID)
	if err == nil {
		t.Fatal("the database stored a ref the search never returned: 00134's refs guard is not in effect")
	}
	if !strings.Contains(err.Error(), "not returned by") && !strings.Contains(err.Error(), "selected_refs") {
		t.Errorf("the refusal did not come from the refs guard: %v", err)
	}
	// And a writer that keeps the search's own ref IS stored by the same
	// path: the guard refuses a stranger, not the insert itself.
	if _, err := fx.pool.Exec(fx.ctx, `
		INSERT INTO research_context_drafts
			(project_id, search_id, created_by, research_question, referenced_refs,
			 dependency_refs, candidate_refs, uncertainties, hypotheses, idempotency_key)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'Which samples reproduce it?',
		        ARRAY['asset:AST-0001@2'], ARRAY[]::text[], ARRAY[]::text[], ARRAY[]::text[], ARRAY[]::text[],
		        'raw-writer-key-2')`,
		rawProjectID, exchange.doc.SearchID, aliceID); err != nil {
		t.Fatalf("the guard refused a ref the search returned: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Helpers

type projectStateCount struct {
	table string
	rows  int
}

// projectStateCounts reads the four tables a research state transition writes.
// They are the acceptance criterion's own list: a start that wrote state would
// have to write at least one row in one of them.
func projectStateCounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID string) []projectStateCount {
	t.Helper()
	return []projectStateCount{
		{"branches", tableCount(t, ctx, pool, `SELECT count(*) FROM branches WHERE project_id = $1::uuid`, projectID)},
		{"project_states", tableCount(t, ctx, pool, `SELECT count(*) FROM project_states WHERE project_id = $1::uuid`, projectID)},
		{"state_commits", tableCount(t, ctx, pool, `SELECT count(*) FROM state_commits WHERE project_id = $1::uuid`, projectID)},
		{"scientific_object_versions", tableCount(t, ctx, pool,
			`SELECT count(*) FROM scientific_object_versions v
			 JOIN scientific_objects o ON o.id = v.object_id
			 WHERE o.project_id = $1::uuid`, projectID)},
	}
}

func tableCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v (%s)", err, sql)
	}
	return n
}

func textColumn(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
		t.Fatalf("read: %v (%s)", err, sql)
	}
	return s
}

// seedSecondCorpusDoc adds a SECOND public row the same query recalls, so
// "the draft carries the caller's choice from the search's refs" has a choice
// to make. With one recalled ref the correspondence below would hold by
// construction: the draft could only carry the ref the answer cited. The row
// is deliberately the weaker match (a shorter document with the query's terms
// and nothing else), so it recalls but ranks below it.
// containsRef reports whether refs holds ref, which is a question about the
// search's recall rather than about rank order: which ref the caller keeps is
// its own choice, and the ranking's order is asserted where the ranker lives.
func containsRef(refs []string, ref string) bool {
	for _, r := range refs {
		if r == ref {
			return true
		}
	}
	return false
}

func seedSecondCorpusDoc(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO search_documents (entity_ref, entity_type, visibility, title, content, structured)
		VALUES ('asset:AST-0002', 'asset', 'public', 'CO2 uptake survey', 'CO2 uptake.', '{"version":"1"}'::jsonb)`); err != nil {
		t.Fatalf("seed asset:AST-0002: %v", err)
	}
}

// assertRefsMatchTheAnswer holds the draft's stored ref sets against the
// answer's citations, element by element: the draft may only carry what the
// search's answer cited, and (for this fixture, which keeps every one of them)
// it carries all of it.
func assertRefsMatchTheAnswer(t *testing.T, ctx context.Context, pool *pgxpool.Pool, draftID string, citations []string) {
	t.Helper()
	var refs, deps, candidates []string
	if err := pool.QueryRow(ctx, `
		SELECT referenced_refs, dependency_refs, candidate_refs
		FROM research_context_drafts WHERE id = $1::uuid`, draftID).Scan(&refs, &deps, &candidates); err != nil {
		t.Fatalf("read the draft's ref sets: %v", err)
	}
	carried := map[string]int{}
	for _, ref := range append(append(append([]string{}, refs...), deps...), candidates...) {
		carried[ref]++
	}
	for ref := range carried {
		found := false
		for _, cited := range citations {
			if cited == ref {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the draft carries %q, which the answer did not cite (citations %v)", ref, citations)
		}
	}
	for _, cited := range citations {
		if carried[cited] == 0 {
			t.Errorf("the answer cited %q and the draft dropped it: this fixture keeps every ref, so the sets must match", cited)
		}
	}
}

func selectedRefsOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, searchID string) []string {
	t.Helper()
	var refs []string
	if err := pool.QueryRow(ctx, `SELECT selected_refs FROM search_records WHERE id = $1::uuid`, searchID).Scan(&refs); err != nil {
		t.Fatalf("read the search's selected_refs: %v", err)
	}
	return refs
}

func envelopeOf(t *testing.T, raw []byte) errorEnvelope {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("the error envelope is not JSON: %s", raw)
	}
	return env
}

func decodeInto(t *testing.T, raw []byte, into any) {
	t.Helper()
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("the response is not the contract document: %v: %s", err, raw)
	}
}

// assertTransitionTrace follows the confirmation from the state commit down to
// the research_question object version — the traceability criterion, read from
// the tables rather than from the response that named them.
func assertTransitionTrace(t *testing.T, ctx context.Context, pool *pgxpool.Pool, confirmed confirmWire, actorID string) {
	t.Helper()
	var (
		branchID string
		resultID string
		actor    string
		message  string
		via      string
	)
	if err := pool.QueryRow(ctx, `
		SELECT branch_id::text, result_state_id::text, actor_id::text, message, via
		FROM state_commits WHERE id = $1::uuid`, confirmed.StateCommitID).
		Scan(&branchID, &resultID, &actor, &message, &via); err != nil {
		t.Fatalf("read the state commit: %v", err)
	}
	if branchID != confirmed.BranchID {
		t.Errorf("the commit's branch = %s, want the answer's %s", branchID, confirmed.BranchID)
	}
	if resultID != confirmed.StateID {
		t.Errorf("the commit's result state = %s, want the answer's %s", resultID, confirmed.StateID)
	}
	if actor != actorID {
		t.Errorf("the commit's actor = %s, want the confirming user", actor)
	}
	if via != "api" || strings.TrimSpace(message) == "" {
		t.Errorf("the commit = via %q, message %q, want the API path and a message", via, message)
	}
	if hash := textColumn(t, ctx, pool, `SELECT state_hash FROM project_states WHERE id = $1::uuid`, confirmed.StateID); hash == "" {
		t.Error("the result state has no hash")
	}

	// The object version the transition created, reached from the commit's
	// result state rather than from the response.
	var (
		objectID  string
		objectTyp string
		versionID string
		stateID   string
	)
	if err := pool.QueryRow(ctx, `
		SELECT o.id::text, o.object_type, v.id::text, v.state_id::text
		FROM scientific_object_versions v
		JOIN scientific_objects o ON o.id = v.object_id
		WHERE v.state_id = $1::uuid`, confirmed.StateID).Scan(&objectID, &objectTyp, &versionID, &stateID); err != nil {
		t.Fatalf("read the version the confirmation's state carries: %v", err)
	}
	if objectTyp != "research_question" {
		t.Errorf("object type = %q, want research_question", objectTyp)
	}
	if objectID != confirmed.ResearchQuestionObjectID || versionID != confirmed.ResearchQuestionVersionID {
		t.Errorf("the state carries %s@%s, want the answer's %s@%s",
			objectID, versionID, confirmed.ResearchQuestionObjectID, confirmed.ResearchQuestionVersionID)
	}
	if stateID != confirmed.StateID {
		t.Errorf("the version's state = %s, want the confirmation's state", stateID)
	}

	// The branch is the project's main, and the transition is its tip: a
	// branch has no head column, the head IS the newest commit's result
	// (docs/07), so that is what is read.
	var (
		branchName string
		baseState  string
	)
	if err := pool.QueryRow(ctx, `
		SELECT name, COALESCE(base_state_id::text, '') FROM branches WHERE id = $1::uuid`, confirmed.BranchID).
		Scan(&branchName, &baseState); err != nil {
		t.Fatalf("read the initial branch: %v", err)
	}
	if branchName != "main" {
		t.Errorf("branch name = %q, want main", branchName)
	}
	if baseState == "" {
		t.Error("the initial branch has no base state: the confirmation did not create genesis")
	}
	tip := textColumn(t, ctx, pool, `
		SELECT result_state_id::text FROM state_commits
		WHERE branch_id = $1::uuid ORDER BY created_at DESC, id DESC LIMIT 1`, confirmed.BranchID)
	if tip != confirmed.StateID {
		t.Errorf("the branch's tip = %s, want the confirmation's state %s", tip, confirmed.StateID)
	}
}
