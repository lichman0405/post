package devorchestrator

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
		// "file write", not "shell write": check_write_target now backs the
		// file-writing tools as well as the shell-write policy, and a message
		// that still said "shell" would be the same narrow-claim problem this
		// task exists to fix, one layer down.
		"refusing to decide a file write",
		"refusing to decide a file read",
		"cannot parse the tool-input JSON", // the fail-closed arm of the path decoder
		"pending = 1",                      // segment reset (second segment command position)
		`*".env"*`,                         // glob-pattern .env matching
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
	// The reaper the Worker actually runs must be the one this source
	// describes: #166's collection lives in that script, and a fix that never
	// reaches the generated file is a fix no Worker ever runs.
	reaper, err := os.ReadFile(filepath.Join(dir, "run-worker.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if string(reaper) != reaperScript {
		t.Error("generated run-worker.sh differs from reaperScript")
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
	if hook["matcher"] != guardMatcher {
		t.Errorf("hook matcher = %v, want %q", hook["matcher"], guardMatcher)
	}
	// ...and it must actually cover the tools the allow list lets through.
	// This is the T1219 seam: the matcher is the only thing that decides
	// whether the hook runs at all, and it used to be narrower than the allow
	// list, so Write and Edit ran bare and unseen.
	matcher := hook["matcher"].(string)
	for _, tool := range perms["allow"].([]any) {
		name := tool.(string)
		if !slices.Contains(strings.Split(matcher, "|"), name) {
			t.Errorf("permissions.allow contains %q but the PreToolUse matcher %q does not name it: that tool runs in dontAsk mode with the guard never invoked for it, which is exactly how Write/Edit escaped the write envelope (T1219)", name, matcher)
		}
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

// TestGuardMatcherNamesExactlyTheKnownTools: the matcher is derived and
// checked in BOTH directions, because both directions fail open.
//
// A tool the matcher omits never reaches the guard — that is how Write/Edit
// escaped (they were in permissions.allow, so dontAsk ran them bare while the
// guard's own text promised a write envelope). A tool the matcher names but
// that does not exist is worse than useless in the other way: it is a rule
// that reads as confinement and matches nothing. So the matcher and this list
// have to be the same set, and adding either side without the other is a
// failure rather than a quiet drift.
//
// HOW THE LIST BELOW IS DERIVED (not remembered):
//
//  1. permissions.allow is {Bash, Write, Edit}. Every one of them must be
//     matched — Bash is the shell tool, Write and Edit are the two file
//     writers dontAsk would otherwise run unseen.
//  2. Every tool a Worker can point at a path must be matched, whether or not
//     it is allowed today: Read/Grep/Glob/NotebookRead (the read tools the
//     guard confines) and NotebookEdit/MultiEdit (file writers the permission
//     layer refuses today because they are absent from allow — an accident of
//     the allow list, and not one the envelope should rest on).
//  3. Non-file tools are deliberately absent: Task, TodoWrite, WebFetch,
//     StructuredOutput and friends name no path, so no path policy applies to
//     them. An unknown tool name exits the guard silently, by design.
//
// That derivation is asserted against permissions.allow in
// TestWriteGuardFiles; this test fixes the set itself.
func TestGuardMatcherNamesExactlyTheKnownTools(t *testing.T) {
	// Derived from 1 and 2 above.
	known := []string{
		"Bash",
		"Read", "Grep", "Glob", "NotebookRead",
		"Write", "Edit", "MultiEdit", "NotebookEdit",
	}
	got := strings.Split(guardMatcher, "|")

	inMatcher := map[string]bool{}
	for _, n := range got {
		if strings.TrimSpace(n) == "" {
			t.Errorf("the matcher %q carries an empty alternative, which matches nothing", guardMatcher)
		}
		if inMatcher[n] {
			t.Errorf("the matcher %q names %q twice", guardMatcher, n)
		}
		inMatcher[n] = true
	}
	for _, n := range known {
		if !inMatcher[n] {
			t.Errorf("the matcher %q does not name %q: a tool named no path policy reaches the guard for it", guardMatcher, n)
		}
	}
	for _, n := range got {
		if !slices.Contains(known, n) {
			t.Errorf("the matcher %q names %q, which is not on the derived tool list — either it has no path and does not belong, or the derivation above is out of date", guardMatcher, n)
		}
	}

	// The tools the guard's own header claims confinement for. If the matcher
	// ever stops naming one of these, that header becomes a promise nothing
	// enforces — the exact defect this test exists for.
	for _, n := range []string{"Write", "Edit"} {
		if !slices.Contains(got, n) {
			t.Fatalf("the matcher stopped naming %q: it is in permissions.allow, so dontAsk runs it in the Worker whether or not the guard sees it", n)
		}
	}
}

// TestFilePathToolsAreConfinedByTheGuard drives the embedded guard with the
// tool-call JSON Claude Code actually sends for the file-path tools, and
// checks the envelope holds for them exactly as it does for a shell write.
//
// The Write cases are the ones T1219 turns on, and they must go through the
// Write tool's own payload shape: `sh -c 'echo > /etc/x'` was already
// confined before this task and would prove nothing about the tool that was
// not. The Edit/MultiEdit/NotebookEdit credential-store cases cover the other
// half — these tools read the file they are about to change, so the read
// confinement has to reach them too.
func TestFilePathToolsAreConfinedByTheGuard(t *testing.T) {
	dir := t.TempDir()
	guard := filepath.Join(dir, "worker-guard.sh")
	if err := os.WriteFile(guard, []byte(GuardScript()), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{
		"POST_REPO_ROOT=/repo",
		"POST_WORKER_TASK_ID=T0001",
		"POST_WORKER_WORKTREE=/repo/.rddev/worktrees/T0001",
		"POST_WORKER_RESULT_DIR=/repo/.rddev/workers/T0001",
	}

	cases := []struct {
		name      string
		tool      string
		field     string
		path      string
		wantBlock bool
		reason    string
	}{
		// The required pair, through the Write tool.
		{"Write outside the envelope", "Write", "file_path", "/repo/.rddev/runtime/tasks/T0001/gate-inputs.json", true, "confined"},
		{"Write to /etc", "Write", "file_path", "/etc/x", true, "confined"},
		{"Write inside the worktree", "Write", "file_path", "/repo/.rddev/worktrees/T0001/notes.txt", false, ""},
		{"Write into the result dir", "Write", "file_path", "/repo/.rddev/workers/T0001/RESULT.json", false, ""},
		// Edit / MultiEdit / NotebookEdit, both halves.
		{"Edit to /etc", "Edit", "file_path", "/etc/hosts", true, "confined"},
		{"Edit inside the worktree", "Edit", "file_path", "/repo/.rddev/worktrees/T0001/main.go", false, ""},
		{"Edit a credential store", "Edit", "file_path", "~/.ssh/id_rsa", true, "credential store"},
		{"MultiEdit a credential store", "MultiEdit", "file_path", "~/.netrc", true, "credential store"},
		{"NotebookEdit outside the envelope", "NotebookEdit", "notebook_path", "/etc/nb.ipynb", true, "confined"},
		{"NotebookEdit a credential store", "NotebookEdit", "notebook_path", "~/.aws/credentials", true, "credential store"},
		{"NotebookEdit inside the worktree", "NotebookEdit", "notebook_path", "/repo/.rddev/worktrees/T0001/nb.ipynb", false, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := json.Marshal(map[string]any{
				"tool_name":  tc.tool,
				"tool_input": map[string]any{tc.field: tc.path},
			})
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", guard)
			cmd.Env = append(os.Environ(), env...)
			cmd.Stdin = bytes.NewReader(payload)
			var out bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &out
			runErr := cmd.Run()

			blocked := false
			if ee, ok := runErr.(*exec.ExitError); ok && ee.ExitCode() == 2 {
				blocked = true
			} else if runErr != nil {
				t.Fatalf("running the guard: %v (%s)", runErr, out.String())
			}
			if blocked != tc.wantBlock {
				t.Fatalf("%s %s=%q: blocked=%v, want %v — output: %s", tc.tool, tc.field, tc.path, blocked, tc.wantBlock, out.String())
			}
			if tc.wantBlock && !strings.Contains(out.String(), tc.reason) {
				t.Errorf("blocked but the reason does not mention %q: %s", tc.reason, out.String())
			}
		})
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

	// ...and the Gate must agree. The end state #166 asks for is not "the pid
	// died" but "collect passes": the scan is what refuses, so it is what has
	// to come back empty. Run it on the same run's anchors.
	workerRaw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the reaper never wrote claude.pid: %v", err)
	}
	workerPID, err := strconv.Atoi(strings.TrimSpace(string(workerRaw)))
	if err != nil {
		t.Fatalf("claude.pid holds %q: %v", strings.TrimSpace(string(workerRaw)), err)
	}
	rec := &WorkerRecord{TaskID: "T0000", PID: workerPID, SessionLeaderPID: r.Process.Pid}
	residue, _, err := scanResidue(rec)
	if err != nil {
		t.Fatalf("scanning the session for residue: %v", err)
	}
	if len(residue) > 0 {
		t.Errorf("collect would still refuse this run: %s", residueReport("Worker", rec, residue))
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
		// Both groups, because the Worker now leads its OWN one: that is the
		// change under test, and it means killing the reaper's group no longer
		// reaches the Worker at all. A Worker still between fork and exec when
		// the reaper dies carries on and recreates worker.log inside this
		// test's TempDir right after RemoveAll has listed its entries — which
		// surfaces as "TempDir RemoveAll cleanup: directory not empty", a
		// failure that names the test's own bookkeeping rather than anything
		// under test. Collected the way the reaper collects it: from the pid it
		// recorded.
		if raw, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				_ = signalProcessGroup(pid, syscall.SIGKILL)
			}
		}
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
