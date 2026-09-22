package events

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
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

// The retry ladder, exactly. T1001 review item 2 was that a PostgreSQL
// outage produced one ERROR line every second for as long as it lasted; the
// fix is only real if the pause between attempts doubles and then stops
// doubling. Asserting the sequence here is what keeps DefaultBackoffMax from
// silently becoming decorative.
func TestNextBackoffDoublesThenCaps(t *testing.T) {
	// The shipped ladder: 1s, 2s, 4s, 8s, 16s, 30s, 30s...
	got := []time.Duration{}
	cur := DefaultBackoffMin
	for range 7 {
		got = append(got, cur)
		cur = nextBackoff(cur, DefaultBackoffMax)
	}
	want := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second,
		8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ladder step %d = %v, want %v (full ladder: %v)", i, got[i], want[i], got)
		}
	}

	// The property the debt was about: the number of ERROR lines an outage
	// produces is bounded by its duration and the cap, not by the poll
	// interval. An hour down is 3600 lines at 1s and ~125 at the 30s cap.
	const hour = time.Hour
	lines, t2 := 0, time.Duration(0)
	cur = DefaultBackoffMin
	for t2 < hour {
		lines++
		t2 += cur
		cur = nextBackoff(cur, DefaultBackoffMax)
	}
	if lines > 200 {
		t.Errorf("an hour-long outage would log %d ERROR lines; the cap is not doing its job", lines)
	}
	if lines < 100 {
		t.Errorf("only %d lines in an hour: the ladder is growing faster than DefaultBackoffMax allows", lines)
	}

	// The cap is a ceiling, not a step: a step that would overshoot lands
	// exactly on it. (nextBackoff takes max >= current as a precondition and
	// does not re-check it — nextBackoff(1s, 1ms) would return 1ms. That
	// precondition is established one level up: WithBackoff resolves min and
	// max together and raises a cap that would sit under the first step, so
	// the pair it installs always satisfies max >= min, and Run only ever
	// hands nextBackoff a current step it climbed to from there.
	// TestWithBackoffNeverInstallsADescendingLadder walks that ladder and
	// asserts it never shortens.)
	if got := nextBackoff(16*time.Second, DefaultBackoffMax); got != DefaultBackoffMax {
		t.Errorf("nextBackoff(16s, 30s) = %v, want the cap 30s", got)
	}
}

// WithBackoff resolves the first step and the cap against each other. The bug
// this test exists for: the option compared `max` against the min it had JUST
// assigned, so WithBackoff(60s, 45s) set min=60s, then rejected 45s as "below
// the min" and kept the default cap of 30s — installing a ladder that DESCENDS
// from its own first step (60s, 30s, 30s…). Run waits on the current step and
// only then climbs, so the pause collapsed from 60s to 30s the moment the
// outage kept going: the longer the failure lasted, the more often it was
// logged, which is precisely what a backoff is for.
//
// Two claims, because the table alone would not catch a future variant that
// installs a sane-looking pair and then walks it downward in Run: the installed
// pair is asserted, and the ladder is walked the way Run walks it.
func TestWithBackoffNeverInstallsADescendingLadder(t *testing.T) {
	cases := []struct {
		name             string
		min, max         time.Duration
		wantMin, wantMax time.Duration
	}{
		{"the defaults, restated", DefaultBackoffMin, DefaultBackoffMax, DefaultBackoffMin, DefaultBackoffMax},
		{"a cap below the first step is not installed", time.Second, time.Millisecond, time.Second, DefaultBackoffMax},
		{"a first step above the cap raises the cap", 60 * time.Second, 45 * time.Second, 60 * time.Second, 60 * time.Second},
		{"a first step above the DEFAULT cap and no cap given", 90 * time.Second, 0, 90 * time.Second, 90 * time.Second},
		{"a non-positive first step keeps the default one", 0, time.Minute, DefaultBackoffMin, time.Minute},
		{"a cap far above", time.Second, time.Hour, time.Second, time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewDispatcher(nil, WithBackoff(tc.min, tc.max))
			if d.backoffMin != tc.wantMin || d.backoffMax != tc.wantMax {
				t.Fatalf("WithBackoff(%v, %v) installed min=%v max=%v, want min=%v max=%v",
					tc.min, tc.max, d.backoffMin, d.backoffMax, tc.wantMin, tc.wantMax)
			}
			if d.backoffMax < d.backoffMin {
				t.Fatalf("the installed cap %v is below the first step %v", d.backoffMax, d.backoffMin)
			}

			// Run's own walk: wait on the current step, then climb. Bounded by
			// a step count rather than by reaching the cap, so a ladder that
			// cannot reach it fails on the assertion below instead of hanging.
			step := d.backoffMin
			for i := range 40 {
				next := nextBackoff(step, d.backoffMax)
				if next < step {
					t.Fatalf("ladder step %d descends: %v -> %v (min=%v max=%v) — the pause shortens the longer the outage lasts",
						i+1, step, next, d.backoffMin, d.backoffMax)
				}
				step = next
			}
			if step != d.backoffMax {
				t.Errorf("after 40 steps the ladder sits at %v, not the cap %v", step, d.backoffMax)
			}
		})
	}
}

