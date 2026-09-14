package resolutions

import (
	"context"

	"github.com/lichman0405/post/internal/application/diffs"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/conflict"
)

// ConflictsPort computes the conflict report of a three-way triple. The
// production implementation is diffs.Service.Conflicts (T0405): the same
// call also validates the triple — the three states must exist and belong
// to the project — so the service reuses it for both the decision check
// and the state validation.
type ConflictsPort interface {
	Conflicts(ctx context.Context, in diffs.Params) (*conflict.Report, error)
}

// StorePort persists resolution plans. The production implementation is
// the pgx adapter in store_pg.go; tests use in-memory fakes.
type StorePort interface {
	// SavePlan upserts the given decisions for the triple in one
	// transaction (audit row included) and returns the full updated plan.
	SavePlan(ctx context.Context, in SavePlanParams) ([]domain.ConflictResolution, error)
	// ListPlan returns the decisions recorded for the triple, in stable
	// order.
	ListPlan(ctx context.Context, projectID, baseStateID, sourceStateID, targetStateID string) ([]domain.ConflictResolution, error)
	// ListEvidenceForObjectVersion returns the evidence assertions that
	// target one object version, joined with the evidence objects they
	// cite (the resolution UI's evidence context).
	ListEvidenceForObjectVersion(ctx context.Context, objectVersionID string) ([]EvidenceItem, error)
}

// SavePlanParams is the store's save input: the resolved domain rows (ID
// empty, the store assigns) and the plan's scope.
type SavePlanParams struct {
	ProjectID     string
	BaseStateID   string
	SourceStateID string
	TargetStateID string
	Decisions     []domain.ConflictResolution
}

// ProjectGate resolves the actor's membership role in the project for the
// write authorization, with the same require shape as the RSG write path
// (the production implementation is projects.Service: a caller who may
// not read the project gets the existence-hiding error before any role is
// decided).
type ProjectGate interface {
	GetMembership(ctx context.Context, actor domain.User, projectID string) (domain.ProjectMembership, error)
}

// Authz is the policy engine the write path evaluates (the production
// implementation is authz.MatrixEngine, wired exactly like the RSG
// service's).
type Authz interface {
	Authorize(ctx context.Context, req authz.Request) (authz.Decision, error)
}

// Reader shapes the project read gate the HTTP surface runs before any
// read (the same gate the validation surface uses: a project is exactly
// as readable as projects.Service.Get decides).
type Reader = projects.Reader
