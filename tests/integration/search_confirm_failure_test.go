// Task T0909 — the CONFIRMATION's failure path, end to end against REAL
// PostgreSQL.
//
// T0908 built the flow and pinned the happy path: a search answer becomes a
// project plus a draft, confirmation is the only thing that forms the initial
// state, and both routes replay by their key. What it did not pin is what
// happens when a confirmation FAILS — and the two failure paths it has both
// leave the project's state behind on disk:
//
//  1. A confirmation that loses on the confirm key's own unique index
//     (research_context_drafts_confirm_key_uniq: one key names one
//     confirmation). The review of T0908 reproduced this against real
//     PostgreSQL: Alice confirms draft1 with key K, then confirms draft2 with
//     the SAME K. The second call is refused — but by then it has already
//     written branch, genesis state, question state, commit and object version
//     into draft2's project, and draft2 is still 'draft'. The state exists and
//     no confirmation record names it, and the draft's own row is append-only
//     (00134's guard refuses UPDATE except the one confirmation and refuses
//     DELETE outright), so nothing can ever reconcile the two.
//
//  2. A confirmation that never finishes its record (the process died, the
//     connection dropped, the client timed out). The branch it created is
//     named 'main' and branch names are unique per project (00004), so the
//     retry — the thing an Idempotency-Key exists to make safe — lands on
//     BRANCH_NAME_TAKEN, which the flow folds into ErrStore and the transport
//     answers 503 retryable:true. A request that can never succeed is told to
//     try again.
//
// Both are the same defect seen twice: the state write and the record that
// names it are not one unit, and nothing about the second attempt RECOGNIZES
// the state the first one left. The fix (see researchcontext.Service.Confirm)
// reads what the project already has before it writes anything: a used key is
// refused before a single row is written, and a state that an earlier attempt
// left — recognized by the research question its commit carries, on the
// project's main branch — is adopted and recorded rather than written again.
//
// Every assertion below is about the DATABASE, read back with SQL, not about
// the response: "the answer was a 4xx" is not the property under test. "The
// four tables a state transition writes hold zero rows for that project" is.
package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/application/validation"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
	"github.com/lichman0405/post/internal/search/answer/answertest"
)

const (
	// t0909GroundingRef is the ref the scripted answer cites below, and the one
	// the drafts carry. It is the T0908 fixture's grounded ref: the same corpus,
	// the same query and the same citation, so the two files' searches select
	// the same refs.
	t0909GroundingRef = "asset:AST-0001@2"
	// t0909CitedAnswer is the answer document the search below is answered with
	// (citation = the grounded ref), byte-identical in spirit to T0908's.
	t0909CitedAnswer = `{
	  "answer_version": "1",
	  "summary": "Mg-MOF-74 takes up 3.2 mmol/g of CO2 at 298 K [asset:AST-0001@2].",
	  "citations": ["asset:AST-0001@2"]
	}`
)

// t0909Envelope is the error envelope as the contract defines it, including the
// field this task is about: `retryable` is exactly "the server says your
// request may succeed later", and a refusal that can never succeed must not
// carry it (docs/45).
type t0909Envelope struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func t0909EnvelopeOf(t0 interface{ Fatalf(string, ...any) }, raw []byte) t0909Envelope {
	var env t0909Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t0.Fatalf("the error envelope is not JSON: %s", raw)
	}
	return env
}

// t0909StartBody is the start route's contract body for one draft, with the
// slug and the research question this file needs to tell two drafts apart.
func t0909StartBody(slug, question string) string {
	body, err := json.Marshal(map[string]any{
		"name":              slug,
		"slug":              slug,
		"visibility":        "private",
		"research_question": question,
		"referenced_refs":   []string{t0909GroundingRef},
		"candidate_refs":    []string{},
		"uncertainties":     []string{},
		"hypotheses":        []string{},
	})
	if err != nil {
		panic(err)
	}
	return string(body)
}

