package devorchestrator

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	// A TRACKED symlink in the baseline, which the task is about to replace with
	// a regular file. That is the shape that makes a restore dangerous: `reset
	// --hard` puts the link back, and writing the task's bytes to that path
	// follows it into whatever it points at — a different file, which the
	// restore would then report as intact.
	if err := os.Symlink("docs/task-notes.md", filepath.Join(f.root, "tracked-link")); err != nil {
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
	if os.IsNotExist(err) {
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
	msg := err.Error()
	if !strings.Contains(msg, "spec_version.py") {
		t.Fatalf("expected the regeneration failure, got: %s", msg)
	}
	// The failure has to be reported as a failure, not as a restore: the two
	// sentences are different claims and only one of them is true here.
	if !strings.Contains(msg, "put back") {
		t.Errorf("the refusal does not say what happened to the worktree: %s", msg)
	}

	// 1. The tree is exactly as it was found, and so is the branch tip. The ref
	//    matters as much as the bytes: gate inputs were written at spawn against
	//    this tip, and a collect compares them.
	if after := f.pathsAndContents(f.baseline); after != before {
		t.Errorf("a failed advance after the rebuild changed the tree:\n--- as found ---\n%s\n--- after ---\n%s", before, after)
	}
	if head := f.git(f.worktree, "rev-parse", "HEAD"); head != beforeHead {
		t.Errorf("the worktree HEAD is at %s after the failure; it was %s — the advance moved it and nothing moved it back", head, beforeHead)
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

	if _, err := RebaselineTask(f.root, "T9005", "", ""); err != nil {
		t.Fatalf("the first advance was refused: %v", err)
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
	restore = func(string, string, string, []snapshotEntry) error {
		return errors.New("the restore could not run at all")
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
