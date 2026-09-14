package gitprovider_test

import (
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/gitprovider"
)

// env builds a Loader over an explicit environment map: only the injected
// keys exist, so tests never leak the real process environment.
func env(values map[string]string) gitprovider.Loader {
	return gitprovider.Loader{Getenv: func(key string) string { return values[key] }}
}

// containsAll reports whether have contains every wanted key.
func containsAll(have []string, want ...string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			if h == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// TestConfigEmptyEnvironmentDisablesProvisioning: Token and WebhookURL are
// OPTIONAL — an empty environment loads with provisioning disabled and the
// missing keys listed. T0006 validates values, not absence: a machine
// without Gitea is a legal deployment, so the API must start without it
// (the base URL is the one value with a default: the dev stack).
func TestConfigEmptyEnvironmentDisablesProvisioning(t *testing.T) {
	cfg, err := env(map[string]string{}).Load()
	if err != nil {
		t.Fatalf("empty environment must load with provisioning disabled, got %v", err)
	}
	if cfg.ProvisioningEnabled() {
		t.Error("empty environment reported provisioning enabled")
	}
	if !containsAll(cfg.Missing, gitprovider.EnvToken, gitprovider.EnvWebhookURL) {
		t.Errorf("Missing = %v, want both %s and %s", cfg.Missing, gitprovider.EnvToken, gitprovider.EnvWebhookURL)
	}
	if cfg.BaseURL != gitprovider.DefaultBaseURL {
		t.Errorf("BaseURL = %q, want the dev default %q", cfg.BaseURL, gitprovider.DefaultBaseURL)
	}
}

// TestConfigValidLoad: the full set loads with provisioning enabled; base
// URL and webhook URL trim trailing slashes, the token lands in a Secret.
func TestConfigValidLoad(t *testing.T) {
	cfg, err := env(map[string]string{
		gitprovider.EnvToken:      "svc-token",
		gitprovider.EnvWebhookURL: "http://127.0.0.1:8080/api/v1/git/hooks/gitea/",
	}).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.ProvisioningEnabled() {
		t.Error("full configuration reported provisioning disabled")
	}
	if cfg.BaseURL != gitprovider.DefaultBaseURL {
		t.Errorf("BaseURL = %q, want the dev default %q", cfg.BaseURL, gitprovider.DefaultBaseURL)
	}
	if cfg.WebhookURL != "http://127.0.0.1:8080/api/v1/git/hooks/gitea" {
		t.Errorf("WebhookURL = %q (trailing slash must be trimmed)", cfg.WebhookURL)
	}
	if string(cfg.Token) != "svc-token" {
		t.Error("token not parsed")
	}
}

func TestConfigBaseURLOverride(t *testing.T) {
	cfg, err := env(map[string]string{
		gitprovider.EnvBaseURL:    "http://gitea.internal:3000/",
		gitprovider.EnvToken:      "t",
		gitprovider.EnvWebhookURL: "http://host/x",
	}).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BaseURL != "http://gitea.internal:3000" {
		t.Errorf("BaseURL = %q (trailing slash must be trimmed)", cfg.BaseURL)
	}
}

// TestConfigMissingTokenDisables: an unset token disables provisioning and
// lands on Missing — it is NOT an error, and nothing else is disturbed.
func TestConfigMissingTokenDisables(t *testing.T) {
	cfg, err := env(map[string]string{
		gitprovider.EnvWebhookURL: "http://host/x",
	}).Load()
	if err != nil {
		t.Fatalf("missing token must disable, not fail: %v", err)
	}
	if cfg.ProvisioningEnabled() {
		t.Error("missing token reported provisioning enabled")
	}
	if !containsAll(cfg.Missing, gitprovider.EnvToken) {
		t.Errorf("Missing = %v, want %s", cfg.Missing, gitprovider.EnvToken)
	}
}

// TestConfigMissingWebhookURLDisables: the mirror image — an unset webhook
// URL disables provisioning and lands on Missing, not an error.
func TestConfigMissingWebhookURLDisables(t *testing.T) {
	cfg, err := env(map[string]string{
		gitprovider.EnvToken: "super-secret-token",
	}).Load()
	if err != nil {
		t.Fatalf("missing webhook URL must disable, not fail: %v", err)
	}
	if cfg.ProvisioningEnabled() {
		t.Error("missing webhook URL reported provisioning enabled")
	}
	if !containsAll(cfg.Missing, gitprovider.EnvWebhookURL) {
		t.Errorf("Missing = %v, want %s", cfg.Missing, gitprovider.EnvWebhookURL)
	}
}

// TestConfigRejectsBadURLs: a PRESENT value must be valid — a non-http(s)
// or hostless value for either URL refuses to load, naming the offending
// key (T0006 validates values, not absence), and a URL that embeds
// credentials is redacted in the message.
func TestConfigRejectsBadURLs(t *testing.T) {
	cases := map[string]string{
		gitprovider.EnvBaseURL:    "ftp://gitea.internal",
		gitprovider.EnvWebhookURL: "not-a-url",
	}
	for key, value := range cases {
		_, err := env(map[string]string{
			gitprovider.EnvToken:      "t",
			gitprovider.EnvWebhookURL: "http://host/x",
			key:                       value,
		}).Load()
		if err == nil {
			t.Errorf("%s=%q accepted", key, value)
			continue
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error %q does not name the offending key %s", err, key)
		}
	}

	// A malformed webhook URL fails closed even when the token is missing —
	// the absence is legal, the malformed value is not.
	_, err := env(map[string]string{
		gitprovider.EnvWebhookURL: "not-a-url",
	}).Load()
	if err == nil || !strings.Contains(err.Error(), gitprovider.EnvWebhookURL) {
		t.Errorf("malformed webhook URL without token = %v, want a problem naming %s",
			err, gitprovider.EnvWebhookURL)
	}

	// Credential-embedding URLs are redacted in the error (defense in
	// depth: the loader validates URLs, and URLs may embed credentials).
	_, err = env(map[string]string{
		gitprovider.EnvToken:      "t",
		gitprovider.EnvWebhookURL: "http://host/x",
		gitprovider.EnvBaseURL:    "ftp://alice:sup3rs3cret@gitea.internal",
	}).Load()
	if err == nil {
		t.Fatal("ftp base URL accepted")
	}
	if strings.Contains(err.Error(), "sup3rs3cret") {
		t.Error("config error leaks credentials embedded in the URL")
	}
}

// TestConfigMultiProblemNamesKeys: the aggregated error names EVERY
// offending key — the most common first-run case (two bad values) is
// exactly the one that needs them spelled out — and a configured token
// never leaks into the message.
func TestConfigMultiProblemNamesKeys(t *testing.T) {
	_, err := env(map[string]string{
		gitprovider.EnvToken:      "super-secret-token",
		gitprovider.EnvBaseURL:    "ftp://gitea.internal",
		gitprovider.EnvWebhookURL: "not-a-url",
	}).Load()
	if err == nil {
		t.Fatal("two invalid values accepted")
	}
	for _, key := range []string{gitprovider.EnvBaseURL, gitprovider.EnvWebhookURL} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("aggregated error %q does not name %s", err, key)
		}
	}
	if strings.Contains(err.Error(), "super-secret-token") {
		t.Error("config error leaks the token")
	}
}

