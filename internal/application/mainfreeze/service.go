package mainfreeze

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
)

// MinIdempotencyKeyLen is the Idempotency-Key bound specs/api/openapi.yaml
// fixes for this route (components.parameters.IdempotencyKey: required,
// minLength 8). The command checks it too, not only the transport: the
// contract's requirements are server-side requirements, and a command
// reachable from anything but the HTTP route must hold to the same ones.
const MinIdempotencyKeyLen = 8

// Command is the freeze use case (T0601).
type Command struct {
	members  MembershipPort
	policies PolicyPort
	rules    RuleEvaluator
	store    StorePort
	authz    authz.Engine
}

// Deps carries the adapters a Command is wired over.
type Deps struct {
	// Members resolves the actor's project membership (the production
	// value is *persistence.ProjectStore).
	Members MembershipPort
	// Policies resolves the policy in force (the production value is
	// *policy.Service).
	Policies PolicyPort
	// Rules evaluates one rule of a policy document (the production value
	// is *policy.RuleEvaluator).
	Rules RuleEvaluator
	// Store is the freeze store (the production value is
	// *persistence.MainFreezeStore).
	Store StorePort
	// Authz is the permission-matrix engine (the production value is
	// authz.NewMatrixEngine()).
	Authz authz.Engine
}

// NewCommand wires the freeze command.
func NewCommand(deps Deps) *Command {
	return &Command{
		members:  deps.Members,
		policies: deps.Policies,
		rules:    deps.Rules,
		store:    deps.Store,
		authz:    deps.Authz,
	}
}

// Actor is the freezing principal: the authenticated user, and whether the
// request arrived as a platform agent rather than a human session.
//
// The flag is a field of a value the TRANSPORT builds from the principal,
// never a field of the request body: a caller that could set it could
// clear it, and the only thing the flag does is refuse.
type Actor struct {
	// User is the authenticated user. An agent acts AS a user — the
	// token's owner — so this is never empty.
	User domain.User
	// IsAgent reports whether the request arrived as a platform agent
	// (MCP/API), not as a human session.
	IsAgent bool
}

// Input is one freeze request.
type Input struct {
	// ProjectID is the project whose main is frozen.
	ProjectID string
	// IdempotencyKey is the contract-required key the request carried
	// (specs/api/openapi.yaml: required, minLength 8). The key does not
	// create a ledger — the frozen state is the idempotency record — but
	// it is required and validated so that a client cannot retry a freeze
	// by accident and cannot claim afterwards that it did not; it is
	// recorded on the audit row of the freeze it named.
	IdempotencyKey string
}

// Freeze runs the freeze and returns the project's state afterwards.
func (c *Command) Freeze(ctx context.Context, actor Actor, in Input) (Result, error) {
	if err := requireShape(in); err != nil {
		return Result{}, err
	}
	if err := c.requireAgent(actor); err != nil {
		return Result{}, err
	}
	if err := c.requireFreeze(ctx, actor.User.ID, in.ProjectID, actor.IsAgent); err != nil {
		return Result{}, err
	}
	if err := c.requirePolicy(ctx, actor.User, in.ProjectID); err != nil {
		return Result{}, err
	}
	if c.store == nil {
		return Result{}, fmt.Errorf("%w: no freeze store configured", ErrStore)
	}
	res, err := c.store.Freeze(ctx, FreezeRequest{
		ProjectID:      in.ProjectID,
		ActorID:        actor.User.ID,
		ActorIsAgent:   actor.IsAgent,
		Audit:          auditEntry(actor, in),
		IdempotencyKey: in.IdempotencyKey,
	})
	if err != nil {
		return Result{}, mapFreezeError(err)
	}
	return res, nil
}

// requireAgent is the domain backstop: an agent never freezes main,
// whatever the matrix says. It reads the actor value alone, so it is
// resolved before any lookup.
func (c *Command) requireAgent(actor Actor) error {
	if actor.IsAgent {
		return &AgentNotPermittedError{Action: "freeze_main"}
	}
	return nil
}

