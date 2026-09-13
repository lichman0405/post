package doctor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This file ports ops/tests/doctor-unit-test.sh case for case (contract
// ops/doctor-checks.md §12). All cases are fixture-driven: no real version
// command is ever executed, no daemon is probed unless the fixture says so.

// mkFixture writes the canonical all-green fixture (doctor-unit-test.sh).
func mkFixture(t *testing.T, dir string) {
	t.Helper()
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
}

// runDoctor runs the doctor engine and returns exit code, stdout, stderr and
// (when stdout is JSON) the parsed document.
func runDoctor(t *testing.T, fixtureDir string, args ...string) (code int, stdout, stderr string, doc map[string]any) {
	t.Helper()
	var out, errb bytes.Buffer
	opts, usageOnErr, perr := ParseArgs(args)
	if perr != nil {
		ue := perr.(*UsageError)
		if usageOnErr {
			PrintUsage(&errb)
		}
		fmt.Fprintf(&errb, "%s\n", ue.Msg)
		return 3, out.String(), errb.String(), nil
	}
	if opts == nil { // --help: usage on stdout, exit 0
		PrintUsage(&out)
		return 0, out.String(), errb.String(), nil
	}
	opts.FixtureDir = fixtureDir
	code = opts.Run(&out)
	if opts.JSON {
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatalf("stdout is not valid JSON (rc=%d): %v\n%s", code, err, out.String())
		}
	}
	return code, out.String(), errb.String(), doc
}

func findCheck(t *testing.T, doc map[string]any, id, field string) string {
	t.Helper()
	checks := doc["checks"].([]any)
	for _, c := range checks {
		cm := c.(map[string]any)
		if cm["id"] == id {
			v, ok := cm[field]
			if !ok || v == nil {
				return ""
			}
			return v.(string)
		}
	}
	t.Fatalf("check %s not found in output", id)
	return ""
}

func TestCanonicalAllGreen(t *testing.T) {
	fix := t.TempDir()
	mkFixture(t, fix)
	code, _, _, doc := runDoctor(t, fix, "--json", "--check-docker-daemon")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := doc["verdict"]; got != "ok" {
		t.Fatalf("verdict = %v, want ok", got)
	}
	if got := doc["exit_code"]; got != float64(0) {
		t.Fatalf("exit_code field = %v, want 0", got)
	}
	if got := findCheck(t, doc, "T-GO", "status"); got != "passed" {
		t.Errorf("T-GO status = %q", got)
	}
	if got := findCheck(t, doc, "T-GO", "version"); got != "1.27" {
		t.Errorf("T-GO version = %q", got)
	}
	if got := findCheck(t, doc, "T-PNPM-PIN", "status"); got != "passed" {
		t.Errorf("T-PNPM-PIN status = %q", got)
	}
	if got := findCheck(t, doc, "RES-DOCKER-DISK", "status"); got != "passed" {
		t.Errorf("RES-DOCKER-DISK status = %q", got)
	}
	if got := findCheck(t, doc, "T-DOCKER-DAEMON", "status"); got != "passed" {
		t.Errorf("T-DOCKER-DAEMON status = %q", got)
	}
	if got := findCheck(t, doc, "RES-REPO-FS", "status"); got != "skipped" {
		t.Errorf("RES-REPO-FS status = %q", got)
	}
	if got := doc["summary"].(map[string]any)["required"].(map[string]any)["failed"]; got != float64(0) {
		t.Errorf("required.failed = %v, want 0", got)
	}
	if got := doc["summary"].(map[string]any)["advisory"].(map[string]any)["warn"]; got != float64(0) {
		t.Errorf("advisory.warn = %v, want 0", got)
	}

	// Human output: exit 0, verdict line, no FAIL lines.
	code, out, _, _ := runDoctor(t, fix, "--check-docker-daemon")
	if code != 0 {
		t.Fatalf("human exit code = %d, want 0", code)
	}
	if !strings.Contains(out, "Verdict: ok (exit 0)") {
		t.Errorf("human output missing verdict line:\n%s", out)
	}
	if strings.Contains(out, "[FAIL]") {
		t.Errorf("human output contains a FAIL line:\n%s", out)
	}
}

