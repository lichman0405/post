package config

import "strings"

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
