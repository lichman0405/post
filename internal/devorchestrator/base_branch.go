package devorchestrator

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// integrationFetchTimeout bounds the one git command this package makes over the
// network. Generous for a fetch of one branch; short enough that a wedged
// connection is an error the driver retries rather than a dispatch that never
// returns.
const integrationFetchTimeout = 2 * time.Minute

// The integration branch is load-bearing, and nothing was keeping it current.
//
// Two places depend on "what is main right now" for CORRECTNESS rather than
// convenience:
//
//	worker_spawn.go  a task branch is cut from it, so the Worker's baseline has
//	                 to contain every dependency the DAG calls merged
//	gate_run.go      the tree a gate verifies is "current main plus the task's
//	                 change", so a stale main grades a composition that no
//	                 longer exists
//
// Both used to read refs/heads/main — the local clone. Only a merge moves
// origin/main, and the merge is performed with `gh pr merge`, which changes the
// forge and not the clone. So between a merge and somebody's next `git pull`,
// local main trails the truth. T0203 was spawned six seconds after its
// dependency was recorded merged and one hundred and two seconds before local
// main caught up: it reported `version after head = 25, want 21` on three
// migration tests — the exact consequence of a baseline without the
// dependency's migration — and was rejected for it (issue #123).
//
// The fix is not to fast-forward the checkout. The shared checkout carries
// uncommitted state by design (`tasks/task_status.json` moves on every
// transition), and a fast-forward refuses to overwrite a locally modified file —
// so it is refused whenever the incoming commits touch that file. That is not
// every merge (a task PR's own squash merge does not touch it; the bookkeeping
// commits on main do), so an ff would usually have worked. Usually is not
// enough: the reader must be right for the next commit rather than this week's,
// it shares the tree with the Supervisor's own git work and the driver's own
// bookkeeping, and a fast-forward is a mutation of that shared state performed
// by the wrong code. Reading the ref that merges move cannot be refused by local
// state and mutates nothing.

// IntegrationTip fetches the integration branch and returns the ref that names
// its current state.
//
// With an `origin` remote the answer is the remote-tracking ref: the forge is
// where merges happen, so it is the only place that is current by
// construction. Without one there is nothing to be current with, and a
// local-only repository (every fixture in this package) falls back to the local
// branch, then to HEAD for a repository that has no commits yet.
//
// A fetch that FAILS is an error rather than a fallback to the local ref: the
// whole point is that a dispatch and a gate must not proceed on a ref of
// unknown age. A delayed dispatch costs less than a Worker run spent building
// on, or a gate spent grading, the wrong tree. A fetch that SUCCEEDS but leaves
// no remote-tracking ref is an error for the same reason — see below.
//
// The one fetch failure that is NOT an error is "the remote does not have this
// branch at all": there is nothing there to be behind, so the local branch is
// the answer. That case is asked of the remote directly rather than inferred
// from the fetch's stderr — see the failure path below.
func IntegrationTip(repoRoot string) (string, error) {
	remote, err := remoteExists(repoRoot, "origin")
	if err != nil {
		return "", err
	}
	if !remote {
		return localTip(repoRoot)
	}
	// The destination is named rather than left to `remote.origin.fetch`. A
	// plain `git fetch origin main` updates the tracking ref only when the
	// configured refspec covers it, so a narrowed one leaves
	// refs/remotes/origin/main exactly where it was — present, and stale, with
	// no signal that anything went wrong. Naming the destination makes the ref
	// the fetch's own proof: if the fetch exits 0, it wrote this ref.
	tip := "refs/remotes/origin/" + DefaultBaseBranch
	refspec := "+refs/heads/" + DefaultBaseBranch + ":" + tip
	// --no-tags: a fetch follows tags by default, so it writes refs this
	// function did not ask for and cannot vouch for. `collect` reads the ref
	// ledger to attribute a change, and a tag that arrived as a side effect of
	// reading main would be attributed to whoever happened to trigger the fetch.
	if _, err := runGit(repoRoot, integrationFetchTimeout, "fetch", "--no-tags", "origin", refspec); err != nil {
		// A failed fetch is not one condition. "I cannot reach the forge" and
		// "the forge has no such branch" both fail it, and only the first is a
		// ref of unknown age: if the branch is not on the remote, the local
		// branch is not behind it, it IS the integration branch. Ask the remote
		// which of the two this is rather than reading the fetch's stderr.
		//
		// Getting this wrong is not hypothetical. The four-gate e2e fixture
		// (tests/acceptance/supervisor-git-e2e.sh) is a repository whose origin
		// is a bare repo it has just created, and it is empty: treating "no
		// such branch on the remote" as a hard error failed every dispatch in
		// it — `worker spawn T0001` returned 1 before the Worker ever started.
		onRemote, lerr := remoteHasBranch(repoRoot, DefaultBaseBranch)
		if lerr != nil {
			return "", fmt.Errorf("fetching origin/%s: %w (and asking the remote whether it has the branch: %v) — a dispatch or a gate must not proceed from a %s of unknown age", DefaultBaseBranch, err, lerr, DefaultBaseBranch)
		}
		if !onRemote {
			return localTip(repoRoot)
		}
		return "", fmt.Errorf("fetching origin/%s: %w — a dispatch or a gate must not proceed from a %s of unknown age", DefaultBaseBranch, err, DefaultBaseBranch)
	}
	exists, err := refExists(repoRoot, tip)
	if err != nil {
		return "", err
	}
	if !exists {
		// Not reachable through the fetch above: an explicit refspec either
		// writes its destination or fails the command (a remote without this
		// branch gives "couldn't find remote ref main", exit 128 — measured,
		// both for an empty remote and for one whose only branch is master).
		// Kept because the answer to "the ref is not there after a fetch that
		// claimed to write it" must be an error, never a quiet fall back to
		// the local branch.
		return "", fmt.Errorf("fetched origin/%s but %s does not exist afterwards, so the fetch cannot be confirmed", DefaultBaseBranch, tip)
	}
	return tip, nil
}

