package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/lichman0405/post/internal/devorchestrator"
)

const refsUsage = `Usage: rddev refs list [--json]
       rddev refs adopt REF [--json]
       rddev refs reconcile [--json]

The Supervisor's ref ledger: the refs the Supervisor itself created, recorded
when it created them. Collect exempts a new ref seen during a Worker's run ONLY
when it is on this record — attribution by record, not by inference, because the
commit identity a ref carries is a field whoever makes the commit chooses
(git -c user.email=…), and a gate that reads a forgeable field is fail-open
against the Worker it exists to catch.

  list       every recorded ref with when it was recorded and by which action
  adopt      record a ref created outside the tooling — a branch opened by hand
             for an investigation — so concurrent collects do not report it as a
             Worker-created ref. The sha is read from the ref; an unknown ref is
             an error, never a silent no-op.
  reconcile  record the task branch of every dispatch on disk that still exists.
             This is the upgrade path for dispatches that predate the ledger; the
             driver runs it at startup, and it is available here for a
             repository whose driver has not been restarted.

Spawn, commit and rebaseline record their own refs automatically; adopt is for
everything else the Supervisor does by hand.
`

// runRefs implements `rddev refs ...`: the Supervisor's window onto (and the
// manual entry point into) the ref ledger collect judges against.
func runRefs(args []string, stdout, stderr io.Writer, jsonOut bool) int {
	if wantsHelp(args) {
		fmt.Fprint(stdout, refsUsage)
		return exitOK
	}
	vals, pos, err := parseFlags(args, flagSpec{"--json", false}, flagSpec{"--repo-root", true})
	if err != nil {
		return usageError(stderr, err.Error(), refsUsage)
	}
	if _, ok := vals["--json"]; ok {
		jsonOut = true
	}
	repoRoot := vals["--repo-root"]
	if repoRoot == "" {
		var err error
		if repoRoot, err = os.Getwd(); err != nil {
			return operationalError(stderr, "rddev refs", err)
		}
	}
	if len(pos) == 0 {
		return usageError(stderr, "rddev refs: expected `list` or `adopt`", refsUsage)
	}
	switch pos[0] {
	case "list":
		refs, err := devorchestrator.ListSupervisorRefs(repoRoot)
		if err != nil {
			return operationalError(stderr, "rddev refs list", err)
		}
		if jsonOut {
			out, err := json.MarshalIndent(struct {
				Refs []devorchestrator.SupervisorRef `json:"refs"`
			}{Refs: refs}, "", "  ")
			if err != nil {
				return operationalError(stderr, "rddev refs list", err)
			}
			fmt.Fprintln(stdout, string(out))
			return exitOK
		}
		if len(refs) == 0 {
			fmt.Fprintln(stdout, "no refs recorded (spawn, commit, rebaseline and `rddev refs adopt` add entries)")
			return exitOK
		}
		for _, r := range refs {
			fmt.Fprintf(stdout, "%-60s %s  %-10s %s\n", r.Name, shortSHA(r.SHA), r.Source, r.TaskID)
		}
		return exitOK
	case "adopt":
		if len(pos) < 2 {
			return usageError(stderr, "rddev refs adopt: a ref name is required", refsUsage)
		}
		if len(pos) > 2 {
			return usageError(stderr, fmt.Sprintf("rddev refs adopt: unexpected argument %q", pos[2]), refsUsage)
		}
		rec, err := devorchestrator.AdoptSupervisorRef(repoRoot, pos[1])
		if err != nil {
			return operationalError(stderr, "rddev refs adopt", err)
		}
		if jsonOut {
			out, err := json.Marshal(rec)
			if err != nil {
				return operationalError(stderr, "rddev refs adopt", err)
			}
			fmt.Fprintln(stdout, string(out))
			return exitOK
		}
		fmt.Fprintf(stdout, "recorded %s at %s as the Supervisor's own — concurrent collects will not report it as Worker-created\n", rec.Name, shortSHA(rec.SHA))
		return exitOK
	case "reconcile":
		recorded, err := devorchestrator.ReconcileSupervisorRefs(repoRoot)
		if err != nil {
			return operationalError(stderr, "rddev refs reconcile", err)
		}
		if jsonOut {
			out, err := json.Marshal(struct {
				Recorded []string `json:"recorded"`
			}{Recorded: recorded})
			if err != nil {
				return operationalError(stderr, "rddev refs reconcile", err)
			}
			fmt.Fprintln(stdout, string(out))
			return exitOK
		}
		fmt.Fprintf(stdout, "recorded %d dispatch branch(es) the ledger did not have\n", len(recorded))
		for _, r := range recorded {
			fmt.Fprintf(stdout, "  %s\n", r)
		}
		return exitOK
	default:
		return usageError(stderr, fmt.Sprintf("rddev refs: unknown subcommand %q", pos[0]), refsUsage)
	}
}

// shortSHA abbreviates a sha for display, leaving anything short as it is.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
