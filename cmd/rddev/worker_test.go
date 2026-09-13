package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/devorchestrator"
)

// These tests exercise the real spawn pipeline end to end against a fake git
// repository and a fake claude binary: worktrees are genuinely created with
// git, the Worker is a genuine separate process started through the reaper
// wrapper, and discovery is always from disk. The only substitutions are the
// claude CLI itself (a fixture script) and the repo (a temp git init).

const fakeClaude = `#!/bin/sh
# fake claude: --version prints the version line; the worker run is driven by
# FAKE_CLAUDE_MODE (write|sleep|crash|write-scope|outside|residue|listener)
# inherited from the spawn environment.
write_result() {
	# $1 = files_changed JSON array; writes a schema-conforming RESULT.json
	cat > "$POST_WORKER_RESULT_DIR/RESULT.json" <<EOF
{"task_id":"$POST_WORKER_TASK_ID","status":"completed","summary":"fake worker","files_changed":$1,"tests":[{"command":"t1","status":"passed","evidence":"fake"}],"acceptance":[{"criterion":"a1","status":"passed","evidence":"fake"}],"risks":[],"follow_up_issues":[],"notes_for_supervisor":""}
EOF
}
case "$1" in
	--version) echo "2.1.269 (Claude Code)"; exit 0 ;;
esac
mode=${FAKE_CLAUDE_MODE:-write}
case "$mode" in
	crash) sleep "${FAKE_CLAUDE_SECONDS:-1}"; exit 3 ;;
	sleep) exec sleep "${FAKE_CLAUDE_SECONDS:-30}" ;;
	write)
		# prove which worktree this process ran in: marker named by task id
		echo "worker $POST_WORKER_TASK_ID at $(pwd)" > "worker-$POST_WORKER_TASK_ID.txt"
		exec sleep "${FAKE_CLAUDE_SECONDS:-3}" ;;
	write-scope)
		# the T0011 happy path: an in-scope change plus a conforming RESULT
		dir=${FAKE_CLAUDE_SCOPE_DIR:-t0001}
		mkdir -p "$dir"
		echo "worker marker" > "$dir/marker.txt"
		write_result "[\"$dir/marker.txt\"]"
		exec sleep "${FAKE_CLAUDE_SECONDS:-3}" ;;
	outside)
		# the T0011 scope violation: a change outside allowed_scope
		mkdir -p docs
		echo "out of scope" > docs/evil.txt
		write_result '["docs/evil.txt"]'
		exec sleep "${FAKE_CLAUDE_SECONDS:-3}" ;;
	residue)
		# leaves a background process running (the T0007 lesson). Real
		# claude's Bash tool starts each command in its own session, so the
		# survivor escapes the reaper session exactly like this setsid does
		# (T0011 Defect 2); the run marker in its environment is what collect
		# must find. The child writes its own pid because setsid forks.
		write_result '[]'
		setsid sh -c 'echo $$ > "$1"; exec sleep "${2:-300}"' sh "$POST_WORKER_RESULT_DIR/residue.pid" "${FAKE_CLAUDE_RESIDUE_SECONDS:-300}" >/dev/null 2>&1 &
		exec sleep "${FAKE_CLAUDE_SECONDS:-2}" ;;
	listener)
		# leaves an environment-scrubbing daemon listening: it escapes both
		# the session (setsid) and the run marker (env -i) — the warn-only
		# class collect surfaces but does not reject.
		#
		# FAKE_CLAUDE_PORT is required, not defaulted: this test used to hardcode
		# 18981 here, and a default would quietly restore it for any caller that
		# forgets — which is the whole defect, in one line. There is no sensible
		# default for "a port nothing else is using".
		write_result '[]'
		env -i PATH=/usr/bin:/bin setsid sh -c 'echo $$ > "$1"; exec python3 -m http.server "${2:?the listener fixture needs a port}" --bind 127.0.0.1' sh "$POST_WORKER_RESULT_DIR/listener.pid" "${FAKE_CLAUDE_PORT:?the listener fixture needs FAKE_CLAUDE_PORT}" >/dev/null 2>&1 &
		exec sleep "${FAKE_CLAUDE_SECONDS:-2}" ;;
esac
`

