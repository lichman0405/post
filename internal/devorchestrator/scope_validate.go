package devorchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Pre-dispatch allowed_scope validation (T0012 requirement 3): the
// Supervisor wrote scopes narrower than the work required three times —
// `internal/config/wiring*` matched no real file, `tests/**` was narrowed to
// invented globs, and a task adding specs/** files could not regenerate
// specs/SPEC_VERSION.json. Spawn now validates the scope against the real
// worktree before dispatch: a plain glob entry that matches nothing is a
// hard error (a `/**` entry that matches nothing is a loud warning — it is
// the legitimate "this directory will be created" form, used by test-only
// tasks), and a scope that covers a marker input of a derived artifact must
// also cover the derived artifact (hard error with a precise message).
// Collect complements this: the derived artifact of a covered marker is
// accepted as in scope, so the Worker can regenerate it without an
// out-of-scope rejection.

// DefaultDerivedArtifactsPath is the marker->derived rule file.
const DefaultDerivedArtifactsPath = "specs/orchestrator/derived-artifacts.json"

// DerivedRule pairs a marker glob with the artifact derived from its inputs.
type DerivedRule struct {
	Marker  string `json:"marker"`
	Derived string `json:"derived"`
	Note    string `json:"note,omitempty"`
}

// DerivedArtifacts is the loaded rule file.
type DerivedArtifacts struct {
	Version int           `json:"version"`
	Rules   []DerivedRule `json:"rules"`
}

// LoadDerivedArtifacts reads the marker->derived rule file at path.
func LoadDerivedArtifacts(path string) (*DerivedArtifacts, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading derived-artifacts %s: %w", path, err)
	}
	var d DerivedArtifacts
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("parsing derived-artifacts %s: %w", path, err)
	}
	if d.Version != 1 {
		return nil, fmt.Errorf("derived-artifacts %s has version %d, want 1", path, d.Version)
	}
	if len(d.Rules) == 0 {
		return nil, fmt.Errorf("derived-artifacts %s declares no rules", path)
	}
	return &d, nil
}

// DerivedArtifactsAt resolves path against repoRoot.
func DerivedArtifactsAt(repoRoot, path string) (*DerivedArtifacts, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(repoRoot, path)
	}
	return LoadDerivedArtifacts(path)
}

// derivedAt loads the repo's derived-artifact rules; nil when the file does
// not exist (scratch repos in e2e tests, pre-T0012 trees) — the derived
// coverage rules simply do not apply there.
func derivedAt(repoRoot string) *DerivedArtifacts {
	d, err := LoadDerivedArtifacts(filepath.Join(repoRoot, DefaultDerivedArtifactsPath))
	if err != nil {
		return nil
	}
	return d
}

// ScopeValidation is the pre-dispatch verdict for one allowed_scope.
type ScopeValidation struct {
	DeadEntries []string `json:"dead_entries,omitempty"` // /** entries matching nothing (warning)
	HardErrors  []string `json:"errors,omitempty"`       // dispatch-blocking problems
	Warnings    []string `json:"warnings,omitempty"`
}

// ValidateScopeAgainstTree checks every allowed_scope entry against the real
// tree at treeRoot. Hard errors: a non-/** entry that matches nothing, every
// entry dead, or a covered marker whose derived artifact is not covered.
// Warnings: a /** entry that matches nothing (directory-to-be-created form).
func ValidateScopeAgainstTree(scopes []string, treeRoot string, derived *DerivedArtifacts) (*ScopeValidation, error) {
	v := &ScopeValidation{}
	paths, err := treePaths(treeRoot)
	if err != nil {
		return nil, err
	}
	matched := 0
	for _, s := range scopes {
		matches, err := scopeEntryMatchesTree(s, paths)
		if err != nil {
			v.HardErrors = append(v.HardErrors, err.Error())
			continue
		}
		if matches {
			matched++
			continue
		}
		if strings.HasSuffix(s, "/**") {
			v.DeadEntries = append(v.DeadEntries, s)
		} else {
			v.HardErrors = append(v.HardErrors, fmt.Sprintf(
				"allowed_scope entry %q matches no file or directory in the worktree — an invented path narrows the task's reach (use %s/** for a directory the task will create, or fix the typo)", s, s))
		}
	}
	if matched == 0 {
		v.HardErrors = append(v.HardErrors, "no allowed_scope entry matches anything in the worktree — the Worker could write nowhere")
	}
	// Derived-artifact coverage: a task that can modify marker inputs must be
	// able to regenerate the artifact derived from them.
	if derived != nil {
		for _, r := range derived.Rules {
			if scopeCoversMarker(scopes, r.Marker, paths) && !scopeCoversPath(scopes, r.Derived) {
				v.HardErrors = append(v.HardErrors, fmt.Sprintf(
					"allowed_scope covers marker input %q but not its derived artifact %q — add %q to allowed_scope (%s)",
					r.Marker, r.Derived, r.Derived, r.Note))
			}
		}
	}
	for _, w := range v.DeadEntries {
		v.Warnings = append(v.Warnings, fmt.Sprintf(
			"allowed_scope entry %q matches nothing yet — assuming the task creates this directory", w))
	}
	sort.Strings(v.HardErrors)
	sort.Strings(v.Warnings)
	sort.Strings(v.DeadEntries)
	return v, nil
}

