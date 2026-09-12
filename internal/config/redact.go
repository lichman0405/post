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

// dsnSecretRe matches the Postgres keyword/value DSN form
// (`host=... password=secret`), which carries a credential without any `://`.
var dsnSecretRe = regexp.MustCompile(`(?i)\b(password|passwd|pwd|token|secret|api_?key|access_?key)\s*=\s*("[^"]*"|'[^']*'|\S+)`)

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
	if r := dsnSecretRe.ReplaceAllString(v, "${1}=***"); r != v {
		return r
	}
	if secretTokenRe.MatchString(v) {
		return "***"
	}
	return v
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
