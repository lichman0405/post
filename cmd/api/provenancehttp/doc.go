// Package provenancehttp is the transport of the provenance graph read
// surface (T0505, docs/10): GET-only routes that answer the two questions
// the projection exists for —
//
//   - the whole project's provenance graph as JSON
//     (GET /api/v1/projects/{projectId}/provenance/graph);
//   - one object's lineage ("where did this Dataset come from") or impact
//     ("what depends on this"), walking upstream or downstream
//     (GET /api/v1/projects/{projectId}/objects/{objectId}/lineage).
//
// The walks run in internal/rsg/provenance over the rebuildable
// provenance_edges projection (infra/migrations/00043). The pgx adapter
// that reads the projection lives here, next to the transport: T0505's
// allowed_scope does not include internal/persistence or
// internal/application, so this surface carries its own store (L1, noted
// in the task result) — the composition in cmd/api/main.go keeps the rest
// of the layering untouched.
//
// Every read runs the same project visibility gate as every other project
// read (projects.Service.Get, the T0106 matrix): a denied reader gets the
// existence-hiding 404, identical whether the project or the object exists.
package provenancehttp
