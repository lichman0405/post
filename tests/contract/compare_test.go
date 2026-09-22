package contract

import (
	"path/filepath"
	"strings"
	"testing"
)

// caseRun runs the comparator over one synthetic case directory: root
// testdata/cases/<dir>, scanning its `tree/` and reading the named contract and
// exemption fixtures. Every path here is a fixture; nothing in this package
// points at cmd/, internal/ or specs/.
type caseRun struct {
	t        *testing.T
	dir      string
	contract string
	exempt   string
}

func (c caseRun) gate() *Gate {
	c.t.Helper()
	opts := Options{
		Root:          filepath.Join("testdata", "cases", c.dir),
		Dirs:          []string{"tree"},
		ContractPath:  c.contract,
		ExemptionPath: c.exempt,
	}
	if c.exempt == "" {
		opts.SkipExemptions = true
	}
	g, err := Run(opts)
	if err != nil {
		c.t.Fatalf("Run(%s, %s): %v", c.dir, c.contract, err)
	}
	return g
}

func findingLines(g *Gate) []string {
	out := make([]string, 0, len(g.Findings))
	for _, f := range g.Findings {
		out = append(out, f.Kind+" "+f.Method+" "+f.Path)
	}
	return out
}

// TestComparatorRefusesToGuessTheContractOrTheTree is the two-direction table
// the task asks for, in one place: a mounted-but-unregistered endpoint and a
// registered-but-unmounted path are both defects, and each fixture is the other
// fixture with one line changed.
func TestComparatorJudgesBothDirections(t *testing.T) {
	cases := []struct {
		name             string
		run              caseRun
		mounted          int
		undocumented     int
		unmounted        int
		wantFindingKinds []string
		wantFiles        []string
	}{
		{
			// POST /api/v1/beta is registered in the tree and absent from the
			// contract: the endpoint the task exists to find.
			name:             "mounted but not in the contract",
			run:              caseRun{t: t, dir: "undocumented", contract: "contract-half.yaml"},
			mounted:          2,
			undocumented:     1,
			unmounted:        0,
			wantFindingKinds: []string{"undocumented POST /api/v1/beta"},
			wantFiles:        []string{"tree/api/main.go"},
		},
		{
			// Same tree, same contract with the line added: quiet.
			name:             "the same tree once the contract names it",
			run:              caseRun{t: t, dir: "undocumented", contract: "contract-complete.yaml"},
			mounted:          2,
			undocumented:     0,
			unmounted:        0,
			wantFindingKinds: nil,
		},
		{
			// GET /api/v1/gamma is declared and nothing mounts it.
			name:             "in the contract but not mounted",
			run:              caseRun{t: t, dir: "unmounted", contract: "contract-extra.yaml"},
			mounted:          2,
			undocumented:     0,
			unmounted:        1,
			wantFindingKinds: []string{"unmounted GET /api/v1/gamma"},
		},
		{
			name:             "the same tree once the stale entry is gone",
			run:              caseRun{t: t, dir: "unmounted", contract: "contract-complete.yaml"},
			mounted:          2,
			undocumented:     0,
			unmounted:        0,
			wantFindingKinds: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := c.run.gate()
			if g.Counts.Mounted != c.mounted {
				t.Errorf("mounted = %d, want %d", g.Counts.Mounted, c.mounted)
			}
			if g.Counts.Undocumented != c.undocumented {
				t.Errorf("undocumented = %d, want %d", g.Counts.Undocumented, c.undocumented)
			}
			if g.Counts.Unmounted != c.unmounted {
				t.Errorf("unmounted = %d, want %d", g.Counts.Unmounted, c.unmounted)
			}
			got := findingLines(g)
			if strings.Join(got, "\n") != strings.Join(c.wantFindingKinds, "\n") {
				t.Errorf("findings\n got: %q\nwant: %q", got, c.wantFindingKinds)
			}
			if g.OK() != (len(c.wantFindingKinds) == 0) {
				t.Errorf("OK() = %v with findings %q", g.OK(), got)
			}
			for _, want := range c.wantFiles {
				if len(g.Findings) > 0 && g.Findings[0].File != want {
					t.Errorf("finding points at %s, want %s", g.Findings[0].File, want)
				}
			}
		})
	}
}

