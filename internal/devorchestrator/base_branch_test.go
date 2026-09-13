package devorchestrator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These pin the property that a task's baseline — and the tree a gate grades —
// contains every dependency the DAG calls merged. The defect (#123) was that it
// did not: the branch was cut from the checkout's current HEAD, and the
// checkout's main was whatever the last `git pull` had left there, so a task
// spawned seconds after a merge started from a tree without the merge. The
// answer is the ref that merges actually move, which is the remote-tracking one.

// bbGit runs git in dir and fails the test on error.
func bbGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func bbCommitFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	bbGit(t, dir, "add", "-A")
	bbGit(t, dir, "-c", "user.name=test", "-c", "user.email=test@test", "commit", "-q", "-m", "add "+name)
	return bbGit(t, dir, "rev-parse", "HEAD")
}

// bbOriginAndClones builds a bare origin with two clones of it: one that
// publishes — the forge, where a merge lands — and one that is the shared
// checkout under test.
func bbOriginAndClones(t *testing.T) (origin, publisher, workspace string) {
	t.Helper()
	root := t.TempDir()
	origin = filepath.Join(root, "origin.git")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	bbGit(t, root, "init", "-q", "--bare", "-b", "main", origin)

	seed := filepath.Join(root, "seed")
	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatal(err)
	}
	bbGit(t, seed, "init", "-q", "-b", "main")
	bbCommitFile(t, seed, "README.md", "# seed\n")
	bbGit(t, seed, "remote", "add", "origin", origin)
	bbGit(t, seed, "push", "-q", "origin", "main")

	publisher = filepath.Join(root, "publisher")
	workspace = filepath.Join(root, "workspace")
	bbGit(t, root, "clone", "-q", origin, publisher)
	bbGit(t, root, "clone", "-q", origin, workspace)
	for _, d := range []string{publisher, workspace} {
		bbGit(t, d, "config", "user.name", "test")
		bbGit(t, d, "config", "user.email", "test@test")
	}
	return origin, publisher, workspace
}

// bbResolve turns a ref name into the commit it names, the way both call sites
// do — the tip is returned as a name, not as a baked-in sha.
func bbResolve(t *testing.T, dir, ref string) string {
	t.Helper()
	return bbGit(t, dir, "rev-parse", ref)
}

// TestIntegrationTipAnswersWithTheRemoteBranch: the ref that merges move is the
// remote-tracking one, and the answer is a name resolved by the caller.
func TestIntegrationTipAnswersWithTheRemoteBranch(t *testing.T) {
	_, publisher, workspace := bbOriginAndClones(t)

	tip, err := IntegrationTip(workspace)
	if err != nil {
		t.Fatalf("IntegrationTip: %v", err)
	}
	if want := "refs/remotes/origin/" + DefaultBaseBranch; tip != want {
		t.Errorf("IntegrationTip = %q, want %q", tip, want)
	}
	if got, want := bbResolve(t, workspace, tip), bbGit(t, publisher, "rev-parse", "HEAD"); got != want {
		t.Errorf("%s resolves to %s, want the forge's %s", tip, got, want)
	}
}

// TestIntegrationTipSeesAMergeTheLocalBranchNeverSaw is #123 at the unit level.
// A merge moves the forge; nothing moves the clone. A baseline cut from the
// clone omits the dependency, which is exactly what T0203 built on.
func TestIntegrationTipSeesAMergeTheLocalBranchNeverSaw(t *testing.T) {
	_, publisher, workspace := bbOriginAndClones(t)
	merged := bbCommitFile(t, publisher, "dependency.sql", "-- the dependency's migration\n")
	bbGit(t, publisher, "push", "-q", "origin", "main")

	// The shape of the incident: the clone's own main is behind.
	if local := bbResolve(t, workspace, "refs/heads/"+DefaultBaseBranch); local == merged {
		t.Fatal("fixture is wrong: the local branch already has the merge")
	}

	tip, err := IntegrationTip(workspace)
	if err != nil {
		t.Fatalf("IntegrationTip: %v", err)
	}
	if got := bbResolve(t, workspace, tip); got != merged {
		t.Errorf("the tip resolves to %s, want the merge %s — a task cut from this omits its dependency", got, merged)
	}
}

