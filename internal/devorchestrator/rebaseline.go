package devorchestrator

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// isAbsentPath reports whether an Lstat error means the path is not there.
// Not-existing is the ordinary case; ENOTDIR counts too, and it is the one a
// path list produces on its own: a task that replaces a tracked directory with
// a file has the file at p and the DELETED entries at p/… in the same list, and
// once p is a file, stat-ing p/… answers "not a directory", not "no such file".
func isAbsentPath(err error) bool {
	return os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR)
}

// rebaselineNow is the clock a kept copy's name is drawn from. It is a variable
// so a test can hold it still: the name has to be unique within a second, and
// "two refusals in the same instant must not merge" is a claim that has to be
// provable rather than usually true.
var rebaselineNow = time.Now

// nextKeepDir makes a fresh directory for one attempt's copy of the task's
// work. A timestamp is not a name: two attempts of the same task can share a
// second, and os.MkdirAll would merge them — one attempt's change.patch
// overwriting the other's, and a refusal that keeps "the work" keeping half of
// it. os.Mkdir refuses to merge, so a collision costs a suffix instead.
func nextKeepDir(repoRoot, taskID string) (string, error) {
	parent := filepath.Join(repoRoot, ".rddev", "runtime", "rebaseline")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", fmt.Errorf("creating %s to keep the task's work: %w", parent, err)
	}
	stem := filepath.Join(parent, taskID+"-"+rebaselineNow().UTC().Format("20060102T150405Z"))
	for n := 0; n < 1000; n++ {
		dir := stem
		if n > 0 {
			dir = fmt.Sprintf("%s-%d", stem, n+1)
		}
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			return dir, nil
		}
		if !os.IsExist(err) {
			return "", fmt.Errorf("creating %s to keep the task's work: %w", dir, err)
		}
	}
	return "", fmt.Errorf("no free name for the kept copy of %s under %s after 1000 tries — remove some of the old ones", taskID, parent)
}

