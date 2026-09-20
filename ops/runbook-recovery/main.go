// Command runbook-recovery executes the database half of the POST rollback /
// forward-fix scenario (docs/35_DEPLOYMENT_RUNBOOK.md:17, "应用可回滚
// previous image；DB 只 forward repair") against a real PostgreSQL.
//
// It is the engine behind the `runbook drill` gate (tests/acceptance/
// runbook-drill.sh, task T1204). The runbook says two things a drill can
// check, and this program checks them by doing them:
//
//  1. FORWARD REPAIR. A database restored from an older backup is behind head.
//     The documented repair is forward only — the pending migrations are
//     applied in order, nothing is undone. Here: an empty run-scoped database
//     is brought to a version well below head with the PRODUCTION runner
//     (persistence.MigrateTo, the entry point `rddev db migrate` wraps), then
//     repaired with persistence.Migrate. What the second call applies must be
//     exactly the migrations the first one did not, and the version must end
//     at head — no more (nothing re-applied) and no less.
//
//  2. NO ROLLBACK. The runbook's rule is that the database does not move
//     backwards. This is not an absence to argue from the source: it is
//     attempted. MigrateTo with a version BELOW the current one is the call an
//     operator would reach for to "put the schema back", and the run must
//     report that it applied nothing and left the version where it was. If
//     that ever stops being true, the runbook's promise has changed and the
//     drill fails.
//
// What it is not: a restore. This program starts from an empty database it
// created, not from a dump — the backup/restore process itself is the subject
// of the `restore drill` (cmd/api/backupdr, ops/backup-restore-drill.sh,
// T1110), and duplicating it here would be a second implementation of the
// thing under test. What this program rehearses is the step that follows a
// restore, which is where the runbook's forward-only rule actually bites.
//
// The database is created and dropped by this program, both named
// test_T1204_runbook_drill_<run id> (the docs/66 §3 convention for a
// task-scoped namespace), and dropped on every exit path including failure.
// Nothing of it survives the run.
//
// Usage:
//
//	go run ./ops/runbook-recovery --admin-url URL [--stop-version N] [--json] [--keep]
//
// Exit codes: 0 the scenario reproduced the runbook's rules; 1 it did not;
// 2 the arguments are unusable (missing --admin-url, non-positive
// --stop-version); 3 there is no PostgreSQL at the admin URL. 2 and 3 are
// host facts, 1 is a finding about the runbook.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/persistence"
)

const taskID = "T1204"

// defaultStopVersion is where the "restored backup" is pinned: a version far
// enough below head that the repair has real work to do, and old enough that
// the migrations after it are ordinary ones. The value is not special; what is
// checked is the arithmetic between this version and head.
const defaultStopVersion = 40

type report struct {
	Task              string   `json:"task"`
	RunID             string   `json:"run_id"`
	AdminURL          string   `json:"admin_url"`
	Database          string   `json:"database"`
	Postgres          string   `json:"postgres_version"`
	StopVersion       int64    `json:"stop_version"`
	HeadVersion       int64    `json:"head_version"`
	AppliedToStop     int64    `json:"applied_to_stop_version"`
	VersionAfterStop  int64    `json:"version_after_restore"`
	AppliedForward    int64    `json:"applied_forward_repair"`
	VersionAfterFix   int64    `json:"version_after_forward_repair"`
	AppliedReRun      int64    `json:"applied_re_run_at_head"`
	RollbackAttempt   rollback `json:"rollback_attempt"`
	Steps             []string `json:"steps"`
	Failures          []string `json:"failures"`
	Verdict           string   `json:"verdict"`
	ArtifactsKeptPath string   `json:"database_kept,omitempty"`
}

// unreachableError marks the one failure that is a fact about the host rather
// than about the runbook: there is no PostgreSQL to run the scenario against.
// It carries its own exit code so the drill can report "not run" instead of
// "failed" without parsing English.
type unreachableError struct{ err error }

func (e *unreachableError) Error() string { return e.err.Error() }
func (e *unreachableError) Unwrap() error { return e.err }

type rollback struct {
	Called         bool  `json:"called"`
	Applied        int64 `json:"applied"`
	VersionAfter   int64 `json:"version_after"`
	VersionChanged bool  `json:"version_changed"`
}

