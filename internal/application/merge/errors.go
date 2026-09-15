package merge

import (
	"errors"
	"fmt"
	"strings"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/integrity"
	rsgmerge "github.com/lichman0405/post/internal/rsg/merge"
)

// Sentinel errors the merge service produces itself (docs/45: the wire
// carries codes, never dependency detail). Outcomes owned by the
// underlying services — a missing state, an unreadable snapshot, a refused
// membership — pass through unchanged so the transport maps each outcome
// once, wherever it arose.
var (
	// ErrForbidden: the authorization engine refused the action. The refusal
	// happens before any PR, branch or state is read, so it never discloses
	// whether they exist.
	ErrForbidden = errors.New("merge: forbidden")
	// ErrValidation: an input fails the domain shape rules.
	ErrValidation = errors.New("merge: validation failed")
	// ErrPullRequestNotFound: no PR exists for the (project, number) pair —
	// an unknown number, or a PR of another project (same outcome, no
	// foreign existence leak).
	ErrPullRequestNotFound = errors.New("merge: pull request not found")
	// ErrBranchNotFound: a branch of the PR's pair does not exist in the
	// project (same outcome for both, docs/45).
	ErrBranchNotFound = errors.New("merge: branch not found in the project")
	// ErrStore: an adapter or the policy engine failed (cause kept).
	ErrStore = errors.New("merge: store failure")
	// ErrGateRefused: the PR's integrity review reports a blocking
	// failure, so the merge does not run (docs/22 §7 — the command re-runs
	// the review server-side and refuses on it). The refusal carries the
	// complete report.
	ErrGateRefused = errors.New("merge: the pull request's integrity review blocks the merge")
	// ErrPolicyRefused: the governance policy in force does not permit the
	// merge. Fail-closed — an unreadable policy, an absent rule and a rule
	// set to false all refuse (see PolicyRefusedError).
	ErrPolicyRefused = errors.New("merge: the policy in force does not permit this merge")
)

// Wire codes (docs/45). Outcomes shared with other packages carry the same
// code strings there, so one outcome has one stable wire name whichever
// package reports it.
const (
	CodeForbidden           = "AUTH_FORBIDDEN"
	CodeValidation          = "VALIDATION_FAILED"
	CodePullRequestNotFound = "PULL_REQUEST_NOT_FOUND"
	CodeBranchNotFound      = "BRANCH_NOT_FOUND"
	CodeNotMergeable        = "PR_NOT_MERGEABLE"
	CodeMergeBlocked        = "MERGE_BLOCKED"
	CodeMergeStale          = "MERGE_STALE"
	CodeAlreadyMerged       = "PR_ALREADY_MERGED"
	CodeUnavailable         = "SERVICE_UNAVAILABLE"
	// CodeIntegrityBlocked: the PR's integrity review reports a blocking
	// failure, so the merge governance refuses to advance the target.
	CodeIntegrityBlocked = "PR_INTEGRITY_BLOCKED"
	// CodePolicyRefused: the policy in force does not permit the merge.
	CodePolicyRefused = "MERGE_POLICY_REFUSED"
)

// NotMergeableError reports a PR that is not in the state a merge requires
// (docs/43: only merge_ready merges). The review machine has to be walked,
// it cannot be skipped by asking the merge engine directly.
type NotMergeableError struct {
	// Number is the PR the merge was attempted on.
	Number int64
	// State is the state the PR is in.
	State domain.PullRequestState
}

// Error implements error.
func (e *NotMergeableError) Error() string {
	return fmt.Sprintf("merge: PR #%d is %s — only a merge_ready PR merges (docs/43: open -> review_required -> changes_requested/approved -> merge_ready -> merged)",
		e.Number, e.State)
}

// Code is the stable wire code of this outcome (docs/45).
func (e *NotMergeableError) Code() string { return CodeNotMergeable }

// whyNothingToMerge is the extra line a nothing-to-merge refusal gets on
// the error surface. The engine's own blocker detail states the shape
// ("no source-side change would be written into the accepted state"); what
// a caller needs on top of it is that this is the DECIDED outcome of the
// proposal and not a missing key or a broken object — the source-side
// change is contested and withheld, kept on the target, carried as
// unresolved or aborted, so there is nothing applicable to merge. Without
// the line the refusal reads like a store fault.
const whyNothingToMerge = " — the proposal's changes are contested or withheld (kept on the target, carried as unresolved, or withheld by the publication rule), so there is nothing applicable to merge"

