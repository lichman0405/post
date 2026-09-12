package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
)

const envUsage = `Usage: rddev env up|down|reset|gc [flags]

Commands:
  up      start the local infrastructure stack (docs/66 §2): wraps
          "docker compose up -d --wait --wait-timeout 300" for the
          docker-compose.yml in the current directory
  down    stop the stack: wraps "docker compose down"
  reset   NOT IMPLEMENTED (exit 4): test/dev namespace reset belongs to the
          infra clients of T0010/T0011 (--test-only / --all-dev-data --confirm)
  gc      NOT IMPLEMENTED (exit 4): unreferenced-state garbage collection
          belongs to the worker registry of T0010/T0011
`

// composeExec runs `docker compose <args>` with the CLI streams attached. It
// is a package variable so tests can substitute a recorder without touching
// the Docker daemon.
var composeExec = func(stdout, stderr io.Writer, args ...string) int {
	cmd := exec.Command("docker", append([]string{"compose"}, args...)...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(stderr, "rddev env: docker compose failed: %v\n", err)
		return exitOperational
	}
	return exitOK
}

func runEnv(args []string, stdout, stderr io.Writer, jsonOut bool) int {
	if wantsHelp(args) {
		fmt.Fprint(stdout, envUsage)
		return exitOK
	}
	// The reset/gc flags are parsed and validated (docs/66 §4 surface) even
	// though the subcommands themselves are honest stubs.
	_, pos, err := parseFlags(args,
		flagSpec{"--json", false},
		flagSpec{"--test-only", false},
		flagSpec{"--all-dev-data", false},
		flagSpec{"--confirm", false},
	)
	if err != nil {
		return usageError(stderr, err.Error(), envUsage)
	}
	if len(pos) != 1 {
		return usageError(stderr, "rddev env: expected exactly one of up|down|reset|gc", envUsage)
	}
	switch pos[0] {
	case "up":
		return composeExec(stdout, stderr, "-f", "docker-compose.yml", "up", "-d", "--wait", "--wait-timeout", "300")
	case "down":
		return composeExec(stdout, stderr, "-f", "docker-compose.yml", "down")
	case "reset", "gc":
		return notImplemented(stdout, stderr, jsonOut, "env "+pos[0], "the infra clients of T0010/T0011")
	default:
		return usageError(stderr, fmt.Sprintf("rddev env: unknown subcommand %q", pos[0]), envUsage)
	}
}

// notImplemented emits the honest-stub failure (exit 4): an explicit message
// naming the owning task, never a silent no-op. With --json the message is a
// JSON error object on stdout.
func notImplemented(stdout, stderr io.Writer, jsonOut bool, what, owner string) int {
	msg := fmt.Sprintf("rddev %s: not implemented — this subsystem belongs to %s", what, owner)
	if jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		enc.Encode(map[string]any{
			"error":     msg,
			"exit_code": exitNotImplemented,
		})
		return exitNotImplemented
	}
	fmt.Fprintln(stderr, msg)
	return exitNotImplemented
}
