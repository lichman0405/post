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
	// `gh pr checks` must answer with parseable JSON: pr merge refuses while
	// the PR's required checks are not green, so a fake that stays silent
	// would make every merge test fail for the wrong reason.
	ghChecks := `[{"name":"job-a","state":"SUCCESS"},{"name":"job-b","state":"SUCCESS"}]`
	for _, name := range []string{"git", "gh"} {
		script := "#!/bin/sh\necho \"$0 $*\" >> \"$FAKE_INVOCATIONS\"\n"
		if name == "gh" {
			script += "case \"$1 $2\" in\n  \"pr checks\") printf '%s' '" + ghChecks + "';;\nesac\n"
		}
		script += "exit 0\n"
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

// A diff IS a patch, and a patch's trailing whitespace is part of it: `git diff`
// writes an empty context line as a single space, and the last line of a diff is
// often one. The trim every other gitOutput caller wants deleted that space, and
// the final hunk was then one line short of the count in its own @@ header — so
// `git apply` called the whole patch corrupt and G2 never ran, for a task whose
// only real problem was being behind main.
//
// The assertion is the one the gate makes: apply it, into a tree at the baseline
// the way prepareIntegrationTree does. Checking for the trailing space is not
// enough on its own — a diff can keep its bytes and still be unappliable — so
// this test also fails if the fixture stops ending on a blank context line,
// which would quietly retire the case.
func TestWorktreeDiffEndsOnABlankContextLineApplies(t *testing.T) {
	dir := t.TempDir()
	runGitIn := func(where string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = where
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, where, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGitIn(dir, "init", "-q")
	// The file ends on a blank line, so the hunk that edits its first line ends
	// with a context line that is empty — which git renders as " ".
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("line1\nline2\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitIn(dir, "add", "-A")
	runGitIn(dir, "commit", "-q", "-m", "baseline")
	baseline := runGitIn(dir, "rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("LINE1\nline2\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff, err := taskWorktreeDiff(&WorkerRecord{Worktree: dir, BaselineSHA: baseline})
	if err != nil {
		t.Fatal(err)
	}
	// A tree at the baseline, as prepareIntegrationTree builds one from main.
	other := t.TempDir()
	runGitIn(other, "clone", "-q", dir, ".")
	patch := filepath.Join(t.TempDir(), "change.patch")
	if err := os.WriteFile(patch, []byte(diff), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", other, "apply", patch).CombinedOutput(); err != nil {
		t.Fatalf("git apply rejected the diff the integration tree is built from: %v\n%s", err, out)
	}
	after, err := os.ReadFile(filepath.Join(other, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != "LINE1\nline2\n\n" {
		t.Errorf("the patch applied but produced %q, want the Worker's content", after)
	}

	// Last, so that a diff which no longer exercises the case fails the apply
	// above rather than being silently retired here.
	if !strings.HasSuffix(diff, "\n \n") {
		t.Fatalf("the fixture no longer ends on a blank context line, so this test would not have exercised the case it exists for; the diff ends %q", tail(diff, 20))
	}
}

// tail is the last n bytes of s, for an error message that has to show why a
// prefix/suffix assertion failed without dumping a whole diff.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// The untracked paths come from the Worker's own tree, so following a symlink
// would let the Worker make the SUPERVISOR read an arbitrary file and embed it
// in the review input — a durable artifact. Lstat, never Stat: a symlink is
// recorded as its target path, the way Git records it, and the target is never
// opened.
func TestWorktreeDiffDoesNotFollowSymlinksOutOfTheTree(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(t.TempDir(), "outside-secret.txt")
	if err := os.WriteFile(secret, []byte("SUPER-SECRET-CONTENT\n"), 0o600); err != nil {
		t.Fatal(err)
	}
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
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "-A")
	runGit("commit", "-q", "-m", "baseline")
	baseline := runGit("rev-parse", "HEAD")

	if err := os.Symlink(secret, filepath.Join(dir, "innocent.txt")); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}

	diff, err := taskWorktreeDiff(&WorkerRecord{Worktree: dir, BaselineSHA: baseline})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(diff, "SUPER-SECRET-CONTENT") {
		t.Errorf("the review diff followed a symlink and embedded the target's contents:\n%s", diff)
	}
	if !strings.Contains(diff, "innocent.txt") {
		t.Errorf("the symlink is missing from the diff entirely — it should be recorded as a link, not omitted:\n%s", diff)
	}
	if !strings.Contains(diff, "120000") {
		t.Errorf("the symlink was not recorded with Git's symlink mode 120000:\n%s", diff)
	}
}

// A green LOCAL G2 record is a replica of CI, not CI. The historical defect
// this tooling exists to prevent was a merge that happened while GitHub's own
// CI was red, and nothing here checked GitHub at all: MergePR ran the local
// four-gate assertion and then invoked `gh pr merge` unconditionally.
func TestMergeRefusesWhileThePRChecksAreRed(t *testing.T) {
	repoRoot, specPath := mergeGateFixture(t) // green collect + green G2 on disk
	binDir := t.TempDir()
	marker := filepath.Join(binDir, "invocations.log")
	// A gh whose `pr checks` reports the second required job red.
	script := "#!/bin/sh\necho \"$0 $*\" >> \"$FAKE_INVOCATIONS\"\n" +
		"case \"$1 $2\" in\n" +
		"  \"pr checks\") printf '%s' '[{\"name\":\"job-a\",\"state\":\"SUCCESS\"},{\"name\":\"job-b\",\"state\":\"FAILURE\"}]';;\n" +
		"  \"pr merge\") echo MERGED;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	writeWorktreeRecord(t, repoRoot)
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_INVOCATIONS", marker)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := MergePR(&GitControlOpts{RepoRoot: repoRoot, GatesPath: specPath, TaskID: "T0001"})
	if err == nil {
		t.Fatal("pr merge succeeded while a required check was failing on GitHub")
	}
	if !strings.Contains(err.Error(), "job-b") {
		t.Errorf("refusal does not name the failing check: %v", err)
	}
	if invoked, _ := os.ReadFile(marker); strings.Contains(string(invoked), "pr merge") {
		t.Errorf("gh pr merge was invoked despite the red check:\n%s", invoked)
	}
}

// The other half of the same refusal, and the one that actually happened.
// `gh pr checks` exits 1 both when a check ran and failed and when no check has
// run at all — a PR pushed one second ago — and it says which only on stderr,
// with nothing on stdout. A runner that keeps stdout and drops the rest reports
// both as `exit status 1`, and the caller cannot tell a WAIT from a DECISION:
// stepAccepted retries while the text says "no checks reported" and escalates
// on everything else, so flattening the reason escalated a PR whose CI had not
// started yet, as "merge failed — needs the Supervisor".
//
// So the assertion is deliberately about the text and not only the refusal. The
// substring is the contract between this function and the driver's retry.
func TestMergeRefusesWhileThePRHasNoChecksYetAndStillSaysWhy(t *testing.T) {
	repoRoot, specPath := mergeGateFixture(t) // green collect + green G2 on disk
	binDir := t.TempDir()
	marker := filepath.Join(binDir, "invocations.log")
	// gh's own wording, taken from the binary, on stderr; stdout stays empty.
	script := "#!/bin/sh\necho \"$0 $*\" >> \"$FAKE_INVOCATIONS\"\n" +
		"case \"$1 $2\" in\n" +
		"  \"pr checks\") echo \"no checks reported on the 'task/T0001-x' branch\" >&2; exit 1;;\n" +
		"  \"pr merge\") echo MERGED;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	writeWorktreeRecord(t, repoRoot)
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_INVOCATIONS", marker)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := MergePR(&GitControlOpts{RepoRoot: repoRoot, GatesPath: specPath, TaskID: "T0001"})
	if err == nil {
		t.Fatal("pr merge proceeded on a branch whose checks GitHub had never reported")
	}
	if !strings.Contains(err.Error(), "no checks reported") {
		t.Errorf("the refusal did not carry gh's reason, so no caller can tell a wait from a failure:\n  %v", err)
	}
	if invoked, _ := os.ReadFile(marker); strings.Contains(string(invoked), "pr merge") {
		t.Errorf("gh pr merge was invoked with no check ever reported:\n%s", invoked)
	}
}

// A refusal has to say WHICH of two things happened, because the driver acts on
// the difference: stepAccepted retries a "not yet" and escalates everything
// else. Before this, every non-SUCCESS state produced the same sentence, and
// that sentence was the one the driver retries on — so a required check that
// had finished FAILURE was retried on every tick forever. The task never
// finished and never asked, which is the outcome §8.2 forbids; the local gate
// was green, so nothing else interceded.
//
// So the assertion is not "it refused" — it always did — but "which of the two
// it said", asked through ciStillRunning, the same predicate the driver uses
// rather than a copy of its logic. The test also pins that the two sentences do
// not overlap, so reverting the classification cannot pass by accident: with a
// single sentence for both, one of the two branches below has to fail.
func TestAMergeRefusalSaysWaitOrDecisionAndNeverBoth(t *testing.T) {
	cases := []struct {
		state string
		wait  bool
		why   string
	}{
		{"SUCCESS", false, "green — no refusal at all"},
		{"PENDING", true, "still running: a wait"},
		{"QUEUED", true, "not started yet: a wait"},
		{"IN_PROGRESS", true, "still running: a wait"},
		{"EXPECTED", true, "required but not posted yet: a wait"},
		{"FAILURE", false, "finished red: a decision — this is #139"},
		{"ERROR", false, "finished red: a decision"},
		{"CANCELLED", false, "terminal: a decision"},
		{"TIMED_OUT", false, "terminal: a decision"},
		{"STARTUP_FAILURE", false, "terminal: a decision"},
		{"ACTION_REQUIRED", false, "terminal: a decision"},
		{"STALE", false, "terminal: a decision"},
		{"SKIPPED", false, "not green — the standard is not widened here"},
		{"NEUTRAL", false, "not green — the standard is not widened here"},
		{"A_STATE_GITHUB_ADDS_LATER", false, "unknown falls to a decision, never a silent loop"},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			// The unit under test needs no gate fixture: it asks gh and
			// classifies the answer. MergePR's plumbing to the driver is pinned
			// by TestMergeRefusesWhileThePRChecksAreRed and its neighbour.
			binDir := t.TempDir()
			script := "#!/bin/sh\n" +
				"case \"$1 $2\" in\n" +
				"  \"pr checks\") printf '%s' '[{\"name\":\"job-a\",\"state\":\"" + tc.state + "\"},{\"name\":\"job-b\",\"state\":\"SUCCESS\"}]';;\n" +
				"esac\nexit 0\n"
			if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

			err := assertRequiredChecksGreen(t.TempDir(), "task/T0001-x", []string{"job-a", "job-b"})

			if tc.state == "SUCCESS" {
				if err != nil {
					t.Fatalf("every required check is green but the merge refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("job-a was %s (%s) and no refusal was produced", tc.state, tc.why)
			}
			got := ciStillRunning(err.Error())
			if got != tc.wait {
				t.Errorf("job-a is %s (%s), so the driver should treat this as wait=%v, but the refusal said wait=%v:\n  %v",
					tc.state, tc.why, tc.wait, got, err)
			}
			// The vocabulary itself, so one sentence for both cases cannot
			// satisfy every row above.
			if tc.wait {
				if !strings.Contains(err.Error(), checksNotYet) {
					t.Errorf("a wait did not carry the wait wording:\n  %v", err)
				}
				if strings.Contains(err.Error(), checksRed) {
					t.Errorf("a wait carried the red wording:\n  %v", err)
				}
			} else {
				if !strings.Contains(err.Error(), checksRed) {
					t.Errorf("a decision did not carry the red wording:\n  %v", err)
				}
				if strings.Contains(err.Error(), checksNotYet) {
					t.Errorf("a decision carried the wait wording, so it will be retried forever:\n  %v", err)
				}
			}
		})
	}
}

// A required check the PR has never reported is the third way to be "not
// finished", and it is a wait: a PR pushed a second ago has none of them yet.
func TestARequiredCheckThatWasNeverReportedIsAWait(t *testing.T) {
	binDir := t.TempDir()
	script := "#!/bin/sh\n" +
		"case \"$1 $2\" in\n" +
		"  \"pr checks\") printf '%s' '[{\"name\":\"job-a\",\"state\":\"SUCCESS\"}]';;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	err := assertRequiredChecksGreen(t.TempDir(), "task/T0001-x", []string{"job-a", "job-b"})
	if err == nil {
		t.Fatal("a required check absent from the PR's checks was accepted")
	}
	if !ciStillRunning(err.Error()) {
		t.Errorf("an unreported required check was not treated as a wait:\n  %v", err)
	}
	if !strings.Contains(err.Error(), "job-b") {
		t.Errorf("the refusal does not name the unreported check:\n  %v", err)
	}
}

// ciStillRunning is a substring test over two constants, so their disjointness
// is a real invariant and not a stylistic preference: if either contained the
// other — or contained gh's "no checks reported" — then one of the two answers
// would silently become the other, which is exactly the defect.
func TestTheTwoRefusalsDoNotReadAsEachOther(t *testing.T) {
	if strings.Contains(checksNotYet, checksRed) || strings.Contains(checksRed, checksNotYet) {
		t.Fatalf("the wait and the decision read as each other: %q / %q", checksNotYet, checksRed)
	}
	for _, s := range []string{checksNotYet, checksRed} {
		if strings.Contains(s, noChecksReported) {
			t.Fatalf("%q contains gh's own %q, so ciStillRunning cannot tell them apart", s, noChecksReported)
		}
	}
	// And each input the driver must retry on is sufficient on its own.
	if !ciStillRunning(checksNotYet) || !ciStillRunning(noChecksReported) {
		t.Fatal("a wait the driver is supposed to retry was not recognised as one")
	}
	if ciStillRunning(checksRed) {
		t.Fatal("a decision would be retried as though it were a wait — #139 is back")
	}
}

// writeWorktreeRecord gives the task a registry entry so loadWorktreeRecord
// resolves; the merge path needs a branch to name.
func writeWorktreeRecord(t *testing.T, repoRoot string) {
	t.Helper()
	if err := SaveRegistry(repoRoot, &WorkerRecord{
		TaskID: "T0001", RunID: "run-1", SessionID: "s", ClaudeVersion: "v",
		PID: 1, StartTime: 1, Worktree: repoRoot, Branch: "task/T0001-x",
		BaselineSHA: "0123456789abcdef", RefsBefore: []string{},
		LogPath: filepath.Join(repoRoot, "w.log"), ResultDir: repoRoot, StartedAt: "t",
	}); err != nil {
		t.Fatal(err)
	}
}

// After a baseline is advanced (a rework onto a newer main), the recorded
// baseline IS the merge commit, so diffing against it shows only what changed
// since — while the task's earlier work sits inside the baseline. T0103's
// review input collapsed to 5 files out of a 27-file task that way. The review
// unit must be the branch's contribution, which is the same diff the pull
// request shows.
func TestWorktreeDiffSurvivesABaselineAdvance(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
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
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "base")

	// The task branch: its deliverable, committed.
	git("checkout", "-q", "-b", "task/T0001-x")
	if err := os.WriteFile(filepath.Join(dir, "task_deliverable.go"), []byte("package task\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "task work")

	// main moves on, then the baseline is advanced by merging it in.
	git("checkout", "-q", "main")
	if err := os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("main moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "main moved")
	git("checkout", "-q", "task/T0001-x")
	git("merge", "-q", "--no-edit", "main")
	advanced := git("rev-parse", "HEAD")

	diff, err := taskWorktreeDiff(&WorkerRecord{Worktree: dir, BaselineSHA: advanced})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "task_deliverable.go") {
		t.Errorf("the task's own deliverable is missing once the baseline was advanced — a Reviewer would be handed a fragment of a task and asked to approve the whole:\n%s", diff)
	}
	if strings.Contains(diff, "unrelated.txt") {
		t.Errorf("main's own changes leaked into the review of the task:\n%s", diff)
	}
}

// The same string is not only the review input: prepareIntegrationTree writes
// it out and applies it to build the tree G2 and G3 verify. So the property
// that has to hold is that APPLYING it reproduces the worktree byte for byte —
// checking the text for a marker would pass on a patch that still loses a
// byte. A file that ends at its last byte (every canonical schema does: they
// end with a closing brace) must not arrive with a newline added, and an empty
// file must arrive empty rather than as a one-line file, and a file of one
// blank line must not arrive empty.
func TestWorktreeDiffReproducesTheWorktreeByteForByte(t *testing.T) {
	dir := t.TempDir()
	runGitIn := func(where string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = where
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, where, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGitIn(dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitIn(dir, "add", "-A")
	runGitIn(dir, "commit", "-q", "-m", "baseline")
	baseline := runGitIn(dir, "rev-parse", "HEAD")

	files := map[string]string{
		"tracked.txt":  "after\n",    // modified, newline at the end
		"no_eol.json":  "{\"a\": 1}", // the shape every specs/schemas file has
		"empty.txt":    "",
		"blank.txt":    "\n",
		"with_eol.txt": "a\nb\n",
		"one_byte.txt": "x",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	diff, err := taskWorktreeDiff(&WorkerRecord{Worktree: dir, BaselineSHA: baseline})
	if err != nil {
		t.Fatal(err)
	}
	applied := filepath.Join(t.TempDir(), "applied")
	runGitIn(dir, "worktree", "add", "-q", "--detach", applied, baseline)
	patch := filepath.Join(t.TempDir(), "change.patch")
	if err := os.WriteFile(patch, []byte(diff), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitIn(applied, "apply", patch)

	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(applied, name))
		if err != nil {
			t.Errorf("%s: the patch did not reproduce the file: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s: applying the diff produced %q, the worktree holds %q — the tree the gate verifies would not be the tree the task produced", name, got, want)
		}
	}
}
