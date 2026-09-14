package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/lichman0405/post/internal/devorchestrator"
)

const branchUsage = `Usage: rddev branch create NAME [--from REV] [--worktree DIR] [--json]

Creates a branch and records it in the Supervisor's ref ledger in the SAME
operation.

Collect exempts a new ref seen during a Worker's run only when the ref is on
that record. Every other way of creating a branch leaves a window between the
ref and the record, and a collect that lands inside it fails a task whose
deliverable is fine — the task is already past the point where it can simply be
re-judged, so recovering it costs a Worker round (#146). Using this command
instead is what closes that window: the two steps happen together.

  --from REV      what the new branch points at (default: HEAD)
  --worktree DIR  also check the branch out in a new worktree at DIR
                  (this is the form to use when starting Supervisor work in
                  post-wt/, so the ref never exists unrecorded)

The ref is created first and recorded second. That order is deliberate: it is
the one whose failure costs a rework round rather than an exemption.

For a branch that already exists, use ` + "`rddev refs adopt`" + `.
`

// runBranch implements `rddev branch ...`.
func runBranch(args []string, stdout, stderr io.Writer, jsonOut bool) int {
	if wantsHelp(args) {
		fmt.Fprint(stdout, branchUsage)
		return exitOK
	}
	vals, pos, err := parseFlags(args,
		flagSpec{"--json", false},
		flagSpec{"--from", true},
		flagSpec{"--worktree", true},
		flagSpec{"--repo-root", true},
	)
	if err != nil {
		return usageError(stderr, err.Error(), branchUsage)
	}
	if _, ok := vals["--json"]; ok {
		jsonOut = true
	}
	repoRoot := vals["--repo-root"]
	if repoRoot == "" {
		if repoRoot, err = os.Getwd(); err != nil {
			return operationalError(stderr, "rddev branch", err)
		}
	}
	if len(pos) == 0 || pos[0] != "create" {
		if len(pos) == 0 {
			return usageError(stderr, "rddev branch: expected `create`", branchUsage)
		}
		return usageError(stderr, fmt.Sprintf("rddev branch: unknown subcommand %q", pos[0]), branchUsage)
	}
	if len(pos) < 2 {
		return usageError(stderr, "rddev branch create: a branch name is required", branchUsage)
	}
	if len(pos) > 2 {
		return usageError(stderr, fmt.Sprintf("rddev branch create: unexpected argument %q", pos[2]), branchUsage)
	}
	rec, err := devorchestrator.CreateSupervisorBranch(repoRoot, pos[1], vals["--from"], vals["--worktree"])
	if err != nil {
		return operationalError(stderr, "rddev branch create", err)
	}
	if jsonOut {
		out, err := json.Marshal(rec)
		if err != nil {
			return operationalError(stderr, "rddev branch create", err)
		}
		fmt.Fprintln(stdout, string(out))
		return exitOK
	}
	fmt.Fprintf(stdout, "created %s at %s and recorded it — concurrent collects will not report it as Worker-created\n", rec.Name, shortSHA(rec.SHA))
	return exitOK
}
