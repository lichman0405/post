package devorchestrator

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
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

	// Which derived artifacts does this change touch? Those travel as a
	// regeneration, not as text. Decided BEFORE anything is touched, so a
	// derived artifact with no regenerator is a refusal that never had to
	// rebuild the worktree to find out.
	derived, err := DerivedArtifactsAt(repoRoot, DefaultDerivedArtifactsPath)
	if err != nil {
		return nil, err
	}
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

	exclude := map[string]bool{}
	for _, r := range derived.Rules {
		exclude[r.Derived] = true
	}
	applyArgs := []string{"apply"}
	for d := range exclude {
		applyArgs = append(applyArgs, "--exclude="+d)
	}

	// The advance rebuilds the worktree in place, and every step of that can
	// fail: `reset --hard` drops the task's tracked modifications and
	// `clean -fdq` deletes its untracked files outright, so from the reset
	// onwards the task's deliverable exists nowhere but here. The first version
	// of this kept it in a temp file deleted on the way out, which made a
	// refused apply a *destructive* outcome — and T0301 was one `git apply`
	// away from exactly that: its Worker had extended the G3 gate script, main
	// had rewritten the same region, and the patch no longer applied.
	//
	// So the change is copied to a durable directory first — the diff for a
	// human to read, plus the exact bytes to restore from — and every failure
	// puts the worktree back. Only a completed advance removes the copy.
	head, err := gitOutput(rec.Worktree, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("reading the worktree's HEAD before advancing it: %w", err)
	}
	keep := filepath.Join(repoRoot, ".rddev", "runtime", "rebaseline",
		taskID+"-"+time.Now().UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(keep, 0o755); err != nil {
		return nil, fmt.Errorf("creating %s to keep the task's work: %w", keep, err)
	}
	if err := os.WriteFile(filepath.Join(keep, "change.patch"), []byte(change), 0o600); err != nil {
		return nil, fmt.Errorf("keeping the task's change at %s: %w", keep, err)
	}
	entries, err := snapshotWorktree(rec.Worktree, before, keep)
	if err != nil {
		return nil, err
	}
	completed := false
	defer func() {
		if completed {
			os.RemoveAll(keep)
		}
	}()
	fail := func(err error) error {
		if rerr := restoreWorktree(rec.Worktree, head, keep, entries); rerr != nil {
			return fmt.Errorf("%w\nThe worktree could NOT be put back: %v\nThe task's work is kept at %s — restore it by hand before dispatching %s again", err, rerr, keep, taskID)
		}
		return fmt.Errorf("%w\nThe worktree was put back the way it was found; the change is also kept at %s", err, keep)
	}

	if _, err := gitOutput(rec.Worktree, "reset", "--hard", to); err != nil {
		return nil, fail(fmt.Errorf("resetting the worktree to %s: %w", to[:12], err))
	}
	if _, err := gitOutput(rec.Worktree, "clean", "-fdq"); err != nil {
		return nil, fail(fmt.Errorf("cleaning the worktree: %w", err))
	}
	if _, err := gitOutput(rec.Worktree, append(applyArgs, filepath.Join(keep, "change.patch"))...); err != nil {
		return nil, fail(fmt.Errorf("the task's change does not apply to %s even after excluding generated files — this needs a human: %w", DefaultBaseBranch, err))
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
	// Everything below is done; the copy kept for a failure that did not happen
	// is now redundant. A refusal after this point still keeps it, which is why
	// this is a flag rather than a defer os.RemoveAll at the top.
	completed = true
	return &RebaselineResult{TaskID: taskID, FromSHA: from, ToSHA: to, Files: len(after), Regenerated: regenerated}, nil
}

// snapshotEntry is one changed path's exact state in the task worktree, as
// taken before the advance overwrites it.
type snapshotEntry struct {
	Path  string      // worktree-relative, as worktreeChangedPaths names it
	State string      // file | symlink | dir | absent
	Mode  os.FileMode // regular files: the permission bits to restore
}

// snapshotWorktree copies every changed path out of the worktree into keep,
// so a refused advance can be undone by CONTENT rather than by re-applying the
// patch. The patch is a document (a human reads it, a PR shows it); this is the
// copy that restores, and it works even when the patch no longer fits — which
// is precisely the case that reaches it. A path that is tracked and deleted is
// recorded as "absent": the content is in git and its absence is the change, so
// restoring means removing the file again.
func snapshotWorktree(worktree string, paths map[string]bool, keep string) ([]snapshotEntry, error) {
	files := filepath.Join(keep, "files")
	if err := os.MkdirAll(files, 0o755); err != nil {
		return nil, fmt.Errorf("creating %s: %w", files, err)
	}
	entries := make([]snapshotEntry, 0, len(paths))
	var manifest strings.Builder
	for _, p := range keysOf(paths) { // sorted: the manifest is read by a human
		if p == "" {
			continue
		}
		src := filepath.Join(worktree, p)
		st, err := os.Lstat(src)
		if os.IsNotExist(err) {
			entries = append(entries, snapshotEntry{Path: p, State: "absent"})
			fmt.Fprintf(&manifest, "absent\t-\t%s\n", p)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s to keep the task's work: %w", p, err)
		}
		dst := filepath.Join(files, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, fmt.Errorf("creating %s: %w", filepath.Dir(dst), err)
		}
		switch {
		case st.Mode()&os.ModeSymlink != 0:
			// The link's TARGET is recorded, never followed: a Worker's tree may
			// hold a symlink, and resolving it here would have the Supervisor copy
			// whatever it points at into a durable artifact. Same reasoning as
			// taskWorktreeDiff's Lstat.
			target, err := os.Readlink(src)
			if err != nil {
				return nil, fmt.Errorf("reading symlink %s to keep the task's work: %w", p, err)
			}
			if err := os.WriteFile(dst, []byte(target), 0o600); err != nil {
				return nil, fmt.Errorf("keeping symlink %s: %w", p, err)
			}
			entries = append(entries, snapshotEntry{Path: p, State: "symlink"})
			fmt.Fprintf(&manifest, "symlink\t-\t%s\n", p)
		case st.Mode().IsRegular():
			data, err := os.ReadFile(src)
			if err != nil {
				return nil, fmt.Errorf("reading %s to keep the task's work: %w", p, err)
			}
			if err := os.WriteFile(dst, data, 0o600); err != nil {
				return nil, fmt.Errorf("keeping %s: %w", p, err)
			}
			entries = append(entries, snapshotEntry{Path: p, State: "file", Mode: st.Mode().Perm()})
			fmt.Fprintf(&manifest, "file\t%04o\t%s\n", st.Mode().Perm(), p)
		default:
			// A directory (empty and untracked, or a gitlink) carries no content
			// of its own; its existence is all there is to restore.
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return nil, fmt.Errorf("keeping directory %s: %w", p, err)
			}
			entries = append(entries, snapshotEntry{Path: p, State: "dir"})
			fmt.Fprintf(&manifest, "dir\t-\t%s\n", p)
		}
	}
	if err := os.WriteFile(filepath.Join(keep, "MANIFEST.txt"), []byte(manifest.String()), 0o600); err != nil {
		return nil, fmt.Errorf("writing the restore manifest in %s: %w", keep, err)
	}
	return entries, nil
}

