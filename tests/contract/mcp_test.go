package contract

import (
	"path/filepath"
	"strings"
	"testing"
)

func reconcileFixture(t *testing.T) *MCPReport {
	t.Helper()
	rep, err := ReconcileMCP(MCPOptions{
		Root:        filepath.Join("testdata", "cases", "mcp"),
		Dirs:        []string{"tree"},
		CatalogPath: "catalog.json",
		DocPath:     filepath.Join("docs", "47_MCP_TOOL_CATALOG.md"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// TestReconcileMCPTellsAWiringFromAMention is the reason the reconciliation is
// worth running at all: a tool named in a string is not a tool anything can
// call, and the report has to make that difference visible rather than counting
// both as "present".
func TestReconcileMCPTellsAWiringFromAMention(t *testing.T) {
	rep := reconcileFixture(t)
	if rep.Declared != 3 || rep.Implemented != 1 || rep.Missing != 2 {
		t.Errorf("declared = %d, implemented = %d, missing = %d; want 3, 1, 2",
			rep.Declared, rep.Implemented, rep.Missing)
	}
	byName := map[string]MCPToolRow{}
	for _, r := range rep.Tools {
		byName[r.Name] = r
	}
	one, ok := byName["alpha.one"]
	if !ok {
		t.Fatalf("alpha.one is missing from the report: %+v", rep.Tools)
	}
	if !one.Dispatched || one.Sites[0].Form != "dispatch" || one.Sites[0].In != "main" {
		t.Errorf("alpha.one = %+v, want one dispatch site in main", one)
	}
	two := byName["alpha.two"]
	if two.Dispatched {
		t.Errorf("alpha.two is only named in a literal and must not read as dispatched: %+v", two)
	}
	if len(two.Sites) != 1 || two.Sites[0].Form != "message" {
		t.Errorf("alpha.two is printed into a message and must be recorded as a mention: %+v", two.Sites)
	}
	if !two.MentionedInCatalogDoc {
		t.Error("alpha.two is named in the prose catalogue and that column must say so")
	}
	if one.Conclusion != "implemented" || !strings.Contains(one.Reason, "dispatch site") {
		t.Errorf("alpha.one's conclusion = %q / %q", one.Conclusion, one.Reason)
	}
	if two.Conclusion != "not_implemented" || !strings.Contains(two.Reason, "NAMES") {
		t.Errorf("alpha.two is named but not wired, and the reason must say so: %q / %q", two.Conclusion, two.Reason)
	}
	three := byName["alpha.three"]
	if len(three.Sites) != 0 || three.Dispatched {
		t.Errorf("alpha.three appears only in a _test.go and must have no sites: %+v", three.Sites)
	}
	if three.Conclusion != "not_implemented" || !strings.Contains(three.Reason, "no call in the tree takes it") {
		t.Errorf("alpha.three's conclusion = %q / %q", three.Conclusion, three.Reason)
	}
	if three.MentionedInCatalogDoc {
		t.Error("alpha.three is not named in the prose catalogue")
	}
}

func TestReconcileMCPReportsAnUndeclaredTool(t *testing.T) {
	rep := reconcileFixture(t)
	if len(rep.OutOfCatalog) != 1 {
		t.Fatalf("got %d undeclared tool names, want 1: %+v", len(rep.OutOfCatalog), rep.OutOfCatalog)
	}
	s := rep.OutOfCatalog[0]
	if s.In != "main" || !strings.Contains(s.File, "mcpserver/main.go") {
		t.Errorf("the undeclared name is reported at %+v", s)
	}
}

func TestMCPReportIsOrderedAndCarriesItsCaveats(t *testing.T) {
	rep := reconcileFixture(t)
	for i := 1; i < len(rep.Tools); i++ {
		if rep.Tools[i-1].Name > rep.Tools[i].Name {
			t.Fatalf("tools are not sorted: %s before %s", rep.Tools[i-1].Name, rep.Tools[i].Name)
		}
	}
	if len(rep.Notes) == 0 {
		t.Error("the report must carry what the scan cannot decide: a literal mention is not a dispatch")
	}
}

func TestToolNameShape(t *testing.T) {
	yes := []string{"project.get", "research_question.create", "a.b"}
	no := []string{"project.get.all", "Project.get", "project", "project.", ".get", "project-get"}
	for _, s := range yes {
		if !toolNameRe.MatchString(s) {
			t.Errorf("%q is a tool-shaped name", s)
		}
	}
	for _, s := range no {
		if toolNameRe.MatchString(s) {
			t.Errorf("%q is not a tool-shaped name", s)
		}
	}
}
