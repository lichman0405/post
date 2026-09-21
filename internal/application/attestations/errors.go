package attestations

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinels. Every refusal the command makes is one of these, so the
// transport maps a code without re-deriving why.
var (
	// ErrValidation: the request is malformed or names a value outside the
	// published vocabulary. It is resolved before anything is read.
	ErrValidation = errors.New("attestations: invalid request")
	// ErrAgentNotPermitted: the request arrived as a platform agent. An
	// attestation is a human governance action (docs/23 §4: private→public
	// is the platform's highest-risk operation and requires a human
	// explicit action), so an agent token never issues one — whatever the
	// matrix says.
	ErrAgentNotPermitted = errors.New("attestations: agents cannot attest")
	// ErrForbidden: the actor's class is not admitted by the permission
	// matrix for this action.
	ErrForbidden = errors.New("attestations: not permitted")
	// ErrProjectNotFound: the attesting project does not exist, or the
	// caller may not read it. The two answer the same thing on purpose —
	// the existence of a private project is itself private (docs/45).
	ErrProjectNotFound = errors.New("attestations: project not found")
	// ErrTargetNotFound: the request names no target the attester could
	// attest to. One sentinel for "no such version" and "a version this
	// project may not attest", which the command deliberately does not
	// distinguish.
	ErrTargetNotFound = errors.New("attestations: target not found")
	// ErrBasisNotFound: the request names no basis state of the attesting
	// project.
	ErrBasisNotFound = errors.New("attestations: basis state not found")
	// ErrReviewNotFound: the request names no internal review of the
	// attesting project's own basis state.
	ErrReviewNotFound = errors.New("attestations: internal review not found")
	// ErrRefused: the shape is well formed and the actor is permitted, but
	// the attestation is not admissible. *Refused carries the reasons.
	ErrRefused = errors.New("attestations: refused")
	// ErrStore: the store itself failed (dependency down, driver error).
	ErrStore = errors.New("attestations: store failure")
)

// Wire codes (docs/45: stable codes, no dependency detail).
const (
	CodeValidationFailed     = "VALIDATION_FAILED"
	CodeAgentAttestDenied    = "ATTESTATION_AGENT_DENIED"
	CodeForbidden            = "ATTESTATION_FORBIDDEN"
	CodeProjectNotFound      = "ATTESTATION_PROJECT_NOT_FOUND"
	CodeTargetNotFound       = "ATTESTATION_TARGET_NOT_FOUND"
	CodeBasisStateNotFound   = "ATTESTATION_BASIS_STATE_NOT_FOUND"
	CodeInternalReviewNeeded = "ATTESTATION_INTERNAL_REVIEW_REQUIRED"
	CodeAttestationRefused   = "ATTESTATION_REFUSED"
	CodeServiceUnavailable   = "SERVICE_UNAVAILABLE"
	CodeAttestationNotFound  = "ATTESTATION_NOT_FOUND"
)

// AgentNotPermittedError is the refusal an agent request gets. It is a type
// rather than a bare sentinel so the message can name the action without
// the transport spelling it, the way knowledgepublish does for the same
// backstop.
type AgentNotPermittedError struct {
	// Action is the refused action's wire name ("attest").
	Action string
}

func (e *AgentNotPermittedError) Error() string {
	return fmt.Sprintf("attestations: agents cannot %s — attesting is a human governance action (docs/23 §4, docs/12 §3)", e.Action)
}

// Unwrap makes errors.Is(err, ErrAgentNotPermitted) true.
func (e *AgentNotPermittedError) Unwrap() error { return ErrAgentNotPermitted }

// Code implements the wire-code interface the transport reads.
func (e *AgentNotPermittedError) Code() string { return CodeAgentAttestDenied }

// Refused is the refusal a well-formed request gets when the attestation is
// not admissible: the target is not public, is not a Protocol/Claim/Asset,
// or the recorded attribution the caller asked for is wider than the
// organization's standing setting.
//
// Reasons is what the caller reads. Preview is the record it was decided
// on, so a caller can see the facts that produced the refusal rather than a
// sentence about them.
type Refused struct {
	// Preview is the attestation preview the refusal was decided on.
	Preview Preview
	// Reasons names, one line each, the entries that blocked.
	Reasons []string
}

func (e *Refused) Error() string {
	msg := ErrRefused.Error()
	if len(e.Reasons) > 0 {
		msg += ": " + strings.Join(e.Reasons, "; ")
	}
	return msg
}

// Unwrap makes errors.Is(err, ErrRefused) true.
func (e *Refused) Unwrap() error { return ErrRefused }

// Code implements the wire-code interface the transport reads.
func (e *Refused) Code() string { return CodeAttestationRefused }
