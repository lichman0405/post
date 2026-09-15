package gitprovider_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/gitprovider"
)

// The provider-side PR merge (T0409) against a scripted Gitea surface: the
// find-or-create of the provider PR, the pins the platform planned against,
// the merge request itself (an ordinary merge pinned to the head, never a
// push), the read-back of the outcome, and the refusals that must stop
// before the merge call. The git runner is scripted with zero steps in every
// test, so ANY git invocation — a push above all — fails the test loudly.

const (
	mergeRepoOwner = "post-git-svc"
	mergeRepoName  = "p-post-abc"
	mergeRepoBase  = "/api/v1/repos/" + mergeRepoOwner + "/" + mergeRepoName
	beforeSHA      = "1111111111111111111111111111111111111111"
	sourceSHA      = "2222222222222222222222222222222222222222"
	mergeCommitSHA = "3333333333333333333333333333333333333333"
)

func mergeRepo() gitprovider.Repository {
	return gitprovider.Repository{Owner: mergeRepoOwner, Name: mergeRepoName}
}

func mergeSpec() gitprovider.PullRequestMergeSpec {
	return gitprovider.PullRequestMergeSpec{
		Repository: mergeRepo(),
		TargetRef:  "refs/heads/main",
		SourceRef:  "refs/heads/feature",
	}
}

// openPRBody is the list answer carrying one open PR on the ref pair.
func openPRBody(number int64, head, base string) string {
	return `[{"number":` + itoa(number) + `,"state":"open","merged":false,` +
		`"head":{"ref":"feature","sha":"` + head + `"},` +
		`"base":{"ref":"main","sha":"` + base + `"}}]`
}

// mergedPRBody is the provider's PR read-back after the merge.
func mergedPRBody(number int64, mergeSHA, actor string) string {
	return `{"number":` + itoa(number) + `,"state":"closed","merged":true,"merge_commit_sha":"` + mergeSHA + `",` +
		`"merged_by":{"login":"` + actor + `"},"head":{"ref":"feature","sha":"` + sourceSHA + `"},` +
		`"base":{"ref":"main","sha":"` + beforeSHA + `"}}`
}

// callsTo counts the recorded requests by method and EXACT decoded path (the
// fake's own helper matches by prefix, which cannot tell the create from the
// merge on this surface).
func callsTo(f *fakeGitea, method, path string) []giteaRequest {
	var out []giteaRequest
	for _, r := range f.reqs {
		if r.method == method && r.path == path {
			out = append(out, r)
		}
	}
	return out
}

// TestGiteaMergePullRequestMergesTheExistingPullRequest: the whole happy
// path in one script — the provider PR already exists (found by the ref
// pair), the pins agree, the merge is an ordinary "Do":"merge" pinned to the
// head, and the result is the target ref read back AFTER the merge plus the
// identity that performed it.
func TestGiteaMergePullRequestMergesTheExistingPullRequest(t *testing.T) {
	var refReads int
	f := &fakeGitea{routes: []giteaRoute{
		// The PR read-back route is registered before the list route: the
		// fake matches the first route whose prefix fits, and "/pulls" is a
		// prefix of "/pulls/12".
		{method: http.MethodGet, prefix: mergeRepoBase + "/pulls/12", status: http.StatusOK,
			body: mergedPRBody(12, mergeCommitSHA, "post-git-svc")},
		{method: http.MethodGet, prefix: mergeRepoBase + "/pulls", status: http.StatusOK,
			body: openPRBody(12, sourceSHA, beforeSHA)},
		{method: http.MethodGet, prefix: mergeRepoBase + "/git/refs/heads/main", serve: func(w http.ResponseWriter, _ *http.Request) bool {
			refReads++
			if refReads == 1 {
				_, _ = w.Write([]byte(refsBody("main", beforeSHA)))
			} else {
				_, _ = w.Write([]byte(refsBody("main", mergeCommitSHA)))
			}
			return true
		}},
		{method: http.MethodPost, prefix: mergeRepoBase + "/pulls/12/merge", status: http.StatusOK},
	}}
	a := newGiteaAdapter(t, f, gitprovider.WithGitRunner(scriptedGit(t)))

	got, err := a.MergePullRequest(testCtx(t), gitprovider.PullRequestMergeSpec{
		Repository: mergeRepo(),
		TargetRef:  "refs/heads/main",
		SourceRef:  "refs/heads/feature",
		TargetSHA:  beforeSHA,
		SourceSHA:  sourceSHA,
	})
	if err != nil {
		t.Fatalf("MergePullRequest: %v", err)
	}
	if got.Number != 12 || got.SHA != mergeCommitSHA || got.Actor != "post-git-svc" {
		t.Errorf("merge result = %+v, want PR 12 at %s merged by post-git-svc", got, mergeCommitSHA)
	}

	merges := callsTo(f, http.MethodPost, mergeRepoBase+"/pulls/12/merge")
	if len(merges) != 1 {
		t.Fatalf("merge calls = %d, want exactly 1", len(merges))
	}
	if merges[0].body["Do"] != "merge" {
		t.Errorf("merge body Do = %v, want merge", merges[0].body["Do"])
	}
	if merges[0].body["head_commit_id"] != sourceSHA {
		t.Errorf("merge body head_commit_id = %v, want the PR's head %s", merges[0].body["head_commit_id"], sourceSHA)
	}
	if creates := callsTo(f, http.MethodPost, mergeRepoBase+"/pulls"); len(creates) != 0 {
		t.Errorf("the adapter created %d PRs although one was open for the pair", len(creates))
	}
	lists := callsTo(f, http.MethodGet, mergeRepoBase+"/pulls")
	if len(lists) != 1 {
		t.Fatalf("PR list reads = %d, want exactly 1", len(lists))
	}
	if !strings.Contains(lists[0].query, "state=open") {
		t.Errorf("PR list query = %q, want it to ask only for open PRs", lists[0].query)
	}
}

