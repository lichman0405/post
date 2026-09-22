package contract

import (
	"path/filepath"
	"strings"
	"testing"
)

// exemptCase runs the comparator over testdata/cases/exemptions with one of its
// exemption fixtures: every fixture differs only in the exemption list, so the
// differences in the reading are the exemption file's doing and nothing else.
func exemptCase(t *testing.T, exemptions string) *Gate {
	t.Helper()
	return caseRun{
		t: t, dir: "exemptions", contract: "contract-empty.yaml", exempt: exemptions,
	}.gate()
}

// TestExemptionsMakeAnUndocumentedRouteDocumented is the property the
// Supervisor's own acceptance probe rests on: an entry whose decision resolves
// removes exactly one route from the undocumented count.
func TestExemptionsMakeAnUndocumentedRouteDocumented(t *testing.T) {
	none := exemptCase(t, "exemptions-empty.yaml")
	if none.Counts.Undocumented != 1 || none.Counts.InExemptions != 0 {
		t.Fatalf("with no exemptions: undocumented = %d, exempted = %d; want 1 and 0",
			none.Counts.Undocumented, none.Counts.InExemptions)
	}
	if len(none.Findings) != 1 || none.Findings[0].Kind != Undocumented {
		t.Fatalf("with no exemptions the reading must be one undocumented finding, got %q", findingLines(none))
	}

	// The three citation forms the exemption file's header names: a numbered
	// section of a doc, a circled entry in tasks/decisions.md, an ADR.
	for _, fixture := range []string{
		"exemptions-cited.yaml",
		"exemptions-cited-entry.yaml",
		"exemptions-cited-decision.yaml",
		"exemptions-cited-adr.yaml",
	} {
		t.Run(fixture, func(t *testing.T) {
			g := exemptCase(t, fixture)
			if !g.OK() {
				t.Errorf("gate is not quiet: %q", findingLines(g))
			}
			if g.Counts.Undocumented != 0 {
				t.Errorf("undocumented = %d, want 0 — the entry documents the route", g.Counts.Undocumented)
			}
			if g.Counts.InExemptions != 1 || g.Counts.Cited != 1 || g.Counts.Exemptions != 1 {
				t.Errorf("exempted = %d, cited = %d, entries = %d; want 1, 1, 1",
					g.Counts.InExemptions, g.Counts.Cited, g.Counts.Exemptions)
			}
			if got := g.RouteState["GET /api/v1/alpha"]; got != "exempted" {
				t.Errorf("route state = %q, want exempted", got)
			}
			if m := g.RouteMatch["GET /api/v1/alpha"]; !strings.HasPrefix(m, "exemption: ") {
				t.Errorf("route match = %q, want an exemption citation", m)
			}
		})
	}
}

// TestExemptionsWithoutADecisionAreNotExemptions is the anti-pattern the file's
// header names first: an entry whose reason is "we did not get to it" must not
// buy the route anything.
func TestExemptionsWithoutADecisionAreNotExemptions(t *testing.T) {
	g := exemptCase(t, "exemptions-uncited.yaml")
	if g.Counts.InExemptions != 0 {
		t.Errorf("exempted = %d, want 0: a citation that resolves to no decision exempts nothing", g.Counts.InExemptions)
	}
	if g.Counts.Undocumented != 1 {
		t.Errorf("undocumented = %d, want 1: the route is exactly as undocumented as it was", g.Counts.Undocumented)
	}
	if g.Counts.Cited != 0 || g.Counts.Exemptions != 1 {
		t.Errorf("cited = %d, entries = %d; want 0 and 1", g.Counts.Cited, g.Counts.Exemptions)
	}
	// Two defects on two lines: the endpoint is undocumented, and the entry in
	// the exemption file rests on nothing. Both are reported.
	if got := len(g.FindingsOfKind(Undocumented)); got != 1 {
		t.Errorf("got %d undocumented findings, want 1: %q", got, findingLines(g))
	}
	uncited := g.FindingsOfKind(ExemptUncited)
	if len(uncited) != 1 {
		t.Fatalf("got %d uncited-exemption findings, want 1: %q", len(uncited), findingLines(g))
	}
	if !strings.Contains(uncited[0].Detail, "names no repository path") {
		t.Errorf("detail does not say why the citation failed: %q", uncited[0].Detail)
	}
}

