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
	// ListProjectActivity returns the project's Activity rows newest-first
	// — governance (audit_log) rows, research events, or both, as source
	// selects ("" reads both registries, T0607). A nil before means "from
	// the top". Every row carries its Source, and the two streams are
	// paginated as ONE ordered sequence on (occurred_at, id): a page of a
	// project that mixes audit rows and events is still exactly one keyset
	// window, never a concatenation of two.
	//
	// readerUserID is the reader the rows are rendered to, and it is an
	// input of the read rather than a filter over its result (ADR-024):
	// the research events carry a visibility of their own, and a public
	// project's read gate admits readers who are not members of it, so the
	// row set cannot be derived from the gate alone. An EMPTY (or
	// unresolvable) reader id is the anonymous audience and must answer the
	// publicly visible rows only — never the full set, and never an error.
	// Governance rows take no audience: audit_log has no per-row visibility
	// column to render them by.
	ListProjectActivity(ctx context.Context, projectID, readerUserID string, before *Cursor, limit int, source domain.ActivitySource) ([]domain.AuditRecord, error)
	// ListOrganizationActivity returns the organization's governance rows
	// newest-first. Research events are project-scoped (research_events
	// carries no organization id), so this feed has one source and no
	// source parameter.
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
