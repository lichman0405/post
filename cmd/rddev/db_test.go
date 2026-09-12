package main

import (
	"bytes"
	"strings"
	"testing"
)

// A migration command's output lands in shells, CI logs and Worker
// transcripts, and a libpq URL carries the password in the clear. Printing
// the URL unredacted would leak the database password into every one of them.
func TestRedactedDBURLNeverPrintsThePassword(t *testing.T) {
	const secret = "postgres_dev_pw"
	cases := []struct{ in, want string }{
		{"postgres://postgres:" + secret + "@127.0.0.1:5432/post", "postgres://postgres:xxxxx@127.0.0.1:5432/post"},
		// No password: nothing to redact, and it must stay readable.
		{"postgres://postgres@127.0.0.1:5432/post", "postgres://postgres@127.0.0.1:5432/post"},
		{"postgres://127.0.0.1:5432/post", "postgres://127.0.0.1:5432/post"},
	}
	for _, c := range cases {
		got := redactedDBURL(c.in)
		if strings.Contains(got, secret) {
			t.Errorf("redactedDBURL(%q) still contains the password: %q", c.in, got)
		}
		if got != c.want {
			t.Errorf("redactedDBURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Usage errors must be usage errors (exit 2), not operational failures: a
// typo must not read as "the database is broken".
func TestDBMigrateUsageErrors(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runDB(nil, &out, &errb, false); code != exitUsage {
		t.Errorf("runDB(nil) = %d, want %d (usage)", code, exitUsage)
	}
	out.Reset()
	errb.Reset()
	if code := runDB([]string{"drop"}, &out, &errb, false); code != exitUsage {
		t.Errorf("runDB(drop) = %d, want %d (usage)", code, exitUsage)
	}
}
