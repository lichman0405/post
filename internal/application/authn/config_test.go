package authn_test

import (
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/authn"
)

// env builds a Loader over an explicit environment map: only the injected
// keys exist, so tests never leak the real process environment.
func env(values map[string]string) authn.Loader {
	return authn.Loader{Getenv: func(key string) string { return values[key] }}
}

func TestConfigDefaults(t *testing.T) {
	cfg, err := env(map[string]string{}).Load()
	if err != nil {
		t.Fatalf("Load(defaults): %v", err)
	}
	if cfg.WebOrigin != authn.DefaultWebOrigin {
		t.Errorf("WebOrigin = %q, want default %q", cfg.WebOrigin, authn.DefaultWebOrigin)
	}
	if cfg.SessionTTL != authn.DefaultSessionTTL {
		t.Errorf("SessionTTL = %v, want %v", cfg.SessionTTL, authn.DefaultSessionTTL)
	}
	if cfg.LoginLimitPerEmail != 5 || cfg.LoginLimitPerIP != 20 || cfg.LoginWindow != time.Minute {
		t.Errorf("login limits = %d/%d/%v, want 5/20/1m", cfg.LoginLimitPerEmail, cfg.LoginLimitPerIP, cfg.LoginWindow)
	}
	if cfg.SignupLimitPerIP != authn.DefaultSignupPerIP {
		t.Errorf("SignupLimitPerIP = %d, want default %d", cfg.SignupLimitPerIP, authn.DefaultSignupPerIP)
	}
	if cfg.OIDC.Enabled {
		t.Error("OIDC must be disabled when no issuer is configured")
	}
}

func TestConfigValidOverride(t *testing.T) {
	cfg, err := env(map[string]string{
		authn.EnvWebOrigin:     "https://post.example.com/",
		authn.EnvSessionTTL:    "48h",
		authn.EnvLoginPerEmail: "10",
		authn.EnvLoginPerIP:    "50",
		authn.EnvLoginWindow:   "5m",
		authn.EnvSignupPerIP:   "30",
		authn.EnvOIDCIssuer:    "https://id.example.com/",
		authn.EnvOIDCClientID:  "post-client",
		authn.EnvOIDCClientSec: "post-secret",
	}).Load()
	if err != nil {
		t.Fatalf("Load(valid): %v", err)
	}
	if cfg.WebOrigin != "https://post.example.com" {
		t.Errorf("WebOrigin = %q (trailing slash must be trimmed)", cfg.WebOrigin)
	}
	if cfg.SessionTTL != 48*time.Hour || cfg.LoginLimitPerEmail != 10 || cfg.LoginLimitPerIP != 50 || cfg.LoginWindow != 5*time.Minute {
		t.Errorf("parsed config = %+v", cfg)
	}
	if cfg.SignupLimitPerIP != 30 {
		t.Errorf("SignupLimitPerIP = %d, want 30", cfg.SignupLimitPerIP)
	}
	if !cfg.OIDC.Enabled || cfg.OIDC.Issuer != "https://id.example.com" {
		t.Errorf("OIDC = %+v, want enabled with trimmed issuer", cfg.OIDC)
	}
	if string(cfg.OIDC.ClientSecret) != "post-secret" {
		t.Error("client secret not parsed")
	}
	if cfg.OIDC.RedirectPath != authn.DefaultOIDCRedirectPath {
		t.Errorf("RedirectPath = %q, want default %q", cfg.OIDC.RedirectPath, authn.DefaultOIDCRedirectPath)
	}
}

// TestConfigRejectsHalfConfiguredOIDC: an issuer without credentials must
// refuse to start (no silent degradation) and must never echo the secret.
func TestConfigRejectsHalfConfiguredOIDC(t *testing.T) {
	_, err := env(map[string]string{
		authn.EnvOIDCIssuer:    "https://id.example.com",
		authn.EnvOIDCClientSec: "super-secret-value",
		// Client id missing -> problem.
	}).Load()
	// (The secret above is set; the missing piece is the client id. The
	// point of this specific shape: the error must mention POST_OIDC_CLIENT_ID
	// and must NOT contain the secret anywhere in its text.)
	if err == nil {
		t.Fatal("half-configured OIDC accepted")
	}
	if !strings.Contains(err.Error(), authn.EnvOIDCClientID) {
		t.Errorf("error %q does not name the missing key %s", err, authn.EnvOIDCClientID)
	}
	if strings.Contains(err.Error(), "super-secret-value") {
		t.Error("config error leaks the client secret")
	}

	// Missing secret (id present) is the mirror image.
	_, err = env(map[string]string{
		authn.EnvOIDCIssuer:   "https://id.example.com",
		authn.EnvOIDCClientID: "post-client",
	}).Load()
	if err == nil || !strings.Contains(err.Error(), authn.EnvOIDCClientSec) {
		t.Errorf("missing secret = %v, want problem naming %s", err, authn.EnvOIDCClientSec)
	}
}

func TestConfigRejectsBadValues(t *testing.T) {
	cases := map[string]string{
		authn.EnvWebOrigin:     "not-a-url",
		authn.EnvSessionTTL:    "1s", // below the 1m floor
		authn.EnvLoginPerEmail: "0",
		authn.EnvLoginPerIP:    "-5",
		authn.EnvLoginWindow:   "200h",   // above the 1h ceiling
		authn.EnvSignupPerIP:   "100000", // above the 10000 ceiling
	}
	for key, value := range cases {
		_, err := env(map[string]string{key: value}).Load()
		if err == nil {
			t.Errorf("%s=%q accepted", key, value)
			continue
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error %q does not name the offending key %s", err, key)
		}
	}
}

// TestConfigOIDCCredentialsWithoutIssuerIgnored: credentials alone do not
// enable OIDC (the issuer is the switch) — but also do not break loading.
func TestConfigOIDCCredentialsWithoutIssuerIgnored(t *testing.T) {
	cfg, err := env(map[string]string{
		authn.EnvOIDCClientID:  "post-client",
		authn.EnvOIDCClientSec: "post-secret",
	}).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OIDC.Enabled {
		t.Error("credentials without an issuer must not enable OIDC")
	}
}