// TestGiteaMergePullRequestCreatesTheProviderPullRequest: with no open PR for
// the ref pair the adapter creates one (the platform's PR is a row in
// PostgreSQL; the provider needs its own object to merge) and then merges it.
func TestGiteaMergePullRequestCreatesTheProviderPullRequest(t *testing.T) {
	var refReads int
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: mergeRepoBase + "/pulls/31", status: http.StatusOK,
			body: mergedPRBody(31, mergeCommitSHA, "post-git-svc")},
		{method: http.MethodGet, prefix: mergeRepoBase + "/pulls", status: http.StatusOK, body: "[]"},
		{method: http.MethodGet, prefix: mergeRepoBase + "/git/refs/heads/main", serve: func(w http.ResponseWriter, _ *http.Request) bool {
			refReads++
			if refReads == 1 {
				_, _ = w.Write([]byte(refsBody("main", beforeSHA)))
			} else {
				_, _ = w.Write([]byte(refsBody("main", mergeCommitSHA)))
			}
			return true
		}},
		{method: http.MethodPost, prefix: mergeRepoBase + "/pulls/31/merge", status: http.StatusOK},
		{method: http.MethodPost, prefix: mergeRepoBase + "/pulls", status: http.StatusCreated,
			body: `{"number":31,"state":"open","merged":false,` +
				`"head":{"ref":"feature","sha":"` + sourceSHA + `"},` +
				`"base":{"ref":"main","sha":"` + beforeSHA + `"}}`},
	}}
	a := newGiteaAdapter(t, f, gitprovider.WithGitRunner(scriptedGit(t)))

	got, err := a.MergePullRequest(testCtx(t), mergeSpec())
	if err != nil {
		t.Fatalf("MergePullRequest: %v", err)
	}
	if got.Number != 31 || got.SHA != mergeCommitSHA {
		t.Errorf("merge result = %+v, want PR 31 at %s", got, mergeCommitSHA)
	}
	created := callsTo(f, http.MethodPost, mergeRepoBase+"/pulls")
	if len(created) != 1 {
		t.Fatalf("PR creates = %d, want exactly 1", len(created))
	}
	if created[0].body["head"] != "feature" || created[0].body["base"] != "main" {
		t.Errorf("PR create body = %v, want head feature base main", created[0].body)
	}
	if title, _ := created[0].body["title"].(string); !strings.Contains(title, "feature") || !strings.Contains(title, "main") {
		t.Errorf("PR create title = %q, want it to name the ref pair", title)
	}
}

