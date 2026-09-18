package notifications

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Notifications configuration (T1005). The worker reads it at startup, like
// every other surface in the repo: a variable that is set but unusable
// refuses to start rather than degrading silently.
//
// The variables are declared here rather than in internal/config because
// this package is where they are consumed — the same arrangement
// internal/application/authn uses for its own POST_* surface. They are read
// through a Loader, so a test injects values without touching the process
// environment.

// EnvMailSinkDir enables email delivery: the directory the development mail
// sink writes messages to. UNSET means email is off — the worker runs
// without a transport, the digest sender does not start, and the pending
// email rows the subscription fan-out writes stay pending (they are not
// lost, and a worker that is given a sink later sends them). This mirrors
// the other optional subsystems (the Gitea token surface starts off and
// says so).
const EnvMailSinkDir = "POST_MAIL_SINK_DIR"

// EnvWebOrigin is the browser origin the digest's links are built on.
// It is the same variable internal/application/authn loads (see its
// EnvWebOrigin) — one deployment, one origin — and the two values must
// agree. The string is repeated here rather than imported from authn: the
// worker must be able to build a link without the OIDC and session
// configuration the API's loader validates, and importing an application
// package for one constant would make the worker's startup depend on the
// whole authn surface.
const EnvWebOrigin = "POST_WEB_ORIGIN"

// DefaultWebOrigin is the dev web app (the same default authn carries).
const DefaultWebOrigin = "http://127.0.0.1:3000"

// Config is the validated notifications configuration.
type Config struct {
	// SinkDir is the dev mail sink's directory; "" when email delivery is
	// off (EnvMailSinkDir unset).
	SinkDir string
	// WebOrigin is the origin digest links point at (no trailing slash).
	WebOrigin string
	// Enabled reports whether a transport is configured. It is derived, not
	// a second input: the sink directory is the switch.
	Enabled bool
}

// Loader resolves the environment for Config.Load; the zero value reads the
// process environment (the internal/config and authn.Loader shape).
type Loader struct {
	Getenv func(string) string
}

func (l Loader) getenv(key string) string {
	if l.Getenv == nil {
		return os.Getenv(key)
	}
	return l.Getenv(key)
}

// Load reads and validates the notifications configuration. Only the web
// origin can be invalid: a sink directory is a path the sink creates if it
// does not exist, and everything about it is checked when the sink opens
// (NewDevSink), where the failure names the path.
func (l Loader) Load() (Config, error) {
	cfg := Config{
		SinkDir:   strings.TrimSpace(l.getenv(EnvMailSinkDir)),
		WebOrigin: DefaultWebOrigin,
	}
	if v := strings.TrimSpace(l.getenv(EnvWebOrigin)); v != "" {
		origin, err := parseOrigin(v)
		if err != nil {
			return Config{}, fmt.Errorf("notifications config: %s: %w", EnvWebOrigin, err)
		}
		cfg.WebOrigin = origin
	}
	cfg.Enabled = cfg.SinkDir != ""
	return cfg, nil
}

// parseOrigin accepts scheme://host[:port] with no path — the same rule
// authn applies to the same variable. A URL with a path would silently
// produce links like http://host/app/projects/x when the app is served at
// the root, so it is refused instead of guessed at.
func parseOrigin(v string) (string, error) {
	u, err := url.Parse(v)
	if err != nil {
		return "", fmt.Errorf("invalid value %s: must be an origin (scheme://host, no path)", strconv.Quote(v))
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("invalid value %s: must be an origin (scheme://host, no path)", strconv.Quote(v))
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid value %s: must be an origin (scheme://host, no path)", strconv.Quote(v))
	}
	return strings.TrimSuffix(v, "/"), nil
}
