package audit_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/audit"
	"github.com/lichman0405/post/internal/application/orgs"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
)

// The Activity service unit suite: cursor encoding, read authorization
// through the owning surfaces' gates, page limits and error mapping. The
// pgx store and the HTTP surface are exercised in the integration suite.

// fakeStore captures the scoped list calls and returns canned rows.
type fakeStore struct {
	calls []fakeStoreCall
	rows  []domain.AuditRecord
	err   error
}

type fakeStoreCall struct {
	scope  string
	before *audit.Cursor
	limit  int
	// source is the registry filter the call carried ("" = both), so a
	// test can pin that the service passes the reader's filter through
	// unchanged instead of the store having to re-derive it.
	source domain.ActivitySource
	// reader is the user id the read was told to render for (ADR-024), so
	// a test can pin that the service hands the row set's audience down
	// rather than leaving the store to guess it.
	reader string
}

func (f *fakeStore) ListProjectActivity(_ context.Context, projectID, readerUserID string, before *audit.Cursor, limit int, source domain.ActivitySource) ([]domain.AuditRecord, error) {
	f.calls = append(f.calls, fakeStoreCall{projectID, before, limit, source, readerUserID})
	return f.rows, f.err
}

func (f *fakeStore) ListOrganizationActivity(_ context.Context, orgID string, before *audit.Cursor, limit int) ([]domain.AuditRecord, error) {
	f.calls = append(f.calls, fakeStoreCall{scope: orgID, before: before, limit: limit})
	return f.rows, f.err
}

// fakeProjectGate stubs the projects surface's Get (the read gate).
type fakeProjectGate struct{ err error }

func (g fakeProjectGate) Get(context.Context, domain.User, string) (domain.Project, error) {
	return domain.Project{}, g.err
}

// fakeOrgGate stubs the orgs surface's Get.
type fakeOrgGate struct{ err error }

func (g fakeOrgGate) Get(context.Context, domain.User, string) (domain.Organization, error) {
	return domain.Organization{}, g.err
}

func newTestService(store audit.Store, projectErr, orgErr error) *audit.Service {
	return audit.NewService(store, fakeProjectGate{err: projectErr}, fakeOrgGate{err: orgErr})
}

func TestCursorRoundTrip(t *testing.T) {
	in := audit.Cursor{OccurredAt: time.Date(2026, 9, 12, 10, 30, 45, 123456789, time.UTC), ID: "5f9c1d2e-0000-4000-8000-000000000001"}
	out, err := audit.DecodeCursor(audit.EncodeCursor(in))
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if !out.OccurredAt.Equal(in.OccurredAt) || out.ID != in.ID {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}
}

func TestCursorRejectsMalformed(t *testing.T) {
	for _, bad := range []string{
		"",                 // empty
		"!!!",              // bad base64
		"bm9zZXA=",         // decodes to "nosep" (no separator)
		"bm8taWQ=",         // decodes to "no-id" (wait — has no "|")
		"Z3J1Ynw=",         // "grub|" — empty id
		"bm90LWEtdGltZXx4", // "not-a-time|x"
		"fHx4",             // "||x" — two separators, unparseable time
	} {
		if _, err := audit.DecodeCursor(bad); !errors.Is(err, audit.ErrValidation) {
			t.Errorf("DecodeCursor(%q) = %v, want audit.ErrValidation", bad, err)
		}
	}
}

// TestCursorRejectsNonUUIDIDs: a parseable timestamp with a non-uuid id
// must never leave the service — the persistence layer's cursor-id parse
// must not see values it can panic on (review B1: cursor=<ts|zzz> once
// panicked the handler; M2: an id smuggling the separator is the same
// family).
func TestCursorRejectsNonUUIDIDs(t *testing.T) {
	ts := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	for _, id := range []string{
		"zzz",                                  // not a uuid at all
		"a|b",                                  // id smuggling the separator
		"5f9c1d2e-0000-4000-8000-00000000000",  // uuid-like, one hex short
		"5f9c1d2e-0000-4000-8000-00000000000z", // uuid shape, non-hex
	} {
		token := audit.EncodeCursor(audit.Cursor{OccurredAt: ts, ID: id})
		if _, err := audit.DecodeCursor(token); !errors.Is(err, audit.ErrValidation) {
			t.Errorf("DecodeCursor(id %q) = %v, want audit.ErrValidation", id, err)
		}
	}
}

func TestProjectActivityAuthorizesThroughGate(t *testing.T) {
	store := &fakeStore{}
	svc := newTestService(store, projects.ErrProjectNotFound, nil)

	_, _, err := svc.ProjectActivity(context.Background(), domain.User{}, "p1", "", "", 10)
	if !errors.Is(err, projects.ErrProjectNotFound) {
		t.Fatalf("error = %v, want projects.ErrProjectNotFound (non-members get the owning surface's sentinel)", err)
	}
	if len(store.calls) != 0 {
		t.Error("the store must not be reached for a denied read")
	}
}

