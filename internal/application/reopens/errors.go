package reopens

import (
	"errors"
	"fmt"
)

// Sentinel errors the service maps for the transport layer (docs/45: the
// wire carries codes, never dependency detail).
var (
	// ErrValidation: an input fails the domain shape rules (a missing
	// project/object, a reference that is not a canonical uuid, a reason code
	// outside its token shape, an empty or oversized explanation, a missing
	// or too-short Idempotency-Key).
	ErrValidation = errors.New("reopens: validation failed")
	// ErrObjectNotFound: no such object in this project — an unknown id, or
	// an object of another project (same outcome, no foreign existence leak,
	// docs/45).
	ErrObjectNotFound = errors.New("reopens: scientific object not found")
	// ErrVersionNotFound: the named version does not exist, or belongs to
	// another object (same outcome). Also the outcome for a version that
	// cannot be the subject of a reopen — one that is not the object's
	// current version, or one whose lifecycle is not 'aborted'.
	//
	// The lifecycle rule is where docs/43:10's "active → aborted → reopened →
	// active" is enforced, and it is ONE check because both directions of the
	// state machine the criterion names fall out of it:
	//
	//   - an object that was never aborted has a current version in lifecycle
	//     'active', so it is refused here;
	//   - an object already reopened has a current version in lifecycle
	//     'reopened', so a second reopen is refused here.
	//
	// The only lifecycle a reopen admits is 'aborted', and the only version it
	// admits is the log's head: a reopen of an OLDER aborted version would
	// leave the object's current state where it is and produce a 'reopened' row
	// that reopens nothing.
	ErrVersionNotFound = errors.New("reopens: object version not found or not reopenable")
	// ErrNotMainObject: the object exists in the project but its current
	// version is not on the project's main line, so docs/09:9-10's "even the
	// Owner may only go through a PR merge" has no subject here. Reported
	// rather than silently proposing a reopen of something main does not
	// carry.
	ErrNotMainObject = errors.New("reopens: the object's current version is not on the project's main line")
	// ErrForbidden: the actor's class does not permit ActionReopenMainObject
	// — resolved before any target lookup, so the denial never discloses
	// whether the project or object exists (the releases.ErrForbidden rule).
	ErrForbidden = errors.New("reopens: the actor may not reopen a main object here")
	// ErrAgentNotPermitted: the domain backstop refused an agent actor before
	// any lookup. Its own sentinel so the wire can say WHICH line stopped it.
	ErrAgentNotPermitted = errors.New("reopens: an agent may not reopen a main object")
	// ErrConflict: the object's version log moved underneath the request and
	// the key it carried names nothing, so the request neither won nor
	// replayed. Retryable by re-reading.
	ErrConflict = errors.New("reopens: the object's version log moved; re-read and retry")
	// ErrIdempotencyKeyInUse: the request's Idempotency-Key already names a
	// proposal in this project, and that proposal is not this object's.
	//
	// The two key indexes disagree by design: the reopen's own key index is
	// per OBJECT (infra/migrations/00123's
	// scientific_object_versions (object_id, reopen_request_key)), while the
	// Research PR's creation key is indexed per PROJECT
	// (infra/migrations/00089's pull_requests (project_id, creation_key)) —
	// and that per-project index is SHARED with the abort command's
	// proposals. One key sent for two objects in one project therefore finds
	// a row in the project-wide index that some earlier request opened, and
	// the adapter ANSWERS with it rather than refusing (the creation replay
	// in internal/persistence/pullrequest_store.go). Answering this object's
	// reopen with another object's proposal would report a reopen as proposed
	// when nothing proposes it, so the command refuses instead.
	ErrIdempotencyKeyInUse = errors.New("reopens: this Idempotency-Key already names a proposal in this project")
	// ErrStore: a dependency failed (cause kept for the log).
	ErrStore = errors.New("reopens: store failure")
)

// Wire codes (docs/45). Outcomes shared with other packages carry the same
// code strings there, so one outcome has one stable wire name whichever
// package reports it; the two outcomes that are this command's own carry
// reopen-specific names, exactly as the abort command's do.
const (
	// CodeValidationFailed: the request is not a reopen request.
	CodeValidationFailed = "VALIDATION_FAILED"
	// CodeObjectNotFound: no such object in this project (existence hiding).
	CodeObjectNotFound = "OBJECT_NOT_FOUND"
	// CodeVersionNotFound: the named version does not name a reopenable
	// version of that object.
	CodeVersionNotFound = "OBJECT_VERSION_NOT_FOUND"
	// CodeNotMainObject: the object is not on the main line.
	CodeNotMainObject = "OBJECT_NOT_ON_MAIN"
	// CodeForbidden: the actor's class does not permit reopen_main_object.
	// Every class the matrix denies answers with this one code — a stranger,
	// a viewer, a contributor, and a caller naming a project that does not
	// exist are indistinguishable from the outside.
	CodeForbidden = "AUTH_FORBIDDEN"
	// CodeAgentDenied: an agent actor was refused by the domain backstop.
	// Distinct from CodeForbidden on purpose: the caller learns that
	// authorization stopped it, and which of the two lines did.
	CodeAgentDenied = "OBJECT_REOPEN_AGENT_DENIED"
	// CodeConflict: this request neither won nor replayed — the version log
	// moved underneath it, or the key it carried already names a proposal in
	// this project that this request did not open (the conflict class's two
	// outcomes; the caller re-reads and retries either way).
	CodeConflict = "OBJECT_REOPEN_CONFLICT"
	// CodeServiceUnavailable: the reopen data is temporarily unavailable.
	CodeServiceUnavailable = "SERVICE_UNAVAILABLE"
)

// AgentNotPermittedError reports the domain backstop's refusal: an agent
// actor attempted a reopen. It wraps ErrAgentNotPermitted so both
// errors.Is(err, ErrAgentNotPermitted) and errors.As(err,
// *AgentNotPermittedError) hold.
type AgentNotPermittedError struct {
	// Action is the refused action's wire name ("reopen_main_object").
	Action string
}

// Error implements error.
func (e *AgentNotPermittedError) Error() string {
	return fmt.Sprintf("reopens: an agent may not %s — a reopen of a main object is a human governance decision, and the matrix's proposal_only cell is resolved as a refusal here (docs/43:10, docs/46:11, docs/12 §3)", e.Action)
}

// Unwrap reports the sentinel this outcome always is, so a caller can test
// either spelling.
func (e *AgentNotPermittedError) Unwrap() error { return ErrAgentNotPermitted }

// Code is the stable wire code of this outcome (docs/45).
func (e *AgentNotPermittedError) Code() string { return CodeAgentDenied }

// IdempotencyKeyInUseError reports the refusal of a request whose
// Idempotency-Key already names a proposal in this project that is not this
// object's (see ErrIdempotencyKeyInUse for why the per-project creation key
// index and the per-object reopen key index disagree).
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
	return "reopens: this Idempotency-Key already names a proposal in this project, and that proposal is not this request's; retry with a new key"
}

// Unwrap reports the sentinel this outcome always is, so a caller can test
// either spelling — and, in the command itself, so the fresh path can tell
// this refusal from the ones a replay may answer (a held key is NOT
// replayable: the replay read would find this request's own version and
// answer 201 for a request that was refused).
func (e *IdempotencyKeyInUseError) Unwrap() error { return ErrIdempotencyKeyInUse }

// Code is the stable wire code of this outcome (docs/45): the conflict class,
// the same code the moved-log conflict carries.
func (e *IdempotencyKeyInUseError) Code() string { return CodeConflict }
