package persistence

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"

	"github.com/lichman0405/post/infra/migrations"
)

// Migrate applies every pending forward-only migration to head and returns
// the number applied (0 when the database is already at head). url is a
// pgx/libpq connection URL. Each migration runs inside its own transaction;
// the set is applied under a session-level advisory lock so concurrent
// migrators cannot interleave (goose v3 locking).
func Migrate(ctx context.Context, url string) (applied int64, err error) {
	return migrateTo(ctx, url, 0)
}

// MigrateTo applies pending migrations up to and including version, enabling
// the documented upgrade path: a database at any older version can be brought
// to head by applying the remaining migrations in order (docs/67: "migration
// 必须 fresh install + upgrade path"). Version 0 means head.
func MigrateTo(ctx context.Context, url string, version int64) (applied int64, err error) {
	if version < 0 {
		return 0, fmt.Errorf("persistence: version %d < 0", version)
	}
	return migrateTo(ctx, url, version)
}

// MigrationVersion reports the last applied migration version (0 when none).
func MigrationVersion(ctx context.Context, url string) (int64, error) {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return 0, fmt.Errorf("persistence: open %q: %w", redactURL(url), err)
	}
	defer db.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS, goose.WithVerbose(false))
	if err != nil {
		return 0, fmt.Errorf("persistence: new provider: %w", err)
	}
	version, err := provider.GetDBVersion(ctx)
	if err != nil {
		return 0, fmt.Errorf("persistence: read version: %w", err)
	}
	return version, nil
}

func migrateTo(ctx context.Context, url string, version int64) (applied int64, err error) {
	// Migrations run over database/sql with the pgx stdlib driver — the same
	// wire protocol the app uses, without tying DDL to a pgx pool.
	db, err := sql.Open("pgx", url)
	if err != nil {
		return 0, fmt.Errorf("persistence: open %q: %w", redactURL(url), err)
	}
	defer db.Close()

	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS, goose.WithVerbose(false))
	if err != nil {
		return 0, fmt.Errorf("persistence: new provider: %w", err)
	}

	// Up/UpTo return only the migrations they actually applied: with no
	// pending work the result list is empty (a clean no-op), and the set is
	// forward-only so no down migrations are ever needed.
	var results []*goose.MigrationResult
	if version == 0 {
		results, err = provider.Up(ctx)
	} else {
		results, err = provider.UpTo(ctx, version)
	}
	if err != nil {
		return 0, fmt.Errorf("persistence: apply migrations up to %d: %w", version, err)
	}
	return int64(len(results)), nil
}

// redactURL strips a password from a URL before it can appear in an error.
func redactURL(url string) string {
	start := strings.Index(url, "://")
	if start < 0 {
		return url
	}
	start += 3
	if at := strings.Index(url[start:], "@"); at >= 0 {
		return url[:start] + "xxxxx" + url[start+at:]
	}
	return url
}
