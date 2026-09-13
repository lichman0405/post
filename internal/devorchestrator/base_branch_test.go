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
// remote said.
//
// It uses a remote that HANGS, because an overdue deadline against a remote that
// fails immediately proves nothing about the hang this bound exists for — and
// the earlier version of this test did exactly that, which is how it passed
// while the bound was removable. An unbounded fetch does not fail a dispatch, it
// stalls it, and a stalled dispatch under `rddev drive` is the whole DAG not
// moving; so the assertion is on the CLOCK: with a 20-second hang and a 300ms
// deadline, the call has to come back long before the hang ends.
func TestAGitCallOverTheNetworkIsBounded(t *testing.T) {
	repo := t.TempDir()
	bbGit(t, repo, "init", "-q", "-b", "main")
	bbCommitFile(t, repo, "README.md", "x\n")
	// An ext:: remote runs a command of our choosing, which is the only portable
	// way to make git block. It needs the transport allowed explicitly.
	bbGit(t, repo, "config", "protocol.ext.allow", "always")
	bbGit(t, repo, "remote", "add", "origin", "ext::sleep 20")

	start := time.Now()
	_, err := runGit(repo, 300*time.Millisecond, "fetch", "origin", DefaultBaseBranch)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("runGit returned no error for a fetch whose deadline had passed")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, want it to name the deadline rather than the remote's failure", err)
	}
	// The deadline is 300ms and the remote hangs for 20s. Waiting out the hang is
	// the failure this catches: killing git is not enough, because git's helper
	// inherits the output pipe and WaitDelay is what stops Wait blocking on it.
	if elapsed > 10*time.Second {
		t.Errorf("the call took %s for a 300ms deadline against a 20s hang — the bound does not bound", elapsed)
	}
}

