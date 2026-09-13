package devorchestrator

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

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
// transition) and the merge commits that land on main touch that same file, so
// a fast-forward is REFUSED exactly when it is needed — a routine block, not a
// rare one. Instead: origin is the answer, and the local ref stops being
// authoritative for anything a verdict depends on.

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
	if _, err := gitOutput(repoRoot, "fetch", "origin", refspec); err != nil {
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
