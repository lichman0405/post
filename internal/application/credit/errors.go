package credit

import "errors"

// Sentinel errors: one per distinct OUTCOME a transport has to be able to
// answer differently (docs/45 — stable outcomes, never dependency detail).
// Everything else is ErrStore.
//
// The order they are listed in is the order the commands resolve them in,
// and that order is the contract: the agent backstop and the shape check
// precede every read, and the authorization precedes every lookup of the
// credit, the target or the dispute (see Command.DeclareAttribution).
var (
	// ErrValidation: the request is not a credit operation at all — no
	// project, a target ref that is not a canonical "kind:value" ref of a
	// kind docs/13 §2 names, an empty claim, a resolution that is not one
	// of the two decided states, a party kind outside the two identity
	// kinds, a party id that is not a uuid, a credit role outside the two
	// the schema admits, or an evidence ref that is not a ledger ref. The
	// refusal names the field; it never restates the request's content.
	ErrValidation = errors.New("credit: validation failed")
	// ErrAgentNotPermitted: an agent actor attempted a credit operation.
	// This is the domain backstop, resolved from the actor value before
	// any lookup. docs/60 §2 lists credit dispute resolution among the
	// actions that must be human-governed, and the permission matrix's
	// credit-carrying row has proposal_or_scoped in its agent column — so
	// an agent is refused twice over, exactly as a publish or a
	// rights-holder change is (the same two lines of defence).
	ErrAgentNotPermitted = errors.New("credit: agents may not declare credit or decide a credit dispute")
	// ErrForbidden: the actor's class does not permit the re-used matrix
	// row at the level this operation requires — docs/13 §3 gives raising
	// a dispute to a contributor and deciding it to maintainer/owner
	// governance. Resolved before any credit, target or dispute lookup, so
	// the denial never discloses whether the project, the target or the
	// dispute exists.
	ErrForbidden = errors.New("credit: the actor may not perform this credit operation")
	// ErrProjectNotFound: no project row exists for the given id, or the
	// project is not visible to the caller (existence hiding, docs/45).
	ErrProjectNotFound = errors.New("credit: project not found")
	// ErrTargetNotFound: the target ref names no row of the table its kind
	// names, or it names one that does not belong to the project the
	// request named. Both are answered the same way and neither is
	// silently repaired: a credit is declared, and a dispute raised,
	// against a target the caller has already established, never against
	// one adopted by guessing. This is also the refusal for a target the
	// caller's authorization was not resolved against — a caller
	// authorized in project A must not be able to write credit for an
	// asset, release or finding of project B by naming A.
	ErrTargetNotFound = errors.New("credit: target not found")
	// ErrPartyNotFound: a party a declaration names does not exist — no
	// user row for kind user, no organization row for kind organization. A
	// stored party that resolves to nothing would be a dangling reference
	// in an append-only declaration, which is the one kind of row a reader
	// can never repair.
	ErrPartyNotFound = errors.New("credit: a declared party was not found")
	// ErrDisputeNotFound: the request named a dispute that does not exist
	// in the project (or belongs to another project, which is answered
	// identically).
	ErrDisputeNotFound = errors.New("credit: dispute not found")
	// ErrDisputeClosed: the dispute is no longer open. Refused rather than
	// re-decided: docs/13 §3's dispute is decided once, a later claim is a
	// new dispute, and 00103's guard refuses the same write in the
	// database whatever the caller.
	ErrDisputeClosed = errors.New("credit: the dispute is already closed")
	// ErrStore: a persistence adapter failed, or the data it returned
	// cannot be rendered (cause kept for the log).
	ErrStore = errors.New("credit: store failure")
)

// Wire codes (docs/45): one per failure shape, so a client branches
// without parsing messages. The transport that lands later maps these;
// they are declared here so the vocabulary is fixed with the outcomes.
const (
	// CodeValidationFailed: the request is not a credit operation.
	CodeValidationFailed = "VALIDATION_FAILED"
	// CodeForbidden: the actor's class does not permit the operation.
	CodeForbidden = "FORBIDDEN"
	// CodeAgentNotPermitted: an agent attempted the operation.
	CodeAgentNotPermitted = "AGENT_NOT_PERMITTED"
	// CodeProjectNotFound: the project is unknown or invisible.
	CodeProjectNotFound = "PROJECT_NOT_FOUND"
	// CodeTargetNotFound: the target ref names nothing in this project.
	CodeTargetNotFound = "TARGET_NOT_FOUND"
	// CodePartyNotFound: a declared party names no user or organization.
	CodePartyNotFound = "PARTY_NOT_FOUND"
	// CodeDisputeNotFound: the dispute is unknown in this project.
	CodeDisputeNotFound = "DISPUTE_NOT_FOUND"
	// CodeDisputeClosed: the dispute is already decided.
	CodeDisputeClosed = "DISPUTE_CLOSED"
	// CodeStoreFailure: everything else.
	CodeStoreFailure = "STORE_FAILURE"
)

// AgentNotPermittedError is the typed backstop refusal, carrying the
// operation it refused. It is the shape assetpublish.AgentNotPermittedError
// and assetrights.AgentNotPermittedError have, for the same reason: the
// refusal is about the ACTOR, and the caller (a transport, or a test
// asserting the backstop) can name it without parsing a message.
type AgentNotPermittedError struct {
	// Operation is the credit operation the agent attempted
	// ("declare_attribution", "open_dispute", "close_dispute").
	Operation string
}

func (e *AgentNotPermittedError) Error() string {
	return "credit: agents may not perform " + e.Operation
}

// Unwrap makes errors.Is(err, ErrAgentNotPermitted) true, so the two
// spellings of one outcome agree.
func (e *AgentNotPermittedError) Unwrap() error { return ErrAgentNotPermitted }
