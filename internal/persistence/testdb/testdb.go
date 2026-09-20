// Package testdb gives integration tests a task/run-scoped PostgreSQL
// namespace, per docs/66 §3: every test database is named
//
//	test_<task_id>_<run_id>
//
// where run_id embeds a wall-clock timestamp and a random suffix, so parallel
// test runs (or a Supervisor acceptance re-run while a Worker run is still
// alive) can never collide. Databases are dropped again during t.Cleanup —
// nothing is left behind (docs/66 §4 forbids Workers from pruning anything
// that is not their own).
//
// # Why a template database (T0815)
//
// Setup used to CREATE DATABASE and then apply the whole migration set, once
// per test. Measured on 2026-09-19 over one full `make test-integration` run
// (382 tests), with the per-phase counters this package now reports itself:
//
//	377 databases created; CREATE DATABASE 11.1s, Migrate 125.6s of a 328.6s run
//
// i.e. 41.6% of the CI job was the same 69 migrations applied 377 times — some
// 26,000 DDL transactions, every one of them paying its own WAL write, catalog
// update and fsync. That is a cost that scales with how *contended* the runner
// is rather than with what the suite tests, which is why a byte-identical tree
// could take 320s on one GitHub runner and 710s on another while no single
// test was stuck: nothing hangs, the whole job just gets slower everywhere.
//
// A migrated database is schema-only — no migration seeds test data, and the
// template is never written to after Migrate returns — so Setup now migrates
// ONE template per (task, admin URL) per run and gives each test a
// file-level clone of it (`CREATE DATABASE ... TEMPLATE`), which PostgreSQL
// implements as a copy of the template's files rather than a replay of the
// DDL. Measured: 60ms for a clone of the full 707-relation schema against
// 333ms to migrate. What a test receives is unchanged — a database at head,
// with the same catalog, the same goose_db_version row and the same empty
// tables a fresh migrate would have produced.
//
// The four tests whose subject IS the migration set keep the real thing:
// they call SetupFromScratch, which is the old body verbatim.
//
// # Every package that calls Setup must install the cleanup hook
//
// The template outlives the individual tests, so no t.Cleanup can drop it;
// only the end of the process can. Every test package that calls Setup
// therefore needs, in a file of its own:
//
//	func TestMain(m *testing.M) { testdb.Main(m) }
//
// Setup refuses to build a template without it (see Main), because the
// alternative is a template database that silently outlives the run that made
// it — which is exactly how T0815 leaked two of them from tests/e2e.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/persistence"
)

// cleanupInstalled records that Main ran, i.e. that this process will drop its
// templates. Building a template without it is a leak, and Setup says so.
var cleanupInstalled atomic.Bool

// Main runs a test package that uses Setup. A package's TestMain is exactly:
//
//	func TestMain(m *testing.M) { testdb.Main(m) }
//
// It drops this run's migrated templates, closes the cached admin pools, and
// prints where the run's database provisioning time went.
//
// The reporting exists because that cost is otherwise invisible: getting a
// test its database is spread over hundreds of call sites, so it never shows
// up in any one test's duration. Before T0815 the provisioning was 41.6% of
// the whole `make test-integration` job (measured 2026-09-19: 11.1s of CREATE
// DATABASE and 125.6s of Migrate in a 328.6s run) and the only way to see that
// was to instrument this helper.
//
// `go test` throws away a *passing* test binary's output, so stderr alone
// would leave `make test-integration` printing "ok ... 247s" and silently
// discarding the number that says where the 247 seconds went. The Makefile
// hands over a path in POST_TESTDB_STATS and prints what lands in it; stderr
// keeps the line for `-v` runs and for failures.
//
// Best effort by construction: a run killed by `go test -timeout` never gets
// here, and leaves a template named test_<task>_<run id>_tpl behind — inert,
// run-scoped, and droppable by name. Nothing sweeps it automatically:
// `rddev env reset --test-only` is an honest stub that exits 4
// (cmd/rddev/env.go), so a leaked template needs an explicit DROP DATABASE.
func Main(m *testing.M) {
	cleanupInstalled.Store(true)

	code := m.Run()

	report := "testdb: " + Provisioning()
	if err := Close(context.Background()); err != nil {
		report = fmt.Sprintf("testdb: cleanup: %v\n%s", err, report)
	}
	report += "\n"
	fmt.Fprint(os.Stderr, report)

	if path := os.Getenv("POST_TESTDB_STATS"); path != "" {
		_ = os.WriteFile(path, []byte(report), 0o644)
	}

	os.Exit(code)
}

