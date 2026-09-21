package security

import (
	"net/http"
	"strings"
)

// Wire header names and the exact values the edge stamps. They are
// constants (not literals sprinkled through handlers) so the guard tests
// can assert the same strings the middleware writes: a test that spells
// the expected value out a second time drifts silently.
const (
	HeaderCSP                = "Content-Security-Policy"
	HeaderContentType        = "Content-Type"
	HeaderContentTypeOptions = "X-Content-Type-Options"
	HeaderFrameOptions       = "X-Frame-Options"
	HeaderReferrerPolicy     = "Referrer-Policy"
	HeaderPermissionsPolicy  = "Permissions-Policy"
	HeaderStrictTransport    = "Strict-Transport-Security"
	HeaderServer             = "Server"
)

const (
	// ValueNosniff is the only value that means anything for
	// X-Content-Type-Options; the header is either this or absent.
	ValueNosniff = "nosniff"
	// ValueFrameDeny duplicates CSP frame-ancestors for browsers that
	// never implemented the CSP directive. Belt and braces, on purpose:
	// they cannot disagree, both are constants.
	ValueFrameDeny = "DENY"
	// ServerToken is the product token only. A Server header carrying a
	// version number is a free fingerprint of the exact build; version
	// disclosure on this API is a deliberate non-feature.
	ServerToken = "post"
	// ValueNoReferrer: this API's URLs are the API surface, and a
	// credential-bearing URL must never travel in a Referer header.
	ValueNoReferrer = "no-referrer"
	// PermissionsPolicyLockdown switches off every powerful feature a
	// document fetched from this origin could otherwise exercise. None of
	// them is used by the JSON API or by the rsghttp HTML pages.
	PermissionsPolicyLockdown = "accelerometer=(), autoplay=(), camera=(), display-capture=(), " +
		"encrypted-media=(), fullscreen=(), geolocation=(), gyroscope=(), magnetometer=(), " +
		"microphone=(), midi=(), payment=(), picture-in-picture=(), publickey-credentials-get=(), " +
		"screen-wake-lock=(), usb=(), xr-spatial-tracking=()"
	// HSTSValue is sent unconditionally. A browser only honours HSTS for
	// an https response, so a dev deployment over plain http is unaffected
	// — and there is no branch here that could be enabled for the wrong
	// environment. One year, subdomains included: the API and the web app
	// share a registrable domain in the documented topology.
	HSTSValue = "max-age=31536000; includeSubDomains"
)

// CSPLockdown is the policy for every non-HTML response: JSON envelopes,
// error bodies, streamed patches. Nothing may be loaded, framed, posted or
// executed; the sandbox directive mirrors docs/23 §6's "HTML/SVG active
// content sandbox" for the case where a caller navigates a browser
// straight at a byte response.
//
// "sandbox allow-downloads" and not a bare "sandbox": a bare sandbox
// applies the HTML `sandbox` attribute's restrictions to the document,
// and the default there blocks downloads — it would break the one
// feature this header set exists to protect (the attachment download
// channel of cmd/api/fileshttp). allow-downloads re-permits exactly that
// and nothing else: scripts, forms, popups, top-level navigation and
// plugins all stay blocked.
const CSPLockdown = "default-src 'none'; base-uri 'none'; form-action 'none'; " +
	"frame-ancestors 'none'; sandbox allow-downloads"

// CSPDocument is the policy for the server-rendered HTML pages
// (cmd/api/rsghttp). Those templates carry `<style>` blocks and inline
// `<svg>` and no `<script>` at all, so the policy permits inline styles and
// nothing executable: script-src 'none' is the directive that matters, and
// it is the one a page cannot relax.
//
// No nonce: a nonce is only meaningful for a document whose HTML the
// server can rewrite per request, and these pages are rendered from
// embedded templates by html/template, whose auto-escaping is the actual
// XSS defence. Adding a nonce would buy nothing here and would make the
// header value request-dependent, which is exactly what makes a
// header-level guard test unable to assert it.
const CSPDocument = "default-src 'none'; base-uri 'none'; form-action 'none'; " +
	"frame-ancestors 'none'; script-src 'none'; style-src 'unsafe-inline'; " +
	"img-src 'self' data:; font-src 'self' data:"

// Headers returns the edge middleware. It stamps the fixed header set
// before the handler runs, and the CSP (which depends on the response's
// Content-Type) at WriteHeader time — the moment the content type is
// finally known, whether the handler set it explicitly or net/http sniffed
// it. Everything written through it also keeps the optional interfaces
// (Flush, Hijack) working via Unwrap.
func Headers() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set(HeaderServer, ServerToken)
			h.Set(HeaderContentTypeOptions, ValueNosniff)
			h.Set(HeaderFrameOptions, ValueFrameDeny)
			h.Set(HeaderReferrerPolicy, ValueNoReferrer)
			h.Set(HeaderPermissionsPolicy, PermissionsPolicyLockdown)
			h.Set(HeaderStrictTransport, HSTSValue)
			next.ServeHTTP(&cspWriter{ResponseWriter: w}, r)
		})
	}
}

// cspWriter sets the Content-Security-Policy exactly once, just before the
// status line goes out.
type cspWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *cspWriter) WriteHeader(code int) {
	if !w.wrote {
		w.wrote = true
		w.Header().Set(HeaderCSP, cspFor(w.Header().Get(HeaderContentType)))
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *cspWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap keeps http.ResponseController working through the middleware.
func (w *cspWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// cspFor picks the policy for one response from its declared content type.
// An unknown or unparseable type gets the lockdown policy: the failure
// direction is "nothing may load", never "the browser guesses".
func cspFor(contentType string) string {
	if isHTMLContentType(contentType) {
		return CSPDocument
	}
	return CSPLockdown
}

// isHTMLContentType reports whether a Content-Type header value declares a
// rendered HTML document. Compared on the media type only, so
// "text/html; charset=utf-8" matches and "application/json; charset=utf-8"
// does not.
func isHTMLContentType(contentType string) bool {
	mediaType, _, _ := strings.Cut(contentType, ";")
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "text/html", "application/xhtml+xml":
		return true
	default:
		return false
	}
}
