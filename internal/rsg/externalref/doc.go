// Package externalref owns the External Reference refresh domain
// (docs/19): the manual refresh of a live identity's pinned snapshot.
//
// The pieces:
//
//   - fetch.go: the upstream metadata port (Fetcher) — the outbound
//     adapter contract. "Adapter" is hexagonal: the adapter adapts an
//     upstream metadata source to the platform's UpstreamMetadata shape.
//   - doi.go: the V1 adapter — DOI content negotiation
//     (https://doi.org/{doi}, CSL-JSON) resolves ANY DOI through the
//     handle system to its registering agency, so no agency-specific
//     API key or registry split is needed (docs/19 §5: V1 is manual
//     refresh + DOI metadata adapter; automatic monitoring is a later
//     iteration).
//   - ssrf.go: the fetch URL guard (docs/23 §7, docs/54 threat #5) —
//     https-only, and no dial into private/loopback/link-local/reserved
//     space, re-checked on every redirect hop. Fetched content is
//     untrusted data, never instructions (docs/23 §8).
//   - snapshot.go: the pinned snapshot — accessed_at (server time),
//     upstream version, metadata document, hash. A snapshot, once
//     written, is never rewritten: the database rejects UPDATE/DELETE of
//     snapshot rows (migrations 00014/00015) and the refresher below
//     only ever PRODUCES snapshots.
//   - refresh.go: one manual refresh — fetch upstream, build the
//     snapshot, compare against the current head. A refresh never
//     mutates an existing snapshot: it either produces a new one
//     (upstream changed) or reports the head as still current.
//
// This package never reads or writes storage: the refresher produces
// snapshots and the caller (the consuming API task) persists them through
// its store — the port boundary lives there, exactly as T0208 assigned
// transport concerns to the consuming API task.
package externalref
