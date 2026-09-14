// Command rddev is the POST development orchestrator CLI: the deterministic
// executor of the Supervisor/Worker development system (docs/61, docs/62,
// specs/orchestrator/rddev-cli.yaml). It makes no product decisions — it
// executes the command skeleton, the task state machine, the environment
// preflight and the compose wrapper.
//
// Exit codes (deterministic contract):
//
//	0  ok
//	1  operational failure (illegal state transition, unreadable state, ...)
//	2  usage error (unknown command or flag)
//	3  doctor usage error (rddev doctor only; ops/doctor-checks.md §2)
//	4  not implemented (subsystem belongs to a later task — honest stub)
//
// Commands whose subsystem does not exist yet are honest stubs that fail with
// exit 4 and an explicit message naming the owning task — never silent
// no-ops that look like success:
//
//	rddev env reset|gc -> infra clients (T0010/T0011)
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/lichman0405/post/internal/devorchestrator/doctor"
	"github.com/lichman0405/post/internal/version"
)

const (
	exitOK             = 0
	exitOperational    = 1
	exitUsage          = 2
	exitDoctorUsage    = 3
	exitNotImplemented = 4
)

const usage = `rddev — POST development orchestrator (deterministic executor, docs/61)

Usage:
  rddev doctor [--json] [--fixture DIR] [--check-docker-daemon]
  rddev env up|down|reset|gc
  rddev task next [--json]
  rddev task ready [TASK] [--json]
  rddev task inspect TASK [--json]
  rddev task verify TASK [--json]
  rddev task accept TASK [--json]
  rddev task reject TASK (--reason TEXT | --reason-file FILE) [--json]
  rddev worker spawn|list|logs|stop|collect|rework|respawn ...
  rddev review spawn|collect TASK
  rddev gate list|run|status|records ...
  rddev git commit|push TASK
  rddev pr open|merge|status TASK
  rddev rebaseline TASK                                (advance a baseline, keep its work)
  rddev refs list|adopt REF                            (the Supervisor's own refs)
  rddev drive [--parallel N] [--poll DUR] [--once]   (persistent Supervisor loop)
  rddev status [--json]                                (driver, workers, decisions)
  rddev db migrate [--url URL]
  rddev workflow TASK
  rddev version
  rddev help

Flags may appear before or after positional arguments. Task commands accept
--tasks-json PATH and --state-json PATH (DAG/state file overrides, default
tasks/tasks.json and tasks/task_status.json in the current directory) and
--run-id ID (default: a fresh run id is generated and reported).

The four-gate loop (T0012): G1 runs at worker collect; G2/G3 run via
rddev gate run (G2 = CI's exact six jobs); G4 is the merge gate asserted by
rddev git/pr, which refuse while any required CI job is red or missing.
rddev workflow TASK resumes an interrupted Supervisor session from disk alone.

Exit codes: 0 ok · 1 operational failure · 2 usage error · 3 doctor usage
error · 4 not implemented (subsystem belongs to a later task).

A grading command (task, worker, review, gate, git, pr, rebaseline, refs, drive,
workflow) refuses to run when main has changed the orchestrator's own source since
this binary was built — a Gate must not grade from a tool older than the rules it
enforces (#135). Rebuild with "make rddev"; set RDDEV_ALLOW_STALE_BINARY=1 to run
an older build on purpose. git and pr are checked one step later, after the
four-gate assertion rather than before it, so that a red gate still refuses
without git or gh being invoked at all.
`

func main() {
	args := os.Args[1:]
	// Before anything else: a grading command must not run from a binary older
	// than the rules it enforces (#135). The check is on the process rather than
	// in run, so the test suite is unaffected by the git state of the checkout.
	if code, refused := guardAgainstStaleBinary(args, os.Stderr); refused {
		os.Exit(code)
	}
	os.Exit(run(args, os.Stdout, os.Stderr))
}

// run executes the CLI against the given streams and returns the process
// exit code (deterministic: same args, same state => same code and output).
func run(args []string, stdout, stderr io.Writer) int {
	// Global --json before the subcommand only. A --json after the subcommand
	// belongs to that subcommand (each parses its own flags), so it passes
	// through untouched.
	jsonOut := false
	rest := args[:0:0]
	for _, a := range args {
		if a == "--json" && len(rest) == 0 {
			jsonOut = true
			continue
		}
		rest = append(rest, a)
	}
	if len(rest) == 0 {
		fmt.Fprint(stdout, usage)
		return exitUsage
	}
	cmd, cmdArgs := rest[0], rest[1:]
	switch cmd {
	case "version":
		fmt.Fprintln(stdout, "rddev", version.Version)
		return exitOK
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return exitOK
	case "doctor":
		return runDoctorCmd(cmdArgs, stdout, stderr, jsonOut)
	case "env":
		return runEnv(cmdArgs, stdout, stderr, jsonOut)
	case "task":
		return runTask(cmdArgs, stdout, stderr, jsonOut)
	case "worker":
		return runWorker(cmdArgs, stdout, stderr, jsonOut)
	case "review":
		return runReview(cmdArgs, stdout, stderr, jsonOut)
	case "gate":
		return runGate(cmdArgs, stdout, stderr, jsonOut)
	case "git":
		return runGit(cmdArgs, stdout, stderr, jsonOut)
	case "pr":
		return runPR(cmdArgs, stdout, stderr, jsonOut)
	case "rebaseline":
		return runRebaseline(cmdArgs, stdout, stderr, jsonOut)
	case "refs":
		return runRefs(cmdArgs, stdout, stderr, jsonOut)
	case "drive":
		return runDrive(cmdArgs, stdout, stderr, jsonOut)
	case "status":
		return runStatus(cmdArgs, stdout, stderr, jsonOut)
	case "db":
		return runDB(cmdArgs, stdout, stderr, jsonOut)
	case "workflow":
		return runWorkflow(cmdArgs, stdout, stderr, jsonOut)
	default:
		fmt.Fprintf(stderr, "rddev: unknown subcommand %q\n\n%s", cmd, usage)
		return exitUsage
	}
}

// runDoctorCmd delegates to the doctor engine (ops/doctor-checks.md):
// usage problems exit 3, the report is emitted by the engine itself. A
// leading global --json (already stripped by run) is re-added so
// `rddev --json doctor` works like `rddev doctor --json`.
func runDoctorCmd(args []string, stdout, stderr io.Writer, jsonOut bool) int {
	if jsonOut {
		args = append([]string{"--json"}, args...)
	}
	opts, usageOnErr, err := doctor.ParseArgs(args)
	if err != nil {
		ue := err.(*doctor.UsageError)
		if usageOnErr {
			doctor.PrintUsage(stderr)
		}
		fmt.Fprintln(stderr, ue.Msg)
		return exitDoctorUsage
	}
	if opts == nil { // -h / --help: usage on stdout, exit 0
		doctor.PrintUsage(stdout)
		return exitOK
	}
	return opts.Run(stdout)
}
