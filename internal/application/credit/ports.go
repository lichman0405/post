package credit

import (
	"context"

	"github.com/lichman0405/post/internal/contribution"
	"github.com/lichman0405/post/internal/domain"
)

// Ports (docs/52: the application orchestrates against ports; adapters
// live in internal/persistence). The commands need three things outside
// themselves: the actor's membership role in the project, the store that
// owns the transactions, and — for the reads — the same store's read half.

// MembershipPort resolves the actor's membership in the project. The
// production implementation is persistence.ProjectStore, the same value
// the publish and rights-holder commands are wired over. A project that
// does not exist, or that the caller is not a member of, answers
// projects.ErrMemberNotFound — so an unknown project is an unknown
// membership rather than a disclosed absence (see Command.requireRole).
type MembershipPort interface {
	GetMembership(ctx context.Context, projectID, userID string) (domain.ProjectMembership, error)
}

// StorePort is the write half: one method per command, each of which runs
// its whole effect in ONE transaction.
//
// The methods answer the sentinels of this package (ErrProjectNotFound,
// ErrTargetNotFound, ErrPartyNotFound, ErrDisputeNotFound,
// ErrDisputeClosed) and ErrStore. None of them writes a partial effect:
// the declaration and its parties, or the dispute row and its event, or the
// closed row, its event and the audit row, commit together or not at all.
type StorePort interface {
	// DeclareAttribution appends one credit declaration for the target the
	// request names, and returns the statement it wrote.
	//
	// It resolves and locks the target row first, so the per-target
	// ordinal it assigns is serialized rather than computed in a race, and
	// it resolves every declared party against the table its kind names
	// before it writes: a declaration that named nobody would be a row no
	// later reader could repair.
	DeclareAttribution(ctx context.Context, req DeclareRequest) (contribution.CreditAttribution, error)
	// OpenDispute writes one credit_disputes row in state open and records
	// the credit.dispute_opened event for it, in the same transaction.
	OpenDispute(ctx context.Context, req OpenDisputeRequest) (contribution.CreditDispute, error)
	// CloseDispute decides the dispute the request names: it locks the row,
	// refuses it unless it is still open (ErrDisputeClosed), sets state /
	// resolution / resolved_at, records the credit.dispute_resolved event
	// and appends the audit row — all in one transaction. It never touches
	// a ledger row or an earlier dispute.
	CloseDispute(ctx context.Context, req CloseDisputeRequest) (contribution.CreditDispute, error)
}

// Reader is the read half of the credit surface: the declaration chain of
// one target, a target's current declaration, and a project's disputes.
//
// # It does not resolve an actor, and that is deliberate
//
// Every read here is a read of rows nothing ever rewrites (00103's
// append-only guards) or of one project's current-state disputes, and none
// of them is a governed command: the reader takes no actor, resolves no
// membership and consults no matrix. That is
// assetrights.HolderReader's arrangement, for the same reason — the
// authorization of a READ belongs to the surface that renders it (its
// project visibility gate), not to a second copy of the policy inside a
// store adapter that a writer could then bypass. No transport is delivered
// by this task (specs/api/openapi.yaml is the API contract and is out of
// scope), so the gate this reader's consumer must apply is named in the
// task RESULT rather than guessed at here.
//
// A reader is a separate interface from StorePort because it is a separate
// concern: a consumer that only renders "who is credited for this" is not
// thereby able to declare a credit or decide a dispute.
type Reader interface {
	// CurrentAttribution returns the target's current declaration (the
	// greatest ordinal), or nil when the target has never been declared.
	CurrentAttribution(ctx context.Context, projectID, targetRef string) (*contribution.CreditAttribution, error)
	// AttributionHistory returns the target's whole declaration chain,
	// oldest first — every declaration ever made about it, corrections
	// included (docs/13 §2: "旧 attribution ... 保留").
	AttributionHistory(ctx context.Context, projectID, targetRef string) ([]contribution.CreditAttribution, error)
	// GetDispute returns one dispute of the project. A dispute of another
	// project answers ErrDisputeNotFound.
	GetDispute(ctx context.Context, projectID, disputeID string) (contribution.CreditDispute, error)
	// ListDisputes returns the project's disputes, newest first.
	ListDisputes(ctx context.Context, projectID string) ([]contribution.CreditDispute, error)
	// ListTargetDisputes returns the disputes about one target, newest
	// first.
	ListTargetDisputes(ctx context.Context, projectID, targetRef string) ([]contribution.CreditDispute, error)
}

// DeclareRequest is everything one declaration needs to execute: which
// target, which parties under which roles, who is asking, and the audit row
// to append inside the same transaction.
type DeclareRequest struct {
	// ProjectID is the project the target must belong to, in text uuid
	// form. The caller names it because the authorization was resolved
	// against it.
	ProjectID string
	// TargetRef is the canonical "kind:value" ref (contribution.NewCreditTargetRef).
	TargetRef string
	// Parties are the declared parties, in the caller's order; Position is
	// assigned by the command from that order.
	Parties []DeclaredParty
	// Actor is the principal the declaration is attributed to.
	Actor Actor
	// Audit is the row the store appends inside the transaction. Action,
	// ActorID, Via, ProjectID and TargetRef are the command's; the
	// summaries and the metadata are the store's.
	Audit domain.AuditEntry
}

// DeclaredParty is one party a declaration names, under one role, with the
// position the caller sent it in.
type DeclaredParty struct {
	// Party is the identity: which kind, which row of it.
	Party domain.Party
	// Role is the credit role the party is declared under.
	Role contribution.CreditRole
	// Position is the caller's index for this role.
	Position int
}

// OpenDisputeRequest is one dispute to raise.
type OpenDisputeRequest struct {
	ProjectID string
	// TargetRef is the canonical ref of the credit the dispute is about.
	TargetRef string
	// Claim is the opener's statement of what is wrong with the credit. It
	// is stored verbatim and is immutable once written.
	Claim string
	// EvidenceRefs are the ledger references the claim points at
	// (docs/13 §3's "附带 ledger evidence"), canonical "kind:value" refs.
	// They travel with the credit.dispute_opened event payload, which is
	// where the append-only record of them lives: 00011's credit_disputes
	// has no column for them and this task may not add one to a table the
	// catalog test pins.
	EvidenceRefs []string
	Actor        Actor
	Audit        domain.AuditEntry
}

// CloseDisputeRequest is one dispute decision.
type CloseDisputeRequest struct {
	ProjectID string
	DisputeID string
	// Outcome is the state the dispute moves to: resolved or rejected.
	// Both are decisions; the two are distinguished because "the claim was
	// upheld" and "the claim was refused" are different facts about the
	// credit.
	Outcome contribution.DisputeState
	// Resolution is governance's reasoning. Required: a decision nobody
	// can read is not a decision.
	Resolution string
	Actor      Actor
	Audit      domain.AuditEntry
}
