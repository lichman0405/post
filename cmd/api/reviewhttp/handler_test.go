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
}

func (f *fakeService) SubmitReview(_ context.Context, actor domain.User, projectID string, number int64, in reviews.SubmitReviewInput) (domain.Review, error) {
	f.actor, f.projectID, f.number, f.in = actor, projectID, number, in
	return f.out, f.err
}

// newReviewTestServer composes the auth surface + review routes like the
// rsghttp tests, and returns the server, the authed client, the signed-up
// user id and the CSRF token the writes must echo.
func newReviewTestServer(t *testing.T, svc Service) (*httptest.Server, *http.Client, string, string) {
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
	New(Deps{Service: svc}).Register(mux)
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
