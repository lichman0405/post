package orgs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lichman0405/post/internal/domain"
)

// Service orchestrates organization governance use cases. All policy lives
// here (docs/52): the transport layer only translates requests into these
// calls. The governance rules (T0103):
//
//   - creating an organization makes the creator its owner (verified,
//     affiliation starting today);
//   - only active owners invite members, adjust roles and dates, and
//     deactivate the organization;
//   - a member whose affiliation ended holds no governance powers;
//   - a non-owner cannot change any membership — their own included, so
//     no member can promote themselves (acceptance criterion);
//   - leaving/removal sets affiliation_end and keeps the row: history is
//     never deleted (docs/04 §6);
//   - an organization never loses its last active owner.
type Service struct {
	store OrgStore
}

// NewService wires the service on the store port.
func NewService(store OrgStore) *Service { return &Service{store: store} }

// maxDescription bounds the free-text description (generous but finite —
// a hostile client must not be able to store megabytes per organization).
const maxDescription = 4000

// Create registers a new organization with the actor as its owner. The
// creator's membership is verified (the act of creating the organization
// under the account proves the affiliation) and starts today.
func (s *Service) Create(ctx context.Context, actor domain.User, slug, name, description string) (domain.Organization, domain.OrganizationMembership, error) {
	name, description, err := validatedOrgFields(name, description)
	if err != nil {
		return domain.Organization{}, domain.OrganizationMembership{}, err
	}
	if !domain.ValidOrgSlug(slug) {
		return domain.Organization{}, domain.OrganizationMembership{}, fmt.Errorf("%w: slug may only contain lowercase letters, digits and dashes (max 64 characters)", ErrValidation)
	}
	org := domain.Organization{
		Slug:        domain.NormalizeOrgSlug(slug),
		Name:        name,
		Description: description,
	}
	created, membership, err := s.store.CreateOrganization(ctx, org, actor.ID, today())
	if err != nil {
		return domain.Organization{}, domain.OrganizationMembership{}, wrapStoreError(err)
	}
	return created, membership, nil
}

// Get returns the organization for a current member. Non-members (and
// unknown organizations) answer ErrOrgNotFound — the organization's
// existence is not disclosed to outsiders. Members keep read access after
// deactivation (their history is still theirs to see); only writes freeze.
// Store failures answer ErrStore (503), never a masked 404.
func (s *Service) Get(ctx context.Context, actor domain.User, orgID string) (domain.Organization, error) {
	org, err := s.store.GetOrganization(ctx, orgID)
	if err != nil {
		return domain.Organization{}, wrapStoreError(err)
	}
	m, err := s.store.GetMembership(ctx, orgID, actor.ID)
	if err != nil {
		if errors.Is(err, ErrMemberNotFound) {
			return domain.Organization{}, ErrOrgNotFound
		}
		return domain.Organization{}, wrapStoreError(err)
	}
	if !m.Active() {
		return domain.Organization{}, ErrOrgNotFound
	}
	return org, nil
}

// List returns the organizations the actor currently belongs to, newest
// first.
func (s *Service) List(ctx context.Context, actor domain.User) ([]domain.Organization, error) {
	orgs, err := s.store.ListOrganizationsForUser(ctx, actor.ID)
	if err != nil {
		return nil, wrapStoreError(err)
	}
	return orgs, nil
}

// Update renames the organization / rewrites its description. Owner only.
// The slug is the organization's stable public identity and is not
// mutable. name/description/attestationAttribution are pointers: nil means
// "unchanged" (PATCH partial semantics — a name-only update must not wipe
// the description), an explicit empty description clears it, an empty name
// is refused after the merge.
//
// attestationAttribution travels into the SAME store call as the two text
// fields rather than following it, so the three edits land together or not
// at all: they are three columns of one row, and a rename that succeeded
// while the attribution that came with it silently did not would be a
// settings screen that lies.
func (s *Service) Update(ctx context.Context, actor domain.User, orgID string, name, description, attestationAttribution *string) (domain.Organization, error) {
	if err := s.requireGovernor(ctx, actor.ID, orgID); err != nil {
		return domain.Organization{}, err
	}
	if name == nil && description == nil && attestationAttribution == nil {
		return domain.Organization{}, fmt.Errorf("%w: at least one of name, description or attestation_attribution is required", ErrValidation)
	}
	// The vocabulary is checked BEFORE anything is read or written: a
	// request naming a value outside it is refused whatever the repository
	// holds (docs/45).
	var attributionPtr *string
	if attestationAttribution != nil {
		attribution := strings.TrimSpace(*attestationAttribution)
		if !ValidAttestationAttribution(attribution) {
			return domain.Organization{}, attributionValidationError(attribution)
		}
		attributionPtr = &attribution
	}
	current, err := s.store.GetOrganization(ctx, orgID)
	if err != nil {
		return domain.Organization{}, wrapStoreError(err)
	}
	next := current
	if name != nil {
		next.Name = strings.TrimSpace(*name)
	}
	if description != nil {
		next.Description = trimTo(*description, maxDescription)
	}
	if !domain.ValidOrgName(next.Name) {
		return domain.Organization{}, fmt.Errorf("%w: name is required (max 200 characters)", ErrValidation)
	}
	updated, err := s.store.UpdateOrganization(ctx, next, attributionPtr)
	if err != nil {
		return domain.Organization{}, wrapStoreError(err)
	}
	return updated, nil
}

