package reopens

import (
	"context"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/application/sciobjects"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
)

// Membership resolves the actor's project membership. It is the same
// narrow surface aborts takes, and the production value is the raw project
// store (as the freeze and publish commands are wired): a caller who may
// not see the project gets the same "no membership" answer as a stranger,
// before any role is decided, so the authorization outcome cannot disclose
// whether the project exists.
type Membership interface {
	GetMembership(ctx context.Context, projectID, actorID string) (domain.ProjectMembership, error)
}

// Authz is the permission-matrix engine. The production value is
// authz.NewMatrixEngine().
type Authz interface {
	Authorize(ctx context.Context, req authz.Request) (authz.Decision, error)
}

// Branches is the branch surface the command needs: the project's branches
// (to find main) and the proposal branch itself — and nothing else.
// Satisfied by *branches.Service.
type Branches interface {
	// List returns every branch of the project (active, merged, aborted).
	List(ctx context.Context, projectID string) ([]domain.Branch, error)
	// Create forks the proposal branch. ErrBranchNameTaken is an outcome
	// the command handles rather than reports — the name is derived from
	// the object and the Idempotency-Key, so a taken name means a previous
	// request with this key already forked it.
	Create(ctx context.Context, in branches.CreateBranchParams) (domain.Branch, error)
}

// PullRequests is the proposal surface. Satisfied by
// *pullrequests.Service.
type PullRequests interface {
	// Create opens the Research PR that carries the proposal. A repeated
	// creation key returns the proposal the first request opened, inside
	// the adapter's own transaction (migration 00089).
	Create(ctx context.Context, in pullrequests.CreatePullRequestParams) (domain.PullRequest, error)
	// GetByCreationKey returns the proposal a previous request with this
	// key opened, or ErrPullRequestNotFound.
	GetByCreationKey(ctx context.Context, projectID, creationKey string) (domain.PullRequest, error)
}

// Commits is the state-commit surface. Satisfied by *states.Service: the
// proposal's version row, its audit row and its domain event commit in one
// transaction or not at all.
type Commits interface {
	Commit(ctx context.Context, in states.CommitParams, write states.WriteFunc) (domain.ProjectState, domain.StateCommit, error)
}

// Objects is the version-log surface the command reads and the one write it
// makes. Satisfied by *persistence.ScientificObjectStore.
type Objects interface {
	// GetObject returns the object row, or sciobjects.ErrObjectNotFound.
	GetObject(ctx context.Context, objectID string) (domain.ScientificObject, error)
	// GetVersionByID returns one version row by its own id, or
	// sciobjects.ErrVersionNotFound.
	GetVersionByID(ctx context.Context, versionID string) (domain.ScientificObjectVersion, error)
	// GetVersion returns one version row by its position in the object's
	// log, or sciobjects.ErrVersionNotFound. The replay path uses it to name
	// the version the recorded reopen is about (the row one number below the
	// one the request key names) without re-deriving the answer the first
	// call already gave.
	GetVersion(ctx context.Context, objectID string, versionNo int) (domain.ScientificObjectVersion, error)
	// GetVersionByReopenRequestKey returns the version an earlier reopen
	// request carrying requestKey appended to objectID, or
	// sciobjects.ErrVersionNotFound. This is the idempotency read: the state
	// itself is the record.
	//
	// It is deliberately NOT the abort's read. The two commands' keys live in
	// two columns (migrations 00100 and 00123) precisely so that one column
	// cannot answer both: a shared column would let a reopen request's key
	// hand this command a row the abort path wrote, or vice versa — a replay
	// of the wrong transition, reported as success.
	GetVersionByReopenRequestKey(ctx context.Context, objectID, requestKey string) (domain.ScientificObjectVersion, error)
	// AppendReopenVersionInTx is the command's ONLY write, and it runs on
	// the commit's transaction: it appends the reopened version and the
	// audit row of the reopen together, so a version row in lifecycle
	// 'reopened' always has the record of who decided it, when and why.
	AppendReopenVersionInTx(ctx context.Context, tx states.Transaction, in ReopenWriteParams) (domain.ScientificObjectVersion, error)
}

// EventRecorder is the transactional outbox surface, the same port shape the
// RSG service takes (internal/application/rsg/ports.go:169): the write
// surface is events.DBTX, which states.Transaction is assignable to, so the
// production value is events.Recorder{} with no adapter.
type EventRecorder interface {
	Record(ctx context.Context, db events.DBTX, e events.Event) error
}

// ReopenWriteParams carries the one write this command makes.
//
// It is deliberately narrow: the version content (VersionParams, whose
// Reopen and ReopenRequestKey fields carry the record and the idempotency
// key) plus the audit entry. Everything the write needs to be atomic is in
// one value, so the adapter cannot write half of it.
type ReopenWriteParams struct {
	// ObjectID names the object whose log the version is appended to.
	ObjectID string
	// ExpectedVersionNo is the object's version counter the append
	// compare-and-swaps on: the append wins only while the log is still
	// where the command read it.
	ExpectedVersionNo int
	// Version is the new version's content, with Reopen set to the record
	// this command's task requires and ReopenRequestKey set to the request's
	// Idempotency-Key.
	Version sciobjects.VersionParams
	// Audit is the governance record of the reopen (docs/26 lists
	// abort/reopen together among the highest-risk audited actions). The
	// adapter appends it inside the same transaction as the version row.
	Audit domain.AuditEntry
}

// The production adapters, asserted at compile time so a signature change in
// a dependency is a build error here rather than a wiring surprise. The
// objects store is asserted where it is implemented
// (internal/persistence/scientific_object_reopen.go), because the
// application layer must not import persistence.
var (
	_ Branches     = (*branches.Service)(nil)
	_ PullRequests = (*pullrequests.Service)(nil)
	_ Commits      = (*states.Service)(nil)
	_ Authz        = (*authz.MatrixEngine)(nil)
)
