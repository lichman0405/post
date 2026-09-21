package feedshttp

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/feeds"
	"github.com/lichman0405/post/internal/observability"
)

// Wire codes (docs/45: stable codes, no dependency detail).
const (
	// CodeFeedNotFound: there is no such public feed. One code for every
	// state that means it — an unknown target, a target whose project is not
	// public, a target with nothing published, and a path segment that could
	// never name a stored row — because they must be indistinguishable from
	// outside (doc.go).
	CodeFeedNotFound = "FEED_NOT_FOUND"
	// CodeFeedInvalidRequest: the request is not answerable as given (today:
	// ?format= naming neither atom nor rss). A malformed QUERY parameter, not
	// a malformed target: see writeUnavailable's neighbours in doc.go for why
	// the two are answered differently.
	CodeFeedInvalidRequest = "FEED_INVALID_REQUEST"
	// CodeFeedUnavailable: no honest document can be produced — the state
	// read failed, or the surface was never wired because the deployment's
	// public origin is not usable. Never a 404: a feed that could not be
	// built is not a feed that does not exist.
	CodeFeedUnavailable = "FEED_UNAVAILABLE"
	// CodeFeedRenderFailed: the document could not be serialized. It is a bug
	// in the platform rather than a state of the data, and it is its own code
	// so that it is never confused with a read that failed — the two need
	// different responses from whoever reads the logs.
	CodeFeedRenderFailed = "FEED_RENDER_FAILED"
)

// cacheControl is the freshness a rendered feed is served with. A public
// feed is the same document for every caller, so it may be cached by shared
// caches — but it changes whenever something is published, so the window is
// short: a subscriber polling every minute sees a new version within one
// poll either way, and a burst of readers of one feed costs one render.
const cacheControl = "public, max-age=60"

// handlers owns the feed routes.
type handlers struct {
	svc Service
}

// handleProjectFeed serves GET /api/v1/feeds/projects/{projectId}.
func (h *handlers) handleProjectFeed(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r, feeds.KindProject, r.PathValue("projectId"))
}

// handleAssetFeed serves GET /api/v1/feeds/assets/{assetId}.
func (h *handlers) handleAssetFeed(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r, feeds.KindAsset, r.PathValue("assetId"))
}

// handleKnowledgeFeed serves GET /api/v1/feeds/knowledge/{objectId}.
func (h *handlers) handleKnowledgeFeed(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r, feeds.KindKnowledge, r.PathValue("objectId"))
}

// serve is the one path all three routes take: negotiate the format, parse
// the target, ask for the document, write it.
//
// The order is the order of what a caller is told: a caller that asked for an
// impossible format is told that (400) even if the target it named does not
// exist, because the format is a statement about its own request and the
// target's existence is what this surface must not disclose. Nothing is read
// before the format is settled: a request this surface cannot answer costs no
// query.
func (h *handlers) serve(w http.ResponseWriter, r *http.Request, kind feeds.Kind, id string) {
	if h.svc == nil {
		writeUnavailable(w, r, "the service is not wired")
		return
	}
	format, ok := requestedFormat(r)
	if !ok {
		writeError(w, r, http.StatusBadRequest, CodeFeedInvalidRequest,
			`format must be "atom" or "rss"`)
		return
	}
	target, err := feeds.ParseTarget(string(kind), id)
	if err != nil {
		// The segment cannot name a stored row, so there is no feed here —
		// answered exactly as an existing target that may not be subscribed
		// to (doc.go). ErrValidation is the only error ParseTarget returns.
		writeNotFound(w, r)
		return
	}
	doc, err := h.svc.Document(r.Context(), target, format)
	if err != nil {
		writeFeedError(w, r, err, target)
		return
	}
	writeDocument(w, r, doc, format)
}

// requestedFormat resolves the representation. ?format= wins when it names a
// format; an absent, empty or blank value is not a choice and falls through
// to Accept, and then to Atom (feeds.ParseAccept's contract). The blank check
// is the model's own rule (feeds.ParseFormat trims before it matches), kept
// identical here so that `?format=%20` and `?format=` cannot be answered two
// different ways.
func requestedFormat(r *http.Request) (feeds.Format, bool) {
	if want := strings.TrimSpace(r.URL.Query().Get("format")); want != "" {
		return feeds.ParseFormat(want)
	}
	if f, ok := feeds.ParseAccept(r.Header.Get("Accept")); ok {
		return f, true
	}
	return feeds.FormatAtom, true
}

