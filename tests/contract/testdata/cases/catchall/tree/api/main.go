// Package main mounts the two shapes the verb-suffix rule is about: the exact
// route, and the catch-all Go forces on a tree that wants `{id}:verb`.
package main

import "net/http"

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/search", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("POST /api/v1/search/{rest...}", func(http.ResponseWriter, *http.Request) {})
}
