// Package responsibilities owns scientific responsibility and the
// Research Owners routing (T0604, docs/04 §3): the project data that says
// which responsibility label answers for which change, who holds which
// label, and the required-review calculation a proposal's reviews are
// measured against.
//
// docs/04 §3 in two sentences, both normative here:
//
//	「不与 Access Role 混合。项目可配置：Experimental Reviewer、
//	  Computational Reviewer、Data Reviewer、Project Lead、IP Reviewer
//	  等责任标签。责任用于 Review routing，不自动赋予更高访问权限。」
//	「类似 CODEOWNERS 的 Research Owners 规则可按对象类型/Schema/领域
//	  匹配 reviewer。」
//
// Two consequences shape this package:
//
//   - The labels and the rules are PROJECT DATA (tables in migration
//     00084), not a vocabulary in code. A project writes its own labels —
//     the docs' list is examples ("等") — and its own mapping from a
//     change to the responsible reviewer.
//
//   - Responsibility grants NO access. Nothing in this package consults
//     internal/authz, and internal/authz consults nothing here. A viewer
//     who holds "Data Reviewer" is still a viewer: the label resolves the
//     conditional submit_scientific_review verdict (docs/04 §2's matrix
//     row) and attributes the review to the responsibility it was signed
//     under — nothing else. The proofs live in the tests
//     (responsibility_never_widens_authz over the full matrix).
//
// Everything is fail-closed (docs/12 §5): a change no rule routes is a
// change nobody answers for, so the required-review calculation makes the
// proposal unsatisfiable rather than requiring nothing; a missing policy,
// a blank label and an unevaluable rule all refuse. Absence is never a
// permission — the same discipline T0601 applied to the frozen-main flag.
package responsibilities

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/rsg/diff"
)

// Service orchestrates the responsibility use cases: the owner-gated
// configuration writes, the member-gated reads, the resolver the review
// permission's conditional verdict hangs on, and the required-review
// calculation the PR projection evaluates.
type Service struct {
	rules     RuleStore
	projects  ProjectReader
	members   MembershipGate
	prs       PRReader
	branches  BranchReader
	diffs     Differ
	policies  PolicyStore
	evaluator Evaluator
}

// Deps carries the adapters the service composes. Production wiring is in
// cmd/api/main.go: every port is a real adapter, and none of the writes
// reaches a table internal/authz reads.
type Deps struct {
	Rules RuleStore
	// Projects reads the project row; Members is the role gate. They are
	// two ports because they answer two different questions (what the
	// project is / what this actor may do here) and the production
	// implementations differ.
	Projects  ProjectReader
	Members   MembershipGate
	PRs       PRReader
	Branches  BranchReader
	Diffs     Differ
	Policies  PolicyStore
	Evaluator Evaluator
}

// NewService wires the service.
func NewService(deps Deps) *Service {
	return &Service{
		rules:     deps.Rules,
		projects:  deps.Projects,
		members:   deps.Members,
		prs:       deps.PRs,
		branches:  deps.Branches,
		diffs:     deps.Diffs,
		policies:  deps.Policies,
		evaluator: deps.Evaluator,
	}
}

// AddRuleInput is one Research Owners rule as the caller supplies it (the
// project comes from the path, the author from the session — never from
// the request body).
type AddRuleInput struct {
	// MatchKind is how the rule matches a change: object_type, schema or
	// domain (docs/04 §3).
	MatchKind domain.ResearchOwnerMatchKind
	// MatchValue is the matched value: an object type, a schema id or a
	// domain.
	MatchValue string
	// Responsibility is the label the matched changes are routed to.
	Responsibility string
}

// ListRules returns the project's routing rules (docs/04 §3). Any member
// may read the project's configuration; an outsider gets the
// existence-hiding project-not-found (docs/45).
func (s *Service) ListRules(ctx context.Context, actor domain.User, projectID string) ([]domain.ResearchOwnerRule, error) {
	if err := s.requireMember(ctx, actor, projectID); err != nil {
		return nil, err
	}
	rules, err := s.rules.ListRules(ctx, projectID)
	if err != nil {
		return nil, wrapStoreError(err)
	}
	return rules, nil
}

