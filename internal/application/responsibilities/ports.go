package responsibilities

import (
	"context"

	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/diff"
)

// Ports (docs/52: application orchestrates against ports; adapters live in
// internal/persistence). Every read surface here is narrow and satisfied
// structurally by the existing production stores, so the composition root
// wires them without adapter code — except the routing store, whose writes
// are the two new canonical tables (migration 00084).

// RuleStore is the persistence port for the project's Research Owners
// routing data (docs/04 §3). The production implementation is
// persistence.ResponsibilityStore.
type RuleStore interface {
	// ListRules returns the project's routing rules; an unknown project
	// has none (empty, not an error).
	ListRules(ctx context.Context, projectID string) ([]domain.ResearchOwnerRule, error)
	// CreateRule writes one rule and its audit row in one transaction.
	// ErrRuleExists when the mapping already exists.
	CreateRule(ctx context.Context, in CreateRuleParams) (domain.ResearchOwnerRule, error)
	// DeleteRule removes the project's rule by id; the bool reports
	// whether a row was actually removed.
	DeleteRule(ctx context.Context, in DeleteRuleParams) (bool, error)
	// ListAssignments returns every responsibility assignment of the
	// project; an unknown project has none (empty, not an error).
	ListAssignments(ctx context.Context, projectID string) ([]domain.ResponsibilityAssignment, error)
	// Assign records that a user holds a label in the project. It is
	// idempotent: a repeated assignment returns the existing row and
	// writes nothing.
	Assign(ctx context.Context, in AssignParams) (domain.ResponsibilityAssignment, error)
	// Unassign removes one assignment; the bool reports whether a row was
	// actually removed.
	Unassign(ctx context.Context, in UnassignParams) (bool, error)
	// LabelsForUser returns the labels one user holds in one project,
	// sorted; an unknown project or user has none (empty, not an error).
	LabelsForUser(ctx context.Context, projectID, userID string) ([]string, error)
}

// CreateRuleParams carries one rule write. The audit entry commits in the
// same transaction as the row.
type CreateRuleParams struct {
	ProjectID      string
	MatchKind      domain.ResearchOwnerMatchKind
	MatchValue     string
	Responsibility string
	CreatedBy      string
	Audit          domain.AuditEntry
}

// DeleteRuleParams carries one rule removal.
type DeleteRuleParams struct {
	ProjectID string
	RuleID    string
	Audit     domain.AuditEntry
}

// AssignParams carries one responsibility assignment.
type AssignParams struct {
	ProjectID      string
	UserID         string
	Responsibility string
	CreatedBy      string
	Audit          domain.AuditEntry
}

// UnassignParams carries one responsibility removal.
type UnassignParams struct {
	ProjectID      string
	UserID         string
	Responsibility string
	Audit          domain.AuditEntry
}

// ProjectReader is the project row read: the owning organization (whose
// policy bounds the project's) and the visibility the public-asset rule
// is about. It is actor-free on purpose — the required-review calculation
// runs for the PROPOSAL, not for whoever happens to be looking. The
// production implementation is persistence.ProjectStore.
type ProjectReader interface {
	// GetProject returns the project or projects.ErrProjectNotFound.
	GetProject(ctx context.Context, projectID string) (domain.Project, error)
}

// MembershipGate is the role gate: the actor's membership in the project
// decides whether they may read the configuration, write it (owner-only)
// or resolve responsibilities at all (members only, existence-hiding for
// everyone else — docs/45). The production implementation is the projects
// application service, so a caller the project surface refuses to serve
// gets that surface's own not-found.
type MembershipGate interface {
	// GetMembership returns the actor's membership or
	// projects.ErrMemberNotFound.
	GetMembership(ctx context.Context, actor domain.User, projectID string) (domain.ProjectMembership, error)
}

// PRReader resolves the PR row the required-review calculation is about
// (its pinned states, its target branch and its project). The production
// implementation is persistence.PullRequestStore.
type PRReader interface {
	// GetPullRequest returns the PR of the project by its number, or the
	// pullrequests package's ErrPullRequestNotFound.
	GetPullRequest(ctx context.Context, projectID string, number int64) (domain.PullRequest, error)
}

// BranchReader resolves the PR's target branch: the released lineage is
// the project's main branch (docs/09 §3, docs/11 §1), so this is what
// decides whether a merge is a release merge. The production
// implementation is persistence.BranchStore.
type BranchReader interface {
	GetBranch(ctx context.Context, projectID, branchID string) (domain.Branch, error)
}

// Differ computes the proposal's three-way Research State Diff — the
// changed objects the routing applies to. The production implementation is
// *prdiff.Service, which already resolves the PR row, the target branch
// head and the T0401 diff engine.
type Differ interface {
	PullRequestDiff(ctx context.Context, projectID string, number int64) (*diff.Diff, error)
}

// PolicyStore is the policy read surface: the newest version of a scope
// (docs/12 §5 — the organization's bound overlaid by the project's). The
// production implementation is persistence.PolicyStore. A scope without a
// policy answers policy.ErrPolicyNotFound, which the service treats as
// the neutral empty policy — an organization without a policy constrains
// nothing, and neither does a project.
type PolicyStore interface {
	Latest(ctx context.Context, scope domain.PolicyScope) (domain.PolicyVersion, error)
}

// Evaluator answers typed governance questions from a policy document
// (policy.RuleEvaluator in production). Its contract is fail-closed: a
// rule whose stored value does not match its registered kind is an error,
// never an absent rule — so a corrupt policy refuses instead of relaxing
// (docs/12 §5).
type Evaluator interface {
	Evaluate(ctx context.Context, p domain.Policy, q policy.Query) (policy.Decision, error)
}
