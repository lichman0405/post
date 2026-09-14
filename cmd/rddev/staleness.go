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
	if !commandsThatGrade[cmd] {
		return 0, false
	}
	if os.Getenv(devorchestrator.AllowStaleBinaryEnv) != "" {
		return 0, false
	}
	repoRoot, err := os.Getwd()
	if err != nil {
		return 0, false
	}
	stamp := devorchestrator.CurrentBuildStamp()
	reason, stale, err := devorchestrator.StaleBinaryReason(repoRoot, stamp.Revision)
	if err != nil || !stale {
		// Nothing proved, nothing refused. StaleBinaryReason documents why the
		// unprovable cases are not treated as stale; the short version is that a
		// refusal it cannot justify would fire on working setups.
		return 0, false
	}
	fmt.Fprintf(stderr, "rddev %s: REFUSED — %s\n", cmd, reason)
	return exitOperational, true
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
