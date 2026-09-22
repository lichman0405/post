// Package chi holds the registration form this tree does not use today: the
// method shorthand. It is here because "we only ever write it the other way"
// is how an enumerator goes silently blind — a new form must be either
// understood or reported, never skipped.
package chi

import "net/http"

// Router is the shape of a third-party router.
type Router struct{}

// Get registers a GET route.
func (r *Router) Get(string, http.HandlerFunc) {}

// Post registers a POST route.
func (r *Router) Post(string, http.HandlerFunc) {}

// Register wires the shorthand routes onto the shared mux.
func (r *Router) Register(mux *http.ServeMux) {
	mux.Get("/api/v1/chi/one", func(http.ResponseWriter, *http.Request) {})
	mux.Post("/api/v1/chi/two", func(http.ResponseWriter, *http.Request) {})
	mux.Delete("/api/v1/chi/{id}", func(http.ResponseWriter, *http.Request) {})
}
