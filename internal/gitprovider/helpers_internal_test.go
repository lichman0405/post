package gitprovider

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestProviderMessageBounded: message extraction truncates on runes and
// flattens whitespace, so an error string can never balloon or inject
// newlines into structured logs.
func TestProviderMessageBounded(t *testing.T) {
	long := strings.Repeat("x", 500)
	msg := providerMessage([]byte(`{"message":"line one\n` + long + `"}`))
	if msg == "" {
		t.Fatal("providerMessage returned empty for a valid provider error")
	}
	// The truncation bound is RUNES: 200 + the "…" marker (bytes can exceed
	// it — 研 at 200 runes is 603 bytes).
	if got := utf8.RuneCountInString(msg); got > 201 {
		t.Errorf("message rune count = %d, want ≤ 201", got)
	}
	if strings.Contains(msg, "\n") {
		t.Errorf("message contains a newline: %q", msg)
	}
	if !strings.HasPrefix(msg, "line one ") {
		t.Errorf("message lost its prefix: %q", msg[:20])
	}
	if !strings.HasSuffix(msg, "…") {
		t.Errorf("truncated message must end with …, got %q", msg[len(msg)-1:])
	}
}

func TestProviderMessageEmpty(t *testing.T) {
	for _, raw := range []string{``, `{}`, `{"message":""}`, `not json`} {
		if got := providerMessage([]byte(raw)); got != "" {
			t.Errorf("providerMessage(%q) = %q, want empty", raw, got)
		}
	}
}

// TestProviderMessageMultibyte: truncation never splits a rune (the stored
// error must stay valid UTF-8).
func TestProviderMessageMultibyte(t *testing.T) {
	msg := providerMessage([]byte(`{"message":"` + strings.Repeat("研", 300) + `"}`))
	if !strings.HasSuffix(msg, "…") || strings.HasSuffix(msg, "研�") {
		t.Errorf("multibyte truncation broke a rune: %q", msg[len(msg)-4:])
	}
}

// TestURLSegmentEscapes: one path segment escapes to itself; a segment with
// separators escapes them, so a name can never inject path structure.
func TestURLSegmentEscapes(t *testing.T) {
	if got := urlSegment("plain-name"); got != "plain-name" {
		t.Errorf("urlSegment(plain-name) = %q", got)
	}
	if got := urlSegment("a/b"); got != "a%2Fb" {
		t.Errorf("urlSegment(a/b) = %q, want a%%2Fb", got)
	}
	if got := urlSegment("o w"); got != "o%20w" {
		t.Errorf("urlSegment(o w) = %q, want o%%20w", got)
	}
}
