// Command mcp-server is the POST MCP endpoint.
//
// T0002 scaffold: the listener exists and answers the MCP route with 501 Not
// Implemented. The MCP protocol wiring (tool catalog per specs/mcp and
// docs/47) arrives with the agent tasks; this process deliberately does not
// fake an MCP handshake.
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

	"github.com/lichman0405/post/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	addr := flag.String("addr", envOr("POST_MCP_ADDR", ":9080"), "HTTP listen address")
	flag.Parse()

	if *showVersion {
		fmt.Println("post-mcp-server", version.Version)
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		fmt.Fprintf(w, `{"error":"MCP protocol wiring not implemented yet (T0002 scaffold)"}`)
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("post-mcp-server scaffold listening", "addr", *addr, "version", version.Version)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		slog.Info("post-mcp-server shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("shutdown failed", "error", err)
			os.Exit(1)
		}
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("post-mcp-server exited", "error", err)
			os.Exit(1)
		}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
