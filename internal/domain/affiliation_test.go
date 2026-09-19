package domain

import (
	"strings"
	"testing"
	"time"
)

// TestAffiliationDayIsTheUTCDate pins the half of the convention that no
// database can be asked about: which calendar date an instant falls on. The
// examples are the ones that made the three call sites disagree (T0816) — a
// local morning east of UTC, whose date is the UTC date's tomorrow, and a
// local evening west of it, whose date is the UTC date's yesterday.
func TestAffiliationDayIsTheUTCDate(t *testing.T) {
	east := time.FixedZone("UTC+8", 8*60*60)
	west := time.FixedZone("UTC-8", -8*60*60)
	for _, tc := range []struct {
		name string
		at   time.Time
		day  string
	}{
		// 07:30 on the 19th in Shanghai is 23:30 on the 18th in UTC.
		{"local morning east of UTC", time.Date(2026, 9, 19, 7, 30, 0, 0, east), "2026-09-18"},
		// ... and the same instant is already the 19th in UTC, which the
		// rule must read from the instant, not from the clock it is asked in.
		{"midday east of UTC", time.Date(2026, 9, 19, 12, 0, 0, 0, east), "2026-09-19"},
		// 18:00 on the 18th in California is 02:00 on the 19th in UTC.
		{"local evening west of UTC", time.Date(2026, 9, 18, 18, 0, 0, 0, west), "2026-09-19"},
		{"UTC midnight", time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC), "2026-09-19"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AffiliationDayText(tc.at); got != tc.day {
				t.Errorf("AffiliationDayText(%s) = %q, want %q", tc.at, got, tc.day)
			}
			day := AffiliationDay(tc.at)
			if got := day.Format(affiliationDayLayout); got != tc.day {
				t.Errorf("AffiliationDay(%s) names %q, want %q", tc.at, got, tc.day)
			}
			// The day is a whole day: truncating it again changes nothing,
			// and it is expressed in UTC whatever zone it was asked in.
			if again := AffiliationDay(day); !again.Equal(day) {
				t.Errorf("AffiliationDay is not a fixed point: %s -> %s", day, again)
			}
			if _, offset := day.Zone(); offset != 0 {
				t.Errorf("AffiliationDay(%s) is labelled %s (offset %d), want UTC", tc.at, day.Location(), offset)
			}
		})
	}
}

// TestAffiliationWindowSQLCoversBothEnds pins the deciding half of the
// convention at its single definition. The string is interpolated verbatim
// into two real queries (the ledger projection and the events audience), so
// its shape IS the rule: start <= day, and the end date covered (>=), not the
// first day after it (>). The integration suite proves what it means against
// PostgreSQL (tests/integration/affiliation_date_test.go); this is the cheap
// guard that a drive-by edit cannot flip the boundary.
func TestAffiliationWindowSQLCoversBothEnds(t *testing.T) {
	got := AffiliationWindowSQL("om", "$3::date")
	for _, want := range []string{
		"om.affiliation_start IS NULL OR om.affiliation_start <= $3::date",
		"om.affiliation_end IS NULL OR om.affiliation_end >= $3::date",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("AffiliationWindowSQL does not contain %q:\n%s", want, got)
		}
	}
	// Nothing may reach into another alias or parameter: the sites pass a
	// date and nothing else.
	if strings.Contains(got, "now()") || strings.Contains(got, "timestamptz") {
		t.Errorf("the window must be compared date-to-date, not against a clock:\n%s", got)
	}
}
