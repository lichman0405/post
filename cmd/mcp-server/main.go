// Command mcp-server is the POST MCP endpoint.
//
// T0006: the server loads the validated configuration (fail closed, like
// every Go entry point) and serves GET /healthz (liveness) plus GET /readyz.
// /readyz answers "ready" with no checks: no downstream dependency is wired
// yet, and reporting a false dependency state would be a lie — the MCP tool
// wiring (tool catalog per specs/mcp and docs/47) arrives with the agent
// tasks, which will add real readiness probes. The /mcp route still answers
// 501: this process deliberately does not fake an MCP handshake.
//
// T0007: like the API, every request passes the observability middleware
// (correlation id at the edge, structured request logs); logs are JSON on
// stderr.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lichman0405/post/internal/config"
	"github.com/lichman0405/post/internal/health"
	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/version"
)

const (
	exitOK      = 0
	exitRuntime = 1
	exitConfig  = 2
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	flags := flag.NewFlagSet("post-mcp-server", flag.ContinueOnError)
	showVersion := flags.Bool("version", false, "print version and exit")
	addr := flags.String("addr", "", "HTTP listen address (default: POST_MCP_ADDR)")
	if err := flags.Parse(args); err != nil {
		return exitConfig
	}

	if *showVersion {
		fmt.Println("post-mcp-server", version.Version)
		return exitOK
	}

	// Validated configuration first (T0006): the server never starts on a
	// guessed environment; a missing/invalid variable fails fast naming it.
	cfg, err := config.LoadFromCwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "post-mcp-server: configuration error:\n%v\n", err)
		return exitConfig
	}
	if *addr != "" {
		cfg.Server.MCPAddr = *addr
	}

	// Structured JSON logs on stderr, request-scoped correlation ids at the
	// edge (T0007).
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	mux := http.NewServeMux()
	healthHandler := health.NewHandler("mcp-server")
	mux.Handle("/healthz", healthHandler)
	mux.Handle("/readyz", healthHandler)
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		fmt.Fprintf(w, `{"error":"MCP protocol wiring not implemented yet (agent tasks)"}`)
	})

	srv := &http.Server{
		Addr:              cfg.Server.MCPAddr,
		Handler:           observability.Middleware(logger)(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("post-mcp-server listening",
			"addr", cfg.Server.MCPAddr, "version", version.Version, "cfg", cfg)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		slog.Info("post-mcp-server shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("shutdown failed", "error", err)
			return exitRuntime
		}
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("post-mcp-server exited", "error", err)
			return exitRuntime
		}
	}
	return exitOK
}
