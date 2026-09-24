package devorchestrator

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestGuardRegressionSuiteRunsWhereCIRunsIt is the second half of T1219, and
// it exists because the first half had nothing watching it.
//
// tests/worker-guard/guard-regression.sh calls itself "the two-sided
// regression suite for worker-guard.sh" and says every isolation rule has a
// test there. It did — and nothing ran them. `.github/workflows/ci.yml` had
// zero hits, the Makefile had zero hits, and no Go test invoked it (all three
// re-checked before this file was written). A suite that no gate executes is
// a guarantee written in a file, which is exactly the defect the rest of this
// task is about: the guard's own header promised a write envelope that the
// matcher never delivered, and the suite that would have noticed was sitting
// there unread. So the fix for the matcher is only complete once the thing
// that proves it runs inside a gate, and `go test ./...` is the gate CI runs
// (`go test $(go list ./... | grep -v -e '/tests/integration' -e '/tests/e2e$')`
// in the `go` job, and `make test-unit`), so this test is how the suite gets
// executed on every CI run and every G2 re-run.
//
// It drives the EMBEDDED script, not the checked-out copy. The embedded
// string is what `rddev worker spawn` writes into every Worker's guard
// directory; the checked-out copy is kept identical by
// TestGuardScriptEmbeddedMatchesExample. Running the embedded one means the
// suite is executed against the bytes that actually confine a Worker, so a
// fix that reaches the file but not the binary cannot pass here.
func TestGuardRegressionSuiteRunsWhereCIRunsIt(t *testing.T) {
	repoRoot, suite := guardRegressionSuitePath(t)

	// The suite takes the guard under test as an argument (its default is the
	// checkout copy); passing the embedded bytes through a temp file is what
	// ties this run to the shipped script.
	dir := t.TempDir()
	guard := filepath.Join(dir, "worker-guard.sh")
	if err := os.WriteFile(guard, []byte(GuardScript()), 0o755); err != nil {
		t.Fatalf("writing the embedded guard for the suite: %v", err)
	}

	cmd := exec.Command("bash", suite, guard)
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	got := string(out)

	// The suite's own verdict first. Deliberately not skipped when bash is
	// missing: `bash` is what runs CI's other unit scripts, and a skip here
	// would reinstate the silent gap this test closes.
	if err != nil {
		t.Fatalf("the guard regression suite failed (exit %v) when run against the embedded guard — every isolation rule it covers is a rule a Worker can violate:\n%s", err, got)
	}

	pass, total := parseSuiteCounts(t, got)
	if pass != total {
		t.Fatalf("the suite reported %d PASS of %d — its exit status said success, which cannot both be true:\n%s", pass, total, got)
	}

	// A floor, not an equality: the count is the observable that a "passing"
	// suite still covers the rules. Deleting cases (or weakening their
	// expectations until they are unreachable) is the failure this catches,
	// and the task that does it has to raise this number on purpose.
	//
	// 188 was the count T1219 landed with: 154 before it, plus the two-sided
	// file-writing-tool pairs (Write/Edit/MultiEdit/NotebookEdit), the
	// decoder-vs-text-scan payloads, and the fail-closed contract-env cases.
	// Every one of them was 154 until the matcher named those tools.
	//
	// 365 is what T1221 raises it to: +177, and every one of them is a case
	// the newline hole needed. The hole was one separator wide and every verb
	// family deep, so the growth is a matrix rather than a case — 6
	// separators (; | && || & and a newline) × 15 dangerous second-position
	// commands (the five shell write verbs, the four privilege-escalation
	// binaries, the three control-plane CLIs, git and printenv) = 90 refusals,
	// plus the allowed neighbour of each family in the same position (11 per
	// separator = 66), plus the raw wire shapes of a two-line command and the
	// two unicode-escape spellings of the same trick (5), the
	// tab-is-not-a-separator pair (3), the payloads the guard cannot read —
	// unparseable documents and no-command Bash calls (5) and the fields that
	// are present but are not the string the rule needs (5) — the
	// unknown-tool-name counter-case (1) and the two shell-read cases that
	// pin the limit the contract sentence states (2). A floor set below the
	// count a task lands with would let that task's cases be deleted again.
	const floor = 365
	if total < floor {
		t.Fatalf("the suite ran %d cases, below the %d it carried when T1221 closed the newline hole in command-position analysis — cases were removed or made unreachable:\n%s", total, floor, got)
	}
	t.Logf("guard regression suite: %d/%d PASS against the embedded guard", pass, total)
}

