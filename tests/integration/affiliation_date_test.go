// Task T0816: the affiliation date convention, pinned where it was broken.
//
// One fact — "was this person affiliated with this organization on calendar
// date D" — is answered in three layers: the contribution ledger resolves
// "affiliation at time" when it projects an event (docs/13 §1), the
// organization service stamps today's date onto the memberships it creates
// and ends, and the subscription store decides whether an organization's
// audience includes a person. Before this task the three sites each carried
// their own idea of which day "today" is, and two of them read the local
// year/month/day while labelling the result UTC:
//
//   - the write side (internal/application/orgs) dated a membership with the
//     LOCAL calendar date, so on a UTC+8 host between 00:00 and 08:00 local
//     a membership created "today" was dated tomorrow (UTC);
//   - the decide side (internal/events' organization audience) compared
//     affiliation_end::timestamptz > now(), which let the database session's
//     timezone decide the answer and read the end date as already expired;
//   - the ledger projection (internal/contribution, merged as T0807) read
//     the event instant's UTC date and covered both ends.
//
// The tests here are written against BEHAVIOUR, not against the helper: each
// expectation is computed from time.Now().UTC() in the test itself, so a
// shared helper that drifted would not take the assertion down with it.
package integration

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/orgs"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
)

// pinClockAheadOfUTC pins the process clock (time.Local, which time.Now()
// stamps onto the instant it returns) to a zone whose local calendar date is
// one day AHEAD of the current UTC date, and restores it when the test ends.
//
// That is the condition the write side got wrong and could not be caught on
// a machine whose clock happened to agree with UTC: local 07:30 on the 19th
// in a UTC+8 zone is 23:30 on the 18th in UTC, so a date read from the local
// year/month/day names a day that has not happened in UTC yet. The suite
// cannot wait for that hour, so the zone is built from the current instant
// instead: the local wall clock is put at 00:10 on the UTC date's tomorrow,
// which holds for the rest of the run whatever hour the suite is started at.
func pinClockAheadOfUTC(t *testing.T) {
	t.Helper()
	restore := time.Local
	now := time.Now().UTC()
	tod := time.Duration(now.Hour())*time.Hour + time.Duration(now.Minute())*time.Minute +
		time.Duration(now.Second())*time.Second + time.Duration(now.Nanosecond())
	offset := 24*time.Hour + 10*time.Minute - tod
	time.Local = time.FixedZone("T0816-ahead-of-utc", int(offset.Seconds()))
	t.Cleanup(func() { time.Local = restore })
}

// pinClockUTC pins the process clock to UTC: the boundary assertion below is
// about the END DATE rule, so the clock is taken out of the picture (with
// local == UTC the old and the new write side stamp the same day, and only
// the reading of the end date decides the answer).
func pinClockUTC(t *testing.T) {
	t.Helper()
	restore := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = restore })
}

// utcDateNow is the UTC calendar date of this instant, in the text form the
// date columns take. Deliberately NOT the shared helper: this is the test's
// own statement of what the answer must be.
func utcDateNow() string { return time.Now().UTC().Format("2006-01-02") }