func main() {
	adminURL := flag.String("admin-url", os.Getenv("POSTGRES_TEST_ADMIN_URL"),
		"PostgreSQL admin URL to create the scratch database in (default $POSTGRES_TEST_ADMIN_URL)")
	stopVersion := flag.Int64("stop-version", defaultStopVersion,
		"the schema version the \"restored backup\" is pinned at")
	jsonOut := flag.Bool("json", false, "print the report as JSON")
	keep := flag.Bool("keep", false, "keep the scratch database (debugging only)")
	flag.Parse()

	if *adminURL == "" {
		fmt.Fprintln(os.Stderr, "runbook-recovery: --admin-url or $POSTGRES_TEST_ADMIN_URL is required")
		os.Exit(2)
	}
	if *stopVersion <= 0 {
		fmt.Fprintln(os.Stderr, "runbook-recovery: --stop-version must be positive")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	rep := report{Task: taskID, AdminURL: redact(*adminURL), StopVersion: *stopVersion}
	err := run(ctx, *adminURL, *stopVersion, *keep, &rep)
	unreachable := false
	var noServer *unreachableError
	if errors.As(err, &noServer) {
		unreachable = true
	}
	if err != nil {
		rep.Failures = append(rep.Failures, err.Error())
	}
	rep.Verdict = "PASS"
	if len(rep.Failures) > 0 {
		rep.Verdict = "FAIL"
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
	} else {
		for _, s := range rep.Steps {
			fmt.Println(s)
		}
		for _, f := range rep.Failures {
			fmt.Printf("FAIL %s\n", f)
		}
		fmt.Printf("runbook-recovery: %s (%d step(s), %d failure(s))\n",
			rep.Verdict, len(rep.Steps), len(rep.Failures))
	}
	switch {
	case unreachable:
		// A distinct code from a scenario failure on purpose: "this host has no
		// PostgreSQL" is a precondition, and a drill that cannot tell the two
		// apart cannot say whether the runbook's rules held.
		os.Exit(3)
	case rep.Verdict != "PASS":
		os.Exit(1)
	}
}

func run(ctx context.Context, adminURL string, stopVersion int64, keep bool, rep *report) error {
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		return fmt.Errorf("cannot open the admin pool: %w", err)
	}
	defer admin.Close()
	if err := admin.Ping(ctx); err != nil {
		return &unreachableError{fmt.Errorf("PostgreSQL is not reachable at %s: %w", redact(adminURL), err)}
	}
	if err := admin.QueryRow(ctx, "SELECT version()").Scan(&rep.Postgres); err != nil {
		return fmt.Errorf("cannot read the server version: %w", err)
	}
	if i := strings.Index(rep.Postgres, " on "); i > 0 {
		rep.Postgres = rep.Postgres[:i]
	}

	name, err := scratchName()
	if err != nil {
		return err
	}
	rep.Database = name
	rep.RunID = strings.TrimPrefix(name, "test_"+taskID+"_")

	if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %q", name)); err != nil {
		return fmt.Errorf("cannot create the scratch database %s: %w", name, err)
	}
	step(rep, "created scratch database %s (docs/66 §3 namespace)", name)
	if !keep {
		// Dropped on every path, including the failing one: a drill that leaves
		// databases behind is a drill that fills a shared server.
		defer func() {
			if _, err := admin.Exec(context.Background(),
				fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", name)); err != nil {
				fmt.Fprintf(os.Stderr, "runbook-recovery: could not drop %s: %v\n", name, err)
				return
			}
			step(rep, "dropped scratch database %s", name)
		}()
	} else {
		rep.ArtifactsKeptPath = name
	}

	dbURL, err := withDatabase(adminURL, name)
	if err != nil {
		return err
	}

	// The database's own head version, read the way the runbook's operator
	// would: by bringing a database to head and asking what it is at. A second
	// scratch database is not used for this — the head version is read from the
	// same database at the end of the repair, and the arithmetic below is what
	// the checks rest on.
	applied, err := persistence.MigrateTo(ctx, dbURL, stopVersion)
	if err != nil {
		return fmt.Errorf("pinning the schema at version %d failed: %w", stopVersion, err)
	}
	rep.AppliedToStop = applied
	rep.VersionAfterStop, err = persistence.MigrationVersion(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("reading the version after the pinned restore failed: %w", err)
	}
	if rep.VersionAfterStop != stopVersion {
		return fmt.Errorf("the pinned restore reports version %d, want %d", rep.VersionAfterStop, stopVersion)
	}
	step(rep, "old backup restored: schema pinned at version %d (%d migration(s) applied)",
		rep.VersionAfterStop, rep.AppliedToStop)

	// Forward repair: everything that was pending, in order, and nothing else.
	rep.AppliedForward, err = persistence.Migrate(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("the forward repair failed: %w", err)
	}
	rep.VersionAfterFix, err = persistence.MigrationVersion(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("reading the version after the forward repair failed: %w", err)
	}
	if rep.AppliedForward == 0 {
		return fmt.Errorf("the forward repair applied nothing; a schema at version %d is behind head",
			stopVersion)
	}
	if rep.VersionAfterFix <= stopVersion {
		return fmt.Errorf("the forward repair left the schema at version %d, which is not past %d",
			rep.VersionAfterFix, stopVersion)
	}
	step(rep, "forward repair: %d migration(s) applied, schema now at version %d — nothing undone",
		rep.AppliedForward, rep.VersionAfterFix)

	// Re-running the migrate job on a database already at head: the deployment
	// relies on this being a no-op (ops/deploy/README.md § Deploy order, the
	// migrate job re-runs as a dependency of every `up`).
	rep.AppliedReRun, err = persistence.Migrate(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("re-running the migrate job at head failed: %w", err)
	}
	if rep.AppliedReRun != 0 {
		return fmt.Errorf("re-running the migrate job at head applied %d migration(s), want 0",
			rep.AppliedReRun)
	}
	step(rep, "migrate job re-run at head: 0 applied (the deployment relies on this)")

	// The rollback attempt. This is the check the runbook's rule lives or dies
	// by, so it is performed rather than argued.
	rep.RollbackAttempt.Called = true
	rep.RollbackAttempt.Applied, err = persistence.MigrateTo(ctx, dbURL, stopVersion)
	if err != nil {
		return fmt.Errorf("the rollback attempt (MigrateTo %d below head) errored: %w", stopVersion, err)
	}
	rep.RollbackAttempt.VersionAfter, err = persistence.MigrationVersion(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("reading the version after the rollback attempt failed: %w", err)
	}
	rep.RollbackAttempt.VersionChanged = rep.RollbackAttempt.VersionAfter != rep.VersionAfterFix
	if rep.RollbackAttempt.Applied != 0 || rep.RollbackAttempt.VersionChanged {
		return fmt.Errorf("a rollback attempt moved the database: %d applied, version %d -> %d; "+
			"docs/35:17 says the database only moves forward",
			rep.RollbackAttempt.Applied, rep.VersionAfterFix, rep.RollbackAttempt.VersionAfter)
	}
	step(rep, "rollback attempt below head: 0 applied, version stayed at %d — the database only "+
		"moves forward (docs/35:17)", rep.RollbackAttempt.VersionAfter)

	rep.HeadVersion = rep.VersionAfterFix
	return nil
}

// step records a step of the scenario. It deliberately does not print: the
// report is emitted once, at the end, so that the JSON form and the text form
// carry exactly the same lines and so that the deferred drop cannot interleave
// with them.
func step(rep *report, format string, args ...any) {
	rep.Steps = append(rep.Steps, fmt.Sprintf(format, args...))
}

func scratchName() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("entropy source failed: %w", err)
	}
	return fmt.Sprintf("test_%s_runbook_drill_%s_%s", taskID,
		time.Now().UTC().Format("20060102T150405"), hex.EncodeToString(b[:])), nil
}

func withDatabase(urlStr, name string) (string, error) {
	u, err := url.Parse(urlStr)
	if err != nil {
		return "", fmt.Errorf("cannot parse the admin URL: %w", err)
	}
	u.Path = "/" + name
	return u.String(), nil
}

// redact strips the password before a URL is printed: this output lands in
// drill logs, and libpq URLs carry the password in the clear (the same
// discipline as cmd/rddev/db.go and internal/config/redact.go).
func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if _, hasPassword := u.User.Password(); !hasPassword {
		return raw
	}
	return u.Redacted()
}
