package contract

import (
	"sort"
	"strings"
	"testing"
)

// formsTree holds one registration per form this tree uses today plus the forms
// it does not use yet. Every expectation below is about the SYNTHETIC tree: no
// test in this package reads cmd/ or internal/, so the instrument can be run by
// any task's gate without that task inheriting this one's reading.
const formsTree = "testdata/forms"

func enumerateForms(t *testing.T) *Enumeration {
	t.Helper()
	enum, err := Enumerate(formsTree, "api")
	if err != nil {
		t.Fatalf("Enumerate(%s): %v", formsTree, err)
	}
	return enum
}

func keys(routes []Route) []string {
	out := make([]string, 0, len(routes))
	for _, r := range routes {
		out = append(out, r.Method+" "+r.Path)
	}
	sort.Strings(out)
	return out
}

func TestEnumerateCoversEveryRegistrationForm(t *testing.T) {
	enum := enumerateForms(t)
	want := []string{
		// method-qualified pattern, HandleFunc, inside a func taking the mux
		// as a *http.ServeMux parameter rather than making one itself.
		"DELETE /api/v1/things/{thingId}",
		// method-qualified pattern via Handle, handler a composite literal.
		"GET /api/v1/things/{thingId}",
		// no method prefix: matches every method, spelled ANY.
		"ANY /api/v1/things/{thingId}/history",
		"POST /api/v1/things",
		// handler is http.HandlerFunc(...), which is a handler, not a mount.
		"ANY /api/v1/sub/two",
		// absolute patterns registered inside a mounted surface.
		"GET /api/v1/sub/one",
		"PATCH /api/v1/sub/three",
		// the chi-style shorthand, which no file in this tree uses yet.
		"DELETE /api/v1/chi/{id}",
		"GET /api/v1/chi/one",
		"POST /api/v1/chi/two",
		// a real registration in a file that also carries commented-out ones.
		"GET /api/v1/commented/real",
		// outside the contract's server prefix: enumerated, gated by nothing.
		"GET /healthz",
	}
	sort.Strings(want)
	got := keys(enum.Routes)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("enumerated routes differ from the expected set\n got:\n  %s\nwant:\n  %s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestEnumerateRecordsMountsNotEndpoints(t *testing.T) {
	enum := enumerateForms(t)
	var got []string
	for _, m := range enum.Mounts {
		got = append(got, m.Path+" -> "+m.Handler)
	}
	sort.Strings(got)
	want := []string{
		"/api/v1/ -> guarder{}.Guard(mux)",
		"/api/v1/sub -> subRouter{}.Routes()",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("mounts differ\n got: %q\nwant: %q", got, want)
	}
}

func TestEnumerateNamesTheRegistrationSite(t *testing.T) {
	enum := enumerateForms(t)
	type site struct{ in, file string }
	want := map[string]site{
		// a mux passed in as a parameter, registered on inside a free function
		"DELETE /api/v1/things/{thingId}": {"register", "api/main.go"},
		// a method on a type (this tree's per-surface packages look like this)
		"PATCH /api/v1/sub/three":              {"(*API).Register", "api/sub/wiring.go"},
		"GET /api/v1/chi/one":                  {"(*Router).Register", "api/chi/chi.go"},
		"GET /api/v1/commented/real":           {"Register", "api/commented/c.go"},
		"ANY /api/v1/things/{thingId}/history": {"main", "api/main.go"},
	}
	for _, r := range enum.Routes {
		w, ok := want[r.Method+" "+r.Path]
		if !ok {
			continue
		}
		if r.In != w.in || r.File != w.file {
			t.Errorf("%s %s: registered in %q in %s, want %q in %s",
				r.Method, r.Path, r.In, r.File, w.in, w.file)
		}
		if r.Line == 0 {
			t.Errorf("%s %s has no line number", r.Method, r.Path)
		}
	}
}

func TestEnumerateSkipsTestsAndComments(t *testing.T) {
	enum := enumerateForms(t)
	forbidden := []string{
		"/api/v1/only-in-a-test", // inside a _test.go: a test's mux is not mounted
		"/api/v1/commented-out",  // inside a comment
		"/api/v1/in-prose",       // inside prose in a comment
		"/api/v1/computed",       // a pattern built at run time (see TestEnumerateReportsWhatItCannotRead)
	}
	for _, r := range enum.Routes {
		for _, bad := range forbidden {
			if strings.Contains(r.Path, bad) {
				t.Errorf("route %s %s (%s:%d) must not be enumerated", r.Method, r.Path, r.File, r.Line)
			}
		}
	}
	for _, a := range enum.Advisory {
		for _, bad := range forbidden[:3] {
			if strings.Contains(a.Expr, bad) {
				t.Errorf("advisory %s:%d reports %s, which is not even a string literal in code", a.File, a.Line, a.Expr)
			}
		}
	}
	// The real registration in that same file IS enumerated — the scanner reads
	// the AST, so a commented-out registration next to a live one is invisible
	// while the live one is found.
	found := false
	for _, r := range enum.Routes {
		if r.Path == "/api/v1/commented/real" {
			found = true
		}
	}
	if !found {
		t.Error("the live registration in commented/c.go was not enumerated")
	}
}

func TestEnumerateReportsWhatItCannotRead(t *testing.T) {
	enum := enumerateForms(t)
	type want struct{ file, why string }
	// in the order the scan sorts them: by file, then line
	wants := []want{
		{"api/dynamic/dyn.go", "not a string literal"},
		{"api/relative/rel.go", "not an absolute path"},
		{"api/unknownform/un.go", "not a registration form this enumerator knows"},
	}
	if len(enum.Unresolved) != len(wants) {
		t.Fatalf("got %d unresolved registrations, want %d: %+v", len(enum.Unresolved), len(wants), enum.Unresolved)
	}
	for i, w := range wants {
		u := enum.Unresolved[i]
		if u.File != w.file {
			t.Errorf("unresolved[%d] is in %s, want %s", i, u.File, w.file)
		}
		if !strings.Contains(u.Why, w.why) {
			t.Errorf("unresolved[%d] (%s) says %q, want it to mention %q", i, w.file, u.Why, w.why)
		}
		if u.Line == 0 || u.In == "" || u.Expr == "" {
			t.Errorf("unresolved[%d] is missing its location or expression: %+v", i, u)
		}
	}
}

func TestEnumerateAdvisoryIsTheCompletenessSignal(t *testing.T) {
	enum := enumerateForms(t)
	var got []string
	for _, a := range enum.Advisory {
		got = append(got, a.File+" "+a.Expr)
	}
	sort.Strings(got)
	want := []string{
		`api/dynamic/dyn.go "/api/v1/computed"`,
		`api/unknownform/un.go "/api/v1/unknown-router/stream"`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("advisory literals differ\n got: %q\nwant: %q", got, want)
	}
}

func TestEnumerateCountsTheFilesItParsed(t *testing.T) {
	enum := enumerateForms(t)
	// seven .go files in the tree, one of them a _test.go that must be skipped
	if enum.FilesScanned != 7 {
		t.Errorf("FilesScanned = %d, want 7", enum.FilesScanned)
	}
}

func TestSplitPatternSeparatesMethodFromPath(t *testing.T) {
	cases := []struct{ lit, form, method, path string }{
		{"POST /api/v1/x", "HandleFunc", "POST", "/api/v1/x"},
		{"/api/v1/x", "Handle", "ANY", "/api/v1/x"},
		{"  PATCH   /api/v1/x  ", "Handle", "PATCH", "/api/v1/x"},
	}
	for _, c := range cases {
		m, p, err := splitPattern(c.lit, c.form)
		if err != nil {
			t.Errorf("splitPattern(%q): %v", c.lit, err)
			continue
		}
		if m != c.method || p != c.path {
			t.Errorf("splitPattern(%q, %q) = (%q, %q), want (%q, %q)", c.lit, c.form, m, p, c.method, c.path)
		}
	}
	if _, _, err := splitPattern("things/{thingId}", "HandleFunc"); err == nil {
		t.Error("a pattern with no leading slash must be refused, not compared as if it were absolute")
	}
	if _, _, err := splitPattern("FIESTA /api/v1/x", "HandleFunc"); err == nil {
		t.Error("a non-method prefix must be refused, not read as a method")
	}
	if _, _, err := splitPattern("POST /api/v1/x", "Get"); err == nil {
		t.Error("a shorthand that contradicts the pattern's method must be refused, not silently re-labelled")
	}
	// the shorthand supplies the method the pattern omits
	if m, p, err := splitPattern("/api/v1/x", "Get"); err != nil || m != "GET" || p != "/api/v1/x" {
		t.Errorf("splitPattern(/api/v1/x, Get) = (%q, %q, %v), want (GET, /api/v1/x, nil)", m, p, err)
	}
}
