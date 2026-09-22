package contract

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// MCPTool is one tool declared in specs/mcp/tools.json.
type MCPTool struct {
	Name string   `json:"name"`
	Mode string   `json:"mode"`
	Args []string `json:"args"`
}

// MCPCatalog is a parsed specs/mcp/tools.json.
type MCPCatalog struct {
	Version int       `json:"version"`
	Tools   []MCPTool `json:"tools"`
	Path    string    `json:"-"`
}

// ReadMCPCatalog parses the machine definition of the MCP tool surface.
func ReadMCPCatalog(path string) (*MCPCatalog, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var doc MCPCatalog
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	doc.Path = path
	return &doc, nil
}

// ToolSite is one place a tool name appears in Go source.
type ToolSite struct {
	File string `json:"file"`
	Line int    `json:"line"`
	In   string `json:"in"`
	// Form is what kind of occurrence it is:
	//
	//	dispatch — an argument of a call that is not a message formatter: a
	//	           registration, a lookup, a dispatch table.
	//	message  — an argument of a print/log/error helper: the name is in the
	//	           text of a message, which is a mention, not a wiring.
	//	literal  — a bare literal expression, assigned or compared somewhere.
	//
	// The dispatch/message split is a stated approximation (the denylist below
	// is a list of message helpers, not a proof about every call), which is why
	// every site is kept in the report with its line: the column is a hint and
	// the reader can go look.
	Form string `json:"form"`
}

// ToolReference is the implementation-side reading for one tool.
type ToolReference struct {
	Name  string     `json:"name"`
	Sites []ToolSite `json:"sites"`
}

// MCPReport is the reconciliation: what the catalog declares against what the
// tree implements.
type MCPReport struct {
	CatalogPath string       `json:"catalog_path"`
	Declared    int          `json:"declared_tools"`
	Implemented int          `json:"tools_with_a_dispatch_site"`
	Missing     int          `json:"tools_the_catalog_declares_and_the_tree_does_not_dispatch"`
	Tools       []MCPToolRow `json:"tools"`
	// OutOfCatalog are tool-name-shaped strings found in the tree that the
	// catalog does not declare — an implemented-but-undeclared tool is the
	// mirror defect and is reported rather than dropped.
	OutOfCatalog []ToolSite `json:"implemented_but_not_in_catalog"`
	// Notes carries what the scan cannot decide, in the report rather than in
	// a comment: a literal mention is not a dispatch, and the difference
	// matters enough to be printed.
	Notes []string `json:"notes"`
}

// MCPToolRow is one line of the reconciliation table.
type MCPToolRow struct {
	Name string   `json:"name"`
	Mode string   `json:"mode"`
	Args []string `json:"args"`
	// Sites are where the tool name appears in non-test Go source.
	Sites []ToolSite `json:"sites"`
	// Dispatched is true when at least one site's form is "dispatch" — the
	// weakest mechanical evidence that something could route a call to it.
	Dispatched bool `json:"dispatched"`
	// MentionedInCatalogDoc is a substring hit in docs/47 (prose, lossy).
	MentionedInCatalogDoc bool `json:"mentioned_in_catalog_doc"`
	// Conclusion is the per-tool verdict this reconciliation exists to give:
	// "implemented" when the tree has a dispatch site, "not_implemented" when
	// the catalogue promises a tool nothing takes. It is mechanical, so it is
	// rewritten on every run rather than preserved.
	Conclusion string `json:"conclusion"`
	// Reason states the evidence for that conclusion, with the file:line.
	Reason string `json:"reason"`
}

