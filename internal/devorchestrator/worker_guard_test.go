package devorchestrator

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestGuardScriptEmbeddedMatchesExample: the .rddev.example copy and the
// embedded canonical guard are byte-identical — the drift check that keeps
// the checked-in reference honest (L1-20260912-8 rule 5).
func TestGuardScriptEmbeddedMatchesExample(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", ".rddev.example", "guard", "worker-guard.sh"))
	if err != nil {
		t.Fatalf("reading .rddev.example copy: %v — regenerate it from the embedded script", err)
	}
	if string(example) != GuardScript() {
		t.Error(".rddev.example/guard/worker-guard.sh differs from the embedded script; copy it from internal/devorchestrator/embed/worker-guard.sh")
	}
}

// TestGuardScriptHasFailClosedContract: the guard must refuse to decide when
// the contract env is missing (regression for the empty-var `/*` admit-all
// hole).
func TestGuardScriptHasFailClosedContract(t *testing.T) {
	for _, want := range []string{
		"refusing to decide a shell write",
		"refusing to decide a file read",
		"pending = 1", // segment reset (second segment command position)
		`*".env"*`,    // glob-pattern .env matching
	} {
		if !strings.Contains(GuardScript(), want) {
			t.Errorf("guard script missing %q", want)
		}
	}
}

// TestWriteGuardFiles: the generated layout is complete and the settings JSON
// wires dontAsk + the deny layer + the PreToolUse hook with an absolute
// guard path.
func TestWriteGuardFiles(t *testing.T) {
	dir := t.TempDir()
	guardPath, err := WriteGuardFiles(dir, GuardOpts{
		RepoRoot: "/repo", TaskID: "T0001",
		Worktree:  "/repo/.rddev/worktrees/T0001",
		ResultDir: "/repo/.rddev/workers/T0001",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"guard/worker-guard.sh", "worker-settings.json", "run-worker.sh"} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("generated %s missing: %v", name, err)
		}
		if name != "worker-settings.json" && fi.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s is not executable", name)
		}
	}
	guard, err := os.ReadFile(filepath.Join(dir, "guard", "worker-guard.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if string(guard) != GuardScript() {
		t.Error("generated guard differs from the embedded script")
	}
	// the hook command must be absolute (the hook runs with the worker cwd)
	var settings map[string]any
	data, err := os.ReadFile(filepath.Join(dir, "worker-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("settings are not valid JSON: %v", err)
	}
	perms := settings["permissions"].(map[string]any)
	if perms["defaultMode"] != "dontAsk" {
		t.Errorf("defaultMode = %v, want dontAsk", perms["defaultMode"])
	}
	hooks := settings["hooks"].(map[string]any)["PreToolUse"].([]any)
	hook := hooks[0].(map[string]any)
	if hook["matcher"] != "Bash|Read|Grep|Glob|NotebookRead" {
		t.Errorf("hook matcher = %v", hook["matcher"])
	}
	cmd := hook["hooks"].([]any)[0].(map[string]any)["command"].(string)
	if !strings.HasPrefix(cmd, "sh /") || !strings.HasSuffix(cmd, "/guard/worker-guard.sh") {
		t.Errorf("hook command %q is not an absolute guard path", cmd)
	}
	if cmd != "sh "+guardPath {
		t.Errorf("hook command %q does not reference the written guard %q", cmd, guardPath)
	}
	deny := perms["deny"].([]any)
	found := map[string]bool{}
	for _, d := range deny {
		found[d.(string)] = true
	}
	for _, want := range []string{"Bash(git commit:*)", "Bash(git push:*)", "Bash(gh:*)", "Bash(sudo:*)"} {
		if !found[want] {
			t.Errorf("deny list missing %q", want)
		}
	}
	// docker is NOT denied at the settings layer: the hook must be able to
	// allow version queries (deny would pre-empt it)
	for d := range found {
		if strings.Contains(d, "docker") {
			t.Errorf("docker must not be in the settings deny list (%s) — the hook owns the version-query exception", d)
		}
	}
	// the Write/Edit allow (L1-20260912-3, dispatch parity): bare entries only.
	// claude 2.1.269 ignores path-scoped Write/Edit allow rules in dontAsk
	// mode (proven by real-claude probes), so a path-scoped entry would be a
	// dead rule that reads as confinement without providing any. The write
	// envelope lives in the guard (shell writes) and collect (worktree diff).
	allow := map[string]bool{}
	for _, a := range perms["allow"].([]any) {
		allow[a.(string)] = true
	}
	for _, want := range []string{"Bash", "Write", "Edit"} {
		if !allow[want] {
			t.Errorf("allow list missing bare %q — the Write tool must work for Workers like it does under the dispatch harness", want)
		}
	}
	for a := range allow {
		if strings.HasPrefix(a, "Write(") || strings.HasPrefix(a, "Edit(") {
			t.Errorf("allow list contains path-scoped %q — claude 2.1.269 does not honor these in dontAsk mode (dead rule)", a)
		}
	}
}

// TestClaudeArgs: the assembled command line carries the session, permission
// mode, budget, optional timeout wrapper, and the inline RESULT schema
// (T0011: the contract is enforced at spawn, not requested in prose).
func TestClaudeArgs(t *testing.T) {
	budget := 1.25
	opts := &SpawnOpts{TaskID: "T0001", MaxBudgetUSD: &budget, Model: "deepseek-v4-pro"}
	args, err := claudeArgs(opts, "sess-1", "/s/worker-settings.json", "/s/system.md", "do the task", `{"type":"object"}`)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"-p do the task", "--session-id sess-1", "--permission-mode dontAsk", "--setting-sources project", "--output-format stream-json", "--verbose", "--max-budget-usd 1.25", "--model deepseek-v4-pro", "--settings /s/worker-settings.json", `--json-schema {"type":"object"}`} {
		if !strings.Contains(joined, want) {
			t.Errorf("claude args missing %q: %v", want, args)
		}
	}
	// the timeout wrapper must surround the claude binary itself — the reaper
	// prepends nothing, so a wrapper inside claudeArgs would be passed to
	// claude as its first argument (regression: `claude timeout ... 1s -p`
	// silently ran without any timeout)
	opts.Timeout = 90 * time.Second
	args, err = claudeArgs(opts, "s", "/s", "/m", "p", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(args, " "), "timeout") {
		t.Errorf("claudeArgs must not carry the wrapper (it becomes a claude argument): %v", args)
	}
	wrapped := wrapTimeout(90*time.Second, "claude", args)
	joined = strings.Join(wrapped, " ")
	for _, want := range []string{"timeout --signal=TERM --kill-after=15 90s claude -p p"} {
		if !strings.Contains(joined, want) {
			t.Errorf("wrapped command missing %q: %v", want, wrapped)
		}
	}
	if wrapped[4] != "claude" {
		t.Errorf("wrapped command[4] = %q, want the claude binary (wrapper must precede it)", wrapped[4])
	}
}

