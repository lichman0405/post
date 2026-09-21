package security

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The byte-exit registry. See doc.go for why this exists; the short version
// is that "which code can write a response body" must be a list somebody
// maintains, not a property somebody remembers.
//
// Discovery is syntactic (go/parser, stdlib only — this module takes no
// dependency for it). It finds every call in cmd/api that can hand bytes to
// the response writer, attributes it to its enclosing function, and
// requires the (file, function) pair to be in the table below with the same
// site count. Both directions fail: an unregistered write site, and a
// registered entry whose site has moved or gone.

// scanRoot is the directory tree whose write sites must all be registered.
//
// It is cmd/api — the API process, the one whose bytes a browser receives —
// and not all of cmd/. The other binaries are accounted for separately by
// TestOnlyTheAPIServesTheProductSurface, which fails when a new one appears
// so that the omission here is always a decision somebody made.
const scanRoot = "../../cmd/api"

// exitSite is one registered byte exit: the function that writes, what it
// writes, and how many write calls it contains.
type exitSite struct {
	// Func is the enclosing top-level function's name.
	Func string
	// Sites is how many byte-writing calls that function contains.
	//
	// Counted, not just listed, so that ADDING a write site to an already
	// registered function is a failure too. Listing the function alone
	// would let a second, unclassified payload ride along inside an
	// already-blessed handler.
	Sites int
	// Kind is what the response is allowed to be. The runtime probes assert
	// the real headers answer exactly this.
	Kind Kind
	// Wire is the media type and disposition the exit is expected to send,
	// asserted exactly (not "a header is present") by the probes.
	Wire string
	// Note is why this exit is what it is. One line; it is the review.
	Note string
}

// Kind mirrors the exit classification the guard judges against. It is
// re-declared here rather than imported so that a change to the production
// enum cannot silently reclassify a registered exit: the probes compare the
// production verdict against this name.
type Kind string

const (
	KindOpaque     Kind = "attachment"
	KindInlineText Kind = "inline-text"
	KindDocument   Kind = "document"
)

