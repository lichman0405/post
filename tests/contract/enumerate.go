package contract

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Route is one endpoint registration: the method and path pattern exactly as
// registered on a mux, and where it is registered.
type Route struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	File   string `json:"file"`
	Line   int    `json:"line"`
	// In is the function or method the registration sits in, e.g.
	// "(*API).Register" — the trail from a mux pattern back to its source.
	In string `json:"in"`
	// Handler is the handler expression as written.
	Handler string `json:"handler,omitempty"`
	// Raw is the pattern literal before the method/path split, kept so a
	// reviewer can check the split is not an artefact of this tool.
	Raw string `json:"raw,omitempty"`
}

// Mount is a registration whose handler is another mux (a subtree mount or a
// guard wrapper) rather than an endpoint handler. Mounts are recorded, not
// gated: they carry no endpoint of their own — the endpoint they make
// reachable is registered inside the mounted handler and appears in Routes
// under its own absolute pattern.
type Mount struct {
	Path    string `json:"path"`
	Handler string `json:"handler"`
	File    string `json:"file"`
	Line    int    `json:"line"`
	In      string `json:"in"`
}

// Unresolved is a call this tool could not turn into a (method, path) pair:
// a mux method it does not know, or a pattern that is not a string literal.
// It is reported rather than skipped — a registration the enumerator silently
// drops is exactly the failure this task exists to prevent, so an unknown
// registration form must be loud.
type Unresolved struct {
	File  string `json:"file"`
	Line  int    `json:"line"`
	In    string `json:"in"`
	Expr  string `json:"expr"`
	Why   string `json:"why"`
	Where string `json:"where"`
}

// Enumeration is everything one scan found.
type Enumeration struct {
	// Routes are endpoint registrations, sorted by path then method.
	Routes []Route
	// Mounts are subtree/guard mounts (no endpoint of their own).
	Mounts []Mount
	// Unresolved are registration-shaped calls the scanner did not understand.
	Unresolved []Unresolved
	// Advisory are string literals that look like an /api/v1 path and that no
	// part of the scan looked at — an outbound client URL, a routing-table key,
	// a route registered on a receiver the scanner cannot call a mux. They do
	// not fail anything: they are the completeness signal for a scan whose
	// whole claim is that it missed nothing, so an empty advisory list means
	// something and a non-empty one is read rather than hidden. A literal that
	// was read and classified (turned into a route, or reported as unresolved)
	// is not advisory: it is already in this document.
	Advisory []Unresolved
	// FilesScanned counts the .go files parsed (tests excluded by default).
	FilesScanned int

	// considered are the pattern literals the scan read, keyed by position.
	// Two passes over the same files must agree on what was already accounted
	// for, and the AST node is not available to the second pass, so the key is
	// the literal's position within its file.
	considered map[string]bool
}

// consideredKey identifies a literal by where it is.
func consideredKey(file string, pos token.Position) string {
	return fmt.Sprintf("%s:%d:%d", file, pos.Line, pos.Column)
}

func (e *Enumeration) consider(file string, pos token.Position) {
	if e.considered == nil {
		e.considered = map[string]bool{}
	}
	e.considered[consideredKey(file, pos)] = true
}

func (e *Enumeration) wasConsidered(file string, pos token.Position) bool {
	return e.considered[consideredKey(file, pos)]
}

// Registration forms this enumerator understands.
//
// The four that actually occur in this tree (found by scanning every call
// under cmd/ and internal/ whose receiver is a mux, not by grepping — a grep
// for "api/v1" counts comments, tests, outbound client URLs and Sprintf
// templates too, and misses method-qualified patterns entirely):
//
//	mux.HandleFunc("POST /api/v1/x", h)   method-qualified pattern
//	mux.Handle("GET /api/v1/x", h)        same, via Handle
//	mux.Handle("/api/v1/x", h)            no method: matches every method
//	v1.Handle("/api/v1/projects", sub.Routes())   subtree mount
//
// plus the chi-style shorthands (r.Get("/x", h) …), which no file in this tree
// uses today but which the scanner must still recognise: the point of an
// enumerator is that a NEW form is either understood or reported loudly, and
// "we only ever write it the way we write it now" is how a gate goes silently
// blind.
var (
	// methodShorthand maps a router method name onto the HTTP method it
	// registers.
	methodShorthand = map[string]string{
		"Get": "GET", "Post": "POST", "Put": "PUT", "Patch": "PATCH",
		"Delete": "DELETE", "Head": "HEAD", "Options": "OPTIONS",
		"Connect": "CONNECT", "Trace": "TRACE",
	}
	// handleNames are the two net/http ServeMux registration methods.
	handleNames = map[string]bool{"Handle": true, "HandleFunc": true}
	// knownForms is every method name the scanner treats as a registration.
	knownForms = func() map[string]bool {
		out := map[string]bool{}
		for k := range handleNames {
			out[k] = true
		}
		for k := range methodShorthand {
			out[k] = true
		}
		return out
	}()
)

