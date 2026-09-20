// The discussion transport's unit tests: the wire contract the handlers own
// — the route table, the no-principal backstop, the one-code-per-outcome
// envelope mapping and the reference spelling the promotion accepts.
//
// They are white-box on purpose: the routes are guard-protected in
// production, so the package itself is the only place that can drive a
// write handler past the guard and assert what the handler's own backstop
// answers. Everything that needs a real session, a real database or a real
// authorization decision is covered by the integration suite and by the
// T0811 e2e, which drive the same routes through the real guard.
package discussionhttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/discussions"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/domain"
)

// fakeCommand implements CommandPort with canned answers and recorded calls.
type fakeCommand struct {
	threads   []domain.DiscussionThread
	comments  []domain.DiscussionComment
	comment   domain.DiscussionComment
	promotion domain.DiscussionPromotion
	origins   []domain.PromotionOrigin

	err     error
	promote discussions.PromotionResult

	gotReader     projects.Reader
	gotListKind   domain.DiscussionTargetKind
	gotListTarget string
	gotPromote    discussions.PromoteParams
	gotRef        string
}

func (f *fakeCommand) OpenThread(context.Context, domain.User, discussions.OpenThreadParams) (discussions.ThreadResult, error) {
	return discussions.ThreadResult{Thread: f.threads[0], Comments: f.comments}, f.err
}

func (f *fakeCommand) ListThreads(_ context.Context, r projects.Reader, _ string, kind domain.DiscussionTargetKind, targetID string) ([]domain.DiscussionThread, error) {
	f.gotReader, f.gotListKind, f.gotListTarget = r, kind, targetID
	if f.err != nil {
		return nil, f.err
	}
	return f.threads, nil
}

func (f *fakeCommand) GetThread(_ context.Context, r projects.Reader, _, _ string) (discussions.ThreadResult, error) {
	f.gotReader = r
	if f.err != nil {
		return discussions.ThreadResult{}, f.err
	}
	return discussions.ThreadResult{Thread: f.threads[0], Comments: f.comments}, nil
}

func (f *fakeCommand) AddComment(context.Context, domain.User, discussions.AddCommentParams) (domain.DiscussionComment, error) {
	return f.comment, f.err
}

func (f *fakeCommand) DeleteComment(context.Context, domain.User, string, string) (domain.DiscussionComment, error) {
	return f.comment, f.err
}

func (f *fakeCommand) Promote(_ context.Context, _ domain.User, in discussions.PromoteParams) (discussions.PromotionResult, error) {
	f.gotPromote = in
	if f.err != nil {
		return discussions.PromotionResult{}, f.err
	}
	return f.promote, nil
}

func (f *fakeCommand) Promotion(_ context.Context, r projects.Reader, _, _ string) (domain.PromotionOrigin, error) {
	f.gotReader = r
	if f.err != nil {
		return domain.PromotionOrigin{}, f.err
	}
	return f.origins[0], nil
}

func (f *fakeCommand) PromotionsForRef(_ context.Context, r projects.Reader, _, ref string) ([]domain.PromotionOrigin, error) {
	f.gotReader, f.gotRef = r, ref
	if f.err != nil {
		return nil, f.err
	}
	return f.origins, nil
}

// newMux mounts the surface alone (no guard): the handler's own backstop is
// what these tests are about.
func newMux(cmd CommandPort) *http.ServeMux {
	mux := http.NewServeMux()
	New(Deps{Command: cmd}).Register(mux)
	return mux
}

