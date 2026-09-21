// The Draft Research Context flow's transport tests: the two routes, their
// keys, their dispatch, and the mapping from the flow's outcomes onto the
// wire.
//
// They run against a REAL guard (authhttp.Guard over memstore sessions), as
// cmd/api/projectshttp's do, because the actor this surface passes to the
// command is the one the guard resolved: a fake principal injected into the
// context would test the injection rather than the route. The flow itself is
// faked — what the flow DOES is internal/application/researchcontext's tests
// and the e2e's, and what this file is about is what the transport decides
// before and after it.
package searchhttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/researchcontext"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/persistence/memstore"
)

// draftTestSearchID / draftTestDraftID / draftTestProjectID are the ids the
// fake flow answers with; they are uuids because the wire carries uuids
// everywhere and a fixture that is not one would let a formatting bug through.
const (
	draftTestSearchID  = "5b1c2d3e-4f50-4617-8899-aabbccddeeff"
	draftTestDraftID   = "0a1b2c3d-4e5f-4607-8192-a3b4c5d6e7f8"
	draftTestProjectID = "11223344-5566-4778-899a-bbccddeeff00"
	draftTestBranchID  = "22334455-6677-4889-9aab-ccddeeff0011"
	draftTestCommitID  = "33445566-7788-499a-abbc-ddeeff001122"
	draftTestStateID   = "44556677-8899-4aab-bccd-eeff00112233"
	draftTestObjectID  = "55667788-99aa-4bbc-cdde-eff001122334"
	draftTestVersionID = "66778899-aabb-4ccd-deef-001122334455"
)

// ---------------------------------------------------------------------------
// The fake flow

type fakeDraftCommands struct {
	start    researchcontext.StartResult
	startErr error
	confirm  researchcontext.ConfirmResult
	confErr  error

	startIn   researchcontext.StartInput
	startAs   domain.User
	startCall int
	confirmIn researchcontext.ConfirmInput
	confAs    domain.User
	confCall  int
}

func (f *fakeDraftCommands) Start(_ context.Context, actor domain.User, in researchcontext.StartInput) (researchcontext.StartResult, error) {
	f.startCall++
	f.startAs = actor
	f.startIn = in
	if f.startErr != nil {
		return researchcontext.StartResult{}, f.startErr
	}
	return f.start, nil
}

func (f *fakeDraftCommands) Confirm(_ context.Context, actor domain.User, in researchcontext.ConfirmInput) (researchcontext.ConfirmResult, error) {
	f.confCall++
	f.confAs = actor
	f.confirmIn = in
	if f.confErr != nil {
		return researchcontext.ConfirmResult{}, f.confErr
	}
	return f.confirm, nil
}

// storedDraft is a draft row as the store returns it: unconfirmed, with the
// three ref sets the request chose.
func storedDraft() researchcontext.Draft {
	return researchcontext.Draft{
		ID:               draftTestDraftID,
		ProjectID:        draftTestProjectID,
		SearchID:         draftTestSearchID,
		CreatedBy:        "the actor",
		ResearchQuestion: "Which Mg-MOF-76 samples reproduce the reported CO2 uptake?",
		ReferencedRefs:   []string{"asset:AST-0001@2"},
		DependencyRefs:   []string{},
		CandidateRefs:    []string{"knowledge:KNW-0007@1"},
		Uncertainties:    []string{"the reported uptake is at 1 bar, not 0.15 bar"},
		Hypotheses:       []string{"the discrepancy is a pelletisation artifact"},
		Status:           researchcontext.DraftStatusDraft,
		IdempotencyKey:   "start-key-0001",
		CreatedAt:        time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC),
	}
}

func startedDraft() researchcontext.StartResult {
	return researchcontext.StartResult{
		Project: domain.Project{ID: draftTestProjectID, Slug: "mof-uptake", Name: "MOF uptake"},
		Draft:   storedDraft(),
	}
}

