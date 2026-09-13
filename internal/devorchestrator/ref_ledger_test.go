package devorchestrator

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The ledger is the anchor the refs check reads instead of a commit's
// (forgeable) identity. These tests pin the properties that make it an anchor:
// one entry per name, the newest record wins, an unreadable ledger is an ERROR
// rather than an empty one, and adopt reads the ref's real sha.

func TestRefLedgerRecordsAndUpserts(t *testing.T) {
	root := t.TempDir()
	if err := RecordSupervisorRef(root, "refs/heads/task/T0001-x", "aaa", "spawn", "T0001"); err != nil {
		t.Fatal(err)
	}
	// The same ref, moved by a commit: one entry, the newest sha.
	if err := RecordSupervisorRef(root, "refs/heads/task/T0001-x", "bbb", "commit", "T0001"); err != nil {
		t.Fatal(err)
	}
	if err := RecordSupervisorRef(root, "feat/other", "ccc", "adopt", ""); err != nil {
		t.Fatal(err)
	}

	refs, err := ReadSupervisorRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("got %d entries %v, want 2 (one per name)", len(refs), refs)
	}
	got := refs["refs/heads/task/T0001-x"]
	if got.SHA != "bbb" || got.Source != "commit" {
		t.Errorf("entry = %+v, want the newest record (sha bbb, source commit)", got)
	}
	if _, ok := refs["refs/heads/feat/other"]; !ok {
		t.Errorf("a bare branch name was not normalized to refs/heads/: %v", refs)
	}

	ordered, err := ListSupervisorRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(ordered) != 2 || ordered[0].Name != "refs/heads/feat/other" {
		t.Errorf("ListSupervisorRefs = %+v, want name-ordered", ordered)
	}

	// The document on disk is a real ledger, not an opaque blob: it parses.
	data, err := os.ReadFile(SupervisorRefsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	var doc RefLedger
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("the ledger does not parse: %v", err)
	}
	if doc.Version != 1 || len(doc.Refs) != 2 {
		t.Errorf("ledger = %+v, want version 1 with 2 refs", doc)
	}
}

// An unreadable ledger must stop collect, not silently decide the question one
// way or the other: read as empty it would reject every concurrent dispatch,
// and read as permissive it would be the fail-open this file replaced.
func TestAnUnreadableRefLedgerIsNotAnEmptyLedger(t *testing.T) {
	root := t.TempDir()
	path := SupervisorRefsPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSupervisorRefs(root); err == nil {
		t.Fatal("a corrupt ledger was read as if it were valid")
	} else if !strings.Contains(err.Error(), path) {
		t.Errorf("the error does not name the file to fix: %v", err)
	}
}

// A symlink planted at the ledger path would make collect read an arbitrary
// file with the Supervisor's reach. Same rule the driver's own files follow.
func TestRefLedgerRefusesASymlink(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(secret, []byte(`{"version":1,"refs":[{"name":"refs/heads/whatever"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := SupervisorRefsPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, path); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	if _, err := ReadSupervisorRefs(root); err == nil {
		t.Fatal("the ledger was read through a symlink")
	}
}

func refTestRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "base")
	return root
}

func TestAdoptSupervisorRefReadsTheSHAFromTheRef(t *testing.T) {
	root := refTestRepo(t)
	cmd := exec.Command("git", "branch", "feat/investigation")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("creating the branch: %v\n%s", err, out)
	}
	shaCmd := exec.Command("git", "rev-parse", "feat/investigation")
	shaCmd.Dir = root
	out, err := shaCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSpace(string(out))

	rec, err := AdoptSupervisorRef(root, "feat/investigation")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Name != "refs/heads/feat/investigation" || rec.SHA != want {
		t.Errorf("adopted %+v, want refs/heads/feat/investigation at %s (read from the ref, not supplied)", rec, want)
	}

	// An unknown ref is an error and records nothing — a typo must not create
	// an exemption for a ref that does not exist.
	if _, err := AdoptSupervisorRef(root, "feat/typo"); err == nil {
		t.Fatal("adopting a nonexistent ref succeeded")
	}
	refs, err := ReadSupervisorRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := refs["refs/heads/feat/typo"]; ok {
		t.Error("a failed adopt still left an entry in the ledger")
	}
}

// Reconcile is the upgrade path: dispatches that predate the ledger get their
// branches recorded from the Supervisor's OWN records — and nothing else does.
func TestReconcileRecordsDispatchBranchesOnly(t *testing.T) {
	root := refTestRepo(t)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("branch", "task/T9001-real-dispatch")
	git("branch", "task/T9999-ghost") // task-shaped, but no dispatch ever recorded it

	// T9001: authoritative gate inputs (the T0012 record).
	dir := RuntimeTasksDir(root, "T9001")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(gateInputsPath(root, "T9001"), marshalIndentBytes(&GateInputs{
		TaskID: "T9001", Branch: "task/T9001-real-dispatch",
	})); err != nil {
		t.Fatal(err)
	}
	// T9002: no gate inputs — the registry fallback for a pre-T0012 dispatch.
	if err := SaveRegistry(root, &WorkerRecord{
		TaskID: "T9002", RunID: "r", Branch: "task/T9002-registry-only",
		Worktree: root, ResultDir: root, StartedAt: "t",
	}); err != nil {
		t.Fatal(err)
	}
	git("branch", "task/T9002-registry-only")

	recorded, err := ReconcileSupervisorRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 2 {
		t.Errorf("recorded %v, want the two dispatch branches", recorded)
	}
	refs, err := ReadSupervisorRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"refs/heads/task/T9001-real-dispatch", "refs/heads/task/T9002-registry-only"} {
		if _, ok := refs[want]; !ok {
			t.Errorf("%s was not recorded by the reconcile", want)
		}
	}
	if _, ok := refs["refs/heads/task/T9999-ghost"]; ok {
		t.Error("the reconcile adopted a task-shaped ref that no dispatch recorded — the ledger would then exempt exactly the refs it exists to catch")
	}
}
