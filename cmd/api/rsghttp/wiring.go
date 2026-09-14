package rsghttp

import (
	"context"
	"net/http"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/domain"
)

// Service is the application surface the handlers call: the RSG write
// commands and the object read. The production implementation is
// *rsg.Service.
type Service interface {
	CreateBranch(ctx context.Context, actor domain.User, projectID string, in rsg.CreateBranchInput) (domain.Branch, error)
	CreateObject(ctx context.Context, actor domain.User, projectID, branchID string, in rsg.CreateObjectInput) (rsg.ObjectResult, error)
	CreateObjectVersion(ctx context.Context, actor domain.User, projectID, branchID, objectID string, in rsg.CreateObjectVersionInput) (rsg.ObjectVersionResult, error)
	CreateRelation(ctx context.Context, actor domain.User, projectID, branchID string, in rsg.CreateRelationInput) (rsg.RelationResult, error)
	GetObject(ctx context.Context, r projects.Reader, projectID, branchID, objectID string) (rsg.ObjectResult, error)
}

// Deps carries the service the surface calls. The production wiring is in
// cmd/api/main.go (the rsg service composed over the persistence stores,
// the projects read gate and the matrix engine).
type Deps struct {
	Service Service
}

// New wires the handler.
func New(deps Deps) *API {
	return &API{handlers: &handlers{svc: deps.Service}}
}

// API is the mounted RSG surface.
type API struct {
	handlers *handlers
}

// Register mounts the RSG routes on the guarded v1 mux. The version route
// uses a remainder wildcard: Go's ServeMux rejects "{objectId}:version"
// ("bad wildcard segment"), so the segment is parsed as "{object...}" and
// the ":version" suffix is split off in the handler — the same shape the
// validation surface already uses for its ":validate" suffix.
func (a *API) Register(v1 *http.ServeMux) {
	v1.HandleFunc("POST /api/v1/projects/{projectId}/branches", a.handlers.handleCreateBranch)
	v1.HandleFunc("POST /api/v1/projects/{projectId}/branches/{branchId}/objects", a.handlers.handleCreateObject)
	v1.HandleFunc("POST /api/v1/projects/{projectId}/branches/{branchId}/objects/{object...}", a.handlers.handleCreateObjectVersion)
	v1.HandleFunc("POST /api/v1/projects/{projectId}/branches/{branchId}/relations", a.handlers.handleCreateRelation)
	v1.HandleFunc("GET /api/v1/projects/{projectId}/branches/{branchId}/objects/{objectId}", a.handlers.handleGetObject)
}
