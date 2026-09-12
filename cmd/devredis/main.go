// Command devredis runs an in-process Redis-protocol server (miniredis) for
// local smoke runs.
//
// It exists because the CI smoke must be runnable without Docker
// (T0006): with no docker-compose stack there is no Redis to consume jobs
// from, and `make smoke` still has to prove the worker really consumes a
// real job over the real Redis wire protocol. Clients connect to it with
// any real Redis client (go-redis, redis-cli); only the server lives
// in-process.
//
// DEV/CI TOOL ONLY: it is not a product binary, it persists nothing, and
// integration gates (G3) use the real Redis from docker compose. Never run
// it as a dependency of a deployed service.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/alicebob/miniredis/v2"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:16379", "Redis-protocol listen address")
	flag.Parse()

	mr := miniredis.NewMiniRedis()
	if err := mr.StartAddr(*addr); err != nil {
		fmt.Fprintf(os.Stderr, "devredis: cannot listen on %s: %v\n", *addr, err)
		os.Exit(1)
	}
	defer mr.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("devredis listening (in-process Redis for smoke runs only)",
		"addr", *addr)
	<-ctx.Done()
	slog.Info("devredis shutting down")
}
