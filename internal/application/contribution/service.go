package contribution

import (
	"context"
	"fmt"

	"github.com/lichman0405/post/internal/contribution"
)

// Service orchestrates the contribution-opportunity use cases against
// the Repository port (internal/contribution). It owns input validation
// (the port trusts, the service verifies), the suggested → open → closed
// machine, and the agent-actor refusals; it does NOT authorize project
// membership/roles — those checks belong to the consuming API task,
// which passes resolved identities in (package doc).
//
// The visibility machine has exactly one surface here: Publicize. No
// other method touches visibility, and the store's publicize is the only
// path the database admits (migration 00062) — so a row becomes visible
// on the open network only through this explicit, audited, human
// action (acceptance "Agent 不能自动 publicize opportunity").
type Service struct {
	repo contribution.Repository
}

// NewService wires the service.
func NewService(repo contribution.Repository) *Service {
	return &Service{repo: repo}
}

// MarkParams carries one maintainer mark: the target, the metadata, and
// the resolved maintainer actor.
type MarkParams struct {
	// ProjectID is the research boundary the opportunity belongs to.
	ProjectID string
	// TargetType names what the opportunity is about.
	TargetType contribution.OpportunityTargetType
	// TargetID is the uuid of the target row (issues.id /
	// scientific_objects.id). The store verifies its existence in the
	// project and snapshots the title.
	TargetID string
	// Description carries the maintainer's context
	// (contribution.ValidOpportunityDescription); empty when none.
	Description string
	// Difficulty is the skill-level metadata.
	Difficulty contribution.Difficulty
	// RequiredCapabilities are the capability tags.
	RequiredCapabilities []string
	// By is the resolved maintainer actor. An agent actor is refused:
	// marking is a maintainer action, and an agent's only path into the
	// model is Suggest (requirement "agent suggestions require
	// approval").
	By contribution.Actor
}

// SuggestParams carries one suggestion: the target, the metadata, and
// the resolved suggester (a platform agent or a member). Suggestions
// land in state suggested and become real opportunities only through a
// maintainer's Approve.
type SuggestParams struct {
	// ProjectID is the research boundary the opportunity belongs to.
	ProjectID string
	// TargetType names what the opportunity is about.
	TargetType contribution.OpportunityTargetType
	// TargetID is the uuid of the target row.
	TargetID string
	// Description carries the suggestion's rationale
	// (contribution.ValidOpportunityDescription); empty when none.
	Description string
	// Difficulty is the suggested skill level.
	Difficulty contribution.Difficulty
	// RequiredCapabilities are the suggested capability tags.
	RequiredCapabilities []string
	// By is the resolved suggester (agents allowed — the suggestion
	// still needs a maintainer's approval).
	By contribution.Actor
}

// MarkOpen marks the target as open for contribution: validates the
// shape, refuses agent actors, hands the creation to the adapter. The
// returned opportunity carries the target-snapshotted title, state open,
// visibility internal — never public.
func (s *Service) MarkOpen(ctx context.Context, in MarkParams) (contribution.ContributionOpportunity, error) {
	if err := validateMark(in); err != nil {
		return contribution.ContributionOpportunity{}, err
	}
	if in.By.IsAgent {
		return contribution.ContributionOpportunity{}, &AgentNotPermittedError{Action: "mark"}
	}
	op, err := s.repo.CreateOpportunity(ctx, contribution.CreateOpportunityParams{
		ProjectID:            in.ProjectID,
		TargetType:           in.TargetType,
		TargetID:             in.TargetID,
		Description:          in.Description,
		Difficulty:           in.Difficulty,
		RequiredCapabilities: in.RequiredCapabilities,
		State:                contribution.OpportunityStateOpen,
		By:                   in.By,
	})
	if err != nil {
		return contribution.ContributionOpportunity{}, wrapStoreError(err)
	}
	return op, nil
}

// Suggest proposes an opportunity (the agent/member path): validates the
// shape and creates a suggested row. The suggestion becomes a real
// opportunity only through a maintainer's Approve (requirement "agent
// suggestions require approval").
func (s *Service) Suggest(ctx context.Context, in SuggestParams) (contribution.ContributionOpportunity, error) {
	if err := validateSuggest(in); err != nil {
		return contribution.ContributionOpportunity{}, err
	}
	op, err := s.repo.CreateOpportunity(ctx, contribution.CreateOpportunityParams{
		ProjectID:            in.ProjectID,
		TargetType:           in.TargetType,
		TargetID:             in.TargetID,
		Description:          in.Description,
		Difficulty:           in.Difficulty,
		RequiredCapabilities: in.RequiredCapabilities,
		State:                contribution.OpportunityStateSuggested,
		By:                   in.By,
	})
	if err != nil {
		return contribution.ContributionOpportunity{}, wrapStoreError(err)
	}
	return op, nil
}

