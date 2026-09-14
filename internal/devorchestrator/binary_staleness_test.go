package devorchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// staleRepo builds a throwaway repository on main.
func staleRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bbGit(t, dir, "init", "-q", "-b", "main")
	bbCommitFile(t, dir, "README.md", "# seed\n")
	return dir
}

// staleCommit commits one file, creating its parent directories, and returns
// the new commit's full hash.
func staleCommit(t *testing.T, dir, name, content string) string {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	bbGit(t, dir, "add", "-A")
	bbGit(t, dir, "-c", "user.name=test", "-c", "user.email=test@test", "commit", "-q", "-m", "add "+name)
	return bbGit(t, dir, "rev-parse", "HEAD")
}

// The rule this test pins down is narrowness: main moving is NOT staleness. The
// driver merges product PRs continuously, so a rule that fired on every merge
// would refuse within a minute of every start, and a guard that cries wolf
// every minute is one that gets switched off.
func TestOnlyAChangeToTheOrchestratorsOwnSourceMakesABinaryStale(t *testing.T) {
	dir := staleRepo(t)
	// The "binary": built from the seeded commit.
	built := bbGit(t, dir, "rev-parse", "HEAD")

	if reason, stale, err := StaleBinaryReason(dir, built); err != nil || stale {
		t.Fatalf("a build at main's tip must not be stale: stale=%v err=%v reason=%q", stale, err, reason)
	}

	// A merge that only touches product code leaves the rules alone.
	staleCommit(t, dir, "internal/gitprovider/gitea.go", "package gitprovider\n")
	if reason, stale, err := StaleBinaryReason(dir, built); err != nil || stale {
		t.Fatalf("a product-only commit must not make the binary stale: stale=%v err=%v reason=%q", stale, err, reason)
	}

	// A change to the orchestrator's own source does — this is #135: the binary
	// that deleted T0301's work predated 4eee191, which is exactly this shape.
	orchestrator := staleCommit(t, dir, "internal/devorchestrator/rebaseline.go", "package devorchestrator\n")
	reason, stale, err := StaleBinaryReason(dir, built)
	if err != nil {
		t.Fatal(err)
	}
	if !stale {
		t.Fatal("a commit touching internal/devorchestrator must make the binary stale")
	}
	if !strings.Contains(reason, orchestrator[:7]) {
		t.Errorf("the refusal must name the offending commit %s, got:\n%s", orchestrator[:7], reason)
	}
	if !strings.Contains(reason, "make rddev") {
		t.Errorf("the refusal must say how to fix it, got:\n%s", reason)
	}

	// cmd/rddev is compiled in too: the CLI's own behaviour is part of the rules.
	cli := staleCommit(t, dir, "cmd/rddev/task.go", "package main\n")
	reason, stale, err = StaleBinaryReason(dir, built)
	if err != nil || !stale {
		t.Fatalf("a commit touching cmd/rddev must make the binary stale: stale=%v err=%v", stale, err)
	}
	for _, want := range []string{orchestrator[:7], cli[:7]} {
		if !strings.Contains(reason, want) {
			t.Errorf("the refusal must list every offending commit; %s missing from:\n%s", want, reason)
		}
	}
}

// A file the binary only READS does not require a rebuild, so changing it must
// not refuse. Listing such paths is the easy mistake this guards against.
func TestAChangeTheBinaryOnlyReadsIsNotStaleness(t *testing.T) {
	dir := staleRepo(t)
	built := bbGit(t, dir, "rev-parse", "HEAD")
	// Spec data, the DAG and the state file are read at run time, not compiled in.
	staleCommit(t, dir, "specs/orchestrator/gates.json", "{}\n")
	staleCommit(t, dir, "specs/orchestrator/worker-result.schema.json", "{}\n")
	staleCommit(t, dir, "tasks/tasks.json", "{}\n")
	if reason, stale, err := StaleBinaryReason(dir, built); err != nil || stale {
		t.Fatalf("a spec-only change must not refuse (the binary reads it at run time): stale=%v err=%v reason=%q", stale, err, reason)
	}
}

