package mergehttp

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
	"github.com/lichman0405/post/internal/application/merge"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/rsg/integrity"
	rsgmerge "github.com/lichman0405/post/internal/rsg/merge"
)

// The handler tests prove the transport through the REAL guard (signup issues
// the session, writes additionally carry the session-bound CSRF token) and the
// real ServeMux patterns: path parsing of the "{number}:merge" segment, the
// required Idempotency-Key, and the mapping of the merge package's own wire
// codes onto statuses. The fake command records what the handler passed it and
// answers canned outcomes — the merge rules themselves are the service's
// (internal/application/merge/service_test.go) and the governance end-to-end
// journey is tests/integration/merge_governance_e2e_test.go.

const (
	mergeTestProjectID = "11111111-2222-4333-8444-555555555555"
	mergeTestKey       = "merge-idempotency-key-0001"
)

// fakeCommand records the call and returns the configured outcome.
type fakeCommand struct {
	actor domain.User
	in    merge.Input
	calls int
	out   *merge.Result
	err   error
}

func (f *fakeCommand) Merge(_ context.Context, actor domain.User, in merge.Input) (*merge.Result, error) {
	f.calls++
	f.actor, f.in = actor, in
	if f.err != nil {
		return nil, f.err
	}
	if f.out == nil {
		return &merge.Result{Number: in.Number, Merge: domain.SemanticMerge{
			ID: "merge-1", ProjectID: in.ProjectID, ResultStateID: "state-1",
			PlanDigest: "sha256:plan", TargetBranchID: "branch-main",
			GitState: domain.GitStatePending, CreatedAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		}}, nil
	}
	return f.out, nil
}

// fakeGate is the project read gate: anything but nil is the gate's answer.
type fakeGate struct {
	err      error
	gotID    string
	gotReadr projects.Reader
}

func (f *fakeGate) Get(_ context.Context, r projects.Reader, projectID string) (domain.Project, error) {
	f.gotID, f.gotReadr = projectID, r
	if f.err != nil {
		return domain.Project{}, f.err
	}
	return domain.Project{ID: projectID}, nil
}

