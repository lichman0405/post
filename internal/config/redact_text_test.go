package config

import (
	"strings"
	"testing"
)

// A persisted reason is prose that has to stay readable, so these tests assert
// two things at once, and the second one is the point: the credential is gone,
// AND everything else is byte-identical. A test that only checked the first
// would pass against an implementation that returns "***" for the whole
// string — which is exactly the behaviour that made a text redactor necessary.

const proseCredential = "hunter2"

func TestRedactTextForOutputPreservesTheRestOfTheText(t *testing.T) {
	in := "out of scope: internal/foo/bar.go (allowed: internal/rsg/**)\n" +
		"command: POSTGRES_TEST_ADMIN_URL=postgres://postgres:" + proseCredential + "@127.0.0.1:5432/post go test -count=1 ./tests/integration/\n" +
		"baseline: 401dfc8847bd463029933e611dc7fa4bc5285e19\n" +
		"contact: dev@example.com\n"

	got := RedactTextForOutput(in)

	if strings.Contains(got, proseCredential) {
		t.Fatalf("the credential survived redaction:\n%s", got)
	}
	// Everything that is not a credential-shaped value must survive verbatim.
	for _, must := range []string{
		"internal/foo/bar.go",
		"allowed: internal/rsg/**",
		"POSTGRES_TEST_ADMIN_URL=postgres://postgres:***@127.0.0.1:5432/post",
		"go test -count=1 ./tests/integration/",
		"dev@example.com",
	} {
		if !strings.Contains(got, must) {
			t.Errorf("redaction lost non-secret text %q:\n%s", must, got)
		}
	}
	// Line structure is part of a reason's readability: separators are copied,
	// not re-rendered, so the newlines and their count survive.
	if gotLines, inLines := strings.Count(got, "\n"), strings.Count(in, "\n"); gotLines != inLines {
		t.Errorf("line count changed: %d -> %d\n%s", inLines, gotLines, got)
	}
	// The one loss this design accepts, asserted rather than left implicit: the
	// 40-character sha is masked, because the rule that catches opaque tokens
	// cannot tell a commit sha from a hex secret and erring toward masking is
	// the safe direction. The loss is bounded to the sha, never the reason.
	if !strings.Contains(got, "baseline: ***") {
		t.Errorf("expected the sha to be masked, got:\n%s", got)
	}
}

func TestRedactTextForOutputDoesNotInheritTheValueRedactorsNuke(t *testing.T) {
	// This pins why two functions exist, using the real shape that produced the
	// split: a multi-kilobyte reason containing one 40-character sha.
	prose := "【基线推进】你的实现没有问题，需要在新基线上重新确认。"
	sha := "401dfc8847bd463029933e611dc7fa4bc5285e19"
	reason := strings.Repeat(prose, 80) + " baseline " + sha

	if len(reason) < 2000 {
		t.Fatalf("fixture is not the case under test: only %d chars", len(reason))
	}
	// The hazard, demonstrated rather than described: the value redactor does
	// not mask the sha, it deletes the entire reason.
	if got := RedactForOutput(reason); got != "***" {
		t.Fatalf("fixture is not the case under test: RedactForOutput returned %d chars, not the whole-string mask — if that hazard has been fixed, this test and RedactTextForOutput should be revisited together", len(got))
	}
	got := RedactTextForOutput(reason)
	// Exactly the sha is masked: 40 characters become 3, and nothing else moves.
	if want := len(reason) - len(sha) + 3; len(got) != want {
		t.Errorf("the text redactor did not limit the loss to the sha: %d chars -> %d, want %d", len(reason), len(got), want)
	}
	if !strings.Contains(got, strings.Repeat(prose, 80)) {
		t.Errorf("the text redactor lost the reason's own words")
	}
	if !strings.Contains(got, "baseline ***") {
		t.Errorf("the sha survived: %s", got[len(got)-60:])
	}
}

