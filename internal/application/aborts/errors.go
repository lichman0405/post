package aborts

import (
	"errors"
	"fmt"
)

// Sentinel errors the service maps for the transport layer (docs/45: the
// wire carries codes, never dependency detail).
var (
	// ErrValidation: an input fails the domain shape rules (a missing
	// project/object, a reference that is not a canonical uuid, a reason
	// code outside its token shape, an empty or oversized explanation, a
	// missing or too-short Idempotency-Key).
	ErrValidation = errors.New("aborts: validation failed")
	// ErrObjectNotFound: no such object in this project — an unknown id, or
	// an object of another project (same outcome, no foreign existence
	// leak, docs/45).
	ErrObjectNotFound = errors.New("aborts: scientific object not found")
	// ErrVersionNotFound: the named version does not exist, or belongs to
	// another object (same outcome). Also the outcome for a version that
	// cannot be the subject of an abort — one that is not the object's
	// current version, or one whose lifecycle is already terminal for this
	// command (aborted, superseded).
	ErrVersionNotFound = errors.New("aborts: object version not found or not aborted-able")
	// ErrNotMainObject: the object exists in the project but its current
	// version is not on the project's main line, so docs/46:9's
	// branch → PR → merge rule has no subject here. Reported rather than
	// silently proposing an abort of something main does not carry.
	ErrNotMainObject = errors.New("aborts: the object's current version is not on the project's main line")
	// ErrForbidden: the actor's class does not permit ActionAbortMainObject
	// — resolved before any target lookup, so the denial never discloses
	// whether the project or object exists (the releases.ErrForbidden rule).
	ErrForbidden = errors.New("aborts: the actor may not abort a main object here")
	// ErrAgentNotPermitted: the domain backstop refused an agent actor
	// before any lookup. Its own sentinel so the wire can say WHICH line
	// stopped it.
	ErrAgentNotPermitted = errors.New("aborts: an agent may not abort a main object")
	// ErrConflict: the object's version log moved underneath the request
	// and the key it carried names nothing, so the request neither won nor
	// replayed. Retryable by re-reading.
	ErrConflict = errors.New("aborts: the object's version log moved; re-read and retry")
	// ErrIdempotencyKeyInUse: the request's Idempotency-Key already names a
	// proposal in this project, and that proposal is not this object's.
	//
	// The two key indexes disagree by design: the abort's own key index is
	// per OBJECT (infra/migrations/00100's
	// scientific_object_versions (object_id, abort_request_key)), while the
	// Research PR's creation key is indexed per PROJECT
	// (infra/migrations/00089's pull_requests (project_id, creation_key)).
	// One key sent for two objects in one project therefore finds a row in
	// the project-wide index that the first object's abort opened, and the
	// adapter ANSWERS with it rather than refusing (the creation replay in
	// internal/persistence/pullrequest_store.go). Answering the second
	// object's abort with the first object's proposal would report an abort
	// as proposed when nothing proposes it, so the command refuses instead.
	ErrIdempotencyKeyInUse = errors.New("aborts: this Idempotency-Key already names a proposal in this project")
	// ErrStore: a dependency failed (cause kept for the log).
	ErrStore = errors.New("aborts: store failure")
)

// Wire codes (docs/45). Outcomes shared with other packages carry the same
// code strings there, so one outcome has one stable wire name whichever
// package reports it.
const (
	// CodeValidationFailed: the request is not an abort request.
	CodeValidationFailed = "VALIDATION_FAILED"
	// CodeObjectNotFound: no such object in this project (existence
	// hiding).
	CodeObjectNotFound = "OBJECT_NOT_FOUND"
	// CodeVersionNotFound: the named version does not name an abortable
	// version of that object.
	CodeVersionNotFound = "OBJECT_VERSION_NOT_FOUND"
	// CodeNotMainObject: the object is not on the main line.
	CodeNotMainObject = "OBJECT_NOT_ON_MAIN"
	// CodeForbidden: the actor's class does not permit abort_main_object.
	// Every class the matrix denies answers with this one code — a
	// stranger, a viewer, a contributor, and a caller naming a project
	// that does not exist are indistinguishable from the outside.
	CodeForbidden = "AUTH_FORBIDDEN"
	// CodeAgentDenied: an agent actor was refused by the domain backstop.
	// Distinct from CodeForbidden on purpose: the caller learns that
	// authorization stopped it, and which of the two lines did.
	CodeAgentDenied = "OBJECT_ABORT_AGENT_DENIED"
	// CodeConflict: this request neither won nor replayed — the version log
	// moved underneath it, or the key it carried already names a proposal
	// in this project that this request did not open (the conflict class's
	// two outcomes; the caller re-reads and retries either way).
	CodeConflict = "OBJECT_ABORT_CONFLICT"
	// CodeServiceUnavailable: the abort data is temporarily unavailable.
	CodeServiceUnavailable = "SERVICE_UNAVAILABLE"
)

// AgentNotPermittedError reports the domain backstop's refusal: an agent
// actor attempted an abort. It wraps ErrAgentNotPermitted so both
// errors.Is(err, ErrAgentNotPermitted) and errors.As(err,
// *AgentNotPermittedError) hold, the shape
// internal/application/contribution's AgentNotPermittedError established.
type AgentNotPermittedError struct {
	// Action is the refused action's wire name ("abort_main_object").
	Action string
}

// Error implements error.
func (e *AgentNotPermittedError) Error() string {
	return fmt.Sprintf("aborts: an agent may not %s — an abort of a main object is a human governance decision, and the matrix's proposal_only cell is resolved as a refusal here (docs/46:9, docs/12 §3)", e.Action)
}

// Unwrap reports the sentinel this outcome always is, so a caller can test
// either spelling.
func (e *AgentNotPermittedError) Unwrap() error { return ErrAgentNotPermitted }

// Code is the stable wire code of this outcome (docs/45).
func (e *AgentNotPermittedError) Code() string { return CodeAgentDenied }

// IdempotencyKeyInUseError reports the refusal of a request whose
// Idempotency-Key already names a proposal in this project that is not this
// object's (see ErrIdempotencyKeyInUse for why the per-project creation key
// index and the per-object abort key index disagree).
//
// It is coded (docs/45) rather than bare so the wire can carry the conflict
// class AND a sentence that is true of this outcome: the version log did not
// move, a key did. The sentence is deliberately silent about what holds the
// key — no object, no version, no proposal number — so the refusal does not
// turn a held key into a read of somebody else's record; the caller already
// knows which key they sent.
type IdempotencyKeyInUseError struct{}

// Error implements error.
func (e *IdempotencyKeyInUseError) Error() string {
	return "aborts: this Idempotency-Key already names a proposal in this project, and that proposal is not this request's; retry with a new key"
}

// Unwrap reports the sentinel this outcome always is, so a caller can test
// either spelling — and, in the command itself, so the fresh path can tell
// this refusal from the ones a replay may answer (a held key is NOT
// replayable: the replay read would find this request's own version and
// answer 201 for a request that was refused).
func (e *IdempotencyKeyInUseError) Unwrap() error { return ErrIdempotencyKeyInUse }

// Code is the stable wire code of this outcome (docs/45): the conflict
// class, the same code the moved-log conflict carries.
func (e *IdempotencyKeyInUseError) Code() string { return CodeConflict }
