package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	// Both ports are chosen from what the kernel says is free. They used to be
	// 18981 and 18982, and a constant here is a claim about the whole machine:
	// this fixture's own product is a listener that escapes its session, so a
	// previous run that leaked one — or any other process that took the port —
	// leaves the next run's fixture unable to bind, and the test then fails on
	// "daemonized listener not surfaced" for a reason that has nothing to do
	// with what it asserts. Measured: a `python3 -m http.server 18982` left by
	// an earlier run of this very test was still holding the port.
	ports := freePorts(t, 2)
	port, prePort := ports[0], ports[1]
	t.Setenv("FAKE_CLAUDE_PORT", strconv.Itoa(port))
	repo := fakeRepo(t)
	killPidFromFile(t, repo, "T0001", "listener.pid")

	// a pre-existing listener (started before spawn) must stay unflagged
	pre := exec.Command("python3", "-m", "http.server", strconv.Itoa(prePort), "--bind", "127.0.0.1")
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
	// The whole address, not the number: `:1898` is a prefix of `:18981`, so a
	// one-number check can be satisfied by the other listener's port. The form
	// is what /proc/net/tcp prints — loopback in hex, the port in decimal.
	if !strings.Contains(out, "listener appeared during the run") || !strings.Contains(out, "0100007F:"+strconv.Itoa(port)) {
		t.Errorf("daemonized listener not surfaced:\n%s", out)
	}
	if strings.Contains(out, "0100007F:"+strconv.Itoa(prePort)) {
		t.Errorf("the pre-existing listener %d was flagged — the spawn baseline comparison over-blocks:\n%s", prePort, out)
	}
}

// freePorts asks the kernel for n ports nothing is holding, for a test that
// needs a real listener on a number it can name to the fixture and then look
// for in the output. Binding :0 and closing is the only way to ask; the port is
// free when it is asked for, which is what these tests need and what a constant
// does not give them.
//
// The n sockets are held at once and closed together, so the returned ports are
// pairwise distinct. Asking twice in sequence could return a number the kernel
// had just handed back, and this fixture needs two listeners that are not each
// other: the pre-existing one must not be sitting on the port the Worker's
// daemonized listener is about to claim.
func freePorts(t *testing.T, n int) []int {
	t.Helper()
	held := make([]net.Listener, 0, n)
	defer func() {
		for _, l := range held {
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}()
	ports := make([]int, 0, n)
	for range n {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, l)
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	return ports
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

// gitIn runs git in dir and fails the test on error (control-plane setup for
// the refs scenarios below).
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A ref minted under the Supervisor's own commit identity is still a finding.
//
// This is the regression for the fail-open rule: collect used to exempt a new
// ref whose tip commit carried the repository's configured author email. That
// email is a field the committer sets — `git -c user.email=…` — so a Worker
// that got past the guard could launder a ref with one flag. Attribution is now
// the Supervisor's own record, and nothing about the commit's identity can
// enter it.
func TestWorkerCollectRejectsARefCarryingTheSupervisorsIdentity(t *testing.T) {
	fakeClaudePath(t, "write-scope")
	t.Setenv("FAKE_CLAUDE_SECONDS", "1")
	repo := fakeRepo(t)

	spawnAndWaitExited(t, repo, "T0001")

	// The Supervisor's identity, as the old rule read it, then a ref whose
	// commit claims exactly that identity — made the way a Worker would forge
	// it, per-invocation rather than through git config.
	gitIn(t, repo, "config", "user.email", "sup@post.local")
	gitIn(t, repo, "config", "user.name", "Supervisor")
	gitIn(t, repo, "checkout", "-q", "-b", "feat/plausible")
	gitIn(t, repo, "-c", "user.email=sup@post.local", "-c", "user.name=Supervisor",
		"commit", "-q", "--allow-empty", "-m", "forged identity")
	gitIn(t, repo, "checkout", "-q", "-")

	// The fixture must actually produce the forgery under test: if the ref's
	// author is not the configured identity, this test would pass for the wrong
	// reason and stop testing anything.
	forged := gitIn(t, repo, "log", "-1", "--format=%ae", "feat/plausible")
	if configured := gitIn(t, repo, "config", "user.email"); forged != configured {
		t.Fatalf("fixture: the planted ref's author is %q but the configured identity is %q — no forgery to defeat", forged, configured)
	}

	code, out, errOut := collectCLI(t, repo, "T0001")
	if code != 1 {
		t.Fatalf("collect with a forged-identity ref: exit %d, want 1\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	for _, want := range []string{"collect rejected", "[FAIL] refs", "feat/plausible"} {
		if !strings.Contains(out, want) {
			t.Errorf("collect output missing %q:\n%s", want, out)
		}
	}
	if got := stateOf(t, repo, "T0001"); got != "rejected" {
		t.Errorf("task state = %s, want rejected", got)
	}
}

// The legitimate neighbour of the test above, and the real incident it came
// from: the Supervisor opens a branch while a Worker runs (T0201 was rejected
// because PR #96's branch appeared mid-run). Recording it — `rddev refs adopt`
// — attributes it without widening the check.
func TestWorkerCollectAcceptsASupervisorRecordedRef(t *testing.T) {
	fakeClaudePath(t, "write-scope")
	t.Setenv("FAKE_CLAUDE_SECONDS", "1")
	repo := fakeRepo(t)

	spawnAndWaitExited(t, repo, "T0001")

	gitIn(t, repo, "branch", "feat/rebaseline")
	code, out, errOut := runWorkerCLI(t, repo, "refs", "adopt", "feat/rebaseline")
	if code != 0 {
		t.Fatalf("refs adopt: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if !strings.Contains(out, "feat/rebaseline") {
		t.Errorf("adopt did not report the ref it recorded:\n%s", out)
	}

	code, out, errOut = collectCLI(t, repo, "T0001")
	if code != 0 {
		t.Fatalf("collect after adopting the Supervisor's own branch: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if !strings.Contains(out, "[ok] refs") {
		t.Errorf("collect output missing the refs pass:\n%s", out)
	}
	if got := stateOf(t, repo, "T0001"); got != "verification" {
		t.Errorf("task state = %s, want verification", got)
	}

	// `rddev refs list` shows what the ledger holds.
	code, out, errOut = runWorkerCLI(t, repo, "refs", "list", "--json")
	if code != 0 {
		t.Fatalf("refs list: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	var doc struct {
		Refs []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"refs"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("refs list --json is not JSON: %v\n%s", err, out)
	}
	var sawAdopted, sawSpawn bool
	for _, r := range doc.Refs {
		switch {
		case r.Name == "refs/heads/feat/rebaseline" && r.Source == "adopt":
			sawAdopted = true
		case strings.HasPrefix(r.Name, "refs/heads/task/T0001-") && r.Source == "spawn":
			sawSpawn = true
		}
	}
	if !sawAdopted {
		t.Errorf("the adopted ref is not in the ledger: %s", out)
	}
	if !sawSpawn {
		t.Errorf("spawn did not record the task branch — a sibling Worker would see it as a new ref: %s", out)
	}
}
