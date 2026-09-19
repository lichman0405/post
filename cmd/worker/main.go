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
//
// T1005: the email digest sender has arrived — a fifth goroutine over the
// same pool turns the pending email rows the subscription fan-out writes
// into digests, honouring each subscriber's cadence
// (immediate/daily/weekly) and re-authorizing every delivery at send time.
// Its transport is configuration: POST_MAIL_SINK_DIR enables the
// development mail sink (messages are written to a directory, not sent);
// unset, email delivery is off and the worker says so at startup.
//
// T0901: the search projection has arrived — a consumer of the same
// published events turns them into search_documents rows (one per entity,
// visibility copied fail-closed from the entity's own axes), and the index
// it maintains is rebuildable from its sources: -search-rebuild, or the
// search.rebuild job type, re-derives every row from current state.
//
// T0902: the embedding half of the search projection has arrived — a sixth
// goroutine over the same pool fills search_documents.embedding (and the
// provenance 00092 adds beside it) through the embedding port
// (internal/search/embedding), and it too is rebuildable through a command:
// -search-embed, or the search.embed job type, recomputes every vector that
// is not the current model's. The embedder the worker runs is the in-process
// one — no provider, no network, no key — because whether platform content
// may be sent to a third-party embedding service is a question the specs do
// not answer, so it is a task of its own rather than a decision this one
// takes (see the package documentation).
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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/lichman0405/post/internal/application/notifications"
	"github.com/lichman0405/post/internal/config"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/search"
	"github.com/lichman0405/post/internal/search/embedding"
	"github.com/lichman0405/post/internal/version"
	"github.com/lichman0405/post/internal/worker"
)

const (
	exitOK      = 0
	exitRuntime = 1
	exitConfig  = 2
)

// searchRebuildJobType is the job type that rebuilds the search projection
// (T0901). A rebuild is idempotent and safe to repeat (it truncates and
// re-derives in one transaction), which is what makes it a legal job: the
// loop's at-least-once delivery can run it more than once.
const searchRebuildJobType = "search.rebuild"

// searchEmbedJobType is the job type that recomputes the projection's
// embeddings (T0902) — the same call -search-embed makes, enqueued instead
// of typed. Its per-type timeout is longer than the loop's 30s default
// because one job drains the whole backlog; a timeout is safe there anyway,
// since the job is idempotent and resumable (the next attempt re-selects
// what is left).
const searchEmbedJobType = "search.embed"

