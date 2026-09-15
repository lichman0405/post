package contribution

import "context"

// Repository is the persistence port for contribution opportunities
// (docs/52: the application orchestrates against ports; the subsystem's
// adapter lives with the subsystem — internal/contribution, the same
// arrangement as internal/gitprovider's stores). The production adapter
// is *OpportunityStore over PostgreSQL; the service validates the shape
// and the store owns the atomicity.
//
// Two invariants are enforced by BOTH the adapter and the database
// (migration 00062), so they hold for any write path:
//
//   - the suggested → open → closed machine: transitions are
//     compare-and-swaps on the expected state; the guard refuses any
//     other move at the database;
//   - publicize is the ONLY visibility change and the ONLY way to
//     public: the adapter's PublicizeOpportunity is the one path that
//     sets the transaction-scoped session flag the guard requires, and
//     it refuses non-open rows; a rogue UPDATE without the flag fails
//     (docs/12 §3 — visibility widening is never automatic).
type Repository interface {
	// CreateOpportunity inserts one opportunity row (a maintainer mark
	// with State open, or a suggestion with State suggested). The title
	// is DERIVED inside the creation transaction: snapshotted from the
	// target row (the issue's title / the research question's current
	// version title), never from caller input. It fails with
	// projects.ErrProjectNotFound for an unknown project,
	// ErrTargetNotFound when the target does not exist in the project
	// (or is not a research_question for that target type),
	// ErrTargetAlreadyActive when the target already carries a
	// non-closed opportunity (the partial unique index's conflict), and
	// ErrValidation for malformed identities.
	CreateOpportunity(ctx context.Context, in CreateOpportunityParams) (ContributionOpportunity, error)
	// GetOpportunity returns the project's opportunity by id, or
	// ErrOpportunityNotFound (an opportunity of another project reports
	// the same outcome — never leak a foreign entity's existence).
	GetOpportunity(ctx context.Context, projectID, id string) (ContributionOpportunity, error)
	// ListOpportunities returns every opportunity of the project,
	// newest first.
	ListOpportunities(ctx context.Context, projectID string) ([]ContributionOpportunity, error)
	// ListPublicOpportunities returns the open-network view: publicized,
	// open opportunities across all projects, newest first. Nothing
	// internal ever appears here — a row becomes part of this listing
	// only through the explicit publicize action.
	ListPublicOpportunities(ctx context.Context) ([]ContributionOpportunity, error)
	// SetOpportunityState transitions the opportunity expected → to
	// through a compare-and-swap, stamping ApprovedBy when the move is
	// the suggested → open approval. It fails with
	// ErrOpportunityNotFound for an unknown or foreign opportunity and
	// *StateConflictError when the row is no longer in the expected
	// state. Every successful transition appends one audit row in the
	// same transaction.
	SetOpportunityState(ctx context.Context, projectID, id string, expected, to OpportunityState, by Actor) (ContributionOpportunity, error)
	// PublicizeOpportunity runs the explicit, audited internal → public
	// widening (docs/12 §3): it refuses a non-open row
	// (*PublicizeError) and is the only path that sets the session flag
	// migration 00062's guard requires, so no other write path can
	// publicize. The transaction stamps PublicizedBy/PublicizedAt and
	// appends the audit row atomically with the visibility change.
	PublicizeOpportunity(ctx context.Context, projectID, id string, by Actor) (ContributionOpportunity, error)
	// UpdateOpportunityMetadata patches the internal-facing metadata
	// (title, description, difficulty, capabilities) of a non-closed,
	// internal opportunity. It fails with ErrOpportunityNotFound,
	// *TerminalError when the row is closed, and ErrPublicizedFrozen
	// when the row is already public — a publicized row's metadata is
	// immutable (the database refuses any other path too).
	UpdateOpportunityMetadata(ctx context.Context, projectID, id string, patch MetadataPatch) (ContributionOpportunity, error)
}

// CreateOpportunityParams carries one opportunity creation request. The
// store derives Title from the target row inside the creation
// transaction; everything else is caller input validated by the service
// (and shape-checked again by the store).
type CreateOpportunityParams struct {
	// ProjectID is the research boundary the opportunity belongs to.
	ProjectID string
	// TargetType names what the opportunity is about.
	TargetType OpportunityTargetType
	// TargetID is the uuid of the target row (issues.id /
	// scientific_objects.id).
	TargetID string
	// Description carries the maintainer's/suggester's context
	// (domain.ValidOpportunityDescription); empty when none.
	Description string
	// Difficulty is the skill-level metadata.
	Difficulty Difficulty
	// RequiredCapabilities are the capability tags
	// (ValidRequiredCapabilities).
	RequiredCapabilities []string
	// State is the row's creation state: Suggested for a suggestion
	// (SuggestedBy set) or Open for a maintainer mark (SuggestedBy
	// nil).
	State OpportunityState
	// By names the creating actor: the suggester for a suggestion, the
	// maintainer for a mark.
	By Actor
}

// MetadataPatch carries one metadata update. Nil fields stay unchanged;
// the empty string is a legitimate Description (clears it) — use a
// pointer for that field, never the zero value as "absent".
type MetadataPatch struct {
	// Title overrides the snapshotted title (ValidOpportunityTitle).
	Title *string
	// Description overrides the context (ValidOpportunityDescription).
	Description *string
	// Difficulty overrides the skill level.
	Difficulty *Difficulty
	// RequiredCapabilities overrides the capability tags.
	RequiredCapabilities *[]string
}
