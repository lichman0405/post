// Command api is the POST HTTP API server.
//
// T0006: the server now loads the validated configuration (internal/config)
// and fails closed when it is missing or invalid — configuration is in
// effect at runtime, not just validated (the T0004 "looks green, isn't in
// effect" closure). It serves:
//
//	GET /healthz  process liveness only (never depends on downstream services)
//	GET /readyz   readiness: PostgreSQL and Redis are probed and reported
//	              truthfully — a down dependency answers 503 not_ready, the
//	              process never crashes and never answers a false "ok".
//
// The database pool is opened lazily on purpose: the API must start (and
// report "not ready") while PostgreSQL is down, not refuse to start.
//
// T0007: every request passes the observability middleware — a correlation
// id is created (or honoured from X-Correlation-ID) at the edge, echoed in
// the response and carried by every log line of the request, including the
// background job the request enqueues. Logs are structured JSON on stderr
// and go-redis's internal chatter is routed through the same logger.
//
// Product endpoints arrive with the OpenAPI-first API tasks; the contract
// seed lives in specs/api/openapi.yaml (docs/52: transport/http ->
// application -> domain).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/lichman0405/post/cmd/api/audithttp"
	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/orgshttp"
	"github.com/lichman0405/post/cmd/api/profilehttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/cmd/api/rsghttp"
	"github.com/lichman0405/post/cmd/api/validationhttp"
	"github.com/lichman0405/post/internal/application/audit"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/states"
	appvalidation "github.com/lichman0405/post/internal/application/validation"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/config"
	"github.com/lichman0405/post/internal/gitprovider"
	"github.com/lichman0405/post/internal/health"
	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
	"github.com/lichman0405/post/internal/version"
	"github.com/lichman0405/post/internal/worker"
)

