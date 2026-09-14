package releases

import (
	"context"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/manifest"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// Ports (docs/52: application orchestrates against ports; adapters live
// in internal/persistence). The builder needs exactly the reads its
// contract names: the state row (its branch decides the accepted-main
// origin), the state's canonical export (T0206, via the manifests
// service), the project row (the organization the org-policy pin must
// belong to), the project's branches (which one is main), the pinned
// policy version rows, the review record of the lineage's research PRs,
// and the schema registry (the content hashes the schema pins carry).

// StatePort resolves the state being released. The production
// implementation is persistence.StateStore; a missing state reports
// states.ErrStateNotFound (the port contract, so the service can map it).
type StatePort interface {
	GetState(ctx context.Context, stateID string) (domain.ProjectState, error)
}

// StateManifestPort renders the state's canonical export to embed. The
// production implementation is the manifests application service
// (manifests.Service.Export); its errors are the manifests package's
// sentinels, which the service maps onto its own.
type StateManifestPort interface {
	Export(ctx context.Context, stateID string) (*manifest.Manifest, error)
}

// ProjectPort resolves the project row: existence (the boundary — a
// release names a project) and the owning organization (the org-policy
// pin's scope). The production implementation is
// persistence.ProjectStore; a missing project reports
// projects.ErrProjectNotFound.
type ProjectPort interface {
	GetProject(ctx context.Context, projectID string) (domain.Project, error)
}

// MainBranchPort lists the project's branches so the service can resolve
// the main branch (domain.MainBranchName). The production implementation
// is persistence.BranchStore.
type MainBranchPort interface {
	ListBranches(ctx context.Context, projectID string) ([]domain.Branch, error)
}

// PolicyVersionPort resolves one pinned policy version by id — any age
// (old versions stay queryable forever, T0603). The production
// implementation is persistence.PolicyStore; a missing version reports
// policy.ErrPolicyNotFound.
type PolicyVersionPort interface {
	GetVersion(ctx context.Context, id string) (domain.PolicyVersion, error)
}

// ReleaseReviewPort reads the review record of the released lineage: the
// reviews of the research PRs targeting main whose proposed states are
// the state or one of its ancestors. The production implementation is
// persistence.ReleaseStore.
type ReleaseReviewPort interface {
	ListReleaseReviews(ctx context.Context, stateID, mainBranchID string) ([]ReviewRecord, error)
}

// SchemaRegistryPort resolves the registered schema versions the manifest
// pins. The production implementation is *schemareg.Registry.
type SchemaRegistryPort interface {
	Get(ref schemareg.Ref) (*schemareg.Schema, bool)
}

// The command ports below (T0606): the release command's boundary —
// authorization resolution, the policy-in-force resolution, the
// server-side release gate, and the release store. The builder ports
// above stay the pure reads of the manifest render; the command composes
// them.

// ProjectAuthzPort is the slice of project state the release command
// needs: the project row (the owning organization, for the org-policy
// pin) and the actor's membership (the authz class input). The
// production adapter is persistence.ProjectStore; the adapters answer
// with the projects application's sentinels, which the command maps onto
// its own.
type ProjectAuthzPort interface {
	// GetProject returns the project or projects.ErrProjectNotFound.
	GetProject(ctx context.Context, projectID string) (domain.Project, error)
	// GetMembership returns the actor's membership or
	// projects.ErrMemberNotFound.
	GetMembership(ctx context.Context, projectID, userID string) (domain.ProjectMembership, error)
}

// PolicyLatestPort resolves the policy version in force for one scope —
// "the policy in force at release time" (docs/12 §5), pinned explicitly
// by the command, never re-derived later. The production implementation
// is persistence.PolicyStore (its Latest); a scope without a policy
// answers policy.ErrPolicyNotFound, which the command treats as "no
// pin".
type PolicyLatestPort interface {
	Latest(ctx context.Context, scope domain.PolicyScope) (domain.PolicyVersion, error)
}

// BranchHeadPort resolves a branch's current head state. The production
// implementation is persistence.StateStore (GetBranchHead); a branch
// without states answers states.ErrStateNotFound.
type BranchHeadPort interface {
	GetBranchHead(ctx context.Context, branchID string) (domain.ProjectState, error)
}

// ReleaseGatePort runs the progressive validation ladder's release gate
// over one branch's persisted snapshot with the release facts attached —
// the server-side re-validation the create command demands (docs/22 §7).
// The production implementation is *validation.Service
// (ValidateBranchWithFacts).
type ReleaseGatePort interface {
	ValidateBranchWithFacts(ctx context.Context, projectID, branchID string, gate rsgvalidation.Gate, release *rsgvalidation.ReleaseFacts, asset *rsgvalidation.AssetFacts) (rsgvalidation.Report, error)
}

// ReleaseStorePort is the persistence port for the immutable release
// rows: the create (transactional — the release row, its idempotency
// ledger entry, its audit row and its research event commit together),
// and the reads that render only from the stored snapshot.
type ReleaseStorePort interface {
	// CreateRelease inserts one release row. idempotencyKey names the
	// caller's Idempotency-Key (nil when none was sent): a key that
	// already created a release for the project returns that release
	// instead (idempotent replay — the returned row IS the first
	// create's row). A fresh create whose version already exists
	// answers ErrVersionTaken.
	CreateRelease(ctx context.Context, r domain.Release, audit domain.AuditEntry, idempotencyKey *string) (domain.Release, error)
	// LookupCreation resolves the release an Idempotency-Key already
	// created (nil, nil when the key has no entry yet). The command's
	// replay fast path: checked before any snapshot resolution, so a
	// replay never re-runs the gate — the same key returns the first
	// create's row forever.
	LookupCreation(ctx context.Context, projectID, idempotencyKey string) (*domain.Release, error)
	// ListReleases returns the project's releases, newest first.
	ListReleases(ctx context.Context, projectID string) ([]domain.Release, error)
	// GetRelease returns one release of the project, or
	// ErrReleaseNotFound — also for a release of another project
	// (existence hiding, docs/45).
	GetRelease(ctx context.Context, projectID, releaseID string) (domain.Release, error)
}
