// Task T0613 required test "activity feed audience e2e" — the store-level
// half of the same rule (docs/adr/ADR-024's failure clause).
//
// The Activity feed's audience is decided by the read itself, and the read
// has to answer something when the reader does not resolve: the store's
// port takes the reader's user id, and a reader that names no user is
// passed to SQL as NULL. The membership clause cannot be true for a reader
// that names no user, so the answer is exactly the public rows.
//
// That is the fail-closed direction the ADR states in words: "读者解析不出来、
// 或成员关系查不到时，只给 public 行" — and it is deliberately the same
// answer for all three ways a reader can fail to resolve, so a caller
// cannot get a WIDER row set by failing:
//
//   - no reader at all (an unresolved actor), which is what the store gets
//     when the caller has no identity to hand down;
//   - an id that cannot be a uuid, which names no user by construction;
//   - a well-formed id with no membership row in this project — "the lookup
//     found nothing" must never read as "is a member".
//
// The same call, one reader over, is the member's: her membership row makes
// the private rows render, so the three cases above measure membership
// rather than "this read only ever returns public rows".
//
// It is a separate file from activity_audience_test.go because it is the one
// case that reaches the store's port directly: the HTTP surface cannot
// produce an unresolvable reader (the route requires a session), so this
// half is pinned where it lives — the production adapter, real PostgreSQL,
// the same fixture.

package integration

import (
	"testing"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence"
)

func TestActivityFeedAudienceFailsClosedWithoutAReader(t *testing.T) {
	ctx := testCtx(t)
	f := newActivityAudienceFixture(t, ctx)
	store := persistence.NewAuditStore(f.pool)

	rendered := func(readerID string) map[string]bool {
		t.Helper()
		rows, err := store.ListProjectActivity(ctx, f.projectID, readerID, nil, 50, domain.ActivitySourceResearch)
		if err != nil {
			t.Fatalf("ListProjectActivity(reader %q): %v", readerID, err)
		}
		out := map[string]bool{}
		for _, r := range rows {
			out[r.ID] = true
		}
		return out
	}

	for _, c := range []struct {
		name     string
		readerID string
	}{
		{name: "no reader at all", readerID: ""},
		{name: "a reader id that names no user", readerID: "not-a-uuid"},
		{name: "a well-formed id with no membership row", readerID: "00000000-0000-4000-8000-0000000000ff"},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			seen := rendered(c.readerID)
			for _, id := range f.publicEventIDs {
				if !seen[id] {
					t.Errorf("the public event %s is missing for reader %q: the fail-closed branch still renders the public rows",
						id, c.readerID)
				}
			}
			for _, id := range f.privateEventIDs {
				if seen[id] {
					t.Errorf("reader %q was rendered the private event %s: an unresolved reader is not a member",
						c.readerID, id)
				}
			}
		})
	}

	// The member's row makes the private rows render — the control that
	// keeps the three cases above from passing on a read that renders
	// public rows to everyone.
	seen := rendered(f.danaID)
	for _, id := range f.privateEventIDs {
		if !seen[id] {
			t.Errorf("the project member (id %s) was not rendered the private event %s: the fail-closed cases would be vacuous",
				f.danaID, id)
		}
	}
}
