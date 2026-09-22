package contract

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// PathSeg is one segment of a route pattern, parsed so that two spellings of
// the same interface can be compared. Wildcard NAMES are not compared — the
// contract writes {prId} where the mux writes {number}, and comparing names
// would report a documented endpoint as undocumented for a spelling
// difference. Wildcard SUFFIXES are compared (":merge" is part of the
// interface, not a name).
type PathSeg struct {
	Literal string
	Param   bool
	Name    string
	Rest    bool // Go catch-all: {name...}
	Suffix  string
}

func (s PathSeg) String() string {
	if !s.Param {
		return s.Literal
	}
	if s.Rest {
		return "{" + s.Name + "...}" + s.Suffix
	}
	return "{" + s.Name + "}" + s.Suffix
}

// Malformed reports a segment that looks like a wildcard but is not one.
func (s PathSeg) Malformed() bool {
	return !s.Param && (strings.Contains(s.Literal, "{") || strings.Contains(s.Literal, "}") || strings.HasSuffix(s.Literal, "..."))
}

// PathSegs splits a path into segments.
func PathSegs(p string) []PathSeg {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	out := make([]PathSeg, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		out = append(out, parseSeg(part))
	}
	return out
}

func parseSeg(s string) PathSeg {
	if strings.HasPrefix(s, "{") {
		if i := strings.Index(s, "}"); i > 0 {
			name := s[1:i]
			rest := strings.HasSuffix(name, "...")
			name = strings.TrimSuffix(name, "...")
			return PathSeg{Param: true, Name: name, Rest: rest, Suffix: s[i+1:]}
		}
	}
	return PathSeg{Literal: s}
}

// compatible reports whether a mounted segment can satisfy a contract segment
// at the same position.
func compatible(m, c PathSeg) bool {
	if !c.Param {
		return !m.Param && m.Literal == c.Literal
	}
	return m.Param && !m.Rest && m.Suffix == c.Suffix
}

// matchHow describes how a mounted route was matched to a contract operation.
const (
	matchExact    = "exact"
	matchCatchall = "catchall"
)

// serves reports whether mounted route m serves contract operation c, and how.
//
// Two ways, and only two:
//
//   - exact: same segment count, every segment compatible. Parameter names are
//     free, so {prId} (contract) matches {number} (mux).
//   - catchall: m's last segment is a Go catch-all `{x...}` and the contract's
//     last segment is a parameter carrying a literal suffix — `{prId}:merge`,
//     `{searchId}:start-project`. Those patterns exist for one reason: Go's
//     ServeMux cannot express `{id}:verb`, so the handler takes the whole tail
//     and splits the suffix itself. Requiring the non-empty suffix is
//     deliberate: without it `POST …/pull-requests/{number...}` would also
//     "document" `POST …/pull-requests`, and deleting the real registration
//     for that entry would leave the gate green — a hole exactly the size of
//     the thing the gate is for.
func serves(m Route, c ContractOp) (bool, string) {
	if m.Method != c.Method && m.Method != "ANY" {
		return false, ""
	}
	ms, cs := PathSegs(m.Path), PathSegs(c.Path)
	if len(ms) != len(cs) || len(ms) == 0 {
		return false, ""
	}
	if covers(ms, cs, len(ms)) {
		return true, matchExact
	}
	last := len(ms) - 1
	if ms[last].Rest && last > 0 && covers(ms, cs, last) && cs[last].Param && cs[last].Suffix != "" {
		return true, matchCatchall + cs[last].Suffix
	}
	return false, ""
}

// covers reports whether the first n mounted segments satisfy the first n
// contract segments.
func covers(ms, cs []PathSeg, n int) bool {
	for i := 0; i < n; i++ {
		if !compatible(ms[i], cs[i]) {
			return false
		}
	}
	return true
}

// IsCatchAll reports whether a mounted path ends in a Go catch-all.
func IsCatchAll(path string) bool {
	segs := PathSegs(path)
	return len(segs) > 0 && segs[len(segs)-1].Rest
}