// registry is the list. Every key is "<relative file>:<function>".
//
// The entries are grouped by exit class, which is the thing a reviewer
// needs to see at a glance: everything opaque leaves as a download,
// everything inline is text a browser displays as text, and every document
// is served with the edge's script-blocking policy.
var registry = map[string]exitSite{
	// ---- the API's own envelopes ------------------------------------
	//
	// One class, many call sites: every product handler's JSON reaches the
	// wire through these writers, which is exactly why classifying the
	// writers is enough to classify the handlers that call them.
	"authhttp/envelope.go:WriteError": {
		Sites: 1, Kind: KindInlineText, Wire: "application/json, no disposition",
		Note: "the canonical error envelope (docs/22 §5, docs/45); every product handler's failure path ends here",
	},
	"authhttp/envelope.go:WriteJSON": {
		Sites: 1, Kind: KindInlineText, Wire: "application/json, no disposition",
		Note: "the canonical success envelope; JSON is data, and the edge sends the lockdown CSP beside it",
	},
	"jobs.go:newJobHandler": {
		Sites: 1, Kind: KindInlineText, Wire: "application/json, no disposition",
		Note: "the scaffold enqueue acknowledgement (POST /internal/jobs)",
	},
	"jobs.go:writeJSONError": {
		Sites: 1, Kind: KindInlineText, Wire: "application/json, no disposition",
		Note: "the enqueue endpoint's error body; its message is redacted through config.RedactForOutput",
	},
	"assetshttp/publish.go:writePublishBlocked": {
		Sites: 1, Kind: KindInlineText, Wire: "application/json, no disposition",
		Note: "the asset publish refusal and its impact report",
	},
	"assetshttp/derive.go:writeDeriveBlocked": {
		Sites: 1, Kind: KindInlineText, Wire: "application/json, no disposition",
		Note: "the asset derive refusal and its impact report (T0708): the second exit in " +
			"assetshttp, classified as its twin above is — a JSON envelope, so no browser " +
			"renders it and no probe is owed for the kind, but it states nosniff itself",
	},
	"knowledgehttp/publish.go:writePublishBlocked": {
		Sites: 1, Kind: KindInlineText, Wire: "application/json, no disposition",
		Note: "the knowledge publish refusal and its re-check report",
	},
	"conflicthttp/handlers.go:writeJSON": {
		Sites: 1, Kind: KindInlineText, Wire: "application/json, no disposition",
		Note: "the conflict report, written from pre-marshalled bytes so the detector's field order survives",
	},
	"attestationhttp/publish.go:writeAttestBlocked": {
		Sites: 1, Kind: KindInlineText, Wire: "application/json, no disposition",
		Note: "the attestation publish refusal (T0812) and its disclosure preview — the same document the preview route answers, so a caller can see which entry blocked it; what may appear in that preview is bounded where it is built, not here",
	},
	"searchhttp/draft.go:writeDraftJSON": {
		Sites: 1, Kind: KindInlineText, Wire: "application/json; charset=utf-8, no-store, no disposition",
		Note: "the Draft Research Context flow's two answers (start-project and confirm); written here rather " +
			"than through authhttp's envelope writers because the contract's 201 carries the draft document " +
			"itself, so the exit sets its own nosniff beside its own no-store — the body names a project and " +
			"an initial state that belong to the caller who just made them",
	},
	"searchhttp/handlers.go:writeSearchJSON": {
		Sites: 1, Kind: KindInlineText, Wire: "application/json; charset=utf-8, no-store, no disposition",
		Note: "the search answer (the search id and the answer document); no-store because it is the caller's own " +
			"scoped result and the citation trail it publishes is recorded under that actor",
	},

	// ---- payload bytes that came from somewhere else ------------------
	//
	// These are the exits the task's "outbound bytes are never interpreted"
	// is actually about: the bytes are a repository blob, a commit patch or
	// a release manifest — content this process did not author.
	"fileshttp/files.go:handleRaw": {
		Sites: 1, Kind: KindOpaque, Wire: `application/octet-stream, attachment; filename="<path>"`,
		Note: "GET .../files/raw — the download channel; provider bytes streamed through, never rendered",
	},
	"fileshttp/files.go:handleDiff": {
		Sites: 1, Kind: KindInlineText, Wire: "text/plain; charset=utf-8, no disposition",
		Note: "GET .../files/diff — the raw patch view; text/plain is displayed as text and nosniff pins it",
	},
	"releasehttp/release_handlers.go:handleGetReleaseManifest": {
		Sites: 1, Kind: KindOpaque, Wire: `application/json, attachment; filename="release-<version>.manifest.json"`,
		Note: "the manifest is handed over as a file, not rendered; the version in the filename is the release's own",
	},

	// ---- rendered documents -------------------------------------------
	//
	// A document is served inline and IS parsed by the browser, so it is
	// only safe with a policy that forbids script. The edge picks that
	// policy from the same media type; the probes assert both halves.
	"feedshttp/handlers.go:writeDocument": {
		Sites: 1, Kind: KindDocument, Wire: "application/atom+xml | application/rss+xml, no disposition",
		Note: "the Atom/RSS feeds; XML is a parsed document, so it carries the script-blocking policy too",
	},
	"rsghttp/page.go:handleObjectDetailPage": {
		Sites: 1, Kind: KindDocument, Wire: "text/html; charset=utf-8, no disposition",
		Note: "the object detail page (html/template)",
	},
	"rsghttp/page.go:renderObjectPageError": {
		Sites: 1, Kind: KindDocument, Wire: "text/html; charset=utf-8, no disposition",
		Note: "the neutral HTML error page",
	},
	"rsghttp/overview.go:handleOverviewPage": {
		Sites: 1, Kind: KindDocument, Wire: "text/html; charset=utf-8, no disposition",
		Note: "the project overview page",
	},
	"rsghttp/outline.go:handleResearchPage": {
		Sites: 1, Kind: KindDocument, Wire: "text/html; charset=utf-8, no disposition",
		Note: "the research (outline) page",
	},
}

// writeMethods are the selector names that put bytes on the wire. WriteHeader
// is deliberately absent: a status line is not a payload.
var writeMethods = map[string]bool{
	"Write":           true,
	"WriteString":     true,
	"Fprint":          true,
	"Fprintf":         true,
	"Fprintln":        true,
	"Execute":         true,
	"ExecuteTemplate": true,
	"Copy":            true,
	"CopyN":           true,
	"CopyBuffer":      true,
}