// TestTheDiffIsMeasuredFromTheRefTheTreeIsCutFrom is the composition failure
// that the dispatch-side change would otherwise have introduced, and it is the
// reason the anchor of the merge-base matters.
//
// A task branch is cut from the integration TIP (ensureWorktree). The tree a
// gate verifies is cut from that same tip (prepareIntegrationTree). If the change
// is measured from refs/heads/main instead — what this clone last heard — then
// with a merge on the forge that nobody has pulled, merge-base reaches back past
// the branch point and the "task's change" carries the merge as well. The gate
// then applies a patch whose contents are already in the tree it is applying to,
// which fails, and the task is refused for a defect in the measuring.
func TestTheDiffIsMeasuredFromTheRefTheTreeIsCutFrom(t *testing.T) {
	origin, _, workspace := bbOriginAndClones(t)

	// The forge moves; the shared checkout's local main does not.
	stage := filepath.Join(filepath.Dir(workspace), "stage")
	bbGit(t, filepath.Dir(workspace), "clone", "-q", origin, stage)
	bbGit(t, stage, "config", "user.name", "test")
	bbGit(t, stage, "config", "user.email", "test@test")
	bbCommitFile(t, stage, "dependency.sql", "-- the dependency's migration\n")
	bbGit(t, stage, "push", "-q", "origin", "main")

	// Dispatch, through the production path: branch from the tip, linked
	// worktree of the shared checkout (so refs/heads/main is the stale one on
	// both sides).
	const branch = "task/T0001-x"
	if err := ensureWorktree(workspace, "T0001", branch); err != nil {
		t.Fatalf("ensureWorktree: %v", err)
	}
	taskWT := filepath.Join(WorktreesDir(workspace), "T0001")
	if err := os.WriteFile(filepath.Join(taskWT, "README.md"), []byte("# seed\nthe task's change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tip, err := IntegrationTip(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveRegistry(workspace, &WorkerRecord{
		TaskID: "T0001", RunID: "r", Branch: branch, Worktree: taskWT,
		ResultDir: taskWT, BaselineSHA: bbResolve(t, workspace, tip), StartedAt: "t",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(IntegrationTreeDir(workspace, "T0001")), 0o755); err != nil {
		t.Fatal(err)
	}

	// The diff is the task's contribution and nothing else. The merge is not the
	// task's work, and it is already in the tree the diff gets applied to.
	rec, err := LoadRegistry(workspace, "T0001")
	if err != nil {
		t.Fatal(err)
	}
	change, err := taskWorktreeDiff(rec)
	if err != nil {
		t.Fatalf("taskWorktreeDiff: %v", err)
	}
	if strings.Contains(change, "dependency.sql") {
		t.Errorf("the task's change carries a merge the branch was cut from — measuring against refs/heads/main instead of the tip:\n%s", nonEmptyLines(change, 40))
	}
	if !strings.Contains(change, "the task's change") {
		t.Errorf("the task's own change is missing from the diff:\n%s", nonEmptyLines(change, 40))
	}

	// And the tree the gate builds must take that diff: this is the failure the
	// measuring prevents, so assert the outcome rather than the text.
	dir, cleanup, err := prepareIntegrationTree(workspace, "T0001")
	if err != nil {
		t.Fatalf("the task's change does not apply to the tree it was measured against: %v", err)
	}
	defer cleanup()
	if _, err := os.Stat(filepath.Join(dir, "dependency.sql")); err != nil {
		t.Errorf("the integration tree is missing the merge: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "the task's change") {
		t.Errorf("the integration tree does not carry the task's change: %q", body)
	}
}

// TestTheRemoteBranchCheckIsNotAPatternMatch: `git ls-remote origin main` takes a
// PATTERN, and a pattern without a slash matches the last path component — so a
// remote whose only branch is feature/main answers "yes, it has main". That
// answer is the whole judgement in the failure path ("nothing is ahead of a
// branch the forge does not have"), so it has to be about the ref, not a match.
func TestTheRemoteBranchCheckIsNotAPatternMatch(t *testing.T) {
	origin, publisher, workspace := bbOriginAndClones(t)
	bbGit(t, publisher, "checkout", "-q", "-b", "feature/main")
	bbGit(t, publisher, "push", "-q", "origin", "feature/main")
	// The branch really is absent from the forge — delete it there, so this
	// cannot pass by the fixture quietly having the ref it asks about.
	bbGit(t, origin, "branch", "-D", "main")

	has, err := remoteHasBranch(workspace, DefaultBaseBranch)
	if err != nil {
		t.Fatalf("remoteHasBranch: %v", err)
	}
	if has {
		t.Error("the remote has no main for this repository (only feature/main), but the check said it does — a pattern match read as an answer")
	}
}

// TestAFetchDoesNotFollowTags: the fetch exists to read ONE ref, and a fetch
// follows tags by default. The refs it writes are attributed to whoever
// triggered the fetch, so a tag arriving as a side effect becomes a change
// somebody is asked to account for.
func TestAFetchDoesNotFollowTags(t *testing.T) {
	_, publisher, workspace := bbOriginAndClones(t)
	bbGit(t, publisher, "tag", "v-from-the-forge")
	bbGit(t, publisher, "push", "-q", "origin", "v-from-the-forge")

	if _, err := IntegrationTip(workspace); err != nil {
		t.Fatalf("IntegrationTip: %v", err)
	}
	if out, err := bbGitErr(workspace, "rev-parse", "--verify", "--quiet", "refs/tags/v-from-the-forge"); err == nil {
		t.Errorf("the fetch wrote refs/tags/v-from-the-forge (%s) — a dispatch or a gate is not the publisher of a release", strings.TrimSpace(out))
	}
}

func nonEmptyLines(s string, max int) string {
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			kept = append(kept, line)
		}
		if len(kept) == max {
			break
		}
	}
	return strings.Join(kept, "\n")
}

// bbGitErr is bbGit for the calls whose FAILURE is the expected answer.
func bbGitErr(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestIntegrationTipIsTheLocalBranchWhenTheRemoteHasNoSuchBranch: "the remote
// does not have this branch" is not "I cannot tell how current the branch is".
// Nothing on the forge is ahead of a branch the forge does not have, so the
// local branch is the answer and not a failure.
//
// This is not a hypothetical. The four-gate e2e fixture
// (tests/acceptance/supervisor-git-e2e.sh) is a repository whose origin is a
// bare repo the script has just created — empty. `git fetch origin
// +refs/heads/main:refs/remotes/origin/main` fails there with "couldn't find
// remote ref refs/heads/main", exit 128, and treating that as fatal turned every
// dispatch in that fixture into `worker spawn T0001` returning 1.
func TestIntegrationTipIsTheLocalBranchWhenTheRemoteHasNoSuchBranch(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	bbGit(t, root, "init", "-q", "--bare", "-b", "main", origin)

	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	bbGit(t, repo, "init", "-q", "-b", "main")
	head := bbCommitFile(t, repo, "README.md", "# only local\n")
	bbGit(t, repo, "remote", "add", "origin", origin)

	tip, err := IntegrationTip(repo)
	if err != nil {
		t.Fatalf("IntegrationTip failed because the remote has no main yet (%v) — a repository whose origin is empty cannot be dispatched to", err)
	}
	if want := "refs/heads/" + DefaultBaseBranch; tip != want {
		t.Errorf("IntegrationTip = %q, want the local %q", tip, want)
	}
	if got := bbResolve(t, repo, tip); got != head {
		t.Errorf("%s resolves to %s, want the local %s", tip, got, head)
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
