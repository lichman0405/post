package devorchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Rebaseline is the tool that destroyed T0203's work (issue #103: it resets and
// cleans the worktree before checking that the task's change still applies, then
// deletes the only copy of the patch), and until this file it had no test at
// all. This pins the one thing about it an advance cannot get wrong quietly:
// where it advances TO.

// TestRebaselineAdvancesToWhatMainIsNotToWhatTheCloneHeard: the driver
// rebaselines a task exactly when a dependency has just been recorded merged,
// which is exactly when the local clone's main is behind the forge. A target
// read from the local branch would leave the task "advanced" onto a tree that
// still lacks the dependency it was sent back for — the same defect as #123,
// one step later in the same flow.
func TestRebaselineAdvancesToWhatMainIsNotToWhatTheCloneHeard(t *testing.T) {
	_, publisher, workspace := bbOriginAndClones(t)

	// The task's worktree: cut from the seed, carrying an uncommitted change.
	base := bbGit(t, workspace, "rev-parse", "HEAD")
	branch := "task/T0001-something"
	bbGit(t, workspace, "branch", branch, base)
	wt := filepath.Join(WorktreesDir(workspace), "T0001")
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		t.Fatal(err)
	}
	bbGit(t, workspace, "worktree", "add", "-q", wt, branch)
	if err := os.WriteFile(filepath.Join(wt, "taskwork.txt"), []byte("the task's change\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A dependency merges on the forge; nothing pulls it.
	merged := bbCommitFile(t, publisher, "dependency.sql", "-- the dependency's migration\n")
	bbGit(t, publisher, "push", "-q", "origin", "main")
	if got := bbGit(t, workspace, "rev-parse", "refs/heads/"+DefaultBaseBranch); got != base {
		t.Fatalf("fixture is wrong: the local branch already moved to %s", got)
	}

	if err := SaveRegistry(workspace, &WorkerRecord{
		TaskID: "T0001", RunID: "r", Branch: branch, Worktree: wt, ResultDir: wt,
		BaselineSHA: base, StartedAt: "t", ExitStatus: new(int),
	}); err != nil {
		t.Fatal(err)
	}
	// Rebaseline reads this to decide which changes travel as a regeneration
	// rather than as text. One rule the task does not touch exercises the path
	// without needing a regenerator to exist (and an empty rule list is an
	// error, not a no-op).
	daPath := filepath.Join(workspace, DefaultDerivedArtifactsPath)
	if err := os.MkdirAll(filepath.Dir(daPath), 0o755); err != nil {
		t.Fatal(err)
	}
	rules := `{"version":1,"rules":[{"marker":"specs/**","derived":"specs/marker.json"}]}`
	if err := os.WriteFile(daPath, []byte(rules+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := RebaselineTask(workspace, "T0001", "", "")
	if err != nil {
		t.Fatalf("RebaselineTask: %v", err)
	}
	if res.ToSHA != merged {
		t.Errorf("the advance target is %s, want the merge %s — it advanced onto the clone's stale branch", res.ToSHA, merged)
	}
	if got := bbGit(t, wt, "rev-parse", "HEAD"); got != merged {
		t.Errorf("the worktree is at %s, want the merge %s", got, merged)
	}
	if _, err := os.Stat(filepath.Join(wt, "dependency.sql")); err != nil {
		t.Errorf("the advanced tree does not contain the dependency the task was sent back for: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(wt, "taskwork.txt"))
	if err != nil {
		t.Fatalf("the advance lost the task's own change: %v", err)
	}
	if !strings.Contains(string(body), "the task's change") {
		t.Errorf("the task's change did not survive the advance: %q", body)
	}
}