// newEncoderCall is the other shape a byte write takes: json.NewEncoder(w)
// returns a value whose Encode method writes, so the response writer appears
// only as an argument to this constructor. It is matched by name (and
// counted as the site) rather than by following the value into its Encode
// call, which would need a type checker.
const newEncoderCall = "NewEncoder"

// discovered is one write site found in the tree.
type discovered struct {
	file string
	fn   string
	line int
}

func (d discovered) key() string { return d.file + ":" + d.fn }

// TestEveryByteExitIsRegistered is the structural half of the guard.
func TestEveryByteExitIsRegistered(t *testing.T) {
	found := discoverExits(t, scanRoot)

	bySite := map[string][]discovered{}
	for _, d := range found {
		bySite[d.key()] = append(bySite[d.key()], d)
	}

	var unregistered []string
	for key, sites := range bySite {
		if _, ok := registry[key]; !ok {
			for _, s := range sites {
				unregistered = append(unregistered, fmt.Sprintf("%s:%d", s.file, s.line))
			}
		}
	}
	if len(unregistered) > 0 {
		sort.Strings(unregistered)
		t.Errorf("these write sites are not in the byte-exit registry:\n  %s\n\n"+
			"Every place that can hand bytes to a client must be classified: decide whether the "+
			"response is an attachment (KindOpaque), inline text (KindInlineText) or a rendered "+
			"document (KindDocument), add the row to registry in %s with the method it sends, and "+
			"add a probe for it to TestRegisteredExitsSendTheHeadersTheyDeclare.",
			strings.Join(unregistered, "\n  "), "tests/security/exits_test.go")
	}

	var missing []string
	for key, want := range registry {
		got := bySite[key]
		switch {
		case len(got) == 0:
			missing = append(missing, fmt.Sprintf("%s (registered, no write site found)", key))
		case len(got) != want.Sites:
			missing = append(missing, fmt.Sprintf("%s: %d write sites found, %d registered",
				key, len(got), want.Sites))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("the registry no longer matches the tree:\n  %s\n\n"+
			"A registered exit that moved or gained a second write site is a new, unclassified "+
			"byte exit: update the row (and the probes) deliberately, or remove it.",
			strings.Join(missing, "\n  "))
	}
}

// TestEveryRegisteredExitStatesItsOwnHeaders is the property that applies
// to every row, including the ones no probe can drive (an unexported writer
// inside package main, reachable only from the process's own wiring).
//
// Each write site's enclosing function must itself declare the two facts
// that make it an exit rather than an accident:
//
//	Content-Type            what the bytes are
//	X-Content-Type-Options: nosniff   that the browser must believe it
//
// "Itself" is transitive through unexported helpers in the same directory
// (rsghttp's four page exits all call writeDocumentHeaders), because
// inlining those would say the same thing four times. What is NOT allowed
// is inheriting either fact from a layer above: the edge middleware is one
// deployment away from being absent, and an exit that depends on it is an
// exit that is wrong everywhere else it is mounted.
func TestEveryRegisteredExitStatesItsOwnHeaders(t *testing.T) {
	dirs, err := loadPackages(scanRoot)
	if err != nil {
		t.Fatalf("load packages: %v", err)
	}
	found := discoverExits(t, scanRoot)
	if len(found) == 0 {
		t.Fatal("no write sites found — a guard that measures nothing must never pass")
	}

	for _, site := range found {
		key := site.key()
		if _, ok := registry[key]; !ok {
			continue // TestEveryByteExitIsRegistered already reports this
		}
		dir := filepath.Dir(site.file)
		body, ok := functionBody(dirs, dir, site.fn)
		if !ok {
			t.Errorf("%s: no top-level function %q found in %s", key, site.fn, dir)
			continue
		}
		sets := headerSetsIn(dirs, dir, body, map[string]bool{})
		if !sets[sniffPair] {
			t.Errorf("%s does not set X-Content-Type-Options: nosniff — not in the function and "+
				"not in any unexported helper it calls. The exit must state it itself; inheriting "+
				"it from the edge above means the fact is absent wherever the edge is absent.", key)
		}
		if !sets[contentTypePair] {
			t.Errorf("%s sets no Content-Type: the browser is left to decide what the bytes "+
				"are, which is the whole sniffing problem nosniff exists to prevent.", key)
		}
	}
}

// The two header writes the rule above looks for, as the pair of string
// literals a `Set` call passes.
const (
	sniffPair       = "X-Content-Type-Options\x00nosniff"
	contentTypePair = "Content-Type\x00*"
)

// pkg is every top-level function of one directory, by name.
type pkg map[string]*ast.FuncDecl

// loadPackages parses the non-test Go files of every directory under root.
func loadPackages(root string) (map[string]pkg, error) {
	out := map[string]pkg{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		dir, _ := filepath.Rel(root, filepath.Dir(path))
		dir = filepath.ToSlash(dir)
		if out[dir] == nil {
			out[dir] = pkg{}
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				out[dir][fn.Name.Name] = fn
			}
		}
		return nil
	})
	return out, err
}

