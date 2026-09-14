package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A task the Supervisor parks as rejected cannot be rejected again — so a
// baseline advance has to hand the reason to the rework instead, or the reason
// is silently dropped (#126, T0206).
func TestRebaselineHandsTheReasonToTheReworkWhenTheTaskIsAlreadyRejected(t *testing.T) {
	parked := rebaselineReworkArgs("T0001", "/tmp/reason.md", true)
	if !hasFlagValue(parked, "--reason-file", "/tmp/reason.md") {
		t.Errorf("a parked task's rework does not carry the reason: %v", parked)
	}
	// The ordinary case is unchanged: `task reject` records the reason, and the
	// rework must not record it a second time.
	moving := rebaselineReworkArgs("T0001", "/tmp/reason.md", false)
	if hasFlagValue(moving, "--reason-file", "/tmp/reason.md") {
		t.Errorf("a task the rejection will record also passed it to the rework: %v", moving)
	}
	for _, args := range [][]string{parked, moving} {
		if strings.Join(args[:3], " ") != "worker rework T0001" {
			t.Errorf("the rework command changed shape: %v", args)
		}
	}
}

// hasFlagValue reports whether args carries `flag value` as an adjacent pair.
func hasFlagValue(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// The parked/not-parked answer comes from the state file, not from the wording
// of a refusal.
func TestTaskIsRejectedReadsTheState(t *testing.T) {
	dir := t.TempDir()
	dagPath := filepath.Join(dir, "tasks.json")
	statePath := filepath.Join(dir, "task_status.json")
	writeFixtureJSON(t, dagPath, map[string]any{
		"version": 1, "task_count": 1,
		"tasks": []map[string]any{{"id": "T0001", "phase": "P1", "title": "t", "dependencies": []string{}}},
	})
	writeFixtureJSON(t, statePath, map[string]any{
		"version": 2,
		"tasks": map[string]any{"T0001": map[string]any{
			"status":  "rejected",
			"history": []any{},
		}},
	})

	parked, err := taskIsRejected(dagPath, statePath, "T0001")
	if err != nil {
		t.Fatal(err)
	}
	if !parked {
		t.Error("a rejected task was not reported as rejected")
	}

	writeFixtureJSON(t, statePath, map[string]any{
		"version": 2,
		"tasks": map[string]any{"T0001": map[string]any{
			"status":  "verification",
			"history": []any{},
		}},
	})
	parked, err = taskIsRejected(dagPath, statePath, "T0001")
	if err != nil {
		t.Fatal(err)
	}
	if parked {
		t.Error("a task in verification was reported as rejected")
	}
}

func writeFixtureJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