// Exit codes: 0 ok, 1 runtime failure, 2 configuration failure (the same
// convention as the scientific adapter).
const (
	exitOK      = 0
	exitRuntime = 1
	exitConfig  = 2
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	flags := flag.NewFlagSet("post-api", flag.ContinueOnError)
	showVersion := flags.Bool("version", false, "print version and exit")
	addr := flags.String("addr", "", "HTTP listen address (default: POST_API_ADDR)")
	if err := flags.Parse(args); err != nil {
		return exitConfig
	}

	if *showVersion {
		fmt.Println("post-api", version.Version)
		return exitOK
	}

	// The validated configuration is in effect at runtime: a missing or
	// invalid variable fails fast here, naming the offending key (T0006).
	cfg, err := config.LoadFromCwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "post-api: configuration error:\n%v\n", err)
		return exitConfig
	}
	if *addr != "" {
		cfg.Server.Addr = *addr
	}

	// GitProvider adapter configuration (T0301): a PRESENT variable must be
	// valid — an invalid value fails fast here, naming the offending key
	// (T0006 validates values, not absence) — while an unset token or
	// webhook URL is a legal deployment: the API starts with provisioning
	// disabled (see the wiring below), mirroring how the database being
	// down or the authn loader being optional never prevents startup.
	gitCfg, err := gitproviderLoader().Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "post-api: gitprovider configuration error:\n%v\n", err)
		return exitConfig
	}

	// Structured JSON logs on stderr; every request-scoped line carries the
	// correlation id (T0007).
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	observability.RouteRedisLogging(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Lazy pool: the API starts while the database is down and reports it
	// through /readyz instead of refusing to start.
	pool, err := persistence.OpenLazy(ctx, databaseDSN(cfg))
	if err != nil {
		slog.Error("post-api: database pool setup failed", "error", err)
		return exitRuntime
	}
	defer pool.Close()

	redisClient := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr})
	defer redisClient.Close()

	mux := http.NewServeMux()
	healthz := newHealthHandler(pool, redisClient)
	mux.Handle("/healthz", healthz)
	mux.Handle("/readyz", healthz)
	// The scaffold enqueue surface (POST /internal/jobs, T0007) keeps its
	// own queue: it feeds cmd/worker's "post" queue with smoke jobs, and
	// that flow is unchanged by T0301.
	scaffoldQueue := worker.NewRedisQueue(redisClient, "post")
	mux.Handle("POST /internal/jobs", newJobHandler(scaffoldQueue, logger))

	// Repository provisioning (T0301): the GitPort adapter on the service
	// account token, the canonical-store adapter, and the job loop that
	// consumes project-provision jobs (enqueued by the project create
	// handler below and by the startup sweep of pending projects). Its
	// own queue prefix keeps it from stealing or dead-lettering the
	// scaffold's smoke jobs, and vice versa.
	//
	// The queue exists unconditionally — project creation enqueues its job
	// either way, and a later restart with the full configuration sweeps
	// the canonical backlog and provisions every pending project — but the
	// pipeline itself only runs on a full configuration. A deployment that
	// does not set the token or the webhook URL is not an error (the API
	// stays up, like it does while the database is down): provisioning is
	// simply off, and the warning names exactly which keys are missing.
	provisioningQueue := worker.NewRedisQueue(redisClient, provisioningQueuePrefix)
	if gitCfg.ProvisioningEnabled() {
		provisioningStore := gitprovider.NewProvisionStore(pool)
		giteaAdapter := gitprovider.NewGiteaAdapter(*gitCfg)
		provisioner := gitprovider.NewProvisioner(giteaAdapter, provisioningStore, gitCfg.WebhookURL)
		provisioningLoop := worker.NewLoop(provisioningQueue, worker.WithLogger(logger))
		provisioningLoop.Register(gitprovider.ProvisionJobType, newProvisioningHandler(provisioner))
		go func() {
			if err := provisioningLoop.Run(ctx); err != nil {
				slog.Error("post-api: provisioning loop failed", "error", err)
			}
		}()
		// Sweep projects left pending by an earlier run (or a down Redis at
		// creation time). Own timeout: the pool is lazy and PostgreSQL may
		// still be starting — a failure here retries on the next API start.
		go func() {
			sweepCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			enqueuePendingProvisioning(sweepCtx, provisioningStore, provisioningQueue, logger)
		}()
		// Main-protection sweep (T0302): the platform layer of main's
		// double protection — re-applies the canonical Gitea rule on every
		// provisioned repository once at startup and then on a schedule, so
		// a rule an operator removed or drifted is healed without a
		// restart. Best effort: failures are logged per repository and
		// retried on the next pass.
		go runMainProtectionSweep(ctx,
			gitprovider.NewProtectionSweeper(giteaAdapter, provisioningStore), logger)
	} else {
		// Redacted by construction: key names only, never values.
		slog.Warn("post-api: GitProvider provisioning disabled — missing configuration",
			"missing", strings.Join(gitCfg.Missing, ", "),
			"effect", "no repositories or webhooks are provisioned; set the named variables and restart to enable")
	}

	// Authentication (T0101) + organizations (T0103): the /api/v1 subtree
	// is guarded by default — every state-changing request under it
	// requires a valid session + CSRF token unless it is an explicit
	// pre-auth route (login/signup). Product routes register on the same
	// apiMux inside the guard and inherit it structurally.
	authCfg, err := authnLoader().Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "post-api: authentication configuration error:\n%v\n", err)
		return exitConfig
	}
	// Audit (T0110): one store writes auth events and serves the Activity
	// reads; the state-changing stores append audit rows inside their own
	// transactions. The auth surface records through it best-effort (the
	// sessions live in Redis, there is no PostgreSQL transaction to join).
	auditStore := persistence.NewAuditStore(pool)
	authAPI := authhttp.New(authhttp.Deps{
		Users:      persistence.NewCredentialStore(pool),
		Sessions:   persistence.NewRedisSessionStore(redisClient),
		Limiter:    persistence.NewRedisRateLimiter(redisClient),
		OIDCClient: newOIDCClientOrNil(authCfg),
		Cfg:        *authCfg,
		Secure:     cfg.Layer == config.LayerProd,
		Audit:      auditStore,
	})
	// Product APIs (T0102+): one shared mux under one guard. The auth, profile,
	// organization and project surfaces all register here and inherit the
	// session/CSRF guard structurally — anonymous reads flow, writes are
	// 401/403 before routing.
	v1 := http.NewServeMux()
	authAPI.Register(v1)
	profileAPI := profilehttp.New(profilehttp.Deps{
		Profiles: persistence.NewProfileStore(pool),
	})
	profileAPI.Register(v1)
	orgStore := persistence.NewOrgStore(pool)
	orgAPI := orgshttp.New(orgshttp.Deps{Store: orgStore})
	// The bare path is registered alongside the subtree so requests to
	// /api/v1/organizations (create + list) hit the routes directly instead of
	// being redirected for a trailing slash.
	v1.Handle("/api/v1/organizations", orgAPI.Routes())
	v1.Handle("/api/v1/organizations/", orgAPI.Routes())
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store:         persistence.NewProjectStore(pool),
		Orgs:          orgStore,
		Authz:         authz.NewMatrixEngine(),
		ProvisionJobs: provisioningQueue,
	})
	v1.Handle("/api/v1/projects", projectAPI.Routes())
	v1.Handle("/api/v1/projects/", projectAPI.Routes())
	// Activity feeds (T0110): read-only GET routes on the same guarded v1
	// mux. Read authorization reuses the owning surfaces' service instances
	// (projectAPI.Service()/orgAPI.Service()), so a resource's activity is
	// exactly as visible as the resource itself.
	auditAPI := audithttp.New(audithttp.Deps{
		Store:    auditStore,
		Projects: audit.ProjectsReadGate(projectAPI.Service()),
		Orgs:     orgAPI.Service(),
	})
	auditAPI.Register(v1)
	// Progressive validation gates (T0207): the :validate endpoint runs one
	// gate over the branch's persisted snapshot and returns the full report.
	reg, err := schemareg.New()
	if err != nil {
		slog.Error("post-api: schema registry failed to load", "error", err)
		return exitRuntime
	}
	validationAPI := validationhttp.New(validationhttp.Deps{
		Validator: appvalidation.NewService(
			persistence.NewValidationSnapshotRepository(persistence.NewStateStore(pool)),
			rsgvalidation.NewValidator(reg),
		),
		// The report is as visible as its project: the same project-read
		// gate every other project read runs (T0106 read matrix).
		Projects: projectAPI.Service(),
	})
	validationAPI.Register(v1)
	// Scientific object domain services (T0208): research branches,
	// object create/version, typed relations and the object read. The rsg
	// service is the consuming API task the object/relation write ports
	// assigned authorization to: it resolves the caller's membership/role
	// through the project surface, evaluates the matrix (ActionCreateBranch
	// / ActionWriteScientificState) with the same require shape as the
	// project service, and commits every scientific-state write as one
	// state commit (gate draft) on the shared validation guard.
	stateStore := persistence.NewStateStore(pool)
	rsgSvc := rsg.NewService(rsg.Deps{
		Projects:  projectAPI.Service(),
		Branches:  branches.NewService(persistence.NewBranchStore(pool)),
		States:    states.NewService(stateStore, appvalidation.NewGuard(rsgvalidation.NewValidator(reg), persistence.NewValidationTxProbe())),
		Latest:    stateStore,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
	})
	rsgAPI := rsghttp.New(rsghttp.Deps{Service: rsgSvc})
	rsgAPI.Register(v1)
	mux.Handle("/api/v1/", authAPI.Guard(v1))

	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           observability.Middleware(logger)(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("post-api listening",
			"addr", cfg.Server.Addr, "version", version.Version, "cfg", cfg)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		slog.Info("post-api shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("shutdown failed", "error", err)
			return exitRuntime
		}
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("post-api exited", "error", err)
			return exitRuntime
		}
	}
	return exitOK
}

