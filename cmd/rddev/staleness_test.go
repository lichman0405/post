package main

import (
	"bytes"
	"testing"
)

// The list is the whole decision about what the guard covers, so it is pinned
// in both directions. Over-covering hides the system from its operator;
// under-covering leaves a grading command able to run from a stale build.
func TestTheGuardCoversEveryCommandThatGrades(t *testing.T) {
	// Each of these judges a task, moves its state, or advances a baseline.
	for _, cmd := range []string{
		"task", "worker", "review", "gate", "git", "pr", "rebaseline", "refs", "drive", "workflow",
	} {
		if !commandsThatGrade[cmd] {
			t.Errorf("%q grades or mutates and must be guarded", cmd)
		}
	}
	// Each of these is how you find out what is going on, or does not grade.
	for _, cmd := range []string{"status", "version", "help", "doctor", "env", "db", ""} {
		if commandsThatGrade[cmd] {
			t.Errorf("%q must stay usable from a stale binary: refusing it hides the problem, "+
				"and refusing the empty command would break `rddev` with no arguments", cmd)
		}
	}
}

// Being guarded is not the same as being guarded at startup, and the
// difference is the whole reason git.go and pr.go pass a FreshnessCheck: those
// two have an older promise — on a red gate they refuse and say "git was never
// invoked" — and the staleness question can only be answered by invoking git.
// Checking them at startup would put a git call in front of that refusal and
// quietly retire the promise. This test pins the split so neither half can
// drift: everything graded is checked exactly once, in one of the two places.
func TestGitAndPRAreCheckedAfterTheirGateAssertionAndEverythingElseBefore(t *testing.T) {
	deferredToTheAction := map[string]bool{"git": true, "pr": true}

	for cmd := range commandsThatGrade {
		switch {
		case deferredToTheAction[cmd] && commandsCheckedAtStartup[cmd]:
			t.Errorf("%q is checked at startup as well as after its gate assertion; the "+
				"startup check runs a git subprocess before the red-gate refusal that "+
				"promises no git was invoked", cmd)
		case !deferredToTheAction[cmd] && !commandsCheckedAtStartup[cmd]:
			t.Errorf("%q grades but is checked nowhere — it would run unguarded from a stale binary", cmd)
		}
	}
	// The other direction: the startup set must not invent a command that is
	// not graded at all, which would refuse something nobody decided to guard.
	for cmd := range commandsCheckedAtStartup {
		if !commandsThatGrade[cmd] {
			t.Errorf("%q is checked at startup but is not a grading command", cmd)
		}
	}
}

// leadingCommand must find the subcommand for both `rddev task …` and
// `rddev --json task …`, because run() accepts the global flag in that one
// position and the guard has to agree with it about where the subcommand is.
func TestTheGuardFindsTheSubcommandPastTheGlobalFlag(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"task", "next"}, "task"},
		{[]string{"--json", "gate", "run", "T0001"}, "gate"},
		{[]string{"--json"}, ""},
		{nil, ""},
	}
	for _, tc := range cases {
		if got := leadingCommand(tc.args); got != tc.want {
			t.Errorf("leadingCommand(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

// The unprovable case, end to end through the guard: a directory that is not a
// repository proves nothing, so a grading command must still run.
func TestTheGuardDoesNotRefuseWhenNothingIsProven(t *testing.T) {
	t.Chdir(t.TempDir())
	var stderr bytes.Buffer
	code, refused := guardAgainstStaleBinary([]string{"gate", "run", "T0001"}, &stderr)
	if refused {
		t.Fatalf("refused outside a repository, with no proof of staleness: %s", stderr.String())
	}
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
}

// A read-only command is never refused, so the guard must not even look.
func TestTheGuardIgnoresCommandsThatDoNotGrade(t *testing.T) {
	var stderr bytes.Buffer
	if _, refused := guardAgainstStaleBinary([]string{"status"}, &stderr); refused {
		t.Fatalf("status must never be refused; got: %s", stderr.String())
	}
}

// The override is deliberately NOT unit-tested here, and the gap is recorded
// rather than papered over: this worktree is not stale relative to its own main,
// so a test asserting "the override suppresses the refusal" would pass whether
// or not the override worked. It is covered end to end instead, by building the
// binary from a checkout whose main then advances, and running it both ways.
