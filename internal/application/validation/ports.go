package validation

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lichman0405/post/internal/domain"
)

// SnapshotRepository is the persistence port the validate endpoint reads
// through: everything needed to assemble one branch snapshot. Reads only —
// the endpoint modifies no state (docs/22 §7), and the surface offers no
// write.
type SnapshotRepository interface {
	// BranchProject returns the project id the branch belongs to, or
	// ErrBranchNotFound when the branch does not exist (an existing branch
	// of another project reports the same outcome).
	BranchProject(ctx context.Context, branchID string) (string, error)
	// GetBranchHead returns the branch's head state, ErrStateNotFound when
	// the branch has no head yet, ErrBranchNotFound when the branch does
	// not exist.
	GetBranchHead(ctx context.Context, branchID string) (domain.ProjectState, error)
	// ListStates returns the branch's state chain, oldest first; an
	// unknown branch has no chain (empty, not an error).
	ListStates(ctx context.Context, branchID string) ([]domain.ProjectState, error)
	// ListCommits returns the branch's commit history, oldest first.
	ListCommits(ctx context.Context, branchID string) ([]domain.StateCommit, error)
	// ListStateObjectVersions returns the scientific object versions the
	// state's transition created (rows whose state_id equals the state).
	ListStateObjectVersions(ctx context.Context, stateID string) ([]domain.ScientificObjectVersion, error)
	// ListStateRelationVersions returns the relation versions the state's
	// transition created.
	ListStateRelationVersions(ctx context.Context, stateID string) ([]domain.RelationVersion, error)
}

// TxQuerier is the transaction read surface the guard needs: the in-flight
// database transaction of a state commit, so the gate can re-validate the
// rows as written — including the rows the commit itself just wrote.
// states.Transaction and pgx.Tx both satisfy it structurally.
type TxQuerier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// TxProbe reads a branch's chain and members through a transaction — the
// in-flight transaction of the commit being guarded, so every row the
// guard checks is the row the database will keep (or, on a blocked gate,
// the rows that roll back with it).
type TxProbe interface {
	ListStatesTx(ctx context.Context, tx TxQuerier, branchID string) ([]domain.ProjectState, error)
	ListCommitsTx(ctx context.Context, tx TxQuerier, branchID string) ([]domain.StateCommit, error)
	ListStateObjectVersionsTx(ctx context.Context, tx TxQuerier, stateID string) ([]domain.ScientificObjectVersion, error)
	ListStateRelationVersionsTx(ctx context.Context, tx TxQuerier, stateID string) ([]domain.RelationVersion, error)
}

// CommitFacts is everything the guard needs to describe the commit being
// guarded that the transaction cannot yet show it: the commit row is
// inserted after the guard runs (the guard is the last step inside the
// transaction, after the semantic writes), so the in-flight commit is
// assembled from these facts instead of read.
type CommitFacts struct {
	// ProjectID is the research boundary the commit belongs to (the
	// compare-and-swap has already proven the branch is in this project).
	ProjectID string
	// BranchID is the branch the transition is committed on.
	BranchID string
	// ActorID is the resolved actor of the commit.
	ActorID string
	// BaseStateID is the state the transition was built on; nil when the
	// branch had no head yet.
	BaseStateID *string
	// ResultStateID is the state the transition produced (the new head).
	ResultStateID string
	// Operations is the ordered operation summary that will be stored
	// verbatim on the commit row — validating against it is validating
	// exactly what will be persisted.
	Operations []domain.StateOperation
}