// t0909Start drives one start-project call and returns the draft and project it
// created. It fails the test rather than returning an empty pair: every caller
// below needs a real draft in a real project for its assertions to be about
// something.
func t0909Start(t *testing.T, uc *testUserClient, searchID, slug, key, question string) (string, string) {
	t.Helper()
	resp, raw := postWithKey(t, uc, "/api/v1/search/"+searchID+":start-project", key, t0909StartBody(slug, question))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("start-project (%s) = %d, want 201: %s", slug, resp.StatusCode, raw)
	}
	var started startProjectWire
	if err := json.Unmarshal(raw, &started); err != nil {
		t.Fatalf("start-project answered a document that is not the contract's: %v: %s", err, raw)
	}
	if started.DraftID == "" || started.ProjectID == "" {
		t.Fatalf("start-project answered no ids: %s", raw)
	}
	return started.DraftID, started.ProjectID
}

// confirmPath is the route both confirm assertions use.
func confirmPath(draftID string) string {
	return "/api/v1/research-context-drafts/" + draftID + ":confirm"
}

// t0909DraftRow is the part of the draft row these tests read back: the record
// the confirmation either wrote or did not.
type t0909DraftRow struct {
	Status        string
	ConfirmKey    *string
	BranchID      *string
	StateID       *string
	CommitID      *string
	ObjectID      *string
	VersionID     *string
	ConfirmedByID *string
}

func t0909ReadDraft(t *testing.T, ctx context.Context, pool *pgxpool.Pool, draftID string) t0909DraftRow {
	t.Helper()
	var row t0909DraftRow
	if err := pool.QueryRow(ctx, `
		SELECT status, confirm_idempotency_key, initial_branch_id::text, initial_state_id::text,
		       initial_commit_id::text, question_object_id::text, question_version_id::text,
		       confirmed_by::text
		FROM research_context_drafts WHERE id = $1::uuid`, draftID).
		Scan(&row.Status, &row.ConfirmKey, &row.BranchID, &row.StateID, &row.CommitID,
			&row.ObjectID, &row.VersionID, &row.ConfirmedByID); err != nil {
		t.Fatalf("read the draft row: %v", err)
	}
	return row
}

// assertNoResearchState fails when the project has ANY of the four rows a state
// transition writes. It is the acceptance criterion's own list, read table by
// table: a refused confirmation must not leave a branch behind, not a state,
// not a commit, not an object version.
func assertNoResearchState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID, because string) {
	t.Helper()
	for _, c := range projectStateCounts(t, ctx, pool, projectID) {
		if c.rows != 0 {
			t.Errorf("%s = %d rows after %s, want 0", c.table, c.rows, because)
		}
	}
}

// ---------------------------------------------------------------------------
// 1. The refusal the review reproduced: one key, two drafts.

