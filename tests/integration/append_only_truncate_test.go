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
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lichman0405/post/internal/persistence/testdb"
)

func TestAppendOnlyTruncateIsRejected(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), appendOnlyTaskID)

	// Every table guarded in 00014 must also refuse TRUNCATE. The list is the
	// same set; if 00014 grows, both migrations and this test grow together.
	guarded := []string{
		"asset_lineage", "audit_log", "contribution_events",
		"external_reference_snapshots", "policy_versions", "project_states",
		"relation_versions", "releases", "research_asset_versions",
		"research_events", "scientific_object_versions", "state_commits",
		"validation_results",
	}
	for _, tbl := range guarded {
		// CASCADE because these tables are FK-referenced; the cascade notices
		// are noise, the error is the point.
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
		if !strings.Contains(pgErr.Message, "append-only") || !strings.Contains(pgErr.Message, "TRUNCATE") {
			t.Errorf("TRUNCATE %s: error does not name the invariant and operation: %s", tbl, pgErr.Message)
		}
	}

	// The guard must be targeted, not a blanket refusal: a table that is
	// mutable by design still truncates.
	if _, err := pool.Exec(ctx, "TRUNCATE search_documents"); err != nil {
		t.Errorf("TRUNCATE on an unguarded table was blocked; the guard is too broad: %v", err)
	}

	// And the guard must not have broken ordinary DML on a guarded table.
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal").Scan(&n); err != nil {
		t.Fatalf("counting triggers: %v", err)
	}
	if n < 26 {
		t.Errorf("expected at least 26 non-internal triggers (13 update/delete + 13 truncate), got %d", n)
	}
}
