package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testEnv builds a Loader from a controlled environment map. Keys set to ""
// model "set but empty", which the loader must reject for required values.
type testEnv map[string]string

func (e testEnv) getenv(key string) string { return e[key] }

func loader(e testEnv) Loader { return Loader{Getenv: e.getenv} }

// fullEnv returns a valid minimal environment for one layer.
func fullEnv(layer string) testEnv {
	return testEnv{
		EnvLayer:               layer,
		"POST_DB_PASSWORD":     "pw-" + layer,
		"POST_DB_SSLMODE":      "disable",
		"POST_BLOB_ACCESS_KEY": "ak-" + layer,
		"POST_BLOB_SECRET_KEY": "sk-" + layer,
		"POST_GITEA_TOKEN":     "tok-" + layer,
	}
}

func mustLoad(t *testing.T, e testEnv, files ...string) *Config {
	t.Helper()
	cfg, err := loader(e).Load(files...)
	if err != nil {
		t.Fatalf("Load(%v) failed: %v", files, err)
	}
	return cfg
}

func wantProblem(t *testing.T, err error, key, sub string) {
	t.Helper()
	ce, ok := err.(*ConfigError)
	if !ok {
		t.Fatalf("want *ConfigError, got %T: %v", err, err)
	}
	for _, p := range ce.Problems {
		if p.Key == key && strings.Contains(p.Msg, sub) {
			return
		}
	}
	t.Fatalf("error %q has no problem for key %q containing %q", err, key, sub)
}

func TestLoadFailsFastWhenLayerIsMissing(t *testing.T) {
	_, err := loader(testEnv{}).Load()
	if err == nil {
		t.Fatal("expected error when POST_ENV is missing")
	}
	wantProblem(t, err, EnvLayer, "not set")
	msg := err.Error()
	if !strings.Contains(msg, "dev, test or prod") {
		t.Errorf("error should state the valid layers and remediation: %s", msg)
	}
}

func TestLoadFailsFastWhenLayerIsUnknown(t *testing.T) {
	_, err := loader(testEnv{EnvLayer: "staging"}).Load()
	if err == nil {
		t.Fatal("expected error for unknown layer")
	}
	wantProblem(t, err, EnvLayer, "unknown configuration layer")
}

func TestLoadAcceptsEachLayer(t *testing.T) {
	for _, layer := range []string{LayerDev, LayerTest, LayerProd} {
		cfg := mustLoad(t, fullEnv(layer))
		if cfg.Layer != layer {
			t.Errorf("layer = %q, want %q", cfg.Layer, layer)
		}
	}
}

func TestLoadAppliesNeutralDefaults(t *testing.T) {
	cfg := mustLoad(t, fullEnv(LayerDev))
	if cfg.Server.Addr != ":8080" {
		t.Errorf("Server.Addr = %q, want default :8080", cfg.Server.Addr)
	}
	if cfg.Database.Host != "127.0.0.1" || cfg.Database.Port != 5432 ||
		cfg.Database.User != "postgres" || cfg.Database.Name != "post" {
		t.Errorf("database defaults wrong: %+v", cfg.Database)
	}
	if cfg.Redis.Addr != "127.0.0.1:6379" {
		t.Errorf("Redis.Addr = %q", cfg.Redis.Addr)
	}
	if cfg.Blob.Bucket != "post" || cfg.Blob.UseTLS {
		t.Errorf("blob defaults wrong: bucket=%q use_tls=%v", cfg.Blob.Bucket, cfg.Blob.UseTLS)
	}
}

func TestMissingRequiredKeysAreNamed(t *testing.T) {
	_, err := loader(testEnv{EnvLayer: LayerProd}).Load()
	if err == nil {
		t.Fatal("expected error with no secrets set")
	}
	for _, key := range []string{
		"POST_DB_PASSWORD", "POST_DB_SSLMODE",
		"POST_BLOB_ACCESS_KEY", "POST_BLOB_SECRET_KEY",
		"POST_GITEA_TOKEN",
	} {
		wantProblem(t, err, key, "missing")
	}
	if !strings.Contains(err.Error(), ".env.example") {
		t.Errorf("error should point at .env.example: %s", err)
	}
}

func TestSecretNeverFallsBackToDefault(t *testing.T) {
	// Even in dev, a secret has no default: the well-known dev value must
	// come from an explicit .env.dev, never from the loader.
	env := fullEnv(LayerDev)
	delete(env, "POST_DB_PASSWORD")
	_, err := loader(env).Load()
	if err == nil {
		t.Fatal("expected error when POST_DB_PASSWORD is absent in dev")
	}
	wantProblem(t, err, "POST_DB_PASSWORD", "missing")
	// The error must not invent a value for the secret.
	if strings.Contains(err.Error(), "postgres_dev_pw") {
		t.Fatalf("loader must never suggest a dev credential: %s", err)
	}
}

func TestEmptySecretValueIsRejected(t *testing.T) {
	env := fullEnv(LayerTest)
	env["POST_GITEA_TOKEN"] = "" // set but empty
	_, err := loader(env).Load()
	if err == nil {
		t.Fatal("expected error for empty POST_GITEA_TOKEN")
	}
	wantProblem(t, err, "POST_GITEA_TOKEN", "missing")
}