// ---------------------------------------------------------------------------
// Run identity

// runID is this test binary's run-scoped identifier, computed once so every
// database the process creates shares one namespace — and so two runs, in
// parallel or in sequence, share nothing.
var runID = sync.OnceValue(func() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("testdb: entropy source failed: %v", err))
	}
	return time.Now().UTC().Format("20060102T150405") + "_" + hex.EncodeToString(b[:])
})

// DatabaseName returns the namespaced database name for a fresh run.
//
// A fresh name every call is deliberate: SetupEmpty is called several times
// inside one test (the upgrade-path cases walk a database through versions),
// and each of those databases has to be a different one.
func DatabaseName(taskID string) string {
	return fmt.Sprintf("test_%s_%s", taskID, freshSuffix())
}

// templateName is the run-scoped name of taskID's migrated template. Unlike
// DatabaseName it must be stable within the run — that is the whole point —
// and it carries both the task id and the run id so it obeys docs/66 §3 and
// can be told apart from another run's template.
func templateName(taskID string) string {
	return fmt.Sprintf("test_%s_%s_tpl", taskID, runID())
}

func freshSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("testdb: entropy source failed: %v", err))
	}
	return time.Now().UTC().Format("20060102T150405") + "_" + hex.EncodeToString(b[:])
}

// ---------------------------------------------------------------------------
// Provisioning accounting
//
// This is the instrument T0815 was asked for: the cost of getting a test its
// database used to be invisible — it is spread over 377 call sites, so it
// shows up only as "the suite is slow" and never as anything a profile points
// at. Every phase is counted, and TestMain prints the summary when the run
// ends, so the next person who asks "where did the ten minutes go" has an
// answer without instrumenting anything.

type provisioningStats struct {
	mu         sync.Mutex
	create     time.Duration // CREATE DATABASE, clone or empty
	migrate    time.Duration // applying the migration set
	drop       time.Duration // DROP DATABASE during cleanup
	databases  int           // test databases made for a test
	templates  int           // migrated templates built
	migrations int           // times the migration set was applied
}

var provisioning provisioningStats

// Provisioning returns the one-line summary of the time this process spent
// creating, migrating and dropping test databases.
func Provisioning() string {
	provisioning.mu.Lock()
	defer provisioning.mu.Unlock()
	return fmt.Sprintf("databases=%d templates=%d migrations=%d create=%s migrate=%s drop=%s",
		provisioning.databases, provisioning.templates, provisioning.migrations,
		provisioning.create.Round(time.Millisecond),
		provisioning.migrate.Round(time.Millisecond),
		provisioning.drop.Round(time.Millisecond))
}

// countDatabaseCreated records a CREATE DATABASE that becomes a test's own
// database — either a clone of the template or, for the from-scratch tests, a
// fresh empty one.
func (s *provisioningStats) countDatabaseCreated(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.create += d
	s.databases++
}

// countTemplateCreated records the one CREATE DATABASE per migrated template.
// It is deliberately not a "database": it is not a test's namespace, and the
// whole point of the number is to see templates and test databases apart.
func (s *provisioningStats) countTemplateCreated(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.create += d
	s.templates++
}

