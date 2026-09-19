package aborts

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
// narrow surface mainfreeze and releases take, and the production value is
// the projects service: a caller who may not see the project gets the
// existence-hiding refusal there, before any role is decided, so the
// authorization outcome cannot disclose whether the project exists.
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
//
// It declares no head read: the command never needs one. The proposal's
// version row is appended on the commit's own transaction and the PR pins
// its proposed state from the source branch's head when the adapter creates
// it, so a head read here would be a value nothing consumes.
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
	// the adapter's own transaction (migration 00089) — which is why the
	// replay path reads rather than re-creates.
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
	// log, or sciobjects.ErrVersionNotFound. The replay path uses it to
	// name the version the recorded abort is about (the row one number
	// below the one the request key names) without re-deriving the answer
	// the first call already gave.
	GetVersion(ctx context.Context, objectID string, versionNo int) (domain.ScientificObjectVersion, error)
	// GetVersionByAbortRequestKey returns the version an earlier abort
	// request carrying requestKey appended to objectID, or
	// sciobjects.ErrVersionNotFound. This is the idempotency read: the
	// state itself is the record.
	GetVersionByAbortRequestKey(ctx context.Context, objectID, requestKey string) (domain.ScientificObjectVersion, error)
	// AppendAbortVersionInTx is the command's ONLY write, and it runs on
	// the commit's transaction: it appends the aborted version and the
	// audit row of the abort together, so a version row in lifecycle
	// 'aborted' always has the record of who decided it, when and why.
	AppendAbortVersionInTx(ctx context.Context, tx states.Transaction, in AbortWriteParams) (domain.ScientificObjectVersion, error)
}

// EventRecorder is the transactional outbox surface, the same port shape
// the RSG service takes (internal/application/rsg/ports.go:169): the write
// surface is events.DBTX, which states.Transaction is assignable to, so the
// production value is events.Recorder{} with no adapter.
type EventRecorder interface {
	Record(ctx context.Context, db events.DBTX, e events.Event) error
}

// AbortWriteParams carries the one write this command makes.
//
// It is deliberately narrow: the version content (VersionParams, whose
// Abort and AbortRequestKey fields carry the record and the idempotency
// key) plus the audit entry. Everything the write needs to be atomic is in
// one value, so the adapter cannot write half of it.
type AbortWriteParams struct {
	// ObjectID names the object whose log the version is appended to.
	ObjectID string
	// ExpectedVersionNo is the object's version counter the append
	// compare-and-swaps on: the append wins only while the log is still
	// where the command read it.
	ExpectedVersionNo int
	// Version is the new version's content, with Abort set to the record
	// docs/46:7 requires and AbortRequestKey set to the request's
	// Idempotency-Key.
	Version sciobjects.VersionParams
	// Audit is the governance record of the abort (docs/26 lists abort
	// among the highest-risk audited actions). The adapter appends it
	// inside the same transaction as the version row.
	Audit domain.AuditEntry
}

// The production adapters, asserted at compile time so a signature change
// in a dependency is a build error here rather than a wiring surprise. The
// objects store is asserted where it is implemented
// (internal/persistence/scientific_object_store.go), because the
// application layer must not import persistence.
var (
	_ Branches     = (*branches.Service)(nil)
	_ PullRequests = (*pullrequests.Service)(nil)
	_ Commits      = (*states.Service)(nil)
	_ Authz        = (*authz.MatrixEngine)(nil)
)
