package observability

import (
	"context"
	"log/slog"
)

type loggerKey struct{}

// WithLogger attaches the request-scoped logger (correlation id attached)
// to ctx. Handlers log through LoggerFromContext(ctx) so their lines carry
// the id without every call site remembering it.
func WithLogger(ctx context.Context, log *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, log)
}

// LoggerFromContext returns the request-scoped logger, or slog.Default()
// when the context carries none (e.g. background jobs without a request).
func LoggerFromContext(ctx context.Context) *slog.Logger {
	if log, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok && log != nil {
		return log
	}
	return slog.Default()
}