// AttestationAttribution reads the organization's standing answer to "may
// this organization be named on an attestation it issues" (migration
// 00120). Any current member may read it — it is the organization's own
// setting, not a governance secret — and it is what the single-organization
// read renders beside the name.
func (s *Service) AttestationAttribution(ctx context.Context, actor domain.User, orgID string) (string, error) {
	if _, err := s.Get(ctx, actor, orgID); err != nil {
		return "", err
	}
	value, err := s.store.GetAttestationAttribution(ctx, orgID)
	if err != nil {
		return "", wrapStoreError(err)
	}
	return value, nil
}

// Deactivate soft-deletes the organization: deactivated_at is set, nothing
// is physically removed, and every governance write is refused afterwards
// (domain invariant: nothing disappears, state only evolves). Owner only.
func (s *Service) Deactivate(ctx context.Context, actor domain.User, orgID string) error {
	if err := s.requireGovernor(ctx, actor.ID, orgID); err != nil {
		return err
	}
	if err := s.store.DeactivateOrganization(ctx, orgID); err != nil {
		return wrapStoreError(err)
	}
	return nil
}

// ListMembers returns every membership of the organization — current and
// historical (ended affiliations stay visible to members). Any current
// member may read it.
func (s *Service) ListMembers(ctx context.Context, actor domain.User, orgID string) ([]domain.OrganizationMembership, error) {
	if _, err := s.Get(ctx, actor, orgID); err != nil {
		return nil, err
	}
	members, err := s.store.ListMembers(ctx, orgID)
	if err != nil {
		return nil, wrapStoreError(err)
	}
	return members, nil
}

// Invite adds a member with the given role. Owner only. The invitee must
// already have an account (V1 has no pending invitations for external
// addresses — that is a product decision for a later task); the invitee's
// affiliation starts today unless a start date is given, and is
// unverified until the owner verifies it (or invites with verified=true).
func (s *Service) Invite(ctx context.Context, actor domain.User, orgID, handle string, role domain.OrgRole, affiliationStart *time.Time, verified bool) (domain.OrganizationMembership, error) {
	if err := s.requireGovernor(ctx, actor.ID, orgID); err != nil {
		return domain.OrganizationMembership{}, err
	}
	if !domain.ValidOrgRole(role) {
		return domain.OrganizationMembership{}, fmt.Errorf("%w: unknown role %q", ErrValidation, role)
	}
	start := today()
	if affiliationStart != nil {
		if affiliationStart.IsZero() {
			return domain.OrganizationMembership{}, fmt.Errorf("%w: affiliation start must be a valid date", ErrValidation)
		}
		start = dateOnly(*affiliationStart)
	}
	target, err := s.store.GetUserByHandle(ctx, domain.NormalizeHandle(handle))
	if err != nil {
		return domain.OrganizationMembership{}, wrapStoreError(err)
	}
	created, err := s.store.AddMembership(ctx, domain.OrganizationMembership{
		OrganizationID:   orgID,
		UserID:           target.ID,
		Role:             role,
		AffiliationStart: start,
		Verified:         verified,
	})
	if err != nil {
		return domain.OrganizationMembership{}, wrapStoreError(err)
	}
	return created, nil
}

// UpdateMembership adjusts a membership: role, affiliation start and/or
// verified flag (at least one must be provided; nil means unchanged).
// Owner only — so a non-owner can never change a role, their own
// included (acceptance: 普通成员不能提升自己). Ending an affiliation is
// not done here: that is RemoveMember, which keeps the row.
func (s *Service) UpdateMembership(ctx context.Context, actor domain.User, orgID, userID string, role *domain.OrgRole, affiliationStart *time.Time, verified *bool) (domain.OrganizationMembership, error) {
	if err := s.requireGovernor(ctx, actor.ID, orgID); err != nil {
		return domain.OrganizationMembership{}, err
	}
	if role == nil && affiliationStart == nil && verified == nil {
		return domain.OrganizationMembership{}, fmt.Errorf("%w: at least one of role, affiliation_start, verified is required", ErrValidation)
	}
	current, err := s.store.GetMembership(ctx, orgID, userID)
	if err != nil {
		return domain.OrganizationMembership{}, wrapStoreError(err)
	}
	next := current
	if role != nil {
		if !domain.ValidOrgRole(*role) {
			return domain.OrganizationMembership{}, fmt.Errorf("%w: unknown role %q", ErrValidation, *role)
		}
		next.Role = *role
	}
	if affiliationStart != nil {
		if affiliationStart.IsZero() {
			return domain.OrganizationMembership{}, fmt.Errorf("%w: affiliation start must be a valid date", ErrValidation)
		}
		next.AffiliationStart = dateOnly(*affiliationStart)
	}
	if verified != nil {
		next.Verified = *verified
	}
	updated, err := s.store.UpdateMembershipRoleAndDates(ctx, orgID, userID, next.Role, next.AffiliationStart, next.Verified)
	if err != nil {
		return domain.OrganizationMembership{}, wrapStoreError(err)
	}
	return updated, nil
}

