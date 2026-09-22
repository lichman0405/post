// Package main mounts ONLY the catch-all. A contract entry for the plain
// collection path must therefore read as unmounted: the catch-all exists for
// `{id}:verb`, and letting it satisfy a verb-less entry would hide the deletion
// of the real registration behind a wildcard.
package main

import "net/http"

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/projects/{projectId}/pull-requests/{number...}", func(http.ResponseWriter, *http.Request) {})
}
