package webhooks

import "errors"

// Sentinel errors the service maps to wire codes. Callers test with
// errors.Is — never by string comparison.
var (
	// ErrNotFound: no endpoint with that id owned by the actor (the store
	// answers identically for "does not exist" and "not yours" — the orgs
	// disclosure rule).
	ErrNotFound = errors.New("webhooks: endpoint not found")
	// ErrLimit: the actor already has events.MaxEndpointsPerUser
	// endpoints.
	ErrLimit = errors.New("webhooks: endpoint limit reached")
	// ErrValidation: malformed input (URL, filters).
	ErrValidation = errors.New("webhooks: invalid input")
	// ErrNotRedeliverable: the delivery is still pending (or unknown).
	ErrNotRedeliverable = errors.New("webhooks: delivery not redeliverable")
	// ErrStore: the store itself failed (dependency down, driver error).
	ErrStore = errors.New("webhooks: store failure")
)

// Codes shared with the rest of the API (VALIDATION_FAILED,
// SERVICE_UNAVAILABLE follow the orgs vocabulary; the webhook-specific
// ones are new).
const (
	// CodeEndpointNotFound answers a missing/foreign endpoint read.
	CodeEndpointNotFound = "WEBHOOK_NOT_FOUND"
	// CodeEndpointLimit answers a create past the per-user limit.
	CodeEndpointLimit = "WEBHOOK_LIMIT_REACHED"
	// CodeNotRedeliverable answers a redelivery of a pending delivery.
	CodeNotRedeliverable = "WEBHOOK_NOT_REDELIVERABLE"
	// CodeValidationFailed answers malformed input.
	CodeValidationFailed = "VALIDATION_FAILED"
	// CodeServiceUnavailable answers a failed store.
	CodeServiceUnavailable = "SERVICE_UNAVAILABLE"
)