// TestAffiliationDateConventionAcrossTheProjection creates a membership at an
// instant whose local calendar date is NOT the UTC date, then projects an
// event that happened at that same instant, and reads the answer back out of
// the ledger table.
//
// The failure it pins: the membership's start date is stamped one day late
// (tomorrow in UTC), so the projection — which resolves strictly at the
// event's UTC date — does not see the membership at all and attributes the
// event to the actor's PREVIOUS organization. A ledger that puts a
// contribution in the wrong organization is the exact failure docs/13 §1's
// "affiliation at time" exists to prevent.
func TestAffiliationDateConventionAcrossTheProjection(t *testing.T) {
	ctx := testCtx(t)
	f := newLedgerFixture(t, ctx)
	// The fixture is built first, on purpose: it dates its own windows in UTC
	// text, and the probe clock would move a window dated from a bare
	// time.Now().
	pinClockAheadOfUTC(t)

	// --- the write: an organization created at the probe instant ---------
	svc := orgs.NewService(persistence.NewOrgStore(f.pool))
	org, _, err := svc.Create(ctx, f.alice, "t0816-probe", "T0816 Probe", "created at the probe instant")
	if err != nil {
		t.Fatalf("create the probe organization: %v", err)
	}

	var stored time.Time
	if err := f.pool.QueryRow(ctx,
		`SELECT affiliation_start FROM organization_memberships
		  WHERE organization_id = $1 AND user_id = $2`, org.ID, f.alice.ID).Scan(&stored); err != nil {
		t.Fatalf("read the creator's affiliation back: %v", err)
	}
	// Read back from the TABLE: the write function's return value is not
	// evidence that the row says what the convention says.
	if got, want := stored.Format("2006-01-02"), utcDateNow(); got != want {
		t.Errorf("affiliation_start = %q, want the UTC calendar date %q (the local one is %q)",
			got, want, time.Now().Format("2006-01-02"))
	}

	// --- the projection: an event at the probe instant -------------------
	eventID := f.recordEvent(t, ctx, ledgerEvent{
		eventType: "contribution.accepted", actorID: f.alice.ID, projectID: f.project.ID,
		occurredAt: time.Now().UTC(),
	})
	batch, err := f.projector.RunOnce(ctx)
	if err != nil {
		t.Fatalf("projection pass: %v", err)
	}
	if batch.Projected != 1 {
		t.Fatalf("projected = %d, want 1 (the event recorded above)", batch.Projected)
	}
	row := f.mustRowFor(t, ctx, eventID)
	if row.OrgAtTime == nil || *row.OrgAtTime != org.ID {
		t.Errorf("organization at the probe instant = %s, want the organization created at that instant (%s); alice's earlier membership is org A %s",
			textOrNull(row.OrgAtTime), org.ID, f.orgA)
	}
}

// TestAffiliationEndDayIsStillAMembershipDay pins the other end of the same
// convention: the affiliation window covers both of its ends, so a membership
// whose affiliate_end is TODAY is a membership today — the end date is the
// last day the affiliation is in force, and taking the day back turns the
// last day of every affiliation into a day of no organization.
//
// The rule is asked through the real service (the membership is ended by
// RemoveMember, which stamps the end date itself) and answered by the
// subscription store, which is the audience gate the whole event pipeline
// shares: delivering an organization's events to its members.
func TestAffiliationEndDayIsStillAMembershipDay(t *testing.T) {
	ctx := testCtx(t)
	f := newLedgerFixture(t, ctx)
	pinClockUTC(t)

	svc := orgs.NewService(persistence.NewOrgStore(f.pool))
	org, _, err := svc.Create(ctx, f.alice, "t0816-end", "T0816 End", "ends today")
	if err != nil {
		t.Fatalf("create the organization: %v", err)
	}
	if _, err := svc.Invite(ctx, f.alice, org.ID, "ledger-bob", domain.OrgRoleContributor, nil, true); err != nil {
		t.Fatalf("invite bob: %v", err)
	}
	if err := svc.RemoveMember(ctx, f.bob, org.ID, f.bob.ID); err != nil {
		t.Fatalf("bob leaves: %v", err)
	}

	// The premise of the assertion below, read back from the table: the
	// departure is dated today. (If the service dated it otherwise, this test
	// would be asking a different question than the one it claims to ask.)
	var end time.Time
	if err := f.pool.QueryRow(ctx,
		`SELECT affiliation_end FROM organization_memberships
		  WHERE organization_id = $1 AND user_id = $2`, org.ID, f.bob.ID).Scan(&end); err != nil {
		t.Fatalf("read the departure date back: %v", err)
	}
	if got, want := end.Format("2006-01-02"), utcDateNow(); got != want {
		t.Fatalf("affiliation_end = %q, want today %q — the fixture is not the one under test", got, want)
	}

	store := events.NewSubscriptionStore(f.pool)
	target := events.Target{Type: events.TargetTypeOrganization, ID: org.ID}
	if level, err := store.TargetAudienceFor(ctx, target, f.bob.ID); err != nil {
		t.Fatalf("resolve the audience: %v", err)
	} else if level != events.AudienceMember {
		t.Errorf("audience of the member whose affiliation ends today = %q, want %q "+
			"(the end date is the LAST day of the affiliation, not the first day without one)",
			level, events.AudienceMember)
	}

	// The other direction still holds — the gate revokes: an affiliation that
	// ended YESTERDAY is not a membership. Without this half, "always a
	// member" would pass the assertion above.
	if _, err := f.pool.Exec(ctx,
		`UPDATE organization_memberships SET affiliation_end = $3::date - 1
		  WHERE organization_id = $1 AND user_id = $2`, org.ID, f.bob.ID, utcDateNow()); err != nil {
		t.Fatalf("move the departure date to yesterday: %v", err)
	}
	if level, err := store.TargetAudienceFor(ctx, target, f.bob.ID); err != nil {
		t.Fatalf("resolve the audience after the end date moved: %v", err)
	} else if level == events.AudienceMember {
		t.Errorf("audience of a member whose affiliation ended yesterday = %q, want anything but %q",
			level, events.AudienceMember)
	}
}