// requireShape validates the request before anything is read. An empty
// actor is a programming error (the transport resolves the principal
// first) and is refused here rather than written into an audit row.
func requireShape(in Input) error {
	if strings.TrimSpace(in.ProjectID) == "" {
		return fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	if in.IdempotencyKey == "" {
		return fmt.Errorf("%w: Idempotency-Key is required on a freeze: it is what makes a repeated request return the state it already produced instead of freezing twice", ErrValidation)
	}
	if len(in.IdempotencyKey) < MinIdempotencyKeyLen {
		return fmt.Errorf("%w: Idempotency-Key must be at least %d characters (specs/api/openapi.yaml)", ErrValidation, MinIdempotencyKeyLen)
	}
	return nil
}

// requireFreeze is the authorization step: the actor's class in this
// project against the permission matrix's freeze_main row
// (internal/authz/matrix.go; specs/policies/permissions-matrix.csv:10 —
// maintainer and owner allow, everyone else and an agent deny, no
// conditional cells). It resolves the membership FIRST and only then the
// decision, and it maps an unknown project to an unknown membership, so a
// denial never discloses whether the project exists (the
// releases.ErrForbidden rule).
func (c *Command) requireFreeze(ctx context.Context, actorID, projectID string, isAgent bool) error {
	if c.members == nil || c.authz == nil {
		return fmt.Errorf("%w: freeze command not fully wired", ErrStore)
	}
	membership, err := c.members.GetMembership(ctx, projectID, actorID)
	var role *domain.ProjectRole
	switch {
	case err == nil:
		r := membership.Role
		role = &r
	case errors.Is(err, projects.ErrMemberNotFound):
		// Not a member — or no such project. The two are the same
		// answer on purpose: the matrix denies the non-member class, so
		// an unknown project is refused by the same step that refuses a
		// stranger, and the caller cannot tell them apart.
		role = nil
	case errors.Is(err, projects.ErrProjectNotFound):
		role = nil
	default:
		return mapFreezeError(err)
	}
	decision, err := c.authz.Authorize(ctx, authz.Request{
		Action: authz.ActionFreezeMain,
		Class:  authz.ClassOf(true, role, isAgent),
	})
	if err != nil {
		return fmt.Errorf("%w: authorize freeze: %v", ErrStore, err)
	}
	if !decision.Permits() {
		return ErrForbidden
	}
	return nil
}

// requirePolicy is the policy-validation step docs/22 §28 requires of a
// high-risk command (docs/22_API_DESIGN.md:28 lists freeze main among the
// commands that must carry server-side authorization AND policy
// validation). It reads the policy in force — the organization's floor
// overlaid by the project's own (docs/12 §5) — and evaluates
// domain.RuleMainProtected over it.
//
// The reading, and why it differs from the merge's at one point:
//
//   - an unreadable or unevaluable policy refuses. Fail-closed, the same
//     way merge.requirePolicy refuses it: a policy that cannot be read is
//     not a permissive one.
//   - a policy that SETS main_protected to false refuses. That is the
//     policy explicitly disowning the platform's rule, and docs/09 §3
//     makes frozen main a platform guarantee no policy may waive — the
//     merge refuses the same value for the same reason. Freezing main
//     under a policy that says main is not protected would be enforcing
//     what the policy denies.
//   - a policy that SETS main_protected to true proceeds: freezing main
//     is exactly the direction that rule asks for.
//   - a policy SILENT about the rule proceeds. This is the one place the
//     freeze reads the rule differently from the merge, and the reason is
//     that the two commands move in opposite directions: the merge
//     ADVANCES main, so an absent rule cannot be read as permission to
//     advance it, while the freeze PROTECTS main, and refusing to protect
//     it because no policy spelled the rule out would fail closed the
//     wrong way — it would leave main unfrozen, which is less protected
//     than docs/09 §3 says it must be. Absence is not a denial: it is
//     silence, and the platform rule stands on its own. (It is also what
//     makes "a new project can be frozen after setup" true for a project
//     that has not had a policy written for it yet.)
func (c *Command) requirePolicy(ctx context.Context, actor domain.User, projectID string) error {
	if c.policies == nil || c.rules == nil {
		return fmt.Errorf("%w: freeze command not fully wired", ErrStore)
	}
	rule := domain.RuleMainProtected
	refuse := func(found, value bool, reason string, cause error) error {
		return &PolicyRefusedError{Rule: rule, Found: found, Bool: value, Reason: reason, Err: cause}
	}
	effective, err := c.policies.EffectivePolicy(ctx, actor, projectID)
	if err != nil {
		return refuse(false, false, "the policy in force could not be read, and an unreadable policy is not a permissive one", err)
	}
	decision, err := c.rules.Evaluate(ctx, effective.Effective, policy.Query{Rule: rule})
	if err != nil {
		return refuse(false, false, "the policy in force cannot be evaluated, and an unevaluable policy is not a permissive one", err)
	}
	if decision.Found && !decision.Bool {
		return refuse(true, false,
			"the policy in force sets "+rule+
				" to false; no policy in this build may waive the frozen-main guarantee (docs/09 §3), so the freeze refuses to enforce a rule the policy disowns", nil)
	}
	return nil
}

// auditEntry renders the audit row the WINNING freeze appends: action
// project.main_frozen, scoped to the project, with the request's
// Idempotency-Key recorded as metadata (the archive's "which request
// turned it on"), together with how the action arrived.
//
// The entry's before/after summaries state the transition the action
// makes, which is the whole of it: main was not frozen, main is frozen.
// docs/26 lists main freeze among the high-risk actions that must be
// audited, and docs/46's rule that nothing is physically removed is why
// this is an appended row on an append-only table rather than an update of
// anything.
func auditEntry(actor Actor, in Input) domain.AuditEntry {
	return domain.AuditEntry{
		ActorID:   actor.User.ID,
		Action:    domain.ActionProjectMainFrozen,
		TargetRef: "project:" + in.ProjectID,
		ProjectID: in.ProjectID,
		BeforeSummary: map[string]any{
			"main_frozen": false,
		},
		AfterSummary: map[string]any{
			"main_frozen": true,
		},
		Metadata: map[string]any{
			"idempotency_key": in.IdempotencyKey,
			// Always false when it is written — the backstop above refuses
			// an agent before the store is reached — and recorded anyway so
			// the row states the fact rather than leaving it to be inferred
			// from the absence of a refusal.
			"actor_is_agent": actor.IsAgent,
		},
	}
}

// mapFreezeError keeps the expected domain outcomes (validation, the agent
// backstop, the authorization refusal, a missing project, the policy
// refusal) and turns everything else into ErrStore for the handler, with
// the cause kept for the log.
func mapFreezeError(err error) error {
	if err == nil ||
		errors.Is(err, ErrValidation) ||
		errors.Is(err, ErrAgentNotPermitted) ||
		errors.Is(err, ErrForbidden) ||
		errors.Is(err, ErrProjectNotFound) ||
		errors.Is(err, ErrPolicyRefused) ||
		errors.Is(err, ErrStore) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}
