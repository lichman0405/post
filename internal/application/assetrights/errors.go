package assetrights

import "errors"

// Sentinel errors: one per distinct OUTCOME a transport has to be able to
// answer differently (docs/45 — stable outcomes, never dependency
// detail). Everything else is ErrStore.
//
// The order they are listed in is the order the command resolves them in,
// and that order is the contract: the agent backstop and the shape check
// precede every read, and the authorization precedes every lookup of the
// asset (see Command.ChangeRightsHolder).
var (
	// ErrValidation: the request is not a rights-holder change at all — no
	// project, a pid that is not a pid, a holder kind outside the two
	// identity kinds, or a holder id that is not a uuid. The refusal names
	// the field; it never restates the request's content.
	ErrValidation = errors.New("assetrights: validation failed")
	// ErrAgentNotPermitted: an agent actor attempted the change. This is
	// the domain backstop, resolved from the actor value before any
	// lookup. specs/mcp/tools.json lists change_rights_holder among
	// forbidden_default_agent_actions (with merge_main and
	// publish_private_to_public), and the permission matrix's agent column
	// is deny — so an agent is refused twice over, exactly as a publish is
	// (internal/application/assetpublish, the same two lines of defence).
	ErrAgentNotPermitted = errors.New("assetrights: agents may not change an asset's rights holder")
	// ErrForbidden: the actor's class does not permit
	// authz.ActionChangeRightsHolder — specs/policies/permissions-matrix.csv:13
	// admits owner and refuses every other column, including maintainer.
	// Resolved before any asset lookup, so the denial never discloses
	// whether the project or the asset exists (the releases.ErrForbidden
	// rule the publish command states).
	ErrForbidden = errors.New("assetrights: the actor may not change this asset's rights holder")
	// ErrProjectNotFound: no project row exists for the given id, or the
	// project is not visible to the caller (existence hiding, docs/45).
	ErrProjectNotFound = errors.New("assetrights: project not found")
	// ErrAssetNotFound: the request named a pid that names no stored
	// asset, or one whose origin project is not the project the request
	// named. Both are answered the same way and neither is silently
	// repaired: a change is proposed against an asset the caller already
	// holds, never against one adopted by guessing.
	ErrAssetNotFound = errors.New("assetrights: asset not found")
	// ErrPartyNotFound: the named holder does not exist — no user row for
	// kind user, no organization row for kind organization. A stored
	// holder that resolves to nothing would be a dangling reference in an
	// append-only record, which is the one kind of row a reader can never
	// repair.
	ErrPartyNotFound = errors.New("assetrights: the named rights holder not found")
	// ErrNoChange: the named party already holds the asset. Refused rather
	// than appended: an event whose before and after are the same party is
	// not a transfer, and appending one would put a row in the chain that
	// says nothing happened — the chain is the record of who holds the
	// asset and how that changed (00082's CHECK refuses the same row
	// again in the database).
	ErrNoChange = errors.New("assetrights: that party already holds this asset")
	// ErrStore: a persistence adapter failed, or the data it returned
	// cannot be rendered (cause kept for the log).
	ErrStore = errors.New("assetrights: store failure")
)

// Wire codes (docs/45): one per failure shape, so a client branches
// without parsing messages. The transport that lands later maps these;
// they are declared here so the vocabulary is fixed with the outcomes.
const (
	// CodeValidationFailed: the request is not a rights-holder change.
	CodeValidationFailed = "VALIDATION_FAILED"
	// CodeForbidden: the actor's class does not permit the action.
	CodeForbidden = "FORBIDDEN"
	// CodeAgentNotPermitted: an agent attempted the action.
	CodeAgentNotPermitted = "AGENT_NOT_PERMITTED"
	// CodeProjectNotFound: the project is unknown or invisible.
	CodeProjectNotFound = "PROJECT_NOT_FOUND"
	// CodeAssetNotFound: the pid names no asset of that project.
	CodeAssetNotFound = "ASSET_NOT_FOUND"
	// CodePartyNotFound: the named holder names no user or organization.
	CodePartyNotFound = "RIGHTS_HOLDER_NOT_FOUND"
	// CodeNoChange: the named party already holds the asset.
	CodeNoChange = "RIGHTS_HOLDER_UNCHANGED"
	// CodeStoreFailure: everything else.
	CodeStoreFailure = "STORE_FAILURE"
)

// AgentNotPermittedError is the typed backstop refusal, carrying the
// action it refused. It is the shape assetpublish.AgentNotPermittedError
// has, for the same reason: the refusal is about the ACTOR, and the caller
// (a transport, or a test asserting the backstop) can name it without
// parsing a message.
type AgentNotPermittedError struct {
	// Action is the action the agent attempted ("change_rights_holder").
	Action string
}

func (e *AgentNotPermittedError) Error() string {
	return "assetrights: agents may not perform " + e.Action
}

// Unwrap makes errors.Is(err, ErrAgentNotPermitted) true, so the two
// spellings of one outcome agree.
func (e *AgentNotPermittedError) Unwrap() error { return ErrAgentNotPermitted }