// TestComparatorResolvesVerbSuffixCatchAlls covers the shape Go's ServeMux
// forces on `{id}:verb`: the handler takes the whole tail and splits the suffix
// itself. The catch-all is the document, so it must resolve to the contract
// entry — and it must resolve to THAT entry, not to any entry that happens to
// share a prefix.
func TestComparatorResolvesVerbSuffixCatchAlls(t *testing.T) {
	g := caseRun{t: t, dir: "catchall", contract: "contract.yaml"}.gate()
	if !g.OK() {
		t.Fatalf("a resolved catch-all must leave the gate quiet, got %q", findingLines(g))
	}
	if g.Counts.InContract != 1 || g.Counts.Catchall != 1 {
		t.Errorf("in_contract = %d, catchall = %d, want 1 and 1", g.Counts.InContract, g.Counts.Catchall)
	}
	if len(g.Resolved) != 1 {
		t.Fatalf("got %d resolutions, want 1: %+v", len(g.Resolved), g.Resolved)
	}
	r := g.Resolved[0]
	if r.How != "catchall:start-project" || r.Matched != "POST /api/v1/search/{searchId}:start-project" {
		t.Errorf("resolution = %+v, want the :start-project contract entry", r)
	}
	if got := g.RouteState["POST /api/v1/search/{rest...}"]; got != "catchall" {
		t.Errorf("route state = %q, want %q", got, "catchall")
	}
}

// TestComparatorReportsAnUnresolvedCatchAll: the same catch-all against a
// contract that declares no verb suffix for it. It documents nothing, and the
// finding says why rather than filing it as a plain undocumented route.
func TestComparatorReportsAnUnresolvedCatchAll(t *testing.T) {
	g := caseRun{t: t, dir: "catchall", contract: "contract-noverb.yaml"}.gate()
	if g.Counts.Undocumented != 1 || g.Counts.Catchall != 0 {
		t.Fatalf("undocumented = %d, catchall = %d, want 1 and 0", g.Counts.Undocumented, g.Counts.Catchall)
	}
	f := g.FindingsOfKind(Undocumented)
	if len(f) != 1 {
		t.Fatalf("got %d undocumented findings, want 1", len(f))
	}
	if !strings.Contains(f[0].Detail, "catch-all") || !strings.Contains(f[0].Detail, "exemption list is forbidden") {
		t.Errorf("detail does not explain the rule: %q", f[0].Detail)
	}
}

// TestComparatorDoesNotLetAWildcardHideAMissingRegistration is the hole this
// rule exists to close: `…/pull-requests/{number...}` accepts
// `…/pull-requests/7/merge`, and a comparator that let it stand in for the
// plain collection route would stay green while the contract's
// `POST …/pull-requests` entry had no registration at all.
//
// The two fixtures are the same tree shape; the fixed one adds the real
// registration, and only that fixture is quiet.
func TestComparatorDoesNotLetAWildcardHideAMissingRegistration(t *testing.T) {
	hole := caseRun{t: t, dir: "catchall-hole", contract: "contract.yaml"}.gate()
	if hole.Counts.Unmounted != 1 {
		t.Errorf("the collection route is not mounted and must be reported: unmounted = %d", hole.Counts.Unmounted)
	}
	if hole.Counts.Undocumented != 1 {
		t.Errorf("the catch-all resolves to no contract entry and must be reported: undocumented = %d", hole.Counts.Undocumented)
	}
	if hole.OK() {
		t.Error("gate is green over a tree whose contract names an endpoint nothing registers")
	}
	var unmounted *Finding
	for i := range hole.Findings {
		if hole.Findings[i].Kind == Unmounted {
			unmounted = &hole.Findings[i]
		}
	}
	if unmounted == nil || unmounted.Method+" "+unmounted.Path != "POST /api/v1/projects/{projectId}/pull-requests" {
		t.Errorf("the unmounted finding must name the collection route, got %+v", unmounted)
	}

	// The same tree against a contract that spells the last segment as a bare
	// parameter with no verb suffix. The catch-all does accept such a request
	// path, and that is precisely why it must not be read as this entry's
	// registration: the entry names a different endpoint, and the catch-all
	// answers it as a coincidence of shape.
	bare := caseRun{t: t, dir: "catchall-hole", contract: "contract-param.yaml"}.gate()
	if bare.Counts.Unmounted != 1 || bare.Counts.Catchall != 0 {
		t.Errorf("unmounted = %d, catchall = %d; want 1 and 0 — a wildcard is not a registration for a suffix-less parameter",
			bare.Counts.Unmounted, bare.Counts.Catchall)
	}
	if len(bare.FindingsOfKind(Unmounted)) != 1 || bare.FindingsOfKind(Unmounted)[0].Path != "/api/v1/projects/{projectId}/pull-requests/{number}" {
		t.Errorf("the unmounted finding must name the parameter entry: %q", findingLines(bare))
	}

	fixed := caseRun{t: t, dir: "catchall-hole-fixed", contract: "contract.yaml"}.gate()
	if !fixed.OK() {
		t.Errorf("with the registration present the gate must be quiet, got %q", findingLines(fixed))
	}
	if fixed.Counts.InContract != 1 || fixed.Counts.Catchall != 1 {
		t.Errorf("in_contract = %d, catchall = %d, want 1 and 1", fixed.Counts.InContract, fixed.Counts.Catchall)
	}
}

