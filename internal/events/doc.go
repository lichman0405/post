// Package events owns domain events and the transactional outbox pattern
// (ADR-013, docs/18_EVENTS_SUBSCRIPTIONS.md): every event is written to
// outbox_events in the SAME database transaction as its state change
// (Record, called from the application layer's transaction callbacks), and
// the background worker publishes each pending outbox row into
// research_events (Dispatcher, run by cmd/worker).
//
// Delivery semantics: at-least-once with an idempotent publish. The
// research_events row carries the outbox row's id (outbox_event_id), and
// the partial unique index research_events_outbox_event_uniq turns a
// retried publish into a no-op — so a crash between "event inserted" and
// "outbox row marked published" can never lose an event nor duplicate its
// side effects (T1001 acceptance: crash/retry 不丢事件不重复副作用).
// Consumers are idempotent on their side; they see exactly one
// research_event per outbox row.
package events
