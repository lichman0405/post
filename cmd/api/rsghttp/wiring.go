package rsghttp

import (
	"context"
	"net/http"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/domain"
)

// Service is the application surface the handlers call: the RSG write
// commands, the object reads and the graph query (T0209). The production
// implementation is *rsg.Service.
type Service interface {
	CreateBranch(ctx context.Context, actor domain.User, projectID string, in rsg.CreateBranchInput) (domain.Branch, error)
	CreateObject(ctx context.Context, actor domain.User, projectID, branchID string, in rsg.CreateObjectInput) (rsg.ObjectResult, error)
	CreateObjectVersion(ctx context.Context, actor domain.User, projectID, branchID, objectID string, in rsg.CreateObjectVersionInput) (rsg.ObjectVersionResult, error)
	CreateRelation(ctx context.Context, actor domain.User, projectID, branchID string, in rsg.CreateRelationInput) (rsg.RelationResult, error)
	CreateEvidenceAssertion(ctx context.Context, actor domain.User, projectID, branchID string, in rsg.CreateEvidenceAssertionInput) (rsg.EvidenceAssertionResult, error)
	GetObject(ctx context.Context, r projects.Reader, projectID, branchID, objectID string) (rsg.ObjectResult, error)
	GetObjectDetail(ctx context.Context, r projects.Reader, projectID, branchID, objectID string, versionNo *int) (rsg.ObjectDetail, error)
	Query(ctx context.Context, r projects.Reader, projectID string, in rsg.QueryInput) (rsg.QueryResult, error)
	ResearchOutline(ctx context.Context, r projects.Reader, projectID string) (rsg.ResearchOutline, error)
	ProjectOverview(ctx context.Context, r projects.Reader, projectID string) (rsg.ProjectOverview, error)
}

// Deps carries the service the surface calls plus the two graph reads the
// object detail page's graph tabs render. The production wiring is in
// cmd/api/main.go (the rsg service composed over the persistence stores, the
// projects read gate and the matrix engine; the provenance projection store
// and the evidence-graph service the JSON routes read through as well).
//
// Provenance and Evidence are optional by contract: a build without them
// serves the page with those two tabs stating that the graph could not be
// read, which is the same answer a failing read gets. nil never means "empty
// graph" — an empty graph is a claim about the data, and an unwired reader
// knows nothing about it.
type Deps struct {
	Service    Service
	Provenance ProvenanceReader
	Evidence   EvidenceReader
}

// New wires the handler.
func New(deps Deps) *API {
	return &API{handlers: &handlers{
		svc:        deps.Service,
		provenance: deps.Provenance,
		evidence:   deps.Evidence,
	}}
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
	// The contract's evidence-assertion route (specs/api/openapi.yaml): the
	// ONE write path an evidence assertion has. There is deliberately no
	// PUT, PATCH or DELETE beside it — an assertion is never edited or
	// deleted (docs/24 §2: external evidence 不可被 origin maintainer 静默
	// 删除), and the database refuses a bare DELETE too (migration 00091).
	v1.HandleFunc("POST /api/v1/projects/{projectId}/branches/{branchId}/evidence-assertions", a.handlers.handleCreateEvidenceAssertion)
	v1.HandleFunc("GET /api/v1/projects/{projectId}/branches/{branchId}/objects/{objectId}", a.handlers.handleGetObject)
	v1.HandleFunc("GET /api/v1/projects/{projectId}/query", a.handlers.handleQuery)
	v1.HandleFunc("GET /api/v1/projects/{projectId}/research", a.handlers.handleResearch)
	v1.HandleFunc("GET /api/v1/projects/{projectId}/overview", a.handlers.handleOverview)
}
