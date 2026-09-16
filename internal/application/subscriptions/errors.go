package subscriptions

import "errors"

// Sentinel errors the service maps to wire codes. Callers test with
// errors.Is — never by string comparison.
var (
	// ErrNotFound: no live subscription with that id owned by the actor,
	// AND — for a target that does not exist or may not be seen — the
	// existence-hiding answer the orgs surfaces give ("exists but not
	// yours" and "does not exist" answer identically, so the subscribe
	// endpoint cannot be used to probe for private targets).
	ErrNotFound = errors.New("subscriptions: not found")
	// ErrExists: the actor already has a live subscription to that target.
	ErrExists = errors.New("subscriptions: already subscribed")
	// ErrLimit: the actor holds events.MaxSubscriptionsPerUser live
	// subscriptions.
	ErrLimit = errors.New("subscriptions: limit reached")
	// ErrValidation: malformed input (target, filters, channels).
	ErrValidation = errors.New("subscriptions: invalid input")
	// ErrStore: the store itself failed (dependency down, driver error).
	ErrStore = errors.New("subscriptions: store failure")
)

// Codes shared with the rest of the API (VALIDATION_FAILED and
// SERVICE_UNAVAILABLE follow the orgs/webhook vocabulary; the
// subscription-specific ones are new with T1002).
const (
	// CodeNotFound answers a missing/foreign subscription, and a subscribe
	// to a target the actor may not see.
	CodeNotFound = "SUBSCRIPTION_NOT_FOUND"
	// CodeExists answers a second live subscription to the same target.
	CodeExists = "SUBSCRIPTION_EXISTS"
	// CodeLimit answers a create past the per-user limit.
	CodeLimit = "SUBSCRIPTION_LIMIT_REACHED"
	// CodeValidationFailed answers malformed input.
	CodeValidationFailed = "VALIDATION_FAILED"
	// CodeServiceUnavailable answers a failed store.
	CodeServiceUnavailable = "SERVICE_UNAVAILABLE"
)
