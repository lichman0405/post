package testdb

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RequireE2EDBEnv is the environment variable that turns "a database would be
// convenient here" into "a database must be here".
//
// It exists because the other outcome is silent. `go test` prints one "ok"
// line for a package no matter how many of its tests skipped, so a green job is
// no evidence that a database-dependent test ran. tests/e2e is exactly that
// shape: 30 of its tests are in-process (httptest + miniredis + memstore) and
// two need a real PostgreSQL — TestE2EConflictResolution and
// TestE2EDiscussionPromotion — and measured 2026-09-23 against b6c1fb1,
// pointing the DSN at a dead port like CI's database-less `go` job does:
//
//	POSTGRES_TEST_ADMIN_URL=...@127.0.0.1:59999/dead go test ./tests/e2e -count=1 -v
//	→ exit 0; 30 --- PASS, 2 --- SKIP, "ok … 2.160s"
//
// i.e. neither journey had ever run in CI, and the job said green anyway. The
// job that does own a database (ci.yml's migration-integration) therefore sets
// this variable, and there an unreachable database fails the run instead of
// being absorbed by a skip.
//
// The name lives here, exported, and nowhere else in the tree: a second copy
// read at a call site is how two journeys start reading the environment
// differently. Unset, nothing changes at all — the journeys skip with the same
// message they have always printed, which is what lets a laptop without a
// database still run the other 30 tests.
//
// The value grammar: unset and "0" mean "not required"; anything else means
// required. A value nobody meant as "off" — a typo, a true — therefore fails
// loudly rather than quietly protecting nothing, which is the failure mode
// this variable exists to remove.
const RequireE2EDBEnv = "POST_REQUIRE_E2E_DB"

// reachableTimeout bounds one reachability probe, so a DSN that blackholes
// (a firewall answering nothing rather than refusing the connection) delays a
// skip by two seconds and not by the connection timeout.
const reachableTimeout = 2 * time.Second

// reachable reports whether a PostgreSQL server answers a ping at adminURL
// within reachableTimeout.
//
// This is the probe that used to sit in tests/e2e (pgReachable). It moved here
// because the question "is there a database?" is asked immediately before
// Setup, which is this package's — so the probe, the skip and the migration of
// the database the journey is about to use are answered by one layer, with one
// message shape, and the two journeys cannot drift apart on what an
// unreachable database means.
func reachable(ctx context.Context, adminURL string) bool {
	ctx, cancel := context.WithTimeout(ctx, reachableTimeout)
	defer cancel()
	pool, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		return false
	}
	defer pool.Close()
	return pool.Ping(ctx) == nil
}

// requiredDB returns the value of RequireE2EDBEnv when this run declared that a
// database must be reachable, and "" when it did not (unset or an explicit
// "0").
func requiredDB() string {
	v := strings.TrimSpace(os.Getenv(RequireE2EDBEnv))
	if v == "" || v == "0" {
		return ""
	}
	return v
}

// RequireDB is the one judgement about what an unreachable database means for a
// test that needs one: skip — this environment never promised a database — or
// failure — it did, through RequireE2EDBEnv.
//
// Call it as the first statement of a test that needs a real database, before
// anything connects:
//
//	testdb.RequireDB(t, ctx, adminURL, "conflict e2e", "the resolution journey needs a real database")
//
// journey is how the suite names the test in its own words; needs says what the
// database is for; adminURL is the DSN the test is about to connect to.
//
// With RequireE2EDBEnv unset this is byte-for-byte the skip the suite printed
// before T1212 — same message, same %s slots — so the default behaviour of a
// machine without a database is unchanged. With it set, an unreachable database
// is a failure whose message names the journey, the DSN it could not reach, the
// variable that made skipping impossible, its value, and the test — the four
// facts needed to tell "the database is down" apart from "nobody ever ran this".
func RequireDB(t *testing.T, ctx context.Context, adminURL, journey, needs string) {
	t.Helper()
	if reachable(ctx, adminURL) {
		return
	}
	if value := requiredDB(); value != "" {
		t.Fatalf("%s: PostgreSQL unreachable at %s — this environment declared that a real database is required "+
			"(%s=%s), so %s failing to run is a failure and not a skip; "+
			"start the stack (make infra-up) or set POSTGRES_TEST_ADMIN_URL and the test runs",
			journey, adminURL, RequireE2EDBEnv, value, t.Name())
	}
	t.Skipf("%s: PostgreSQL unreachable at %s — %s; "+
		"start the stack (make infra-up) or set POSTGRES_TEST_ADMIN_URL and the test runs",
		journey, adminURL, needs)
}
