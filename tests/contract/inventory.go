package contract

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// InventoryRoute is one line of ops/contract/route-inventory.json. The
// mechanical fields are written by the tool; `suggest`, `reason` and
// `uncertain` are the Worker's judgement and are preserved across
// regeneration, because they are the part a human wrote.
type InventoryRoute struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	// File and Line are the registration point; Registration is the same fact
	// as one "file:line" string so a reader can jump without joining fields.
	File         string `json:"file"`
	Line         int    `json:"line"`
	Registration string `json:"registration"`
	In           string `json:"registered_in"`
	Handler      string `json:"handler,omitempty"`
	// Status is the comparator's verdict: in_contract | catchall | exempted |
	// undocumented.
	Status string `json:"status"`
	// Matched is the contract operation (or exemption) this route was matched
	// to, quoted so the match is auditable.
	Matched string `json:"matched,omitempty"`
	// Suggest is contract or exempt; deliberately absent while the route is
	// already documented (Status in_contract/catchall/exempted) — the field
	// exists to propose a disposition for the UNDOCUMENTED routes.
	Suggest string `json:"suggest,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// Uncertain marks a judgement the Worker could not settle from the specs;
	// it is for the Supervisor, not a hedge.
	Uncertain bool `json:"uncertain"`
}

// SuggestedExemption mirrors specs/api/openapi-exemptions.yaml's entry shape,
// field for field and name for name: the Supervisor pastes these straight into
// that file.
type SuggestedExemption struct {
	Method   string `json:"method"`
	Path     string `json:"path"`
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
	FollowUp string `json:"follow_up,omitempty"`
}

// Inventory is the machine-readable deliverable: every mounted /api/v1 route
// with its registration point, the comparator's verdict on it, and — for the
// undocumented ones — a suggested disposition with a reason.
type Inventory struct {
	Version   int                  `json:"version"`
	Task      string               `json:"task"`
	Generated string               `json:"generated_by"`
	Source    string               `json:"source"`
	Summary   Counts               `json:"summary"`
	Routes    []InventoryRoute     `json:"routes"`
	Suggested []SuggestedExemption `json:"suggested_exemptions"`
	// Below: the surrounding facts the counts rest on, so the numbers can be
	// re-derived from this file without re-running the tool.
	OutOfScope []InventoriedOutside `json:"out_of_scope"`
	Unresolved []Unresolved         `json:"unresolved_registrations"`
	Mounts     []Mount              `json:"mounts"`
	Catchall   []Resolution         `json:"catchall_resolutions"`
	Advisory   []Unresolved         `json:"advisory"`
	Findings   map[string]int       `json:"finding_counts"`
	Notes      []string             `json:"notes"`
}

// InventoriedOutside is a registration outside the contract's server prefix.
type InventoriedOutside struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	File   string `json:"file"`
	Line   int    `json:"line"`
	In     string `json:"in"`
	Note   string `json:"note"`
}

const inventoryGeneratedBy = "go run ./tests/cmd/contractgate -write-inventory ops/contract/route-inventory.json"

// BuildInventory turns one gate run into the inventory document, preserving
// the judgements already written in `previous` for routes that are still
// mounted. Regeneration must never erase a reason: the reasons are the
// deliverable.
func BuildInventory(g *Gate, previous *Inventory, source string) *Inventory {
	prior := map[string]InventoryRoute{}
	if previous != nil {
		for _, r := range previous.Routes {
			prior[r.Method+" "+r.Path] = r
		}
	}
	inv := &Inventory{
		Version: 1, Task: "T1205", Generated: inventoryGeneratedBy, Source: source,
		Summary: g.Counts, Unresolved: g.Unresolved, Mounts: g.Mounts,
		Catchall: g.Resolved, Advisory: g.Advisory, Findings: map[string]int{}, Notes: inventoryNotes(),
	}
	for _, f := range g.Findings {
		inv.Findings[f.Kind]++
	}
	for _, r := range g.Routes {
		key := r.Method + " " + r.Path
		row := InventoryRoute{
			Method: r.Method, Path: r.Path, File: r.File, Line: r.Line,
			Registration: fmt.Sprintf("%s:%d", r.File, r.Line),
			In:           r.In, Handler: r.Handler,
			Status: g.RouteState[key], Matched: g.RouteMatch[key],
		}
		if old, ok := prior[key]; ok {
			row.Suggest, row.Reason, row.Uncertain = old.Suggest, old.Reason, old.Uncertain
		}
		inv.Routes = append(inv.Routes, row)
	}
	// The suggested exemptions are a judgement too, and they are the only part
	// of this file the Supervisor pastes: regeneration must not erase one just
	// because the exemption list it was proposed FOR is still empty. Previous
	// entries are kept verbatim; entries that are now really in the exemption
	// list are added beside them, so the difference between "proposed" and
	// "already exempted" stays visible instead of collapsing.
	if previous != nil {
		inv.Suggested = append(inv.Suggested, previous.Suggested...)
	}
	for _, e := range g.Exempt.Exemptions {
		if !hasRoute(inv.Routes, e.Method, e.Path) || hasSuggestion(inv.Suggested, e.Method, e.Path) {
			continue
		}
		inv.Suggested = append(inv.Suggested, SuggestedExemption(e))
	}
	sort.SliceStable(inv.Suggested, func(i, j int) bool {
		if inv.Suggested[i].Path != inv.Suggested[j].Path {
			return inv.Suggested[i].Path < inv.Suggested[j].Path
		}
		return inv.Suggested[i].Method < inv.Suggested[j].Method
	})
	sort.Slice(inv.Routes, func(i, j int) bool {
		if inv.Routes[i].Path != inv.Routes[j].Path {
			return inv.Routes[i].Path < inv.Routes[j].Path
		}
		return inv.Routes[i].Method < inv.Routes[j].Method
	})
	for _, r := range g.Outside {
		inv.OutOfScope = append(inv.OutOfScope, InventoriedOutside{
			Method: r.Method, Path: r.Path, File: r.File, Line: r.Line, In: r.In,
			Note: "outside the contract's server prefix (" + g.Prefix + "): probed by the same scan, gated by nothing — it is not part of the API contract's surface",
		})
	}
	return inv
}

func hasSuggestion(rows []SuggestedExemption, method, path string) bool {
	for _, r := range rows {
		if r.Method == method && r.Path == path {
			return true
		}
	}
	return false
}

func hasRoute(rows []InventoryRoute, method, path string) bool {
	for _, r := range rows {
		if r.Method == method && r.Path == path {
			return true
		}
	}
	return false
}

func inventoryNotes() []string {
	return []string{
		"status is the comparator's verdict, not a judgement: in_contract | catchall | exempted | undocumented.",
		"`catchall` means the route is a Go catch-all that resolves to a contracted `:verb` operation; it is documented, and it must NOT be parked in specs/api/openapi-exemptions.yaml (that file's header forbids it).",
		"suggest is filled only for undocumented routes: `contract` means the Supervisor should add it to specs/api/openapi.yaml, `exempt` means it should go to specs/api/openapi-exemptions.yaml — in which case the entry also appears in suggested_exemptions with a citation that resolves in this tree.",
		"uncertain marks a disposition the Worker could not settle from the specs alone; it is a flag for the Supervisor, not a hedge.",
		"Nothing in this file is written into specs/** by the Worker: CLAUDE.md §8.1 makes specs/** Supervisor-only, so the contract and the exemption list are the Supervisor's pen.",
	}
}

// LoadInventory reads a previously written inventory (nil when absent).
func LoadInventory(path string) (*Inventory, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var inv Inventory
	if err := json.Unmarshal(raw, &inv); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &inv, nil
}

// WriteInventory writes the inventory as indented JSON.
func WriteInventory(path string, inv *Inventory) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(inv, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

// CheckInventory verifies the inventory against a fresh gate run: it must
// cover every mounted route exactly, the undocumented ones must carry a
// disposition with a reason, and every suggested exemption must cite text that
// exists. This is what makes "the count in the inventory equals the count the
// comparator reports" a property rather than a hope.
func CheckInventory(g *Gate, inv *Inventory, root string) []string {
	var problems []string
	if inv == nil {
		return []string{"inventory file is missing"}
	}
	seen := map[string]InventoryRoute{}
	for _, r := range inv.Routes {
		key := r.Method + " " + r.Path
		if _, dup := seen[key]; dup {
			problems = append(problems, fmt.Sprintf("inventory lists %s twice", key))
		}
		seen[key] = r
	}
	for _, r := range g.Routes {
		key := r.Method + " " + r.Path
		row, ok := seen[key]
		if !ok {
			problems = append(problems, fmt.Sprintf("mounted route %s (%s:%d) is missing from the inventory", key, r.File, r.Line))
			continue
		}
		if row.File != r.File || row.Line != r.Line {
			problems = append(problems, fmt.Sprintf("inventory registration point for %s is %s:%d, the tree says %s:%d", key, row.File, row.Line, r.File, r.Line))
		}
		if want := g.RouteState[key]; row.Status != want {
			problems = append(problems, fmt.Sprintf("inventory status for %s is %q, the comparator says %q", key, row.Status, want))
		}
		if row.Status == "undocumented" {
			switch row.Suggest {
			case "contract", "exempt":
			default:
				problems = append(problems, fmt.Sprintf("%s is undocumented and suggests %q — every undocumented route needs a disposition (contract|exempt)", key, row.Suggest))
			}
			if strings.TrimSpace(row.Reason) == "" {
				problems = append(problems, fmt.Sprintf("%s is undocumented with no reason — a disposition without a reason is the judgement being dropped", key))
			}
		}
	}
	for key := range seen {
		found := false
		for _, r := range g.Routes {
			if r.Method+" "+r.Path == key {
				found = true
				break
			}
		}
		if !found {
			problems = append(problems, fmt.Sprintf("inventory lists %s, which the tree does not mount", key))
		}
	}
	if inv.Summary.Undocumented != g.Counts.Undocumented || inv.Summary.Mounted != g.Counts.Mounted {
		problems = append(problems, fmt.Sprintf("inventory summary says %d mounted / %d undocumented, this run says %d / %d — regenerate it with %s",
			inv.Summary.Mounted, inv.Summary.Undocumented, g.Counts.Mounted, g.Counts.Undocumented, inventoryGeneratedBy))
	}
	for _, s := range inv.Suggested {
		row, ok := seen[s.Method+" "+s.Path]
		if !ok {
			problems = append(problems, fmt.Sprintf("suggested exemption %s %s names a route the inventory does not list", s.Method, s.Path))
			continue
		}
		if row.Suggest != "exempt" {
			problems = append(problems, fmt.Sprintf("suggested exemption %s %s contradicts the route's suggest=%q", s.Method, s.Path, row.Suggest))
		}
		if _, err := ResolveDecision(root, s.Decision); err != nil {
			problems = append(problems, fmt.Sprintf("suggested exemption %s %s cites %q, which does not resolve: %v", s.Method, s.Path, s.Decision, err))
		}
		if strings.TrimSpace(s.Reason) == "" {
			problems = append(problems, fmt.Sprintf("suggested exemption %s %s has no reason", s.Method, s.Path))
		}
		// The exemption file's header names the strongest anti-pattern: an
		// entry whose reason is "we did not get to it".
		if !isDecisionShaped(s.Decision) {
			problems = append(problems, fmt.Sprintf("suggested exemption %s %s cites %q, which does not look like a decision (docs/NN_*.md §N, docs/adr/*.md, or tasks/decisions.md <circled entry>)", s.Method, s.Path, s.Decision))
		}
	}
	return problems
}

func isDecisionShaped(s string) bool {
	return repoPathRe.MatchString(s)
}

// WriteMCPInventory writes the MCP reconciliation as its own machine-readable
// file: same shape as the route inventory's judgement columns, so the
// Supervisor reads one convention, not two.
func WriteMCPInventory(path string, rep *MCPReport) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}