// guardRegressionSuitePath resolves the suite and the repo root ABSOLUTELY.
// `go test` runs with the package directory as the working directory, and both
// tests below set `cmd.Dir` to the repo root so the suite runs the way a
// human runs it — a relative script path would then be resolved against the
// wrong directory and bash would report "No such file or directory" (which is
// how this was caught).
func guardRegressionSuitePath(t *testing.T) (repoRoot, suite string) {
	t.Helper()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving the repo root: %v", err)
	}
	suite = filepath.Join(repoRoot, "tests", "worker-guard", "guard-regression.sh")
	if _, err := os.Stat(suite); err != nil {
		t.Fatalf("the two-sided guard regression suite is missing: %v — the guard's rules would then be unenforced prose", err)
	}
	return repoRoot, suite
}

var suiteCountRE = regexp.MustCompile(`guard-regression: ([0-9]+)/([0-9]+) PASS`)

// parseSuiteCounts reads the suite's summary line. A suite that exits 0 while
// printing no summary has not been shown to have run anything, so the absence
// is fatal rather than treated as "nothing to check".
func parseSuiteCounts(t *testing.T, out string) (pass, total int) {
	t.Helper()
	m := suiteCountRE.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("the guard regression suite printed no 'N/M PASS' summary line, so nothing shows it ran any case:\n%s", out)
	}
	pass, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("parsing %q from the suite summary: %v", m[1], err)
	}
	total, err = strconv.Atoi(m[2])
	if err != nil {
		t.Fatalf("parsing %q from the suite summary: %v", m[2], err)
	}
	if total == 0 {
		t.Fatalf("the suite reported 0 cases; a suite with no cases passes vacuously:\n%s", out)
	}
	return pass, total
}

// TestGuardRegressionSuiteCanFail is the instrument's own two-sided test: it
// proves the runner above can report a failure, so a green
// TestGuardRegressionSuiteRunsWhereCIRunsIt is evidence about the guard
// rather than evidence that nothing was measured.
//
// It runs the real suite against a guard that is broken in the way the suite
// exists to catch — shell writes no longer confined — and requires the run to
// come back non-zero. Breaking the SCRIPT rather than a rule in it keeps this
// from depending on any single case's spelling: whichever cases cover write
// confinement, at least one of them must notice.
func TestGuardRegressionSuiteCanFail(t *testing.T) {
	repoRoot, suite := guardRegressionSuitePath(t)

	// The mutation: every write target is admitted. `check_write_target`
	// returns before it can block anything, which is the guard's write
	// envelope removed without touching a single case.
	const anchor = "check_write_target() {\n\ttarget=$1"
	if !strings.Contains(GuardScript(), anchor) {
		t.Fatalf("cannot find check_write_target in the embedded guard to mutate; this test must be updated with it rather than silently pass")
	}
	mutated := strings.Replace(GuardScript(), anchor, anchor+"\n\treturn 0", 1)

	dir := t.TempDir()
	guard := filepath.Join(dir, "worker-guard.sh")
	if err := os.WriteFile(guard, []byte(mutated), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", suite, guard)
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the suite passed against a guard whose write confinement was removed: it cannot fail on the rule it is named for, so its PASS carries no information\n%s", out)
	}
	if !strings.Contains(string(out), "FAILED") {
		t.Errorf("the suite failed but did not report FAILED cases:\n%s", out)
	}
}
