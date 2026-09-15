// Command worker is the POST background worker.
//
// T0006: the worker now loads the validated configuration and runs a real
// minimal job loop over Redis (docs/20 §9): jobs are consumed with the
// reliable-queue pattern and completed with the idempotency / retry /
// dead-letter shape docs/52「Workers」 and ADR-013 require. The only
// registered job type is "smoke".
//
// T1001: the domain event outbox consumer has arrived — a dispatcher
// goroutine publishes outbox rows (written by the API inside its commit
// transactions) into research_events, at-least-once with an idempotent
// publish. It retries forever while PostgreSQL is down, like every other
// dependency outage, and stops only on shutdown.
//
// T0007: logs are structured JSON on stderr, go-redis's internal retry
// chatter is routed through the structured logger, and every job log line
// carries the correlation id of whatever triggered the job (the loop
// attaches it per job; the enqueueing request's id is embedded in the job
// payload by the API).
//
// T1006: the signed-webhook pipeline has arrived — after the outbox
// dispatcher publishes research events, a fan-out goroutine turns
// published events into per-endpoint delivery rows (public events only,
// fail closed) and a deliverer goroutine POSTs them signed (HMAC +
// timestamp) with exponential-backoff retries and the disable policy.
// Both loop forever on transient failures like the dispatcher and stop
// only on shutdown.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"

	"github.com/redis/go-redis/v9"

	"github.com/lichman0405/post/internal/config"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/persistence"
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

	// The transactional-outbox dispatcher (T1001): publishes the outbox
	// rows the API recorded inside its commit transactions into
	// research_events. The pool is lazy — the worker starts and keeps
	// retrying while PostgreSQL is down, like every other dependency
	// outage — and the dispatcher loop runs until shutdown.
	pool, err := persistence.OpenLazy(ctx, databaseDSN(cfg))
	if err != nil {
		slog.Error("post-worker: opening database pool", "error", err)
		return exitConfig
	}
	dispatcher := events.NewDispatcher(pool, events.WithLogger(logger))
	var dispatcherWG sync.WaitGroup
	dispatcherWG.Add(1)
	go func() {
		defer dispatcherWG.Done()
		_ = dispatcher.Run(ctx)
	}()
	// The signed-webhook pipeline (T1006), two more consumers of the same
	// pool: FanOut turns published events into delivery rows, Deliverer
	// posts them. Both poll with their own cadence and stop on ctx
	// cancellation.
	fanout := events.NewFanOut(pool, events.WithFanOutLogger(logger))
	deliverer := events.NewDeliverer(pool, events.WithDelivererLogger(logger))
	var webhookWG sync.WaitGroup
	webhookWG.Add(2)
	go func() {
		defer webhookWG.Done()
		_ = fanout.Run(ctx)
	}()
	go func() {
		defer webhookWG.Done()
		_ = deliverer.Run(ctx)
	}()
	// Shutdown ordering (T1001 review): cancel the root context first —
	// the deferred stop() above would run too late for this join (defers
	// run LIFO, so it fires only after the cleanup below) — then join the
	// dispatcher and webhook goroutines, then close the pool. Closing
	// under a live consumer would let its next pass fail against a closed
	// pool and log a misleading error.
	defer shutdown(stop, dispatcherWG.Wait, webhookWG.Wait, pool.Close)

	slog.Info("post-worker running",
		"version", version.Version, "queue", "post:queue:jobs", "cfg", cfg)
	if err := loop.Run(ctx); err != nil {
		slog.Error("post-worker loop failed", "error", err)
		return exitRuntime
	}
	slog.Info("post-worker shutting down")
	return exitOK
}

// shutdown cancels the root context, joins the outbox dispatcher and
// webhook pipeline goroutines, and only then closes the database pool —
// in that order (T1001 review: closing the pool under a live consumer
// makes its next pass fail against a closed pool and log a misleading
// error). stop is called here rather than via a top-level defer because
// defers run LIFO: a deferred stop would fire after the join and deadlock
// it.
func shutdown(stop context.CancelFunc, joins ...func()) {
	stop()
	for _, join := range joins {
		join()
	}
}

// databaseDSN builds the PostgreSQL connection URL the outbox dispatcher
// opens — the same shape as cmd/api's databaseDSN (both are package main,
// the helper is copied rather than shared).
func databaseDSN(cfg *config.Config) string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(cfg.Database.User, string(cfg.Database.Password)),
		Host:   net.JoinHostPort(cfg.Database.Host, strconv.Itoa(cfg.Database.Port)),
		Path:   "/" + cfg.Database.Name,
	}
	q := u.Query()
	q.Set("sslmode", cfg.Database.SSLMode)
	u.RawQuery = q.Encode()
	return u.String()
}
