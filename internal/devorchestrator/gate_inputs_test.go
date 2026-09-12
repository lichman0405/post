package devorchestrator

import (
	"os"
	"path/filepath"
	"testing"
)

// gateFixture builds the authoritative record, the task dir (Worker-writable)
// and a matching registry the way Spawn does (early WriteGateInputs, then
// FinalizeGateInputs once the process exists).
func gateFixture(t *testing.T) (repoRoot, taskID string, rec *WorkerRecord, gate *GateInputs) {
	t.Helper()
	repoRoot = t.TempDir()
	taskID = "T0001"
	taskDir := filepath.Join(repoRoot, "workers", taskID)
	if err := os.MkdirAll(filepath.Join(taskDir, "guard"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"task-package.json":     `{"task_id":"T0001","allowed_scope":["internal/config/**"]}` + "\n",
		"worker-settings.json":  `{"permissions":{"defaultMode":"dontAsk"}}` + "\n",
		"run-worker.sh":         "#!/bin/bash\necho reaper\n",
		"guard/worker-guard.sh": "#!/bin/bash\necho guard\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(taskDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gate = &GateInputs{
		TaskID:             taskID,
		RunID:              "run-1",
		SessionID:          "session-1",
		BaselineSHA:        "abc123",
		Branch:             "task/T0001-x",
		Worktree:           filepath.Join(repoRoot, "wt"),
		ResultDir:          taskDir,
		LogPath:            filepath.Join(taskDir, "worker.log"),
		RefsBefore:         []string{"refs/heads/main"},
		AllowedScope:       []string{"internal/config/**"},
		RequiredTests:      []string{"go test ./internal/config/..."},
		AcceptanceCriteria: []string{"a1"},
	}
	if err := WriteGateInputs(repoRoot, taskID, gate, taskDir); err != nil {
		t.Fatal(err)
	}
	if err := FinalizeGateInputs(repoRoot, taskID, "run-1", "session-1", 4242, 1001, 4241, "2026-09-12T10:00:00Z", []string{"127.0.0.1:8080"}); err != nil {
		t.Fatal(err)
	}
	gate, err := LoadGateInputs(repoRoot, taskID)
	if err != nil {
		t.Fatal(err)
	}
	rec = &WorkerRecord{
		TaskID: taskID, RunID: "run-1", SessionID: "session-1",
		BaselineSHA: "abc123", Branch: "task/T0001-x",
		Worktree: filepath.Join(repoRoot, "wt"), ResultDir: taskDir,
		LogPath:    filepath.Join(taskDir, "worker.log"),
		RefsBefore: []string{"refs/heads/main"},
		PID:        4242, StartTime: 1001, SessionLeaderPID: 4241,
		StartedAt:       "2026-09-12T10:00:00Z",
		ListenersBefore: []string{"127.0.0.1:8080"},
	}
	return repoRoot, taskID, rec, gate
}

// TestGateInputsUntamperedPasses: the happy path — matching registry and
// untouched task dir produce zero tamper findings.
func TestGateInputsUntamperedPasses(t *testing.T) {
	repoRoot, _, rec, gate := gateFixture(t)
	// exit.status: the reaper writes both copies identically.
	if err := os.WriteFile(authoritativeExitStatusPath(repoRoot, "T0001"), []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rec.ResultDir, "exit.status"), []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if tampers := VerifyGateInputs(repoRoot, rec, gate); len(tampers) != 0 {
		t.Fatalf("untampered fixture reported tampers: %+v", tampers)
	}
}

// TestGateInputsRegistryTamperDetected: the Worker rewrites its own
// registry.json (baseline_sha, pid, start time — the exact fields collect
// used to trust). Every rewrite is detected; collect judges by the
// authoritative record instead.
func TestGateInputsRegistryTamperDetected(t *testing.T) {
	repoRoot, _, rec, gate := gateFixture(t)

	// baseline_sha rewrite: the classic gate escape — moving the baseline
	// forward to hide out-of-scope changes.
	tampered := *rec
	tampered.BaselineSHA = "deadbeef"
	if tampers := VerifyGateInputs(repoRoot, &tampered, gate); len(tampers) == 0 {
		t.Fatal("baseline_sha rewrite was not detected")
	}

	// pid/start-time rewrite: blinding the residue check.
	tampered = *rec
	tampered.PID = 1
	tampered.StartTime = 2
	tampers := VerifyGateInputs(repoRoot, &tampered, gate)
	found := map[string]bool{}
	for _, tp := range tampers {
		found[tp.What] = true
	}
	if !found["registry"] {
		t.Fatalf("pid/start-time rewrite produced no registry tamper: %+v", tampers)
	}

	// refs_before rewrite: hiding a ref the Worker created.
	tampered = *rec
	tampered.RefsBefore = []string{"refs/heads/main", "refs/heads/sneaky"}
	if tampers := VerifyGateInputs(repoRoot, &tampered, gate); len(tampers) == 0 {
		t.Fatal("refs_before rewrite was not detected")
	}
}

// TestGateInputsPackageAndGuardTamperDetected: the Worker edits
// task-package.json (its own allowed_scope) or the guard layer it runs
// under. The byte comparison against the spawn-time copies catches both.
func TestGateInputsPackageAndGuardTamperDetected(t *testing.T) {
	repoRoot, _, rec, gate := gateFixture(t)

	// task-package.json: widen the scope.
	if err := os.WriteFile(filepath.Join(rec.ResultDir, "task-package.json"), []byte(`{"task_id":"T0001","allowed_scope":["**"]}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if tampers := VerifyGateInputs(repoRoot, rec, gate); len(tampers) == 0 {
		t.Fatal("task-package.json rewrite was not detected")
	}

	// Guard layer: disable the write hook.
	repoRoot2, _, rec2, gate2 := gateFixture(t)
	if err := os.WriteFile(filepath.Join(rec2.ResultDir, "guard", "worker-guard.sh"), []byte("#!/bin/bash\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if tampers := VerifyGateInputs(repoRoot2, rec2, gate2); len(tampers) == 0 {
		t.Fatal("guard script rewrite was not detected")
	}

	// run-worker.sh: a reaper that writes a forged exit status.
	repoRoot3, _, rec3, gate3 := gateFixture(t)
	if err := os.WriteFile(filepath.Join(rec3.ResultDir, "run-worker.sh"), []byte("#!/bin/bash\necho 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if tampers := VerifyGateInputs(repoRoot3, rec3, gate3); len(tampers) == 0 {
		t.Fatal("run-worker.sh rewrite was not detected")
	}
}

// TestGateInputsExitStatusTamperDetected: the Worker rewrites its own
// exit.status to 0; the authoritative reaper copy says otherwise.
func TestGateInputsExitStatusTamperDetected(t *testing.T) {
	repoRoot, _, rec, gate := gateFixture(t)
	if err := os.WriteFile(authoritativeExitStatusPath(repoRoot, "T0001"), []byte("3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rec.ResultDir, "exit.status"), []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tampers := VerifyGateInputs(repoRoot, rec, gate)
	found := false
	for _, tp := range tampers {
		if tp.What == "exit.status" {
			found = true
		}
	}
	if !found {
		t.Fatalf("exit.status mismatch was not detected: %+v", tampers)
	}
}

// TestFinalizeGateInputsRefusesRewrittenRecord: finalize must never bless a
// record the Worker (or anyone) rewrote between spawn and finalize — a
// swapped run id would anchor the registry to a foreign authoritative record.
func TestFinalizeGateInputsRefusesRewrittenRecord(t *testing.T) {
	repoRoot, _, _, _ := gateFixture(t)
	err := FinalizeGateInputs(repoRoot, "T0001", "run-OTHER", "session-OTHER", 1, 1, 1, "2026-09-12T10:00:00Z", nil)
	if err == nil {
		t.Fatal("finalize against a mismatched run/session succeeded")
	}
	// The legitimate neighbour: the correct pair works.
	if err := FinalizeGateInputs(repoRoot, "T0001", "run-1", "session-1", 4242, 1001, 4241, "2026-09-12T10:00:00Z", []string{"127.0.0.1:8080"}); err != nil {
		t.Fatalf("finalize with the correct run/session failed: %v", err)
	}
}

// TestLoadGateInputsRejectsForeignRecord: the record on disk must carry the
// task's own id — a copied record from another task is a load error.
func TestLoadGateInputsRejectsForeignRecord(t *testing.T) {
	repoRoot, _, _, _ := gateFixture(t)
	path := gateInputsPath(repoRoot, "T0001")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Write a record for T0002 carrying T0001's id: the load must refuse.
	if err := os.MkdirAll(RuntimeTasksDir(repoRoot, "T0002"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gateInputsPath(repoRoot, "T0002"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGateInputs(repoRoot, "T0002"); err == nil {
		t.Fatal("LoadGateInputs accepted a record whose task_id belongs to another task")
	}
}
