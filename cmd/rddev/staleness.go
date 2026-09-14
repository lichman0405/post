package main

import (
	"fmt"
	"io"
	"os"

	"github.com/lichman0405/post/internal/devorchestrator"
)

// commandsThatGrade are the subcommands whose behaviour IS the rule set: they
// judge a task, move its state, or advance a baseline. Running one from a
// binary older than main is how #135 destroyed a Worker's uncommitted
// deliverable — the binary in use predated the fix that preserved it, and
// nothing said so.
//
// The read-only and informational commands are deliberately absent: refusing
// `status` would hide the one command that shows what is going on, and the
// guard's job is to stop grading, not to make the tool unreadable.
var commandsThatGrade = map[string]bool{
	"task":       true,
	"worker":     true,
	"review":     true,
	"gate":       true,
	"git":        true,
	"pr":         true,
	"rebaseline": true,
	"refs":       true,
	"drive":      true,
	"workflow":   true,
}

// commandsCheckedAtStartup is the part of commandsThatGrade that main refuses
// before it dispatches anything.
//
// `git` and `pr` are missing on purpose, and they are not unguarded — they are
// checked later, by RunGitControl, between the four-gate assertion and the
// action (see freshnessCheck). The reason is an older promise those two
// commands already keep: `rddev git commit` on a red gate refuses and says
// "git was never invoked", and the acceptance e2e holds it to that with a fake
// git that records any invocation. The staleness question can only be answered
// by asking git, so checking at startup would put a git call in front of that
// refusal and the promise would quietly stop being true. For every other
// command there is no such refusal to get in front of, so startup is right:
// refusing early is the whole point.
var commandsCheckedAtStartup = withoutGitAndPR(commandsThatGrade)

// withoutGitAndPR is the difference, built rather than written out so the two
// sets cannot drift: a command added to commandsThatGrade is checked at startup
// unless it is one of the two that defer.
func withoutGitAndPR(all map[string]bool) map[string]bool {
	out := make(map[string]bool, len(all))
	for cmd := range all {
		out[cmd] = true
	}
	for _, deferred := range []string{"git", "pr"} {
		delete(out, deferred)
	}
	return out
}

// guardAgainstStaleBinary refuses to run a grading command from a binary that
// predates a change to the orchestrator's own source on main.
//
// It is called from main rather than from run because it is a statement about
// THIS PROCESS — the revision compiled into it — and run is what the tests
// exercise. A test binary builds from whatever branch is checked out, so
// enforcing there would make the suite pass or fail according to the git state
// of the machine rather than the behaviour of the code.
func guardAgainstStaleBinary(args []string, stderr io.Writer) (int, bool) {
	cmd := leadingCommand(args)
	if !commandsCheckedAtStartup[cmd] {
		return 0, false
	}
	repoRoot, err := os.Getwd()
	if err != nil {
		return 0, false
	}
	reason, stale := staleReason(repoRoot)
	if !stale {
		return 0, false
	}
	fmt.Fprintf(stderr, "rddev %s: REFUSED — %s\n", cmd, reason)
	return exitOperational, true
}

// staleRefusal is the answer the control-plane path gets when the binary is
// too old. It is a distinct type so git.go and pr.go can tell it apart from a
// GateRefusalError and from an operational failure — the three mean different
// things to whoever reads the output.
type staleRefusal struct{ reason string }

func (e *staleRefusal) Error() string { return e.reason }

// freshnessCheck builds the check RunGitControl runs between the four-gate
// assertion and the control-plane action.
//
// The check returns an error rather than exiting, so the refusal is still the
// command's to phrase and its exit code stays the one the contract documents.
func freshnessCheck(repoRoot string) func() error {
	return func() error {
		reason, stale := staleReason(repoRoot)
		if !stale {
			return nil
		}
		return &staleRefusal{reason: reason}
	}
}

// staleReason answers the staleness question for a working directory.
//
// ("", false) covers three different situations that all mean "do not refuse":
// the override is set, the build recorded no revision to compare, or the
// comparison could not be resolved. StaleBinaryReason documents why the
// unprovable ones are not treated as stale; the short version is that a
// refusal it cannot justify would fire on working setups.
func staleReason(repoRoot string) (string, bool) {
	if os.Getenv(devorchestrator.AllowStaleBinaryEnv) != "" {
		return "", false
	}
	stamp := devorchestrator.CurrentBuildStamp()
	reason, stale, err := devorchestrator.StaleBinaryReason(repoRoot, stamp.Revision)
	if err != nil || !stale {
		return "", false
	}
	return reason, true
}

// leadingCommand returns the subcommand: the first argument that is not the
// global --json, which run accepts before the subcommand only.
func leadingCommand(args []string) string {
	for _, a := range args {
		if a == "--json" {
			continue
		}
		return a
	}
	return ""
}
