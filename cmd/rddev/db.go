package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"time"

	"github.com/lichman0405/post/internal/persistence"
)

const dbUsage = `Usage: rddev db migrate [--url URL] [--json]

Applies the embedded forward-only migrations to a PostgreSQL database and
reports how many were applied. Idempotent: a database already at head reports
0 applied.

URL defaults to $POSTGRES_TEST_ADMIN_URL, then to the local infra stack
(docs/66): postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post

Why this exists: the documented local flow was "make infra-up, then make dev",
but nothing ever migrated the dev database — make dev started the five
applications against a database with no tables. The only code that migrated
anything was the integration suite, into a throwaway namespaced database it
dropped on the way out. Every real-services check needs a migrated database
first, so the entry point has to exist rather than be improvised per test.
`

// runDB implements `rddev db ...`. Exit codes follow rddev's contract:
// 0 ok, 1 operational failure, 2 usage error.
func runDB(args []string, stdout, stderr io.Writer, jsonOut bool) int {
	if wantsHelp(args) {
		fmt.Fprint(stdout, dbUsage)
		return exitOK
	}
	vals, pos, err := parseFlags(args,
		flagSpec{"--json", false},
		flagSpec{"--url", true},
	)
	if err != nil {
		return usageError(stderr, err.Error(), dbUsage)
	}
	if _, ok := vals["--json"]; ok {
		jsonOut = true
	}
	if len(pos) == 0 || pos[0] != "migrate" {
		return usageError(stderr, "rddev db: expected `migrate`", dbUsage)
	}
	if len(pos) > 1 {
		return usageError(stderr, fmt.Sprintf("rddev db migrate: unexpected argument %q", pos[1]), dbUsage)
	}

	dbURL := vals["--url"]
	if dbURL == "" {
		dbURL = os.Getenv("POSTGRES_TEST_ADMIN_URL")
	}
	if dbURL == "" {
		// The same default the Makefile and ops/DEV_COMMANDS.md document, and
		// the port `make infra-up` actually publishes.
		dbURL = "postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	applied, err := persistence.Migrate(ctx, dbURL)
	if err != nil {
		return operationalError(stderr, "rddev db migrate", err)
	}
	shown := redactedDBURL(dbURL)
	if jsonOut {
		out, err := json.Marshal(struct {
			URL     string `json:"url"`
			Applied int64  `json:"applied"`
		}{URL: shown, Applied: applied})
		if err != nil {
			return operationalError(stderr, "rddev db migrate", err)
		}
		fmt.Fprintln(stdout, string(out))
		return exitOK
	}
	fmt.Fprintf(stdout, "rddev db migrate: %d migration(s) applied to %s\n", applied, shown)
	return exitOK
}

// redactedDBURL strips any password before a database URL is printed. The
// command's output lands in shells, CI logs and Worker transcripts, and
// libpq URLs carry the password in the clear — the same discipline the API's
// config redaction follows (internal/config/redact.go).
func redactedDBURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if _, hasPassword := u.User.Password(); !hasPassword {
		return raw
	}
	return u.Redacted()
}