// countMigrated records one application of the migration set, whether to a
// template or to a from-scratch test database. This is the number that used
// to equal the test count.
func (s *provisioningStats) countMigrated(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.migrate += d
	s.migrations++
}

func (s *provisioningStats) countDropped(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drop += d
}

// ---------------------------------------------------------------------------
// The per-run migrated template

type templateKey struct{ adminURL, taskID string }

type templateState struct {
	adminURL, name string
	err            error
}

var (
	templatesMu sync.Mutex
	templates   = map[templateKey]*templateState{}
)

// Setup connects to adminURL (the maintenance database, usually .../post),
// creates a namespaced database test_<taskID>_<runID> as a clone of the run's
// already-migrated template for taskID, and returns a pool for it plus the
// database's URL. The database is dropped (WITH (FORCE), so lingering pooled
// connections cannot block it) when the test finishes.
//
// The clone and a fresh `Migrate` on an empty database are the same database:
// same relations, same constraints and indexes, same extensions, same
// goose_db_version rows, same empty tables and same sequence values, because
// the template was built by exactly that fresh migrate and is never written
// to afterwards. A test verifies behaviour on a migrated database; the four
// tests that verify the *migration* call SetupFromScratch instead.
//
// adminURL must point at a database the caller may CREATE DATABASE in.
func Setup(t *testing.T, ctx context.Context, adminURL, taskID string) (*pgxpool.Pool, string) {
	t.Helper()
	template := ensureTemplate(t, ctx, adminURL, taskID)

	name := DatabaseName(taskID)
	cloneDatabase(t, ctx, adminURL, name, template)
	t.Cleanup(func() {
		start := time.Now()
		dropDatabase(context.Background(), adminURL, name)
		provisioning.countDropped(time.Since(start))
	})
	return openFor(t, ctx, adminURL, name)
}

// SetupFromScratch is Setup without the template: it creates the database and
// applies the whole migration set to it, in this database, now.
//
// It is for the tests whose subject is the migration set itself — a fresh
// install from empty, the no-op re-migrate, and the upgraded-vs-fresh
// comparison. Every other test wants a database at head, not a re-run of the
// DDL that got it there, and pays 5x less for saying so.
func SetupFromScratch(t *testing.T, ctx context.Context, adminURL, taskID string) (*pgxpool.Pool, string) {
	t.Helper()
	name := DatabaseName(taskID)
	createDatabase(t, ctx, adminURL, name)
	t.Cleanup(func() {
		start := time.Now()
		dropDatabase(context.Background(), adminURL, name)
		provisioning.countDropped(time.Since(start))
	})

	url, err := WithDatabase(adminURL, name)
	if err != nil {
		t.Fatalf("testdb: rewrite URL for %s: %v", name, err)
	}
	start := time.Now()
	if _, err := persistence.Migrate(ctx, url); err != nil {
		t.Fatalf("testdb: migrate %s: %v", name, err)
	}
	provisioning.countMigrated(time.Since(start))
	return openFor(t, ctx, adminURL, name)
}

// SetupEmpty is Setup without migrating: for the upgrade-path test, which
// starts below head on purpose.
func SetupEmpty(t *testing.T, ctx context.Context, adminURL, taskID string) (pool *pgxpool.Pool, url string) {
	t.Helper()
	name := DatabaseName(taskID)
	createDatabase(t, ctx, adminURL, name)
	t.Cleanup(func() {
		start := time.Now()
		dropDatabase(context.Background(), adminURL, name)
		provisioning.countDropped(time.Since(start))
	})
	return openFor(t, ctx, adminURL, name)
}

// WithDatabase returns url with its database name replaced by name.
//
// NB: pgx's ConnConfig.ConnString() returns the *original* string captured at
// parse time, so a rewritten config does not survive that round trip — the
// URL itself is rewritten here instead.
func WithDatabase(urlStr, name string) (string, error) {
	u, err := url.Parse(urlStr)
	if err != nil {
		return "", err
	}
	u.Path = "/" + name
	return u.String(), nil
}