// writeDocument writes one rendered feed with its cache metadata.
//
// The ETag is a hash of the BYTES, not of the state behind them: the
// transport cannot see the state, and hashing what it is about to write is
// the only claim it can actually check. It is a strong validator because
// feeds.Render is deterministic — the same state renders the same bytes —
// and the model keeps the request's clock and host out of the document
// (doc.go).
//
// A 304 carries the validator and the freshness window and nothing else: it
// is an answer about the representation the client already holds, so it
// describes no representation of its own.
func writeDocument(w http.ResponseWriter, r *http.Request, doc []byte, format feeds.Format) {
	etag := etagOf(doc)
	header := w.Header()
	header.Set("ETag", etag)
	header.Set("Cache-Control", cacheControl)
	// The representation depends on the Accept header, so a shared cache must
	// key on it. ?format= needs no Vary entry: a query string is part of the
	// cache key already.
	header.Set("Vary", "Accept")
	if matchesETag(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	header.Set("Content-Type", format.ContentType())
	// An XML feed is a document, not an opaque payload: the browser parses
	// it, so the declared media type has to be the one it parses it as.
	// nosniff is stated here as well as at the edge (internal/security) so
	// the fact belongs to this exit — the guard in tests/security judges
	// the handler on its own, without the edge in the chain.
	header.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(doc)
}

// writeFeedError maps the use case's errors onto this surface's answers.
//
// Not-found is this surface's 404; a store failure is 503 and is logged with
// its cause, because the caller can do nothing with the cause (docs/45: raw
// dependency errors never reach the envelope) while the operator needs it.
// Anything else is the renderer failing, which is a 500: it is not a
// statement about the target and must not be answered as one.
func writeFeedError(w http.ResponseWriter, r *http.Request, err error, target feeds.Target) {
	switch {
	case errors.Is(err, feeds.ErrNotFound):
		writeNotFound(w, r)
	case errors.Is(err, feeds.ErrStore):
		observability.LoggerFromContext(r.Context()).Error("feeds: state read failed",
			"error", err, "kind", string(target.Kind), "target", target.ID)
		writeUnavailable(w, r, "the read failed")
	default:
		observability.LoggerFromContext(r.Context()).Error("feeds: render failed",
			"error", err, "kind", string(target.Kind), "target", target.ID)
		writeError(w, r, http.StatusInternalServerError, CodeFeedRenderFailed,
			"the feed could not be rendered")
	}
}

// writeNotFound is the one 404 this surface has. It names nothing — not the
// target, not the kind, not the reason — because the states that produce it
// must be indistinguishable (doc.go).
func writeNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusNotFound, CodeFeedNotFound, "feed not found")
}

// writeUnavailable answers the states in which this surface cannot produce a
// document at all, and logs why (the caller is told nothing about the cause).
func writeUnavailable(w http.ResponseWriter, r *http.Request, cause string) {
	observability.LoggerFromContext(r.Context()).Error("feeds: surface unavailable", "cause", cause)
	writeError(w, r, http.StatusServiceUnavailable, CodeFeedUnavailable,
		"feeds are temporarily unavailable")
}

// writeError writes the platform's error envelope and marks it uncacheable: a
// failure — transient or not — must never be cached as an answer about the
// feed.
func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	w.Header().Set("Cache-Control", "no-store")
	authhttp.WriteError(w, r, status, code, message)
}

// etagOf is the strong validator of one rendered document.
func etagOf(doc []byte) string {
	sum := sha256.Sum256(doc)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// matchesETag reports whether an If-None-Match header names the validator.
//
// The comparison is the weak one RFC 9110 prescribes for If-None-Match (a
// W/ prefix on either side is ignored): a client that declares its copy to be
// semantically equivalent, which is what a reader caching a feed means, is
// exactly the client a 304 is for. `*` matches any existing representation,
// which is what it means here — the document exists, since it was just built.
func matchesETag(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" {
			return true
		}
		if strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}