// AddRule writes one routing rule. Owner-only (L1, the same gate the
// project policy and member-role surfaces use): the routing decides who
// must review what, which is governance configuration.
func (s *Service) AddRule(ctx context.Context, actor domain.User, projectID string, in AddRuleInput) (domain.ResearchOwnerRule, error) {
	if err := s.requireOwner(ctx, actor, projectID); err != nil {
		return domain.ResearchOwnerRule{}, err
	}
	if err := validateRule(in); err != nil {
		return domain.ResearchOwnerRule{}, err
	}
	audit, err := s.audit(ctx, actor, projectID, domain.ActionResearchOwnerRuleCreated, map[string]any{
		"match_kind":     string(in.MatchKind),
		"match_value":    in.MatchValue,
		"responsibility": in.Responsibility,
	})
	if err != nil {
		return domain.ResearchOwnerRule{}, err
	}
	rule, err := s.rules.CreateRule(ctx, CreateRuleParams{
		ProjectID:      projectID,
		MatchKind:      in.MatchKind,
		MatchValue:     in.MatchValue,
		Responsibility: in.Responsibility,
		CreatedBy:      actor.ID,
		Audit:          audit,
	})
	if err != nil {
		return domain.ResearchOwnerRule{}, wrapStoreError(err)
	}
	return rule, nil
}

// RemoveRule deletes one routing rule (owner-only). The bool reports
// whether a rule was removed; the removal is audited only when it
// happened.
func (s *Service) RemoveRule(ctx context.Context, actor domain.User, projectID, ruleID string) (bool, error) {
	if err := s.requireOwner(ctx, actor, projectID); err != nil {
		return false, err
	}
	if ruleID == "" {
		return false, fmt.Errorf("%w: rule_id is required", ErrValidation)
	}
	audit, err := s.audit(ctx, actor, projectID, domain.ActionResearchOwnerRuleDeleted, map[string]any{"rule_id": ruleID})
	if err != nil {
		return false, err
	}
	removed, err := s.rules.DeleteRule(ctx, DeleteRuleParams{ProjectID: projectID, RuleID: ruleID, Audit: audit})
	if err != nil {
		return false, wrapStoreError(err)
	}
	return removed, nil
}

// ListAssignments returns every responsibility assignment of the project
// (member read).
func (s *Service) ListAssignments(ctx context.Context, actor domain.User, projectID string) ([]domain.ResponsibilityAssignment, error) {
	if err := s.requireMember(ctx, actor, projectID); err != nil {
		return nil, err
	}
	assignments, err := s.rules.ListAssignments(ctx, projectID)
	if err != nil {
		return nil, wrapStoreError(err)
	}
	return assignments, nil
}

// Assign records that userID holds the responsibility label in the project
// (owner-only). It is idempotent: assigning what already holds returns the
// existing assignment and writes nothing, so a repeated request cannot
// inflate the record.
func (s *Service) Assign(ctx context.Context, actor domain.User, projectID, userID, responsibility string) (domain.ResponsibilityAssignment, error) {
	if err := s.requireOwner(ctx, actor, projectID); err != nil {
		return domain.ResponsibilityAssignment{}, err
	}
	if userID == "" {
		return domain.ResponsibilityAssignment{}, fmt.Errorf("%w: user_id is required", ErrValidation)
	}
	label := strings.TrimSpace(responsibility)
	if !domain.ValidResponsibilityLabel(label) {
		return domain.ResponsibilityAssignment{}, fmt.Errorf("%w: responsibility must be 1..%d characters", ErrValidation, domain.MaxResponsibilityLabelLen)
	}
	audit, err := s.audit(ctx, actor, projectID, domain.ActionResponsibilityAssigned, map[string]any{
		"user_id":        userID,
		"responsibility": label,
	})
	if err != nil {
		return domain.ResponsibilityAssignment{}, err
	}
	assignment, err := s.rules.Assign(ctx, AssignParams{
		ProjectID:      projectID,
		UserID:         userID,
		Responsibility: label,
		CreatedBy:      actor.ID,
		Audit:          audit,
	})
	if err != nil {
		return domain.ResponsibilityAssignment{}, wrapStoreError(err)
	}
	return assignment, nil
}

