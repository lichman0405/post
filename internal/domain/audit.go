package domain

import (
	"context"
	"time"
)

// The audit domain (T0110): every high-risk action — authentication,
// governance, membership, project creation/visibility — appends one
// immutable audit_log row (CLAUDE.md §9: nothing disappears, state only
// evolves; the database itself rejects UPDATE/DELETE/TRUNCATE on the
// table, migrations 00014/00015). Each row names who acted (actor), how
// the action arrived (via) and which request caused it (correlation id —
// docs/26), so an audited event can be traced from the Activity page back
// to the request that performed it.

// Stable audit action names (the audit_log.action column). Actions are
// dotted, machine-readable strings; the Activity UI renders them, it never
// parses them.
const (
	ActionAuthSignup       = "auth.account.signup"
	ActionAuthLoginSuccess = "auth.login.success"
	ActionAuthLoginFailed  = "auth.login.failed"
	ActionAuthLogout       = "auth.logout"
	ActionOrgCreated       = "org.created"
	ActionOrgUpdated       = "org.updated"
	ActionOrgDeactivated   = "org.deactivated"
	ActionOrgMemberInvited = "org.member.invited"
	ActionOrgMemberUpdated = "org.member.updated"
	ActionOrgMemberRemoved = "org.member.removed"
	ActionProjectCreated   = "project.created"
	// T0109 settings actions: written by the project settings surface.
	ActionProjectMemberRoleChanged = "project.member_role_changed"
	ActionProjectSettingsUpdated   = "project.settings_updated"
	// T0603 policy actions: one version row per action, written in the
	// same transaction as the policy version itself.
	ActionPolicyVersionSet = "policy.version_set"
)

// Stable via values (the audit_log.via column): how the action arrived.
const (
	ViaSession  = "session"  // session-authenticated request under /api/v1
	ViaPassword = "password" // email+password login/signup
	ViaOIDC     = "oidc"     // OIDC flow
	ViaInternal = "internal" // no HTTP request context (background/ops)
)

// AuditEntry is one audit record to append. IDs are the domain text form;
// empty strings render as NULL (an action without a known actor, a global
// event without a scope). Summaries are marshalled to jsonb by the store.
type AuditEntry struct {
	ActorID        string // "" when the actor is unknown (failed login)
	Via            string // how the action arrived (Via* constants)
	Action         string // what happened (Action* constants)
	TargetRef      string // "project:<id>", "organization:<id>", "user:<id>" — "" = none
	ProjectID      string // project scope ("" = none)
	OrganizationID string // organization scope ("" = none)
	CorrelationID  string // request correlation id (docs/26)
	BeforeSummary  any    // state before the change (jsonb; nil = NULL)
	AfterSummary   any    // state after the change (jsonb; nil = NULL)
	Metadata       any    // extra facts (jsonb; nil renders as {})
}

// AuditRecord is one stored audit row as read back for the Activity page,
// with the actor's handle/display name joined in for rendering.
type AuditRecord struct {
	ID               string
	ActorID          *string
	ActorHandle      *string
	ActorDisplayName *string
	Via              string
	Action           string
	TargetRef        *string
	ProjectID        *string
	OrganizationID   *string
	CorrelationID    string
	BeforeSummary    []byte // raw jsonb; nil when NULL
	AfterSummary     []byte
	Metadata         []byte
	OccurredAt       time.Time
}

// RequestInfo carries the audit identity of the current call: who acted and
// how it arrived. The HTTP edge attaches it (the auth guard resolves the
// principal and the observability middleware the correlation id); stores
// read it back and write the audit row in the same transaction as the
// state change, so a high-risk action either commits with its audit record
// or not at all.
type RequestInfo struct {
	ActorID       string // "" when unauthenticated
	Via           string // Via* constant ("" when the call has no session)
	CorrelationID string // docs/26 request id
}

type requestInfoKey struct{}

// WithRequestInfo attaches the audit identity to ctx.
func WithRequestInfo(ctx context.Context, info RequestInfo) context.Context {
	return context.WithValue(ctx, requestInfoKey{}, info)
}

// RequestInfoFrom returns the audit identity attached to ctx, if any.
func RequestInfoFrom(ctx context.Context) (RequestInfo, bool) {
	info, ok := ctx.Value(requestInfoKey{}).(RequestInfo)
	return info, ok
}
