package observability

import (
	"log/slog"
	"net/http"
	"time"
)

// Middleware returns the HTTP edge middleware: it assigns the correlation
// id (honouring a valid incoming one), echoes it in the response, attaches
// the request-scoped logger to the context, and logs one structured
// request-completion line per request.
//
// The completion line records method, path, status and duration. It never
// records the query string — query values are a classic credential carrier
// and there is no way to know they are safe.
func Middleware(log *slog.Logger) func(http.Handler) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := resolveRequestID(r, log)
			w.Header().Set(HeaderCorrelationID, string(id))

			reqLog := log
			if id != "" {
				reqLog = log.With("correlation_id", string(id))
			}
			ctx := WithLogger(r.Context(), reqLog)
			ctx = WithCorrelationID(ctx, id)
			r = r.WithContext(ctx)

			sw := &statusWriter{ResponseWriter: w}
			start := time.Now()
			next.ServeHTTP(sw, r)

			reqLog.Info("http request completed",
				"method", r.Method,
				"path", r.URL.Path, // never r.URL.RawQuery: may carry credentials
				"status", sw.statusCode(),
				"duration", time.Since(start),
			)
		})
	}
}

// resolveRequestID honours a valid incoming X-Correlation-ID; anything
// invalid (or absent) gets a fresh edge-created id.
func resolveRequestID(r *http.Request, log *slog.Logger) CorrelationID {
	if id, ok := ParseCorrelationID(r.Header.Get(HeaderCorrelationID)); ok {
		return id
	}
	id, err := NewCorrelationID()
	if err != nil {
		// crypto/rand failed: the process has bigger problems; log and
		// continue without a correlation id rather than guessing one.
		log.Error("correlation id generation failed; request runs uncorrelated", "error", err)
		return ""
	}
	return id
}

// statusWriter captures the response status for the completion log while
// preserving the optional interfaces (Flusher, Hijacker, ...) via Unwrap.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// statusCode returns the recorded status; 200 when the handler wrote a body
// without calling WriteHeader.
func (w *statusWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// Unwrap keeps http.ResponseController working through the middleware
// (Flush, Hijack, SetReadDeadline, ...).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
