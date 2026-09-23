// T1212-TEST "e2e db guard" (blocking): the judgement that turns a silently
// skipped database journey into a failure is only worth trusting once it has
// been watched decide both ways.
//
//	go test ./tests/e2e -run TestE2EDBGuardDecidesBothWays -count=1 -v
//
// No database is involved: the journeys are pointed at a loopback port nothing
// is listening on, and the variable under test decides the outcome.
package e2e

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/persistence/testdb"
)

// guardTestName is this file's test. The child is selected by -test.run
// anchored to it, so a child process runs one test and nothing else.
const guardTestName = "TestE2EDBGuardDecidesBothWays"

// guardChildEnv names the variable that tells a re-executed copy of this test
// binary which half of the test it is. It is unset in every ordinary run.
const guardChildEnv = "POST_E2E_DB_GUARD_CHILD"

// guardJourneys are the two journeys in this package that need a real
// PostgreSQL, with the words and the clause their skip reasons have always
// carried (conflict_e2e_test.go, discussion_promote_e2e_test.go). The clause is
// here so the unset case can be held to the pre-T1212 message verbatim.
var guardJourneys = []struct {
	journey string // the value carried to the child
	label   string // how the suite names the journey in the message
	needs   string // the clause in the middle of the skip reason
}{
	{"conflict", "conflict e2e", "the resolution journey needs a real database"},
	{"discussion", "discussion promotion e2e", "the journey needs a real database"},
}

// TestE2EDBGuardDecidesBothWays watches testdb.RequireDB decide, in both
// directions, against a database that is definitely not there.
//
// The judgement under test ends in t.Fatalf or t.Skipf, and both of those end
// the test that makes the call — so it cannot be observed from inside the run
// that makes it. This test therefore has two halves. Run normally it is the
// parent: for every case it re-executes this same binary with -test.run pointed
// at itself and asserts what came back from the outside — exit status, and the
// verdict and message go test printed for that run. Re-executed with
// guardChildEnv set it is the child: it calls the journeys' own first line,
// newConflictEnv / newDiscussionEnv, and lets testdb.RequireDB decide its fate.
//
// One function rather than two, deliberately: a second helper test that skipped
// whenever it was not the child would add a "--- SKIP" line to every verbose
// run of a package whose entire subject is that a skip is not a pass.
//
// Going through the real call sites is what makes this more than a unit test of
// the helper: revert either journey to its own `if !pgReachable(…) { t.Skipf(…) }`
// and the required case reds, because the child would skip where the parent
// demands a failure.
func TestE2EDBGuardDecidesBothWays(t *testing.T) {
	if journey := os.Getenv(guardChildEnv); journey != "" {
		guardChildRun(t, journey)
		return
	}

	dead := deadPortDSN(t)
	for _, j := range guardJourneys {
		for _, require := range []bool{true, false} {
			name := j.label + ", database optional"
			if require {
				name = j.label + ", database required"
			}
			t.Run(name, func(t *testing.T) {
				guardCase(t, j.journey, j.label, j.needs, require, dead)
			})
		}
	}
}

// guardCase runs one child and asserts the verdict it came back with.
func guardCase(t *testing.T, journey, label, needs string, require bool, deadDSN string) {
	t.Helper()
	out, err := runGuardChild(t, journey, require, deadDSN)

	// The child's output is not echoed on success. It contains the verdict
	// banners this test asserts on, and the run whose acceptance criterion is
	// "with a database, no SKIP" must not have the word printed into it by a
	// test about skips. Every failure below prints the whole thing instead.
	defer func() {
		if t.Failed() {
			t.Logf("child output (journey %s, %s=%v):\n%s", journey, testdb.RequireE2EDBEnv, require, out)
		}
	}()

	verdict := verdictOf(out)
	switch {
	case require:
		if verdict != "--- FAIL: " {
			t.Errorf("a dead-port DSN with %s=1 must fail the journey; it %s instead",
				testdb.RequireE2EDBEnv, verdictPhrase(verdict))
		} else if code, ok := exitCode(err); !ok || code != 1 {
			t.Errorf("the failing child exited %d (want 1): %v", code, err)
		}
		// The message has to name the four facts that tell "the database is
		// down" apart from "nobody ever ran this".
		for _, want := range []string{
			"--- FAIL: " + guardTestName,
			testdb.RequireE2EDBEnv, // the variable that made skipping impossible
			deadDSN,                // the DSN it could not reach
			label,                  // which journey went unrun
		} {
			if !strings.Contains(out, want) {
				t.Errorf("the failure does not name %q", want)
			}
		}
	case verdict != "--- SKIP: ":
		t.Errorf("an unset %s must leave the journey skipping; it %s instead",
			testdb.RequireE2EDBEnv, verdictPhrase(verdict))
	default:
		if err != nil {
			t.Errorf("the skipping child exited non-zero: %v", err)
		}
		// Verbatim, including the clause in the middle: this is the behaviour
		// a machine without a database has always had, and pinning the whole
		// sentence is how a reworded one has to be a deliberate act. Change
		// the reason in testdb.RequireDB and this line changes with it.
		wantReason := fmt.Sprintf("%s: PostgreSQL unreachable at %s — %s; "+
			"start the stack (make infra-up) or set POSTGRES_TEST_ADMIN_URL and the test runs",
			label, deadDSN, needs)
		for _, want := range []string{"--- SKIP: " + guardTestName, wantReason} {
			if !strings.Contains(out, want) {
				t.Errorf("the skip does not carry %q", want)
			}
		}
	}
}