func TestMissingRequiredTool(t *testing.T) {
	fix := t.TempDir()
	mkFixture(t, fix)
	os.Remove(filepath.Join(fix, "go"))
	os.WriteFile(filepath.Join(fix, "go.absent"), nil, 0o644)
	code, out, _, doc := runDoctor(t, fix, "--json")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if got := doc["verdict"]; got != "toolchain_failure" {
		t.Fatalf("verdict = %v", got)
	}
	if got := findCheck(t, doc, "T-GO", "status"); got != "failed" {
		t.Errorf("T-GO status = %q", got)
	}
	if got := findCheck(t, doc, "T-GO", "remediation"); !strings.Contains(got, "Go 1.27.x") {
		t.Errorf("remediation %q does not name Go 1.27.x", got)
	}
	if got := findCheck(t, doc, "T-GO", "measured"); got != "missing" {
		t.Errorf("measured = %q, want missing", got)
	}
	if !strings.Contains(out, "missing") {
		t.Errorf("JSON does not contain the missing marker")
	}

	code, out, _, _ = runDoctor(t, fix)
	if code != 1 {
		t.Fatalf("human exit code = %d, want 1", code)
	}
	for _, want := range []string{"T-GO", "fix:", "Go 1.27.x", "Verdict: toolchain_failure (exit 1)"} {
		if !strings.Contains(out, want) {
			t.Errorf("human output missing %q:\n%s", want, out)
		}
	}
}

func TestGoMajorMinorDrift(t *testing.T) {
	fix := t.TempDir()
	mkFixture(t, fix)
	os.WriteFile(filepath.Join(fix, "go"), []byte("go version go1.28.0 linux/amd64\n"), 0o644)
	code, _, _, doc := runDoctor(t, fix, "--json")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if got := findCheck(t, doc, "T-GO", "status"); got != "failed" {
		t.Errorf("T-GO status = %q", got)
	}
	if got := findCheck(t, doc, "T-GO", "version"); got != "1.28" {
		t.Errorf("T-GO version = %q", got)
	}
	rem := findCheck(t, doc, "T-GO", "remediation")
	if !strings.Contains(rem, "1.27.x") || !strings.Contains(rem, "Supervisor") {
		t.Errorf("remediation %q must name the pinned baseline and Supervisor sign-off", rem)
	}
}

func TestPythonBelowBaseline(t *testing.T) {
	fix := t.TempDir()
	mkFixture(t, fix)
	os.WriteFile(filepath.Join(fix, "python3"), []byte("Python 3.11.9\n"), 0o644)
	code, _, _, doc := runDoctor(t, fix, "--json")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if got := findCheck(t, doc, "T-PYTHON3", "status"); got != "failed" {
		t.Errorf("T-PYTHON3 status = %q", got)
	}
	if rem := findCheck(t, doc, "T-PYTHON3", "remediation"); !strings.Contains(rem, "3.12") {
		t.Errorf("remediation %q does not name 3.12", rem)
	}
}

func TestWindowsNativeUnsupported(t *testing.T) {
	fix := t.TempDir()
	mkFixture(t, fix)
	os.WriteFile(filepath.Join(fix, "os.kernel"), []byte("MINGW64_NT-10.0-22631\n"), 0o644)
	code, _, _, doc := runDoctor(t, fix, "--json")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if got := doc["verdict"]; got != "unsupported" {
		t.Fatalf("verdict = %v", got)
	}
	if got := findCheck(t, doc, "ENV-OS-KERNEL", "status"); got != "failed" {
		t.Errorf("ENV-OS-KERNEL status = %q", got)
	}
	rem := findCheck(t, doc, "ENV-OS-KERNEL", "remediation")
	if !strings.Contains(rem, "WSL2") || !strings.Contains(rem, "Linux filesystem") {
		t.Errorf("remediation %q must name WSL2 and the Linux filesystem", rem)
	}

	code, out, _, _ := runDoctor(t, fix)
	if code != 2 {
		t.Fatalf("human exit code = %d, want 2", code)
	}
	if !strings.Contains(out, "Windows Native") || !strings.Contains(out, "WSL2") {
		t.Errorf("human output must say Windows Native and WSL2:\n%s", out)
	}
}

func TestOtherKernelUnsupported(t *testing.T) {
	fix := t.TempDir()
	mkFixture(t, fix)
	os.WriteFile(filepath.Join(fix, "os.kernel"), []byte("Darwin\n"), 0o644)
	code, _, _, doc := runDoctor(t, fix, "--json")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if got := doc["verdict"]; got != "unsupported" {
		t.Fatalf("verdict = %v", got)
	}
}

