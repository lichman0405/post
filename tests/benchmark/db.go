package main

import (
	"context"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/persistence"
)

// defaultAdminURL is the same default `make test-integration` and `make
// migrate` use (Makefile:147-171), so the documented local flow works here
// without exporting anything: `make infra-up` publishes 5432, and this points
// at it. POSTGRES_TEST_ADMIN_URL overrides it for a non-default stack.
const defaultAdminURL = "postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post"

// defaultDBName is the benchmark database. It is dropped and recreated by
// every `seed`, so it is never a place to keep anything.
//
// A FIXED name rather than the test suite's task/run-scoped one
// (internal/persistence/testdb names databases test_<task>_<run>): the
// benchmark database is meant to outlive one process, because `seed`,
// `measure` and `all` are separate commands a person runs one at a time while
// looking at the numbers — and because the spec tier costs minutes to load, so
// re-measuring it without reseeding has to be possible. Nothing else in the
// repository may use it, and `--db` overrides it for a parallel run.
const defaultDBName = "post_bench"

// envAdminURL resolves the admin URL. Read once, here, so every command agrees
// on which server it is talking about and one report cannot blend two.
func envAdminURL() string {
	if v := strings.TrimSpace(os.Getenv("POSTGRES_TEST_ADMIN_URL")); v != "" {
		return v
	}
	return defaultAdminURL
}

// withDatabase rewrites a URL's path to name a different database, preserving
// credentials, host and options.
func withDatabase(raw, name string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("benchmark: parse %q: %w", raw, err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "", fmt.Errorf("benchmark: %q is not a postgres URL", raw)
	}
	u.Path = "/" + name
	return u.String(), nil
}

// redactURL hides the password, the way `rddev db migrate` prints it: a report
// that carries a credential is a report that cannot be pasted anywhere.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable url>"
	}
	if u.User != nil {
		if _, has := u.User.Password(); has {
			u.User = url.UserPassword(u.User.Username(), "xxxxx")
		}
	}
	return u.String()
}

// openSeed connects to the benchmark database with pgvector's `vector` type
// registered in pgx's type map.
//
// # Why the seed pool needs a codec and the measurement pool must not have one
//
// A vector column is 6 KB of float32 per row, and there are 100k of them in the
// network tier. pgx's COPY is always the BINARY format (`copy ... from stdin
// binary;` in pgx's copy_from.go), and pgvector's binary form is a 4-byte
// header plus the raw big-endian float32s, so a Go string holding the text
// literal `[0.1,0.2,...]` is interpreted as a length header and the server
// answers "vector cannot have more than 16000 dimensions". Registering
// vectorCodec below turns 100k rows of ~13.8 KB of ASCII into 100k rows of
// 6.2 KB of float32 and takes the float formatting out of the path entirely.
//
// The codec is registered ONLY on this pool. The measurement pool has none,
// because production has none: the sqlc queries take the embedding as a
// `string` and hand it to `$1::vector`, so the server parses pgvector's text
// syntax on every call. A codec on the measurement pool would change how the
// parameter travels and quietly measure a path the application does not take.
// That is why the measurement pool is built by pgxpool.ParseConfig in
// openWorkload (workload.go) rather than by any helper in this file: it is the
// one pool whose type-map EMPTINESS is load-bearing, and it should be read next
// to the settings it also has to carry.
func openSeed(ctx context.Context, dbURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		return nil, fmt.Errorf("benchmark: parse seed url: %w", err)
	}
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		var oid uint32
		if err := c.QueryRow(ctx, `SELECT oid FROM pg_type WHERE typname = 'vector'`).Scan(&oid); err != nil {
			return fmt.Errorf("benchmark: the `vector` type is not in this database "+
				"(CREATE EXTENSION vector — infra/migrations/00001_extensions.sql does it): %w", err)
		}
		c.TypeMap().RegisterType(&pgtype.Type{Name: "vector", OID: oid, Codec: vectorCodec{}})
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("benchmark: open seed pool: %w", err)
	}
	return pool, nil
}

