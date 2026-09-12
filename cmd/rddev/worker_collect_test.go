package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// `rddev worker collect` end-to-end: every scenario runs the real spawn
// pipeline against the fake repo and the fake claude binary (the same
// substitution worker_test.go uses), then collects the finished run. Each
// rule is tested both ways — the violation is rejected AND the legitimate
// neighbouring run is collected cleanly.

// collectCLI runs worker collect and returns exit code + output.
func collectCLI(t *testing.T, repo, taskID string) (int, string, string) {
	t.Helper()
	return runWorkerCLI(t, repo, "worker", "collect", taskID)
}

// spawnAndWaitExited spawns taskID and waits until the registry shows the
// run exited (the reaper recorded exit.status).
func spawnAndWaitExited(t *testing.T, repo, taskID string) {
	t.Helper()
	code, out, errOut := runWorkerCLI(t, repo, "worker", "spawn", taskID)
	if code != 0 {
		t.Fatalf("spawn %s: exit %d\nstdout: %s\nstderr: %s", taskID, code, out, errOut)
	}
	waitFor(t, taskID+" exited", func() bool {
		s, _ := listStatus(repo, taskID)
		return s == "exited"
	})
}

// killPidFromFile kills the pid recorded in resultDir/name (test hygiene for
// the residue/listener scenarios).
func killPidFromFile(t *testing.T, repo, taskID, name string) {
	t.Helper()
	t.Cleanup(func() {
		data, err := os.ReadFile(filepath.Join(repo, ".rddev", "workers", taskID, name))
		if err != nil {
			return
		}
		var pid int
		if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err != nil || pid <= 0 {
			return
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})
}

// stateOf reads the task's status from the state file.
func stateOf(t *testing.T, repo, taskID string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repo, "tasks", "task_status.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Tasks map[string]struct {
			Status string `json:"status"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Tasks[taskID].Status
}

// TestWorkerCollectHappyPath: an in-scope change with a conforming
// RESULT.json is collected cleanly and moves the task to verification —
// the boundary must not break legitimate work.
func TestWorkerCollectHappyPath(t *testing.T) {
	fakeClaudePath(t, "write-scope")
	t.Setenv("FAKE_CLAUDE_SECONDS", "2")
	repo := fakeRepo(t)

	spawnAndWaitExited(t, repo, "T0001")
	code, out, errOut := collectCLI(t, repo, "T0001")
	if code != 0 {
		t.Fatalf("collect: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	for _, want := range []string{"collect ok", "state -> verification", "[ok] head-baseline", "[ok] scope", "[ok] result-schema", "[ok] residue", "t0001/marker.txt"} {
		if !strings.Contains(out, want) {
			t.Errorf("collect output missing %q:\n%s", want, out)
		}
	}
	if got := stateOf(t, repo, "T0001"); got != "verification" {
		t.Errorf("task state = %s, want verification", got)
	}
	// the report is persisted next to the RESULT it judged
	reportPath := filepath.Join(repo, ".rddev", "workers", "T0001", "collect-report.json")
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("collect report not written: %v", err)
	}
	var report struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &report); err != nil || report.Status != "ok" {
		t.Errorf("collect-report.json = %s (%v), want status ok", data, err)
	}
}

// TestWorkerCollectRejectsNonConformingResult: the T0010 failure shape —
// tests[].output instead of .evidence and acceptance[] as plain strings — is
// rejected with field-level errors; the neighbouring conforming run is
// accepted (the happy path test above).
func TestWorkerCollectRejectsNonConformingResult(t *testing.T) {
	fakeClaudePath(t, "sleep")
	t.Setenv("FAKE_CLAUDE_SECONDS", "1")
	repo := fakeRepo(t)

	spawnAndWaitExited(t, repo, "T0001")

	// plant the exact mistake the T0010 Worker delivered
	bad := `{
  "task_id": "T0001",
  "status": "completed",
  "summary": "planted non-conforming result",
  "files_changed": [],
  "tests": [{"command": "true", "status": "passed", "output": "ok"}],
  "acceptance": ["AC1 passed", "AC2 passed"],
  "risks": [],
  "follow_up_issues": [],
  "notes_for_supervisor": ""
}`
	if err := os.WriteFile(filepath.Join(repo, ".rddev", "workers", "T0001", "RESULT.json"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := collectCLI(t, repo, "T0001")
	if code != 1 {
		t.Fatalf("collect of a non-conforming RESULT: exit %d, want 1\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	for _, want := range []string{
		"collect rejected",
		`[FAIL] result-schema`,
		`tests[0]: unknown field "output"`,
		"acceptance[0]:", "is not of type object",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("collect output missing %q:\n%s", want, out)
		}
	}
	if got := stateOf(t, repo, "T0001"); got != "rejected" {
		t.Errorf("task state = %s, want rejected", got)
	}
}

// TestWorkerCollectRejectsHeadMoved: a bypassed guard's commit moves HEAD;
// collection force-rejects it because HEAD differs from the recorded
// baseline (the second half of the git control-plane denial: even when the
// guard never sees it, the move is caught at collection).
func TestWorkerCollectRejectsHeadMoved(t *testing.T) {
	fakeClaudePath(t, "write-scope")
	t.Setenv("FAKE_CLAUDE_SECONDS", "1")
	repo := fakeRepo(t)

	spawnAndWaitExited(t, repo, "T0001")
	worktree := filepath.Join(repo, ".rddev", "worktrees", "T0001")
	cmd := exec.Command("git", "-c", "user.name=evil-worker", "-c", "user.email=evil@test", "commit", "--allow-empty", "-m", "control-plane move")
	cmd.Dir = worktree
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("planting the commit: %v\n%s", err, out)
	}

	code, out, errOut := collectCLI(t, repo, "T0001")
	if code != 1 {
		t.Fatalf("collect with moved HEAD: exit %d, want 1\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	for _, want := range []string{"collect rejected", "[FAIL] head-baseline", "[FAIL] branch-ref"} {
		if !strings.Contains(out, want) {
			t.Errorf("collect output missing %q:\n%s", want, out)
		}
	}
	if got := stateOf(t, repo, "T0001"); got != "rejected" {
		t.Errorf("task state = %s, want rejected", got)
	}
}

// TestWorkerCollectRejectsNewRef: a tag created during the run is an
// unauthorized ref and is rejected; an untagged run passes (happy path).
func TestWorkerCollectRejectsNewRef(t *testing.T) {
	fakeClaudePath(t, "write-scope")
	t.Setenv("FAKE_CLAUDE_SECONDS", "1")
	repo := fakeRepo(t)

	spawnAndWaitExited(t, repo, "T0001")
	cmd := exec.Command("git", "tag", "evil-release-tag")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("planting the tag: %v\n%s", err, out)
	}

	code, out, errOut := collectCLI(t, repo, "T0001")
	if code != 1 {
		t.Fatalf("collect with a new ref: exit %d, want 1\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	for _, want := range []string{"collect rejected", "[FAIL] refs", "evil-release-tag"} {
		if !strings.Contains(out, want) {
			t.Errorf("collect output missing %q:\n%s", want, out)
		}
	}
}

// TestWorkerCollectRejectsOutOfScopeWrite: a change outside allowed_scope is
// rejected at collection with the offending path named.
func TestWorkerCollectRejectsOutOfScopeWrite(t *testing.T) {
	fakeClaudePath(t, "outside")
	t.Setenv("FAKE_CLAUDE_SECONDS", "1")
	repo := fakeRepo(t)

	spawnAndWaitExited(t, repo, "T0001")
	code, out, errOut := collectCLI(t, repo, "T0001")
	if code != 1 {
		t.Fatalf("collect with an out-of-scope write: exit %d, want 1\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	for _, want := range []string{"collect rejected", "[FAIL] scope", "docs/evil.txt"} {
		if !strings.Contains(out, want) {
			t.Errorf("collect output missing %q:\n%s", want, out)
		}
	}
	if got := stateOf(t, repo, "T0001"); got != "rejected" {
		t.Errorf("task state = %s, want rejected", got)
	}
}

// TestWorkerCollectRejectsResidue: a process the Worker started and did not
// stop (the T0007 PostgreSQL lesson) is found in the Worker's session and
// rejects the collection; the clean worker (happy path) passes.
func TestWorkerCollectRejectsResidue(t *testing.T) {
	fakeClaudePath(t, "residue")
	t.Setenv("FAKE_CLAUDE_SECONDS", "1")
	t.Setenv("FAKE_CLAUDE_RESIDUE_SECONDS", "300")
	repo := fakeRepo(t)
	killPidFromFile(t, repo, "T0001", "residue.pid")

	spawnAndWaitExited(t, repo, "T0001")
	code, out, errOut := collectCLI(t, repo, "T0001")
	if code != 1 {
		t.Fatalf("collect with residue: exit %d, want 1\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	for _, want := range []string{"collect rejected", "[FAIL] residue", "sleep 300", "still running"} {
		if !strings.Contains(out, want) {
			t.Errorf("collect output missing %q:\n%s", want, out)
		}
	}
	if got := stateOf(t, repo, "T0001"); got != "rejected" {
		t.Errorf("task state = %s, want rejected", got)
	}
}

// TestWorkerCollectSurfacesDaemonizedListener: a service that daemonizes out
// of the Worker's session keeps its listener; collect cannot prove the
// session link but surfaces the listener with its owner (started during the
// run window). A listener that existed at spawn is never flagged — the
// two-sided proof the baseline comparison does not over-block.
func TestWorkerCollectSurfacesDaemonizedListener(t *testing.T) {
	fakeClaudePath(t, "listener")
	t.Setenv("FAKE_CLAUDE_SECONDS", "1")
	t.Setenv("FAKE_CLAUDE_PORT", "18981")
	repo := fakeRepo(t)
	killPidFromFile(t, repo, "T0001", "listener.pid")

	// a pre-existing listener (started before spawn) must stay unflagged
	pre := exec.Command("python3", "-m", "http.server", "18982", "--bind", "127.0.0.1")
	if err := pre.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pre.Process.Kill() })
	time.Sleep(300 * time.Millisecond) // let the pre-existing server bind

	spawnAndWaitExited(t, repo, "T0001")
	code, out, errOut := collectCLI(t, repo, "T0001")
	if code != 0 {
		t.Fatalf("collect with a daemonized listener: exit %d, want 0 (surfaced, not rejected)\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if !strings.Contains(out, "collect ok") {
		t.Errorf("collect output missing clean verdict:\n%s", out)
	}
	if !strings.Contains(out, "listener appeared during the run") || !strings.Contains(out, "18981") {
		t.Errorf("daemonized listener not surfaced:\n%s", out)
	}
	if strings.Contains(out, "18982") {
		t.Errorf("pre-existing listener 18982 was flagged — the spawn baseline comparison over-blocks:\n%s", out)
	}
}

// TestWorkerCollectFailedWorker: a Worker that exits nonzero is collected as
// failed and moves to worker_failed — a crash can never read as a completed
// task.
func TestWorkerCollectFailedWorker(t *testing.T) {
	fakeClaudePath(t, "crash")
	t.Setenv("FAKE_CLAUDE_SECONDS", "1")
	repo := fakeRepo(t)

	spawnAndWaitExited(t, repo, "T0001")
	code, out, errOut := collectCLI(t, repo, "T0001")
	if code != 1 {
		t.Fatalf("collect of a crashed Worker: exit %d, want 1\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if !strings.Contains(out, "collect failed") || !strings.Contains(out, "worker_failed") || !strings.Contains(out, "exit status 3") {
		t.Errorf("collect output missing the failed verdict:\n%s", out)
	}
	if got := stateOf(t, repo, "T0001"); got != "worker_failed" {
		t.Errorf("task state = %s, want worker_failed", got)
	}
}

// TestWorkerCollectRefusesUnfinishedRun: a still-running Worker has no
// recorded exit; collect refuses to judge it (and never marks it completed).
func TestWorkerCollectRefusesUnfinishedRun(t *testing.T) {
	fakeClaudePath(t, "sleep")
	t.Setenv("FAKE_CLAUDE_SECONDS", "30")
	repo := fakeRepo(t)

	code, out, errOut := runWorkerCLI(t, repo, "worker", "spawn", "T0001")
	if code != 0 {
		t.Fatalf("spawn: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	t.Cleanup(func() { stopAll(t, repo) })

	code, out, errOut = collectCLI(t, repo, "T0001")
	if code != 1 {
		t.Fatalf("collect of a running Worker: exit %d, want 1\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if !strings.Contains(errOut, "no recorded exit status") {
		t.Errorf("collect error missing the unfinished-run refusal:\n%s", errOut)
	}
	if got := stateOf(t, repo, "T0001"); got != "running" {
		t.Errorf("task state = %s, want running (collect must not judge an unfinished run)", got)
	}
}
