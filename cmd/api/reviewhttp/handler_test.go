package reviewhttp

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
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/application/reviews"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/memstore"
)

// The handler tests prove the transport wiring through the REAL guard:
// real signup issues the session, writes additionally require the
// session-bound CSRF token, and the route passes the guarded principal and
// the parsed path values to the reviews service. The fake service records
// what the handlers called it with and answers canned outcomes, so route
// parsing, wire shapes and error mapping are testable without a database.
// The reviews service rules themselves (authorization order, the matrix,
// the responsibility hook) are covered by internal/application/reviews/
// service_test.go.

const reviewTestProjectID = "11111111-2222-4333-8444-555555555555"

// fakeService records the submission and returns the configured outcome.
type fakeService struct {
	actor     domain.User
	projectID string
	number    int64
	in        reviews.SubmitReviewInput
	out       domain.Review
	err       error

	// The list read records its own call and answer.
	listed      []domain.Review
	listErr     error
	listCalls   int
	listProject string
	listNumber  int64
}

func (f *fakeService) SubmitReview(_ context.Context, actor domain.User, projectID string, number int64, in reviews.SubmitReviewInput) (domain.Review, error) {
	f.actor, f.projectID, f.number, f.in = actor, projectID, number, in
	return f.out, f.err
}

func (f *fakeService) List(_ context.Context, projectID string, number int64) ([]domain.Review, error) {
	f.listCalls++
	f.listProject, f.listNumber = projectID, number
	return f.listed, f.listErr
}

// fakeProjectGate is the read gate the list route runs first: nil error
// means "the caller may read this project", anything else is the gate's
// answer (the tests use the projects package's own not-found sentinel).
type fakeProjectGate struct {
	err      error
	gotID    string
	gotReadr projects.Reader
}

func (f *fakeProjectGate) Get(_ context.Context, r projects.Reader, projectID string) (domain.Project, error) {
	f.gotID, f.gotReadr = projectID, r
	if f.err != nil {
		return domain.Project{}, f.err
	}
	return domain.Project{ID: projectID}, nil
}

// newReviewTestServer composes the auth surface + review routes like the
// rsghttp tests, and returns the server, the authed client, the signed-up
// user id and the CSRF token the writes must echo. The read gate is an
// allowing one; the gate's own outcomes are exercised through
// newReviewTestServerWithGate.
func newReviewTestServer(t *testing.T, svc Service) (*httptest.Server, *http.Client, string, string) {
	t.Helper()
	return newReviewTestServerWithGate(t, svc, &fakeProjectGate{})
}