// The guard script is `go:embed`ed, which makes it source rather than data. It
// lives under internal/devorchestrator, so the path rule already covers it —
// this pins that down, because moving it out of that tree would silently stop
// grading the file the Worker sandbox is built from.
func TestTheEmbeddedGuardScriptCountsAsSource(t *testing.T) {
	dir := staleRepo(t)
	built := bbGit(t, dir, "rev-parse", "HEAD")
	staleCommit(t, dir, "internal/devorchestrator/embed/worker-guard.sh", "#!/bin/sh\n")
	reason, stale, err := StaleBinaryReason(dir, built)
	if err != nil || !stale {
		t.Fatalf("an embedded file is compiled in; changing it must make the binary stale: stale=%v err=%v", stale, err)
	}
	if !strings.Contains(reason, "internal/devorchestrator") {
		t.Error("the refusal should attribute the change to the path that caused it")
	}
}

// Nothing proved, nothing refused. Each of these is a setup where the question
// cannot be answered, and answering it anyway would refuse working setups.
func TestAnUnprovableStalenessIsNotARefusal(t *testing.T) {
	dir := staleRepo(t)
	built := bbGit(t, dir, "rev-parse", "HEAD")
	staleCommit(t, dir, "internal/devorchestrator/x.go", "package devorchestrator\n")

	cases := []struct {
		name string
		repo string
		rev  string
	}{
		{"the build recorded no revision", dir, ""},
		{"the revision is not in this repository", dir, "0123456789abcdef0123456789abcdef01234567"},
		{"the directory is not a repository at all", t.TempDir(), built},
		{"the directory does not exist", filepath.Join(dir, "nope"), built},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, stale, err := StaleBinaryReason(tc.repo, tc.rev)
			if err != nil {
				t.Fatalf("an unanswerable question must not be an error either: %v", err)
			}
			if stale {
				t.Errorf("must not refuse what it cannot prove; got %q", reason)
			}
		})
	}
}

// A repository with no main branch cannot be compared against main. The driver
// always runs in a repo that has one; a fixture that does not must not turn
// into a refusal.
func TestARepositoryWithoutMainIsNotARefusal(t *testing.T) {
	dir := t.TempDir()
	bbGit(t, dir, "init", "-q", "-b", "trunk")
	bbCommitFile(t, dir, "README.md", "# seed\n")
	built := bbGit(t, dir, "rev-parse", "HEAD")
	bbGit(t, dir, "-c", "user.name=test", "-c", "user.email=test@test", "commit", "-q", "--allow-empty", "-m", "move trunk")
	if _, stale, err := StaleBinaryReason(dir, built); err != nil || stale {
		t.Fatalf("no main branch must not refuse: stale=%v err=%v", stale, err)
	}
}

// The refusal is read by a person under time pressure, so it must not grow with
// the number of commits it is complaining about.
func TestTheRefusalListsABoundedNumberOfCommits(t *testing.T) {
	dir := staleRepo(t)
	built := bbGit(t, dir, "rev-parse", "HEAD")
	for i := 0; i < 20; i++ {
		// The content has to differ per commit: identical content leaves the
		// tree clean, and the second `git commit` fails rather than adding a
		// commit — which is how this test first failed to test anything.
		staleCommit(t, dir, "internal/devorchestrator/many.go",
			fmt.Sprintf("package devorchestrator\n\n// revision %d\n", i))
	}
	reason, stale, err := StaleBinaryReason(dir, built)
	if err != nil || !stale {
		t.Fatalf("expected staleness: stale=%v err=%v", stale, err)
	}
	// Only the commit lines are indented; the advice lines are flush, so the
	// list and the instructions cannot be confused by a reader — or by this
	// count, which is how the indentation was found to be ambiguous.
	var listed int
	for _, line := range strings.Split(reason, "\n") {
		if strings.HasPrefix(line, "    ") {
			listed++
		}
	}
	if listed == 0 || listed > maxOrchestratorCommitsListed {
		t.Errorf("listed %d commits, want 1..%d; refusal was:\n%s", listed, maxOrchestratorCommitsListed, reason)
	}
}