// gitPaths runs a NUL-separated git listing and returns the raw names.
//
// -z is not a nicety. Without it git C-quotes any path that needs it — a
// non-ASCII name comes back as "docs/\350\256\276\350\256\241.md" — and that
// string is not a path on disk: Lstat fails, the path is recorded as absent,
// its bytes are never kept, and a restore that reports success removes nothing.
// Splitting on NUL also removes the need for TrimSpace, which would eat a
// leading space from the first name; a space is a legal character in a filename
// and this code goes to some trouble elsewhere to handle such names.
func gitPaths(dir string, args ...string) ([]string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), ee, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	var paths []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

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
	// Measured from the same commit the patch is, which is NOT rec.BaselineSHA:
	// the patch is the branch's contribution since it diverged from main, so a
	// path set taken from the recorded baseline describes a different thing.
	// They are the same value until the first advance and then diverge, and
	// comparing the two sets is what produced a refusal that named main's own
	// files as "paths the task had changed" — on the second rebaseline of the
	// same task, which is the normal case after a rework.
	diffBase := taskDiffBase(rec)
	before, err := worktreeChangedPaths(rec.Worktree, diffBase)
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
	keep, err := nextKeepDir(repoRoot, taskID)
	if err != nil {
		return nil, err
	}
	// Up to the reset, nothing has been touched: the worktree still holds the
	// task's work, so a failure here removes the directory that was just made
	// for it. A copy that names the task and holds half a snapshot — or nothing
	// at all — is worse than no copy to whoever finds it after a refusal, since
	// the directory is what the error names as "where the work is kept".
	//
	// One armed discard rather than one os.RemoveAll per failure: there were
	// two, and the third failure added between them would have been the one
	// nobody removed. Everything that can refuse from here to the reset is
	// under this defer by construction, and the disarm is a single line after
	// the reset, which is the point past which the copy is the only copy.
	discard := true
	defer func() {
		if discard {
			os.RemoveAll(keep)
		}
	}()
	if err := os.WriteFile(filepath.Join(keep, "change.patch"), []byte(change), 0o600); err != nil {
		return nil, fmt.Errorf("keeping the task's change at %s: %w", keep, err)
	}
	entries, err := snapshot(rec.Worktree, before, keep)
	if err != nil {
		return nil, err
	}
	discard = false
	completed := false
	defer func() {
		if completed {
			os.RemoveAll(keep)
		}
	}()
	fail := func(err error) error {
		return restoreOrExplain(rec.Worktree, head, keep, entries, err, taskID)
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
			if os.IsNotExist(err) {
				// A task may have ADDED the artifact rather than edited it, and
				// an added artifact is excluded from the patch like any other —
				// so after the reset it does not exist yet. Absent is a
				// legitimate first-pass state; the first regeneration is what
				// creates it. Returning the error here instead made the whole
				// advance fail with a bare `open …: no such file or directory`,
				// which is a poor way to report a case the loop is designed for.
				continue
			}
			if err != nil {
				return "", err
			}
			b.Write(data)
		}
		return b.String(), nil
	}
	// Every failure from here on goes through fail(), because from here on the
	// worktree has already been rebuilt: `reset --hard to` moved the task branch
	// and dropped the task's modifications. A plain `return nil, err` at any of
	// these points leaves the branch at the new baseline, the tree without the
	// work, and the gate inputs (written at spawn) describing the old one — so
	// the next `rddev worker collect` fails its HEAD and branch-ref checks and
	// blames the Worker for a state the Supervisor created. fail() puts the ref
	// and the tree back, which also puts the gate inputs back in agreement.
	for pass := 0; pass < 4; pass++ {
		beforePass, err := fingerprint()
		if err != nil {
			return nil, fail(err)
		}
		if err := runAll(); err != nil {
			return nil, fail(err)
		}
		afterPass, err := fingerprint()
		if err != nil {
			return nil, fail(err)
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
			return nil, fail(fmt.Errorf("%s does not describe the merged tree after regenerating — refusing the advance: %s", art, strings.TrimSpace(string(out))))
		}
	}

	// The postcondition: every path the task changed still holds what the task
	// put there, except the artifacts that are regenerated by construction.
	// Comparing against the kept copy rather than against a set of path names
	// is what makes the claim about the WORK rather than about its shape; see
	// verifyTheWorkSurvived for what the set comparison could not see.
	if err := verifyTheWorkSurvived(rec.Worktree, keep, diffBase, entries, regenerated); err != nil {
		return nil, fail(err)
	}

	// The ledger write is LAST. It is what tells a sibling Worker collecting right
	// now that this branch moved under the Supervisor's hand rather than the
	// Worker's (ref_ledger.go), so it must describe a ref that really moved: every
	// failure above it restores the ref to where the ledger still believes it is,
	// and nothing below it can fail. "Last" means after EVERY check that can
	// refuse the advance, and each of the three has a test that drives it here —
	// the apply, the artifact postcondition, and the work-survival postcondition,
	// which the task-replaces-a-tracked-path-with-a-directory shape does reach
	// (T9019) — so the position is pinned rather than argued.
	if err := RecordSupervisorRef(repoRoot, "refs/heads/"+rec.Branch, to, "rebaseline", taskID); err != nil {
		return nil, fail(fmt.Errorf("recording refs/heads/%s in the Supervisor ref ledger: %w", rec.Branch, err))
	}
	// Everything is done; the copy kept for a failure that did not happen is now
	// redundant. A refusal still keeps it, which is why this is a flag rather
	// than a defer os.RemoveAll at the top.
	completed = true
	// Files is the number of paths this advance carried — the entries the
	// snapshot holds — not how many of them still differ from the new baseline.
	// Both describe the change; only the first is the same number on every
	// advance of the same work, which is what makes it reportable.
	return &RebaselineResult{TaskID: taskID, FromSHA: from, ToSHA: to, Files: len(entries), Regenerated: regenerated}, nil
}

// snapshotEntry is one changed path's exact state in the task worktree, as
// taken before the advance overwrites it.
type snapshotEntry struct {
	Path  string      // worktree-relative, as worktreeChangedPaths names it
	State string      // file | symlink | dir | absent
	Mode  os.FileMode // regular files: the permission bits to restore
}

// snapshot is snapshotWorktree, as a variable, for the same reason restore is:
// the failure it guards cannot be produced on demand here. It is the one that
// happens a second before the worktree is rebuilt, and its whole handling is
// "do not leave a directory that names the task and holds nothing" — a claim
// about what is on disk after an error, which needs a real advance to be worth
// anything. A test drives it during one.
var snapshot = snapshotWorktree

