package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lichman0405/post/internal/application/orgs"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
)

// Service orchestrates the policy use cases. All policy lives here
// (docs/52): the transport layer only translates requests into these
// calls. The rules (docs/12 §5, T0603):
//
//   - policy is versioned: every write appends a NEW version row (the
//     table is append-only at the database itself), old versions stay
//     queryable forever;
//   - organization policy is the lower bound: a project policy may only
//     be stricter — a write that relaxes an org rule is refused with
//     ErrProjectRelaxesOrg naming every offending rule;
//   - writes are owner-only (L1, see the package doc); reads are
//     member-only;
//   - the effective policy of a project is the org policy overlaid by
//     the project policy (domain.MergeEffective) — evaluation never
//     errors on a merge, the lower bound always wins a conflict;
//   - every accepted write lands its audit_log row in the same
//     transaction as the version row (T0110's shared domain.AuditEntry).
type Service struct {
	store    PolicyStore
	orgs     OrgGate
	projects ProjectGate
	eval     Evaluator
}

// NewService wires the service on the store, the two gates and the
// evaluator. Pass nil evaluator for the V1 RuleEvaluator.
func NewService(store PolicyStore, orgs OrgGate, projects ProjectGate, eval Evaluator) *Service {
	if eval == nil {
		eval = NewRuleEvaluator()
	}
	return &Service{store: store, orgs: orgs, projects: projects, eval: eval}
}

// SetOrgPolicy appends a new organization policy version. Owner only; the
// organization must be active. The policy document is parsed and
// validated here — the wire may carry any JSON, the stored row carries
// only well-formed policy.
func (s *Service) SetOrgPolicy(ctx context.Context, actor domain.User, orgID, version string, doc json.RawMessage) (domain.PolicyVersion, error) {
	p, err := parsePolicyInput(version, doc)
	if err != nil {
		return domain.PolicyVersion{}, err
	}
	// ValidPolicyVersion accepts surrounding whitespace; store the exact
	// string validation saw, so " v1 " and "v1" are the SAME version (the
	// per-scope unique index exists to enforce that, not to keep two
	// spellings of one version apart).
	version = strings.TrimSpace(version)
	if err := s.requireOrgGovernor(ctx, actor.ID, orgID); err != nil {
		return domain.PolicyVersion{}, err
	}
	audit, err := s.policyAudit(ctx, actor, domain.PolicyScope{OrganizationID: orgID}, version, p)
	if err != nil {
		return domain.PolicyVersion{}, err
	}
	v, err := s.store.CreateOrgVersion(ctx, domain.PolicyVersion{
		Scope:     domain.PolicyScope{OrganizationID: orgID},
		Version:   version,
		Policy:    p,
		CreatedBy: actor.ID,
	}, audit)
	if err != nil {
		return domain.PolicyVersion{}, wrapStoreError(err)
	}
	return v, nil
}

// SetProjectPolicy appends a new project policy version. Owner only. The
// project policy must not relax the organization's current policy — the
// check runs twice: once against the state the service read, and once
// inside the store transaction under the organization row lock, so a
// concurrent org-policy publication can never be slipped under
// (ErrProjectRelaxesOrg names every offending rule). Personal projects
// (no organization) have no lower bound.
func (s *Service) SetProjectPolicy(ctx context.Context, actor domain.User, projectID, version string, doc json.RawMessage) (domain.PolicyVersion, error) {
	p, err := parsePolicyInput(version, doc)
	if err != nil {
		return domain.PolicyVersion{}, err
	}
	// Same as SetOrgPolicy: the stored string is the one validation saw.
	version = strings.TrimSpace(version)
	proj, err := s.requireProjectOwner(ctx, actor.ID, projectID)
	if err != nil {
		return domain.PolicyVersion{}, err
	}
	// Pre-check against the org policy as read now: the store re-checks
	// under its lock, this read only produces the early, precise error.
	if proj.OrganizationID != nil {
		latest, err := s.store.Latest(ctx, domain.PolicyScope{OrganizationID: *proj.OrganizationID})
		if err != nil && !errors.Is(err, ErrPolicyNotFound) {
			return domain.PolicyVersion{}, wrapStoreError(err)
		}
		if err == nil {
			if err := validateAgainstOrg(latest.Policy, p); err != nil {
				return domain.PolicyVersion{}, err
			}
		}
	}
	againstOrg := func(orgPolicy *domain.Policy) error {
		if orgPolicy == nil {
			return nil
		}
		return validateAgainstOrg(*orgPolicy, p)
	}
	audit, err := s.policyAudit(ctx, actor, domain.PolicyScope{ProjectID: projectID}, version, p)
	if err != nil {
		return domain.PolicyVersion{}, err
	}
	v, err := s.store.CreateProjectVersion(ctx, domain.PolicyVersion{
		Scope:     domain.PolicyScope{ProjectID: projectID},
		Version:   version,
		Policy:    p,
		CreatedBy: actor.ID,
	}, proj.OrganizationID, againstOrg, audit)
	if err != nil {
		return domain.PolicyVersion{}, wrapStoreError(err)
	}
	return v, nil
}