// newHealthHandler wires the health surface: liveness never touches a
// dependency; readiness probes PostgreSQL and Redis and reports the truth.
func newHealthHandler(pool *pgxpool.Pool, redisClient *redis.Client) http.Handler {
	return health.NewHandler("api",
		health.Probe{
			Name: "postgresql",
			Check: func(ctx context.Context) error {
				return pool.Ping(ctx)
			},
		},
		health.Probe{
			Name: "redis",
			Check: func(ctx context.Context) error {
				return redisClient.Ping(ctx).Err()
			},
		},
	)
}

// databaseDSN builds the pgx connection URL from the validated config.
// Reading the raw password here is the one legitimate use of a config.Secret:
// only code that actually opens the connection may call Raw/string forms
// (internal/config).
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

// authnLoader resolves the auth configuration environment (the loader is
// injectable so main_test can run without real env).
var authnLoader = func() authn.Loader { return authn.Loader{} }

// gitproviderLoader resolves the GitProvider configuration environment
// (injectable, same reason as authnLoader).
var gitproviderLoader = func() gitprovider.Loader { return gitprovider.Loader{} }

// newOIDCClientOrNil builds the provider client when OIDC is configured.
// The redirect URI is always derived from the callback request (the API's
// own origin is unknowable at startup), so the client's default stays
// empty; the callback handler passes the request-derived URI explicitly.
func newOIDCClientOrNil(cfg *authn.Config) authn.OIDCProvider {
	if cfg == nil || !cfg.OIDC.Enabled {
		return nil
	}
	return authn.NewOIDCClient(cfg.OIDC.Issuer, cfg.OIDC.ClientID,
		string(cfg.OIDC.ClientSecret), "")
}
