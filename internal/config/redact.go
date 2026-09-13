package config

import (
	"regexp"
	"strings"
)

// secretTokenRe matches well-known credential prefixes and long opaque
// tokens. A value like this must never be echoed, even when it was pasted
// into a field that is not itself secret-shaped.
var secretTokenRe = regexp.MustCompile(
	`(?i)(gh[pousr]_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{20,}|sk-[A-Za-z0-9]{16,}|xox[baprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|[A-Za-z0-9+/]{40,}={0,2})`)

// secretAssignmentRe matches an assignment whose KEY names a credential —
// the Postgres keyword/value DSN form (`host=... password=secret`), which
// carries a credential without any `://`, and the same shape in a command
// line, a JSON fragment or a YAML one:
//
//	password=hunter2    PGPASSWORD=hunter2    POST_DB_PASSWORD=hunter2
//	password="hunter2"  "password": "hunter2"  password: hunter2
//
// The old rule anchored the keyword with `\b` and so matched only a BARE name.
// In PGPASSWORD the preceding character is `_`, which is a word character, so
// there is no boundary at all and the assignment was invisible: the value went
// into the file. Those prefixed names are not hypothetical — they are the ones
// this repository's own test commands use (Makefile, tests/acceptance/
// auth-real-services-e2e.sh, tests/observability/canary-sweep.sh) — and a
// rejection reason quoting such a command is the incident this redactor was
// written for, so the shape it missed is the shape that mattered most.
//
// `:` is here as well as `=`, for the JSON and YAML forms. Both are separators
// that only mean "a credential follows" when the key says so, which is what the
// word list is for; the key is allowed a prefix and a suffix and may be quoted.
//
// secretKeyRe (secretscan.go) is the same idea for env files, and it is
// deliberately not reused verbatim: it carries `auth`, `dsn` and `webhook`,
// which are common enough in a sentence of prose — the text redactor's input —
// that using it would mask the sentence rather than the secret.
var secretAssignmentRe = regexp.MustCompile(
	`(?i)(["']?[A-Za-z0-9_.-]*(password|passwd|pwd|secret|token|credential|api[_-]?key|access[_-]?key|private[_-]?key)[A-Za-z0-9_.-]*["']?)` +
		`(\s*[:=]\s*)("[^"\n]*"|'[^'\n]*'|\S+)`)

// bearerRe matches an Authorization header value. The scheme word is not
// itself a secret and is kept, in its original case and spacing; what follows
// it is the credential.
var bearerRe = regexp.MustCompile(`(?i)\b(bearer)(\s+)([A-Za-z0-9._~+/=+-]+)`)

// secretFlagRe matches a command-line flag that carries its value as a separate
// argument — `--password hunter2`, `--token=abc`. The value alternative is
// quoted-or-one-word like the assignment rule's; a short flag form (`-phunter2`)
// is not covered, because `-p` is also how callers spell `--port`.
var secretFlagRe = regexp.MustCompile(
	`(?i)(--?(?:password|passwd|pwd|token|secret|api-?key|access-?key)\s*[= ]\s*)("[^"\n]*"|'[^'\n]*'|\S+)`)

// RedactForOutput masks credentials in *any* value on its way to a log, an
// error message or JSON output.
//
// RedactURL alone is not enough: a credential can reach an output path in
// shapes it does not recognise — a Postgres keyword/value DSN, or simply a
// token pasted into the wrong variable. Error paths are the highest-risk
// case because a misconfiguration is exactly when a value gets echoed, so
// this is applied by default inside the error builders rather than being
// left to each call site to remember.
func RedactForOutput(v string) string {
	if v == "" {
		return v
	}
	if r := RedactURL(v); r != v {
		return r
	}
	if r := secretAssignmentRe.ReplaceAllString(v, "${1}${3}***"); r != v {
		return r
	}
	if secretTokenRe.MatchString(v) {
		return "***"
	}
	return v
}

