package assetrights

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
)

// Actor is the principal asking for the change: the authenticated user,
// and whether the request arrived as a platform agent rather than a human
// session.
//
// The flag is a field of a value the TRANSPORT builds from the principal,
// never a field of a request body: a caller that could set it could clear
// it, and the only thing the flag does is refuse. It is the shape
// assetpublish.Actor has, for the same reason — a second spelling of
// "this call is an agent" is a second place the flag could be read from a
// caller's bytes.
type Actor struct {
	// User is the authenticated user. An agent acts AS a user (the
	// token's owner), so this is never empty.
	User domain.User
	// IsAgent reports whether the request arrived as a platform agent
	// (MCP/API), not as a human session.
	IsAgent bool
}

// ChangeParams is one rights-holder change request.
type ChangeParams struct {
	// ProjectID is the project the asset belongs to. It is required
	// rather than derived, and that is the authorization's shape: the
	// actor's membership is resolved for THIS project before the asset is
	// looked up at all, so the denial precedes the lookup the way every
	// other governed command in this tree resolves it
	// (internal/application/assetpublish.requirePublish states the rule).
	// The store refuses a change whose asset's origin project is not this
	// one, so the pair cannot be used to authorize in one project and
	// write in another.
	ProjectID string
	// AssetPID is the asset's persistent identifier.
	AssetPID string
	// Holder is the party that is to hold the asset — a user or an
	// organization, named by kind AND id (domain.Party).
	Holder domain.Party
}

// Command is the asset rights-holder change use case.
//
// It owns the order of the steps, and the order is the contract:
//
//  1. agent  — the domain backstop, from the actor value alone
//  2. shape  — validate the request, before anything is read
//  3. owner  — membership class vs the matrix, before any asset lookup
//  4. change — the store's transaction: lock, resolve, append, audit
//
// Steps 1 and 2 precede every read because neither needs one; step 3
// precedes the asset lookup because a denial must not disclose whether
// the pid exists (an unknown pid and an asset the caller may not govern
// are answered the same).
type Command struct {
	members MembershipPort
	store   StorePort
	authz   authz.Engine
}

// Deps carries the adapters a Command is wired over.
type Deps struct {
	// Members resolves the actor's project membership (the production
	// value is *persistence.ProjectStore).
	Members MembershipPort
	// Store is the governance store (the production value is
	// *persistence.AssetRightsStore).
	Store StorePort
	// Authz is the permission-matrix engine (the production value is
	// authz.NewMatrixEngine()).
	Authz authz.Engine
}

// NewCommand wires the change command.
func NewCommand(deps Deps) *Command {
	return &Command{members: deps.Members, store: deps.Store, authz: deps.Authz}
}

// ChangeRightsHolder changes who holds the asset, as one append-only
// governance event, and returns the event it wrote.
//
// Nothing about the asset's identity moves: the id, the pid, the origin
// project, the versions, their origin refs and the credited creators are
// the rows they already were. What changes is which party the governance
// chain names — and it changes by APPENDING, so the party that held the
// asset before is still readable (T0711's acceptance; docs/11 §6).
func (c *Command) ChangeRightsHolder(ctx context.Context, actor Actor, in ChangeParams) (HolderChange, error) {
	if err := requireHuman(actor); err != nil {
		return HolderChange{}, err
	}
	req, err := c.prepare(actor, in)
	if err != nil {
		return HolderChange{}, err
	}
	if err := c.requireOwner(ctx, actor.User.ID, in.ProjectID, actor.IsAgent); err != nil {
		return HolderChange{}, err
	}
	change, err := c.store.ChangeHolder(ctx, req)
	if err != nil {
		return HolderChange{}, mapChangeError(err)
	}
	return change, nil
}

// requireHuman is the domain backstop: an agent never changes a rights
// holder, whatever the matrix says. It reads the actor value alone, so it
// is resolved before any lookup — the same two lines of defence a publish
// carries, and for the same reason: the matrix's agent column is a policy
// document a governance change may edit, while this is the product rule
// the edit cannot waive (specs/mcp/tools.json:
// forbidden_default_agent_actions).
func requireHuman(actor Actor) error {
	if actor.IsAgent {
		return &AgentNotPermittedError{Action: string(authz.ActionChangeRightsHolder)}
	}
	return nil
}

