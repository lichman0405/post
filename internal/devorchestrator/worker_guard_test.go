package devorchestrator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
// mode, budget, and optional timeout wrapper.
func TestClaudeArgs(t *testing.T) {
	budget := 1.25
	opts := &SpawnOpts{TaskID: "T0001", MaxBudgetUSD: &budget, Model: "deepseek-v4-pro"}
	args, err := claudeArgs(opts, "sess-1", "/s/worker-settings.json", "/s/system.md", "do the task")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"-p do the task", "--session-id sess-1", "--permission-mode dontAsk", "--output-format stream-json", "--verbose", "--max-budget-usd 1.25", "--model deepseek-v4-pro", "--settings /s/worker-settings.json"} {
		if !strings.Contains(joined, want) {
			t.Errorf("claude args missing %q: %v", want, args)
		}
	}
	// the timeout wrapper must surround the claude binary itself — the reaper
	// prepends nothing, so a wrapper inside claudeArgs would be passed to
	// claude as its first argument (regression: `claude timeout ... 1s -p`
	// silently ran without any timeout)
	opts.Timeout = 90 * time.Second
	args, err = claudeArgs(opts, "s", "/s", "/m", "p")
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
