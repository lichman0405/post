package contract

import (
	"path/filepath"
	"strings"
	"testing"
)

const decisionRoot = "testdata/cases/exemptions"

func TestResolveDecisionReadsTheTextItPointsAt(t *testing.T) {
	cases := []struct {
		citation string
		kind     string
		locator  string
		heading  string
	}{
		{"docs/22_API_DESIGN.md §3", "section", "§3", "## 3. 必备 headers"},
		{"docs/22_API_DESIGN.md §8", "section", "§8", "## 8. Search API"},
		{"tasks/decisions.md ㉚", "entry", "㉚", "## ㉚ fixture entry"},
		{"tasks/decisions.md L1-20260914-63", "entry", "L1-20260914-63", "## L1-20260914-63 fixture entry"},
		{"docs/adr/ADR-011-agent-semantic-api.md", "file", "", ""},
		{"see docs/22_API_DESIGN.md:3 for the header rules", "line", ":3", ""},
	}
	for _, c := range cases {
		ref, err := ResolveDecision(decisionRoot, c.citation)
		if err != nil {
			t.Errorf("ResolveDecision(%q): %v", c.citation, err)
			continue
		}
		if ref.Kind != c.kind || ref.Locator != c.locator {
			t.Errorf("ResolveDecision(%q) = (%s, %q), want (%s, %q)", c.citation, ref.Kind, ref.Locator, c.kind, c.locator)
		}
		if c.heading != "" && !strings.Contains(ref.Heading, c.heading) {
			t.Errorf("ResolveDecision(%q) landed on %q, want %q", c.citation, ref.Heading, c.heading)
		}
	}
}

func TestResolveDecisionRefusesCitationsThatDoNotResolve(t *testing.T) {
	cases := []struct {
		citation string
		why      string
	}{
		{"", "no decision citation at all"},
		{"we did not get to it", "names no repository path"},
		{"docs/22_API_DESIGN.md §99", "no heading numbered 99"},
		{"docs/22_API_DESIGN.md:900", "has"},
		{"tasks/decisions.md ㊿", "no heading carrying that numeral"},
		{"tasks/decisions.md L1-20990101-99", "no heading starting with it"},
		{"docs/99_NOT_A_FILE.md §1", "does not exist"},
	}
	for _, c := range cases {
		_, err := ResolveDecision(decisionRoot, c.citation)
		if err == nil {
			t.Errorf("ResolveDecision(%q) resolved; a citation that names nothing must fail", c.citation)
			continue
		}
		if !strings.Contains(err.Error(), c.why) {
			t.Errorf("ResolveDecision(%q) = %v, want it to mention %q", c.citation, err, c.why)
		}
	}
}

// TestResolveDecisionDemandsAHeadingNotJustTheNumber: a section citation has to
// land on a heading. "§3" appearing in a sentence is not a section, and the
// difference is what stops a citation from being satisfied by its own echo in
// some other paragraph.
func TestResolveDecisionDemandsAHeadingNotJustTheNumber(t *testing.T) {
	if _, err := ResolveDecision(decisionRoot, "docs/22_API_DESIGN.md §1"); err != nil {
		t.Errorf("§1 is a heading in the fixture: %v", err)
	}
	// docs/22_API_DESIGN.md's body mentions "3" nowhere as a heading other than
	// §3 itself; a citation to a number that only appears in prose must fail.
	if _, err := ResolveDecision(decisionRoot, "docs/22_API_DESIGN.md §22"); err == nil {
		t.Error("§22 is not a heading and must not resolve")
	}
}

func TestResolveDecisionRootIsTheArgumentNotTheWorkingDirectory(t *testing.T) {
	abs, err := filepath.Abs(decisionRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveDecision(abs, "docs/22_API_DESIGN.md §3"); err != nil {
		t.Errorf("an absolute root must work as well as a relative one: %v", err)
	}
	if _, err := ResolveDecision(filepath.Join(abs, "docs"), "docs/22_API_DESIGN.md §3"); err == nil {
		t.Error("a wrong root must fail rather than fall back to the working directory")
	}
}