// searchEmbedTypeTimeout bounds one search.embed job.
const searchEmbedTypeTimeout = 2 * time.Minute

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	flags := flag.NewFlagSet("post-worker", flag.ContinueOnError)
	showVersion := flags.Bool("version", false, "print version and exit")
	// -search-rebuild is the operator's one-shot entry point for the search
	// projection's rebuild (T0901): rebuild the index from its sources and
	// exit, without touching the job queue. It is a flag as well as a job
	// type because a rebuild is something an operator runs while fixing an
	// index, not something a request enqueues — it needs no Redis, and it is
	// the same call either way (runSearchRebuild).
	searchRebuild := flags.Bool("search-rebuild", false,
		"rebuild the search projection from its sources and exit")
	// -search-embed is the same kind of entry point for the projection's
	// vectors (T0902): recompute every search_documents row whose embedding
	// is not the current model's, report what it did, and exit. It is the
	// "rebuild" of a derived column, so it is idempotent, it needs PostgreSQL
	// and nothing else (no Redis, no queue), and it is the call the
	// search.embed job type makes.
	searchEmbed := flags.Bool("search-embed", false,
		"recompute the embedding of every search document that needs it and exit")
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

	// The database pool every consumer below shares (T1001). It is lazy —
	// the worker starts and keeps retrying while PostgreSQL is down, like
	// every other dependency outage.
	pool, err := persistence.OpenLazy(ctx, databaseDSN(cfg))
	if err != nil {
		slog.Error("post-worker: opening database pool", "error", err)
		return exitConfig
	}

	if *searchRebuild {
		return runSearchRebuild(ctx, pool, logger)
	}

	// The embedding port's one implementation, built once and used by both
	// paths below. LocalModel names the in-process embedder this binary
	// contains; a deployment that reaches a real provider replaces the
	// implementation AND its identity together, which is what the provenance
	// columns record. There is no configuration here on purpose: no
	// environment variable turns on an outbound embedder, because none
	// exists yet (T0902 package documentation).
	embedder, err := embedding.NewDeterministic(embedding.LocalModel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "post-worker: embedding configuration error:\n%v\n", err)
		return exitConfig
	}
	embedJob, err := embedding.NewWorker(pool, embedder, embedding.WithLogger(logger))
	if err != nil {
		fmt.Fprintf(os.Stderr, "post-worker: embedding configuration error:\n%v\n", err)
		return exitConfig
	}
	if *searchEmbed {
		return runSearchEmbed(ctx, embedJob, logger)
	}

	redisClient := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr})
	defer redisClient.Close()

	queue := worker.NewRedisQueue(redisClient, "post")
	loop := worker.NewLoop(queue, worker.WithLogger(logger),
		worker.WithTypeTimeout(searchEmbedJobType, searchEmbedTypeTimeout))
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
	// The search projection (T0901): a fourth consumer of the outbox beside
	// the webhook and subscription fan-outs. It turns published events into
	// search_documents rows and keeps its own cursor
	// (search_projected_events), so it consumes the same published rows
	// without seeing, or being seen by, the other consumers' progress. Being
	// a projection, it is also the one consumer with a rebuild — the job type
	// below, and the -search-rebuild entry point an operator runs directly.
	projector := search.NewProjector(pool, search.WithLogger(logger))
	loop.Register(searchRebuildJobType, func(ctx context.Context, job worker.Job) error {
		report, err := projector.Rebuild(ctx)
		if err != nil {
			// Returning the error retries the job (it is idempotent, so a
			// retry re-derives rather than doubles) and dead-letters it after
			// the cap.
			slog.Error("post-worker: search rebuild job failed",
				"job_id", job.ID, "correlation_id", job.CorrelationID, "error", err)
			return err
		}
		slog.Info("post-worker: search rebuild job complete",
			"job_id", job.ID, "correlation_id", job.CorrelationID,
			"removed", report.Removed, "projected", report.Projected,
			"by_entity_type", report.EntityTypes())
		return nil
	})
	// The embedding backlog (T0902): the job-queue form of -search-embed,
	// and the same call. Returning the error is the whole of the retry
	// policy — the loop retries with capped exponential backoff and
	// dead-letters after the cap (internal/worker/worker.go), and the job is
	// idempotent, so an attempt that dies mid-backlog loses nothing.
	loop.Register(searchEmbedJobType, func(ctx context.Context, job worker.Job) error {
		report, err := embedJob.Recompute(ctx)
		if err != nil {
			slog.Error("post-worker: search embed job failed",
				"job_id", job.ID, "correlation_id", job.CorrelationID,
				"model", embedJob.Model().String(), "error", err)
			return err
		}
		slog.Info("post-worker: search embed job complete",
			"job_id", job.ID, "correlation_id", job.CorrelationID,
			"embedded", report.Embedded, "batches", report.Batches,
			"model", embedJob.Model().String())
		return nil
	})

	// The transactional-outbox dispatcher (T1001): publishes the outbox
	// rows the API recorded inside its commit transactions into
	// research_events, and runs until shutdown.
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
	// cancellation. Its fan-out cursor lives in outbox_events
	// (webhook_fanned_out_at), so it and the subscription fan-out below
	// consume the same published rows without seeing each other's progress.
	fanout := events.NewFanOut(pool, events.WithFanOutLogger(logger))
	deliverer := events.NewDeliverer(pool, events.WithDelivererLogger(logger))
	var pipelineWG sync.WaitGroup
	pipelineWG.Add(2)
	go func() {
		defer pipelineWG.Done()
		_ = fanout.Run(ctx)
	}()
	go func() {
		defer pipelineWG.Done()
		_ = deliverer.Run(ctx)
	}()
	// The subscription fan-out (T1002), a third consumer of the same pool:
	// it turns published events into per-subscriber delivery rows, resolving
	// each subscriber's access to the event's target against live state as
	// it goes (events.SubscriptionFanOut). It joins the same wait group as
	// the webhook pair — same pool, same shutdown contract — and its own
	// cursor table (subscription_fanned_events) keeps it independent of the
	// webhook pipeline's.
	subscriptionFanout := events.NewSubscriptionFanOut(pool, events.WithSubscriptionFanOutLogger(logger))
	pipelineWG.Add(1)
	go func() {
		defer pipelineWG.Done()
		_ = subscriptionFanout.Run(ctx)
	}()
	// The search projection (T0901), the fourth consumer of the same pool: it
	// turns published events into search_documents rows, one per entity, and
	// keeps its cursor in search_projected_events (00090) so it consumes the
	// same published rows as the two fan-outs without sharing their progress.
	// It joins the same wait group and stops on ctx cancellation, exactly
	// like every other consumer.
	pipelineWG.Add(1)
	go func() {
		defer pipelineWG.Done()
		_ = projector.Run(ctx)
	}()
	// The embedding backlog (T0902), the sixth consumer of the same pool: it
	// fills search_documents.embedding for the rows the projector writes (and
	// for any row a model change has made stale), through the embedding port.
	// It joins the same wait group and stops on ctx cancellation like every
	// other consumer. What it is embedding with is stated at startup, for the
	// same reason the mail transport is: "no vectors appeared" and "embeddings
	// were never configured" must not look the same.
	pipelineWG.Add(1)
	go func() {
		defer pipelineWG.Done()
		_ = embedJob.Run(ctx)
	}()
	// The email digest sender (T1005), the last consumer of the same pool:
	// it claims the pending email rows the subscription fan-out writes, when
	// the subscriber's cadence says a digest is due, re-authorizes every
	// claimed delivery against live state (the send-time gate — the fan-out
	// cannot revisit a target until the next event on it arrives), renders
	// one digest per subscriber and hands it to the transport.
	//
	// The transport is configuration, not code: POST_MAIL_SINK_DIR names the
	// dev mail sink's directory. Unset means email delivery is OFF — the
	// sender is not started at all and the pending rows stay pending (they
	// are not lost: a worker configured with a sink later sends them). The
	// switch is stated in the startup log either way, because "no email
	// arrived" and "email was never configured" must not look the same.
	mailCfg, err := notifications.Loader{}.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "post-worker: configuration error:\n%v\n", err)
		return exitConfig
	}
	if mailCfg.Enabled {
		sink, err := notifications.NewDevSink(mailCfg.SinkDir, notifications.WithDevSinkLogger(logger))
		if err != nil {
			fmt.Fprintf(os.Stderr, "post-worker: mail sink error:\n%v\n", err)
			return exitConfig
		}
		sender := notifications.NewSender(
			events.NewNotificationStore(pool), sink,
			notifications.WithSenderLogger(logger),
			notifications.WithSenderBaseURL(mailCfg.WebOrigin),
		)
		if cfg.Layer == config.LayerProd {
			// The dev sink writes messages to a local directory instead of
			// sending them: in a production layer that is a silent no-op for
			// every subscriber, so it is said out loud.
			slog.Warn("post-worker: email delivery uses the DEVELOPMENT mail sink — messages are written to disk, not sent",
				"dir", mailCfg.SinkDir, "layer", cfg.Layer,
				"effect", "subscribers receive nothing until a real transport is configured")
		}
		pipelineWG.Add(1)
		go func() {
			defer pipelineWG.Done()
			_ = sender.Run(ctx)
		}()
		slog.Info("post-worker: email digest sender started",
			"dir", mailCfg.SinkDir, "web_origin", mailCfg.WebOrigin)
	} else {
		slog.Info("post-worker: email delivery disabled — no mail transport configured",
			"missing", notifications.EnvMailSinkDir,
			"effect", "pending email deliveries stay pending until a transport is configured")
	}
	// Shutdown ordering (T1001 review): cancel the root context first —
	// the deferred stop() above would run too late for this join (defers
	// run LIFO, so it fires only after the cleanup below) — then join the
	// dispatcher and pipeline goroutines, then close the pool. Closing
	// under a live consumer would let its next pass fail against a closed
	// pool and log a misleading error.
	defer shutdown(stop, dispatcherWG.Wait, pipelineWG.Wait, pool.Close)

	slog.Info("post-worker running",
		"version", version.Version, "queue", "post:queue:jobs", "cfg", cfg,
		"embedding_model", embedJob.Model().String(), "embedding_transport", "in-process (no provider, no network)")
	if err := loop.Run(ctx); err != nil {
		slog.Error("post-worker loop failed", "error", err)
		return exitRuntime
	}
	slog.Info("post-worker shutting down")
	return exitOK
}