func TestMalformedValuesAreNamed(t *testing.T) {
	cases := []struct {
		env  testEnv
		key  string
		want string
	}{
		{env: testEnv{EnvLayer: LayerDev, "POST_DB_PORT": "not-a-port"}, key: "POST_DB_PORT", want: "invalid value"},
		{env: testEnv{EnvLayer: LayerDev, "POST_DB_PORT": "70000"}, key: "POST_DB_PORT", want: "invalid value"},
		{env: testEnv{EnvLayer: LayerDev, "POST_DB_SSLMODE": "bogus"}, key: "POST_DB_SSLMODE", want: "invalid value"},
		{env: testEnv{EnvLayer: LayerDev, "POST_BLOB_USE_TLS": "maybe"}, key: "POST_BLOB_USE_TLS", want: "true or false"},
		{env: testEnv{EnvLayer: LayerDev, "POST_BLOB_ENDPOINT": "://not-a-url"}, key: "POST_BLOB_ENDPOINT", want: "invalid value"},
		{env: testEnv{EnvLayer: LayerDev, "POST_GITEA_BASE_URL": "ftp://127.0.0.1:3000"}, key: "POST_GITEA_BASE_URL", want: "http(s)"},
	}
	for _, tc := range cases {
		env := fullEnv(LayerDev)
		for k, v := range tc.env {
			env[k] = v
		}
		_, err := loader(env).Load()
		if err == nil {
			t.Fatalf("expected error for %s=%q", tc.key, tc.env[tc.key])
		}
		wantProblem(t, err, tc.key, tc.want)
	}
}

func TestLayerFileIsLoadedOnlyForMatchingLayer(t *testing.T) {
	dir := t.TempDir()
	devFile := filepath.Join(dir, ".env.dev")
	os.WriteFile(devFile, []byte("POST_DB_PASSWORD=from_dev_file\n"), 0o600)

	// dev layer: the file applies (env does not set the password).
	env := fullEnv(LayerDev)
	delete(env, "POST_DB_PASSWORD")
	cfg := mustLoad(t, env, devFile)
	if cfg.Database.Password != "from_dev_file" {
		t.Errorf("dev layer file not applied: %v", cfg.Database.Password)
	}

	// test layer: the same file is refused — no cross-layer fallback.
	_, err := loader(fullEnv(LayerTest)).Load(devFile)
	if err == nil {
		t.Fatal("expected refusal to load .env.dev under layer test")
	}
	wantProblem(t, err, EnvLayer, "cross-layer fallback is forbidden")
}

func TestMissingValueIsNotSatisfiedByAnotherLayersFile(t *testing.T) {
	// A .env.dev file may exist on disk, but a test-layer run never loads it
	// (layer files are explicit) — so the missing value is reported, and the
	// dev value is never silently picked up.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".env.dev"),
		[]byte("POST_DB_PASSWORD=only_in_dev\n"), 0o600)

	env := fullEnv(LayerTest)
	delete(env, "POST_DB_PASSWORD")
	_, err := loader(env).Load()
	if err == nil {
		t.Fatal("expected error")
	}
	wantProblem(t, err, "POST_DB_PASSWORD", "missing")
}

func TestProcessEnvOverridesLayerFile(t *testing.T) {
	dir := t.TempDir()
	devFile := filepath.Join(dir, ".env.dev")
	os.WriteFile(devFile, []byte(
		"POST_DB_PASSWORD=file_value\nPOST_DB_PORT=5555\n"), 0o600)

	env := fullEnv(LayerDev)
	delete(env, "POST_DB_PASSWORD")
	env["POST_DB_PORT"] = "6666"
	cfg := mustLoad(t, env, devFile)
	if cfg.Database.Password != "file_value" {
		t.Errorf("file value for password not applied: %v", cfg.Database.Password)
	}
	if cfg.Database.Port != 6666 {
		t.Errorf("process env should override the file: port=%d", cfg.Database.Port)
	}
}

func TestLayerFileParsesAndValidatesValues(t *testing.T) {
	dir := t.TempDir()
	devFile := filepath.Join(dir, ".env.dev")
	os.WriteFile(devFile, []byte(`
# dev-only values (git-ignored, never committed)
POST_DB_PASSWORD='quoted value'
POST_DB_PORT=6432
`), 0o600)
	env := fullEnv(LayerDev)
	delete(env, "POST_DB_PASSWORD")
	cfg := mustLoad(t, env, devFile)
	if cfg.Database.Password != "quoted value" {
		t.Errorf("quoted value not stripped: %v", cfg.Database.Password)
	}
	if cfg.Database.Port != 6432 {
		t.Errorf("port from file = %d", cfg.Database.Port)
	}
}

func TestUnknownVariablesAreIgnored(t *testing.T) {
	env := fullEnv(LayerDev)
	env["POST_DB_PASSWRD"] = "typo" // unknown: ignored
	env["COMPLETELY_UNRELATED"] = "x"
	cfg := mustLoad(t, env)
	if cfg.Database.Password != "pw-dev" {
		t.Errorf("typo should not overwrite the real value")
	}
}

func TestLoadReportsAllProblemsAtOnce(t *testing.T) {
	_, err := loader(testEnv{EnvLayer: LayerProd, "POST_DB_PORT": "abc"}).Load()
	ce, ok := err.(*ConfigError)
	if !ok {
		t.Fatalf("want *ConfigError, got %T", err)
	}
	if len(ce.Problems) < 2 {
		t.Fatalf("expected aggregate problems, got %d: %v", len(ce.Problems), err)
	}
}

func TestParseEnvBytes(t *testing.T) {
	values, err := ParseEnvBytes(`
# comment
A=1
B = 2
C="keep # hash"
D='single'
E=unterminated
CRLF=ok`+"\r"+`
F=A=B
BAD KEY=1
`, "fake.env")
	if err == nil {
		t.Fatal("expected an error for the invalid KEY line")
	}
	if !strings.Contains(err.Error(), "fake.env:10") {
		t.Errorf("error should name file and line: %v", err)
	}
	if len(values) != 0 {
		t.Errorf("parse should fail atomically, got %v", values)
	}
}