// TestGiteaMergePullRequestRefusesAMovedTarget: the target ref moved between
// the plan and the merge. The refusal happens BEFORE any PR is looked for —
// a merge that would land on a different target than the plan was computed
// over must not even open a provider PR.
func TestGiteaMergePullRequestRefusesAMovedTarget(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: mergeRepoBase + "/git/refs/heads/main", status: http.StatusOK,
			body: refsBody("main", mergeCommitSHA)},
	}}
	a := newGiteaAdapter(t, f, gitprovider.WithGitRunner(scriptedGit(t)))

	_, err := a.MergePullRequest(testCtx(t), gitprovider.PullRequestMergeSpec{
		Repository: mergeRepo(),
		TargetRef:  "refs/heads/main",
		SourceRef:  "refs/heads/feature",
		TargetSHA:  beforeSHA,
	})
	if !errors.Is(err, gitprovider.ErrConflict) {
		t.Fatalf("MergePullRequest on a moved target = %v, want ErrConflict", err)
	}
	if len(callsTo(f, http.MethodPost, mergeRepoBase+"/pulls")) != 0 {
		t.Errorf("a refused merge still created a provider PR")
	}
	if len(callsTo(f, http.MethodPost, mergeRepoBase+"/pulls/12/merge")) != 0 {
		t.Errorf("a refused merge still asked for a merge")
	}
}

// TestGiteaMergePullRequestRefusesAMovedSource: the same fence on the source
// side — the provider's PR head is not the commit the plan was computed over,
// so nothing merges.
func TestGiteaMergePullRequestRefusesAMovedSource(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: mergeRepoBase + "/git/refs/heads/main", status: http.StatusOK,
			body: refsBody("main", beforeSHA)},
		{method: http.MethodGet, prefix: mergeRepoBase + "/pulls", status: http.StatusOK,
			body: openPRBody(12, mergeCommitSHA, beforeSHA)}, // the PR's head is a different commit
	}}
	a := newGiteaAdapter(t, f, gitprovider.WithGitRunner(scriptedGit(t)))

	_, err := a.MergePullRequest(testCtx(t), gitprovider.PullRequestMergeSpec{
		Repository: mergeRepo(),
		TargetRef:  "refs/heads/main",
		SourceRef:  "refs/heads/feature",
		SourceSHA:  sourceSHA,
	})
	if !errors.Is(err, gitprovider.ErrConflict) {
		t.Fatalf("MergePullRequest on a moved source = %v, want ErrConflict", err)
	}
	if len(callsTo(f, http.MethodPost, mergeRepoBase+"/pulls/12/merge")) != 0 {
		t.Errorf("a refused merge still asked for a merge")
	}
}

// TestGiteaMergePullRequestReportsAnAlreadyMergedPullRequest: the platform
// retries the Git step of a merge that already landed at the provider (the
// first attempt merged and the response was lost, or somebody else merged).
// There is no open PR for the pair any more, the provider refuses to create a
// second one, and the adapter has to converge on the merged PR — reading the
// provider's own facts (the merge commit and the identity that made it) so
// RefGuard judges the identity instead of the platform silently claiming a
// merge its own service did not make. The ref pair carries the identity here:
// the pins are absent because a replay has no plan left to pin against.
func TestGiteaMergePullRequestReportsAnAlreadyMergedPullRequest(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		// The read-back route goes first: the fake matches by prefix and
		// "/pulls" is a prefix of "/pulls/9".
		{method: http.MethodGet, prefix: mergeRepoBase + "/pulls/9", status: http.StatusOK,
			body: mergedPRBody(9, mergeCommitSHA, "postadmin")},
		{method: http.MethodGet, prefix: mergeRepoBase + "/pulls", serve: func(w http.ResponseWriter, r *http.Request) bool {
			if strings.Contains(r.URL.RawQuery, "state=open") {
				_, _ = w.Write([]byte("[]")) // nothing open for the pair
			} else {
				_, _ = w.Write([]byte(`[` + mergedPRBody(9, mergeCommitSHA, "postadmin") + `]`))
			}
			return true
		}},
		{method: http.MethodGet, prefix: mergeRepoBase + "/git/refs/heads/main", status: http.StatusOK,
			body: refsBody("main", mergeCommitSHA)},
		{method: http.MethodPost, prefix: mergeRepoBase + "/pulls/9/merge", status: http.StatusMethodNotAllowed,
			body: `{"message":"Pull request has already been merged."}`},
		{method: http.MethodPost, prefix: mergeRepoBase + "/pulls", status: http.StatusConflict,
			body: `{"message":"pull request already exists for these branches"}`},
	}}
	a := newGiteaAdapter(t, f, gitprovider.WithGitRunner(scriptedGit(t)))

	got, err := a.MergePullRequest(testCtx(t), mergeSpec())
	if err != nil {
		t.Fatalf("MergePullRequest replaying an already-merged PR: %v", err)
	}
	if got.SHA != mergeCommitSHA || got.Actor != "postadmin" {
		t.Errorf("result = %+v, want the provider's own merge %s by postadmin", got, mergeCommitSHA)
	}
	// The adapter never pretends it merged: it asked, the provider said the PR
	// was already merged, and only the provider's own record was reported.
	if merges := callsTo(f, http.MethodPost, mergeRepoBase+"/pulls/9/merge"); len(merges) != 1 {
		t.Errorf("merge attempts = %d, want exactly 1", len(merges))
	}
}