// ScanToolReferences finds every occurrence of a declared tool name as a Go
// string literal under dirs. Tests are skipped for the same reason the route
// enumerator skips them: a name in a test is not a wiring.
func ScanToolReferences(root string, dirs []string, names []string) (map[string][]ToolSite, error) {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	out := map[string][]ToolSite{}
	for _, dir := range dirs {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
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
			fset := token.NewFileSet()
			file, perr := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
			if perr != nil {
				return fmt.Errorf("parsing %s: %w", p, perr)
			}
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil {
				rel = p
			}
			spans := funcSpans(file, fset)
			// argForm is how the literal is used, by the call it is an argument
			// of: dispatch for a real call, message for a text helper.
			argForm := map[*ast.BasicLit]string{}
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				form := "dispatch"
				if callName(call) != "" && messageCallNames[callName(call)] {
					form = "message"
				}
				for _, a := range call.Args {
					if lit, ok := a.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						// A message call wins over a dispatch when one literal
						// is somehow both: only the stricter reading is safe.
						if cur, seen := argForm[lit]; !seen || (cur == "dispatch" && form == "message") {
							argForm[lit] = form
						}
					}
				}
				return true
			})
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				s, uerr := strconv.Unquote(lit.Value)
				if uerr != nil || !want[s] {
					return true
				}
				form := "literal"
				if f, ok := argForm[lit]; ok {
					form = f
				}
				pos := fset.Position(lit.Pos())
				out[s] = append(out[s], ToolSite{File: rel, Line: pos.Line, In: spanName(spans, lit.Pos()), Form: form})
				return true
			})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	for k := range out {
		sort.Slice(out[k], func(i, j int) bool {
			if out[k][i].File != out[k][j].File {
				return out[k][i].File < out[k][j].File
			}
			return out[k][i].Line < out[k][j].Line
		})
	}
	return out, nil
}

// MCPOptions selects the inputs of a reconciliation.
type MCPOptions struct {
	Root string
	Dirs []string
	// CatalogPath and DocPath are relative to Root.
	CatalogPath string
	DocPath     string
}

func (o *MCPOptions) withDefaults() {
	if o.Root == "" {
		o.Root = "."
	}
	if len(o.Dirs) == 0 {
		o.Dirs = []string{"cmd", "internal"}
	}
	if o.CatalogPath == "" {
		o.CatalogPath = filepath.Join("specs", "mcp", "tools.json")
	}
	if o.DocPath == "" {
		o.DocPath = filepath.Join("docs", "47_MCP_TOOL_CATALOG.md")
	}
}

// ReconcileMCP compares the catalog against the tree. The prose catalogue
// (docs/47) is only used for a substring "is it mentioned there" column — the
// implementation is the truth and the prose is the copy, which is the direction
// the task states.
func ReconcileMCP(opts MCPOptions) (*MCPReport, error) {
	opts.withDefaults()
	root, dirs, catalogPath := opts.Root, opts.Dirs, opts.CatalogPath
	cat, err := ReadMCPCatalog(filepath.Join(root, catalogPath))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(cat.Tools))
	for _, t := range cat.Tools {
		names = append(names, t.Name)
	}
	refs, err := ScanToolReferences(root, dirs, names)
	if err != nil {
		return nil, err
	}
	docText, _ := os.ReadFile(filepath.Join(root, opts.DocPath))

	outside, err := scanToolShapedLiterals(root, dirs, names)
	if err != nil {
		return nil, err
	}
	if outside == nil {
		outside = []ToolSite{}
	}
	rep := &MCPReport{CatalogPath: catalogPath, Declared: len(cat.Tools), OutOfCatalog: outside}
	rep.Notes = append(rep.Notes,
		"conclusion is mechanical and rewritten every run: implemented = at least one dispatch site, not_implemented = none. Whether an unimplemented tool should be built or dropped from the catalogue is a product decision, and this report does not pretend to make it.",
		"a tool-name literal is evidence of a MENTION, not of a working dispatch: cmd/mcp-server answers /mcp with 501 by design (T0006), so a tool with no dispatch site is unimplemented in every sense that matters, and a tool with a dispatch site still deserves a human look.",
		"undeclared tool names are reported from files whose path names mcp — the surface a tool can be wired from. Literals shaped like a tool name elsewhere in the tree are other people's identifiers (the doctor's fixture keys, a drill's column names), and a list made mostly of those is a list nobody reads.",
		"docs/47 mentions are substring hits in prose, which abbreviates (`project.get/list`); the column is a hint, never the verdict.")
	for _, t := range cat.Tools {
		sites := refs[t.Name]
		row := MCPToolRow{
			Name: t.Name, Mode: t.Mode, Args: t.Args, Sites: sites,
			MentionedInCatalogDoc: strings.Contains(string(docText), t.Name),
		}
		for _, s := range sites {
			if s.Form == "dispatch" {
				row.Dispatched = true
			}
		}
		if sites == nil {
			row.Sites = []ToolSite{}
		}
		if row.Dispatched {
			rep.Implemented++
			row.Conclusion = "implemented"
			row.Reason = fmt.Sprintf("%s declares it and the tree has a dispatch site: %s:%d in %s",
				catalogPath, sites[0].File, sites[0].Line, sites[0].In)
		} else {
			rep.Missing++
			row.Conclusion = "not_implemented"
			if len(sites) == 0 {
				row.Reason = fmt.Sprintf("%s declares it and no call in the tree takes it: the catalogue promises a tool nothing can route to", catalogPath)
			} else {
				row.Reason = fmt.Sprintf("%s declares it and the tree only NAMES it (%s:%d, %s) — a name in a string is not a wiring",
					catalogPath, sites[0].File, sites[0].Line, sites[0].Form)
			}
		}
		rep.Tools = append(rep.Tools, row)
	}
	sort.Slice(rep.Tools, func(i, j int) bool { return rep.Tools[i].Name < rep.Tools[j].Name })
	return rep, nil
}