// GetOrgPolicy returns the organization's newest policy version. Any
// current member may read it; outsiders get the existence-hiding
// ErrOrgNotFound. ErrPolicyNotFound when the org has no policy yet.
func (s *Service) GetOrgPolicy(ctx context.Context, actor domain.User, orgID string) (domain.PolicyVersion, error) {
	if err := s.requireOrgReader(ctx, actor.ID, orgID); err != nil {
		return domain.PolicyVersion{}, err
	}
	v, err := s.store.Latest(ctx, domain.PolicyScope{OrganizationID: orgID})
	if err != nil {
		return domain.PolicyVersion{}, wrapStoreError(err)
	}
	return v, nil
}

// GetProjectPolicy returns the project's newest policy version. Any
// member may read it; outsiders get the existence-hiding
// ErrProjectNotFound. ErrPolicyNotFound when the project has none yet.
func (s *Service) GetProjectPolicy(ctx context.Context, actor domain.User, projectID string) (domain.PolicyVersion, error) {
	if err := s.requireProjectReader(ctx, actor.ID, projectID); err != nil {
		return domain.PolicyVersion{}, err
	}
	v, err := s.store.Latest(ctx, domain.PolicyScope{ProjectID: projectID})
	if err != nil {
		return domain.PolicyVersion{}, wrapStoreError(err)
	}
	return v, nil
}

// EffectivePolicy computes the project's evaluation view: the org lower
// bound, the project policy, and their merge (domain.MergeEffective).
// Any member may read it.
func (s *Service) EffectivePolicy(ctx context.Context, actor domain.User, projectID string) (domain.EffectivePolicy, error) {
	if err := s.requireProjectReader(ctx, actor.ID, projectID); err != nil {
		return domain.EffectivePolicy{}, err
	}
	proj, err := s.projects.GetProject(ctx, projectID)
	if err != nil {
		return domain.EffectivePolicy{}, wrapProjectGateError(err)
	}
	out := domain.EffectivePolicy{}
	if proj.OrganizationID != nil {
		latest, err := s.store.Latest(ctx, domain.PolicyScope{OrganizationID: *proj.OrganizationID})
		if err != nil && !errors.Is(err, ErrPolicyNotFound) {
			return domain.EffectivePolicy{}, wrapStoreError(err)
		}
		if err == nil {
			out.Org = &latest
		}
	}
	latest, err := s.store.Latest(ctx, domain.PolicyScope{ProjectID: projectID})
	if err != nil && !errors.Is(err, ErrPolicyNotFound) {
		return domain.EffectivePolicy{}, wrapStoreError(err)
	}
	if err == nil {
		out.Project = &latest
	}
	orgDoc, projectDoc := domain.EmptyPolicy(), domain.EmptyPolicy()
	if out.Org != nil {
		orgDoc = out.Org.Policy
	}
	if out.Project != nil {
		projectDoc = out.Project.Policy
	}
	out.Effective = domain.MergeEffective(orgDoc, projectDoc)
	return out, nil
}

// ListVersions returns every policy version of the scope, newest first —
// the full history (acceptance: 旧 policy version 可查询). Reads require
// membership of the scope, like every other policy read.
func (s *Service) ListVersions(ctx context.Context, actor domain.User, scope domain.PolicyScope) ([]domain.PolicyVersion, error) {
	if !scope.Valid() {
		return nil, fmt.Errorf("%w: scope must name exactly one of organization or project", ErrValidation)
	}
	if scope.OrganizationID != "" {
		if err := s.requireOrgReader(ctx, actor.ID, scope.OrganizationID); err != nil {
			return nil, err
		}
	} else {
		if err := s.requireProjectReader(ctx, actor.ID, scope.ProjectID); err != nil {
			return nil, err
		}
	}
	versions, err := s.store.List(ctx, scope)
	if err != nil {
		return nil, wrapStoreError(err)
	}
	return versions, nil
}

