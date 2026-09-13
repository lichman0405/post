package audit

import (
	"context"

	"github.com/lichman0405/post/internal/domain"
)

// Store is the read port for the Activity page: the append-only audit log,
// queried per scope with keyset pagination. There is deliberately no write
// method here — recording happens inside the owning stores' transactions
// (persistence.appendAudit) and through the authn service's recorder, never
// through this surface.
type Store interface {
	// ListProjectActivity returns the project's audit rows newest-first.
	// A nil before means "from the top".
	ListProjectActivity(ctx context.Context, projectID string, before *Cursor, limit int) ([]domain.AuditRecord, error)
	// ListOrganizationActivity returns the organization's audit rows
	// newest-first.
	ListOrganizationActivity(ctx context.Context, orgID string, before *Cursor, limit int) ([]domain.AuditRecord, error)
}

// ProjectReadGate authorizes project-scoped reads: Get answers "not found"
// for non-members (read existence hiding), the same rule the project
// surface applies. Satisfied by *projects.Service — the activity page of a
// project is exactly as visible as the project itself.
type ProjectReadGate interface {
	Get(ctx context.Context, actor domain.User, projectID string) (domain.Project, error)
}

// OrgReadGate authorizes organization-scoped reads with the same
// member-only, existence-hiding rule as the organization surface.
// Satisfied by *orgs.Service.
type OrgReadGate interface {
	Get(ctx context.Context, actor domain.User, orgID string) (domain.Organization, error)
}