// TestConfigBlankValuesAreMissing: whitespace-only values are missing, not
// present — they disable provisioning like an unset key, never an error.
func TestConfigBlankValuesAreMissing(t *testing.T) {
	cfg, err := env(map[string]string{
		gitprovider.EnvToken:      "   ",
		gitprovider.EnvWebhookURL: "http://host/x",
	}).Load()
	if err != nil {
		t.Fatalf("blank token must count as unset, got %v", err)
	}
	if !containsAll(cfg.Missing, gitprovider.EnvToken) {
		t.Errorf("Missing = %v, want %s (blank is missing, not present)", cfg.Missing, gitprovider.EnvToken)
	}
}

// TestConfigUserAccessDisabledWithoutAdminPassword: user access is its own
// feature gate — provisioning can be fully configured while the admin
// password is missing, and the user-access keys land on UserAccessMissing
// (not Missing, which gates provisioning).
func TestConfigUserAccessDisabledWithoutAdminPassword(t *testing.T) {
	cfg, err := env(map[string]string{
		gitprovider.EnvToken:      "svc-token",
		gitprovider.EnvWebhookURL: "http://host/x",
	}).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.ProvisioningEnabled() {
		t.Error("provisioning must stay enabled without the admin pair")
	}
	if cfg.UserAccessEnabled() {
		t.Error("missing admin password reported user access enabled")
	}
	if !containsAll(cfg.UserAccessMissing, gitprovider.EnvAdminPassword) {
		t.Errorf("UserAccessMissing = %v, want %s", cfg.UserAccessMissing, gitprovider.EnvAdminPassword)
	}
	if cfg.AdminUser != gitprovider.DefaultAdminUser {
		t.Errorf("AdminUser = %q, want the dev default %q", cfg.AdminUser, gitprovider.DefaultAdminUser)
	}
}

// TestConfigUserAccessEnabled: the admin pair enables user access, the
// username overrides, and the password lands in a Secret. Provisioning's
// gate is untouched.
func TestConfigUserAccessEnabled(t *testing.T) {
	cfg, err := env(map[string]string{
		gitprovider.EnvToken:         "svc-token",
		gitprovider.EnvWebhookURL:    "http://host/x",
		gitprovider.EnvAdminUser:     "siteadmin",
		gitprovider.EnvAdminPassword: "admin-pw",
	}).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.UserAccessEnabled() {
		t.Error("full configuration reported user access disabled")
	}
	if cfg.AdminUser != "siteadmin" {
		t.Errorf("AdminUser = %q, want the override", cfg.AdminUser)
	}
	if string(cfg.AdminPassword) != "admin-pw" {
		t.Error("admin password not parsed")
	}
	if !cfg.ProvisioningEnabled() {
		t.Error("provisioning must stay enabled")
	}
}

// TestConfigUserAccessNeedsServiceToken: user access shares the service
// account token with provisioning (collaborator grants are made as the
// repository owner) — a missing token disables BOTH gates.
func TestConfigUserAccessNeedsServiceToken(t *testing.T) {
	cfg, err := env(map[string]string{
		gitprovider.EnvAdminUser:     "siteadmin",
		gitprovider.EnvAdminPassword: "admin-pw",
	}).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UserAccessEnabled() {
		t.Error("missing service token reported user access enabled")
	}
	if !containsAll(cfg.UserAccessMissing, gitprovider.EnvToken) {
		t.Errorf("UserAccessMissing = %v, want %s", cfg.UserAccessMissing, gitprovider.EnvToken)
	}
}