// TestConfirmWithAUsedKeyLeavesNoStateE2E is the reproduction, asserted against
// the storage rather than the status line.
//
// Alice confirms draft1 under key K and then presents the SAME K for draft2.
// The contract's answer is 409 IDEMPOTENCY_CONFLICT — one key names one
// confirmation (00134) — and the state of the refused draft's project must be
// exactly what it was before the call: NOTHING. A response of "409" whose
// project has gained a branch, a genesis state, a question state, a commit and
// an object version is the defect, not the fix: the draft is refused and can
// never be confirmed afterwards either, because the retry hits the branch name
// the refused call created.
func TestConfirmWithAUsedKeyLeavesNoStateE2E(t *testing.T) {
	provider := answertest.Reply(t0909CitedAnswer)
	t.Cleanup(provider.Close)
	fx := newStartProjectFixture(t, provider)
	seedAnswerCorpus(t, fx.ctx, fx.pool)

	alice, _ := signup(t, fx.server.URL, "t0909-usedkey-alice@example.com", "t0909-usedkey-alice")
	first := ask(t, alice, `{"query":"CO2 uptake"}`)
	second := ask(t, alice, `{"query":"CO2 uptake"}`)
	if !containsRef(selectedRefsOf(t, fx.ctx, fx.pool, second.doc.SearchID), t0909GroundingRef) {
		t.Fatalf("the search selected %v, which does not include %s",
			selectedRefsOf(t, fx.ctx, fx.pool, second.doc.SearchID), t0909GroundingRef)
	}

	draft1, project1 := t0909Start(t, alice, first.doc.SearchID, "t0909-usedkey-one", "t0909-start-key-one",
		"Which Mg-MOF-74 samples reproduce the reported CO2 uptake?")
	draft2, project2 := t0909Start(t, alice, second.doc.SearchID, "t0909-usedkey-two", "t0909-start-key-two",
		"Which Mg-MOF-74 pellets reproduce the reported CO2 uptake?")

	// The baseline both assertions below are relative to: a started draft's
	// project has no research state at all (T0908's rule, re-read here because
	// "nothing was written" is only a statement about a zero baseline).
	assertNoResearchState(t, fx.ctx, fx.pool, project2, "start-project")

	const sharedKey = "t0909-confirm-key-shared"
	resp, _ := postWithKey(t, alice, confirmPath(draft1), sharedKey, "")
	mustStatus(t, resp, http.StatusCreated)

	// The same key, a different draft.
	resp, raw := postWithKey(t, alice, confirmPath(draft2), sharedKey, "")
	// Not fatal: the storage assertions below are the ones that decide whether
	// this is fixed, and a refusal whose project has gained five rows is the
	// defect even when the status line happens to read 409. (It does not read
	// 409 before the fix — the confirm route exists to answer 409 here and is
	// unreachable, so the caller is told 503 retryable:true instead.)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("confirming a second draft under a used key = %d, want 409: %s", resp.StatusCode, raw)
	}
	env := t0909EnvelopeOf(t, raw)
	if env.Code != "IDEMPOTENCY_CONFLICT" {
		t.Errorf("code = %q, want IDEMPOTENCY_CONFLICT (the store classifies the key's unique index that way, and the confirm route has a branch for it)", env.Code)
	}
	if env.Retryable {
		t.Errorf("retryable = true for a refused confirmation: this key names another draft's confirmation and no number of retries changes that: %+v", env)
	}

	// The heart of it: the refused call wrote nothing into draft2's project.
	assertNoResearchState(t, fx.ctx, fx.pool, project2, "a confirmation refused on the key")
	row := t0909ReadDraft(t, fx.ctx, fx.pool, draft2)
	if row.Status != "draft" || row.ConfirmKey != nil || row.BranchID != nil {
		t.Errorf("the refused draft = %+v, want an untouched draft: no status change and no confirmation record", row)
	}
	// ... and draft1's confirmation is untouched by the refusal.
	if n := tableCount(t, fx.ctx, fx.pool, `SELECT count(*) FROM state_commits WHERE project_id = $1::uuid`, project1); n != 1 {
		t.Errorf("state_commits for the confirmed project = %d, want 1", n)
	}

	// The draft is not stuck: its OWN key confirms it, and then the state is
	// exactly the one a first confirmation makes (one branch, two states — the
	// genesis root and the question — one commit, one object version).
	resp, raw = postWithKey(t, alice, confirmPath(draft2), "t0909-confirm-key-second", "")
	mustStatus(t, resp, http.StatusCreated)
	var confirmed confirmWire
	decodeInto(t, raw, &confirmed)
	if confirmed.BranchID == "" || confirmed.StateCommitID == "" {
		t.Fatalf("the confirmation answered no branch/commit: %s", raw)
	}
	for _, c := range projectStateCounts(t, fx.ctx, fx.pool, project2) {
		want := 1
		if c.table == "project_states" {
			want = 2
		}
		if c.rows != want {
			t.Errorf("%s = %d after the draft's own confirmation, want %d", c.table, c.rows, want)
		}
	}
	row = t0909ReadDraft(t, fx.ctx, fx.pool, draft2)
	if row.Status != "confirmed" || row.ConfirmKey == nil || *row.ConfirmKey != "t0909-confirm-key-second" {
		t.Errorf("the draft = %+v, want confirmed under its own key", row)
	}
	if row.CommitID == nil || *row.CommitID != confirmed.StateCommitID {
		t.Errorf("the draft records commit %v, want the answer's %s", row.CommitID, confirmed.StateCommitID)
	}
}

