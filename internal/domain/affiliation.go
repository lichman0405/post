package domain

import (
	"fmt"
	"time"
)

// # The affiliation date convention
//
// One fact — "which organization was this person affiliated with on calendar
// date D" — is asked by three layers:
//
//   - the contribution ledger resolves "affiliation at time" when it projects
//     an event (docs/13 §1);
//   - the organization service stamps a day onto every membership it creates
//     and ends (docs/04 §6);
//   - the subscription store decides whether a person belongs to an
//     organization's audience, which is who a private event reaches.
//
// Until T0816 each site answered that question with its own idea of what a day
// is, and the answers drifted. The convention lives here, once, and every one
// of those sites calls these functions instead of repeating the rule:
//
//   - THE DAY IS THE UTC CALENDAR DAY of the instant (AffiliationDay). A
//     calendar date is not a property of an instant until a zone is named,
//     and the zone named here is UTC. Reading the process's LOCAL
//     year/month/day — which is what all three sites did — makes the answer
//     depend on the server's clock rather than on the instant: on a UTC+8 host
//     between 00:00 and 08:00 local the local date is the UTC date's
//     tomorrow, so a membership created "today" is dated a day that has not
//     happened yet in UTC, and the ledger resolves the actor's contribution to
//     no organization at all (or to their previous one).
//   - THE WINDOW COVERS BOTH OF ITS ENDS (AffiliationWindowSQL):
//     start <= D AND (end IS NULL OR end >= D). A NULL start means "since
//     always", a NULL end "still". The end date is therefore the LAST day of
//     the affiliation, not the first day without one — the day a membership
//     ends is still a day of membership.
//
// The canonical columns are `date`, never `timestamptz`
// (infra/migrations/00002_identity.sql): the granularity is a whole day, and
// no clock inside that day decides anything. Comparing a date to an instant —
// `affiliation_end::timestamptz > now()` — hands the decision to the database
// session's timezone, which is how the same row read "member" from a UTC-12
// session and "not a member" from a UTC one.
//
// This file renders the convention as SQL text for the two stores that
// resolve an affiliation in the database. It takes no database handle and
// imports no driver, which is why the rule can live beside the model it
// describes.

// affiliationDayLayout is the text form the date columns take.
const affiliationDayLayout = "2006-01-02"

// AffiliationDay returns the calendar date the instant t falls on in UTC.
func AffiliationDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// AffiliationDayText renders AffiliationDay as the text form the date columns
// take ("YYYY-MM-DD").
//
// Date parameters travel as text on purpose: a Go time.Time handed to a
// ::date cast is a timestamptz first, and that cast then reads it in the
// session's timezone — the same bug in a different disguise.
func AffiliationDayText(t time.Time) string {
	return AffiliationDay(t).Format(affiliationDayLayout)
}

// AffiliationWindowSQL renders the convention's window predicate as SQL over
// one membership row. membership names the row (a table alias, or any SQL
// expression naming one) and day is the SQL expression evaluating to the
// calendar date being asked about — a text parameter cast, `$2::date`, for
// instance.
//
// It returns text rather than a statement, and runs nothing: the ledger
// projection (internal/contribution) and the organization audience
// (internal/events) interpolate it into their own queries so that the
// boundary they compare against cannot drift from this file.
func AffiliationWindowSQL(membership, day string) string {
	return fmt.Sprintf(
		"((%[1]s.affiliation_start IS NULL OR %[1]s.affiliation_start <= %[2]s)"+
			" AND (%[1]s.affiliation_end IS NULL OR %[1]s.affiliation_end >= %[2]s))",
		membership, day)
}
