package policy

import "errors"

// Sentinel errors the service maps to wire codes. Callers test with
// errors.Is — never by string comparison.
var (
	// ErrPolicyNotFound: the scope has no policy (yet), or a named
	// version does not exist.
	ErrPolicyNotFound = errors.New("policy: policy version not found")
	// ErrVersionTaken: the scope already has a version with this string
	// (per-scope uniqueness, migration 00033 — append-only means a
	// version string is never reused).
	ErrVersionTaken = errors.New("policy: version already exists for this scope")
	// ErrOrgNotFound: the organization does not exist — or exists but the
	// caller may not see it (read existence hiding, same shape as the
	// orgs surface).
	ErrOrgNotFound = errors.New("policy: organization not found")
	// ErrOrgDeactivated: a policy write targeted a deactivated
	// organization.
	ErrOrgDeactivated = errors.New("policy: organization deactivated")
	// ErrProjectNotFound: the project does not exist — or exists but the
	// caller may not see it.
	ErrProjectNotFound = errors.New("policy: project not found")
	// ErrForbidden: the actor lacks the governance role for the action
	// (writes are owner-only; the same answer for "not a member" and
	// "role too low" — the check discloses nothing).
	ErrForbidden = errors.New("policy: not allowed")
	// ErrProjectRelaxesOrg: the project policy would relax an
	// organization rule (docs/12 §5). Wraps domain.MergeViolations with
	// every offending rule.
	ErrProjectRelaxesOrg = errors.New("policy: project policy relaxes the organization policy")
	// ErrValidation: malformed input (version string, policy document).
	ErrValidation = errors.New("policy: invalid input")
	// ErrStore: the store itself failed (dependency down, driver error).
	ErrStore = errors.New("policy: store failure")
)

// Error codes on the wire (docs/45): every policy failure maps to one of
// these stable codes, rendered inside the standard error envelope.
const (
	// CodePolicyNotFound: no policy (yet) for the scope, or an unknown
	// version id.
	CodePolicyNotFound = "POLICY_NOT_FOUND"
	// CodePolicyVersionTaken: the version string already exists for the
	// scope.
	CodePolicyVersionTaken = "POLICY_VERSION_TAKEN"
	// CodePolicyOrgNotFound: unknown organization (or not visible to the
	// caller).
	CodePolicyOrgNotFound = "POLICY_ORG_NOT_FOUND"
	// CodePolicyOrgDeactivated: a write targeted a deactivated
	// organization.
	CodePolicyOrgDeactivated = "POLICY_ORG_DEACTIVATED"
	// CodePolicyProjectNotFound: unknown project (or not visible).
	CodePolicyProjectNotFound = "POLICY_PROJECT_NOT_FOUND"
	// CodePolicyForbidden: the actor may not perform the action.
	CodePolicyForbidden = "POLICY_FORBIDDEN"
	// CodePolicyRelaxesOrg: the project policy relaxes an org rule.
	CodePolicyRelaxesOrg = "POLICY_RELAXES_ORG"
	// CodeValidationFailed: malformed input.
	CodeValidationFailed = "VALIDATION_FAILED"
	// CodeServiceUnavailable: the store failed.
	CodeServiceUnavailable = "SERVICE_UNAVAILABLE"
)
