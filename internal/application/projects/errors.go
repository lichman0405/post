package projects

import "errors"

// Error codes on the wire (docs/45): every project failure maps to one of
// these stable codes, rendered inside the standard error envelope. Codes
// shared with the rest of the API (VALIDATION_FAILED, SERVICE_UNAVAILABLE,
// ORG_NOT_FOUND, ORG_DEACTIVATED) keep the same spelling here on purpose.
const (
	// CodeProjectNotFound is returned when the project does not exist —
	// or exists but the caller may not see it (read existence hiding).
	CodeProjectNotFound = "PROJECT_NOT_FOUND"
	// CodeProjectSlugTaken is returned when creating a project with a
	// slug that is already in use (inside the same organization, or
	// globally for personal projects).
	CodeProjectSlugTaken = "PROJECT_SLUG_TAKEN"
	// CodeProjectForbidden is returned when the actor may not create a
	// project in the organization (not an active member).
	CodeProjectForbidden = "PROJECT_FORBIDDEN"
	// CodeProjectMembershipNotFound is returned when the actor holds no
	// membership in a project they may read (the shell's Settings gate
	// treats this as "no role", not as an error).
	CodeProjectMembershipNotFound = "PROJECT_MEMBERSHIP_NOT_FOUND"
	// CodeOrgNotFound is returned when the organization named at creation
	// does not exist.
	CodeOrgNotFound = "ORG_NOT_FOUND"
	// CodeOrgDeactivated is returned when the organization named at
	// creation is deactivated.
	CodeOrgDeactivated = "ORG_DEACTIVATED"
	// CodeProgramNotFound is returned when the program named at creation
	// does not exist.
	CodeProgramNotFound = "PROGRAM_NOT_FOUND"
	// CodeProgramOrgMismatch is returned when the program belongs to a
	// different organization than the project.
	CodeProgramOrgMismatch = "PROGRAM_ORG_MISMATCH"
	// CodeSettingsForbidden is returned when the actor may not manage the
	// project's settings (below maintainer, or not a member).
	CodeSettingsForbidden = "SETTINGS_FORBIDDEN"
	// CodeMemberNotFound is returned when the user a role change targets
	// holds no membership in the project.
	CodeMemberNotFound = "MEMBER_NOT_FOUND"
	// CodeLastOwner is returned when a role change would demote or remove
	// the project's last owner.
	CodeLastOwner = "LAST_OWNER"
	// CodeSelfRoleChangeForbidden is returned when the actor tries to
	// change their own role (self-demotion could orphan the project).
	CodeSelfRoleChangeForbidden = "SELF_ROLE_CHANGE_FORBIDDEN"
	// CodeOwnerRoleChangeForbidden is returned when a maintainer tries to
	// grant or revoke the owner role (owner management is owner-only).
	CodeOwnerRoleChangeForbidden = "OWNER_ROLE_CHANGE_FORBIDDEN"
	// CodeVisibilityChangeNotSupported is returned when a client asks to
	// change visibility: the setting is preview-only until the publishing
	// guard lands (T0109 requirement).
	CodeVisibilityChangeNotSupported = "VISIBILITY_CHANGE_NOT_SUPPORTED"
	// CodeValidationFailed is returned for malformed input.
	CodeValidationFailed = "VALIDATION_FAILED"
	// CodeServiceUnavailable is returned when the store failed.
	CodeServiceUnavailable = "SERVICE_UNAVAILABLE"
)

// Sentinel errors the service maps to wire codes. Callers test with
// errors.Is — never by string comparison.
var (
	// ErrProjectNotFound: the project does not exist (the service also
	// answers this for "exists but the caller may not see it" — read
	// existence hiding, matching the org surface).
	ErrProjectNotFound = errors.New("projects: project not found")
	// ErrSlugTaken: the project slug is already in use (inside the same
	// organization, or globally for personal projects).
	ErrSlugTaken = errors.New("projects: project slug already taken")
	// ErrForbidden: the actor may not create a project in this
	// organization (not an active member).
	ErrForbidden = errors.New("projects: not allowed")
	// ErrValidation: malformed input (slug, name, purpose, visibility,
	// program reference).
	ErrValidation = errors.New("projects: invalid input")
	// ErrOrgNotFound: the organization named at creation does not exist.
	ErrOrgNotFound = errors.New("projects: organization not found")
	// ErrOrgDeactivated: the organization named at creation is deactivated
	// (governance refuses new projects on it).
	ErrOrgDeactivated = errors.New("projects: organization deactivated")
	// ErrProgramNotFound: the program named at creation does not exist.
	ErrProgramNotFound = errors.New("projects: program not found")
	// ErrProgramOrgMismatch: the program belongs to a different
	// organization than the project (or to none).
	ErrProgramOrgMismatch = errors.New("projects: program does not belong to the project's organization")
	// ErrMemberNotFound: the project membership does not exist.
	ErrMemberNotFound = errors.New("projects: membership not found")
	// ErrTargetMemberNotFound: the user a role change targets holds no
	// membership in the project.
	ErrTargetMemberNotFound = errors.New("projects: target membership not found")
	// ErrSettingsForbidden: the actor may not manage the project's
	// settings (below maintainer, or not a member at all — the same
	// default-deny answer for both, so a stranger learns nothing).
	ErrSettingsForbidden = errors.New("projects: settings action not allowed")
	// ErrLastOwner: the change would demote the project's last owner.
	ErrLastOwner = errors.New("projects: project must keep at least one owner")
	// ErrSelfRoleChange: the actor may not change their own role.
	ErrSelfRoleChange = errors.New("projects: cannot change your own role")
	// ErrOwnerRoleChange: a maintainer may not grant or revoke the owner
	// role.
	ErrOwnerRoleChange = errors.New("projects: only owners manage the owner role")
	// ErrVisibilityChangeNotSupported: visibility is preview-only until
	// the publishing guard lands.
	ErrVisibilityChangeNotSupported = errors.New("projects: visibility changes are not supported yet")
	// ErrStore: the store itself failed (dependency down, driver error).
	ErrStore = errors.New("projects: store failure")
)
