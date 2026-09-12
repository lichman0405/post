package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"regexp"
)

// HeaderCorrelationID is the HTTP header every service reads at its edge
// and echoes in its responses: the Go API, the web app and the scientific
// adapter all speak it, so one id traces a request across all of them.
const HeaderCorrelationID = "X-Correlation-ID"

// CorrelationID is the trace identifier carried across every boundary
// (docs/26 §2: each request/command/job carries a correlation id).
type CorrelationID string

// String renders the id (slog/fmt safe).
func (id CorrelationID) String() string { return string(id) }

// correlationIDRe is the accepted shape for an incoming correlation id:
// 8-64 chars of letters, digits, '.', '_' or '-'. Go generates 32 hex
// chars, the web app UUIDs, the adapter 32 hex chars — all fit. Anything
// else (empty, spaces, slashes, log-injection characters) is rejected and
// the edge creates a fresh id: an untrusted header must never reach a log.
var correlationIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{7,63}$`)

// ParseCorrelationID validates an incoming correlation id and returns it.
func ParseCorrelationID(s string) (CorrelationID, bool) {
	if !correlationIDRe.MatchString(s) {
		return "", false
	}
	return CorrelationID(s), true
}

// NewCorrelationID creates a fresh random correlation id (16 bytes hex).
func NewCorrelationID() (CorrelationID, error) {
	id, err := NewRandomID()
	if err != nil {
		return "", err
	}
	return CorrelationID(id), nil
}

// NewRandomID returns n=16 random bytes as lowercase hex — the same source
// job ids use, so the two share one code path.
func NewRandomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

type correlationIDKey struct{}

// WithCorrelationID attaches id to ctx.
func WithCorrelationID(ctx context.Context, id CorrelationID) context.Context {
	return context.WithValue(ctx, correlationIDKey{}, id)
}

// FromContext returns the correlation id attached to ctx, if any.
func FromContext(ctx context.Context) (CorrelationID, bool) {
	id, ok := ctx.Value(correlationIDKey{}).(CorrelationID)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}
