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
	// T0213 schema profile actions: one version row per registration,
	// written in the same transaction as the profile version itself
	// (same one-unit rule as the policy surface).
	ActionSchemaProfileRegistered = "project.schema_profile_registered"

	// T0606 release governance: one release row per action, written in
	// the same transaction as the release snapshot itself.
	ActionReleaseCreated = "release.created"

	// T0407 conflict resolution: one row per saved resolution plan, in the
	// same transaction as the decisions themselves (docs/60: scientific
	// conflict final resolution must be human-governed — the audit row is
	// the record that a human, not an agent, decided).
	ActionConflictResolutionSaved = "conflict.resolution_saved"

	// T0409 merge governance: one row per Research PR merge, in the same
	// transaction as the accepted state it produced. docs/09 §3 freezes
	// main, so this row is the audit trail of every change main ever
	// accepted — who merged which proposal into which state, under which
	// plan. Its name follows the dotted `<subject>.<verb-past>` convention
	// the other actions use.
	//
	// It spells the same dotted name as the research EVENT the merge emits
	// (pull_request.merged, specs/events/event-types.yaml) and that is a
	// coincidence of two separate registries, not a shared identity: the
	// audit vocabulary is this file, the event vocabulary is the event
	// spec, subscribers route on events, the Activity page reads audit
	// actions, and neither list is derived from the other.
	ActionPullRequestMerged = "pull_request.merged"

	// T0609 project milestones: one milestone row per action, written in
	// the same transaction as the milestone row itself.
	ActionMilestoneCreated = "milestone.created"

	// T0711 asset governance: one row per rights-holder change, written in
	// the same transaction as the append-only event it records. docs/26 §5
	// lists ownership transfer among the HIGHEST-risk audited actions
	// (beside visibility, rights, policy, merge, publish and abort/reopen)
	// and requires the audit itself to be append-only, which audit_log
	// already is by trigger (00014/00015) — so this row is the audit half
	// of docs/11 §6's "Ownership transfer 是 append-only governance event",
	// and asset_rights_holder_events is the state half.
	//
	// The name follows the dotted `<subject>.<verb-past>` convention of the
	// constants above and spells the permission-matrix action it implements
	// (change_rights_holder, specs/policies/permissions-matrix.csv:13) in
	// past tense. It deliberately does NOT reuse a research event's name:
	// the audit vocabulary is this file, the event vocabulary is
	// specs/events/event-types.yaml, and neither list is derived from the
	// other (the note beside ActionPullRequestMerged).
	ActionAssetRightsHolderChanged = "asset.rights_holder_changed"

	// T0604 scientific responsibility: one row per Research Owners rule
	// written or deleted, and one per responsibility assignment written or
	// removed, each in the same transaction as the row itself. docs/04 §3
	// makes the routing project data, so the mapping's history is the
	// audit log — the rows themselves are ordinary configuration and are
	// deleted when a rule stops applying (migration 00084). The dotted
	// names follow the `<scope>.<verb-past>` convention of
	// project.settings_updated / project.member_role_changed: the
	// responsibility surface is project configuration, and the actor who
	// changed it is the record that matters.
	ActionResearchOwnerRuleCreated = "project.research_owner_rule_created"
	ActionResearchOwnerRuleDeleted = "project.research_owner_rule_deleted"
	ActionResponsibilityAssigned   = "project.responsibility_assigned"
	ActionResponsibilityUnassigned = "project.responsibility_unassigned"
	// T0604 required-review projection: one row per PR whose required
	// reviews were met and whose state the projection advanced
	// (review_required -> approved -> merge_ready, docs/43). It is the
	// governance record that a proposal became MERGEABLE, and under which
	// reviews: the row is written in the same transaction as the state
	// move, so a PR that is merge_ready always has the audit row saying
	// when and by whose review the calculation was satisfied. The
	// remaining advances (approved -> merged, T0409) carry their own
	// records.
	ActionPullRequestReviewCompleted = "pull_request.review_completed"

	// T0601 freeze governance: one row per project whose main was frozen,
	// written in the same transaction as the flag itself. docs/26 lists
	// "main freeze" among the high-risk actions that must be audited, and
	// docs/09 §3 makes the frozen state the precondition of every later
	// advance of main — so this row is the record of when the guarantee
	// began for a project, and who turned it on.
	//
	// It spells the same dotted name as the research EVENT the freeze
	// emits (project.main_frozen, specs/events/event-types.yaml). That is
	// the same coincidence of two registries ActionPullRequestMerged
	// records above: the audit vocabulary is this file, the event
	// vocabulary is the event spec, and neither list is derived from the
	// other.
	ActionProjectMainFrozen = "project.main_frozen"

	// T0805 knowledge publication: one row per knowledge object version
	// published to the network, written in the same transaction as the
	// publication row itself. docs/12 §3 makes every widening of
	// visibility an explicit, audited human action, and a publication is
	// the knowledge surface's version of one — so this row is the record
	// of who put which version out, under which public name and which
	// rights declaration.
	//
	// It spells the same dotted name as the research EVENT the publish
	// emits (knowledge.version_published,
	// specs/events/event-types.yaml) — the coincidence of two registries
	// the two constants above already record. The published asset
	// command's action (internal/application/assetpublish
	// .ActionAssetVersionPublished) could not spell its event's name the
	// same way and had to pick a different one, because that event is
	// namespaced `research_asset.` and this vocabulary has no such
	// prefix; the knowledge event's name already has the shape this
	// vocabulary uses.
	ActionKnowledgeVersionPublished = "knowledge.version_published"

	// T0602 abort governance: one row per scientific-object version written
	// in lifecycle 'aborted', in the same transaction as the version row
	// itself. docs/46:7 requires every abort to record actor/time/reason
	// code/human explanation beside the transition, and docs/26 lists
	// abort/reopen among the highest-risk actions that must be audited —
	// so this row is the governance record that the append-only version row
	// is not: it names the request that decided the abort (via,
	// correlation_id) and it is queryable per project without walking the
	// version log.
	//
	// It spells the same dotted name as the research EVENT the abort emits
	// (scientific_object.aborted, specs/events/event-types.yaml) — the
	// coincidence of two registries ActionPullRequestMerged records above.
	// The audit vocabulary is this file, the event vocabulary is the event
	// spec, and neither list is derived from the other.
	ActionScientificObjectAborted = "scientific_object.aborted"

	// T0811 discussion promotion: one row per discussion comment promoted
	// into a proposed object (an Issue, a Hypothesis scientific object, or
	// an external-evidence proposal), written in the same transaction as
	// the promotion record it belongs to. docs/26 §5 makes the HIGHEST-risk
	// actions audited, and this one is a governance action by the ruling in
	// the task package: it creates a row in ANOTHER object's table out of a
	// conversation, under write_scientific_state, and who did it is exactly
	// what the provenance chain must answer later.
	//
	// # Why NOTHING in this file audits a comment
	//
	// A comment is not an audited action and no constant for one exists
	// here on purpose. Writing and withdrawing a comment changes no
	// scientific state (internal/domain/discussion.go), the row records its
	// own author and time, and 00104 keeps a withdrawn comment as a
	// tombstone — so the audit log would be a second copy of facts the
	// discussion tables already hold, for an action the audit vocabulary
	// (docs/26's high-risk list: authentication, governance, membership,
	// visibility, merge, publish, abort/reopen) does not name. A future
	// decision to audit comments belongs to whoever decides it is a
	// high-risk action; inventing the constant here would decide it.
	//
	// The name follows the dotted `<subject>.<verb-past>` convention of the
	// constants above. It is deliberately NOT an event name: it does not
	// appear in specs/events/event-types.yaml, and no research event is
	// emitted for a promotion (the two registries are separate — the note
	// beside ActionPullRequestMerged).
	ActionDiscussionPromoted = "discussion.promoted"
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
