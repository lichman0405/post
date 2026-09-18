package backupdr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// T1111. The drill's git credential must ride the process environment, not
// argv: argv is readable by every account on the machine (`ps aux`,
// /proc/<pid>/cmdline), so a token in a `-c key=value` argument is a
// published token. These tests pin the invocation itself — what the child
// process is handed — because that is the only place the property lives; the
// source of git() can be read either way.

const testDrillToken = "drill-token-9f3c1a2b4d5e"

// gitInvocation is one recorded call to the git seam.
type gitInvocation struct {
	bin  string
	dir  string
	env  []string
	args []string
}

// recordGit substitutes the git seam: every invocation is recorded and
// answered with the given stdout (and nil error).
func recordGit(calls *[]gitInvocation, stdout string) gitRunner {
	return func(_ context.Context, bin, dir string, env []string, args ...string) ([]byte, []byte, error) {
		*calls = append(*calls, gitInvocation{
			bin: bin, dir: dir, env: env, args: append([]string(nil), args...),
		})
		return []byte(stdout), nil, nil
	}
}

// envValue resolves one key the way a child process sees it: the LAST
// occurrence wins, which is what exec's de-duplication produces when a key
// is both inherited and set by the caller.
func envValue(env []string, key string) (string, bool) {
	value, found := "", false
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			value, found = v, true
		}
	}
	return value, found
}

func newTestGitClient(token string) *gitClient {
	return newGitClient("http://127.0.0.1:3000", token, "t1111")
}

// TestGitTokenRidesInEnvNotArgv is the test this task exists for: the token
// reaches git through the environment and through NO argv element. It fails
// both ways a regression could arrive — a `-c http.extraHeader=...` argument
// (token in argv) and an invocation that simply forgets the credential
// (token nowhere), which git would answer with an anonymous request.
func TestGitTokenRidesInEnvNotArgv(t *testing.T) {
	var calls []gitInvocation
	g := newTestGitClient(testDrillToken)
	g.runner = recordGit(&calls, "refs\n")

	const url = "http://127.0.0.1:3000/drill-src/repo.git"
	out, err := g.git(context.Background(), "/scratch/mirror", "ls-remote", "--heads", url)
	if err != nil {
		t.Fatalf("git: %v", err)
	}
	if string(out) != "refs\n" {
		t.Errorf("stdout = %q, want the runner's stdout returned unchanged", out)
	}
	if len(calls) != 1 {
		t.Fatalf("git invocations = %d, want 1", len(calls))
	}
	call := calls[0]

	// The arguments are the caller's, in order, with nothing prepended.
	wantArgs := []string{"ls-remote", "--heads", url}
	if strings.Join(call.args, " ") != strings.Join(wantArgs, " ") || len(call.args) != len(wantArgs) {
		t.Errorf("argv = %v, want %v (no `-c` prefix)", call.args, wantArgs)
	}
	for i, a := range call.args {
		if strings.Contains(a, testDrillToken) {
			t.Errorf("argv[%d] = %q carries the token — argv is world-readable", i, a)
		}
	}
	if strings.Contains(strings.Join(call.args, " "), "extraHeader") {
		t.Errorf("argv = %v still passes a config override as an argument", call.args)
	}

	// The credential is in the environment, in the exact shape
	// internal/gitprovider/gitea.go uses.
	if got, ok := envValue(call.env, "GIT_CONFIG_COUNT"); !ok || got != "2" {
		t.Errorf("GIT_CONFIG_COUNT = %q (present: %v), want 2", got, ok)
	}
	if got, ok := envValue(call.env, "GIT_CONFIG_KEY_0"); !ok || got != "http.extraHeader" {
		t.Errorf("GIT_CONFIG_KEY_0 = %q (present: %v), want http.extraHeader", got, ok)
	}
	if got, ok := envValue(call.env, "GIT_CONFIG_VALUE_0"); !ok || got != "Authorization: Bearer "+testDrillToken {
		t.Errorf("GIT_CONFIG_VALUE_0 = %q (present: %v), want the Authorization header", got, ok)
	}
	// The second entry is the empty credential.helper the `-c
	// credential.helper=` argument used to carry: an empty value, present.
	if got, ok := envValue(call.env, "GIT_CONFIG_KEY_1"); !ok || got != "credential.helper" {
		t.Errorf("GIT_CONFIG_KEY_1 = %q (present: %v), want credential.helper", got, ok)
	}
	value1, ok := envValue(call.env, "GIT_CONFIG_VALUE_1")
	if !ok || value1 != "" {
		t.Errorf("GIT_CONFIG_VALUE_1 = %q (present: %v), want present and empty", value1, ok)
	}
	if got, ok := envValue(call.env, "GIT_TERMINAL_PROMPT"); !ok || got != "0" {
		t.Errorf("GIT_TERMINAL_PROMPT = %q (present: %v), want 0", got, ok)
	}

	// Behaviour that must not have moved: the working directory, and the
	// binary git resolved.
	if call.dir != "/scratch/mirror" {
		t.Errorf("dir = %q, want the caller's working directory", call.dir)
	}
	if !strings.HasSuffix(call.bin, "git") {
		t.Errorf("bin = %q, want the resolved git binary", call.bin)
	}
}

