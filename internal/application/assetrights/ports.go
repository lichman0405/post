package assetrights

import (
	"context"
	"time"

	"github.com/lichman0405/post/internal/domain"
)

// Ports (docs/52: the application orchestrates against ports; adapters
// live in internal/persistence). The change command needs exactly two
// things outside itself — the actor's membership role in the project, and
// the store that owns the governance transaction.

// MembershipPort resolves the actor's membership in the project. The
// production implementation is persistence.ProjectStore, the same value
// the publish command is wired over. A project that does not exist, or
// that the caller is not a member of, answers projects.ErrMemberNotFound —
// so an unknown project is an unknown membership rather than a disclosed
// absence (see Command.requireOwner).
type MembershipPort interface {
	GetMembership(ctx context.Context, projectID, userID string) (domain.ProjectMembership, error)
}

// StorePort is the store that owns the rights-holder chain's write.
//
// One method, because the command has one job: the whole change — the
// previous holder read, the party resolved, the event appended, the audit
// row written — is ONE transaction, and there is no pre-check the command
// could usefully run outside it. That is deliberate and it is the
// difference from the publish port (whose LookupCreation exists because a
// replay is a READ that must not run a gate): a rights-holder change has
// no replay to answer, so it has no read to hoist.
type StorePort interface {
	// ChangeHolder appends one rights-holder event for the asset the
	// request names, in one transaction, and returns the event it wrote.
	//
	// It answers the sentinels of this package (ErrProjectNotFound,
	// ErrAssetNotFound, ErrPartyNotFound, ErrNoChange) and ErrStore. It
	// NEVER writes a partial change: the event row and the audit row
	// commit together or not at all.
	ChangeHolder(ctx context.Context, req ChangeRequest) (HolderChange, error)
}

// ChangeRequest is everything one change needs to execute: which asset
// (named twice — by the project it belongs to and by its persistent
// identifier), who is to hold it, who is asking, and the audit row to
// append inside the same transaction.
//
// The audit entry is built by the command and written by the store, the
// pattern every governed write in this tree follows: the audit record
// commits with the state change or not at all. Its BeforeSummary and
// AfterSummary are filled by the STORE, not by the command — the before
// half is the current holder, which only the transaction knows.
type ChangeRequest struct {
	// ProjectID is the project the asset must belong to, in text uuid
	// form. The caller names it because the authorization was resolved
	// against it: a change that resolved the actor's role in one project
	// and then wrote to an asset of another would be an authorization of
	// the wrong thing (see Command.ChangeRightsHolder).
	ProjectID string
	// AssetPID is the asset's persistent identifier (research_assets.pid).
	AssetPID string
	// Holder is the party that is to hold the asset: which kind of
	// identity, and which row of it.
	Holder domain.Party
	// Actor is the principal the change is attributed to.
	Actor Actor
	// Audit is the row the store appends inside the transaction. Action,
	// ActorID, Via, ProjectID and TargetRef are the command's; the two
	// summaries are the store's.
	Audit domain.AuditEntry
}

// HolderChange is one written governance event: what the chain now says,
// and what it said before.
//
// Previous is nil when the asset had no holder before this event — an
// honest absence and not an error (an asset nobody has ever held is a
// state a first designation resolves). It is never a fabricated party.
type HolderChange struct {
	// EventID is the asset_rights_holder_events row id.
	EventID string
	// AssetID is the research_assets row id (internal).
	AssetID string
	// AssetPID is the asset's persistent identifier.
	AssetPID string
	// Ordinal is this event's position in the asset's chain, 1 for the
	// first.
	Ordinal int
	// Holder is the party the chain now names, with the identity the store
	// resolved for it.
	Holder PartyIdentity
	// Previous is the party the event supersedes, with its resolved
	// identity, or nil.
	Previous *PartyIdentity
	// RecordedBy is the acting user's id and RecordedAt the instant the
	// row was written.
	RecordedBy string
	RecordedAt time.Time
}

// PartyIdentity is one party WITH the display facts the identity table
// holds for it: a user's handle and display name, an organization's slug
// and name. The kind and the id are the party (domain.Party); the two
// strings are what a surface renders, resolved from the table the kind
// names — never guessed from the other kind's shape.
type PartyIdentity struct {
	Party domain.Party
	// Handle is a user's handle or an organization's slug.
	Handle string
	// DisplayName is a user's display name or an organization's name.
	DisplayName string
}

// HolderEvent is one row of an asset's chain as read back: the same fields
// a HolderChange carries, for a row of any age. The chain is read oldest
// first, so the LAST element is the current holder and every earlier one
// is a holder the asset has had — which is what the acceptance "转移之后，
// 转移之前的持有关系仍要读得出来" is checked through.
type HolderEvent struct {
	EventID    string
	Ordinal    int
	Holder     PartyIdentity
	Previous   *PartyIdentity
	RecordedBy string
	RecordedAt time.Time
}

// HolderReader is the read half of the governance surface: the current
// holder, and the whole chain. Both are reads of rows nothing ever
// updates (00082's triggers), so neither can answer differently later for
// the same asset.
//
// It is a separate interface from StorePort because it is a separate
// concern: the command does not read the chain, and a consumer that only
// renders "who holds this" is not thereby able to write one.
type HolderReader interface {
	// CurrentHolder returns the asset's current holder, or nil when the
	// asset has never had one.
	CurrentHolder(ctx context.Context, assetPID string) (*HolderEvent, error)
	// HolderHistory returns the asset's whole chain, oldest first.
	HolderHistory(ctx context.Context, assetPID string) ([]HolderEvent, error)
}
