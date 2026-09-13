package devorchestrator

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// RebaselineTask advances a task's baseline onto the integration branch while
// keeping its work.
//
// The task branch IS the baseline (worker_spawn reads refs/heads/task/<TASK>),
// so moving the branch moves the baseline. That has been done by hand five
// times now — T0102, T0103, T0106, T0201, T0301 — every time because main moved
// under a task while it was being reviewed, which is normal: the Supervisor
// merges other work, and the scratch gate then correctly refuses a change that
// no longer composes. The refusal is right; repeating a ten-step git sequence
// by hand is not.
//
// The one genuinely interesting part is generated files. A derived artifact —
// specs/SPEC_VERSION.json, specs/database/postgres.sql — is a function of its
// inputs, so a textual three-way merge of it is meaningless: applying the
// branch's version would restore a digest that describes the branch's old specs,
// and applying main's would drop the task's spec edits. Both are wrong. The
// correct resolution is to REGENERATE it from the merged tree, which is what
// derived-artifacts.json declares the artifact to be.
//
// So: exclude every derived artifact from the patch, apply the rest, regenerate
// the excluded ones, and verify the change set is otherwise identical.
type RebaselineResult struct {
	TaskID      string   `json:"task_id"`
	FromSHA     string   `json:"from_sha"`
	ToSHA       string   `json:"to_sha"`
	Files       int      `json:"files"`
	Regenerated []string `json:"regenerated"`
}

// Regenerators maps a derived artifact to the command that regenerates it, run
// with the worktree as cwd so it reads that tree's inputs rather than the
// Supervisor's.
// The Write command regenerates the artifact; the Check command asserts it now
// describes the tree it sits in. The check is not decoration: convergence to a
// fixed point is what makes the result correct, and failing to converge is
// exactly the bug this had first — a marker computed before the snapshot it
// describes had been rewritten. The postcondition is asserted, not assumed.
var regenerators = map[string]struct {
	Write []string
	Check []string
}{
	"specs/SPEC_VERSION.json":     {Write: []string{"python3", "scripts/spec_version.py", "--write"}, Check: []string{"python3", "scripts/spec_version.py", "--check"}},
	"specs/database/postgres.sql": {Write: []string{"python3", "scripts/gen_schema_snapshot.py"}, Check: []string{"python3", "scripts/gen_schema_snapshot.py", "--check"}},
}

