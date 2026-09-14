package policy

import (
	"context"

	"github.com/lichman0405/post/internal/domain"
)

// Ports: the policy store is the application's one gateway to canonical
// policy state (docs/52: application orchestrates against ports; adapters
// live in internal/persistence). Policy writes are append-only by
// construction — a change is a new version row, never an UPDATE — and the
// org lower-bound re-check runs inside the write transaction, so a
// concurrent org-policy publication cannot slip under it.

// PolicyStore is the persistence port for policy versions. The canonical
// policy_versions row carries exactly one scope (organization XOR
// project, the schema CHECK); the scope parameter carries that shape.
type PolicyStore interface {
	// CreateOrgVersion inserts an organization-scoped policy version in
	// one transaction with its audit row, serialized on the organization
	// row lock (the same lock org-policy writes everywhere take). It
	// fails with ErrVersionTaken when the scope already has this version
	// string, ErrOrgNotFound for an unknown organization.
	CreateOrgVersion(ctx context.Context, v domain.PolicyVersion, audit domain.AuditEntry) (domain.PolicyVersion, error)
	// CreateProjectVersion inserts a project-scoped policy version in one
	// transaction with its audit row. When orgID is non-nil the
	// transaction row-locks the organization, re-reads the org's latest
	// policy inside the lock and runs againstOrg — the lower-bound check
	// the service already applied against the state it read — so a
	// concurrent org-policy publication can never be slipped under.
	// againstOrg receives nil when the organization has no policy yet.
	// Fails with ErrVersionTaken, ErrOrgNotFound (unknown org),
	// ErrProjectNotFound (unknown project), or the againstOrg error.
	CreateProjectVersion(ctx context.Context, v domain.PolicyVersion, orgID *string, againstOrg func(orgPolicy *domain.Policy) error, audit domain.AuditEntry) (domain.PolicyVersion, error)
	// GetVersion returns one stored version by id — any age: old
	// versions stay queryable forever (acceptance). ErrPolicyNotFound
	// when the id matches nothing.
	GetVersion(ctx context.Context, id string) (domain.PolicyVersion, error)
	// Latest returns the newest version of the scope, or
	// ErrPolicyNotFound when the scope has none.
	Latest(ctx context.Context, scope domain.PolicyScope) (domain.PolicyVersion, error)
	// List returns every version of the scope, newest first — the full
	// version history (acceptance: 旧 policy version 可查询).
	List(ctx context.Context, scope domain.PolicyScope) ([]domain.PolicyVersion, error)
}

// OrgGate is the slice of organization state the policy service needs:
// the organization must exist (and be active, for writes) and the actor's
// membership decides the role gate. The production adapter is
// persistence.OrgStore; the adapter answers with the orgs application's
// sentinels, which the service maps onto its own.
type OrgGate interface {
	// GetOrganization returns the organization or orgs.ErrOrgNotFound.
	GetOrganization(ctx context.Context, orgID string) (domain.Organization, error)
	// GetMembership returns the actor's membership or
	// orgs.ErrMemberNotFound.
	GetMembership(ctx context.Context, orgID, userID string) (domain.OrganizationMembership, error)
}

// ProjectGate is the slice of project state the policy service needs: the
// project's owning organization (the lower bound source) and the actor's
// membership (the role gate). The production adapter is
// persistence.ProjectStore; the adapter answers with the projects
// application's sentinels, which the service maps onto its own.
type ProjectGate interface {
	// GetProject returns the project or projects.ErrProjectNotFound.
	GetProject(ctx context.Context, projectID string) (domain.Project, error)
	// GetMembership returns the actor's membership or
	// projects.ErrMemberNotFound.
	GetMembership(ctx context.Context, projectID, userID string) (domain.ProjectMembership, error)
}