// fakeRepo builds a git repo with a three-task DAG, all tasks ready, and the
// real task-package schema copied in (spawn validates against the repo's own
// schema — no embedded copy).
func fakeRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# fake repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// T0012: spawn validates allowed_scope against the real tree and refuses a
	// glob matching nothing — the fixture scopes (t0001/** ...) must match
	// real files, like the tasks they model.
	for _, id := range []string{"T0001", "T0002", "T0003"} {
		dir := filepath.Join(repo, strings.ToLower(id))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".keep"), []byte(id+" scope\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "-A")
	git("-c", "user.name=test", "-c", "user.email=test@test", "commit", "-q", "-m", "initial")
	if err := os.MkdirAll(filepath.Join(repo, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "specs", "orchestrator"), 0o755); err != nil {
		t.Fatal(err)
	}
	schema, err := os.ReadFile(filepath.Join("..", "..", "specs", "orchestrator", "task-package.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "specs", "orchestrator", "task-package.schema.json"), schema, 0o644); err != nil {
		t.Fatal(err)
	}
	// spawn derives --json-schema from the repo's own result schema (T0011)
	// and collect validates RESULT.json against it — the fixture must carry
	// the real schema.
	resultSchema, err := os.ReadFile(filepath.Join("..", "..", "specs", "orchestrator", "worker-result.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "specs", "orchestrator", "worker-result.schema.json"), resultSchema, 0o644); err != nil {
		t.Fatal(err)
	}
	tasks := make([]map[string]any, 0, 3)
	status := map[string]any{"version": 2, "tasks": map[string]any{}}
	for _, id := range []string{"T0001", "T0002", "T0003"} {
		tasks = append(tasks, map[string]any{
			"id": id, "phase": "P0", "title": "Fake task " + id,
			"requirements": []string{"r1"}, "acceptance_criteria": []string{"a1"},
			"tests": []string{"t1"}, "allowed_scope": []string{strings.ToLower(id) + "/**"},
			"forbidden_scope": []string{}, "decision_level_max": "L1",
		})
		status["tasks"].(map[string]any)[id] = map[string]any{"status": "ready", "history": []any{}}
	}
	dagJSON, _ := json.MarshalIndent(map[string]any{"tasks": tasks}, "", "  ")
	if err := os.WriteFile(filepath.Join(repo, "tasks", "tasks.json"), dagJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	statusJSON, _ := json.MarshalIndent(status, "", "  ")
	if err := os.WriteFile(filepath.Join(repo, "tasks", "task_status.json"), statusJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

// fakeClaudePath writes the fake claude script and prepends its dir to PATH.
func fakeClaudePath(t *testing.T, mode string) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(fakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLAUDE_MODE", mode)
}

// runWorkerCLI invokes the CLI in-process from the fixture repo.
func runWorkerCLI(t *testing.T, repo string, args ...string) (int, string, string) {
	t.Helper()
	t.Chdir(repo)
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// stopAll terminates every still-live worker (test hygiene; the workers were
// started detached, so they would otherwise outlive the test process).
func stopAll(t *testing.T, repo string) {
	t.Helper()
	views, err := devorchestrator.DiscoverWorkers(repo)
	if err != nil {
		return
	}
	for _, v := range views {
		if v.Status == devorchestrator.WorkerRunning {
			_ = devorchestrator.StopWorker(repo, &v.Record)
		}
	}
}

// waitFor polls fn until it returns true or the deadline passes.
func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func listStatus(repo, taskID string) (string, *int) {
	var stdout bytes.Buffer
	code := run([]string{"worker", "list", "--json"}, &stdout, &bytes.Buffer{})
	if code != 0 {
		return "list-failed", nil
	}
	var doc struct {
		Workers []struct {
			TaskID     string `json:"task_id"`
			Status     string `json:"status"`
			PID        int    `json:"pid"`
			ExitStatus *int   `json:"exit_status"`
		} `json:"workers"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		return "unmarshal-failed", nil
	}
	for _, w := range doc.Workers {
		if w.TaskID == taskID {
			return w.Status, w.ExitStatus
		}
	}
	return "absent", nil
}

// TestWorkerSpawnTwoConcurrentWorkers: two Workers for different tasks run
// concurrently as separate processes in separate worktrees; each writes only
// into its own worktree (no cross-pollution), and both state records flip to
// running with a worker_run_id.
func TestWorkerSpawnTwoConcurrentWorkers(t *testing.T) {
	fakeClaudePath(t, "write")
	t.Setenv("FAKE_CLAUDE_SECONDS", "4")
	repo := fakeRepo(t)

	code, out, errOut := runWorkerCLI(t, repo, "worker", "spawn", "T0001")
	if code != 0 {
		t.Fatalf("spawn T0001: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	code, out, errOut = runWorkerCLI(t, repo, "worker", "spawn", "T0002")
	if code != 0 {
		t.Fatalf("spawn T0002: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}

	// both live at once, derived from disk
	waitFor(t, "both workers live", func() bool {
		s1, _ := listStatus(repo, "T0001")
		s2, _ := listStatus(repo, "T0002")
		return s1 == "running" && s2 == "running"
	})

	// two separate processes in two separate worktrees
	recs := map[string]map[string]any{}
	for _, id := range []string{"T0001", "T0002"} {
		recs[id] = readRegistry(t, repo, id)
	}
	if recs["T0001"]["pid"] == recs["T0002"]["pid"] {
		t.Error("both Workers share one pid — they are not separate processes")
	}
	if recs["T0001"]["worktree"] == recs["T0002"]["worktree"] {
		t.Error("both Workers share one worktree — worktrees must never be shared")
	}
	if recs["T0001"]["session_id"] == recs["T0002"]["session_id"] {
		t.Error("both Workers share one session id")
	}
	if recs["T0001"]["run_id"] == recs["T0002"]["run_id"] {
		t.Error("both Workers share one run id")
	}
	// registry facts required by L1-20260912-5
	if recs["T0001"]["claude_version"] != "2.1.269 (Claude Code)" {
		t.Errorf("claude_version = %v", recs["T0001"]["claude_version"])
	}
	if recs["T0001"]["branch"] == "" || recs["T0001"]["baseline_sha"] == "" {
		t.Errorf("registry missing branch/baseline: %v", recs["T0001"])
	}

	// both state entries went ready -> running with a worker_run_id
	for _, id := range []string{"T0001", "T0002"} {
		data, err := os.ReadFile(filepath.Join(repo, "tasks", "task_status.json"))
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Tasks map[string]struct {
				Status      string `json:"status"`
				WorkerRunID string `json:"worker_run_id"`
				StartedAt   string `json:"started_at"`
			} `json:"tasks"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Tasks[id].Status != "running" || doc.Tasks[id].WorkerRunID == "" || doc.Tasks[id].StartedAt == "" {
			t.Errorf("task %s state = %+v, want running with worker_run_id and started_at", id, doc.Tasks[id])
		}
	}

	// wait for both to exit cleanly; exit 0 recorded for each
	waitFor(t, "both workers exited", func() bool {
		s1, _ := listStatus(repo, "T0001")
		s2, _ := listStatus(repo, "T0002")
		return s1 == "exited" && s2 == "exited"
	})
	_, e1 := listStatus(repo, "T0001")
	_, e2 := listStatus(repo, "T0002")
	if e1 == nil || *e1 != 0 || e2 == nil || *e2 != 0 {
		t.Errorf("exit statuses = %v/%v, want 0/0", e1, e2)
	}

	// cross-pollution proof: each marker exists only in its own worktree
	for _, id := range []string{"T0001", "T0002"} {
		marker := "worker-" + id + ".txt"
		own := filepath.Join(repo, ".rddev", "worktrees", id, marker)
		if _, err := os.Stat(own); err != nil {
			t.Errorf("%s did not write its marker into its own worktree: %v", id, err)
		}
		for _, other := range []string{"T0001", "T0002"} {
			if other == id {
				continue
			}
			stray := filepath.Join(repo, ".rddev", "worktrees", other, marker)
			if _, err := os.Stat(stray); err == nil {
				t.Errorf("%s's marker appeared in %s's worktree — cross-pollution", id, other)
			}
		}
	}
}

// TestWorkerCrashRecordedNotCompleted: a crashing Worker is recorded as
// exited with its real exit code, the task stays running (collect/T0011 owns
// the failure verdict), and the parallelism slot frees for the next spawn.
func TestWorkerCrashRecordedNotCompleted(t *testing.T) {
	fakeClaudePath(t, "crash")
	repo := fakeRepo(t)
	// The second spawn below is still running when the test returns, and
	// `t.TempDir` removes the directory on the way out. The two race: the
	// Worker writes under .rddev/workers/T0002 while RemoveAll walks it, and
	// the run dies in cleanup rather than in an assertion —
	//
	//   --- FAIL: TestWorkerCrashRecordedNotCompleted (6.38s)
	//       testing.go:1617: TempDir RemoveAll cleanup: unlinkat
	//       /tmp/TestWorkerCrashRecordedNotCompleted…/.rddev/workers/T0002:
	//       directory not empty
	//
	// measured once in 40 runs of this test alone and once in 3 full-package
	// runs. It was first reported as a pre-existing flake — not caused by the
	// pull request it reddened — by the independent adversarial review of
	// #109 (verdict of 2026-09-13, delivered to the Supervisor as a review
	// rather than posted to the forge), which measured it as load-sensitive
	// and recommended merging that pull request and filing this separately.
	// Stop the Workers first: Cleanup is LIFO, and the cleanup that removes
	// the whole tree is registered by the *first* t.TempDir() call in the
	// test — fakeClaudePath's, not fakeRepo's — so this runs before it.
	t.Cleanup(func() { stopAll(t, repo) })

	code, out, errOut := runWorkerCLI(t, repo, "worker", "spawn", "T0001")
	if code != 0 {
		t.Fatalf("spawn: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	waitFor(t, "crashed worker recorded", func() bool {
		s, _ := listStatus(repo, "T0001")
		return s == "exited"
	})
	_, exitCode := listStatus(repo, "T0001")
	if exitCode == nil || *exitCode != 3 {
		t.Fatalf("exit status = %v, want 3 (the crash code, from exit.status)", exitCode)
	}
	// never "completed": the task state must NOT have moved past running
	data, err := os.ReadFile(filepath.Join(repo, "tasks", "task_status.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"status": "running"`) {
		t.Errorf("task status moved after a crash:\n%s", data)
	}
	// the slot is free: the next spawn succeeds even at --parallel 1
	code, out, errOut = runWorkerCLI(t, repo, "worker", "spawn", "T0002", "--parallel", "1")
	if code != 0 {
		t.Fatalf("spawn after crash blocked: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
}

// TestWorkerTimeoutRecordedNotCompleted: a Worker that outlives --timeout is
// killed by the timeout wrapper; the reaper records 124 — never "completed".
func TestWorkerTimeoutRecordedNotCompleted(t *testing.T) {
	fakeClaudePath(t, "sleep")
	t.Setenv("FAKE_CLAUDE_SECONDS", "60")
	repo := fakeRepo(t)

	code, out, errOut := runWorkerCLI(t, repo, "worker", "spawn", "T0001", "--timeout", "1s")
	if code != 0 {
		t.Fatalf("spawn: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	waitFor(t, "timed-out worker recorded", func() bool {
		s, _ := listStatus(repo, "T0001")
		return s == "exited"
	})
	_, exitCode := listStatus(repo, "T0001")
	if exitCode == nil || *exitCode != 124 {
		t.Fatalf("exit status = %v, want 124 (timeout)", exitCode)
	}
}

// TestWorkerParallelismLimit: the disk-derived running count gates spawns at
// the limit (default 3) and the hard maximum of 4 is refused up front; a
// refused spawn starts no new Worker.
func TestWorkerParallelismLimit(t *testing.T) {
	fakeClaudePath(t, "sleep")
	t.Setenv("FAKE_CLAUDE_SECONDS", "30")
	repo := fakeRepo(t)
	t.Cleanup(func() { stopAll(t, repo) })

	// hard max 4 is refused before anything else runs
	code, _, errOut := runWorkerCLI(t, repo, "worker", "spawn", "T0001", "--parallel", "5")
	if code != 1 || !strings.Contains(errOut, "hard maximum") {
		t.Fatalf("--parallel 5: exit %d, want refusal; stderr: %s", code, errOut)
	}

	// three sleepers fill the default limit
	for _, id := range []string{"T0001", "T0002", "T0003"} {
		code, _, errOut = runWorkerCLI(t, repo, "worker", "spawn", id)
		if code != 0 {
			t.Fatalf("spawn %s: exit %d: %s", id, code, errOut)
		}
	}
	before := readRegistry(t, repo, "T0001")

	// a fourth spawn is refused while three are running
	code, _, errOut = runWorkerCLI(t, repo, "worker", "spawn", "T0001", "--parallel", "2")
	if code != 1 || !strings.Contains(errOut, "parallelism limit") {
		t.Fatalf("spawn over the limit: exit %d, want refusal; stderr: %s", code, errOut)
	}
	after := readRegistry(t, repo, "T0001")
	if before["run_id"] != after["run_id"] {
		t.Error("a refused spawn replaced the registry — it must start nothing")
	}
}

// TestWorkerRestartDiscoveryAndStale: a fresh process (this test) re-derives
// running/stale purely from disk — the Supervisor-restart scenario — and a
// stale Worker is never reported as running or completed.
func TestWorkerRestartDiscoveryAndStale(t *testing.T) {
	fakeClaudePath(t, "sleep")
	t.Setenv("FAKE_CLAUDE_SECONDS", "30")
	repo := fakeRepo(t)
	t.Cleanup(func() { stopAll(t, repo) })

	code, _, errOut := runWorkerCLI(t, repo, "worker", "spawn", "T0001")
	if code != 0 {
		t.Fatalf("spawn: exit %d: %s", code, errOut)
	}
	waitFor(t, "worker live", func() bool {
		s, _ := listStatus(repo, "T0001")
		return s == "running"
	})

	// separate-process proof: the worker's parent is the reaper wrapper, not
	// this CLI process — a subagent would be a child of the caller
	rec := readRegistry(t, repo, "T0001")
	pid := int(rec["pid"].(float64))
	ppid := readStatField(t, pid, 4)
	if ppid == os.Getpid() {
		t.Error("worker is a child of the CLI process — it must be a separate, independent process (not a subagent)")
	}

	// a dead pid with no exit.status reads as stale, never running/completed
	if err := os.MkdirAll(filepath.Join(repo, ".rddev", "workers", "T0009"), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := map[string]any{
		"task_id": "T0009", "run_id": "run-stale", "session_id": "s",
		"claude_version": "v", "pid": 999999, "start_time": 1,
		"worktree": "/w", "branch": "b", "baseline_sha": "0123456789abcdef",
		"refs_before": []any{}, "log_path": "/l", "result_dir": "/r", "started_at": "t",
	}
	staleJSON, _ := json.Marshal(stale)
	if err := os.WriteFile(filepath.Join(repo, ".rddev", "workers", "T0009", "registry.json"), staleJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	s, _ := listStatus(repo, "T0009")
	if s != "stale" {
		t.Errorf("dead pid without exit.status read as %q, want stale", s)
	}
	s1, _ := listStatus(repo, "T0001")
	if s1 != "running" {
		t.Errorf("live worker read as %q after restart-style discovery, want running", s1)
	}
}

// TestWorkerStop: stop terminates the Worker, records its exit, and never
// marks the task completed.
func TestWorkerStop(t *testing.T) {
	fakeClaudePath(t, "sleep")
	t.Setenv("FAKE_CLAUDE_SECONDS", "60")
	repo := fakeRepo(t)
	t.Cleanup(func() { stopAll(t, repo) })

	code, _, errOut := runWorkerCLI(t, repo, "worker", "spawn", "T0001")
	if code != 0 {
		t.Fatalf("spawn: exit %d: %s", code, errOut)
	}
	waitFor(t, "worker live", func() bool {
		s, _ := listStatus(repo, "T0001")
		return s == "running"
	})
	code, out, errOut := runWorkerCLI(t, repo, "worker", "stop", "T0001")
	if code != 0 {
		t.Fatalf("stop: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if !strings.Contains(out, "stopped") {
		t.Errorf("stop output = %q, want a stopped report", out)
	}
	waitFor(t, "stopped worker recorded", func() bool {
		s, _ := listStatus(repo, "T0001")
		return s == "exited"
	})
	_, exitCode := listStatus(repo, "T0001")
	if exitCode == nil || (*exitCode != 143 && *exitCode != 137 && *exitCode != -1) {
		t.Errorf("exit status = %v, want a signal code (143/137) recorded", exitCode)
	}
	data, err := os.ReadFile(filepath.Join(repo, "tasks", "task_status.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"status": "running"`) {
		t.Errorf("stop moved the task state:\n%s", data)
	}
}

// TestWorkerSpawnErrors: usage and operational failures are loud, and spawn
// without claude on PATH fails honestly.
func TestWorkerSpawnErrors(t *testing.T) {
	repo := fakeRepo(t)
	code, _, errOut := runWorkerCLI(t, repo, "worker", "spawn")
	if code != 2 || !strings.Contains(errOut, "missing TASK") {
		t.Errorf("spawn without TASK: exit %d, want 2; stderr: %s", code, errOut)
	}
	code, _, errOut = runWorkerCLI(t, repo, "worker", "spawn", "T0999")
	if code != 1 || !strings.Contains(errOut, "unknown task") {
		t.Errorf("spawn unknown task: exit %d, want 1; stderr: %s", code, errOut)
	}
	code, _, errOut = runWorkerCLI(t, repo, "worker", "spawn", "T0001", "--max-budget-usd", "cheap")
	if code != 2 || !strings.Contains(errOut, "invalid --max-budget-usd") {
		t.Errorf("spawn bad budget: exit %d, want 2; stderr: %s", code, errOut)
	}
	// no claude anywhere on PATH (fixture not installed): honest failure
	t.Setenv("PATH", "/nonexistent")
	code, _, errOut = runWorkerCLI(t, repo, "worker", "spawn", "T0001")
	if code != 1 || !strings.Contains(errOut, "claude CLI not found") {
		t.Errorf("spawn without claude: exit %d, want 1 with explicit message; stderr: %s", code, errOut)
	}
}

func readRegistry(t *testing.T, repo, taskID string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repo, ".rddev", "workers", taskID, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// readStatField reads /proc/<pid>/stat field n (n >= 3: field 3 is state,
// field 4 ppid, field 22 starttime — the comm in parens may contain spaces).
func readStatField(t *testing.T, pid, n int) int {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	rest := string(data)
	if i := strings.LastIndexByte(rest, ')'); i >= 0 {
		rest = rest[i+1:]
	}
	fields := strings.Fields(rest)
	if len(fields) < n-2 {
		t.Fatalf("/proc/%d/stat has %d fields after the command", pid, len(fields))
	}
	var v int
	fmt.Sscanf(fields[n-3], "%d", &v)
	return v
}

// TestWorkerGuardFilesGeneratedWithIsolation: the spawned task dir carries
// the guard layer, and the generated guard blocks control-plane and
// credential probes while allowing legitimate neighbours — the two-sided
// contract in situ.
func TestWorkerGuardFilesGeneratedWithIsolation(t *testing.T) {
	fakeClaudePath(t, "write")
	t.Setenv("FAKE_CLAUDE_SECONDS", "2")
	repo := fakeRepo(t)
	// The Worker spawned below sleeps 2s and this test returns in about one,
	// so it is still running when `t.TempDir` removes the tree — the same race
	// TestWorkerCrashRecordedNotCompleted was fixed for: the reaper writes
	// exit.status under .rddev/runtime/tasks/T0001 after RemoveAll has walked
	// past, which leaks a tree in $TMPDIR and fails the run when the write
	// lands mid-walk. Measured before this: 3 leftover trees in 5 isolated
	// runs; a reviewer measuring under heavier load saw 5 in 5, so the count
	// is load-dependent and the race's presence, not its rate, is the point.
	// Every assertion in this test is made before teardown, so stopping the
	// Worker here changes no verdict.
	t.Cleanup(func() { stopAll(t, repo) })

	code, _, errOut := runWorkerCLI(t, repo, "worker", "spawn", "T0001")
	if code != 0 {
		t.Fatalf("spawn: exit %d: %s", code, errOut)
	}
	guard := filepath.Join(repo, ".rddev", "workers", "T0001", "guard", "worker-guard.sh")
	if _, err := os.Stat(guard); err != nil {
		t.Fatalf("generated guard missing: %v", err)
	}
	// two-sided probes through the generated guard with the spawn contract env
	env := []string{
		"POST_REPO_ROOT=" + repo,
		"POST_WORKER_TASK_ID=T0001",
		"POST_WORKER_WORKTREE=" + filepath.Join(repo, ".rddev", "worktrees", "T0001"),
		"POST_WORKER_RESULT_DIR=" + filepath.Join(repo, ".rddev", "workers", "T0001"),
	}
	probe := func(name, jsonText string, wantBlock bool) {
		t.Helper()
		cmd := exec.Command("sh", guard)
		cmd.Env = append(os.Environ(), env...)
		cmd.Stdin = strings.NewReader(jsonText)
		err := cmd.Run()
		blocked := false
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 2 {
			blocked = true
		}
		if blocked != wantBlock {
			t.Errorf("%s: blocked=%v, want %v", name, blocked, wantBlock)
		}
	}
	probe("control-plane blocked", `{"tool_name":"Bash","tool_input":{"command":"git push origin main"}}`, true)
	probe("neighbour git allowed", `{"tool_name":"Bash","tool_input":{"command":"git status --porcelain"}}`, false)
	probe("words-in-arguments allowed", `{"tool_name":"Bash","tool_input":{"command":"echo \"git commit\" && grep -n push README.md"}}`, false)
	probe("own worktree write allowed", fmt.Sprintf(`{"tool_name":"Bash","tool_input":{"command":"echo hi > %s/probe.txt"}}`, filepath.Join(repo, ".rddev", "worktrees", "T0001")), false)
	probe("device write allowed", `{"tool_name":"Bash","tool_input":{"command":"echo x > /dev/null"}}`, false)
	probe("outside write blocked", `{"tool_name":"Bash","tool_input":{"command":"tee /etc/x"}}`, true)
	probe("own worktree read allowed", `{"tool_name":"Read","tool_input":{"file_path":"`+filepath.Join(repo, ".rddev", "worktrees", "T0001", "README.md")+`"}}`, false)
	probe("credential store read blocked", `{"tool_name":"Read","tool_input":{"file_path":"/root/.git-credentials"}}`, true)
}