func confirmedDraft() researchcontext.ConfirmResult {
	d := storedDraft()
	at := time.Date(2026, 9, 20, 10, 5, 0, 0, time.UTC)
	by := "the actor"
	d.Status = researchcontext.DraftStatusConfirmed
	d.ConfirmedAt = &at
	d.ConfirmedBy = &by
	d.InitialBranchID = strPtr(draftTestBranchID)
	d.InitialStateID = strPtr(draftTestStateID)
	d.InitialCommitID = strPtr(draftTestCommitID)
	d.QuestionObjectID = strPtr(draftTestObjectID)
	d.QuestionVersionID = strPtr(draftTestVersionID)
	return researchcontext.ConfirmResult{
		Draft: d, BranchID: draftTestBranchID, StateCommitID: draftTestCommitID,
		StateID: draftTestStateID, QuestionObjectID: draftTestObjectID,
		QuestionVersionID: draftTestVersionID,
	}
}

func strPtr(s string) *string { return &s }

// ---------------------------------------------------------------------------
// The server

// draftTestServer is a guard-protected server whose search surface serves the
// two draft routes over the given flow.
type draftTestServer struct {
	ts     *httptest.Server
	client *http.Client
	csrf   string
}

// newDraftTestServer composes the auth surface and the search surface through
// the REAL guard, exactly as cmd/api/main.go does, and signs up one user. The
// flow is the fake; provision may be nil (no queue wired).
func newDraftTestServer(t *testing.T, commands DraftCommands, provision ProvisionEnqueuer) *draftTestServer {
	t.Helper()
	authAPI := authhttp.New(authhttp.Deps{
		Users:    memstore.NewUsers(),
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
	api := New(Deps{
		Scope: &fakeScope{}, Retriever: &fakeRetriever{}, Ranker: &fakeRanker{},
		Answerer: &fakeAnswerer{}, Records: &fakeRecords{},
		Drafts:           commands,
		ProvisionProject: provision,
	})
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	api.Register(mux)
	ts := httptest.NewServer(observability.Middleware(nil)(authAPI.Guard(mux)))
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Post(ts.URL+"/api/v1/auth/signup", "application/json",
		strings.NewReader(`{"email":"draft-flow@example.com","password":"long-enough-password-1","handle":"draft-flow","display_name":"Draft Flow"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("signup = %d", resp.StatusCode)
	}
	var payload struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	return &draftTestServer{ts: ts, client: client, csrf: payload.CSRFToken}
}

// post sends one request as the signed-up user, with the CSRF token every
// state change under the guard needs.
func (s *draftTestServer) post(t *testing.T, path, body string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", s.csrf)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, raw
}

func decodeWire(t *testing.T, raw []byte) wireError {
	t.Helper()
	var body wireError
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("the error envelope is not JSON: %s", raw)
	}
	return body
}

// ---------------------------------------------------------------------------
// The start route

// TestStartProjectCreatesViaTheFlow: the route is mounted on the contract's
// path, the key and the body reach the command, and the 201 carries the draft
// id, the project id and the draft — the contract's own body.
func TestStartProjectCreatesViaTheFlow(t *testing.T) {
	commands := &fakeDraftCommands{start: startedDraft()}
	srv := newDraftTestServer(t, commands, nil)

	resp, raw := srv.post(t, "/api/v1/search/"+draftTestSearchID+":start-project",
		`{"name":"MOF uptake","slug":"mof-uptake","visibility":"private",`+
			`"research_question":"Which Mg-MOF-76 samples reproduce the reported CO2 uptake?",`+
			`"referenced_refs":["asset:AST-0001@2"],"candidate_refs":["knowledge:KNW-0007@1"],`+
			`"uncertainties":["the reported uptake is at 1 bar, not 0.15 bar"],`+
			`"hypotheses":["the discrepancy is a pelletisation artifact"]}`,
		map[string]string{idempotencyHeader: "start-key-0001"})

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d (body %s), want 201", resp.StatusCode, raw)
	}
	if commands.startCall != 1 {
		t.Fatalf("Start called %d times, want 1", commands.startCall)
	}
	if commands.startIn.SearchID != draftTestSearchID {
		t.Errorf("search id = %q, want the path's %q", commands.startIn.SearchID, draftTestSearchID)
	}
	if commands.startIn.IdempotencyKey != "start-key-0001" {
		t.Errorf("key = %q, want the header's", commands.startIn.IdempotencyKey)
	}
	if commands.startAs.ID == "" {
		t.Error("the command was called with no actor: the route did not read the guard's principal")
	}
	if got := commands.startIn.Uncertainties; len(got) != 1 || got[0] != "the reported uptake is at 1 bar, not 0.15 bar" {
		t.Errorf("uncertainties = %v, want the request's own list", got)
	}
	if got := commands.startIn.Hypotheses; len(got) != 1 {
		t.Errorf("hypotheses = %v, want the request's own list", got)
	}
	if got := commands.startIn.Visibility; got != domain.VisibilityPrivate {
		t.Errorf("visibility = %q, want private", got)
	}

	var body struct {
		DraftID   string       `json:"draft_id"`
		ProjectID string       `json:"project_id"`
		Replayed  bool         `json:"replayed"`
		Draft     draftPayload `json:"draft"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("201 body is not JSON: %s", raw)
	}
	if body.DraftID != draftTestDraftID || body.ProjectID != draftTestProjectID {
		t.Errorf("ids = %q/%q, want %q/%q", body.DraftID, body.ProjectID, draftTestDraftID, draftTestProjectID)
	}
	if body.Replayed {
		t.Error("a first create answered replayed=true")
	}
	if body.Draft.Status != researchcontext.DraftStatusDraft {
		t.Errorf("draft status = %q, want draft", body.Draft.Status)
	}
	// An unconfirmed draft renders the confirmation record as null rather
	// than as empty strings: "no initial state yet" is the truth here.
	if body.Draft.InitialBranchID != nil || body.Draft.InitialCommitID != nil {
		t.Errorf("an unconfirmed draft published a confirmation record: %s", raw)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", resp.Header.Get("Cache-Control"))
	}
}

// TestStartProjectEnqueuesProvisioningOnce: the new project is provisioned
// the same way POST /projects provisions one, and a REPLAY provisions
// nothing — nothing was created, so there is nothing to provision.
func TestStartProjectEnqueuesProvisioningOnce(t *testing.T) {
	commands := &fakeDraftCommands{start: startedDraft()}
	var provisioned []string
	srv := newDraftTestServer(t, commands, func(_ context.Context, projectID string) error {
		provisioned = append(provisioned, projectID)
		return nil
	})

	headers := map[string]string{idempotencyHeader: "start-key-0001"}
	if resp, raw := srv.post(t, "/api/v1/search/"+draftTestSearchID+":start-project",
		`{"name":"MOF uptake","slug":"mof-uptake","research_question":"Which Mg-MOF-76 samples reproduce the reported CO2 uptake?"}`,
		headers); resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d (body %s)", resp.StatusCode, raw)
	}
	if len(provisioned) != 1 || provisioned[0] != draftTestProjectID {
		t.Fatalf("provisioned = %v, want exactly [%s]", provisioned, draftTestProjectID)
	}

	// A replay: the same key, the same search, the flow answers replayed.
	replayed := startedDraft()
	replayed.Replayed = true
	commands.start = replayed
	if resp, raw := srv.post(t, "/api/v1/search/"+draftTestSearchID+":start-project",
		`{"name":"MOF uptake","slug":"mof-uptake","research_question":"Which Mg-MOF-76 samples reproduce the reported CO2 uptake?"}`,
		headers); resp.StatusCode != http.StatusCreated {
		t.Fatalf("replay status = %d (body %s)", resp.StatusCode, raw)
	}
	if len(provisioned) != 1 {
		t.Errorf("provisioned = %v after a replay, want no second enqueue", provisioned)
	}
}

// TestProvisioningFailureDoesNotFailTheCreate: the project and the draft are
// committed by the time the hook runs, so a queue that is down costs the
// enqueue (the startup sweep back-fills) and never the answer.
func TestProvisioningFailureDoesNotFailTheCreate(t *testing.T) {
	commands := &fakeDraftCommands{start: startedDraft()}
	srv := newDraftTestServer(t, commands, func(context.Context, string) error {
		return errors.New("redis is down")
	})
	resp, raw := srv.post(t, "/api/v1/search/"+draftTestSearchID+":start-project",
		`{"name":"MOF uptake","slug":"mof-uptake","research_question":"Which Mg-MOF-76 samples reproduce the reported CO2 uptake?"}`,
		map[string]string{idempotencyHeader: "start-key-0001"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d (body %s), want 201 despite the enqueue failure", resp.StatusCode, raw)
	}
}

// TestDraftRoutesNeedAKey: both routes carry the contract's required
// Idempotency-Key, and a missing or short one is refused before the flow is
// asked anything.
func TestDraftRoutesNeedAKey(t *testing.T) {
	cases := []struct {
		name string
		path string
		body string
		keys map[string]string
	}{
		{"start without a key", "/api/v1/search/" + draftTestSearchID + ":start-project",
			`{"name":"MOF uptake","slug":"mof-uptake","research_question":"Which samples reproduce it?"}`, nil},
		{"start with a short key", "/api/v1/search/" + draftTestSearchID + ":start-project",
			`{"name":"MOF uptake","slug":"mof-uptake","research_question":"Which samples reproduce it?"}`,
			map[string]string{idempotencyHeader: "short"}},
		{"confirm without a key", "/api/v1/research-context-drafts/" + draftTestDraftID + ":confirm", `{}`, nil},
		{"confirm with a short key", "/api/v1/research-context-drafts/" + draftTestDraftID + ":confirm", `{}`,
			map[string]string{idempotencyHeader: "nope"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			commands := &fakeDraftCommands{start: startedDraft(), confirm: confirmedDraft()}
			srv := newDraftTestServer(t, commands, nil)
			resp, raw := srv.post(t, tc.path, tc.body, tc.keys)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d (body %s), want 400", resp.StatusCode, raw)
			}
			if got := decodeWire(t, raw); got.Code != researchcontext.CodeValidationFailed {
				t.Errorf("code = %q, want %q", got.Code, researchcontext.CodeValidationFailed)
			}
			if commands.startCall != 0 || commands.confCall != 0 {
				t.Error("the flow was asked a request that is not replayable")
			}
		})
	}
}

// TestStartProjectRefusesABodyThatIsNotTheContracts: the three required
// fields are the contract's; a body that is not JSON at all is refused with
// the same code.
func TestStartProjectRefusesABodyThatIsNotTheContracts(t *testing.T) {
	commands := &fakeDraftCommands{start: startedDraft()}
	srv := newDraftTestServer(t, commands, nil)
	resp, raw := srv.post(t, "/api/v1/search/"+draftTestSearchID+":start-project", `not json`,
		map[string]string{idempotencyHeader: "start-key-0001"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d (body %s), want 400", resp.StatusCode, raw)
	}
	if commands.startCall != 0 {
		t.Error("a body that is not JSON reached the flow")
	}
}

// ---------------------------------------------------------------------------
// The confirm route

// TestConfirmAnswersTheTransition: the 201 carries the branch, the state
// commit and the research_question object version the confirmation produced —
// the trail a reader follows to check what the confirmation did.
func TestConfirmAnswersTheTransition(t *testing.T) {
	commands := &fakeDraftCommands{confirm: confirmedDraft()}
	srv := newDraftTestServer(t, commands, nil)

	resp, raw := srv.post(t, "/api/v1/research-context-drafts/"+draftTestDraftID+":confirm", `{}`,
		map[string]string{idempotencyHeader: "confirm-key-01"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d (body %s), want 201", resp.StatusCode, raw)
	}
	if commands.confirmIn.DraftID != draftTestDraftID || commands.confirmIn.IdempotencyKey != "confirm-key-01" {
		t.Errorf("command input = %+v, want the path's id and the header's key", commands.confirmIn)
	}
	if commands.confAs.ID == "" {
		t.Error("the command was called with no actor")
	}
	var body confirmResponse
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("201 body is not JSON: %s", raw)
	}
	if body.BranchID != draftTestBranchID || body.StateCommitID != draftTestCommitID ||
		body.StateID != draftTestStateID || body.ResearchQuestionVersionID != draftTestVersionID {
		t.Errorf("confirmation trail = %+v, want the flow's own ids", body)
	}
	if body.DraftID != draftTestDraftID || body.ProjectID != draftTestProjectID {
		t.Errorf("ids = %q/%q, want the draft's", body.DraftID, body.ProjectID)
	}
}

// ---------------------------------------------------------------------------
// Dispatch

// TestSuffixDispatchIsNotARouteWidener: the remainder wildcard each collection
// is registered as answers 404 for every suffix it does not own, including a
// suffix addressed to the other collection and a remainder that crosses a
// segment boundary. Without this the wildcard would be a way to reach a
// handler with a path that names no object.
func TestSuffixDispatchIsNotARouteWidener(t *testing.T) {
	commands := &fakeDraftCommands{start: startedDraft(), confirm: confirmedDraft()}
	srv := newDraftTestServer(t, commands, nil)

	paths := []string{
		"/api/v1/search/" + draftTestSearchID + ":merge",
		"/api/v1/search/" + draftTestSearchID + ":confirm",
		"/api/v1/search/:start-project",
		"/api/v1/search/a/b:start-project",
		"/api/v1/research-context-drafts/" + draftTestDraftID + ":start-project",
		"/api/v1/research-context-drafts/:confirm",
		"/api/v1/research-context-drafts/a/b:confirm",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			resp, raw := srv.post(t, path, `{}`, map[string]string{idempotencyHeader: "some-key-0001"})
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("POST %s = %d (body %s), want 404", path, resp.StatusCode, raw)
			}
		})
	}
	if commands.startCall != 0 || commands.confCall != 0 {
		t.Errorf("an unknown suffix reached the flow (start %d, confirm %d)", commands.startCall, commands.confCall)
	}
}

