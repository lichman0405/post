package feedshttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/feeds"
)

// testPID is a conforming pid (26 lowercase Crockford base32 characters,
// the shape the database's CHECK and assets.ValidPID both enforce). It is
// written out rather than generated so a failure prints the same value twice.
const testPID = "0123456789abcdefghjkmnpqrs"

// The transport's own contract, tested without a database and without the
// real use case: what a request is ANSWERED is this file's business (status,
// content type, validator, negotiation), and what a document CONTAINS is
// internal/application/feeds'. The stub below is deliberately not a feed
// model — it answers fixed bytes — so a failure here can only be about the
// transport.

// stubService is the Service port with canned answers and a call log.
type stubService struct {
	doc []byte
	err error

	calls  int
	gotTgt feeds.Target
	gotFmt feeds.Format
}

var errBoom = errors.New("boom")

func (s *stubService) Document(_ context.Context, target feeds.Target, format feeds.Format) ([]byte, error) {
	s.calls++
	s.gotTgt, s.gotFmt = target, format
	if s.err != nil {
		return nil, s.err
	}
	return s.doc, nil
}

func newTestServer(svc Service) *http.ServeMux {
	mux := http.NewServeMux()
	New(Deps{Service: svc}).Register(mux)
	return mux
}

func do(mux *http.ServeMux, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

const (
	projectID   = "3f1b0c2e-9a44-4c1d-8b7e-2f5a6d0e1c33"
	knowledgeID = "8c7d6e5f-1a2b-4c3d-9e8f-0a1b2c3d4e5f"
)

func TestFeedRoutesAnswerAnonymously(t *testing.T) {
	svc := &stubService{doc: []byte("<feed/>\n")}
	mux := newTestServer(svc)
	pid := testPID

	cases := []struct {
		name string
		path string
		kind feeds.Kind
		id   string
	}{
		{"project", "/api/v1/feeds/projects/" + projectID, feeds.KindProject, projectID},
		{"asset", "/api/v1/feeds/assets/" + pid, feeds.KindAsset, pid},
		{"knowledge", "/api/v1/feeds/knowledge/" + knowledgeID, feeds.KindKnowledge, knowledgeID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// No cookie, no session, no principal: the acceptance criterion is
			// that an anonymous caller can subscribe, so every case here is an
			// unauthenticated GET.
			rec := do(mux, http.MethodGet, tc.path, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("anonymous GET %s = %d, want 200 (body %q)", tc.path, rec.Code, rec.Body.String())
			}
			if svc.gotTgt.Kind != tc.kind || svc.gotTgt.ID != tc.id {
				t.Errorf("target = %+v, want {%s %s}", svc.gotTgt, tc.kind, tc.id)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/atom+xml; charset=utf-8" {
				t.Errorf("default Content-Type = %q, want Atom", got)
			}
			if rec.Body.String() != "<feed/>\n" {
				t.Errorf("body = %q, want the service's bytes verbatim", rec.Body.String())
			}
		})
	}
}

