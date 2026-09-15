package gitprovider

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// How hard one merge call tries before it reports the provider's refusal.
// mergeAttempts × mergeFirstRetryWait doubling: 100ms + 200ms + 400ms +
// 800ms + 1600ms of waiting over six attempts — long enough for a provider
// that is still deciding the pair is mergeable, short enough that a real
// refusal does not park the caller (the request context cuts it shorter
// still if the client gives up).
const (
	mergeAttempts       = 6
	mergeFirstRetryWait = 100 * time.Millisecond
)

// The provider-side pull-request merge (T0409): the ONE write path that may
// advance a protected branch. T0302 made the provider itself enforce that —
// `enable_push` false for everyone including the owner and the instance
// admins, `enable_merge_whitelist` true with the merge service as its only
// member — so "main moves through a PR merge and nothing else" is not a
// convention this code follows but a rule the provider applies to this code
// too. The adapter therefore has no push anywhere in this file, and it could
// not use one: a push to main by any identity is refused at the transport
// (tests/integration/gitea_protection_test.go is the live proof).
//
// The provider PR itself is a transport artifact. The platform's Research PR
// is a PostgreSQL row (pull_requests, T0402) and the provider has no merge
// without its own PR object, so this adapter finds the provider PR for the
// (head, base) ref pair and creates it when it is missing. It is found, not
// registered: no canonical table maps the two, and none is needed — the ref
// pair identifies the PR while it is open, and once it is merged the merge
// commit is what both sides record.
//
// What the platform pins: the ref pair is the one the plan was computed
// over, so a provider-side merge can never land on a different pair. The
// pins are the states' recorded commit shas (project_states.git_commit_sha,
// set by push ingestion T0305) — ABSENT for a state the platform has never
// seen a push for (a fixture-built state, or a merge created entirely
// through the API), and a pin that does not exist cannot be checked. The
// merge request's head_commit_id is the provider's own guard at that point:
// it refuses a merge whose head moved since the read.

// PullRequestMerger is the port over the provider-side PR merge. The
// application-side port is internal/application/merge.GitMerger; this package
// cannot implement that one directly (the merge application imports this
// package for RefGuard, so the dependency only runs one way) and the
// composition root bridges the two — see cmd/api/mergegit.
type PullRequestMerger interface {
	MergePullRequest(ctx context.Context, spec PullRequestMergeSpec) (PullRequestMergeResult, error)
}

// PullRequestMergeSpec names one provider-side pull request to merge.
type PullRequestMergeSpec struct {
	// Repository is the project's provisioned repository
	// (git_repository_provisions; the caller resolves it — this port is
	// project-scoped, not project-aware).
	Repository Repository
	// Title is the provider PR's title when the adapter has to create it.
	// Display only: nothing reads it back, and the platform's own PR title
	// lives in PostgreSQL.
	Title string
	// TargetRef and SourceRef are the FULL refs the merge runs between
	// ("refs/heads/main" → "refs/heads/feature"). Both must be branch refs:
	// a tag cannot be merged into.
	TargetRef string
	SourceRef string
	// TargetSHA and SourceSHA are optional pins ("" when the platform has
	// no recorded commit for that state). When set, the provider's own view
	// of the pair must agree before anything is merged — the merge lands the
	// exact refs the plan was computed over or it does not land.
	TargetSHA string
	SourceSHA string
}

// PullRequestMergeResult is what the provider reports after the merge.
type PullRequestMergeResult struct {
	// Number is the provider PR's index (the provider's own numbering — the
	// platform number is not mirrored onto it).
	Number int64
	// SHA is the target ref's head AFTER the merge, read back from the
	// provider: the merge commit for an ordinary merge.
	SHA string
	// Actor is the provider login that performed the merge
	// (merged_by.login). The platform's RefGuard judges the update by it —
	// a merge somebody else performed is refused there, not silently
	// accepted here.
	Actor string
}

// pullRequestBody is the provider's pull-request shape, the subset the merge
// needs. Gitea 1.27.3 fills all of it on read and on create.
type pullRequestBody struct {
	Number   int64  `json:"number"`
	State    string `json:"state"`
	Title    string `json:"title"`
	Merged   bool   `json:"merged"`
	MergeSHA string `json:"merge_commit_sha"`
	MergedBy *struct {
		Login string `json:"login"`
	} `json:"merged_by"`
	Head struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
}

// mergedByLogin reads the merging identity, "" when the provider reported
// none (an unmerged PR).
func (pr pullRequestBody) mergedByLogin() string {
	if pr.MergedBy == nil {
		return ""
	}
	return pr.MergedBy.Login
}