// newMergeTestServer composes the auth surface + the merge route exactly as
// cmd/api does (the guard wraps the v1 subtree), signs a user up and returns
// the server, the authed client, the actor id and the CSRF token.
func newMergeTestServer(t *testing.T, cmd mergeCommand, gate projectReader) (*httptest.Server, *http.Client, string, string) {
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
	})
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	New(Deps{Command: cmd, Projects: gate}).Register(mux)
	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	authed := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := authed.Post(ts.URL+"/api/v1/auth/signup", "application/json",
		strings.NewReader(`{"email":"merge-handler@example.com","password":"long-enough-password-1","handle":"merge-handler","display_name":"Merge Handler"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("signup = %d", resp.StatusCode)
	}
	var payload struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	return ts, authed, payload.User.ID, payload.CSRFToken
}

// mergePost performs one merge write with the CSRF token and the given
// Idempotency-Key ("" sends no key at all).
func mergePost(t *testing.T, client *http.Client, url, csrf, key, body string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(http.MethodPost, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// envelope reads the JSON body and its error code ("" for a success).
func envelope(t *testing.T, resp *http.Response) (status int, body []byte, code string) {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var e struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, raw)
	}
	return resp.StatusCode, raw, e.Code
}

func mergeURL(server *httptest.Server, number string) string {
	return server.URL + "/api/v1/projects/" + mergeTestProjectID + "/pull-requests/" + number + ":merge"
}

// TestMergeRouteMergesOnTheContractPath: the endpoint the contract defines
// answers 200 with the merge it produced, and the command received the actor
// the guard resolved, the parsed number, the key and the optional message.
func TestMergeRouteMergesOnTheContractPath(t *testing.T) {
	cmd := &fakeCommand{}
	gate := &fakeGate{}
	ts, client, actorID, csrf := newMergeTestServer(t, cmd, gate)

	resp := mergePost(t, client, mergeURL(ts, "12"), csrf, mergeTestKey, `{"message":"merge the annealing protocol"}`)
	status, body, code := envelope(t, resp)
	if status != http.StatusOK || code != "" {
		t.Fatalf("merge = %d %s (code %q), want 200", status, body, code)
	}
	var payload mergePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("payload: %v (%s)", err, body)
	}
	if payload.Number != 12 || payload.MergeID != "merge-1" || payload.StateID != "state-1" {
		t.Fatalf("payload = %+v, want the merge it produced", payload)
	}
	if payload.PlanDigest != "sha256:plan" || payload.GitState != string(domain.GitStatePending) {
		t.Fatalf("payload = %+v, want the plan digest and the Git saga state", payload)
	}
	if cmd.calls != 1 {
		t.Fatalf("command calls = %d, want 1", cmd.calls)
	}
	if cmd.actor.ID != actorID {
		t.Fatalf("command actor = %q, want the guarded principal %q", cmd.actor.ID, actorID)
	}
	if cmd.in.ProjectID != mergeTestProjectID || cmd.in.Number != 12 {
		t.Fatalf("command input = %+v, want the path's project and number", cmd.in)
	}
	if cmd.in.IdempotencyKey == nil || *cmd.in.IdempotencyKey != mergeTestKey {
		t.Fatalf("command key = %v, want %q", cmd.in.IdempotencyKey, mergeTestKey)
	}
	if cmd.in.Message != "merge the annealing protocol" {
		t.Fatalf("command message = %q, want the body's message", cmd.in.Message)
	}
	if gate.gotID != mergeTestProjectID || !gate.gotReadr.Authenticated || gate.gotReadr.UserID != actorID {
		t.Fatalf("read gate = id %q reader %+v, want the authenticated caller", gate.gotID, gate.gotReadr)
	}
}

// TestMergeRouteAcceptsNoBody: the contract defines no request body, so the
// common call carries none and still merges.
func TestMergeRouteAcceptsNoBody(t *testing.T) {
	cmd := &fakeCommand{}
	ts, client, _, csrf := newMergeTestServer(t, cmd, &fakeGate{})

	status, body, code := envelope(t, mergePost(t, client, mergeURL(ts, "12"), csrf, mergeTestKey, ""))
	if status != http.StatusOK {
		t.Fatalf("merge without a body = %d %s (code %q)", status, body, code)
	}
	if cmd.calls != 1 || cmd.in.Message != "" {
		t.Fatalf("command calls = %d message = %q, want one call with no message", cmd.calls, cmd.in.Message)
	}
}

// TestMergeRequiresAnIdempotencyKey: the route's Idempotency-Key is
// contract-required (minLength 8). A missing or too-short key is refused
// before the command runs — the merge is never attempted unkeyed.
func TestMergeRequiresAnIdempotencyKey(t *testing.T) {
	for _, tc := range []struct{ name, key string }{
		{"missing", ""},
		{"empty-looking", " "},
		{"too short", "short"},
		{"one below the minimum", "1234567"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &fakeCommand{}
			ts, client, _, csrf := newMergeTestServer(t, cmd, &fakeGate{})
			status, body, code := envelope(t, mergePost(t, client, mergeURL(ts, "12"), csrf, tc.key, ""))
			if status != http.StatusBadRequest || code != merge.CodeValidation {
				t.Fatalf("key %q = %d %s (code %q), want 400 %s", tc.key, status, body, code, merge.CodeValidation)
			}
			if cmd.calls != 0 {
				t.Fatalf("a request with key %q reached the merge command", tc.key)
			}
		})
	}

	// The boundary is inclusive: exactly the minimum length merges.
	cmd := &fakeCommand{}
	ts, client, _, csrf := newMergeTestServer(t, cmd, &fakeGate{})
	if status, body, code := envelope(t, mergePost(t, client, mergeURL(ts, "12"), csrf, "12345678", "")); status != http.StatusOK {
		t.Fatalf("minimum-length key = %d %s (code %q), want 200", status, body, code)
	}
}

// TestMergeRouteOnlyMatchesTheVerbSuffix: the remainder wildcard routes
// everything under the PR prefix to this handler, and the suffix check turns
// the other paths back into 404s. The route is not widened by the trick.
//
// The answer is the mux's own plain-text 404 rather than the JSON envelope,
// exactly as the branch ":validate" route answers a path without its suffix
// (cmd/api/validationhttp): the URL names no route at all, so there is no
// outcome to give a code to.
func TestMergeRouteOnlyMatchesTheVerbSuffix(t *testing.T) {
	for _, segment := range []string{"12", "12:merge-now", "12:review", "merge", "12/13"} {
		t.Run(segment, func(t *testing.T) {
			cmd := &fakeCommand{}
			ts, client, _, csrf := newMergeTestServer(t, cmd, &fakeGate{})
			url := ts.URL + "/api/v1/projects/" + mergeTestProjectID + "/pull-requests/" + segment
			resp := mergePost(t, client, url, csrf, mergeTestKey, "")
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("POST %s = %d %s, want 404", segment, resp.StatusCode, body)
			}
			if cmd.calls != 0 {
				t.Fatalf("POST %s reached the merge command", segment)
			}
		})
	}
}

// TestMergeRouteMergesTheVerbSuffixPath: the positive control for the test
// above — the same request builder with the suffix present does reach the
// command, so the 404s are the suffix check and not a broken request.
func TestMergeRouteMergesTheVerbSuffixPath(t *testing.T) {
	cmd := &fakeCommand{}
	ts, client, _, csrf := newMergeTestServer(t, cmd, &fakeGate{})
	status, body, code := envelope(t, mergePost(t, client, mergeURL(ts, "12"), csrf, mergeTestKey, ""))
	if status != http.StatusOK {
		t.Fatalf("the :merge path = %d %s (code %q), want 200", status, body, code)
	}
	if cmd.calls != 1 {
		t.Fatalf("command calls = %d, want 1", cmd.calls)
	}
}

// TestMergeNumberMustBePositive: a segment that carries the verb but not a
// number is a malformed request, not an unknown route (the intent is
// unambiguous), and the command is never reached.
func TestMergeNumberMustBePositive(t *testing.T) {
	for _, segment := range []string{"0:merge", "-3:merge", "abc:merge"} {
		t.Run(segment, func(t *testing.T) {
			cmd := &fakeCommand{}
			ts, client, _, csrf := newMergeTestServer(t, cmd, &fakeGate{})
			status, body, code := envelope(t, mergePost(t, client, mergeURL(ts, segment), csrf, mergeTestKey, ""))
			if status != http.StatusBadRequest || code != merge.CodeValidation {
				t.Fatalf("number %q = %d %s (code %q), want 400 %s", segment, status, body, code, merge.CodeValidation)
			}
			if cmd.calls != 0 {
				t.Fatalf("number %q reached the merge command", segment)
			}
		})
	}
}

// TestMergeMalformedBodyIsRefused: a body that is not a JSON object is a 400
// before the command runs.
func TestMergeMalformedBodyIsRefused(t *testing.T) {
	cmd := &fakeCommand{}
	ts, client, _, csrf := newMergeTestServer(t, cmd, &fakeGate{})
	status, body, code := envelope(t, mergePost(t, client, mergeURL(ts, "12"), csrf, mergeTestKey, `{"message":`))
	if status != http.StatusBadRequest || code != merge.CodeValidation {
		t.Fatalf("malformed body = %d %s (code %q), want 400", status, body, code)
	}
	if cmd.calls != 0 {
		t.Fatalf("a malformed body reached the merge command")
	}
}

// TestMergeHidesUnreadableProjects: a project the caller cannot read answers
// the same existence-hiding 404 every other project route answers, and the
// command never runs for it.
func TestMergeHidesUnreadableProjects(t *testing.T) {
	cmd := &fakeCommand{}
	ts, client, _, csrf := newMergeTestServer(t, cmd, &fakeGate{err: projects.ErrProjectNotFound})

	status, body, code := envelope(t, mergePost(t, client, mergeURL(ts, "12"), csrf, mergeTestKey, ""))
	if status != http.StatusNotFound || code != projects.CodeProjectNotFound {
		t.Fatalf("merge of an unreadable project = %d %s (code %q), want 404", status, body, code)
	}
	if cmd.calls != 0 {
		t.Fatalf("an unreadable project's merge reached the command")
	}
}

// TestMergeMapsTheCommandsOwnOutcomes: every refusal the merge command
// produces reaches the wire with its own code and status — the transport
// invents nothing and hides nothing.
func TestMergeMapsTheCommandsOwnOutcomes(t *testing.T) {
	blocked := &rsgmerge.Plan{Blockers: []rsgmerge.Blocker{{
		Code: "MERGE_VALIDATION_BRANCH_REQUIRED", TargetID: "obj-1",
		Detail: "the change requires a validation branch",
	}}}
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
		says   string
	}{
		{"forbidden", merge.ErrForbidden, http.StatusForbidden, merge.CodeForbidden, "may not merge"},
		{"validation", merge.ErrValidation, http.StatusBadRequest, merge.CodeValidation, ""},
		{"unknown pr", merge.ErrPullRequestNotFound, http.StatusNotFound, merge.CodePullRequestNotFound, ""},
		{"missing branch", merge.ErrBranchNotFound, http.StatusNotFound, merge.CodeBranchNotFound, ""},
		{"not mergeable", &merge.NotMergeableError{Number: 12, State: domain.PullRequestStateOpen},
			http.StatusConflict, merge.CodeNotMergeable, "only a merge_ready PR merges"},
		{"blocked plan", &merge.BlockedError{Plan: blocked},
			http.StatusConflict, merge.CodeMergeBlocked, "requires a validation branch"},
		{"stale", &merge.StaleMergeError{Reason: "the target branch advanced"},
			http.StatusConflict, merge.CodeMergeStale, "refresh the proposal"},
		{"already merged", &merge.AlreadyMergedError{PullRequestID: "pr-1"},
			http.StatusConflict, merge.CodeAlreadyMerged, "a PR merges once"},
		{"branch not active", &merge.BranchNotActiveError{BranchID: "branch-1"},
			http.StatusConflict, "BRANCH_NOT_ACTIVE", "does not merge"},
		{"policy", &merge.PolicyRefusedError{Rule: domain.RuleMainProtected, Reason: "the rule is absent"},
			http.StatusForbidden, merge.CodePolicyRefused, "does not permit this merge"},
		{"store", merge.ErrStore, http.StatusServiceUnavailable, merge.CodeUnavailable, "nothing was written"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &fakeCommand{err: tc.err}
			ts, client, _, csrf := newMergeTestServer(t, cmd, &fakeGate{})
			status, body, code := envelope(t, mergePost(t, client, mergeURL(ts, "12"), csrf, mergeTestKey, ""))
			if status != tc.status || code != tc.code {
				t.Fatalf("%s = %d %s (code %q), want %d %s", tc.name, status, body, code, tc.status, tc.code)
			}
			if tc.says != "" && !strings.Contains(string(body), tc.says) {
				t.Fatalf("%s body = %s, want it to say %q", tc.name, body, tc.says)
			}
		})
	}
}

// TestMergeGateRefusalCarriesTheReviewReport: the integrity refusal is a 409
// whose message is the report's own explanation — the engine's rendering of
// every failed check with its subject, detail and why — so the author sees
// which checks blocked the merge instead of a generic line. The complete
// report stays on the in-process error (GateRefused.Report); the wire carries
// its rendering.
func TestMergeGateRefusalCarriesTheReviewReport(t *testing.T) {
	report := integrity.Report{
		Kind: "integrity", PRNumber: 12, Verdict: integrity.VerdictBlocked,
		Explanation: "integrity check for PR #12: blocked\n" +
			"- [blocking] schema/schema_payload_conforms on claim v1 (obj-1): " +
			"missing property: assessment (the payload must satisfy the claim's schema)",
		Results: []integrity.Result{{
			Dimension: integrity.DimensionSchema, Check: integrity.CheckSchemaPayloadConforms,
			Severity: integrity.SeverityBlocking, Passed: false,
			Subject: "claim v1 (obj-1)",
			Detail:  "missing property: assessment",
			Why:     "the payload must satisfy the claim's schema",
		}},
	}
	cmd := &fakeCommand{err: &merge.GateRefused{Report: report}}
	ts, client, _, csrf := newMergeTestServer(t, cmd, &fakeGate{})

	status, body, code := envelope(t, mergePost(t, client, mergeURL(ts, "12"), csrf, mergeTestKey, ""))
	if status != http.StatusConflict || code != merge.CodeIntegrityBlocked {
		t.Fatalf("gate refusal = %d %s (code %q), want 409 %s", status, body, code, merge.CodeIntegrityBlocked)
	}
	for _, want := range []string{"missing property: assessment", string(integrity.CheckSchemaPayloadConforms), "blocked"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("gate refusal body = %s, want it to carry %q", body, want)
		}
	}
}

// TestMergeUnmappedErrorIsAnInternalError: an error the transport does not
// know is a 500 with a generic body — never a code invented on the spot, and
// never dependency detail on the wire.
func TestMergeUnmappedErrorIsAnInternalError(t *testing.T) {
	cmd := &fakeCommand{err: errors.New("merge: something nobody mapped")}
	ts, client, _, csrf := newMergeTestServer(t, cmd, &fakeGate{})

	status, body, code := envelope(t, mergePost(t, client, mergeURL(ts, "12"), csrf, mergeTestKey, ""))
	if status != http.StatusInternalServerError || code != "INTERNAL_ERROR" {
		t.Fatalf("unmapped error = %d %s (code %q), want 500 INTERNAL_ERROR", status, body, code)
	}
	if strings.Contains(string(body), "nobody mapped") {
		t.Fatalf("the 500 leaked the internal error: %s", body)
	}
}

// TestMergeReplayIsReportedAsSuch: a repeated Idempotency-Key answers 200 with
// the merge the first call produced and replayed=true — the caller can tell a
// replay from a fresh merge, which is what makes a client retry safe.
func TestMergeReplayIsReportedAsSuch(t *testing.T) {
	cmd := &fakeCommand{out: &merge.Result{
		Number:   12,
		Replayed: true,
		Merge: domain.SemanticMerge{
			ID: "merge-1", ProjectID: mergeTestProjectID, ResultStateID: "state-1",
			TargetBranchID: "branch-main", GitState: domain.GitStateUpdated,
			GitSHA: strPtr("cafe"), CreatedAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		},
	}}
	ts, client, _, csrf := newMergeTestServer(t, cmd, &fakeGate{})

	status, body, code := envelope(t, mergePost(t, client, mergeURL(ts, "12"), csrf, mergeTestKey, ""))
	if status != http.StatusOK || code != "" {
		t.Fatalf("replay = %d %s (code %q), want 200", status, body, code)
	}
	var payload mergePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("payload: %v (%s)", err, body)
	}
	if !payload.Replayed || payload.MergeID != "merge-1" {
		t.Fatalf("payload = %+v, want the replayed merge", payload)
	}
	if payload.GitState != string(domain.GitStateUpdated) {
		t.Fatalf("payload git state = %q, want the first call's completed saga", payload.GitState)
	}
}

func strPtr(s string) *string { return &s }