func TestNonUbuntuDistroUnsupported(t *testing.T) {
	fix := t.TempDir()
	mkFixture(t, fix)
	os.WriteFile(filepath.Join(fix, "os.id"), []byte("debian\n"), 0o644)
	os.WriteFile(filepath.Join(fix, "os.version_id"), []byte("12\n"), 0o644)
	code, _, _, doc := runDoctor(t, fix, "--json")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if got := doc["verdict"]; got != "unsupported" {
		t.Fatalf("verdict = %v", got)
	}
	if got := findCheck(t, doc, "ENV-OS-DISTRO", "status"); got != "failed" {
		t.Errorf("ENV-OS-DISTRO status = %q", got)
	}
}

func TestWSL1AdvisoryWarn(t *testing.T) {
	fix := t.TempDir()
	mkFixture(t, fix)
	os.WriteFile(filepath.Join(fix, "os.wsl"), []byte("wsl1\n"), 0o644)
	code, _, _, doc := runDoctor(t, fix, "--json")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := findCheck(t, doc, "ENV-WSL", "status"); got != "warn" {
		t.Errorf("ENV-WSL status = %q", got)
	}
	if got := doc["summary"].(map[string]any)["advisory"].(map[string]any)["warn"]; got != float64(1) {
		t.Errorf("advisory.warn = %v, want 1", got)
	}
}

func TestWSL2RepoOnMntFails(t *testing.T) {
	fix := t.TempDir()
	mkFixture(t, fix)
	os.WriteFile(filepath.Join(fix, "os.wsl"), []byte("wsl2\n"), 0o644)
	os.WriteFile(filepath.Join(fix, "os.repo_path"), []byte("/mnt/c/Users/dev/post\n"), 0o644)
	code, _, _, doc := runDoctor(t, fix, "--json")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if got := findCheck(t, doc, "RES-REPO-FS", "status"); got != "failed" {
		t.Errorf("RES-REPO-FS status = %q", got)
	}
	if rem := findCheck(t, doc, "RES-REPO-FS", "remediation"); !strings.Contains(rem, "/mnt") {
		t.Errorf("remediation %q does not name /mnt", rem)
	}
}

func TestPnpmPinMismatch(t *testing.T) {
	fix := t.TempDir()
	mkFixture(t, fix)
	os.WriteFile(filepath.Join(fix, "pnpm"), []byte("12.0.0\n"), 0o644)
	code, _, _, doc := runDoctor(t, fix, "--json")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if got := findCheck(t, doc, "T-PNPM-PIN", "status"); got != "failed" {
		t.Errorf("T-PNPM-PIN status = %q", got)
	}
	if rem := findCheck(t, doc, "T-PNPM-PIN", "remediation"); !strings.Contains(rem, "12.4.1") {
		t.Errorf("remediation %q does not name the pin", rem)
	}
}

func TestPnpmPinMalformedSkipped(t *testing.T) {
	fix := t.TempDir()
	mkFixture(t, fix)
	repo := filepath.Join(fix, "repo")
	os.WriteFile(filepath.Join(repo, "package.json"), []byte(`{"name":"post","packageManager":"npm@10.0.0"}`+"\n"), 0o644)
	code, _, _, doc := runDoctor(t, fix, "--json")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := findCheck(t, doc, "T-PNPM-PIN", "status"); got != "skipped" {
		t.Errorf("T-PNPM-PIN status = %q", got)
	}
}

func TestDockerDaemonDownWarnAndSkip(t *testing.T) {
	fix := t.TempDir()
	mkFixture(t, fix)
	os.WriteFile(filepath.Join(fix, "res.docker_reachable"), []byte("false\n"), 0o644)
	code, _, _, doc := runDoctor(t, fix, "--json", "--check-docker-daemon")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := findCheck(t, doc, "T-DOCKER-DAEMON", "status"); got != "warn" {
		t.Errorf("T-DOCKER-DAEMON status = %q", got)
	}
	if got := findCheck(t, doc, "RES-DOCKER-DISK", "status"); got != "skipped" {
		t.Errorf("RES-DOCKER-DISK status = %q", got)
	}
}