// restoreWorktree puts the worktree back the way the caller found it: the
// branch tip it had, no files the failed advance left behind, and the task's
// own files written back from the snapshot. `reset --hard head` plus the
// snapshot is what makes it exact — head restores everything the task did NOT
// change, the snapshot restores everything it did.
func restoreWorktree(worktree, head, keep string, entries []snapshotEntry) error {
	if _, err := gitOutput(worktree, "reset", "--hard", head); err != nil {
		return fmt.Errorf("resetting the worktree to %s: %w", head, err)
	}
	if _, err := gitOutput(worktree, "clean", "-fdq"); err != nil {
		return fmt.Errorf("cleaning the worktree: %w", err)
	}
	for _, e := range entries {
		dst := filepath.Join(worktree, e.Path)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("recreating the directory for %s: %w", e.Path, err)
		}
		switch e.State {
		case "absent":
			if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("removing %s again (the task deleted it): %w", e.Path, err)
			}
		case "dir":
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return fmt.Errorf("recreating the directory %s: %w", e.Path, err)
			}
		case "symlink":
			target, err := os.ReadFile(filepath.Join(keep, "files", filepath.FromSlash(e.Path)))
			if err != nil {
				return fmt.Errorf("reading the kept symlink %s: %w", e.Path, err)
			}
			if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("clearing %s for the symlink: %w", e.Path, err)
			}
			if err := os.Symlink(string(target), dst); err != nil {
				return fmt.Errorf("recreating the symlink %s: %w", e.Path, err)
			}
		default:
			data, err := os.ReadFile(filepath.Join(keep, "files", filepath.FromSlash(e.Path)))
			if err != nil {
				return fmt.Errorf("reading the kept copy of %s: %w", e.Path, err)
			}
			if err := os.WriteFile(dst, data, e.Mode); err != nil {
				return fmt.Errorf("writing %s back: %w", e.Path, err)
			}
			// WriteFile applies the umask; the gate scripts a task may have
			// changed have to come back executable.
			if err := os.Chmod(dst, e.Mode); err != nil {
				return fmt.Errorf("restoring the mode of %s: %w", e.Path, err)
			}
		}
	}
	return nil
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
