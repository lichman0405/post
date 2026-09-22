package observability

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// ServeMetrics starts the Prometheus endpoint on addr and returns the
// server. It is used by the processes that have no HTTP surface of their own
// (cmd/worker); cmd/api mounts Metrics.Handler on its existing server
// instead, because that process already listens.
//
// The listener is bound BEFORE returning, so a caller that gets a nil error
// knows the port is actually serving — a metrics endpoint that silently
// failed to bind is the failure this whole task removes, and a goroutine
// that logs the bind error into a log nobody greps would reintroduce it.
//
// It serves GET /metrics only; every other path is 404, so the endpoint
// cannot grow into an accidental admin surface.
func ServeMetrics(ctx context.Context, addr string, m *Metrics, log *slog.Logger) (*http.Server, error) {
	if m == nil {
		m = Default()
	}
	if log == nil {
		log = slog.Default()
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("observability: metrics listen on %s: %w", addr, err)
	}

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Info("metrics endpoint listening", "addr", ln.Addr().String(), "path", "/metrics")
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Error("metrics endpoint stopped", "addr", addr, "error", err)
		}
	}()

	// Shutdown is driven by ctx so the listener cannot outlive the process's
	// own signal handling; the caller may also call Shutdown directly.
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	return srv, nil
}
