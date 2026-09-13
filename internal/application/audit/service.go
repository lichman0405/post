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

// ProjectActivity returns one page of the project's audit log, newest
// first, plus the cursor for the next page ("" when the page is the last).
// Non-members get the same not-found answer as for the project itself
// (existence hiding, via the projects surface's gate).
func (s *Service) ProjectActivity(ctx context.Context, actor domain.User, projectID, cursor string, limit int) ([]domain.AuditRecord, string, error) {
	if _, err := s.projects.Get(ctx, actor, projectID); err != nil {
		return nil, "", err // the gate's sentinel: PROJECT_NOT_FOUND for non-members
	}
	return s.page(ctx, s.store.ListProjectActivity, projectID, cursor, limit)
}

// OrgActivity returns one page of the organization's audit log, newest
// first, plus the cursor for the next page. Non-members get the
// organization surface's not-found answer.
func (s *Service) OrgActivity(ctx context.Context, actor domain.User, orgID, cursor string, limit int) ([]domain.AuditRecord, string, error) {
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