func TestOrgActivityAuthorizesThroughGate(t *testing.T) {
	store := &fakeStore{}
	svc := newTestService(store, nil, orgs.ErrOrgNotFound)

	_, _, err := svc.OrgActivity(context.Background(), domain.User{}, "o1", "", "", 10)
	if !errors.Is(err, orgs.ErrOrgNotFound) {
		t.Fatalf("error = %v, want orgs.ErrOrgNotFound", err)
	}
	if len(store.calls) != 0 {
		t.Error("the store must not be reached for a denied read")
	}
}

func TestActivityPageDefaultsAndClampsLimit(t *testing.T) {
	store := &fakeStore{}
	svc := newTestService(store, nil, nil)

	// limit 0 -> DefaultLimit.
	if _, _, err := svc.ProjectActivity(context.Background(), domain.User{}, "p1", "", "", 0); err != nil {
		t.Fatalf("ProjectActivity: %v", err)
	}
	// limit 500 -> clamped to 200.
	if _, _, err := svc.ProjectActivity(context.Background(), domain.User{}, "p1", "", "", 500); err != nil {
		t.Fatalf("ProjectActivity: %v", err)
	}
	got := []int{store.calls[0].limit, store.calls[1].limit}
	if got[0] != audit.DefaultLimit || got[1] != 200 {
		t.Errorf("limits = %v, want [%d 200]", got, audit.DefaultLimit)
	}
	if store.calls[0].scope != "p1" {
		t.Errorf("scope = %q, want p1", store.calls[0].scope)
	}
	if store.calls[0].before != nil {
		t.Error("empty cursor must reach the store as nil before")
	}
}

func TestActivityPageCursorAndTermination(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	rows := func(n int) []domain.AuditRecord {
		out := make([]domain.AuditRecord, n)
		for i := range out {
			// Database rows carry canonical uuids; cursors are validated
			// against exactly that shape, so the fixture uses it too.
			out[i] = domain.AuditRecord{
				ID:         fmt.Sprintf("00000000-0000-4000-8000-%012x", i),
				OccurredAt: now.Add(-time.Duration(i) * time.Minute),
			}
		}
		return out
	}
	// A full page yields a cursor naming the last row.
	store := &fakeStore{rows: rows(audit.DefaultLimit)}
	svc := newTestService(store, nil, nil)
	records, next, err := svc.ProjectActivity(context.Background(), domain.User{}, "p1", "", "", audit.DefaultLimit)
	if err != nil || len(records) != audit.DefaultLimit {
		t.Fatalf("page = %d rows, %v", len(records), err)
	}
	c, err := audit.DecodeCursor(next)
	if err != nil {
		t.Fatalf("next cursor %q does not decode: %v", next, err)
	}
	last := records[len(records)-1]
	if !c.OccurredAt.Equal(last.OccurredAt) || c.ID != last.ID {
		t.Errorf("cursor = %+v, want last row %+v", c, last)
	}
	// Feeding the cursor back reaches the store as the before pair.
	if _, _, err := svc.ProjectActivity(context.Background(), domain.User{}, "p1", "", next, audit.DefaultLimit); err != nil {
		t.Fatalf("second page: %v", err)
	}
	call := store.calls[len(store.calls)-1]
	if call.before == nil || !call.before.OccurredAt.Equal(c.OccurredAt) || call.before.ID != c.ID {
		t.Errorf("before = %+v, want %+v", call.before, c)
	}

	// A short page terminates the log: next cursor is empty.
	store.rows = rows(3)
	if _, next, err := svc.ProjectActivity(context.Background(), domain.User{}, "p1", "", "", 10); err != nil || next != "" {
		t.Errorf("short page next = %q, %v; want empty", next, err)
	}
}

func TestActivityRejectsMalformedCursor(t *testing.T) {
	// Bad base64 and well-formed-but-invalid tokens (a parseable timestamp
	// with a non-uuid id, or an id smuggling the separator) are all
	// validation errors — and none may reach the store.
	now := time.Now().UTC()
	for _, bad := range []string{
		"!!!",
		audit.EncodeCursor(audit.Cursor{OccurredAt: now, ID: "zzz"}),
		audit.EncodeCursor(audit.Cursor{OccurredAt: now, ID: "a|b"}),
	} {
		store := &fakeStore{}
		svc := newTestService(store, nil, nil)
		if _, _, err := svc.ProjectActivity(context.Background(), domain.User{}, "p1", "", bad, 10); !errors.Is(err, audit.ErrValidation) {
			t.Errorf("cursor %q: error = %v, want audit.ErrValidation", bad, err)
		}
		if len(store.calls) != 0 {
			t.Error("a malformed cursor must not reach the store")
		}
	}
}