func functionBody(pkgs map[string]pkg, dir, name string) (*ast.FuncDecl, bool) {
	fn, ok := pkgs[dir][name]
	return fn, ok
}

// headerSetsIn walks a function body and the bodies of the same-directory
// functions it calls, collecting the header pairs it writes. seen guards
// against a call cycle (two helpers calling each other).
func headerSetsIn(pkgs map[string]pkg, dir string, fn *ast.FuncDecl, seen map[string]bool) map[string]bool {
	out := map[string]bool{}
	if fn == nil || seen[fn.Name.Name] {
		return out
	}
	seen[fn.Name.Name] = true

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if pair, ok := headerPair(call); ok {
			out[pair] = true
			return true
		}
		// A call to a same-directory helper: descend into it. Anything
		// else (another package's function) is not this exit's own
		// statement — that is the whole point of the rule.
		if id, ok := call.Fun.(*ast.Ident); ok {
			if callee, ok := pkgs[dir][id.Name]; ok {
				for pair := range headerSetsIn(pkgs, dir, callee, seen) {
					out[pair] = true
				}
			}
		}
		return true
	})
	return out
}

// headerPair recognises `X.Set("Name", "value")` and reports it as
// "Name\x00value". A non-literal argument (a computed value) is reported as
// "Name\x00*": the rule checks that the header is declared, not what the
// expression evaluates to — the runtime probes check the values.
func headerPair(call *ast.CallExpr) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Set" || len(call.Args) < 2 {
		return "", false
	}
	name, ok := stringLiteral(call.Args[0])
	if !ok {
		return "", false
	}
	// Content-Type is matched by NAME only: whether the value is a literal
	// or a computed one (feedshttp passes format.ContentType()) is not this
	// rule's business. The runtime probes assert the value.
	if name == "Content-Type" {
		return contentTypePair, true
	}
	value, ok := stringLiteral(call.Args[1])
	if !ok {
		return "", false
	}
	return name + "\x00" + value, true
}

func stringLiteral(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	return strings.Trim(lit.Value, `"`), true
}

// TestOnlyTheAPIServesTheProductSurface is the other half of scanRoot's
// narrowness: the guard above only looks at cmd/api, so this test makes a
// NEW binary under cmd/ a decision rather than a gap. Each entry names the
// reason that binary's responses are outside the API's byte-exit surface.
var nonAPIBinaries = map[string]string{
	"devredis": "an in-process Redis protocol server for the dev/smoke loop (docs/64): it serves " +
		"the RESP protocol on a private port, never a browser, and it holds no product data",
	"worker": "the background job consumer: it speaks Redis and PostgreSQL, opens no listener, " +
		"and its output is its log",
	"rddev": "the orchestrator CLI: a terminal program. Its stdout is a person's screen, and " +
		"the API's headers are meaningless there",
	"mcp-server": "a stub binary that answers 'not implemented yet' on its own port. It mounts " +
		"no product data and none of the edge chain; when it serves real bytes it needs its own " +
		"edge and its own registry, and this entry is where that decision gets made",
}

