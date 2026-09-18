// Package feedshttp is the transport of the public syndication feeds
// (T1004, docs/18 §4 "RSS/Atom（公开对象/项目）"):
//
//	GET /api/v1/feeds/projects/{projectId}
//	GET /api/v1/feeds/assets/{assetId}
//	GET /api/v1/feeds/knowledge/{objectId}
//
// Three routes, one per family the requirement names, all GET, all mounted
// on the shared /api/v1 mux and therefore inside the session/CSRF guard the
// whole subtree carries. Anonymous callers reach them for the same reason
// they reach /api/v1/assets/{assetId}: the guard only challenges
// state-changing methods, so a read flows unauthenticated and the handler
// decides what that caller may see. That is the first acceptance criterion
// of this task — 未登录可订阅 public feed — and it is a property of the
// plumbing, not of this file: nothing here reads a principal, and nothing
// here changes what it answers for a caller who happens to be signed in.
//
// # Why a namespace of its own
//
// The feeds are mounted under /api/v1/feeds/… rather than negotiated onto
// the entity routes themselves. An Accept-driven representation of
// /api/v1/assets/{assetId} would be elegant, and it would also mean that
// every existing JSON read of that path acquires a second serialization and
// a second disclosure rule to keep in step (the asset page's own rules
// answer for a member differently than for the network; a feed has no
// member's variant). A separate path prefix keeps both facts local: the
// contract's asset path answers exactly what it answered before this task,
// and a feed is a document whose audience is the open network by
// construction.
//
// The paths are this surface's own choice — the contract
// (specs/api/openapi.yaml) declares no feed path and docs/18 §4 names the
// output, not its URL — so they are recorded here and in the task result
// rather than presented as something a client already had. What a client
// needs is the URL it was handed; what it must be able to rely on is that
// the URL keeps answering, which is why nothing in the document's identity
// is derived from the request (see below).
//
// # Format negotiation
//
// ?format=atom|rss chooses explicitly, and an unknown value is refused 400
// (FEED_INVALID_REQUEST) rather than quietly served as Atom: a reader that
// asked for RSS should be told it cannot have it, not handed a document in
// a format it did not ask for. Otherwise the Accept header decides when it
// names exactly one of application/atom+xml and application/rss+xml — which
// is how a feed reader subscribes to a URL it did not build itself — and
// everything else (`*/*`, a browser's text/html list, no header at all)
// gets Atom, the default. Preference lists and quality values are not
// negotiated: the documented way to choose is ?format=.
//
// # What may be rendered is the model's decision, not this file's
//
// Every rule about whether a feed exists and what an entry may say lives in
// internal/application/feeds (BuildFeed, Render), with unit tests that name
// the criteria they come from — including the one this task is accepted on:
// the target's project must be public, so a PRIVATE PROJECT HAS NO FEED, for
// an anonymous caller and for its own members alike, and an asset or
// knowledge target with nothing public published has none either. This file
// parses a target, asks for a document and writes it; it filters nothing,
// because a handler that dropped an entry on its way out would be a second
// implementation of a disclosure rule in the layer that has no test for it.
//
// # One 404, three states
//
// An unknown target id, a target whose project is private, and a target with
// nothing published all answer the SAME 404 (FEED_NOT_FOUND) with the same
// sentence, because telling them apart would let an anonymous caller
// enumerate private projects and unpublished versions by diffing answers
// (docs/45: an authorization failure is indistinguishable from absence). A
// path segment that could never name a stored row — a slug where a pid
// belongs, a uuid that is not one — is answered the same way, for the same
// reason the asset page answers it that way: it names no feed, and a 400
// would be a second, distinguishable answer to "is there anything here?".
// A malformed QUERY parameter is the opposite case and is a 400: ?format= is
// the caller talking about its own request, not naming an entity.
//
// A failure to read is never any of those. It answers 503 and logs its cause
// (FEED_UNAVAILABLE), because "this project has published nothing" and "the
// database did not answer" are different statements and a feed must not make
// the first when it means the second. An error response is the platform's
// JSON envelope (docs/45), not XML: one error shape for the whole API, and a
// feed reader that does not understand it still sees the status code it acts
// on.
//
// # Caching, and why the document is stable
//
// A 200 carries an ETag derived from the rendered bytes and
// `Cache-Control: public, max-age=60`, and honours If-None-Match with a 304.
// That is honest only because the document is a function of the stored state
// and not of the request: the model reads no clock (an empty feed is stamped
// with its entity's creation time, not with "now"), every URL in it is built
// from the configured POST_WEB_ORIGIN rather than from the Host header, and
// the entry order is total. Two requests over one database state therefore
// render identical bytes, which is what makes the ETag a fact about the
// content rather than about the moment. Error responses are `no-store`: a
// transient failure must never be cached as an answer.
package feedshttp