// TestGiteaMergePullRequestRefusesAnUnmergedClosedPullRequest: the pair's
// provider PR was closed WITHOUT merging. The provider still refuses a second
// PR for the pair, and a closed PR is not a merge — the adapter refuses
// rather than reporting somebody's abandoned pull request as an accepted
// change.
func TestGiteaMergePullRequestRefusesAnUnmergedClosedPullRequest(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: mergeRepoBase + "/pulls", serve: func(w http.ResponseWriter, r *http.Request) bool {
			if strings.Contains(r.URL.RawQuery, "state=open") {
				_, _ = w.Write([]byte("[]"))
			} else {
				_, _ = w.Write([]byte(`[{"number":4,"state":"closed","merged":false,` +
					`"head":{"ref":"feature","sha":"` + sourceSHA + `"},` +
					`"base":{"ref":"main","sha":"` + beforeSHA + `"}}]`))
			}
			return true
		}},
		{method: http.MethodGet, prefix: mergeRepoBase + "/git/refs/heads/main", status: http.StatusOK,
			body: refsBody("main", beforeSHA)},
		{method: http.MethodPost, prefix: mergeRepoBase + "/pulls", status: http.StatusConflict,
			body: `{"message":"pull request already exists for these branches"}`},
	}}
	a := newGiteaAdapter(t, f, gitprovider.WithGitRunner(scriptedGit(t)))

	_, err := a.MergePullRequest(testCtx(t), mergeSpec())
	if !errors.Is(err, gitprovider.ErrConflict) {
		t.Fatalf("MergePullRequest with a closed, unmerged provider PR = %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("refusal = %v, want the provider's own message", err)
	}
}

// TestGiteaMergePullRequestRefusesSomethingToNothing: the ref pair maps onto
// a PR the provider refuses to create (409/422 — an empty diff, a closed
// branch), and the refusal is the provider's, mapped onto the package
// sentinel. It is never reported as a merge.
func TestGiteaMergePullRequestRefusesSomethingToNothing(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: mergeRepoBase + "/git/refs/heads/main", status: http.StatusOK,
			body: refsBody("main", beforeSHA)},
		{method: http.MethodGet, prefix: mergeRepoBase + "/pulls", status: http.StatusOK, body: "[]"},
		{method: http.MethodPost, prefix: mergeRepoBase + "/pulls", status: http.StatusUnprocessableEntity,
			body: `{"message":"This pull request is empty"}`},
	}}
	a := newGiteaAdapter(t, f, gitprovider.WithGitRunner(scriptedGit(t)))

	_, err := a.MergePullRequest(testCtx(t), mergeSpec())
	if !errors.Is(err, gitprovider.ErrConflict) {
		t.Fatalf("MergePullRequest with nothing to merge = %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "This pull request is empty") {
		t.Errorf("refusal = %v, want the provider's own message", err)
	}
}

