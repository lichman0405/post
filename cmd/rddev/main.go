// Command rddev is the POST development orchestrator CLI.
//
// T0002 scaffold: only the version subcommand is wired. The deterministic
// development commands (`rddev doctor` per ops/doctor-checks.md, `rddev env`
// per docs/66) arrive with the orchestrator tasks; this process must not
// half-implement them.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/lichman0405/post/internal/version"
)

const usage = `rddev — POST development orchestrator (scaffold)

Usage:
  rddev version           print the rddev version
  rddev help              show this help

Planned (later tasks): doctor (ops/doctor-checks.md), env up/reset (docs/66).
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the CLI and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stdout, usage)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, "rddev", version.Version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "rddev: unknown subcommand %q\n\n%s", args[0], usage)
		return 2
	}
}