func TestComposeMissingFails(t *testing.T) {
	fix := t.TempDir()
	mkFixture(t, fix)
	os.Remove(filepath.Join(fix, "docker-compose"))
	os.WriteFile(filepath.Join(fix, "docker-compose.absent"), nil, 0o644)
	code, _, _, doc := runDoctor(t, fix, "--json")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if got := findCheck(t, doc, "T-DOCKER-COMPOSE", "status"); got != "failed" {
		t.Errorf("T-DOCKER-COMPOSE status = %q", got)
	}
	if rem := findCheck(t, doc, "T-DOCKER-COMPOSE", "remediation"); !strings.Contains(rem, "docker-compose-plugin") {
		t.Errorf("remediation %q does not name the plugin", rem)
	}
}

func TestComposeV5Passes(t *testing.T) {
	fix := t.TempDir()
	mkFixture(t, fix)
	os.WriteFile(filepath.Join(fix, "docker-compose"), []byte("Docker Compose version v5.5.1\n"), 0o644)
	code, _, _, doc := runDoctor(t, fix, "--json")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := findCheck(t, doc, "T-DOCKER-COMPOSE", "status"); got != "passed" {
		t.Errorf("T-DOCKER-COMPOSE status = %q", got)
	}
	if got := findCheck(t, doc, "T-DOCKER-COMPOSE", "version"); got != "5.5" {
		t.Errorf("T-DOCKER-COMPOSE version = %q", got)
	}
}

func TestComposeV1Fails(t *testing.T) {
	fix := t.TempDir()
	mkFixture(t, fix)
	os.WriteFile(filepath.Join(fix, "docker-compose"), []byte("Docker Compose version v1.99.9\n"), 0o644)
	code, _, _, doc := runDoctor(t, fix, "--json")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if got := findCheck(t, doc, "T-DOCKER-COMPOSE", "status"); got != "failed" {
		t.Errorf("T-DOCKER-COMPOSE status = %q", got)
	}
	if rem := findCheck(t, doc, "T-DOCKER-COMPOSE", "remediation"); !strings.Contains(rem, "v2 plugin lineage") {
		t.Errorf("remediation %q does not name the v2 plugin lineage", rem)
	}
}

func TestDefaultRunNeverProbesDaemon(t *testing.T) {
	fix := t.TempDir()
	mkFixture(t, fix)
	code, _, _, doc := runDoctor(t, fix, "--json")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := findCheck(t, doc, "RES-DOCKER-DISK", "status"); got != "skipped" {
		t.Errorf("RES-DOCKER-DISK status = %q", got)
	}
	if got := findCheck(t, doc, "T-DOCKER-DAEMON", "status"); got != "skipped" {
		t.Errorf("T-DOCKER-DAEMON status = %q", got)
	}
	if reason := findCheck(t, doc, "RES-DOCKER-DISK", "reason"); !strings.Contains(reason, "--check-docker-daemon") {
		t.Errorf("skip reason %q does not mention the opt-in flag", reason)
	}
}

func TestUsageErrors(t *testing.T) {
	code, out, errOut, _ := runDoctor(t, "", "--bogus")
	if code != 3 {
		t.Fatalf("unknown option exit code = %d, want 3", code)
	}
	if out != "" {
		t.Errorf("stdout on usage error = %q, want empty (no JSON)", out)
	}
	if !strings.Contains(errOut, "unknown option") {
		t.Errorf("stderr %q does not name the option", errOut)
	}

	code, out, errOut, _ = runDoctor(t, "", "--fixture", "/nonexistent-dir-xyz")
	if code != 3 {
		t.Fatalf("bad fixture dir exit code = %d, want 3", code)
	}
	if out != "" {
		t.Errorf("stdout on usage error = %q, want empty", out)
	}
	if !strings.Contains(errOut, "fixture directory not found") {
		t.Errorf("stderr %q does not report the fixture problem", errOut)
	}
}

func TestDeterminismByteIdentical(t *testing.T) {
	fix := t.TempDir()
	mkFixture(t, fix)
	_, j1, _, _ := runDoctor(t, fix, "--json", "--check-docker-daemon")
	_, j2, _, _ := runDoctor(t, fix, "--json", "--check-docker-daemon")
	if j1 != j2 {
		t.Fatalf("JSON outputs differ between two runs:\n--- run 1\n%s\n--- run 2\n%s", j1, j2)
	}
	_, h1, _, _ := runDoctor(t, fix, "--check-docker-daemon")
	_, h2, _, _ := runDoctor(t, fix, "--check-docker-daemon")
	if h1 != h2 {
		t.Fatalf("human outputs differ between two runs:\n--- run 1\n%s\n--- run 2\n%s", h1, h2)
	}
}

