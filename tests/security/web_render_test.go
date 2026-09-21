package security

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// webRoot is the web app, relative to this package's directory (go test runs
// each package with its own directory as the working directory).
const webRoot = "../../apps/web"

// webSourceExts are the extensions the guard reads. Everything a browser can
// be handed a string of markup from lives in one of them.
var webSourceExts = map[string]bool{
	".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".mjs": true,
}

// skippedWebDirs are never read: build output and installed packages are not
// this repository's source.
var skippedWebDirs = map[string]bool{
	"node_modules": true, ".next": true, "dist": true, "coverage": true,
}

// renderExemption classifies the one raw-HTML injection site the tree is
// allowed to have.
type renderExemption struct {
	// Literal is the exact text the __html expression must be. Registering
	// the content and not just the location is the point: the danger of a
	// dangerouslySetInnerHTML is never the call, it is what ends up inside
	// it, and an exemption that waves through "whatever is on this line
	// today" is not a review.
	Literal string
	// Why this site is safe.
	Why string
}

// renderExemptions is keyed "<path relative to apps/web>:<line>".
//
// The line number is part of the key on purpose. A site that moves is a site
// whose surrounding context changed, and re-registering it costs one line of
// diff — which is the cheapest possible moment to look at it again.
var renderExemptions = map[string]renderExemption{
	"app/(main)/explore/page.tsx:58": {
		Literal: ".explore-panel[hidden]{display:block !important}",
		Why: "A CSS rule inside a <noscript> element on the Explore page: " +
			"without script the tablist cannot switch panels, so this reveals " +
			"every panel and the page degrades to one complete document. It is " +
			"a compile-time constant in this file — no request data, no " +
			"database, no template — and React needs it as a raw string " +
			"because a <style> element's children are text, not elements.",
	},
}

// markupSinks are the DOM APIs that turn a string into markup, plus the two
// that turn a string into code. None of them can appear in the web app.
//
// dangerouslySetInnerHTML is deliberately NOT in this list: it is the one
// sink with a legitimate use, so it is handled by its own rule below rather
// than banned outright.
var markupSinks = []string{
	"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write",
	"srcdoc", "new Function", "eval(",
}

// htmlSite is one dangerouslySetInnerHTML occurrence.
type htmlSite struct {
	// Path is the file relative to apps/web, slash-separated, so a failure
	// message names the same path a reviewer clicks.
	Path string
	Line int
	// Literal is the string the __html expression evaluates to, and OK is
	// false when it is not a literal at all — a variable, a call, a member
	// expression, a template with a substitution.
	Literal string
	OK      bool
	// Shape is what actually stood where a literal was expected, for the
	// failure message.
	Shape string
}

// Key is the registration key: "path:line".
func (s htmlSite) Key() string { return fmt.Sprintf("%s:%d", s.Path, s.Line) }

