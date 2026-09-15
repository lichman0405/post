// Package mergegit is the composition bridge between the merge command and the
// provider-side Git adapter (T0409).
//
// internal/application/merge declares the port it needs (merge.GitMerger) and
// internal/gitprovider owns the adapter that can serve it
// (*GiteaAdapter.MergePullRequest), but the two packages cannot be introduced
// to each other directly: the merge application already imports gitprovider for
// RefGuard, so gitprovider cannot import the application port back (an import
// cycle). The composition root is where the two meet.
//
// It lives here, in a package of its own under cmd/api, rather than as a
// function in package main: the integration/E2E suites compose the same graph
// cmd/api composes, and a bridge that only existed inside main would force them
// to re-implement the adapter — testing something other than what ships.
//
// The bridge resolves the two things the adapter is deliberately not
// project-aware of: the project's provisioned repository
// (git_repository_provisions) and the ref pair the platform planned the merge
// over. Nothing else happens here — no merge logic, no ref writes, NO PUSH:
// main advances only through the provider's own pull-request merge, which is
// what T0302's branch-protection rule permits and nothing else.
package mergegit

import (
	"context"
	"fmt"

	"github.com/lichman0405/post/internal/application/merge"
	"github.com/lichman0405/post/internal/gitprovider"
)

// RepoResolver is the provisioned-repository lookup (the production value is
// gitprovider.PGUserAccessStore).
type RepoResolver interface {
	RepoRef(ctx context.Context, projectID string) (gitprovider.RepoRef, error)
}

// Bridge adapts the provider-side PR merge onto the application's GitMerger
// port.
type Bridge struct {
	merger gitprovider.PullRequestMerger
	repos  RepoResolver
	// title is the provider PR's title when the adapter has to create one.
	// Display only — the platform's own PR title lives in PostgreSQL.
	title string
}

// New wires the bridge. Both arguments are required; a nil one is refused at
// call time (see MergePullRequest) rather than at construction, so a
// deployment can assemble its graph in any order.
func New(merger gitprovider.PullRequestMerger, repos RepoResolver) *Bridge {
	return &Bridge{merger: merger, repos: repos, title: "POST research pull request merge"}
}

// MergePullRequest implements merge.GitMerger: resolve the repository, then
// merge the ref pair the plan named, pinned to the commits the plan was
// computed over.
//
// A nil bridge (or a nil port inside it) is refused rather than dereferenced:
// the service treats a nil Git port as "the Git half has not happened" and
// records a pending step, and this keeps that meaning even if some wiring path
// hands it a typed nil (merge.NewService normalizes the same case — see
// absentToNil).
func (b *Bridge) MergePullRequest(ctx context.Context, in merge.GitMergeRequest) (merge.GitMergeResult, error) {
	if b == nil || b.merger == nil || b.repos == nil {
		return merge.GitMergeResult{}, fmt.Errorf("%w: the provider merge adapter is not wired", merge.ErrStore)
	}
	repo, err := b.repos.RepoRef(ctx, in.ProjectID)
	if err != nil {
		return merge.GitMergeResult{}, fmt.Errorf("resolve the project's repository: %w", err)
	}
	res, err := b.merger.MergePullRequest(ctx, gitprovider.PullRequestMergeSpec{
		Repository: gitprovider.Repository{Owner: repo.Owner, Name: repo.Name},
		Title:      b.title,
		TargetRef:  in.TargetRef,
		SourceRef:  in.SourceRef,
		TargetSHA:  in.TargetSHA,
		SourceSHA:  in.SourceSHA,
	})
	if err != nil {
		return merge.GitMergeResult{}, err
	}
	return merge.GitMergeResult{SHA: res.SHA, Actor: res.Actor}, nil
}