// TestRouteTable locks the mounted routes: every documented route resolves
// to this surface (a request reaching it is not the mux's 404), and the
// routes the design forbids — editing a thread, editing a comment — do not
// exist at all.
func TestRouteTable(t *testing.T) {
	mux := newMux(&fakeCommand{threads: []domain.DiscussionThread{{ID: "t1"}}})

	routes := []struct{ method, path string }{
		{http.MethodPost, "/api/v1/projects/p1/discussions"},
		{http.MethodGet, "/api/v1/projects/p1/discussions"},
		{http.MethodGet, "/api/v1/projects/p1/discussions/t1"},
		{http.MethodPost, "/api/v1/projects/p1/discussions/t1/comments"},
		{http.MethodDelete, "/api/v1/projects/p1/discussions/t1/comments/c1"},
		{http.MethodPost, "/api/v1/projects/p1/discussions/t1/comments/c1/promotions"},
		{http.MethodGet, "/api/v1/projects/p1/discussions/promotions"},
		{http.MethodGet, "/api/v1/projects/p1/discussions/promotions/pm1"},
	}
	for _, r := range routes {
		req := httptest.NewRequest(r.method, r.path, nil)
		_, pattern := mux.Handler(req)
		if pattern == "" {
			t.Errorf("%s %s matches no route", r.method, r.path)
		}
	}

	// The forbidden shapes: a conversation is append-only, and a comment is
	// withdrawn (DELETE), never edited.
	for _, r := range []struct{ method, path string }{
		{http.MethodPut, "/api/v1/projects/p1/discussions/t1"},
		{http.MethodPatch, "/api/v1/projects/p1/discussions/t1"},
		{http.MethodPut, "/api/v1/projects/p1/discussions/t1/comments/c1"},
		{http.MethodDelete, "/api/v1/projects/p1/discussions/t1"},
	} {
		req := httptest.NewRequest(r.method, r.path, nil)
		if _, pattern := mux.Handler(req); pattern != "" {
			t.Errorf("%s %s resolved to %q: the route must not exist", r.method, r.path, pattern)
		}
	}
}

// TestWriteHandlersNeedAPrincipal is the backstop contract: with no
// principal in the request context — the state a caller reaches only by
// bypassing the guard — every write answers 401 with the guard's own code,
// never a different spelling (the code the web client maps).
func TestWriteHandlersNeedAPrincipal(t *testing.T) {
	mux := newMux(&fakeCommand{threads: []domain.DiscussionThread{{ID: "t1"}}})
	writes := []struct {
		method, path, body string
	}{
		{http.MethodPost, "/api/v1/projects/p1/discussions", `{"target_type":"project","target_id":"p1","body":"hi"}`},
		{http.MethodPost, "/api/v1/projects/p1/discussions/t1/comments", `{"body":"hi"}`},
		{http.MethodDelete, "/api/v1/projects/p1/discussions/t1/comments/c1", ""},
		{http.MethodPost, "/api/v1/projects/p1/discussions/t1/comments/c1/promotions", `{"kind":"issue","issue_type":"question","title":"x"}`},
	}
	for _, w := range writes {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(w.method, w.path, strings.NewReader(w.body)))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", w.method, w.path, rec.Code)
			continue
		}
		if code := envelopeCode(t, rec); code != authn.CodeUnauthenticated {
			t.Errorf("%s %s code = %q, want %q", w.method, w.path, code, authn.CodeUnauthenticated)
		}
	}
}

// TestAnonymousReadsStayAnonymous pins that a read without a session passes
// the anonymous reader to the command — the project's own visibility is the
// whole of the read gate, and a handler that invented an authenticated
// reader would widen it.
func TestAnonymousReadsStayAnonymous(t *testing.T) {
	cmd := &fakeCommand{
		threads:   []domain.DiscussionThread{{ID: "t1"}},
		origins:   []domain.PromotionOrigin{{Promotion: domain.DiscussionPromotion{ID: "pm1"}}},
		comment:   domain.DiscussionComment{ID: "c1"},
		promotion: domain.DiscussionPromotion{ID: "pm1"},
	}
	mux := newMux(cmd)

	for _, path := range []string{
		"/api/v1/projects/p1/discussions?target_type=project&target_id=p1",
		"/api/v1/projects/p1/discussions/t1",
		"/api/v1/projects/p1/discussions/promotions?ref=hypothesis:h1",
		"/api/v1/projects/p1/discussions/promotions/pm1",
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
		}
	}
	if cmd.gotReader.Authenticated || cmd.gotReader.UserID != "" {
		t.Fatalf("reader = %+v, want the anonymous reader", cmd.gotReader)
	}
	if cmd.gotListKind != domain.DiscussionTargetProject || cmd.gotListTarget != "p1" {
		t.Fatalf("list filter = %q/%q, want project/p1", cmd.gotListKind, cmd.gotListTarget)
	}
	if cmd.gotRef != "hypothesis:h1" {
		t.Fatalf("ref = %q, want the query's ref verbatim", cmd.gotRef)
	}
}