// Enumerate scans dirs (relative to root) for endpoint registrations.
// Files named *_test.go are skipped: a test's own mux is not mounted in any
// binary, and counting them would turn "documented endpoint" into "somebody
// wrote this string in a test once".
func Enumerate(root string, dirs ...string) (*Enumeration, error) {
	out := &Enumeration{}
	var files []string
	for _, dir := range dirs {
		base := filepath.Join(root, dir)
		err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" || d.Name() == "node_modules" {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			files = append(files, p)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walking %s: %w", base, err)
		}
	}
	sort.Strings(files)
	for _, f := range files {
		if err := scanFile(root, f, out); err != nil {
			return nil, err
		}
		out.FilesScanned++
	}
	sort.Slice(out.Routes, func(i, j int) bool {
		if out.Routes[i].Path != out.Routes[j].Path {
			return out.Routes[i].Path < out.Routes[j].Path
		}
		return out.Routes[i].Method < out.Routes[j].Method
	})
	sort.Slice(out.Mounts, func(i, j int) bool {
		if out.Mounts[i].Path != out.Mounts[j].Path {
			return out.Mounts[i].Path < out.Mounts[j].Path
		}
		return out.Mounts[i].Line < out.Mounts[j].Line
	})
	sort.Slice(out.Unresolved, func(i, j int) bool {
		if out.Unresolved[i].File != out.Unresolved[j].File {
			return out.Unresolved[i].File < out.Unresolved[j].File
		}
		return out.Unresolved[i].Line < out.Unresolved[j].Line
	})
	advisory, err := routeShapedLiterals(root, files, out)
	if err != nil {
		return nil, err
	}
	out.Advisory = advisory
	return out, nil
}