// runSearchRebuild is the -search-rebuild entry point: rebuild the search
// projection from its sources, report what it did, and exit.
//
// It is the same call the search.rebuild job type makes, driven directly,
// and it is deliberately independent of the job queue: rebuilding an index
// is an operator action on a machine that has PostgreSQL, and requiring a
// running Redis and a live loop to do it would make an index repair depend
// on a component the repair does not touch. It opens the pool (lazy, like
// every other consumer), so it fails with the connection error rather than
// silently reporting an empty rebuild.
//
// Running it twice is the same as running it once: the rebuild truncates and
// re-derives every row from source state in one transaction, so the index
// after the second run is the index after the first (updated_at is stamped
// now() on each run and is the one column that moves).
func runSearchRebuild(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) int {
	report, err := search.NewProjector(pool, search.WithLogger(logger)).Rebuild(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "post-worker: search rebuild failed:\n%v\n", err)
		return exitRuntime
	}
	logger.Info("post-worker: search rebuild complete",
		"removed", report.Removed, "projected", report.Projected,
		"by_entity_type", report.EntityTypes())
	// The report also goes to stdout, one line, so an operator running the
	// command reads what it did without parsing the JSON log.
	fmt.Printf("search rebuild: removed=%d projected=%d %s\n",
		report.Removed, report.Projected, strings.Join(report.EntityTypes(), " "))
	return exitOK
}

