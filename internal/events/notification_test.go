package events

import (
	"testing"
	"time"
)

// T1005 unit suite for the notification model: the cadence vocabulary and
// the pure "is a digest due" rule the SQL claim implements a second time
// (parameterized by the windows this file defines). The suite is about the
// BOUNDARY: off-by-one at the interval edge is the difference between a
// daily digest and an hourly one.

func TestValidateCadence(t *testing.T) {
	cases := []struct {
		cadence string
		ok      bool
	}{
		{CadenceImmediate, true},
		{CadenceDaily, true},
		{CadenceWeekly, true},
		// The refusals matter more than the acceptances: an empty string is
		// the shape a missing field arrives as, and a store that accepted it
		// would put an unreadable cadence in a CHECK-constrained column.
		{"", false},
		{"DAILY", false},
		{"hourly", false},
		{"daily ", false},
		{"instant", false},
	}
	for _, tc := range cases {
		err := ValidateCadence(tc.cadence)
		if tc.ok && err != nil {
			t.Errorf("ValidateCadence(%q) = %v, want nil", tc.cadence, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("ValidateCadence(%q) = nil, want a refusal", tc.cadence)
		}
	}
}

func TestEffectiveCadence(t *testing.T) {
	// The stored value wins when there is one; the default is what an
	// account with no row gets, so "never opened the setting" and "explicitly
	// chose daily" are the same behaviour and stay distinguishable in the
	// data (Stored).
	if got := (NotificationPreferences{Cadence: CadenceWeekly, Stored: true}).EffectiveCadence(); got != CadenceWeekly {
		t.Errorf("stored weekly = %q, want %q", got, CadenceWeekly)
	}
	if got := (NotificationPreferences{}).EffectiveCadence(); got != DefaultCadence {
		t.Errorf("absent preference = %q, want the default %q", got, DefaultCadence)
	}
	if DefaultCadence != CadenceDaily {
		t.Errorf("DefaultCadence = %q, want daily: email is the channel that leaves the building and "+
			"one message per research event is what docs/18 §3 exists to prevent", DefaultCadence)
	}
	if ValidCadence(DefaultCadence) == false {
		t.Errorf("DefaultCadence %q is not a valid cadence", DefaultCadence)
	}
}

func TestDigestWindow(t *testing.T) {
	cases := map[string]time.Duration{
		CadenceImmediate: 0,
		CadenceDaily:     24 * time.Hour,
		CadenceWeekly:    7 * 24 * time.Hour,
		// An unknown cadence has no window: no window means always due, and
		// always due is the safe direction for a vocabulary the store
		// refuses to write anyway (a row that somehow holds one must not
		// become undeliverable).
		"nonsense": 0,
	}
	for cadence, want := range cases {
		if got := DigestWindow(cadence); got != want {
			t.Errorf("DigestWindow(%q) = %v, want %v", cadence, got, want)
		}
	}
}

func TestDigestDue(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	sent := func(d time.Duration) *time.Time {
		t := now.Add(-d)
		return &t
	}
	cases := []struct {
		name    string
		cadence string
		last    *time.Time
		want    bool
	}{
		// The FIRST digest is never made to wait: a new daily subscriber who
		// hears nothing for a day has been told the feature is broken.
		{"never sent, daily", CadenceDaily, nil, true},
		{"never sent, weekly", CadenceWeekly, nil, true},
		{"immediate always", CadenceImmediate, sent(time.Second), true},
		// Inside the window: not due. A daily subscriber who got a digest an
		// hour ago has nothing to be told again.
		{"daily, an hour ago", CadenceDaily, sent(time.Hour), false},
		{"weekly, six days ago", CadenceWeekly, sent(6 * 24 * time.Hour), false},
		// The boundary is inclusive: exactly one window later is due.
		{"daily, exactly 24h", CadenceDaily, sent(24 * time.Hour), true},
		{"daily, 24h+1s", CadenceDaily, sent(24*time.Hour + time.Second), true},
		{"weekly, exactly 7d", CadenceWeekly, sent(7 * 24 * time.Hour), true},
		// The anchor is the last SEND, not a midnight: a digest delayed by a
		// pass does not push the next one out by a whole interval.
		{"daily, 23h59m", CadenceDaily, sent(24*time.Hour - time.Minute), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DigestDue(tc.cadence, tc.last, now); got != tc.want {
				t.Errorf("DigestDue(%q, %v, %v) = %v, want %v", tc.cadence, tc.last, now, got, tc.want)
			}
		})
	}
}

// TestDigestWindowMatchesTheClaim is the drift guard between the rule above
// and the SQL that applies it (NotificationStore.claimEmailDeliveries). The
// claim is parameterized with DigestWindow's values and switches between
// them in a CASE, so the SQL can only pick the WRONG window — it cannot
// invent a duration. This pins the pairing: daily takes the daily window,
// and anything that is not immediate and not daily takes the weekly one.
func TestDigestWindowMatchesTheClaim(t *testing.T) {
	if DigestWindow(CadenceWeekly) <= DigestWindow(CadenceDaily) {
		t.Fatalf("the claim's CASE has two branches and no comparison: weekly (%v) must exceed daily (%v) "+
			"or the second branch is unreachable", DigestWindow(CadenceWeekly), DigestWindow(CadenceDaily))
	}
	if DigestWindow(CadenceImmediate) != 0 {
		t.Errorf("immediate's window = %v, want 0: the claim's first branch treats immediate as always due, "+
			"and a non-zero window here would contradict it", DigestWindow(CadenceImmediate))
	}
}
