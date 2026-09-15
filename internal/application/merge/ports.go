package merge

import (
	"context"

	"github.com/lichman0405/post/internal/application/diffs"
	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/relations"
	"github.com/lichman0405/post/internal/application/sciobjects"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/gitprovider"
	"github.com/lichman0405/post/internal/rsg/diff"
	"github.com/lichman0405/post/internal/rsg/integrity"
	"github.com/lichman0405/post/internal/rsg/manifest"
)

// StorePort is the persistence surface the merge needs (docs/52: the
// application orchestrates against ports; the adapter lives in
// internal/persistence). Two kinds of method live here and the split is
// deliberate:
//
//   - the unlocked reads the plan is computed from (a PR, two branches, the
//     version counters);
//   - the LOCKED re-reads and the writes, which all run on the commit
//     transaction so the plan, the accepted state and the merge record are
//     one atomic fact.
type StorePort interface {
	// GetPullRequest reads one PR by project and number, or
	// ErrPullRequestNotFound.
	GetPullRequest(ctx context.Context, projectID string, number int64) (domain.PullRequest, error)
	// GetBranch reads one project-scoped branch, or ErrBranchNotFound.
	GetBranch(ctx context.Context, projectID, branchID string) (domain.Branch, error)
	// VersionHeads reads the current version counters of the objects and
	// relations the plan will materialize. This is the optimistic
	// prediction read: the write transaction re-reads them under a row lock
	// and refuses (retryably) if they moved.
	VersionHeads(ctx context.Context, objectIDs, relationIDs []string) (VersionHeads, error)
	// LockMergeScope re-reads the merge's rows ON THE COMMIT TRANSACTION,
	// locking them (the branches FOR UPDATE, then the PR).
	LockMergeScope(ctx context.Context, tx states.Transaction, in LockScopeParams) (MergeScope, error)
	// LockVersionHeads is VersionHeads on the commit transaction, under the
	// same row locks the writes take.
	LockVersionHeads(ctx context.Context, tx states.Transaction, objectIDs, relationIDs []string) (VersionHeads, error)
	// WriteMerge inserts the merge record and the conflicts it carries, on
	// the commit transaction.
	WriteMerge(ctx context.Context, tx states.Transaction, in WriteMergeParams) (domain.SemanticMerge, error)
	// MarkPullRequestMerged closes the PR (merge_ready → merged) on the
	// commit transaction; anything but merge_ready fails closed.
	MarkPullRequestMerged(ctx context.Context, tx states.Transaction, projectID, pullRequestID string) error
	// MarkBranchMerged closes the source branch (active → merged) on the
	// commit transaction.
	MarkBranchMerged(ctx context.Context, tx states.Transaction, branchID string) error
	// CompleteGitStep records a finished Git step and returns the updated
	// row.
	CompleteGitStep(ctx context.Context, in GitStepParams) (domain.SemanticMerge, error)
	// RecordGitAttempt records a failed (or still-pending) Git step and
	// returns the updated row.
	RecordGitAttempt(ctx context.Context, in GitStepParams) (domain.SemanticMerge, error)
	// GetMergeByPullRequest reads a PR's merge record, or
	// domain.ErrSemanticMergeNotFound.
	GetMergeByPullRequest(ctx context.Context, projectID, pullRequestID string) (domain.SemanticMerge, error)
	// ListCarriedConflicts returns the conflicts a merge carried into the
	// accepted state, in insertion order.
	ListCarriedConflicts(ctx context.Context, mergeID string) ([]domain.SemanticMergeConflict, error)
	// LookupMergeCreation reads the Idempotency-Key ledger (migration
	// 00070): the merge the key already created, or nil when the key is
	// unused. The write side is WriteMergeParams.IdempotencyKey — the
	// ledger row lands in the merge's own transaction.
	LookupMergeCreation(ctx context.Context, projectID, idempotencyKey string) (*domain.SemanticMerge, error)
}

// LockScopeParams names the rows LockMergeScope locks.
type LockScopeParams struct {
	ProjectID      string
	PRNumber       int64
	SourceBranchID string
	TargetBranchID string
}

// MergeScope is the locked truth of the merge's rows: what the transaction
// sees of the PR and the two branches. The service compares it against the
// plan's inputs and refuses on any difference — the fence the T0402 review
// asked for.
type MergeScope struct {
	PullRequest domain.PullRequest
	Source      domain.Branch
	Target      domain.Branch
}

// VersionHeads is the current version counter of every container the merge
// will append a version to. A container that does not exist is absent from
// the map (the merge then fails with *MissingEntityError rather than
// inventing a head).
type VersionHeads struct {
	Objects   map[string]int
	Relations map[string]int
}

