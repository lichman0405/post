package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/version"
)

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(version) exit code = %d, want 0", code)
	}
	want := "rddev " + version.Version + "\n"
	if stdout.String() != want {
		t.Errorf("run(version) output = %q, want %q", stdout.String(), want)
	}
	if stderr.Len() != 0 {
		t.Errorf("run(version) stderr = %q, want empty", stderr.String())
	}
}

func TestRunUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"frobnicate"}, &stdout, &stderr); code != 2 {
		t.Fatalf("run(unknown) exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), `unknown subcommand "frobnicate"`) {
		t.Errorf("run(unknown) stderr = %q, want unknown-subcommand message", stderr.String())
	}
}

func TestRunNoArgsShowsUsageAndExitsNonZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 2 {
		t.Fatalf("run(no args) exit code = %d, want 2", code)
	}
	if !strings.Contains(stdout.String(), "rddev version") {
		t.Errorf("run(no args) stdout = %q, want usage text", stdout.String())
	}
}

func TestRunHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(help) exit code = %d, want 0", code)
	}
	for _, want := range []string{"rddev doctor", "rddev task next", "rddev worker", "rddev pr"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("help output missing %q", want)
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("run(help) stderr = %q, want empty", stderr.String())
	}
}

// The doctor engine itself is covered by the doctor package tests; here the
// dispatch contract is checked: exit 0 for --help, exit 3 with usage on
// stderr for unknown options, nothing on stdout in either case.
func TestRunDoctorDelegation(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"doctor", "--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(doctor --help) exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "--check-docker-daemon") {
		t.Errorf("doctor --help stdout = %q, want doctor usage", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"doctor", "--bogus"}, &stdout, &stderr); code != 3 {
		t.Fatalf("run(doctor --bogus) exit code = %d, want 3 (doctor usage)", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("run(doctor --bogus) stdout = %q, want empty (no report)", stdout.String())
	}
	if !strings.Contains(stderr.String(), "unknown option") {
		t.Errorf("run(doctor --bogus) stderr = %q, want unknown-option error", stderr.String())
	}
}

