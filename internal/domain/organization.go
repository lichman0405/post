package domain

import (
	"strings"
	"time"
)

// Organization is the network subject (company, lab, institute, team) that
// owns projects (docs/03 §1, docs/04 §6). Like User, it is never physically
// deleted: DeactivatedAt marks a deactivated organization (governance must
// refuse new members/projects/role changes on it), history keeps referring
// to it (CLAUDE.md §9.8). The canonical columns live in
// infra/migrations/00002_identity.sql.
type Organization struct {
	// ID is the uuid v4 text form (matches the organizations.id uuid column).
	ID          string
	Slug        string
	Name        string
	Description string
	CreatedAt   time.Time
	// DeactivatedAt marks a deactivated (soft-deleted) organization, nil
	// while active. Deactivation is the only "delete" V1 offers.
	DeactivatedAt *time.Time
}

// Active reports whether the organization is currently usable: not
// deactivated (a future deactivated_at timestamp still counts as active
// until it is reached).
func (o Organization) Active() bool {
	return o.DeactivatedAt == nil || o.DeactivatedAt.After(time.Now())
}

// OrgRole is the access role a membership grants inside an organization
// (docs/04 §2). The canonical constraint allows exactly these four values
// (organization_memberships.role CHECK).
type OrgRole string

const (
	OrgRoleOwner       OrgRole = "owner"
	OrgRoleMaintainer  OrgRole = "maintainer"
	OrgRoleContributor OrgRole = "contributor"
	OrgRoleViewer      OrgRole = "viewer"
)

// ValidOrgRole reports whether r is one of the four canonical roles.
func ValidOrgRole(r OrgRole) bool {
	switch r {
	case OrgRoleOwner, OrgRoleMaintainer, OrgRoleContributor, OrgRoleViewer:
		return true
	}
	return false
}

// Governs reports whether the role may manage organization governance:
// membership (invite/adjust role/end affiliation) and the organization
// profile itself. V1: owners only (docs/04 — maintainer governance applies
// inside projects, T0105).
func (r OrgRole) Governs() bool { return r == OrgRoleOwner }

// NormalizeOrgSlug canonicalizes an organization slug: trimmed and
// lowercased (same convention as NormalizeHandle — identity charsets are
// [a-z0-9-]).
func NormalizeOrgSlug(slug string) string {
	return strings.ToLower(strings.TrimSpace(slug))
}

// ValidOrgSlug reports whether slug has the organization identity shape
// after normalization: 1..64 characters of [a-z0-9-], no leading/trailing
// '-' — the slug is the organization's stable public identifier, so it
// follows the handle rules (T0103 L1).
func ValidOrgSlug(slug string) bool {
	n := NormalizeOrgSlug(slug)
	if n == "" || len(n) > 64 {
		return false
	}
	if strings.HasPrefix(n, "-") || strings.HasSuffix(n, "-") {
		return false
	}
	for _, r := range n {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

// ValidOrgName reports whether name is a usable organization display name:
// non-blank, at most 200 characters (room for long institutional names).
func ValidOrgName(name string) bool {
	n := strings.TrimSpace(name)
	return n != "" && len(n) <= 200
}

// OrganizationMembership is the relationship between a person and an
// organization (canonical table organization_memberships). One row per
// (organization, user) pair: the row carries the current role plus the
// affiliation span and verification status. Ending an affiliation sets
// AffiliationEnd and keeps the row — history is never deleted
// (docs/04 §6: "离职只终止 affiliation/role").
type OrganizationMembership struct {
	OrganizationID string
	UserID         string
	Role           OrgRole
	// AffiliationStart/End are calendar dates (date column). End is nil
	// while the affiliation is current; setting it is how a membership
	// ends, never a row deletion.
	AffiliationStart time.Time
	AffiliationEnd   *time.Time
	// Verified: the organization confirmed the affiliation (owner action).
	Verified bool
}

// Active reports whether the affiliation is currently in force: started and
// not ended yet. Comparison runs date-to-date (calendar days, UTC) — the
// canonical columns are dates, so "starts today" is active for the whole
// of today regardless of the local clock.
func (m OrganizationMembership) Active() bool {
	now := dateUTC(time.Now())
	if m.AffiliationEnd != nil && !m.AffiliationEnd.After(now) {
		return false
	}
	return !m.AffiliationStart.After(now)
}

// dateUTC truncates a timestamp to its UTC calendar date.
func dateUTC(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