// newReviewTestServerWithGate is newReviewTestServer with the read gate
// the caller chooses (used by the list route's gate assertions).
func newReviewTestServerWithGate(t *testing.T, svc Service, gate ProjectReader) (*httptest.Server, *http.Client, string, string) {
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
	New(Deps{Service: svc, Projects: gate}).Register(mux)
	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	authed := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := authed.Post(ts.URL+"/api/v1/auth/signup", "application/json",
		strings.NewReader(`{"email":"review-handler@example.com","password":"long-enough-password-1","handle":"review-handler","display_name":"Review Handler"}`))
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

// reviewWrite performs one JSON write with the CSRF token attached, exactly
// as the web app sends it.
func reviewWrite(t *testing.T, client *http.Client, method, url, csrf, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func reviewEnvelope(t *testing.T, resp *http.Response) (status int, body []byte, code string) {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("envelope not JSON: %v (body %s)", err, raw)
	}
	return resp.StatusCode, raw, envelope.Code
}

func submitReviewBody(kind, decision string) string {
	return `{"kind":` + strconvQuote(kind) + `,"decision":` + strconvQuote(decision) + `,"body":"needs the sampling rationale"}`
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestHandleSubmitReviewCreated(t *testing.T) {
	svc := &fakeService{out: domain.Review{
		ID: "r-1", PullRequestID: "pr-1", ReviewerID: "u-1",
		Kind: domain.ReviewKindScientific, Decision: domain.ReviewDecisionChangesRequested,
		ReviewedStateID: "s-1", Responsibility: "Experimental Reviewer",
	}}
	ts, authed, userID, csrf := newReviewTestServer(t, svc)
	url := ts.URL + "/api/v1/projects/" + reviewTestProjectID + "/pull-requests/7/reviews"

	resp := reviewWrite(t, authed, http.MethodPost, url, csrf, submitReviewBody("scientific", "changes_requested"))
	status, body, _ := reviewEnvelope(t, resp)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", status, body)
	}
	var payload reviewPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if payload.ID != "r-1" || payload.Kind != "scientific" || payload.Decision != "changes_requested" ||
		payload.Responsibility != "Experimental Reviewer" {
		t.Fatalf("payload wrong: %+v", payload)
	}
	if svc.projectID != reviewTestProjectID || svc.number != 7 || svc.actor.ID != userID {
		t.Fatalf("service got wrong identities: %s %d %s", svc.projectID, svc.number, svc.actor.ID)
	}
	if svc.in.Kind != domain.ReviewKindScientific || svc.in.Decision != domain.ReviewDecisionChangesRequested ||
		svc.in.Body != "needs the sampling rationale" {
		t.Fatalf("service got wrong input: %+v", svc.in)
	}
}

func TestHandleSubmitReviewOutcomes(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"malformed body", "{", nil, http.StatusBadRequest, reviews.CodeValidation},
		{"forbidden", submitReviewBody("scientific", "comment"), reviews.ErrForbidden, http.StatusForbidden, reviews.CodeForbidden},
		{"project hidden", submitReviewBody("scientific", "comment"), projects.ErrProjectNotFound, http.StatusNotFound, projects.CodeProjectNotFound},
		{"PR unknown", submitReviewBody("scientific", "comment"), pullrequests.ErrPullRequestNotFound, http.StatusNotFound, pullrequests.CodePullRequestNotFound},
		{"duplicate decision", submitReviewBody("scientific", "comment"), reviews.ErrAlreadyReviewed, http.StatusConflict, reviews.CodeAlreadyReviewed},
		{"terminal PR", submitReviewBody("scientific", "comment"), &pullrequests.TerminalError{Number: 7, State: domain.PullRequestStateMerged}, http.StatusConflict, pullrequests.CodeTerminal},
		{"validation", submitReviewBody("scientific", "comment"), reviews.ErrValidation, http.StatusBadRequest, reviews.CodeValidation},
		{"store failure", submitReviewBody("scientific", "comment"), errors.New("boom"), http.StatusServiceUnavailable, reviews.CodeUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeService{err: tc.err}
			ts, authed, _, csrf := newReviewTestServer(t, svc)
			resp := reviewWrite(t, authed, http.MethodPost,
				ts.URL+"/api/v1/projects/"+reviewTestProjectID+"/pull-requests/7/reviews", csrf, tc.body)
			status, _, code := reviewEnvelope(t, resp)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d", status, tc.wantStatus)
			}
			if code != tc.wantCode {
				t.Fatalf("code = %q, want %q", code, tc.wantCode)
			}
		})
	}
}

func TestHandleSubmitReviewNonNumericPRID(t *testing.T) {
	svc := &fakeService{}
	ts, authed, _, csrf := newReviewTestServer(t, svc)
	resp := reviewWrite(t, authed, http.MethodPost,
		ts.URL+"/api/v1/projects/"+reviewTestProjectID+"/pull-requests/abc/reviews", csrf,
		submitReviewBody("scientific", "comment"))
	status, _, code := reviewEnvelope(t, resp)
	if status != http.StatusBadRequest || code != reviews.CodeValidation {
		t.Fatalf("status = %d code = %q, want 400 %s", status, code, reviews.CodeValidation)
	}
	if svc.number != 0 {
		t.Fatalf("service called with number = %d on a malformed path", svc.number)
	}
}

func TestHandleSubmitReviewRequiresSession(t *testing.T) {
	svc := &fakeService{}
	ts, _, _, _ := newReviewTestServer(t, svc)
	// A client with no session cookie: the guard answers before routing.
	anon := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp := reviewWrite(t, anon, http.MethodPost,
		ts.URL+"/api/v1/projects/"+reviewTestProjectID+"/pull-requests/7/reviews", "",
		submitReviewBody("scientific", "comment"))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
	if svc.number != 0 {
		t.Errorf("service was called (number = %d) without a session", svc.number)
	}
}

// ---- the reviews list route (T0408) ----