// routeShapedLiterals collects string literals that mention the /api/v1 prefix
// and that the scan did not classify: a scan that claims to be complete owes
// the reader the list of the things it saw and did not look at. Format
// templates (they contain a verb) and the literals scanFile already read — as a
// registration, a mount, or an unresolved registration — are excluded, so what
// remains is small enough to read.
//
// The exclusion is positional rather than by name, because the question is
// "did the scan read this literal", not "does it look like the scan should
// have": a literal passed to a call this tool never treats as a registration is
// exactly what the advisory list is for.
func routeShapedLiterals(root string, files []string, enum *Enumeration) ([]Unresolved, error) {
	var out []Unresolved
	for _, path := range files {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		spans := funcSpans(file, fset)
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || enum.wasConsidered(rel, fset.Position(lit.Pos())) {
				return true
			}
			s, uerr := strconv.Unquote(lit.Value)
			if uerr != nil || !strings.Contains(s, "/api/v1") || strings.Contains(s, "%") {
				return true
			}
			pos := fset.Position(lit.Pos())
			out = append(out, Unresolved{
				File: rel, Line: pos.Line, In: spanName(spans, lit.Pos()),
				Expr: strconv.Quote(s),
				Why:  "an /api/v1 string the enumerator did not turn into a route — an outbound client URL, a routing-table key, or a route registered through a form the scanner could not attribute",
			})
			return true
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	return out, nil
}

type funcSpan struct {
	start, end token.Pos
	name       string
}

// scanFile parses one file and records every registration on a mux.
func scanFile(root, path string, out *Enumeration) error {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	muxes := muxIdents(file)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}
	spans := funcSpans(file, fset)
	for _, call := range allCalls(file) {
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		name := sel.Sel.Name
		recv := receiverName(sel.X)
		if recv == "" {
			continue
		}
		// A call on a mux whose first argument is a string literal is a
		// registration attempt: either a form we know, or one we must report.
		lit := firstStringLit(call)
		patternLit := firstStringLitNode(call)
		isMux := muxes[recv]
		// read marks the pattern literal as accounted for, so the advisory
		// pass does not report a literal this pass has already classified.
		read := func() {
			if patternLit != nil {
				out.consider(rel, fset.Position(patternLit.Pos()))
			}
		}
		if !isMux && !handleNames[name] {
			// Not a call the scanner can attribute to a mux. It is still worth
			// reporting when it LOOKS like a registration — a method-qualified
			// pattern is unambiguous — because a router type the scanner has
			// never seen would otherwise register routes in silence. A bare
			// path on an unknown receiver is not reported here: it is
			// indistinguishable from any other call that takes a path, and the
			// advisory list (routeShapedLiterals) names those instead.
			if lit != "" && looksLikeRegistration(lit, false) {
				read()
				out.Unresolved = append(out.Unresolved, Unresolved{
					File: rel, Line: fset.Position(call.Pos()).Line,
					In: spanName(spans, call.Pos()), Expr: exprString(fset, call),
					Why:   fmt.Sprintf("registration-shaped call on %q, which this enumerator does not know to be a mux — an unknown form must be loud, not skipped", recv),
					Where: recv,
				})
			}
			continue
		}
		if lit == "" {
			if knownForms[name] && muxes[recv] {
				out.Unresolved = append(out.Unresolved, Unresolved{
					File: rel, Line: fset.Position(call.Pos()).Line,
					In: spanName(spans, call.Pos()), Expr: exprString(fset, call),
					Why:   "registration whose pattern is not a string literal — the enumerator cannot know which path this mounts, so it is reported instead of guessed at",
					Where: "pattern",
				})
			}
			continue
		}
		if !knownForms[name] {
			// A mux with a method the scanner does not know: report it rather
			// than drop it. `mux.Stream("/api/v1/x", h)` is a registration even
			// though no mux in this tree has a Stream method.
			read()
			out.Unresolved = append(out.Unresolved, Unresolved{
				File: rel, Line: fset.Position(call.Pos()).Line,
				In: spanName(spans, call.Pos()), Expr: exprString(fset, call),
				Why:   fmt.Sprintf("%s.%s is not a registration form this enumerator knows — an unknown form must be loud, not skipped", recv, name),
				Where: recv,
			})
			continue
		}
		method, pattern, perr := splitPattern(lit, name)
		if perr != nil {
			read()
			out.Unresolved = append(out.Unresolved, Unresolved{
				File: rel, Line: fset.Position(call.Pos()).Line,
				In: spanName(spans, call.Pos()), Expr: exprString(fset, call),
				Why:   perr.Error(),
				Where: "pattern",
			})
			continue
		}
		pos := fset.Position(call.Pos())
		read()
		handler, handlerText := handlerExpr(fset, call)
		if isMount(handler, muxes) {
			out.Mounts = append(out.Mounts, Mount{
				Path: pattern, Handler: handlerText, File: rel, Line: pos.Line,
				In: spanName(spans, call.Pos()),
			})
			continue
		}
		out.Routes = append(out.Routes, Route{
			Method: method, Path: pattern, File: rel, Line: pos.Line,
			In: spanName(spans, call.Pos()), Handler: handlerText, Raw: lit,
		})
	}
	return nil
}

// muxIdents collects the identifiers that hold a mux: assigned from
// http.NewServeMux(), typed *http.ServeMux as a parameter, or — self-teaching,
// so a mux handed in from somewhere unforeseen is still recognised — the
// receiver of a Handle/HandleFunc call.
func muxIdents(file *ast.File) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			for i, rhs := range node.Rhs {
				if !isServeMuxCall(rhs) || i >= len(node.Lhs) {
					continue
				}
				if id := receiverName(node.Lhs[i]); id != "" {
					out[id] = true
				}
			}
		case *ast.ValueSpec:
			if !isServeMuxType(node.Type) {
				return true
			}
			for _, name := range node.Names {
				out[name.Name] = true
			}
		case *ast.Field:
			if !isServeMuxType(node.Type) {
				return true
			}
			for _, name := range node.Names {
				out[name.Name] = true
			}
		case *ast.CallExpr:
			sel, ok := node.Fun.(*ast.SelectorExpr)
			if !ok || !handleNames[sel.Sel.Name] {
				return true
			}
			if id := receiverName(sel.X); id != "" {
				out[id] = true
			}
		}
		return true
	})
	return out
}

func isServeMuxCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "NewServeMux" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "http"
}

func isServeMuxType(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.StarExpr:
		return isServeMuxType(t.X)
	case *ast.SelectorExpr:
		id, ok := t.X.(*ast.Ident)
		return ok && id.Name == "http" && t.Sel.Name == "ServeMux"
	}
	return false
}

