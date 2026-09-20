package audit

import (
	"context"
	"fmt"

	"github.com/lichman0405/post/internal/domain"
)

// Service serves the Activity page queries. It holds no policy of its own:
// read authorization goes through the owning surfaces' gates (a project's
// activity is visible exactly to the project's members, an organization's
// to the organization's members), and the log itself is append-only at the
// database.
type Service struct {
	store    Store
	projects ProjectReadGate
	orgs     OrgReadGate
}

// NewService wires the service on the read store and the two read gates.
// Pass the real *projects.Service and *orgs.Service in production — the
// same instances the HTTP surfaces use, so visibility rules can never
// drift between a resource and its activity feed.
func NewService(store Store, projects ProjectReadGate, orgs OrgReadGate) *Service {
	return &Service{store: store, projects: projects, orgs: orgs}
}

// DefaultLimit is the page size when the client sends none.
const DefaultLimit = 50

// maxLimit bounds a client-requested page (the rows are joined with user
// names; a hostile client must not be able to demand unbounded pages).
const maxLimit = 200

// ProjectActivity returns one page of the project's Activity, newest
// first, plus the cursor for the next page ("" when the page is the last).
// Non-members get the same not-found answer as for the project itself
// (existence hiding, via the projects surface's gate).
//
// source selects which registry the page reads: "" reads both — the
// governance rows (audit_log, T0110) and the research events
// (research_events, T1001) as one (occurred_at, id)-ordered sequence, each
// row carrying its domain.ActivitySource — while ActivitySourceGovernance
// and ActivitySourceResearch narrow the page to one registry. Any other
// value is ErrValidation, never an empty page: a filter the reader did not
// ask for must not look like "there is nothing here".
//
// One read gate covers both sources, and it is the project's own: the
// activity feed shows what its scope may show. The gate alone is enough for
// the governance rows — audit_log has no per-row visibility column, so
// "as visible as the project" is the only rule there is to apply to them —
// but it is NOT enough for the research events: those carry a visibility
// of their own (rsg/events.go: an event is never more visible than its
// subject), and a public project's read is allowed for every matrix class,
// so passing the gate does not make the caller a member of the project it
// passed on.
//
// So the reader travels INTO the read (ADR-024's third outlet): the actor
// is the research rows' audience, and the store decides which of them are
// rendered — a row that is public, or one belonging to a project the actor
// is a member of. This is not a second authorization rule beside the
// matrix: it is the one the row's own visibility column names, applied
// where the rows are. A reader who cannot be resolved gets the public rows
// only (fail closed), never the whole set.
func (s *Service) ProjectActivity(ctx context.Context, actor domain.User, projectID string, source domain.ActivitySource, cursor string, limit int) ([]domain.AuditRecord, string, error) {
	if _, err := s.projects.Get(ctx, actor, projectID); err != nil {
		return nil, "", err // the gate's sentinel: PROJECT_NOT_FOUND for non-members
	}
	reader := actor.ID
	list := func(ctx context.Context, scope string, before *Cursor, limit int) ([]domain.AuditRecord, error) {
		return s.store.ListProjectActivity(ctx, scope, reader, before, limit, source)
	}
	return s.page(ctx, list, projectID, cursor, limit)
}

// OrgActivity returns one page of the organization's audit log, newest
// first, plus the cursor for the next page. Non-members get the
// organization surface's not-found answer.
//
// The organization feed is governance-only, so source may be "" or
// ActivitySourceGovernance and nothing else: research events are
// project-scoped (research_events carries no organization id, 00012), so
// "the organization's research events" is a query this database cannot
// answer, and answering ActivitySourceResearch with an empty page would
// present that gap as a fact about the organization. It is ErrValidation —
// the same refusal an unknown filter value gets.
func (s *Service) OrgActivity(ctx context.Context, actor domain.User, orgID string, source domain.ActivitySource, cursor string, limit int) ([]domain.AuditRecord, string, error) {
	if source == domain.ActivitySourceResearch {
		return nil, "", fmt.Errorf("%w: the organization activity feed carries governance records only", ErrValidation)
	}
	if _, err := s.orgs.Get(ctx, actor, orgID); err != nil {
		return nil, "", err // the gate's sentinel: ORG_NOT_FOUND for non-members
	}
	return s.page(ctx, s.store.ListOrganizationActivity, orgID, cursor, limit)
}

// page decodes the cursor, bounds the limit and runs one scoped query,
// returning the rows and the next-page cursor ("" when this page ends the
// log). A full page always yields a cursor — the log may have grown since
// the page was read and the client must be able to ask for more.
func (s *Service) page(ctx context.Context, list func(ctx context.Context, scope string, before *Cursor, limit int) ([]domain.AuditRecord, error), scope, cursor string, limit int) ([]domain.AuditRecord, string, error) {
	before, err := decodeBefore(cursor)
	if err != nil {
		return nil, "", err
	}
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	records, err := list(ctx, scope, before, limit)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrStore, err)
	}
	next := ""
	if len(records) == limit {
		last := records[len(records)-1]
		next = EncodeCursor(Cursor{OccurredAt: last.OccurredAt, ID: last.ID})
	}
	return records, next, nil
}

// decodeBefore parses an empty cursor as "no cursor" (the top of the log).
func decodeBefore(s string) (*Cursor, error) {
	if s == "" {
		return nil, nil
	}
	c, err := DecodeCursor(s)
	if err != nil {
		return nil, err
	}
	return &c, nil
}
