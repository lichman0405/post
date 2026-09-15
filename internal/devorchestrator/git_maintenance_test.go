package devorchestrator

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Fixture repositories must not be able to spawn a process that outlives the
// git command that started it.
//
// Every test in this package that builds a repo (init, commit, worktree add)
// builds it under t.TempDir(), and t.TempDir()'s cleanup is a RemoveAll whose
// last act is an rmdir of each directory. If anything creates an entry in
// <repo>/.git between the read of that directory and the rmdir, cleanup fails
// with
//
//	TempDir RemoveAll cleanup: unlinkat /tmp/.../.git: directory not empty
//
// which is exactly what CI reported once, on 433fbc0, in
// TestReviewVerdictIsBoundToTheCodeItJudged: the test body passed and only the
// removal failed. A fixture repo is written to by nobody but the test, so the
// writer has to be a process the test did not wait for.
//
// The one mechanism by which a git command leaves one behind is git's own
// auto-maintenance: `git commit` and `git merge` run `git maintenance run
// --auto`, and with the default maintenance.autoDetach=true the work happens
// in a detached child. A repo this small gives that child nothing to do — but
// it is still not a process the fixture waited for, and the fixture is
// entitled to nothing from it.
//
// This is not a fix aimed at a reproduced trace: 12 runs of this package and
// 40 runs of the failing test alone did not reproduce it, so a targeted fix
// could not be confirmed either. It removes an ASSUMPTION instead — that the
// tool the fixtures shell out to will not leave a writer behind — which is the
// same move as L1-20260912-56 (wait for the precondition rather than assume
// it) and holds whatever the writer turns out to be. Two knobs close it at the
// source: gc.auto=0 takes the gc task off the auto list, maintenance.auto=false
// stops the automatic run itself.
//
// The config goes through GIT_CONFIG_COUNT/KEY_n/VALUE_n (git >= 2.31, the
// channel internal/gitprovider/gitea.go already uses) rather than a config
// file because it is ADDITIVE: it does not replace the user's or the system's
// config, so nothing else the tests rely on moves. Every fixture builds its
// child environment with append(os.Environ(), ...), so one injection here
// reaches every git invocation in the package without editing ten fixtures.
//
// The guard for it is TestFixtureReposCarryNoAutoMaintenance; if this ever
// stops taking effect, that test says so directly instead of leaving the
// cleanup failure to be rediscovered as a flake.
func TestMain(m *testing.M) {
	injectGitConfig(map[string]string{
		"gc.auto":          "0",
		"maintenance.auto": "false",
	})
	os.Exit(m.Run())
}

// injectGitConfig adds config entries through git's environment channel,
// appending to a count the surrounding environment already set rather than
// overwriting it: a GIT_CONFIG_COUNT this process inherited describes entries
// whose keys and values are still in the environment, and renumbering from
// zero would silently drop them.
func injectGitConfig(entries map[string]string) {
	base := 0
	if n, err := strconv.Atoi(os.Getenv("GIT_CONFIG_COUNT")); err == nil && n > 0 {
		base = n
	}
	// Sorted, so the injected numbering depends on the map's contents and not
	// on Go's iteration order.
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		setenv(fmt.Sprintf("GIT_CONFIG_KEY_%d", base+i), k)
		setenv(fmt.Sprintf("GIT_CONFIG_VALUE_%d", base+i), entries[k])
	}
	setenv("GIT_CONFIG_COUNT", strconv.Itoa(base+len(keys)))
}

// setenv fails the build's test run loudly: an environment this process cannot
// write to makes the injection above a no-op, and a no-op would be read as
// "the fixtures are covered" when they are not.
func setenv(key, value string) {
	if err := os.Setenv(key, value); err != nil {
		panic(fmt.Sprintf("setting %s for the fixture git injection: %v", key, err))
	}
}

// TestFixtureReposCarryNoAutoMaintenance is the guard for the injection above:
// it builds a repo the way the fixtures do and asserts that a git run inside
// it — through the same append(os.Environ(), ...) environment they use — sees
// both knobs off. It asserts the injection, not anything about the repo, so a
// green run means the assumption the fixtures now make is actually in force.
func TestFixtureReposCarryNoAutoMaintenance(t *testing.T) {
	repo := t.TempDir()
	wt := filepath.Join(t.TempDir(), "wt")
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v (in %s): %v\n%s", args, dir, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(repo, "add", "-A")
	git(repo, "commit", "-q", "-m", "base")
	git(repo, "worktree", "add", "-q", "-b", "task/x", wt, git(repo, "rev-parse", "HEAD"))

	for _, tc := range []struct{ key, want string }{
		{"gc.auto", "0"},
		{"maintenance.auto", "false"},
	} {
		// Read through the tolerant variant: an UNSET key exits 1, and that
		// is one of the two ways this guard should fail, so it has to be a
		// readable value here rather than a fatal from the helper.
		cmd := exec.Command("git", "config", "--get", tc.key)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e")
		out, _ := cmd.Output()
		if got := strings.TrimSpace(string(out)); got != tc.want {
			t.Errorf("git config --get %s in a fixture repo = %q, want %q — the fixture git injection is not in force, so a git command in this package may still leave a detached auto-maintenance process writing into the repo after it returns",
				tc.key, got, tc.want)
		}
	}
}