// runSearchEmbed is the -search-embed entry point: recompute every document
// whose stored vector is not the current model's, report what it did, and
// exit.
//
// It is the same call the search.embed job type makes, driven directly, and
// it is independent of the job queue for the same reason -search-rebuild is:
// recomputing a derived column is an operator action on a machine that has
// PostgreSQL, and making it require a live Redis would make an embedding
// repair depend on a component the repair does not touch.
//
// Running it twice is the same as running it once: the second run selects
// nothing (every row already carries the current model's vector and
// provenance) and reports embedded=0 — the idempotence is in the selection
// predicate, not in a comparison of results.
func runSearchEmbed(ctx context.Context, job *embedding.Worker, logger *slog.Logger) int {
	report, err := job.Recompute(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "post-worker: search embed failed:\n%v\n", err)
		return exitRuntime
	}
	logger.Info("post-worker: search embed complete",
		"embedded", report.Embedded, "batches", report.Batches,
		"model", job.Model().String())
	// The report also goes to stdout in one line, like the rebuild's, so an
	// operator reads what it did without parsing the JSON log — and so a run
	// that embedded nothing is distinguishable from a run that did nothing.
	fmt.Printf("search embed: %s model=%s\n", report, job.Model())
	return exitOK
}

// shutdown cancels the root context, joins the outbox dispatcher, webhook
// pipeline and subscription fan-out goroutines, and only then closes the
// database pool —
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
