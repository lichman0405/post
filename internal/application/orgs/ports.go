package orgs

import (
	"context"
	"errors"
	"time"

	"github.com/lichman0405/post/internal/domain"
)

// Ports: the organization store is the application's one gateway to
// canonical state (docs/52: application orchestrates against ports;
// adapters live in internal/persistence). The store is deliberately
// atomic at the operation level: multi-step writes (create org + creator
// membership, last-owner-guarded role changes) are one store call each,
// so the governance invariants hold under concurrency.

// Sentinel errors the service maps to wire codes. Callers test with
// errors.Is — never by string comparison.
var (
	// ErrOrgNotFound: the organization does not exist (the service also
	// answers this for "exists but the caller may not see it").
	ErrOrgNotFound = errors.New("orgs: organization not found")
	// ErrSlugTaken: the organization slug is already in use.
	ErrSlugTaken = errors.New("orgs: organization slug already taken")
	// ErrForbidden: the actor lacks the governance role for the action.
	ErrForbidden = errors.New("orgs: not allowed")
	// ErrValidation: malformed input (slug, name, role, dates).
	ErrValidation = errors.New("orgs: invalid input")
	// ErrOrgDeactivated: a write targeted a deactivated organization.
	ErrOrgDeactivated = errors.New("orgs: organization deactivated")
	// ErrMemberNotFound: the membership does not exist.
	ErrMemberNotFound = errors.New("orgs: membership not found")
	// ErrAlreadyMember: the user already has a membership (any state).
	ErrAlreadyMember = errors.New("orgs: user is already a member")
	// ErrLastOwner: the change would leave the organization without an
	// active owner (demoting or removing the last one).
	ErrLastOwner = errors.New("orgs: organization would lose its last owner")
	// ErrUserNotFound: an invite named an unknown user.
	ErrUserNotFound = errors.New("orgs: user not found")
	// ErrStore: the store itself failed (dependency down, driver error).
	ErrStore = errors.New("orgs: store failure")
)

// OrgStore is the persistence port for organizations and memberships.
type OrgStore interface {
	// CreateOrganization creates the organization and the creator's owner
	// membership (verified, affiliation starting affiliationStart) in one
	// transaction. It fails with ErrSlugTaken when the slug is taken.
	CreateOrganization(ctx context.Context, org domain.Organization, creatorUserID string, affiliationStart time.Time) (domain.Organization, domain.OrganizationMembership, error)
	// GetOrganization returns the organization or ErrOrgNotFound.
	GetOrganization(ctx context.Context, orgID string) (domain.Organization, error)
	// UpdateOrganization persists name/description changes and returns the
	// updated organization, or ErrOrgNotFound.
	UpdateOrganization(ctx context.Context, org domain.Organization) (domain.Organization, error)
	// DeactivateOrganization sets deactivated_at (soft delete — the only
	// "delete" the domain offers), or ErrOrgNotFound.
	DeactivateOrganization(ctx context.Context, orgID string) error
	// ListOrganizationsForUser returns the organizations the user
	// currently belongs to (open affiliation), newest first.
	ListOrganizationsForUser(ctx context.Context, userID string) ([]domain.Organization, error)
	// GetMembership returns one membership or ErrMemberNotFound.
	GetMembership(ctx context.Context, orgID, userID string) (domain.OrganizationMembership, error)
	// GetUserByHandle resolves an account by its handle (invites name
	// people, not ids), or ErrUserNotFound.
	GetUserByHandle(ctx context.Context, handle string) (domain.User, error)
	// ListMembers returns every membership of the organization (current
	// and historical — history is never deleted). Order (as stored by the
	// adapter): current members first (open affiliation), then most
	// recent affiliation start, then user_id for a stable order.
	ListMembers(ctx context.Context, orgID string) ([]domain.OrganizationMembership, error)
	// AddMembership inserts a new membership. It fails with
	// ErrAlreadyMember when any membership row for the pair exists,
	// ErrUserNotFound for an unknown user, ErrOrgNotFound for an unknown
	// organization, and ErrOrgDeactivated when the organization is
	// deactivated (checked inside the insert transaction under the
	// organization row lock).
	AddMembership(ctx context.Context, m domain.OrganizationMembership) (domain.OrganizationMembership, error)
	// UpdateMembershipRoleAndDates persists a full role/start/verified
	// change. affiliation_end is never written by this method — ending an
	// affiliation is EndAffiliation's exclusive job — so an adjustment of
	// an ended membership preserves the departure date. When the target is
	// currently an active owner and the new role is not owner, the store
	// refuses with ErrLastOwner if the organization would lose its last
	// active owner; unknown memberships answer ErrMemberNotFound.
	UpdateMembershipRoleAndDates(ctx context.Context, orgID, userID string, role domain.OrgRole, affiliationStart time.Time, verified bool) (domain.OrganizationMembership, error)
	// EndAffiliation sets affiliation_end on the membership and keeps the
	// row (离职不删除历史). Ending an already-ended membership is a no-op —
	// the historical end date is never re-stamped. It refuses with
	// ErrLastOwner when ending the affiliation of the last active owner;
	// unknown memberships answer ErrMemberNotFound.
	EndAffiliation(ctx context.Context, orgID, userID string, end time.Time) error
}
