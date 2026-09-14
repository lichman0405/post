package validation

import (
	"context"
	"errors"
	"fmt"

	"github.com/lichman0405/post/internal/domain"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// Service assembles branch snapshots from persistence and runs the gate
// engine over them. It owns input validation and the existence-hiding
// project boundary; the engine owns the verdicts.
type Service struct {
	repo SnapshotRepository
	val  *rsgvalidation.Validator
}

// NewService wires the service over the snapshot repository and the gate
// engine.
func NewService(repo SnapshotRepository, val *rsgvalidation.Validator) *Service {
	return &Service{repo: repo, val: val}
}

// ValidateBranch runs gate over the branch's persisted snapshot and
// returns the full report. Release/asset facts are absent (their domains
// arrive later), so those two gates fail their fact checks with the
// facts-missing explanation — use ValidateBranchWithFacts once the
// release/asset domains supply them.
func (s *Service) ValidateBranch(ctx context.Context, projectID, branchID string, gate rsgvalidation.Gate) (rsgvalidation.Report, error) {
	return s.ValidateBranchWithFacts(ctx, projectID, branchID, gate, nil, nil)
}

// ValidateBranchWithFacts is ValidateBranch with the release/asset facts
// the two snapshot gates demand (docs/11 §1/§4). The facts are asserted,
// not derived here: their domain services own them.
func (s *Service) ValidateBranchWithFacts(ctx context.Context, projectID, branchID string, gate rsgvalidation.Gate, release *rsgvalidation.ReleaseFacts, asset *rsgvalidation.AssetFacts) (rsgvalidation.Report, error) {
	if projectID == "" || branchID == "" {
		return rsgvalidation.Report{}, fmt.Errorf("%w: project_id and branch_id are required", ErrValidation)
	}
	if !rsgvalidation.ValidGate(gate) {
		return rsgvalidation.Report{}, fmt.Errorf("%w: unknown gate %q (canonical gates: draft, pr, main, release, asset)", ErrValidation, gate)
	}
	// The project boundary first: a branch of another project is "not
	// found", never "forbidden" — the endpoint must not leak another
	// project's entity existence (docs/45).
	branchProject, err := s.repo.BranchProject(ctx, branchID)
	if err != nil {
		return rsgvalidation.Report{}, mapReadError(err)
	}
	if branchProject != projectID {
		return rsgvalidation.Report{}, ErrBranchNotFound
	}

	head, err := s.repo.GetBranchHead(ctx, branchID)
	switch {
	case errors.Is(err, ErrStateNotFound):
		head = domain.ProjectState{} // an empty branch: nothing to validate yet
	case err != nil:
		return rsgvalidation.Report{}, mapReadError(err)
	}
	states, err := s.repo.ListStates(ctx, branchID)
	if err != nil {
		return rsgvalidation.Report{}, mapReadError(err)
	}
	commits, err := s.repo.ListCommits(ctx, branchID)
	if err != nil {
		return rsgvalidation.Report{}, mapReadError(err)
	}
	// The branch's members: every version row whose state is on the
	// branch's chain. A version is created in exactly one state, so the
	// per-state reads are deduped by row id defensively.
	var headInStates bool
	for _, st := range states {
		if st.ID == head.ID {
			headInStates = true
			break
		}
	}
	if head.ID != "" && !headInStates {
		states = append(states, head)
	}
	seenObjects := make(map[string]bool)
	seenRelations := make(map[string]bool)
	var objects []domain.ScientificObjectVersion
	var relations []domain.RelationVersion
	for _, st := range states {
		ovs, err := s.repo.ListStateObjectVersions(ctx, st.ID)
		if err != nil {
			return rsgvalidation.Report{}, mapReadError(err)
		}
		for _, ov := range ovs {
			if !seenObjects[ov.ID] {
				seenObjects[ov.ID] = true
				objects = append(objects, ov)
			}
		}
		rvs, err := s.repo.ListStateRelationVersions(ctx, st.ID)
		if err != nil {
			return rsgvalidation.Report{}, mapReadError(err)
		}
		for _, rv := range rvs {
			if !seenRelations[rv.ID] {
				seenRelations[rv.ID] = true
				relations = append(relations, rv)
			}
		}
	}
	snap := rsgvalidation.Snapshot{
		ProjectID:        projectID,
		BranchID:         branchID,
		States:           states,
		Commits:          commits,
		ObjectVersions:   objects,
		RelationVersions: relations,
		Release:          release,
		Asset:            asset,
	}
	if head.ID != "" {
		headCopy := head
		snap.Head = &headCopy
	}
	return s.val.Validate(gate, snap), nil
}

// mapReadError translates repository outcomes into the package sentinels;
// everything unexpected becomes ErrStore with the cause kept for the log.
func mapReadError(err error) error {
	if err == nil ||
		errors.Is(err, ErrBranchNotFound) ||
		errors.Is(err, ErrStateNotFound) ||
		errors.Is(err, ErrValidation) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}
