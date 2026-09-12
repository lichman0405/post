// Package persistence owns PostgreSQL access (pgx + sqlc per docs/52):
// explicit SQL, transactional boundaries, append-only version/event
// constraints enforced in both DB and application. PostgreSQL is the
// semantic canonical store (ADR-020, docs/21_DATA_MODEL.md). T0002 scaffold.
package persistence
