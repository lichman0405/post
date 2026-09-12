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
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/persistence"
)

// Setup connects to adminURL (the maintenance database, usually .../post),
// creates a namespaced database test_<taskID>_<runID>, migrates it to head
// and returns a pool for it plus the migrated database's URL. The database is
// dropped (WITH (FORCE), so lingering pooled connections cannot block it)
// when the test finishes.
//
// adminURL must point at a database the caller may CREATE DATABASE in.
func Setup(t *testing.T, ctx context.Context, adminURL, taskID string) (*pgxpool.Pool, string) {
	t.Helper()
	name := DatabaseName(taskID)
	createDatabase(t, ctx, adminURL, name)
	t.Cleanup(func() { dropDatabase(context.Background(), adminURL, name) })

	url, err := WithDatabase(adminURL, name)
	if err != nil {
		t.Fatalf("testdb: rewrite URL for %s: %v", name, err)
	}
	if _, err := persistence.Migrate(ctx, url); err != nil {
		t.Fatalf("testdb: migrate %s: %v", name, err)
	}
	pool, err := persistence.Open(ctx, url)
	if err != nil {
		t.Fatalf("testdb: open pool for %s: %v", name, err)
	}
	t.Cleanup(pool.Close)
	return pool, url
}

// SetupEmpty is Setup without migrating: for the upgrade-path test, which
// starts below head on purpose.
func SetupEmpty(t *testing.T, ctx context.Context, adminURL, taskID string) (pool *pgxpool.Pool, url string) {
	t.Helper()
	name := DatabaseName(taskID)
	createDatabase(t, ctx, adminURL, name)
	t.Cleanup(func() { dropDatabase(context.Background(), adminURL, name) })

	url, err := WithDatabase(adminURL, name)
	if err != nil {
		t.Fatalf("testdb: rewrite URL for %s: %v", name, err)
	}
	pool, err = persistence.Open(ctx, url)
	if err != nil {
		t.Fatalf("testdb: open pool for %s: %v", name, err)
	}
	t.Cleanup(pool.Close)
	return pool, url
}

// DatabaseName returns the namespaced database name for a fresh run.
func DatabaseName(taskID string) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("testdb: entropy source failed: %v", err))
	}
	return fmt.Sprintf("test_%s_%s_%s", taskID, time.Now().UTC().Format("20060102T150405"), hex.EncodeToString(b[:]))
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

func createDatabase(t *testing.T, ctx context.Context, adminURL, name string) {
	t.Helper()
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Fatalf("testdb: connect admin: %v", err)
	}
	defer admin.Close()
	// Identifiers come from a controlled alphabet (task id + digits); no user
	// input is interpolated.
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %q`, name)); err != nil {
		t.Fatalf("testdb: create database %s: %v", name, err)
	}
}

func dropDatabase(ctx context.Context, adminURL, name string) {
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		return // nothing sane to do without a connection; the run ends anyway
	}
	defer admin.Close()
	_, _ = admin.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, name))
}
