package domain

import (
	"strings"
	"time"
)

// Project is the research boundary: one explicit R&D goal, the hosting unit
// for the repository and the RSG (docs/03 §1, CLAUDE.md §9.1). The canonical
// columns live in infra/migrations/00003_projects.sql; 00019 adds
// provision_status. A project is never physically deleted: lifecycle is
// expressed by activity_status (planning/active/paused/archived).
type Project struct {
	// ID is the uuid v4 text form (matches the projects.id uuid column).
	ID string
	// OrganizationID is the owning organization, nil for a personal project
	// (projects.organization_id is nullable — the canonical schema and the
	// OpenAPI contract both anticipate personal projects).
	OrganizationID *string
	// ProgramID optionally groups the project under a program (docs/03:
	// Program 是长期研发方向的可选分组，不承载版本控制); nil when none.
	ProgramID *string
	Slug      string
	Name      string
	// Purpose states the research goal in one sentence (required).
	Purpose string
	// ActivityStatus is the lifecycle state (canonical default 'planning').
	ActivityStatus string
	// Visibility is the Public/Private preset chosen at creation.
	Visibility ProjectVisibility
	// MainFrozen reports whether the frozen main branch gate is on.
	MainFrozen bool
	// GitRepositoryExternalID is the GitProvider-side repository reference;
	// nil until the repository is provisioned (T0301).
	GitRepositoryExternalID *string
	CreatedBy               string
	CreatedAt               time.Time
	// ProvisionStatus tracks the GitProvider provisioning state: every new
	// project is pending until the provisioning task (T0301) flips it to
	// provisioned (or failed).
	ProvisionStatus ProvisionStatus
}

// Personal reports whether the project is a personal project (no
// owning organization).
func (p Project) Personal() bool { return p.OrganizationID == nil }

// ProjectVisibility is the Public/Private preset (projects.visibility
// CHECK). It is chosen once at creation; the read-isolation rules build on
// it in T0106.
type ProjectVisibility string

const (
	VisibilityPublic  ProjectVisibility = "public"
	VisibilityPrivate ProjectVisibility = "private"
)

// ValidProjectVisibility reports whether v is one of the two canonical
// presets.
func ValidProjectVisibility(v ProjectVisibility) bool {
	return v == VisibilityPublic || v == VisibilityPrivate
}

// ValidActivityStatus reports whether s is one of the four canonical
// lifecycle states (projects.activity_status CHECK).
func ValidActivityStatus(s string) bool {
	switch s {
	case "planning", "active", "paused", "archived":
		return true
	}
	return false
}

// ProvisionStatus is the GitProvider provisioning state
// (projects.provision_status CHECK). New projects are pending by default;
// T0301's Gitea adapter moves them to provisioned (or failed).
type ProvisionStatus string

const (
	ProvisionPending     ProvisionStatus = "pending"
	ProvisionProvisioned ProvisionStatus = "provisioned"
	ProvisionFailed      ProvisionStatus = "failed"
)

// ValidProvisionStatus reports whether s is one of the three canonical
// states.
func ValidProvisionStatus(s ProvisionStatus) bool {
	switch s {
	case ProvisionPending, ProvisionProvisioned, ProvisionFailed:
		return true
	}
	return false
}

// ProjectRole is the access role a membership grants inside a project
// (project_memberships.role CHECK). The permission matrix lands with T0105;
// T0104 only ever writes owner.
type ProjectRole string

const (
	ProjectRoleOwner       ProjectRole = "owner"
	ProjectRoleMaintainer  ProjectRole = "maintainer"
	ProjectRoleContributor ProjectRole = "contributor"
	ProjectRoleViewer      ProjectRole = "viewer"
)

// ValidProjectRole reports whether r is one of the four canonical roles.
func ValidProjectRole(r ProjectRole) bool {
	switch r {
	case ProjectRoleOwner, ProjectRoleMaintainer, ProjectRoleContributor, ProjectRoleViewer:
		return true
	}
	return false
}

// Rank orders the four project roles by authority (docs/04 §2): viewer <
// contributor < maintainer < owner. The permission matrix's role columns
// are consistent with this order — internal/authz's
// TestRoleColumnsMonotonic enforces it. An unknown role ranks -1.
func (r ProjectRole) Rank() int {
	switch r {
	case ProjectRoleViewer:
		return 0
	case ProjectRoleContributor:
		return 1
	case ProjectRoleMaintainer:
		return 2
	case ProjectRoleOwner:
		return 3
	}
	return -1
}

// AtLeast reports whether r carries at least the authority of min.
// Unknown roles answer false: an unparseable role grants nothing.
func (r ProjectRole) AtLeast(min ProjectRole) bool {
	rank, minRank := r.Rank(), min.Rank()
	return rank >= 0 && minRank >= 0 && rank >= minRank
}

// ProjectMembership is the relationship between a person and a project
// (canonical table project_memberships). One row per (project, user) pair.
type ProjectMembership struct {
	ProjectID string
	UserID    string
	Role      ProjectRole
	CreatedAt time.Time
}

// ProjectMember is one row of the settings member list: the membership
// joined with the user's public identity (handle + display name). The
// store's ListProjectMembers query is the only producer.
type ProjectMember struct {
	UserID      string
	Handle      string
	DisplayName string
	Role        ProjectRole
	JoinedAt    time.Time
}

// Program is a long-term research direction that optionally groups projects
// (docs/03: "长期研发方向，例如 MOF 气体分离"). It carries no version
// control of its own. Canonical table: programs.
type Program struct {
	ID string
	// OrganizationID is the owning organization (nullable in the canonical
	// schema; V1 only ever creates programs inside organizations).
	OrganizationID *string
	Slug           string
	Name           string
	Description    string
	CreatedAt      time.Time
}

// NormalizeProjectSlug canonicalizes a project slug: trimmed and lowercased
// (same convention as NormalizeOrgSlug — the identity charset is [a-z0-9-]).
func NormalizeProjectSlug(slug string) string {
	return strings.ToLower(strings.TrimSpace(slug))
}

// ValidProjectSlug reports whether slug has the project identity shape
// after normalization: 1..64 characters of [a-z0-9-], no leading/trailing
// '-' — the slug is the project's public identifier inside its
// organization (or globally for personal projects), so it follows the same
// rules as org slugs (T0104 L1).
func ValidProjectSlug(slug string) bool {
	n := NormalizeProjectSlug(slug)
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

// ValidProjectName reports whether name is a usable project display name:
// non-blank, at most 200 characters.
func ValidProjectName(name string) bool {
	n := strings.TrimSpace(name)
	return n != "" && len(n) <= 200
}

// ValidProjectPurpose reports whether purpose is a usable research-goal
// statement: non-blank and bounded (generous but finite — a hostile client
// must not be able to store megabytes per project).
func ValidProjectPurpose(purpose string) bool {
	n := strings.TrimSpace(purpose)
	return n != "" && len(n) <= 4000
}
