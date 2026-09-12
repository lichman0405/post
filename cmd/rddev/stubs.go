package main

import (
	"fmt"
	"io"
)

// Honest stubs for subsystems owned by later tasks. Each exits 4 with an
// explicit message naming the owning task — never a silent no-op that looks
// like success (rddev-cli.yaml: commands whose subsystem does not exist yet).
//
//	worker collect  -> T0011 (RESULT validation; spawn/list/logs/stop are real
//	                   since T0010)
//	git commit      -> T0012 (Git control plane, Supervisor-only)
//	pr open|merge   -> T0012 (Git control plane, Supervisor-only)

// hasJSONFlag reports whether args carry --json (the stubs accept it in any
// position, like the task/env subcommands).
func hasJSONFlag(args []string) bool {
	for _, a := range args {
		if a == "--json" {
			return true
		}
	}
	return false
}

func runGit(args []string, stdout, stderr io.Writer, jsonOut bool) int {
	if wantsHelp(args) {
		fmt.Fprint(stdout, "Usage: rddev git commit TASK\n\nNOT IMPLEMENTED (exit 4): the Git control plane (commit/push) belongs to T0012 and is Supervisor-only.\n")
		return exitOK
	}
	return notImplemented(stdout, stderr, jsonOut || hasJSONFlag(args), "git commit", "T0012 (Git control plane)")
}

func runPR(args []string, stdout, stderr io.Writer, jsonOut bool) int {
	if wantsHelp(args) {
		fmt.Fprint(stdout, "Usage: rddev pr open|merge TASK\n\nNOT IMPLEMENTED (exit 4): PR management belongs to T0012 and is Supervisor-only.\n")
		return exitOK
	}
	sub := "pr"
	if len(args) > 0 {
		sub = "pr " + args[0]
	}
	return notImplemented(stdout, stderr, jsonOut || hasJSONFlag(args), sub, "T0012 (Git control plane)")
}