// writeDoctorFixture writes the canonical all-green fixture (same shape as
// the doctor package's own fixture): every measurement and tool is overridden,
// so the run performs no host probing.
func writeDoctorFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"os.kernel": "Linux\n", "os.arch": "x86_64\n", "os.id": "ubuntu\n",
		"os.version_id": "24.04\n", "os.pretty": "Ubuntu 24.04.5 LTS\n",
		"os.codename": "noble\n", "os.wsl": "none\n",
		"res.nproc": "16\n", "res.mem_total_kb": "73400320\n",
		"res.swap_total_kb": "67108864\n", "res.fd_limit": "1048576\n",
		"res.disk_free_kb": "209715200\n", "res.docker_reachable": "true\n",
		"res.docker_root_free_kb": "131072000\n",
		"res.bwrap":               "present\n", "res.socat": "present\n",
		"git": "git version 2.43.0\n", "git-lfs": "git-lfs/3.4.1 (GitHub; linux amd64)\n",
		"claude": "2.1.269 (Claude Code)\n", "go": "go version go1.27.1 linux/amd64\n",
		"node": "v24.21.0\n", "pnpm": "12.4.1\n", "python3": "Python 3.12.7\n",
		"uv": "uv 0.7.9\n", "docker": "Docker version 29.8.0, build abc123\n",
		"docker-compose": "Docker Compose version v2.39.4\n", "jq": "jq-1.6\n",
		"psql": "psql (PostgreSQL) 16.15\n", "redis-cli": "redis-cli 7.0.15\n",
		"make": "GNU Make 4.3\n", "rg": "ripgrep 14.1.1\n",
		"shellcheck": "ShellCheck - shell script analysis tool\nversion: 0.10.0\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "package.json"),
		[]byte(`{"name":"post","packageManager":"pnpm@12.4.1"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "os.repo_path"), []byte(repo+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Regression: a --json after the subcommand must reach the doctor engine, not
// be consumed by the global flag scan.
func TestRunDoctorJSONFlagReachesEngine(t *testing.T) {
	fix := writeDoctorFixture(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"doctor", "--json", "--fixture", fix}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(doctor --json --fixture) exit code = %d, stderr = %q", code, stderr.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not JSON (the --json flag was stripped): %v\n%s", err, stdout.String())
	}
	if doc["verdict"] != "ok" || doc["schema_version"] != float64(1) {
		t.Errorf("doctor JSON = %v, want ok verdict with schema_version 1", doc)
	}
}

// Honest stubs: every command owned by a later task fails with exit 4 and an
// explicit message naming the owning task — never a silent no-op.
// (spawn/list/logs/stop became real in T0010, collect in T0011, and the
// T0012 git/pr control plane became real with the four-gate assertion — only
// the env stubs remain.)
func TestStubsExitNotImplemented(t *testing.T) {
	cases := []struct {
		args    []string
		owner   string
		jsonErr bool
	}{
		{[]string{"env", "reset", "--test-only"}, "T0010/T0011", false},
		{[]string{"env", "gc"}, "T0010/T0011", false},
	}
	for _, tc := range cases {
		var stdout, stderr bytes.Buffer
		code := run(tc.args, &stdout, &stderr)
		if code != 4 {
			t.Errorf("run(%v) exit code = %d, want 4", tc.args, code)
			continue
		}
		msg := stderr.String()
		if tc.jsonErr {
			var doc map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
				t.Errorf("run(%v) stdout is not valid JSON: %v\n%s", tc.args, err, stdout.String())
				continue
			}
			if doc["exit_code"] != float64(4) {
				t.Errorf("run(%v) json exit_code = %v, want 4", tc.args, doc["exit_code"])
			}
			msg, _ = doc["error"].(string)
		}
		if !strings.Contains(msg, "not implemented") || !strings.Contains(msg, tc.owner) {
			t.Errorf("run(%v) message = %q, want explicit not-implemented naming %s", tc.args, msg, tc.owner)
		}
	}
}

// TestGitPRActionsRefusedByRedGate: the T0012 control plane replaced the
// exit-4 stubs with real actions whose FIRST step is the four-gate assertion.
// With no evidence records on disk the assertion is red, so every action
// refuses (exit 1) and states plainly that git/gh was never invoked.
func TestGitPRActionsRefusedByRedGate(t *testing.T) {
	// The real repo's gates.json (relative to the package dir the tests run
	// in) so the assertion loads, then fails on the missing evidence.
	gates := filepath.Join("..", "..", "specs", "orchestrator", "gates.json")
	for _, args := range [][]string{
		{"git", "commit", "T0009"},
		{"git", "push", "T0009"},
		{"pr", "open", "T0009"},
		{"pr", "merge", "T0009"},
		{"--json", "pr", "merge", "T0009"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(append(args, "--gates", gates), &stdout, &stderr)
		if code != 1 {
			t.Errorf("run(%v) exit code = %d, want 1 (refused)", args, code)
			continue
		}
		msg := stderr.String()
		if !strings.Contains(msg, "REFUSED") {
			t.Errorf("run(%v) stderr = %q, want an explicit four-gate refusal", args, msg)
		}
		if !strings.Contains(msg, "was never invoked") {
			t.Errorf("run(%v) stderr = %q, must state git/gh was never invoked", args, msg)
		}
		if strings.Contains(msg, "not implemented") {
			t.Errorf("run(%v) stderr = %q, the T0012 stub is still there", args, msg)
		}
	}
}

func TestRunEnvUsageErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"env"}, &stdout, &stderr); code != 2 {
		t.Fatalf("run(env) exit code = %d, want 2", code)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"env", "bogus"}, &stdout, &stderr); code != 2 {
		t.Fatalf("run(env bogus) exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), `unknown subcommand "bogus"`) {
		t.Errorf("run(env bogus) stderr = %q", stderr.String())
	}
}

// TestGateRunCLIParsesAndExecutes: `rddev gate run GATE TASK` takes TWO
// positionals (a regression: the first version read pos[0] — "run" — as the
// gate name and always exited with a usage error). The gate's steps really
// execute and the evidence record lands on disk.
func TestGateRunCLIParsesAndExecutes(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "specs", "orchestrator"), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := `{
  "version": 1,
  "required_jobs": ["job-a"],
  "gates": {
    "G1": {"name": "", "description": "", "runs_jobs": [], "asserts_jobs": []},
    "G2": {"name": "", "description": "", "runs_jobs": ["job-a"], "asserts_jobs": []},
    "G3": {"name": "", "description": "", "runs_jobs": [], "asserts_jobs": []},
    "G4": {"name": "", "description": "", "runs_jobs": [], "asserts_jobs": ["job-a"]}
  },
  "jobs": {"job-a": {"steps": [{"run": "echo cli-ok"}]}},
  "review": {"required_for_merge": false},
  "task_overrides": {}
}`
	if err := os.WriteFile(filepath.Join(dir, "specs", "orchestrator", "gates.json"), []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"gate", "run", "G2", "T0009"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(gate run G2 T0009) exit code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "G2 passed") {
		t.Errorf("stdout = %q, want the passed G2 run", stdout.String())
	}
	// The evidence record exists on disk (the executor really ran the step).
	entries, err := os.ReadDir(filepath.Join(dir, ".rddev", "runtime", "gates", "T0009"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "gate-run-") {
			found = true
		}
	}
	if !found {
		t.Errorf("no gate-run record written under .rddev/runtime/gates/T0009: %v", entries)
	}

	// G1/G4 are not executable by hand: explicit refusal, never a silent no-op.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"gate", "run", "G4", "T0009"}, &stdout, &stderr); code != 2 {
		t.Errorf("run(gate run G4) exit code = %d, want 2 (usage refusal)", code)
	}
	if !strings.Contains(stderr.String(), "not executable") {
		t.Errorf("run(gate run G4) stderr = %q, want the not-executable refusal", stderr.String())
	}
}

func TestParseFlags(t *testing.T) {
	specs := []flagSpec{
		{"--json", false},
		{"--reason", true},
		{"--reason-file", true},
	}
	vals, pos, err := parseFlags([]string{"reject", "T0009", "--reason", "scope", "--json", "--reason-file=rej.md"}, specs...)
	if err != nil {
		t.Fatal(err)
	}
	if len(pos) != 2 || pos[0] != "reject" || pos[1] != "T0009" {
		t.Errorf("positionals = %v", pos)
	}
	if _, ok := vals["--json"]; !ok {
		t.Error("--json not parsed")
	}
	if vals["--reason"] != "scope" || vals["--reason-file"] != "rej.md" {
		t.Errorf("vals = %v", vals)
	}

	// Unknown flag, missing value, and value on a boolean flag are all errors.
	if _, _, err := parseFlags([]string{"--bogus"}, specs...); err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("unknown flag accepted: %v", err)
	}
	if _, _, err := parseFlags([]string{"--reason"}, specs...); err == nil || !strings.Contains(err.Error(), "requires a value") {
		t.Errorf("missing value accepted: %v", err)
	}
	if _, _, err := parseFlags([]string{"--json=1"}, specs...); err == nil || !strings.Contains(err.Error(), "does not take a value") {
		t.Errorf("value on boolean flag accepted: %v", err)
	}
}
