// Package reviewhttp serves the PR review surface
// (specs/api/openapi.yaml: /projects/{projectId}/pull-requests/{prId}/reviews,
// task T0404) — the submission POST and, since T0408, the list GET the PR
// page's review section reads (both dimensions of the current head, and
// the earlier rounds' decisions with the state each judged).
//
// The submission handler is thin: resolve the guarded principal, decode
// the body, call the reviews service — which owns validation and
// authorization (the submit_scientific_review matrix row with the
// reviewer-responsibility hook). A denied caller gets the same 403
// envelope whether or not the PR exists (docs/45).
//
// The list handler is a project read: it runs the project visibility gate
// BEFORE the service exactly as the pull-request and checks reads do (a
// denied read is the existence-hiding 404, and the service is never
// reached). Without the gate wired the route fails closed at 503.
package reviewhttp
