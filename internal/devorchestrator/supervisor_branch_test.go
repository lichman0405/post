package devorchestrator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The property #146 is about, tested through the predicate collect actually
// runs rather than a paraphrase of it: a branch made by this command is not a
// finding, and a branch made any other way still is.
func TestCreateBranchIsOnTheRecordBeforeCollectCanLook(t *testing.T) {
	root := refTestRepo(t)
	before, err := refsSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}

	rec, err := CreateSupervisorBranch(root, "fix/recorded-at-creation", "main", "")
	if err != nil {
		t.Fatal(err)
	}

	wantSHA := gitRevParse(t, root, "main")
	if rec.Name != "refs/heads/fix/recorded-at-creation" {
		t.Errorf("recorded %q, want the full ref name", rec.Name)
	}
	if rec.SHA != wantSHA {
		t.Errorf("recorded %s, want %s — the sha is read from the ref, not from `from`", rec.SHA, wantSHA)
	}
	if rec.Source != "branch" {
		t.Errorf("recorded source %q, want \"branch\"", rec.Source)
	}

	ledger, err := ReadSupervisorRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ledger[rec.Name]; !ok {
		t.Fatalf("the branch is not in the ledger: %v", ledger)
	}

	// The control: a ref created by plain git, in the same window, is still a
	// finding. Without this the test would pass on an exemption widened to
	// "anything with a plausible name", which is the inference the ledger
	// replaced (see the package comment on ReadSupervisorRefs).
	byHand := exec.Command("git", "branch", "fix/created-by-hand")
	byHand.Dir = root
	if out, err := byHand.CombinedOutput(); err != nil {
		t.Fatalf("creating the control branch: %v\n%s", err, out)
	}

	after, err := refsSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	unattributable := unattributableNewRefs(before, after, ledger)
	if len(unattributable) != 1 || !strings.Contains(unattributable[0], "fix/created-by-hand") {
		t.Fatalf("collect would report %v, want exactly the hand-made branch", unattributable)
	}
}

func TestCreateBranchDefaultsToHead(t *testing.T) {
	root := refTestRepo(t)
	rec, err := CreateSupervisorBranch(root, "fix/from-head", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := gitRevParse(t, root, "HEAD"); rec.SHA != want {
		t.Errorf("branch points at %s, want HEAD's %s", rec.SHA, want)
	}
}

func TestCreateBranchCanOpenAWorktreeOnIt(t *testing.T) {
	root := refTestRepo(t)
	dir := filepath.Join(t.TempDir(), "wt")
	rec, err := CreateSupervisorBranch(root, "fix/in-a-worktree", "main", dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf("no worktree at %s: %v", dir, err)
	}
	ledger, err := ReadSupervisorRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ledger[rec.Name]; !ok {
		t.Errorf("the worktree's branch was not recorded: %v", ledger)
	}
}

// A failed creation must leave no exemption behind. The ledger failing toward
// a missing entry costs a rework round; failing toward an entry exempts a ref
// this command never made.
func TestAFailedBranchCreateRecordsNothing(t *testing.T) {
	root := refTestRepo(t)
	if _, err := CreateSupervisorBranch(root, "fix/dup", "main", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateSupervisorBranch(root, "fix/dup", "main", ""); err == nil {
		t.Fatal("creating an existing branch succeeded")
	}
	refs, err := ReadSupervisorRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Errorf("ledger holds %d entries after a failed create, want 1", len(refs))
	}
}

func TestBranchCreateRefusesATag(t *testing.T) {
	root := refTestRepo(t)
	if _, err := CreateSupervisorBranch(root, "refs/tags/v1", "main", ""); err == nil {
		t.Fatal("a tag name was accepted by a command that creates branches")
	}
	refs, err := ReadSupervisorRefs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Errorf("a refused create still wrote the ledger: %v", refs)
	}
}

func gitRevParse(t *testing.T, dir, rev string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", rev)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse %s: %v", rev, err)
	}
	return strings.TrimSpace(string(out))
}