// guardChildRun is the child half: it calls the first line the journey test
// itself calls, with a DSN nothing is listening on, and lets testdb.RequireDB
// decide what happens to this test. Reaching one of the Fatalfs below means the
// journey reached its database anyway — either it decided for itself instead of
// consulting the judgement, or something really is answering on the port the
// parent reported dead — and both are failures here, with the DSN in hand.
func guardChildRun(t *testing.T, journey string) {
	t.Helper()
	switch journey {
	case "conflict":
		_ = newConflictEnv(t, context.Background())
		t.Fatalf("db guard child: newConflictEnv returned instead of being decided by testdb.RequireDB, "+
			"against %s", conflictAdminURL())
	case "discussion":
		_ = newDiscussionEnv(t, context.Background())
		t.Fatalf("db guard child: newDiscussionEnv returned instead of being decided by testdb.RequireDB, "+
			"against %s", discussionAdminURL())
	default:
		t.Fatalf("db guard child: %s=%q is not a journey this test knows (conflict or discussion)", guardChildEnv, journey)
	}
}

// runGuardChild re-executes this test binary as the child and returns its
// combined output and exit error.
func runGuardChild(t *testing.T, journey string, require bool, deadDSN string) (string, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+guardTestName+"$", "-test.v")
	cmd.Env = guardChildEnvFor(journey, require, deadDSN)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if _, ok := exitCode(err); !ok {
			t.Fatalf("db guard: cannot run the child %s: %v", os.Args[0], err)
		}
	}
	return string(out), err
}

// guardChildEnvFor is the child's whole environment: this process's, minus the
// three variables that decide the outcome, plus the ones this case asks for.
//
// Filtering rather than appending is the part that matters. The parent may
// itself be running with POST_REQUIRE_E2E_DB set — the CI job that owns a
// database sets it for the whole package, which is the point of it — and
// without the filter the "unset skips" case would be decided by the parent's
// environment rather than by the case.
func guardChildEnvFor(journey string, require bool, deadDSN string) []string {
	deciding := []string{testdb.RequireE2EDBEnv, "POSTGRES_TEST_ADMIN_URL", guardChildEnv}
	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		if !hasAnyPrefix(kv, deciding...) {
			env = append(env, kv)
		}
	}
	env = append(env, "POSTGRES_TEST_ADMIN_URL="+deadDSN, guardChildEnv+"="+journey)
	if require {
		env = append(env, testdb.RequireE2EDBEnv+"=1")
	}
	return env
}

func hasAnyPrefix(kv string, names ...string) bool {
	for _, n := range names {
		if strings.HasPrefix(kv, n+"=") {
			return true
		}
	}
	return false
}

// deadPortDSN returns a DSN on a loopback port nothing is listening on. The
// port is one the kernel hands out for a listener that is then closed, so the
// test does not depend on a hand-picked number staying free; and if something
// did answer there, it would not be a PostgreSQL answering a ping, so the probe
// still reports unreachable.
func deadPortDSN(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("db guard: no free loopback port to point the probe at: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("db guard: cannot close the probe port %d: %v", port, err)
	}
	return fmt.Sprintf("postgres://postgres:postgres_dev_pw@127.0.0.1:%d/dead", port)
}

// verdictOf reports which verdict line the child printed, or "" for none.
func verdictOf(out string) string {
	for _, v := range []string{"--- FAIL: ", "--- SKIP: ", "--- PASS: "} {
		if strings.Contains(out, v) {
			return v
		}
	}
	return ""
}

func verdictPhrase(verdict string) string {
	switch verdict {
	case "--- FAIL: ":
		return "failed"
	case "--- SKIP: ":
		return "skipped"
	case "--- PASS: ":
		return "passed"
	default:
		return "reached no verdict"
	}
}

// exitCode reports the child's exit status, and whether it exited at all.
func exitCode(err error) (int, bool) {
	if err == nil {
		return 0, true
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return 0, false
	}
	return exit.ExitCode(), true
}
