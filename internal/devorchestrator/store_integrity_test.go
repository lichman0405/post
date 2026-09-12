package devorchestrator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests encode the two probes that exposed the security review findings
// against store.go. Both defects were reachable only through cases the original
// tests never tried, so each test below is the failing case, not the happy path.

func writeDAG(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "tasks.json")
	dag := map[string]any{
		"version": 1, "task_count": 2, "phases": map[string]string{"P0": "x"},
		"tasks": []map[string]any{
			{"id": "T0001", "title": "one", "phase": "P0", "dependencies": []string{},
				"requirements": []string{"r"}, "acceptance_criteria": []string{"a"},
				"allowed_scope": []string{"x/**"}, "decision_level_max": "L1"},
			{"id": "T0002", "title": "two", "phase": "P0", "dependencies": []string{"T0001"},
				"requirements": []string{"r"}, "acceptance_criteria": []string{"a"},
				"allowed_scope": []string{"x/**"}, "decision_level_max": "L1"},
		},
	}
	b, err := json.Marshal(dag)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The lock must be a property of the FILE, not of the string a caller typed.
// Previously the default spelling used one lock file and an explicitly
// re-spelled path used another, so two processes writing the same state file
// took different locks and silently lost an update.
func TestLockPathIsCanonicalAcrossSpellings(t *testing.T) {
	dir := t.TempDir()
	dagPath := writeDAG(t, dir)

	// Spellings that all denote the same file.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()

	spellings := []string{
		"task_status.json",
		"./task_status.json",
		filepath.Join(dir, "task_status.json"),
		filepath.Join(dir, ".", "task_status.json"),
	}
	var locks []string
	for _, sp := range spellings {
		s, err := OpenStore(dagPath, sp)
		if err != nil {
			t.Fatalf("OpenStore(%q): %v", sp, err)
		}
		locks = append(locks, s.lockPath)
	}
	for i := 1; i < len(locks); i++ {
		if locks[i] != locks[0] {
			t.Errorf("lock path differs by spelling:\n  %q\n  %q\n"+
				"two writers naming the same file would not exclude each other", locks[0], locks[i])
		}
	}
}

// A state file that exists but declares no tasks is drift (truncated, renamed,
// hand-edited), NOT "every task is todo". Reading it as all-todo silently
// reset every completed task and let finished work be re-dispatched.
func TestStateDriftIsRefusedNotReadAsAllTodo(t *testing.T) {
	dir := t.TempDir()
	dagPath := writeDAG(t, dir)
	statePath := filepath.Join(dir, "task_status.json")

	// A missing file is still a legitimate fresh start.
	s, err := OpenStore(dagPath, statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Next(); err != nil {
		t.Fatalf("a missing state file must be a fresh start, got: %v", err)
	}

	// A file that exists but carries no tasks is drift.
	for _, content := range []string{
		`{}`,
		`{"version":2}`,
		`{"version":2,"tasks":{}}`,
		`{"version":2,"tasks":null}`,
	} {
		if err := os.WriteFile(statePath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		s, err := OpenStore(dagPath, statePath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Next(); err == nil {
			t.Errorf("state %s: Next() accepted a taskless state file; "+
				"every completed task would silently read as todo", content)
		} else if !strings.Contains(err.Error(), "declares no tasks") {
			t.Errorf("state %s: error does not explain the drift: %v", content, err)
		}
		if _, err := s.Inspect("T0001"); err == nil {
			t.Errorf("state %s: Inspect() accepted a taskless state file", content)
		}
		if _, err := s.Transition("T0001", StateReady, NewRunID(), ""); err == nil {
			t.Errorf("state %s: Transition() accepted a taskless state file — "+
				"a merged task could be re-dispatched", content)
		}
	}
}

// The drift guard must not fire on a state file that genuinely has entries.
func TestNormalStateStillWorks(t *testing.T) {
	dir := t.TempDir()
	dagPath := writeDAG(t, dir)
	statePath := filepath.Join(dir, "task_status.json")
	if err := os.WriteFile(statePath, []byte(
		`{"version":2,"tasks":{"T0001":{"status":"merged"},"T0002":{"status":"todo"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(dagPath, statePath)
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.Next()
	if err != nil {
		t.Fatalf("Next() on a valid state file: %v", err)
	}
	// T0001 is merged and T0002's dependency is satisfied, so T0002 is dispatchable.
	if len(next) != 1 || next[0].ID != "T0002" {
		t.Fatalf("expected exactly T0002 to be dispatchable, got %+v", next)
	}
}