// TestIntegrationTipDoesNotTrustTheConfiguredRefspec: a plain
// `git fetch origin main` updates the remote-tracking ref only when
// `remote.origin.fetch` covers it, so a narrowed one leaves
// refs/remotes/origin/main sitting where it was — present, stale, and with no
// signal that the fetch brought nothing. A stale ref is the defect this whole
// function exists to remove, so the fetch names its destination.
func TestIntegrationTipDoesNotTrustTheConfiguredRefspec(t *testing.T) {
	_, publisher, workspace := bbOriginAndClones(t)
	merged := bbCommitFile(t, publisher, "merged.txt", "the merge\n")
	bbGit(t, publisher, "push", "-q", "origin", "main")
	// The clone is told not to track main at all.
	bbGit(t, workspace, "config", "remote.origin.fetch", "refs/heads/master:refs/remotes/origin/master")

	tip, err := IntegrationTip(workspace)
	if err != nil {
		t.Fatalf("IntegrationTip: %v", err)
	}
	if got := bbResolve(t, workspace, tip); got != merged {
		t.Errorf("%s resolves to %s, want the merge %s — the fetch did not update the ref it names", tip, got, merged)
	}
}

// TestAGitCallOverTheNetworkIsBounded pins the mechanism, not the value: a call
// with a deadline that has passed reports the deadline rather than whatever the
// remote said. The 2-minute bound the fetch actually uses cannot be exercised
// here — it would need a remote that hangs, and a suite that waits out a hang is
// its own problem. What is pinned is that a bound exists at all: an unbounded
// fetch does not fail a dispatch, it stalls it, and a stalled dispatch under
// `rddev drive` is the whole DAG not moving.
func TestAGitCallOverTheNetworkIsBounded(t *testing.T) {
	repo := t.TempDir()
	bbGit(t, repo, "init", "-q", "-b", "main")
	bbCommitFile(t, repo, "README.md", "x\n")
	// Unreachable as well as overdue, so the command fails whatever the race
	// between the deadline and the process start.
	bbGit(t, repo, "remote", "add", "origin", filepath.Join(t.TempDir(), "does-not-exist.git"))

	_, err := runGit(repo, time.Nanosecond, "fetch", "origin", DefaultBaseBranch)
	if err == nil {
		t.Fatal("runGit returned no error for a fetch whose deadline had already passed")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, want it to name the deadline rather than the remote's failure", err)
	}
}