// splitPattern turns a pattern literal into (method, path). A pattern without
// a method prefix matches every method, which is spelled "ANY" here (the same
// spelling specs/api/openapi-exemptions.yaml documents).
func splitPattern(lit, form string) (string, string, error) {
	s := strings.TrimSpace(lit)
	method := ""
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		method = strings.ToUpper(strings.TrimSpace(s[:i]))
		s = strings.TrimSpace(s[i+1:])
		if !validMethod(method) {
			return "", "", fmt.Errorf("pattern %q starts with %q, which is not an HTTP method", lit, method)
		}
	}
	if shorthand, ok := methodShorthand[form]; ok {
		if method != "" && method != shorthand {
			return "", "", fmt.Errorf("registered via .%s(…) but the pattern names method %s", form, method)
		}
		method = shorthand
	}
	if method == "" {
		method = "ANY"
	}
	if !strings.HasPrefix(s, "/") {
		return "", "", fmt.Errorf("pattern %q is not an absolute path: a relative pattern cannot be compared against the contract without resolving its mount prefix, and guessing one would put a route in the wrong place", lit)
	}
	return method, s, nil
}

// looksLikeRegistration reports whether a string literal is shaped like a
// route registration: a method-qualified pattern ("GET /api/v1/x", unambiguous
// wherever it appears), or a bare absolute path on a receiver already known to
// be a mux.
func looksLikeRegistration(lit string, isMux bool) bool {
	s := strings.TrimSpace(lit)
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		method := strings.ToUpper(strings.TrimSpace(s[:i]))
		rest := strings.TrimSpace(s[i+1:])
		return validMethod(method) && strings.HasPrefix(rest, "/")
	}
	return isMux && strings.HasPrefix(s, "/")
}

func validMethod(m string) bool {
	switch m {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "CONNECT", "TRACE", "ANY":
		return true
	}
	return false
}

// handlerExpr returns the handler expression of a registration call and its
// source text.
func handlerExpr(fset *token.FileSet, call *ast.CallExpr) (ast.Expr, string) {
	if len(call.Args) < 2 {
		return nil, ""
	}
	return call.Args[1], exprString(fset, call.Args[1])
}

// isMount reports whether the handler delegates to another mux rather than
// answering a request: `sub.Routes()`, a guard wrapper, or a bare mux value.
func isMount(e ast.Expr, muxes map[string]bool) bool {
	switch h := e.(type) {
	case *ast.CallExpr:
		sel, ok := h.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		return sel.Sel.Name == "Routes" || sel.Sel.Name == "Guard"
	case *ast.Ident:
		return muxes[h.Name]
	}
	return false
}

func firstStringLit(call *ast.CallExpr) string {
	lit := firstStringLitNode(call)
	if lit == nil {
		return ""
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return ""
	}
	return s
}

// firstStringLitNode is firstStringLit with the node kept: the advisory pass
// has to name the literal it did not read by position, not by its text (the
// same text can appear twice).
func firstStringLitNode(call *ast.CallExpr) *ast.BasicLit {
	if len(call.Args) == 0 {
		return nil
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return nil
	}
	return lit
}

func receiverName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	}
	return ""
}

func funcSpans(file *ast.File, fset *token.FileSet) []funcSpan {
	var out []funcSpan
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		out = append(out, funcSpan{start: fn.Pos(), end: fn.End(), name: funcName(fn, fset)})
	}
	return out
}

func funcName(fn *ast.FuncDecl, fset *token.FileSet) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	return "(" + exprString(fset, fn.Recv.List[0].Type) + ")." + fn.Name.Name
}

// spanName returns the innermost enclosing function of pos: the smallest span
// that contains it.
func spanName(spans []funcSpan, pos token.Pos) string {
	best, bestWidth := "", token.Pos(0)
	for _, s := range spans {
		if pos < s.start || pos > s.end {
			continue
		}
		width := s.end - s.start
		if best == "" || width < bestWidth {
			best, bestWidth = s.name, width
		}
	}
	return best
}

func allCalls(file *ast.File) []*ast.CallExpr {
	var out []*ast.CallExpr
	ast.Inspect(file, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			out = append(out, call)
		}
		return true
	})
	return out
}

func exprString(fset *token.FileSet, e ast.Expr) string {
	if e == nil {
		return ""
	}
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, e); err != nil {
		return "?"
	}
	return buf.String()
}