// Close drops every template database this process created and closes the
// cached admin pools. TestMain calls it when the run ends.
//
// It is best effort by construction: a run killed by `go test -timeout`
// never reaches TestMain, and a template left behind then is named
// test_<task>_<run id>_tpl — inert (nothing connects to it, nothing lists it)
// and identifiable by name, so it can be dropped by hand. `rddev env reset
// --test-only` is NOT a fallback for this: it is an honest stub that exits 4
// (cmd/rddev/env.go), so a leaked template needs an explicit DROP DATABASE.
func Close(ctx context.Context) error {
	templatesMu.Lock()
	built := make([]*templateState, 0, len(templates))
	for _, st := range templates {
		if st.err == nil && st.name != "" {
			built = append(built, st)
		}
	}
	templates = map[templateKey]*templateState{}
	templatesMu.Unlock()

	var errs []error
	for _, st := range built {
		admin, err := adminPoolFor(ctx, st.adminURL)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		start := time.Now()
		if _, err := admin.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, st.name)); err != nil {
			errs = append(errs, fmt.Errorf("drop template %s: %w", st.name, err))
		}
		provisioning.countDropped(time.Since(start))
	}
	closeAdminPools()
	return errors.Join(errs...)
}

// ensureTemplate returns taskID's migrated template, building it on first
// use. A template that could not be built fails every test that needs it with
// the migrator's own error — the suite cannot quietly run against a database
// that is not at head.
func ensureTemplate(t *testing.T, ctx context.Context, adminURL, taskID string) string {
	t.Helper()
	// Refusing here is the whole defence against the leak T0815 shipped in its
	// first draft: tests/e2e called Setup and had no TestMain, so a template
	// nobody would ever drop got built and left behind, and nothing said so.
	// A panic or a skip would be quieter; this fails the run with the fix.
	if !cleanupInstalled.Load() {
		t.Fatalf("testdb: this package calls Setup but has no TestMain calling testdb.Main, "+
			"so the migrated template for %s would outlive the run and leak. Add:\n\n"+
			"\tfunc TestMain(m *testing.M) { testdb.Main(m) }\n", taskID)
	}
	name, err := templateFor(ctx, adminURL, taskID)
	if err != nil {
		t.Fatalf("testdb: build the migrated template for %s: %v", taskID, err)
	}
	return name
}

func templateFor(ctx context.Context, adminURL, taskID string) (string, error) {
	key := templateKey{adminURL: adminURL, taskID: taskID}

	templatesMu.Lock()
	defer templatesMu.Unlock()
	if st, ok := templates[key]; ok {
		return st.name, st.err
	}

	// Reserved before the work starts so a second caller — today only
	// reachable through t.Parallel, but the concurrency is cheap to make
	// correct — blocks here and sees the finished template instead of
	// racing a second CREATE DATABASE.
	st := &templateState{adminURL: adminURL, name: templateName(taskID)}
	templates[key] = st
	if err := buildTemplate(ctx, adminURL, st.name); err != nil {
		st.name, st.err = "", err
	}
	return st.name, st.err
}