// MergePullRequest implements PullRequestMerger. The order is the contract's:
// resolve the provider PR for the ref pair (creating it when it is missing),
// verify the pair the platform planned against the provider's own view, merge
// with the head pinned, and read the outcome back — the new target head and
// the identity that made it.
//
// Every failure maps onto the package's sentinels: ErrNotFound when the
// repository or a ref is missing, ErrConflict when the merge cannot land as
// specified (a pinned ref moved, the provider refuses the merge, the ref pair
// has nothing to merge), ErrUnauthorized / ErrUnavailable as everywhere else.
// A provider that is still deciding the pair is mergeable is retried within
// the call, bounded — see mergeProviderPullRequest.
func (a *GiteaAdapter) MergePullRequest(ctx context.Context, spec PullRequestMergeSpec) (PullRequestMergeResult, error) {
	fail := func(err error) (PullRequestMergeResult, error) { return PullRequestMergeResult{}, err }

	base, err := branchNameOf(spec.TargetRef)
	if err != nil {
		return fail(fmt.Errorf("%w: target %s", err, spec.TargetRef))
	}
	head, err := branchNameOf(spec.SourceRef)
	if err != nil {
		return fail(fmt.Errorf("%w: source %s", err, spec.SourceRef))
	}
	if spec.Repository.Owner == "" || spec.Repository.Name == "" {
		return fail(fmt.Errorf("%w: the repository is required to merge a pull request", ErrConflict))
	}

	before, err := a.GetBranch(ctx, spec.Repository, base)
	if err != nil {
		return fail(fmt.Errorf("read the target ref before merging: %w", err))
	}
	if spec.TargetSHA != "" && before.HeadSHA != spec.TargetSHA {
		return fail(fmt.Errorf("%w: the target ref %s is at %s, not the %s the merge was planned against",
			ErrConflict, spec.TargetRef, before.HeadSHA, spec.TargetSHA))
	}

	pr, err := a.ensurePullRequest(ctx, spec, base, head)
	if err != nil {
		return fail(err)
	}
	if err := verifyPullRequestPins(spec, pr); err != nil {
		return fail(err)
	}
	return a.mergeProviderPullRequest(ctx, spec, before, pr)
}

// mergeProviderPullRequest asks the provider to merge the pull request and
// reads the outcome back.
//
// The provider decides whether a pull request is mergeable ASYNCHRONOUSLY:
// the pair is checked in the background, and until that check has run the
// merge is answered with 405 and the provider's own message ("Please try
// again later" — Gitea 1.27.3's answer for a pull request it does not
// currently consider mergeable; the same call lands moments later, which is
// how this was found). A refusal that the provider did NOT back with a merge
// is therefore re-asked a bounded number of times, each time re-reading the
// pull request first: the read supplies the head pin for the next attempt,
// and it is also the request the provider re-checks the pair on. Every
// attempt carries the same pins, so the retry can only ever land the exact
// pair the plan was computed over.
//
// The retry is bounded (mergeAttempts, with the wait doubling from
// mergeFirstRetryWait) and narrow (only this status): a genuine refusal must
// stay a refusal, and 405 is the provider's overloaded status — it also
// carries "you are not allowed to merge this pull request" (T0302's merge
// whitelist). A retry cannot talk a refusal into a merge: when the PR is
// still unmerged after the budget, the provider's own words are what the
// caller gets, with the attempt count attached.
func (a *GiteaAdapter) mergeProviderPullRequest(ctx context.Context, spec PullRequestMergeSpec, before BranchRef, pr pullRequestBody) (PullRequestMergeResult, error) {
	mergePath := repoAPIPath(spec.Repository) + "/pulls/" + strconv.FormatInt(pr.Number, 10) + "/merge"
	wait := a.mergeRetryWait
	for attempt := 1; ; attempt++ {
		// head_commit_id is the provider's own staleness guard: it refuses
		// when the PR's head moved since the read, so the merge cannot land a
		// source that changed under it. Every other merge style is left at the
		// repository's default ("merge"), which is what makes the result a
		// commit both sides can name.
		code, raw, err := a.call(ctx, http.MethodPost, mergePath, mergeBody(pr.Head.SHA), nil)
		if err != nil {
			return PullRequestMergeResult{}, err
		}
		switch {
		case code == http.StatusOK:
			// Read the merged PR back for the identity, then the target ref
			// for the new head. The two reads are one fact — the ref moved by
			// the merge the PR recorded.
			merged, err := a.readPullRequest(ctx, spec.Repository, pr.Number)
			if err != nil {
				return PullRequestMergeResult{}, err
			}
			if !merged.Merged {
				return PullRequestMergeResult{}, fmt.Errorf("%w: the provider answered the merge with %d but reports the pull request unmerged",
					ErrUnavailable, code)
			}
			return a.mergedResult(ctx, spec, before, merged)
		case code == http.StatusMethodNotAllowed:
			// Already merged — the provider refuses a second merge of the same
			// PR. That is not a failure of this call: read the PR back and
			// report what the provider has. A retry after a response was lost
			// converges here, and a merge somebody else performed surfaces
			// with THEIR identity, which RefGuard then refuses.
			//
			// The same status also covers the provider's two refusals — "not
			// mergeable" and "not allowed to merge" — and what separates any
			// of them from a merge is this read, not the status.
			replay, err := a.readPullRequest(ctx, spec.Repository, pr.Number)
			if err != nil {
				return PullRequestMergeResult{}, err
			}
			if replay.Merged {
				return a.mergedResult(ctx, spec, before, replay)
			}
			refusal := fmt.Errorf("%w (after %d merge attempts)", a.mapStatus(code, "merge pull request", raw), attempt)
			if attempt >= mergeAttempts || wait <= 0 {
				return PullRequestMergeResult{}, refusal
			}
			// The provider has not decided the pair is mergeable yet (or has
			// refused for good). Wait, re-read the pull request for the fresh
			// head pin — and, on this provider, to have it re-check the pair —
			// and ask again.
			if err := a.pause(ctx, wait); err != nil {
				return PullRequestMergeResult{}, err
			}
			wait *= 2
			pr, err = a.readPullRequest(ctx, spec.Repository, pr.Number)
			if err != nil {
				return PullRequestMergeResult{}, err
			}
			if err := verifyPullRequestPins(spec, pr); err != nil {
				return PullRequestMergeResult{}, err
			}
			if pr.Merged {
				// Somebody (or something) merged it between the refusal and
				// the re-read. Report the provider's fact rather than asking
				// for a second merge.
				return a.mergedResult(ctx, spec, before, pr)
			}
		default:
			return PullRequestMergeResult{}, a.mapStatus(code, "merge pull request", raw)
		}
	}
}

