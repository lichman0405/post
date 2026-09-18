package notifications

import (
	"strings"
	"testing"
)

// T1005 unit suite for the notifications configuration. The rules it pins:
// email delivery is OFF unless a transport is named (never a silent
// half-configuration), and the web origin a digest's links are built on is
// an origin or nothing.

func loaderFrom(values map[string]string) Loader {
	return Loader{Getenv: func(key string) string { return values[key] }}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := loaderFrom(nil).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Enabled || cfg.SinkDir != "" {
		t.Errorf("with no POST_MAIL_SINK_DIR the sink is %q / enabled=%v, want off", cfg.SinkDir, cfg.Enabled)
	}
	if cfg.WebOrigin != DefaultWebOrigin {
		t.Errorf("WebOrigin = %q, want the default %q", cfg.WebOrigin, DefaultWebOrigin)
	}
}

func TestLoadSinkDirEnablesDelivery(t *testing.T) {
	cfg, err := loaderFrom(map[string]string{EnvMailSinkDir: "  /tmp/post-mail  "}).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Enabled {
		t.Error("Enabled = false although a sink directory is configured")
	}
	if cfg.SinkDir != "/tmp/post-mail" {
		t.Errorf("SinkDir = %q, want the trimmed value", cfg.SinkDir)
	}
	// A blank-but-set variable is the same as unset: "POST_MAIL_SINK_DIR="
	// in a layer file is how a deployment turns email off, and reading it as
	// "configured" would start a sender with no directory.
	cfg, err = loaderFrom(map[string]string{EnvMailSinkDir: "   "}).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Enabled {
		t.Error("a blank sink directory enabled email delivery")
	}
}

func TestLoadWebOrigin(t *testing.T) {
	cases := []struct {
		value string
		want  string
		bad   bool
	}{
		{"http://127.0.0.1:3000", "http://127.0.0.1:3000", false},
		{"https://post.example.com", "https://post.example.com", false},
		// A trailing slash is the same origin, and links are built by
		// concatenation — leaving it would produce http://host//projects/x.
		{"https://post.example.com/", "https://post.example.com", false},
		// A path is NOT silently kept or dropped: an app served under a
		// prefix would get links that 404, so this is refused.
		{"https://post.example.com/app", "", true},
		{"post.example.com", "", true},
		{"ftp://post.example.com", "", true},
		{"http://", "", true},
		{"not a url", "", true},
		{"https://post.example.com?x=1", "", true},
		{"https://post.example.com#frag", "", true},
	}
	for _, tc := range cases {
		cfg, err := loaderFrom(map[string]string{EnvWebOrigin: tc.value}).Load()
		if tc.bad {
			if err == nil {
				t.Errorf("%s=%q = nil error, want a refusal", EnvWebOrigin, tc.value)
				continue
			}
			if !strings.Contains(err.Error(), EnvWebOrigin) {
				t.Errorf("the error does not name the variable: %v", err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s=%q: %v", EnvWebOrigin, tc.value, err)
			continue
		}
		if cfg.WebOrigin != tc.want {
			t.Errorf("%s=%q gave WebOrigin %q, want %q", EnvWebOrigin, tc.value, cfg.WebOrigin, tc.want)
		}
	}
}

// TestEnvWebOriginIsTheAuthnVariable pins the one thing the two packages
// cannot share by import: the string. A digest link and the API's CORS
// origin must be the same deployment value, and a rename in one place would
// otherwise leave the digest pointing at a variable nothing sets.
func TestEnvWebOriginIsTheAuthnVariable(t *testing.T) {
	if EnvWebOrigin != "POST_WEB_ORIGIN" {
		t.Errorf("EnvWebOrigin = %q, want POST_WEB_ORIGIN (the variable internal/application/authn loads)", EnvWebOrigin)
	}
	if DefaultWebOrigin != "http://127.0.0.1:3000" {
		t.Errorf("DefaultWebOrigin = %q, want the dev web app origin", DefaultWebOrigin)
	}
}

func TestLoadFromTheProcessEnvironment(t *testing.T) {
	// The zero Loader reads the real environment (the internal/config and
	// authn.Loader shape): cmd/worker uses it, and a Loader whose zero value
	// silently returned defaults would make the worker ignore its
	// configuration.
	t.Setenv(EnvMailSinkDir, t.TempDir())
	t.Setenv(EnvWebOrigin, "https://post.example.com")
	cfg, err := Loader{}.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Enabled || cfg.WebOrigin != "https://post.example.com" {
		t.Errorf("Load() = %+v, want the process environment's values", cfg)
	}
}
