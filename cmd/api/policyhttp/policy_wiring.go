package policyhttp

import (
	"net/http"

	"github.com/lichman0405/post/internal/application/policy"
)

// The policy API wiring: the store adapter and the two gates go in,
// guarded routes come out (the guard itself is owned by authhttp and
// composed in cmd/api/main.go). cmd/api builds the production graph (pgx
// PolicyStore + pgx OrgStore/ProjectStore as the gates); the integration
// suite builds the same graph over the same real PostgreSQL.

// Deps carries the adapters the policy API needs.
type Deps struct {
	Store policy.PolicyStore
	// Orgs is the organization gate: existence/activation checks and the
	// actor's membership role. The production adapter is
	// persistence.OrgStore.
	Orgs policy.OrgGate
	// Projects is the project gate: the project's owning organization
	// (the lower-bound source) and the actor's membership role. The
	// production adapter is persistence.ProjectStore.
	Projects policy.ProjectGate
}

// New wires the service.
func New(deps Deps) *API {
	return &API{
		handlers: &handlers{svc: policy.NewService(deps.Store, deps.Orgs, deps.Projects, nil)},
	}
}

// API is the mounted policy surface.
type API struct {
	handlers *handlers
}

// Service exposes the wired service so sibling surfaces (and later tasks
// like T0604/T0605) share the exact same instance — one evaluator, one
// store, no drift.
func (a *API) Service() *policy.Service { return a.handlers.svc }

// Register mounts the policy routes directly on the v1 mux. The full-path
// patterns are more specific than the /api/v1/organizations/ and
// /api/v1/projects/ subtrees, so they coexist with them (same technique
// as audithttp). Every route is session-guarded by construction — the
// whole v1 mux sits inside authhttp.API.Guard.
func (a *API) Register(v1 *http.ServeMux) {
	h := a.handlers
	v1.HandleFunc("GET /api/v1/organizations/{organizationId}/policy", h.handleGetOrgPolicy)
	v1.HandleFunc("GET /api/v1/organizations/{organizationId}/policy/versions", h.handleListOrgPolicyVersions)
	v1.HandleFunc("PUT /api/v1/organizations/{organizationId}/policy", h.handleSetOrgPolicy)
	v1.HandleFunc("GET /api/v1/projects/{projectId}/policy", h.handleGetProjectPolicy)
	v1.HandleFunc("GET /api/v1/projects/{projectId}/policy/effective", h.handleGetEffectivePolicy)
	v1.HandleFunc("GET /api/v1/projects/{projectId}/policy/versions", h.handleListProjectPolicyVersions)
	v1.HandleFunc("PUT /api/v1/projects/{projectId}/policy", h.handleSetProjectPolicy)
}
