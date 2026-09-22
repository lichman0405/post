// Package commented holds a commented-out registration and a route-shaped
// string in prose: neither is a registration, and the enumerator reads the
// AST rather than the bytes, so neither can be mistaken for one.
package commented

import "net/http"

// Register mounts nothing; the line below is a comment.
//
//	mux.HandleFunc("GET /api/v1/commented-out", handler)
//
// and this sentence mentions "POST /api/v1/in-prose" without registering it.
func Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/commented/real", func(http.ResponseWriter, *http.Request) {})
}
