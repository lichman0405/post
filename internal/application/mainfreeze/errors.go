package mainfreeze

import (
	"errors"
	"fmt"
)

// Sentinel errors the command maps for its consumers (docs/45: stable
// outcomes, never dependency detail). Each one is a distinct OUTCOME the
// transport has to be able to answer differently; everything else is
// ErrStore.
var (
	// ErrValidation: the request is not a freeze request at all — an empty
	// project id, a missing or too-short Idempotency-Key. The refusal
	// names the field; it never restates the request's content.
	ErrValidation = errors.New("mainfreeze: validation failed")
	// ErrAgentNotPermitted: an agent actor attempted the freeze. This is
	// the domain backstop, resolved before any lookup — see
	// AgentNotPermittedError and the package doc's two-lines-of-defence
	// note.
	ErrAgentNotPermitted = errors.New("mainfreeze: agents may not freeze main")
	// ErrForbidden: the actor's class does not permit
	// authz.ActionFreezeMain. Resolved before any target lookup, so the
	// denial never discloses whether the project exists (the
	// releases.ErrForbidden rule) — which is why an unknown project id
	// answers this and not "project not found".
	ErrForbidden = errors.New("mainfreeze: the actor may not freeze main here")
	// ErrProjectNotFound: no project row exists for the given id. It is
	// reachable only through paths that already resolved a membership (a
	// member of a project that stopped existing between the two reads) —
	// the authorization above hides existence from everyone else.
	ErrProjectNotFound = errors.New("mainfreeze: project not found")
	// ErrPolicyRefused: the governance policy in force does not permit the
	// freeze — see PolicyRefusedError.
	ErrPolicyRefused = errors.New("mainfreeze: the policy in force does not permit freezing main")
	// ErrStore: a persistence adapter failed (cause kept for the log).
	ErrStore = errors.New("mainfreeze: store failure")
)

// Wire codes (docs/45): one code per failure shape, so a client branches
// without parsing messages. The authorization refusal uses the error
// model's own AUTH_FORBIDDEN; the policy refusal uses the code that names
// the policy action's effect.
const (
	// CodeValidationFailed: the request is not a freeze request.
	CodeValidationFailed = "VALIDATION_FAILED"
	// CodeForbidden: the actor's class does not permit freeze_main.
	CodeForbidden = "AUTH_FORBIDDEN"
	// CodeAgentFreezeDenied: an agent actor attempted the freeze. A code of
	// its own, although the matrix denies agents too: the two refusals come
	// from different places (the domain backstop versus the matrix cell) and
	// a test that cannot tell them apart cannot show that the backstop is an
	// independent second line.
	CodeAgentFreezeDenied = "MAIN_FREEZE_AGENT_DENIED"
	// CodeProjectNotFound: the project does not exist. Reachable only after
	// a successful authorization (see ErrProjectNotFound).
	CodeProjectNotFound = "PROJECT_NOT_FOUND"
	// CodePolicyRefused: the policy in force blocks the freeze (docs/45's
	// own RIGHTS_POLICY_BLOCKS_ACTION is the code for "the policy refused a
	// governance action"; here the rule is main_protected).
	CodePolicyRefused = "RIGHTS_POLICY_BLOCKS_ACTION"
	// CodeServiceUnavailable: the freeze data is temporarily unavailable.
	CodeServiceUnavailable = "SERVICE_UNAVAILABLE"
)

// AgentNotPermittedError reports a freeze refused because the actor is an
// agent. It is the domain's own refusal, not the matrix's: freezing main
// is a human governance action (docs/09 §3 — the freeze is what makes
// main protected), and it is refused from the actor value alone, before
// any lookup. Shape copied from internal/application/contribution's and
// internal/application/assetpublish's AgentNotPermittedError, which refuse
// the same kind of action for the same reason.
type AgentNotPermittedError struct {
	// Action is the refused action's wire name ("freeze_main").
	Action string
}

// Error implements error.
func (e *AgentNotPermittedError) Error() string {
	return fmt.Sprintf("mainfreeze: agents cannot %s — freezing main is a human governance action (docs/09 §3)", e.Action)
}

// Unwrap keeps errors.Is(err, ErrAgentNotPermitted) working through the
// wrap.
func (e *AgentNotPermittedError) Unwrap() error { return ErrAgentNotPermitted }

// Code is the stable wire code of this outcome (docs/45).
func (e *AgentNotPermittedError) Code() string { return CodeAgentFreezeDenied }

// PolicyRefusedError reports that the governance policy in force does not
// permit the freeze. Shape copied from merge.PolicyRefusedError and
// assetpublish.PolicyRefusedError, which refuse over the same machinery
// (policy.Service.EffectivePolicy — the organization's floor overlaid by
// the project's own, docs/12 §5 — plus the rule evaluator).
//
// The rule is domain.RuleMainProtected, and it is read the way the merge
// reads it: absent, false, unreadable and unevaluable all refuse. That is
// deliberate and is the strict half of docs/12 §5 — the project policy may
// be stricter than the org's, never silently looser — but see the freeze
// command's own note: freezing main is the SUPPORTED direction of
// main_protected, so a policy that sets the rule true is the one a freeze
// is compatible with, and it is a policy that DISABLES the platform rule
// (main_protected: false) that no freeze may proceed under.
type PolicyRefusedError struct {
	// Rule is the rule key the refusal is about (domain.RuleMainProtected).
	Rule string
	// Found reports whether the policy set the rule at all.
	Found bool
	// Bool is the rule's value when Found.
	Bool bool
	// Reason is the human-readable line naming which case fired.
	Reason string
	// Err is the underlying failure when the policy could not be read.
	Err error
}

// Error implements error.
func (e *PolicyRefusedError) Error() string {
	return "mainfreeze: the policy in force does not permit freezing main — " + e.Reason
}

// Unwrap keeps errors.Is(err, ErrPolicyRefused) working, and carries the
// store failure when the refusal was a failed read.
func (e *PolicyRefusedError) Unwrap() error {
	if e.Err != nil {
		return fmt.Errorf("%w: %w", ErrPolicyRefused, e.Err)
	}
	return ErrPolicyRefused
}

// Code is the stable wire code of this outcome (docs/45).
func (e *PolicyRefusedError) Code() string { return CodePolicyRefused }