// TestProjectActivityPassesSourceFilter: the reader's source filter reaches
// the store verbatim, and the absence of one is the absence of a filter —
// not a third value the store would have to interpret. The three cases are
// the three a client can send; the store's own behaviour under each is the
// integration suite's (real SQL, real branches).
func TestProjectActivityPassesSourceFilter(t *testing.T) {
	for _, tc := range []struct {
		source domain.ActivitySource
	}{
		{""}, // both registries
		{domain.ActivitySourceGovernance},
		{domain.ActivitySourceResearch},
	} {
		store := &fakeStore{}
		svc := newTestService(store, nil, nil)
		if _, _, err := svc.ProjectActivity(context.Background(), domain.User{}, "p1", tc.source, "", 10); err != nil {
			t.Fatalf("ProjectActivity(source=%q): %v", tc.source, err)
		}
		if len(store.calls) != 1 {
			t.Fatalf("source %q: store calls = %d, want 1", tc.source, len(store.calls))
		}
		if got := store.calls[0].source; got != tc.source {
			t.Errorf("store source = %q, want %q (the filter is passed through, not re-derived)", got, tc.source)
		}
	}
}

// TestProjectActivityCarriesTheReader: ADR-024 makes the reader an INPUT of
// the read, and the Activity feed's research rows are the ones that need it
// — a public project's gate admits signed-in non-members, so the row set
// has to be the reader's. The service must hand the actor's id down with
// every page, cursor included: the second page of a feed is the same
// audience as the first, or a reader could page past the boundary.
//
// The zero actor (an unresolvable reader) is passed as the EMPTY id, which
// is the store's anonymous-audience branch — never a wider set, and never
// an error the surface would have to turn into an outage.
func TestProjectActivityCarriesTheReader(t *testing.T) {
	store := &fakeStore{}
	svc := newTestService(store, nil, nil)
	actor := domain.User{ID: "11111111-2222-4333-8444-555555555555"}

	if _, _, err := svc.ProjectActivity(context.Background(), actor, "p1", "", "", 10); err != nil {
		t.Fatalf("ProjectActivity: %v", err)
	}
	if len(store.calls) != 1 {
		t.Fatalf("store calls = %d, want 1", len(store.calls))
	}
	if got := store.calls[0].reader; got != actor.ID {
		t.Errorf("the read was given reader %q, want the actor id %q (the row set is the reader's, ADR-024)", got, actor.ID)
	}

	// The org feed has no per-row visibility to render (audit_log carries
	// none), so it carries no reader: the governance rows are exactly as
	// visible as the organization the gate already admitted the caller to.
	store.calls = nil
	if _, _, err := svc.OrgActivity(context.Background(), actor, "o1", "", "", 10); err != nil {
		t.Fatalf("OrgActivity: %v", err)
	}
	if len(store.calls) != 1 {
		t.Fatalf("store calls = %d, want 1", len(store.calls))
	}

	// An unresolvable actor travels as the empty id, not as a guess.
	store.calls = nil
	if _, _, err := svc.ProjectActivity(context.Background(), domain.User{}, "p1", "", "", 10); err != nil {
		t.Fatalf("ProjectActivity (no actor): %v", err)
	}
	if got := store.calls[0].reader; got != "" {
		t.Errorf("an unresolvable actor was given to the read as reader %q, want the empty id (fail closed)", got)
	}
}

// TestOrgActivityRefusesResearchSource: research events are project-scoped,
// so "the organization's research events" is a query this database cannot
// answer. It is refused as a validation error — and refused BEFORE the
// read gate and before the store, because the answer does not depend on
// who is asking: a member must not get an empty page that reads as "this
// organization has no research events", and a non-member must not get the
// not-found that would confirm the organization exists.
func TestOrgActivityRefusesResearchSource(t *testing.T) {
	store := &fakeStore{}
	svc := newTestService(store, nil, orgs.ErrOrgNotFound)
	if _, _, err := svc.OrgActivity(context.Background(), domain.User{}, "o1", domain.ActivitySourceResearch, "", 10); !errors.Is(err, audit.ErrValidation) {
		t.Errorf("error = %v, want audit.ErrValidation", err)
	}
	if len(store.calls) != 0 {
		t.Error("a refused source must not reach the store")
	}
	// The governance source (and the absence of one) is the feed itself.
	if _, _, err := svc.OrgActivity(context.Background(), domain.User{}, "o1", domain.ActivitySourceGovernance, "", 10); !errors.Is(err, orgs.ErrOrgNotFound) {
		t.Errorf("governance source: error = %v, want the org gate's sentinel", err)
	}
}

func TestActivityStoreFailureWrapsSentinel(t *testing.T) {
	store := &fakeStore{err: errors.New("connection refused")}
	svc := newTestService(store, nil, nil)
	if _, _, err := svc.OrgActivity(context.Background(), domain.User{}, "o1", "", "", 10); !errors.Is(err, audit.ErrStore) {
		t.Errorf("error = %v, want audit.ErrStore", err)
	}
}