// RedactTextForOutput masks credentials inside free text — a command line, an
// error detail, a rejection reason — and leaves the text otherwise intact.
//
// RedactForOutput is the wrong tool for prose, and the difference is not a
// matter of taste. That function is built for a *value*: when secretTokenRe
// matches it returns "***" for the entire input, because a value that looks
// like a token must not be echoed at all. Applied to a persisted reason it is
// destructive rather than conservative. Measured against the 126 reason texts
// in tasks/task_status.json: 13 of them lose more than 40 characters, and the
// worst goes from 2,999 characters to the 3-character string "***", because a
// 2.9 KB explanation happens to contain one 40+ character alphanumeric run (a
// commit sha). A committed reason that reads "***" is an audit trail that has
// been deleted — and this repository already states the standard it violates:
// redact_test.go fails on "output lost non-secret values (over-redaction?)".
//
// So every rule here rewrites the match and never the surrounding text:
//
//   - URL userinfo, per whitespace-separated token. RedactURL splits at the
//     LAST '@' of whatever it is given, which is right for one URL and wrong
//     for a command line containing a URL plus a later '@':
//     "PGURL=postgres://u:p@h/db go test --to dev@example.com" came back as
//     "PGURL=postgres://u:***@example.com" — the credential removed, but the
//     host silently replaced and everything between deleted.
//   - password/token assignments — `key=value`, `key: value`, prefixed names
//     like POST_DB_PASSWORD, quoted JSON/YAML keys — plus `Bearer <token>` and
//     `--password <value>`, and the well-known credential prefixes.
//   - the generic long-run heuristic last, because it is the only rule that can
//     fire on something that is not a secret. A 40-character commit sha does
//     become "***" and the reader loses which commit the text was about; that
//     loss is bounded here to the sha rather than to the whole reason.
//
// A value the key rule cannot take whole is masked only as far as the rule
// reaches: `password=pa ss` leaves ` ss` behind, because the value alternative
// stops at whitespace. That is a residual, not a guarantee, and it is why the
// URL rule above is the one that has to handle a whitespace-bearing credential
// properly — it is the shape a wrapped command line actually produces.
func RedactTextForOutput(v string) string {
	if v == "" {
		return v
	}
	out := redactUserinfoPerToken(v)
	out = bearerRe.ReplaceAllString(out, "${1}${2}***")
	out = secretFlagRe.ReplaceAllString(out, "${1}***")
	out = secretAssignmentRe.ReplaceAllString(out, "${1}${3}***")
	out = secretTokenRe.ReplaceAllString(out, "***")
	return out
}

