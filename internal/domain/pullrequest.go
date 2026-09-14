package domain

import (
	"strings"
	"time"
)

// PullRequest is a proposed RSG diff with review context (docs/03: "一组
// proposed RSG diff + file diff + review context"; canonical table
// pull_requests, docs/09 §4). It pairs a source branch with a target
// branch and pins two project states:
//
//   - BaseStateID — the base the proposal is measured against: the target
//     branch's head at creation time, fixed for the PR's lifetime. It is
//     deliberately NOT re-read from the target branch afterwards: the base
//     never drifts as the target (typically main) advances (acceptance:
//     "PR base 不随 main 漂移"). Re-evaluating against the target's
//     current head is the merge flow's job (T0406), not the PR row's.
//   - ProposedStateID — the proposal's head: the source branch's head at
//     creation time, re-pinned to the source branch's current head only
//     through the explicit head refresh (acceptance: "head update 可显式
//     refresh"). Like the base, it never moves on its own.
//
// Both are therefore FIXED states (docs/09 §4 "base state"): the database
// itself rejects any other write path (migration 00051 fixity guard).
// The number addresses the PR inside its project (UNIQUE(project_id,
// number)); it is allocated per project at creation.
type PullRequest struct {
	// ID is the uuid v4 text form (matches the pull_requests.id uuid
	// column).
	ID string
	// ProjectID is the research boundary the PR belongs to.
	ProjectID string
	// Number is the per-project PR number (1-based, sequential per
	// project; UNIQUE(project_id, number)).
	Number int64
	// SourceBranchID names the branch the proposed changes live on (the
	// head side).
	SourceBranchID string
	// TargetBranchID names the branch the proposal merges into (the base
	// side; typically main, docs/09 §3).
	TargetBranchID string
	// BaseStateID is the fixed base state: the target branch's head at
	// creation. Immutable for the PR's lifetime (migration 00051).
	BaseStateID string
	// ProposedStateID is the proposed head state: the source branch's
	// head at creation, re-pinned only by the explicit head refresh.
	ProposedStateID string
	// Title is the one-line proposal summary; non-blank, bounded
	// (ValidPullRequestTitle).
	Title string
	// Body carries the free-form proposal context; empty when none.
	Body string
	// State is the PR's lifecycle state (docs/43 machine).
	State PullRequestState
	// CreatedBy names the user who opened the PR.
	CreatedBy string
	CreatedAt time.Time
	// MergedAt is set when the PR reaches merged; nil otherwise
	// (migration 00051 enforces the consistency).
	MergedAt *time.Time
}

// PullRequestState is a PR's lifecycle state (pull_requests.state CHECK;
// docs/43: "open → review_required → changes_requested/approved →
// merge_ready → merged；也可 closed/aborted"):
//
//   - open: proposed, not yet in review (the creation state);
//   - review_required: reviews were requested — the proposal awaits
//     review decisions;
//   - changes_requested: at least one review dimension asked for changes
//     (per-dimension decisions live in the reviews table, T0404; this is
//     the PR-level projection);
//   - approved: the review round came back without requested changes;
//   - merge_ready: the proposal may be merged (integrity gates included);
//   - merged: the proposal became part of the accepted state — terminal;
//   - closed: withdrawn without merging — terminal;
//   - aborted: invalidated without merging — terminal.
//
// Scientific/Integrity review keep their own per-dimension states
// (docs/43: "Scientific/Integrity review 各自状态"); this vocabulary is
// the PR row's.
type PullRequestState string

const (
	PullRequestStateOpen             PullRequestState = "open"
	PullRequestStateReviewRequired   PullRequestState = "review_required"
	PullRequestStateChangesRequested PullRequestState = "changes_requested"
	PullRequestStateApproved         PullRequestState = "approved"
	PullRequestStateMergeReady       PullRequestState = "merge_ready"
	PullRequestStateMerged           PullRequestState = "merged"
	PullRequestStateClosed           PullRequestState = "closed"
	PullRequestStateAborted          PullRequestState = "aborted"
)

// ValidPullRequestState reports whether s is one of the canonical states.
func ValidPullRequestState(s PullRequestState) bool {
	switch s {
	case PullRequestStateOpen, PullRequestStateReviewRequired,
		PullRequestStateChangesRequested, PullRequestStateApproved,
		PullRequestStateMergeReady, PullRequestStateMerged,
		PullRequestStateClosed, PullRequestStateAborted:
		return true
	}
	return false
}

// prStateTransitions is the docs/43 transition map. Terminal states have
// no outgoing edges: a merged/closed/aborted PR never moves again
// ("Nothing disappears; state only evolves" — closing is a transition,
// reopening a closed PR is not part of the V1 machine). The one cycle is
// the review loop: changes_requested → review_required re-enters review
// after the author updated the head and re-requested it — the standard
// review_required re-entry docs/43's line implies for a proposal that
// received change requests. Migration 00051's guard enforces the same
// map for any database write path.
var prStateTransitions = map[PullRequestState][]PullRequestState{
	PullRequestStateOpen: {
		PullRequestStateReviewRequired,
		PullRequestStateClosed,
		PullRequestStateAborted,
	},
	PullRequestStateReviewRequired: {
		PullRequestStateChangesRequested,
		PullRequestStateApproved,
		PullRequestStateClosed,
		PullRequestStateAborted,
	},
	PullRequestStateChangesRequested: {
		PullRequestStateReviewRequired,
		PullRequestStateClosed,
		PullRequestStateAborted,
	},
	PullRequestStateApproved: {
		PullRequestStateMergeReady,
		PullRequestStateClosed,
		PullRequestStateAborted,
	},
	PullRequestStateMergeReady: {
		PullRequestStateMerged,
		PullRequestStateClosed,
		PullRequestStateAborted,
	},
	PullRequestStateMerged:  nil,
	PullRequestStateClosed:  nil,
	PullRequestStateAborted: nil,
}

// CanTransitionPullRequestState reports whether docs/43 allows the
// lifecycle move from → to. open is a creation state only: no transition
// targets it.
func CanTransitionPullRequestState(from, to PullRequestState) bool {
	for _, t := range prStateTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// IsTerminalPullRequestState reports whether the PR accepts no further
// transitions, head refreshes or review rounds: merged, closed or
// aborted (docs/43).
func IsTerminalPullRequestState(s PullRequestState) bool {
	switch s {
	case PullRequestStateMerged, PullRequestStateClosed, PullRequestStateAborted:
		return true
	}
	return false
}

// ValidPullRequestTitle reports whether title is a usable proposal
// summary: non-blank and bounded (generous but finite — a hostile client
// must not be able to store megabytes per PR).
func ValidPullRequestTitle(title string) bool {
	t := strings.TrimSpace(title)
	return t != "" && len(t) <= 200
}

// ValidPullRequestBody reports whether body is a storable proposal
// context: empty, or bounded (generous but finite, same discipline as
// the title).
func ValidPullRequestBody(body string) bool {
	return len(body) <= 50_000
}
