package authn

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lichman0405/post/internal/config"
)

// Config is the validated authentication configuration. Values are parsed
// once at startup (fail-closed like internal/config, T0006): a missing or
// invalid variable refuses to start, there is no silent fallback.
type Config struct {
	// WebOrigin is the browser origin the API serves (CORS + OIDC
	// redirects). Default http://127.0.0.1:3000 (the dev web app).
	WebOrigin string
	// SessionTTL is the server-side session lifetime.
	SessionTTL time.Duration
	// LoginLimitPerEmail / LoginLimitPerIP / LoginWindow configure the
	// login rate limit (fixed window). SignupLimitPerIP limits signup
	// attempts per IP in the same window — signup burns the same argon2id
	// work plus an INSERT per call, so it needs its own budget (per-IP:
	// the DoS vector is rotating emails, not one email).
	LoginLimitPerEmail int
	LoginLimitPerIP    int
	LoginWindow        time.Duration
	SignupLimitPerIP   int
	// OIDC holds the provider settings; Enabled is false when no issuer
	// is configured and the OIDC endpoints answer OIDC_NOT_CONFIGURED.
	OIDC OIDCConfig
}

// OIDCConfig configures one OIDC provider.
type OIDCConfig struct {
	Enabled      bool
	Issuer       string
	ClientID     string
	ClientSecret config.Secret // masked on every output path (internal/config)
	// RedirectPath is the API's own callback path (fixed: the deployment
	// is one server; the provider gets a full URL built from the request).
	RedirectPath string
}

// Defaults (documented constants so the smoke/e2e runs need no extra
// variables). POST_WEB_ORIGIN=... overrides WebOrigin.
const (
	DefaultWebOrigin        = "http://127.0.0.1:3000"
	DefaultSessionTTL       = 24 * time.Hour
	DefaultLoginPerEmail    = 5
	DefaultLoginPerIP       = 20
	DefaultSignupPerIP      = 10
	DefaultLoginWindow      = time.Minute
	DefaultOIDCRedirectPath = "/api/v1/auth/oidc/callback"
)

// EnvNames: every variable this package reads (the canonical loader is
// internal/config; these fold into it in a follow-up — see T0101 RESULT).
const (
	EnvWebOrigin       = "POST_WEB_ORIGIN"
	EnvSessionTTL      = "POST_AUTH_SESSION_TTL"
	EnvLoginPerEmail   = "POST_AUTH_LOGIN_PER_EMAIL"
	EnvLoginPerIP      = "POST_AUTH_LOGIN_PER_IP"
	EnvLoginWindow     = "POST_AUTH_LOGIN_WINDOW"
	EnvSignupPerIP     = "POST_AUTH_SIGNUP_PER_IP"
	EnvOIDCIssuer      = "POST_OIDC_ISSUER"
	EnvOIDCClientID    = "POST_OIDC_CLIENT_ID"
	EnvOIDCClientSec   = "POST_OIDC_CLIENT_SECRET"
	envOIDCCallbackURL = "POST_OIDC_REDIRECT_URI" // optional override
)

// Loader resolves the environment for Config.Load; the zero value reads
// the process environment. Mirrors config.Loader so tests inject values
// without touching the real environment.
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

// Error aggregates configuration problems; it never contains secret
// values (the secret is only ever echoed as ***).
type Error struct {
	Problems []Problem
}

func (e *Error) Error() string {
	if len(e.Problems) == 1 {
		p := e.Problems[0]
		return fmt.Sprintf("authn config: %s: %s (%s)", p.Key, p.Msg, p.Fix)
	}
	return fmt.Sprintf("authn config: %d problems", len(e.Problems))
}

