package devorchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A NEW Worker must not start from a branch older than the main it is graded
// against. The defect (2026-09-21): `worker spawn` cuts a MISSING branch from
// the integration tip, but ADOPTS an existing one — and an existing branch is
// what a vanished or parked attempt leaves behind. T1007 and T1102 were both
// re-dispatched onto branches 33 commits behind main, one of them with 120 of
// main's 172 changed files inside its own allowed_scope, and nothing said so.
//
// The fixtures are base_branch_test.go's: an origin, a publisher (the forge,
// where a merge lands) and the workspace (the shared checkout under test).

func TestRequireBaselineNotBehindMainRefusesAStaleTaskBranch(t *testing.T) {
	origin, publisher, workspace := bbOriginAndClones(t)
	_ = origin

	// The task branch is cut first, at the tip the workspace knows today.
	bbGit(t, workspace, "branch", "task/T9999-stale")
	staleTip := bbGit(t, workspace, "rev-parse", "refs/heads/task/T9999-stale")

	// Then main moves on the forge — the ordinary event this guard exists for.
	bbCommitFile(t, publisher, "merged.txt", "a dependency that just landed\n")
	bbGit(t, publisher, "push", "-q", "origin", "main")

	err := requireBaselineNotBehindMain(workspace, "T9999", "task/T9999-stale")
	if err == nil {
		t.Fatal("a task branch that predates a merged dependency was accepted as a baseline: the Worker would build on a tree without it")
	}
	msg := err.Error()
	if !strings.Contains(msg, "rebaseline") {
		t.Errorf("the refusal must name the verb that fixes it (`rddev rebaseline`), got: %s", msg)
	}
	if !strings.Contains(msg, "task/T9999-stale") {
		t.Errorf("the refusal must name the branch it refused, got: %s", msg)
	}
	// The branch is where it was: refusing must not have moved anything.
	if after := bbGit(t, workspace, "rev-parse", "refs/heads/task/T9999-stale"); after != staleTip {
		t.Errorf("the refusal moved the branch: %s -> %s", staleTip, after)
	}
}

// The guard must not fire on the two states that are legitimate: a branch that
// already contains main's current tip, and one carrying the Worker's own
// commits on top of it. A guard that refuses ordinary work is worse than none —
// it stops the whole DAG.
func TestRequireBaselineNotBehindMainAcceptsCurrentAndAheadBranches(t *testing.T) {
	_, publisher, workspace := bbOriginAndClones(t)

	bbCommitFile(t, publisher, "merged.txt", "main moves\n")
	bbGit(t, publisher, "push", "-q", "origin", "main")
	bbGit(t, workspace, "fetch", "-q", "origin", "main")

	// (a) Exactly at the tip.
	bbGit(t, workspace, "branch", "task/T9997-current", "origin/main")
	if err := requireBaselineNotBehindMain(workspace, "T9997", "task/T9997-current"); err != nil {
		t.Errorf("a branch at the integration tip was refused: %v", err)
	}

	// (b) The tip plus the Worker's own commits — the normal shape of every
	// task branch that has work on it.
	bbGit(t, workspace, "checkout", "-q", "-b", "task/T9998-with-work", "origin/main")
	bbCommitFile(t, workspace, "work.txt", "the task's own commit\n")
	bbGit(t, workspace, "checkout", "-q", "main")
	if err := requireBaselineNotBehindMain(workspace, "T9998", "task/T9998-with-work"); err != nil {
		t.Errorf("a branch carrying its own commits on top of the tip was refused: %v", err)
	}
}

// A branch that went its own way from an older main is refused too: it contains
// commits main does not, and it lacks main's. `merge-base --is-ancestor` is the
// question being asked precisely because "behind" is not the only bad shape.
func TestRequireBaselineNotBehindMainRefusesADivergedBranch(t *testing.T) {
	_, publisher, workspace := bbOriginAndClones(t)

	bbCommitFile(t, workspace, "task.txt", "the task's own commit\n")
	bbGit(t, workspace, "branch", "task/T9996-diverged")

	bbCommitFile(t, publisher, "merged.txt", "main moves\n")
	bbGit(t, publisher, "push", "-q", "origin", "main")

	if err := requireBaselineNotBehindMain(workspace, "T9996", "task/T9996-diverged"); err == nil {
		t.Fatal("a diverged task branch was accepted: it lacks commits the Worker is graded against and carries commits main does not")
	}
}

// The helper above is only half the property: what has to be true is that
// `worker spawn` ASKS the question. A test that calls the helper directly would
// keep passing if the call site were deleted — so this one goes through Spawn
// and asserts it refuses before a Worker process exists.
//
// It stops at the guard, which is the point: the fixtures are a synthetic DAG
// (store_test.go), a clone with a task branch one commit behind the forge, and
// a stand-in for the claude binary — a real one is never reached.
func TestSpawnRefusesAStaleTaskBranchBeforeStartingAWorker(t *testing.T) {
	_, publisher, workspace := bbOriginAndClones(t)

	dagPath := writeSyntheticDAG(t, workspace)
	statePath := filepath.Join(workspace, "task_status.json")

	// T1000's title ("task A") is what TaskBranch slugs into the branch name,
	// so this is the branch Spawn will look for.
	branch := TaskBranch("T1000", "task A")
	bbGit(t, workspace, "branch", branch)
	staleTip := bbGit(t, workspace, "rev-parse", "refs/heads/"+branch)

	// The forge merges something after the branch was cut.
	bbCommitFile(t, publisher, "merged.txt", "a dependency that just landed\n")
	bbGit(t, publisher, "push", "-q", "origin", "main")

	// A stand-in for the claude binary. Spawn checks PATH before anything else,
	// so without this the test would be measuring this machine's PATH.
	fakeClaude := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(fakeClaude, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(WorktreesDir(workspace), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := Spawn(&SpawnOpts{
		RepoRoot:  workspace,
		DagPath:   dagPath,
		StatePath: statePath,
		TaskID:    "T1000",
		ClaudeBin: fakeClaude,
	})
	if err == nil {
		t.Fatal("spawn started a Worker on a task branch 1 commit behind main: a new session must not read a tree older than the main it is graded against")
	}
	if !strings.Contains(err.Error(), "rebaseline") {
		t.Errorf("spawn's refusal must name the verb that fixes it, got: %v", err)
	}
	if after := bbGit(t, workspace, "rev-parse", "refs/heads/"+branch); after != staleTip {
		t.Errorf("the refused spawn moved the branch: %s -> %s", staleTip, after)
	}
}