// Unassign removes one responsibility assignment (owner-only).
func (s *Service) Unassign(ctx context.Context, actor domain.User, projectID, userID, responsibility string) (bool, error) {
	if err := s.requireOwner(ctx, actor, projectID); err != nil {
		return false, err
	}
	label := strings.TrimSpace(responsibility)
	if userID == "" || label == "" {
		return false, fmt.Errorf("%w: user_id and responsibility are required", ErrValidation)
	}
	audit, err := s.audit(ctx, actor, projectID, domain.ActionResponsibilityUnassigned, map[string]any{
		"user_id":        userID,
		"responsibility": label,
	})
	if err != nil {
		return false, err
	}
	removed, err := s.rules.Unassign(ctx, UnassignParams{
		ProjectID:      projectID,
		UserID:         userID,
		Responsibility: label,
		Audit:          audit,
	})
	if err != nil {
		return false, wrapStoreError(err)
	}
	return removed, nil
}

// Responsibilities is the reviewer-responsibility resolver: the labels the
// reviewing actor holds in this project, sorted (docs/04 §3). It is the
// one thing the conditional submit_scientific_review verdict hangs on —
// "may this actor review here?" — and it answers with labels the ACTOR
// holds, never with a verdict about access: the caller (the reviews
// service) already resolved the matrix row, and an empty answer means the
// condition is unmet, not that access is decided here.
//
// The caller must be a member: an outsider gets the existence-hiding
// project-not-found (docs/45) — the same answer the membership gate gives,
// so a non-member cannot use the resolver to probe a project.
func (s *Service) Responsibilities(ctx context.Context, actor domain.User, projectID string) ([]string, error) {
	if err := s.requireMember(ctx, actor, projectID); err != nil {
		return nil, err
	}
	labels, err := s.rules.LabelsForUser(ctx, projectID, actor.ID)
	if err != nil {
		return nil, wrapStoreError(err)
	}
	return labels, nil
}