func TestExemptionCitationMustLandOnTextThatExists(t *testing.T) {
	g := exemptCase(t, "exemptions-phantom-section.yaml")
	if g.Counts.InExemptions != 0 || g.Counts.Cited != 0 {
		t.Errorf("exempted = %d, cited = %d; want 0 and 0 — the section is not in the file",
			g.Counts.InExemptions, g.Counts.Cited)
	}
	uncited := g.FindingsOfKind(ExemptUncited)
	if len(uncited) != 1 {
		t.Fatalf("got %d uncited findings, want 1: %q", len(uncited), findingLines(g))
	}
	if !strings.Contains(uncited[0].Detail, "no heading numbered 99") {
		t.Errorf("detail does not say which heading is missing: %q", uncited[0].Detail)
	}
}

func TestExemptionForARouteNobodyMountsIsStale(t *testing.T) {
	g := exemptCase(t, "exemptions-unused.yaml")
	unused := g.FindingsOfKind(ExemptUnused)
	if len(unused) != 1 {
		t.Fatalf("got %d unused-exemption findings, want 1: %q", len(unused), findingLines(g))
	}
	if unused[0].Method+" "+unused[0].Path != "GET /api/v1/nowhere" {
		t.Errorf("the finding names %s %s", unused[0].Method, unused[0].Path)
	}
	// It is cited correctly, so it is not also reported as uncited: one defect,
	// one finding.
	if len(g.FindingsOfKind(ExemptUncited)) != 0 {
		t.Errorf("a correctly cited but stale entry is reported as uncited too: %q", findingLines(g))
	}
}

// TestExemptionListIsNotAWildcardParkingLot: the verb-suffix catch-alls resolve
// to contract entries. Parking one in the exemption list hides a real endpoint
// behind a shape the contract cannot express, and it is reported even though
// the citation is perfectly good.
func TestExemptionListIsNotAWildcardParkingLot(t *testing.T) {
	g := exemptCase(t, "exemptions-catchall.yaml")
	bad := g.FindingsOfKind(ExemptionCatchall)
	if len(bad) != 1 {
		t.Fatalf("got %d catch-all-exemption findings, want 1: %q", len(bad), findingLines(g))
	}
	if bad[0].Path != "/api/v1/search/{rest...}" {
		t.Errorf("the finding names %s", bad[0].Path)
	}
	if !strings.Contains(bad[0].Detail, "belongs in the contract resolution") {
		t.Errorf("detail does not say where it belongs: %q", bad[0].Detail)
	}
	if g.Counts.Cited != 1 {
		t.Errorf("cited = %d: the finding is about the shape, not the citation", g.Counts.Cited)
	}
}

func TestExemptionEntryShapeMatchesTheFile(t *testing.T) {
	list, err := ReadExemptions(filepath.Join("testdata", "cases", "exemptions", "exemptions-cited.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if list.Version != 1 || len(list.Exemptions) != 1 {
		t.Fatalf("parsed %+v", list)
	}
	e := list.Exemptions[0]
	if e.Method != "GET" || e.Path != "/api/v1/alpha" || e.Decision != "docs/22_API_DESIGN.md §3" {
		t.Errorf("entry read as %+v", e)
	}
	if e.Reason == "" {
		t.Error("reason was not read")
	}
	if e.IsCatchAll() {
		t.Error("a literal path is not a catch-all")
	}
	cat, err := ReadExemptions(filepath.Join("testdata", "cases", "exemptions", "exemptions-catchall.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !cat.Exemptions[0].IsCatchAll() {
		t.Error("{rest...} must read as a catch-all")
	}
}

// TestUnreadableExemptionFileIsAnErrorNotAFinding: an instrument that cannot
// read its own input must say so, rather than reporting the tree as clean.
func TestUnreadableExemptionFileIsAnErrorNotAFinding(t *testing.T) {
	_, err := Run(Options{
		Root: filepath.Join("testdata", "cases", "exemptions"), Dirs: []string{"tree"},
		ContractPath: "contract-empty.yaml", ExemptionPath: "exemptions-does-not-exist.yaml",
	})
	if err == nil {
		t.Fatal("a missing exemption list must be an error: a gate that treats 'I could not read it' as 'nothing is exempt' reports a clean tree it never read")
	}
}