// Finding is one defect the gate reports. Every finding names where it is, so
// a reader can go to the line rather than re-derive the scan.
type Finding struct {
	Kind   string `json:"kind"`
	Method string `json:"method,omitempty"`
	Path   string `json:"path,omitempty"`
	File   string `json:"file,omitempty"`
	Line   int    `json:"line,omitempty"`
	Detail string `json:"detail"`
}

// Finding kinds.
const (
	// Undocumented: mounted under the server prefix, in neither the contract
	// nor the exemption list — the thing this task's acceptance is about.
	Undocumented = "undocumented"
	// Unmounted: the contract declares an operation nothing registers.
	Unmounted = "unmounted"
	// ExemptUncited: an exemption whose decision: does not resolve to real text.
	ExemptUncited = "exemption_uncited"
	// ExemptUnused: an exemption matching no mounted route (a stale entry, or
	// a path spelled the way the contract spells it instead of the mux).
	ExemptUnused = "exemption_unused"
	// ExemptionCatchall: a verb-suffix catch-all parked in the exemption list,
	// which the file's header forbids (it must resolve to a contract entry).
	ExemptionCatchall = "exemption_catchall"
	// Unresolved: a registration the enumerator could not read.
	UnresolvedKind = "registration_unresolved"
	// Malformed: a pattern that cannot be a mux pattern.
	Malformed = "pattern_malformed"
)

// Resolution records a mounted route matched to a contract operation, so the
// catch-all resolutions (the interesting ones) are visible rather than implied.
type Resolution struct {
	Method  string `json:"method"`
	Path    string `json:"path"`
	How     string `json:"how"`
	Matched string `json:"matched_contract_op"`
}

// Counts is the reconciliation basis: the Supervisor closes the gate against
// these numbers, so they are printed, not summarised away.
type Counts struct {
	ScannedFiles int `json:"scanned_files"`
	Mounted      int `json:"mounted_routes_in_gate_scope"`
	InContract   int `json:"mounted_and_in_contract"`
	InExemptions int `json:"mounted_and_exempted"`
	Undocumented int `json:"mounted_and_undocumented"`
	Catchall     int `json:"mounted_catchall_resolved_to_contract"`
	OutOfScope   int `json:"registrations_outside_gate_scope"`
	ContractOps  int `json:"contract_operations"`
	ContractPath int `json:"contract_path_entries"`
	Unmounted    int `json:"contract_operations_not_mounted"`
	Exemptions   int `json:"exemption_entries"`
	Cited        int `json:"exemption_entries_with_resolvable_citation"`
}

// Gate is one run of the comparator.
type Gate struct {
	Root       string         `json:"root"`
	Prefix     string         `json:"server_prefix"`
	Counts     Counts         `json:"counts"`
	Routes     []Route        `json:"routes"`
	Mounts     []Mount        `json:"mounts"`
	Outside    []Route        `json:"out_of_scope"`
	Resolved   []Resolution   `json:"resolved"`
	Findings   []Finding      `json:"findings"`
	Unresolved []Unresolved   `json:"unresolved_registrations"`
	Advisory   []Unresolved   `json:"advisory"`
	Contract   *Contract      `json:"-"`
	Exempt     *ExemptionList `json:"-"`
	// RouteState is per-mounted-route disposition, the join key for the
	// inventory: in_contract | catchall | exempted | undocumented.
	RouteState map[string]string `json:"-"`
	// RouteMatch is the contract operation each documented route matched.
	RouteMatch map[string]string `json:"-"`
}

// Options selects the inputs. The defaults are the repository's canonical
// paths, so the command has no flags to get wrong.
type Options struct {
	Root           string
	Dirs           []string
	ContractPath   string
	ExemptionPath  string
	SkipExemptions bool
}

func (o *Options) withDefaults() {
	if o.Root == "" {
		o.Root = "."
	}
	if len(o.Dirs) == 0 {
		o.Dirs = []string{"cmd", "internal"}
	}
	if o.ContractPath == "" {
		o.ContractPath = filepath.Join("specs", "api", "openapi.yaml")
	}
	if o.ExemptionPath == "" {
		o.ExemptionPath = filepath.Join("specs", "api", "openapi-exemptions.yaml")
	}
}

