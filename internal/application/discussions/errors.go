package discussions

import "errors"

// Sentinel errors the command maps for the transport layer (docs/45: the
// wire carries codes, never dependency detail). One outcome has one wire
// name, and a refusal never discloses whether a foreign entity exists.
var (
	// ErrValidation: an input fails the domain shape rules (an unknown
	// target kind or promotion kind, a blank or oversized body, a blank
	// id, a field that does not belong to the promotion kind it was sent
	// with).
	ErrValidation = errors.New("discussions: validation failed")
	// ErrForbidden: the actor may not do this. For a write that creates
	// state in another table (a promotion) it is the matrix's answer for
	// ActionWriteScientificState; for a comment it is "no authenticated
	// actor" — the read gate itself refuses with ErrProjectNotFound (an
	// actor who may not read the target may not learn it exists).
	ErrForbidden = errors.New("discussions: the actor may not do this here")
	// ErrProjectNotFound: no project row exists for the id, or the project
	// is not visible to the caller — one outcome for both (docs/45).
	ErrProjectNotFound = errors.New("discussions: project not found")
	// ErrThreadNotFound: no thread row exists for the id in this project.
	// A thread of another project answers the same thing.
	ErrThreadNotFound = errors.New("discussions: discussion thread not found")
	// ErrCommentNotFound: no comment row exists for the id in this
	// project (a comment of another project, or of another thread, answers
	// the same thing).
	ErrCommentNotFound = errors.New("discussions: discussion comment not found")
	// ErrCommentDeleted: the comment exists but carries a tombstone. A
	// withdrawn proposal is not promoted (its author took it back) and a
	// withdrawn comment is not withdrawn twice — the second delete would
	// otherwise overwrite the first tombstone's author.
	ErrCommentDeleted = errors.New("discussions: the comment was withdrawn")
	// ErrTargetNotFound: the thing a thread is about does not exist in
	// this project — an unknown id, or a target of another project (one
	// outcome, no foreign existence leak, docs/45).
	ErrTargetNotFound = errors.New("discussions: the discussion target does not exist in this project")
	// ErrBranchNotFound: a promotion named a research branch that does not
	// exist in this project (or that the actor may not read). The branch is
	// the only caller-supplied id a promotion adds besides the comment, and
	// a state commit cannot land anywhere else — so it is reported on its
	// own rather than folded into ErrValidation, where a caller could not
	// tell a malformed request from a misplaced one.
	ErrBranchNotFound = errors.New("discussions: the research branch does not exist in this project")
	// ErrPromotionNotFound: no promotion row exists for the id in this
	// project.
	ErrPromotionNotFound = errors.New("discussions: promotion not found")
	// ErrStore: a persistence adapter, the policy engine, or one of the
	// creating services failed (cause kept for the log).
	ErrStore = errors.New("discussions: store failure")
)

// Wire codes (docs/45). One outcome has one stable wire name.
const (
	// CodeDiscussionValidationFailed: the request fails the shape rules.
	CodeDiscussionValidationFailed = "DISCUSSION_VALIDATION_FAILED"
	// CodeDiscussionForbidden: the actor's class denies the action.
	CodeDiscussionForbidden = "DISCUSSION_FORBIDDEN"
	// CodeDiscussionUnauthenticated: the surface needs an authenticated
	// actor (a comment, a promotion) and this request has none.
	CodeDiscussionUnauthenticated = "UNAUTHENTICATED"
	// CodeDiscussionProjectNotFound: the project does not exist or is not
	// visible to the caller.
	CodeDiscussionProjectNotFound = "DISCUSSION_PROJECT_NOT_FOUND"
	// CodeDiscussionThreadNotFound: no thread row for the id in the
	// project.
	CodeDiscussionThreadNotFound = "DISCUSSION_THREAD_NOT_FOUND"
	// CodeDiscussionCommentNotFound: no comment row for the id in the
	// project (or in the thread named by the path).
	CodeDiscussionCommentNotFound = "DISCUSSION_COMMENT_NOT_FOUND"
	// CodeDiscussionCommentDeleted: the comment was withdrawn.
	CodeDiscussionCommentDeleted = "DISCUSSION_COMMENT_DELETED"
	// CodeDiscussionTargetNotFound: the thread's target does not exist in
	// the project.
	CodeDiscussionTargetNotFound = "DISCUSSION_TARGET_NOT_FOUND"
	// CodeDiscussionBranchNotFound: the branch a promotion named does not
	// exist in the project.
	CodeDiscussionBranchNotFound = "DISCUSSION_BRANCH_NOT_FOUND"
	// CodeDiscussionPromotionNotFound: no promotion row for the id.
	CodeDiscussionPromotionNotFound = "DISCUSSION_PROMOTION_NOT_FOUND"
	// CodeDiscussionServiceUnavailable: the discussion data is temporarily
	// unavailable.
	CodeDiscussionServiceUnavailable = "SERVICE_UNAVAILABLE"
)
