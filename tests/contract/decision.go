package contract

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DecisionRef is what a `decision:` citation resolved to. An exemption whose
// citation does not resolve to real text is worse than no exemption: it looks
// like a decision was taken. specs/api/openapi-exemptions.yaml says exactly
// that in its header, and this resolver is what makes the sentence executable.
type DecisionRef struct {
	// Path is the repository path the citation named (verified to exist).
	Path string `json:"path"`
	// Kind is how strong the resolution is: "file" (the file exists),
	// "section" (a heading numbered as cited), "entry" (a decisions.md
	// circled entry), "line" (a line number within the file).
	Kind string `json:"kind"`
	// Locator is the section/entry/line the citation named, if any.
	Locator string `json:"locator,omitempty"`
	// Heading is the heading line that satisfied the locator, quoted back so
	// the reader sees the text rather than trusting a boolean.
	Heading string `json:"heading,omitempty"`
}

// repoPathRe finds the repository path inside a free-text citation. The
// citation is written by a human ("docs/22 §28" or "tasks/decisions.md ㉚"), so
// the tool's job is to find the path, then check that the text it points at
// really says something.
var repoPathRe = regexp.MustCompile(`(?:docs|tasks|specs|ops|packages|infra|tests)/[A-Za-z0-9_][A-Za-z0-9_./-]*\.(?:md|yaml|yml|json|csv|txt)`)

// circledRe matches the circled numerals tasks/decisions.md uses to number its
// entries (①-⑳, ㉑-㉟, ㊱-㊿).
var circledRe = regexp.MustCompile(`[\x{2460}-\x{2473}\x{3251}-\x{325F}\x{32B1}-\x{32BF}]`)

// sectionRe matches a "§N" or "§N.M" locator.
var sectionRe = regexp.MustCompile(`§\s*([0-9]+(?:\.[0-9]+)*)`)

// decisionEntryRe matches the heading form tasks/decisions.md actually uses for
// today's entries: "## L1-20260914-63". The file's older entries are numbered
// with circled numerals, and the exemption file's header names that form; this
// one is the same kind of locator (an entry heading in that file), and a
// citation that names an existing heading is text that resolves, whatever
// alphabet the heading is written in.
var decisionEntryRe = regexp.MustCompile(`\bL[0-3]-\d{8}-\d+\b`)

// lineRe matches a ":N" locator (a line number).
var lineRe = regexp.MustCompile(`:([0-9]+)\b`)

// ResolveDecision verifies a citation against the repository at root.
//
// Rules, in the order the exemption file's header states them:
//   - the citation must name a repository path, and that path must exist;
//   - if it names a section ("§N" / "§N.M"), the target file must contain a
//     heading numbered N (or N.M);
//   - if it names a circled entry ("㉚"), the target file must contain a
//     heading carrying that numeral;
//   - if it names a line (":N"), the file must have that many lines.
//
// A citation with only a path resolves as "file": weaker, and reported as
// weaker rather than silently treated as a section reference.
func ResolveDecision(root, citation string) (DecisionRef, error) {
	trimmed := strings.TrimSpace(citation)
	if trimmed == "" {
		return DecisionRef{}, fmt.Errorf("no decision citation at all: an exemption without one is an undocumented endpoint wearing a hat")
	}
	loc := repoPathRe.FindString(trimmed)
	if loc == "" {
		return DecisionRef{}, fmt.Errorf("citation %q names no repository path (docs/NN_*.md, docs/adr/*.md or tasks/decisions.md)", citation)
	}
	abs := filepath.Join(root, loc)
	raw, err := os.ReadFile(abs)
	if err != nil {
		return DecisionRef{}, fmt.Errorf("citation %q names %s, which does not exist in this tree", citation, loc)
	}
	text := string(raw)
	rest := strings.Replace(trimmed, loc, " ", 1)
	ref := DecisionRef{Path: loc, Kind: "file"}

	if m := sectionRe.FindStringSubmatch(rest); m != nil {
		heading := findNumberedHeading(text, m[1])
		if heading == "" {
			return DecisionRef{}, fmt.Errorf("citation %q names section §%s, but %s has no heading numbered %s", citation, m[1], loc, m[1])
		}
		ref.Kind, ref.Locator, ref.Heading = "section", "§"+m[1], heading
		return ref, nil
	}
	if m := circledRe.FindString(rest); m != "" {
		heading := findCircledHeading(text, m)
		if heading == "" {
			return DecisionRef{}, fmt.Errorf("citation %q cites entry %s, but %s has no heading carrying that numeral", citation, m, loc)
		}
		ref.Kind, ref.Locator, ref.Heading = "entry", m, heading
		return ref, nil
	}
	if m := decisionEntryRe.FindString(rest); m != "" {
		heading := findTitledHeading(text, m)
		if heading == "" {
			return DecisionRef{}, fmt.Errorf("citation %q cites entry %s, but %s has no heading starting with it", citation, m, loc)
		}
		ref.Kind, ref.Locator, ref.Heading = "entry", m, heading
		return ref, nil
	}
	if m := lineRe.FindStringSubmatch(rest); m != nil {
		n := 0
		fmt.Sscanf(m[1], "%d", &n)
		lines := strings.Count(text, "\n") + 1
		if n == 0 || n > lines {
			return DecisionRef{}, fmt.Errorf("citation %q names line %d, but %s has %d lines", citation, n, loc, lines)
		}
		ref.Kind, ref.Locator = "line", ":"+m[1]
		return ref, nil
	}
	return ref, nil
}

// findNumberedHeading returns the first heading line numbered as given. Docs
// number their headings "## N. Title" (docs/12, docs/22, …) and decisions.md
// numbers its entries with circled numerals, so a section citation has to land
// on a heading, not merely on any occurrence of the number.
func findNumberedHeading(text, num string) string {
	pat := regexp.MustCompile(`^#{1,6}\s*` + regexp.QuoteMeta(num) + `(?:[.\s、:：)]|$)`)
	for _, ln := range strings.Split(text, "\n") {
		t := strings.TrimSpace(ln)
		if !strings.HasPrefix(t, "#") {
			continue
		}
		if pat.MatchString(t) {
			return t
		}
	}
	return ""
}

func findTitledHeading(text, title string) string {
	pat := regexp.MustCompile(`^#{1,6}\s*` + regexp.QuoteMeta(title) + `\b`)
	for _, ln := range strings.Split(text, "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "#") && pat.MatchString(t) {
			return t
		}
	}
	return ""
}

func findCircledHeading(text, numeral string) string {
	for _, ln := range strings.Split(text, "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "#") && strings.Contains(t, numeral) {
			return t
		}
	}
	return ""
}
