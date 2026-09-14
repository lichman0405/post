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

// cleanWhat is the half of the clean's flags that decides WHAT it removes: `-d`,
// without which git leaves whole untracked directories behind. It is written once
// because both command lines below are built from it and they have to agree — the
// snapshot's reach is the dry run's answer, so a flag the dry run did not have
// would be a deletion that nothing recorded, and one it had alone would refuse an
// advance over work that was never at risk.
const cleanWhat = "d"

// cleanArgs is the clean the advance runs (dry false) or the dry run of the very
// same command (dry true). The two differ only in the flags that are about doing
// it rather than about what it does: a dry run has nothing to force, and it must
// not be quiet, because its output is the whole point of it.
func cleanArgs(dry bool) []string {
	if dry {
		return []string{"clean", "-n" + cleanWhat}
	}
	return []string{"clean", "-f" + cleanWhat + "q"}
}

// restoreCleanArgs is the clean the RESTORE runs: the advance's own clean,
// limited to the paths the snapshot holds. It is built from cleanArgs for the
// same reason the advance's own two command lines are built from one constant —
// a flag that decides WHAT is removed, added here alone, would make the restore
// a different command from the one whose reach was asked (T9042) — and the paths
// arrive as `:(literal)` pathspecs, because a path on disk may hold `[`, `*` or
// `?`, which a pathspec would read as a pattern and match some OTHER name with.
func restoreCleanArgs(safe []string) []string {
	args := append(cleanArgs(false), "--")
	for _, p := range safe {
		args = append(args, ":(literal)"+p)
	}
	return args
}

// restoreCleanBatchBytes bounds one invocation's names. argv is capped by
// ARG_MAX (2 MiB here, shared with the environment), and a path costs its own
// bytes plus `:(literal)` plus a pointer — so a task worktree holding about
// 80,000 untracked paths made the restore fail with `argument list too long`
// (fork/exec: E2BIG), on a tree the code before this one restored: a refusal
// reported as "the worktree could NOT be put back" where nothing had been lost.
// The budget is deliberately far under the limit rather than just under it,
// because the environment occupies the same space and is not this function's to
// measure. It is a variable rather than a constant so that a test can lower it:
// the tree that makes one invocation fail for real holds some 80,000 paths,
// which is not a fixture, and a budget of a few hundred bytes takes the same
// loop through the same number of batches.
var restoreCleanBatchBytes = 64 << 10

// restoreClean runs one batch of the restore's clean. It is a variable for the
// same reason restore and snapshot are: what a test has to be able to see here
// is the BATCHING — that the paths went out in more than one invocation, and
// that each invocation is the clean whose reach was asked — and the shape that
// would show it without a seam is the 80,000-path tree again.
var restoreClean = func(worktree string, args ...string) (string, error) {
	return gitOutput(worktree, args...)
}

