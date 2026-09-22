// Package dynamic registers a pattern held in a variable: the enumerator
// cannot read a path it cannot see, and a skipped registration is precisely
// the failure this instrument exists to prevent.
package dynamic

import "net/http"

const prefix = "/api/v1/computed"

// Register mounts a computed pattern.
func Register(mux *http.ServeMux) {
	pattern := prefix + "/things"
	mux.HandleFunc("GET "+pattern, func(http.ResponseWriter, *http.Request) {})
}
