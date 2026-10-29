package rsg

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
)

// BranchList is the project's branch surface: every branch the project
// owns (active, merged and aborted), oldest first. It is the read that
// powers the authoring surface's branch selector — a member must be able
// to see which research paths exist before writing on one or forking a new
// one (specs/api/openapi.yaml, "List branches").
//
// The list answers ONLY branches: the per-branch object slice is the graph
// query's job (Query with BranchID), so a branch row here carries its
// identity, its lifecycle and its head coordinate — never a fabricated
// object count or a summary the list did not read.
type BranchList struct {
	ProjectID string
	Branches  []BranchSummary
}

// BranchSummary is one branch row of the list.
type BranchSummary struct {
	ID string
	// Name is the branch name (unique per project, e.g. "main").
	Name string
	// Purpose is the research-path intent ("" when none was stated).
	Purpose string
	// Visibility is the branch visibility (within the project's preset).
	Visibility string
	// Lifecycle is the branch lifecycle code: active / merged / aborted.
	Lifecycle string
	// IsMain reports the project's canonical integration branch.
	IsMain bool
	// HeadStateID is the branch's current head state ("" when none).
	HeadStateID string
	CreatedAt   time.Time
}

// ListBranches returns the project's branches, oldest first. The read runs
// the same project gate every RSG read runs (projects.Get IS the
// visibility-aware requireRead), so a caller who may not read the project
// answers projects.ErrProjectNotFound — existence hiding, never forbidden
// (docs/45). A store failure aborts: the caller never receives a silently
// partial branch list.
func (s *Service) ListBranches(ctx context.Context, r projects.Reader, projectID string) (BranchList, error) {
	if projectID == "" {
		return BranchList{}, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	if _, err := s.projects.Get(ctx, r, projectID); err != nil {
		return BranchList{}, wrapError(err)
	}
	if s.branches == nil {
		return BranchList{}, fmt.Errorf("%w: no branch port configured", ErrStore)
	}
	branches, err := s.branches.List(ctx, projectID)
	if err != nil {
		return BranchList{}, wrapError(err)
	}
	out := BranchList{ProjectID: projectID, Branches: make([]BranchSummary, 0, len(branches))}
	for _, b := range branches {
		if b.ProjectID != projectID {
			// The store is project-scoped; a foreign row would be a store
			// bug — drop it rather than leak another project's branch.
			continue
		}
		out.Branches = append(out.Branches, BranchSummary{
			ID:          b.ID,
			Name:        b.Name,
			Purpose:     branchPurpose(b),
			Visibility:  string(b.Visibility),
			Lifecycle:   string(b.Lifecycle),
			IsMain:      b.IsMain(),
			HeadStateID: branchBaseStateID(b),
			CreatedAt:   b.CreatedAt,
		})
	}
	// Display order: the integration branch first, then the research paths
	// in flight, then the closed ones; within a rank the store's oldest-
	// first order is kept (stable sort), so new paths append at the end.
	slices.SortStableFunc(out.Branches, func(a, b BranchSummary) int {
		return branchLifecycleRank(a) - branchLifecycleRank(b)
	})
	return out, nil
}

// branchLifecycleRank orders the branch list for display: main first (the
// accepted state the authoring surface defaults to), then the active
// research paths, then the closed ones. Kept here so the wire order is a
// property of the domain read, not of one client.
func branchLifecycleRank(b BranchSummary) int {
	switch {
	case b.IsMain:
		return 0
	case b.Lifecycle == string(domain.BranchLifecycleActive):
		return 1
	case b.Lifecycle == string(domain.BranchLifecycleMerged):
		return 2
	default:
		return 3
	}
}