// toolNameRe is the shape tools.json uses: lowercase dotted names,
// `domain.verb`.
var toolNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*\.[a-z][a-z0-9_]*$`)

// messageCallNames are the helper names whose string arguments are prose rather
// than values: a tool name inside one of these calls is being written into a
// message. The list is a hint, not a proof — the site is still reported, only
// its form changes, so a mistake here moves a line between two visible columns
// instead of hiding it.
var messageCallNames = map[string]bool{
	// fmt / log / slog
	"Print": true, "Printf": true, "Println": true,
	"Sprint": true, "Sprintf": true, "Sprintln": true,
	"Fprint": true, "Fprintf": true, "Fprintln": true,
	"Log": true, "Logf": true, "Logln": true,
	"Info": true, "Infof": true, "Infoln": true, "Infow": true,
	"Warn": true, "Warnf": true, "Warnw": true,
	"Debug": true, "Debugf": true, "Debugw": true,
	"Trace": true, "Tracef": true,
	// error construction and the testing helpers
	"New": true, "Newf": true, "Wrap": true, "Wrapf": true,
	"Errorf": true, "Error": true, "Fatal": true, "Fatalf": true, "Fatalln": true,
	"Panic": true, "Panicf": true, "Panicln": true,
}

// callName is the function name a call goes through, whatever shape the callee
// has: `register(…)` and `mcp.RegisterTool(…)` both name their callee.
func callName(call *ast.CallExpr) string {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// scanToolShapedLiterals finds call-argument literals shaped like a tool name
// that the catalog does not declare: the mirror of a declared-but-unimplemented
// tool. Restricted two ways, both of them measured rather than guessed:
//
//   - call arguments only, so the check does not report every dotted lowercase
//     string in the tree;
//   - files whose path names mcp, because that is the surface a tool can be
//     wired from. Scanning all of cmd/ and internal/ reported 43 "undeclared
//     tools" on this tree, every one of them someone's identifier: the doctor's
//     fixture keys ("os.kernel", "os.arch"), a backup drill's column name. A
//     list that is mostly noise is not evidence, it is a list nobody reads.
//
// The cost is stated rather than hidden: a tool wired outside a file whose path
// says mcp is not reported here. It would still be caught by the declared side
// if its name were in the catalog.
func scanToolShapedLiterals(root string, dirs, known []string) ([]ToolSite, error) {
	declared := map[string]bool{}
	for _, n := range known {
		declared[n] = true
	}
	var out []ToolSite
	for _, dir := range dirs {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
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
			if !strings.Contains(strings.ToLower(p), "mcp") {
				return nil
			}
			fset := token.NewFileSet()
			file, perr := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
			if perr != nil {
				return fmt.Errorf("parsing %s: %w", p, perr)
			}
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil {
				rel = p
			}
			spans := funcSpans(file, fset)
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				// A name written into a message is prose, however tool-shaped it
				// looks: reporting it as an implementation would invert this
				// check's whole purpose.
				if messageCallNames[callName(call)] {
					return true
				}
				for _, a := range call.Args {
					lit, ok := a.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					s, uerr := strconv.Unquote(lit.Value)
					if uerr != nil || declared[s] || !toolNameRe.MatchString(s) {
						continue
					}
					pos := fset.Position(lit.Pos())
					out = append(out, ToolSite{File: rel, Line: pos.Line, In: spanName(spans, lit.Pos()), Form: "dispatch"})
				}
				return true
			})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	return out, nil
}
