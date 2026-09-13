package states

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lichman0405/post/internal/domain"
)

// Repository is the persistence port for project states and state commits
// (docs/52: application orchestrates against ports; adapters live in
// internal/persistence). Deliberately ABSENT from the surface: any method
// that could mutate an existing state or commit row. Both tables are
// append-only on two layers at once — no update path here, and the
// database rejects UPDATE/DELETE itself (migrations 00014/00015).
//
// CommitState is the transaction boundary the task exists for: the adapter
// must run the head advance, the state insert, the operation callback and
// the commit insert in ONE database transaction, so a traceable transition
// is never observable without its members and a failed transaction leaves
// no half state (docs/53: domain event and state change share one
// transaction; here the state change IS the domain event).
type Repository interface {
	// CommitState executes one state transition atomically:
	//
	//  1. the result state row is inserted (its content hash colliding
	//     with an existing state fails with ErrStateExists);
	//  2. the branch's head pointer advances from in.BaseStateID to the
	//     new state through a compare-and-swap — a head that moved
	//     underneath the commit fails with *StateConflictError
	//     (BRANCH_STATE_CONFLICT); a branch that does not exist in the
	//     project fails with ErrBranchNotFound;
	//  3. write runs inside the same transaction and receives the new
	//     state id, so the semantic rows it writes reference the state
	//     they belong to; its error is returned wrapped in
	//     *CommitWriteError and the whole transaction rolls back;
	//  4. the state commit row (actor, via, message, operation summary,
	//     base → result) is inserted.
	//
	// Any failure after the transaction began rolls the whole transition
	// back: no state, no commit, none of the callback's rows.
	CommitState(ctx context.Context, in CommitStateParams, write WriteFunc) (domain.ProjectState, domain.StateCommit, error)
	// CreateInitialState creates the project's genesis state: the empty
	// root with no parent, no branch and no commit. The canonical schema
	// cannot represent a commit without a branch (state_commits.branch_id
	// is NOT NULL) and the genesis root is not itself a semantic write —
	// it is the starting point semantic writes build on (branches are
	// T0205's domain; a later provisioning flow seeds one per project).
	CreateInitialState(ctx context.Context, in InitialStateParams) (domain.ProjectState, error)
	// GetState returns one state by id, or ErrStateNotFound.
	GetState(ctx context.Context, stateID string) (domain.ProjectState, error)
	// GetStateByHash returns the state of the project with the given
	// content hash, or ErrStateNotFound.
	GetStateByHash(ctx context.Context, projectID, stateHash string) (domain.ProjectState, error)
	// GetBranchHead returns the branch's current head state (the
	// branches.base_state_id projection, docs/21 §5), or
	// ErrStateNotFound when the branch has no head yet, ErrBranchNotFound
	// when the branch does not exist.
	GetBranchHead(ctx context.Context, branchID string) (domain.ProjectState, error)
	// ListStates returns the branch's state chain, oldest first.
	ListStates(ctx context.Context, branchID string) ([]domain.ProjectState, error)
	// GetCommit returns one state commit by id, or ErrCommitNotFound.
	GetCommit(ctx context.Context, commitID string) (domain.StateCommit, error)
	// ListCommits returns the branch's commit history, oldest first.
	ListCommits(ctx context.Context, branchID string) ([]domain.StateCommit, error)
	// ListStateObjectVersions returns the scientific object versions the
	// state's transition created (the state snapshot's direct object
	// members: rows whose state_id equals the state).
	ListStateObjectVersions(ctx context.Context, stateID string) ([]domain.ScientificObjectVersion, error)
	// ListStateRelationVersions returns the relation versions the state's
	// transition created (the state snapshot's direct relation members).
	ListStateRelationVersions(ctx context.Context, stateID string) ([]domain.RelationVersion, error)
}

// Transaction is the write surface the commit hands to the operation
// callback: the in-flight database transaction, so every semantic write
// the callback performs shares the commit's atomicity. pgx.Tx satisfies it
// and it is assignable to the sqlc query runner surface, so callbacks
// write through the generated query layer as usual.
type Transaction interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// WriteFunc performs the commit's semantic writes inside the transaction:
// create object/relation versions, assertions, attachments — every row it
// writes must carry stateID as its state_id, so the traceability
// acceptance criterion holds (each member names the transition it was
// created in). Returning a non-nil error aborts the whole commit.
type WriteFunc func(ctx context.Context, tx Transaction, stateID string) error

// CommitStateParams carries one prepared state transition. StateHash is
// the service-computed content address (domain.ComputeStateHash over
// BaseStateID + Operations): the adapter stores it as given, it never
// derives hashes itself.
type CommitStateParams struct {
	ProjectID string
	BranchID  string
	ActorID   string
	Via       domain.StateVia
	Message   string
	// Operations is the ordered operation summary, marshaled to canonical
	// JSON for state_commits.operation_summary by the adapter.
	Operations  []domain.StateOperation
	BaseStateID *string
	// StateHash is domain.ComputeStateHash(BaseStateID, Operations).
	StateHash string
	// GitCommitSHA pins the GitProvider commit the transition came from
	// (via git_compat); nil otherwise.
	GitCommitSHA *string
	// ManifestVersion is the manifest format version the state is written
	// under.
	ManifestVersion string
}

// InitialStateParams carries a genesis state creation.
type InitialStateParams struct {
	ProjectID string
	// StateHash is domain.ComputeStateHash(nil, nil) — the empty root's
	// content address, service-computed like every other state hash.
	StateHash string
	// GitCommitSHA pins the GitProvider commit the project's repository
	// was provisioned at; nil otherwise.
	GitCommitSHA *string
	// ManifestVersion is the manifest format version the state is written
	// under.
	ManifestVersion string
}
