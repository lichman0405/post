// Package main mounts BOTH shapes: the plain collection route and the catch-all
// the verb suffixes are split out of. It is the green twin of catchall-hole/ —
// same catch-all, same contract entry for the collection path, but here the
// registration the contract names really exists, so the gate must be quiet.
package main

import "net/http"

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/projects/{projectId}/pull-requests", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("POST /api/v1/projects/{projectId}/pull-requests/{number...}", func(http.ResponseWriter, *http.Request) {})
}