// TestGiteaMergePullRequestRetriesAProviderThatHasNotDecidedYet: the provider
// refuses the merge with its own "not mergeable yet" answer and keeps the PR
// unmerged (Gitea computes mergeability in the background and answers 405
// "Please try again later" until the check has run). The adapter must ask
// again instead of reporting a merge that did not happen — and the second
// attempt must carry the same head pin as the first.
func TestGiteaMergePullRequestRetriesAProviderThatHasNotDecidedYet(t *testing.T) {
	mergeCalls, prReads := 0, 0
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: mergeRepoBase + "/pulls/12", serve: func(w http.ResponseWriter, _ *http.Request) bool {
			prReads++
			// Unmerged while the provider has not decided; merged once it has.
			if prReads <= 2 {
				_, _ = w.Write([]byte(`{"number":12,"state":"open","merged":false,"mergeable":false,` +
					`"head":{"ref":"feature","sha":"` + sourceSHA + `"},` +
					`"base":{"ref":"main","sha":"` + beforeSHA + `"}}`))
				return true
			}
			_, _ = w.Write([]byte(mergedPRBody(12, mergeCommitSHA, "post-git-svc")))
			return true
		}},
		{method: http.MethodGet, prefix: mergeRepoBase + "/pulls", status: http.StatusOK,
			body: openPRBody(12, sourceSHA, beforeSHA)},
		{method: http.MethodGet, prefix: mergeRepoBase + "/git/refs/heads/main", serve: func(w http.ResponseWriter, _ *http.Request) bool {
			if mergeCalls == 0 {
				_, _ = w.Write([]byte(refsBody("main", beforeSHA)))
			} else {
				_, _ = w.Write([]byte(refsBody("main", mergeCommitSHA)))
			}
			return true
		}},
		{method: http.MethodPost, prefix: mergeRepoBase + "/pulls/12/merge", serve: func(w http.ResponseWriter, _ *http.Request) bool {
			mergeCalls++
			if mergeCalls == 1 {
				w.WriteHeader(http.StatusMethodNotAllowed)
				_, _ = w.Write([]byte(`{"message":"Please try again later"}`))
				return true
			}
			_, _ = w.Write([]byte(`{}`))
			return true
		}},
	}}
	a := newGiteaAdapter(t, f, gitprovider.WithGitRunner(scriptedGit(t)),
		gitprovider.WithMergeRetryWait(time.Millisecond))

	got, err := a.MergePullRequest(testCtx(t), gitprovider.PullRequestMergeSpec{
		Repository: mergeRepo(),
		TargetRef:  "refs/heads/main",
		SourceRef:  "refs/heads/feature",
		TargetSHA:  beforeSHA,
		SourceSHA:  sourceSHA,
	})
	if err != nil {
		t.Fatalf("MergePullRequest over a provider that had not decided yet: %v", err)
	}
	if got.Number != 12 || got.SHA != mergeCommitSHA || got.Actor != "post-git-svc" {
		t.Errorf("merge result = %+v, want PR 12 at %s merged by post-git-svc", got, mergeCommitSHA)
	}
	if mergeCalls != 2 {
		t.Errorf("merge attempts = %d, want 2 (the refusal, then the merge)", mergeCalls)
	}
	merges := callsTo(f, http.MethodPost, mergeRepoBase+"/pulls/12/merge")
	for i, m := range merges {
		if m.body["head_commit_id"] != sourceSHA {
			t.Errorf("merge attempt %d pinned head_commit_id to %v, want %s", i+1, m.body["head_commit_id"], sourceSHA)
		}
	}
}

// TestGiteaMergePullRequestReportsARefusalThatSurvivesEveryAttempt: the
// provider keeps refusing and keeps reporting the PR unmerged — the adapter's
// retry is BOUNDED and the provider's own words are what the caller gets. A
// retry must never turn a refusal into a merge, and it must not loop.
func TestGiteaMergePullRequestReportsARefusalThatSurvivesEveryAttempt(t *testing.T) {
	mergeCalls := 0
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: mergeRepoBase + "/pulls/12", status: http.StatusOK,
			body: `{"number":12,"state":"open","merged":false,"mergeable":false,` +
				`"head":{"ref":"feature","sha":"` + sourceSHA + `"},` +
				`"base":{"ref":"main","sha":"` + beforeSHA + `"}}`},
		{method: http.MethodGet, prefix: mergeRepoBase + "/pulls", status: http.StatusOK,
			body: openPRBody(12, sourceSHA, beforeSHA)},
		{method: http.MethodGet, prefix: mergeRepoBase + "/git/refs/heads/main", status: http.StatusOK,
			body: refsBody("main", beforeSHA)},
		{method: http.MethodPost, prefix: mergeRepoBase + "/pulls/12/merge", serve: func(w http.ResponseWriter, _ *http.Request) bool {
			mergeCalls++
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = w.Write([]byte(`{"message":"Please try again later"}`))
			return true
		}},
	}}
	a := newGiteaAdapter(t, f, gitprovider.WithGitRunner(scriptedGit(t)),
		gitprovider.WithMergeRetryWait(time.Millisecond))

	_, err := a.MergePullRequest(testCtx(t), gitprovider.PullRequestMergeSpec{
		Repository: mergeRepo(),
		TargetRef:  "refs/heads/main",
		SourceRef:  "refs/heads/feature",
		TargetSHA:  beforeSHA,
		SourceSHA:  sourceSHA,
	})
	if !errors.Is(err, gitprovider.ErrUnavailable) {
		t.Fatalf("MergePullRequest against a provider that never merges = %v, want ErrUnavailable", err)
	}
	if !strings.Contains(err.Error(), "Please try again later") {
		t.Errorf("refusal = %v, want the provider's own message", err)
	}
	if mergeCalls != 6 {
		t.Errorf("merge attempts = %d, want the bounded 6", mergeCalls)
	}
}

