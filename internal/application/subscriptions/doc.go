// Package subscriptions is the follow/watch use-case layer (T1002): a user
// follows a target (Project / Research Asset / Published Knowledge Object /
// Person / Organization — docs/18 §3), choosing which event types reach
// them and through which channels (docs/18 §4), and can change or end that
// at any time.
//
// All policy lives here (docs/52: the application layer is the only layer
// that may decide): the shape rules of a target, filters and channels; the
// rule that you may only follow what you may currently SEE (a subscription
// to something you cannot read would be both an existence oracle and a
// future leak); and owner scoping on every operation.
//
// What is NOT here is the delivery decision. The fan-out
// (events.SubscriptionFanOut, run by cmd/worker) re-resolves the
// subscriber's relationship to the target against live state for every
// event, so a permission change takes effect at the next event rather than
// at the next subscribe call. That re-resolution and this package's
// subscribe-time check are the same rule — events.SubscriptionStore.
// TargetAudience — deliberately, because two copies of it are how a
// revoked membership keeps receiving private events.
package subscriptions