// A rework resumes the Worker's own session; it must NOT also name a session
// id. Real claude 2.1.269 refuses the whole invocation when both are present:
//
//	Error: --session-id can only be used with --continue or --resume if
//	--fork-session is also specified.
//
// Every rework dispatch died at startup with exit 1 and a 125-byte log — the
// path had only ever been exercised against a stubbed claude binary, so the
// flag combination was never seen by the real one.
func TestClaudeArgsDoNotMixResumeWithSessionID(t *testing.T) {
	// Fresh spawn: names the session it creates.
	args, err := claudeArgs(&SpawnOpts{TaskID: "T0001"}, "sess-new", "/s", "/m", "p", "{}")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--session-id sess-new") {
		t.Errorf("a fresh spawn must name its new session: %v", args)
	}
	if strings.Contains(joined, "--resume") {
		t.Errorf("a fresh spawn must not carry --resume: %v", args)
	}

	// Rework: resumes, and says nothing about a session id.
	args, err = claudeArgs(&SpawnOpts{TaskID: "T0001", ResumeSession: "sess-old"}, "sess-old", "/s", "/m", "p", "{}")
	if err != nil {
		t.Fatal(err)
	}
	joined = strings.Join(args, " ")
	if !strings.Contains(joined, "--resume sess-old") {
		t.Errorf("a rework must resume the Worker's session: %v", args)
	}
	if strings.Contains(joined, "--session-id") {
		t.Errorf("a rework passed --session-id alongside --resume; real claude refuses this combination: %v", args)
	}
}