// restoreCleanBatches splits the paths into runs of one clean each, in order and
// without repeating a path. Batching does not change what the restore removes:
// the union of the batches is the set of paths, every invocation carries the
// same flags (cleanArgs) and its own `:(literal)` pathspecs, and running them one
// after another is what running one would have done — minus the argument list
// that cannot be built.
func restoreCleanBatches(safe []string) [][]string {
	var batches [][]string
	var cur []string
	size := 0
	for _, p := range safe {
		n := len(p) + len(":(literal)") + 1
		if len(cur) > 0 && size+n > restoreCleanBatchBytes {
			batches = append(batches, cur)
			cur, size = nil, 0
		}
		cur = append(cur, p)
		size += n
	}
	if len(cur) > 0 {
		batches = append(batches, cur)
	}
	return batches
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
	// The advance goes to what the integration branch IS, not to what this
	// clone last heard — the third reader of the same question, answered by the
	// same function. A rebaseline exists to bring a task up to date with a
	// dependency that just merged, and the driver runs it exactly then, so a
	// target read from the local branch is stale precisely when it matters: the
	// task would be "advanced" onto a tree that still lacks the dependency it
	// was sent back for. Resolved to a sha because everything below — the
	// nothing-to-advance comparison, the reset, the ref ledger, the result —
	// names a commit.
	toRef, err := IntegrationTip(repoRoot)
	if err != nil {
		return nil, err
	}
	to, err := gitOutput(repoRoot, "rev-parse", toRef)
	if err != nil {
		return nil, fmt.Errorf("resolving %s as the advance target: %w", toRef, err)
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
	// under this defer by construction. The disarm is a single line after the
	// SNAPSHOT — not after the reset, which is where the comment used to put it
	// (the fifth round): by then the copy has content, and every failure from the
	// reset onwards goes through fail(), which keeps it.
	discard := true
	defer func() {
		if discard {
			os.RemoveAll(keep)
		}
	}()
	if err := os.WriteFile(filepath.Join(keep, "change.patch"), []byte(change), 0o600); err != nil {
		return nil, fmt.Errorf("keeping the task's change at %s: %w", keep, err)
	}
	entries, err := snapshot(rec.Worktree, head, to, before, keep)
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
	// The clean's reach, asked again where it is about to happen. The snapshot
	// took its answer one reset ago, and a reset moves what the clean can see: a
	// directory that was not removable while a tracked file sat inside it becomes
	// removable the moment main deletes that file. Nothing has been deleted yet,
	// so a name the snapshot does not hold is a refusal that still has a tree —
	// and the alternative is a clean that deletes it with no other copy anywhere.
	if err := assertTheCleanIsCovered(rec.Worktree, entries); err != nil {
		return nil, fail(err)
	}
	if _, err := gitOutput(rec.Worktree, cleanArgs(false)...); err != nil {
		return nil, fail(fmt.Errorf("cleaning the worktree: %w", err))
	}
	if _, err := gitOutput(rec.Worktree, append(applyArgs, filepath.Join(keep, "change.patch"))...); err != nil {
		textual := err
		// A textual apply asks whether the patch's LINES still fit, and that is
		// the wrong question when a task and main have touched the same file:
		// `git apply` refuses on a CONTEXT line that main rewrote even though the
		// two changes are nowhere near each other, and refusing there is what
		// sent the Supervisor to the ten-step git sequence by hand — T0302 and
		// T0303 both added to the provisioning block of cmd/api/main.go, T0302
		// and T0304 both extended tests/acceptance/gitea-real-services-e2e.sh,
		// and in both shapes the patch's context was the whole of the problem.
		//
		// So the second attempt asks git to COMPOSE the two changes instead:
		// `--3way` merges the patch's pre-image (the task's baseline) against
		// what the worktree holds now, which is main. It is the same three
		// versions, and the same merge, that the postcondition below recomputes
		// to check the result — so a compose that is not reproducible is refused
		// there rather than trusted here.
		_, threeErr := gitOutput(rec.Worktree, append(threeWayApplyArgs(applyArgs), filepath.Join(keep, "change.patch"))...)
		// Read BEFORE the unstage below: the unmerged stages are index entries,
		// and the reset that unstages takes them away with everything else.
		conflicted, cerr := unmergedPaths(rec.Worktree)
		if cerr != nil {
			return nil, fail(cerr)
		}
		// `--3way` implies `--index`, and unlike `git apply` it is not atomic:
		// it merges every path it can and STAGES the result, so both outcomes
		// arrive in a shape the rest of the advance does not expect. On the way
		// through, a composed change has to be an UNCOMMITTED diff, because a
		// Worker's deliverable is one and collect reads it with `git diff`,
		// which does not show what is staged — getting that wrong would report a
		// task that changed nothing. On the way OUT it is worse than cosmetic:
		// a refusal is followed by the restore, and the restore reads the tree
		// to decide what it may delete, while `reset --hard` DELETES a path that
		// is staged and not in the target commit. So every path this three-way
		// attempt created would be gone by the time the restore's clean asks
		// what is there — restored afterwards by the snapshot's write-back, so
		// nothing is lost, but deleted without the guard the restore exists to
		// apply (it removes a path only when the snapshot holds a copy to write
		// back) and never named to the clean that is written to bound exactly
		// that list, nor to the caller either. Unstaging is what both outcomes
		// need: the created paths stay on disk as untracked, which is the shape
		// a refused `git apply` leaves, and the markers this wrote into tracked
		// files go with the restore's own `reset --hard`, which runs before
		// anything is read back.
		if _, err := gitOutput(rec.Worktree, "reset", "-q"); err != nil {
			return nil, fail(fmt.Errorf("unstaging the three-way apply's result: %w", err))
		}
		if threeErr != nil {
			if len(conflicted) > 0 {
				// A real conflict: the two changes rewrite the same lines, and
				// choosing between them needs both halves' reasoning. That is the
				// Supervisor's call — docs/61 §G2 — and never this tool's, so the
				// advance is refused with the paths named. The work survives the
				// refusal: fail() restores the tree, and the kept copy holds every
				// path by content.
				return nil, fail(fmt.Errorf(
					"the task's change does not apply to %s as a patch, and a three-way merge of it conflicts with main's own change to the same lines of %s — composing them needs a human: %w",
					DefaultBaseBranch, strings.Join(conflicted, ", "), textual))
			}
			// No conflict to point at: `--3way` could not compose it either, and
			// the patch is the thing that is wrong.
			return nil, fail(fmt.Errorf("the task's change does not apply to %s even after excluding generated files — this needs a human: %w", DefaultBaseBranch, textual))
		}
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
	if err := verifyTheWorkSurvived(rec.Worktree, keep, diffBase, to, entries, regenerated); err != nil {
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
	// Most of them are the task's change; a few are directories the clean removes
	// whole, which the snapshot carries because nothing else would (cleanReach). So
	// it is the same number on every advance of the same work against the same
	// tree, and it can move by one when main moves a file into or out of one of
	// those directories — a change in what the clean deletes, not in what the task
	// did (T9005 pins both directions of that).
	return &RebaselineResult{TaskID: taskID, FromSHA: from, ToSHA: to, Files: len(entries), Regenerated: regenerated}, nil
}

// threeWayApplyArgs is the same apply, asking git to compose rather than to
// splice. It is built from applyArgs rather than written out beside it so the
// two cannot drift: the excluded derived artifacts have to be excluded from
// both, and a second hand-written list is how one of them stops excluding one.
func threeWayApplyArgs(applyArgs []string) []string {
	out := make([]string, 0, len(applyArgs)+1)
	out = append(out, "apply", "--3way")
	return append(out, applyArgs[1:]...)
}

// unmergedPaths names the paths an attempted three-way apply left in conflict.
//
// Asked of the diff rather than of `ls-files -u`, which answers with
// "<mode> <object> <stage>\t<path>" once per stage — three lines per path, none
// of them a path.
func unmergedPaths(worktree string) ([]string, error) {
	raw, err := gitPaths(worktree, "diff", "--name-only", "-z", "--diff-filter=U")
	if err != nil {
		return nil, fmt.Errorf("listing the paths a three-way apply left in conflict: %w", err)
	}
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		if p != "" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
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
// restoring means removing the file again.
//
// What is recorded is the union of three questions, because the two operations
// that rebuild this worktree — `reset --hard <target>` and `git clean -fdq` —
// destroy what git's path lists do not name:
//
//   - the paths the task changed, which is what the advance has to carry;
//   - the directories standing where HEAD or the target commit has a FILE. Both
//     resets — `reset --hard <target>` and the restore's `reset --hard <head>` —
//     delete such a directory WHOLE, contents and all, because it stands in the
//     way of the tracked path they are writing. What is inside one is exactly
//     what git's path lists do not name: content the ignore rules cover, an
//     empty directory, a fifo. `git status` calls the whole directory ignored,
//     so nothing in worktreeChangedPaths mentions it, and asking the commits is
//     the only way it is found at all (T9026 is that shape: the directory is
//     invisible to git, the target has a file at that path, and without this
//     the reset deleted the task's work in silence).
//   - the paths the CLEAN removes, which is the other operation and had no such
//     question asked of it. It is the one that reaches furthest: it takes a
//     whole untracked directory, and everything inside that directory goes with
//     it without being named by anything — an empty directory, a fifo, a socket,
//     an ignored file standing next to an untracked one. `git status` collapses
//     the directory to one `?? dir/` line and `ls-files --others` lists only the
//     files the ignore rules do not cover, so a plain untracked directory
//     holding a fifo was in neither list, nothing recorded it, and the advance
//     deleted it and reported success (T9027). The answer comes from the clean
//     itself — see cleanReach — and the directories in it are walked for the
//     same reason the obstructed ones are.
func snapshotWorktree(worktree, head, target string, paths map[string]bool, keep string) ([]snapshotEntry, error) {
	files := filepath.Join(keep, "files")
	if err := os.MkdirAll(files, 0o755); err != nil {
		return nil, fmt.Errorf("creating %s: %w", files, err)
	}
	obstructing, err := obstructedDirs(worktree, head, target)
	if err != nil {
		return nil, err
	}
	cleaned, cleanedFiles, err := cleanReach(worktree)
	if err != nil {
		return nil, err
	}
	// whole is every directory the advance deletes CONTENTS AND ALL: the ones a
	// reset clears to write a tracked path, and the ones the clean removes.
	whole := make(map[string]bool, len(obstructing)+len(cleaned))
	recorded := make(map[string]bool, len(paths)+len(obstructing)+len(cleaned))
	for p := range paths {
		recorded[onePathName(p)] = true
	}
	for _, p := range cleanedFiles {
		recorded[onePathName(p)] = true
	}
	for p := range obstructing {
		p = onePathName(p)
		recorded[p] = true
		whole[p] = true
	}
	for p := range cleaned {
		p = onePathName(p)
		recorded[p] = true
		whole[p] = true
	}
	entries := make([]snapshotEntry, 0, len(recorded))
	var manifest strings.Builder
	// taken is what the entries ALREADY hold, which is not the same thing as
	// recorded being true: a walk under one directory reaches paths that are
	// themselves keys of the set — a directory the advance deletes whole standing
	// inside another one that it does too — and the main loop would take them a
	// second time, once from the walk and once as its own key. Both walks and the
	// main loop share this map so that each path is taken exactly once: the same
	// work named twice in the manifest, and counted twice in Files, is a report
	// that says the advance carried two paths where it carried one (T9030).
	taken := make(map[string]bool, len(recorded))
	for _, p := range keysOf(recorded) { // sorted: the manifest is read by a human
		if p == "" || taken[p] {
			continue
		}
		e, err := snapshotOne(worktree, files, p)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
		taken[p] = true
		manifest.WriteString(manifestLine(e))
		// A directory standing where one of those commits has a file: the reset
		// does not merely leave it alone, it DELETES it to write the tracked
		// path — so what is inside travels with the snapshot, or the restore
		// cannot reproduce the tree it promises to have put back. The clean
		// deletes the other kind just as whole, so the same answer follows.
		if e.State != "dir" || !whole[p] {
			continue
		}
		sub, err := snapshotDirContents(worktree, files, taken, p)
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

// onePathName is the single spelling of a path in the record. `git ls-files
// --others` names an untracked DIRECTORY with a trailing slash — a nested
// repository is what it does that for — while every other listing names the
// same directory without one, so the same directory arrived as two keys and was
// recorded, counted in Files and written to the manifest twice, once as
// `scratch/inner` and once as `scratch/inner/` (the sixth round's second
// finding; no work was lost, the report a human reads overstated what was
// carried). A trailing slash is the only difference between the two spellings:
// the slash is the separator and never stands for itself in a path.
func onePathName(p string) string { return strings.TrimRight(p, "/") }

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
	case st.IsDir():
		// A directory (a gitlink, or a tracked path the task turned into a
		// directory) carries no content of its own; its existence is all
		// there is to restore. What is INSIDE one of these is walked by
		// snapshotDirContents, and only when the advance is going to delete it:
		// the directories a reset clears arrive through obstructedDirs, and the
		// ones the clean removes through cleanReach, both of them contents and
		// all.
		//
		// An EMPTY UNTRACKED directory used to be named here as a boundary git
		// imposes — `ls-files --others` cannot list one, so nothing recorded it,
		// and `clean -fdq` removed it on the advance. That was true of the two
		// path lists and false of the advance: the clean removes such a directory
		// whole, and the reach of the clean is now asked of the clean. It arrives
		// here as an entry, and the advance then REFUSES rather than drop it,
		// because a directory is one more thing a patch cannot carry (T9028). A
		// directory that neither operation clears still never reaches here, and no
		// longer needs to: nothing is going to delete it.
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return snapshotEntry{}, fmt.Errorf("keeping directory %s: %w", p, err)
		}
		return snapshotEntry{Path: p, State: "dir"}, nil
	case st.Mode()&os.ModeNamedPipe != 0:
		// A fifo. git has no opinion about it — `ls-files --others` does not even
		// list one — so it is only ever reached through the walk, and it is
		// content the restore has to reproduce AS what it is. Recorded as a `dir`
		// it came back as an empty directory: a refusal that says the tree was
		// put back the way it was found, and hands back a different tree.
		//
		// Nothing is written into the kept copy: a fifo has no bytes, and opening
		// one to copy it would wait for a writer that never comes. The kind and
		// the mode are in the manifest, which is the document a human reads.
		return snapshotEntry{Path: p, State: "fifo", Mode: st.Mode().Perm()}, nil
	default:
		// A socket or a device node. Neither can be reproduced from a copy: a
		// socket is a rendezvous rather than content, and a device node needs a
		// privilege the Supervisor may not have. Recording it as a `dir` and
		// restoring an empty directory is the defect class this function exists
		// to close — a restore that reports success and hands back a different
		// tree — so the advance refuses instead, and refuses HERE: the snapshot
		// runs before the reset, so the worktree still holds everything it had
		// and the copy is not yet the only copy.
		return snapshotEntry{}, fmt.Errorf("%s is a %s, which the advance cannot carry across and will not silently replace with something else — move it out of the way and dispatch again. Nothing has been touched", p, fileKindWord(st.Mode()))
	}
}

// fileKindWord names what an Lstat says a path is, for the paths that are not a
// file, a symlink, a directory or a fifo.
func fileKindWord(m os.FileMode) string {
	switch {
	case m&os.ModeSocket != 0:
		return "socket"
	case m&os.ModeDevice != 0 && m&os.ModeCharDevice != 0:
		return "character device"
	case m&os.ModeDevice != 0:
		return "block device"
	default:
		return "kind of file this does not handle"
	}
}

// snapshotDirContents records everything inside a directory that stands where a
// reset will write a tracked path, for the reason given at its caller: the reset
// deletes that directory whole, so nothing inside it survives on its own — and
// git named none of it, since a path covered by the ignore rules is exactly what
// `ls-files --others --exclude-standard` omits.
//
// The walk is of the FILESYSTEM, not of another git listing, for the reason the
// guard tests in #99 learned twice over: a listing is a selection criterion
// written down once, and what it does not name is invisible. A directory inside
// this one that is empty, a fifo, a symlink — git has no opinion about any of
// them, and every one of them is content the restore would otherwise have to
// invent. What that "invent" looks like when it is got wrong is in snapshotOne:
// a fifo came back as an empty directory.
//
// taken holds every path the snapshot has already taken, and they are skipped
// here: a file inside this directory can be in the task's path list AND in the
// walk (an untracked file under a directory that replaced a tracked path is
// named by `ls-files --others`), and taking it twice would put two lines in the
// manifest and two entries in the count — a report that says the advance carried
// two paths where it carried one. T9023 pins the path list's half of that; T9030
// pins the walk's, where the outer directory reaches the inner one's contents
// before the inner directory has been walked as a directory of its own.
//
// The map is UPDATED as paths are taken, and it is the caller's map: the caller
// reads it before choosing the next key, which is what makes the two walks and
// the main loop one act of taking rather than three that have to agree.
func snapshotDirContents(worktree, files string, taken map[string]bool, p string) ([]snapshotEntry, error) {
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
		if rel == "." || taken[rel] {
			return nil
		}
		e, kerr := snapshotOne(worktree, files, rel)
		if kerr != nil {
			return kerr
		}
		taken[rel] = true
		out = append(out, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// assertTheCleanIsCovered asks the clean a second time — where it is about to run
// rather than where the snapshot was taken — and refuses if it names anything the
// snapshot does not hold.
//
// The snapshot's reach was computed one reset ago, and a reset moves what a clean
// can see. A directory is removable only while it holds nothing tracked, so the
// one whose single tracked file main has just deleted becomes removable the
// moment the reset runs, and everything invisible inside it — an empty directory,
// a fifo — would be deleted along with it, out of a tree nothing took a copy of.
// Asking again costs one dry run and closes the window: the answer is then about
// the tree the clean itself is looking at. If it names something the snapshot
// does not hold, the advance refuses HERE, where the clean has not run: the
// restore behind the refusal puts the tracked file back, the directory stops
// being removable, and the tree comes back whole.
func assertTheCleanIsCovered(worktree string, entries []snapshotEntry) error {
	held := make(map[string]bool, len(entries))
	for _, e := range entries {
		held[e.Path] = true
	}
	dirs, files, err := cleanReach(worktree)
	if err != nil {
		return err
	}
	for _, p := range append(keysOf(dirs), files...) {
		// The snapshot holding the PATH is the whole of the test — not the path or
		// any directory around it. Holding an ancestor is what a directory's own
		// entry means, and it means it only for the paths that were inside it when
		// the walk ran: `gathered/` being walked is a record of that moment, and a
		// file that arrived afterwards is in no entry. The clean names that file
		// individually whenever it cannot name the directory whole — a directory
		// holding an ignore rule of its own is one it cannot remove whole, so what
		// is inside it goes out one name at a time (T9049) — and then the question
		// is about the path itself.
		if !held[p] {
			return fmt.Errorf("the clean that follows the reset would delete %s, and the snapshot does not hold it — the advance refuses rather than delete work it could not put back. Move it out of the way and dispatch again", p)
		}
		if dirs[p] {
			// Holding the DIRECTORY is not holding what is inside it, and the
			// clean removes a directory whole: the entries beside it are the
			// record, one path each, and a path that arrived after the snapshot
			// was taken is in none of them. The same rule as the branch above, at
			// the granularity git actually removes.
			if unheld := unheldInside(worktree, p, held); len(unheld) > 0 {
				return fmt.Errorf("the clean that follows the reset would delete the directory %s, and the snapshot holds no copy of %s inside it — the advance refuses rather than delete work it could not put back. Move %s out of the way and dispatch again", p, named(unheld), named(unheld))
			}
		}
	}
	return nil
}

// obstructedDirs names the directories in the worktree that stand where one of
// the given commits has a FILE. Both resets this function's callers perform do
// the same thing with such a directory: they delete it whole, contents and all,
// because it is in the way of the tracked path being written. That is the only
// reason a directory the ignore rules cover has to be recorded at all.
//
// The names come from the commit, not from a pathspec per path: a file name is
// not a pattern, and asking about a path one at a time is how a name that needs
// quoting — or a name that is only whitespace apart from another — gets treated
// as something it is not. `-z` gives the name as it is on disk. A gitlink is
// skipped: it is a directory on disk, but no reset writes a file over it, and
// walking into one would be walking into another repository.
func obstructedDirs(worktree string, revs ...string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, rev := range revs {
		recs, err := lsTreeRecords(worktree, rev)
		if err != nil {
			return nil, err
		}
		for _, r := range recs {
			if r.Mode&0o170000 == 0o160000 {
				continue // a gitlink: a commit pointer, not a path a reset writes
			}
			st, err := os.Lstat(filepath.Join(worktree, filepath.FromSlash(r.Path)))
			if err != nil || !st.IsDir() {
				continue // nothing stands in the way there
			}
			out[r.Path] = true
		}
	}
	return out, nil
}

// cleanEnv is the environment the clean's dry run reads.
//
// LC_ALL=C is DROPPED-then-APPENDED, not merely appended. glibc's getenv answers
// with the FIRST match, so a locale variable inherited from the Supervisor's shell
// would be the one git read and an LC_ALL added at the end would be read by
// nobody — the parser below would then be reading English sentences in a language
// it does not know, and every line would be a refusal. Dropping LANG, LANGUAGE and
// every LC_* first makes LC_ALL=C the only answer.
func cleanEnv() []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if name == "LANG" || name == "LANGUAGE" || strings.HasPrefix(name, "LC_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "LC_ALL=C")
}

// cleanReach asks the clean what it is about to remove and answers in paths on
// disk: the directories it will delete whole, and the files it will delete.
//
// It is asked of the command itself rather than of a listing that describes it.
// `git status --porcelain` collapses a wholly untracked directory into one `??`
// line and says nothing about what is inside it, and `ls-files --others
// --exclude-standard` names only the files in there that the ignore rules do not
// cover — so a directory whose content is a fifo, a socket, an empty directory,
// or an ignored file standing beside an untracked one is in NEITHER list, and
// `git clean -fd` deletes it whole anyway. What a listing does not name is
// invisible (#99 learned that shape twice), so the reach of an operation is taken
// from the operation: `git clean -nd` is `git clean -fdq` told to do nothing and
// say what it would have done, built from the same flags (cleanArgs).
//
// git has no machine-readable form of that answer — `git clean` has no -z — so it
// is read out of the two sentences git says:
//
//	Would remove <path>
//	Would skip repository <path>
//
// The second is not a removal: a repository inside the tree is left alone, which
// also keeps everything around it from becoming removable, so there is nothing to
// carry. ANY OTHER LINE IS A REFUSAL — that is the reason this parse is not
// lenient. A line it does not understand is a clean whose reach is unknown, and
// an unknown reach is the defect, not something to advance on. LC_ALL=C pins
// those words (the environment is cleared of any other locale setting first,
// because getenv takes the FIRST match and an inherited LC_ALL would win);
// -c core.quotePath=false keeps a name with a non-ASCII character out of the
// octal escapes git would write it in; and the quoting that is left — a quote, a
// backslash, a control character, a newline above all, which would otherwise turn
// one removal into two lines — is read back by unquoteC.
//
// Every name is also Lstat-ed. A dry run names what exists, so a name this got
// wrong is a refusal here, where nothing has been touched, rather than a path
// that quietly is not there when the restore reaches it.
func cleanReach(worktree string) (dirs map[string]bool, files []string, err error) {
	args := append([]string{"-c", "core.quotePath=false"}, cleanArgs(true)...)
	cmd := exec.Command("git", args...)
	cmd.Dir = worktree
	cmd.Env = cleanEnv()
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), ee, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	dirs = map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		p, isDir, skip, err := parseCleanLine(line)
		if err != nil {
			return nil, nil, err
		}
		if skip {
			continue
		}
		if _, err := os.Lstat(filepath.Join(worktree, filepath.FromSlash(p))); err != nil {
			return nil, nil, fmt.Errorf("git clean said it would remove %q, which is not in the worktree (%v) — the advance reads that answer instead of trusting it, and will not act on one it cannot check", p, err)
		}
		if isDir {
			dirs[p] = true
			continue
		}
		files = append(files, p)
	}
	return dirs, files, nil
}

// parseCleanLine reads one line of `git clean -nd`: the path it names, whether
// the path is a whole directory (git says so with a trailing slash), and whether
// the line is one of the skips. Anything else is an error, and the error is the
// point of the function: the caller cannot tell what the clean would delete from
// a line it does not understand, and a clean whose reach is unknown is exactly the
// defect this whole path exists to close.
func parseCleanLine(line string) (p string, isDir, skip bool, err error) {
	rest, ok := strings.CutPrefix(line, "Would remove ")
	if !ok {
		if strings.HasPrefix(line, "Would skip repository ") {
			return "", false, true, nil
		}
		return "", false, false, fmt.Errorf("git clean said %q and this does not know what that means — the advance cannot tell what the clean would remove and will not guess", line)
	}
	p, err = unquoteC(rest)
	if err != nil {
		return "", false, false, fmt.Errorf("reading a path out of git clean's %q: %w", line, err)
	}
	// The trailing slash is git's mark for "the whole directory, contents
	// included". It arrives INSIDE the quotes when the name is quoted — measured,
	// git writes `"back\\slash/"` and `"new\nline/"` — and outside them when the
	// name is written as it is (`sp ace/`). Reading it after the name is decoded
	// catches both, and nothing else can put a slash at the end of a decoded path:
	// the slash is the separator, so it never stands for itself.
	isDir = strings.HasSuffix(p, "/")
	p = strings.TrimSuffix(p, "/")
	return p, isDir, false, nil
}

// unquoteC reads back the one form git writes a path in when it cannot write the
// path itself: a double-quoted C string. git quotes a name holding a quote, a
// backslash or a control character — and, unless core.quotePath is off, one
// holding any byte outside ASCII, which is what the caller turns off. Every other
// name arrives as itself, so the caller never has to decide which of the two it
// is looking at: a name that begins with a quote was quoted, because a name that
// begins with a quote is one git cannot write as it is.
func unquoteC(s string) (string, error) {
	if !strings.HasPrefix(s, `"`) {
		return s, nil
	}
	if len(s) < 2 || !strings.HasSuffix(s, `"`) {
		return "", fmt.Errorf("%s opens a quote it does not close", s)
	}
	body := s[1 : len(s)-1]
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(body) {
			return "", fmt.Errorf("%s ends in half an escape", s)
		}
		switch e := body[i]; e {
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'v':
			b.WriteByte('\v')
		case '\\', '"':
			b.WriteByte(e)
		case '0', '1', '2', '3', '4', '5', '6', '7':
			// One byte, three octal digits: how git writes everything it cannot
			// write as itself, including each byte of a multi-byte character.
			if i+2 >= len(body) {
				return "", fmt.Errorf("%s ends in half an octal escape", s)
			}
			n, err := strconv.ParseUint(body[i:i+3], 8, 8)
			if err != nil {
				return "", fmt.Errorf("%s has %q where an octal escape should be", s, body[i:i+3])
			}
			b.WriteByte(byte(n))
			i += 2
		default:
			return "", fmt.Errorf("%s escapes %q, which is not an escape this knows", s, string(e))
		}
	}
	return b.String(), nil
}

// lsTreeRecord is one entry of `git ls-tree -r -z <rev>`: the mode, the object
// holding the content, and the path — which -z gives as the name on disk rather
// than as git's quoted rendering of it.
type lsTreeRecord struct {
	Mode   uint64 // octal, as git records it: 100644, 100755, 120000, 160000
	Path   string
	Object string
}

func lsTreeRecords(worktree, rev string) ([]lsTreeRecord, error) {
	cmd := exec.Command("git", "ls-tree", "-r", "-z", rev)
	cmd.Dir = worktree
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("git ls-tree %s: %w: %s", rev, ee, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("git ls-tree %s: %w", rev, err)
	}
	var recs []lsTreeRecord
	for _, rec := range strings.Split(string(out), "\x00") {
		if rec == "" {
			continue
		}
		meta, p, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		fields := strings.Split(meta, " ")
		if len(fields) != 3 {
			continue
		}
		mode, err := strconv.ParseUint(fields[0], 8, 32)
		if err != nil {
			return nil, fmt.Errorf("reading the mode git recorded for %s: %w", p, err)
		}
		recs = append(recs, lsTreeRecord{Mode: mode, Path: p, Object: fields[2]})
	}
	return recs, nil
}

// manifestLine is one line of the manifest a human reads after a refusal. The
// path is written by manifestPath, so that one entry is one line whatever the
// name holds: written as it is, a name with a newline in it produced two lines and
// a reader counting them counted one path where the advance carried two.
func manifestLine(e snapshotEntry) string {
	switch e.State {
	case "file":
		return fmt.Sprintf("file\t%04o\t%s\n", e.Mode.Perm(), manifestPath(e.Path))
	case "symlink":
		return fmt.Sprintf("symlink\t-\t%s\n", manifestPath(e.Path))
	case "dir":
		return fmt.Sprintf("dir\t-\t%s\n", manifestPath(e.Path))
	case "fifo":
		return fmt.Sprintf("fifo\t%04o\t%s\n", e.Mode.Perm(), manifestPath(e.Path))
	default:
		return fmt.Sprintf("absent\t-\t%s\n", manifestPath(e.Path))
	}
}

// manifestPath writes a path so that it can be read back. A name that needs
// nothing — no quote, no backslash, no control character — is written as it is,
// which is what almost every path is; anything else is written the way Go writes a
// string literal, in quotes. The two forms cannot be confused: the quoted one
// always opens with a quote, and a name that opens with a quote is always quoted,
// because a quote is one of the three things that send a name down that branch.
func manifestPath(p string) string {
	if strings.IndexFunc(p, func(r rune) bool {
		return r < 0x20 || r == 0x7f || r == '"' || r == '\\'
	}) < 0 {
		return p
	}
	return strconv.Quote(p)
}

// restoreOrExplain puts the worktree back and says which of the two happened.
// It is the only way a failure after the reset leaves this function, so that
// "the worktree was put back" is a claim the code either made or did not — and
// when it could not be made, the error says so and names the copy, because the
// copy is then the only place the task's work exists.
func restoreOrExplain(worktree, head, keep string, entries []snapshotEntry, cause error, taskID string) error {
	left, rerr := restore(worktree, head, keep, entries)
	if rerr != nil {
		return fmt.Errorf("%w\nThe worktree could NOT be put back: %v\nThe task's work is kept at %s — restore it by hand before dispatching %s again", cause, rerr, keep, taskID)
	}
	if len(left) > 0 {
		return fmt.Errorf("%w\nThe worktree was put back, except that %s. The change is also kept at %s", cause, leftInPlace(left), keep)
	}
	return fmt.Errorf("%w\nThe worktree was put back the way it was found; the change is also kept at %s", cause, keep)
}

// leftInPlace is the sentence that says which paths the restore kept and why it
// could not remove them. It is written for both numbers — a refusal that leaves
// two files and says "it" reads as if one were meant — and it names both ways a
// path ends up with no copy in the snapshot, because the fix that reads the
// clean's reach at the path's own granularity made the second one reachable: a
// path hidden by an ignore rule the task had not committed, and a path written
// after the snapshot was taken inside a directory the clean removes whole.
func leftInPlace(left []string) string {
	if len(left) == 1 {
		return fmt.Sprintf("the restore left %s in place rather than delete it with nothing to write back: the snapshot holds no copy of it — either an ignore rule the task had not committed hid it (the advance's reset drops that rule, and this restore's reset cannot bring it back), or it was written after the snapshot was taken, inside a directory the clean removes whole. Nothing was lost: it is still where the task left it", named(left))
	}
	return fmt.Sprintf("the restore left %s in place rather than delete them with nothing to write back: the snapshot holds no copy of them — either an ignore rule the task had not committed hid them (the advance's reset drops that rule, and this restore's reset cannot bring it back), or they were written after the snapshot was taken, inside a directory the clean removes whole. Nothing was lost: they are still where the task left them", named(left))
}

// named is a list of paths as one readable phrase. A name holding a comma would
// read as two names in it, and the list is the only place these paths are ever
// reported, so the ones that cannot stand for themselves are written the way the
// manifest writes them. A list longer than a reader will take in is cut, and the
// rest is a count rather than silence: the number is what says how much is still
// there.
func named(paths []string) string {
	const shown = 8
	cut := paths
	var more string
	if len(cut) > shown {
		more = fmt.Sprintf(" (and %d more)", len(cut)-shown)
		cut = cut[:shown]
	}
	out := make([]string, len(cut))
	for i, p := range cut {
		if strings.Contains(p, ",") {
			out[i] = strconv.Quote(p)
			continue
		}
		out[i] = manifestPath(p)
	}
	return strings.Join(out, ", ") + more
}

// restore is restoreWorktree, as a variable, so that a test can make it fail
// during a REAL advance. The branch that reports a tree that could not be put
// back is the one an operator depends on most and the one the filesystem alone
// will not produce here — every trick that makes a write fail (a read-only
// directory, an unwritable file) is bypassed by root, and the tests run as
// whatever the CI user is.
var restore = restoreWorktree

// unheldInside names what a directory the clean would remove holds that the
// snapshot has no copy of. It is the difference between "the snapshot holds this
// directory" and "the snapshot holds everything under it", and the clean needs
// the second: `git clean -fd` takes a directory whole, so everything inside goes
// with it whether or not anything recorded it.
//
// The entries beside the directory are the record of what the walk saw: a path
// written after the snapshot was taken is in neither, and so is everything under
// a directory that only became removable when the reset deleted the file that
// was keeping it. Both are work the restore cannot write back.
//
// A path the walk cannot list counts as unheld. What cannot be listed cannot be
// promised, and the two directions are not equally bad: leaving a path costs the
// next dispatch a confusing extra file, and deleting one costs the work.
func unheldInside(worktree, dir string, held map[string]bool) []string {
	var unheld []string
	root := filepath.Join(worktree, filepath.FromSlash(dir))
	_ = filepath.WalkDir(root, func(full string, _ fs.DirEntry, werr error) error {
		rel, rerr := filepath.Rel(worktree, full)
		if rerr != nil {
			unheld = append(unheld, dir)
			return fs.SkipAll
		}
		rel = onePathName(filepath.ToSlash(rel))
		if werr != nil {
			unheld = append(unheld, rel)
			return nil
		}
		if rel == onePathName(dir) {
			return nil // the directory itself: held, which is why it is walked
		}
		if !held[rel] {
			unheld = append(unheld, rel)
		}
		return nil
	})
	return unheld
}

// restoreWorktree puts the worktree back the way the caller found it: the
// branch tip it had, no files the failed advance left behind, and the task's
// own files written back from the snapshot. `reset --hard head` plus the
// snapshot is what makes it exact — head restores everything the task did NOT
// change, the snapshot restores everything it did.
//
// It returns the paths it deliberately did NOT delete (see below); the caller
// reports them, because "the worktree was put back the way it was found" is
// false for a tree that still holds one of them.
func restoreWorktree(worktree, head, keep string, entries []snapshotEntry) ([]string, error) {
	if _, err := gitOutput(worktree, "reset", "--hard", head); err != nil {
		return nil, fmt.Errorf("resetting the worktree to %s: %w", head, err)
	}
	// The restore's own clean asks first, and it has to. `reset --hard head`
	// above has already written head's ignore files, so every rule the task added
	// and had not committed is gone by the time the clean runs and a file those
	// rules hid is now an ordinary untracked file the clean would delete. The
	// snapshot holds no copy of it — `--exclude-standard` is blind to ignored
	// files, which is exactly why the advance's own ask refused over this file —
	// so the restore would delete work it cannot write back and then report the
	// worktree put back. That is the sixth round's first finding, reproduced in
	// TestTheRestoreKeepsAFileTheResetsOwnIgnoreRuleHid.
	//
	// So the clean is limited to the paths the snapshot holds, which the
	// placements below write back. A path in neither category is not deleted: it
	// is named to the caller instead. Leaving a file the task wrote costs a
	// re-dispatch a confusing extra path; deleting one it cannot be given back
	// costs the work, which is the defect.
	held := make(map[string]bool, len(entries))
	for _, e := range entries {
		held[e.Path] = true
	}
	dirs, files, err := cleanReach(worktree)
	if err != nil {
		return nil, err
	}
	var safe, left []string
	for _, p := range append(keysOf(dirs), files...) {
		// The snapshot holding the PATH is the whole of the test here, exactly as
		// it is in the ask above: a path the snapshot holds only through the
		// directory around it is one it has no copy of, and a path in neither
		// category is named to the caller rather than deleted.
		if !held[p] {
			left = append(left, p)
			continue
		}
		if dirs[p] {
			// A directory goes in one piece, so holding the directory is not the
			// claim the clean needs — it needs the contents, which is what the
			// entries beside it are and what the clean removes with it. A path
			// written after the snapshot was taken is held by neither, and it
			// would ride out on the removal, unnamed, with the message saying the
			// worktree was put back. This is the rule above, at the granularity
			// git actually removes.
			if unheld := unheldInside(worktree, p, held); len(unheld) > 0 {
				left = append(left, unheld...)
				continue
			}
		}
		safe = append(safe, p)
	}
	for _, batch := range restoreCleanBatches(safe) {
		if _, err := restoreClean(worktree, restoreCleanArgs(batch)...); err != nil {
			return left, fmt.Errorf("cleaning the worktree: %w", err)
		}
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
				return left, err
			}
			continue
		}
		placements = append(placements, e)
	}
	for _, e := range placements {
		if err := restoreEntry(worktree, keep, e); err != nil {
			return left, err
		}
	}
	return left, nil
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
	case "fifo":
		// Whatever head put at this path has to go first — mkfifo(2) fails with
		// EEXIST — and what goes back is a fifo, not a directory with the same
		// name. syscall.Mkfifo is mkfifo(2); os has no wrapper for it, and the
		// canonical platform here is Linux (CLAUDE.md §7).
		if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("clearing %s before restoring it: %w", e.Path, err)
		}
		if err := syscall.Mkfifo(dst, uint32(e.Mode.Perm())); err != nil {
			return fmt.Errorf("recreating the fifo %s: %w", e.Path, err)
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
// task changed holds what the advance was supposed to put there — the task's
// change, merged with whatever main did to the same path — except the artifacts
// that are regenerated by construction, whose value is a function of the tree
// and therefore correct by definition.
//
// It replaces a comparison of path SETS, which could not see content at all. It
// used to compare against the kept copy, which could not see a MERGE: the
// advance applies the task's change to main, so a path main also changed holds
// both changes, and the task's copy is not what should be there. That version
// refused the ordinary clean merge — forever, and with a message saying the
// advance had not carried the work across, which was the opposite of true
// (T9022 is that shape, and it is the fifth round's first finding).
//
// So the expectation is built independently of the patch: the three-way merge of
// the same three versions the advance had — main's at the target commit, the
// merge base's, and the task's. diff3 rather than `git apply`, which makes it a
// measurement of the result instead of a restatement of the input.
func verifyTheWorkSurvived(worktree, keep, base, target string, entries []snapshotEntry, regenerated []string) error {
	skip := map[string]bool{}
	for _, r := range regenerated {
		skip[r] = true
	}
	// Both trees as git records them. One listing each serves everything below:
	// the exec bit git gives a path in the base, and the object holding each
	// path's content on either side of the advance.
	baseRecs, err := lsTreeRecords(worktree, base)
	if err != nil {
		return err
	}
	targetRecs, err := lsTreeRecords(worktree, target)
	if err != nil {
		return err
	}
	baseExec := execBits(baseRecs)
	baseBlobs := blobIndex(baseRecs)
	targetBlobs := blobIndex(targetRecs)
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
		if e.State != "file" && e.State != "symlink" {
			// A directory's existence is the whole of its state here, an absent
			// path has just been compared by the line above, and a fifo has no
			// content to compare — there is no kept copy of one, because a fifo
			// is not bytes.
			continue
		}
		// The kept copy holds what the restore would write back: the bytes of a
		// file, the target of a symlink. Reading it is reading the snapshot, and
		// it is one of the three versions the expectation is built from.
		want, err := os.ReadFile(filepath.Join(keep, "files", filepath.FromSlash(e.Path)))
		if err != nil {
			return fmt.Errorf("reading the kept copy of %s: %w", e.Path, err)
		}
		expected, why, err := expectedContent(worktree, baseBlobs, targetBlobs, e.Path, want)
		if err != nil {
			return err
		}
		if why != "" {
			lost = append(lost, why)
			continue
		}
		if !bytes.Equal(data, expected) {
			// The words the loss is measured in have to be the true ones: where
			// main left the path alone the task's own bytes are the whole of what
			// belongs there, and where main changed it too, what belongs there is
			// the merge of the two changes.
			against := fmt.Sprintf("the task left %s", shortDigest(want))
			if !bytes.Equal(expected, want) {
				against = fmt.Sprintf("the merge of main's, the base's and the task's copies of it is %s", shortDigest(expected))
			}
			lost = append(lost, fmt.Sprintf("%s came back as %s; %s", e.Path, shortDigest(data), against))
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
	case st.IsDir():
		return "dir", 0, nil, nil
	case st.Mode()&os.ModeNamedPipe != 0:
		// A fifo, in the word the snapshot records it as. Not "dir", which is
		// what the arm below this one used to call everything that was not a
		// file or a symlink: an entry recorded as a fifo and read back as a
		// directory is work reported as lost when it is sitting right there.
		return "fifo", 0, nil, nil
	default:
		// A socket or a device node: a kind the snapshot refuses rather than
		// copies, so a path in this state cannot be one of the entries — and the
		// words are this function's own naming rather than "dir", which it was,
		// for a reason that was only ever one edit away from mattering.
		return fileKindWord(st.Mode()), 0, nil, nil
	}
}

// execBits returns, for every path git has in a tree, whether git records it as
// executable. A path the task created has no entry — it did not exist in the
// base — and the caller judges those by a different rule: git creates a file
// from a patch with the mode the patch names.
func execBits(recs []lsTreeRecord) map[string]bool {
	bits := make(map[string]bool, len(recs))
	for _, r := range recs {
		bits[r.Path] = r.Mode&0o111 != 0
	}
	return bits
}

// blobIndex maps each path in a tree to the object holding its content.
func blobIndex(recs []lsTreeRecord) map[string]string {
	out := make(map[string]string, len(recs))
	for _, r := range recs {
		out[r.Path] = r.Object
	}
	return out
}

// expectedContent is what the advanced tree should hold at p, and where that
// comes from. The task's own copy is one of the three inputs, never the answer:
// the advance applies the task's change TO MAIN, so a path main also changed
// holds both changes together, and the task's copy on its own is not what should
// be there. Comparing against it refused the ordinary clean merge forever, with
// a message saying the work had come back different.
//
// The returned reason is empty when the path is fine, and names the loss when it
// is not.
func expectedContent(worktree string, baseBlobs, targetBlobs map[string]string, p string, theirs []byte) ([]byte, string, error) {
	targetSHA, inTarget := targetBlobs[p]
	if !inTarget {
		// Main has nothing at this path. The task's version is the whole of it:
		// either the task created the path, or main deleted it and the patch
		// would not have applied.
		return theirs, "", nil
	}
	if baseSHA, inBase := baseBlobs[p]; inBase && baseSHA == targetSHA {
		// The same object on both sides: main did not touch this path, so the
		// task's bytes are the whole of the change — and no blob has to be read
		// to know it. This is also the ordinary case, so it is worth the branch.
		return theirs, "", nil
	}
	ours, err := gitBytes(worktree, "cat-file", "blob", targetSHA)
	if err != nil {
		return nil, "", fmt.Errorf("reading what %s holds at the target commit: %w", p, err)
	}
	if bytes.Equal(ours, theirs) {
		return ours, "", nil // both sides hold the same bytes; the merge is those
	}
	var base []byte
	if sha, ok := baseBlobs[p]; ok {
		if base, err = gitBytes(worktree, "cat-file", "blob", sha); err != nil {
			return nil, "", fmt.Errorf("reading %s as the merge base has it: %w", p, err)
		}
	}
	// No second look at base is needed before merging: the branch above returned
	// early when the target's object IS the base's, and two different objects
	// hold different bytes — a comparison here would be a line nothing can
	// reach, which is how a check starts reading as coverage it does not have.
	merged, conflicting, err := mergeTheSameThreeWays(ours, base, theirs)
	if err != nil {
		return nil, "", err
	}
	if conflicting {
		return nil, fmt.Sprintf("%s: main and the task changed the same lines of it, and the advance spliced the task's change in where a three-way merge of the same three versions reports a conflict", p), nil
	}
	return merged, "", nil
}

// mergeTheSameThreeWays runs git's own three-way merge (diff3, the algorithm a
// merge without conflicts uses) on the same three versions, in a scratch
// directory that is not the worktree: the worktree is being measured, and a
// measurement that writes into it is not one.
func mergeTheSameThreeWays(ours, base, theirs []byte) ([]byte, bool, error) {
	dir, err := os.MkdirTemp("", "rddev-merge")
	if err != nil {
		return nil, false, fmt.Errorf("making a scratch directory for the three-way merge: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	paths := make([]string, 0, 3)
	for i, data := range [][]byte{ours, base, theirs} {
		p := filepath.Join(dir, []string{"ours", "base", "theirs"}[i])
		if err := os.WriteFile(p, data, 0o600); err != nil {
			return nil, false, fmt.Errorf("writing the %s copy for the three-way merge: %w", []string{"ours", "base", "theirs"}[i], err)
		}
		paths = append(paths, p)
	}
	out, err := exec.Command("git", "merge-file", "-p", paths[0], paths[1], paths[2]).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if ee.ExitCode() == 1 {
				// Exit 1 means the two changes conflict; the output is the merged
				// text with conflict markers in it, which is a result the advance
				// must never have produced.
				return nil, true, nil
			}
			return nil, false, fmt.Errorf("git merge-file: %w: %s", ee, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, false, fmt.Errorf("git merge-file: %w", err)
	}
	return out, false, nil
}

// gitBytes is gitOutput for content rather than for a message: gitOutput trims
// whitespace, and a file's bytes are not a message — the trailing newline is
// part of what has to match.
func gitBytes(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), ee, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
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