// snapshotWorktree copies every changed path out of the worktree into keep,
// so a refused advance can be undone by CONTENT rather than by re-applying the
// patch. The patch is a document (a human reads it, a PR shows it); this is the
// copy that restores, and it works even when the patch no longer fits — which
// is precisely the case that reaches it. A path that is tracked and deleted is
// recorded as "absent": the content is in git and its absence is the change, so
// restoring means removing the file again. What is inside a directory the task
// put where a TRACKED path was is copied too: see the loop below.
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
		e, err := snapshotOne(worktree, files, p)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
		manifest.WriteString(manifestLine(e))
		// A directory standing where a tracked path is: the task replaced that
		// path with a directory, and what is inside it is a changed path git
		// never named — the ignored content `ls-files --others
		// --exclude-standard` leaves out, and any empty directory it cannot
		// represent at all. `reset --hard` does not merely leave this directory
		// alone, it DELETES it, contents and all, because the directory stands
		// in the way of the tracked path it is restoring. So the content has to
		// travel with the snapshot or the restore cannot reproduce the tree it
		// promises to have put back.
		if e.State != "dir" {
			continue
		}
		tracked, err := trackedInHead(worktree, p)
		if err != nil {
			return nil, err
		}
		if !tracked {
			continue
		}
		sub, err := snapshotDirContents(worktree, files, paths, p)
		if err != nil {
			return nil, err
		}
		for _, s := range sub {
			entries = append(entries, s)
			manifest.WriteString(manifestLine(s))
		}
	}
	if err := os.WriteFile(filepath.Join(keep, "MANIFEST.txt"), []byte(manifest.String()), 0o600); err != nil {
		return nil, fmt.Errorf("writing the restore manifest in %s: %w", keep, err)
	}
	return entries, nil
}

// snapshotOne records one changed path into the kept copy and returns what the
// restore has to replay. A path that is tracked and deleted is recorded as
// "absent": the content is in git and its absence is the change, so restoring
// means removing the file again.
func snapshotOne(worktree, files, p string) (snapshotEntry, error) {
	src := filepath.Join(worktree, p)
	st, err := os.Lstat(src)
	if isAbsentPath(err) {
		return snapshotEntry{Path: p, State: "absent"}, nil
	}
	if err != nil {
		return snapshotEntry{}, fmt.Errorf("reading %s to keep the task's work: %w", p, err)
	}
	dst := filepath.Join(files, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return snapshotEntry{}, fmt.Errorf("creating %s: %w", filepath.Dir(dst), err)
	}
	switch {
	case st.Mode()&os.ModeSymlink != 0:
		// The link's TARGET is recorded, never followed: a Worker's tree may
		// hold a symlink, and resolving it here would have the Supervisor copy
		// whatever it points at into a durable artifact. Same reasoning as
		// taskWorktreeDiff's Lstat.
		target, err := os.Readlink(src)
		if err != nil {
			return snapshotEntry{}, fmt.Errorf("reading symlink %s to keep the task's work: %w", p, err)
		}
		if err := os.WriteFile(dst, []byte(target), 0o600); err != nil {
			return snapshotEntry{}, fmt.Errorf("keeping symlink %s: %w", p, err)
		}
		return snapshotEntry{Path: p, State: "symlink"}, nil
	case st.Mode().IsRegular():
		data, err := os.ReadFile(src)
		if err != nil {
			return snapshotEntry{}, fmt.Errorf("reading %s to keep the task's work: %w", p, err)
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return snapshotEntry{}, fmt.Errorf("keeping %s: %w", p, err)
		}
		return snapshotEntry{Path: p, State: "file", Mode: st.Mode().Perm()}, nil
	default:
		// A directory (a gitlink, or a tracked path the task turned into a
		// directory) carries no content of its own; its existence is all
		// there is to restore. What is INSIDE one of these is walked by
		// snapshotDirContents, which is where a directory the task put where a
		// tracked path was is handled.
		//
		// An EMPTY UNTRACKED directory does not reach here and is not
		// restored, because it never reaches the path list either: git
		// cannot represent an empty directory, so `ls-files --others`
		// omits it, and `clean -fdq` removes it on the advance. This is
		// git's own boundary rather than an oversight, but it is a case
		// where a successful advance does not carry everything the task
		// left behind, so it is written down instead of assumed.
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return snapshotEntry{}, fmt.Errorf("keeping directory %s: %w", p, err)
		}
		return snapshotEntry{Path: p, State: "dir"}, nil
	}
}