func TestRgMissingNamesBinary(t *testing.T) {
	fix := t.TempDir()
	mkFixture(t, fix)
	os.Remove(filepath.Join(fix, "rg"))
	os.WriteFile(filepath.Join(fix, "rg.absent"), nil, 0o644)
	code, _, _, doc := runDoctor(t, fix, "--json")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if got := findCheck(t, doc, "T-RG", "status"); got != "failed" {
		t.Errorf("T-RG status = %q", got)
	}
	if rem := findCheck(t, doc, "T-RG", "remediation"); !strings.Contains(rem, "rg") {
		t.Errorf("remediation %q does not note the binary name", rem)
	}
}

// TestJSONShapeContract verifies the ops/doctor-output.schema.json shape:
// field presence, status/remediation/reason invariants, and summary counts
// consistent with the checks array.
func TestJSONShapeContract(t *testing.T) {
	fix := t.TempDir()
	mkFixture(t, fix)
	code, _, _, doc := runDoctor(t, fix, "--json", "--check-docker-daemon")
	if code != 0 {
		t.Fatal(code)
	}
	if doc["schema_version"] != float64(1) {
		t.Errorf("schema_version = %v", doc["schema_version"])
	}
	if doc["contract"] != "ops/doctor-checks.md" {
		t.Errorf("contract = %v", doc["contract"])
	}
	env := doc["environment"].(map[string]any)
	for _, k := range []string{"kernel", "arch", "distro_id", "distro_version_id", "distro_pretty", "codename", "wsl", "repo_path", "repo_fs_mount"} {
		if _, ok := env[k]; !ok {
			t.Errorf("environment missing %q", k)
		}
	}
	if env["wsl"] != "none" {
		t.Errorf("environment.wsl = %v", env["wsl"])
	}
	baselines := doc["baselines"].(map[string]any)
	for _, k := range []string{"go", "node", "python3", "psql", "redis-cli", "git", "git-lfs", "jq", "make", "shellcheck", "docker-compose", "claude", "uv", "pnpm", "cpu", "ram", "swap", "disk", "fd", "docker-disk"} {
		if _, ok := baselines[k]; !ok {
			t.Errorf("baselines missing %q", k)
		}
	}
	if baselines["go"] != "1.27.x" {
		t.Errorf("baselines.go = %v", baselines["go"])
	}

	// Fixed check order and id set.
	checks := doc["checks"].([]any)
	if len(checks) != 35 {
		t.Fatalf("checks count = %d, want 35", len(checks))
	}
	wantOrder := []string{
		"ENV-OS-KERNEL", "ENV-OS-DISTRO", "ENV-OS-ARCH", "ENV-WSL",
		"RES-CPU", "RES-CPU-TIER", "RES-RAM", "RES-RAM-TIER", "RES-SWAP", "RES-SWAP-TIER",
		"RES-DISK", "RES-DISK-TIER", "RES-FD", "RES-DOCKER-DISK", "RES-REPO-FS", "RES-BWRAP", "RES-SOCAT",
		"T-GIT", "T-GIT-LFS", "T-CLAUDE", "T-GO", "T-NODE", "T-PNPM", "T-PNPM-PIN",
		"T-PYTHON3", "T-UV", "T-DOCKER", "T-DOCKER-COMPOSE", "T-DOCKER-DAEMON",
		"T-JQ", "T-PSQL", "T-REDIS-CLI", "T-MAKE", "T-RG", "T-SHELLCHECK",
	}
	reqTotal, reqPassed, reqFailed, reqSkipped := 0, 0, 0, 0
	advTotal, advPassed, advWarn, advSkipped := 0, 0, 0, 0
	for i, c := range checks {
		cm := c.(map[string]any)
		if cm["id"] != wantOrder[i] {
			t.Fatalf("checks[%d].id = %v, want %s (order is contract)", i, cm["id"], wantOrder[i])
		}
		for _, k := range []string{"id", "name", "category", "severity", "status", "measured", "version", "baseline", "remediation", "reason"} {
			if _, ok := cm[k]; !ok {
				t.Errorf("%s missing field %q", cm["id"], k)
			}
		}
		status := cm["status"].(string)
		if cm["remediation"] == nil != (status != "failed" && status != "warn") {
			t.Errorf("%s: remediation %v inconsistent with status %q", cm["id"], cm["remediation"], status)
		}
		if cm["reason"] == nil != (status != "skipped") {
			t.Errorf("%s: reason %v inconsistent with status %q", cm["id"], cm["reason"], status)
		}
		severity := cm["severity"].(string)
		if severity == "required" {
			reqTotal++
			switch status {
			case "passed":
				reqPassed++
			case "failed":
				reqFailed++
			case "skipped":
				reqSkipped++
			default:
				t.Errorf("%s: required check with status %q", cm["id"], status)
			}
		} else {
			advTotal++
			switch status {
			case "passed":
				advPassed++
			case "warn":
				advWarn++
			case "skipped":
				advSkipped++
			default:
				t.Errorf("%s: advisory check with status %q", cm["id"], status)
			}
		}
	}
	sum := doc["summary"].(map[string]any)
	req := sum["required"].(map[string]any)
	if req["total"] != float64(reqTotal) || req["passed"] != float64(reqPassed) ||
		req["failed"] != float64(reqFailed) || req["skipped"] != float64(reqSkipped) {
		t.Errorf("required summary %v inconsistent with checks (%d/%d/%d/%d)", req, reqTotal, reqPassed, reqFailed, reqSkipped)
	}
	adv := sum["advisory"].(map[string]any)
	if adv["total"] != float64(advTotal) || adv["passed"] != float64(advPassed) ||
		adv["warn"] != float64(advWarn) || adv["skipped"] != float64(advSkipped) {
		t.Errorf("advisory summary %v inconsistent with checks (%d/%d/%d/%d)", adv, advTotal, advPassed, advWarn, advSkipped)
	}
}

