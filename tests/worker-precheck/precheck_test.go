// Package workerprecheck is the controlled sample for WEB-28 (T1333): the
// smallest Go test that proves an independent rddev Worker can read a file,
// edit a file, execute a command and leave the worktree green. It is not
// product code and makes no claim about platform behaviour.
//
// The Worker created testdata/probe.txt with its file tools and wrote the
// expected bytes below by hand, so the assertion can fail in the two ways the
// task asks about: the file is missing (the state before the Worker wrote it),
// or its content differs from what the Worker wrote. Command execution is the
// Bash invocation `go test ./tests/worker-precheck/` itself; the test only
// reads the file the Worker produced.
package workerprecheck

import (
	"os"
	"testing"
)

const (
	probeFile = "testdata/probe.txt"

	// wantProbe is the byte-for-byte content the Worker wrote. It is spelled
	// out here rather than derived from disk: a test that read the expected
	// value from the same file could never fail.
	wantProbe = "WEB-28 worker precheck\n" +
		"task=T1333\n" +
		"model=deepseek-flash\n" +
		"scope=tests/worker-precheck\n"
)

// TestProbeFileMatchesTheContentTheWorkerWrote asserts the exact content of the
// file the Worker created. It fails while the probe file does not exist yet and
// passes once the Worker has written it.
func TestProbeFileMatchesTheContentTheWorkerWrote(t *testing.T) {
	path := probeFile

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (the probe file must exist before this test can pass)", path, err)
	}
	if string(got) != wantProbe {
		t.Fatalf("content of %s does not match what the Worker wrote\n got: %q\nwant: %q", path, got, wantProbe)
	}
}