// RemoveMember ends a membership: affiliation_end is set to today and the
// row is kept (离职不删除历史). An owner may end anyone's affiliation;
// any member may end their own (leaving the organization). The last active
// owner can neither be removed nor leave — ownership must be transferred
// first (promote another owner, then leave).
func (s *Service) RemoveMember(ctx context.Context, actor domain.User, orgID, userID string) error {
	org, err := s.store.GetOrganization(ctx, orgID)
	if err != nil {
		return wrapStoreError(err)
	}
	if !org.Active() {
		return ErrOrgDeactivated
	}
	actorMembership, err := s.store.GetMembership(ctx, orgID, actor.ID)
	if err != nil {
		if errors.Is(err, ErrMemberNotFound) {
			return ErrForbidden
		}
		return wrapStoreError(err)
	}
	if !actorMembership.Active() || (!actorMembership.Role.Governs() && actor.ID != userID) {
		return ErrForbidden
	}
	if err := s.store.EndAffiliation(ctx, orgID, userID, today()); err != nil {
		return wrapStoreError(err)
	}
	return nil
}

// dateOnly truncates a timestamp to the calendar date it names (the canonical
// columns are date, not timestamptz).
//
// It reads the value's OWN year/month/day, and that is the point: this is the
// normalizer for dates a caller supplied. "2026-09-19" is a day, not an
// instant, and it must not be moved across a day boundary by whatever zone
// the server happens to run in (the transport parses it as UTC midnight, so
// the day it names is the day it meant). Only "now" needs the affiliation
// convention's UTC day — an instant has no date of its own; see today below
// and domain.AffiliationDay.
func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// today returns the affiliation day of "now": the current calendar date in
// UTC, as the convention in internal/domain defines it and the ledger
// projection resolves against (docs/13 §1 "affiliation at time").
//
// It used to read the LOCAL year/month/day and label it UTC, which dated a
// membership created in the first hours of a UTC+8 morning one day into the
// future: the projection then found no organization for the person's work on
// the day they joined. One convention, asked of one place
// (domain.AffiliationDay) — see tests/integration/affiliation_date_test.go.
func today() time.Time { return domain.AffiliationDay(time.Now()) }

// validatedOrgFields applies the free-text input rules: name required
// (max 200 chars), description bounded (max 4000 chars).
func validatedOrgFields(name, description string) (string, string, error) {
	if !domain.ValidOrgName(name) {
		return "", "", fmt.Errorf("%w: name is required (max 200 characters)", ErrValidation)
	}
	return trimTo(name, 200), trimTo(description, maxDescription), nil
}

// trimTo trims and length-bounds a free-text field. Truncation happens on
// rune boundaries — slicing bytes could split a multi-byte rune and store
// invalid UTF-8.
func trimTo(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	r := []rune(s)
	if len(r) > max {
		s = string(r[:max])
	}
	return s
}

// requireGovernor resolves the actor's membership and requires an active
// owner role. The organization itself must be active too: a deactivated
// organization refuses every governance write. A store failure answers
// ErrStore (503), never a masked 403 — only a membership that exists but
// does not govern is a permission problem.
func (s *Service) requireGovernor(ctx context.Context, actorID, orgID string) error {
	org, err := s.store.GetOrganization(ctx, orgID)
	if err != nil {
		return wrapStoreError(err)
	}
	if !org.Active() {
		return ErrOrgDeactivated
	}
	m, err := s.store.GetMembership(ctx, orgID, actorID)
	if err != nil {
		if errors.Is(err, ErrMemberNotFound) {
			return ErrForbidden
		}
		return wrapStoreError(err)
	}
	if !m.Active() || !m.Role.Governs() {
		return ErrForbidden
	}
	return nil
}

// wrapStoreError maps the store's sentinel errors onto the service's
// (they share the names by design, but the store contract is explicit
// about which errors each method may return).
func wrapStoreError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrOrgNotFound),
		errors.Is(err, ErrSlugTaken),
		errors.Is(err, ErrMemberNotFound),
		errors.Is(err, ErrAlreadyMember),
		errors.Is(err, ErrLastOwner),
		errors.Is(err, ErrUserNotFound),
		errors.Is(err, ErrOrgDeactivated):
		return err
	default:
		return fmt.Errorf("%w: %v", ErrStore, err)
	}
}
