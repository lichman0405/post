package feeds

import "context"

// Reader is the persistence port: what the feed surface needs from storage.
// The production implementation is *persistence.FeedStore.
//
// LoadFeedState answers ONE question — "what rows does this target have?" —
// and the entries it answers with are the ones a public feed may RENDER: the
// bounded window is counted in renderable rows, so a private version can
// never spend the window that a public one needs (see
// internal/persistence/queries/feeds.sql). The decision about what may be
// rendered nevertheless belongs to BuildFeed, which applies the disclosure
// rule to every entry it is handed — the reader's filter is a read strategy,
// and a query change must not be able to publish a private row on its own.
//
// An id that names nothing is Found=false and a nil error — a state, not a
// failure. A read that fails is an error, and the transport answers it 503
// rather than serving an empty feed: "nothing has been published" and "the
// database is down" are different statements about a project, and a feed
// must not report the first when it means the second.
type Reader interface {
	// LoadFeedState reads one target's state. limit bounds how many entries
	// per entry kind the reader has to consider — see Service.Feed.
	LoadFeedState(ctx context.Context, target Target, limit int) (State, error)
}
