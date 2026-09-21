package prchecks

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/integrity"
)

// Service assembles one PR's integrity snapshot from persistence and runs
// the engine over it. It owns input validation and the read mapping; the
// engine owns the verdicts. Reads only — the check never modifies state
// (docs/22 §7).
type Service struct {
	prs      PRReader
	projects ProjectReader
	states   StateReader
	branches BranchHeadReader
	manifest ManifestReader
	policies PolicyLister
	engine   *integrity.Engine
}

// Deps carries the adapters the service needs. Every port is a narrow
// read surface satisfied structurally by the existing persistence stores,
// so no adapter code exists for this package (the composition root wires
// the production implementations).
type Deps struct {
	PRs      PRReader
	Projects ProjectReader
	States   StateReader
	Branches BranchHeadReader
	Manifest ManifestReader
	Policies PolicyLister
	Engine   *integrity.Engine
}

// NewService wires the service.
func NewService(deps Deps) *Service {
	return &Service{
		prs:      deps.PRs,
		projects: deps.Projects,
		states:   deps.States,
		branches: deps.Branches,
		manifest: deps.Manifest,
		policies: deps.Policies,
		engine:   deps.Engine,
	}
}

// CheckPullRequest runs the integrity review of one PR and returns the
// full machine report. The report is derived, never stored: the PR's
// pinned states are fixed and the rows behind them append-only, so the
// same PR always derives the same outcomes — the engine is the
// deterministic core, the timestamp is this run's stamp.
func (s *Service) CheckPullRequest(ctx context.Context, projectID string, number int64) (integrity.Report, error) {
	if projectID == "" {
		return integrity.Report{}, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	if number < 1 {
		return integrity.Report{}, fmt.Errorf("%w: number must be positive", ErrValidation)
	}
	pr, err := s.prs.GetPullRequest(ctx, projectID, number)
	if err != nil {
		return integrity.Report{}, WrapStoreError(err)
	}
	orgID, err := s.projectOrg(ctx, pr.ProjectID)
	if err != nil {
		return integrity.Report{}, err
	}
	targetStates, err := s.states.ListStates(ctx, pr.TargetBranchID)
	if err != nil {
		return integrity.Report{}, WrapStoreError(err)
	}
	sourceStates, err := s.states.ListStates(ctx, pr.SourceBranchID)
	if err != nil {
		return integrity.Report{}, WrapStoreError(err)
	}
	sourceCommits, err := s.states.ListCommits(ctx, pr.SourceBranchID)
	if err != nil {
		return integrity.Report{}, WrapStoreError(err)
	}
	baseManifest, err := s.manifest.GetManifestSnapshot(ctx, pr.BaseStateID)
	if err != nil {
		return integrity.Report{}, WrapStoreError(err)
	}
	proposedManifest, err := s.manifest.GetManifestSnapshot(ctx, pr.ProposedStateID)
	if err != nil {
		return integrity.Report{}, WrapStoreError(err)
	}
	policies, err := s.policyUniverse(ctx, pr.ProjectID, orgID)
	if err != nil {
		return integrity.Report{}, err
	}
	boundaries, err := s.resolveBoundaries(ctx, pr, targetStates, sourceStates)
	if err != nil {
		return integrity.Report{}, err
	}

	snap := integrity.Snapshot{
		ProjectID:         pr.ProjectID,
		PRNumber:          pr.Number,
		Base:              integrity.StateRef{ID: pr.BaseStateID},
		Proposed:          integrity.StateRef{ID: pr.ProposedStateID},
		TargetStates:      targetStates,
		SourceStates:      sourceStates,
		SourceCommits:     sourceCommits,
		BaseObjects:       baseManifest.ObjectVersions,
		ProposedObjects:   proposedManifest.ObjectVersions,
		BaseRelations:     baseManifest.RelationVersions,
		ProposedRelations: proposedManifest.RelationVersions,
		BaseBlobRefs:      baseManifest.BlobRefs,
		ProposedBlobRefs:  proposedManifest.BlobRefs,
		PolicyVersions:    policies,
		TargetBoundaries:  boundaries.target,
		SourceBoundaries:  boundaries.source,
	}
	report := s.engine.Check(snap)
	report.ComputedAt = time.Now().UTC().Format(time.RFC3339)
	return report, nil
}

// projectOrg resolves the project's owning organization id ("" for a
// personal project, whose policy universe is the project's own).
func (s *Service) projectOrg(ctx context.Context, projectID string) (string, error) {
	project, err := s.projects.GetProject(ctx, projectID)
	if err != nil {
		return "", WrapStoreError(err)
	}
	if project.OrganizationID == nil {
		return "", nil
	}
	return *project.OrganizationID, nil
}

// policyUniverse lists every policy version a proposed pin may resolve to:
// the organization's history plus the project's (docs/12 §5 — the org is
// the lower bound; a pin may name either scope's versions).
func (s *Service) policyUniverse(ctx context.Context, projectID, orgID string) ([]domain.PolicyVersion, error) {
	var out []domain.PolicyVersion
	if orgID != "" {
		orgPolicies, err := s.policies.List(ctx, domain.PolicyScope{OrganizationID: orgID})
		if err != nil {
			return nil, WrapStoreError(err)
		}
		out = append(out, orgPolicies...)
	}
	projectPolicies, err := s.policies.List(ctx, domain.PolicyScope{ProjectID: projectID})
	if err != nil {
		return nil, WrapStoreError(err)
	}
	out = append(out, projectPolicies...)
	return out, nil
}

// branchBoundaries carries the two chains' legitimate exits as separate
// sets — the engine accepts a target-side pin only against the target's
// boundaries and a source-side pin only against the source's, so the two
// are never mixed into one map.
type branchBoundaries struct {
	target map[string]bool
	source map[string]bool
}

// resolveBoundaries derives the states each branch chain may legitimately
// touch without containing them: for a chain with states, the state its
// root builds on (the fork point — which may be the genesis root or a
// state of a third branch); for a chain without states, the branch's own
// head (a branch that has no states of its own has never moved, so its
// head is the only state a pin may name for it). The target and source
// resolutions stay in separate sets. A candidate is accepted only when it
// exists and belongs to the project of the chain it is a boundary OF —
// anything else is left out and the engine's provenance checks fail on it.
//
// That project is per side, and it is read rather than assumed (ADR-025):
// the TARGET branch is the PR's own (the proposal's target is the branch
// its project advances), while the SOURCE branch may live in the
// contributor's fork, so its chain's boundary is a state of the fork's
// project. Judging the source boundary against the PR's project would drop
// exactly the boundary an external proposal has — the fork project's root
// state that the fork branch was created from — and the source chain would
// then be reported as broken (or, before the copied state is chained at
// all, as having two heads). The rule is the same one every other
// cross-project read in this build applies: ask which project the row
// belongs to.
func (s *Service) resolveBoundaries(ctx context.Context, pr domain.PullRequest, targetStates, sourceStates []domain.ProjectState) (branchBoundaries, error) {
	// Each side collects its own candidates first: the same state may
	// legitimately be a boundary for both chains (a source branch forked
	// from the target's head roots from a state the target names too), so
	// the sets accumulate per side rather than sharing one slot per id.
	//
	// The empty-chain candidate comes from the BRANCH ROW, never from the
	// PR being checked: a pin resolved from the row under review would
	// accept itself and the provenance check would compare the pin with
	// the pin — true for any value, which is how a corrupt row that pins a
	// state of an unrelated branch passed the machine review.
	targetCandidates := map[string]bool{}
	sourceCandidates := map[string]bool{}
	if len(targetStates) == 0 {
		head, err := s.emptyChainBoundary(ctx, pr.TargetBranchID)
		if err != nil {
			return branchBoundaries{}, err
		}
		if head != "" {
			targetCandidates[head] = true
		}
	} else if parent := chainRoot(targetStates).ParentStateID; parent != nil {
		targetCandidates[*parent] = true
	}
	if len(sourceStates) == 0 {
		head, err := s.emptyChainBoundary(ctx, pr.SourceBranchID)
		if err != nil {
			return branchBoundaries{}, err
		}
		if head != "" {
			sourceCandidates[head] = true
		}
	} else if parent := chainRoot(sourceStates).ParentStateID; parent != nil {
		sourceCandidates[*parent] = true
	}
	// Resolve each candidate once PER SIDE: it counts only when it exists
	// and belongs to the project of the chain it is the boundary of —
	// anything else is left out and the engine's provenance checks fail on
	// it. The per-side resolution is not a detail: a state can be a
	// boundary of one chain and not of the other (the two sides may live in
	// different projects, ADR-025), so one shared verdict per id would let
	// the second side's answer overwrite the first's — accepting a boundary
	// for the chain that does not own it, or dropping one from the chain
	// that does.
	sourceProjectID, err := s.sourceProject(ctx, pr)
	if err != nil {
		return branchBoundaries{}, err
	}
	out := branchBoundaries{target: map[string]bool{}, source: map[string]bool{}}
	for id := range targetCandidates {
		ok, err := s.validBoundary(ctx, pr.ProjectID, id)
		if err != nil {
			return branchBoundaries{}, err
		}
		if ok {
			out.target[id] = true
		}
	}
	for id := range sourceCandidates {
		ok, err := s.validBoundary(ctx, sourceProjectID, id)
		if err != nil {
			return branchBoundaries{}, err
		}
		if ok {
			out.source[id] = true
		}
	}
	return out, nil
}

// sourceProject resolves the project the PR's SOURCE branch belongs to — the
// side a source chain's boundary is judged in (ADR-025). The branch row is
// the answer, read by the branch's own id; the PR's project is never
// substituted for it, because assuming the side is exactly the reading this
// task removes.
//
// An unknown source branch answers the PR's project, which decides nothing:
// a branch that cannot be read has no chain and no head, so
// resolveBoundaries produces no candidate for it at all (emptyChainBoundary
// answers "" for the same reason) and the value is never consulted. Any
// other read failure is a store error and is reported as one — an
// unreadable database must not be reported as a blocked proposal.
func (s *Service) sourceProject(ctx context.Context, pr domain.PullRequest) (string, error) {
	projectID, err := s.branches.GetBranchProject(ctx, pr.SourceBranchID)
	switch {
	case err == nil:
		return projectID, nil
	case errors.Is(err, branches.ErrBranchNotFound):
		return pr.ProjectID, nil
	default:
		return "", WrapStoreError(err)
	}
}

// emptyChainBoundary resolves the boundary of a branch that has no states
// of its own: its own head, and nothing else. A branch whose head cannot
// be read — an unknown branch, no head, or a head pointer naming no state
// — yields "" (no boundary at all), so the pin checks fail closed on it
// instead of accepting the pin under review; a store failure is returned
// as such, because an unreadable database must not be reported as a
// blocked proposal.
func (s *Service) emptyChainBoundary(ctx context.Context, branchID string) (string, error) {
	head, err := s.branches.GetBranchHead(ctx, branchID)
	switch {
	case err == nil:
		return head.ID, nil
	case errors.Is(err, branches.ErrBranchNotFound), errors.Is(err, branches.ErrStateNotFound):
		return "", nil
	default:
		return "", WrapStoreError(err)
	}
}

// validBoundary reports whether the state exists and is the project's —
// the two conditions a candidate must meet to be a legitimate exit.
func (s *Service) validBoundary(ctx context.Context, projectID, stateID string) (bool, error) {
	state, err := s.states.GetState(ctx, stateID)
	if err != nil {
		if errors.Is(err, states.ErrStateNotFound) {
			return false, nil // the engine reports the dangling reference
		}
		return false, WrapStoreError(err)
	}
	return state.ProjectID == projectID, nil
}

// chainRoot returns the unique state of the chain whose parent names no
// chain state (the chain's first state); a broken or forked chain has
// several and the engine reports it — here the candidates only need the
// state the walk would exit through, and a broken chain contributes none.
func chainRoot(states []domain.ProjectState) domain.ProjectState {
	inSet := make(map[string]bool, len(states))
	for _, st := range states {
		inSet[st.ID] = true
	}
	for _, st := range states {
		if st.ParentStateID == nil || !inSet[*st.ParentStateID] {
			return st
		}
	}
	return domain.ProjectState{}
}