// TestSearchIDIsNotShapeCheckedHere: the suffix is the transport's, the ID is
// the flow's. An id that names no record is refused by the application (409
// SEARCH_NOT_OWNED, see TestDraftErrorMapping) with the same answer another
// actor's record gets, so a shape check here would add a second, weaker copy
// of a rule whose whole point is that it cannot be probed.
func TestSearchIDIsNotShapeCheckedHere(t *testing.T) {
	commands := &fakeDraftCommands{start: startedDraft()}
	srv := newDraftTestServer(t, commands, nil)
	resp, raw := srv.post(t, "/api/v1/search/not-a-uuid:start-project",
		`{"name":"MOF uptake","slug":"mof-uptake","research_question":"Which samples reproduce it?"}`,
		map[string]string{idempotencyHeader: "start-key-0001"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d (body %s): the transport shape-checked the search id", resp.StatusCode, raw)
	}
	if commands.startIn.SearchID != "not-a-uuid" {
		t.Errorf("search id = %q, want the path's own", commands.startIn.SearchID)
	}
}

// ---------------------------------------------------------------------------
// Error mapping

// TestDraftErrorMapping pins the code and status each outcome is answered
// with. The two mappings that hide existence are the ones worth having here:
// an unknown search and another actor's search answer the SAME 409, and a
// draft the caller may not confirm and one that does not exist answer the
// SAME 404.
func TestDraftErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"validation", researchcontext.ErrValidation, http.StatusBadRequest, researchcontext.CodeValidationFailed},
		{"ungrounded ref", &researchcontext.UngroundedRefError{Ref: "asset:AST-9999@1"},
			http.StatusBadRequest, researchcontext.CodeValidationFailed},
		{"unknown search", researchcontext.ErrSearchNotFound, http.StatusConflict, researchcontext.CodeSearchNotOwned},
		{"another actor's search", researchcontext.ErrSearchNotOwned, http.StatusConflict, researchcontext.CodeSearchNotOwned},
		{"already started", researchcontext.ErrAlreadyStarted, http.StatusConflict, researchcontext.CodeAlreadyStarted},
		{"key conflict", researchcontext.ErrIdempotencyConflict, http.StatusConflict, researchcontext.CodeIdempotencyConflict},
		{"draft not found", researchcontext.ErrDraftNotFound, http.StatusNotFound, researchcontext.CodeDraftNotFound},
		{"not confirmable", researchcontext.ErrNotConfirmable, http.StatusConflict, researchcontext.CodeNotConfirmable},
		{"slug taken", projects.ErrSlugTaken, http.StatusConflict, projects.CodeProjectSlugTaken},
		{"create forbidden", projects.ErrForbidden, http.StatusForbidden, projects.CodeProjectForbidden},
		{"project validation", projects.ErrValidation, http.StatusBadRequest, projects.CodeValidationFailed},
		{"store failure", errors.New("database is on fire"), http.StatusServiceUnavailable, researchcontext.CodeServiceUnavailable},
		{"wrapped store failure", errors.Join(researchcontext.ErrStore, errors.New("boom")),
			http.StatusServiceUnavailable, researchcontext.CodeServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			commands := &fakeDraftCommands{startErr: tc.err}
			srv := newDraftTestServer(t, commands, nil)
			resp, raw := srv.post(t, "/api/v1/search/"+draftTestSearchID+":start-project",
				`{"name":"MOF uptake","slug":"mof-uptake","research_question":"Which samples reproduce it?"}`,
				map[string]string{idempotencyHeader: "start-key-0001"})
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d (body %s), want %d", resp.StatusCode, raw, tc.status)
			}
			if got := decodeWire(t, raw); got.Code != tc.code {
				t.Errorf("code = %q, want %q", got.Code, tc.code)
			}
		})
	}
}