// TestOrganizationAudienceIgnoresTheSessionTimezone asks the same question
// through three database sessions whose timezones are a day apart. The
// affiliation columns are dates; a session's timezone must not be an input to
// what a date means, and the answer must be the same from all three.
func TestOrganizationAudienceIgnoresTheSessionTimezone(t *testing.T) {
	ctx := testCtx(t)
	f := newLedgerFixture(t, ctx)
	pinClockUTC(t)

	svc := orgs.NewService(persistence.NewOrgStore(f.pool))
	org, _, err := svc.Create(ctx, f.alice, "t0816-tz", "T0816 TZ", "one row, three sessions")
	if err != nil {
		t.Fatalf("create the organization: %v", err)
	}
	if _, err := svc.Invite(ctx, f.alice, org.ID, "ledger-bob", domain.OrgRoleContributor, nil, true); err != nil {
		t.Fatalf("invite bob: %v", err)
	}
	if err := svc.RemoveMember(ctx, f.bob, org.ID, f.bob.ID); err != nil {
		t.Fatalf("bob leaves: %v", err)
	}

	target := events.Target{Type: events.TargetTypeOrganization, ID: org.ID}
	// UTC+14 and UTC-12 are as far apart as real zones get: their midnight is
	// 26 hours apart, so a rule that reads "the end date's midnight in this
	// session's zone" answers differently in each.
	for _, zone := range []string{"Etc/UTC", "Etc/GMT-14", "Etc/GMT+12"} {
		pool := poolWithSessionTimezone(t, ctx, f.pool, zone)
		t.Cleanup(pool.Close)
		level, err := events.NewSubscriptionStore(pool).TargetAudienceFor(ctx, target, f.bob.ID)
		if err != nil {
			t.Fatalf("resolve the audience from a %s session: %v", zone, err)
		}
		if level != events.AudienceMember {
			t.Errorf("audience from a %s session = %q, want %q (the same answer the date columns give everywhere)",
				zone, level, events.AudienceMember)
		}
	}
}

// poolWithSessionTimezone opens a second pool on the same database whose
// sessions run in zone.
func poolWithSessionTimezone(t *testing.T, ctx context.Context, pool *pgxpool.Pool, zone string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(pool.Config().ConnString())
	if err != nil {
		t.Fatalf("parse the fixture's connection string: %v", err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["timezone"] = zone
	second, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open a %s session pool: %v", zone, err)
	}
	return second
}