// redactUserinfoPerToken applies RedactURL to each whitespace-separated token
// and copies everything else byte for byte, whitespace included. Splitting into
// tokens is what keeps a URL-shaped redactor from being applied to a string
// that merely contains a URL; copying the separators is what keeps the result
// faithful enough to be committed as a record.
//
// Whitespace is not always a token boundary, which the first version of this
// function got wrong. A userinfo may CONTAIN whitespace — a shell line
// continuation inside a pasted DSN, a tab, a space in a password — and then
// splitting hands RedactURL two halves neither of which has an "@":
//
//	in   POSTGRES_TEST_ADMIN_URL=postgres://postgres:hunter2\<newline>@127.0.0.1:5432/post go test
//	half postgres://postgres:hunter2     — "://", no "@", returned unchanged
//	half @127.0.0.1:5432/post            — no "://", returned unchanged
//
// and the credential goes into the committed file. RedactURL cannot see this
// from one token: it needs both the "://" and the "@". So a token that is a URL
// whose userinfo is still OPEN — userinfoCrosses below decides that, and the
// separator is half the question — is not a token yet. It absorbs the separator
// and the next token when that token supplies the "@", and the credential is the
// whole span between them. The separators inside it are part of the password, so
// they are redacted with it rather than copied.
//
// Where the wrap lands is not fixed, and the colon was the wrong thing to lean
// on for deciding that a run is unfinished. A `:<newline>` is one of four
// positions; `postgres://postgres\<newline>:hunter2@h` and
// `postgres://\<newline>postgres:hunter2@h` put the first token where neither of
// userinfoIsOpen's two tests fires, because the colon it looks for is on the
// other side of the wrap. The shell line continuation is what is actually
// visible at all four, so it is what userinfoCrosses requires before it lets one
// line be joined to the next.
//
// Keeping that narrow is what stops this collapsing back into the whole-string
// rule the function exists to replace. Two shapes that look similar are left
// alone, both correctly:
//
//	postgres://h/db go test --to dev@example.com  no colon and nothing
//	                                              credential-shaped after the
//	                                              "//", so the URL is complete
//	                                              and has no userinfo
//	postgres://host:5432 go run dev@example.com   userinfo looks open, but the
//	                                              next token has no "@", so
//	                                              nothing is absorbed
//
// It is the second of those that would otherwise repeat the old bug: the old
// rule redacted the host and deleted everything between, on a line that
// contained no credential at all. That is still true with a newline instead of a
// space, and by the same route — the fix is only that a newline now has to be
// earned by a continuation, so
//
//	postgres://host:5432<newline>owner@example.com
//
// is left alone rather than becoming `postgres://host:***@example.com`. The
// residual gap is a password containing MORE than one whitespace run
// ("u:pa ss word@h"), where the next token alone does not reach the "@"; that is
// left as-is and named here rather than papered over, because absorbing further
// tokens is exactly the over-redaction above.
func redactUserinfoPerToken(v string) string {
	if !strings.Contains(v, "://") {
		return v
	}
	var b strings.Builder
	b.Grow(len(v))
	i := 0
	for i < len(v) {
		j := i
		for j < len(v) && isSpaceByte(v[j]) {
			j++
		}
		b.WriteString(v[i:j]) // separators verbatim
		if j == len(v) {
			break
		}
		k := j
		for k < len(v) && !isSpaceByte(v[k]) {
			k++
		}
		tok := v[j:k]
		m := k
		for m < len(v) && isSpaceByte(v[m]) {
			m++
		}
		n := m
		for n < len(v) && !isSpaceByte(v[n]) {
			n++
		}
		if n > m && strings.ContainsRune(v[m:n], '@') && userinfoCrosses(tok, v[k:m]) {
			b.WriteString(redactOpenUserinfo(tok + v[k:m] + v[m:n]))
			i = n
			continue
		}
		b.WriteString(RedactURL(tok))
		i = k
	}
	return b.String()
}

