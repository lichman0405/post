// Package policyhttp is the policy HTTP surface (T0603): read/write
// routes for organization and project policy versions, mounted as
// full-path patterns on the shared /api/v1 mux (more specific than the
// organizations/projects subtrees, so they coexist — same technique as
// audithttp). Every handler: resolve the principal (the guard put it
// there — reads require a session too, policy visibility is member-only),
// parse the request, call the application service, render the payload or
// the standard error envelope. No policy decision lives here.
package policyhttp
