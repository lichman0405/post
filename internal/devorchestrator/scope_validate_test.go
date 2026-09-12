package devorchestrator

import (
	"os"
	"path/filepath"
	"testing"
)

// makeTree builds a scratch tree with the given relative file paths and
// returns its root (mimics a worktree: a specs/ dir with orchestrator files,
// a tasks/ dir, an internal/ dir).
func makeTree(t *testing.T, files ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range files {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestScopeValidationDeadGlobRefused is T0012 requirement 3's first defect:
// a plain scope entry matching nothing in the real tree is a hard error at
// dispatch — the invented glob `internal/config/wiring*` must never dispatch
// a Worker that can write nowhere.
func TestScopeValidationDeadGlobRefused(t *testing.T) {
	root := makeTree(t, "internal/config/env.go", "tasks/tasks.json")
	v, err := ValidateScopeAgainstTree([]string{"internal/config/wiring*"}, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.HardErrors) == 0 {
		t.Fatalf("a glob matching nothing produced no hard error: %+v", v)
	}
}

// TestScopeValidationStarGlobMatchesNothingIsWarning: the `dir/**` form is
// the legitimate "this directory will be created" idiom — a loud warning,
// not a dispatch refusal, when the scope also has live entries (the
// legitimate neighbour must still work; an ALL-dead scope is still refused,
// see TestScopeValidationScopeCoveringNothingRefused).
func TestScopeValidationStarGlobMatchesNothingIsWarning(t *testing.T) {
	root := makeTree(t, "internal/config/env.go", "tasks/tasks.json")
	v, err := ValidateScopeAgainstTree([]string{"internal/config/**", "tests/e2e/**"}, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.HardErrors) != 0 {
		t.Errorf("a dead `/**` entry beside a live one must not block dispatch, got: %v", v.HardErrors)
	}
	if len(v.Warnings) == 0 {
		t.Errorf("a dead `/**` entry must warn (directory-to-be-created assumption), got none")
	}
}

// TestScopeValidationCoveredMarkerRequiresDerivedArtifact is requirement 3's
// second defect: a task whose scope covers specs/** (the marker inputs of
// specs/SPEC_VERSION.json) must also cover specs/SPEC_VERSION.json itself —
// otherwise it cannot regenerate the derived marker its edits invalidate.
func TestScopeValidationCoveredMarkerRequiresDerivedArtifact(t *testing.T) {
	root := makeTree(t, "specs/orchestrator/gates.json", "specs/SPEC_VERSION.json", "tasks/tasks.json")
	derived := &DerivedArtifacts{Version: 1, Rules: []DerivedRule{
		{Marker: "specs/**", Derived: "specs/SPEC_VERSION.json", Note: "spec-version marker"},
	}}
	v, err := ValidateScopeAgainstTree([]string{"specs/orchestrator/**"}, root, derived)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range v.HardErrors {
		if e != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("scope covering specs/orchestrator/** without specs/SPEC_VERSION.json produced no hard error: %+v", v)
	}
	// The fixed scope dispatches clean.
	v, err = ValidateScopeAgainstTree([]string{"specs/**"}, root, derived)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.HardErrors) != 0 {
		t.Errorf("specs/** must cover the derived marker, got errors: %v", v.HardErrors)
	}
}

// TestScopeValidationScopeCoveringNothingRefused: every entry dead is a hard
// error even when they are all `/**` forms (a Worker that can write nowhere
// must never dispatch).
func TestScopeValidationScopeCoveringNothingRefused(t *testing.T) {
	root := makeTree(t, "tasks/tasks.json")
	v, err := ValidateScopeAgainstTree([]string{"nope/**"}, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.HardErrors) == 0 {
		t.Fatal("an all-dead scope must be a hard error")
	}
}

// TestScopeValidationHappyPath: a real scope against a real tree passes
// without errors or warnings.
func TestScopeValidationHappyPath(t *testing.T) {
	root := makeTree(t, "internal/config/env.go", "internal/config/wiring.go", "tasks/tasks.json")
	v, err := ValidateScopeAgainstTree([]string{"internal/config/**"}, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.HardErrors) != 0 || len(v.Warnings) != 0 {
		t.Errorf("valid scope produced findings: %+v", v)
	}
}

// TestDerivedScopeAllowance: collect accepts the derived marker as in scope
// when the scope covers its marker inputs — the Worker regenerating
// specs/SPEC_VERSION.json is not falsely rejected.
func TestDerivedScopeAllowance(t *testing.T) {
	derived := &DerivedArtifacts{Version: 1, Rules: []DerivedRule{
		{Marker: "specs/**", Derived: "specs/SPEC_VERSION.json"},
	}}
	if !ScopeMatchesPathWithDerived("specs/SPEC_VERSION.json", []string{"specs/**"}, derived) {
		t.Error("specs/SPEC_VERSION.json must be in scope for a specs/** task")
	}
	if ScopeMatchesPathWithDerived("specs/SPEC_VERSION.json", []string{"specs/orchestrator/**"}, derived) {
		t.Error("a specs/orchestrator/** task must NOT be able to write specs/SPEC_VERSION.json (the marker covers all of specs/**)")
	}
	if ScopeMatchesPathWithDerived("docs/61.md", []string{"specs/**"}, derived) {
		t.Error("docs/ must stay out of scope for a specs/** task")
	}
	if got := DerivedScopeAllowance([]string{"specs/**"}, nil); len(got) != 0 {
		t.Errorf("nil derived rules must yield no allowance, got %v", got)
	}
}
