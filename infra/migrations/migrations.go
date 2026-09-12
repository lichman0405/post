// Package migrations embeds the POST forward-only SQL migration set.
//
// The canonical logical schema lives in specs/database/postgres.sql; the
// numbered *.sql files in this directory are its faithful, dependency-ordered
// decomposition into immutable, forward-only migrations (docs/53:
// "migration forward-only；已经发布 migration 不修改").
//
// The migration runner lives in internal/persistence (Migrate / MigrateTo).
package migrations

import "embed"

// FS holds every numbered migration file. Files are applied in lexical order
// of their numeric prefix; once a numbered file has shipped it must never be
// edited — new changes get a new, higher number.
//
//go:embed *.sql
var FS embed.FS
