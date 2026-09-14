package projectshttp

import (
	"context"
	"net/http"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/worker"
)

// The project API wiring: the store adapter and the organization gate go
// in, guarded routes come out (the guard itself is owned by authhttp and
// composed in cmd/api/main.go). cmd/api builds the production graph (pgx
// ProjectStore + pgx OrgStore as the gate); the integration suite builds
// the same graph over the same real PostgreSQL, so the tests exercise the
// exact production handlers and middleware.

// Deps carries the adapters the project API needs.
type Deps struct {
	Store projects.ProjectStore
	// Orgs is the organization gate: create-inside-organization checks
	// (organization exists, active, actor is an active member). The
	// production adapter is persistence.OrgStore.
	Orgs projects.OrgGate
	// Authz is the policy engine the service enforces through (T0105,
	// docs/50: every public action passes an explicit authorization
	// check — hiding a control in a client never substitutes for it).
	// Production composes authz.NewMatrixEngine().
	Authz authz.Engine
	// ProvisionJobs is the T0301 provisioning-job sink: a successful
	// create enqueues one project-provision job for the new project (the
	// consuming loop lives in cmd/api/main.go). Optional: nil (unit
	// tests) disables the enqueue — the project row stays the source of
	// truth (provision_status='pending') and the API's startup sweep
	// back-fills anything a missing queue skipped.
	ProvisionJobs JobSink
}

// JobSink is the slice of the job queue the create path needs.
type JobSink interface {
	Enqueue(ctx context.Context, job worker.Job) error
}

// New wires the service.
func New(deps Deps) *API {
	return &API{
		handlers: &handlers{svc: projects.NewService(deps.Store, deps.Orgs, deps.Authz), jobs: deps.ProvisionJobs},
	}
}

// API is the mounted /api/v1/projects subtree.
type API struct {
	handlers *handlers
}

// Service exposes the wired service so sibling surfaces can share the exact
// same instance: the Activity feed (audithttp) authorizes project reads
// with the same object that serves the project itself, so visibility rules
// can never drift between a resource and its activity.
func (a *API) Service() *projects.Service { return a.handlers.svc }

// Routes registers the project surface (no guard — main.go wraps the whole
// /api/v1 subtree in authhttp.API.Guard, so every write here is session +
// CSRF protected by construction).
func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	h := a.handlers
	mux.HandleFunc("POST /api/v1/projects", h.handleCreate)
	mux.HandleFunc("GET /api/v1/projects", h.handleList)
	mux.HandleFunc("GET /api/v1/projects/{projectId}", h.handleGet)
	mux.HandleFunc("PATCH /api/v1/projects/{projectId}", h.handleUpdateSettings)
	mux.HandleFunc("GET /api/v1/projects/{projectId}/membership", h.handleMembership)
	mux.HandleFunc("GET /api/v1/projects/{projectId}/members", h.handleListMembers)
	mux.HandleFunc("PUT /api/v1/projects/{projectId}/members/{userId}", h.handleSetMemberRole)
	return mux
}