// TestFeedTargetRefusedBeforeAnyRead: a path segment that cannot name a
// stored row is this surface's 404, and it costs no read. The stub's call
// counter is the assertion — a handler that read first and mapped after would
// make every mistyped id (and every hostile one) a database round trip.
func TestFeedTargetRefusedBeforeAnyRead(t *testing.T) {
	svc := &stubService{doc: []byte("<feed/>\n")}
	mux := newTestServer(svc)

	notUUID := []string{
		"/api/v1/feeds/projects/not-a-uuid",
		"/api/v1/feeds/projects/my-project-slug",
		"/api/v1/feeds/knowledge/12345",
		"/api/v1/feeds/assets/my-dataset-slug",
		// A pid one character short of the fixed 26, and one carrying 'u' —
		// the letters Crockford base32 leaves out, which is why a
		// lookalike-proof pids alphabet is not "all lowercase letters".
		"/api/v1/feeds/assets/0123456789abcdefghjkmnpqr",
		"/api/v1/feeds/assets/0123456789abcdefghjkmnpqru",
	}
	for _, path := range notUUID {
		rec := do(mux, http.MethodGet, path, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
		if code := errorCode(t, rec); code != CodeFeedNotFound {
			t.Errorf("GET %s code = %q, want %q", path, code, CodeFeedNotFound)
		}
	}
	if svc.calls != 0 {
		t.Errorf("service called %d times for unnameable targets, want 0", svc.calls)
	}
}

// TestFeedErrorsAreIndistinguishable: the three states the requirement's
// second acceptance criterion covers — a private project's feed, an unknown
// target, and a target with nothing published — are ONE answer from outside.
// internal/application/feeds collapses them into ErrNotFound; this asserts
// the transport does not take them apart again (same status, same code, same
// body).
func TestFeedErrorsAreIndistinguishable(t *testing.T) {
	var bodies []string
	for _, err := range []error{feeds.ErrNotFound, feeds.ErrNotFound, feeds.ErrNotFound} {
		mux := newTestServer(&stubService{err: err})
		rec := do(mux, http.MethodGet, "/api/v1/feeds/projects/"+projectID, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		if code := errorCode(t, rec); code != CodeFeedNotFound {
			t.Fatalf("code = %q, want %q", code, CodeFeedNotFound)
		}
		// The body must not name the target or the reason: if the three
		// answers differed at all, an anonymous caller could tell a private
		// project from an absent one.
		if body := rec.Body.String(); strings.Contains(body, projectID) || strings.Contains(body, "private") {
			t.Errorf("404 body discloses the target or the reason: %q", body)
		}
		bodies = append(bodies, rec.Body.String())
	}
	for i, body := range bodies {
		if body != bodies[0] {
			t.Errorf("404 body %d = %q, want identical to %q", i, body, bodies[0])
		}
	}
}

// TestFeedReadFailureIsNotANotFound: "the database did not answer" must never
// be answered as "there is no such feed" — a subscriber would unsubscribe
// from a feed that is merely having a bad minute. It is a 503, it is not
// cached, and the dependency's error text stays out of the envelope.
func TestFeedReadFailureIsNotANotFound(t *testing.T) {
	svc := &stubService{err: errors.Join(feeds.ErrStore, errBoom)}
	rec := do(newTestServer(svc), http.MethodGet, "/api/v1/feeds/projects/"+projectID, nil)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if code := errorCode(t, rec); code != CodeFeedUnavailable {
		t.Errorf("code = %q, want %q", code, CodeFeedUnavailable)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store on a failure", got)
	}
	if body := rec.Body.String(); strings.Contains(body, "boom") {
		t.Errorf("envelope leaks the dependency error: %q", body)
	}
}

// TestFeedRenderFailureIs500: a renderer that failed is a bug in this
// platform, not a state of the target, and it is answered as what it is
// rather than as either of the two honest answers above.
func TestFeedRenderFailureIs500(t *testing.T) {
	svc := &stubService{err: errors.New("feeds: render atom: boom")}
	rec := do(newTestServer(svc), http.MethodGet, "/api/v1/feeds/assets/"+testPID, nil)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if code := errorCode(t, rec); code != CodeFeedRenderFailed {
		t.Errorf("code = %q, want %q", code, CodeFeedRenderFailed)
	}
}

// TestFeedSurfaceUnwired: when the deployment's public origin is unusable the
// surface is registered but not wired. It must answer 503 — never 404, which
// would tell every subscriber the feed does not exist (and every crawler to
// drop it).
func TestFeedSurfaceUnwired(t *testing.T) {
	mux := newTestServer(nil)
	rec := do(mux, http.MethodGet, "/api/v1/feeds/projects/"+projectID, nil)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired status = %d, want 503", rec.Code)
	}
	if code := errorCode(t, rec); code != CodeFeedUnavailable {
		t.Errorf("code = %q, want %q", code, CodeFeedUnavailable)
	}
}

// TestFeedFormatNegotiation: the format is chosen by ?format= first, then by
// Accept, then defaulted — and a ?format= that names nothing is refused
// rather than served as Atom.
func TestFeedFormatNegotiation(t *testing.T) {
	const path = "/api/v1/feeds/projects/" + projectID
	cases := []struct {
		name       string
		query      string
		accept     string
		wantCT     string
		wantFormat feeds.Format
	}{
		{"default is atom", "", "", "application/atom+xml; charset=utf-8", feeds.FormatAtom},
		{"query rss", "?format=rss", "", "application/rss+xml; charset=utf-8", feeds.FormatRSS},
		{"query atom", "?format=atom", "", "application/atom+xml; charset=utf-8", feeds.FormatAtom},
		{"query case-insensitive", "?format=RSS", "", "application/rss+xml; charset=utf-8", feeds.FormatRSS},
		{"query beats accept", "?format=atom", "application/rss+xml", "application/atom+xml; charset=utf-8", feeds.FormatAtom},
		{"accept rss", "", "application/rss+xml", "application/rss+xml; charset=utf-8", feeds.FormatRSS},
		{"accept atom", "", "application/atom+xml", "application/atom+xml; charset=utf-8", feeds.FormatAtom},
		{"accept with parameters and q", "", "application/rss+xml;q=0.9, application/xml;q=0.5", "application/rss+xml; charset=utf-8", feeds.FormatRSS},
		{"browser accept falls back to atom", "", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8", "application/atom+xml; charset=utf-8", feeds.FormatAtom},
		{"wildcard falls back to atom", "", "*/*", "application/atom+xml; charset=utf-8", feeds.FormatAtom},
		{"no accept header falls back to atom", "", "", "application/atom+xml; charset=utf-8", feeds.FormatAtom},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &stubService{doc: []byte("<doc/>\n")}
			headers := map[string]string{}
			if tc.accept != "" {
				headers["Accept"] = tc.accept
			}
			rec := do(newTestServer(svc), http.MethodGet, path+tc.query, headers)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != tc.wantCT {
				t.Errorf("Content-Type = %q, want %q", got, tc.wantCT)
			}
			if svc.gotFmt != tc.wantFormat {
				t.Errorf("format asked of the service = %q, want %q", svc.gotFmt, tc.wantFormat)
			}
		})
	}
}

