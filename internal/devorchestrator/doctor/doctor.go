// Package doctor implements the POST canonical environment preflight as the
// `rddev doctor` command: a Go port of ops/doctor.sh (T0000) with identical
// check semantics, exit codes and output shapes.
//
// The authoritative contract is ops/doctor-checks.md; the JSON shape is
// ops/doctor-output.schema.json. This package deliberately re-implements the
// same 35 checks, remediation strings, fixture injection point and
// exit-code/verdict mapping — it does not invent new check semantics, and it
// must not weaken any check. The unit tests port ops/tests/doctor-unit-test.sh
// case for case, and a parity test asserts byte-identical JSON with the bash
// reference for identical fixtures.
package doctor

import (
	"fmt"
	"io"
	"os"
)

// Options carries the doctor flags (contract §1).
type Options struct {
	JSON              bool
	FixtureDir        string
	CheckDockerDaemon bool
	ScriptName        string // header label: "rddev doctor" (bash uses "ops/doctor.sh")
}

// UsageError is a usage problem: exit 3, message + usage on stderr, no JSON.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

const usageText = `Usage: rddev doctor [--json] [--fixture DIR] [--check-docker-daemon] [-h|--help]

POST canonical environment preflight (Ubuntu 24.04 LTS amd64, see docs/64_UBUNTU_DEV_ENV.md).
Run from the repo root.

Options:
  --json                  machine-readable output (JSON, ops/doctor-output.schema.json)
  --fixture DIR           test-only: override measured inputs from fixture files
                          (contract: ops/doctor-checks.md §6)
  --check-docker-daemon   also probe the Docker daemon (docker info) and check free
                          space on the docker data-root (docs/64 §6). Off by default:
                          probing the daemon needs Docker socket access, which is
                          reserved on Worker machines.
  -h, --help              show this help

Exit codes: 0 = all required checks passed; 1 = toolchain failure (missing tool or
version outside pinned baseline); 2 = unsupported environment (non-Linux, Windows
Native, non-Ubuntu 24.04, non-amd64); 3 = usage error.
`

// ParseArgs parses doctor flags, accepting both --fixture DIR and
// --fixture=DIR like the reference implementation. A non-nil *UsageError asks
// for exit 3 with the error line (and, for unknown options, the usage text)
// on stderr — never JSON.
func ParseArgs(args []string) (*Options, bool, error) {
	o := &Options{ScriptName: "rddev doctor"}
	showUsageOnError := true
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--json":
			o.JSON = true
		case a == "--fixture":
			if i+1 >= len(args) {
				return nil, showUsageOnError, &UsageError{Msg: "error: --fixture requires a directory"}
			}
			i++
			o.FixtureDir = args[i]
		case len(a) > len("--fixture=") && a[:len("--fixture=")] == "--fixture=":
			o.FixtureDir = a[len("--fixture="):]
			if o.FixtureDir == "" {
				return nil, showUsageOnError, &UsageError{Msg: "error: --fixture requires a directory"}
			}
		case a == "--check-docker-daemon":
			o.CheckDockerDaemon = true
		case a == "-h" || a == "--help":
			return nil, false, nil // help: usage on stdout, exit 0
		default:
			return nil, showUsageOnError, &UsageError{Msg: "error: unknown option: " + a}
		}
		i++
	}
	if o.FixtureDir != "" {
		if st, err := os.Stat(o.FixtureDir); err != nil || !st.IsDir() {
			return nil, false, &UsageError{Msg: "error: fixture directory not found: " + o.FixtureDir}
		}
	}
	return o, showUsageOnError, nil
}

// Run executes the preflight: measures, evaluates all 35 checks, computes the
// verdict and writes the report to w. It returns the process exit code
// (contract §2). Usage errors are detected by ParseArgs before Run and never
// emit a report.
func (o *Options) Run(w io.Writer) int {
	m := o.measure()
	checks, verdict, code := evaluate(o, m)
	if o.JSON {
		EmitJSON(w, m, checks, verdict, code)
	} else {
		EmitHuman(w, o.ScriptName, m, checks, verdict, code)
	}
	return code
}

// PrintUsage writes the usage text to w (stdout for --help, stderr for
// errors, matching the reference implementation).
func PrintUsage(w io.Writer) { fmt.Fprint(w, usageText) }
