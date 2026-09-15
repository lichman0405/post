package milestones

import (
	"context"

	"github.com/lichman0405/post/internal/domain"
)

// Ports (docs/52: application orchestrates against ports; adapters live
// in internal/persistence). The command needs exactly the reads its
// contract names: the project row and the actor's membership (the authz
// class input), the release named by an optional link (the boundary check
// — the link must name a release of the same project), and the milestone
// store.

// ProjectAuthzPort is the slice of project state the create needs: the
// project row (existence — a milestone names a project) and the actor's
// membership (the authz class input). The production adapter is
// persistence.ProjectStore; the adapters answer with the projects
// application's sentinels, which the command maps onto its own.
type ProjectAuthzPort interface {
	// GetProject returns the project or projects.ErrProjectNotFound.
	GetProject(ctx context.Context, projectID string) (domain.Project, error)
	// GetMembership returns the actor's membership or
	// projects.ErrMemberNotFound.
	GetMembership(ctx context.Context, projectID, userID string) (domain.ProjectMembership, error)
}

// ReleasePort resolves the release named by an optional milestone link.
// The production implementation is persistence.ReleaseStore; a release
// that does not exist in the project answers releases.ErrReleaseNotFound
// (the store's existence-hiding read — the command maps it).
type ReleasePort interface {
	GetRelease(ctx context.Context, projectID, releaseID string) (domain.Release, error)
}

// StorePort is the persistence port for the milestone rows: the create
// (transactional — the milestone row, its idempotency ledger entry and
// its audit row commit together) and the reads that render the timeline.
type StorePort interface {
	// CreateMilestone inserts one milestone row. idempotencyKey names
	// the caller's Idempotency-Key (nil when none was sent): a key that
	// already created a milestone for the project returns that milestone
	// instead (idempotent replay — the returned row IS the first
	// create's row).
	CreateMilestone(ctx context.Context, m domain.Milestone, audit domain.AuditEntry, idempotencyKey *string) (domain.Milestone, error)
	// LookupCreation resolves the milestone an Idempotency-Key already
	// created (nil, nil when the key has no entry yet). The command's
	// replay fast path: checked before any release resolution, so a
	// replay never re-validates the link — the same key returns the
	// first create's row forever.
	LookupCreation(ctx context.Context, projectID, idempotencyKey string) (*domain.Milestone, error)
	// ListMilestones returns the project's timeline: occurred_at
	// ascending, creation order breaking date ties.
	ListMilestones(ctx context.Context, projectID string) ([]domain.Milestone, error)
	// GetMilestone returns one milestone of the project, or
	// ErrMilestoneNotFound — also for a milestone of another project
	// (existence hiding, docs/45).
	GetMilestone(ctx context.Context, projectID, milestoneID string) (domain.Milestone, error)
}