// TestReaperCollectsTheWorkerSession is #166's regression: a Worker that
// leaves a process behind must not leave it RUNNING past its own exit.
//
// rddev gives every Worker its own session on purpose (SysProcAttr{Setsid}
// in spawn) so that "the session" is the unit that can be collected — but
// nothing ever collected it. `worker stop` signals the recorded Worker pid
// (a positive pid signals exactly one process, never its group), and a
// Worker that exits on its own gets no signal at all. So anything it started
// outlived it, silently: for T0206's review that was a runaway probe pinning
// a core for twelve minutes, and after T0305's rebaseline it was a reviewer
// spinning on a cwd that had already been deleted.
//
// The reaper is the one process that outlives the Worker on the normal-exit
// path (it `wait`s for it), so the collection belongs there and not in any
// caller — a caller that has to remember is a caller that will forget.
func TestReaperCollectsTheWorkerSession(t *testing.T) {
	dir := t.TempDir()
	reaper := filepath.Join(dir, "run-worker.sh")
	if err := os.WriteFile(reaper, []byte(reaperScript), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "worker.log")
	pidFile := filepath.Join(dir, "claude.pid")
	statusFile := filepath.Join(dir, "exit.status")
	authStatus := filepath.Join(dir, "authoritative", "exit.status")
	leftoverFile := filepath.Join(dir, "leftover.pid")

	// The #166 shape: the Worker starts something in the background, records
	// its pid, and exits immediately. The leftover is an accident of the
	// Worker's own shell — not a service anyone asked to keep.
	worker := "sleep 300 & echo $! > " + leftoverFile + "; exit 0"

	r := startSetsidChild(t, reaper, dir, logPath, pidFile, statusFile, authStatus, "bash", "-c", worker)
	if err := r.Wait(); err != nil {
		t.Fatalf("the reaper exited non-zero: %v", err)
	}

	// Both copies of the exit status are written BEFORE the collection, so
	// they must be there however the collection turns out: the exit code is
	// the thing collect trusts, and it must never be lost to a cleanup.
	for _, p := range []string{statusFile, authStatus} {
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("exit status %s was not written: %v", p, err)
		}
		if strings.TrimSpace(string(got)) != "0" {
			t.Errorf("exit status %s = %q, want 0", p, strings.TrimSpace(string(got)))
		}
	}

	raw, err := os.ReadFile(leftoverFile)
	if err != nil {
		t.Fatalf("the Worker never recorded its leftover pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("leftover pid %q: %v", strings.TrimSpace(string(raw)), err)
	}
	if !waitProcessNotRunning(pid, 5*time.Second) {
		_ = syscall.Kill(pid, syscall.SIGKILL) // do not leak the evidence
		t.Fatalf("pid %d, started by the Worker, is still running after the reaper exited: the Worker's session was not collected", pid)
	}
}

// TestReaperPidIsTheFirstStdoutLine is the contract spawn parses: rddev reads
// ONE line from the reaper's stdout and Sscanf's it into the Worker pid,
// aborting the spawn when it is not a number (worker_spawn.go). The reaper now
// turns job control on (the `set -m` that gives the Worker its own group for
// the collection above) — and a shell in monitor mode is a shell that talks
// about its jobs. A single such line ahead of the pid would end every spawn
// with "the reaper wrapper reported an invalid pid". Observed quiet on this
// bash; asserted here so it stays that way for the wrong reason to be caught.
func TestReaperPidIsTheFirstStdoutLine(t *testing.T) {
	dir := t.TempDir()
	reaper := filepath.Join(dir, "run-worker.sh")
	if err := os.WriteFile(reaper, []byte(reaperScript), 0o755); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(dir, "claude.pid")
	// The Worker is short-lived and is collected by the reaper's own group
	// kill, so this test leaves nothing behind.
	cmd := exec.Command("bash", reaper, dir, filepath.Join(dir, "worker.log"),
		pidFile, filepath.Join(dir, "exit.status"),
		filepath.Join(dir, "authoritative", "exit.status"), "bash", "-c", "sleep 2")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = signalProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})

	sc := bufio.NewScanner(stdout)
	if !sc.Scan() {
		t.Fatal("the reaper printed no line on stdout — spawn reads the pid from there and would abort after 10s")
	}
	var printed int
	if _, err := fmt.Sscanf(sc.Text(), "%d", &printed); err != nil {
		t.Fatalf("the reaper's first stdout line is %q, which spawn cannot read as a pid: %v", sc.Text(), err)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the reaper never wrote claude.pid: %v", err)
	}
	recorded, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("claude.pid holds %q: %v", strings.TrimSpace(string(raw)), err)
	}
	if printed != recorded {
		t.Errorf("the reaper printed pid %d but wrote %d to claude.pid — spawn and collect would disagree about which process is the Worker", printed, recorded)
	}
}

// waitProcessNotRunning polls until /proc/<pid> is gone OR the process is a
// zombie (") Z " in stat, the same test waitZombie uses). Both mean the thing
// #166 is about: nothing is running any more. Which of the two a caller sees
// depends only on who the parent is — a leftover reparented to init is reaped
// and vanishes, one whose parent is the test process itself lingers as a
// zombie until the test reaps it — and no test should care about that.
//
// A zombie is not a weaker result: the production scan applies the same rule
// (sessionResidue skips Z — "a zombie is already dead — its parent has simply
// not reaped it ... there is nothing to stop"), and a zombie holds no CPU, no
// socket and no lock, which is the whole of what the Gate refuses.
func waitProcessNotRunning(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if os.IsNotExist(err) {
			return true
		}
		if err == nil && strings.Contains(string(data), ") Z ") {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
