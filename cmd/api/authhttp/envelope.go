package authhttp

import (
	"encoding/json"
	"net/http"

	"github.com/lichman0405/post/internal/observability"
)

// Error envelope (docs/22 §5, docs/45): one shape for every API failure —
// code, human message, the request id, optional details. Raw dependency
// errors (SQL, Redis, provider text) never reach this envelope; they are
// logged and mapped to a stable code.
type errorEnvelope struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Retryable bool   `json:"retryable"`
}

// writeError renders the envelope with the given status. The message is
// client-safe by construction: callers pass fixed strings, never error
// text from a dependency.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{
		Code:      code,
		Message:   message,
		RequestID: requestID(r),
		Retryable: status == http.StatusServiceUnavailable,
	})
}

func requestID(r *http.Request) string {
	id, ok := observability.FromContext(r.Context())
	if !ok {
		return ""
	}
	return id.String()
}

// writeJSON renders a success payload.
func WriteJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// logAuthFailure keeps one structured line per failed authentication for
// the security trail (docs/50: no credentials, ever).
func logAuthFailure(r *http.Request, code string) {
	log := observability.LoggerFromContext(r.Context())
	log.Warn("authentication rejected",
		"code", code, "method", r.Method, "path", r.URL.Path)
}