// userinfoCrosses reports whether the separator after tok may be crossed — that
// is, whether tok and the token after it are really one token that whitespace
// happens to have cut in half.
//
// The two separators are not the same question, and treating them as one is
// what let a credential through at two of the four wrap positions.
//
// A space or a tab is crossed on userinfoIsOpen alone: the run before it looks
// like a userinfo still being written, so it is one.
//
// A NEWLINE is crossed only at a VISIBLE cut. A shell line continuation puts a
// "\" at the end of the line — the token did not end there, it was cut there,
// and everything the shell joins onto that line belongs to it. Nothing else
// justifies joining two lines: a line that ends without one has ended. Without
// this, `postgres://host:5432` on one line and an email address on the next are
// read as a userinfo that spans them, the port is deleted and an address is
// rewritten on a line containing no credential at all.
//
// Requiring the "\" is also what closes the two positions userinfoIsOpen cannot
// see. The token on the first line is then only required to BE half a token —
// "://", no "@" — because a dangling continuation is itself proof of that: at
//
//	POSTGRES_TEST_ADMIN_URL=postgres://postgres\<newline>:hunter2@127.0.0.1:5432/post
//	POSTGRES_TEST_ADMIN_URL=postgres://\<newline>postgres:hunter2@127.0.0.1:5432/post
//
// the first token is `postgres://postgres\` and `postgres://\`, which have no
// colon and nothing credential-shaped after the "//". userinfoIsOpen says a
// complete URL with no userinfo; the continuation says otherwise, and is right.
func userinfoCrosses(tok, sep string) bool {
	if strings.ContainsAny(sep, "\n\r") {
		return strings.HasSuffix(tok, `\`) &&
			strings.Contains(tok, "://") &&
			!strings.ContainsRune(tok, '@')
	}
	return userinfoIsOpen(tok)
}

// userinfoIsOpen reports whether a token carries "://" and then the beginning
// of a userinfo that has not been closed yet. A token that already has an "@"
// is complete and RedactURL's job.
//
// Two beginnings count. A colon before any "/" or "@" is `user:` with the
// password still to come. Failing that, a run that is itself credential-shaped
// — `https://ghp_xxx` with the "@" on the next line — is a token-only userinfo
// in progress, and a URL whose host is `ghp_xxx` is not a thing. The second
// test is what keeps `postgres://host` (a real host, no path) a complete URL:
// it is neither colon-leading nor credential-shaped, so nothing is absorbed.
//
// This answers only the space-and-tab half of the question; a newline is
// userinfoCrosses' to decide, and it asks a narrower one.
func userinfoIsOpen(tok string) bool {
	if strings.ContainsRune(tok, '@') {
		return false
	}
	_, rest, ok := strings.Cut(tok, "://")
	if !ok {
		return false
	}
	if cut := strings.IndexAny(rest, "/@"); cut >= 0 {
		rest = rest[:cut]
	}
	if strings.ContainsRune(rest, ':') {
		return true
	}
	return secretTokenRe.MatchString(rest)
}

// redactOpenUserinfo masks the userinfo of a URL span that was allowed to run
// across whitespace. Same rule as RedactURL — keep the user, drop the secret,
// drop a token-only userinfo entirely — applied to a span that is known to
// contain the closing "@".
func redactOpenUserinfo(span string) string {
	scheme, rest, ok := strings.Cut(span, "://")
	if !ok {
		return span
	}
	at := strings.LastIndex(rest, "@")
	if at <= 0 {
		return span
	}
	userinfo := rest[:at]
	if user, _, ok := strings.Cut(userinfo, ":"); ok {
		return scheme + "://" + user + ":***@" + rest[at+1:]
	}
	return scheme + "://***@" + rest[at+1:]
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

// truncateRedacted redacts a value and caps its length for an error message.
// Error text should help locate a bad line, not reproduce its contents — an
// unbounded echo is how a credential reaches a log in the first place.
func truncateRedacted(v string) string {
	const max = 24
	r := RedactForOutput(v)
	if len(r) > max {
		return r[:max] + "…"
	}
	return r
}

// RedactURL strips embedded credentials from a URL before it is ever emitted.
//
// `postgres://user:pw@host/db` and `https://x-access-token:ghp_xxx@github/…`
// are common credential-bearing forms. The password/token must not reach
// logs, error messages or JSON output, so every URL that lands in an output
// path goes through here first. This mirrors scripts/speclib.py redact_url()
// exactly (T0004 precedent):
//
//   - only scheme://userinfo@host is rewritten; `user:secret@host` becomes
//     `user:***@host`, token-only userinfo becomes `***@host`;
//   - scp-like `git@host:path` keeps its conventional user and is left alone;
//   - the input is returned unchanged when it carries no userinfo.
func RedactURL(url string) string {
	if !strings.Contains(url, "://") {
		return url
	}
	scheme, rest, _ := strings.Cut(url, "://")
	if !strings.Contains(rest, "@") {
		return url
	}
	// Split at the LAST '@': a password may itself contain '@'.
	idx := strings.LastIndex(rest, "@")
	userinfo, hostpart := rest[:idx], rest[idx+1:]
	if userinfo == "" {
		return url
	}
	if user, _, ok := strings.Cut(userinfo, ":"); ok {
		// user:secret@host -> user:***@host (keeps the user, drops the secret)
		return scheme + "://" + user + ":***@" + hostpart
	}
	return scheme + "://***@" + hostpart
}
