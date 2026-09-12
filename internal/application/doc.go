// Package application orchestrates use cases across domain objects and
// ports: transaction boundaries, authorization checks and state transitions
// (docs/52: application is the only layer that may mutate canonical state).
// T0002 scaffold — use cases arrive with the API/worker tasks.
package application
