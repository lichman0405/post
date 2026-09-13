package devorchestrator

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// RebaselineTask moves a task branch onto a newer main by rebuilding its
// worktree in place. The task's deliverable lives ONLY in that worktree —
// Workers produce uncommitted diffs — so the moment between `clean -fdq` and
// the `git apply` is the moment the work has no other copy anywhere.
//
// It is not a hypothetical: T0301's Worker had extended the G3 gate script,
// main had rewritten the same region while the task was in review, and
// `git apply --check` of its patch against the fixed script fails. The first
// version of this function reset and cleaned the worktree, failed to apply,
// returned, and deleted the temp file holding the only copy on the way out.
// A refusal that a Supervisor is *supposed* to hit — this one, on this task —
// was a silent deletion of the task's work.
//
// These tests pin the outcome for the shape that reaches it: the advance is
// refused, the refusal says where the work is, and the tree is exactly as it
// was found.

// rebaselineFixture is the shape RebaselineTask runs against in production: an
// integration repo on `main`, a linked worktree on the task branch holding the
// task's UNCOMMITTED change, and the registry entry that names them.
type rebaselineFixture struct {
	t        *testing.T
	root     string
	worktree string
	taskID   string
	branch   string
	baseline string
}

// The gate script both the task and main edit — the T0301 shape, in miniature.
// The task's hunk sits in the middle, so main rewriting the surrounding lines is
// enough to make the patch stop applying.
const gateScriptBaseline = `#!/usr/bin/env bash
# the G3 gate
set -euo pipefail
probe_version() { echo "checking api version"; }
probe_push() { echo "a direct push to protected main is refused"; }
probe_hook() { echo "the webhook fired"; }
probe_tree() { echo "the tree under test is as this script found it"; }
report() { echo "report"; }
main() { probe_version; probe_push; probe_hook; probe_tree; report; }
main "$@"
`