// Approve approves a suggestion (suggested → open): the maintainer's
// decision that makes the suggestion a real opportunity. Agent actors
// are refused — an agent cannot approve its own or anyone's suggestion.
func (s *Service) Approve(ctx context.Context, projectID, id string, by contribution.Actor) (contribution.ContributionOpportunity, error) {
	if err := validateRef(projectID, id); err != nil {
		return contribution.ContributionOpportunity{}, err
	}
	if by.IsAgent {
		return contribution.ContributionOpportunity{}, &AgentNotPermittedError{Action: "approve"}
	}
	return s.transition(ctx, projectID, id, contribution.OpportunityStateSuggested, contribution.OpportunityStateOpen, by)
}

// Reject rejects a suggestion (suggested → closed, terminal): the
// maintainer's decision that the proposal does not become an
// opportunity. The row stays as history.
func (s *Service) Reject(ctx context.Context, projectID, id string, by contribution.Actor) (contribution.ContributionOpportunity, error) {
	if err := validateRef(projectID, id); err != nil {
		return contribution.ContributionOpportunity{}, err
	}
	return s.transition(ctx, projectID, id, contribution.OpportunityStateSuggested, contribution.OpportunityStateClosed, by)
}

// Close closes an open opportunity (open → closed, terminal): fulfilled
// or withdrawn. The row stays as history; a re-mark creates a new row.
func (s *Service) Close(ctx context.Context, projectID, id string, by contribution.Actor) (contribution.ContributionOpportunity, error) {
	if err := validateRef(projectID, id); err != nil {
		return contribution.ContributionOpportunity{}, err
	}
	return s.transition(ctx, projectID, id, contribution.OpportunityStateOpen, contribution.OpportunityStateClosed, by)
}

// Publicize runs the explicit, audited internal → public widening
// (docs/12 §3). Agent actors are refused outright — an agent never
// publicizes, whatever else holds. The store additionally refuses
// non-open rows and is the only path the database admits.
func (s *Service) Publicize(ctx context.Context, projectID, id string, by contribution.Actor) (contribution.ContributionOpportunity, error) {
	if err := validateRef(projectID, id); err != nil {
		return contribution.ContributionOpportunity{}, err
	}
	if by.UserID == "" {
		return contribution.ContributionOpportunity{}, fmt.Errorf("%w: publicize requires a resolved actor", ErrValidation)
	}
	if by.IsAgent {
		return contribution.ContributionOpportunity{}, &AgentNotPermittedError{Action: "publicize"}
	}
	op, err := s.repo.PublicizeOpportunity(ctx, projectID, id, by)
	if err != nil {
		return contribution.ContributionOpportunity{}, wrapStoreError(err)
	}
	return op, nil
}

// UpdateMetadata patches the internal-facing metadata of a non-closed,
// internal opportunity. Publicized rows are frozen (the store and
// migration 00062 refuse).
func (s *Service) UpdateMetadata(ctx context.Context, projectID, id string, patch contribution.MetadataPatch) (contribution.ContributionOpportunity, error) {
	if err := validateRef(projectID, id); err != nil {
		return contribution.ContributionOpportunity{}, err
	}
	if err := validatePatch(patch); err != nil {
		return contribution.ContributionOpportunity{}, err
	}
	op, err := s.repo.UpdateOpportunityMetadata(ctx, projectID, id, patch)
	if err != nil {
		return contribution.ContributionOpportunity{}, wrapStoreError(err)
	}
	return op, nil
}

// Get returns one opportunity of the project by its id, or
// contribution.ErrOpportunityNotFound (also for an opportunity of
// another project — never leak a foreign entity's existence).
func (s *Service) Get(ctx context.Context, projectID, id string) (contribution.ContributionOpportunity, error) {
	if err := validateRef(projectID, id); err != nil {
		return contribution.ContributionOpportunity{}, err
	}
	op, err := s.repo.GetOpportunity(ctx, projectID, id)
	if err != nil {
		return contribution.ContributionOpportunity{}, wrapStoreError(err)
	}
	return op, nil
}

