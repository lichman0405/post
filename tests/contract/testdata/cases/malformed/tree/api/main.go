// Package main registers a pattern with a dangling brace. Go's ServeMux would
// panic on it, so it never reaches production — the point is that the
// comparator names the segment instead of comparing a half-read path as if it
// were a literal.
package main

import "net/http"

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/broken/{thingId", func(http.ResponseWriter, *http.Request) {})
}
