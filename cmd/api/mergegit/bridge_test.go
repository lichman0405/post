package mergegit

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/merge"
	"github.com/lichman0405/post/internal/gitprovider"
)

// The bridge is the only place the application's GitMerger port meets the
// provider adapter, and it is what the integration/E2E suites compose — so its
// own unit tests pin the two things the journeys would otherwise be asserting
// through several layers: the EXACT spec the adapter receives, and the
// typed-nil behaviour (a bridge that dereferenced a nil adapter would panic
// inside the merge's post-commit saga, after PostgreSQL had already accepted
// the state).

const (
	bridgeProjectID = "11111111-2222-4333-8444-555555555555"
	bridgeSourceSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	bridgeTargetSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	bridgeMergeSHA  = "cccccccccccccccccccccccccccccccccccccccc"
)

// fakeMerger records the spec it was handed.
type fakeMerger struct {
	calls int
	spec  gitprovider.PullRequestMergeSpec
	out   gitprovider.PullRequestMergeResult
	err   error
}

func (f *fakeMerger) MergePullRequest(_ context.Context, spec gitprovider.PullRequestMergeSpec) (gitprovider.PullRequestMergeResult, error) {
	f.calls++
	f.spec = spec
	if f.err != nil {
		return gitprovider.PullRequestMergeResult{}, f.err
	}
	if f.out.SHA == "" {
		return gitprovider.PullRequestMergeResult{SHA: bridgeMergeSHA, Actor: "post-git-svc"}, nil
	}
	return f.out, nil
}

// fakeRepos serves the provisioned repository.
type fakeRepos struct {
	calls int
	gotID string
	ref   gitprovider.RepoRef
	err   error
}

func (f *fakeRepos) RepoRef(_ context.Context, projectID string) (gitprovider.RepoRef, error) {
	f.calls++
	f.gotID = projectID
	if f.err != nil {
		return gitprovider.RepoRef{}, f.err
	}
	return f.ref, nil
}

func bridgeRequest() merge.GitMergeRequest {
	return merge.GitMergeRequest{
		ProjectID: bridgeProjectID,
		Number:    12,
		TargetRef: "refs/heads/main",
		SourceRef: "refs/heads/annealing",
		TargetSHA: bridgeTargetSHA,
		SourceSHA: bridgeSourceSHA,
	}
}

// TestBridgeHandsTheAdapterTheResolvedRepositoryAndTheRefPair: the adapter is
// not project-aware, so everything project-shaped must arrive resolved — and
// the ref pair with its two pins must arrive verbatim, because the adapter
// refuses a provider merge whose refs have moved past them.
func TestBridgeHandsTheAdapterTheResolvedRepositoryAndTheRefPair(t *testing.T) {
	merger := &fakeMerger{}
	repos := &fakeRepos{ref: gitprovider.RepoRef{
		ProjectID: bridgeProjectID, Owner: "post-git-svc", Name: "p-" + bridgeProjectID,
	}}
	got, err := New(merger, repos).MergePullRequest(context.Background(), bridgeRequest())
	if err != nil {
		t.Fatalf("MergePullRequest: %v", err)
	}
	if repos.calls != 1 || repos.gotID != bridgeProjectID {
		t.Fatalf("repository lookups = %d for %q, want one for %q", repos.calls, repos.gotID, bridgeProjectID)
	}
	if merger.calls != 1 {
		t.Fatalf("adapter calls = %d, want 1", merger.calls)
	}
	if merger.spec.Repository.Owner != "post-git-svc" || merger.spec.Repository.Name != "p-"+bridgeProjectID {
		t.Errorf("adapter repository = %+v, want the provisioned repository", merger.spec.Repository)
	}
	if merger.spec.TargetRef != "refs/heads/main" || merger.spec.SourceRef != "refs/heads/annealing" {
		t.Errorf("adapter refs = %q -> %q, want the plan's pair", merger.spec.SourceRef, merger.spec.TargetRef)
	}
	if merger.spec.TargetSHA != bridgeTargetSHA || merger.spec.SourceSHA != bridgeSourceSHA {
		t.Errorf("adapter pins = %q/%q, want the plan's commits", merger.spec.TargetSHA, merger.spec.SourceSHA)
	}
	if merger.spec.Title == "" {
		t.Error("adapter title is empty: the provider pull request the bridge may have to create needs one")
	}
	if got.SHA != bridgeMergeSHA || got.Actor != "post-git-svc" {
		t.Errorf("result = %+v, want the provider's own answer (sha + the identity that merged)", got)
	}
}

// TestBridgeRefusesANilPortInsteadOfPanicking: a bridge with nothing behind it
// reports an unwired adapter. The merge service then records an unfinished —
// not a failed-with-a-panic — Git step, which is what keeps the database truth
// and a half-wired deployment in a state an operator can read.
func TestBridgeRefusesANilPortInsteadOfPanicking(t *testing.T) {
	var typedNil *Bridge
	cases := []struct {
		name string
		brg  *Bridge
	}{
		{"typed nil bridge", typedNil},
		{"nil adapter", New(nil, &fakeRepos{})},
		{"nil resolver", New(&fakeMerger{}, nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.brg.MergePullRequest(context.Background(), bridgeRequest())
			if !errors.Is(err, merge.ErrStore) {
				t.Fatalf("err = %v, want it to be a store-shaped refusal the saga records as unfinished", err)
			}
		})
	}
}

// TestBridgeReportsARepositoryResolutionFailureWithoutMerging: when the
// project has no provisioned repository the adapter must never be called — a
// merge without a repository is not a merge of some other repository.
func TestBridgeReportsARepositoryResolutionFailureWithoutMerging(t *testing.T) {
	merger := &fakeMerger{}
	repos := &fakeRepos{err: gitprovider.ErrRepoNotProvisioned}
	_, err := New(merger, repos).MergePullRequest(context.Background(), bridgeRequest())
	if !errors.Is(err, gitprovider.ErrRepoNotProvisioned) {
		t.Fatalf("err = %v, want the resolver's own failure carried through", err)
	}
	if !strings.Contains(err.Error(), "repository") {
		t.Errorf("err = %v, want it to name what could not be resolved", err)
	}
	if merger.calls != 0 {
		t.Fatalf("adapter calls = %d, want 0 without a repository", merger.calls)
	}
}

// TestBridgeCarriesTheAdaptersRefusalVerbatim: the adapter's refusals are the
// saga's evidence (a moved ref, an empty pull request, a refused identity), so
// the bridge must not paraphrase or swallow them.
func TestBridgeCarriesTheAdaptersRefusalVerbatim(t *testing.T) {
	refusal := errors.New("gitea: refusing to merge: the pull request is empty")
	merger := &fakeMerger{err: refusal}
	repos := &fakeRepos{ref: gitprovider.RepoRef{Owner: "post-git-svc", Name: "p-1"}}
	got, err := New(merger, repos).MergePullRequest(context.Background(), bridgeRequest())
	if !errors.Is(err, refusal) {
		t.Fatalf("err = %v, want the adapter's own refusal", err)
	}
	if got.SHA != "" || got.Actor != "" {
		t.Fatalf("result = %+v, want nothing: a refused merge must not report a sha", got)
	}
}