func newRebaselineFixture(t *testing.T, taskID string) *rebaselineFixture {
	t.Helper()
	f := &rebaselineFixture{t: t, root: t.TempDir(), taskID: taskID, branch: "task/" + taskID + "-x"}

	// The rule file RebaselineTask loads BEFORE it touches anything. This task
	// changes no derived artifact, so no regenerator runs — the file has to
	// exist and parse regardless, because the decision to refuse an
	// unregenerable artifact is made up front, not after the tree is rebuilt.
	if err := os.MkdirAll(filepath.Join(f.root, "specs", "orchestrator"), 0o755); err != nil {
		t.Fatal(err)
	}
	rules := `{"version":1,"rules":[{"marker":"specs/**","derived":"specs/SPEC_VERSION.json"}]}`
	if err := os.WriteFile(filepath.Join(f.root, DefaultDerivedArtifactsPath), []byte(rules), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.root, "tests", "acceptance"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "tests/acceptance/gate.sh"), []byte(gateScriptBaseline), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.root, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "internal/old.txt"), []byte("the task deletes this\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A file whose mode is the only thing a task can change about it. Git
	// records exactly one permission bit, so this is the smallest change there
	// is — and the one a comparison of path names could not tell apart from the
	// work being lost when main makes the same change.
	if err := os.MkdirAll(filepath.Join(f.root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "scripts", "helper.sh"), []byte("#!/bin/sh\necho helper\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(f.root, "scripts", "helper.sh"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A TRACKED symlink in the baseline, which the task is about to replace with
	// a regular file. That is the shape that makes a restore dangerous: `reset
	// --hard` puts the link back, and writing the task's bytes to that path
	// follows it into whatever it points at — a different file, which the
	// restore would then report as intact.
	if err := os.Symlink("docs/task-notes.md", filepath.Join(f.root, "tracked-link")); err != nil {
		t.Fatal(err)
	}
	// A tracked DIRECTORY holding a file, for the task that puts a file where
	// the directory was: the one shape where the restore has to empty a
	// directory head put back before it can write what stands there now.
	if err := os.MkdirAll(filepath.Join(f.root, "internal", "legacy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "internal", "legacy", "old.txt"), []byte("the task deletes this too\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A second tracked symlink, for the task that replaces it with a DIRECTORY —
	// the mirror of tracked-link, and the shape where MkdirAll follows the link
	// instead of replacing it.
	if err := os.Symlink("internal", filepath.Join(f.root, "tracked-dir-link")); err != nil {
		t.Fatal(err)
	}

	f.git(f.root, "init", "-q", "-b", "main")
	f.git(f.root, "add", "-A")
	f.git(f.root, "commit", "-q", "-m", "baseline")
	f.baseline = f.git(f.root, "rev-parse", "HEAD")

	// A real worktree of the same repo (not a clone): the advance resets it to
	// a commit main just made, which only exists in the shared object store.
	f.worktree = filepath.Join(f.root, ".rddev", "worktrees", taskID)
	f.git(f.root, "worktree", "add", "-q", "-b", f.branch, f.worktree, "main")

	// The task's change, exactly as a Worker leaves it: uncommitted, part
	// tracked modification, part new untracked file, part deletion, part
	// symlink. Untracked files are the half `git diff` does not show and
	// `clean -fdq` deletes outright.
	script := strings.Replace(gateScriptBaseline,
		`probe_tree() { echo "the tree under test is as this script found it"; }`,
		`probe_tree() { echo "the tree under test is as this script found it"; }`+"\n"+
			`probe_hmac() { echo "the webhook signature verifies"; }`, 1)
	f.write("tests/acceptance/gate.sh", script, 0o755)
	f.write("docs/task-notes.md", "what the task did and why\n", 0o644)
	// The two byte-level shapes a synthetic new-file patch gets wrong: a file
	// with no final newline, and a file that is nothing but a newline. Neither
	// shows up in a path list, which is why they survived a test that compared
	// path sets.
	f.write("docs/no-final-newline.txt", "no trailing newline here", 0o644)
	f.write("docs/one-newline.txt", "\n", 0o644)
	// A file with no bytes at all. The synthetic new-file patch has a branch of
	// its own for it — the header with no body — and a branch nothing exercises
	// is a branch that quietly stops working: the code before it emitted a hunk
	// with one empty line, which git applies happily and which turns this file
	// into a file containing a newline.
	f.write("docs/empty.txt", "", 0o644)
	// A path git C-quotes by default. The quoted string is not a path on disk.
	f.write("docs/设计.md", "非 ASCII 路径\n", 0o644)
	if err := os.Symlink("docs/task-notes.md", filepath.Join(f.worktree, "notes-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.worktree, "internal/old.txt")); err != nil {
		t.Fatal(err)
	}
	// The task replaces the tracked symlink with a regular file.
	if err := os.Remove(filepath.Join(f.worktree, "tracked-link")); err != nil {
		t.Fatal(err)
	}
	f.write("tracked-link", "the task turned the link into a file\n", 0o644)

	exit := 0
	if err := SaveRegistry(f.root, &WorkerRecord{
		TaskID: taskID, RunID: "run-1", SessionID: "s", ClaudeVersion: "v",
		PID: 1, StartTime: 1, Worktree: f.worktree, Branch: f.branch,
		BaselineSHA: f.baseline, RefsBefore: []string{},
		LogPath: filepath.Join(f.root, "w.log"), ResultDir: f.root, StartedAt: "t",
		ExitStatus: &exit,
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *rebaselineFixture) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *rebaselineFixture) write(rel, content string, mode os.FileMode) {
	f.t.Helper()
	p := filepath.Join(f.worktree, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		f.t.Fatal(err)
	}
}

// mainRewritesTheSameRegion is T0301 in miniature: main moved under the task
// while it was in review, touching the very lines the task touched, so the
// task's patch no longer applies.
func (f *rebaselineFixture) mainRewritesTheSameRegion() string {
	f.t.Helper()
	rewritten := strings.ReplaceAll(gateScriptBaseline, `"`, `"v2: `)
	if err := os.WriteFile(filepath.Join(f.root, "tests/acceptance/gate.sh"), []byte(rewritten), 0o755); err != nil {
		f.t.Fatal(err)
	}
	f.git(f.root, "add", "-A")
	f.git(f.root, "commit", "-q", "-m", "main rewrote the gate script")
	return f.git(f.root, "rev-parse", "HEAD")
}

// mainMovesElsewhere advances main without touching anything the task changed.
func (f *rebaselineFixture) mainMovesElsewhere() string {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Join(f.root, "docs"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "docs/main-moved.md"), []byte("unrelated\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.git(f.root, "add", "-A")
	f.git(f.root, "commit", "-q", "-m", "main moved on")
	return f.git(f.root, "rev-parse", "HEAD")
}

// pathsAndContents describes every path that differs from base: enough to tell
// two trees apart, and enough to tell that a tree is the same one — the HEAD
// sha, each path's state, mode and content hash.
func (f *rebaselineFixture) pathsAndContents(base string) string {
	f.t.Helper()
	paths, err := worktreeChangedPaths(f.worktree, base)
	if err != nil {
		f.t.Fatal(err)
	}
	var b strings.Builder
	for _, p := range keysOf(paths) {
		if p == "" {
			continue
		}
		b.WriteString(f.describe(p))
	}
	return b.String()
}

func (f *rebaselineFixture) describe(p string) string {
	f.t.Helper()
	abs := filepath.Join(f.worktree, filepath.FromSlash(p))
	st, err := os.Lstat(abs)
	// ENOTDIR says the path cannot exist for a different reason than ENOENT —
	// one of its parents is a file — and it is the shape a path list produces
	// on its own when a task replaces a directory with a file.
	if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
		return "absent\t" + p + "\n"
	}
	if err != nil {
		f.t.Fatal(err)
	}
	switch {
	case st.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(abs)
		if err != nil {
			f.t.Fatal(err)
		}
		return fmt.Sprintf("symlink\t%s\t%s\n", p, target)
	case st.Mode().IsRegular():
		data, err := os.ReadFile(abs)
		if err != nil {
			f.t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		// The executable bit, not the whole mode: `git apply` creates a file it
		// has not seen before with 0666 & ~umask, and that group-write bit is the
		// environment's, not the task's. Git itself records exactly one
		// permission bit, so nothing downstream — the commit, the review diff —
		// can see the difference. The exec bit is the one that matters: gate
		// scripts are run, not read.
		return fmt.Sprintf("file\t%04o\t%s\t%s\n", st.Mode().Perm()&0o111, p, hex.EncodeToString(sum[:]))
	default:
		return fmt.Sprintf("other\t%v\t%s\n", st.Mode(), p)
	}
}

// keptDirs lists the durable copies the refusal left behind.
func (f *rebaselineFixture) keptDirs() []string {
	f.t.Helper()
	base := filepath.Join(f.root, ".rddev", "runtime", "rebaseline")
	entries, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		f.t.Fatal(err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, filepath.Join(base, e.Name()))
	}
	return out
}

func TestRebaselineRefusalDoesNotTakeTheTasksWorkWithIt(t *testing.T) {
	f := newRebaselineFixture(t, "T9001")
	f.mainRewritesTheSameRegion()

	before := f.pathsAndContents(f.baseline)
	beforeHead := f.git(f.worktree, "rev-parse", "HEAD")
	gateBefore := readFileOrFail(t, filepath.Join(f.worktree, "tests/acceptance/gate.sh"))
	modesBefore := map[string]os.FileMode{}
	for _, path := range []string{"tests/acceptance/gate.sh", "docs/task-notes.md", "notes-link"} {
		st, err := os.Lstat(filepath.Join(f.worktree, path))
		if err != nil {
			t.Fatal(err)
		}
		modesBefore[path] = st.Mode()
	}

	// The restore has to reproduce the tree, not re-create it under whatever
	// umask rddev happens to run with. A Worker's files carry the Worker's
	// modes; writing them back with a bare create would silently drop every bit
	// this process's umask clears. 0o077 clears all of them, so the modes below
	// are only preserved if the restore sets them deliberately.
	prevUmask := syscall.Umask(0o077)
	defer syscall.Umask(prevUmask)

	res, err := RebaselineTask(f.root, "T9001", "", "")
	if err == nil {
		t.Fatalf("the advance was not refused, and it must be: the task's patch does not apply to the rewritten gate script (result %+v)", res)
	}
	msg := err.Error()

	// The refusal has to be the APPLY that failed, and the assertion has to be
	// git's own wording: the wrapper around it says "does not apply" whatever
	// happened, so matching that would pass even when `git apply` never ran —
	// which it did once, against a patch file this code had failed to hand it.
	// "patch does not apply" is git's line, and it is only reachable after the
	// worktree has already been reset and cleaned.
	if !strings.Contains(msg, "patch does not apply") {
		t.Fatalf("expected git to refuse the patch, got: %s", msg)
	}

	// 1. The tree is exactly as it was found. Not "mostly": the task's untracked
	//    file, its symlink and its deletion are all part of the deliverable.
	after := f.pathsAndContents(f.baseline)
	if before != after {
		t.Errorf("the refused advance changed the tree it refused to advance:\n--- as found ---\n%s\n--- after ---\n%s", before, after)
	}
	if head := f.git(f.worktree, "rev-parse", "HEAD"); head != beforeHead {
		t.Errorf("the worktree HEAD moved to %s despite the refusal; it was %s", head, beforeHead)
	}
	gateAfter := readFileOrFail(t, filepath.Join(f.worktree, "tests/acceptance/gate.sh"))
	if gateBefore != gateAfter {
		t.Errorf("the task's edit of the gate script was lost:\n%s", gateAfter)
	}
	// The paths the task changed, read one by one rather than through the
	// fingerprint above. The fingerprint is computed with worktreeChangedPaths,
	// which is the function under test — two nets that share a blind spot agree
	// with each other, and the tree above can be identical on both sides while a
	// file is missing from both. These read the bytes.
	for path, want := range map[string]string{
		"docs/task-notes.md":        "what the task did and why\n",
		"docs/no-final-newline.txt": "no trailing newline here",
		"docs/one-newline.txt":      "\n",
		"docs/empty.txt":            "",
		"docs/设计.md":                "非 ASCII 路径\n",
	} {
		if got := readFileOrFail(t, filepath.Join(f.worktree, path)); got != want {
			t.Errorf("%s came back as %q, want %q", path, got, want)
		}
	}
	// The path the tracked symlink pointed at. A restore that writes "through"
	// the link instead of clearing it destroys THIS file, and reports the path
	// it was asked to restore as restored.
	if got := readFileOrFail(t, filepath.Join(f.worktree, "docs/task-notes.md")); got != "what the task did and why\n" {
		t.Errorf("the file the tracked symlink points at was overwritten by the restore: %q", got)
	}
	// And the task's replacement of the link is restored as a regular file, not
	// as a link.
	st, err := os.Lstat(filepath.Join(f.worktree, "tracked-link"))
	if err != nil {
		t.Fatal(err)
	}
	if !st.Mode().IsRegular() {
		t.Errorf("tracked-link came back as %v; the task had made it a regular file", st.Mode())
	}
	if got := readFileOrFail(t, filepath.Join(f.worktree, "tracked-link")); got != "the task turned the link into a file\n" {
		t.Errorf("tracked-link came back as %q", got)
	}
	// Restoring is by content, so the modes come back as they were found rather
	// than as a fresh `git apply` would have made them — which for an untracked
	// file means the umask, not the mode the Worker chose.
	for _, path := range []string{"tests/acceptance/gate.sh", "docs/task-notes.md", "notes-link"} {
		st, err := os.Lstat(filepath.Join(f.worktree, path))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := st.Mode(), modesBefore[path]; got != want {
			t.Errorf("%s came back as %v, not %v — the restore is supposed to reproduce the tree, not rebuild it", path, got, want)
		}
	}

	// 2. A second copy exists at a durable path, it holds the WHOLE change, and
	//    the refusal names it — a Supervisor reading the error has to be able to
	//    find the work.
	kept := f.keptDirs()
	if len(kept) != 1 {
		t.Fatalf("expected exactly one kept copy of the task's work, found %d: %v", len(kept), kept)
	}
	dir := kept[0]
	if !strings.HasPrefix(filepath.Base(dir), "T9001-") {
		t.Errorf("the kept directory %s does not name the task", dir)
	}
	if !strings.Contains(msg, dir) {
		t.Errorf("the refusal does not say where the work is kept (expected %s in): %s", dir, msg)
	}
	patch := readFileOrFail(t, filepath.Join(dir, "change.patch"))
	for _, want := range []string{"probe_hmac", "docs/task-notes.md", "internal/old.txt"} {
		if !strings.Contains(patch, want) {
			t.Errorf("the kept patch does not carry %q:\n%s", want, patch)
		}
	}
	// The copy that restores is the bytes, not the diff: a patch that no longer
	// applies is what got us here.
	if got := readFileOrFail(t, filepath.Join(dir, "files", "tests/acceptance/gate.sh")); got != gateBefore {
		t.Errorf("the kept copy of the gate script is not the task's version:\n%s", got)
	}
	manifest := readFileOrFail(t, filepath.Join(dir, "MANIFEST.txt"))
	for _, want := range []string{"tests/acceptance/gate.sh", "docs/task-notes.md", "notes-link", "internal/old.txt"} {
		if !strings.Contains(manifest, want) {
			t.Errorf("the restore manifest does not list %q:\n%s", want, manifest)
		}
	}
}

// The happy path is the one that runs hundreds of times: it must still advance
// the branch and keep the work, and it must not leave copies of every task's
// diff lying under .rddev/runtime.
func TestRebaselineAdvanceKeepsTheWorkAndLeavesNoCopy(t *testing.T) {
	f := newRebaselineFixture(t, "T9002")
	newMain := f.mainMovesElsewhere()

	before := f.pathsAndContents(f.baseline)

	res, err := RebaselineTask(f.root, "T9002", "", "")
	if err != nil {
		t.Fatalf("advancing onto a main that touched none of the task's files was refused: %v", err)
	}
	if res.ToSHA != newMain {
		t.Errorf("the advance landed on %s, want %s", res.ToSHA, newMain)
	}
	if head := f.git(f.worktree, "rev-parse", "HEAD"); head != newMain {
		t.Errorf("the worktree is at %s; the baseline is %s", head, newMain)
	}
	// The task's paths, modes and bytes survive the rebuild — measured against
	// the NEW baseline, so this says the change is still a change from main.
	if after := f.pathsAndContents(newMain); after != before {
		t.Errorf("the advance altered the task's work:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
	if kept := f.keptDirs(); len(kept) != 0 {
		t.Errorf("a completed advance left its copies behind: %v", kept)
	}
	// The exact bytes of the shapes the synthetic new-file patch used to get
	// wrong: the path-set comparison above cannot see any of them, because a
	// byte added or dropped does not change which paths are listed.
	if got := readFileOrFail(t, filepath.Join(f.worktree, "docs/no-final-newline.txt")); got != "no trailing newline here" {
		t.Errorf("the file without a final newline came back as %q", got)
	}
	if got := readFileOrFail(t, filepath.Join(f.worktree, "docs/one-newline.txt")); got != "\n" {
		t.Errorf("the file that is a single newline came back as %q", got)
	}
	if got := readFileOrFail(t, filepath.Join(f.worktree, "docs/empty.txt")); got != "" {
		t.Errorf("the empty file came back as %q — a header-only patch creates it empty, while a body of one empty line makes it a file containing a newline", got)
	}
	if got := readFileOrFail(t, filepath.Join(f.worktree, "docs/设计.md")); got != "非 ASCII 路径\n" {
		t.Errorf("the file at a path git would C-quote came back as %q", got)
	}
	// And the refusal path is still armed for the next time: the ledger knows
	// the branch moved (ref_ledger.go), so a sibling collect attributes it to
	// the Supervisor rather than rejecting a branch whose tip it cannot explain.
	refs, err := ListSupervisorRefs(f.root)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range refs {
		if r.Name == "refs/heads/"+f.branch && r.SHA == newMain {
			found = true
		}
	}
	if !found {
		t.Errorf("the advanced branch is not in the Supervisor ref ledger: %+v", refs)
	}
}

// A task with nothing to carry is refused before anything is copied or touched.
func TestRebaselineRefusesAnEmptyChangeWithoutSideEffects(t *testing.T) {
	f := newRebaselineFixture(t, "T9003")
	// Undo the task's change entirely, so the worktree is clean at its baseline.
	f.git(f.worktree, "checkout", "--", "tests/acceptance/gate.sh", "internal/old.txt", "tracked-link")
	if err := os.RemoveAll(filepath.Join(f.worktree, "docs")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.worktree, "notes-link")); err != nil {
		t.Fatal(err)
	}
	f.mainMovesElsewhere()

	if _, err := RebaselineTask(f.root, "T9003", "", ""); err == nil {
		t.Fatal("a task with no change was advanced")
	} else if !strings.Contains(err.Error(), "no change to carry across") {
		t.Fatalf("expected the empty-change refusal, got: %v", err)
	}
	if kept := f.keptDirs(); len(kept) != 0 {
		t.Errorf("an empty change left a copy behind: %v", kept)
	}
	if head := f.git(f.worktree, "rev-parse", "HEAD"); head != f.baseline {
		t.Errorf("the worktree moved to %s on an empty change", head)
	}
}

func readFileOrFail(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

// taskEditsADerivedArtifact puts the derived artifact itself into the task's
// change, which is what puts it on the regeneration path rather than the
// textual one.
func (f *rebaselineFixture) taskEditsADerivedArtifact(content string) {
	f.t.Helper()
	f.write("specs/SPEC_VERSION.json", content, 0o644)
}

// mainMovesTheDerivedArtifact makes main change the same generated file the
// task changed. This is the ordinary case, not a contrived one: every spec edit
// regenerates the marker, so main moves it constantly while tasks are open.
func (f *rebaselineFixture) mainMovesTheDerivedArtifact() string {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, "specs/SPEC_VERSION.json"), []byte("{\"marker\":\"main's own regeneration\"}\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.git(f.root, "add", "-A")
	f.git(f.root, "commit", "-q", "-m", "main regenerated the spec version marker")
	return f.git(f.root, "rev-parse", "HEAD")
}

// fakeTheRegenerator stands in for the real scripts/spec_version.py, which does
// not exist in a fixture. The command writes a fixed string, so "was this
// merged as text or regenerated" is answerable from the content.
func fakeTheRegenerator(t *testing.T, content string) {
	t.Helper()
	real := regenerators["specs/SPEC_VERSION.json"]
	regenerators["specs/SPEC_VERSION.json"] = struct {
		Write []string
		Check []string
	}{
		Write: []string{"sh", "-c", `printf '%s' "$0" > specs/SPEC_VERSION.json`, content},
		Check: []string{"sh", "-c", `test "$(cat specs/SPEC_VERSION.json)" = "$(printf '%s' "$0")"`, content},
	}
	t.Cleanup(func() { regenerators["specs/SPEC_VERSION.json"] = real })
}

// A failure AFTER the worktree has been rebuilt is the case the first version
// of this function returned from without putting anything back: `reset --hard`
// had already moved the task branch, so the tree was left at the new baseline
// with the task's work gone, the ref ledger unchanged and the gate inputs
// describing a baseline that no longer existed. The next collect then reported
// the Worker moving HEAD, which was the Supervisor's own reset.
func TestRebaselinePostApplyFailurePutsTheWorkBackAndNamesIt(t *testing.T) {
	f := newRebaselineFixture(t, "T9004")
	f.taskEditsADerivedArtifact("{\"marker\":\"the task's spec edit\"}\n")
	f.mainMovesElsewhere()

	before := f.pathsAndContents(f.baseline)
	beforeHead := f.git(f.worktree, "rev-parse", "HEAD")

	res, err := RebaselineTask(f.root, "T9004", "", "")
	if err == nil {
		t.Fatalf("the advance was not refused, and it must be: there is no scripts/spec_version.py in the fixture to regenerate the marker with (result %+v)", res)
	}
	// 1. The tree is exactly as it was found, and so is the branch tip. The ref
	//    matters as much as the bytes: gate inputs were written at spawn against
	//    this tip, and a collect compares them.
	//
	//    This comes FIRST, before anything is asserted about the error text. The
	//    reversion battery names this test as the one that has to die when the
	//    added-artifact branch is reverted, and it can only mean that if the
	//    assertions about the tree are the ones that fail: a test whose first
	//    assertion is a string match dies at the string when the failure MODE
	//    changes, and says nothing about whether the work was put back.
	if after := f.pathsAndContents(f.baseline); after != before {
		t.Errorf("a failed advance after the rebuild changed the tree:\n--- as found ---\n%s\n--- after ---\n%s", before, after)
	}
	if head := f.git(f.worktree, "rev-parse", "HEAD"); head != beforeHead {
		t.Errorf("the worktree HEAD is at %s after the failure; it was %s — the advance moved it and nothing moved it back", head, beforeHead)
	}

	msg := err.Error()
	if !strings.Contains(msg, "spec_version.py") {
		t.Fatalf("expected the regeneration failure, got: %s", msg)
	}
	// The failure has to be reported as a failure, not as a restore: the two
	// sentences are different claims and only one of them is true here.
	if !strings.Contains(msg, "put back") {
		t.Errorf("the refusal does not say what happened to the worktree: %s", msg)
	}

	// 2. The copy is kept, and the failure names it. This is the only pointer a
	//    Supervisor has to the work; a kept directory with no error naming it is
	//    a copy nobody finds.
	kept := f.keptDirs()
	if len(kept) != 1 {
		t.Fatalf("expected exactly one kept copy, found %d: %v", len(kept), kept)
	}
	if !strings.Contains(msg, kept[0]) {
		t.Errorf("the refusal does not say where the work is kept (expected %s in): %s", kept[0], msg)
	}
	if got := readFileOrFail(t, filepath.Join(kept[0], "files", "specs", "SPEC_VERSION.json")); got != "{\"marker\":\"the task's spec edit\"}\n" {
		t.Errorf("the kept copy is not the task's version of the marker: %q", got)
	}
}

// The second advance of the same task, with main untouched and the registry's
// BaselineSHA still the original one — which is the state after an advance whose
// rework has not run yet. `before` used to be taken from that recorded baseline
// while the patch was taken from the merge base, so main's own files were
// credited to the task and the advance refused with "lost 2 path(s) the task had
// changed: [.gitignore docs/main-moved.md]".
func TestRebaselineSecondAdvanceIsNotRefusedAsLostPaths(t *testing.T) {
	f := newRebaselineFixture(t, "T9005")
	newMain := f.mainMovesElsewhere()

	// The task's change, counted from the tree: this is what an advance carries.
	// The count is asserted because the set it comes from says which commit the
	// change is measured against, and that is what the check below compares.
	changed, err := worktreeChangedPaths(f.worktree, f.baseline)
	if err != nil {
		t.Fatal(err)
	}
	// Plus the directories the clean removes WHOLE, which are entries of their
	// own now (a restore has to be able to put one back, and an empty one cannot
	// be rebuilt from the patch at all). The fixture's `docs/` is one: nothing in
	// it is tracked, so the clean deletes the directory along with everything the
	// task put inside it. The claim the count is about is untouched — the set is
	// measured against the commit the patch is measured from, and main's own files
	// are not in it — so the sum is pinned to a shape rather than to a number.
	whole, _, err := cleanReach(f.worktree)
	if err != nil {
		t.Fatal(err)
	}
	if len(whole) != 1 || !whole["docs"] {
		t.Fatalf("the clean removes whole directories %v, want just docs — this test's arithmetic is about that shape", keysOf(whole))
	}
	want := len(changed) + len(whole)

	first, err := RebaselineTask(f.root, "T9005", "", "")
	if err != nil {
		t.Fatalf("the first advance was refused: %v", err)
	}
	if first.Files != want {
		t.Errorf("the first advance carried %d path(s), want %d — the task's own change and nothing else", first.Files, want)
	}
	// Nothing on main has moved since, and the registry still records the
	// original baseline: the task's contribution is unchanged.
	before := f.pathsAndContents(newMain)

	res, err := RebaselineTask(f.root, "T9005", "", "")
	if err != nil {
		t.Fatalf("the second advance was refused, and main has not moved since the first: %v", err)
	}
	if res.ToSHA != newMain {
		t.Errorf("the second advance landed on %s, want %s", res.ToSHA, newMain)
	}
	// The same work is the same size — measured, as before, against the commit the
	// patch is measured from, so main's own files are not part of it. It is one
	// path smaller for a reason that is not main's files: after the first advance
	// main's OWN `docs/main-moved.md` is in the tree, so `docs/` no longer stands
	// as a directory the clean removes whole, and the entry that stood for it goes
	// with it. What the count is measured from has not moved; what moved is how
	// much of the tree the clean would delete.
	if now, _, err := cleanReach(f.worktree); err != nil {
		t.Fatal(err)
	} else if len(now) != 0 {
		t.Fatalf("after the first advance the clean still removes whole directories %v — this test's arithmetic is about main's file having made docs/ unremovable", keysOf(now))
	}
	if res.Files != want-len(whole) {
		t.Errorf("the second advance carried %d path(s), want %d — the same work, and main's own files are not part of it", res.Files, want-len(whole))
	}
	if after := f.pathsAndContents(newMain); after != before {
		t.Errorf("the second advance altered the task's work:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
	if kept := f.keptDirs(); len(kept) != 0 {
		t.Errorf("a completed advance left copies behind: %v", kept)
	}
}

// Two refusals inside one second must not share a directory. The name was a
// second-granular timestamp and the directory was made with MkdirAll, which
// succeeds on an existing one: one attempt's change.patch overwrote the other's,
// and both errors pointed at the same half-full copy.
func TestRebaselineTwoRefusalsInTheSameSecondKeepSeparateCopies(t *testing.T) {
	f := newRebaselineFixture(t, "T9006")
	f.mainRewritesTheSameRegion()

	real := rebaselineNow
	rebaselineNow = func() time.Time { return time.Date(2026, 9, 13, 5, 5, 56, 0, time.UTC) }
	defer func() { rebaselineNow = real }()

	for i := 0; i < 2; i++ {
		if _, err := RebaselineTask(f.root, "T9006", "", ""); err == nil {
			t.Fatalf("attempt %d was not refused", i+1)
		}
	}
	kept := f.keptDirs()
	if len(kept) != 2 {
		t.Fatalf("two refused attempts left %d kept copies, want 2: %v", len(kept), kept)
	}
	if kept[0] == kept[1] {
		t.Fatalf("both attempts were given the same directory %s", kept[0])
	}
	for _, dir := range kept {
		patch := readFileOrFail(t, filepath.Join(dir, "change.patch"))
		if !strings.Contains(patch, "probe_hmac") {
			t.Errorf("the copy at %s does not hold a complete change (a merged directory keeps whichever attempt wrote last):\n%s", dir, patch)
		}
	}
}

// When the restore ITSELF fails, the error has to say so. The alternative is an
// error claiming a recovery that did not happen, which is worse than either
// outcome on its own: a Supervisor reads "the worktree was put back" and
// re-dispatches onto a tree that is not the one the task left.
func TestRebaselineSaysSoWhenTheRestoreItselfFails(t *testing.T) {
	f := newRebaselineFixture(t, "T9007")
	entries := []snapshotEntry{{Path: "docs/task-notes.md", State: "file", Mode: 0o644}}

	// The same call, with the kept copy readable: it reports the restore.
	good := t.TempDir()
	keepPath := filepath.Join(good, "files", "docs", "task-notes.md")
	if err := os.MkdirAll(filepath.Dir(keepPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keepPath, []byte("what the task did and why\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := restoreOrExplain(f.worktree, f.baseline, good, entries, errTheAdvanceWasRefused, "T9007"); err != nil {
		if !strings.Contains(err.Error(), "was put back") {
			t.Errorf("a restore that succeeded is not reported as one: %v", err)
		}
	}

	// Now a kept copy that cannot be read back — a directory where a file
	// belongs, which is root-proof unlike a permission bit.
	bad := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bad, "files", "docs", "task-notes.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := restoreOrExplain(f.worktree, f.baseline, bad, entries, errTheAdvanceWasRefused, "T9007")
	if err == nil {
		t.Fatal("a restore that could not read the kept copy reported success")
	}
	msg := err.Error()
	if !strings.Contains(msg, "could NOT be put back") {
		t.Errorf("the error does not say the worktree was left unrestored: %s", msg)
	}
	if !strings.Contains(msg, bad) {
		t.Errorf("the error does not name where the work is kept (expected %s in): %s", bad, msg)
	}
	if !strings.Contains(msg, "T9007") {
		t.Errorf("the error does not name the task whose work is at stake: %s", msg)
	}
}

var errTheAdvanceWasRefused = errors.New("the advance was refused")

// The restore failing during a real advance — not in a unit call to the helper,
// but on the path a Supervisor actually reaches. The refusal has to say the tree
// is NOT as it was found, because the alternative is an error that claims a
// recovery which did not happen and a Supervisor who re-dispatches onto a tree
// the task never touched.
func TestRebaselineReportsARestoreThatFailedDuringARealAdvance(t *testing.T) {
	f := newRebaselineFixture(t, "T9009")
	f.mainRewritesTheSameRegion() // the task's patch no longer applies

	real := restore
	restore = func(string, string, string, []snapshotEntry) ([]string, error) {
		return nil, errors.New("the restore could not run at all")
	}
	defer func() { restore = real }()

	_, err := RebaselineTask(f.root, "T9009", "", "")
	if err == nil {
		t.Fatal("the advance was not refused")
	}
	msg := err.Error()
	if !strings.Contains(msg, "could NOT be put back") {
		t.Errorf("a failed restore was not reported as one: %s", msg)
	}
	if !strings.Contains(msg, "the restore could not run at all") {
		t.Errorf("the error does not carry why the restore failed: %s", msg)
	}
	if kept := f.keptDirs(); len(kept) != 1 || !strings.Contains(msg, kept[0]) {
		t.Errorf("the error does not name the kept copy (%v): %s", kept, msg)
	}
}

// The postcondition, not the regeneration: the regenerator RUNS and reports
// success, and the artifact still does not describe the tree it sits in. This
// is the failure that would otherwise ship a marker describing different specs
// and be caught at CI, after a rework had been spent on it.
func TestRebaselineRefusesWhenTheRegeneratedArtifactDoesNotDescribeTheTree(t *testing.T) {
	f := newRebaselineFixture(t, "T9010")
	f.taskEditsADerivedArtifact("{\"marker\":\"the task's spec edit\"}\n")
	f.mainMovesElsewhere()
	// The regenerator runs and writes; only its own check disagrees.
	real := regenerators["specs/SPEC_VERSION.json"]
	regenerators["specs/SPEC_VERSION.json"] = struct {
		Write []string
		Check []string
	}{
		Write: []string{"sh", "-c", `printf '%s' "$0" > specs/SPEC_VERSION.json`, "a marker that describes some other tree\n"},
		Check: []string{"false"},
	}
	t.Cleanup(func() { regenerators["specs/SPEC_VERSION.json"] = real })

	before := f.pathsAndContents(f.baseline)
	beforeHead := f.git(f.worktree, "rev-parse", "HEAD")

	_, err := RebaselineTask(f.root, "T9010", "", "")
	if err == nil {
		t.Fatal("an advance whose derived artifact does not describe the merged tree was allowed")
	}
	if msg := err.Error(); !strings.Contains(msg, "does not describe the merged tree") {
		t.Fatalf("expected the postcondition refusal, got: %s", msg)
	}
	// And the postcondition failing is a failure AFTER the rebuild, so it has to
	// put everything back like the others.
	if after := f.pathsAndContents(f.baseline); after != before {
		t.Errorf("the postcondition refusal changed the tree:\n--- as found ---\n%s\n--- after ---\n%s", before, after)
	}
	if head := f.git(f.worktree, "rev-parse", "HEAD"); head != beforeHead {
		t.Errorf("the worktree HEAD is at %s after the postcondition refusal; it was %s", head, beforeHead)
	}
	// And no ledger entry: an implementation that records the ref before this
	// point leaves a record naming a move the restore behind it undid, and the
	// next collect reads that as "the Supervisor moved this branch" and stops
	// looking. This is the latest failure THIS shape reaches; the one after it is
	// the work-survival postcondition, whose failure branch T9019 drives through
	// RebaselineTask — the same assertion, one failure later. (This comment used
	// to say nothing could reach it, which was false: a task-created 0700 file
	// does, and that is T9017.)
	refs, err := ListSupervisorRefs(f.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range refs {
		if r.Name == "refs/heads/"+f.branch {
			t.Errorf("the postcondition refusal recorded %s at %s; the ref is back at the task's own tip", r.Name, r.SHA)
		}
	}
}

// A task whose tree contains a nested git repository. `git clean -fdq` refuses
// to remove one without -ff, so the directory it names is still there, non-empty,
// when the restore runs — and the restore's clear-first step used to os.Remove it
// for every state, which fails with ENOTEMPTY. Every refusal of such a task then
// claimed a failure that had not happened, with the tree half-restored and stopped
// at whatever the sorted order put after the directory.
func TestRebaselineADirectoryCleanWouldNotRemoveDoesNotAbortTheRestore(t *testing.T) {
	f := newRebaselineFixture(t, "T9011")
	nested := filepath.Join(f.worktree, "vendor", "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	f.git(nested, "init", "-q")
	if err := os.WriteFile(filepath.Join(nested, "f.txt"), []byte("inside a repository of its own\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.mainRewritesTheSameRegion() // the task's patch no longer applies, so the restore runs

	before := f.pathsAndContents(f.baseline)
	_, err := RebaselineTask(f.root, "T9011", "", "")
	if err == nil {
		t.Fatal("the advance was not refused")
	}
	// The refusal is the apply; the restore behind it is what this test is for.
	if msg := err.Error(); !strings.Contains(msg, "patch does not apply") {
		t.Fatalf("expected git to refuse the patch, got: %s", msg)
	}
	if msg := err.Error(); strings.Contains(msg, "could NOT be put back") {
		t.Errorf("the restore reported a failure it did not have: %s", msg)
	}
	if after := f.pathsAndContents(f.baseline); after != before {
		t.Errorf("the refused advance changed the tree it refused to advance:\n--- as found ---\n%s\n--- after ---\n%s", before, after)
	}
	if got := readFileOrFail(t, filepath.Join(nested, "f.txt")); got != "inside a repository of its own\n" {
		t.Errorf("the nested repository did not survive the refusal: %q", got)
	}
}

// A mode-only change that main has since made too. Git records one permission
// bit, so "the task made this executable" and "main made this executable" are
// the same change — and after the advance the path agrees with main entirely,
// which is indistinguishable from lost work to anything that compares path
// NAMES. The postcondition compares the kept copy instead, and it must not
// refuse this: the work is there, main just made it too.
func TestRebaselineAModeOnlyChangeMainAlsoMade(t *testing.T) {
	f := newRebaselineFixture(t, "T9012")
	if err := os.Chmod(filepath.Join(f.worktree, "scripts", "helper.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(f.root, "scripts", "helper.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.git(f.root, "add", "-A")
	f.git(f.root, "commit", "-q", "-m", "main makes the helper executable")
	newMain := f.git(f.root, "rev-parse", "HEAD")

	res, err := RebaselineTask(f.root, "T9012", "", "")
	if err != nil {
		t.Fatalf("the advance was refused: the task's change is a mode main has since made too, which is not a loss: %v", err)
	}
	if res.ToSHA != newMain {
		t.Errorf("the advance landed on %s, want %s", res.ToSHA, newMain)
	}
	if out := f.git(f.worktree, "status", "--porcelain"); strings.Contains(out, "helper.sh") {
		t.Errorf("the advanced tree still differs from main at scripts/helper.sh:\n%s", out)
	}
	st, err := os.Lstat(filepath.Join(f.worktree, "scripts", "helper.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o111 == 0 {
		t.Errorf("the executable bit the task set did not survive the advance: scripts/helper.sh is %v", st.Mode())
	}
}

// A task that replaced a tracked directory with a file. `reset --hard head`
// puts the directory back, so the restore has to empty it before it can write
// what stands there now — and the file the task deleted inside it is a snapshot
// entry of its own, which sorts AFTER the path it has to empty. In name order
// the restore met a directory it could not remove and reported a tree it could
// have reproduced as one it could not.
func TestRebaselineATaskThatReplacedADirectoryWithAFile(t *testing.T) {
	f := newRebaselineFixture(t, "T9014")
	legacy := filepath.Join(f.worktree, "internal", "legacy")
	if err := os.Remove(filepath.Join(legacy, "old.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(legacy); err != nil {
		t.Fatal(err)
	}
	f.write("internal/legacy", "the task put a file where the directory was\n", 0o644)
	f.mainRewritesTheSameRegion() // refused, so the restore is what has to rebuild this shape

	before := f.pathsAndContents(f.baseline)
	_, err := RebaselineTask(f.root, "T9014", "", "")
	if err == nil {
		t.Fatal("the advance was not refused")
	}
	if msg := err.Error(); strings.Contains(msg, "could NOT be put back") {
		t.Errorf("the restore reported a failure it did not have: %s", msg)
	} else if !strings.Contains(msg, "was put back") {
		t.Errorf("the refusal does not say what happened to the worktree: %s", msg)
	}
	if after := f.pathsAndContents(f.baseline); after != before {
		t.Errorf("the refused advance changed the tree it refused to advance:\n--- as found ---\n%s\n--- after ---\n%s", before, after)
	}
	st, err := os.Lstat(legacy)
	if err != nil {
		t.Fatalf("internal/legacy is gone: %v", err)
	}
	if !st.Mode().IsRegular() {
		t.Errorf("internal/legacy came back as %v; the task had made it a regular file", st.Mode())
	}
	if got := readFileOrFail(t, legacy); got != "the task put a file where the directory was\n" {
		t.Errorf("internal/legacy came back as %q", got)
	}
	if _, err := os.Lstat(filepath.Join(legacy, "old.txt")); err == nil {
		t.Errorf("internal/legacy/old.txt is back; the task deleted it along with the directory it lived in")
	}
}

// The mirror of the tracked-link case: the task replaced a tracked SYMLINK with
// a directory. `reset --hard head` puts the link back, and MkdirAll follows a
// symlink to a directory — so the restore reported success with the link still
// standing, and the file that belongs under the new directory was written
// THROUGH the link, into a subtree the snapshot never mentioned.
func TestRebaselineATaskThatReplacedATrackedSymlinkWithADirectory(t *testing.T) {
	f := newRebaselineFixture(t, "T9015")
	if err := os.Remove(filepath.Join(f.worktree, "tracked-dir-link")); err != nil {
		t.Fatal(err)
	}
	f.write("tracked-dir-link/inner.txt", "inside the directory that replaced the link\n", 0o644)
	f.mainRewritesTheSameRegion()

	before := f.pathsAndContents(f.baseline)
	_, err := RebaselineTask(f.root, "T9015", "", "")
	if err == nil {
		t.Fatal("the advance was not refused")
	}
	if after := f.pathsAndContents(f.baseline); after != before {
		t.Errorf("the refused advance changed the tree it refused to advance:\n--- as found ---\n%s\n--- after ---\n%s", before, after)
	}
	replaced := filepath.Join(f.worktree, "tracked-dir-link")
	st, err := os.Lstat(replaced)
	if err != nil {
		t.Fatalf("tracked-dir-link is gone: %v", err)
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		t.Errorf("tracked-dir-link came back as %v; the task had made it a directory", st.Mode())
	}
	if got := readFileOrFail(t, filepath.Join(replaced, "inner.txt")); got != "inside the directory that replaced the link\n" {
		t.Errorf("tracked-dir-link/inner.txt came back as %q", got)
	}
	// The link's target is a different subtree, and nothing the task did belongs
	// in it.
	if _, err := os.Lstat(filepath.Join(f.worktree, "internal", "inner.txt")); err == nil {
		t.Errorf("the restore wrote through the symlink: internal/inner.txt exists, and the snapshot never mentioned it")
	}
}

// The ledger entry is what tells a sibling Worker collecting right now that the
// branch moved under the Supervisor's hand rather than the Worker's, so it must
// not describe a move that was undone. Every failure after the rebuild restores
// the ref, which is why the write is last; an entry naming main while the ref is
// back at the task's own tip is worse than no entry at all, because the next
// collect reads it as "the Supervisor moved this" and stops looking.
func TestRebaselineARefusedAdvanceLeavesNoLedgerEntry(t *testing.T) {
	f := newRebaselineFixture(t, "T9013")
	f.mainRewritesTheSameRegion()

	if _, err := RebaselineTask(f.root, "T9013", "", ""); err == nil {
		t.Fatal("the advance was not refused")
	}
	refs, err := ListSupervisorRefs(f.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range refs {
		if r.Name == "refs/heads/"+f.branch {
			t.Errorf("the refused advance recorded %s at %s; the ref is back at the task's own tip, so the ledger describes a move that was undone", r.Name, r.SHA)
		}
	}
}

// The work-survival postcondition is the instrument that replaced the comparison
// of path SETS, and what it reports IS its whole value: a path set cannot tell
// "the task's bytes" from "main's bytes", and everything it says about a change
// the task never made is a refusal invented rather than observed.
//
// This drives the check directly, one rule at a time, on real files — which is
// what dies when a rule is removed. It is not the only way in: T9017, T9018 and
// T9019 reach the same failure branch through RebaselineTask. (This comment used
// to claim none could — "the advance either carries the work or dies earlier" —
// and that was false: a task-created 0700 file reaches it, which is T9017.)
// Three rules are pinned with it:
//
//   - a path whose bytes came back different is a loss, named with both digests;
//   - an executable bit the TASK set and the advance dropped is a loss;
//   - an executable bit MAIN set, on a path whose mode the task never touched, is
//     not — the advanced tree holding main's mode there is main's change, and
//     calling it a loss refuses ordinary advances.
func TestTheWorkSurvivalCheckReportsALossAndSaysNothingAboutOneThatIsNotOne(t *testing.T) {
	f := newRebaselineFixture(t, "T9016")
	keep := t.TempDir()

	writeKept := func(rel, content string) {
		t.Helper()
		dst := filepath.Join(keep, "files", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The two shapes a byte comparison has to separate: the task's bytes, and
	// main's bytes where the task's used to be.
	const theTasks = "what the task wrote\n"
	const mains = "what main wrote over it, with a different length\n"
	f.write("docs/kept.txt", theTasks, 0o644)
	writeKept("docs/kept.txt", theTasks)

	loss := func(entries []snapshotEntry, regenerated []string) string {
		t.Helper()
		// The same commit on both sides: nothing in this fixture is in the
		// baseline and the target at once with different bytes, so "what main
		// has" and "what the base has" are the same question here. The merge
		// with a main that moved is T9022, through RebaselineTask.
		err := verifyTheWorkSurvived(f.worktree, keep, f.baseline, f.baseline, entries, regenerated)
		if err == nil {
			return ""
		}
		return err.Error()
	}
	entry := func(p, state string, mode os.FileMode) snapshotEntry {
		return snapshotEntry{Path: p, State: state, Mode: mode}
	}

	// The advance carried it: the bytes are the task's, and the check says so by
	// saying nothing.
	if got := loss([]snapshotEntry{entry("docs/kept.txt", "file", 0o644)}, nil); got != "" {
		t.Fatalf("the check called an advance that carried the work a loss: %s", got)
	}
	// Main's bytes in its place: the path is still there and still a file, so
	// nothing but a comparison of content can see this.
	f.write("docs/kept.txt", mains, 0o644)
	got := loss([]snapshotEntry{entry("docs/kept.txt", "file", 0o644)}, nil)
	if !strings.Contains(got, "did not carry the task's work across") || !strings.Contains(got, "docs/kept.txt") {
		t.Fatalf("a path holding main's bytes instead of the task's was not reported as a loss: %q", got)
	}
	for _, want := range []string{shortDigest([]byte(theTasks)), shortDigest([]byte(mains))} {
		if !strings.Contains(got, want) {
			t.Errorf("the loss of docs/kept.txt does not say what it found vs. what it lost (%s): %s", want, got)
		}
	}

	// The executable bit the task created, dropped by the advance. The path is
	// new — nothing in the baseline to compare a mode against — so the task's own
	// bit is the only thing the check can mean.
	f.write("docs/created.sh", "#!/bin/sh\n", 0o755)
	writeKept("docs/created.sh", "#!/bin/sh\n")
	created := []snapshotEntry{entry("docs/created.sh", "file", 0o755)}
	// os.WriteFile does not change the mode of a file that is already there, so
	// the drop this is about is made the way the advance would make it.
	if err := os.Chmod(filepath.Join(f.worktree, "docs", "created.sh"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loss(created, nil); !strings.Contains(got, "docs/created.sh came back not executable") {
		t.Errorf("a dropped executable bit the task had set was not reported as a loss: %q", got)
	}
	if err := os.Chmod(filepath.Join(f.worktree, "docs", "created.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := loss(created, nil); got != "" {
		t.Errorf("the check called the task's own executable bit, carried across intact, a loss: %s", got)
	}

	// The same bit, set by MAIN while the task was in review, on a path whose mode
	// the task never touched. git records one permission bit; the task's copy of
	// it is 644 and the tree's is 755, and that difference is main's own change.
	if err := os.Chmod(filepath.Join(f.worktree, "scripts", "helper.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	untouched := []snapshotEntry{entry("scripts/helper.sh", "file", 0o644)}
	writeKept("scripts/helper.sh", "#!/bin/sh\necho helper\n") // the bytes are the task's; only the mode moved
	if got := loss(untouched, nil); got != "" {
		t.Errorf("main making a file executable was reported as work the advance lost: %s", got)
	}

	// A regenerated artifact is correct by construction: it is a function of the
	// merged tree, so the task's own copy of it is expected to be gone.
	if got := loss([]snapshotEntry{entry("specs/SPEC_VERSION.json", "file", 0o644)}, []string{"specs/SPEC_VERSION.json"}); got != "" {
		t.Errorf("a regenerated artifact was compared against the task's copy of it: %s", got)
	}
}

// A derived artifact is a function of its inputs, so a textual merge of it is
// meaningless in both directions. It is excluded from the patch and regenerated
// from the merged tree — and the exclusion is what makes that possible when main
// has moved the same generated file, which is the normal case rather than an
// exotic one.
func TestRebaselineRegeneratesADerivedArtifactItWillNotMergeAsText(t *testing.T) {
	f := newRebaselineFixture(t, "T9008")
	f.taskEditsADerivedArtifact("{\"marker\":\"the task's spec edit\"}\n")
	newMain := f.mainMovesTheDerivedArtifact()
	fakeTheRegenerator(t, "regenerated from the merged tree\n")

	res, err := RebaselineTask(f.root, "T9008", "", "")
	if err != nil {
		t.Fatalf("the advance was refused: a conflict on a derived artifact is not a conflict, it is a regeneration: %v", err)
	}
	if res.ToSHA != newMain {
		t.Errorf("the advance landed on %s, want %s", res.ToSHA, newMain)
	}
	if len(res.Regenerated) != 1 || res.Regenerated[0] != "specs/SPEC_VERSION.json" {
		t.Errorf("the advance reports regenerated %v, want exactly [specs/SPEC_VERSION.json]", res.Regenerated)
	}
	got := readFileOrFail(t, filepath.Join(f.worktree, "specs/SPEC_VERSION.json"))
	if got != "regenerated from the merged tree\n" {
		t.Errorf("the artifact in the advanced tree is %q — neither the task's text nor main's, but the regenerated one", got)
	}
}

// Git records WHETHER a file is executable, not how. The mode it writes for a
// task-created executable is 100755 whatever exec bits the task set, and `git
// apply` writes that masked by the umask — so a file the task left at 0700 comes
// back 0755. Both are executable, and both are the same change as far as
// anything downstream can see it: the commit, the review diff, CI.
//
// The postcondition compared the three-bit MASKS, and 0755&0111 (0111) is not
// 0700&0111 (0100), so a task-created 0700 file was a loss on every attempt —
// with a message that contradicted itself ("docs/created.sh came back
// executable; the task left it executable"). The refusal put the 0700 file back,
// so the next attempt took the same path: not a transient failure but a task
// that could never advance, refused by a check whose comparison depended on the
// umask of the process that ran it.
func TestRebaselineATaskCreatedFileWithAnUnusualExecMode(t *testing.T) {
	f := newRebaselineFixture(t, "T9017")
	f.mainMovesElsewhere() // the advance is meant to succeed, so nothing else may refuse it
	p := filepath.Join(f.worktree, "docs", "created.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho created\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o700); err != nil {
		t.Fatal(err)
	}
	// The umask the advance's `git apply` inherits. It is what makes a mask
	// comparison an assertion about the environment rather than about the change.
	prev := syscall.Umask(0o022)
	defer syscall.Umask(prev)

	if _, err := RebaselineTask(f.root, "T9017", "", ""); err != nil {
		t.Fatalf("a task-created 0700 file was refused: %v", err)
	}
	st, err := os.Lstat(p)
	if err != nil {
		t.Fatalf("docs/created.sh is gone after the advance: %v", err)
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o100 == 0 {
		t.Errorf("docs/created.sh came back as %v, want a regular file git can record as executable", st.Mode())
	}
	if got := readFileOrFail(t, p); got != "#!/bin/sh\necho created\n" {
		t.Errorf("docs/created.sh came back as %q", got)
	}
}

// The same defect on a path that IS in the base: a tracked file the task made
// executable at 0700. The base's entry says "not executable" and the task's mode
// says "executable", so the mode IS the task's own change and the check has to
// compare it to what the advance produced. It is the second half of T9017's
// fix — a version that special-cased task-CREATED paths, "nothing in base, so
// any exec bit will do", passes T9017 and refuses this one forever, and the two
// shapes differ in the base's entry rather than in anything the task did.
func TestRebaselineATrackedFileTheTaskMadeExecutable(t *testing.T) {
	f := newRebaselineFixture(t, "T9018")
	f.mainMovesElsewhere()
	p := filepath.Join(f.worktree, "scripts", "helper.sh")
	if err := os.Chmod(p, 0o700); err != nil {
		t.Fatal(err)
	}
	prev := syscall.Umask(0o022)
	defer syscall.Umask(prev)

	if _, err := RebaselineTask(f.root, "T9018", "", ""); err != nil {
		t.Fatalf("a tracked file the task made executable at 0700 was refused: %v", err)
	}
	st, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o100 == 0 {
		t.Errorf("scripts/helper.sh came back as %v; the executable bit the task set is gone", st.Mode())
	}
}

// A task that replaced a tracked file with a DIRECTORY whose only content git
// will not name: an ignored file. `reset --hard` deletes such a directory
// outright when it stands where a tracked path has to go — contents and all —
// so if the copy does not hold what is inside it, the restore is asked to
// reproduce a tree it no longer has the bytes for, and the refusal says the
// tree "was put back" while the task's directory is gone.
//
// It also reaches the work-survival postcondition through RebaselineTask: after
// the reset the tracked path is a file again and the task had made it a
// directory, which is exactly what that check reports. So it is the latest
// failure a test can drive here, and the ledger write has to be after it — an
// entry naming main while the ref is back at the task's own tip is read by the
// next collect as "the Supervisor moved this branch", and it stops looking.
func TestRebaselinePutsBackWhatWasInsideTheDirectoryThatReplacedATrackedPath(t *testing.T) {
	f := newRebaselineFixture(t, "T9019")
	f.mainMovesElsewhere()
	// The task's own .gitignore, so the pattern is part of its change.
	f.write(".gitignore", "scratch/\n", 0o644)
	if err := os.Remove(filepath.Join(f.worktree, "scripts", "helper.sh")); err != nil {
		t.Fatal(err)
	}
	hidden := filepath.Join(f.worktree, "scripts", "helper.sh", "scratch", "x.txt")
	if err := os.MkdirAll(filepath.Dir(hidden), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hidden, []byte("an ignored file the task left\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	before := f.pathsAndContents(f.baseline)
	beforeHead := f.git(f.worktree, "rev-parse", "HEAD")
	_, err := RebaselineTask(f.root, "T9019", "", "")
	if err == nil {
		t.Fatal("the advance succeeded with the task's directory of ignored content deleted")
	}
	msg := err.Error()
	// The work-survival check, not the apply: this is the branch whose position
	// in the order the ledger assertion below is about.
	if !strings.Contains(msg, "did not carry the task's work across") {
		t.Fatalf("a different refusal reached this test: %s", msg)
	}
	if strings.Contains(msg, "could NOT be put back") {
		t.Errorf("the restore reported a failure it did not have: %s", msg)
	}
	if after := f.pathsAndContents(f.baseline); after != before {
		t.Errorf("the refused advance changed the tree it refused to advance:\n--- as found ---\n%s\n--- after ---\n%s", before, after)
	}
	if _, err := os.Lstat(hidden); err != nil {
		// Said as what it means rather than left to the read below: this path is
		// ignored, so it is in no listing the test compares and this line is the
		// only one that notices it is gone.
		t.Fatalf("the ignored file the task left is not in the tree the restore says it put back: %v", err)
	}
	if got := readFileOrFail(t, hidden); got != "an ignored file the task left\n" {
		t.Errorf("the restore claims the tree is as it was found, but the ignored file came back as %q", got)
	}
	if head := f.git(f.worktree, "rev-parse", "HEAD"); head != beforeHead {
		t.Errorf("the worktree HEAD is at %s after the refusal; it was %s", head, beforeHead)
	}
	refs, err := ListSupervisorRefs(f.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range refs {
		if r.Name == "refs/heads/"+f.branch {
			t.Errorf("the work-survival refusal recorded %s at %s; the ref is back at the task's own tip", r.Name, r.SHA)
		}
	}
}

// The failure a second before the worktree is rebuilt: the copy cannot be taken.
// The worktree still holds the task's work — it has not been touched — so the
// only thing to get right is the directory that was made for the copy. It names
// the task, and a directory that names a task and holds nothing is worse than no
// directory at all: the refusal that follows it says "where the work is kept"
// and points at an empty room.
//
// The failure cannot be produced on demand — hence the seam — and what it must
// not do is leave that directory behind, or touch the tree on its way out.
func TestRebaselineLeavesNoKeptCopyWhenTheSnapshotCannotBeTaken(t *testing.T) {
	f := newRebaselineFixture(t, "T9020")
	f.mainRewritesTheSameRegion() // a refusal is coming either way

	real := snapshot
	snapshot = func(string, string, string, map[string]bool, string) ([]snapshotEntry, error) {
		return nil, errors.New("the snapshot could not be taken")
	}
	defer func() { snapshot = real }()

	before := f.pathsAndContents(f.baseline)
	beforeHead := f.git(f.worktree, "rev-parse", "HEAD")
	_, err := RebaselineTask(f.root, "T9020", "", "")
	if err == nil {
		t.Fatal("a snapshot that could not be taken did not stop the advance")
	}
	if !strings.Contains(err.Error(), "the snapshot could not be taken") {
		t.Errorf("the refusal does not say what went wrong: %s", err)
	}
	if kept := f.keptDirs(); len(kept) != 0 {
		t.Errorf("a failure before the worktree was touched left %v behind; the error names that directory as where the work is kept", kept)
	}
	if after := f.pathsAndContents(f.baseline); after != before {
		t.Errorf("a failure before the worktree was touched changed it:\n--- as found ---\n%s\n--- after ---\n%s", before, after)
	}
	if head := f.git(f.worktree, "rev-parse", "HEAD"); head != beforeHead {
		t.Errorf("the worktree HEAD is at %s after the failure; it was %s", head, beforeHead)
	}
}

// A task that replaced a tracked DIRECTORY with a file, on a main that moved
// elsewhere — so the advance succeeds, and the verifier walks a snapshot of the
// tree the task left. One of its entries is internal/legacy/old.txt, a tracked
// file inside the directory the task deleted, and in the tree the task left that
// path cannot exist for a reason other than "it is gone": its parent is now a
// regular file. Lstat answers ENOTDIR there, and a check that reads only ENOENT
// as absence refuses this advance with an error about reading a path that cannot
// exist — which is the shape T9014 reaches on the refusal path, and this is the
// same shape on the path that has to be allowed through.
func TestRebaselineADirectoryReplacedByAFileOnAMainThatMoved(t *testing.T) {
	f := newRebaselineFixture(t, "T9021")
	legacy := filepath.Join(f.worktree, "internal", "legacy")
	if err := os.Remove(filepath.Join(legacy, "old.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(legacy); err != nil {
		t.Fatal(err)
	}
	f.write("internal/legacy", "the task put a file where the directory was\n", 0o644)
	newMain := f.mainMovesElsewhere()

	res, err := RebaselineTask(f.root, "T9021", "", "")
	if err != nil {
		t.Fatalf("the advance was refused: %v", err)
	}
	if res.ToSHA != newMain {
		t.Errorf("the advance landed on %s, want %s", res.ToSHA, newMain)
	}
	st, err := os.Lstat(legacy)
	if err != nil {
		t.Fatalf("internal/legacy is gone: %v", err)
	}
	if !st.Mode().IsRegular() {
		t.Errorf("internal/legacy came back as %v; the task had made it a regular file", st.Mode())
	}
	if got := readFileOrFail(t, legacy); got != "the task put a file where the directory was\n" {
		t.Errorf("internal/legacy came back as %q", got)
	}
}

// mainAddsItsOwnFileAt advances main with a new tracked FILE at the path given —
// the shape where the target commit has a file exactly where the task may have
// put a directory.
func (f *rebaselineFixture) mainAddsItsOwnFileAt(rel string) string {
	f.t.Helper()
	p := filepath.Join(f.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("main's own file\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.git(f.root, "add", "-A")
	f.git(f.root, "commit", "-q", "-m", "main added a file where the task has a directory")
	return f.git(f.root, "rev-parse", "HEAD")
}

// mainChangesTheSameFileElsewhere is the clean merge: main edits the SAME file
// the task edited, in a different region of it, so neither change is lost and
// neither side is overwritten.
func (f *rebaselineFixture) mainChangesTheSameFileElsewhere() string {
	f.t.Helper()
	p := filepath.Join(f.root, "tests", "acceptance", "gate.sh")
	base := readFileOrFail(f.t, p)
	if err := os.WriteFile(p, []byte(base+"\nprobe_main() { echo \"main's own probe\"; }\n"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	f.git(f.root, "add", "-A")
	f.git(f.root, "commit", "-q", "-m", "main added its own probe to the gate script")
	return f.git(f.root, "rev-parse", "HEAD")
}

// A file both sides changed, in different places, is the ordinary clean merge —
// and the advance has to carry BOTH changes, because it applies the task's
// change TO MAIN rather than replacing main with the task's copy.
//
// The check that ran before this one compared the merged file against the
// task's own copy and refused when they differed, which is precisely what a
// merge makes them do. The refusal came with "the advance did not carry the
// task's work across" — the opposite of what had happened — and it could never
// be resolved by reworking the task, because there was nothing wrong with it.
func TestRebaselineCarriesBothChangesToTheSameFile(t *testing.T) {
	f := newRebaselineFixture(t, "T9022")
	newMain := f.mainChangesTheSameFileElsewhere()

	res, err := RebaselineTask(f.root, "T9022", "", "")
	if err != nil {
		t.Fatalf("a clean merge of two changes to one file was refused: %v", err)
	}
	if res.ToSHA != newMain {
		t.Errorf("the advance landed on %s, want %s", res.ToSHA, newMain)
	}
	got := readFileOrFail(t, filepath.Join(f.worktree, "tests", "acceptance", "gate.sh"))
	for _, want := range []string{"probe_hmac", "probe_main"} {
		if !strings.Contains(got, want) {
			t.Errorf("the advanced gate script does not hold %s — one side's change was dropped:\n%s", want, got)
		}
	}
	if kept := f.keptDirs(); len(kept) != 0 {
		t.Errorf("a completed advance left %v behind", kept)
	}
}

// A file inside the directory that replaced a tracked path can be named TWICE:
// once by the task's own path list (it is untracked and not ignored, so
// `ls-files --others` lists it) and once by the walk that records what is inside
// that directory. Recording it twice would put two lines in the manifest and two
// entries in Files — a report of one path as two, which is how a measurement
// stops agreeing with what it measures.
//
// The name ends in a space on purpose: it is the shape a comparison that trims
// whitespace gets wrong, and nothing between the path list and the manifest may
// trim it here either.
func TestTheWalkDoesNotRecordAPathTwice(t *testing.T) {
	f := newRebaselineFixture(t, "T9023")
	// The task replaces the tracked scripts/helper.sh with a directory holding
	// one file — untracked, not ignored, so git names it.
	if err := os.Remove(filepath.Join(f.worktree, "scripts", "helper.sh")); err != nil {
		t.Fatal(err)
	}
	const inner = "scripts/helper.sh/scratch/notes "
	f.write(inner, "named twice if the walk forgets what the path list took\n", 0o644)

	paths, err := worktreeChangedPaths(f.worktree, f.baseline)
	if err != nil {
		t.Fatal(err)
	}
	if !paths[inner] {
		t.Fatalf("the fixture is not the shape this test is about: %q is not in the task's path list", inner)
	}
	head := f.git(f.worktree, "rev-parse", "HEAD")
	keep := t.TempDir()
	entries, err := snapshotWorktree(f.worktree, head, f.baseline, paths, keep)
	if err != nil {
		t.Fatalf("the snapshot refused a tree it has to be able to record: %v", err)
	}

	n := 0
	for _, e := range entries {
		if e.Path == inner {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the snapshot holds %d entries for %s; the path list took it, so the walk must leave it alone", n, inner)
	}
	manifest := readFileOrFail(t, filepath.Join(keep, "MANIFEST.txt"))
	if got := strings.Count(manifest, inner); got != 1 {
		t.Errorf("the manifest names %s %d times:\n%s", inner, got, manifest)
	}
	if !strings.Contains(manifest, inner) {
		t.Errorf("the manifest does not name %s at all — the name was trimmed somewhere between the path list and the file:\n%s", inner, manifest)
	}
}

// A fifo inside a directory a reset deletes is content the restore has to put
// back AS a fifo. Recorded as a "dir" — which is what the default arm of the
// snapshot did with it — it came back as an empty directory: the refusal says
// the tree was put back the way it was found and hands back a different tree.
// (The fifo is only ever reached through the walk: git does not list one at all,
// so `ls-files --others` never names it and the path list never holds it.)
func TestRebaselinePutsBackAFifoAsAFifo(t *testing.T) {
	f := newRebaselineFixture(t, "T9024")
	f.mainMovesElsewhere()
	f.write(".gitignore", "scratch/\n", 0o644)
	if err := os.Remove(filepath.Join(f.worktree, "scripts", "helper.sh")); err != nil {
		t.Fatal(err)
	}
	const hidden = "an ignored file the task left\n"
	f.write("scripts/helper.sh/scratch/x.txt", hidden, 0o644)
	pipe := filepath.Join(f.worktree, "scripts", "helper.sh", "scratch", "pipe")
	if err := syscall.Mkfifo(pipe, 0o644); err != nil {
		t.Fatalf("making the fifo the fixture is about: %v", err)
	}

	// The ignored content cannot travel in a patch, so the advance refuses —
	// which is what makes the restore, and the fifo's kind, the thing under test.
	_, err := RebaselineTask(f.root, "T9024", "", "")
	if err == nil {
		t.Fatal("the advance succeeded with the task's ignored content deleted")
	}
	if !strings.Contains(err.Error(), "did not carry the task's work across") {
		t.Fatalf("a different refusal reached this test: %v", err)
	}
	if got := readFileOrFail(t, filepath.Join(f.worktree, "scripts", "helper.sh", "scratch", "x.txt")); got != hidden {
		t.Errorf("the restore left the ignored file as %q", got)
	}
	st, err := os.Lstat(pipe)
	if err != nil {
		t.Fatalf("the fifo is gone after the refusal: %v", err)
	}
	if st.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("the task's fifo came back as %v — the refusal reports a tree put back the way it was found", st.Mode())
	}
	// Nothing opened it: a fifo with no writer blocks the reader forever, which
	// is why the kept copy holds no bytes for one.
	if got := st.Mode().Perm(); got != 0o644 {
		t.Errorf("the fifo came back with mode %04o, want 0644", got)
	}
}

// T9025.
//
// A socket is a rendezvous rather than content: there is nothing to keep and
// nothing to put back, and calling it a directory — which is what the snapshot's
// default arm did — is a refusal that reports a tree put back and hands back a
// different one. The advance refuses instead, and refuses in the SNAPSHOT, which
// runs before the reset: the tree still holds everything and the copy is not yet
// the only copy.
//
// This drives the snapshot directly rather than through RebaselineTask: a unix
// socket's address is at most about a hundred bytes and the fixture's root is a
// path as long as the test's own name, so the socket is made where there is
// room. What the advance-level refusal does about the tree and the copy is
// T9020's shape, which drives the snapshot failing for any reason at all.
func TestTheSnapshotRefusesASocketRatherThanCallingItADirectory(t *testing.T) {
	root, err := os.MkdirTemp("", "rd-sock")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(root) }()
	sock := filepath.Join(root, "s")
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("making the socket the fixture is about: %v", err)
	}
	defer func() { _ = syscall.Close(fd) }()
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: sock}); err != nil {
		t.Fatalf("binding %s: %v", sock, err)
	}
	fifo := filepath.Join(root, "p")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}

	keep := t.TempDir()
	_, err = snapshotOne(root, keep, "s")
	if err == nil {
		t.Fatal("a socket was recorded as something it can be restored into")
	}
	for _, want := range []string{"s is a socket", "Nothing has been touched"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(keep, "files", "s")); !os.IsNotExist(err) {
		t.Errorf("the refused kind was written into the kept copy anyway (%v)", err)
	}
	// The fifo is the control: the arm above is a refusal for kinds that cannot
	// be reproduced, not a refusal of everything that is not a regular file.
	e, err := snapshotOne(root, keep, "p")
	if err != nil {
		t.Fatalf("a fifo was refused as a kind that cannot be carried: %v", err)
	}
	if e.State != "fifo" {
		t.Errorf("a fifo was recorded as %q", e.State)
	}
}

// A directory the task created, holding only content the ignore rules cover,
// standing where the TARGET commit has a file. git cannot name the directory at
// all — a directory whose whole content is ignored is invisible to `git status`,
// to `ls-files --others --exclude-standard` and to `git diff` — so it is in no
// path list, and `reset --hard <target>` deletes it whole to write main's file.
// The reset is silent about it (that is what ignored means), and the advance
// was silent too: it succeeded with the task's work deleted.
//
// Asking the TASK's HEAD whether it tracks the path is the wrong question here
// twice over: HEAD has never heard of a file main added after the branch point,
// and the only reset that matters for the preservation is the one being made to
// the target. The directors are found by asking both commits instead, which is
// what makes the refusal below possible at all. (Before that, the walk was
// triggered from an entry in the path list, and there was never an entry.)
func TestRebaselineNoticesADirectoryOnlyTheTargetWouldDelete(t *testing.T) {
	f := newRebaselineFixture(t, "T9026")
	f.write(".gitignore", "scratch/\n", 0o644) // the task's own ignore rule
	const hidden = "the task's ignored work\n"
	inner := filepath.Join(f.worktree, "notes", "scratch", "x.txt")
	if err := os.MkdirAll(filepath.Dir(inner), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inner, []byte(hidden), 0o644); err != nil {
		t.Fatal(err)
	}
	// main's own file, exactly where the task has a directory.
	newMain := f.mainAddsItsOwnFileAt("notes")

	_, err := RebaselineTask(f.root, "T9026", "", "")
	if err == nil {
		t.Fatal("the advance deleted the task's directory of ignored content and reported success")
	}
	if !strings.Contains(err.Error(), "did not carry the task's work across") || !strings.Contains(err.Error(), "notes") {
		t.Fatalf("a different refusal reached this test: %v", err)
	}
	if got := readFileOrFail(t, inner); got != hidden {
		t.Errorf("the refusal kept the work and put it back as %q", got)
	}
	if head := f.git(f.worktree, "rev-parse", "HEAD"); head == newMain {
		t.Errorf("the refused advance left the branch at main's tip %s", head)
	}
	// The copy is where the error says the work is kept, and it holds the file.
	kept := f.keptDirs()
	if len(kept) != 1 {
		t.Fatalf("the refusal kept %v; the error names that directory as where the work is", kept)
	}
	keptFile := filepath.Join(kept[0], "files", "notes", "scratch", "x.txt")
	if got, err := os.ReadFile(keptFile); err != nil || string(got) != hidden {
		t.Errorf("the kept copy of the ignored file is (%q, %v)", got, err)
	}
}

// treeDump describes every path in the worktree outside `.git` — the kind, the
// mode and the content. It is what "the tree is exactly as it was found" means
// when part of the tree is content git cannot name: a fifo, an empty directory, a
// socket. `pathsAndContents` cannot say it, because it is built from git's path
// lists, and those are precisely what leave such content out.
//
// `.git` is skipped: reading the tree moves it (that is a thing the round-7 work
// on #99 learned the hard way), and it is not part of the task's work either way.
func (f *rebaselineFixture) treeDump() string {
	f.t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(f.worktree, func(full string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(f.worktree, full)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if rel == ".git" {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil // a linked worktree: .git is a file
		}
		st, lerr := os.Lstat(full)
		if lerr != nil {
			return lerr
		}
		switch {
		case st.Mode()&os.ModeSymlink != 0:
			target, lerr := os.Readlink(full)
			if lerr != nil {
				return lerr
			}
			fmt.Fprintf(&b, "symlink\t%s\t%s\n", rel, target)
		case st.Mode().IsRegular():
			data, rerr := os.ReadFile(full)
			if rerr != nil {
				return rerr
			}
			sum := sha256.Sum256(data)
			fmt.Fprintf(&b, "file\t%04o\t%s\t%s\n", st.Mode().Perm(), rel, hex.EncodeToString(sum[:]))
		case st.IsDir():
			fmt.Fprintf(&b, "dir\t%s\n", rel)
		case st.Mode()&os.ModeNamedPipe != 0:
			fmt.Fprintf(&b, "fifo\t%04o\t%s\n", st.Mode().Perm(), rel)
		default:
			fmt.Fprintf(&b, "other\t%v\t%s\n", st.Mode(), rel)
		}
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return b.String()
}

// mainDeletes removes a tracked path on main: the shape where a directory in the
// task's tree stops being one a clean will leave alone.
func (f *rebaselineFixture) mainDeletes(rel string) string {
	f.t.Helper()
	f.git(f.root, "rm", "-q", rel)
	f.git(f.root, "commit", "-q", "-m", "main deleted "+rel)
	return f.git(f.root, "rev-parse", "HEAD")
}

// A fifo inside a PLAIN untracked directory — nothing tracked stands where the
// directory is, so no reset is involved — was deleted by the advance with no
// refusal, no record and no copy. It is in neither of git's path lists: `git
// status` says `?? scratchpad/` and stops at the directory, and `ls-files --others
// --exclude-standard` lists files, which a fifo is not. The clean removes the
// directory WHOLE, so the fifo goes with it, and the advance then reported that
// it had carried the task's work across.
//
// The refusal is the designed outcome — a fifo cannot travel in a patch, which is
// T9024's shape — and what this pins is that it happens and that the tree comes
// back whole: a refusal that quietly loses the fifo while saying the worktree was
// put back the way it was found is the same defect wearing the other face.
func TestRebaselineCarriesAFifoInAPlainUntrackedDirectory(t *testing.T) {
	f := newRebaselineFixture(t, "T9027")
	f.mainMovesElsewhere()
	pipe := filepath.Join(f.worktree, "scratchpad", "pipe")
	if err := os.MkdirAll(filepath.Dir(pipe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(pipe, 0o644); err != nil {
		t.Fatalf("making the fifo the fixture is about: %v", err)
	}
	before := f.treeDump()

	_, err := RebaselineTask(f.root, "T9027", "", "")
	if err == nil {
		t.Fatal("the advance deleted the task's fifo and reported success")
	}
	if msg := err.Error(); !strings.Contains(msg, "did not carry the task's work across") || !strings.Contains(msg, "scratchpad/pipe") {
		t.Fatalf("a different refusal reached this test: %v", err)
	}
	st, err := os.Lstat(pipe)
	if err != nil {
		t.Fatalf("the fifo is gone after the refusal: %v", err)
	}
	if st.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("scratchpad/pipe came back as %v — the refusal reports a tree put back the way it was found", st.Mode())
	}
	if got := st.Mode().Perm(); got != 0o644 {
		t.Errorf("the fifo came back with mode %04o, want 0644", got)
	}
	if after := f.treeDump(); after != before {
		t.Errorf("the refused advance did not put the tree back the way it was found:\n--- as found ---\n%s\n--- after ---\n%s", before, after)
	}
}

// An empty untracked directory is the other shape git cannot represent at all: it
// is in no commit, so `ls-files --others` cannot list it and `git status` never
// mentions it — and `clean -fdq` removes it, and any empty directory under it,
// without a word. Before this the advance reported success and the task's
// directories were gone.
func TestRebaselineCarriesAnEmptyUntrackedDirectory(t *testing.T) {
	f := newRebaselineFixture(t, "T9028")
	f.mainMovesElsewhere()
	if err := os.MkdirAll(filepath.Join(f.worktree, "notes-dir", "empty", "deeper"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := f.treeDump()

	_, err := RebaselineTask(f.root, "T9028", "", "")
	if err == nil {
		t.Fatal("the advance deleted the task's empty directories and reported success")
	}
	if msg := err.Error(); !strings.Contains(msg, "did not carry the task's work across") || !strings.Contains(msg, "notes-dir") {
		t.Fatalf("a different refusal reached this test: %v", err)
	}
	if after := f.treeDump(); after != before {
		t.Errorf("the refused advance did not put the tree back the way it was found:\n--- as found ---\n%s\n--- after ---\n%s", before, after)
	}
}

// The other side of the same question: a directory the clean does NOT remove. An
// ignored file keeps one alive — git will not delete it without -x, and it will
// not delete a directory it cannot empty — so that file survives the advance and
// nothing about it is carried or refused. A reach that took the whole directory
// anyway would refuse an advance over a file the advance never touches.
func TestRebaselineLeavesAnIgnoredFileTheCleanWouldNotRemove(t *testing.T) {
	f := newRebaselineFixture(t, "T9029")
	f.write(".gitignore", "*.ign\n", 0o644)
	f.write("staging/keep.txt", "the task's own file\n", 0o644)
	f.write("staging/notes.ign", "ignored, and left alone\n", 0o644)
	newMain := f.mainMovesElsewhere()

	res, err := RebaselineTask(f.root, "T9029", "", "")
	if err != nil {
		t.Fatalf("the advance was refused over a file it does not touch: %v", err)
	}
	if res.ToSHA != newMain {
		t.Errorf("the advance landed on %s, want %s", res.ToSHA, newMain)
	}
	if got := readFileOrFail(t, filepath.Join(f.worktree, "staging", "notes.ign")); got != "ignored, and left alone\n" {
		t.Errorf("the ignored file the clean does not remove came back as %q", got)
	}
	if got := readFileOrFail(t, filepath.Join(f.worktree, "staging", "keep.txt")); got != "the task's own file\n" {
		t.Errorf("the task's own file next to it came back as %q", got)
	}
}

// The refusal above is right, and the restore that follows it has to ask the
// same question of its own clean. The rule that hid this file belonged to a
// change the task had not committed, so `reset --hard <target>` dropped it —
// and the restore's `reset --hard head` cannot bring it back either, because
// head does not hold it. The restore's clean therefore saw an ordinary untracked
// file, deleted it, and the placement of the task's `.gitignore` a moment later
// hid the hole: the error said the worktree had been put back the way it was
// found, and the file the advance had just refused over was gone.
//
// What it pins is the outcome, not the mechanism: the file is still there, byte
// for byte, and the report says it was left rather than deleted. A restore that
// deletes it still fails the first check; one that deletes it and reports the
// tree put back fails both.
func TestTheRestoreKeepsAFileTheResetsOwnIgnoreRuleHid(t *testing.T) {
	f := newRebaselineFixture(t, "T9039")
	// The ignore file has to be TRACKED for any of this to happen. A rule the
	// task adds to an untracked `.gitignore` survives both resets, so the clean
	// never sees the file it hides and nothing is refused; main's new commit is a
	// fast-forward of the task's branch, which is the ordinary case.
	f.mainAddsItsOwnFileAt(".gitignore")
	f.git(f.worktree, "merge", "--ff-only", "main")
	const secret = "notes.secret"
	const content = "the task's own ignored deliverable\n"
	f.write(".gitignore", "main's own file\n*.secret\n", 0o644)
	f.write(secret, content, 0o644)
	f.mainMovesElsewhere()

	_, err := RebaselineTask(f.root, "T9039", "", "")
	if err == nil {
		t.Fatal("the advance deleted a file it had no copy of and reported success")
	}
	msg := err.Error()
	if !strings.Contains(msg, "would delete "+secret) {
		t.Errorf("the refusal this test is about is not the one that reached it: %v", err)
	}
	if !strings.Contains(msg, "left "+secret+" in place") {
		t.Errorf("the report does not name the file the restore left behind, so a reader believes the tree is as it was found: %v", err)
	}
	got, rerr := os.ReadFile(filepath.Join(f.worktree, secret))
	if rerr != nil {
		t.Fatalf("the restore deleted the file the advance refused over, and says the tree was put back: %v", rerr)
	}
	if string(got) != content {
		t.Errorf("the file the advance refused to delete holds %q, want %q", got, content)
	}
	if got := readFileOrFail(t, filepath.Join(f.worktree, ".gitignore")); !strings.Contains(got, "*.secret") {
		t.Errorf("the task's ignore rule did not come back, so the tree is not the one the restore found: %q", got)
	}
}

// The same directory can reach the snapshot under two spellings, and only one
// of them is a path. `git ls-files --others` names an untracked DIRECTORY with a
// trailing slash — which is what it does for a nested repository — while every
// other listing names the same directory without one, so a nested repository
// standing where main has a FILE arrived twice: once from the obstruction the
// reset clears, once from the listing. Nothing was lost (the restore treats both
// idempotently), but the manifest named the directory on two lines and Files
// counted the advance as having carried two paths where it carried one — and
// Files is len(entries), which is the number this test counts.
//
// A trailing slash is the whole of the difference, and it is never part of a
// path: the slash is the separator. So the assertion is not about this fixture
// alone — no entry and no manifest line may end in one.
func TestTheSnapshotSpellsANestedRepositoryOneWay(t *testing.T) {
	f := newRebaselineFixture(t, "T9040")
	target := f.mainAddsItsOwnFileAt("scratch/inner")
	nested := filepath.Join(f.worktree, "scratch", "inner")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	f.git(nested, "init", "-q", ".")
	// A file inside the nested repository, and one beside it. Neither can be
	// written by a patch, both are inside a directory the reset clears whole.
	f.git(nested, "add", "-A")
	if err := os.WriteFile(filepath.Join(nested, "notes.txt"), []byte("the task's own file\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	paths, err := worktreeChangedPaths(f.worktree, f.baseline)
	if err != nil {
		t.Fatal(err)
	}
	head := f.git(f.worktree, "rev-parse", "HEAD")
	keep := t.TempDir()
	entries, err := snapshotWorktree(f.worktree, head, target, paths, keep)
	if err != nil {
		t.Fatalf("the snapshot refused a tree it has to be able to record: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the snapshot recorded nothing, so this test is not the shape it is about")
	}
	// Counted by the path, not by the spelling: a trailing slash is the only
	// difference between the two, so trimming it here is what the test means by
	// "the same directory" — the production function is deliberately not used, so
	// that a wrong answer in it cannot move the line this test draws.
	distinct := map[string]bool{}
	for _, e := range entries {
		distinct[strings.TrimRight(e.Path, "/")] = true
		if strings.HasSuffix(e.Path, "/") {
			t.Errorf("the snapshot holds a path spelled with a trailing slash: %q", e.Path)
		}
	}
	if len(distinct) != len(entries) {
		t.Errorf("the snapshot holds %d entries for %d paths — the same directory under two spellings is carried once:\n%+v", len(entries), len(distinct), entries)
	}
	// The fixture is this shape only if the nested repository is in the snapshot
	// as the directory the reset clears — reached from the obstruction, since the
	// clean skips repositories and the listing names no file inside one.
	found := false
	for _, e := range entries {
		if e.Path == "scratch/inner" && e.State == "dir" {
			found = true
		}
	}
	if !found {
		t.Errorf("the snapshot does not hold scratch/inner as a directory — the fixture is not this test's shape: %+v", entries)
	}
	manifest := readFileOrFail(t, filepath.Join(keep, "MANIFEST.txt"))
	for _, line := range strings.Split(strings.TrimSuffix(manifest, "\n"), "\n") {
		fields := strings.Split(line, "\t")
		p := manifestFieldPath(t, fields)
		if strings.HasSuffix(p, "/") {
			t.Errorf("the manifest names a directory with a trailing slash: %q", line)
		}
	}
	if got := strings.Count(manifest, "\tscratch/inner\n"); got != 1 {
		t.Errorf("the manifest names scratch/inner %d times:\n%s", got, manifest)
	}
}

// manifestFieldPath reads the path back out of a manifest line the way the
// manifest writes it, so the test above compares paths rather than spellings of
// them: the last field is quoted when it holds a quote, a backslash or a control
// character, and written as it is otherwise.
func manifestFieldPath(t *testing.T, fields []string) string {
	t.Helper()
	if len(fields) != 3 {
		t.Fatalf("a manifest line has %d fields, want 3", len(fields))
	}
	if !strings.HasPrefix(fields[2], `"`) {
		return fields[2]
	}
	p, err := strconv.Unquote(fields[2])
	if err != nil {
		t.Fatalf("the manifest's path field cannot be read back: %q", fields[2])
	}
	return p
}

// A directory the advance deletes whole, standing INSIDE another one it deletes
// whole: the outer walk reaches the inner directory's contents before the inner
// one is reached as a directory of its own, and both take it. The walk used to
// append what it found without adding it to the set of what had been taken, so
// the manifest named the same file twice and Files counted the advance as having
// carried two paths where it carried one. The outer directory here is one the
// clean removes; the inner one stands where main put a file.
func TestTheSnapshotRecordsNestedWholeDirectoriesOnce(t *testing.T) {
	f := newRebaselineFixture(t, "T9030")
	target := f.mainAddsItsOwnFileAt("scratch/inner")
	const inner = "scratch/inner/notes.txt"
	f.write(inner, "the task's own file\n", 0o644)

	paths, err := worktreeChangedPaths(f.worktree, f.baseline)
	if err != nil {
		t.Fatal(err)
	}
	head := f.git(f.worktree, "rev-parse", "HEAD")
	keep := t.TempDir()
	entries, err := snapshotWorktree(f.worktree, head, target, paths, keep)
	if err != nil {
		t.Fatalf("the snapshot refused a tree it has to be able to record: %v", err)
	}

	counts := map[string]int{}
	for _, e := range entries {
		counts[e.Path]++
	}
	for p, n := range counts {
		if n != 1 {
			t.Errorf("the snapshot holds %d entries for %s; it is carried once, from whichever walk reaches it first", n, p)
		}
	}
	// The fixture is the shape the test is about only if both directories are
	// there as directories of their own — the outer from the clean, the inner
	// from main's file standing where the task has a directory.
	for _, p := range []string{"scratch", "scratch/inner"} {
		found := false
		for _, e := range entries {
			if e.Path == p && e.State == "dir" {
				found = true
			}
		}
		if !found {
			t.Errorf("the snapshot does not hold %s as a directory — the fixture is not this test's shape", p)
		}
	}
	manifest := readFileOrFail(t, filepath.Join(keep, "MANIFEST.txt"))
	if got := strings.Count(manifest, "\t"+inner+"\n"); got != 1 {
		t.Errorf("the manifest names %s %d times:\n%s", inner, got, manifest)
	}
}

// T9031.
//
// One entry, one line — whatever the name holds. A path with a newline in it was
// written as it is, so one entry produced two lines and a reader counting them
// counted a path the advance never carried. The path is the last field and it is
// escaped by one rule: written as it is when it needs nothing, quoted the way Go
// quotes a string when it holds a quote, a backslash or a control character. The
// two forms cannot be confused, because a quoted one always opens with a quote
// and a name that opens with a quote is always the quoted form.
func TestTheManifestWritesOneEntryToOneLine(t *testing.T) {
	names := []string{
		"docs/task-notes.md",
		"docs/设计.md",
		"a name with spaces.txt",
		"a\nnewline.txt",
		"a\tname.txt",
		`a"quote.txt`,
		`a\backslash.txt`,
	}
	for _, name := range names {
		line := manifestLine(snapshotEntry{Path: name, State: "file", Mode: 0o644})
		if got := strings.Count(line, "\n"); got != 1 || !strings.HasSuffix(line, "\n") {
			t.Errorf("the manifest entry for %q is %d lines, want exactly one: %q", name, got, line)
			continue
		}
		fields := strings.Split(strings.TrimSuffix(line, "\n"), "\t")
		if len(fields) != 3 {
			t.Errorf("the manifest entry for %q has %d fields, want 3: %q", name, len(fields), line)
			continue
		}
		field := fields[2]
		if strings.HasPrefix(field, `"`) {
			back, err := strconv.Unquote(field)
			if err != nil {
				t.Errorf("the manifest entry for %q is quoted in a form that cannot be read back: %q", name, field)
				continue
			}
			if back != name {
				t.Errorf("the manifest entry for %q reads back as %q", name, back)
			}
			continue
		}
		if field != name {
			t.Errorf("the manifest entry for %q is written as %q", name, field)
		}
	}
}

// T9034.
//
// The names of what a clean removes, read off a real tree. Everything here is a
// name git cannot write as it is — a newline, a quote, a backslash, a space, a
// character outside ASCII — and the answer has to be the names on disk, because
// they are what the walk and the restore go on to use. The nested repository is
// the skip arm: git leaves it and everything around it alone, so the directory
// holding it is not a removal and never appears.
func TestTheCleanReachIsTheNamesOnDisk(t *testing.T) {
	root := t.TempDir()
	gitIn(t, root, "init", "-q", ".")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("tracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-q", "-m", "base")

	odd := []string{"new\nline", `quote"dir`, `back\slash`, "sp ace", "naïve"}
	for _, name := range odd {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A repository of its own inside a directory that would otherwise be removed
	// whole, with an untracked file beside it.
	if err := os.MkdirAll(filepath.Join(root, "e1", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "e1", "u.txt"), []byte("u\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, filepath.Join(root, "e1", "sub"), "init", "-q")
	// Files whose names are quoted too, standing where they are files rather than
	// directories: the quoting is the same and the answer has to come back the
	// other way round, or every quoted name would be read as a directory.
	quoted := []string{"a\nb.txt", `q"f.txt`, `b\f.txt`}
	for _, name := range quoted {
		if err := os.WriteFile(filepath.Join(root, name), []byte("y\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	dirs, files, err := cleanReach(root)
	if err != nil {
		t.Fatalf("cleanReach refused a tree it has to be able to read: %v", err)
	}
	for _, name := range odd {
		if !dirs[name] {
			t.Errorf("cleanReach does not name %q, which the clean removes whole (dirs: %v)", name, keysOf(dirs))
		}
	}
	for _, name := range quoted {
		if dirs[name] {
			t.Errorf("cleanReach reads %q as a directory; it is a file with a name git cannot write as it is", name)
		}
	}
	if dirs["e1"] || dirs["e1/sub"] {
		t.Errorf("cleanReach names %v, which git skips — a repository inside the tree is left alone", keysOf(dirs))
	}
	want := append([]string{"e1/u.txt"}, quoted...)
	sort.Strings(want)
	if len(files) != len(want) {
		t.Fatalf("cleanReach names the files %v, want %v", files, want)
	}
	sort.Strings(files)
	for i := range want {
		if files[i] != want[i] {
			t.Errorf("cleanReach names the files %v, want %v", files, want)
			break
		}
	}
}

// T9041.
//
// What the ask names is what the act removes — measured, in both directions.
// The dry run's answer IS the snapshot's reach, which is why the two command
// lines are built from one constant; the constant is a claim about the code, and
// this is the measurement behind it. A flag the dry run carried alone would
// refuse an advance over work that was never at risk; a flag the act carried
// alone would delete what nothing recorded, which is the defect this file exists
// to prevent. The tree holds the shapes where the two answers could plausibly
// differ: an untracked file standing BESIDE an ignored one, a directory holding
// only an ignored file, an empty directory, a directory holding a fifo, a nested
// repository, and a bare fifo.
func TestTheCleanRemovesWhatTheDryRunNames(t *testing.T) {
	root := t.TempDir()
	gitIn(t, root, "init", "-q", ".")
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fifo := func(rel string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitignore", "*.ign\n")
	write("plain/f.txt", "untracked, in a directory it is the only thing in\n")
	write("mixed/u.txt", "untracked, standing beside an ignored file\n")
	write("mixed/i.ign", "ignored\n")
	write("ignonly/i.ign", "ignored, and alone — the clean cannot empty this directory\n")
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	fifo("withfifo/pipe")
	fifo("bare-pipe")
	write("repo/inner.txt", "tracked by the repository inside\n")
	gitIn(t, filepath.Join(root, "repo"), "init", "-q", ".")
	gitIn(t, filepath.Join(root, "repo"), "add", "-A")
	gitIn(t, filepath.Join(root, "repo"), "commit", "-q", "-m", "inner")
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-q", "-m", "base")

	before := walkPaths(t, root)
	for _, p := range []string{"mixed/i.ign", "ignonly/i.ign", "empty", "withfifo/pipe", "bare-pipe", "repo/inner.txt"} {
		if !before[p] {
			t.Fatalf("the fixture does not hold %s, so it is not the shape this test is about: %v", p, keysOf(before))
		}
	}
	dirs, files, err := cleanReach(root)
	if err != nil {
		t.Fatalf("cleanReach refused a tree it has to be able to read: %v", err)
	}
	// What the dry run promises: the names themselves, and — for a directory it
	// names whole — everything standing inside it.
	named := map[string]bool{}
	for _, p := range append(keysOf(dirs), files...) {
		named[p] = true
		for _, q := range keysOf(before) {
			if strings.HasPrefix(q, p+"/") {
				named[q] = true
			}
		}
	}
	if _, err := gitOutput(root, cleanArgs(false)...); err != nil {
		t.Fatalf("running the clean the advance runs: %v", err)
	}
	after := walkPaths(t, root)
	for _, p := range keysOf(named) {
		if after[p] {
			t.Errorf("the dry run names %s and the clean left it in place; an advance would be refused over work that is not at risk", p)
		}
	}
	for _, p := range keysOf(before) {
		if !after[p] && !named[p] {
			t.Errorf("the clean removed %s, which the dry run never named; the snapshot holds no copy of it", p)
		}
	}
}

// walkPaths is every path under root, in slash form, as a set. `.git` is skipped
// — reading it is not this test's business, and the fixture has two of them.
func walkPaths(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := filepath.WalkDir(root, func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, full)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() && d.Name() == ".git" {
			return fs.SkipDir
		}
		out[rel] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// T9042.
//
// Every clean the advance runs comes from one constant, and what that constant
// holds is the flags that decide WHAT is removed. The three command lines this
// file builds — the ask, the act, and the restore's own act — have to differ in
// nothing else. That is the property `cleanWhat` buys, and it is not visible in
// any one of the three: `-x` (or `-e`, or dropping `-d`) on the ask alone
// refuses an advance over work that was never at risk, and on an act alone
// deletes what nothing recorded. Nothing else in this suite would notice, since
// each shape the suite builds is fed to whichever of the three the path under
// test happens to run.
func TestTheThreeCleanCommandLinesDifferOnlyInDoingIt(t *testing.T) {
	// The flags that decide what is removed: everything on the command line
	// except the ones that are about doing it rather than about what it does —
	// -n asks instead of acting, -f refuses to ask whether to act, -q says less.
	what := func(args []string) string {
		t.Helper()
		if len(args) < 2 || args[0] != "clean" {
			t.Fatalf("a clean command line that does not start with `git clean`: %q", args)
		}
		var b strings.Builder
		for _, a := range args[1:] {
			if a == "--" {
				break
			}
			if !strings.HasPrefix(a, "-") {
				t.Fatalf("%q is not a flag, and it comes before the end of the flags in %q", a, args)
			}
			for _, r := range strings.TrimPrefix(a, "-") {
				switch r {
				case 'n', 'f', 'q':
				default:
					b.WriteRune(r)
				}
			}
		}
		return b.String()
	}
	ask, act := what(cleanArgs(true)), what(cleanArgs(false))
	if ask != act {
		t.Errorf("the ask is `%s` and the act is `%s`: a flag about what is removed that only one of them has is either a deletion nothing recorded or a refusal over work that is not at risk", ask, act)
	}
	if ask == "" {
		t.Errorf("the two command lines share no flag about what is removed, so this test would pass on any pair of them")
	}
	if got := what(restoreCleanArgs([]string{"a"})); got != act {
		t.Errorf("the restore's clean is `%s` and the advance's act is `%s`", got, act)
	}
	// And what it was given is named literally: a path on disk may hold `[`, `*`
	// or `?`, which a pathspec would read as a pattern and match some OTHER name
	// with.
	want := []string{"clean", "-" + "f" + cleanWhat + "q", "--", ":(literal)a.txt", ":(literal)na[me].txt"}
	if got := restoreCleanArgs([]string{"a.txt", "na[me].txt"}); !slices.Equal(got, want) {
		t.Errorf("the restore's clean is %q, want %q", got, want)
	}
}

// T9035.
//
// The two sentences git says about a clean, and the refusal for anything else.
// The refusal is the point: a line this does not understand is a clean whose reach
// is unknown, and an unknown reach is the defect this exists to close — so the
// parser fails closed, on the line, before anything has been touched.
func TestTheCleanLineIsReadOnlyInTheTwoFormsGitWrites(t *testing.T) {
	for _, c := range []struct {
		line  string
		path  string
		isDir bool
		skip  bool
		bad   bool
	}{
		{line: "Would remove docs/x.md", path: "docs/x.md"},
		{line: "Would remove docs/", path: "docs", isDir: true},
		{line: "Would remove sp ace/", path: "sp ace", isDir: true},
		// The trailing slash is INSIDE the quotes when the name is quoted. That
		// is what git writes — measured against `git clean -nd` on a real tree
		// rather than taken from the documentation — and reading it after the
		// name is decoded is the only way these lines come out as directories.
		{line: `Would remove "new\nline/"`, path: "new\nline", isDir: true},
		{line: `Would remove "quote\"dir/"`, path: `quote"dir`, isDir: true},
		{line: `Would remove "back\\slash/"`, path: `back\slash`, isDir: true},
		{line: `Would remove "na\303\257ve/"`, path: "naïve", isDir: true},
		{line: `Would remove "tab\tdir/"`, path: "tab\tdir", isDir: true},
		{line: `Would remove "a\nb.txt"`, path: "a\nb.txt"},
		// A slash OUTSIDE the quotes on a quoted name is refused, not read: git
		// does not write it, and a parser that guesses at a line it has never
		// seen is the parser that fails to notice the day git starts writing
		// something else. Refusing means the advance stops and says so.
		{line: `Would remove "back\\slash"/`, bad: true},
		{line: "Would skip repository e1/sub", skip: true},
		{line: "Would remove", bad: true},
		{line: "Removing docs/x.md", bad: true},
		{line: `Would remove "unterminated`, bad: true},
		{line: `Would remove "bad\qescape"/`, bad: true},
		{line: `Would remove "half\`, bad: true},
	} {
		p, isDir, skip, err := parseCleanLine(c.line)
		if c.bad {
			if err == nil {
				t.Errorf("parseCleanLine(%q) read %q out of a line it does not understand", c.line, p)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseCleanLine(%q) failed: %v", c.line, err)
			continue
		}
		if p != c.path || isDir != c.isDir || skip != c.skip {
			t.Errorf("parseCleanLine(%q) = (%q, %v, %v), want (%q, %v, %v)", c.line, p, isDir, skip, c.path, c.isDir, c.skip)
		}
	}
}

// The snapshot's reach is taken one reset before the clean runs, and a reset moves
// what a clean can see. A directory is removable only while it holds nothing
// tracked, so the one whose only tracked file main has deleted becomes removable
// the moment the reset runs — and a fifo inside it was in neither path list: git
// cannot name a fifo, and the directory was not removable when the lists were
// taken. Without asking again the clean deleted the fifo and the advance reported
// success. The refusal comes before the clean, and the tree comes back whole,
// which is what makes it a refusal rather than a loss.
func TestRebaselineAsksTheCleanAgainWhereItRuns(t *testing.T) {
	f := newRebaselineFixture(t, "T9032")
	f.mainDeletes("internal/legacy/old.txt")
	pipe := filepath.Join(f.worktree, "internal", "legacy", "pipe")
	if err := syscall.Mkfifo(pipe, 0o644); err != nil {
		t.Fatalf("making the fifo the fixture is about: %v", err)
	}
	before := f.treeDump()

	_, err := RebaselineTask(f.root, "T9032", "", "")
	if err == nil {
		t.Fatal("the clean deleted the task's fifo and the advance reported success")
	}
	if msg := err.Error(); !strings.Contains(msg, "would delete internal/legacy") {
		t.Fatalf("a different refusal reached this test: %v", err)
	}
	st, err := os.Lstat(pipe)
	if err != nil {
		t.Fatalf("the fifo is gone after the refusal: %v", err)
	}
	if st.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("internal/legacy/pipe came back as %v", st.Mode())
	}
	if after := f.treeDump(); after != before {
		t.Errorf("the refused advance did not put the tree back the way it was found:\n--- as found ---\n%s\n--- after ---\n%s", before, after)
	}
}

// A socket is the kind the advance cannot carry across at all: it is a rendezvous
// rather than content, and a copy of one is not a socket. Standing in a plain
// untracked directory — where git names the directory and nothing inside it — it
// used to be deleted by the clean with no refusal and no record. The reach now
// finds it, and the refusal is the one the snapshot already makes for that kind:
// before the reset, with "Nothing has been touched" in the message and true.
func TestTheSnapshotRefusesASocketInAPlainUntrackedDirectory(t *testing.T) {
	f := newRebaselineFixture(t, "T9033")
	f.mainMovesElsewhere()
	dir := filepath.Join(f.worktree, "sockdir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "s")
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("making the socket the fixture is about: %v", err)
	}
	defer func() { _ = syscall.Close(fd) }()
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: sock}); err != nil {
		t.Fatalf("binding %s: %v", sock, err)
	}
	beforeHead := f.git(f.worktree, "rev-parse", "HEAD")
	before := f.treeDump()

	_, err = RebaselineTask(f.root, "T9033", "", "")
	if err == nil {
		t.Fatal("a socket was carried across as something it is not")
	}
	for _, want := range []string{"sockdir/s is a socket", "Nothing has been touched"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if after := f.treeDump(); after != before {
		t.Errorf("the snapshot changed the tree it refused to advance:\n--- as found ---\n%s\n--- after ---\n%s", before, after)
	}
	if head := f.git(f.worktree, "rev-parse", "HEAD"); head != beforeHead {
		t.Errorf("the refused advance moved the branch to %s", head)
	}
}

// gitIn runs git in a directory of a test's own making, for the tests that need a
// tree rather than the full fixture.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// T9037.
//
// The locale the clean's answer is read in. The answer is two English sentences
// and the parser reads them as written, so git has to be asked in a locale that
// writes them that way — and the one that arrives with the test is not it, which
// is the point: an LC_ALL appended to an inherited locale variable would be read
// by nobody, because getenv answers with the FIRST match. The variables that
// decide the locale are therefore dropped before LC_ALL=C is added, and this is
// the test of that rule. It is a unit test because it cannot be an end-to-end one
// here: the machine has no second locale installed, so a clean run under an
// inherited zh_CN.UTF-8 answers in English anyway and the difference would be
// invisible in the output.
func TestTheCleanReadsTheLocaleItCanReadTheAnswerIn(t *testing.T) {
	t.Setenv("LANG", "fr_FR.UTF-8")
	t.Setenv("LANGUAGE", "fr:de")
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	t.Setenv("LC_MESSAGES", "ja_JP.UTF-8")
	t.Setenv("POST_KEEP_ME", "yes")

	env := cleanEnv()
	var locales []string
	kept := false
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		switch {
		case name == "LANG" || name == "LANGUAGE" || strings.HasPrefix(name, "LC_"):
			locales = append(locales, kv)
		case name == "POST_KEEP_ME":
			kept = true
		}
	}
	if len(locales) != 1 || locales[0] != "LC_ALL=C" {
		t.Errorf("the clean would run with the locale variables %v, want only LC_ALL=C: the answer is read in English, and an inherited locale would be the one read", locales)
	}
	if !kept {
		t.Error("the environment the clean runs in is not the one it was given, minus the locale variables")
	}
}

// T9038.
//
// The record of what is inside a directory that was walked WHOLE is the
// directory's own entry: the walk under it records every path beneath it, so a
// path arriving later — from the clean's second answer, or from anywhere else —
// is held by the ancestor even though it is not an entry of its own. Nothing
// end-to-end reaches this today (see the function's own note); it is tested here
// so that the arm is a claim with a test rather than a line nothing exercises.
func TestTheSnapshotHoldsWhatIsInsideADirectoryItWalked(t *testing.T) {
	held := map[string]bool{"scratch": true, "docs/a.md": true, "internal": true}
	for _, c := range []struct {
		path string
		want bool
	}{
		{path: "scratch/pipe", want: true},
		{path: "scratch/deep/inside.txt", want: true},
		{path: "internal/legacy", want: true},
		{path: "internal/legacy/old.txt", want: true},
		// A path that IS an entry is the caller's first question, not this one:
		// this arm answers only for what the ancestor's entry stands for.
		{path: "docs/a.md", want: false},
		{path: "docs/b.md", want: false},
		{path: "scratchpad", want: false},
		{path: "docs", want: false},
		{path: "", want: false},
	} {
		if got := heldByAnAncestor(held, c.path); got != c.want {
			t.Errorf("heldByAnAncestor(%q) = %v, want %v — a directory that was walked holds everything under it, and nothing else is held", c.path, got, c.want)
		}
	}
}

// Two names that are not lines: one with a newline in it, one that begins with a
// space. The path lists are read with -z for this — a NUL-separated field is the
// whole name whatever is in it, and a line is not. Read as lines, the first name
// becomes two paths that exist nowhere, and the second loses its leading space to
// the trim that a line-based read needs; both then travel as names of files that
// were never there, and the work they stood for is not carried.
func TestThePathListKeepsNamesThatAreNotLines(t *testing.T) {
	f := newRebaselineFixture(t, "T9036")
	const newline = "docs/a\nb.md"
	const spaced = " docs/space.md"
	f.write(newline, "one name\n", 0o644)
	f.write(spaced, "another name\n", 0o644)

	paths, err := worktreeChangedPaths(f.worktree, f.baseline)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{newline, spaced} {
		if !paths[p] {
			t.Errorf("the path list does not name %q, which is a file in the worktree with work in it", p)
		}
	}
	// The fragments a line-based read produces instead. Naming one of these is
	// naming a path that is not there.
	for _, p := range []string{"docs/a", "b.md", "docs/space.md"} {
		if paths[p] {
			t.Errorf("the path list names %q, which no file answers to — the listing was read as lines", p)
		}
	}
}

// T9043.
//
// The restore's clean is limited to the paths the snapshot holds, and a directory
// the snapshot walked is held as ONE name plus one entry per path inside it — while
// `git clean -fd` removes a directory whole. So the directory's own name standing
// in the held set was not the claim the clean needed: it needed the contents. A
// file written into one of those directories after the snapshot was taken is in no
// entry, and it rode out on the removal unnamed, with the error saying the worktree
// was put back the way it was found — a deletion reported as a restoration.
func TestTheRestoreKeepsAFileWrittenInsideADirectoryItRemovesWhole(t *testing.T) {
	f := newRebaselineFixture(t, "T9043")
	f.write("gathered/first.txt", "the task gathered this\n", 0o644)
	f.mainRewritesTheSameRegion() // the task's patch no longer applies, so the restore runs

	const late = "written into a directory the clean removes whole\n"
	real := restore
	restore = func(worktree, head, keep string, entries []snapshotEntry) ([]string, error) {
		// The advance's own clean has already taken `gathered` and put nothing
		// back, so the directory the restore is about to decide over does not exist
		// yet: this writes it, and a file in it, in the window between the snapshot
		// and the clean the restore asks about — which is exactly the window a
		// Worker's own process, or anything else on the machine, can write in.
		if err := os.MkdirAll(filepath.Join(worktree, "gathered"), 0o755); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(filepath.Join(worktree, "gathered", "late.txt"), []byte(late), 0o644); err != nil {
			t.Error(err)
		}
		return real(worktree, head, keep, entries)
	}
	defer func() { restore = real }()

	before := f.pathsAndContents(f.baseline)
	_, err := RebaselineTask(f.root, "T9043", "", "")
	if err == nil {
		t.Fatal("the advance was not refused")
	}
	msg := err.Error()
	// The refusal has to be the apply's, or the restore never ran and the rest of
	// this test is about nothing.
	if !strings.Contains(msg, "patch does not apply") {
		t.Fatalf("the refusal this test needs is the apply's, so that the restore ran at all: %s", msg)
	}
	if !strings.Contains(msg, "left gathered/late.txt in place") {
		t.Errorf("the restore did not say it left the file, so a reader believes the tree is as it was found: %s", msg)
	}
	if got := readFileOrFail(t, filepath.Join(f.worktree, "gathered", "late.txt")); got != late {
		t.Errorf("the file the restore left in place holds %q, want %q", got, late)
	}
	if got := readFileOrFail(t, filepath.Join(f.worktree, "gathered", "first.txt")); got != "the task gathered this\n" {
		t.Errorf("the task's own file in that directory did not come back: %q", got)
	}
	// Everything else is as it was found, and the one line the fingerprint gained
	// is the file the restore deliberately left. Leaving it is the point; stopping
	// halfway through the tree is not, so nothing else may differ.
	after := f.pathsAndContents(f.baseline)
	leftLine := f.describe("gathered/late.txt")
	if !strings.Contains(after, leftLine) {
		t.Fatalf("the fingerprint of the restored tree does not hold the file that was left: %s", after)
	}
	if rest := strings.Replace(after, leftLine, "", 1); rest != before {
		t.Errorf("the refused advance changed the tree it refused to advance:\n--- as found ---\n%s\n--- after ---\n%s", before, after)
	}
}

// T9044.
//
// The same rule at the point where nothing has been deleted yet. The snapshot's
// reach was computed one reset ago, so the advance asks the clean again where it is
// about to run — and a directory the snapshot walked is one whose every path it
// holds, except the ones that arrived in between. Refusing here costs a dispatch;
// not refusing costs the file, and the tree it was in.
func TestTheAdvanceRefusesRatherThanCleanADirectoryItHasNoCopyOf(t *testing.T) {
	f := newRebaselineFixture(t, "T9044")
	f.write("gathered/first.txt", "the task gathered this\n", 0o644)
	f.mainRewritesTheSameRegion()

	const late = "arrived after the snapshot was taken\n"
	real := snapshot
	snapshot = func(worktree, head, target string, paths map[string]bool, keep string) ([]snapshotEntry, error) {
		entries, err := real(worktree, head, target, paths, keep)
		if err != nil {
			return nil, err
		}
		// AFTER the walk, so the file is in no entry and in no kept copy — which is
		// what makes the clean that follows unsafe for it rather than merely
		// unrecorded.
		if err := os.WriteFile(filepath.Join(worktree, "gathered", "late.txt"), []byte(late), 0o644); err != nil {
			t.Error(err)
		}
		return entries, nil
	}
	defer func() { snapshot = real }()

	before := f.pathsAndContents(f.baseline)
	_, err := RebaselineTask(f.root, "T9044", "", "")
	if err == nil {
		t.Fatal("the advance deleted a file it had no copy of and reported success")
	}
	msg := err.Error()
	if !strings.Contains(msg, "would delete the directory gathered") || !strings.Contains(msg, "gathered/late.txt") {
		t.Fatalf("the refusal this test is about is not the one that reached it: %s", msg)
	}
	// Refused BEFORE the clean: the file is still there with its bytes. A refusal
	// that arrives after the deletion names a path it has already taken.
	if got := readFileOrFail(t, filepath.Join(f.worktree, "gathered", "late.txt")); got != late {
		t.Errorf("the file the advance refused over holds %q, want %q", got, late)
	}
	if got := readFileOrFail(t, filepath.Join(f.worktree, "gathered", "first.txt")); got != "the task gathered this\n" {
		t.Errorf("the task's own file in that directory did not survive the refusal: %q", got)
	}
	after := f.pathsAndContents(f.baseline)
	lateLine := f.describe("gathered/late.txt")
	if !strings.Contains(after, lateLine) {
		t.Fatalf("the fingerprint of the tree the refusal left does not hold the file it refused over: %s", after)
	}
	if rest := strings.Replace(after, lateLine, "", 1); rest != before {
		t.Errorf("the refusal changed the tree it refused to advance:\n--- as found ---\n%s\n--- after ---\n%s", before, after)
	}
}

// T9045 (batching, as a partition).
//
// The restore's clean carries one pathspec per path, and a worktree holding about
// 80,000 untracked paths made that one argument list longer than the kernel
// accepts: fork/exec failed with E2BIG on a tree this code restores, and the
// refusal it produced said the worktree could NOT be put back where nothing had
// been lost. The paths now go out in batches, and what has to stay true of them is
// that the batches are the list: in order, once each, and nothing dropped for being
// too big to pair up.
func TestTheCleanBatchesPartitionThePathsInOrder(t *testing.T) {
	prev := restoreCleanBatchBytes
	restoreCleanBatchBytes = 40
	defer func() { restoreCleanBatchBytes = prev }()

	const budget = 40
	cost := func(batch []string) int {
		n := 0
		for _, p := range batch {
			n += len(p) + len(":(literal)") + 1
		}
		return n
	}
	paths := []string{"a", "bb", "ccc", "dddd", "eeeee"}
	batches := restoreCleanBatches(paths)
	if len(batches) < 2 {
		t.Fatalf("a list of %d bytes went out in %d batch(es): the split this test is about did not happen", cost(paths), len(batches))
	}
	var flat []string
	for _, b := range batches {
		if len(b) == 0 {
			t.Errorf("an empty batch: a clean invocation that removes nothing, for no reason")
		}
		// A batch over the budget is allowed only when it is one path, which cannot
		// be made smaller — and then it still has to go out.
		if c := cost(b); c > budget && len(b) != 1 {
			t.Errorf("a batch of %d paths costs %d bytes, over the %d it was split by: %v", len(b), c, budget, b)
		}
		flat = append(flat, b...)
	}
	if !slices.Equal(flat, paths) {
		t.Errorf("the batches read %v, want the list they partition, in order and once each: %v", flat, paths)
	}

	// A path longer than the whole budget. Dropping it deletes nothing and says the
	// worktree was put back; a loop that never bounds itself is the E2BIG this
	// exists to avoid. Neither is what one long name should produce.
	long := strings.Repeat("x", 200)
	if got := restoreCleanBatches([]string{long}); len(got) != 1 || !slices.Equal(got[0], []string{long}) {
		t.Errorf("one path over the budget became %v, want it alone in one batch", got)
	}
	if got := restoreCleanBatches(nil); len(got) != 0 {
		t.Errorf("no paths became %v, want no invocations at all", got)
	}
}

// T9046 (batching, end to end).
//
// The partition is not the claim; the claim is that batching changes nothing about
// what the restore removes. So the same refused advance is run twice — once with
// the real budget, once with one the fixture's own paths cross — and the two
// sentences are compared: the paths the clean was given, in order, and the tree the
// restore produced. Without the seam the second half would need a tree of 80,000
// paths, which is the one shape a test cannot carry.
func TestTheRestoreCleanIsBatchedWithoutChangingWhatItRemoves(t *testing.T) {
	// Long enough that a small budget splits between them, and in a TRACKED
	// directory, so git names each one individually rather than collapsing the
	// directory to a single name: the batching is about the length of the list.
	const gathered = 12

	run := func(taskID string, budget int) (before, after string, invocations [][]string) {
		t.Helper()
		prevBudget := restoreCleanBatchBytes
		restoreCleanBatchBytes = budget
		defer func() { restoreCleanBatchBytes = prevBudget }()
		real := restoreClean
		restoreClean = func(worktree string, args ...string) (string, error) {
			invocations = append(invocations, slices.Clone(args))
			return real(worktree, args...)
		}
		defer func() { restoreClean = real }()

		f := newRebaselineFixture(t, taskID)
		for i := 0; i < gathered; i++ {
			f.write(fmt.Sprintf("scripts/gathered-%02d.txt", i), "one more path\n", 0o644)
		}
		f.mainRewritesTheSameRegion()

		// The advance's own clean has already taken every untracked path in this
		// tree, so by the time the restore runs, its clean has nothing left to
		// remove and the list this test is about would be empty — which is what the
		// first version of this test found. Writing them back asks the restore's
		// clean the question it is about: a list of held paths, long enough that the
		// budget decides how it goes out. It is the same window T9043 covers, a path
		// present at the snapshot and at the restore and not in between, with the
		// paths coming back recorded rather than unrecorded.
		realRestore := restore
		restore = func(worktree, head, keep string, entries []snapshotEntry) ([]string, error) {
			for i := 0; i < gathered; i++ {
				p := filepath.Join(worktree, fmt.Sprintf("scripts/gathered-%02d.txt", i))
				if err := os.WriteFile(p, []byte("one more path\n"), 0o644); err != nil {
					t.Error(err)
				}
			}
			return realRestore(worktree, head, keep, entries)
		}
		defer func() { restore = realRestore }()

		before = f.pathsAndContents(f.baseline)
		if _, err := RebaselineTask(f.root, taskID, "", ""); err == nil {
			t.Fatalf("%s: the advance was not refused", taskID)
		} else if msg := err.Error(); !strings.Contains(msg, "patch does not apply") {
			t.Fatalf("%s: the refusal this test needs is the apply's, so that the restore ran at all: %s", taskID, msg)
		}
		after = f.pathsAndContents(f.baseline)
		return
	}

	beforeOne, afterOne, one := run("T9046A", 64<<10)
	beforeMany, afterMany, many := run("T9046B", 100)

	// The outcome batching must not change: the tree the advance was refused on.
	if beforeOne != afterOne {
		t.Errorf("with one invocation the restore did not reproduce the tree:\n--- as found ---\n%s\n--- after ---\n%s", beforeOne, afterOne)
	}
	if beforeMany != afterMany {
		t.Errorf("with the paths batched the restore did not reproduce the tree:\n--- as found ---\n%s\n--- after ---\n%s", beforeMany, afterMany)
	}
	if beforeOne != beforeMany {
		t.Fatalf("the two runs did not start from the same tree, so comparing what they removed says nothing")
	}
	if afterOne != afterMany {
		t.Errorf("the batched restore produced a different tree than the single invocation it replaced:\n--- one ---\n%s\n--- many ---\n%s", afterOne, afterMany)
	}

	// The paths an invocation was given, which are the clean's own flags followed by
	// `--` and one `:(literal)` pathspec each.
	pathsOf := func(args []string) []string {
		t.Helper()
		i := slices.Index(args, "--")
		if i < 0 {
			t.Fatalf("a restore clean with no `--` before the paths: %q", args)
		}
		if want := restoreCleanArgs(nil); !slices.Equal(args[:i+1], want) {
			t.Errorf("a batched invocation is not the clean whose reach was asked: %q, want it to open with %q", args, want)
		}
		out := make([]string, 0, len(args)-i-1)
		for _, a := range args[i+1:] {
			if !strings.HasPrefix(a, ":(literal)") {
				t.Fatalf("%q is not a literal pathspec, so a name holding `*` or `[` would match some other file: %q", a, args)
			}
			out = append(out, strings.TrimPrefix(a, ":(literal)"))
		}
		return out
	}

	if len(one) != 1 {
		t.Fatalf("with the whole list inside the budget the restore ran the clean %d times, want exactly 1: %v", len(one), one)
	}
	want := pathsOf(one[0])
	// A list short enough to fit anywhere would make the split below true for a
	// reason that has nothing to do with the budget.
	if len(want) < gathered {
		t.Fatalf("only %d paths reached the restore's clean, fewer than the %d this fixture wrote: %v", len(want), gathered, want)
	}
	if len(many) < 2 {
		t.Fatalf("with a 100-byte budget the restore still ran the clean %d time(s): the paths went out in one argument list: %v", len(many), many)
	}
	var got []string
	for _, args := range many {
		got = append(got, pathsOf(args)...)
	}
	if !slices.Equal(got, want) {
		t.Errorf("the batches carry %v, want the same paths in the same order as the one invocation they replace: %v", got, want)
	}
}

// T9047.
//
// The sentence a refusal ends with, in the numbers it can be read in. One path and
// two are the same code and different English, and a reader told "it" about two
// files goes looking for the one that is not there. A name holding a comma would
// read as two names in a comma-separated list, and the list is cut rather than
// unbounded because it is written into an error a human reads — with the count of
// what was left out, since silence about the rest reads as "eight".
func TestTheLeftInPlaceSentenceReadsForOnePathAndForMany(t *testing.T) {
	one := leftInPlace([]string{"gathered/late.txt"})
	for _, want := range []string{
		"left gathered/late.txt in place rather than delete it with nothing to write back",
		"the snapshot holds no copy of it ",
		"it is still where the task left it",
	} {
		if !strings.Contains(one, want) {
			t.Errorf("the one-path sentence is missing %q: %s", want, one)
		}
	}
	for _, wrong := range []string{"delete them", "no copy of them", "they are still where"} {
		if strings.Contains(one, wrong) {
			t.Errorf("the one-path sentence says %q, which reads as more than one path: %s", wrong, one)
		}
	}

	two := leftInPlace([]string{"gathered/late.txt", "docs/a,b.md"})
	for _, want := range []string{
		`left gathered/late.txt, "docs/a,b.md" in place rather than delete them with nothing to write back`,
		"the snapshot holds no copy of them ",
		"they are still where the task left them",
	} {
		if !strings.Contains(two, want) {
			t.Errorf("the two-path sentence is missing %q: %s", want, two)
		}
	}
	for _, wrong := range []string{"delete it with", "no copy of it ", "it is still where"} {
		if strings.Contains(two, wrong) {
			t.Errorf("the two-path sentence says %q, which reads as one path: %s", wrong, two)
		}
	}

	// The comma is not decoration: without the quote this reads as three names, and
	// the third — `b.md` — is a path that is not there.
	if !strings.Contains(named([]string{"docs/a,b.md"}), `"docs/a,b.md"`) {
		t.Errorf("a name holding a comma was written as it is, so the list names paths that do not exist: %s", named([]string{"docs/a,b.md"}))
	}

	// More names than a reader will take in. Both halves matter: the eight, and the
	// count that says the list is not the whole of it.
	var many []string
	for i := 0; i < 17; i++ {
		many = append(many, fmt.Sprintf("docs/p%02d.txt", i))
	}
	got := named(many)
	if !strings.HasSuffix(got, " (and 9 more)") {
		t.Errorf("17 paths were listed as %q, want a count of the rest", got)
	}
	if n := strings.Count(got, "docs/p"); n != 8 {
		t.Errorf("named() listed %d paths of 17: %s", n, got)
	}
	if !strings.Contains(got, "docs/p07.txt") || strings.Contains(got, "docs/p08.txt") {
		t.Errorf("the cut is not the first eight in order: %s", got)
	}
}

// T9048.
//
// The same rule where the CLEAN names the file itself, which is the shape that
// makes "an ancestor of this path is held" the wrong question. A directory git
// cannot name whole is named file by file, and the reason is the ordinary one: it
// holds an ignore rule of its own, so the ignored file inside it must stay and git
// removes the rest one path at a time. The directory is held all the same — an
// obstruction the target's own commit put there holds it contents and all — and a
// file that arrived after the snapshot is inside a held directory and in no entry.
// Leaving it costs a re-dispatch; deleting it costs the work.
func TestTheRestoreKeepsAFileWrittenIntoADirectoryTheCleanNamesFileByFile(t *testing.T) {
	f := newRebaselineFixture(t, "T9048")
	// The task's own directory, with the task's own file in it.
	f.write("notes/plain.txt", "the task's own file in its own directory\n", 0o644)
	// main has a FILE exactly where the task has that directory: the reset writes
	// it, the directory goes whole, and the snapshot is what holds the contents.
	f.mainAddsItsOwnFileAt("notes")
	f.mainRewritesTheSameRegion() // and the task's patch no longer applies

	const late = "written after the snapshot was taken\n"
	const ignored = "ignored by the rule beside it\n"
	real := restore
	restore = func(worktree, head, keep string, entries []snapshotEntry) ([]string, error) {
		// The target's file stands at `notes` until the restore's own reset takes it
		// away, and the directory has to be there for the clean to name anything
		// inside it — so the tree this describes is the one the clean is about to
		// look at, not the one the reset found.
		notes := filepath.Join(worktree, "notes")
		if err := os.RemoveAll(notes); err != nil {
			t.Error(err)
		}
		if err := os.MkdirAll(notes, 0o755); err != nil {
			t.Error(err)
		}
		for name, content := range map[string]string{
			".gitignore":  "*.secret\n",
			"late.secret": ignored,
			"late.txt":    late,
		} {
			if err := os.WriteFile(filepath.Join(notes, name), []byte(content), 0o644); err != nil {
				t.Error(err)
			}
		}
		return real(worktree, head, keep, entries)
	}
	defer func() { restore = real }()

	before := f.pathsAndContents(f.baseline)
	_, err := RebaselineTask(f.root, "T9048", "", "")
	if err == nil {
		t.Fatal("the advance was not refused")
	}
	msg := err.Error()
	if !strings.Contains(msg, "patch does not apply") {
		t.Fatalf("the refusal this test needs is the apply's, so that the restore ran at all: %s", msg)
	}
	// The clean named the files, not the directory, so this is the file-granularity
	// half of the rule: the directory is held, and what is inside it that arrived
	// afterwards is not.
	if !strings.Contains(msg, "left notes/.gitignore, notes/late.txt in place") {
		t.Errorf("the restore did not name both files it left, so a reader believes the tree is as it was found: %s", msg)
	}
	if got := readFileOrFail(t, filepath.Join(f.worktree, "notes", "late.txt")); got != late {
		t.Errorf("the file the restore left in place holds %q, want %q", got, late)
	}
	// The ignored file is not in the clean's reach at all, and it is still there:
	// the ignore rule the clean obeyed is the one in the directory it was reading.
	if got := readFileOrFail(t, filepath.Join(f.worktree, "notes", "late.secret")); got != ignored {
		t.Errorf("the ignored file beside it holds %q, want %q", got, ignored)
	}
	if got := readFileOrFail(t, filepath.Join(f.worktree, "notes", "plain.txt")); got != "the task's own file in its own directory\n" {
		t.Errorf("the task's own file in that directory did not come back: %q", got)
	}
	// The tree is as it was found, plus exactly the two files the clean would have
	// removed and the restore therefore left. The ignored one is in no listing by
	// construction, which is the whole reason it is still there.
	after := f.pathsAndContents(f.baseline)
	var rest = after
	for _, p := range []string{"notes/.gitignore", "notes/late.txt"} {
		line := f.describe(p)
		if !strings.Contains(rest, line) {
			t.Fatalf("the fingerprint of the restored tree does not hold %s: %s", p, rest)
		}
		rest = strings.Replace(rest, line, "", 1)
	}
	if rest != before {
		t.Errorf("the refused advance changed the tree it refused to advance:\n--- as found ---\n%s\n--- after ---\n%s", before, after)
	}
}