// TestGiteaMergePullRequestReportsADenialWithoutRetrying: a status that is a
// DECISION (the provider denies this identity the merge) is reported at once.
// The retry is for a provider that has not decided yet, not for a provider
// that has — hammering a permission refusal would only delay the answer.
func TestGiteaMergePullRequestReportsADenialWithoutRetrying(t *testing.T) {
	mergeCalls := 0
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: mergeRepoBase + "/pulls", status: http.StatusOK,
			body: openPRBody(12, sourceSHA, beforeSHA)},
		{method: http.MethodGet, prefix: mergeRepoBase + "/git/refs/heads/main", status: http.StatusOK,
			body: refsBody("main", beforeSHA)},
		{method: http.MethodPost, prefix: mergeRepoBase + "/pulls/12/merge", serve: func(w http.ResponseWriter, _ *http.Request) bool {
			mergeCalls++
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"User not allowed to merge PR"}`))
			return true
		}},
	}}
	a := newGiteaAdapter(t, f, gitprovider.WithGitRunner(scriptedGit(t)),
		gitprovider.WithMergeRetryWait(time.Millisecond))

	_, err := a.MergePullRequest(testCtx(t), gitprovider.PullRequestMergeSpec{
		Repository: mergeRepo(),
		TargetRef:  "refs/heads/main",
		SourceRef:  "refs/heads/feature",
		TargetSHA:  beforeSHA,
		SourceSHA:  sourceSHA,
	})
	if !errors.Is(err, gitprovider.ErrUnauthorized) {
		t.Fatalf("MergePullRequest denied by the provider = %v, want ErrUnauthorized", err)
	}
	if !strings.Contains(err.Error(), "User not allowed to merge PR") {
		t.Errorf("refusal = %v, want the provider's own message", err)
	}
	if mergeCalls != 1 {
		t.Errorf("merge attempts = %d, want 1 (a denial is not a retry)", mergeCalls)
	}
}

// TestGiteaMergePullRequestRefusesANonBranchRef: only branch refs merge. A
// tag or a bare sha in the ref pair is refused before any provider call —
// asking for a PR between refs that are not branches would only produce a
// confusing provider error.
func TestGiteaMergePullRequestRefusesANonBranchRef(t *testing.T) {
	f := &fakeGitea{}
	a := newGiteaAdapter(t, f, gitprovider.WithGitRunner(scriptedGit(t)))

	for _, tc := range []struct{ target, source string }{
		{"refs/tags/v1", "refs/heads/feature"},
		{"refs/heads/main", "refs/tags/v1"},
		{"main", "refs/heads/feature"},
		{"refs/heads/main", ""},
	} {
		_, err := a.MergePullRequest(testCtx(t), gitprovider.PullRequestMergeSpec{
			Repository: mergeRepo(), TargetRef: tc.target, SourceRef: tc.source,
		})
		if !errors.Is(err, gitprovider.ErrConflict) {
			t.Errorf("MergePullRequest %s → %s = %v, want ErrConflict", tc.source, tc.target, err)
		}
	}
	if len(f.reqs) != 0 {
		t.Errorf("a refused ref pair reached the provider %d time(s)", len(f.reqs))
	}
}
