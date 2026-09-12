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
	// ErrStore: the store itself failed (dependency down, driver error).
	ErrStore = errors.New("projects: store failure")
)
