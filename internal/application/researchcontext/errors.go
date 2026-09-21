package researchcontext

import "errors"

// Error codes on the wire (docs/45: stable codes, one per failure shape).
// The vocabulary the error model fixes is used where it exists
// (IDEMPOTENCY_CONFLICT, VALIDATION_FAILED, SERVICE_UNAVAILABLE); the codes
// below it does not list name outcomes this surface alone has.
const (
	// CodeSearchNotOwned: the search record exists and is not the caller's.
	// specs/api/openapi.yaml lists it under the start route's 409 ("The
	// search record is not the caller's, or was already started").
	//
	// It is a 409 and not a 404 on purpose: the caller named a real search,
	// and the answer "that is not yours" is the truth the contract chose to
	// publish. What it must NOT do is leak the record — no query, no ref, no
	// actor is echoed back, and the response body is the standard envelope.
	CodeSearchNotOwned = "SEARCH_NOT_OWNED"
	// CodeIdempotencyConflict: an Idempotency-Key names something other than
	// this request (docs/45's own code). On the start route: the key already
	// started a different search. On the confirm route: the key already
	// confirmed a different draft.
	CodeIdempotencyConflict = "IDEMPOTENCY_CONFLICT"
	// CodeAlreadyStarted: this search already has a draft, created under a
	// DIFFERENT key. The other half of the contract's 409 for the start
	// route: the caller is not replaying anything, so answering with the
	// existing draft would hand back a project they did not ask for under a
	// key they did not use.
	CodeAlreadyStarted = "RESEARCH_CONTEXT_ALREADY_STARTED"
	// CodeDraftNotFound: no such draft — or one the caller may not see. The
	// same answer for both (existence hiding, docs/45): the confirm route is
	// restricted to the project's maintainers, and a caller who is not one
	// must not be able to tell a draft that exists from one that does not.
	CodeDraftNotFound = "RESEARCH_CONTEXT_DRAFT_NOT_FOUND"
	// CodeNotConfirmable: the draft is not in a state that can be confirmed
	// (contract, 409). In V1 there are two ways to be in one: the draft is
	// already confirmed and the request carries a different key, or the
	// project it belongs to already has a main branch carrying research state
	// this draft did not make (Service.Confirm's ProjectState read: a
	// confirmation records the state it formed, and that branch is not it).
	CodeNotConfirmable = "RESEARCH_CONTEXT_DRAFT_NOT_CONFIRMABLE"
	// CodeValidationFailed: the request is not one of these commands as
	// given (a missing key, a blank question, an unknown visibility).
	CodeValidationFailed = "VALIDATION_FAILED"
	// CodeServiceUnavailable: a dependency failed, or the data it returned
	// cannot be used. The cause is logged, never returned.
	CodeServiceUnavailable = "SERVICE_UNAVAILABLE"
)

// Sentinel errors the service maps for its consumers. Callers test with
// errors.Is — never by string comparison. The project-creation outcomes are
// NOT repeated here: they are internal/application/projects' own sentinels
// (ErrSlugTaken, ErrForbidden, ErrValidation, ErrOrgNotFound), passed through
// wrapped so the transport answers the same code the project surface answers
// for the same condition.
var (
	// ErrValidation: the request is not a start/confirm candidate — a missing
	// or too-short Idempotency-Key, a blank name or slug, a research question
	// shorter than the schema's minimum, an unknown visibility.
	ErrValidation = errors.New("researchcontext: invalid request")
	// ErrSearchNotFound: the id names no search record. The transport answers
	// it as the contract's 409 for the start route (the contract lists no
	// 404 there): the caller named something that is not an answered search.
	ErrSearchNotFound = errors.New("researchcontext: search record not found")
	// ErrSearchNotOwned: the search record belongs to another actor.
	ErrSearchNotOwned = errors.New("researchcontext: the search record belongs to another actor")
	// ErrAlreadyStarted: the search already has a draft, made under a
	// different key.
	ErrAlreadyStarted = errors.New("researchcontext: this search already started a draft research context")
	// ErrIdempotencyConflict: an Idempotency-Key already names a different
	// draft (start) or a different confirmation (confirm).
	ErrIdempotencyConflict = errors.New("researchcontext: this Idempotency-Key was used for a different request")
	// ErrDraftNotFound: no such draft, or a draft the caller may not see —
	// "no permission" and "cannot see" answer the same, so this is the single
	// error both produce.
	ErrDraftNotFound = errors.New("researchcontext: draft not found")
	// ErrNotConfirmable: the draft is not in a state that can be confirmed —
	// already confirmed under a different key, or the project already carries
	// a state this draft did not make (see CodeNotConfirmable).
	ErrNotConfirmable = errors.New("researchcontext: the draft is not in a state that can be confirmed")
	// ErrUngroundedRef: a ref the draft carries was not returned by the
	// search it was built from. It is its own sentinel because the caller can
	// fix it (drop the ref) and because it is the refusal that keeps a draft
	// from naming a source the answer never had. The value carries the ref.
	ErrUngroundedRef = errors.New("researchcontext: a draft ref was not returned by the search")
	// ErrStore: a persistence adapter, the RSG write path or the authorizer
	// failed (cause kept for the log).
	ErrStore = errors.New("researchcontext: store failure")
)

// UngroundedRefError names the ref that was refused, for the caller's
// sentence and the operator's log. The ref itself is the SEARCH's own
// selected ref (the caller asked for it by sending it), so naming it
// discloses nothing the caller did not already have.
type UngroundedRefError struct {
	// Ref is the offending ref, verbatim from the request.
	Ref string
}

func (e *UngroundedRefError) Error() string {
	return "researchcontext: ref " + e.Ref + " was not returned by the search this draft comes from"
}

// Is reports the sentinel, so errors.Is(err, ErrUngroundedRef) holds for
// every instance and callers never match on the message.
func (e *UngroundedRefError) Is(target error) bool { return target == ErrUngroundedRef }