// vectorCodec encodes pgvector's `vector` type in its binary wire form.
//
// pgvector's vector_recv reads int16 dim, int16 unused (which must be 0), then
// dim big-endian float32 — see pgvector's vector.c. Decoding is deliberately
// not wired: nothing in this package ever reads a vector back, and a codec that
// silently returned nothing would be a scan that looks like it worked. Both
// decode paths return an error saying so instead.
type vectorCodec struct{}

func (vectorCodec) FormatSupported(f int16) bool { return f == pgtype.BinaryFormatCode }
func (vectorCodec) PreferredFormat() int16       { return pgtype.BinaryFormatCode }

func (vectorCodec) PlanEncode(_ *pgtype.Map, _ uint32, format int16, value any) pgtype.EncodePlan {
	vec, ok := value.([]float32)
	if !ok || format != pgtype.BinaryFormatCode {
		return nil
	}
	return encodeVector{vec: vec}
}

func (vectorCodec) PlanScan(*pgtype.Map, uint32, int16, any) pgtype.ScanPlan { return nil }

func (vectorCodec) DecodeDatabaseSQLValue(*pgtype.Map, uint32, int16, []byte) (driver.Value, error) {
	return nil, errors.New("benchmark: vectorCodec cannot decode; this pool only writes vectors")
}

func (vectorCodec) DecodeValue(*pgtype.Map, uint32, int16, []byte) (any, error) {
	return nil, errors.New("benchmark: vectorCodec cannot decode; this pool only writes vectors")
}

type encodeVector struct{ vec []float32 }

func (e encodeVector) Encode(_ any, buf []byte) ([]byte, error) {
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(e.vec)))
	buf = binary.BigEndian.AppendUint16(buf, 0)
	for _, x := range e.vec {
		buf = binary.BigEndian.AppendUint32(buf, math.Float32bits(x))
	}
	return buf, nil
}

// provision drops and recreates the benchmark database, then migrates it to
// head with the embedded migration set — the same
// internal/persistence.Migrate the application and the integration suite run,
// so the schema under the corpus is the schema under production.
//
// Dropping first is the only way to reset: the four append-only tables reject
// DELETE and TRUNCATE outright (infra/migrations/00014_append_only_enforcement
// .sql, 00015_append_only_truncate.sql — append_only_guard raises P0001), which
// is the domain rule "Nothing disappears; state only evolves" enforced by the
// database itself. A corpus is not history, so it is removed the only way
// history can be removed: by starting a new database.
func provision(ctx context.Context, adminURL, dbName string) (string, time.Duration, error) {
	start := time.Now()
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		return "", 0, fmt.Errorf("benchmark: connect to %s: %w", redactURL(adminURL), err)
	}
	defer admin.Close()

	if err := admin.Ping(ctx); err != nil {
		return "", 0, fmt.Errorf("benchmark: no PostgreSQL at %s: %w\n"+
			"the benchmark needs a reachable server; `make infra-up` publishes one on 5432, "+
			"or set POSTGRES_TEST_ADMIN_URL", redactURL(adminURL), err)
	}

	// Identifiers cannot be parameters, so the name is quoted through
	// pgx.Identifier and never interpolated raw.
	quoted := pgx.Identifier{dbName}.Sanitize()
	if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS `+quoted+` WITH (FORCE)`); err != nil {
		return "", 0, fmt.Errorf("benchmark: drop database %s: %w", dbName, err)
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+quoted); err != nil {
		return "", 0, fmt.Errorf("benchmark: create database %s: %w", dbName, err)
	}

	dbURL, err := withDatabase(adminURL, dbName)
	if err != nil {
		return "", 0, err
	}
	if _, err := persistence.Migrate(ctx, dbURL); err != nil {
		return "", 0, fmt.Errorf("benchmark: migrate %s: %w", dbName, err)
	}
	return dbURL, time.Since(start), nil
}

// dropBenchmarkDB removes the benchmark database. It is what --drop does, and
// it is the only cleanup this package performs: nothing outside
// defaultDBName is ever dropped.
func dropBenchmarkDB(ctx context.Context, adminURL, dbName string) error {
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		return err
	}
	defer admin.Close()
	_, err = admin.Exec(ctx, `DROP DATABASE IF EXISTS `+pgx.Identifier{dbName}.Sanitize()+` WITH (FORCE)`)
	return err
}
