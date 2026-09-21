package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
)

// cmdReset drops and recreates the target database. It is the honest answer
// to "the second run must not fail on what the first left": the builder is
// idempotent on its own, but an operator who wants a genuinely empty database
// gets one verb that says exactly what it deletes, and the verb refuses to
// run against a database whose name does not look like a demo database
// unless --force is given.
//
// It does NOT migrate: the migrations are the product's own
// (`rddev db migrate`), and a second implementation of "what the schema is"
// inside a demo builder is exactly the kind of drift this repository avoids.
func cmdReset(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("reset", flag.ExitOnError)
	db := fs.String("db", os.Getenv("SEED_DEMO_DB_URL"), "PostgreSQL URL whose database will be dropped and recreated")
	force := fs.Bool("force", false, "allow dropping a database whose name does not contain 'seed' or 'demo'")
	keep := fs.Bool("keep", false, "create the database empty but do not drop an existing one")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *db == "" {
		return fmt.Errorf("--db (or SEED_DEMO_DB_URL) is required")
	}
	name, adminCfg, err := splitDatabaseURL(*db)
	if err != nil {
		return err
	}
	lower := strings.ToLower(name)
	if !*force && !strings.Contains(lower, "seed") && !strings.Contains(lower, "demo") {
		return fmt.Errorf("refusing to reset %q: the name does not look like a demo database "+
			"(pass --force to reset it anyway, or point --db at the demo database)", name)
	}
	if *keep {
		exists, err := databaseExists(ctx, adminCfg, name)
		if err != nil {
			return err
		}
		if exists {
			fmt.Fprintf(os.Stderr, "seeddemo reset: %s already exists and --keep was given; nothing dropped\n", name)
			return nil
		}
	}
	conn, err := pgx.ConnectConfig(ctx, adminCfg)
	if err != nil {
		return fmt.Errorf("connect to the maintenance database: %w", err)
	}
	defer conn.Close(ctx)
	if !*keep {
		stmts := []string{
			fmt.Sprintf(`drop database if exists %q with (force)`, name),
			fmt.Sprintf(`create database %q`, name),
		}
		for _, stmt := range stmts {
			if _, err := conn.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("%s: %w", stmt, err)
			}
		}
		fmt.Fprintf(os.Stderr, "seeddemo reset: dropped and recreated database %s (all rows in it are gone; "+
			"the schema is empty until the migrations run)\n", name)
		return nil
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf(`create database %q`, name)); err != nil {
		return fmt.Errorf("create database %q: %w", name, err)
	}
	fmt.Fprintf(os.Stderr, "seeddemo reset: created empty database %s\n", name)
	return nil
}

// splitDatabaseURL returns the database name and a connection config for the
// maintenance database, which is where DROP/CREATE DATABASE has to run. It
// returns a config rather than a URL because pgx keeps the original connection
// string inside a parsed config, so editing Database and re-rendering the
// string would silently hand back the same target database.
func splitDatabaseURL(raw string) (name string, admin *pgx.ConnConfig, err error) {
	cfg, err := pgx.ParseConfig(raw)
	if err != nil {
		return "", nil, fmt.Errorf("parse --db: %w", err)
	}
	if cfg.Database == "" {
		return "", nil, fmt.Errorf("--db names no database")
	}
	admin = cfg.Copy()
	admin.Database = "postgres"
	return cfg.Database, admin, nil
}

func databaseExists(ctx context.Context, admin *pgx.ConnConfig, name string) (bool, error) {
	conn, err := pgx.ConnectConfig(ctx, admin)
	if err != nil {
		return false, fmt.Errorf("connect to the maintenance database: %w", err)
	}
	defer conn.Close(ctx)
	var one int
	err = conn.QueryRow(ctx, `select 1 from pg_database where datname = $1`, name).Scan(&one)
	if err != nil {
		if err == pgx.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