// BlockedError reports a plan that must not execute. It carries the plan
// itself: the blockers name what stopped the merge (an undecided conflict,
// a validation branch, a request for evidence, nothing to merge), and the
// plan is the evidence an operator reads. A blocked merge writes nothing —
// no accepted state, no partial application (the engine's whole output is
// the unit).
type BlockedError struct {
	// Plan is the blocked plan (never executable).
	Plan *rsgmerge.Plan
}

// Error implements error: the first blocker, plus the count when there are
// more. The blockers are already canonical in the plan.
func (e *BlockedError) Error() string {
	if e.Plan == nil || len(e.Plan.Blockers) == 0 {
		return "merge: the plan is not executable"
	}
	first := e.Plan.Blockers[0]
	detail := fmt.Sprintf("merge: blocked by %s (%s)", first.Code, first.Detail)
	if onlyNothingToMerge(e.Plan.Blockers) {
		detail += whyNothingToMerge
	}
	if rest := len(e.Plan.Blockers) - 1; rest > 0 {
		codes := make([]string, 0, rest)
		for _, b := range e.Plan.Blockers[1:] {
			codes = append(codes, b.Code)
		}
		detail += fmt.Sprintf(" and %d more: %s", rest, strings.Join(codes, ", "))
	}
	return detail
}

// onlyNothingToMerge reports whether the plan's whole blocker list is the
// nothing-to-merge refusal.
func onlyNothingToMerge(blockers []rsgmerge.Blocker) bool {
	for _, b := range blockers {
		if b.Code != rsgmerge.CodeNothingToMerge {
			return false
		}
	}
	return true
}

// Code is the stable wire code of this outcome (docs/45).
func (e *BlockedError) Code() string { return CodeMergeBlocked }

// StaleMergeError reports that the merge's inputs moved between the plan
// and the write: the target branch advanced, the PR left merge_ready, the
// proposed state changed, or a branch closed. The whole transaction rolled
// back; nothing was written.
//
// It is NOT retried automatically and that is the point: a human decided
// the conflicts of one exact base/source/target triple, and a moved target
// is a different triple whose conflicts the human has not seen. The
// proposal has to be refreshed and decided again (docs/09 §8 — the
// decisions bind to states, not to branch heads that move).
type StaleMergeError struct {
	// Reason says which fact moved.
	Reason string
}

// Error implements error.
func (e *StaleMergeError) Error() string {
	return "merge: the merge inputs moved under the plan: " + e.Reason +
		" — nothing was written; refresh the proposal and decide again"
}

// Code is the stable wire code of this outcome (docs/45).
func (e *StaleMergeError) Code() string { return CodeMergeStale }

// AlreadyMergedError reports a second merge row for one PR. The database
// refuses it (semantic_merges is UNIQUE(pull_request_id)), and the service
// reports the refusal in the domain's words instead of a raw 23505: a PR
// merges once, and a second merge would advance the target branch twice.
type AlreadyMergedError struct {
	// PullRequestID is the PR that already has a merge.
	PullRequestID string
}

// Error implements error.
func (e *AlreadyMergedError) Error() string {
	return fmt.Sprintf("merge: pull request %s already has a merge — a PR merges once (docs/43)", e.PullRequestID)
}

// Code is the stable wire code of this outcome (docs/45).
func (e *AlreadyMergedError) Code() string { return CodeAlreadyMerged }

// PullRequestNotMergeableError reports a PR that left merge_ready between
// the plan and the write (the in-transaction CAS found it elsewhere).
type PullRequestNotMergeableError struct {
	// PullRequestID is the PR whose transition missed.
	PullRequestID string
}

// Error implements error.
func (e *PullRequestNotMergeableError) Error() string {
	return fmt.Sprintf("merge: pull request %s is no longer merge_ready — the merge's CAS missed (nothing was written)", e.PullRequestID)
}

// Code is the stable wire code of this outcome (docs/45).
func (e *PullRequestNotMergeableError) Code() string { return CodeNotMergeable }

// BranchNotActiveError reports a branch that left the active lifecycle
// before the merge could close it (docs/43: closed research paths are
// immutable).
type BranchNotActiveError struct {
	// BranchID is the branch that was not active.
	BranchID string
}

// Error implements error.
func (e *BranchNotActiveError) Error() string {
	return fmt.Sprintf("merge: branch %s is not active — a closed research path does not merge (docs/43)", e.BranchID)
}

// Code is the stable wire code of this outcome (docs/45).
func (e *BranchNotActiveError) Code() string { return "BRANCH_NOT_ACTIVE" }

