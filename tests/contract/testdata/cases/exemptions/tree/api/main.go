// Package main mounts one endpoint that the fixture contract does not declare:
// the exemption list is the only thing that can make it not-undocumented, which
// is what the exemption fixtures vary.
package main

import "net/http"

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/alpha", func(http.ResponseWriter, *http.Request) {})
}