// Equal reports whether two head readings agree, naming the first
// disagreement. Both maps must cover the same containers — the locked read
// is compared against the prediction read entry by entry.
func (h VersionHeads) Equal(other VersionHeads) error {
	if err := headsEqual("object", h.Objects, other.Objects); err != nil {
		return err
	}
	return headsEqual("relation", h.Relations, other.Relations)
}

// headsEqual compares one side of a head reading.
func headsEqual(kind string, predicted, actual map[string]int) error {
	for id, want := range predicted {
		got, ok := actual[id]
		if !ok {
			return &MissingEntityError{Kind: kind, ID: id}
		}
		if got != want {
			return &versionHeadsMovedError{kind: kind, id: id, predicted: want, actual: got}
		}
	}
	return nil
}

// WriteMergeParams carries one merge record. PlanJSON is the engine's
// canonical plan bytes and PlanDigest their sha256: the digest is what makes
// "these three states under these decisions produce this merge" checkable, by
// recomputing the plan and hashing the recomputed bytes. The column it lands in
// is jsonb, which normalizes whitespace and key order, so the stored JSON is the
// same VALUE as those bytes and not the same text — the audit path is
// recompute-from-the-triple, never a byte comparison against the row.
type WriteMergeParams struct {
	ProjectID      string
	PullRequestID  string
	SourceBranchID string
	TargetBranchID string
	BaseStateID    string
	SourceStateID  string
	TargetStateID  string
	ResultStateID  string
	ActorID        string
	PlanVersion    string
	PlanJSON       []byte
	PlanDigest     string
	Applied        int
	KeptTarget     int
	Carried        int
	Aborted        int
	Withheld       int
	// CarriedConflicts are the conflicts the merge carries into the
	// accepted state (docs/09 §8), one row each.
	CarriedConflicts []CarriedConflict
	// GitRef is the provider ref the saga will advance ("" when neither
	// branch has one recorded).
	GitRef *string
	// GitState is the state the saga starts in ('pending').
	GitState domain.SemanticMergeGitState
	// IdempotencyKey is the request's Idempotency-Key when it carried one.
	// The ledger row (migration 00070) is written on this same transaction,
	// so an entry and the merge it points at cannot come apart — which is
	// what makes a replay a read.
	IdempotencyKey *string
	// Audit is the audit row this merge appends, written on this same
	// transaction (docs/22 §3: the merge, its ledger entry, its audit row
	// and its domain event are one unit). The store fills in the target ref
	// once the insert has assigned the merge id.
	Audit domain.AuditEntry
}

// CarriedConflict is one conflict the merge carries rather than resolves:
// both sides' versions stay, and the human decision that kept them is
// recorded with the conflict.
type CarriedConflict struct {
	TargetKind      domain.ConflictResolutionTargetKind
	TargetID        string
	Code            string
	Category        string
	Fields          []string
	PayloadKeys     []string
	OtherObjectID   *string
	Detail          string
	Decision        domain.ResolutionKind
	DecidedBy       string
	Note            string
	SourceVersionID string
	TargetVersionID *string
}

// GitStepParams carries one Git saga step. Error is the failure text the
// saga records ("" for a completed step).
type GitStepParams struct {
	MergeID string
	Ref     *string
	SHA     string
	State   domain.SemanticMergeGitState
	Error   string
}

// DiffPort composes the engine inputs of the base/source/target triple. The
// production implementation is diffs.Service.Inputs (T0401): the same reads
// the conflict report is computed from, so the merge and the detector can
// never disagree about what changed.
type DiffPort interface {
	Inputs(ctx context.Context, in diffs.Params) (diff.Inputs, error)
}

// PlanPort reads the human resolution plan recorded for the triple. The
// production implementation is resolutions.Service.Plan (T0407): the
// decisions the humans saved, keyed by the pinned states.
type PlanPort interface {
	Plan(ctx context.Context, projectID, baseStateID, sourceStateID, targetStateID string) ([]domain.ConflictResolution, error)
}

// CommitPort runs one state transition. The production implementation is
// states.Service: the merge's writes all happen inside the transaction it
// opens, with the main gate re-validated server-side before the commit
// lands.
type CommitPort interface {
	Commit(ctx context.Context, in states.CommitParams, write states.WriteFunc) (domain.ProjectState, domain.StateCommit, error)
}

// ObjectWriter appends one scientific object version on the commit
// transaction. The production implementation is
// persistence.ScientificObjectStore, whose transaction-scoped version write
// carries the same signature (and the same expect-a-specific-head CAS) the
// RSG write path uses.
//
// Only APPENDS exist here, and that is the merge's whole write surface: a
// three-way diff can only report an object the database has versions of, so
// the container always exists — the merge appends the version the accepted
// state carries. It never creates a container, and it never edits one
// (invariant 8: nothing disappears, state only evolves).
type ObjectWriter interface {
	CreateVersionInTx(ctx context.Context, tx states.Transaction, objectID string, expected int, in sciobjects.VersionParams) (domain.ScientificObjectVersion, error)
}

