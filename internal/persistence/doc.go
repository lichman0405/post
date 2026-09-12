// Package persistence owns PostgreSQL access (pgx + sqlc per docs/52, docs/68):
// explicit SQL, transactional boundaries, append-only version/event
// constraints enforced in both DB and application. PostgreSQL is the
// semantic canonical store (ADR-020, docs/21_DATA_MODEL.md).
//
// Layout:
//
//	migrate.go   forward-only migration runner (goose) over infra/migrations
//	db.go        pgxpool construction
//	tx.go        WithTx: rollback on error, panic-safe, commit otherwise
//	queries/     sqlc query sources (explicit SQL)
//	sqlc/        generated, strongly typed query code — checked in; drift is
//	             detected by tests/integration/check-sqlc-drift.sh
//	testdb/      task/run-scoped test database namespace (docs/66 §3)
package persistence
