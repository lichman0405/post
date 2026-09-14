package devorchestrator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The tests below are about one question: what a merge does when the migrations
// in the repository would land out of order. The hazard and why CI cannot see
// it are in migration_order.go.

// migrationOrderFixture builds a repository a merge can actually be driven
// through: a real Git repository whose `main` holds one migration, the green
// collect + G2 records MergePR asserts over, a registry record for T0001 whose
// worktree IS repoRoot, and the DAG and state file the merge settles into.
func migrationOrderFixture(t *testing.T) (repoRoot, specPath, dagPath, statePath string) {
	t.Helper()
	repoRoot, specPath = mergeGateFixture(t) // green collect + green G2 on disk
	git := gitInRepo(t, repoRoot)
	git("init", "-q", "-b", "main")
	writeMigration(t, repoRoot, "00001_first.sql")
	git("add", "-A")
	git("commit", "-q", "-m", "schema history")
	writeWorktreeRecord(t, repoRoot)
	dagPath = filepath.Join(repoRoot, "tasks.json")
	statePath = filepath.Join(repoRoot, "task_status.json")
	writeJSON(t, dagPath, map[string]any{
		"version": 1, "task_count": 1, "phases": map[string]any{},
		"tasks": []map[string]any{{"id": "T0001", "phase": "P1", "title": "task", "dependencies": []string{}}},
	})
	writeJSON(t, statePath, map[string]any{
		"overall": "P1_in_progress",
		"tasks":   map[string]any{"T0001": map[string]any{"status": "accepted"}},
	})
	return repoRoot, specPath, dagPath, statePath
}

