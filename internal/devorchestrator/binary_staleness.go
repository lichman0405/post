package devorchestrator

import (
	"fmt"
	"runtime/debug"
	"strings"
)

// OrchestratorSourcePaths lists the paths whose contents are compiled into
// rddev.
//
// The four-gate loop grades a task with the rules in this binary, so a commit
// that changes one of these paths changes what those rules are. A binary built
// before such a commit enforces a rule set main no longer has, and it does so
// silently: nothing about the binary says it is old. That is not hypothetical —
// issue #135 is the case where exactly this deleted a Worker's uncommitted
// deliverable, because the binary in use predated the fix that preserved it.
//
// Only compiled-in sources belong here. Files the binary *reads* at run time do
// not: specs/orchestrator/gates.json, the task DAG and the state file change
// without needing a rebuild, so listing them would refuse work that is not
// stale. internal/devorchestrator/embed/worker-guard.sh is here because it is
// `go:embed`ed, which makes it source rather than data.
var OrchestratorSourcePaths = []string{"cmd/rddev", "internal/devorchestrator"}

// maxOrchestratorCommitsListed bounds how many offending commits the refusal
// names, so a long-stale binary reports a reason rather than a log.
const maxOrchestratorCommitsListed = 8

// BuildStamp is what the running binary knows about how it was built. Go
// records it from the checkout at build time; none of it is present when the
// binary was built without VCS stamping.
type BuildStamp struct {
	Revision string // vcs.revision (full hash); empty when the build recorded none
	Time     string // vcs.time, RFC 3339
	Modified bool   // vcs.modified: the working tree was dirty at the build
	Known    bool   // false when no revision was recorded
}

// CurrentBuildStamp reads the stamp Go embedded in the running binary.
//
// Modified is reported but never used to refuse: this repository's state file is
// rewritten by the driver on every tick, so its working tree is dirty at almost
// every build, and a rule that fired on that would fire always.
func CurrentBuildStamp() BuildStamp {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return BuildStamp{}
	}
	var st BuildStamp
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			st.Revision = s.Value
		case "vcs.time":
			st.Time = s.Value
		case "vcs.modified":
			st.Modified = s.Value == "true"
		}
	}
	st.Known = st.Revision != ""
	return st
}

// StaleBinaryReason decides whether a binary built from rev is too old to grade
// what main currently describes, and returns the refusal text when it is.
//
// The rule is deliberately narrower than "main has moved": it fires only when
// main holds a commit the build does not contain AND that commit changed the
// orchestrator's own source. A merge that only touches product code leaves the
// rules unchanged, and refusing there would stop the loop for no reason — the
// driver merges product PRs constantly, so a broader rule would refuse almost
// immediately after every merge.
//
// Two cases are not refusals, and both are deliberate:
//
//   - An empty rev means the build recorded no revision. There is nothing to
//     compare against, and refusing on "cannot tell" would break every build
//     made without VCS stamping (or outside a checkout).
//   - A git call that fails (no such revision, no main branch, not a
//     repository) is treated the same way. The guard exists to catch the
//     staleness it can prove, and a refusal it cannot justify is a false
//     positive on a working setup.
//
// The caller decides what to do with a refusal. This function only answers.
func StaleBinaryReason(repoRoot, rev string) (string, bool, error) {
	if rev == "" {
		return "", false, nil
	}
	args := append([]string{"log", "--format=%h %s", rev + "..main", "--"}, OrchestratorSourcePaths...)
	out, err := gitOutput(repoRoot, args...)
	if err != nil {
		return "", false, nil
	}
	commits := nonBlankLines(out, maxOrchestratorCommitsListed)
	if len(commits) == 0 {
		return "", false, nil
	}
	short := rev
	if len(short) > 12 {
		short = short[:12]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "this rddev was built from %s, and main has since changed the orchestrator's own source:\n", short)
	for _, c := range commits {
		fmt.Fprintf(&b, "    %s\n", c)
	}
	fmt.Fprintf(&b, "a gate must not grade from a tool older than the rules it enforces.\n"+
		"stop the driver, run `make rddev`, then start it again.\n"+
		"(%s=1 runs anyway, for a build that is deliberately not main's)", AllowStaleBinaryEnv)
	return b.String(), true, nil
}

// AllowStaleBinaryEnv is the environment variable that turns the refusal into a
// warning. The guard's job is to make staleness impossible to miss, not
// impossible to intend: a Supervisor building rddev from a feature branch needs
// to run it, and doing so should require saying so out loud.
const AllowStaleBinaryEnv = "RDDEV_ALLOW_STALE_BINARY"

// nonBlankLines returns at most max non-blank lines of git's output.
func nonBlankLines(s string, max int) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, line)
		if len(out) == max {
			break
		}
	}
	return out
}
