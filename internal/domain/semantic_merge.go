package domain

import (
	"errors"
	"time"
)

// Semantic merge (T0406): the record of one Research PR merge — the
// transition that advances an accepted state, and the conflicts that
// transition carries instead of resolving.
//
// docs/09 §3 freezes main: it advances only through a Research PR merge,
// and this is that merge's record. The row is written in the same
// transaction that commits the merged state, so the accepted state and
// the record of how it was accepted cannot come apart.
//
// The GitProvider half is a SAGA, not part of that transaction (docs/08:
// Git is the repository/file truth beside PostgreSQL's semantic truth).
// The database truth commits first; the Git step is recorded on the same
// row through GitState and stays retryable when it fails. That is the
// deliberate trade-off: a merge whose Git step failed is visible and
// resumable, never silently divergent.

// SemanticMergeGitState is the Git half of the merge saga.
type SemanticMergeGitState string

const (
	// GitStatePending: the database truth is committed, the GitProvider
	// ref update has not happened yet (or the merge runs with no provider
	// merge adapter wired — T0409 owns that adapter, and until it exists
	// the step is not silently declared done).
	GitStatePending SemanticMergeGitState = "pending"
	// GitStateUpdated: the provider-side merge happened; GitSHA names it.
	GitStateUpdated SemanticMergeGitState = "updated"
	// GitStateFailed: the provider refused or could not be reached. The
	// merge stays resumable; GitError carries the reason.
	GitStateFailed SemanticMergeGitState = "failed"
	// GitStateSkipped: there was nothing to update (neither branch has a
	// provider ref recorded).
	GitStateSkipped SemanticMergeGitState = "skipped"
)

// ValidSemanticMergeGitState reports whether s is one of the saga states.
func ValidSemanticMergeGitState(s SemanticMergeGitState) bool {
	switch s {
	case GitStatePending, GitStateUpdated, GitStateFailed, GitStateSkipped:
		return true
	}
	return false
}

// SemanticMerge is one Research PR merge (the semantic_merges row).
type SemanticMerge struct {
	ID            string
	ProjectID     string
	PullRequestID string
	// SourceBranchID is the proposed research path; TargetBranchID is the
	// branch the merge advanced.
	SourceBranchID string
	TargetBranchID string
	// BaseStateID, SourceStateID and TargetStateID are the three states
	// the plan was computed over: the PR's fixed base, the proposed head
	// at merge time, and the target head at merge time.
	BaseStateID   string
	SourceStateID string
	TargetStateID string
	// ResultStateID is the accepted state this merge committed.
	ResultStateID string
	ActorID       string
	// PlanVersion is the plan format the engine emitted (v1), Plan is its
	// canonical JSON, and PlanDigest is the sha256 of those exact bytes —
	// what makes "the same inputs produce the same merge" checkable.
	PlanVersion string
	Plan        []byte
	PlanDigest  string
	// The plan's roll-up.
	Applied    int
	KeptTarget int
	Carried    int
	Aborted    int
	Withheld   int
	// The Git saga.
	GitRef      *string
	GitSHA      *string
	GitState    SemanticMergeGitState
	GitError    string
	GitAttempts int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// SemanticMergeConflict is one conflict a merge CARRIED rather than
// resolved (docs/09 §8: keep both versions / explicit coexistence, and
// the contested/unresolved state main may keep). Rows are append-only:
// the record that main holds an open scientific disagreement is part of
// main's state history, and a later decision is a new merge.
type SemanticMergeConflict struct {
	ID            string
	MergeID       string
	ProjectID     string
	ResultStateID string
	// The classifier key, shared with ConflictResolution: the carried
	// record and the human decision it came from are unmistakably the same
	// conflict.
	TargetKind    ConflictResolutionTargetKind
	TargetID      string
	Code          string
	Category      string
	Fields        []string
	PayloadKeys   []string
	OtherObjectID *string
	Detail        string
	// Kind is the decision that carried it (keep_both,
	// explicit_coexistence or unresolved).
	Kind      ResolutionKind
	DecidedBy string
	Note      string
	// Both sides' versions stay — that is what carrying the conflict means.
	SourceVersionID string
	TargetVersionID *string
	CreatedAt       time.Time
}

// Sentinel errors of the merge store contract.
var (
	// ErrSemanticMergeNotFound: no merge row for the project/PR.
	ErrSemanticMergeNotFound = errors.New("domain: semantic merge not found")
)