// ---------------------------------------------------------------------------
// 2. The attempt that never recorded itself.

// TestConfirmCompletesAfterAHalfDoneAttemptE2E is the crash/timeout window, in
// the only shape it can be built deterministically: the branch, the question
// state, the commit and the object version exist and the draft does not name
// them, because the call that wrote them died before its compare-and-swap (or
// never got to run it).
//
// The retry must SUCCEED — not by writing a second state (it cannot: main is
// taken) and not with a 503 that tells the caller to try again forever, but by
// recognizing the state its own earlier attempt left and recording it. The
// assertions below are the two halves: the ids the answer publishes are the
// ones already in the database, and the transition count did not move.
func TestConfirmCompletesAfterAHalfDoneAttemptE2E(t *testing.T) {
	provider := answertest.Reply(t0909CitedAnswer)
	t.Cleanup(provider.Close)
	fx := newStartProjectFixture(t, provider)
	seedAnswerCorpus(t, fx.ctx, fx.pool)

	alice, aliceID := signup(t, fx.server.URL, "t0909-halfdone-alice@example.com", "t0909-halfdone-alice")
	exchange := ask(t, alice, `{"query":"CO2 uptake"}`)
	const question = "Which Mg-MOF-74 samples reproduce the reported CO2 uptake at 298 K?"
	draftID, projectID := t0909Start(t, alice, exchange.doc.SearchID, "t0909-halfdone", "t0909-start-key-halfdone", question)
	assertNoResearchState(t, fx.ctx, fx.pool, projectID, "start-project")

	// The half-done attempt, written through the SAME RSG write path the
	// confirmation uses (a second rsg.Service over the same pool is the same
	// stateless write path — see t0909RSGWriter): main on the project's genesis
	// root, then the research question on it. Nothing records it on the draft.
	writer := t0909RSGWriter(t, fx.pool)
	actor := domain.User{ID: aliceID}
	purpose := question
	branch, err := writer.CreateBranch(fx.ctx, actor, projectID, rsg.CreateBranchInput{
		Name:       domain.MainBranchName,
		Visibility: domain.BranchVisibility(domain.VisibilityPrivate),
		Purpose:    &purpose,
	})
	if err != nil {
		t.Fatalf("the half-done attempt could not create main: %v", err)
	}
	object, err := writer.CreateObject(fx.ctx, actor, projectID, branch.ID, rsg.CreateObjectInput{
		ObjectType: "research_question",
		Payload:    []byte(`{"statement":` + t0909JSONString(question) + `,"question_state":"open"}`),
	})
	if err != nil {
		t.Fatalf("the half-done attempt could not write the question: %v", err)
	}

	// The window, as it exists on disk: state written, nothing recorded.
	for _, c := range projectStateCounts(t, fx.ctx, fx.pool, projectID) {
		want := 1
		if c.table == "project_states" {
			want = 2
		}
		if c.rows != want {
			t.Fatalf("%s = %d after the half-done attempt, want %d", c.table, c.rows, want)
		}
	}
	if row := t0909ReadDraft(t, fx.ctx, fx.pool, draftID); row.Status != "draft" || row.ConfirmKey != nil {
		t.Fatalf("the draft = %+v, want an unrecorded draft (that IS the window)", row)
	}

	// The retry, under the draft's own key.
	resp, raw := postWithKey(t, alice, confirmPath(draftID), "t0909-confirm-after-crash", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("the retry after a half-done attempt = %d, want 201: %s", resp.StatusCode, raw)
	}
	var confirmed confirmWire
	decodeInto(t, raw, &confirmed)
	if confirmed.BranchID != branch.ID {
		t.Errorf("the confirmation answered branch %s, want the one already in the project (%s)", confirmed.BranchID, branch.ID)
	}
	if confirmed.StateID != object.Version.StateID {
		t.Errorf("the confirmation answered state %s, want the state the earlier attempt wrote (%s)", confirmed.StateID, object.Version.StateID)
	}
	if confirmed.ResearchQuestionObjectID != object.Object.ID || confirmed.ResearchQuestionVersionID != object.Version.ID {
		t.Errorf("the confirmation answered object %s@%s, want the earlier attempt's %s@%s",
			confirmed.ResearchQuestionObjectID, confirmed.ResearchQuestionVersionID, object.Object.ID, object.Version.ID)
	}

	// No second transition: the retry RECORDED the state, it did not make one.
	for _, c := range projectStateCounts(t, fx.ctx, fx.pool, projectID) {
		want := 1
		if c.table == "project_states" {
			want = 2
		}
		if c.rows != want {
			t.Errorf("%s = %d after the retry, want %d: the retry wrote a second state", c.table, c.rows, want)
		}
	}

	// And the draft now names it, so a reader reaches the state from the record.
	row := t0909ReadDraft(t, fx.ctx, fx.pool, draftID)
	if row.Status != "confirmed" || row.ConfirmKey == nil || *row.ConfirmKey != "t0909-confirm-after-crash" {
		t.Fatalf("the draft = %+v, want it confirmed under the retry's key", row)
	}
	if row.BranchID == nil || *row.BranchID != branch.ID ||
		row.CommitID == nil || *row.CommitID != confirmed.StateCommitID ||
		row.StateID == nil || *row.StateID != object.Version.StateID {
		t.Errorf("the draft records %v/%v/%v, want the adopted branch/commit/state %s/%s/%s",
			row.BranchID, row.CommitID, row.StateID, branch.ID, confirmed.StateCommitID, object.Version.StateID)
	}
}

