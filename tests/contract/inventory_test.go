package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// builtInventory is the inventory the real command would write for the
// synthetic exemptions case: one mounted route the contract does not declare,
// and a root that really contains the docs the suggested citations name.
func builtInventory(t *testing.T) (*Gate, *Inventory) {
	t.Helper()
	g := caseRun{t: t, dir: "exemptions", contract: "contract-empty.yaml"}.gate()
	inv := BuildInventory(g, nil, "test")
	if len(inv.Routes) != 1 {
		t.Fatalf("fixture should have exactly one mounted route, got %+v", inv.Routes)
	}
	return g, inv
}

func invRoot() string { return filepath.Join("testdata", "cases", "exemptions") }

func check(t *testing.T, g *Gate, inv *Inventory) string {
	t.Helper()
	return strings.Join(CheckInventory(g, inv, invRoot()), "\n")
}

func TestInventoryCoversTheRoutesAndTheirRegistrationPoints(t *testing.T) {
	g, inv := builtInventory(t)
	r := inv.Routes[0]
	if r.Method != "GET" || r.Path != "/api/v1/alpha" {
		t.Fatalf("route row = %+v", r)
	}
	if r.Status != "undocumented" {
		t.Errorf("status = %q, want undocumented (the contract is empty)", r.Status)
	}
	if r.Registration != r.File+":"+strconv.Itoa(r.Line) || !strings.HasSuffix(r.File, "tree/api/main.go") {
		t.Errorf("registration point = %q in %q", r.Registration, r.File)
	}
	if r.In != "main" {
		t.Errorf("registered_in = %q, want main", r.In)
	}
	if inv.Summary.Undocumented != g.Counts.Undocumented || inv.Summary.Mounted != 1 {
		t.Errorf("summary = %+v, gate says %+v", inv.Summary, g.Counts)
	}
	if inv.Findings[Undocumented] != 1 {
		t.Errorf("finding counts = %+v", inv.Findings)
	}
	if len(inv.Notes) == 0 {
		t.Error("the inventory must carry its conventions: status is a verdict, suggest is a judgement")
	}
}

// clone deep-copies the parts of an inventory a test tampers with, so one
// assertion cannot leak into the next through a shared slice.
func clone(inv *Inventory) *Inventory {
	out := *inv
	out.Routes = append([]InventoryRoute(nil), inv.Routes...)
	out.Suggested = append([]SuggestedExemption(nil), inv.Suggested...)
	return &out
}

// TestInventoryRefusesAPlaceholderForAJudgement: the whole point of the file is
// the disposition column. An undocumented route with no disposition, or with a
// disposition and no reason, is the judgement dropped on the floor.
func TestInventoryRefusesAPlaceholderForAJudgement(t *testing.T) {
	g, inv := builtInventory(t)
	if problems := check(t, g, inv); !strings.Contains(problems, "needs a disposition") {
		t.Errorf("a fresh build must be reported as undecided, got:\n%s", problems)
	}
	inv.Routes[0].Suggest = "contract"
	if problems := check(t, g, inv); !strings.Contains(problems, "no reason") {
		t.Errorf("a disposition with no reason must be reported, got:\n%s", problems)
	}
	inv.Routes[0].Reason = "the endpoint exists, the contract does not describe it"
	if problems := check(t, g, inv); problems != "" {
		t.Errorf("a complete row must pass, got:\n%s", problems)
	}
}

