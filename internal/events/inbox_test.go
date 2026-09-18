package events

import (
	"errors"
	"testing"
	"time"
)

// Task T1003 unit suite for the inbox vocabulary. The aggregation itself
// is SQL over real rows (the integration suite drives it end to end); what
// this file pins is the part that is decided in Go and that a caller
// depends on: which views exist, how a page size is bounded, and what
// "read" means to a caller holding an entry.

func TestValidateInboxFilter(t *testing.T) {
	cases := []struct {
		name   string
		filter string
		ok     bool
	}{
		{"unread is a view", InboxFilterUnread, true},
		{"all is a view", InboxFilterAll, true},
		{"empty is not a view name (the service defaults it)", "", false},
		{"the value is exact, not lowercased", "UNREAD", false},
		{"a near miss is not a view", "unread_entries", false},
		{"an unrelated word is not a view", "everything", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateInboxFilter(tc.filter)
			if tc.ok {
				if err != nil {
					t.Fatalf("ValidateInboxFilter(%q) = %v, want nil", tc.filter, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateInboxFilter(%q) = nil, want an error", tc.filter)
			}
			if !errors.Is(err, ErrInboxFilter) {
				t.Errorf("error %v does not wrap ErrInboxFilter — callers test with errors.Is", err)
			}
		})
	}
}

// TestInboxLimitClampsBothEnds: the page size is the server's protection,
// so an over-large request is capped rather than refused, and a request
// that names no size gets a page rather than nothing.
func TestInboxLimitClampsBothEnds(t *testing.T) {
	cases := []struct {
		name      string
		requested int
		want      int
	}{
		{"no size named", 0, DefaultInboxLimit},
		{"a negative size is not a size", -5, DefaultInboxLimit},
		{"a small page is obeyed", 1, 1},
		{"a page under the maximum is obeyed", MaxInboxLimit - 1, MaxInboxLimit - 1},
		{"the maximum itself is obeyed", MaxInboxLimit, MaxInboxLimit},
		{"one above the maximum is capped, not refused", MaxInboxLimit + 1, MaxInboxLimit},
		{"an unbounded page is capped", 1 << 30, MaxInboxLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := InboxLimit(tc.requested); got != tc.want {
				t.Errorf("InboxLimit(%d) = %d, want %d", tc.requested, got, tc.want)
			}
		})
	}
	if DefaultInboxLimit > MaxInboxLimit {
		t.Errorf("the default page (%d) exceeds the maximum (%d) — a caller that names no size would "+
			"ask for a page the cap then cuts", DefaultInboxLimit, MaxInboxLimit)
	}
}

func TestInboxEntryReadAndTarget(t *testing.T) {
	window := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	read := InboxEntry{TargetType: TargetTypeProject, TargetID: "p-1", EventType: "state.committed",
		WindowStart: window, Count: 3, Unread: 0}
	if !read.Read() {
		t.Errorf("an entry with no unread delivery reports unread: %+v", read)
	}
	if got := read.Target(); got != (Target{Type: TargetTypeProject, ID: "p-1"}) {
		t.Errorf("Target() = %+v, want project/p-1", got)
	}

	partially := read
	partially.Count, partially.Unread = 3, 1
	if partially.Read() {
		t.Errorf("an entry with one unread delivery reports read: %+v", partially)
	}

	// The aggregation window is the entry's own identity component: two
	// entries of the same target and type differ by it, so it must be part
	// of what a caller sees rather than hidden.
	if read.WindowStart != window {
		t.Errorf("WindowStart = %s, want %s", read.WindowStart, window)
	}
}
