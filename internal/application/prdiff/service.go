package prdiff

import (
	"context"
	"errors"
	"fmt"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/diffs"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/rsg/diff"
)

// Service assembles one PR's Research State Diff from the PR row, the
// target branch's head and the T0401 diff use case. Reads only: nothing
// here writes, and nothing is stored — the PR's pins are fixed and the
// rows behind them append-only, so the same PR always renders the same
// diff (the engine's canonical serialization, internal/rsg/diff).
type Service struct {
	prs      PRReader
	branches BranchHeadReader
	diffs    Differ
}

// NewService wires the service.
func NewService(prs PRReader, heads BranchHeadReader, differ Differ) *Service {
	return &Service{prs: prs, branches: heads, diffs: differ}
}

// PullRequestDiff computes the PR's three-way diff:
//
//	base   = pr.BaseStateID      (fixed at creation; never re-derived)
//	source = pr.ProposedStateID  (the pinned proposal; moves only through
//	                              the explicit head refresh)
//	target = the target branch's current head state — the concurrent side
//	          of the three-way, reported, never judged
//
// The three states are resolved server-side from the PR row and the
// branch row; no caller-supplied state id takes part. Failures map to
// this package's sentinels: a missing PR to ErrPullRequestNotFound, a
// state that no longer resolves to ErrStateNotFound, an adapter failure
// (or stored rows the engine cannot render) to ErrStore.
func (s *Service) PullRequestDiff(ctx context.Context, projectID string, number int64) (*diff.Diff, error) {
	if projectID == "" {
		return nil, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	if number < 1 {
		return nil, fmt.Errorf("%w: number must be positive", ErrValidation)
	}
	if s.prs == nil || s.branches == nil || s.diffs == nil {
		return nil, fmt.Errorf("%w: the diff read is not wired", ErrStore)
	}
	pr, err := s.prs.GetPullRequest(ctx, projectID, number)
	if err != nil {
		return nil, wrapStoreError(err)
	}
	target, err := s.branches.GetBranchHead(ctx, pr.TargetBranchID)
	if err != nil {
		if errors.Is(err, branches.ErrStateNotFound) {
			// The target branch carries no head state: there is no third
			// state to compare against, and no diff to render. The
			// outcome is the missing-state one, not a caller error.
			return nil, fmt.Errorf("%w: target branch %s has no head state", ErrStateNotFound, pr.TargetBranchID)
		}
		return nil, wrapStoreError(err)
	}
	d, err := s.diffs.Diff(ctx, diffs.Params{
		ProjectID:     pr.ProjectID,
		BaseStateID:   pr.BaseStateID,
		SourceStateID: pr.ProposedStateID,
		TargetStateID: target.ID,
	})
	if err != nil {
		return nil, wrapDiffError(err)
	}
	return d, nil
}

// wrapStoreError maps a dependency outcome onto this package's
// vocabulary. A not-found sentinel maps to the matching outcome; a
// dependency validation failure is NOT a caller error here (every state
// id is resolved server-side), so it is reported as a store failure — the
// stored PR row names something the store cannot resolve.
func wrapStoreError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pullrequests.ErrPullRequestNotFound) {
		return ErrPullRequestNotFound
	}
	if errors.Is(err, ErrValidation) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}

// wrapDiffError maps the diff use case's outcomes. A resolution failure
// on a pin the PR row itself carries is a missing state, never an input
// error: nothing the caller sent took part in it. The diff package's
// validation failure (a state that does not belong to the PR's project)
// is stored-data corruption by the same argument, so it is a store
// failure here too.
func wrapDiffError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, diffs.ErrStateNotFound) {
		return fmt.Errorf("%w: %v", ErrStateNotFound, err)
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}
