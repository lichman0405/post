package devorchestrator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGitBin writes a `git`/`gh` recorder into binDir: every invocation
// appends its argument line to the marker file. The refusal tests assert the
// marker stays empty — the tooling must exit before any control-plane
// invocation.
func fakeGitBin(t *testing.T) (binDir, marker string) {
	t.Helper()
	binDir = t.TempDir()
	marker = filepath.Join(binDir, "invocations.log")
	for _, name := range []string{"git", "gh"} {
		script := "#!/bin/sh\necho \"$0 $*\" >> \"$FAKE_INVOCATIONS\"\nexit 0\n"
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The marker exists from the start: an empty file means "never invoked".
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_INVOCATIONS", marker)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return binDir, marker
}

// TestGitControlRefusesRedGateBeforeAnyInvocation is T0012 requirement 4:
// "a blocking red gate prevents merge — demonstrated, not asserted". With
// zero evidence records on disk, every control-plane action refuses with a
// GateRefusalError carrying the reasons, and the fake git/gh proves NO
// control-plane binary was ever invoked.
func TestGitControlRefusesRedGateBeforeAnyInvocation(t *testing.T) {
	repoRoot, specPath := writeGateSpec(t, miniGateSpec) // red: no collect, no G2
	_, marker := fakeGitBin(t)
	for _, action := range []string{"commit", "push", "pr-open", "pr-merge"} {
		_, err := RunGitControl(&GitControlOpts{
			RepoRoot: repoRoot, GatesPath: specPath, TaskID: "T0001",
		}, action)
		refusal, ok := err.(*GateRefusalError)
		if !ok {
			t.Fatalf("%s: error = %v, want GateRefusalError", action, err)
		}
		if len(refusal.Reasons) == 0 {
			t.Errorf("%s: refusal carries no reasons", action)
		}
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Fatalf("git/gh was invoked despite the red gate:\n%s", data)
	}
}

// TestGitControlCommitOnGreenGate: the legitimate neighbour — with green
// evidence, commit runs real git in the task worktree, records the sha, and
// writes the GitRecord evidence.
func TestGitControlCommitOnGreenGate(t *testing.T) {
	repoRoot, specPath := mergeGateFixture(t) // green: ok collect + green G2
	taskID := "T0001"

	// A real scratch worktree on the task branch with an uncommitted change.
	wt := filepath.Join(t.TempDir(), "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = wt
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	// Hermetic: configure an identity IN the scratch repo. The code under test
	// (CommitTask) shells out to `git commit` and must inherit one; relying on the
	// ambient developer/CI git config made this test pass locally and fail on a
	// bare CI runner with "Author identity unknown". Tests must not depend on
	// developer machine state.
	git("config", "user.name", "test")
	git("config", "user.email", "test@test")
	if err := os.WriteFile(filepath.Join(wt, "file.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("-c", "user.name=test", "-c", "user.email=test@test", "commit", "-q", "-m", "initial")
	git("checkout", "-q", "-b", "task/T0001-x")
	if err := os.WriteFile(filepath.Join(wt, "file.txt"), []byte("hello worker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	baseline := git("rev-parse", "HEAD")

	taskDir := filepath.Join(repoRoot, "workers", taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SaveRegistry(repoRoot, &WorkerRecord{
		TaskID: taskID, RunID: "run-1", Worktree: wt, Branch: "task/T0001-x",
		BaselineSHA: baseline, ResultDir: taskDir, StartedAt: "2026-09-12T10:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	sha, err := CommitTask(&GitControlOpts{
		RepoRoot: repoRoot, GatesPath: specPath, TaskID: taskID,
		CommitMsg: "worker change\n", DagPath: filepath.Join(repoRoot, "x"), StatePath: filepath.Join(repoRoot, "y"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if sha == "" {
		t.Fatal("commit returned no sha")
	}
	head := git("rev-parse", "HEAD")
	if head != sha {
		t.Errorf("worktree HEAD = %s, commit sha = %s", head, sha)
	}
	// The evidence trail: a git record exists for the commit.
	rec, ok, err := LatestRecord[GitRecord](repoRoot, taskID, RecordGit)
	if err != nil || !ok {
		t.Fatalf("no git record after commit: %v %v", ok, err)
	}
	if rec.Action != "commit" || rec.CommitSHA != sha || rec.GateStatus != "passed" {
		t.Errorf("git record = %+v, want commit %s under gate passed", rec, sha)
	}
}

// TestGateRefusalErrorMessage: the refusal error names the action and every
// reason — the Supervisor's stderr is the rejection evidence.
func TestGateRefusalErrorMessage(t *testing.T) {
	err := &GateRefusalError{Action: "pr merge", Reasons: []string{"G2 red", "no review"}}
	msg := err.Error()
	if !strings.Contains(msg, "pr merge") || !strings.Contains(msg, "G2 red") || !strings.Contains(msg, "no review") {
		t.Errorf("error = %q, want action and all reasons", msg)
	}
}

// The review input must be the COMPLETE change. `git diff` shows only tracked
// changes, and a task that adds files adds them untracked — so T0101's
// diff.txt carried 9 of its 39 changed paths, and a Reviewer trusting the
// document would have reviewed under a quarter of the work. The Reviewer that
// caught this read the worktree instead; the document must not depend on that.
func TestWorktreeDiffIncludesUntrackedFiles(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "-A")
	runGit("commit", "-q", "-m", "baseline")
	baseline := runGit("rev-parse", "HEAD")

	// One tracked modification and one NEW file, as a real task has.
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	newFile := filepath.Join(dir, "new_auth_test.go")
	if err := os.WriteFile(newFile, []byte("package x\n\nfunc TestX(t *testing.T) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff, err := taskWorktreeDiff(&WorkerRecord{Worktree: dir, BaselineSHA: baseline})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "after") {
		t.Errorf("the tracked modification is missing from the review diff:\n%s", diff)
	}
	if !strings.Contains(diff, "new_auth_test.go") {
		t.Errorf("the NEW file is missing from the review diff — a Reviewer trusting this document reviews a fraction of the change:\n%s", diff)
	}
	if !strings.Contains(diff, "func TestX(t *testing.T) {}") {
		t.Errorf("the new file's contents are missing, only its name appears:\n%s", diff)
	}
}
