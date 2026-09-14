// Package rsghttp is the HTTP surface of the RSG domain services (T0208):
// research branches, scientific object create/version and typed relation
// create under /api/v1/projects/{projectId}/branches, plus the object
// read. The contract is OpenAPI-first (specs/api/openapi.yaml); the
// handlers translate the request into the internal/application/rsg
// service commands and the service's outcomes into the wire (docs/45:
// stable codes, never dependency detail — a denied write answers the same
// envelope whether the object exists or not, and a missing object, branch
// or project share the same 404 shapes the other surfaces use).
package rsghttp
