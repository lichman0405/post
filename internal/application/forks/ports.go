package forks

import (
	"context"
	"time"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/gitprovider"
)

// Fork is one lineage row: the fork project, the project it was forked
// from, the actor who forked it, and the branch pair the content travelled
// along. RelationType is the canonical vocabulary's value ('forked_from',
// docs/44) and is stored, not implied by the table's name.
type Fork struct {
	ForkProjectID   string
	ParentProjectID string
	ForkedBy        string
	RelationType    string
	SourceBranchID  string
	ForkBranchID    string
	// ForkedSHA is the parent commit the import landed in the fork's
	// repository: nil until the import has run, then fixed (the guard
	// trigger of 00086 makes it write-once).
	ForkedSHA *string
	CreatedAt time.Time
}

// ClaimRequest carries one fork's lineage insert. The audit entry travels
// with it so the lineage row, the audit row and the domain event are one
// transaction's facts (docs/53) — the store stamps the correlation id,
// owns the event payload, and derives the event's visibility from the
// projects the row names.
type ClaimRequest struct {
	ForkProjectID   string
	ParentProjectID string
	ActorID         string
	SourceBranchID  string
	ForkBranchID    string
	Audit           domain.AuditEntry
}

// StorePort is the persistence surface the fork service needs. Two of its
// methods are compare-and-swaps and neither reads before it writes:
// ClaimFork loses on the (parent, actor) unique key instead of checking
// first, and SetForkSHA moves the one-shot cell only while it is NULL
// (internal/persistence/fork_store.go).
type StorePort interface {
	// ClaimFork inserts the lineage row and, when this call wins it,
	// writes the audit row and the domain event in the same transaction.
	// inserted=false means the (parent, actor) pair already had a fork:
	// the existing row comes back and NOTHING was written — no second
	// lineage row, audit row or event, which is what makes a repeated
	// fork request a no-op by construction rather than by remembering
	// request keys.
	ClaimFork(ctx context.Context, req ClaimRequest) (fork Fork, inserted bool, err error)
	// FindFork reads one actor's fork of one project, ok=false when there
	// is none.
	FindFork(ctx context.Context, parentProjectID, actorID string) (fork Fork, ok bool, err error)
	// ForkOfProject reads the lineage row of a fork project (by the fork
	// project's id, which is the table's primary key).
	ForkOfProject(ctx context.Context, forkProjectID string) (fork Fork, ok bool, err error)
	// ForkOfBranch reads the lineage row of the fork project that owns
	// branchID, ok=false when the branch belongs to a project that is not
	// a fork. It is the read the allow_from_fork condition is resolved
	// against: the caller holds a source branch id, and the question is
	// which lineage governs it.
	ForkOfBranch(ctx context.Context, branchID string) (fork Fork, ok bool, err error)
	// ListForks returns a parent's forks, newest first.
	ListForks(ctx context.Context, parentProjectID string) ([]Fork, error)
	// SetForkSHA writes the fork point once: it matches only while
	// forked_sha is still NULL, and reports whether this call was the one
	// that set it.
	SetForkSHA(ctx context.Context, forkProjectID, sha string) (bool, error)
	// PersonalProjectCreator reads who created the personal project that
	// holds a slug — the read that explains a lost slug insert, which is
	// what the insert itself is for (the CAS discipline: the write decides,
	// the read only explains). held=false means no personal project answers
	// to that slug. It is scoped to personal projects (organization_id IS
	// NULL) because that is the unique index the fork's insert can violate
	// (projects_personal_slug_idx, 00019).
	PersonalProjectCreator(ctx context.Context, slug string) (creatorID string, held bool, err error)
}

// ProjectGate is the project surface the fork uses: the reads that decide
// whether the caller may fork at all (visibility-aware, existence-hiding),
// the membership read that resolves the caller's matrix column, and the
// create that makes the fork the actor's own project.
type ProjectGate interface {
	Get(ctx context.Context, r projects.Reader, projectID string) (domain.Project, error)
	GetMembership(ctx context.Context, actor domain.User, projectID string) (domain.ProjectMembership, error)
	Create(ctx context.Context, actor domain.User, in projects.CreateProjectInput) (domain.Project, domain.ProjectMembership, error)
}

// BranchReader is the branch read surface (branches.Service).
type BranchReader interface {
	Get(ctx context.Context, projectID, branchID string) (domain.Branch, error)
	List(ctx context.Context, projectID string) ([]domain.Branch, error)
}

// BranchCreator creates the fork's branch through the canonical RSG
// service, so the fork's branch is an ordinary branch: its row, its
// genesis base state, its Git ref row and its audit trail are exactly what
// any other branch creation produces, and the actor is authorized for it
// in their own project like anybody else.
type BranchCreator interface {
	CreateBranch(ctx context.Context, actor domain.User, projectID string, in rsg.CreateBranchInput) (domain.Branch, error)
}

// RepoProvisioner makes a project's GitProvider repository exist
// (gitprovider.Provisioner.Provision). Idempotent — the canonical
// provision row is its compare-and-swap — so calling it for a project that
// already has one is a no-op.
type RepoProvisioner interface {
	Provision(ctx context.Context, projectID string) error
}

// ContentImporter copies the parent's commit into the fork's repository
// and records the copy through the same push inspection every other
// arrival runs (gitprovider.ForkImporter). It is a port rather than a bare
// call so the service's tests can drive the flow without a provider.
type ContentImporter interface {
	Import(ctx context.Context, in gitprovider.ForkImportRequest) (gitprovider.ForkImportResult, error)
}

// PullRequestOpener is the EXISTING pull-request path (T0205): the fork
// path proposes through it rather than growing a second way to open a PR,
// so reviews, the semantic gate and the PR state machine are the same
// machinery for an external contribution as for an internal one.
type PullRequestOpener interface {
	Create(ctx context.Context, in pullrequests.CreatePullRequestParams) (domain.PullRequest, error)
}

// Authz is the policy engine the three conditional cells are resolved
// through (authz.Engine).
type Authz = authz.Engine