// List returns every opportunity of the project, newest first.
func (s *Service) List(ctx context.Context, projectID string) ([]contribution.ContributionOpportunity, error) {
	if projectID == "" {
		return nil, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	ops, err := s.repo.ListOpportunities(ctx, projectID)
	if err != nil {
		return nil, wrapStoreError(err)
	}
	return ops, nil
}

// ListPublic returns the open-network view: publicized, open
// opportunities across all projects, newest first. Only rows that went
// through the explicit publicize action ever appear here.
func (s *Service) ListPublic(ctx context.Context) ([]contribution.ContributionOpportunity, error) {
	ops, err := s.repo.ListPublicOpportunities(ctx)
	if err != nil {
		return nil, wrapStoreError(err)
	}
	return ops, nil
}

// transition runs one named state move: read the current state, check
// the machine, then hand the compare-and-swap to the adapter (the
// expected state in the CAS is the read's — a concurrent move fails
// with *StateConflictError instead of overwriting).
func (s *Service) transition(ctx context.Context, projectID, id string, from, to contribution.OpportunityState, by contribution.Actor) (contribution.ContributionOpportunity, error) {
	if !contribution.ValidOpportunityState(from) || !contribution.ValidOpportunityState(to) ||
		!contribution.CanTransitionOpportunityState(from, to) {
		return contribution.ContributionOpportunity{}, &TransitionError{ID: id, From: from, To: to}
	}
	if by.UserID == "" {
		return contribution.ContributionOpportunity{}, fmt.Errorf("%w: state transitions require a resolved actor", ErrValidation)
	}
	current, err := s.repo.GetOpportunity(ctx, projectID, id)
	if err != nil {
		return contribution.ContributionOpportunity{}, wrapStoreError(err)
	}
	if current.State != from {
		return contribution.ContributionOpportunity{}, &TransitionError{ID: id, From: current.State, To: to}
	}
	op, err := s.repo.SetOpportunityState(ctx, projectID, id, from, to, by)
	if err != nil {
		return contribution.ContributionOpportunity{}, wrapStoreError(err)
	}
	return op, nil
}

// validateRef checks the (project, id) address shape.
func validateRef(projectID, id string) error {
	if projectID == "" {
		return fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	if id == "" {
		return fmt.Errorf("%w: id is required", ErrValidation)
	}
	return nil
}

// validateMark checks the mark request's shape: identities present, the
// target type in the domain vocabulary, the metadata in the domain
// shape, and a resolved actor.
func validateMark(in MarkParams) error {
	if in.ProjectID == "" {
		return fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	if !contribution.ValidOpportunityTargetType(in.TargetType) {
		return fmt.Errorf("%w: unknown target_type %q (issue | research_question)", ErrValidation, in.TargetType)
	}
	if in.TargetID == "" {
		return fmt.Errorf("%w: target_id is required", ErrValidation)
	}
	if !contribution.ValidDifficulty(in.Difficulty) {
		return fmt.Errorf("%w: unknown difficulty %q", ErrValidation, in.Difficulty)
	}
	if !contribution.ValidRequiredCapabilities(in.RequiredCapabilities) {
		return fmt.Errorf("%w: invalid capability list (at most %d lowercase hyphenated tags, no duplicates)", ErrValidation, contribution.MaxRequiredCapabilities)
	}
	if !contribution.ValidOpportunityDescription(in.Description) {
		return fmt.Errorf("%w: description is oversized", ErrValidation)
	}
	if in.By.UserID == "" {
		return fmt.Errorf("%w: by.user_id is required", ErrValidation)
	}
	return nil
}

// validatePatch checks the metadata patch's shape: at least one field,
// and every present field in the domain shape. The store shape-checks
// again (defense in depth), but the service owns input validation.
func validatePatch(patch contribution.MetadataPatch) error {
	if patch.Title == nil && patch.Description == nil && patch.Difficulty == nil && patch.RequiredCapabilities == nil {
		return fmt.Errorf("%w: empty metadata patch", ErrValidation)
	}
	if (patch.Title != nil && !contribution.ValidOpportunityTitle(*patch.Title)) ||
		(patch.Description != nil && !contribution.ValidOpportunityDescription(*patch.Description)) ||
		(patch.Difficulty != nil && !contribution.ValidDifficulty(*patch.Difficulty)) ||
		(patch.RequiredCapabilities != nil && !contribution.ValidRequiredCapabilities(*patch.RequiredCapabilities)) {
		return fmt.Errorf("%w: invalid metadata patch", ErrValidation)
	}
	return nil
}

// validateSuggest checks the suggestion request's shape — the same
// shape as a mark (the store derives the title from the target either
// way).
func validateSuggest(in SuggestParams) error {
	return validateMark(MarkParams(in))
}
