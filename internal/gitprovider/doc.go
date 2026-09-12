// Package gitprovider defines the port over the internal Gitea transport
// (ADR-003, docs/16_GIT_COMPATIBILITY.md). Product domain objects never use
// Gitea identity as primary identity (docs/52); this package keeps the
// product model decoupled from the Git data model. T0002 scaffold.
package gitprovider
