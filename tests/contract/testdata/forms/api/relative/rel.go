// Package relative registers a pattern with no leading slash. net/http would
// panic on it, so it never reaches production; the enumerator refuses it rather
// than compare a path whose meaning it would have to guess.
package relative

import "net/http"

// Register mounts a pattern that is not an absolute path.
func Register(mux *http.ServeMux) {
	mux.HandleFunc("things/{thingId}", func(http.ResponseWriter, *http.Request) {})
}
