package observability

import (
	"log/slog"
	"net/http"
	"time"
)

// Middleware returns the HTTP edge middleware: it assigns the correlation
// id (honouring a valid incoming one), echoes it in the response, attaches
// the request-scoped logger to the context, logs one structured
// request-completion line per request, and records the request in the HTTP
// metric families (docs/26 §3 HTTP latency/error).
//
// The completion line records method, path, status and duration. It never
// records the query string — query values are a classic credential carrier
// and there is no way to know they are safe.
//
// The metrics deliberately do NOT carry the path: post_http_requests_total's
// route label is http.Request.Pattern, the ServeMux pattern that matched
// ("/api/v1/projects/{projectId}"), because a raw path puts a project or
// asset id in a label — unbounded cardinality, and an endpoint that answers
// "does this id exist" to anyone who can read it. A request that matched no
// pattern is recorded as route="unmatched".
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
			// The cell that carries the innermost matched route back up
			// through any middleware that rebuilds the request (see
			// route.go — the /api/v1 guard does).
			ctx, routeCell := withRouteCell(ctx)
			r = r.WithContext(ctx)

			sw := &statusWriter{ResponseWriter: w}
			start := time.Now()
			next.ServeHTTP(sw, r)
			elapsed := time.Since(start)

			status := sw.statusCode()
			reqLog.Info("http request completed",
				"method", r.Method,
				"path", r.URL.Path, // never r.URL.RawQuery: may carry credentials
				"status", status,
				"duration", elapsed,
			)

			Default().ObserveHTTP(r.Method, resolveRoute(routeCell.get(), r), status, elapsed.Seconds())
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