func buildTemplate(ctx context.Context, adminURL, name string) error {
	url, err := WithDatabase(adminURL, name)
	if err != nil {
		return err
	}
	start := time.Now()
	if err := createDatabaseRaw(ctx, adminURL, name, ""); err != nil {
		return err
	}
	provisioning.countTemplateCreated(time.Since(start))
	start = time.Now()
	if _, err := persistence.Migrate(ctx, url); err != nil {
		return fmt.Errorf("migrate template %s: %w", name, err)
	}
	provisioning.countMigrated(time.Since(start))

	// Migrate closes its own database/sql pool before returning, so the
	// template should already have no sessions on it; this is the belt to
	// that brace. CREATE DATABASE ... TEMPLATE is refused outright while any
	// session is attached to the template, and one straggler here would be a
	// hard failure in the very first test that clones it — which is exactly
	// the kind of intermittency this change exists to remove.
	admin, err := adminPoolFor(ctx, adminURL)
	if err != nil {
		return err
	}
	if _, err := admin.Exec(ctx,
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		  WHERE datname = $1 AND pid <> pg_backend_pid()`, name); err != nil {
		return fmt.Errorf("detach sessions from template %s: %w", name, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Small helpers

func openFor(t *testing.T, ctx context.Context, adminURL, name string) (*pgxpool.Pool, string) {
	t.Helper()
	url, err := WithDatabase(adminURL, name)
	if err != nil {
		t.Fatalf("testdb: rewrite URL for %s: %v", name, err)
	}
	pool, err := persistence.Open(ctx, url)
	if err != nil {
		t.Fatalf("testdb: open pool for %s: %v", name, err)
	}
	t.Cleanup(pool.Close)
	return pool, url
}

// cloneDatabase gives name a file-level copy of template's contents.
func cloneDatabase(t *testing.T, ctx context.Context, adminURL, name, template string) {
	t.Helper()
	start := time.Now()
	if err := createDatabaseRaw(ctx, adminURL, name, template); err != nil {
		t.Fatalf("testdb: clone %s from template %s: %v", name, template, err)
	}
	provisioning.countDatabaseCreated(time.Since(start))
}

// createDatabase creates an empty database (from template1) and fails the
// test if it cannot.
func createDatabase(t *testing.T, ctx context.Context, adminURL, name string) {
	t.Helper()
	start := time.Now()
	if err := createDatabaseRaw(ctx, adminURL, name, ""); err != nil {
		t.Fatalf("testdb: create database %s: %v", name, err)
	}
	provisioning.countDatabaseCreated(time.Since(start))
}

// createDatabaseRaw is the one place that issues CREATE DATABASE. Names come
// from a controlled alphabet (task id, run id, hex), so nothing a caller
// passes can escape the %q quoting.
func createDatabaseRaw(ctx context.Context, adminURL, name, template string) error {
	admin, err := adminPoolFor(ctx, adminURL)
	if err != nil {
		return err
	}
	stmt := fmt.Sprintf(`CREATE DATABASE %q`, name)
	if template != "" {
		stmt += fmt.Sprintf(` TEMPLATE %q`, template)
	}
	if _, err := admin.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("create database %s: %w", name, err)
	}
	return nil
}

func dropDatabase(ctx context.Context, adminURL, name string) {
	admin, err := adminPoolFor(ctx, adminURL)
	if err != nil {
		return // nothing sane to do without a connection; the run ends anyway
	}
	_, _ = admin.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, name))
}

// ---------------------------------------------------------------------------
// The admin pool
//
// One pool per admin URL for the whole process. It used to be a fresh
// pgxpool.New per CREATE and per DROP — 377 creates and 384 drops, each
// paying a connect and a password handshake. It holds sessions only on the
// maintenance database, which is not a database any test creates or drops, so
// it cannot block a DROP DATABASE or a CREATE DATABASE ... TEMPLATE.

var (
	adminMu    sync.Mutex
	adminPools = map[string]*pgxpool.Pool{}
)

func adminPoolFor(ctx context.Context, adminURL string) (*pgxpool.Pool, error) {
	adminMu.Lock()
	defer adminMu.Unlock()
	if p, ok := adminPools[adminURL]; ok {
		return p, nil
	}
	p, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		return nil, fmt.Errorf("connect admin %s: %w", adminURL, err)
	}
	adminPools[adminURL] = p
	return p, nil
}

func closeAdminPools() {
	adminMu.Lock()
	defer adminMu.Unlock()
	for _, p := range adminPools {
		p.Close()
	}
	adminPools = map[string]*pgxpool.Pool{}
}