// repoRoot resolves the repository root from this package directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// internal/devorchestrator/doctor -> repo root
	abs, err := filepath.Abs(filepath.Join(dir, "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// TestParityWithBashReference asserts the Go port reproduces the reference
// implementation byte-for-byte for identical fixture inputs (JSON output,
// and human output from line 2 on — line 1 carries the script name).
func TestParityWithBashReference(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available; parity test skipped")
	}
	root := repoRoot(t)
	script := filepath.Join(root, "ops", "doctor.sh")
	if _, err := os.Stat(script); err != nil {
		t.Skip("ops/doctor.sh not present; parity test skipped")
	}
	fix := t.TempDir()
	mkFixture(t, fix)

	for _, flags := range [][]string{{"--json", "--check-docker-daemon"}, {"--check-docker-daemon"}} {
		jsonMode := flags[0] == "--json"
		cmd := exec.Command(bash, script, "--fixture", fix)
		cmd.Args = append(cmd.Args, flags...)
		cmd.Env = append(os.Environ(), "LC_ALL=C")
		bashOut, err := cmd.Output()
		if err != nil {
			t.Fatalf("reference ops/doctor.sh failed: %v", err)
		}

		var goOut bytes.Buffer
		opts := &Options{FixtureDir: fix, CheckDockerDaemon: true, JSON: jsonMode, ScriptName: "rddev doctor"}
		code := opts.Run(&goOut)
		if jsonMode && code != 0 {
			t.Fatalf("rddev doctor exit code = %d, want 0", code)
		}

		if jsonMode {
			if !bytes.Equal(bashOut, goOut.Bytes()) {
				t.Fatalf("JSON parity broken (bash %d bytes vs go %d bytes):\n--- ops/doctor.sh\n%s\n--- rddev doctor\n%s",
					len(bashOut), goOut.Len(), bashOut, goOut.Bytes())
			}
		} else {
			bashLines := strings.Split(string(bashOut), "\n")
			goLines := strings.Split(goOut.String(), "\n")
			// Line 1 is the header (script name differs); the rest must match.
			if len(bashLines) != len(goLines) {
				t.Fatalf("human line count differs: bash %d vs go %d", len(bashLines), len(goLines))
			}
			for i := 1; i < len(bashLines); i++ {
				if bashLines[i] != goLines[i] {
					t.Fatalf("human parity broken at line %d:\nbash: %q\ngo:   %q", i+1, bashLines[i], goLines[i])
				}
			}
		}
	}
}