func TestComparatorReportsAMalformedPattern(t *testing.T) {
	g := caseRun{t: t, dir: "malformed", contract: "contract.yaml"}.gate()
	f := g.FindingsOfKind(Malformed)
	if len(f) != 1 {
		t.Fatalf("got %d malformed findings, want 1: %q", len(f), findingLines(g))
	}
	if !strings.Contains(f[0].Detail, `"{thingId"`) {
		t.Errorf("detail must quote the segment it could not read: %q", f[0].Detail)
	}
}

func TestComparatorIgnoresRegistrationsOutsideTheServerPrefix(t *testing.T) {
	// The forms tree registers GET /healthz beside its /api/v1 routes: the scan
	// reports it, the gate does not judge it. The contract fixture comes from
	// the undocumented case, so nothing here needs a real-tree contract.
	g, err := Run(Options{
		Root: formsTree, Dirs: []string{"api"},
		ContractPath:   filepath.Join("..", "cases", "undocumented", "contract-complete.yaml"),
		SkipExemptions: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	healthz := false
	for _, r := range g.Outside {
		if r.Method+" "+r.Path == "GET /healthz" {
			healthz = true
		}
	}
	if !healthz {
		t.Errorf("GET /healthz is not in the out-of-scope list: %+v", g.Outside)
	}
	if g.Counts.OutOfScope != len(g.Outside) || g.Counts.OutOfScope == 0 {
		t.Errorf("out-of-scope count = %d over %d routes", g.Counts.OutOfScope, len(g.Outside))
	}
	for _, r := range g.Routes {
		if !strings.HasPrefix(r.Path, g.Prefix) {
			t.Errorf("%s is outside the server prefix %s but was gated", r.Path, g.Prefix)
		}
	}
}

// TestComparatorCarriesAdvisoryThroughToGate verifies that the advisory list
// (the enumerator's completeness signal) is not dropped between Enumerate and
// Run: a reader that cannot see it would silently hide registration-shaped
// literals it chose not to classify.
func TestComparatorCarriesAdvisoryThroughToGate(t *testing.T) {
	g, err := Run(Options{
		Root: formsTree, Dirs: []string{"api"},
		ContractPath:   filepath.Join("..", "cases", "undocumented", "contract-complete.yaml"),
		SkipExemptions: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The forms fixture deliberately leaves two /api/v1 literals unclassified:
	// one built at runtime (dynamic/dyn.go) and one on an unknown receiver
	// (unknownform/un.go). Both must survive the trip into the Gate.
	if len(g.Advisory) == 0 {
		t.Fatalf("Gate.Advisory is empty; the advisory list from the enumerator was dropped")
	}
	exprs := make([]string, 0, len(g.Advisory))
	for _, a := range g.Advisory {
		exprs = append(exprs, a.Expr)
	}
	if !strings.Contains(strings.Join(exprs, "\n"), "/api/v1/unknown-router/stream") {
		t.Errorf("advisory list does not contain the unknown-receiver literal: %q", exprs)
	}
}

// TestPathSegsComparesNamesButNotWildcardNames pins the two decisions the
// comparison rests on: `{prId}` and `{number}` are the same interface, while
// `:merge` is not an optional decoration.
func TestPathSegsComparesNamesButNotWildcardNames(t *testing.T) {
	cases := []struct {
		mounted, contract string
		want              bool
	}{
		{"/api/v1/projects/{number}", "/api/v1/projects/{prId}", true},
		{"/api/v1/projects/{number}", "/api/v1/projects", false},
		{"/api/v1/x/{a}/y", "/api/v1/x/{b}/y", true},
		{"/api/v1/x/{a}/y", "/api/v1/x/{b}/z", false},
	}
	for _, c := range cases {
		got, _ := serves(
			Route{Method: "GET", Path: c.mounted},
			ContractOp{Method: "GET", Path: c.contract},
		)
		if got != c.want {
			t.Errorf("serves(%s, %s) = %v, want %v", c.mounted, c.contract, got, c.want)
		}
	}
}

func TestServesRejectsACatchAllWithoutAVerbSuffix(t *testing.T) {
	mounted := Route{Method: "POST", Path: "/api/v1/projects/{projectId}/pull-requests/{number...}"}
	cases := []struct {
		contract string
		want     bool
	}{
		{"/api/v1/projects/{projectId}/pull-requests", false},
		{"/api/v1/projects/{projectId}/pull-requests/{number}:merge", true},
		{"/api/v1/projects/{projectId}/pull-requests/{number}", false},
		{"/api/v1/projects/{projectId}/pull-requests/{number}:merge/{x}", false},
	}
	for _, c := range cases {
		got, _ := serves(mounted, ContractOp{Method: "POST", Path: c.contract})
		if got != c.want {
			t.Errorf("serves(catch-all, %s) = %v, want %v", c.contract, got, c.want)
		}
	}
}