func TestRedactTextForOutputRedactsEachRule(t *testing.T) {
	cases := []struct{ what, in, want string }{
		{
			// RedactURL splits at the LAST '@' of its input. Handed the whole
			// command line it replaced the host and deleted everything between;
			// handed one token it cannot.
			what: "a URL token with a later '@' in the same command",
			in:   "PGURL=postgres://u:pw@127.0.0.1:5432/db go test --to dev@example.com",
			want: "PGURL=postgres://u:***@127.0.0.1:5432/db go test --to dev@example.com",
		},
		{
			what: "a token-only userinfo",
			in:   "clone https://ghp_AAAAAAAAAAAAAAAAAAAAAAAA@github.com/o/r.git now",
			want: "clone https://***@github.com/o/r.git now",
		},
		{
			what: "the Postgres keyword/value form",
			in:   "host=db password=pw123 port=5432",
			want: "host=db password=*** port=5432",
		},
		{
			what: "a bare well-known prefix outside any URL",
			in:   "GITEA_TOKEN=ghp_ABCDEFGHIJKLMNOPQRSTUVWX go test",
			want: "GITEA_TOKEN=*** go test",
		},
		{
			what: "an AWS-shaped key",
			in:   "AKIAIOSFODNN7EXAMPLE was rejected",
			want: "*** was rejected",
		},
		{
			what: "prose with no credential",
			in:   "scope violation: internal/foo/x.go is outside allowed_scope",
			want: "scope violation: internal/foo/x.go is outside allowed_scope",
		},
		// The prefixed assignment names. `\b` cannot see these: the character
		// before the keyword is `_`, a word character, so there is no boundary
		// to anchor on and the value was never masked at all.
		{
			what: "a prefixed password name",
			in:   "make test POST_DB_PASSWORD=hunter2 POST_GITEA_TOKEN=another",
			want: "make test POST_DB_PASSWORD=*** POST_GITEA_TOKEN=***",
		},
		{
			what: "an unprefixed PGPASSWORD",
			in:   "PGPASSWORD=hunter2 psql -h db",
			want: "PGPASSWORD=*** psql -h db",
		},
		{
			what: "a JSON assignment",
			in:   `{"user": "u", "password": "hunter2"}`,
			want: `{"user": "u", "password": ***}`,
		},
		{
			what: "a YAML assignment",
			in:   "database:\n  password: hunter2\n  port: 5432",
			want: "database:\n  password: ***\n  port: 5432",
		},
		{
			what: "an Authorization header",
			in:   "curl -H 'Authorization: Bearer hunter2' $URL",
			want: "curl -H 'Authorization: Bearer ***' $URL",
		},
		{
			what: "a flag with its value as the next argument",
			in:   "mytool --password hunter2 --verbose",
			want: "mytool --password *** --verbose",
		},
	}
	for _, tc := range cases {
		if got := RedactTextForOutput(tc.in); got != tc.want {
			t.Errorf("%s:\n  in   %q\n  got  %q\n  want %q", tc.what, tc.in, got, tc.want)
		}
	}
}