// Load validates the authentication configuration. OIDC is optional: an
// empty POST_OIDC_ISSUER disables it, but a set issuer requires client id
// and secret — half-configured OIDC refuses to start, never silently
// degrades.
func (l Loader) Load() (*Config, error) {
	values := map[string]string{}
	for _, key := range []string{
		EnvWebOrigin, EnvSessionTTL, EnvLoginPerEmail, EnvLoginPerIP,
		EnvLoginWindow, EnvSignupPerIP, EnvOIDCIssuer, EnvOIDCClientID, EnvOIDCClientSec,
		envOIDCCallbackURL,
	} {
		if v := l.getenv(key); strings.TrimSpace(v) != "" {
			values[key] = v
		}
	}

	cfg := &Config{
		WebOrigin:          DefaultWebOrigin,
		SessionTTL:         DefaultSessionTTL,
		LoginLimitPerEmail: DefaultLoginPerEmail,
		LoginLimitPerIP:    DefaultLoginPerIP,
		LoginWindow:        DefaultLoginWindow,
		SignupLimitPerIP:   DefaultSignupPerIP,
		OIDC: OIDCConfig{
			RedirectPath: DefaultOIDCRedirectPath,
		},
	}
	var problems []Problem
	bad := func(key, msg, fix string) {
		problems = append(problems, Problem{Key: key, Msg: msg, Fix: fix})
	}

	if v, ok := values[EnvWebOrigin]; ok {
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" && u.Path != "/" {
			bad(EnvWebOrigin, fmt.Sprintf("invalid value %s: must be an origin (scheme://host, no path)", strconv.Quote(config.RedactForOutput(v))),
				"set "+EnvWebOrigin+" to an origin like http://127.0.0.1:3000")
		} else {
			cfg.WebOrigin = strings.TrimSuffix(v, "/")
		}
	}
	if v, ok := values[EnvSessionTTL]; ok {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Minute || d > 30*24*time.Hour {
			bad(EnvSessionTTL, fmt.Sprintf("invalid value %s: must be a duration between 1m and 720h", strconv.Quote(v)),
				"set "+EnvSessionTTL+" to a Go duration, e.g. 24h")
		} else {
			cfg.SessionTTL = d
		}
	}
	parseInt := func(key string, def int, lo, hi int) int {
		v, ok := values[key]
		if !ok {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi {
			bad(key, fmt.Sprintf("invalid value %s: must be an integer between %d and %d", strconv.Quote(v), lo, hi),
				"set "+key+" to an integer")
			return def
		}
		return n
	}
	cfg.LoginLimitPerEmail = parseInt(EnvLoginPerEmail, DefaultLoginPerEmail, 1, 1000)
	cfg.LoginLimitPerIP = parseInt(EnvLoginPerIP, DefaultLoginPerIP, 1, 10000)
	cfg.SignupLimitPerIP = parseInt(EnvSignupPerIP, DefaultSignupPerIP, 1, 10000)
	if v, ok := values[EnvLoginWindow]; ok {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second || d > time.Hour {
			bad(EnvLoginWindow, fmt.Sprintf("invalid value %s: must be a duration between 1s and 1h", strconv.Quote(v)),
				"set "+EnvLoginWindow+" to a Go duration, e.g. 1m")
		} else {
			cfg.LoginWindow = d
		}
	}

	if issuer, ok := values[EnvOIDCIssuer]; ok {
		cfg.OIDC.Enabled = true
		cfg.OIDC.Issuer = strings.TrimSuffix(issuer, "/")
		cfg.OIDC.ClientID = values[EnvOIDCClientID]
		cfg.OIDC.ClientSecret = config.Secret(values[EnvOIDCClientSec])
		if cfg.OIDC.ClientID == "" {
			bad(EnvOIDCClientID, "OIDC is enabled (issuer set) but the client id is missing",
				"set "+EnvOIDCClientID+" to the provider-issued client id")
		}
		if cfg.OIDC.ClientSecret.Empty() {
			bad(EnvOIDCClientSec, "OIDC is enabled (issuer set) but the client secret is missing",
				"set "+EnvOIDCClientSec+" to the provider-issued client secret (never commit it)")
		}
		if v, ok := values[envOIDCCallbackURL]; ok {
			if !strings.HasPrefix(v, "/") || strings.Contains(v, "?") {
				bad(envOIDCCallbackURL, fmt.Sprintf("invalid value %s: must be an absolute path", strconv.Quote(v)),
					"set "+envOIDCCallbackURL+" to a path like "+DefaultOIDCRedirectPath)
			} else {
				cfg.OIDC.RedirectPath = v
			}
		}
	}

	if len(problems) > 0 {
		return nil, &Error{Problems: problems}
	}
	return cfg, nil
}