// WithPollInterval is the T1001 review item 5 debt: batch size had a tuning
// option and the interval did not. The assertion is on observable behaviour
// of the loop, not on a struct field, so it fails if Run stops reading it.
func TestDispatcherPollIntervalIsIndependentOfTheFailureLadder(t *testing.T) {
	d := NewDispatcher(nil, WithPollInterval(7*time.Millisecond))
	if d.poll != 7*time.Millisecond {
		t.Fatalf("poll = %v, want 7ms", d.poll)
	}
	if d.backoffMin != DefaultBackoffMin || d.backoffMax != DefaultBackoffMax {
		t.Fatalf("setting the success-path interval changed the failure ladder: min=%v max=%v",
			d.backoffMin, d.backoffMax)
	}

	// Non-positive values are ignored rather than zeroing the pause: a zero
	// poll interval is a hot loop that would hammer PostgreSQL, which is the
	// failure mode this option exists to make tunable, not to introduce.
	// Same contract as worker.RedisQueue.WithPollTimeout.
	for _, bad := range []time.Duration{0, -time.Second} {
		if got := NewDispatcher(nil, WithPollInterval(bad)).poll; got != DefaultPollInterval {
			t.Errorf("WithPollInterval(%v) = %v, want the default %v", bad, got, DefaultPollInterval)
		}
	}

	// A cap below the first step does not survive either — here it is dropped
	// and the default cap stands, because the default cap is above the first
	// step. TestWithBackoffNeverInstallsADescendingLadder is where that rule
	// is asserted in full.
	if got := NewDispatcher(nil, WithBackoff(time.Second, time.Millisecond)); got.backoffMax != DefaultBackoffMax {
		t.Errorf("WithBackoff(min > max) installed max=%v, want the default %v", got.backoffMax, DefaultBackoffMax)
	}
	if got := NewDispatcher(nil, WithBackoff(0, time.Minute)); got.backoffMin != DefaultBackoffMin {
		t.Errorf("WithBackoff(0, ...) installed min=%v, want the default %v", got.backoffMin, DefaultBackoffMin)
	}
}

// The behavioural half of the same debt, with no database involved: point
// the dispatcher at a port nothing listens on (connection refused is
// immediate, so the failure path is exercised at its real speed rather than
// waiting on a connect timeout) and count what the loop LOGS.
//
// Before the ladder, Run retried every DefaultPollInterval forever, so a
// fixed window produced one line per interval. With the ladder, the same
// window produces a handful: the lines thin out. That is the observable
// difference, and it is asserted as a band rather than a count because the
// only thing that can move it is how fast this machine refuses a connection.
func TestDispatcherRunThinsOutItsErrorsWhilePostgresIsUnreachable(t *testing.T) {
	pool, err := pgxpool.New(t.Context(), "postgres://postgres:pw@127.0.0.1:1/post?sslmode=disable")
	if err != nil {
		t.Fatalf("building a pool for an unreachable address: %v", err)
	}
	defer pool.Close()

	var mu sync.Mutex
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&lockedWriter{mu: &mu, w: &buf}, nil))

	d := NewDispatcher(pool,
		WithLogger(log),
		WithBackoff(20*time.Millisecond, 80*time.Millisecond),
	)

	const window = 600 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), window)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run returned %v; it must only stop on context cancellation", err)
	}

	mu.Lock()
	body := buf.String()
	mu.Unlock()
	lines := strings.Count(body, "outbox publish pass failed")
	// Printed under -v because this number, next to the poll interval, is the
	// T1001 review item 2 evidence: the same window at the same settings with
	// the ladder removed logs one line (the poll interval), not this many.
	t.Logf("ladder %v..%v over %v produced %d failure lines (poll interval %v)",
		d.backoffMin, d.backoffMax, window, lines, d.poll)

	// 600ms on the 20ms ladder is roughly six attempts (t=0,20,60,140,220,
	// 300,380,460,540 -> eight). The same window with no backoff is thirty.
	if lines < 2 {
		t.Fatalf("only %d failure lines in %v: the loop is not retrying (or stopped on an error)\nlogs:\n%s",
			lines, window, body)
	}
	if lines > 15 {
		t.Errorf("%d failure lines in %v: the loop is not backing off (an unbounded retry at the poll interval would log ~30)\nlogs:\n%s",
			lines, window, body)
	}

	// The first failure still has to be reported immediately — backing off
	// must not mean going quiet at the start of an outage.
	if !strings.HasPrefix(strings.TrimSpace(body), "{") {
		t.Errorf("no line was logged at all:\n%s", body)
	}
}

// lockedWriter serialises slog's writes against the test's read of the same
// buffer; pgxpool's background goroutines are not involved, but the race
// detector does not know that.
type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