// Run performs one comparison. An error means the instrument could not read
// its inputs (exit 1 territory); findings are data, not errors — a gate that
// conflates "I could not measure" with "the measurement is bad" cannot be
// trusted to say either.
func Run(opts Options) (*Gate, error) {
	opts.withDefaults()
	enum, err := Enumerate(opts.Root, opts.Dirs...)
	if err != nil {
		return nil, err
	}
	contractPath := filepath.Join(opts.Root, opts.ContractPath)
	con, err := ReadContract(contractPath)
	if err != nil {
		return nil, err
	}
	var exempt *ExemptionList
	if !opts.SkipExemptions {
		exempt, err = ReadExemptions(filepath.Join(opts.Root, opts.ExemptionPath))
		if err != nil {
			return nil, err
		}
	} else {
		exempt = &ExemptionList{Path: "(not read: -skip-exemptions)"}
	}

	g := &Gate{
		Root: opts.Root, Prefix: con.ServerPrefix, Contract: con, Exempt: exempt,
		Mounts: enum.Mounts, Unresolved: enum.Unresolved, Advisory: enum.Advisory,
		RouteState: map[string]string{}, RouteMatch: map[string]string{},
	}
	g.Counts.ScannedFiles = enum.FilesScanned
	g.Counts.ContractOps = len(con.Ops)
	g.Counts.ContractPath = con.Paths

	inGate := func(p string) bool {
		return p == con.ServerPrefix || strings.HasPrefix(p, con.ServerPrefix+"/")
	}
	for _, r := range enum.Routes {
		if !inGate(r.Path) {
			g.Outside = append(g.Outside, r)
			continue
		}
		g.Routes = append(g.Routes, r)
	}
	g.Counts.Mounted = len(g.Routes)
	g.Counts.OutOfScope = len(g.Outside)

	for _, u := range enum.Unresolved {
		g.Findings = append(g.Findings, Finding{
			Kind: UnresolvedKind, File: u.File, Line: u.Line,
			Detail: fmt.Sprintf("%s (%s)", u.Why, u.Expr),
		})
	}

	key := func(method, path string) string { return method + " " + path }

	// Direction 1: every mounted route must be contracted or exempted.
	exemptByKey := map[string]Exemption{}
	for _, e := range exempt.Exemptions {
		exemptByKey[key(e.Method, e.Path)] = e
	}
	for _, r := range g.Routes {
		for _, seg := range PathSegs(r.Path) {
			if seg.Malformed() {
				g.Findings = append(g.Findings, Finding{
					Kind: Malformed, Method: r.Method, Path: r.Path, File: r.File, Line: r.Line,
					Detail: fmt.Sprintf("segment %q is neither a literal nor a well-formed wildcard", seg.Literal),
				})
			}
		}
		matched := false
		for _, op := range con.Ops {
			ok, how := serves(r, op)
			if !ok {
				continue
			}
			matched = true
			if how != matchExact {
				g.Counts.Catchall++
				g.Resolved = append(g.Resolved, Resolution{
					Method: r.Method, Path: r.Path, How: how,
					Matched: op.Method + " " + op.Path,
				})
				g.RouteState[key(r.Method, r.Path)] = "catchall"
			} else {
				g.Counts.InContract++
				g.RouteState[key(r.Method, r.Path)] = "in_contract"
			}
			if _, seen := g.RouteMatch[key(r.Method, r.Path)]; !seen {
				g.RouteMatch[key(r.Method, r.Path)] = op.Method + " " + op.Path
			}
			break
		}
		if matched {
			continue
		}
		if e, ok := exemptByKey[key(r.Method, r.Path)]; ok {
			ref, rerr := ResolveDecision(opts.Root, e.Decision)
			if rerr == nil {
				g.Counts.InExemptions++
				g.RouteState[key(r.Method, r.Path)] = "exempted"
				g.RouteMatch[key(r.Method, r.Path)] = "exemption: " + ref.Path + " " + ref.Locator
				continue
			}
			// An entry whose citation does not resolve does not document
			// anything: the route stays undocumented and is counted as such, so
			// a dead citation cannot quietly remove a route from the arithmetic
			// (mounted = in contract + exempted + undocumented). The entry is
			// reported a second time below, from the exemption file's side,
			// because those are two defects on two different lines.
			g.Counts.Undocumented++
			g.RouteState[key(r.Method, r.Path)] = "undocumented"
			g.Findings = append(g.Findings, Finding{
				Kind: Undocumented, Method: r.Method, Path: r.Path, File: r.File, Line: r.Line,
				Detail: fmt.Sprintf("%s lists an exemption for it, but that exemption's decision does not resolve (%v) — an exemption that rests on nothing leaves the endpoint where it was", exempt.Path, rerr),
			})
			continue
		}
		g.Counts.Undocumented++
		g.RouteState[key(r.Method, r.Path)] = "undocumented"
		detail := "registered here and in neither " + filepath.Join(opts.ContractPath) + " nor " + filepath.Join(opts.ExemptionPath)
		if IsCatchAll(r.Path) {
			detail = "verb-suffix catch-all that resolves to NO contract entry: it must name a contracted `:verb` operation, and parking it in the exemption list is forbidden by that file's header"
		}
		g.Findings = append(g.Findings, Finding{
			Kind: Undocumented, Method: r.Method, Path: r.Path, File: r.File, Line: r.Line,
			Detail: detail,
		})
	}

	// Exemption entries: cited, used, and not catch-alls.
	for _, e := range exempt.Exemptions {
		g.Counts.Exemptions++
		if _, err := ResolveDecision(opts.Root, e.Decision); err != nil {
			g.Findings = append(g.Findings, Finding{
				Kind: ExemptUncited, Method: e.Method, Path: e.Path,
				Detail: fmt.Sprintf("%s lists it but its decision citation does not resolve: %v", exempt.Path, err),
			})
		} else {
			g.Counts.Cited++
		}
		if e.IsCatchAll() {
			g.Findings = append(g.Findings, Finding{
				Kind: ExemptionCatchall, Method: e.Method, Path: e.Path,
				Detail: "a `{x...}` catch-all belongs in the contract resolution, not the exemption list: parking it here hides a real endpoint behind a shape the contract cannot express",
			})
		}
	}
	mountedKeys := map[string]bool{}
	for _, r := range g.Routes {
		mountedKeys[key(r.Method, r.Path)] = true
	}
	for _, e := range exempt.Exemptions {
		if !mountedKeys[key(e.Method, e.Path)] {
			g.Findings = append(g.Findings, Finding{
				Kind: ExemptUnused, Method: e.Method, Path: e.Path,
				Detail: fmt.Sprintf("%s exempts a route this tree does not mount under %s — either the path is spelled the way the contract spells it instead of the way the mux does, or the route is gone and the entry is stale", exempt.Path, con.ServerPrefix),
			})
		}
	}

	// Direction 2: every contracted operation must be mounted.
	for _, op := range con.Ops {
		ok := false
		for _, r := range g.Routes {
			if served, _ := serves(r, op); served {
				ok = true
				break
			}
		}
		if !ok {
			g.Counts.Unmounted++
			g.Findings = append(g.Findings, Finding{
				Kind: Unmounted, Method: op.Method, Path: op.Path,
				Detail: fmt.Sprintf("declared in %s as %s %s and no route in cmd/** or internal/** registers it", opts.ContractPath, op.Method, op.Relative),
			})
		}
	}

	sort.SliceStable(g.Resolved, func(i, j int) bool {
		if g.Resolved[i].Path != g.Resolved[j].Path {
			return g.Resolved[i].Path < g.Resolved[j].Path
		}
		return g.Resolved[i].Method < g.Resolved[j].Method
	})
	sort.SliceStable(g.Findings, func(i, j int) bool {
		if g.Findings[i].Kind != g.Findings[j].Kind {
			return g.Findings[i].Kind < g.Findings[j].Kind
		}
		if g.Findings[i].Path != g.Findings[j].Path {
			return g.Findings[i].Path < g.Findings[j].Path
		}
		return g.Findings[i].Method < g.Findings[j].Method
	})
	return g, nil
}

// OK reports whether the gate found nothing to report.
func (g *Gate) OK() bool { return len(g.Findings) == 0 }

// FindingsOfKind filters by kind.
func (g *Gate) FindingsOfKind(kind string) []Finding {
	var out []Finding
	for _, f := range g.Findings {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}