// TestFeedUnknownFormatRefused: a reader that asked for a format this surface
// does not speak is told so, instead of being handed a document in a format
// it did not ask for — and the refusal costs no read, because it is about the
// caller's own request and not about the target.
func TestFeedUnknownFormatRefused(t *testing.T) {
	for _, query := range []string{"?format=json", "?format=xml", "?format=rss2", "?format=ATOM%20RSS"} {
		svc := &stubService{doc: []byte("<doc/>\n")}
		rec := do(newTestServer(svc), http.MethodGet, "/api/v1/feeds/projects/"+projectID+query, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", query, rec.Code)
		}
		if code := errorCode(t, rec); code != CodeFeedInvalidRequest {
			t.Errorf("GET %s code = %q, want %q", query, code, CodeFeedInvalidRequest)
		}
		if svc.calls != 0 {
			t.Errorf("GET %s read the state %d times, want 0", query, svc.calls)
		}
	}
	// An empty or blank value is not a choice, it is the default — the same
	// rule the model states (feeds.ParseFormat), so the two cannot disagree.
	for _, query := range []string{"?format=", "?format=%20", "?format=%20rss%20"} {
		svc := &stubService{doc: []byte("<doc/>\n")}
		rec := do(newTestServer(svc), http.MethodGet, "/api/v1/feeds/projects/"+projectID+query, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 (body %q)", query, rec.Code, rec.Body.String())
		}
	}
}