// snapshotDirContents records everything inside a directory the task put where
// a tracked path was, for the reason given at its caller: `reset --hard` deletes
// that directory to restore the tracked path, so nothing inside it survives on
// its own — and git named none of it, since a path covered by the ignore rules
// is exactly what `ls-files --others --exclude-standard` omits.
//
// The walk is of the FILESYSTEM, not of another git listing, for the reason the
// guard tests in #99 learned twice over: a listing is a selection criterion
// written down once, and what it does not name is invisible. A directory inside
// this one that is empty, a fifo, a symlink — git has no opinion about any of
// them, and all of them are content the restore would have to invent.
func snapshotDirContents(worktree, files string, paths map[string]bool, p string) ([]snapshotEntry, error) {
	var out []snapshotEntry
	err := filepath.WalkDir(filepath.Join(worktree, filepath.FromSlash(p)), func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("reading what is inside %s to keep the task's work: %w", p, err)
		}
		rel, rerr := filepath.Rel(worktree, full)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." || paths[rel] {
			return nil
		}
		e, kerr := snapshotOne(worktree, files, rel)
		if kerr != nil {
			return kerr
		}
		out = append(out, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// trackedInHead reports whether HEAD has a path exactly at p — the question that
// decides whether `reset --hard` will delete a directory standing there, since
// git removes what obstructs the tracked paths it is writing.
func trackedInHead(worktree, p string) (bool, error) {
	out, err := gitOutput(worktree, "ls-tree", "--name-only", "-z", "HEAD", "--", p)
	if err != nil {
		return false, fmt.Errorf("asking whether HEAD tracks %s: %w", p, err)
	}
	for _, name := range strings.Split(out, "\x00") {
		if strings.TrimSpace(name) == p {
			return true, nil
		}
	}
	return false, nil
}

// manifestLine is one line of the manifest a human reads after a refusal.
func manifestLine(e snapshotEntry) string {
	switch e.State {
	case "file":
		return fmt.Sprintf("file\t%04o\t%s\n", e.Mode.Perm(), e.Path)
	case "symlink":
		return fmt.Sprintf("symlink\t-\t%s\n", e.Path)
	case "dir":
		return fmt.Sprintf("dir\t-\t%s\n", e.Path)
	default:
		return fmt.Sprintf("absent\t-\t%s\n", e.Path)
	}
}

// restoreOrExplain puts the worktree back and says which of the two happened.
// It is the only way a failure after the reset leaves this function, so that
// "the worktree was put back" is a claim the code either made or did not — and
// when it could not be made, the error says so and names the copy, because the
// copy is then the only place the task's work exists.
func restoreOrExplain(worktree, head, keep string, entries []snapshotEntry, cause error, taskID string) error {
	if rerr := restore(worktree, head, keep, entries); rerr != nil {
		return fmt.Errorf("%w\nThe worktree could NOT be put back: %v\nThe task's work is kept at %s — restore it by hand before dispatching %s again", cause, rerr, keep, taskID)
	}
	return fmt.Errorf("%w\nThe worktree was put back the way it was found; the change is also kept at %s", cause, keep)
}

// restore is restoreWorktree, as a variable, so that a test can make it fail
// during a REAL advance. The branch that reports a tree that could not be put
// back is the one an operator depends on most and the one the filesystem alone
// will not produce here — every trick that makes a write fail (a read-only
// directory, an unwritable file) is bypassed by root, and the tests run as
// whatever the CI user is.
var restore = restoreWorktree

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
	// Removals first, then everything that puts something back. The order is not
	// cosmetic: a task that replaced a tracked directory with a file has BOTH a
	// file at p and an absent path at p/… in the snapshot, and `reset --hard
	// head` puts head's directory back at p. Emptying p of the file the task
	// deleted is what makes the file at p restorable; in name order p comes
	// first, so the restore used to find p non-empty and give up on a tree it
	// could have reproduced. The other order cannot conflict: in the tree the
	// snapshot describes, nothing sits under a path the snapshot calls absent,
	// because the parent would have had to exist.
	var placements []snapshotEntry
	for _, e := range entries {
		if e.State == "absent" {
			if err := restoreEntry(worktree, keep, e); err != nil {
				return err
			}
			continue
		}
		placements = append(placements, e)
	}
	for _, e := range placements {
		if err := restoreEntry(worktree, keep, e); err != nil {
			return err
		}
	}
	return nil
}

