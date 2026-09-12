// Command worker is the POST background worker.
//
// T0006: the worker now loads the validated configuration and runs a real
// minimal job loop over Redis (docs/20 §9): jobs are consumed with the
// reliable-queue pattern and completed with the idempotency / retry /
// dead-letter shape docs/52 §17 and ADR-013 require. The only registered
// job type is "smoke" — the domain event outbox consumer arrives with the
// event tasks; this loop already consumes and completes real jobs end to
// end.
//
// T0007: logs are structured JSON on stderr, go-redis's internal retry
// chatter is routed through the structured logger, and every job log line
// carries the correlation id of whatever triggered the job (the loop
// attaches it per job; the enqueueing request's id is embedded in the job
// payload by the API).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/redis/go-redis/v9"

	"github.com/lichman0405/post/internal/config"
	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/version"
	"github.com/lichman0405/post/internal/worker"
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
	flags := flag.NewFlagSet("post-worker", flag.ContinueOnError)
	showVersion := flags.Bool("version", false, "print version and exit")
	if err := flags.Parse(args); err != nil {
		return exitConfig
	}

	if *showVersion {
		fmt.Println("post-worker", version.Version)
		return exitOK
	}

	// Validated configuration first (T0006): the worker never starts on a
	// guessed environment; a missing/invalid variable fails fast naming it.
	cfg, err := config.LoadFromCwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "post-worker: configuration error:\n%v\n", err)
		return exitConfig
	}

	// Structured JSON logs on stderr; go-redis chatter through the same
	// logger (T0007).
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	observability.RouteRedisLogging(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	redisClient := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr})
	defer redisClient.Close()

	queue := worker.NewRedisQueue(redisClient, "post")
	loop := worker.NewLoop(queue, worker.WithLogger(logger))
	// The minimal registered job type: completes a real job end to end. The
	// outbox dispatcher handlers arrive with the event tasks.
	loop.Register("smoke", func(ctx context.Context, job worker.Job) error {
		// The payload is caller-controlled input on this scaffold path, so
		// it goes through the shared redactor before it reaches the log —
		// a credential-shaped value must never be logged raw (T0007).
		slog.Info("post-worker smoke job",
			"job_id", job.ID,
			"correlation_id", job.CorrelationID,
			"payload", config.RedactForOutput(string(job.Payload)),
		)
		return nil
	})

	slog.Info("post-worker running",
		"version", version.Version, "queue", "post:queue:jobs", "cfg", cfg)
	if err := loop.Run(ctx); err != nil {
		slog.Error("post-worker loop failed", "error", err)
		return exitRuntime
	}
	slog.Info("post-worker shutting down")
	return exitOK
}
