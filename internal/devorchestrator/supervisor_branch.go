package devorchestrator

import (
	"fmt"
	"strings"
)

// CreateSupervisorBranch creates a branch and records it in the Supervisor's
// ref ledger in one operation.
//
// The ledger exempts a new ref seen during a Worker's run only when that ref is
// ON THE RECORD (worker_collect.go's unattributableNewRefs), which is the right
// rule — attribution by record, not by inference — but it left the window
// between creating a ref and recording it unguarded. A collect that landed
// inside that window failed a task whose deliverable was never in question, and
// the task had already advanced far enough that recovering it costs a Worker
// round: #146 measured this twice in one day, on two unrelated refs.
//
// Nothing but a mechanism closes that window. `rddev refs adopt` reads the sha
// FROM the ref, so it cannot record a ref that does not exist yet, and
// pre-adopting planned work is therefore impossible. This command is the
// mechanism: the ref and its record are made by one operation, so the window is
// a crash between two adjacent statements rather than the minutes between two
// tool calls.
//
// Order of the two steps, and why it is this one: the ref is created FIRST and
// the ledger written second, even though that is the order whose failure hurts
// the Supervisor. The reverse order fails open — a record written before its
// ref leaves an entry that, should anything later create a ref of that name,
// exempts a ref this command never made, which is the same fail-open as
// trusting a forgeable author field one level down. Failing toward a false
// finding costs a rework round; failing toward an exemption costs the check.
// The ledger's whole design takes the first, and so does this.
func CreateSupervisorBranch(repoRoot, name, from, worktreeDir string) (*SupervisorRef, error) {
	full, short, err := supervisorBranchName(name)
	if err != nil {
		return nil, err
	}
	if from == "" {
		from = "HEAD"
	}
	if worktreeDir == "" {
		if _, err := gitOutput(repoRoot, "branch", short, from); err != nil {
			return nil, fmt.Errorf("creating branch %s at %s: %w", full, from, err)
		}
	} else {
		if _, err := gitOutput(repoRoot, "worktree", "add", "-b", short, worktreeDir, from); err != nil {
			return nil, fmt.Errorf("creating worktree %s on branch %s at %s: %w", worktreeDir, full, from, err)
		}
	}
	// Read the sha back from the ref rather than reusing the one `from` resolved
	// to — the same rule adopt follows, so the record can only ever describe a
	// state the ref is actually in.
	sha, err := gitOutput(repoRoot, "rev-parse", "--verify", full)
	if err != nil {
		return nil, fmt.Errorf("reading back the new ref %s: %w", full, err)
	}
	if err := RecordSupervisorRef(repoRoot, full, sha, "branch", ""); err != nil {
		return nil, err
	}
	return &SupervisorRef{Name: full, SHA: sha, Source: "branch", At: nowRFC3339()}, nil
}

// supervisorBranchName normalizes a requested branch name and returns both the
// full ref name (what the ledger records) and the short name (what git's branch
// and worktree commands take).
//
// A tag is refused rather than silently reinterpreted: this command creates
// branches, and `refs/tags/…` reaching it is a caller who meant something else.
func supervisorBranchName(name string) (full, short string, err error) {
	full, err = NormalizeRefName(name)
	if err != nil {
		return "", "", err
	}
	short, ok := strings.CutPrefix(full, "refs/heads/")
	if !ok || short == "" {
		return "", "", fmt.Errorf("%s is not a branch name (this command creates refs/heads/…)", full)
	}
	return full, short, nil
}
