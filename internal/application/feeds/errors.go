package feeds

import "errors"

// Sentinel errors the service maps to wire codes. Callers test with
// errors.Is — never by string comparison.
var (
	// ErrNotFound: there is no such public feed. It is the answer for an
	// unknown target, for a target whose project is not public, and for an
	// asset or knowledge target with nothing published — deliberately one
	// answer for all of them (docs/45: an anonymous caller must not be able
	// to tell "private" from "does not exist").
	ErrNotFound = errors.New("feeds: no such public feed")
	// ErrValidation: malformed input — a target id whose shape could never
	// name a stored row, or an unknown kind/format.
	ErrValidation = errors.New("feeds: invalid input")
	// ErrStore: the reader failed (dependency down, driver error). The
	// surface is unavailable, which is NOT the same as "no feed".
	ErrStore = errors.New("feeds: store failure")
	// ErrConfig: the service was built without a usable public base URL, so
	// no entry link could be built. It is a wiring failure, never a request
	// failure (NewService reports it at startup).
	ErrConfig = errors.New("feeds: public base URL is not configured")
)
