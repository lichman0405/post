// Package main mounts two endpoints. The two directions the comparator has to
// judge are exercised against this tree: a contract missing POST /api/v1/beta
// (mounted but undocumented) and a contract naming a path nothing mounts
// (documented but unmounted).
package main

import "net/http"

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/alpha", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("POST /api/v1/beta", func(http.ResponseWriter, *http.Request) {})
}