// RebaselineTask performs the advance. It does not reject or re-dispatch: the
// caller owns the state machine, and the reason a task is being sent back is
// the caller's to phrase.
func RebaselineTask(repoRoot, taskID, dagPath, statePath string) (*RebaselineResult, error) {
	rec, err := LoadRegistry(repoRoot, taskID)
	if err != nil || rec == nil {
		return nil, fmt.Errorf("rebaseline needs the task's Worker record: %w", err)
	}
	if rec.ExitStatus == nil {
		return nil, fmt.Errorf("the Worker for %s is still running — rebaselining a live Worker would pull the tree out from under it", taskID)
	}
	if _, err := os.Stat(rec.Worktree); err != nil {
		return nil, fmt.Errorf("worktree %s: %w", rec.Worktree, err)
	}
	from := rec.BaselineSHA
	to, err := gitOutput(repoRoot, "rev-parse", DefaultBaseBranch)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", DefaultBaseBranch, err)
	}
	if to == from {
		return nil, fmt.Errorf("%s is already at %s — nothing to advance", taskID, from[:12])
	}

	// Capture the COMPLETE change first: tracked modifications plus every
	// untracked file, which `git diff` alone omits. This is the same document
	// the review is handed, and it is the whole insurance policy for what
	// follows.
	change, err := taskWorktreeDiff(rec)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(change) == "" {
		return nil, fmt.Errorf("%s has no change to carry across", taskID)
	}
	before, err := worktreeChangedPaths(rec.Worktree, from)
	if err != nil {
		return nil, err
	}
	patch := filepath.Join(os.TempDir(), "post-rebaseline-"+taskID+".patch")
	if err := os.WriteFile(patch, []byte(change), 0o600); err != nil {
		return nil, err
	}
	defer os.Remove(patch)

	// Which derived artifacts does this change touch? Those travel as a
	// regeneration, not as text.
	derived, err := DerivedArtifactsAt(repoRoot, DefaultDerivedArtifactsPath)
	if err != nil {
		return nil, err
	}
	exclude := map[string]bool{}
	for _, r := range derived.Rules {
		exclude[r.Derived] = true
	}
	applyArgs := []string{"apply"}
	for d := range exclude {
		applyArgs = append(applyArgs, "--exclude="+d)
	}
	applyArgs = append(applyArgs, patch)

	if _, err := gitOutput(rec.Worktree, "reset", "--hard", to); err != nil {
		return nil, fmt.Errorf("resetting the worktree to %s: %w", to[:12], err)
	}
	if _, err := gitOutput(rec.Worktree, "clean", "-fdq"); err != nil {
		return nil, fmt.Errorf("cleaning the worktree: %w", err)
	}
	if _, err := gitOutput(rec.Worktree, applyArgs...); err != nil {
		return nil, fmt.Errorf("the task's change does not apply to %s even after excluding generated files — this needs a human: %w", DefaultBaseBranch, err)
	}

	// Regenerate the excluded artifacts from the MERGED tree, to a fixed point.
	//
	// One pass is not enough, and the reason is a real dependency: the spec
	// version marker is derived from ALL of specs/**, which includes
	// specs/database/postgres.sql — itself derived from infra/migrations/**.
	// Regenerating the marker in the same pass as postgres.sql computes a digest
	// of the tree as it was BEFORE postgres.sql was rewritten, and the result is
	// a marker that describes the old specs. That is what this did the first
	// time: T0301 came out with a marker matching main while its schema
	// snapshot did not, which spec-validation would have caught later, at CI,
	// after a rework had already been spent on it.
	wanted := map[string]bool{}
	var regenerated []string
	for _, r := range derived.Rules {
		if !before[r.Derived] || wanted[r.Derived] {
			continue
		}
		if _, ok := regenerators[r.Derived]; !ok {
			return nil, fmt.Errorf("%s is a derived artifact of %s but has no regenerator — add one to regenerators in rebaseline.go rather than merging it as text", r.Derived, r.Marker)
		}
		wanted[r.Derived] = true
		regenerated = append(regenerated, r.Derived)
	}
	sort.Strings(regenerated)

	runAll := func() error {
		for _, art := range regenerated {
			cmd := regenerators[art].Write
			c := exec.Command(cmd[0], cmd[1:]...)
			c.Dir = rec.Worktree
			if out, err := c.CombinedOutput(); err != nil {
				return fmt.Errorf("regenerating %s in the merged tree: %v: %s", art, err, strings.TrimSpace(string(out)))
			}
		}
		return nil
	}
	fingerprint := func() (string, error) {
		var b strings.Builder
		for _, art := range regenerated {
			data, err := os.ReadFile(filepath.Join(rec.Worktree, art))
			if err != nil {
				return "", err
			}
			b.Write(data)
		}
		return b.String(), nil
	}
	for pass := 0; pass < 4; pass++ {
		beforePass, err := fingerprint()
		if err != nil {
			return nil, err
		}
		if err := runAll(); err != nil {
			return nil, err
		}
		afterPass, err := fingerprint()
		if err != nil {
			return nil, err
		}
		if beforePass == afterPass && pass > 0 {
			break // stable: every derived artifact now describes the final tree
		}
	}

	// The postcondition: every regenerated artifact now describes the tree it
	// sits in. Anything else means the passes did not converge, and shipping a
	// marker that describes different specs fails later, at CI, after a rework
	// has been spent.
	for _, art := range regenerated {
		cmd := regenerators[art].Check
		c := exec.Command(cmd[0], cmd[1:]...)
		c.Dir = rec.Worktree
		if out, err := c.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("%s does not describe the merged tree after regenerating — refusing the advance: %s", art, strings.TrimSpace(string(out)))
		}
	}

	// The advance moved the task branch onto the new baseline; the ledger
	// follows the ref (ref_ledger.go), so a sibling Worker collecting right now
	// still finds this branch attributed to the Supervisor.
	if err := RecordSupervisorRef(repoRoot, "refs/heads/"+rec.Branch, to, "rebaseline", taskID); err != nil {
		return nil, fmt.Errorf("recording refs/heads/%s in the Supervisor ref ledger: %w", rec.Branch, err)
	}

	after, err := worktreeChangedPaths(rec.Worktree, to)
	if err != nil {
		return nil, err
	}
	// The change set must be the same set of paths, modulo the regenerated
	// ones: anything else means the advance lost or invented work.
	missing := difference(before, after)
	delete(missing, "")
	for _, r := range regenerated {
		// A regenerated artifact may legitimately stop differing from the
		// merged tree — that is a real outcome (the task's spec edit is what
		// main already says), not a lost file.
		delete(missing, r)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("the advance lost %d path(s) the task had changed: %v — refusing, the worktree is at %s", len(missing), keysOf(missing), to[:12])
	}
	return &RebaselineResult{TaskID: taskID, FromSHA: from, ToSHA: to, Files: len(after), Regenerated: regenerated}, nil
}

// worktreeChangedPaths lists every path that differs from base, tracked or not.
func worktreeChangedPaths(worktree, base string) (map[string]bool, error) {
	out := map[string]bool{}
	tracked, err := gitOutput(worktree, "diff", "--name-only", base, "--")
	if err != nil {
		return nil, err
	}
	untracked, err := gitOutput(worktree, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	for _, list := range []string{tracked, untracked} {
		for _, p := range strings.Split(list, "\n") {
			if p = strings.TrimSpace(p); p != "" {
				out[p] = true
			}
		}
	}
	return out, nil
}

func difference(a, b map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		if !b[k] {
			out[k] = true
		}
	}
	return out
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
