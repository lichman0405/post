// Package unknownform holds two registration-shaped calls the enumerator must
// not swallow:
//
//   - a mux with a method it has never seen (reported as a registration it
//     cannot read — loud, not skipped);
//   - a bare path on a receiver it cannot call a mux (reported in the advisory
//     list, because it is indistinguishable from any other call taking a path).
//
// The file is parsed and never compiled: the whole point is a form that does
// not exist yet, so the method below is deliberately not one today's
// http.ServeMux has.
package unknownform

import "net/http"

// Router is the shape a future mux wrapper would have.
type Router struct{}

// Stream registers an upgrade route.
func (Router) Stream(string, http.HandlerFunc) {}

// Register mounts a route through a mux method the enumerator does not know.
func Register(mux *http.ServeMux) {
	mux.Stream("/api/v1/stream", func(http.ResponseWriter, *http.Request) {})
}

// RegisterOnUnknownRouter has a route-shaped literal on a receiver the scanner
// cannot attribute to a mux.
func RegisterOnUnknownRouter(r Router) {
	r.Stream("/api/v1/unknown-router/stream", nil)
}