// reviewRead performs one session-carrying GET (reads need no CSRF).
func reviewRead(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestHandleListReviews(t *testing.T) {
	svc := &fakeService{listed: []domain.Review{
		{
			ID: "rev-1", PullRequestID: "pr-1", ReviewerID: "u-bob",
			Kind: domain.ReviewKindScientific, Decision: domain.ReviewDecisionApproved,
			ReviewedStateID: "state-s2", Responsibility: "materials lead",
			Body: "the synthesis route holds", CreatedAt: time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC),
		},
		{
			ID: "rev-2", PullRequestID: "pr-1", ReviewerID: "u-carol",
			Kind: domain.ReviewKindIntegrity, Decision: domain.ReviewDecisionChangesRequested,
			ReviewedStateID: "state-s2", Body: "rights on dataset d-7 unstated",
			CreatedAt: time.Date(2026, 9, 14, 11, 0, 0, 0, time.UTC),
		},
	}}
	gate := &fakeProjectGate{}
	ts, authed, userID, _ := newReviewTestServerWithGate(t, svc, gate)

	resp := reviewRead(t, authed, ts.URL+"/api/v1/projects/"+reviewTestProjectID+"/pull-requests/7/reviews")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	defer resp.Body.Close()
	var got []reviewPayload
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("reviews = %+v, want both dimensions", got)
	}
	if got[0].Kind != string(domain.ReviewKindScientific) || got[0].Decision != string(domain.ReviewDecisionApproved) {
		t.Fatalf("first review = %+v", got[0])
	}
	// The reviewed state rides on the wire: a reader must be able to tell
	// which head a decision judged.
	if got[0].ReviewedStateID != "state-s2" || got[1].ReviewedStateID != "state-s2" {
		t.Fatalf("reviewed states = %q/%q", got[0].ReviewedStateID, got[1].ReviewedStateID)
	}
	if got[1].Kind != string(domain.ReviewKindIntegrity) || got[1].Decision != string(domain.ReviewDecisionChangesRequested) {
		t.Fatalf("second review = %+v", got[1])
	}
	if got[0].ReviewerID != "u-bob" || got[0].Responsibility != "materials lead" {
		t.Fatalf("attribution = %+v", got[0])
	}
	if svc.listCalls != 1 || svc.listProject != reviewTestProjectID || svc.listNumber != 7 {
		t.Fatalf("list called %d times with %q/#%d", svc.listCalls, svc.listProject, svc.listNumber)
	}
	if gate.gotID != reviewTestProjectID {
		t.Fatalf("gate ran for %q", gate.gotID)
	}
	if !gate.gotReadr.Authenticated || gate.gotReadr.UserID != userID {
		t.Fatalf("gate reader = %+v, want the signed-up actor", gate.gotReadr)
	}
}

func TestHandleListReviewsEmptyIsAnArray(t *testing.T) {
	svc := &fakeService{listed: nil}
	ts, authed, _, _ := newReviewTestServer(t, svc)

	resp := reviewRead(t, authed, ts.URL+"/api/v1/projects/"+reviewTestProjectID+"/pull-requests/7/reviews")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	// "No reviews recorded" is an empty array, never null: the client can
	// iterate the answer without a null check.
	if strings.TrimSpace(string(raw)) != "[]" {
		t.Fatalf("body = %s, want []", raw)
	}
}

func TestHandleListReviewsMapsErrors(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"validation", reviews.ErrValidation, http.StatusBadRequest, reviews.CodeValidation},
		{"store failure", errors.New("boom"), http.StatusServiceUnavailable, reviews.CodeUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeService{listErr: tc.err}
			ts, authed, _, _ := newReviewTestServer(t, svc)
			resp := reviewRead(t, authed, ts.URL+"/api/v1/projects/"+reviewTestProjectID+"/pull-requests/7/reviews")
			status, _, code := reviewEnvelope(t, resp)
			if status != tc.wantStatus || code != tc.wantCode {
				t.Fatalf("status = %d code = %q, want %d %q", status, code, tc.wantStatus, tc.wantCode)
			}
		})
	}
}

