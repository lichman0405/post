package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// canary is a unique marker planted in every secret position. After any code
// path renders the config, the output is swept for it: one hit fails the test.
const canary = "canary-s3cret-7f3a9c2e-0004"

func TestRedactURLMirrorsSpeclib(t *testing.T) {
	cases := []struct{ in, want string }{
		// user:secret@host -> user:***@host (keeps the user, drops the secret)
		{"https://user:pw@example.com/x", "https://user:***@example.com/x"},
		{"postgres://app:p%40ss@db.internal:5432/post", "postgres://app:***@db.internal:5432/post"},
		// token-only userinfo -> ***@host
		{"https://x-access-token:ghp_xxx@github.com/o/r.git", "https://x-access-token:***@github.com/o/r.git"},
		{"https://token-only@host/path", "https://***@host/path"},
		// last '@' delimits userinfo (passwords may contain '@')
		{"https://u:p@ss@word@host/path", "https://u:***@host/path"},
		// scp-like forms keep their conventional user (no ://); the ssh://
		// form is rewritten exactly as speclib does
		{"git@github.com:owner/name.git", "git@github.com:owner/name.git"},
		{"ssh://git@github.com/owner/name.git", "ssh://***@github.com/owner/name.git"},
		// no userinfo, no scheme: unchanged
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"host:5432", "host:5432"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := RedactURL(tc.in); got != tc.want {
			t.Errorf("RedactURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRedactURLWithCanary(t *testing.T) {
	in := "https://u:" + canary + "@blob.example.com"
	got := RedactURL(in)
	if strings.Contains(got, canary) {
		t.Fatalf("canary leaked through RedactURL: %s", got)
	}
	if got != "https://u:***@blob.example.com" {
		t.Errorf("RedactURL = %q", got)
	}
}

// canaryConfig returns a fully populated config with the canary planted in
// every secret field and in credential-bearing URLs.
func canaryConfig() *Config {
	return &Config{
		Layer: LayerProd,
		Server: ServerConfig{
			Addr: ":8080",
		},
		Database: DatabaseConfig{
			Host: "127.0.0.1", Port: 5432, User: "postgres",
			Password: Secret(canary), Name: "post", SSLMode: "verify-full",
		},
		Redis: RedisConfig{Addr: "127.0.0.1:6379"},
		Blob: BlobConfig{
			Endpoint:  SecretURL("https://u:" + canary + "@blob.example.com"),
			AccessKey: Secret(canary),
			SecretKey: Secret(canary),
			Bucket:    "post",
			UseTLS:    true,
		},
		GitProvider: GitProviderConfig{
			BaseURL: SecretURL("https://u:" + canary + "@gitea.example.com"),
			Token:   Secret(canary),
		},
	}
}

// sweep asserts the canary appears nowhere in the rendered output and that
// the output carries a redaction marker. Full-config dumps additionally must
// keep non-secret values (so the redaction is not vacuous over-redaction).
func sweep(t *testing.T, path, output string) {
	t.Helper()
	if strings.Contains(output, canary) {
		t.Fatalf("SECRET LEAK: canary found on %s output:\n%s", path, output)
	}
	if !strings.Contains(output, "***") {
		t.Errorf("%s output has no redaction marker (vacuous redaction?):\n%s", path, output)
	}
}

func sweepFull(t *testing.T, path, output string) {
	t.Helper()
	sweep(t, path, output)
	if !strings.Contains(output, ":8080") && !strings.Contains(output, "127.0.0.1") {
		t.Errorf("%s output lost non-secret values (over-redaction?):\n%s", path, output)
	}
}

func TestCanarySweepAcrossAllOutputPaths(t *testing.T) {
	cfg := canaryConfig()

	// 1. fmt via Stringer: %v, %+v and %s all go through String().
	for _, format := range []string{"%v", "%+v", "%s"} {
		sweepFull(t, "fmt "+format, fmt.Sprintf(format, cfg))
	}

	// 2. slog text and JSON handlers (LogValue).
	var text bytes.Buffer
	slog.New(slog.NewTextHandler(&text, nil)).Info("boot", "config", cfg)
	sweepFull(t, "slog text", text.String())

	var js bytes.Buffer
	slog.New(slog.NewJSONHandler(&js, nil)).Info("boot", "config", cfg)
	sweepFull(t, "slog json", js.String())
	if !json.Valid(js.Bytes()) {
		t.Errorf("slog json output is not valid JSON: %s", js.String())
	}

	// 3. JSON marshalling of the config itself.
	out, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	sweepFull(t, "json.Marshal", string(out))

	// 4. Direct field printing — even a careless fmt of a secret field must
	// mask the value.
	sweep(t, "Secret.String", cfg.Database.Password.String())
	sweep(t, "SecretURL.String", cfg.Blob.Endpoint.String())
	sweep(t, "fmt %v secret field", fmt.Sprintf("%v", cfg.GitProvider.Token))

	// 5. The raw accessor is the one deliberate escape hatch — document that
	// it is raw on purpose (connection code only).
	if cfg.Blob.Endpoint.Raw() != "https://u:"+canary+"@blob.example.com" {
		t.Errorf("SecretURL.Raw should return the raw value for connection use")
	}
	if string(cfg.Database.Password) != canary {
		t.Errorf("underlying string of Secret should keep the raw value")
	}
}

func TestValidationErrorsNeverEchoSecrets(t *testing.T) {
	// A malformed credential-bearing URL must be reported in its redacted
	// form only.
	env := fullEnv(LayerProd)
	env["POST_BLOB_ENDPOINT"] = "https://u:" + canary + "@/no-host"
	_, err := loader(env).Load()
	if err == nil {
		t.Fatal("expected an error for the malformed endpoint")
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatalf("SECRET LEAK: canary in validation error:\n%s", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Errorf("error should show the redacted URL: %s", err)
	}
	wantProblem(t, err, "POST_BLOB_ENDPOINT", "invalid value")

	// Errors about missing secrets name the key, never a value.
	_, err = loader(testEnv{EnvLayer: LayerProd}).Load()
	if err == nil {
		t.Fatal("expected missing-secret error")
	}
	for _, secretValue := range []string{"postgres_dev_pw", "minio_dev_pw", "gitea_dev_pw", canary} {
		if strings.Contains(err.Error(), secretValue) {
			t.Fatalf("error must not contain %q: %s", secretValue, err)
		}
	}
}