// scopeEntryMatchesTree reports whether one scope entry matches any path of
// the tree. Entries with glob metacharacters other than a trailing /** are
// expanded with filepath.Match against tree paths; the /** form matches the
// directory itself or anything under it (the collect-time semantics).
func scopeEntryMatchesTree(entry string, paths []string) (bool, error) {
	entry = strings.TrimSuffix(entry, "/")
	if strings.HasSuffix(entry, "/**") {
		base := strings.TrimSuffix(entry, "/**")
		for _, p := range paths {
			if p == base || strings.HasPrefix(p, base+"/") {
				return true, nil
			}
		}
		return false, nil
	}
	if !hasGlobMeta(entry) {
		for _, p := range paths {
			if p == entry || strings.HasPrefix(p, entry+"/") {
				return true, nil
			}
		}
		return false, nil
	}
	for _, p := range paths {
		ok, err := filepath.Match(entry, p)
		if err != nil {
			return false, fmt.Errorf("allowed_scope entry %q is not a valid glob: %w", entry, err)
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

func hasGlobMeta(s string) bool {
	return strings.ContainsAny(s, "*?[")
}

// scopeCoversMarker reports whether the scope can modify at least one real
// tree path matched by the marker glob (the marker means "any input file
// under here", so coverage = at least one such file is writable by the
// task).
func scopeCoversMarker(scopes []string, marker string, paths []string) bool {
	for _, p := range paths {
		ok, err := filepath.Match(strings.TrimSuffix(marker, "/**")+"/**", p)
		if err != nil {
			continue
		}
		if !ok {
			continue
		}
		if scopeCoversPath(scopes, p) {
			return true
		}
	}
	return false
}

// scopeCoversPath reports whether path is inside any scope entry (the
// collect-time matching semantics).
func scopeCoversPath(scopes []string, p string) bool {
	for _, s := range scopes {
		if strings.HasSuffix(s, "/**") {
			base := strings.TrimSuffix(s, "/**")
			if p == base || strings.HasPrefix(p, base+"/") {
				return true
			}
			continue
		}
		base := strings.TrimSuffix(s, "/")
		if p == base || strings.HasPrefix(p, base+"/") {
			return true
		}
	}
	return false
}

// treePaths returns every file and directory path under root (repo-relative,
// .git excluded, sorted).
func treePaths(root string) ([]string, error) {
	out := []string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking the tree for scope validation: %w", err)
	}
	sort.Strings(out)
	return out, nil
}

// DerivedScopeAllowance returns the derived artifact paths the scope may
// modify because it covers their marker inputs. Collect adds these to the
// effective scope so a Worker regenerating a derived marker is not falsely
// rejected (T0012: the specs/** task that could not regenerate
// specs/SPEC_VERSION.json).
func DerivedScopeAllowance(scopes []string, derived *DerivedArtifacts) []string {
	if derived == nil {
		return nil
	}
	out := []string{}
	for _, r := range derived.Rules {
		if scopeCoversPath(scopes, r.Marker) || scopeGlobCoversGlob(scopes, r.Marker) {
			out = append(out, r.Derived)
		}
	}
	sort.Strings(out)
	return out
}

// scopeGlobCoversGlob reports whether any scope entry is at least as broad
// as the marker glob (e.g. scope specs/** covers marker specs/**; scope
// specs/orchestrator/** does not).
func scopeGlobCoversGlob(scopes []string, marker string) bool {
	for _, s := range scopes {
		if strings.HasSuffix(s, "/**") {
			base := strings.TrimSuffix(s, "/**")
			markerBase := strings.TrimSuffix(marker, "/**")
			if base == markerBase {
				return true
			}
			// a scope glob broader than the marker: specs/** covers specs/x/**
			if strings.HasPrefix(markerBase+"/", base+"/") {
				return true
			}
		}
	}
	return false
}

// ScopeMatchesPathWithDerived is the collect-time scope predicate: the plain
// semantics plus the derived-artifact allowance.
func ScopeMatchesPathWithDerived(p string, scopes []string, derived *DerivedArtifacts) bool {
	if pathMatchesScope(p, scopes) {
		return true
	}
	for _, d := range DerivedScopeAllowance(scopes, derived) {
		if p == d || strings.HasPrefix(p, strings.TrimSuffix(d, "/")+"/") {
			return true
		}
	}
	return false
}