// gitInRepo runs git in dir with a fixed identity, so a fixture can make
// commits without depending on the machine's Git configuration.
func gitInRepo(t *testing.T, dir string) func(args ...string) string {
	t.Helper()
	return func(args ...string) string {
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
}

// writeMigration puts a migration file where the guard looks for one: on disk,
// uncommitted. That is not a shortcut — a task in flight IS an uncommitted
// working-tree diff, so a fixture that committed its migration would be testing
// the one shape the guard will never see.
func writeMigration(t *testing.T, worktree, name string) {
	t.Helper()
	dir := filepath.Join(worktree, "infra", "migrations")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("-- "+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// greenGh installs a `gh` that reports every required check green, answers the
// mergeability query and merges — so nothing but the migration order can refuse.
func greenGh(t *testing.T) (marker string) {
	t.Helper()
	binDir := t.TempDir()
	marker = filepath.Join(binDir, "invocations.log")
	script := "#!/bin/sh\necho \"$0 $*\" >> \"$FAKE_INVOCATIONS\"\n" +
		"case \"$1 $2\" in\n" +
		"  \"pr view\") printf '%s' '{\"mergeable\":\"MERGEABLE\",\"mergeStateStatus\":\"CLEAN\"}';;\n" +
		"  \"pr checks\") printf '%s' '[{\"name\":\"job-a\",\"state\":\"SUCCESS\"},{\"name\":\"job-b\",\"state\":\"SUCCESS\"}]';;\n" +
		"  \"pr merge\") echo MERGED;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_INVOCATIONS", marker)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return marker
}

func ghInvocations(t *testing.T, marker string) string {
	t.Helper()
	data, err := os.ReadFile(marker)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

// The hazard itself: a task whose migration would land on main while a
// lower-numbered one is still in flight.
//
// The refusal is asserted through the driver's own classifier as well as on its
// text. A refusal that reads as "CI is still running" is retried every tick
// forever and never becomes a decision — T0304's PR #159 sat in exactly that
// state — and a migration-order violation is the opposite of a wait: no amount
// of waiting reorders it.
func TestMergeRefusesAMigrationThatWouldOvertakeAnUnmergedOne(t *testing.T) {
	repoRoot, specPath, dagPath, statePath := migrationOrderFixture(t)
	writeMigration(t, repoRoot, "00005_fifth.sql")
	writeMigration(t, filepath.Join(WorktreesDir(repoRoot), "T0002"), "00004_fourth.sql")
	marker := greenGh(t)

	_, err := MergePR(&GitControlOpts{
		RepoRoot: repoRoot, GatesPath: specPath, TaskID: "T0001",
		DagPath: dagPath, StatePath: statePath, RunID: "run-1",
	})
	if err == nil {
		t.Fatal("pr merge proceeded with a migration that would land out of order")
	}
	if !strings.Contains(err.Error(), "T0002") || !strings.Contains(err.Error(), "00004") {
		t.Errorf("the refusal does not name what is blocking it, so the operator cannot act on it:\n  %v", err)
	}
	if !strings.Contains(err.Error(), "out-of-order") {
		t.Errorf("the refusal does not say WHY the order matters, so the next person reorders it back as a preference:\n  %v", err)
	}
	if ciStillRunning(err.Error()) {
		t.Errorf("an ordering violation was refused as though CI were still running, so the driver will retry it forever:\n  %v", err)
	}
	if invoked := ghInvocations(t, marker); strings.Contains(invoked, "pr merge") {
		t.Errorf("gh pr merge was invoked despite the ordering violation:\n%s", invoked)
	}
}

// The direction of the comparison, which a guard written with the wrong
// inequality inverts: holding the LOWER migration is the whole point of
// merging. T0305 then T0206 then T0209 is the intended sequence, and only the
// last of those is ever in this state.
func TestMergeProceedsWhenTheTaskHoldsTheLowerMigration(t *testing.T) {
	repoRoot, specPath, dagPath, statePath := migrationOrderFixture(t)
	writeMigration(t, repoRoot, "00004_fourth.sql")
	writeMigration(t, filepath.Join(WorktreesDir(repoRoot), "T0002"), "00005_fifth.sql")
	marker := greenGh(t)

	out, err := MergePR(&GitControlOpts{
		RepoRoot: repoRoot, GatesPath: specPath, TaskID: "T0001",
		DagPath: dagPath, StatePath: statePath, RunID: "run-1",
	})
	if err != nil {
		t.Fatalf("the merge was refused although this task holds the lower migration: %v", err)
	}
	if strings.TrimSpace(out) != "MERGED" {
		t.Errorf("merge result = %q, want MERGED", out)
	}
	if invoked := ghInvocations(t, marker); !strings.Contains(invoked, "pr merge") {
		t.Errorf("gh pr merge was never invoked:\n%s", invoked)
	}
}

// A migration file that is already on main is not pending, however old the
// worktree holding it is. Without that, every task that has ever carried a
// migration blocks every task that carries a higher one — and worktrees
// outlive their merges (the baseline is left where the task finished), so this
// is the ordinary case and not the corner.
func TestMergeIgnoresMigrationsTheIntegrationBranchAlreadyHas(t *testing.T) {
	repoRoot, specPath, dagPath, statePath := migrationOrderFixture(t)
	git := gitInRepo(t, repoRoot)
	writeMigration(t, repoRoot, "00002_second.sql")
	git("add", "-A")
	git("commit", "-q", "-m", "second migration, merged")
	writeMigration(t, repoRoot, "00003_third.sql")
	// T0002's tree is older than main's: it still holds 00001 and 00002, both
	// of which main has.
	older := filepath.Join(WorktreesDir(repoRoot), "T0002")
	writeMigration(t, older, "00001_first.sql")
	writeMigration(t, older, "00002_second.sql")
	marker := greenGh(t)

	if _, err := MergePR(&GitControlOpts{
		RepoRoot: repoRoot, GatesPath: specPath, TaskID: "T0001",
		DagPath: dagPath, StatePath: statePath, RunID: "run-1",
	}); err != nil {
		t.Fatalf("an already-merged migration in a stale worktree blocked the merge: %v", err)
	}
	if invoked := ghInvocations(t, marker); !strings.Contains(invoked, "pr merge") {
		t.Errorf("gh pr merge was never invoked:\n%s", invoked)
	}
}

// A Review Worker's `<TASK>-review` directory is a scratch directory, not a
// task worktree, and it is never merged — so nothing inside it can land on
// main. What matters here is that a full copy of the tree landing in one is
// not mistaken for a task in flight, which is a shape that has appeared: the
// reviewer reads the repository it is reviewing.
func TestMergeIgnoresAReviewScratchDirectory(t *testing.T) {
	repoRoot, specPath, dagPath, statePath := migrationOrderFixture(t)
	writeMigration(t, repoRoot, "00005_fifth.sql")
	writeMigration(t, filepath.Join(WorktreesDir(repoRoot), "T0009-review"), "00004_fourth.sql")
	marker := greenGh(t)

	if _, err := MergePR(&GitControlOpts{
		RepoRoot: repoRoot, GatesPath: specPath, TaskID: "T0001",
		DagPath: dagPath, StatePath: statePath, RunID: "run-1",
	}); err != nil {
		t.Fatalf("a review scratch directory blocked the merge: %v", err)
	}
	if invoked := ghInvocations(t, marker); !strings.Contains(invoked, "pr merge") {
		t.Errorf("gh pr merge was never invoked:\n%s", invoked)
	}
}

// A task carrying migrations whose already-applied set cannot be read is
// refused: the order is not known to be wrong, it is UNKNOWN, and the refusal
// says which of the two it is rather than borrowing the wording of the other.
// The distinction is what stops the driver from reporting a repository it
// cannot read as a repository in violation.
func TestMergeRefusesWhenTheMigrationOrderCannotBeChecked(t *testing.T) {
	repoRoot, specPath := mergeGateFixture(t) // deliberately not a Git repository
	writeWorktreeRecord(t, repoRoot)
	writeMigration(t, repoRoot, "00002_second.sql")
	marker := greenGh(t)

	_, err := MergePR(&GitControlOpts{RepoRoot: repoRoot, GatesPath: specPath, TaskID: "T0001"})
	if err == nil {
		t.Fatal("a task carrying migrations was merged without the order being checked")
	}
	if !strings.Contains(err.Error(), "cannot be checked") {
		t.Errorf("the refusal does not distinguish an unreadable repository from an ordering violation:\n  %v", err)
	}
	if strings.Contains(err.Error(), "out-of-order") {
		t.Errorf("an unreadable repository was refused with the wording of an ordering violation:\n  %v", err)
	}
	if invoked := ghInvocations(t, marker); strings.Contains(invoked, "pr merge") {
		t.Errorf("gh pr merge was invoked while the migration order was unknown:\n%s", invoked)
	}
}
