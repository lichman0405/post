package security

import (
	"fmt"
	"net/http"
	"strings"
)

// The outbound-byte contract (docs/23 §6, docs/17 "Files is a read-only
// projection"): whatever leaves the API as payload bytes reaches a browser
// that will do something with them, and "something" must never be "execute
// it". Three header facts, in order of what they buy:
//
//  1. X-Content-Type-Options: nosniff — the browser must believe the
//     declared media type. Without it every type below is negotiable.
//  2. Content-Disposition: attachment — an opaque payload is a download,
//     never a document. The browser's own download UI is the sandbox.
//  3. A media type the browser does not execute — for the responses that
//     are legitimately displayed inline (this API's own JSON envelopes,
//     the streamed git patch, the server-rendered HTML pages).
//
// JudgeExit is that contract as one function, and it is written to say no:
// the tests in tests/security drive it from both directions (a conforming
// response and every single-property mutation of one), and the handler
// probes assert the values a real handler actually sends.
const (
	// HeaderContentDisposition is RFC 6266's disposition field.
	HeaderContentDisposition = "Content-Disposition"
	// DispositionAttachment is the download disposition: the browser
	// writes the bytes to a file and never builds a document from them.
	DispositionAttachment = "attachment"
	// DispositionInline means "display this". Only three media types may
	// use it here, and each has its own reason (see JudgeExit).
	DispositionInline = "inline"
)

// inlineMediaTypes are the only media types this API may serve without an
// attachment disposition.
//
// The list is a whitelist and not "anything a browser cannot execute": a
// judge that accepts application/octet-stream inline would also accept the
// download channel after its attachment disposition was dropped, which is
// the exact regression the guard exists to catch. Newlines and unknown
// types default to "must be an attachment".
var inlineMediaTypes = map[string]bool{
	// The API's own envelope: JSON is data, and the envelope is the API.
	"application/json": true,
	// The streamed git patch (cmd/api/fileshttp handleDiff): the raw
	// diff view, deliberately readable. text/plain is never rendered as
	// anything but text, and nosniff pins it.
	"text/plain": true,
	// Markdown renders as text in every current browser (no HTML
	// interpretation), but it is listed so a future markdown preview does
	// not have to become a download to pass this judge.
	"text/markdown": true,
}

// documentMediaTypes are the rendered-document media types: the
// server-rendered HTML pages of cmd/api/rsghttp and the Atom/RSS documents
// of cmd/api/feedshttp. They are handled apart from inlineMediaTypes
// because they carry an extra obligation — no script may run — which is
// enforced by the CSP the edge picks from this same content type.
//
// XML is in this class, not in inlineMediaTypes, because the browser does
// parse it: a feed is a document with a tree, processing instructions and
// (in some readers) a stylesheet. It is not HTML, so it is not script the
// same way, but the obligation is the same one and it costs one CSP
// directive to meet — a media-type list that said "XML is fine, HTML needs
// a policy" would be drawing the line from memory of how one browser
// behaves rather than from what the response is.
var documentMediaTypes = map[string]bool{
	"text/html":             true,
	"application/xhtml+xml": true,
	"application/atom+xml":  true,
	"application/rss+xml":   true,
}

// ExitKind is how a byte-carrying response left the process.
type ExitKind string

const (
	// ExitOpaque: bytes handed over as a download.
	ExitOpaque ExitKind = "attachment"
	// ExitInlineText: server-produced text or JSON displayed as itself.
	ExitInlineText ExitKind = "inline-text"
	// ExitDocument: a server-rendered HTML document.
	ExitDocument ExitKind = "document"
)

// MediaTypeOf reduces a Content-Type header value to its lowercase media
// type ("text/plain; charset=utf-8" -> "text/plain").
func MediaTypeOf(contentType string) string {
	mediaType, _, _ := strings.Cut(contentType, ";")
	return strings.ToLower(strings.TrimSpace(mediaType))
}

// DispositionTypeOf reduces a Content-Disposition header value to its
// lowercase type token ("attachment; filename=\"x\"" -> "attachment").
func DispositionTypeOf(disposition string) string {
	return MediaTypeOf(disposition)
}

// JudgeExit applies the outbound-byte contract to one response's headers.
// It returns the exit kind it recognised, or an error naming the first
// unmet obligation — the error text is the guard's failure message, so it
// names the header to add, not just the rule that failed.
func JudgeExit(h http.Header) (ExitKind, error) {
	mediaType := MediaTypeOf(h.Get(HeaderContentType))
	if mediaType == "" {
		return "", fmt.Errorf("no Content-Type declared: the browser would sniff the payload "+
			"(set %s and %s: %s)", HeaderContentType, HeaderContentTypeOptions, ValueNosniff)
	}
	if h.Get(HeaderContentTypeOptions) != ValueNosniff {
		return "", fmt.Errorf("%s is %q, want %q: without it the declared media type %q is only a suggestion "+
			"and the browser may reinterpret the payload", HeaderContentTypeOptions,
			h.Get(HeaderContentTypeOptions), ValueNosniff, mediaType)
	}

	switch disposition := DispositionTypeOf(h.Get(HeaderContentDisposition)); disposition {
	case DispositionAttachment:
		// Any media type may travel as an attachment: the browser writes
		// it to disk and never builds a document from it, so even
		// text/html is safe here.
		return ExitOpaque, nil

	case DispositionInline, "":
		switch {
		case documentMediaTypes[mediaType]:
			if !disallowsScripts(h.Get(HeaderCSP)) {
				return "", fmt.Errorf("%s is %q but no script-blocking %s is present: a rendered document "+
					"must carry %q (or a policy with %q)", HeaderContentType, mediaType, HeaderCSP,
					CSPDocument, "script-src 'none'")
			}
			return ExitDocument, nil
		case inlineMediaTypes[mediaType]:
			return ExitInlineText, nil
		default:
			return "", fmt.Errorf("%s is %q, which may not be served inline: that media type is "+
				"neither a declared rendered document nor one of the inline text types, so the "+
				"response must say %s: set %s: %s", HeaderContentType, mediaType,
				DispositionAttachment, HeaderContentDisposition, DispositionAttachment)
		}

	default:
		return "", fmt.Errorf("%s type %q is not a disposition this API may serve: use %s or %s",
			HeaderContentDisposition, h.Get(HeaderContentDisposition), DispositionAttachment, DispositionInline)
	}
}

// disallowsScripts reports whether a CSP value forbids script execution,
// either explicitly (script-src 'none') or by falling back to a
// script-src-less default-src 'none'.
func disallowsScripts(csp string) bool {
	if csp == "" {
		return false
	}
	lower := strings.ToLower(csp)
	if strings.Contains(lower, "script-src 'none'") {
		return true
	}
	return strings.Contains(lower, "default-src 'none'") && !strings.Contains(lower, "script-src")
}
