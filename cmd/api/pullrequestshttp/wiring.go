package pullrequestshttp

import (
	"net/http"
)

// Deps carries the adapters the pull-request API needs.
type Deps struct {
	// PullRequests serves the list and detail endpoints (the pullrequests
	// application service).
	PullRequests PullRequests
	// Create serves the open-pull-request endpoint (the forks application
	// service, whose OpenExternalPR resolves the open_pr matrix cell and
	// proposes through the same pull-request path as an internal PR).
	// REQUIRED: without it the create route fails closed (503) rather than
	// opening proposals with no authorization at all.
	Create PRCreator
	// Review serves the request-review endpoint (the same forks application
	// service, whose RequestReview resolves the open_pr cell for the move
	// docs/43 names and drives the pull-request command). REQUIRED for that
	// route: without it the route fails closed (503) rather than moving any
	// state with no authorization at all.
	Review RequestReviewer
	// Checks serves the checks endpoint (the prchecks application
	// service).
	Checks CheckRunner
	// Diff serves the diff endpoint (the prdiff application service).
	Diff DiffRunner
	// Projects must be wired: PRs and their reports are exactly as
	// visible as their project (the same read gate every other project
	// read runs).
	Projects ProjectReader
}

// New wires the handler.
func New(deps Deps) *API {
	return &API{handlers: &handlers{
		prs:      deps.PullRequests,
		create:   deps.Create,
		review:   deps.Review,
		checks:   deps.Checks,
		diff:     deps.Diff,
		projects: deps.Projects,
	}}
}

// API is the mounted pull-request surface.
type API struct {
	handlers *handlers
}

// Register mounts the collection's read endpoint, the open-pull-request
// write, and the per-PR reads. The sub-resource routes register before the
// {number} detail route: ServeMux matches the most specific pattern, but
// the explicit order documents the intent — "/checks" and "/diff" are never
// consumed as a {number} (which they could not parse anyway).
//
// The POST is the collection's second verb and the only write on this
// surface (specs/api/openapi.yaml, "Open pull request with RSG diff"). It
// coexists with the merge surface's remainder route
// (cmd/api/mergehttp: POST /api/v1/projects/{projectId}/pull-requests/
// {number...}) because that pattern requires at least one more segment,
// while this one stops at the collection — ServeMux picks the more specific
// pattern for a longer path and this one for the bare collection.
//
// The collection has a THIRD write — POST .../{prId}:request-review (T0411) —
// and it is deliberately not registered here: its verb lives inside the last
// path segment, which ServeMux can only capture as a remainder wildcard, and
// this prefix's one remainder owner is the merge surface. This package exports
// the handler for it (RequestReviewHandler) and mergehttp dispatches the verb
// to that handler; see requestreview.go for the whole of that reasoning.
func (a *API) Register(v1 *http.ServeMux) {
	v1.HandleFunc("GET /api/v1/projects/{projectId}/pull-requests", a.handlers.handleList)
	v1.HandleFunc("POST /api/v1/projects/{projectId}/pull-requests", a.handlers.handleCreate)
	v1.HandleFunc("GET /api/v1/projects/{projectId}/pull-requests/{number}/checks", a.handlers.handleChecks)
	v1.HandleFunc("GET /api/v1/projects/{projectId}/pull-requests/{number}/diff", a.handlers.handleDiff)
	v1.HandleFunc("GET /api/v1/projects/{projectId}/pull-requests/{number}", a.handlers.handleGet)
}
