package events

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateErrorKeepsRunesWhole(t *testing.T) {
	// "世" is 3 bytes: a byte-slice truncation landing inside it (max=4 or
	// max=7 below) leaves invalid UTF-8. last_error is read by humans and
	// tools, and invalid UTF-8 is neither (T1001 review).
	msg := strings.Repeat("世", 10) // 10 runes, 30 bytes
	for _, max := range []int{3, 4, 7, 10} {
		got := truncateError(msg, max)
		if !utf8.ValidString(got) {
			t.Errorf("max=%d: result is not valid UTF-8: %q", max, got)
		}
		if n := utf8.RuneCountInString(got); n != max {
			t.Errorf("max=%d: got %d runes, want %d (%q)", max, n, max, got)
		}
	}

	// Under the bound, the message passes through untouched; over the
	// bound (maxErrorLen), an ASCII error is cut to exactly the bound.
	if got := truncateError("short", 100); got != "short" {
		t.Errorf("under-bound message changed: %q", got)
	}
	if got := truncateError(strings.Repeat("a", maxErrorLen+500), maxErrorLen); utf8.RuneCountInString(got) != maxErrorLen {
		t.Errorf("over-bound ASCII error: got %d runes, want %d", utf8.RuneCountInString(got), maxErrorLen)
	}
}
