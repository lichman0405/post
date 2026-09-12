// Package events owns domain events and the transactional outbox pattern
// (ADR-013, docs/18_EVENTS_SUBSCRIPTIONS.md): every event is written in the
// same transaction as its DB state; consumers are idempotent. T0002 scaffold.
package events