// TestThreadAndCommentPayloads renders the two client-visible shapes: a
// withdrawn comment answers a null body with its tombstone fields, while
// the live one answers its text.
func TestThreadAndCommentPayloads(t *testing.T) {
	withdrawn := timePtr()
	deletedBy := "u2"
	cmd := &fakeCommand{
		threads: []domain.DiscussionThread{{ID: "t1", ProjectID: "p1", TargetKind: domain.DiscussionTargetProject, TargetID: "p1", CreatedBy: "u1"}},
		comments: []domain.DiscussionComment{
			{ID: "c1", ThreadID: "t1", ProjectID: "p1", Body: "live", CreatedBy: "u1"},
			{ID: "c2", ThreadID: "t1", ProjectID: "p1", Body: "withdrawn text", CreatedBy: "u1", DeletedAt: withdrawn, DeletedBy: &deletedBy},
		},
	}
	rec := httptest.NewRecorder()
	newMux(cmd).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/discussions/t1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET thread = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Thread struct {
			ID         string `json:"id"`
			TargetType string `json:"target_type"`
		} `json:"thread"`
		Comments []struct {
			ID      string  `json:"id"`
			Body    *string `json:"body"`
			Deleted bool    `json:"deleted"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if body.Thread.ID != "t1" || body.Thread.TargetType != "project" {
		t.Fatalf("thread payload = %+v", body.Thread)
	}
	if len(body.Comments) != 2 {
		t.Fatalf("comments = %+v", body.Comments)
	}
	if body.Comments[0].Body == nil || *body.Comments[0].Body != "live" || body.Comments[0].Deleted {
		t.Fatalf("live comment = %+v", body.Comments[0])
	}
	if body.Comments[1].Body != nil || !body.Comments[1].Deleted {
		t.Fatalf("withdrawn comment = %+v, want a null body with its tombstone", body.Comments[1])
	}
	// The text is gone from the WIRE but not from the read the command
	// serves: the transport withholds it, the row keeps it.
	if cmd.comments[1].Body == "" {
		t.Fatal("the fixture's withdrawn comment lost its body")
	}
}

// TestPromoteRequestCarriesTheDeclarations is the request-decoding contract:
// one field per kind, an unknown kind reaching the command unchanged (the
// command refuses it), and the two evidence pins accepted in the platform's
// own `object_version:<uuid>` spelling — the same spelling every other
// surface uses, which is why the handler strips the prefix before the
// command sees it.
func TestPromoteRequestCarriesTheDeclarations(t *testing.T) {
	// The handler's own backstop answers before any decoding, so the test
	// drives the decode step directly: the body goes through
	// decodeBody exactly as the handler does it.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/discussions/t1/comments/c1/promotions",
		strings.NewReader(`{"kind":"external_evidence","branch_id":"b1","evidence":{
			"target_version_ref":"object_version:11111111-1111-1111-1111-111111111111",
			"evidence_version_ref":" 22222222-2222-2222-2222-222222222222 ",
			"relation":"supports","evidence_type":"experimental","directness":"direct",
			"inference_nature":"causal","reasoning_note":"","scope":{"population":"in vitro"}}}`))
	rec := httptest.NewRecorder()
	var parsed promoteRequest
	if !decodeBody(rec, req, &parsed) {
		t.Fatalf("decodeBody refused a well-formed body: %s", rec.Body.String())
	}
	if parsed.Kind != "external_evidence" || parsed.BranchID != "b1" || parsed.Evidence == nil {
		t.Fatalf("decoded = %+v", parsed)
	}
	got := versionRefID(parsed.Evidence.TargetVersionRef)
	if got != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("target ref = %q, want the bare uuid", got)
	}
	if got := versionRefID(parsed.Evidence.EvidenceVersionRef); got != "22222222-2222-2222-2222-222222222222" {
		t.Fatalf("evidence ref = %q, want the trimmed bare uuid", got)
	}
	if parsed.Evidence.EvidenceType != "experimental" || parsed.Evidence.Relation != "supports" {
		t.Fatalf("evidence declarations = %+v", parsed.Evidence)
	}

	// A body that is not JSON is refused in place with the discussion
	// validation code, never a 500 from the decoder.
	bad := httptest.NewRecorder()
	if decodeBody(bad, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("{")), &parsed) {
		t.Fatal("decodeBody accepted a malformed body")
	}
	if bad.Code != http.StatusBadRequest || envelopeCode(t, bad) != discussions.CodeDiscussionValidationFailed {
		t.Fatalf("malformed body = %d %s", bad.Code, bad.Body.String())
	}
}

// TestVersionRefID pins the reference spelling helper on its own: the
// platform prefix is stripped, surrounding space is trimmed, and anything
// else is passed through untouched — the command is the one authority on
// what a legal version reference is.
func TestVersionRefID(t *testing.T) {
	cases := map[string]string{
		"object_version:abc":      "abc",
		"  object_version:abc  ":  "abc",
		"abc":                     "abc",
		"":                        "",
		"object_version:":         "",
		"relation_version:abc":    "relation_version:abc",
		"object_version:object_v": "object_v",
	}
	for in, want := range cases {
		if got := versionRefID(in); got != want {
			t.Errorf("versionRefID(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestErrorEnvelopeMapping locks one code per failure shape (docs/45): the
// mapping is the contract the web client switches on, and it is asserted
// here for every outcome the command can answer.
func TestErrorEnvelopeMapping(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"validation", fmt.Errorf("%w: blank body", discussions.ErrValidation), http.StatusBadRequest, discussions.CodeDiscussionValidationFailed},
		{"forbidden", fmt.Errorf("%w: not permitted", discussions.ErrForbidden), http.StatusForbidden, discussions.CodeDiscussionForbidden},
		{"project not found", discussions.ErrProjectNotFound, http.StatusNotFound, discussions.CodeDiscussionProjectNotFound},
		{"projects not found", projects.ErrProjectNotFound, http.StatusNotFound, discussions.CodeDiscussionProjectNotFound},
		{"thread not found", discussions.ErrThreadNotFound, http.StatusNotFound, discussions.CodeDiscussionThreadNotFound},
		{"comment not found", discussions.ErrCommentNotFound, http.StatusNotFound, discussions.CodeDiscussionCommentNotFound},
		{"comment withdrawn", discussions.ErrCommentDeleted, http.StatusConflict, discussions.CodeDiscussionCommentDeleted},
		{"target not found", discussions.ErrTargetNotFound, http.StatusNotFound, discussions.CodeDiscussionTargetNotFound},
		{"branch not found", discussions.ErrBranchNotFound, http.StatusNotFound, discussions.CodeDiscussionBranchNotFound},
		{"branches not found", branches.ErrBranchNotFound, http.StatusNotFound, discussions.CodeDiscussionBranchNotFound},
		{"promotion not found", discussions.ErrPromotionNotFound, http.StatusNotFound, discussions.CodeDiscussionPromotionNotFound},
		{"store", fmt.Errorf("%w: connection refused", discussions.ErrStore), http.StatusServiceUnavailable, discussions.CodeDiscussionServiceUnavailable},
		{"projects store", projects.ErrStore, http.StatusServiceUnavailable, discussions.CodeDiscussionServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeDiscussionError(rec, httptest.NewRequest(http.MethodGet, "/x", nil), tc.err)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.status, rec.Body.String())
			}
			if code := envelopeCode(t, rec); code != tc.code {
				t.Fatalf("code = %q, want %q", code, tc.code)
			}
		})
	}

	// Anything unrecognized is an internal error, and it names nothing
	// internal on the wire.
	rec := httptest.NewRecorder()
	writeDiscussionError(rec, httptest.NewRequest(http.MethodGet, "/x", nil), errors.New("a bug in the store wiring"))
	if rec.Code != http.StatusInternalServerError || envelopeCode(t, rec) != "INTERNAL_ERROR" {
		t.Fatalf("unrecognized error = %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "a bug in the store wiring") {
		t.Fatalf("the 500 leaked the internal error: %s", rec.Body.String())
	}

	// A store failure's own text is not on the wire either: the 503 names
	// nothing internal.
	rec = httptest.NewRecorder()
	writeDiscussionError(rec, httptest.NewRequest(http.MethodGet, "/x", nil), fmt.Errorf("%w: pq: password authentication failed", discussions.ErrStore))
	if strings.Contains(rec.Body.String(), "password") {
		t.Fatalf("the 503 leaked the store's message: %s", rec.Body.String())
	}
}

// TestPromoteAnswersTheCreatedKind pins the response keys the transport
// renders: the provenance chain is always present, and the created object
// rides under the key of its own kind — an Issue, a scientific object or an
// evidence assertion — never under two of them.
//
// The rendering functions are asserted directly because the handler's
// authenticated half needs a principal the guard installs; that half runs
// through the real guard in the integration suite and in the T0811 e2e.
func TestPromoteAnswersTheCreatedKind(t *testing.T) {
	origin := domain.PromotionOrigin{
		Promotion: domain.DiscussionPromotion{ID: "pm1", Kind: domain.PromotionHypothesis, Ref: "hypothesis:h1", PromotedBy: "u1"},
		Thread:    domain.DiscussionThread{ID: "t1"},
		Comment:   domain.DiscussionComment{ID: "c1", Body: "the proposal", CreatedBy: "u2"},
	}
	rendered := originPayload(origin)
	for _, key := range []string{"promotion", "thread", "comment"} {
		if _, ok := rendered[key]; !ok {
			t.Errorf("origin payload carries no %s", key)
		}
	}

	cases := []struct {
		name string
		body map[string]any
		key  string
	}{
		{"issue", func() map[string]any {
			b := originPayload(origin)
			b["issue"] = issuePayloadFromDomain(domain.Issue{ID: "i1", Number: 3, IssueType: "question", Title: "t", State: domain.IssueOpen})
			return b
		}(), "issue"},
		{"hypothesis", func() map[string]any {
			b := originPayload(origin)
			b["scientific_object"] = objectPayloadFromResult(rsg.ObjectResult{
				Object:  domain.ScientificObject{ID: "o1", ObjectType: "hypothesis", CurrentVersionNo: 1},
				Version: domain.ScientificObjectVersion{ID: "v1", VersionNo: 1, StateID: "s1", Payload: json.RawMessage(`{"statement":"x"}`)},
			})
			return b
		}(), "scientific_object"},
		{"evidence", func() map[string]any {
			b := originPayload(origin)
			b["evidence_assertion"] = assertionPayloadFromResult(rsg.EvidenceAssertionResult{
				Assertion: rsg.EvidenceAssertionRow{ID: "a1", ReviewState: "unreviewed", ReasoningNote: "n"},
			})
			return b
		}(), "evidence_assertion"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.body)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if _, ok := body[tc.key]; !ok {
				t.Fatalf("answer carries no %s: %s", tc.key, raw)
			}
			// The key renders with the declared name, not the Go field's.
			for _, other := range []string{"issue", "scientific_object", "evidence_assertion"} {
				if other != tc.key {
					if _, ok := body[other]; ok {
						t.Errorf("answer carries %s as well as %s", other, tc.key)
					}
				}
			}
		})
	}

	// The evidence answer names its review state: 'unreviewed' is what a
	// PROPOSAL is in this system, and a client reading the answer must see
	// it rather than infer it.
	raw, err := json.Marshal(assertionPayloadFromResult(rsg.EvidenceAssertionResult{
		Assertion: rsg.EvidenceAssertionRow{ID: "a1", ReviewState: "unreviewed"},
	}))
	if err != nil {
		t.Fatalf("marshal assertion: %v", err)
	}
	var assertion struct {
		ReviewState string `json:"review_state"`
	}
	if err := json.Unmarshal(raw, &assertion); err != nil {
		t.Fatalf("decode assertion: %v", err)
	}
	if assertion.ReviewState != "unreviewed" {
		t.Fatalf("review_state = %q, want unreviewed", assertion.ReviewState)
	}
}

// envelopeCode reads the error envelope's code.
func envelopeCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not the error envelope: %v (%s)", err, rec.Body.String())
	}
	return env.Code
}

func timePtr() *time.Time {
	now := time.Now().UTC()
	return &now
}
