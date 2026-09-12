// Package orgshttp is the organization HTTP surface (T0103):
// /api/v1/organizations CRUD and membership governance. Every handler is a
// thin translation: parse the request, resolve the principal the auth
// guard put on the context, call the orgs Service, render the payload or
// the standard error envelope — transport never contains policy
// (docs/52). The contract is OpenAPI-first; the canonical spec addition
// is handed to the Supervisor in the RESULT (specs/api/** is outside
// Worker scope).
package orgshttp