func TestHandleListReviewsGate(t *testing.T) {
	t.Run("a denied project read hides it and reaches no service", func(t *testing.T) {
		svc := &fakeService{}
		gate := &fakeProjectGate{err: projects.ErrProjectNotFound}
		ts, authed, _, _ := newReviewTestServerWithGate(t, svc, gate)
		resp := reviewRead(t, authed, ts.URL+"/api/v1/projects/"+reviewTestProjectID+"/pull-requests/7/reviews")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
		resp.Body.Close()
		if svc.listCalls != 0 {
			t.Fatalf("the review read ran behind a denied gate (%d calls)", svc.listCalls)
		}
	})
	t.Run("a gate failure is not a 200", func(t *testing.T) {
		svc := &fakeService{}
		gate := &fakeProjectGate{err: errors.New("store down")}
		ts, authed, _, _ := newReviewTestServerWithGate(t, svc, gate)
		resp := reviewRead(t, authed, ts.URL+"/api/v1/projects/"+reviewTestProjectID+"/pull-requests/7/reviews")
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", resp.StatusCode)
		}
		resp.Body.Close()
		if svc.listCalls != 0 {
			t.Fatalf("the review read ran after a failed gate (%d calls)", svc.listCalls)
		}
	})
	t.Run("missing gate wiring fails closed", func(t *testing.T) {
		svc := &fakeService{}
		ts, authed, _, _ := newReviewTestServerWithGate(t, svc, nil)
		resp := reviewRead(t, authed, ts.URL+"/api/v1/projects/"+reviewTestProjectID+"/pull-requests/7/reviews")
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", resp.StatusCode)
		}
		resp.Body.Close()
		if svc.listCalls != 0 {
			t.Fatalf("the review read ran without a gate (%d calls)", svc.listCalls)
		}
	})
}

func TestHandleListReviewsMalformedPRIDNamesNothing(t *testing.T) {
	svc := &fakeService{}
	ts, authed, _, _ := newReviewTestServer(t, svc)
	for _, segment := range []string{"abc", "0", "-3"} {
		resp := reviewRead(t, authed, ts.URL+"/api/v1/projects/"+reviewTestProjectID+"/pull-requests/"+segment+"/reviews")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("segment %q: status = %d, want 404", segment, resp.StatusCode)
		}
		resp.Body.Close()
	}
	if svc.listCalls != 0 {
		t.Fatalf("the review read ran for a malformed segment (%d calls)", svc.listCalls)
	}
}

// TestHandleListReviewsAnonymousIsARead: the guard lets anonymous READS
// through (reads never 401 — the read matrix decides visibility), so the
// list route must hand the gate an anonymous Reader and answer whatever
// the gate decides. The write route is the one the guard stops at the
// door (TestHandleSubmitReviewRequiresSession).
func TestHandleListReviewsAnonymousIsARead(t *testing.T) {
	svc := &fakeService{listed: []domain.Review{{ID: "rev-1", Kind: domain.ReviewKindScientific}}}
	gate := &fakeProjectGate{}
	ts, _, _, _ := newReviewTestServerWithGate(t, svc, gate)
	anon := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp := reviewRead(t, anon, ts.URL+"/api/v1/projects/"+reviewTestProjectID+"/pull-requests/7/reviews")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (an anonymous read is the gate's call, not the guard's)", resp.StatusCode)
	}
	resp.Body.Close()
	if gate.gotReadr.Authenticated || gate.gotReadr.UserID != "" {
		t.Fatalf("gate reader = %+v, want the anonymous reader", gate.gotReadr)
	}
	if svc.listCalls != 1 {
		t.Fatalf("list calls = %d, want 1", svc.listCalls)
	}
}

// TestHandleListReviewsAnonymousDeniedProjectHidesIt: the existence-hiding
// 404 is the gate's answer for a private project the anonymous caller may
// not read — and the service is never reached.
func TestHandleListReviewsAnonymousDeniedProjectHidesIt(t *testing.T) {
	svc := &fakeService{}
	gate := &fakeProjectGate{err: projects.ErrProjectNotFound}
	ts, _, _, _ := newReviewTestServerWithGate(t, svc, gate)
	anon := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp := reviewRead(t, anon, ts.URL+"/api/v1/projects/"+reviewTestProjectID+"/pull-requests/7/reviews")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
	if svc.listCalls != 0 {
		t.Fatalf("the review read ran behind a denied gate (%d calls)", svc.listCalls)
	}
}
