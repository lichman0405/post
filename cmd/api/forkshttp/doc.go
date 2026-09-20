// Package forkshttp serves the external-contribution entry point:
//
//	POST /api/v1/projects/{projectId}/forks
//
// specs/api/openapi.yaml declares it as "Fork a research line into the
// caller's own space; the external contribution entry". The route is a
// transport over internal/application/forks.Service.Fork and nothing more:
// the three matrix cells this path resolves (create_branch =
// external_fork_only for a non-member, write_scientific_state =
// own_fork_only, open_pr = allow_from_fork) are the service's business, and
// a handler that re-derived any of them would be a second, drifting copy of
// specs/policies/permissions-matrix.csv.
//
// What the transport DOES own is the contract: the status each of the
// service's outcomes is answered with, the wire shape of the body it
// accepts and of the fork it returns, and the one condition the contract
// states at this layer rather than in the service — a fork of a parent that
// is not PUBLIC may not be created public (see handleFork).
package forkshttp
