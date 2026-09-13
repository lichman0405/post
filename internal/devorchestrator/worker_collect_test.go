package devorchestrator

import (
	"bytes"
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

// TestPathMatchesScope: delimiter-safe prefix matching, /** dir-entry
// semantics, and the plain-entry = exact-or-subtree rule.
func TestPathMatchesScope(t *testing.T) {
	scopes := []string{"cmd/rddev/**", "internal/devorchestrator/worker.go", "specs/orchestrator/**"}
	cases := []struct {
		p    string
		want bool
	}{
		{"cmd/rddev/main.go", true},
		{"cmd/rddev", true}, // the /** dir itself
		{"cmd/rddev/sub/deep/file.go", true},
		{"cmd/rddevx/foo.go", false}, // prefix collision must not match
		{"internal/devorchestrator/worker.go", true},
		{"internal/devorchestrator/worker_collect.go", false}, // sibling, not subtree
		{"specs/orchestrator/x.yaml", true},
		{"specs/SPEC_VERSION.json", false},
		{"README.md", false},
		{"docs/cmd/rddev/main.go", false},
	}
	for _, c := range cases {
		if got := pathMatchesScope(c.p, scopes); got != c.want {
			t.Errorf("pathMatchesScope(%q, %v) = %v, want %v", c.p, scopes, got, c.want)
		}
	}
}

// TestScanSecretsFindsAndSkips: documented credential shapes are found, the
// finding names the pattern class and never the matched text, and binary
// files are skipped.
func TestScanSecretsFindsAndSkips(t *testing.T) {
	dir := t.TempDir()
	secret := "ghp_AbCdEfGhIjKlMnOpQrStUvWxYz123456"
	writeFile := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("clean.txt", "no secrets here\n")
	writeFile("leak.txt", "token="+secret+"\n-----BEGIN OPENSSH PRIVATE KEY-----\n")
	// >8192 bytes with a NUL inside the probed prefix: the binary-skip path.
	bin := "A\x00" + strings.Repeat("A", 9000) + secret
	writeFile("bin.dat", bin)

	findings, err := scanSecrets(
		[]string{"clean.txt", "leak.txt", "bin.dat"},
		dir,
		filepath.Join(dir, "no-such-result.json"), // missing file is skipped
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 2 {
		t.Fatalf("got %d findings %v, want exactly 2 (PAT + private key)", len(findings), findings)
	}
	for _, f := range findings {
		if !strings.Contains(f, "leak.txt") {
			t.Errorf("finding %q does not name leak.txt", f)
		}
		if strings.Contains(f, secret) {
			t.Errorf("finding %q echoes the secret text", f)
		}
	}
	joined := strings.Join(findings, "; ")
	if !strings.Contains(joined, "GitHub classic PAT (ghp_)") || !strings.Contains(joined, "private key block") {
		t.Errorf("findings %q missing the pattern classes", joined)
	}
}

// TestParseTCPListeners: LISTEN rows are extracted with the inode from field
// index 9, non-LISTEN rows are skipped, and malformed rows fail loudly.
func TestParseTCPListeners(t *testing.T) {
	content := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 12345\n" +
		"   1: 00000000:0050 00000000:0000 01 00000000:00000000 00:00000000 00000000 1000 0 99999\n"
	got, err := parseTCPListeners(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d listeners %v, want exactly the LISTEN row", len(got), got)
	}
	li, ok := got["0100007F:8080"]
	if !ok {
		t.Fatalf("missing 0100007F:8080, got %v", got)
	}
	if li.Inode != "12345" {
		t.Errorf("inode = %q, want 12345", li.Inode)
	}

	if _, err := parseTCPListeners("  sl local_address\n   0: only-three-fields\n"); err == nil {
		t.Error("malformed row accepted without error")
	}
}

// TestValidateWorkerResultFile: the T0010 contract break — tests[].output
// instead of evidence and acceptance[] as plain strings — is rejected with
// EVERY defect listed, not just the first, and a conforming document passes.
func TestValidateWorkerResultFile(t *testing.T) {
	schemaPath := filepath.Join("..", "..", "specs", "orchestrator", "worker-result.schema.json")

	t.Run("T0010 shape rejected with all defects", func(t *testing.T) {
		bad := `{
			"task_id": "T0010",
			"status": "completed",
			"summary": "x",
			"files_changed": ["a"],
			"tests": [{"command": "go test", "status": "passed", "output": "ok"}],
			"acceptance": ["criterion one", "criterion two"],
			"risks": [],
			"follow_up_issues": [],
			"notes_for_supervisor": ""
		}`
		path := filepath.Join(t.TempDir(), "RESULT.json")
		if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
			t.Fatal(err)
		}
		err := ValidateWorkerResultFile(schemaPath, path)
		if err == nil {
			t.Fatal("T0010-shaped RESULT accepted")
		}
		for _, want := range []string{
			`tests[0]: unknown field "output"`,
			`acceptance[0]`,
			"is not of type object",
			`acceptance[1]`,
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing %q — all defects must be listed", err, want)
			}
		}
	})

	t.Run("conforming accepted", func(t *testing.T) {
		good := `{
			"task_id": "T0011",
			"status": "completed",
			"summary": "sum",
			"files_changed": ["a", "b"],
			"tests": [{"command": "go test ./...", "status": "passed", "evidence": "ok"}],
			"acceptance": [{"criterion": "c", "status": "passed", "evidence": "e"}],
			"risks": [],
			"follow_up_issues": [],
			"notes_for_supervisor": "n"
		}`
		path := filepath.Join(t.TempDir(), "RESULT.json")
		if err := os.WriteFile(path, []byte(good), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ValidateWorkerResultFile(schemaPath, path); err != nil {
			t.Errorf("conforming RESULT rejected: %v", err)
		}
	})

	t.Run("invalid JSON rejected", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "RESULT.json")
		if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ValidateWorkerResultFile(schemaPath, path); err == nil || !strings.Contains(err.Error(), "not valid JSON") {
			t.Errorf("invalid JSON: got %v", err)
		}
	})
}