// TestGitFailureTextIsRedactedAndUnchanged: the error text is the surface
// two callers and every report read, and it is also the SECOND credential
// path (git quotes the header it was given). The environment change must
// leave it byte-identical — same prefix, same wrapped exit error, same
// redaction.
func TestGitFailureTextIsRedactedAndUnchanged(t *testing.T) {
	g := newTestGitClient(testDrillToken)
	g.runner = func(context.Context, string, string, []string, ...string) ([]byte, []byte, error) {
		return nil, []byte("fatal: unable to access 'http://127.0.0.1:3000/o/n.git/': " +
				"the requested URL returned error: 403\nremote: Authorization: Bearer " + testDrillToken + "\n"),
			errors.New("exit status 128")
	}
	_, err := g.git(context.Background(), "/scratch/mirror", "push", "--mirror", "--quiet", "http://127.0.0.1:3000/o/n.git")
	if err == nil {
		t.Fatal("git push on a refusing remote = nil error, want the failure")
	}
	want := "git push: exit status 128: fatal: unable to access 'http://127.0.0.1:3000/o/n.git/': " +
		"the requested URL returned error: 403\nremote: Authorization: Bearer [redacted]\n"
	if err.Error() != want {
		t.Errorf("error text =\n%q\nwant\n%q", err.Error(), want)
	}
	if strings.Contains(err.Error(), testDrillToken) {
		t.Error("the token reached the error text — redactSecrets must keep running")
	}
}

// TestGitNotFoundOnPATH is the other fixed error text: it is returned before
// the runner is reached and must survive the seam unchanged.
func TestGitNotFoundOnPATH(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // a PATH with no git on it
	g := newTestGitClient(testDrillToken)
	called := false
	g.runner = func(context.Context, string, string, []string, ...string) ([]byte, []byte, error) {
		called = true
		return nil, nil, nil
	}
	_, err := g.git(context.Background(), "", "ls-remote", "http://127.0.0.1:3000/o/n.git")
	if err == nil || !strings.HasPrefix(err.Error(), "git: not on PATH: ") {
		t.Errorf("error = %v, want the `git: not on PATH` message", err)
	}
	if called {
		t.Error("the runner ran with no git on PATH")
	}
}

// TestGitEnvReachesTheRealGitBinary runs the REAL git binary with the real
// runner and asks it what the header resolves to. The recording-runner test
// above shows what the drill hands over; this one shows git itself accepting
// it — a header that never lands in git's config would fail every clone with
// an anonymous 401, and a credential that "passes" only in a recorded slice
// would be a test measuring itself.
//
// The ambient environment is hostile on purpose: an inherited
// GIT_CONFIG_COUNT already describing http.extraHeader. exec keeps the LAST
// value for a repeated key, which is exactly why gitEnv's entries win.
func TestGitEnvReachesTheRealGitBinary(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "http.extraHeader")
	t.Setenv("GIT_CONFIG_VALUE_0", "Authorization: Bearer ambient-token-1a2b")

	g := newTestGitClient(testDrillToken)
	out, err := g.git(context.Background(), "", "config", "--get", "http.extraHeader")
	if err != nil {
		t.Fatalf("git config --get http.extraHeader: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "Authorization: Bearer "+testDrillToken {
		t.Errorf("git resolved http.extraHeader = %q, want the drill's own header (an inherited config entry must not win)", got)
	}

	// And the same invocation, read from the outside: a real child process
	// prints its own argv and environment. This is the claim under test in
	// the form the attacker has it — what /proc/<pid>/cmdline shows.
	argv, env := realChildArgvAndEnv(t, g)
	if strings.Contains(strings.Join(argv, " "), testDrillToken) {
		t.Errorf("the token is in a real child process's argv: %v", argv)
	}
	if strings.Contains(strings.Join(argv, " "), "extraHeader") {
		t.Errorf("a config override is still an argument: %v", argv)
	}
	if !containsLine(env, "GIT_CONFIG_VALUE_0=Authorization: Bearer "+testDrillToken) {
		// Only the config entries are printed: a child's whole environment
		// carries the ambient credentials of whoever ran the test.
		t.Errorf("the token is not in the real child's environment: %v", gitConfigEntries(env))
	}
}

// gitConfigEntries returns the child's GIT_* entries — the ones this test
// is about — so a failure report does not print the runner's environment.
func gitConfigEntries(env []string) []string {
	var out []string
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_") {
			out = append(out, kv)
		}
	}
	return out
}

// realChildArgvAndEnv runs one drill git invocation against a stand-in for
// the git binary that prints the argv and environment it was executed with,
// and returns them as the child reported them.
func realChildArgvAndEnv(t *testing.T, g *gitClient) (argv, env []string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "git")
	const body = "#!/bin/sh\nprintf 'ARGV\\n'\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done\nprintf 'ENV\\n'\nenv\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write the stand-in git: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, err := g.git(context.Background(), "", "ls-remote", "--heads", "http://127.0.0.1:3000/o/n.git")
	if err != nil {
		t.Fatalf("stand-in git: %v", err)
	}
	parts := strings.SplitN(string(out), "ENV\n", 2)
	if len(parts) != 2 {
		t.Fatalf("the stand-in printed no ENV section: %q", out)
	}
	for _, line := range strings.Split(strings.TrimPrefix(parts[0], "ARGV\n"), "\n") {
		if line != "" {
			argv = append(argv, line)
		}
	}
	for _, line := range strings.Split(parts[1], "\n") {
		if line != "" {
			env = append(env, line)
		}
	}
	if len(argv) == 0 || len(env) == 0 {
		t.Fatalf("the stand-in reported nothing to read: argv %v env %d entries", argv, len(env))
	}
	return argv, env
}

func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

// TestRedactSecretsUntouched pins the OTHER credential path: T1111 moved the
// header into the environment and must not have touched the redaction that
// keeps git's error output from quoting it.
func TestRedactSecretsUntouched(t *testing.T) {
	const secret = "a-long-enough-secret"
	if got := redactSecrets("boom "+secret+" boom", secret); got != "boom [redacted] boom" {
		t.Errorf("redactSecrets = %q, want the secret replaced", got)
	}
	if got := redactSecrets("short", "abc"); got != "short" {
		t.Errorf("redactSecrets = %q, want a too-short value left alone", got)
	}
}
