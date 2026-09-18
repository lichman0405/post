// Package feeds is the public RSS/Atom surface (T1004, docs/18 §4:
// "RSS/Atom（公开对象/项目）") for the three entities a subscriber can point
// a feed reader at: a public Project, a public Research Asset and a
// published Knowledge object.
//
// # What a feed is here
//
// A feed is a DERIVED, read-only view of the platform's published output —
// the immutable versions of an entity, newest first — rendered as Atom 1.0
// (RFC 4287) or RSS 2.0. It is not a table: nothing in this task adds a
// row, a cursor or a cache, so a feed cannot drift from the canonical
// store, cannot be stale, and disappears the moment the thing it describes
// stops being public (CLAUDE.md §8: PostgreSQL holds the semantic truth).
//
// The unit of a feed is a PUBLISHED VERSION, because that is the unit with
// a stable identity and a permanent address:
//
//	project feed    the project's published versions — its assets' public
//	                versions and its objects' knowledge publications
//	asset feed      one asset's public versions
//	knowledge feed  one knowledge object's publications
//
// Each entry carries an id that is a function of an immutable row's uuid
// (urn:post:asset-version:<uuid>, urn:post:knowledge-publication:<uuid>),
// stable forever: a version row is append-only (migration 00014) and a uuid
// is never reused, so a reader that stored a feed entry last year still holds
// the same identity today. That id is UNCONDITIONAL — every entry has one, in
// both formats (Atom's <id>, RSS's <guid isPermaLink="false">).
//
// A link is emitted only where a page really exists. An asset version has a
// permanent address by construction (/assets/{pid}/{version}, the T0701
// scheme of internal/assets/url.go) and carries it; a knowledge publication
// has no route yet — T0805 owns the published knowledge object's surface —
// and therefore carries no <link> at all, rather than an href pointing at a
// 404 ("a link to a route that does not exist is a lie in the shape of an
// href", internal/application/explore/index.go). The requirement's "stable
// ids/version links" is met by the ids, which never move; the links are never
// invented to fill the sentence out.
//
// # Why not the research event log
//
// The project feed is often reached for as "the project's activity", and the
// platform does keep that history (research_events). It is deliberately NOT
// the entry source here. A research event's payload names the rows it is
// about — an asset id and a version label, a release id — and those names
// can be PRIVATE: internal/assets.withholds an event whose version label
// resolves to no version the reader may see, precisely because printing the
// label would disclose the private version's existence. Deriving feed links
// from payload bytes would have to re-implement that judgement in a second
// place; deriving them from the version rows themselves cannot get it wrong,
// because a feed entry IS a public version row.
//
// # The one disclosure rule
//
// A feed exists only for a target an ANONYMOUS reader may see, and the rule
// is the one the platform already uses for these three target types
// (events.SubscriptionStore.TargetAudience, T1002): a project is public when
// its visibility is public; an asset and a knowledge object are public when
// the project that owns them is. A target that is not public answers the
// same 404 an unknown id gets — "this is private" and "this does not exist"
// must be indistinguishable from outside (docs/45), or the feed route
// becomes an existence oracle for private work.
//
// Two consequences are deliberate and worth stating:
//
//   - A private project has NO feed at all — not for its assets, not for its
//     knowledge objects, not for members either. A feed is an unauthenticated
//     URL that a reader subscribes to; a URL whose answer depends on who
//     fetches it is not a feed, and docs/12 §3 (visibility widening is never
//     automatic) is the rule that keeps the private compartment private.
//   - A target with nothing published has no feed either (an asset whose
//     every version is private, a knowledge object that was never published):
//     an empty feed would say "this exists and is public but has no output
//     yet", which is a statement about a private thing. A PUBLIC PROJECT with
//     no publications is the one exception — its feed exists and is empty,
//     because the project itself is public and its emptiness says nothing
//     about anything else.
//
// # Layering
//
// The split is internal/assets': a reader (internal/persistence.
// FeedStore) answers questions about ROWS — it returns the target's state
// with the newest rows a public feed may render — and BuildFeed owns every
// rule about what may be rendered, as a pure function with unit tests that
// name the criteria they come from. The reader's visibility filter is a read
// strategy (the window is bounded, so it must be counted in rows the feed can
// carry); the rule is BuildFeed's, and it applies to every entry whatever the
// reader returned. The transport (cmd/api/feedshttp) translates: it parses
// the target and the requested format, and serializes the model's output and
// nothing else.
package feeds
