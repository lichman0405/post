// Command worker is the POST background worker.
//
// T0002 scaffold: no jobs are wired yet. The real worker consumes the domain
// event outbox (ADR-013) with idempotency, retry and dead-letter handling
// (docs/52); those arrive with the event tasks. For now the process starts,
// logs its identity and waits for SIGINT/SIGTERM so lifecycle wiring is real.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/lichman0405/post/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("post-worker", version.Version)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("post-worker scaffold running",
		"version", version.Version,
		"note", "no jobs wired yet — outbox consumer arrives with the event tasks (ADR-013)")
	<-ctx.Done()
	slog.Info("post-worker shutting down")
}
