package security

import (
	"net/http"
	"strings"
	"testing"
)

// JudgeExit from both directions: a conforming response passes and reports
// the right kind, and every single-property mutation of one fails. The
// mutation half is the half that matters — a judge that only ever says yes
// is a comment, and this file is where that is cheapest to show.

func headerSet(pairs ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(pairs); i += 2 {
		h.Set(pairs[i], pairs[i+1])
	}
	return h
}

// conforming is the base response every mutation below is a one-property
// edit of: the download channel, as cmd/api/fileshttp actually sends it.
func conforming() http.Header {
	return headerSet(
		HeaderContentType, "application/octet-stream",
		HeaderContentTypeOptions, ValueNosniff,
		HeaderContentDisposition, `attachment; filename="data.csv"`,
	)
}

func TestJudgeExitAcceptsEachExitKindItNames(t *testing.T) {
	cases := []struct {
		name string
		h    http.Header
		want ExitKind
	}{
		{
			name: "provider bytes as a download",
			h:    conforming(),
			want: ExitOpaque,
		},
		{
			name: "the API's own JSON envelope",
			h: headerSet(
				HeaderContentType, "application/json",
				HeaderContentTypeOptions, ValueNosniff,
			),
			want: ExitInlineText,
		},
		{
			name: "the streamed git patch",
			h: headerSet(
				HeaderContentType, "text/plain; charset=utf-8",
				HeaderContentTypeOptions, ValueNosniff,
			),
			want: ExitInlineText,
		},
		{
			name: "a server-rendered page under the document CSP",
			h: headerSet(
				HeaderContentType, "text/html; charset=utf-8",
				HeaderContentTypeOptions, ValueNosniff,
				HeaderCSP, CSPDocument,
			),
			want: ExitDocument,
		},
		{
			name: "an HTML document may also be handed over as a download",
			h: headerSet(
				HeaderContentType, "text/html; charset=utf-8",
				HeaderContentTypeOptions, ValueNosniff,
				HeaderContentDisposition, `attachment; filename="report.html"`,
			),
			want: ExitOpaque,
		},
		{
			name: "a feed document under the edge's lockdown policy",
			h: headerSet(
				HeaderContentType, "application/atom+xml; charset=utf-8",
				HeaderContentTypeOptions, ValueNosniff,
				HeaderCSP, CSPLockdown,
			),
			want: ExitDocument,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := JudgeExit(tc.h)
			if err != nil {
				t.Fatalf("JudgeExit refused a conforming response: %v", err)
			}
			if got != tc.want {
				t.Errorf("kind = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestJudgeExitRefusesEverySinglePropertyMutation(t *testing.T) {
	mutate := func(edit func(http.Header)) http.Header {
		h := conforming()
		edit(h)
		return h
	}
	cases := []struct {
		name string
		h    http.Header
	}{
		{
			// The one the task names: the download channel relabelled as a
			// document. Same bytes, same type, one different word — and the
			// browser now renders whatever the bytes say.
			name: "attachment relabelled inline on a type the browser executes",
			h:    mutate(func(h http.Header) { h.Set(HeaderContentDisposition, DispositionInline) }),
		},
		{
			// The other one the task names.
			name: "nosniff deleted",
			h:    mutate(func(h http.Header) { h.Del(HeaderContentTypeOptions) }),
		},
		{
			name: "nosniff present but wrong",
			h:    mutate(func(h http.Header) { h.Set(HeaderContentTypeOptions, "sniff") }),
		},
		{
			// The download channel with the disposition silently dropped:
			// the payload is still octet-stream + nosniff, so a browser does
			// the safe thing — but nothing in the response SAYS it is a
			// download, and the next handler that sets a sniffable type
			// would inherit the omission.
			name: "no disposition on an opaque payload",
			h: headerSet(
				HeaderContentType, "application/octet-stream",
				HeaderContentTypeOptions, ValueNosniff,
			),
		},
		{
			name: "no Content-Type declared",
			h:    mutate(func(h http.Header) { h.Del(HeaderContentType) }),
		},
		{
			// An unknown disposition is refused rather than defaulted: the
			// judge has to know which of the two the response meant.
			name: "an unknown disposition",
			h:    mutate(func(h http.Header) { h.Set(HeaderContentDisposition, "filename=metadata") }),
		},
		{
			name: "inline on an unknown media type",
			h: headerSet(
				HeaderContentType, "application/zip",
				HeaderContentTypeOptions, ValueNosniff,
				HeaderContentDisposition, DispositionInline,
			),
		},
		{
			// A document is only safe as a document if script cannot run in
			// it: drop the policy and the same page becomes an XSS delivery
			// channel with a reassuring header set.
			name: "a rendered document with no CSP",
			h:    headerSet(HeaderContentType, "text/html", HeaderContentTypeOptions, ValueNosniff),
		},
		{
			name: "a rendered document whose CSP only forbids framing",
			h: headerSet(
				HeaderContentType, "text/html",
				HeaderContentTypeOptions, ValueNosniff,
				HeaderCSP, "default-src *; script-src 'unsafe-inline'",
			),
		},
		{
			name: "a document CSP that allows eval through default-src",
			h: headerSet(
				HeaderContentType, "text/html",
				HeaderContentTypeOptions, ValueNosniff,
				HeaderCSP, "default-src 'self'; script-src 'unsafe-eval'",
			),
		},
		{
			name: "an HTML exit that is neither a document nor an attachment",
			h: headerSet(
				HeaderContentType, "text/html; charset=utf-8",
				HeaderContentTypeOptions, ValueNosniff,
			),
		},
		{
			// A feed served as a document with no policy of its own: XML is
			// still a document, and the class is decided by the media type,
			// not by whether it happens to be HTML.
			name: "an XML feed document with no CSP",
			h: headerSet(
				HeaderContentType, "application/atom+xml; charset=utf-8",
				HeaderContentTypeOptions, ValueNosniff,
			),
		},
		{
			name: "an exit with nothing declared at all",
			h:    http.Header{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, err := JudgeExit(tc.h)
			if err == nil {
				t.Fatalf("JudgeExit accepted it and reported kind %q — the guard cannot say no here", kind)
			}
			if kind != "" {
				t.Errorf("refused with kind %q; a refusal must not also name an exit kind", kind)
			}
			if !strings.Contains(err.Error(), "Content-") {
				t.Errorf("refusal %q does not name the header to fix", err.Error())
			}
		})
	}
}

// The document class must not be reachable by a plain CSP that happens to
// contain the words: disallowsScripts reads the directive, not a substring
// of the whole policy.
func TestDisallowsScriptsReadsDirectives(t *testing.T) {
	for _, csp := range []string{
		CSPDocument,
		CSPLockdown,
		"default-src 'none'",
		"script-src 'none'; img-src 'self'",
	} {
		if !disallowsScripts(csp) {
			t.Errorf("disallowsScripts(%q) = false, want true", csp)
		}
	}
	for _, csp := range []string{
		"",
		"default-src 'self'",
		// The dangerous near-miss: everything else is locked down, script
		// is not. A judge that matched on "'none'" would pass this.
		"default-src 'none'; script-src 'unsafe-inline'",
		"script-src 'self'",
		"report-uri 'none'",
	} {
		if disallowsScripts(csp) {
			t.Errorf("disallowsScripts(%q) = true, want false", csp)
		}
	}
}

// MediaTypeOf / DispositionTypeOf are the two parsers under the judge; a
// parameter they fail to strip turns "attachment; filename=x" into a
// disposition nobody recognises.
func TestMediaTypeParsing(t *testing.T) {
	for in, want := range map[string]string{
		"application/json":               "application/json",
		"text/plain; charset=utf-8":      "text/plain",
		"TEXT/HTML;CHARSET=UTF-8":        "text/html",
		" application/octet-stream ":     "application/octet-stream",
		`attachment; filename="a b.csv"`: "attachment",
		"inline":                         "inline",
		"":                               "",
	} {
		if got := MediaTypeOf(in); got != want {
			t.Errorf("MediaTypeOf(%q) = %q, want %q", in, got, want)
		}
	}
}
