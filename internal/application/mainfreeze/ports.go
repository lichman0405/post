package mainfreeze

import (
	"context"

	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/domain"
)

// Ports (docs/52: the application orchestrates against ports; adapters live
// in internal/persistence). The freeze needs exactly four things outside
// itself — the actor's membership role in the project, the governance
// policy in force, the rule evaluator, and the store that owns the freeze
// transaction.

// MembershipPort resolves the actor's membership in the project. The
// production implementation is persistence.ProjectStore; a project that
// does not exist, or that the caller is not a member of, answers
// projects.ErrMemberNotFound — which is the resolution the freeze
// authorization is built on: an unknown project is an unknown membership,
// never a disclosed absence (see Command.requireFreeze).
type MembershipPort interface {
	GetMembership(ctx context.Context, projectID, userID string) (domain.ProjectMembership, error)
}

// PolicyPort resolves the policy in force for one project: the
// organization's lower bound overlaid by the project's own (docs/12 §5).
// The production implementation is *policy.Service; it runs the project
// read gate as part of the resolution, which is why the freeze calls it
// only AFTER the authorization above has passed.
type PolicyPort interface {
	EffectivePolicy(ctx context.Context, actor domain.User, projectID string) (domain.EffectivePolicy, error)
}

// RuleEvaluator answers one typed governance question about a policy
// document. The production implementation is policy.RuleEvaluator.
type RuleEvaluator interface {
	Evaluate(ctx context.Context, p domain.Policy, q policy.Query) (policy.Decision, error)
}

// StorePort is the freeze store: the transaction that sets the flag, the
// audit row and the domain event together.
//
// There is no lookup half and no ledger: idempotency is the state itself
// (see the package doc), so the only read the command needs before the
// write is the one the store's own transaction performs to resolve the
// compare-and-swap's zero-row outcome.
type StorePort interface {
	// Freeze runs the freeze: the compare-and-swap that moves
	// projects.main_frozen from false to true, and — only for the call
	// that wins it — the audit row and the project.main_frozen domain
	// event, in ONE transaction.
	//
	// It answers *Result with AlreadyFrozen set when the project was
	// already frozen (this call wrote nothing: no audit row, no event),
	// ErrProjectNotFound when no project row exists, and the package's
	// other sentinels otherwise. A freeze that fails leaves no row of any
	// kind behind — in particular the flag is never set without its audit
	// row (docs/26) or without its event (docs/53).
	Freeze(ctx context.Context, req FreezeRequest) (Result, error)
}

// FreezeRequest is everything one freeze needs to execute: the project,
// the actor, the audit row to append when the call wins the
// compare-and-swap, and the Idempotency-Key the caller named.
//
// It carries the actor and the audit entry rather than an identity string,
// because the store writes the audit row inside its own transaction (the
// pattern every governed write in this tree follows: the audit record
// commits with the state change or not at all).
type FreezeRequest struct {
	// ProjectID is the project to freeze, in text uuid form.
	ProjectID string
	// ActorID is the authenticated user performing the freeze (text uuid).
	// It is the audit row's actor and the event's actor.
	ActorID string
	// ActorIsAgent reports whether the request arrived as a platform agent.
	// The command refuses agents before it reaches the store; the flag
	// travels with the request so the audit row records how the action
	// arrived, not only that it happened.
	ActorIsAgent bool
	// Audit is the audit row to append when this call wins the
	// compare-and-swap: action project.main_frozen, scoped to the project.
	Audit domain.AuditEntry
	// IdempotencyKey is the contract-required key the request carried. It
	// is recorded on the audit row (the request that turned the freeze on)
	// and is otherwise unused: the state is the idempotency ledger, and a
	// repeated key finds the flag already set and writes nothing.
	IdempotencyKey string
}

// Result is the outcome of a freeze that succeeded — including the outcome
// "it was already frozen", which is the same state.
type Result struct {
	// ProjectID is the project whose main is frozen.
	ProjectID string
	// MainFrozen is the project's flag after the call. It is always true:
	// a call that could not reach that state is an error, never a
	// successful-looking result.
	MainFrozen bool
	// AlreadyFrozen reports that the flag was already set when this call
	// ran, so this call changed nothing and wrote no audit row and no
	// event. The state it reports is the one an earlier freeze produced.
	AlreadyFrozen bool
}