// restoreEntry puts one path back the way the snapshot recorded it: the state it
// had, and the bytes, mode or link target the kept copy holds.
func restoreEntry(worktree, keep string, e snapshotEntry) error {
	dst := filepath.Join(worktree, e.Path)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("recreating the directory for %s: %w", e.Path, err)
	}
	switch e.State {
	case "absent":
		// The task deleted this path, so the restore removes it again — head can
		// hold it, since the task's deletion was uncommitted, and that is the
		// case this branch exists for.
		if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing %s again (the task deleted it): %w", e.Path, err)
		}
	case "dir":
		// A REAL directory is left exactly as it is: MkdirAll is already a no-op
		// there, and a nested git repository — which `clean -fdq` refuses to
		// remove without -ff — is a directory the restore has nothing to say
		// about, so clearing it first made the restore report a failure it did
		// not have.
		//
		// Anything that is not a directory is cleared, because head can hold it:
		// a tracked symlink the task replaced with a directory. MkdirAll would
		// FOLLOW that link and report success leaving it in place, and every
		// write under the path would land in the link's target — a different
		// subtree, which the restore would then report as restored.
		if st, err := os.Lstat(dst); err == nil && !st.IsDir() {
			if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("clearing %s before recreating the directory: %w", e.Path, err)
			}
		}
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return fmt.Errorf("recreating the directory %s: %w", e.Path, err)
		}
	case "symlink":
		// WriteFile and MkdirAll follow a symlink and head can hold one here, so
		// the destination is cleared first: writing "through" it would put the
		// task's bytes at the link's target, a DIFFERENT path, and report the
		// path it was asked to restore as restored.
		if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("clearing %s before restoring it: %w", e.Path, err)
		}
		target, err := os.ReadFile(filepath.Join(keep, "files", filepath.FromSlash(e.Path)))
		if err != nil {
			return fmt.Errorf("reading the kept symlink %s: %w", e.Path, err)
		}
		if err := os.Symlink(string(target), dst); err != nil {
			return fmt.Errorf("recreating the symlink %s: %w", e.Path, err)
		}
	default:
		if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("clearing %s before restoring it: %w", e.Path, err)
		}
		data, err := os.ReadFile(filepath.Join(keep, "files", filepath.FromSlash(e.Path)))
		if err != nil {
			return fmt.Errorf("reading the kept copy of %s: %w", e.Path, err)
		}
		if err := os.WriteFile(dst, data, e.Mode); err != nil {
			return fmt.Errorf("writing %s back: %w", e.Path, err)
		}
		// WriteFile applies the umask; the gate scripts a task may have changed
		// have to come back executable.
		if err := os.Chmod(dst, e.Mode); err != nil {
			return fmt.Errorf("restoring the mode of %s: %w", e.Path, err)
		}
	}
	return nil
}

// verifyTheWorkSurvived is the postcondition: after the advance, every path the
// task changed still holds what the task put there — except the artifacts that
// are regenerated by construction, whose value is a function of the tree and
// therefore correct by definition.
//
// It replaces a comparison of path SETS, which was the wrong instrument twice
// over. A set of names says nothing about content: the advance could put main's
// bytes over the task's, or drop the executable bit the task had set, and the
// sets would still match. And it refused the ordinary case of a change main has
// since made itself — a path that now matches main is in neither set, so the
// check called it lost. The kept copy is the exact pre-advance state, so
// comparing against it is the promise stated rather than inferred.
func verifyTheWorkSurvived(worktree, keep, base string, entries []snapshotEntry, regenerated []string) error {
	skip := map[string]bool{}
	for _, r := range regenerated {
		skip[r] = true
	}
	// Git records WHETHER a file is executable, not how: the one bit it has is
	// 100755 for any exec bit the task set, and `git apply` writes that mode
	// masked by the umask, so a file the task left at 0700 comes back 0755. Both
	// are executable and both are the same change as anything downstream — the
	// commit, the review diff, CI — can see it, which is why this compares the
	// BIT and not the three-bit mask. Comparing the mask refuses a task-created
	// 0700 file forever, with a message that contradicts itself ("came back
	// executable; the task left it executable"), because 0755&0111 is 0111 and
	// 0700&0111 is 0100; it is also exactly the umask-sensitive comparison this
	// check exists to avoid.
	//
	// It is compared only where it is the TASK's own change: where the task left
	// the mode alone, the mode in the advanced tree is main's, which is a change
	// main made rather than work the advance lost.
	baseExec, err := baseExecBits(worktree, base)
	if err != nil {
		return err
	}
	var lost []string
	for _, e := range entries {
		if e.Path == "" || skip[e.Path] {
			continue
		}
		state, mode, data, err := describeState(worktree, e.Path)
		if err != nil {
			return err
		}
		if state != e.State {
			lost = append(lost, fmt.Sprintf("%s is now %s; the task left it %s", e.Path, state, e.State))
			continue
		}
		if e.State == "dir" || e.State == "absent" {
			// A directory's existence is the whole of its state here, and an
			// absent path has just been compared by the line above.
			continue
		}
		// The kept copy holds what the restore would write back: the bytes of a
		// file, the target of a symlink. Reading it is reading the snapshot.
		want, err := os.ReadFile(filepath.Join(keep, "files", filepath.FromSlash(e.Path)))
		if err != nil {
			return fmt.Errorf("reading the kept copy of %s: %w", e.Path, err)
		}
		if !bytes.Equal(data, want) {
			lost = append(lost, fmt.Sprintf("%s came back as %s; the task left %s", e.Path, shortDigest(data), shortDigest(want)))
			continue
		}
		if e.State == "file" && (mode&0o111 != 0) != (e.Mode&0o111 != 0) {
			if fromBase, tracked := baseExec[e.Path]; !tracked || fromBase != (e.Mode&0o111 != 0) {
				lost = append(lost, fmt.Sprintf("%s came back %s; the task left it %s", e.Path, execWord(mode), execWord(e.Mode)))
			}
		}
	}
	if len(lost) > 0 {
		return fmt.Errorf("the advance did not carry the task's work across: %s", strings.Join(lost, "; "))
	}
	return nil
}

