// Package sub is mounted at /api/v1/sub by the synthetic main, and registers
// its endpoints under their ABSOLUTE patterns — the convention cmd/api's
// per-surface packages use (net/http's ServeMux hands the mounted handler the
// full path, so the inner patterns are absolute too).
package sub

import "net/http"

// API is a surface.
type API struct{}

// Routes returns the surface's own mux.
func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/sub/one", func(http.ResponseWriter, *http.Request) {})
	mux.Handle("/api/v1/sub/two", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	return mux
}

// Register mounts directly on a shared mux.
func (a *API) Register(v1 *http.ServeMux) {
	v1.HandleFunc("PATCH /api/v1/sub/three", func(http.ResponseWriter, *http.Request) {})
}
