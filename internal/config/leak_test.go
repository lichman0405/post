package config

import (
	"strings"
	"testing"
)

// These tests encode the exact probes that exposed the T0004 security-review
// findings. The lesson they exist to prevent: a canary sweep only proves a
// gate catches the shapes its author thought of. Each case below is a shape
// the original implementation missed.

const (
	probeSecret = "SECRETPW123"
	probeToken  = "ghp_LONGREALSECRET0123456789abcdef"
)

// --- finding 1: error messages echoed credentials unredacted ----------------

func TestRedactForOutputCoversNonURLShapes(t *testing.T) {
	cases := []struct {
		name, in string
		mustHide string
	}{
		{"url with password", "postgres://u:" + probeSecret + "@h/db", probeSecret},
		{"url token only", "https://" + probeToken + "@github.com/o/r.git", probeToken},
		{"postgres keyword DSN", "host=h user=u password=" + probeSecret, probeSecret},
		{"pg password shorthand", "host=h pwd=" + probeSecret, probeSecret},
		{"bare github token", probeToken, probeToken},
	}
	for _, c := range cases {
		got := RedactForOutput(c.in)
		if strings.Contains(got, c.mustHide) {
			t.Errorf("%s: secret survived redaction: %q -> %q", c.name, c.in, got)
		}
	}
	// A harmless value must pass through so diagnostics stay useful.
	if got := RedactForOutput("8080"); got != "8080" {
		t.Errorf("plain value was mangled: %q", got)
	}
}

func TestBadValueNeverEchoesACredential(t *testing.T) {
	// A credential pasted into a field that is not itself secret-shaped used
	// to be echoed in full, because only the two URL fields called RedactURL.
	for _, in := range []string{
		"postgres://u:" + probeSecret + "@h/db",
		"host=h password=" + probeSecret,
		"\x00not-a-port",
	} {
		p := badValue(&fieldSpec{key: "POST_DB_PORT"}, in, "must be an integer between 1 and 65535")
		if strings.Contains(p.Msg, probeSecret) {
			t.Errorf("badValue echoed a credential: %s", p.Msg)
		}
		if !strings.Contains(p.Msg, "POST_DB_PORT") {
			t.Errorf("badValue lost the offending key: %s", p.Msg)
		}
	}
}

// --- finding 2: envfile errors echoed the whole line ------------------------

func TestEnvFileErrorNeverEchoesTheLine(t *testing.T) {
	// A credential-bearing URL pasted on its own line has no '=', which used
	// to fall into the "not a KEY=VALUE line" branch and echo the whole line.
	content := "POST_ENV=dev\npostgres://postgres:" + probeSecret + "@host/db\n"
	_, err := ParseEnvBytes(content, "layer.env")
	if err == nil {
		t.Fatal("expected a parse error")
	}
	if strings.Contains(err.Error(), probeSecret) {
		t.Fatalf("env-file error echoed the secret: %v", err)
	}
	if !strings.Contains(err.Error(), "layer.env:2") {
		t.Errorf("error lost its location: %v", err)
	}

	// Same for an invalid KEY, which is echoed too.
	_, err2 := ParseEnvBytes("bad-key="+probeSecret+"\n", "layer.env")
	if err2 == nil {
		t.Fatal("expected an invalid-KEY error")
	}
	if strings.Contains(err2.Error(), probeSecret) {
		t.Fatalf("invalid-KEY error echoed the secret: %v", err2)
	}
}

// --- finding 3: scanner detection gaps --------------------------------------

func TestScanFlagsShapesItPreviouslyMissed(t *testing.T) {
	cases := []struct{ name, line string }{
		// Token-only userinfo: RedactURL already treats this as
		// credential-bearing, so the scanner must agree.
		{"token-only url", "POST_BLOB_URL=https://" + probeToken + "@host/x\n"},
		// Key names outside the original regex.
		{"credentials key", "POST_CREDENTIALS=" + probeToken + "\n"},
		{"dsn key", "POST_DB_DSN=host=h password=" + probeSecret + "\n"},
		{"auth key", "POST_AUTH_BASIC=dXNlcjpwYXNz\n"},
		// Below the old 4-character floor.
		{"short secret", "POST_TOKEN=abc\n"},
	}
	for _, c := range cases {
		if f := ScanExampleContent(c.line, "x.env.example"); len(f) == 0 {
			t.Errorf("%s: scanner reported no finding (detection gap): %q", c.name, strings.TrimSpace(c.line))
		}
	}

	// Documented placeholders must stay allowed, or the gate becomes noise.
	for _, ok := range []string{
		"POST_DB_PASSWORD=change-me\n",
		"POST_GITEA_TOKEN=your-token\n",
		"POST_BLOB_SECRET_KEY=<secret>\n",
		"# POST_DB_PASSWORD=whatever\n",
		"POST_API_ADDR=:8080\n",
	} {
		if f := ScanExampleContent(ok, "x.env.example"); len(f) != 0 {
			t.Errorf("placeholder wrongly flagged: %q -> %v", ok, f)
		}
	}
}

func TestExampleFileNamesCovered(t *testing.T) {
	for _, name := range []string{
		".env.example", ".env.sample", ".env.template",
		"service.env.example", "POST.env.TEMPLATE",
	} {
		if !isExampleFileName(name) {
			t.Errorf("%s should be scanned", name)
		}
	}
	for _, name := range []string{".env", ".envrc", "env.example.txt", "readme.md"} {
		if isExampleFileName(name) {
			t.Errorf("%s should NOT be scanned", name)
		}
	}
}