// ---------------------------------------------------------------------------
// 2b. The attempt that stopped one step earlier still.

// TestConfirmFinishesABranchAnEarlierAttemptLeftE2E covers the narrower half of
// the same window: the earlier attempt created main and died before the research
// question. On disk that is a branch with nothing on it, and it is the WORST
// shape of the defect — the branch name is unique per project (00004), so
// CreateBranch can never succeed again for that project, which makes the draft
// unconfirmable forever while the project keeps a main branch the draft does not
// name.
//
// The retry finishes the work on the branch that is already there. "Finishes"
// is asserted where it can be seen: one branch (not two, and not zero), the
// question written on THAT branch, and exactly one transition, whose commit is
// the one the draft records.
func TestConfirmFinishesABranchAnEarlierAttemptLeftE2E(t *testing.T) {
	provider := answertest.Reply(t0909CitedAnswer)
	t.Cleanup(provider.Close)
	fx := newStartProjectFixture(t, provider)
	seedAnswerCorpus(t, fx.ctx, fx.pool)

	alice, aliceID := signup(t, fx.server.URL, "t0909-branchonly-alice@example.com", "t0909-branchonly-alice")
	exchange := ask(t, alice, `{"query":"CO2 uptake"}`)
	const question = "Which Mg-MOF-74 samples reproduce the reported CO2 uptake at 250 K?"
	draftID, projectID := t0909Start(t, alice, exchange.doc.SearchID, "t0909-branchonly", "t0909-start-key-branchonly", question)
	assertNoResearchState(t, fx.ctx, fx.pool, projectID, "start-project")

	// The attempt's first step and nothing more: main, created the way the
	// confirmation creates it (empty BaseRef → the project's genesis root), with
	// the draft's question as its purpose. No object, no commit, no record.
	writer := t0909RSGWriter(t, fx.pool)
	actor := domain.User{ID: aliceID}
	purpose := question
	branch, err := writer.CreateBranch(fx.ctx, actor, projectID, rsg.CreateBranchInput{
		Name:       domain.MainBranchName,
		Visibility: domain.BranchVisibility(domain.VisibilityPrivate),
		Purpose:    &purpose,
	})
	if err != nil {
		t.Fatalf("the stopped attempt could not create main: %v", err)
	}
	// The window as it exists on disk: the branch and the genesis state it was
	// created on, and nothing else. This is also the precondition the adoption
	// rule rests on (a branch with no transitions), so it is asserted rather
	// than assumed.
	stopped := map[string]int{"branches": 1, "project_states": 1, "state_commits": 0, "scientific_object_versions": 0}
	for _, c := range projectStateCounts(t, fx.ctx, fx.pool, projectID) {
		if c.rows != stopped[c.table] {
			t.Fatalf("%s = %d after the stopped attempt, want %d", c.table, c.rows, stopped[c.table])
		}
	}
	if row := t0909ReadDraft(t, fx.ctx, fx.pool, draftID); row.Status != "draft" || row.ConfirmKey != nil {
		t.Fatalf("the draft = %+v, want an unrecorded draft (that IS the window)", row)
	}

	// The retry. It has to work on the branch that exists — CreateBranch would
	// be answered BRANCH_NAME_TAKEN, which is the 503 loop the review found.
	resp, raw := postWithKey(t, alice, confirmPath(draftID), "t0909-confirm-after-stopped", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("the retry after a stopped attempt = %d, want 201: %s", resp.StatusCode, raw)
	}
	var confirmed confirmWire
	decodeInto(t, raw, &confirmed)
	if confirmed.BranchID != branch.ID {
		t.Errorf("the confirmation answered branch %s, want the branch the stopped attempt left (%s)", confirmed.BranchID, branch.ID)
	}

	// One branch, one transition, one object version — the ordinary shape of a
	// first confirmation — and the transition is on the branch that was already
	// there.
	for _, c := range projectStateCounts(t, fx.ctx, fx.pool, projectID) {
		want := 1
		if c.table == "project_states" {
			want = 2
		}
		if c.rows != want {
			t.Errorf("%s = %d after the retry, want %d", c.table, c.rows, want)
		}
	}
	if got := textColumn(t, fx.ctx, fx.pool,
		`SELECT branch_id::text FROM state_commits WHERE project_id = $1::uuid`, projectID); got != branch.ID {
		t.Errorf("the transition was committed on branch %s, want the branch the stopped attempt left (%s)", got, branch.ID)
	}

	// And the draft names that branch and that commit.
	row := t0909ReadDraft(t, fx.ctx, fx.pool, draftID)
	if row.Status != "confirmed" || row.ConfirmKey == nil || *row.ConfirmKey != "t0909-confirm-after-stopped" {
		t.Fatalf("the draft = %+v, want it confirmed under the retry's key", row)
	}
	if row.BranchID == nil || *row.BranchID != branch.ID ||
		row.CommitID == nil || *row.CommitID != confirmed.StateCommitID {
		t.Errorf("the draft records %v/%v, want the branch/commit %s/%s",
			row.BranchID, row.CommitID, branch.ID, confirmed.StateCommitID)
	}
}