// verifyPullRequestPins checks the provider's own view of the ref pair
// against the commit shas the merge was planned over. A pin that is absent
// ("") is not checkable and is skipped; a pin that is set must agree.
func verifyPullRequestPins(spec PullRequestMergeSpec, pr pullRequestBody) error {
	if spec.SourceSHA != "" && pr.Head.SHA != spec.SourceSHA {
		return fmt.Errorf("%w: %s is at %s, not the %s the merge was planned against",
			ErrConflict, spec.SourceRef, pr.Head.SHA, spec.SourceSHA)
	}
	if spec.TargetSHA != "" && pr.Base.SHA != "" && pr.Base.SHA != spec.TargetSHA {
		return fmt.Errorf("%w: the provider reads %s at %s, not the %s the merge was planned against",
			ErrConflict, spec.TargetRef, pr.Base.SHA, spec.TargetSHA)
	}
	return nil
}

// pause waits between two merge attempts, and gives up the moment the caller
// does: a cancelled request must not sit out a retry budget.
func (a *GiteaAdapter) pause(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// mergedResult reads the target ref back and builds the result. The merge
// must have moved the ref — a success answer that left the target where it
// was is a provider inconsistency, not a merge, and reporting it as one would
// record a Git step that never happened.
func (a *GiteaAdapter) mergedResult(ctx context.Context, spec PullRequestMergeSpec, before BranchRef, pr pullRequestBody) (PullRequestMergeResult, error) {
	base, err := branchNameOf(spec.TargetRef)
	if err != nil {
		return PullRequestMergeResult{}, err
	}
	after, err := a.GetBranch(ctx, spec.Repository, base)
	if err != nil {
		return PullRequestMergeResult{}, fmt.Errorf("read the target ref after merging: %w", err)
	}
	if after.HeadSHA == "" || after.HeadSHA == before.HeadSHA {
		// The one legitimate case of an unmoved target is the replay of a
		// merge that landed earlier — the platform's own retry. It is only
		// legitimate when the provider says the PR is merged AND the ref
		// already is the merge commit.
		if pr.Merged && pr.MergeSHA != "" && pr.MergeSHA == after.HeadSHA {
			return PullRequestMergeResult{Number: pr.Number, SHA: after.HeadSHA, Actor: pr.mergedByLogin()}, nil
		}
		return PullRequestMergeResult{}, fmt.Errorf("%w: the merge of pull request %d reported success but %s is still at %s",
			ErrUnavailable, pr.Number, spec.TargetRef, after.HeadSHA)
	}
	return PullRequestMergeResult{Number: pr.Number, SHA: after.HeadSHA, Actor: pr.mergedByLogin()}, nil
}

// mergeBody is the provider's merge request body: an ordinary merge, pinned
// to the head the PR had when it was read.
func mergeBody(headSHA string) map[string]any {
	body := map[string]any{"Do": "merge"}
	if headSHA != "" {
		body["head_commit_id"] = headSHA
	}
	return body
}

// ensurePullRequest finds the provider PR for (head → base) or creates it.
// An open PR is the normal case; a MERGED one is the retry of a merge that
// already landed (the platform's git step is retried until it is recorded,
// and the provider PR stays merged after the first success) — it is only
// accepted as such, never as an open PR to merge again. A pair with no PR at
// all is a PR to create.
func (a *GiteaAdapter) ensurePullRequest(ctx context.Context, spec PullRequestMergeSpec, base, head string) (pullRequestBody, error) {
	pr, found, err := a.findPullRequest(ctx, spec.Repository, base, head, "open")
	if err != nil {
		return pullRequestBody{}, err
	}
	if found {
		return pr, nil
	}
	title := spec.Title
	if title == "" {
		title = fmt.Sprintf("POST merge %s into %s", head, base)
	}
	body := map[string]any{"title": title, "head": head, "base": base}
	var created pullRequestBody
	code, raw, err := a.call(ctx, http.MethodPost, repoAPIPath(spec.Repository)+"/pulls", body, &created)
	if err != nil {
		return pullRequestBody{}, err
	}
	switch code {
	case http.StatusCreated:
		return created, nil
	case http.StatusConflict, http.StatusUnprocessableEntity, http.StatusBadRequest:
		// Lost the race to a concurrent create (the provider refuses a second
		// open PR for the same pair) — or the pair's PR is already merged, which
		// the same statuses cover. Look again: an open PR first, then the merged
		// one. A closed-but-unmerged PR for the pair stays a refusal: the
		// provider's own state says this pair cannot be merged.
		if pr, found, ferr := a.findPullRequest(ctx, spec.Repository, base, head, "open"); ferr == nil && found {
			return pr, nil
		}
		if pr, found, ferr := a.findPullRequest(ctx, spec.Repository, base, head, "all"); ferr == nil && found {
			return pr, nil
		}
		return pullRequestBody{}, a.mapStatus(code, "create pull request", raw)
	default:
		return pullRequestBody{}, a.mapStatus(code, "create pull request", raw)
	}
}

// findPullRequest lists the repository's pull requests in the given state
// ("open" or "all") and returns one whose head and base refs are the pair.
// With state "all" only MERGED pull requests qualify: an unmerged one cannot
// be reported as a merge, and the caller's whole reason to look is a merge
// that may have already landed. The newest such PR wins (a pair can be
// merged, advance, and be opened again).
//
// The list is per-repository and bounded (50, the provider's page maximum is
// larger but a repository with more open PRs than that is an operator
// situation, not a merge one): the merge only ever needs the PR for one
// specific pair, and a pair with no PR is a PR to create, not a reason to
// page.
func (a *GiteaAdapter) findPullRequest(ctx context.Context, repo Repository, base, head, state string) (pullRequestBody, bool, error) {
	var list []pullRequestBody
	code, raw, err := a.call(ctx, http.MethodGet,
		repoAPIPath(repo)+"/pulls?state="+state+"&limit=50", nil, &list)
	if err != nil {
		return pullRequestBody{}, false, err
	}
	if code != http.StatusOK {
		return pullRequestBody{}, false, a.mapStatus(code, "list pull requests", raw)
	}
	var best pullRequestBody
	var found bool
	for _, pr := range list {
		if pr.Head.Ref != head || pr.Base.Ref != base {
			continue
		}
		if state != "all" {
			return pr, true, nil
		}
		if !pr.Merged {
			continue
		}
		if !found || pr.Number > best.Number {
			best, found = pr, true
		}
	}
	return best, found, nil
}

// readPullRequest reads one provider PR by its index.
func (a *GiteaAdapter) readPullRequest(ctx context.Context, repo Repository, number int64) (pullRequestBody, error) {
	var pr pullRequestBody
	code, raw, err := a.call(ctx, http.MethodGet,
		repoAPIPath(repo)+"/pulls/"+strconv.FormatInt(number, 10), nil, &pr)
	if err != nil {
		return pullRequestBody{}, err
	}
	if code != http.StatusOK {
		return pullRequestBody{}, a.mapStatus(code, "read pull request", raw)
	}
	return pr, nil
}

// branchNameOf turns a full branch ref into the provider's branch name. Only
// refs/heads/ refs are branches — a tag or a bare sha cannot be merged, and
// silently treating one as a branch name would ask the provider for a PR
// between refs that do not exist.
func branchNameOf(ref string) (string, error) {
	name := strings.TrimPrefix(ref, "refs/heads/")
	if name == ref || name == "" {
		return "", fmt.Errorf("%w: %q is not a branch ref", ErrConflict, ref)
	}
	return name, nil
}

// repoAPIPath is the repository's API base path.
func repoAPIPath(repo Repository) string {
	return "/api/v1/repos/" + urlSegment(repo.Owner) + "/" + urlSegment(repo.Name)
}