// MissingEntityError reports a version head the merge had to lock that does
// not exist: the diff read the entity's versions, so its row must be there.
// Reaching this means the reads disagree, which is a store-level fault, not
// a merge decision.
type MissingEntityError struct {
	// Kind is "object" or "relation".
	Kind string
	// ID is the missing container id.
	ID string
}

// Error implements error.
func (e *MissingEntityError) Error() string {
	return fmt.Sprintf("merge: %s %s has no row (the diff read its versions, so this is a store inconsistency)", e.Kind, e.ID)
}

// Code is the stable wire code of this outcome (docs/45).
func (e *MissingEntityError) Code() string { return CodeUnavailable }

// GateRefused is the merge's integrity refusal: ErrGateRefused wrapped
// with the COMPLETE review report, the same shape releases.GateRefused
// uses. docs/22 §7: the command re-runs the review and the client sees the
// whole result, not a summary — a refusal that hid the failing checks
// would send an author back to the PR page to guess what stopped it.
type GateRefused struct {
	Report integrity.Report
}

// Error implements error: the report's own explanation when it has one
// (the engine renders every failed check with its why), a fixed line
// otherwise.
func (e *GateRefused) Error() string {
	if e.Report.Explanation != "" {
		return ErrGateRefused.Error() + ": " + e.Report.Explanation
	}
	return ErrGateRefused.Error()
}

// Unwrap keeps errors.Is(err, ErrGateRefused) working through the wrap.
func (e *GateRefused) Unwrap() error { return ErrGateRefused }

// Code is the stable wire code of this outcome (docs/45).
func (e *GateRefused) Code() string { return CodeIntegrityBlocked }

// BlockingFailures lists the report's blocking failures, in report order —
// the evidence the transport renders and the tests assert on.
func (e *GateRefused) BlockingFailures() []integrity.Result {
	out := []integrity.Result{}
	for _, f := range e.Report.Failures() {
		if f.Severity == integrity.SeverityBlocking {
			out = append(out, f)
		}
	}
	return out
}

// PolicyRefusedError reports that the governance policy in force does not
// permit the merge. It is fail-closed on all three of its inputs, and the
// fields say which one fired:
//
//   - the policy could not be read at all (Err set) — an unreadable policy
//     is not a permissive one;
//   - the rule is ABSENT (Found false) — that is not a permission either.
//     Frozen main is structurally protected (docs/09 §3: main advances
//     only through a Research PR merge, enforced by the branch protection
//     and the ref guard), so a policy that does not mention
//     main_protected says nothing about main, and reading its silence as
//     "unprotected" would let a document waive a platform guarantee;
//   - the rule is present and FALSE (Bool false) — a vacuum. No product
//     rule in this build can turn the merge path into an unprotected
//     write, so false is refused rather than obeyed. Treating it as a
//     licence would make `main_protected: false` a switch that disables
//     docs/09 §3 itself, which is a decision above this task's pay grade
//     — it is reported to the Supervisor instead (see RESULT).
type PolicyRefusedError struct {
	// Rule is the rule key the refusal is about (domain.RuleMainProtected).
	Rule string
	// Found reports whether the policy set the rule at all.
	Found bool
	// Bool is the rule's value when Found.
	Bool bool
	// Reason is the human-readable line naming which of the three cases
	// fired.
	Reason string
	// Err is the underlying failure when the policy could not be read.
	Err error
}

// Error implements error.
func (e *PolicyRefusedError) Error() string {
	return "merge: the policy in force does not permit this merge — " + e.Reason
}

// Unwrap keeps errors.Is(err, ErrPolicyRefused) working, and carries the
// store failure when the refusal was a failed read.
func (e *PolicyRefusedError) Unwrap() error {
	if e.Err != nil {
		return fmt.Errorf("%w: %w", ErrPolicyRefused, e.Err)
	}
	return ErrPolicyRefused
}

// Code is the stable wire code of this outcome (docs/45).
func (e *PolicyRefusedError) Code() string { return CodePolicyRefused }

// versionHeadsMovedError is the internal retry signal: the version counters
// of the objects/relations the plan materializes moved between the
// prediction read and the locked read. The states did NOT move (that is
// StaleMergeError) — only the per-object version counters did, which
// happens when another branch commits a version of the same object.
//
// It is unexported on purpose: it is invisible to callers by design. The
// merge re-reads the counters and tries again, and the caller sees either a
// merge or a real refusal.
type versionHeadsMovedError struct {
	kind      string
	id        string
	predicted int
	actual    int
}

// Error implements error.
func (e *versionHeadsMovedError) Error() string {
	return fmt.Sprintf("merge: %s %s version head moved from %d to %d while planning",
		e.kind, e.id, e.predicted, e.actual)
}