// prepare validates the request shape and renders the store request.
//
// Every refusal here is a statement about the REQUEST, made before
// anything is read: a project must be named, the pid must be a pid, and
// the holder must be a party this platform can store — a kind AND an id,
// with the kind one of the two identity kinds. The holder id's shape is
// checked (a uuid, because the column is a uuid); whether the row EXISTS
// is the store's read, because a command that cannot look anything up
// cannot decide it.
func (c *Command) prepare(actor Actor, in ChangeParams) (ChangeRequest, error) {
	projectID := strings.TrimSpace(in.ProjectID)
	if projectID == "" {
		return ChangeRequest{}, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	pid := strings.TrimSpace(in.AssetPID)
	if !ValidPID(pid) {
		return ChangeRequest{}, fmt.Errorf("%w: asset_pid %q is not a persistent identifier (26 Crockford base32 characters)", ErrValidation, in.AssetPID)
	}
	if !in.Holder.Kind.Valid() {
		return ChangeRequest{}, fmt.Errorf("%w: holder kind must be one of user|organization|project, got %q", ErrValidation, in.Holder.Kind)
	}
	if !in.Holder.IsIdentity() {
		// A project is not a rights holder: docs/11 §6 lists Originating
		// Project as its own role, and a holder is a person or an
		// organization (L3-20260916-1). Refused by name rather than
		// translated into one of the two identity kinds.
		return ChangeRequest{}, fmt.Errorf("%w: a rights holder is a user or an organization, and %q is neither — an asset's originating project is a different role (docs/11 §6)", ErrValidation, in.Holder.Kind)
	}
	holderID := strings.TrimSpace(in.Holder.ID)
	if !domain.ValidUUID(holderID) {
		return ChangeRequest{}, fmt.Errorf("%w: holder id %q is not a uuid, so it names no user or organization row", ErrValidation, in.Holder.ID)
	}
	return ChangeRequest{
		ProjectID: projectID,
		AssetPID:  pid,
		Holder:    domain.Party{Kind: in.Holder.Kind, ID: holderID},
		Actor:     actor,
		Audit:     auditEntry(actor.User, projectID, pid),
	}, nil
}

// requireOwner authorizes the change: resolve the actor's membership role
// and evaluate authz.ActionChangeRightsHolder for the class.
//
// specs/policies/permissions-matrix.csv:13 is
// `change_rights_holder,deny,deny,deny,deny,deny,allow,deny` — owner and
// nothing else. The maintainer cell is a plain `deny` here, unlike the
// publish row's `conditional`, so there is no condition to interpret and
// no judgement to make: any class the matrix does not explicitly allow is
// refused, and the engine itself denies an unknown action or class
// (docs/50: silence is never permission).
//
// The denial precedes every lookup of the asset, and that is the point
// rather than an artifact of the ordering: the membership resolution
// answers projects.ErrMemberNotFound for a project that does not exist
// exactly as it does for one the caller does not belong to, so a caller
// probing pids through this command is answered "not permitted" either
// way. Only projects.ErrProjectNotFound — the deliberate disclosure the
// project surface makes for an invisible project — is relayed, and only
// when the caller could not be resolved a role.
//
// A nil or erroring engine is a wiring failure, never a permission: the
// refusal is ErrStore.
func (c *Command) requireOwner(ctx context.Context, actorID, projectID string, isAgent bool) error {
	if c.members == nil || c.authz == nil {
		return fmt.Errorf("%w: change command not fully wired", ErrStore)
	}
	membership, err := c.members.GetMembership(ctx, projectID, actorID)
	var role *domain.ProjectRole
	switch {
	case err == nil:
		r := membership.Role
		role = &r
	case errors.Is(err, projects.ErrMemberNotFound):
		// Not a member (or the project does not exist): the class is the
		// anonymous/authenticated-non-member one, and the matrix denies
		// both for this action.
		role = nil
	case errors.Is(err, projects.ErrProjectNotFound):
		return ErrProjectNotFound
	default:
		return mapChangeError(err)
	}
	decision, err := c.authz.Authorize(ctx, authz.Request{
		Action: authz.ActionChangeRightsHolder,
		Class:  authz.ClassOf(true, role, isAgent),
	})
	if err != nil {
		return fmt.Errorf("%w: authorize rights holder change: %v", ErrStore, err)
	}
	if !decision.Permits() {
		return ErrForbidden
	}
	return nil
}

// auditEntry renders the audit row the store appends inside the same
// transaction as the event. docs/26 §5 puts ownership transfer in the
// highest-risk tier and requires the audit to be append-only, which
// audit_log already is by trigger (00014/00015).
//
// The two summaries are the store's: the BEFORE half is the current
// holder, which only the transaction can read. What this function fixes
// is everything the request itself knows — who acted, how it arrived,
// what happened, and which asset it was about (named by its persistent
// identifier, the identity every later reader resolves through).
func auditEntry(actor domain.User, projectID, assetPID string) domain.AuditEntry {
	return domain.AuditEntry{
		ActorID:   actor.ID,
		Via:       domain.ViaSession,
		Action:    domain.ActionAssetRightsHolderChanged,
		TargetRef: "asset:" + assetPID,
		ProjectID: projectID,
	}
}

// ValidPID reports whether pid has the persistent-identifier shape
// (migration 00064: 26 Crockford base32 characters). It IS
// assets.ValidPID — the same predicate the publish command and the publish
// gate use, named here so this surface reads the same predicate rather
// than a second opinion about what a pid is.
func ValidPID(pid string) bool { return assets.ValidPID(pid) }

// mapChangeError keeps the sentinels of this package and of its ports,
// and turns everything else into ErrStore.
func mapChangeError(err error) error {
	switch {
	case err == nil,
		errors.Is(err, ErrValidation),
		errors.Is(err, ErrAgentNotPermitted),
		errors.Is(err, ErrForbidden),
		errors.Is(err, ErrProjectNotFound),
		errors.Is(err, ErrAssetNotFound),
		errors.Is(err, ErrPartyNotFound),
		errors.Is(err, ErrNoChange),
		errors.Is(err, ErrStore):
		return err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}