// GetVersion returns one stored version by id — any age. Reads require
// membership of the version's scope.
func (s *Service) GetVersion(ctx context.Context, actor domain.User, versionID string) (domain.PolicyVersion, error) {
	v, err := s.store.GetVersion(ctx, versionID)
	if err != nil {
		return domain.PolicyVersion{}, wrapStoreError(err)
	}
	if v.Scope.OrganizationID != "" {
		if err := s.requireOrgReader(ctx, actor.ID, v.Scope.OrganizationID); err != nil {
			return domain.PolicyVersion{}, err
		}
	} else {
		if err := s.requireProjectReader(ctx, actor.ID, v.Scope.ProjectID); err != nil {
			return domain.PolicyVersion{}, err
		}
	}
	return v, nil
}

// Evaluate answers one governance question against a policy document
// through the evaluate interface (Evaluator). Fail-closed: errors are
// default deny.
func (s *Service) Evaluate(ctx context.Context, p domain.Policy, q Query) (Decision, error) {
	return s.eval.Evaluate(ctx, p, q)
}

// EvaluateEffective answers one governance question against the project's
// current effective policy (org lower bound overlaid by the project
// policy). Any member may call it.
func (s *Service) EvaluateEffective(ctx context.Context, actor domain.User, projectID string, q Query) (Decision, error) {
	eff, err := s.EffectivePolicy(ctx, actor, projectID)
	if err != nil {
		return Decision{}, err
	}
	return s.eval.Evaluate(ctx, eff.Effective, q)
}

// EvaluateVersion answers one governance question against one stored
// version — the pinned-policy evaluation path releases need (T0605 binds
// the release to a policy_version_id, not to whatever is newest).
func (s *Service) EvaluateVersion(ctx context.Context, actor domain.User, versionID string, q Query) (Decision, error) {
	v, err := s.GetVersion(ctx, actor, versionID)
	if err != nil {
		return Decision{}, err
	}
	return s.eval.Evaluate(ctx, v.Policy, q)
}