func TestInventoryCheckNoticesTheTreeMovingUnderIt(t *testing.T) {
	g, inv := builtInventory(t)

	moved := clone(inv)
	moved.Routes[0].Suggest, moved.Routes[0].Reason = "contract", "same reason"
	moved.Routes[0].Line += 1
	if problems := check(t, g, moved); !strings.Contains(problems, "registration point") {
		t.Errorf("a moved registration point must be reported, got:\n%s", problems)
	}

	status := clone(inv)
	status.Routes[0].Suggest, status.Routes[0].Reason = "contract", "same reason"
	status.Routes[0].Status = "in_contract"
	if problems := check(t, g, status); !strings.Contains(problems, "the comparator says") {
		t.Errorf("a status that contradicts the run must be reported, got:\n%s", problems)
	}

	dropped := clone(inv)
	dropped.Routes = nil
	if problems := check(t, g, dropped); !strings.Contains(problems, "is missing from the inventory") {
		t.Errorf("a dropped route must be reported, got:\n%s", problems)
	}

	phantom := clone(inv)
	phantom.Routes = append([]InventoryRoute{{Method: "GET", Path: "/api/v1/ghost", Status: "undocumented", Suggest: "contract", Reason: "x"}}, inv.Routes...)
	if problems := check(t, g, phantom); !strings.Contains(problems, "does not mount") {
		t.Errorf("a route the tree does not mount must be reported, got:\n%s", problems)
	}

	dup := clone(inv)
	dup.Routes = append([]InventoryRoute{inv.Routes[0]}, inv.Routes...)
	if problems := check(t, g, dup); !strings.Contains(problems, "twice") {
		t.Errorf("a duplicated row must be reported, got:\n%s", problems)
	}

	stale := clone(inv)
	stale.Summary.Undocumented = 7
	if problems := check(t, g, stale); !strings.Contains(problems, "regenerate it") {
		t.Errorf("a stale summary must be reported, got:\n%s", problems)
	}
}

// TestInventorySuggestedExemptionIsPasteable: the suggested entry exists to be
// pasted into specs/api/openapi-exemptions.yaml, so it has to agree with the
// route's own disposition and cite text that is really in the tree.
func TestInventorySuggestedExemptionIsPasteable(t *testing.T) {
	g, inv := builtInventory(t)
	inv.Routes[0].Suggest = "exempt"
	inv.Routes[0].Reason = "internal-only route"

	// The field names are the exemption file's, character for character: a
	// rename here would make the paste a manual edit.
	raw, err := json.Marshal(SuggestedExemption{})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	// method/path/decision/reason are always present, empty or not: the paste
	// is a copy, so the keys have to be there to copy.
	for _, want := range []string{"method", "path", "decision", "reason"} {
		if _, ok := fields[want]; !ok {
			t.Errorf("suggested exemption has no %q field: %s", want, raw)
		}
	}
	if withFollowUp, err := json.Marshal(SuggestedExemption{FollowUp: "T1206"}); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(string(withFollowUp), `"follow_up":"T1206"`) {
		t.Errorf("follow_up must keep the exemption file's field name: %s", withFollowUp)
	}

	inv.Suggested = []SuggestedExemption{{
		Method: "GET", Path: "/api/v1/alpha",
		Decision: "docs/22_API_DESIGN.md §3", Reason: "internal operator route",
	}}
	if problems := check(t, g, inv); problems != "" {
		t.Errorf("a resolvable suggestion must pass, got:\n%s", problems)
	}

	bad := clone(inv)
	bad.Suggested = []SuggestedExemption{{
		Method: "GET", Path: "/api/v1/alpha",
		Decision: "we did not get to it", Reason: "internal operator route",
	}}
	problems := check(t, g, bad)
	if !strings.Contains(problems, "does not resolve") || !strings.Contains(problems, "does not look like a decision") {
		t.Errorf("a citation that names no decision must be reported twice over, got:\n%s", problems)
	}

	phantom := clone(inv)
	phantom.Suggested = []SuggestedExemption{{
		Method: "GET", Path: "/api/v1/alpha",
		Decision: "docs/22_API_DESIGN.md §99", Reason: "internal operator route",
	}}
	if problems := check(t, g, phantom); !strings.Contains(problems, "does not resolve") {
		t.Errorf("a section that is not in the file must be reported, got:\n%s", problems)
	}

	contradiction := clone(inv)
	contradiction.Routes[0].Suggest = "contract"
	if problems := check(t, g, contradiction); !strings.Contains(problems, "contradicts") {
		t.Errorf("a suggested exemption for a route marked `contract` must be reported, got:\n%s", problems)
	}

	reasonless := clone(inv)
	reasonless.Suggested = []SuggestedExemption{{
		Method: "GET", Path: "/api/v1/alpha",
		Decision: "docs/22_API_DESIGN.md §3",
	}}
	if problems := check(t, g, reasonless); !strings.Contains(problems, "has no reason") {
		t.Errorf("a reasonless suggestion must be reported, got:\n%s", problems)
	}

	unknown := clone(inv)
	unknown.Suggested = []SuggestedExemption{{
		Method: "GET", Path: "/api/v1/ghost",
		Decision: "docs/22_API_DESIGN.md §3", Reason: "x",
	}}
	if problems := check(t, g, unknown); !strings.Contains(problems, "does not list") {
		t.Errorf("a suggestion for an unlisted route must be reported, got:\n%s", problems)
	}
}