// webFiles walks the web app and returns every source file, path relative to
// webRoot with slashes.
func webFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(webRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skippedWebDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !webSourceExts[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		rel, relErr := filepath.Rel(webRoot, path)
		if relErr != nil {
			return relErr
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", webRoot, err)
	}
	if len(files) == 0 {
		t.Fatalf("%s contained no source files; the guard would pass vacuously", webRoot)
	}
	sort.Strings(files)
	return files
}

func readWebFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(webRoot, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// scanInnerHTML finds every dangerouslySetInnerHTML in the web app and
// classifies its __html expression.
func scanInnerHTML(t *testing.T) []htmlSite {
	t.Helper()
	var sites []htmlSite
	for _, rel := range webFiles(t) {
		src := readWebFile(t, rel)
		for _, off := range occurrences(src, innerHTMLAttr) {
			literal, ok, shape := literalExpression(src, off+len(innerHTMLAttr))
			sites = append(sites, htmlSite{
				Path:    rel,
				Line:    1 + strings.Count(src[:off], "\n"),
				Literal: literal,
				OK:      ok,
				Shape:   shape,
			})
		}
	}
	return sites
}

const innerHTMLAttr = "dangerouslySetInnerHTML"

// occurrences returns the byte offset of every occurrence of needle in src.
func occurrences(src, needle string) []int {
	var out []int
	for i := 0; ; {
		j := strings.Index(src[i:], needle)
		if j < 0 {
			return out
		}
		out = append(out, i+j)
		i += j + len(needle)
	}
}

// literalExpression reads the assignment that follows a
// dangerouslySetInnerHTML attribute and reports the string it hands to
// React, if it is a string at all.
//
// The accepted shape is exactly one, and it is a shape rather than a
// pattern: nothing between the attribute and the opening braces but
// whitespace, then `{{`, then `__html`, then `:`, then a quoted string or a
// template literal that interpolates nothing, then `}}`. Everything else —
// an identifier, a call, a member expression, a concatenation, a `${}` —
// leaves ok false, because that is the shape in which request data reaches
// the DOM unescaped. Being strict here is what makes the guard able to say
// no: a rule phrased as "must not contain a variable" would have to
// recognise every way of writing one.
func literalExpression(src string, start int) (literal string, ok bool, shape string) {
	i := skipSpace(src, start)
	if i >= len(src) || src[i] != '=' {
		return "", false, "not an assignment"
	}
	i = skipSpace(src, i+1)
	if !strings.HasPrefix(src[i:], "{{") {
		return "", false, "not an object literal"
	}
	i = skipSpace(src, i+2)
	if !strings.HasPrefix(src[i:], "__html") {
		return "", false, "the object does not start with __html"
	}
	i = skipSpace(src, i+len("__html"))
	if i >= len(src) || src[i] != ':' {
		return "", false, "__html is not assigned"
	}
	i = skipSpace(src, i+1)
	if i >= len(src) {
		return "", false, "no expression"
	}

	switch src[i] {
	case '"', '\'':
		end := closingQuote(src, i)
		if end < 0 {
			return "", false, "an unterminated string"
		}
		literal, i = src[i+1:end], end+1
	case '`':
		end := closingBacktick(src, i)
		if end < 0 {
			return "", false, "an unterminated template literal"
		}
		body := src[i+1 : end]
		if strings.Contains(unquote(body), "${") {
			return "", false, "a template literal with an interpolation"
		}
		literal, i = body, end+1
	default:
		return "", false, "a " + describeExpression(src[i:])
	}

	i = skipSpace(src, i)
	// A trailing comma is how the formatter writes a one-property object;
	// it changes nothing about the value.
	if i < len(src) && src[i] == ',' {
		i = skipSpace(src, i+1)
	}
	if !strings.HasPrefix(src[i:], "}}") {
		return "", false, "the object does not close right after __html"
	}
	_ = skipSpace(src, i+2)
	return literal, true, ""
}

func skipSpace(src string, i int) int {
	for i < len(src) && (src[i] == ' ' || src[i] == '\t' || src[i] == '\n' || src[i] == '\r') {
		i++
	}
	return i
}

// closingQuote returns the index of the quote that ends the string opening
// at i, or -1.
func closingQuote(src string, i int) int {
	quote := src[i]
	for j := i + 1; j < len(src); j++ {
		switch src[j] {
		case '\\':
			j++
		case quote:
			return j
		}
	}
	return -1
}

func closingBacktick(src string, i int) int {
	for j := i + 1; j < len(src); j++ {
		switch src[j] {
		case '\\':
			j++
		case '`':
			return j
		}
	}
	return -1
}

// unquote removes backslash escapes so an escaped interpolation is not read
// as an interpolation.
func unquote(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// describeExpression quotes what stood where a string was expected, so the
// failure message names the expression a reader has to go and look at
// (a template about to be built is fine; an identifier is not).
func describeExpression(rest string) string {
	n := 0
	for n < len(rest) && n < 40 {
		c := rest[n]
		if c == ',' || c == '\n' || c == '}' || c == ')' || c == ';' {
			break
		}
		n++
	}
	if n == 0 {
		return "an unreadable expression"
	}
	return fmt.Sprintf("%q", strings.TrimSpace(rest[:n]))
}

// TestEveryRawHTMLSiteIsARegisteredLiteral is the guard. It fails in both
// directions: an unregistered site of any shape, and a registered site whose
// content is no longer what was registered — including a site that has gone
// away, which is a registration nobody can vouch for any more.
func TestEveryRawHTMLSiteIsARegisteredLiteral(t *testing.T) {
	sites := scanInnerHTML(t)

	seen := make(map[string]bool, len(sites))
	var problems []string
	for _, s := range sites {
		seen[s.Key()] = true
		ex, registered := renderExemptions[s.Key()]
		switch {
		case !registered:
			problems = append(problems, fmt.Sprintf(
				"%s has an unregistered dangerouslySetInnerHTML (__html is %s)",
				s.Key(), orLiteral(s)))
		case !s.OK:
			problems = append(problems, fmt.Sprintf(
				"%s is registered as a literal but __html is now %s",
				s.Key(), s.Shape))
		case s.Literal != ex.Literal:
			problems = append(problems, fmt.Sprintf(
				"%s renders %q; the registration says %q",
				s.Key(), s.Literal, ex.Literal))
		}
	}
	for key := range renderExemptions {
		if !seen[key] {
			problems = append(problems, fmt.Sprintf(
				"%s is registered but no longer in the tree", key))
		}
	}
	sort.Strings(problems)

	if len(problems) > 0 {
		t.Fatalf("the raw-HTML guard refuses this tree:\n  %s\n\n"+
			"Registering a site means stating the exact string it renders and "+
			"why that string cannot carry request data. Deleting a site to make "+
			"this pass is the other half of the same failure: the registration "+
			"is what makes the deletion visible.",
			strings.Join(problems, "\n  "))
	}

	if len(sites) == 0 {
		t.Fatalf("no dangerouslySetInnerHTML was found anywhere in %s — the "+
			"guard scanned nothing, and a guard that scans nothing passes for "+
			"every tree", webRoot)
	}
}

func orLiteral(s htmlSite) string {
	if s.OK {
		return fmt.Sprintf("the literal %q", s.Literal)
	}
	return s.Shape
}

// TestNoOtherMarkupSinkExists holds the rest of the line: the sinks that are
// banned outright rather than registered. All seven are at zero today, and
// the assertion is what keeps a change from making one of them nonzero
// without anyone reading it.
func TestNoOtherMarkupSinkExists(t *testing.T) {
	var findings []string
	for _, rel := range webFiles(t) {
		src := readWebFile(t, rel)
		for _, sink := range markupSinks {
			for _, off := range occurrences(src, sink) {
				// "innerHTML" is a substring of the attribute the guard
				// above owns; the two rules must not double-report one site.
				if sink == "innerHTML" && strings.HasSuffix(src[:off], "dangerouslySet") {
					continue
				}
				findings = append(findings, fmt.Sprintf(
					"%s:%d uses %s", rel, 1+strings.Count(src[:off], "\n"), sink))
			}
		}
	}
	sort.Strings(findings)
	if len(findings) > 0 {
		t.Fatalf("the web app has markup sinks it must not have:\n  %s\n\n"+
			"Every one of these turns a string into markup or into code. If a "+
			"site genuinely needs one, remove it from markupSinks in this file "+
			"deliberately and say why — do not leave it unlisted.",
			strings.Join(findings, "\n  "))
	}
}
