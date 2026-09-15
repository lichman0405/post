// Package webhooks is the webhook endpoint registry use-case layer
// (T1006): creating/listing/updating/removing delivery endpoints, their
// once-shown signing secrets, and reading/redelivering the delivery log
// (docs/22 §9). All policy lives here — the transport layer only
// translates requests into these calls. The persistence port is
// implemented by events.WebhookStore (the webhook tables belong to the
// events domain, which owns the outbox and the delivery pipeline).
//
// Delivery itself is NOT here: the worker's events.FanOut and
// events.Deliverer own the fan-out, signing, retry and disable policy.
// This package only manages what the owner can see and control.
package webhooks
