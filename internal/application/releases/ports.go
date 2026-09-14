package releases

import (
	"context"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/manifest"
	"github.com/lichman0405/post/internal/rsg/schemareg"
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
