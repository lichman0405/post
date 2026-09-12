package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setEnvForLoad installs a full dev environment into the process environment
// (LoadFromCwd reads the real environment) and returns a cleanup func.
func setEnvForLoad(t *testing.T, extra ...string) {
	t.Helper()
	t.Setenv(EnvLayer, LayerDev)
	t.Setenv("POST_DB_PASSWORD", "pw")
	t.Setenv("POST_DB_SSLMODE", "disable")
	t.Setenv("POST_BLOB_ACCESS_KEY", "ak")
	t.Setenv("POST_BLOB_SECRET_KEY", "sk")
	t.Setenv("POST_GITEA_TOKEN", "tok")
	for i := 0; i+1 < len(extra); i += 2 {
		t.Setenv(extra[i], extra[i+1])
	}
}

func TestLoadFromCwdReadsMatchingLayerFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".env.dev"),
		"POST_DB_HOST=layer-host\nPOST_DB_PORT=55432\n")
	setEnvForLoad(t)
	t.Chdir(dir)

	cfg, err := LoadFromCwd()
	if err != nil {
		t.Fatalf("LoadFromCwd failed: %v", err)
	}
	if cfg.Database.Host != "layer-host" || cfg.Database.Port != 55432 {
		t.Errorf("layer file not applied: host=%q port=%d", cfg.Database.Host, cfg.Database.Port)
	}
}

func TestLoadFromCwdProcessEnvOverridesLayerFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".env.dev"), "POST_DB_HOST=layer-host\n")
	setEnvForLoad(t, "POST_DB_HOST", "process-host")
	t.Chdir(dir)

	cfg, err := LoadFromCwd()
	if err != nil {
		t.Fatalf("LoadFromCwd failed: %v", err)
	}
	if cfg.Database.Host != "process-host" {
		t.Errorf("process environment must override the layer file, got %q", cfg.Database.Host)
	}
}

func TestLoadFromCwdIgnoresFileOfAnotherLayer(t *testing.T) {
	// Only .env.<current-layer> may be selected: a .env.prod in the cwd must
	// not leak values into a dev load (cross-layer fallback is forbidden).
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".env.prod"), "POST_DB_HOST=prod-host\n")
	setEnvForLoad(t)
	t.Chdir(dir)

	cfg, err := LoadFromCwd()
	if err != nil {
		t.Fatalf("LoadFromCwd failed: %v", err)
	}
	if cfg.Database.Host != "127.0.0.1" {
		t.Errorf("cross-layer value leaked: host=%q", cfg.Database.Host)
	}
}

func TestLoadFromCwdWithoutLayerFailsNamingPOSTEnv(t *testing.T) {
	// No POST_ENV: nothing must load, and the error must name the variable.
	dir := t.TempDir()
	t.Chdir(dir)
	// Scrub the layer variable; other vars are irrelevant without it.
	t.Setenv(EnvLayer, "")

	_, err := LoadFromCwd()
	if err == nil {
		t.Fatal("expected failure without POST_ENV")
	}
	if !strings.Contains(err.Error(), EnvLayer) {
		t.Errorf("error must name %s: %v", EnvLayer, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
