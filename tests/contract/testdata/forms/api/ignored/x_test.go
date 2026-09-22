package ignored

import (
	"net/http"
	"testing"
)

// TestMuxIsNotARoute proves the enumerator skips test files: a mux assembled
// inside a test is not mounted in any binary, and counting it would turn
// "documented endpoint" into "somebody wrote this string in a test once".
func TestMuxIsNotARoute(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/only-in-a-test", func(http.ResponseWriter, *http.Request) {})
}