// TestRedactTextForOutputRedactsAUserinfoThatSpansWhitespace pins the gap that
// the per-token splitter opened. RedactURL needs both the "://" and the "@" to
// see a userinfo at all, and splitting on whitespace can put them in different
// tokens — so the credential was handed back untouched, and committed. The old
// whole-string rule happened to catch these, which is what made it a weakening
// rather than a gap that had always been there.
//
// Wrapping a shell line is how this happens for real: a pasted DSN broken after
// the colon, with the continuation sitting between the credential and its host.
func TestRedactTextForOutputRedactsAUserinfoThatSpansWhitespace(t *testing.T) {
	cases := []struct{ what, in, want string }{
		{
			what: "a shell line continuation inside the userinfo",
			in:   "POSTGRES_TEST_ADMIN_URL=postgres://postgres:" + proseCredential + "\\\n@127.0.0.1:5432/post go test -count=1 ./tests/integration/",
			want: "POSTGRES_TEST_ADMIN_URL=postgres://postgres:***@127.0.0.1:5432/post go test -count=1 ./tests/integration/",
		},
		{
			what: "a tab between the user and the password",
			in:   "dsn=postgres://postgres:\t" + proseCredential + "@127.0.0.1:5432/post",
			want: "dsn=postgres://postgres:***@127.0.0.1:5432/post",
		},
		{
			what: "a space between the user and the password",
			in:   "dsn=postgres://postgres:" + proseCredential + " @127.0.0.1:5432/post",
			want: "dsn=postgres://postgres:***@127.0.0.1:5432/post",
		},
		{
			what: "a token-only userinfo split across the newline",
			in:   "clone https://ghp_AAAAAAAAAAAAAAAAAAAAAAAA\\\n@github.com/o/r.git",
			want: "clone https://***@github.com/o/r.git",
		},
		// The wrap does not have to land after the colon. At these two
		// positions the first token is `postgres://postgres\` or `postgres://\`
		// — no colon, nothing credential-shaped after the "//" — so a rule that
		// asks only "does this look like a userinfo" calls it a complete URL
		// with no userinfo, and the credential rides into the committed file
		// behind a second token that has no "://" of its own. What is visible
		// at all four positions is the line continuation.
		{
			what: "a line continuation landing before the colon",
			in:   "POSTGRES_TEST_ADMIN_URL=postgres://postgres\\\n:" + proseCredential + "@127.0.0.1:5432/post go test -count=1",
			want: "POSTGRES_TEST_ADMIN_URL=postgres://postgres\\\n:***@127.0.0.1:5432/post go test -count=1",
		},
		{
			what: "a line continuation landing immediately after the scheme",
			in:   "POSTGRES_TEST_ADMIN_URL=postgres://\\\npostgres:" + proseCredential + "@127.0.0.1:5432/post go test -count=1",
			want: "POSTGRES_TEST_ADMIN_URL=postgres://\\\npostgres:***@127.0.0.1:5432/post go test -count=1",
		},
		// The two shapes that must NOT be touched, and the reason the rule is
		// narrow: absorbing the following token whenever the run has an "@"
		// would redact a host and delete the middle of a line that contains no
		// credential — which is precisely the old bug this function replaced.
		{
			what: "a complete URL with no userinfo, followed by an email address",
			in:   "PGURL=postgres://h/db go test --to dev@example.com",
			want: "PGURL=postgres://h/db go test --to dev@example.com",
		},
		{
			what: "a host:port that is not a userinfo, followed by an email address",
			in:   "PGURL=postgres://host:5432 go test --to dev@example.com",
			want: "PGURL=postgres://host:5432 go test --to dev@example.com",
		},
		// The same shape with the "@" on the NEXT line rather than two tokens
		// away. Here the colon test alone says "open", the following token does
		// have an "@", and the span is rewritten: the port is deleted, an email
		// becomes a userinfo, and two lines are joined — on a line with no
		// credential anywhere. A newline is not evidence of a wrap; a trailing
		// continuation is.
		{
			what: "a host:port with no userinfo and an address on the next line",
			in:   "PGURL=postgres://host:5432\n      owner@example.com",
			want: "PGURL=postgres://host:5432\n      owner@example.com",
		},
		{
			what: "a complete URL with no userinfo and an address on the next line",
			in:   "PGURL=postgres://h/db\n      owner@example.com",
			want: "PGURL=postgres://h/db\n      owner@example.com",
		},
	}
	for _, tc := range cases {
		got := RedactTextForOutput(tc.in)
		if strings.Contains(got, proseCredential) {
			t.Errorf("%s: the credential survived:\n  in   %q\n  got  %q", tc.what, tc.in, got)
		}
		// The sha rule is not what is under test here; only the exact shape is.
		if tc.want != "" && got != tc.want {
			t.Errorf("%s:\n  in   %q\n  got  %q\n  want %q", tc.what, tc.in, got, tc.want)
		}
	}
}

func TestRedactTextForOutputIsIdentityOnEmpty(t *testing.T) {
	if got := RedactTextForOutput(""); got != "" {
		t.Errorf("RedactTextForOutput(\"\") = %q", got)
	}
}
