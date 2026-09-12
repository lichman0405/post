package orgs

// Error codes on the wire (docs/45): every organization failure maps to one
// of these stable codes, rendered inside the standard error envelope.
// Codes shared with the rest of the API (VALIDATION_FAILED,
// SERVICE_UNAVAILABLE) keep the same spelling here on purpose.
const (
	// CodeOrgNotFound is returned when the organization does not exist —
	// or exists but the caller may not see it (read existence hiding).
	CodeOrgNotFound = "ORG_NOT_FOUND"
	// CodeOrgSlugTaken is returned when creating an organization with a
	// slug that is already in use.
	CodeOrgSlugTaken = "ORG_SLUG_TAKEN"
	// CodeOrgForbidden is returned when a governance action is attempted
	// by a member who is not an owner (T0103: only owners invite and
	// adjust roles).
	CodeOrgForbidden = "ORG_FORBIDDEN"
	// CodeOrgDeactivated is returned when a write targets a deactivated
	// organization (nothing disappears; governance is frozen, not deleted).
	CodeOrgDeactivated = "ORG_DEACTIVATED"
	// CodeMemberNotFound is returned when a membership does not exist.
	CodeMemberNotFound = "MEMBER_NOT_FOUND"
	// CodeMemberAlreadyExists is returned when inviting someone who is
	// already a member of the organization.
	CodeMemberAlreadyExists = "MEMBER_ALREADY_EXISTS"
	// CodeLastOwner is returned when a change would leave the organization
	// without an active owner.
	CodeLastOwner = "LAST_OWNER"
	// CodeUserNotFound is returned when an invite names an unknown user.
	CodeUserNotFound = "USER_NOT_FOUND"
	// CodeValidationFailed is returned for malformed input.
	CodeValidationFailed = "VALIDATION_FAILED"
	// CodeServiceUnavailable is returned when the store failed.
	CodeServiceUnavailable = "SERVICE_UNAVAILABLE"
)
