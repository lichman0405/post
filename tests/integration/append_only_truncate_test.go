// Regression test for the TRUNCATE hole disclosed by T0013 and closed by
// infra/migrations/00015_append_only_truncate.sql.
//
// Row-level triggers do not fire on TRUNCATE, so the immutability added in
// 00014 could still be bypassed wholesale — one statement could erase an
// entire history. Statement-level BEFORE TRUNCATE triggers close it.
//
// This is the "actively try to make X happen" case: the original T0013 tests
// proved UPDATE and DELETE were rejected and never tried the operation that
// bypasses row triggers.
package integration

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/persistence/testdb"
)

// truncateGuardedTables reads, from pg_trigger, every table carrying a
// `<table>_no_truncate` trigger — the statement-level BEFORE TRUNCATE half of
// the guard that 00015 introduced and every later migration reuses (00034,
// 00038, 00047, 00053, 00056). The list is QUERIED rather than hand-kept so
// the behavior loop cannot fall behind the schema: a table whose migration
// adds the trigger is covered the moment it lands, whether or not someone
// remembered to extend a literal. A DISABLED trigger still shows up here (the
// lookup filters on the name, never on tgenabled), so a guard that stopped
// firing fails on behavior instead of quietly leaving the list; a dropped one
// is caught by the design-list cross-check and the guard count below.
func truncateGuardedTables(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT c.relname
		FROM pg_trigger t
		JOIN pg_class c ON c.oid = t.tgrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND NOT t.tgisinternal
		  AND t.tgname = c.relname || '_no_truncate'
		ORDER BY c.relname`)
	if err != nil {
		t.Fatalf("truncateGuardedTables: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var rel string
		if err := rows.Scan(&rel); err != nil {
			t.Fatalf("truncateGuardedTables: scan: %v", err)
		}
		out = append(out, rel)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("truncateGuardedTables: %v", err)
	}
	return out
}

func TestAppendOnlyTruncateIsRejected(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), appendOnlyTaskID)

	// Every table carrying the TRUNCATE guard must actually refuse TRUNCATE.
	// "Every" is true by construction because the list is read from the
	// catalog; append_only_test.go holds a hardcoded design list instead,
	// because THAT question — which tables are append-only by design — is a
	// declaration, and its assertTriggers proves the guard's SHAPE (both
	// halves present and enabled per append-only table). This loop proves the
	// BEHAVIOR of whatever the catalog says is guarded, which makes it a
	// superset of the design list: it also covers the three tables that are
	// truncate-guarded without being append-only pairs at all
	// (git_push_semantic_candidates from 00034, git_reconciliation_findings
	// and git_reconciliation_runs from 00047).
	guarded := truncateGuardedTables(t, ctx, pool)
	if len(guarded) == 0 {
		t.Fatal("no `<table>_no_truncate` trigger in the catalog — every table would be skipped and this loop would prove nothing")
	}
	// The catalog set must still cover the design list: a table that is
	// declared append-only there but is not guarded here would be the drift
	// this lookup exists to prevent, one layer up.
	for _, tbl := range appendOnlyTables {
		if !slices.Contains(guarded, tbl) {
			t.Errorf("append-only table %s is in the design list but carries no `<table>_no_truncate` trigger", tbl)
		}
	}
	for _, tbl := range guarded {
		// CASCADE because these tables are FK-referenced; the cascade notices
		// are noise, the error is the point. The rejection must name THIS
		// table: CASCADE also reaches the tables that reference it, so an
		// error raised by a dependent's guard (TRUNCATE projects CASCADE is
		// refused by policy_versions) would otherwise pass the loop below
		// without proving the table itself is guarded.
		_, err := pool.Exec(ctx, "TRUNCATE "+tbl+" CASCADE")
		if err == nil {
			t.Errorf("TRUNCATE %s succeeded — history could be erased wholesale", tbl)
			continue
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Errorf("TRUNCATE %s: expected a PostgreSQL error, got %v", tbl, err)
			continue
		}
		if pgErr.Code != "P0001" {
			t.Errorf("TRUNCATE %s: SQLSTATE = %s, want P0001", tbl, pgErr.Code)
		}
		if !strings.Contains(pgErr.Message, "append-only") || !strings.Contains(pgErr.Message, "TRUNCATE") ||
			!strings.Contains(pgErr.Message, "table "+tbl+" ") {
			t.Errorf("TRUNCATE %s: error does not name this table, the invariant and the operation: %s", tbl, pgErr.Message)
		}
	}

	// The guard must be targeted, not a blanket refusal: a table that is
	// mutable by design still truncates.
	if _, err := pool.Exec(ctx, "TRUNCATE search_documents"); err != nil {
		t.Errorf("TRUNCATE on an unguarded table was blocked; the guard is too broad: %v", err)
	}

	// Count the guard triggers the two catalogs pin — each append-only table's
	// `<table>_append_only` + `<table>_no_truncate` pair, plus every
	// targetedGuardTriggers entry — and require exactly that many. The count is
	// what the two lists add up to (the expected number is derived from them,
	// never written down), so it moves with them instead of going stale: a
	// guard that a migration dropped or disabled shows up as a shortfall, and a
	// guard nobody pinned shows up as a surplus. It counts guards, not the
	// schema's whole trigger set: 00043's relation_versions_provenance_projection
	// is deliberately outside both catalogs (the shape test tolerates it because
	// it hangs off an append-only table) and is not counted here.
	guardKeys := make([]string, 0, len(targetedGuardTriggers))
	for key := range targetedGuardTriggers {
		guardKeys = append(guardKeys, key)
	}
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM pg_trigger t
		JOIN pg_class c ON c.oid = t.tgrelid
		JOIN pg_namespace ns ON ns.oid = c.relnamespace
		WHERE ns.nspname = 'public' AND NOT t.tgisinternal
		  AND (t.tgname IN (c.relname || '_append_only', c.relname || '_no_truncate')
		       OR c.relname || ':' || t.tgname = ANY($1))`, guardKeys).Scan(&n); err != nil {
		t.Fatalf("counting guard triggers: %v", err)
	}
	if want := 2*len(appendOnlyTables) + len(targetedGuardTriggers); n != want {
		t.Errorf("guard triggers present = %d, want exactly %d = %d append-only tables × [update/delete + truncate] + %d targeted guards",
			n, want, len(appendOnlyTables), len(targetedGuardTriggers))
	}
}
