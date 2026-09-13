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
	}
	for _, tc := range cases {
		if got := RedactTextForOutput(tc.in); got != tc.want {
			t.Errorf("%s:\n  in   %q\n  got  %q\n  want %q", tc.what, tc.in, got, tc.want)
		}
	}
}

func TestRedactTextForOutputIsIdentityOnEmpty(t *testing.T) {
	if got := RedactTextForOutput(""); got != "" {
		t.Errorf("RedactTextForOutput(\"\") = %q", got)
	}
}