// parsePolicyInput validates the version string and parses/validates the
// policy document. Both checks live here so no store ever sees a
// malformed row and no caller gets a permissive answer from malformed
// input.
func parsePolicyInput(version string, doc json.RawMessage) (domain.Policy, error) {
	if !domain.ValidPolicyVersion(version) {
		return domain.Policy{}, fmt.Errorf("%w: version must be 1..64 characters of letters, digits, '.', '_' or '-'", ErrValidation)
	}
	if len(doc) == 0 {
		return domain.Policy{}, fmt.Errorf("%w: policy document is required", ErrValidation)
	}
	p, err := domain.PolicyFromJSON(doc)
	if err != nil {
		return domain.Policy{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	return p, nil
}

// validateAgainstOrg applies the lower-bound rule and wraps every
// violation in ErrProjectRelaxesOrg (errors.Is on both the sentinel and
// domain.MergeViolations works — the violation details ride along).
func validateAgainstOrg(orgPolicy, projectPolicy domain.Policy) error {
	if err := domain.ValidateProjectPolicy(orgPolicy, projectPolicy); err != nil {
		return fmt.Errorf("%w: %w", ErrProjectRelaxesOrg, err)
	}
	return nil
}

// requireOrgGovernor: the org must exist and be active, and the actor
// must be an active owner (docs/04: owner is the org governance role).
func (s *Service) requireOrgGovernor(ctx context.Context, actorID, orgID string) error {
	o, err := s.orgs.GetOrganization(ctx, orgID)
	if err != nil {
		return wrapOrgGateError(err)
	}
	if !o.Active() {
		return ErrOrgDeactivated
	}
	m, err := s.orgs.GetMembership(ctx, orgID, actorID)
	if err != nil {
		if errors.Is(err, orgs.ErrMemberNotFound) {
			return ErrForbidden
		}
		return wrapOrgGateError(err)
	}
	if !m.Active() || !m.Role.Governs() {
		return ErrForbidden
	}
	return nil
}

// requireOrgReader: any current member may read (outsiders get the
// existence-hiding ErrOrgNotFound, mirroring the orgs surface).
func (s *Service) requireOrgReader(ctx context.Context, actorID, orgID string) error {
	if _, err := s.orgs.GetOrganization(ctx, orgID); err != nil {
		return wrapOrgGateError(err)
	}
	m, err := s.orgs.GetMembership(ctx, orgID, actorID)
	if err != nil {
		if errors.Is(err, orgs.ErrMemberNotFound) {
			return ErrOrgNotFound
		}
		return wrapOrgGateError(err)
	}
	if !m.Active() {
		return ErrOrgNotFound
	}
	return nil
}

// requireProjectOwner: the project must exist and the actor must be an
// owner (writes are owner-only — L1, see the package doc). "Not a
// member" and "role too low" answer the same ErrForbidden.
func (s *Service) requireProjectOwner(ctx context.Context, actorID, projectID string) (domain.Project, error) {
	proj, err := s.projects.GetProject(ctx, projectID)
	if err != nil {
		return domain.Project{}, wrapProjectGateError(err)
	}
	m, err := s.projects.GetMembership(ctx, projectID, actorID)
	if err != nil {
		if errors.Is(err, projects.ErrMemberNotFound) {
			return domain.Project{}, ErrForbidden
		}
		return domain.Project{}, wrapProjectGateError(err)
	}
	if m.Role != domain.ProjectRoleOwner {
		return domain.Project{}, ErrForbidden
	}
	return proj, nil
}

// requireProjectReader: any member may read; outsiders get the
// existence-hiding ErrProjectNotFound.
func (s *Service) requireProjectReader(ctx context.Context, actorID, projectID string) error {
	if _, err := s.projects.GetMembership(ctx, projectID, actorID); err != nil {
		if errors.Is(err, projects.ErrMemberNotFound) {
			return ErrProjectNotFound
		}
		return wrapProjectGateError(err)
	}
	return nil
}

// policyAudit builds the audit entry for a policy write (T0110's shared
// domain.AuditEntry — the version row and its audit row commit together).
// Via is ViaSession: every policy write arrives as a session-authenticated
// /api/v1 request. The correlation id comes from the request context; a
// context without one — a direct service call — gets a fresh id so the
// NOT NULL column always holds something traceable. A failure to mint the
// id refuses the action (fail closed: an audit-less governance write must
// not happen).
func (s *Service) policyAudit(ctx context.Context, actor domain.User, scope domain.PolicyScope, version string, p domain.Policy) (domain.AuditEntry, error) {
	correlationID, ok := observability.FromContext(ctx)
	if !ok {
		id, err := observability.NewCorrelationID()
		if err != nil {
			return domain.AuditEntry{}, fmt.Errorf("%w: cannot mint audit correlation id: %v", ErrStore, err)
		}
		correlationID = id
	}
	entry := domain.AuditEntry{
		ActorID:       actor.ID,
		Via:           domain.ViaSession,
		Action:        domain.ActionPolicyVersionSet,
		CorrelationID: correlationID.String(),
		AfterSummary: map[string]any{
			"version": version,
			"policy":  p,
		},
	}
	if scope.OrganizationID != "" {
		entry.TargetRef = "organization:" + scope.OrganizationID
		entry.OrganizationID = scope.OrganizationID
	} else {
		entry.TargetRef = "project:" + scope.ProjectID
		entry.ProjectID = scope.ProjectID
	}
	return entry, nil
}

// wrapStoreError passes the store sentinels through and wraps the rest.
func wrapStoreError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrPolicyNotFound),
		errors.Is(err, ErrVersionTaken),
		errors.Is(err, ErrOrgNotFound),
		errors.Is(err, ErrProjectNotFound),
		errors.Is(err, ErrProjectRelaxesOrg),
		errors.Is(err, ErrValidation):
		return err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}

// wrapOrgGateError maps the orgs application's sentinels onto the policy
// service's own.
func wrapOrgGateError(err error) error {
	switch {
	case errors.Is(err, orgs.ErrOrgNotFound):
		return ErrOrgNotFound
	case errors.Is(err, orgs.ErrMemberNotFound):
		return ErrForbidden
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}

// wrapProjectGateError maps the projects application's sentinels onto the
// policy service's own.
func wrapProjectGateError(err error) error {
	switch {
	case errors.Is(err, projects.ErrProjectNotFound):
		return ErrProjectNotFound
	case errors.Is(err, projects.ErrMemberNotFound):
		return ErrProjectNotFound
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}
