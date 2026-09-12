// Package orgs owns organization governance use cases (T0103):
// organization create/read/update/deactivate, membership invites, role
// adjustments and affiliation end (docs/52: application is the only layer
// that may mutate canonical state; docs/04 §6 for the affiliation model).
// Every mutation is an explicit governance decision — the actor's role is
// checked here, never trusted from the transport layer.
package orgs
