// Package reviewhttp serves the PR review submission surface
// (specs/api/openapi.yaml: POST
// /projects/{projectId}/pull-requests/{prId}/reviews, task T0404).
//
// The handler is thin: resolve the guarded principal, decode the body,
// call the reviews service — which owns validation and authorization
// (the submit_scientific_review matrix row with the reviewer-
// responsibility hook). A denied caller gets the same 403 envelope
// whether or not the PR exists (docs/45).
package reviewhttp