// RelationWriter appends one relation version on the commit transaction.
// The production implementation is persistence.RelationStore
// (AppendRelationVersionInTx); the same append-only rule as ObjectWriter, and
// the same expect-a-specific-head CAS.
type RelationWriter interface {
	AppendRelationVersionInTx(ctx context.Context, tx states.Transaction, relationID string, expected int, in relations.VersionParams) (domain.RelationVersion, error)
}

// ProjectGate resolves the actor's membership role in the project, with the
// same require shape as the RSG write path (the production implementation
// is projects.Service: a caller who may not read the project gets the
// existence-hiding error before any role is decided).
type ProjectGate interface {
	GetMembership(ctx context.Context, actor domain.User, projectID string) (domain.ProjectMembership, error)
}

// Authz is the policy engine the merge evaluates. The production
// implementation is authz.MatrixEngine, wired exactly like the RSG
// service's: the merge row is ActionMergeMain's maintainer gate.
type Authz interface {
	Authorize(ctx context.Context, req authz.Request) (authz.Decision, error)
}

// GitMerger performs the provider-side pull request merge — the second half
// of the merge saga. The production adapter is T0409's (the platform's Git
// service identity merging the PR in Gitea); until it is wired the saga
// records `pending` and stays retryable rather than pretending the Git half
// happened.
//
// Note what this port does NOT have: a push. main is advanced by a
// provider-side PR merge and by nothing else, which is why the platform
// layer (gitprovider.RefGuard) can decide legitimacy from the actor alone.
type GitMerger interface {
	MergePullRequest(ctx context.Context, in GitMergeRequest) (GitMergeResult, error)
}

// GitMergeRequest names one provider-side PR merge.
type GitMergeRequest struct {
	ProjectID string
	// Number is the platform PR number the provider PR mirrors.
	Number int64
	// TargetRef is the full ref the merge advances; SourceRef the ref it
	// merges from.
	TargetRef string
	SourceRef string
	// TargetSHA and SourceSHA pin the refs the merge is expected to move,
	// so a provider-side merge can never land on a different pair than the
	// one the plan was computed over.
	TargetSHA string
	SourceSHA string
}

// GitMergeResult is what the provider reports back: the merge commit it
// created and the identity that created it.
type GitMergeResult struct {
	// SHA is the new head of the target ref.
	SHA string
	// Actor is the provider login that performed the update. It is checked
	// against gitprovider.RefGuard, which decides whether the platform
	// accepts the update as a merge rather than a bypass.
	Actor string
}

// IntegrityChecker re-runs one PR's integrity review server-side. The
// production implementation is prchecks.Service (T0408).
//
// The merge runs it because docs/22 §7 makes a command re-run its
// validation instead of trusting a precheck: the review a human read on
// the PR page is exactly the review that must hold at the moment main
// moves, and a client cannot hand the command a pre-computed verdict.
type IntegrityChecker interface {
	CheckPullRequest(ctx context.Context, projectID string, number int64) (integrity.Report, error)
}

// PolicyReader reads the policy in force for one project: the organization
// lower bound, the project overlay and their merge. The production
// implementation is policy.Service.EffectivePolicy.
type PolicyReader interface {
	EffectivePolicy(ctx context.Context, actor domain.User, projectID string) (domain.EffectivePolicy, error)
}

// RuleEvaluator answers one typed governance question about a policy
// document (the production implementation is policy.RuleEvaluator). Asking
// through the typed surface — rather than reading policy_json here — is
// what keeps one evaluation contract behind every enforcement site.
type RuleEvaluator interface {
	Evaluate(ctx context.Context, p domain.Policy, q policy.Query) (policy.Decision, error)
}

// EventRecorder is the outbox write surface, the same port the RSG service
// composes (internal/application/rsg/ports.go). The production
// implementation is events.Recorder. Recording happens INSIDE the merge's
// transaction: the event commits with the accepted state or not at all
// (docs/53).
type EventRecorder interface {
	Record(ctx context.Context, db events.DBTX, e events.Event) error
}

// RefGuard is the platform policy over the target ref update
// (gitprovider.RefGuard, T0302): main may only be advanced by the configured
// merge-service identity, through a provider-side PR merge. The merge
// service runs it over the update it just caused — an update the guard
// refuses is recorded as a failed saga step, never as a legitimate merge.
type RefGuard = gitprovider.RefGuard

// RefUpdate is one ref transition the guard judges.
type RefUpdate = gitprovider.RefUpdate

// ManifestVersion is the manifest format the merge's state transition is
// written under (the same v1 the RSG writes use).
const ManifestVersion = manifest.FormatV1
