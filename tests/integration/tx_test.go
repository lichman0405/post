package integration

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

// TestWithTx verifies the transaction boundary against a real PostgreSQL:
// commit on nil, rollback on error (including the row disappearing), and
// rollback on panic with the panic re-raised.
func TestWithTx(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), taskID)

	insertOrg := func(dbtx persistence.DBTX, slug string) error {
		_, err := dbtx.Exec(ctx,
			`INSERT INTO organizations (slug, name) VALUES ($1, $2)`, slug, slug)
		return err
	}
	countOrgs := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM organizations`).Scan(&n); err != nil {
			t.Fatalf("count organizations: %v", err)
		}
		return n
	}

	t.Run("commit on success", func(t *testing.T) {
		before := countOrgs()
		err := persistence.WithTx(ctx, pool, func(tx pgx.Tx) error {
			return insertOrg(tx, "committed-org")
		})
		if err != nil {
			t.Fatalf("WithTx committed path returned error: %v", err)
		}
		if got := countOrgs(); got != before+1 {
			t.Errorf("commit: %d organizations, want %d", got, before+1)
		}
	})

	t.Run("rollback on error", func(t *testing.T) {
		before := countOrgs()
		sentinel := errors.New("deliberate failure")
		err := persistence.WithTx(ctx, pool, func(tx pgx.Tx) error {
			if err := insertOrg(tx, "rolled-back-org"); err != nil {
				return err
			}
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("WithTx error path: got %v, want sentinel", err)
		}
		if got := countOrgs(); got != before {
			t.Errorf("rollback: %d organizations, want %d (insert must be rolled back)", got, before)
		}
	})

	t.Run("rollback on panic, panic re-raised", func(t *testing.T) {
		before := countOrgs()
		panicVal := "kaboom"
		func() {
			defer func() {
				r := recover()
				if r == nil {
					t.Error("WithTx swallowed the panic")
					return
				}
				if fmt.Sprint(r) != panicVal {
					t.Errorf("recovered %v, want %q", r, panicVal)
				}
			}()
			_ = persistence.WithTx(ctx, pool, func(tx pgx.Tx) error {
				if err := insertOrg(tx, "panicked-org"); err != nil {
					t.Fatalf("insert inside panicking fn: %v", err)
				}
				panic(panicVal)
			})
		}()
		if got := countOrgs(); got != before {
			t.Errorf("panic rollback: %d organizations, want %d (insert must be rolled back)", got, before)
		}
	})

	t.Run("sqlc queries run inside the transaction", func(t *testing.T) {
		// The generated query layer and WithTx share the DBTX surface: the
		// same generated call must work on the pool and inside a tx.
		err := persistence.WithTx(ctx, pool, func(tx pgx.Tx) error {
			return insertOrg(tx, "tx-org")
		})
		if err != nil {
			t.Fatalf("WithTx with query layer: %v", err)
		}
		if countOrgs() == 0 {
			t.Fatal("query layer inside tx did not persist")
		}
	})

	t.Run("ctx cancellation still rolls back", func(t *testing.T) {
		before := countOrgs()
		cancelCtx, cancel := context.WithCancel(ctx)
		err := persistence.WithTx(cancelCtx, pool, func(tx pgx.Tx) error {
			if err := insertOrg(tx, "cancelled-org"); err != nil {
				return err
			}
			cancel() // the common defer cancel() pattern firing mid-transaction
			return errors.New("cancelled mid-flight")
		})
		cancel()
		if err == nil {
			t.Fatal("expected an error from the cancelled transaction")
		}
		if got := countOrgs(); got != before {
			t.Errorf("cancel rollback: %d organizations, want %d", got, before)
		}
	})
}
