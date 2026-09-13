package gitprovider

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/lichman0405/post/internal/config"
)

// Config is the validated GitProvider adapter configuration, parsed once at
// startup. T0006 applies to VALUES, not absence (mirroring the authn
// package's optional loader, internal/application/authn): a PRESENT variable
// must be valid — an invalid value refuses to start, naming the key, with no
// silent fallback — while an UNSET token or webhook URL is a legal
// deployment (a machine without Gitea wants no webhooks), so the load
// succeeds with provisioning disabled and the missing keys on Missing. The
// canonical loader is internal/config; these keys fold into it when that
// package is in scope again (see T0301 RESULT follow-up).
type Config struct {
	// BaseURL is the provider API base URL, e.g. http://127.0.0.1:3000.
	BaseURL string
	// Token is the service account access token (init-gitea.sh's bot user
	// post-git-svc): every provider call the platform makes uses it, so
	// the provider audit trail attributes all platform-driven changes to
	// one machine identity (docs/16 §3 controlled service identity).
	// Empty when the key is unset — provisioning is then disabled.
	Token config.Secret
	// WebhookURL is the delivery target registered on every provisioned
	// repository's push webhook — the platform API endpoint the provider
	// must be able to reach (docs/16 §4 push ingestion, T0305's receiver).
	// There is no derivable default: in local dev it is the docker-bridge
	// gateway address of the host (the provider runs in a container), in
	// deployments it is the API's public URL. Empty when the key is unset
	// — provisioning is then disabled: a deployment that cannot reach the
	// API from the provider must not register hooks that can never
	// deliver, and must not refuse to start over it.
	WebhookURL string
	// Missing lists the environment keys that are unset. Non-empty means
	// provisioning is DISABLED: the API starts without the GitProvider
	// features and logs a warning naming these keys (names only — key
	// names never carry secrets).
	Missing []string
}

// ProvisioningEnabled reports whether the full provisioning configuration
// is present. When false, callers must skip wiring the provisioning
// pipeline and say which keys are missing (Config.Missing).
func (c *Config) ProvisioningEnabled() bool { return len(c.Missing) == 0 }

// EnvNames: every variable this package reads.
const (
	EnvBaseURL    = "POST_GITEA_BASE_URL"
	EnvToken      = "POST_GITEA_TOKEN"
	EnvWebhookURL = "POST_GITEA_WEBHOOK_URL"
)

// DefaultBaseURL matches docker-compose's GITEA_WEB_PORT default (the dev
// stack; every deployment overrides it).
const DefaultBaseURL = "http://127.0.0.1:3000"

// Loader resolves the environment for Config.Load; the zero value reads
// the process environment. Mirrors config.Loader/authn.Loader so tests
// inject values without touching the real environment.
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
// values (a bad token is echoed as ***, and every URL is redacted — a URL
// may embed credentials).
type Error struct {
	Problems []Problem
}

func (e *Error) Error() string {
	// Every problem names its key — including the aggregated branch: the
	// most common first-run case (several bad values at once) is exactly
	// the one that needs the keys spelled out, and each per-problem
	// message is redacted by construction.
	msgs := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		msgs[i] = fmt.Sprintf("%s: %s (%s)", p.Key, p.Msg, p.Fix)
	}
	return "gitprovider config: " + strings.Join(msgs, "; ")
}

// Load validates the GitProvider configuration. BaseURL defaults to the
// dev stack. Token and WebhookURL have no defaults and are OPTIONAL: an
// unset key (or a blank value) disables provisioning — the load succeeds
// and the key lands on Config.Missing for the caller's warning — while a
// PRESENT key must be valid, or the load fails naming the key (T0006
// validates values, not absence).
func (l Loader) Load() (*Config, error) {
	cfg := &Config{BaseURL: DefaultBaseURL}
	var problems []Problem
	bad := func(key, msg, fix string) {
		problems = append(problems, Problem{Key: key, Msg: msg, Fix: fix})
	}

	if v := strings.TrimSpace(l.getenv(EnvBaseURL)); v != "" {
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			bad(EnvBaseURL, fmt.Sprintf("invalid value %s: must be an http(s) URL",
				strconv.Quote(config.RedactURL(v))),
				"set "+EnvBaseURL+" to the internal Gitea API URL, e.g. http://127.0.0.1:3000")
		} else {
			cfg.BaseURL = strings.TrimSuffix(v, "/")
		}
	}

	token := strings.TrimSpace(l.getenv(EnvToken))
	if token == "" {
		cfg.Missing = append(cfg.Missing, EnvToken)
	} else {
		cfg.Token = config.Secret(token)
	}

	if v := strings.TrimSpace(l.getenv(EnvWebhookURL)); v == "" {
		cfg.Missing = append(cfg.Missing, EnvWebhookURL)
	} else {
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			bad(EnvWebhookURL, fmt.Sprintf("invalid value %s: must be an http(s) URL",
				strconv.Quote(config.RedactURL(v))),
				"set "+EnvWebhookURL+" to the platform API URL the provider can reach")
		} else {
			cfg.WebhookURL = strings.TrimSuffix(v, "/")
		}
	}

	if len(problems) > 0 {
		return nil, &Error{Problems: problems}
	}
	return cfg, nil
}
