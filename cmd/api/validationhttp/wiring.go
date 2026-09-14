package validationhttp

import (
	"context"
	"net/http"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// BranchValidator is the application port the handler calls: run one gate
// over one branch and return the report. The production implementation is
// internal/application/validation.Service.
type BranchValidator interface {
	ValidateBranch(ctx context.Context, projectID, branchID string, gate rsgvalidation.Gate) (rsgvalidation.Report, error)
}

// ProjectReader is the visibility gate the handler runs before any
// validation report is returned: resolve whether the caller may read the
// project at all. The production implementation is
// internal/application/projects.Service — Get with a Reader evaluates the
// T0106 read matrix (public projects readable by anyone, private projects
// only by members) and answers ErrProjectNotFound for a denied read, the
// existence-hiding 404.
type ProjectReader interface {
	Get(ctx context.Context, r projects.Reader, projectID string) (domain.Project, error)
}

// Deps carries the adapters the validation API needs.
type Deps struct {
	Validator BranchValidator
	// Projects must be wired: a validation report about a branch is
	// exactly as visible as its project (the same read gate every other
	// project read runs).
	Projects ProjectReader
}

// New wires the handler.
func New(deps Deps) *API {
	return &API{handlers: &handlers{svc: deps.Validator, projects: deps.Projects}}
}

// API is the mounted validation surface.
type API struct {
	handlers *handlers
}

// Register mounts the validate endpoint. The route uses a remainder
// wildcard: Go's ServeMux rejects "{branchId}:validate" ("bad wildcard
// segment"), so the segment is parsed as "{branch...}" and the ":validate"
// suffix is split off in the handler — any path without the suffix is a
// 404.
func (a *API) Register(v1 *http.ServeMux) {
	v1.HandleFunc("POST /api/v1/projects/{projectId}/branches/{branch...}", a.handlers.handleValidate)
}