// ---------------------------------------------------------------------------
// 3. The refusal that is not ours to adopt.

// TestConfirmRefusesAProjectStateItDidNotMakeE2E pins the other side of the
// adoption rule: an existing main branch is adopted only when it carries THIS
// draft's research question. A project whose initial state came from somewhere
// else — the project route plus a state written on it directly, which is what
// the raw-SQL writes below stand in for — must be refused, and refused in a way
// a caller can act on: not a 503 that says "try again", and never a
// confirmation record naming a state this draft did not create.
func TestConfirmRefusesAProjectStateItDidNotMakeE2E(t *testing.T) {
	provider := answertest.Reply(t0909CitedAnswer)
	t.Cleanup(provider.Close)
	fx := newStartProjectFixture(t, provider)
	seedAnswerCorpus(t, fx.ctx, fx.pool)

	alice, aliceID := signup(t, fx.server.URL, "t0909-foreignmain-alice@example.com", "t0909-foreignmain-alice")
	exchange := ask(t, alice, `{"query":"CO2 uptake"}`)

	// A project of Alice's, made through the contract's own route.
	resp, _ := postWithKey(t, alice, "/api/v1/projects", "",
		`{"slug":"t0909-foreign-main","name":"Foreign main","purpose":"someone else's state","visibility":"private"}`)
	mustStatus(t, resp, http.StatusCreated)
	projectID := textColumn(t, fx.ctx, fx.pool, `SELECT id::text FROM projects WHERE slug = 't0909-foreign-main'`)

	// A draft on it, written directly: the refs are empty, so 00134's refs
	// guard has nothing to refuse and the row is the ordinary shape.
	const draftQuestion = "Which Mg-MOF-74 samples reproduce the reported CO2 uptake?"
	var draftID string
	if err := fx.pool.QueryRow(fx.ctx, `
		INSERT INTO research_context_drafts
			(project_id, search_id, created_by, research_question, referenced_refs,
			 dependency_refs, candidate_refs, uncertainties, hypotheses, idempotency_key)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4,
		        ARRAY[]::text[], ARRAY[]::text[], ARRAY[]::text[], ARRAY[]::text[], ARRAY[]::text[],
		        't0909-foreign-main-key')
		RETURNING id::text`, projectID, exchange.doc.SearchID, aliceID, draftQuestion).Scan(&draftID); err != nil {
		t.Fatalf("insert the draft: %v", err)
	}

	// The project's initial state, carrying a question that is NOT the draft's.
	writer := t0909RSGWriter(t, fx.pool)
	actor := domain.User{ID: aliceID}
	other := "Which samples reproduce a different reported uptake?"
	branch, err := writer.CreateBranch(fx.ctx, actor, projectID, rsg.CreateBranchInput{
		Name:       domain.MainBranchName,
		Visibility: domain.BranchVisibility(domain.VisibilityPrivate),
		Purpose:    &other,
	})
	if err != nil {
		t.Fatalf("create main: %v", err)
	}
	if _, err := writer.CreateObject(fx.ctx, actor, projectID, branch.ID, rsg.CreateObjectInput{
		ObjectType: "research_question",
		Payload:    []byte(`{"statement":` + t0909JSONString(other) + `,"question_state":"open"}`),
	}); err != nil {
		t.Fatalf("write the foreign question: %v", err)
	}

	resp, raw := postWithKey(t, alice, confirmPath(draftID), "t0909-foreign-main-confirm", "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("confirming a draft whose project already has someone else's state = %d, want 409: %s", resp.StatusCode, raw)
	}
	if env := t0909EnvelopeOf(t, raw); env.Code != "RESEARCH_CONTEXT_DRAFT_NOT_CONFIRMABLE" {
		t.Errorf("code = %q, want RESEARCH_CONTEXT_DRAFT_NOT_CONFIRMABLE: the draft cannot be confirmed in the project's state", env.Code)
	}
	// Refused, and nothing added: the counts are still the foreign state's own,
	// and the draft is still an unconfirmed draft with no record.
	for _, c := range projectStateCounts(t, fx.ctx, fx.pool, projectID) {
		want := 1
		if c.table == "project_states" {
			want = 2
		}
		if c.rows != want {
			t.Errorf("%s = %d after the refusal, want %d: the refusal wrote state", c.table, c.rows, want)
		}
	}
	if row := t0909ReadDraft(t, fx.ctx, fx.pool, draftID); row.Status != "draft" || row.ConfirmKey != nil {
		t.Errorf("the refused draft = %+v, want it untouched", row)
	}
}

// ---------------------------------------------------------------------------
// Helpers

// t0909JSONString renders s as a JSON string literal, so a payload built by
// concatenation in a test is still a document the schema validator will accept.
func t0909JSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// t0909RSGWriter builds the RSG write path a second time, over the same pool
// the fixture's API uses. The fixture does not hand its own rsg.Service out (it
// is T0908's, and this task leaves that file alone), and a service is
// stateless: a second one over the same database is the same write path, with
// the same authorization, validation and commit machinery, which is what makes
// "the state an earlier attempt left" a state the production path really
// produces.
func t0909RSGWriter(t *testing.T, pool *pgxpool.Pool) *rsg.Service {
	t.Helper()
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: persistence.NewProjectStore(pool),
		Orgs:  persistence.NewOrgStore(pool),
		Authz: authz.NewMatrixEngine(),
	})
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	stateStore := persistence.NewStateStore(pool)
	return rsg.NewService(rsg.Deps{
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
}