// TestResultSchemaForCLI: the --json-schema argument is the repo schema with
// the header metadata stripped (claude rejects the draft URI) and every
// validation keyword intact; a missing schema fails loudly.
func TestResultSchemaForCLI(t *testing.T) {
	out, err := resultSchemaForCLI(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	for _, k := range []string{"$schema", "$id", "title"} {
		if _, ok := doc[k]; ok {
			t.Errorf("header key %q not stripped", k)
		}
	}
	if doc["type"] != "object" {
		t.Errorf("type = %v, want object", doc["type"])
	}
	if req, ok := doc["required"].([]any); !ok || len(req) == 0 {
		t.Error("required keyword lost")
	}

	if _, err := resultSchemaForCLI(t.TempDir()); err == nil {
		t.Error("missing schema did not fail")
	}
}

// startSetsidChild starts cmdline in its own session (what spawn does for the
// reaper) and registers cleanup that stops and reaps it.
func startSetsidChild(t *testing.T, name string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

// TestSessionResidueFindsLiveChildren: a live setsid child of the session is
// reported with its pid/cmdline/session; the recorded Worker pid is excluded.
func TestSessionResidueFindsLiveChildren(t *testing.T) {
	a := startSetsidChild(t, "sleep", "30")
	b := startSetsidChild(t, "sleep", "30")

	found, err := sessionResidue(a.Process.Pid, b.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("found %d processes %v, want exactly child A (child B is the recorded Worker pid)", len(found), found)
	}
	if found[0].PID != a.Process.Pid {
		t.Errorf("PID = %d, want %d", found[0].PID, a.Process.Pid)
	}
	if !strings.Contains(found[0].Cmdline, "sleep") {
		t.Errorf("Cmdline = %q, want the sleep command", found[0].Cmdline)
	}
	if found[0].Session != a.Process.Pid {
		t.Errorf("Session = %d, want %d (setsid child is its own session leader)", found[0].Session, a.Process.Pid)
	}
}

// TestSessionResidueExcludesReaperAndZombie: the spawn wrapper still exiting
// and an unreaped dead child are spawn machinery and death, not a service the
// Worker left running — both must not count as residue.
func TestSessionResidueExcludesReaperAndZombie(t *testing.T) {
	// The reaper: a script named run-worker.sh started in its own session,
	// exactly what spawn does with the wrapper. Its child (the sleep) is the
	// Worker, so it is passed as workerPID — the same call shape collect uses
	// (the recorded Worker pid, not an ancestry guess). The test waits for
	// the child to exist instead of racing the wrapper's fork (the race
	// flaked under full-suite load: a scan that beat the fork passed, one
	// that lost it failed).
	dir := t.TempDir()
	reaper := filepath.Join(dir, "run-worker.sh")
	if err := os.WriteFile(reaper, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := startSetsidChild(t, reaper)
	childPID := waitForChild(t, r.Process.Pid)
	found, err := sessionResidue(r.Process.Pid, childPID)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("reaper reported as residue: %v", found)
	}

	// The zombie: a dead setsid child whose parent (this test) has not yet
	// reaped it. Kill it without Wait so it stays a zombie, then check.
	z := startSetsidChild(t, "sleep", "30")
	if err := z.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitZombie(t, z.Process.Pid)
	found, err = sessionResidue(z.Process.Pid, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("zombie reported as residue: %v", found)
	}
}

// waitZombie polls /proc/<pid>/stat until the state field is Z.
func waitZombie(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err == nil && strings.Contains(string(data), ") Z ") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid %d did not become a zombie", pid)
}

// waitForChild polls /proc/<pid>/task/<pid>/children until pid has at least
// one live child and returns its pid. Spawn's reaper wrapper backgrounds the
// Worker as its child, so a deterministic test must observe that child —
// never race the fork.
func waitForChild(t *testing.T, pid int) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", pid, pid))
		if err == nil {
			fields := strings.Fields(string(data))
			if len(fields) > 0 {
				child, err := strconv.Atoi(fields[0])
				if err == nil && child > 0 {
					return child
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("pid %d never showed a child", pid)
	return 0
}

// startMarkerChild starts cmdline in its own session with the given extra
// environment (what a Worker descendant inherits). Returns the live cmd.
func startMarkerChild(t *testing.T, extraEnv []string, name string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

// TestMarkerResidueFindsEnvAttributedChild: the run-marker signal catches the
// class the session scan misses (T0011 Defect 2) — real claude's Bash tool
// starts each command in its own session, so a nohup'd survivor is invisible
// to the session scan; its environment still carries POST_WORKER_RUN_ID.
func TestMarkerResidueFindsEnvAttributedChild(t *testing.T) {
	runID := "run-test-marker"
	// the attributed survivor: escapes the session (setsid), keeps the marker
	marked := startMarkerChild(t, []string{"POST_WORKER_RUN_ID=" + runID}, "sleep", "30")
	// the legitimate neighbours: (a) a child that inherited the marker but
	// scrubbed its environment before exec, the way a daemonizing service
	// does with env -i — the warn-only class must stay unattributable;
	// (b) a child carrying a different run's marker.
	scrubbed := exec.Command("env", "-i", "PATH=/usr/bin:/bin", "sleep", "30")
	scrubbed.Env = append(os.Environ(), "POST_WORKER_RUN_ID="+runID)
	scrubbed.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := scrubbed.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = scrubbed.Process.Kill()
		_ = scrubbed.Wait()
	})
	other := startMarkerChild(t, []string{"POST_WORKER_RUN_ID=run-other"}, "sleep", "30")

	// Precondition: the scrubbed child must have EXEC'd before the scan.
	//
	// `env -i ... sleep` carries the marker in its own environment until it
	// execs sleep, at which point the kernel replaces /proc/PID/environ with
	// the scrubbed one. Reading during that window observes `env`, which
	// genuinely does carry the marker — and the detector is right to attribute
	// it, because "this process carries the marker" is the rule. So the race is
	// in this fixture's assumption, not in the detector: the window is tiny and
	// load-dependent, which is why it passed on a developer machine and failed
	// on a loaded CI runner.
	waitForEnvironWithoutMarker(t, scrubbed.Process.Pid, "POST_WORKER_RUN_ID="+runID)

	startTicks, err := procStartTicks(marked.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}

	found, err := markerResidue(runID, startTicks-1, map[int]bool{})
	if err != nil {
		t.Fatal(err)
	}
	var gotMarked, gotScrubbed, gotOther bool
	for _, f := range found {
		switch f.PID {
		case marked.Process.Pid:
			gotMarked = true
			if !strings.Contains(f.Cmdline, "sleep") {
				t.Errorf("marked child cmdline = %q", f.Cmdline)
			}
		case scrubbed.Process.Pid:
			gotScrubbed = true
		case other.Process.Pid:
			gotOther = true
		}
	}
	if !gotMarked {
		t.Errorf("marker scan missed the environment-attributed child %d: %v", marked.Process.Pid, found)
	}
	if gotScrubbed {
		t.Errorf("marker scan attributed the environment-scrubbed child %d — the warn-only class must stay unattributable", scrubbed.Process.Pid)
	}
	if gotOther {
		t.Errorf("marker scan attributed child %d from a different run", other.Process.Pid)
	}

	// pre-existing processes are never attributed: a startTicks cutoff after
	// their start must exclude them even when the marker matches.
	lateCutoff, err := procStartTicks(marked.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	found, err = markerResidue(runID, lateCutoff+1, map[int]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Errorf("marker scan with a post-start cutoff attributed pre-existing processes: %v", found)
	}
}

// newRefsSince decides whether the Worker created a ref. refsSnapshot stores
// "<refname> <objectname>", and comparing whole entries made any ref that
// merely MOVED look newly created — while refs move routinely during a run
// precisely because the Supervisor merges other work. T0101 was rejected with
// "new ref(s) created: refs/heads/main a07ac01…" because two unrelated PRs
// merged while it ran.
func TestMovingAnExistingRefIsNotANewRef(t *testing.T) {
	before := []string{"refs/heads/main aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"refs/tags/v0 1111111111111111111111111111111111111111"}

	// The Supervisor merges during the run: main advances, nothing is created.
	moved := []string{"refs/heads/main bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"refs/tags/v0 1111111111111111111111111111111111111111"}
	if got := newRefsSince(before, moved); len(got) != 0 {
		t.Errorf("advancing an existing ref was reported as a new ref: %v — the Supervisor's own merges would reject the collect", got)
	}

	// An entry recorded without a sha (older records, fixtures) must still
	// suppress by name.
	if got := newRefsSince([]string{"refs/heads/main", "refs/tags/v0"}, moved); len(got) != 0 {
		t.Errorf("a sha-less before entry did not suppress by name: %v", got)
	}

	// Creating one is still caught — that is the capability the guard denies.
	created := append([]string{}, moved...)
	created = append(created, "refs/heads/sneaky cccccccccccccccccccccccccccccccccccccccc")
	got := newRefsSince(before, created)
	if len(got) != 1 || !strings.HasPrefix(got[0], "refs/heads/sneaky") {
		t.Errorf("a genuinely new ref was not reported: %v", got)
	}

	// Deleting a ref is not "creating" one.
	if got := newRefsSince(before, []string{"refs/heads/main bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}); len(got) != 0 {
		t.Errorf("a removed ref was reported as new: %v", got)
	}
}

// waitForEnvironWithoutMarker blocks until pid's environment no longer carries
// marker, or fails. It waits for the condition the test actually depends on
// rather than assuming it, and a fixture that never scrubs its environment
// fails here with a message that says so instead of surfacing later as a
// confusing attribution failure.
func waitForEnvironWithoutMarker(t *testing.T, pid int, marker string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
		if err == nil {
			has := false
			for _, kv := range bytes.Split(environ, []byte{0}) {
				if string(kv) == marker {
					has = true
					break
				}
			}
			if !has {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("pid %d still carries %s after 10s — the fixture was supposed to scrub it before exec", pid, marker)
}

// Only the task namespace is judged. The intended risk is a Worker turning its
// work into a ref, which would land under refs/heads/task/**; watched the other
// way round the check fires on the Supervisor's own pull-request branches, which
// are created during runs constantly. T0201 was rejected because PR #96's branch
// appeared while its Worker ran — a gate that fires on every run is as broken as
// one that never fires. (The rule had been inverted relative to its purpose.)
func TestARefIsJudgedByWhoseWorkItCarries(t *testing.T) {
	root := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Supervisor", "GIT_AUTHOR_EMAIL=sup@post.local",
			"GIT_COMMITTER_NAME=Supervisor", "GIT_COMMITTER_EMAIL=sup@post.local")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "sup@post.local")
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "base")

	// The Supervisor's own branch, opened while a Worker ran, carries a commit
	// the Supervisor authored — not a Worker's doing, and every pull request
	// creates one.
	if got := refAuthor(root, "HEAD"); got != "sup@post.local" {
		t.Fatalf("fixture: HEAD author = %q", got)
	}
	// A commit made with a different identity is unattributable to the
	// Supervisor, so a ref carrying it is a finding however it is named.
	cmd := exec.Command("git", "commit", "-q", "--allow-empty", "-m", "planted")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=someone", "GIT_AUTHOR_EMAIL=planted@elsewhere",
		"GIT_COMMITTER_NAME=someone", "GIT_COMMITTER_EMAIL=planted@elsewhere")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("planting: %v\n%s", err, out)
	}
	if got := refAuthor(root, "HEAD"); got == "sup@post.local" {
		t.Fatal("fixture: the planted commit reports the Supervisor's identity")
	}
}