// describeState reads one path as snapshotWorktree would record it: the kind,
// the permission bits for a regular file, and the bytes the kept copy also
// holds — for a symlink that is its target, which is what was written there.
func describeState(root, p string) (string, os.FileMode, []byte, error) {
	abs := filepath.Join(root, filepath.FromSlash(p))
	st, err := os.Lstat(abs)
	if isAbsentPath(err) {
		return "absent", 0, nil, nil
	}
	if err != nil {
		return "", 0, nil, fmt.Errorf("reading %s: %w", p, err)
	}
	switch {
	case st.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(abs)
		if err != nil {
			return "", 0, nil, fmt.Errorf("reading the symlink %s: %w", p, err)
		}
		return "symlink", 0, []byte(target), nil
	case st.Mode().IsRegular():
		data, err := os.ReadFile(abs)
		if err != nil {
			return "", 0, nil, fmt.Errorf("reading %s: %w", p, err)
		}
		return "file", st.Mode().Perm(), data, nil
	default:
		return "dir", 0, nil, nil
	}
}

// baseExecBits returns, for every path git has in base, whether git records it
// as executable. A path the task created has no entry — it did not exist in
// base — and the caller judges those by a different rule: git creates a file
// from a patch with the mode the patch names.
func baseExecBits(worktree, base string) (map[string]bool, error) {
	cmd := exec.Command("git", "ls-tree", "-r", "-z", base)
	cmd.Dir = worktree
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("git ls-tree %s: %w: %s", base, ee, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("git ls-tree %s: %w", base, err)
	}
	bits := map[string]bool{}
	for _, rec := range strings.Split(string(out), "\x00") {
		if rec == "" {
			continue
		}
		meta, p, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		modeStr, _, ok := strings.Cut(meta, " ")
		if !ok {
			continue
		}
		mode, err := strconv.ParseUint(modeStr, 8, 32)
		if err != nil {
			return nil, fmt.Errorf("reading the mode git recorded for %s: %w", p, err)
		}
		bits[p] = mode&0o111 != 0
	}
	return bits, nil
}

func execWord(m os.FileMode) string {
	if m&0o111 != 0 {
		return "executable"
	}
	return "not executable"
}

// shortDigest is what a human compares when two files differ: not the whole
// hash, but enough of it to say "these are the same bytes" or "look here".
func shortDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%d bytes, sha256:%s", len(data), hex.EncodeToString(sum[:6]))
}

// worktreeChangedPaths lists every path that differs from base, tracked or not.
// The names are returned exactly as git emits them under -z; see gitPaths for
// why quoting is not a cosmetic matter here.
func worktreeChangedPaths(worktree, base string) (map[string]bool, error) {
	out := map[string]bool{}
	tracked, err := gitPaths(worktree, "diff", "--name-only", "-z", base, "--")
	if err != nil {
		return nil, err
	}
	untracked, err := gitPaths(worktree, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	for _, list := range [][]string{tracked, untracked} {
		for _, p := range list {
			out[p] = true
		}
	}
	return out, nil
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
