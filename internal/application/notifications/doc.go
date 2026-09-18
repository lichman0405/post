// Package notifications is the email notification layer (T1005, docs/18 §4
// "Email immediate/digest abstraction"; docs/20 §9 lists the notification
// digest among the worker's jobs). It has two halves that share one
// vocabulary:
//
//   - the CADENCE SETTING the account owns — get it, change it
//     (Service). The row is the one 00081 added; an account that never
//     touched the setting has no row and gets events.DefaultCadence, and
//     absence is not "no email".
//
//   - the DIGEST SENDER — the background process that turns the pending
//     email rows the subscription fan-out writes (00078) into messages
//     (Sender), through a transport port (Mailer; the development
//     implementation is DevSink) and a concise template (Render).
//
// What lives here and why: everything above is POLICY or presentation —
// when a digest is due (events.DigestDue), what an email may say, and when
// a delivery must be withdrawn. What is deliberately NOT here is the
// authorization rule. The sender re-resolves the subscriber's relationship
// to the event's target at SEND time through the same implementation the
// subscription fan-out uses (events.SubscriptionStore.TargetAudience) and
// applies the same one predicate (events.Delivers), so a queued email for a
// target its recipient may no longer see is withdrawn rather than sent —
// the fan-out cannot close that window on its own, because it only runs
// when a NEW event for that target arrives.
//
// The package depends on internal/events (the subscription model, the
// delivery rows, the audience rule) and never the other way round: the
// events domain owns the rows, this layer decides what to do with them.
package notifications