// TestIntegrationTipLeavesADirtyCheckoutAlone is why this returns a ref instead
// of fast-forwarding one. The shared checkout is dirty by design — every state
// transition rewrites tasks/task_status.json, and merges touch that same file —
// so a fast-forward is refused exactly when it is needed. Reading cannot be
// refused by local state, and must not change it.
func TestIntegrationTipLeavesADirtyCheckoutAlone(t *testing.T) {
	_, publisher, workspace := bbOriginAndClones(t)
	// The merge touches README.md, which the checkout has modified.
	merged := bbCommitFile(t, publisher, "README.md", "merged on the forge\n")
	bbGit(t, publisher, "push", "-q", "origin", "main")
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("uncommitted local edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	head := bbGit(t, workspace, "rev-parse", "HEAD")

	tip, err := IntegrationTip(workspace)
	if err != nil {
		t.Fatalf("IntegrationTip refused because the checkout is dirty (%v) — dispatches would then be blocked by state that is normally present", err)
	}
	if got := bbResolve(t, workspace, tip); got != merged {
		t.Errorf("the tip resolves to %s, want the merge %s", got, merged)
	}
	if got := bbGit(t, workspace, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD moved: %s -> %s — answering must not touch the checkout", head, got)
	}
	if got := bbGit(t, workspace, "branch", "--show-current"); got != DefaultBaseBranch {
		t.Errorf("the checkout moved to %q", got)
	}
	body, rerr := os.ReadFile(filepath.Join(workspace, "README.md"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(body), "uncommitted local edit") {
		t.Errorf("the local edit was overwritten: %q", body)
	}
}

// TestIntegrationTipRefusesAFetchThatFails: dispatching from a ref of unknown
// age is the defect, so an unreachable remote is an error rather than a silent
// fallback to whatever the local branch says.
func TestIntegrationTipRefusesAFetchThatFails(t *testing.T) {
	repo := t.TempDir()
	bbGit(t, repo, "init", "-q", "-b", "main")
	bbCommitFile(t, repo, "README.md", "x\n")
	bbGit(t, repo, "remote", "add", "origin", filepath.Join(t.TempDir(), "does-not-exist.git"))

	if _, err := IntegrationTip(repo); err == nil {
		t.Fatal("IntegrationTip answered from a local ref of unknown age when the fetch failed")
	} else if !strings.Contains(err.Error(), DefaultBaseBranch) {
		t.Errorf("error = %q, want it to name the branch it could not fetch", err)
	}
}

// TestIntegrationTipIsTheLocalBranchWithoutARemote: there is nothing to be
// current with, so the local branch is the answer. This is also what keeps the
// package's other fixtures working unchanged.
func TestIntegrationTipIsTheLocalBranchWithoutARemote(t *testing.T) {
	repo := t.TempDir()
	bbGit(t, repo, "init", "-q", "-b", "main")
	bbCommitFile(t, repo, "README.md", "x\n")

	tip, err := IntegrationTip(repo)
	if err != nil {
		t.Fatalf("IntegrationTip on a repo with no remote: %v", err)
	}
	if want := "refs/heads/" + DefaultBaseBranch; tip != want {
		t.Errorf("IntegrationTip = %q, want %q", tip, want)
	}
}

// TestIntegrationTipIsHEADWhenThereAreNoCommits: `git init` with nothing
// committed has no branch to name, and that must not be a failure.
func TestIntegrationTipIsHEADWhenThereAreNoCommits(t *testing.T) {
	repo := t.TempDir()
	bbGit(t, repo, "init", "-q", "-b", "main")

	tip, err := IntegrationTip(repo)
	if err != nil {
		t.Fatalf("IntegrationTip on an empty repo: %v", err)
	}
	if tip != "HEAD" {
		t.Errorf("IntegrationTip = %q, want HEAD", tip)
	}
}

// TestTaskBranchIsCutFromMainNotTheCheckout: a task branch must be cut from the
// integration branch even when the shared checkout is somewhere else, and
// creating it must not move that checkout.
func TestTaskBranchIsCutFromMainNotTheCheckout(t *testing.T) {
	repo := t.TempDir()
	bbGit(t, repo, "init", "-q", "-b", "main")
	bbCommitFile(t, repo, "README.md", "main\n")
	mainSHA := bbGit(t, repo, "rev-parse", "refs/heads/main")

	// A branch that is AHEAD of main, checked out — a review branch, a probe,
	// a Supervisor's own work in progress.
	bbGit(t, repo, "checkout", "-q", "-b", "in-progress-review")
	ahead := bbCommitFile(t, repo, "review-only.txt", "not merged anywhere\n")
	if ahead == mainSHA {
		t.Fatal("fixture is wrong: the review branch is not ahead")
	}

	if err := os.MkdirAll(WorktreesDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	branch := "task/T0001-something"
	if err := ensureWorktree(repo, "T0001", branch); err != nil {
		t.Fatalf("ensureWorktree: %v", err)
	}

	if got := bbGit(t, repo, "rev-parse", "refs/heads/"+branch); got != mainSHA {
		t.Errorf("task branch base = %s, want main %s — it was cut from something else", got, mainSHA)
	}
	if _, err := os.Stat(filepath.Join(repo, ".rddev", "worktrees", "T0001", "review-only.txt")); err == nil {
		t.Error("the task worktree carries a commit that was never merged")
	}
	if got := bbGit(t, repo, "branch", "--show-current"); got != "in-progress-review" {
		t.Errorf("creating the task branch moved the shared checkout to %q", got)
	}
}

// TestTaskBranchCarriesAMergeThatLandedAfterTheLastPull is the incident's other
// half, end to end through ensureWorktree: the forge has a merge the clone does
// not, and the task must still be cut from the merged tree. With a stale main
// this is the failure that rejected T0203 — the task's tree would not contain
// its dependency's migration.
func TestTaskBranchCarriesAMergeThatLandedAfterTheLastPull(t *testing.T) {
	_, publisher, workspace := bbOriginAndClones(t)
	want := bbCommitFile(t, publisher, "dependency.sql", "the dependency's migration\n")
	bbGit(t, publisher, "push", "-q", "origin", "main")

	// The checkout is not on main, so nothing has pulled the merge.
	bbGit(t, workspace, "checkout", "-q", "-b", "review")
	if err := os.MkdirAll(WorktreesDir(workspace), 0o755); err != nil {
		t.Fatal(err)
	}
	branch := "task/T0002-typed-relations"
	if err := ensureWorktree(workspace, "T0002", branch); err != nil {
		t.Fatalf("ensureWorktree: %v", err)
	}

	if got := bbGit(t, workspace, "rev-parse", "refs/heads/"+branch); got != want {
		t.Errorf("task branch = %s, want the merge %s", got, want)
	}
	wt := filepath.Join(workspace, ".rddev", "worktrees", "T0002")
	if _, err := os.Stat(filepath.Join(wt, "dependency.sql")); err != nil {
		t.Errorf("the task's tree does not contain its dependency's merged work: %v", err)
	}
}

// TestTheIntegrationTreeIsCutFromTheFetchedTip pins the second reader. A gate
// grades "current main plus the task's change", so a stale main grades a
// composition the forge stopped having at the last merge — and it is the same
// answer the baseline comes from, which is why both call IntegrationTip.
func TestTheIntegrationTreeIsCutFromTheFetchedTip(t *testing.T) {
	origin, publisher, workspace := bbOriginAndClones(t)
	merged := bbCommitFile(t, publisher, "dependency.sql", "-- the dependency's migration\n")
	bbGit(t, publisher, "push", "-q", "origin", "main")

	// The task's own worktree: a second clone of the same origin, carrying the
	// uncommitted change a Worker leaves behind.
	taskWT := filepath.Join(filepath.Dir(workspace), "taskwt")
	bbGit(t, filepath.Dir(workspace), "clone", "-q", origin, taskWT)
	bbGit(t, taskWT, "config", "user.name", "test")
	bbGit(t, taskWT, "config", "user.email", "test@test")
	if err := os.WriteFile(filepath.Join(taskWT, "README.md"), []byte("# seed\nthe task's change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SaveRegistry(workspace, &WorkerRecord{
		TaskID: "T0001", RunID: "r", Branch: "task/T0001-x", Worktree: taskWT,
		ResultDir: taskWT, BaselineSHA: merged, StartedAt: "t",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(IntegrationTreeDir(workspace, "T0001")), 0o755); err != nil {
		t.Fatal(err)
	}

	dir, cleanup, err := prepareIntegrationTree(workspace, "T0001")
	if err != nil {
		t.Fatalf("prepareIntegrationTree: %v", err)
	}
	defer cleanup()

	if _, err := os.Stat(filepath.Join(dir, "dependency.sql")); err != nil {
		t.Errorf("the integration tree does not contain the merge that landed after the clone's last pull: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "the task's change") {
		t.Errorf("the integration tree does not carry the task's change: %q", body)
	}
}