func TestOnlyTheAPIServesTheProductSurface(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(scanRoot, ".."))
	if err != nil {
		t.Fatalf("read cmd/: %v", err)
	}
	var found []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(scanRoot, "..", e.Name(), "main.go")); err != nil {
			continue // not a binary
		}
		found = append(found, e.Name())
	}
	if len(found) == 0 {
		t.Fatal("no cmd/*/main.go found — the scan root is wrong, and a guard that finds nothing " +
			"must never pass")
	}
	sort.Strings(found)
	for _, name := range found {
		if name == "api" {
			continue
		}
		if _, ok := nonAPIBinaries[name]; !ok {
			t.Errorf("cmd/%s is a new binary and this guard does not know what it serves.\n"+
				"Decide: does it serve product bytes over HTTP? If yes, it needs the edge "+
				"(internal/security) and its own byte-exit registry. If no, add it to "+
				"nonAPIBinaries with the reason.", name)
		}
	}
	for name := range nonAPIBinaries {
		if _, err := os.Stat(filepath.Join(scanRoot, "..", name, "main.go")); err != nil {
			t.Errorf("nonAPIBinaries names cmd/%s, which no longer exists: remove the row", name)
		}
	}
}

// discoverExits parses every non-test Go file under root and returns the
// call sites that can put bytes on the wire.
func discoverExits(t *testing.T, root string) []discovered {
	t.Helper()
	var out []discovered
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		rel, _ := filepath.Rel(root, path)
		out = append(out, exitsInFile(fset, file, filepath.ToSlash(rel))...)
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", root, err)
	}
	return out
}

// exitsInFile walks one file, tracking the current top-level function and
// the identifiers bound to an http.ResponseWriter, and reports the write
// sites. Nested function literals are attributed to their enclosing
// top-level function — which is what makes the counted Sites meaningful for
// handlers registered as closures (jobs.go's newJobHandler is one).
func exitsInFile(fset *token.FileSet, file *ast.File, rel string) []discovered {
	var out []discovered
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		// The writers in scope: the function's own parameters, plus the
		// parameters of every function literal inside it — a handler
		// registered as a closure (mux.HandleFunc(..., func(w, r) {...}))
		// names its response writer in the literal, not in the outer
		// declaration.
		writers := map[string]bool{}
		addResponseWriterParams(writers, fn.Type.Params)
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if lit, ok := n.(*ast.FuncLit); ok {
				addResponseWriterParams(writers, lit.Type.Params)
			}
			return true
		})
		if len(writers) == 0 {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if isByteWrite(call, writers) {
				out = append(out, discovered{file: rel, fn: fn.Name.Name, line: fset.Position(call.Pos()).Line})
			}
			return true
		})
	}
	return out
}

// addResponseWriterParams collects the parameter names declared as
// http.ResponseWriter.
func addResponseWriterParams(writers map[string]bool, params *ast.FieldList) {
	if params == nil {
		return
	}
	for _, param := range params.List {
		if !isResponseWriterType(param.Type) {
			continue
		}
		for _, name := range param.Names {
			writers[name.Name] = true
		}
	}
}

func isResponseWriterType(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "ResponseWriter" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "http"
}

// isByteWrite reports whether one call puts payload bytes on the response.
func isByteWrite(call *ast.CallExpr, writers map[string]bool) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	touches := func(e ast.Expr) bool {
		id, ok := e.(*ast.Ident)
		return ok && writers[id.Name]
	}
	if writeMethods[sel.Sel.Name] {
		if touchesExpr(sel.X, touches) {
			return true
		}
		for _, arg := range call.Args {
			if touches(arg) {
				return true
			}
		}
		return false
	}
	if sel.Sel.Name == newEncoderCall {
		for _, arg := range call.Args {
			if touches(arg) {
				return true
			}
		}
	}
	return false
}

// touchesExpr reports whether an expression is (or directly wraps) a
// response-writer identifier. It is a one-level unwrap: the selector
// receiver in every write site in this tree is either the parameter itself
// or a value derived on the same line.
func touchesExpr(e ast.Expr, match func(ast.Expr) bool) bool {
	if match(e) {
		return true
	}
	if p, ok := e.(*ast.ParenExpr); ok {
		return touchesExpr(p.X, match)
	}
	return false
}