// TestFeedValidatorIsStableAndHonoured: the same bytes produce the same
// validator, and a client that already holds them is answered 304 with no
// body. The document is a function of the stored state (see doc.go), so this
// is what makes the cache metadata honest.
func TestFeedValidatorIsStableAndHonoured(t *testing.T) {
	const path = "/api/v1/feeds/projects/" + projectID
	svc := &stubService{doc: []byte("<feed><title>t</title></feed>\n")}
	mux := newTestServer(svc)

	first := do(mux, http.MethodGet, path, nil)
	etag := first.Header().Get("ETag")
	if etag == "" || !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) {
		t.Fatalf("ETag = %q, want a quoted strong validator", etag)
	}
	if got := first.Header().Get("Cache-Control"); got != cacheControl {
		t.Errorf("Cache-Control = %q, want %q", got, cacheControl)
	}
	if got := first.Header().Get("Vary"); got != "Accept" {
		t.Errorf("Vary = %q, want Accept", got)
	}
	second := do(mux, http.MethodGet, path, nil)
	if second.Header().Get("ETag") != etag {
		t.Errorf("ETag changed between two identical requests: %q then %q", etag, second.Header().Get("ETag"))
	}

	for _, header := range []string{etag, "W/" + etag, `"other", ` + etag, "*"} {
		rec := do(mux, http.MethodGet, path, map[string]string{"If-None-Match": header})
		if rec.Code != http.StatusNotModified {
			t.Errorf("If-None-Match %q = %d, want 304", header, rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("If-None-Match %q answered a body: %q", header, rec.Body.String())
		}
		if rec.Header().Get("ETag") != etag {
			t.Errorf("If-None-Match %q: 304 carries ETag %q, want %q", header, rec.Header().Get("ETag"), etag)
		}
	}
	// A validator the client holds for something else must not be answered
	// 304 — that would hand a reader a stale document for a live one.
	for _, header := range []string{`"stale"`, "", "garbage"} {
		rec := do(mux, http.MethodGet, path, map[string]string{"If-None-Match": header})
		if rec.Code != http.StatusOK {
			t.Errorf("If-None-Match %q = %d, want 200", header, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("If-None-Match %q answered no body on a 200", header)
		}
	}
	// Different bytes are a different representation, so a client holding the
	// first must be sent the second rather than a 304.
	svc.doc = []byte("<feed><title>u</title></feed>\n")
	rec := do(mux, http.MethodGet, path, map[string]string{"If-None-Match": etag})
	if rec.Code != http.StatusOK {
		t.Errorf("changed document = %d, want 200", rec.Code)
	}
	if rec.Header().Get("ETag") == etag {
		t.Errorf("changed document kept the old validator %q", etag)
	}
}

// TestFeedRoutesAreGetOnly: there is nothing on this surface to write. A POST
// is refused by the mux (405) rather than reaching a handler — and in
// production the v1 guard refuses it even earlier, for want of a session.
func TestFeedRoutesAreGetOnly(t *testing.T) {
	mux := newTestServer(&stubService{doc: []byte("<feed/>\n")})
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := do(mux, method, "/api/v1/feeds/projects/"+projectID, nil)
		if rec.Code == http.StatusOK {
			t.Errorf("%s answered 200; the feed surface has no write", method)
		}
	}
}

// TestFeedErrorEnvelopeShape: errors use the platform's one envelope
// (docs/45), so a client's generic error handling works on this surface too.
func TestFeedErrorEnvelopeShape(t *testing.T) {
	rec := do(newTestServer(nil), http.MethodGet, "/api/v1/feeds/projects/"+projectID, nil)
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("error Content-Type = %q, want application/json", got)
	}
	var envelope struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
		Retryable bool   `json:"retryable"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("error body is not the JSON envelope: %v (%q)", err, rec.Body.String())
	}
	if envelope.Code != CodeFeedUnavailable || envelope.Message == "" {
		t.Errorf("envelope = %+v, want code %q and a message", envelope, CodeFeedUnavailable)
	}
	if !envelope.Retryable {
		t.Errorf("503 envelope reports retryable=false; a transient failure is retryable")
	}
}

// errorCode reads the envelope's code out of a recorder, failing the test if
// the body is not an envelope at all.
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var envelope struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("body is not the error envelope: %v (%q)", err, rec.Body.String())
	}
	return envelope.Code
}
