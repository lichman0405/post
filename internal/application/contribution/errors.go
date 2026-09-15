package contribution

import (
	"errors"
	"fmt"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/contribution"
)

// Sentinel errors the service maps for the transport layer (docs/45: the
// wire carries codes, never dependency detail). The domain-package
// sentinels (internal/contribution) pass through as-is — they are this
// use case's outcomes.
var (
	// ErrValidation: an input fails the domain shape rules (an unknown
	// target type/difficulty/state, an oversized text, a malformed id,
	// ...). Internal/contribution.ErrValidation is aliased through the
	// same outcome below.
	ErrValidation = errors.New("contribution: validation failed")
	// ErrStore: the persistence adapter failed (cause kept for the log).
	ErrStore = errors.New("contribution: store failure")
)

// Wire codes (docs/45). Outcomes shared with the domain package carry
// the same code strings there, so one outcome has one stable wire name
// whichever package reports it.
const (
	CodeOpportunityNotFound  = "OPPORTUNITY_NOT_FOUND"
	CodeTargetNotFound       = "OPPORTUNITY_TARGET_NOT_FOUND"
	CodeTargetAlreadyActive  = "OPPORTUNITY_TARGET_ALREADY_ACTIVE"
	CodeInvalidTransition    = "OPPORTUNITY_INVALID_TRANSITION"
	CodeStateConflict        = "OPPORTUNITY_STATE_CONFLICT"
	CodeTerminal             = "OPPORTUNITY_TERMINAL"
	CodeNotPublicizable      = "OPPORTUNITY_NOT_PUBLICIZABLE"
	CodePublicizedFrozen     = "OPPORTUNITY_PUBLICIZED_FROZEN"
	CodeAgentMarkDenied      = "OPPORTUNITY_AGENT_MARK_DENIED"
	CodeAgentApproveDenied   = "OPPORTUNITY_AGENT_APPROVE_DENIED"
	CodeAgentPublicizeDenied = "OPPORTUNITY_AGENT_PUBLICIZE_DENIED"
	CodeValidation           = "VALIDATION_FAILED"
	CodeUnavailable          = "SERVICE_UNAVAILABLE"
)

// AgentNotPermittedError reports a governance action refused for an
// agent actor: mark, approve or publicize. An agent's only path into the
// opportunity model is Suggest; approval and visibility widening are
// human maintainer decisions (docs/12 §3: Agent/MCP 不得自动执行此类扩大
// 操作).
type AgentNotPermittedError struct {
	// Action is the refused action's wire name ("mark", "approve",
	// "publicize").
	Action string
}

// Error implements error.
func (e *AgentNotPermittedError) Error() string {
	return fmt.Sprintf("contribution: agents cannot %s a contribution opportunity — suggestions require maintainer approval and publicizing is a human governance action (docs/12 §3)", e.Action)
}

// Code is the stable wire code of this outcome (one per action, so the
// transport can tell mark/approve/publicize refusals apart).
func (e *AgentNotPermittedError) Code() string {
	switch e.Action {
	case "mark":
		return CodeAgentMarkDenied
	case "approve":
		return CodeAgentApproveDenied
	default:
		return CodeAgentPublicizeDenied
	}
}

// TransitionError reports a lifecycle move the machine does not allow —
// it cannot be skipped (suggested → suggested) and closed is terminal.
// The same map is enforced by migration 00062 for any database write
// path.
type TransitionError struct {
	// ID is the opportunity the transition was attempted on.
	ID string
	// From is the state the opportunity is in.
	From contribution.OpportunityState
	// To is the state the transition was attempted to.
	To contribution.OpportunityState
}

// Error implements error.
func (e *TransitionError) Error() string {
	return fmt.Sprintf("contribution: opportunity %s cannot move %s -> %s (suggested -> open/closed; open -> closed; closed is terminal)",
		e.ID, e.From, e.To)
}

// Code is the stable wire code of this outcome (docs/45).
func (e *TransitionError) Code() string { return CodeInvalidTransition }

// wrapStoreError keeps the expected domain outcomes (missing
// opportunity/target, duplicate active target, invalid transition,
// CAS conflict, terminal, not-publicizable, frozen, validation,
// project missing) and turns everything else — including an adapter
// that cannot run — into ErrStore for the handler, with the cause kept
// for the log.
func wrapStoreError(err error) error {
	if err == nil ||
		errors.Is(err, contribution.ErrOpportunityNotFound) ||
		errors.Is(err, contribution.ErrTargetNotFound) ||
		errors.Is(err, contribution.ErrTargetAlreadyActive) ||
		errors.Is(err, contribution.ErrPublicizedFrozen) ||
		errors.Is(err, contribution.ErrValidation) ||
		errors.Is(err, projects.ErrProjectNotFound) ||
		errors.As(err, new(*contribution.PublicizeError)) ||
		errors.As(err, new(*contribution.StateConflictError)) ||
		errors.As(err, new(*contribution.TerminalError)) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}
