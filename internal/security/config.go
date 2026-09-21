package security

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Every budget is an engineering constant, not a spec value: no document
// in this repository states a number (docs/23 §7 asks for "rate limiting"
// and "login brute force protection" and stops there). The defaults below
// are the conservative reading of those two words for a single-node V1
// deployment, and every one of them is overridable — a deployment whose
// real traffic does not fit a default must be able to change it without a
// code change.
const (
	// DefaultWindow mirrors authn.DefaultLoginWindow (1 minute): one
	// fixed-window size across the whole system means an operator reading
	// a Retry-After has one mental model, not two.
	DefaultWindow = time.Minute
	// DefaultAnonymousPerIP = 240/min = 4 req/s sustained from one IP.
	// A browser page load on this app costs a handful of API calls, so a
	// human is never near it; a scraper or a fuzzer is immediately over.
	DefaultAnonymousPerIP = 240
	// DefaultAuthenticatedPerSession = 1200/min = 20 req/s. Higher than
	// the anonymous budget on purpose: a signed-in user is identified, so
	// the cost of a false positive (breaking a legitimate bulk action) is
	// higher and the cost of a false negative (one identified account
	// hammering) is bounded by the account's existence.
	DefaultAuthenticatedPerSession = 1200
	// DefaultCredentialPerIP = 20/min, deliberately the same number as
	// authn.DefaultLoginPerIP. The two are different mechanisms with the
	// same policy value; matching them means neither ever masks the other
	// in a test that counts attempts.
	DefaultCredentialPerIP = 20
	// DefaultOutboundPerKey = 10/min. Every one of these calls makes the
	// API perform an outbound HTTP request to a caller-chosen URL; the
	// budget is small because the operation is rare and expensive.
	DefaultOutboundPerKey = 10
)

// Environment variables. Names are prefixed POST_SECURITY_ so they sort
// next to the authn limits they complement.
const (
	EnvWindow            = "POST_SECURITY_RATE_LIMIT_WINDOW"
	EnvAnonymousPerIP    = "POST_SECURITY_RATE_LIMIT_ANONYMOUS_PER_IP"
	EnvAuthenticatedPerS = "POST_SECURITY_RATE_LIMIT_AUTHENTICATED_PER_SESSION"
	EnvCredentialPerIP   = "POST_SECURITY_RATE_LIMIT_CREDENTIAL_PER_IP"
	EnvOutboundPerKey    = "POST_SECURITY_RATE_LIMIT_OUTBOUND_PER_KEY"
)

// There is deliberately no "turn the limiter off" environment variable.
// The five budgets above are the operator's knob: a deployment behind its
// own gateway raises them, and the raised number still bounds what a
// single client can do. A boolean switch would instead be one environment
// variable away from an API with no edge limit at all, set once during an
// incident and never unset — the same reason authn's login limits are
// numbers and not a flag.

// Loader resolves the environment; the zero value reads the process
// environment. It mirrors authn.Loader so tests inject values without
// touching the real environment.
type Loader struct {
	Getenv func(string) string
}

func (l Loader) getenv(key string) string {
	if l.Getenv == nil {
		return os.Getenv(key)
	}
	return l.Getenv(key)
}

// Problem is one configuration failure (mirrors internal/config.Problem).
type Problem struct {
	Key string
	Msg string
	Fix string
}

// Error aggregates configuration problems. No value here is secret, so
// unlike authn's config errors the message may quote the offending value.
type Error struct {
	Problems []Problem
}

func (e *Error) Error() string {
	if len(e.Problems) == 1 {
		p := e.Problems[0]
		return fmt.Sprintf("security config: %s: %s (%s)", p.Key, p.Msg, p.Fix)
	}
	return fmt.Sprintf("security config: %d problems", len(e.Problems))
}

// Load validates the edge configuration. Fail-closed like the rest of the
// tree (internal/config, authn.Config): an unparseable or out-of-range
// value refuses to start rather than silently falling back to a default
// the operator did not choose.
func (l Loader) Load() (*RateLimitConfig, error) {
	cfg := &RateLimitConfig{
		Window:                  DefaultWindow,
		AnonymousPerIP:          DefaultAnonymousPerIP,
		AuthenticatedPerSession: DefaultAuthenticatedPerSession,
		CredentialPerIP:         DefaultCredentialPerIP,
		OutboundPerKey:          DefaultOutboundPerKey,
		ExemptPaths:             ExemptProbePaths(),
	}
	var problems []Problem
	bad := func(key, msg, fix string) {
		problems = append(problems, Problem{Key: key, Msg: msg, Fix: fix})
	}

	if v := strings.TrimSpace(l.getenv(EnvWindow)); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second || d > time.Hour {
			bad(EnvWindow, fmt.Sprintf("invalid value %q: must be a duration between 1s and 1h", v),
				"set "+EnvWindow+" to a Go duration, e.g. 1m")
		} else {
			cfg.Window = d
		}
	}
	parseInt := func(key string, dst *int, lo, hi int) {
		v := strings.TrimSpace(l.getenv(key))
		if v == "" {
			return
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi {
			bad(key, fmt.Sprintf("invalid value %q: must be an integer between %d and %d", v, lo, hi),
				"set "+key+" to an integer")
			return
		}
		*dst = n
	}
	// Lower bounds are 1, not 0: a budget of zero would make every request
	// to that class a 429, which is the one way to "configure" this
	// middleware into an outage. The upper bound is a sanity ceiling, not a
	// policy: a budget of a million is an operator saying "effectively no
	// limit", which is their call to make explicitly.
	parseInt(EnvAnonymousPerIP, &cfg.AnonymousPerIP, 1, 1_000_000)
	parseInt(EnvAuthenticatedPerS, &cfg.AuthenticatedPerSession, 1, 1_000_000)
	parseInt(EnvCredentialPerIP, &cfg.CredentialPerIP, 1, 1_000_000)
	parseInt(EnvOutboundPerKey, &cfg.OutboundPerKey, 1, 1_000_000)

	if len(problems) > 0 {
		return nil, &Error{Problems: problems}
	}
	return cfg, nil
}
