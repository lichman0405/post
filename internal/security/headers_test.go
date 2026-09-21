package security

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// The response-header contract, asserted on the exact values a real
// response carries. "The header exists" is not the assertion: a header that
// exists with the wrong value (a CSP with 'unsafe-eval', a Server line with
// the build number) is worse than a missing one, because it reads as done.

func TestHeadersStampsEveryHeaderWithItsExactValue(t *testing.T) {
	h := get(t, Headers(), http.MethodGet, "/api/v1/projects", "application/json", nil)

	for header, want := range map[string]string{
		HeaderContentTypeOptions: ValueNosniff,
		HeaderFrameOptions:       ValueFrameDeny,
		HeaderReferrerPolicy:     ValueNoReferrer,
		HeaderPermissionsPolicy:  PermissionsPolicyLockdown,
		HeaderStrictTransport:    HSTSValue,
		HeaderServer:             ServerToken,
		HeaderCSP:                CSPLockdown,
	} {
		if got := h.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestHeadersVersionFreeServerToken(t *testing.T) {
	h := get(t, Headers(), http.MethodGet, "/healthz", "application/json", nil)

	server := h.Get(HeaderServer)
	if server != ServerToken {
		t.Fatalf("Server = %q, want %q", server, ServerToken)
	}
	// A digit anywhere in the token is how a version gets in: "post/1.4.2",
	// "post 0.9", even an internal build id. The token has to stay a
	// product name.
	if regexp.MustCompile(`[0-9]`).MatchString(server) {
		t.Errorf("Server = %q carries a digit — a version disclose", server)
	}
	if strings.ContainsAny(server, "/. ") {
		t.Errorf("Server = %q carries a separator — product/version separators are how versions travel", server)
	}
}

// The CSP is chosen from the response's own Content-Type: this API serves
// JSON envelopes and server-rendered HTML pages (cmd/api/rsghttp) from the
// same mux, and one blanket policy would either leave the pages unstyled or
// leave every JSON response able to load a script.
func TestHeadersCSPFollowsTheResponseContentType(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		want        string
	}{
		{"json envelope", "application/json", CSPLockdown},
		{"json with charset", "application/json; charset=utf-8", CSPLockdown},
		{"streamed patch", "text/plain; charset=utf-8", CSPLockdown},
		{"attachment bytes", "application/octet-stream", CSPLockdown},
		{"no content type at all", "", CSPLockdown},
		{"rendered page", "text/html; charset=utf-8", CSPDocument},
		{"rendered page, xhtml", "application/xhtml+xml", CSPDocument},
		{"uppercase is still html", "TEXT/HTML", CSPDocument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := get(t, Headers(), http.MethodGet, "/x", tc.contentType, nil)
			if got := h.Get(HeaderCSP); got != tc.want {
				t.Errorf("CSP for %q = %q, want %q", tc.contentType, got, tc.want)
			}
		})
	}
}

// The document policy must forbid script, or an HTML exit would be a
// stored-XSS delivery channel with a policy header that looks reassuring.
func TestDocumentCSPForbidsScriptAndFraming(t *testing.T) {
	for _, directive := range []string{"script-src 'none'", "frame-ancestors 'none'", "base-uri 'none'", "form-action 'none'"} {
		if !strings.Contains(CSPDocument, directive) {
			t.Errorf("CSPDocument is missing %q: %s", directive, CSPDocument)
		}
	}
	// The one 'unsafe-inline' in the document policy must be the style
	// directive (the embedded templates use <style> blocks) and never the
	// script directive.
	if strings.Contains(CSPDocument, "'unsafe-eval'") {
		t.Errorf("CSPDocument allows eval: %s", CSPDocument)
	}
	if strings.Contains(CSPDocument, "script-src 'unsafe-inline'") ||
		strings.Contains(CSPDocument, "script-src 'self'") {
		t.Errorf("CSPDocument allows script: %s", CSPDocument)
	}
	if !strings.Contains(CSPDocument, "style-src 'unsafe-inline'") {
		t.Errorf("CSPDocument does not permit the templates' inline <style> blocks: %s", CSPDocument)
	}
	for _, directive := range []string{"frame-ancestors 'none'", "sandbox"} {
		if !strings.Contains(CSPLockdown, directive) {
			t.Errorf("CSPLockdown is missing %q: %s", directive, CSPLockdown)
		}
	}
}

// A handler that writes a body without calling WriteHeader still gets the
// policy: net/http calls WriteHeader for it, and the wrapper has to be in
// front of that call.
func TestHeadersSetCSPWhenTheHandlerNeverCallsWriteHeader(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(HeaderContentType, "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html></html>"))
	})
	rec := httptest.NewRecorder()
	Headers()(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if got := rec.Header().Get(HeaderCSP); got != CSPDocument {
		t.Errorf("CSP = %q, want %q", got, CSPDocument)
	}
}

// A handler that sets a header itself must not be able to erase the edge's
// — but it must be able to override the CSP it disagrees with only through
// the middleware's own content-type mechanism. This test pins the weaker,
// honest property: the wrapper does not silently drop a handler's own
// Content-Type.
func TestHeadersPreserveTheHandlersContentType(t *testing.T) {
	h := get(t, Headers(), http.MethodGet, "/x", "application/octet-stream", nil)
	if got := h.Get(HeaderContentType); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want the handler's own value untouched", got)
	}
}

// The middleware must never emit a CORS wildcard. The credentialed CORS
// policy belongs to the /api/v1 guard (cmd/api/authhttp), which is an
// allow-list; a second producer of Access-Control-Allow-Origin in the edge
// is how a "*" gets in.
func TestHeadersNeverEmitACORSOrigin(t *testing.T) {
	h := get(t, Headers(), http.MethodGet, "/api/v1/projects", "application/json",
		map[string]string{"Origin": "https://evil.example"})
	if got := h.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("edge middleware emitted Access-Control-Allow-Origin: %q", got)
	}
	if got := h.Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("edge middleware emitted Access-Control-Allow-Credentials: %q", got)
	}
}

// The counter-test: without the middleware the same handler sends none of
// these headers. It is what makes the assertions above mean something —
// they fail if the middleware is dropped from the chain, and they would
// also fail if a future refactor moved the header writes into a subset of
// routes.
func TestUnwrappedHandlerCarriesNoneOfTheHeaders(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(HeaderContentType, "application/json")
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	next.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil))

	for _, header := range []string{
		HeaderCSP, HeaderContentTypeOptions, HeaderFrameOptions,
		HeaderReferrerPolicy, HeaderPermissionsPolicy, HeaderStrictTransport, HeaderServer,
	} {
		if got := rec.Header().Get(header); got != "" {
			t.Errorf("unwrapped handler sent %s: %q — the header set is not coming from the handler", header, got)
		}
	}
}

// get runs one request through a handler chain and returns the response
// headers. contentTypes "" leaves the Content-Type unset.
func get(t *testing.T, mw func(http.Handler) http.Handler, method, path, contentType string, hdr map[string]string) http.Header {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if contentType != "" {
			w.Header().Set(HeaderContentType, contentType)
		}
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(method, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, req)
	return rec.Header()
}
