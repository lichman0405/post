// Package inbox is the research inbox use-case layer (T1003, docs/18
// §3-4): the web channel's read surface over the delivery rows the
// subscription fan-out writes.
//
// Three things belong to this layer and not to the store or the handler:
//
//   - AGGREGATION is presented, not invented. The store returns entries —
//     one per (target, event type, window) — and this layer hands them out
//     with the one thing that is a web-app fact rather than a database
//     fact: the link to the subject's page (EntryURL).
//   - VALIDATION happens before the store (docs/52): the view name, the
//     page size and the shape of every mark-read anchor.
//   - OWNER SCOPING is the store's, and this layer never widens it: an
//     actor only ever reads and marks their own rows, and an anchor that
//     is not theirs answers ErrNotFound rather than disclosing that it
//     exists.
//
// What is deliberately NOT here is a notification policy of its own. The
// rule "低级每 commit 不默认轰炸用户" (docs/18 §3) is answered where the
// spam would otherwise be created — by aggregation in the read model — and
// not by this layer second-guessing which deliveries the fan-out wrote:
// a delivery exists because a subscription asked for that event type, and
// an inbox layer that hid deliveries its own subscription model promised
// would be a second, invisible filter over the first. Aggregating keeps
// every delivery visible AND keeps the count honest.
package inbox
