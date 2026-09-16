package explorehttp

import (
	"context"
	"fmt"

	appcontribution "github.com/lichman0405/post/internal/application/contribution"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/contribution"
	"github.com/lichman0405/post/internal/domain"
)

// The adapters that reuse the platform's existing public reads. Three of the
// Explore sections are already served somewhere else, and this file is the
// whole of the glue: no section's disclosure rule is restated here, because
// a second copy of a rule in a layer nobody tests is how two surfaces start
// disagreeing (T0709's own warning).

// ProjectSource is the Projects section: the public project list
// (internal/application/projects, T0106 — a private row can never reach this
// result set because the store's query carries the predicate).
//
// List is called with the ZERO projects.Reader: no actor, no membership
// union, so the answer is the public half and nothing else. The other half
// (a caller's own private projects) belongs to the signed-in project
// directory, not to the network's index.
type ProjectSource struct {
	Service *projects.Service
}

// ListPublicProjects returns the public projects, newest first.
func (s ProjectSource) ListPublicProjects(ctx context.Context) ([]domain.Project, error) {
	if s.Service == nil {
		return nil, fmt.Errorf("explore: project source is not wired")
	}
	return s.Service.List(ctx, projects.Reader{})
}

// AssetPageReader is the asset hub's browse read (T0709). The production
// value is *persistence.AssetPageStore.
type AssetPageReader interface {
	ListBrowseAssets(ctx context.Context, filter *assets.Type) ([]assets.BrowseRowState, error)
}

// AssetSource is the Assets section: the asset hub's own list, built by the
// hub's own rule.
//
// The unfiltered browse list is taken (nil filter: every type) and
// internal/assets.BuildBrowse decides what may be rendered — an asset is in
// it when it has a public version, and it names its project only when that
// project is public. Building it here rather than re-deriving it is what
// makes /explore's assets tab and /assets one index instead of two opinions.
type AssetSource struct {
	Pages AssetPageReader
}

// ListPublicAssets returns the asset hub's public browse items.
func (s AssetSource) ListPublicAssets(ctx context.Context) ([]assets.BrowseItem, error) {
	if s.Pages == nil {
		return nil, fmt.Errorf("explore: asset source is not wired")
	}
	rows, err := s.Pages.ListBrowseAssets(ctx, nil)
	if err != nil {
		return nil, err
	}
	return assets.BuildBrowse(rows, nil).Assets, nil
}

// ContributionSource is the Open Contributions section: the open-network
// view T0803 built (internal/application/contribution.Service.ListPublic over
// ListPublicOpportunities) — publicized AND open rows, which only the
// explicit, audited publicize action can produce (docs/12 §3).
type ContributionSource struct {
	Service *appcontribution.Service
}

// ListPublicContributions returns the publicized, open opportunities.
func (s ContributionSource) ListPublicContributions(ctx context.Context) ([]contribution.ContributionOpportunity, error) {
	if s.Service == nil {
		return nil, fmt.Errorf("explore: contribution source is not wired")
	}
	return s.Service.ListPublic(ctx)
}

// Sources composes the six reads into the one explore.Reader the service
// takes: the three reuse adapters above, and *Store (store.go) for the three
// sections that had no read before this task.
//
// The composition is embedding, so each read keeps the name and the
// documentation of the thing that implements it, and a missing piece is a
// nil field that fails loudly on the first request rather than a method that
// silently answers empty.
type Sources struct {
	ProjectSource
	AssetSource
	ContributionSource
	*Store
}
