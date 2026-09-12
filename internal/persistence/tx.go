package persistence

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DBTX is the common transaction surface shared by *pgxpool.Pool and pgx.Tx.
// sqlc's generated *Queries methods accept the same shape, so a generated
// query set can run either directly on the pool or inside WithTx.
type DBTX interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Beginner is implemented by *pgxpool.Pool and pgx.Tx (nested boundaries are
// not supported: pgx has no savepoint helpers here, so nesting a tx inside a
// tx must go through explicit savepoints).
type Beginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

// WithTx runs fn inside one transaction and returns its result:
//
//   - fn returned nil  → the transaction is committed;
//   - fn returned err  → the transaction is rolled back and err returned
//     (a failed rollback is reported as a wrapped error, never silently);
//   - fn panicked      → the transaction is rolled back and the panic is
//     re-raised, so the process' panic handling stays intact.
//
// Rollback on error runs with a non-cancelled context (context.WithoutCancel)
// so a caller cancelling ctx — the common defer cancel() pattern — cannot
// abort the rollback itself.
//
// Domain events written to outbox_events inside fn share the transaction
// (docs/53: "Domain event + outbox 与状态变更同一 transaction").
func WithTx(ctx context.Context, db Beginner, fn func(tx pgx.Tx) error) (err error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("persistence: begin transaction: %w", err)
	}
	rollbackCtx := context.WithoutCancel(ctx)

	defer func() {
		if r := recover(); r != nil {
			_ = tx.Rollback(rollbackCtx) // best effort; the server aborts on connection loss anyway
			panic(r)
		}
		if err != nil {
			if rbErr := tx.Rollback(rollbackCtx); rbErr != nil {
				err = fmt.Errorf("persistence: %v (rollback also failed: %w)", err, rbErr)
			}
			return
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			err = fmt.Errorf("persistence: commit transaction: %w", commitErr)
		}
	}()

	return fn(tx)
}