// TestUngroundedRefIsNamedInTheSentence: the caller sent the ref, so naming it
// discloses nothing and is the only way the caller learns which one is wrong.
func TestUngroundedRefIsNamedInTheSentence(t *testing.T) {
	commands := &fakeDraftCommands{startErr: &researchcontext.UngroundedRefError{Ref: "asset:AST-9999@1"}}
	srv := newDraftTestServer(t, commands, nil)
	_, raw := srv.post(t, "/api/v1/search/"+draftTestSearchID+":start-project",
		`{"name":"MOF uptake","slug":"mof-uptake","research_question":"Which samples reproduce it?"}`,
		map[string]string{idempotencyHeader: "start-key-0001"})
	body := decodeWire(t, raw)
	if !strings.Contains(body.Message, "asset:AST-9999@1") {
		t.Errorf("message = %q, want the refused ref named", body.Message)
	}
}

// TestAnonymousIsRefusedBeforeTheFlow: the guard is what authenticates in
// production, and the handler checks anyway — the alternative to checking is
// starting a project on behalf of nobody.
func TestAnonymousIsRefusedBeforeTheFlow(t *testing.T) {
	commands := &fakeDraftCommands{start: startedDraft(), confirm: confirmedDraft()}
	svc := newService(Deps{
		Scope: &fakeScope{}, Retriever: &fakeRetriever{}, Ranker: &fakeRanker{},
		Answerer: &fakeAnswerer{}, Records: &fakeRecords{}, Drafts: commands,
	})
	rec := httptest.NewRecorder()
	svc.handleStartProject(rec, httptest.NewRequest(http.MethodPost,
		"/api/v1/search/"+draftTestSearchID+":start-project", strings.NewReader(`{}`)), draftTestSearchID)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	rec = httptest.NewRecorder()
	svc.handleConfirm(rec, httptest.NewRequest(http.MethodPost,
		"/api/v1/research-context-drafts/"+draftTestDraftID+":confirm", strings.NewReader(`{}`)), draftTestDraftID)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("confirm status = %d, want 401", rec.Code)
	}
	if commands.startCall != 0 || commands.confCall != 0 {
		t.Error("an anonymous request reached the flow")
	}
}

// TestDraftSurfaceFailsClosed: a deployment wired without the flow answers
// 503 rather than pretending. The search itself keeps working — the two
// surfaces share a mux, not a dependency.
func TestDraftSurfaceFailsClosed(t *testing.T) {
	srv := newDraftTestServer(t, nil, nil)
	for _, path := range []string{
		"/api/v1/search/" + draftTestSearchID + ":start-project",
		"/api/v1/research-context-drafts/" + draftTestDraftID + ":confirm",
	} {
		resp, raw := srv.post(t, path, `{"name":"n","slug":"s","research_question":"q?"}`,
			map[string]string{idempotencyHeader: "start-key-0001"})
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("POST %s = %d (body %s), want 503", path, resp.StatusCode, raw)
		}
		if got := decodeWire(t, raw); got.Code != researchcontext.CodeServiceUnavailable {
			t.Errorf("code = %q, want %q", got.Code, researchcontext.CodeServiceUnavailable)
		}
	}
}
