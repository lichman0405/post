// Package validationhttp is the HTTP surface of the progressive
// validation gates (T0207): POST
// /api/v1/projects/{projectId}/branches/{branchId}:validate runs one gate
// over the branch's persisted snapshot and returns the full report
// (specs/api/openapi.yaml, docs/22 §7 — the endpoint returns the complete
// result and modifies no state).
//
// The gate enum on the wire is pr|main|release|asset: the draft gate is the
// everyday commit baseline and is not exposed as an endpoint gate (drafts
// are validated server-side by the commit guard instead). The handler is a
// thin transport: parse, call the application service, render the report or
// the standard error envelope.
package validationhttp