// RequiredReviews computes the required-review calculation for one
// proposal (domain.BuildRequiredReviews): the changes the proposal
// carries, each routed through the project's Research Owners rules, plus
// the two policy rules the calculation consumes. It is the calculation the
// review projection evaluates and the one a caller renders to explain why
// a PR is not mergeable.
//
// Every input is resolved server-side from the PR row and the project's
// configuration; nothing is caller-supplied. Fail-closed throughout:
//
//   - an unreadable diff, rules, project or policy is an error (the caller
//     refuses — an unknown requirement set is not an empty one);
//   - a policy rule whose stored value does not match its registered kind
//     is an error (the evaluator's contract), never an absent rule;
//   - a change no rule routes stays unrouted, which makes the calculation
//     unsatisfiable rather than trivially satisfied.
func (s *Service) RequiredReviews(ctx context.Context, projectID string, number int64) (domain.RequiredReviews, error) {
	if projectID == "" {
		return domain.RequiredReviews{}, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	if number < 1 {
		return domain.RequiredReviews{}, fmt.Errorf("%w: number must be positive", ErrValidation)
	}
	if s.prs == nil || s.diffs == nil || s.rules == nil || s.projects == nil || s.members == nil || s.evaluator == nil {
		return domain.RequiredReviews{}, fmt.Errorf("%w: the required-review calculation is not wired", ErrStore)
	}
	pr, err := s.prs.GetPullRequest(ctx, projectID, number)
	if err != nil {
		return domain.RequiredReviews{}, wrapStoreError(err)
	}
	project, err := s.projects.GetProject(ctx, projectID)
	if err != nil {
		return domain.RequiredReviews{}, wrapStoreError(err)
	}
	d, err := s.diffs.PullRequestDiff(ctx, projectID, number)
	if err != nil {
		return domain.RequiredReviews{}, wrapStoreError(err)
	}
	rules, err := s.rules.ListRules(ctx, projectID)
	if err != nil {
		return domain.RequiredReviews{}, wrapStoreError(err)
	}
	releaseMerge, err := s.isReleaseMerge(ctx, pr)
	if err != nil {
		return domain.RequiredReviews{}, err
	}
	effective, err := s.effectivePolicy(ctx, project)
	if err != nil {
		return domain.RequiredReviews{}, err
	}
	minReviewers, err := s.intRule(ctx, effective, domain.RuleReleaseMinReviewers)
	if err != nil {
		return domain.RequiredReviews{}, err
	}
	ipReview, err := s.boolRule(ctx, effective, domain.RulePublicAssetIPReview)
	if err != nil {
		return domain.RequiredReviews{}, err
	}
	return domain.BuildRequiredReviews(domain.RequiredReviewInput{
		Subjects:     Subjects(d.ObjectChanges),
		Rules:        rules,
		HeadStateID:  pr.ProposedStateID,
		ReleaseMerge: releaseMerge,
		MinApprovals: minReviewers,
		// The IP-review rule is about PUBLIC assets (docs/12 §5): it is
		// read against the project's visibility, and a private project's
		// changes are not public assets. Both must hold — the policy's
		// requirement and the visibility it is about.
		PublicAssetIPReview: ipReview && project.Visibility == domain.VisibilityPublic,
	}), nil
}

// Subjects renders the diff's object changes as routing subjects: what a
// rule matches (docs/04 §3) is the object's type, the schema the proposed
// version pins and the domain its payload declares. Relations are not
// routed in V1 (the docs' three match kinds are object properties), and a
// proposal whose changes are all relations therefore has no routed
// requirement at all — the calculation reports it unsatisfiable, which is
// the fail-closed reading of "nobody is answerable for this change".
func Subjects(changes []diff.ObjectChange) []domain.ReviewSubject {
	out := make([]domain.ReviewSubject, 0, len(changes))
	for _, c := range changes {
		out = append(out, domain.ReviewSubject{
			ObjectID:   c.ObjectID,
			ObjectType: c.ObjectType,
			SchemaID:   c.SourceVersion.SchemaRef.ID,
			Domain:     payloadDomain(c.SourceVersion.Payload),
		})
	}
	return out
}

// payloadDomain reads the payload's `domain` member — the subject-area
// field docs/04 §3 names as 「领域」 (the protocol schema's domain
// property). A payload that is not a JSON object, has no such member, or
// carries a non-string or empty one has no domain: an absent field matches
// no domain rule, it is never a wildcard.
func payloadDomain(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var doc struct {
		Domain *string `json:"domain"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		return ""
	}
	if doc.Domain == nil {
		return ""
	}
	return strings.TrimSpace(*doc.Domain)
}

// isReleaseMerge reports whether the PR targets the released lineage: the
// project's main branch, the state a release is cut from (docs/09 §3,
// docs/11 §1). This is what makes `release_min_reviewers` apply — a
// proposal into a feature branch is not a release merge, and the rule is
// not read for it.
func (s *Service) isReleaseMerge(ctx context.Context, pr domain.PullRequest) (bool, error) {
	if s.branches == nil {
		return false, fmt.Errorf("%w: the target branch cannot be read", ErrStore)
	}
	branch, err := s.branches.GetBranch(ctx, pr.ProjectID, pr.TargetBranchID)
	if err != nil {
		return false, wrapStoreError(err)
	}
	return branch.IsMain(), nil
}

// effectivePolicy composes the policy in force (docs/12 §5): the
// organization's newest version overlaid by the project's, with the org as
// the lower bound (domain.MergeEffective). A scope without a policy is the
// neutral empty policy, not an error — an organization that has written no
// rules constrains nothing, and so does a project.
//
// It reads the versions directly instead of going through the policy
// service's actor-gated EffectivePolicy: the calculation runs for the
// PROPOSAL, not for the reviewing actor (the reviewer may legitimately not
// be able to read the project's policy surface), and docs/12 §5's
// composition is a pure function of the two stored versions.
func (s *Service) effectivePolicy(ctx context.Context, project domain.Project) (domain.Policy, error) {
	if s.policies == nil {
		return domain.Policy{}, fmt.Errorf("%w: the policy cannot be read", ErrStore)
	}
	org := domain.EmptyPolicy()
	if project.OrganizationID != nil {
		v, err := s.policies.Latest(ctx, domain.PolicyScope{OrganizationID: *project.OrganizationID})
		switch {
		case err == nil:
			org = v.Policy
		case errors.Is(err, policy.ErrPolicyNotFound):
			// No org policy: the neutral lower bound.
		default:
			return domain.Policy{}, fmt.Errorf("%w: read the organization policy", ErrStore)
		}
	}
	proj := domain.EmptyPolicy()
	v, err := s.policies.Latest(ctx, domain.PolicyScope{ProjectID: project.ID})
	switch {
	case err == nil:
		proj = v.Policy
	case errors.Is(err, policy.ErrPolicyNotFound):
		// No project policy: the org policy alone is in force.
	default:
		return domain.Policy{}, fmt.Errorf("%w: read the project policy", ErrStore)
	}
	return domain.MergeEffective(org, proj), nil
}

// intRule evaluates an int rule. An absent rule answers 0 (the caller's
// safe default: no extra requirement comes from a rule nobody wrote); an
// unevaluable one is an error — corrupt state refuses the whole
// calculation rather than reading as absent (docs/12 §5).
func (s *Service) intRule(ctx context.Context, p domain.Policy, rule string) (int, error) {
	decision, err := s.evaluator.Evaluate(ctx, p, policy.Query{Rule: rule})
	if err != nil {
		return 0, fmt.Errorf("%w: rule %q cannot be evaluated: %v", ErrStore, rule, err)
	}
	if !decision.Found {
		return 0, nil
	}
	if decision.Int < 0 {
		return 0, fmt.Errorf("%w: rule %q is negative and cannot be satisfied", ErrStore, rule)
	}
	return decision.Int, nil
}

// boolRule evaluates a bool rule; absent answers false (see intRule).
func (s *Service) boolRule(ctx context.Context, p domain.Policy, rule string) (bool, error) {
	decision, err := s.evaluator.Evaluate(ctx, p, policy.Query{Rule: rule})
	if err != nil {
		return false, fmt.Errorf("%w: rule %q cannot be evaluated: %v", ErrStore, rule, err)
	}
	return decision.Found && decision.Bool, nil
}

// validateRule checks one rule write: the match kind is one of the three
// documented kinds, the match value is 1..200 characters and already
// trimmed (a value with invisible whitespace matches nothing and would
// silently stop routing), and the label is non-blank and bounded.
func validateRule(in AddRuleInput) error {
	if !domain.ValidResearchOwnerMatchKind(in.MatchKind) {
		return fmt.Errorf("%w: match_kind must be object_type, schema or domain (docs/04 §3)", ErrValidation)
	}
	if !domain.ValidResearchOwnerMatchValue(in.MatchValue) {
		return fmt.Errorf("%w: match_value must be 1..%d characters, trimmed", ErrValidation, domain.MaxResponsibilityLabelLen)
	}
	if !domain.ValidResponsibilityLabel(in.Responsibility) {
		return fmt.Errorf("%w: responsibility must be 1..%d characters", ErrValidation, domain.MaxResponsibilityLabelLen)
	}
	return nil
}

// requireOwner: the project must be visible to the actor and the actor
// must be an owner. A project the actor may not read answers the
// existence-hiding project-not-found — never "forbidden", which would
// confirm that a private project exists (docs/45). A readable project
// without the owner role answers ErrForbidden: for a project the actor can
// already see, their role is not a secret.
func (s *Service) requireOwner(ctx context.Context, actor domain.User, projectID string) error {
	if _, err := s.visibleProject(ctx, actor, projectID); err != nil {
		return err
	}
	membership, err := s.members.GetMembership(ctx, actor, projectID)
	if err != nil {
		switch {
		case errors.Is(err, projects.ErrProjectNotFound):
			return ErrProjectNotFound
		case errors.Is(err, projects.ErrMemberNotFound):
			return ErrForbidden
		default:
			return wrapStoreError(err)
		}
	}
	if membership.Role != domain.ProjectRoleOwner {
		return ErrForbidden
	}
	return nil
}

// requireMember: any member may read; outsiders get ErrProjectNotFound —
// including the private-project case, where the membership gate answers the
// existence-hiding not-found itself rather than a missing membership row.
func (s *Service) requireMember(ctx context.Context, actor domain.User, projectID string) error {
	if _, err := s.visibleProject(ctx, actor, projectID); err != nil {
		return err
	}
	if _, err := s.members.GetMembership(ctx, actor, projectID); err != nil {
		if errors.Is(err, projects.ErrMemberNotFound) || errors.Is(err, projects.ErrProjectNotFound) {
			return ErrProjectNotFound
		}
		return wrapStoreError(err)
	}
	return nil
}

// visibleProject resolves the project the actor is asking about. An actor
// with no identity cannot see one: the anonymous case answers the
// existence-hiding not-found, never a role question.
func (s *Service) visibleProject(ctx context.Context, actor domain.User, projectID string) (domain.Project, error) {
	if s.projects == nil || s.members == nil {
		return domain.Project{}, fmt.Errorf("%w: no project gate configured", ErrStore)
	}
	if actor.ID == "" {
		return domain.Project{}, ErrProjectNotFound
	}
	if projectID == "" {
		return domain.Project{}, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	project, err := s.projects.GetProject(ctx, projectID)
	if err != nil {
		return domain.Project{}, wrapStoreError(err)
	}
	return project, nil
}

// audit builds the audit entry for one responsibility write. Via is
// ViaSession: the writes arrive as session-authenticated /api/v1 requests.
// The correlation id comes from the request context; a context without one
// gets a fresh id, so the NOT NULL column always holds something traceable
// — and a failure to mint one refuses the action (an unrecorded
// governance write must not happen).
func (s *Service) audit(ctx context.Context, actor domain.User, projectID, action string, summary map[string]any) (domain.AuditEntry, error) {
	correlationID, ok := observability.FromContext(ctx)
	if !ok {
		id, err := observability.NewCorrelationID()
		if err != nil {
			return domain.AuditEntry{}, fmt.Errorf("%w: cannot mint audit correlation id: %v", ErrStore, err)
		}
		correlationID = id
	}
	return domain.AuditEntry{
		ActorID:       actor.ID,
		Via:           domain.ViaSession,
		Action:        action,
		TargetRef:     "project:" + projectID,
		ProjectID:     projectID,
		CorrelationID: correlationID.String(),
		AfterSummary:  summary,
	}, nil
}

// wrapStoreError passes the expected domain outcomes through and turns
// everything else — including a dependency that cannot run — into ErrStore
// with the cause kept for the log.
func wrapStoreError(err error) error {
	if err == nil ||
		errors.Is(err, ErrValidation) ||
		errors.Is(err, ErrForbidden) ||
		errors.Is(err, ErrProjectNotFound) ||
		errors.Is(err, ErrUserNotFound) ||
		errors.Is(err, ErrRuleExists) ||
		errors.Is(err, ErrStore) ||
		errors.Is(err, pullrequests.ErrPullRequestNotFound) ||
		errors.Is(err, projects.ErrProjectNotFound) ||
		errors.Is(err, policy.ErrPolicyNotFound) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}