// localTip falls back to the local branch, then to HEAD, for a repository with
// no remote to be current with.
func localTip(repoRoot string) (string, error) {
	ref := "refs/heads/" + DefaultBaseBranch
	exists, err := refExists(repoRoot, ref)
	if err != nil {
		return "", err
	}
	if !exists {
		// An unborn branch (`git init` with no commits): HEAD is the only
		// answer, and is safe precisely because there is no second branch to
		// confuse it with.
		return "HEAD", nil
	}
	return ref, nil
}

// remoteHasBranch asks `origin` whether it has the branch, which is a different
// question from whether a fetch of it succeeded: an unreachable remote fails
// both, and only the first of those two failures means "there is nothing to be
// current with". `ls-remote` answers it without writing any local ref — exit 0
// with no output is "no such ref", which is the answer this asks for, not an
// error.
//
// Bounded like the fetch, because it is the same network and the same reason.
func remoteHasBranch(repoRoot, branch string) (bool, error) {
	out, err := runGit(repoRoot, integrationFetchTimeout, "ls-remote", "--heads", "origin", branch)
	if err != nil {
		return false, err
	}
	// The argument is a PATTERN, matched against the ref name, so "main" also
	// matches refs/heads/feature/main — and then "the remote has this branch"
	// is answered yes for a remote that has no such branch, which is the
	// fall-back decision made on a false premise. Compare the ref name.
	want := "refs/heads/" + branch
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == want {
			return true, nil
		}
	}
	return false, nil
}

// integrationBase names the ref a task's change is measured against, and it has
// to be the ref the integration tree is cut from or the patch does not apply.
//
// Both cuts name the same thing — ensureWorktree creates the task branch from
// IntegrationTip, and prepareIntegrationTree builds the verified tree from
// IntegrationTip — so measuring against anything else is measuring against a
// different commit. refs/heads/main, this clone's copy, is behind the tip
// whenever a merge has landed and nobody has pulled: merge-base(local main,
// HEAD) then reaches back PAST the branch point, the "task's change" comes out
// carrying every commit merged since, and applying it to a tree that already
// has them fails — in the gate and in rebaseline alike.
//
// refs/remotes/origin/main is where merges actually land, and every dispatch,
// gate and rebaseline refreshes it (IntegrationTip), so the two readers agree at
// the moment they are used. Without an origin — every fixture in this package —
// the local branch is the only answer there is, and is correct by construction.
func integrationBase(dir string) string {
	tracking := "refs/remotes/origin/" + DefaultBaseBranch
	if ok, err := refExists(dir, tracking); err == nil && ok {
		return tracking
	}
	return DefaultBaseBranch
}

func remoteExists(repoRoot, name string) (bool, error) {
	out, err := gitOutput(repoRoot, "remote")
	if err != nil {
		return false, fmt.Errorf("listing remotes: %w", err)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == name {
			return true, nil
		}
	}
	return false, nil
}

// refExists reports whether a fully-qualified ref resolves.
func refExists(repoRoot, ref string) (bool, error) {
	_, err := gitOutput(repoRoot, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		// `rev-parse --verify --quiet` exits 1 for a ref that does not exist,
		// which is the answer this asks for, not a failure.
		if exitCodeOf(err) == 1 {
			return false, nil
		}
		return false, fmt.Errorf("checking %s: %w", ref, err)
	}
	return true, nil
}

func exitCodeOf(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}