// TestInventoryRegenerationKeepsTheJudgements: `suggest`, `reason` and
// `uncertain` are the part a human wrote. Regenerating the mechanical columns
// must not erase them, which is the difference between a reviewable document
// and a table that has to be rewritten every time the tree moves.
func TestInventoryRegenerationKeepsTheJudgements(t *testing.T) {
	g, inv := builtInventory(t)
	inv.Routes[0].Suggest = "exempt"
	inv.Routes[0].Reason = "decision recorded elsewhere"
	inv.Routes[0].Uncertain = true

	again := BuildInventory(g, inv, "test again")
	if again.Routes[0].Suggest != "exempt" || again.Routes[0].Reason != "decision recorded elsewhere" || !again.Routes[0].Uncertain {
		t.Errorf("regeneration dropped the judgement: %+v", again.Routes[0])
	}
	if again.Routes[0].Registration != inv.Routes[0].Registration {
		t.Errorf("regeneration changed the registration point: %s then %s", inv.Routes[0].Registration, again.Routes[0].Registration)
	}
}

// TestInventoryRegenerationKeepsASuggestedExemption: the suggestions are the
// only part of the file the Supervisor pastes, and they are written WHILE the
// exemption list is still empty — which is exactly the state in which a
// regeneration that rebuilds them from that empty list erases them.
func TestInventoryRegenerationKeepsASuggestedExemption(t *testing.T) {
	g, inv := builtInventory(t)
	inv.Routes[0].Suggest = "exempt"
	inv.Routes[0].Reason = "internal-only route"
	inv.Suggested = []SuggestedExemption{{
		Method: "GET", Path: "/api/v1/alpha",
		Decision: "docs/22_API_DESIGN.md §3", Reason: "internal operator route",
	}}
	if problems := check(t, g, inv); problems != "" {
		t.Fatalf("the suggestion must pass before regenerating, got:\n%s", problems)
	}
	again := BuildInventory(g, inv, "test again")
	if len(again.Suggested) != 1 || again.Suggested[0] != inv.Suggested[0] {
		t.Errorf("regeneration erased or altered the suggested exemption: %+v", again.Suggested)
	}
	if problems := check(t, g, again); problems != "" {
		t.Errorf("the regenerated document must still pass, got:\n%s", problems)
	}
}

func TestInventoryRoundTripsThroughDisk(t *testing.T) {
	_, inv := builtInventory(t)
	path := filepath.Join(t.TempDir(), "ops", "contract", "route-inventory.json")
	if err := WriteInventory(path, inv); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(raw), "}\n") || !strings.Contains(string(raw), "\n  \"version\"") {
		t.Errorf("the file must be indented JSON with a trailing newline:\n%s", string(raw)[:len(string(raw))/4])
	}
	back, err := LoadInventory(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.Task != inv.Task || back.Routes[0].Path != inv.Routes[0].Path || back.Summary != inv.Summary {
		t.Errorf("round trip changed the document: %+v", back)
	}
	absent, err := LoadInventory(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || absent != nil {
		t.Errorf("a missing inventory must read as (nil, nil), got (%v, %v)", absent, err)
	}
	if problems := CheckInventory(nil, nil, invRoot()); len(problems) != 1 || !strings.Contains(problems[0], "missing") {
		t.Errorf("checking a missing inventory must say so, got %v", problems)
	}
}
