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
	"github.com/lichman0405/post/cmd/api/fileshttp"
	"github.com/lichman0405/post/cmd/api/gittokenshttp"
	"github.com/lichman0405/post/cmd/api/orgshttp"
	"github.com/lichman0405/post/cmd/api/policyhttp"
	"github.com/lichman0405/post/cmd/api/profilehttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/cmd/api/provenancehttp"
	"github.com/lichman0405/post/cmd/api/rsghttp"
	"github.com/lichman0405/post/cmd/api/schemaprofileshttp"
	"github.com/lichman0405/post/cmd/api/validationhttp"
	"github.com/lichman0405/post/internal/application/audit"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/schemaprofiles"
	"github.com/lichman0405/post/internal/application/states"
	appvalidation "github.com/lichman0405/post/internal/application/validation"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/config"
	"github.com/lichman0405/post/internal/events"
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

	// The canonical schema registry is loaded once, up front: both the
	// validation surface (T0207) and the push-ingestion webhook receiver
	// (T0305) classify against the same instance — a manifest is a known
	// scientific manifest under exactly one registry. A failure fails fast,
	// before any provisioning goroutine starts.
	reg, err := schemareg.New()
	if err != nil {
		slog.Error("post-api: schema registry failed to load", "error", err)
		return exitRuntime
	}

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
		// Branch ref sync (T0303): the second job type on the same loop and
		// queue, sharing the adapter (and its resolved service account
		// identity). The mapping rows are maintained by migration 00031's
		// triggers; this loop keeps the provider refs consistent with them
		// (create on branch insert, delete once merged/aborted).
		refStore := gitprovider.NewBranchRefStore(pool)
		refSyncer := gitprovider.NewBranchRefSyncer(giteaAdapter, refStore)
		provisioningLoop.Register(gitprovider.BranchRefJobType, newBranchRefSyncHandler(refSyncer))
		// Push webhook receiver (T0305): the delivery target T0301 registers
		// on every provisioned repository (POST_GITEA_WEBHOOK_URL). It sits
		// on the ROOT mux — outside the session/CSRF guard below — because
		// the provider is a machine, not a browser: the HMAC signature over
		// the raw body, verified against the per-repository secret in
		// git_repository_provisions, IS the authentication. Registered only
		// when provisioning is enabled: without a configured provider there
		// is nothing that could sign a delivery, and an unverified receiver
		// must not exist.
		ingestStore := gitprovider.NewPushIngestStore(pool)
		pushIngester := gitprovider.NewPushIngester(giteaAdapter, ingestStore, reg)
		mux.Handle("POST /api/v1/git/hooks/gitea",
			gitprovider.NewPushWebhookHandler(pushIngester, ingestStore))
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
		// Sweep branch refs left unsynced by an earlier run (a down Redis
		// at branch-creation time, a provider outage on the last attempt,
		// or close requests waiting for their deletion). Same policy and
		// timeout as the provisioning sweep.
		go func() {
			sweepCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			enqueuePendingBranchRefs(sweepCtx, refStore, provisioningQueue, logger)
		}()
	} else {
		// Redacted by construction: key names only, never values.
		slog.Warn("post-api: GitProvider provisioning disabled — missing configuration",
			"missing", strings.Join(gitCfg.Missing, ", "),
			"effect", "no repositories or webhooks are provisioned and files reads are disabled; set the named variables and restart to enable")
	}

	// Git ↔ RSG reconciliation (T0309): the periodic drift check between
	// the canonical store and the GitProvider (docs/16 §5 — any drift is a
	// high-severity alert). Runs unconditionally: without provider
	// configuration the pass still verifies the canonical-store dimensions
	// (state hashes, mappings); with it, the provider dimensions (refs,
	// repositories) join in. The reconciler never repairs — every finding
	// carries a proposal, nothing is applied. Same best-effort policy as
	// the protection sweep: a failed pass retries on the next tick.
	var reconcilerPort gitprovider.ReconcilerPort
	if gitCfg.ProvisioningEnabled() {
		reconcilerPort = gitprovider.NewGiteaAdapter(*gitCfg)
	}
	go runReconciliationSweep(ctx,
		gitprovider.NewReconciler(reconcilerPort, gitprovider.NewReconcilerStore(pool)), logger)

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
	// Scoped git tokens (T0304): the user-credential surface over the
	// internal Gitea. Like provisioning it needs provider configuration,
	// but its own gate: the admin credentials may be unset while
	// provisioning runs, and vice versa. The routes register either way —
	// disabled, they answer 503 naming the missing keys instead of a
	// misleading 404.
	gitTokensAPI := gittokenshttp.New(gittokenshttp.Deps{
		Projects: projectAPI.Service(),
		Access:   gitAccessService(gitCfg, pool),
		Audit:    auditStore,
		Missing:  gitCfg.UserAccessMissing,
	})
	gitTokensAPI.Register(v1)
	if !gitCfg.UserAccessEnabled() {
		// Redacted by construction: key names only, never values.
		slog.Warn("post-api: git user access disabled — missing configuration",
			"missing", strings.Join(gitCfg.UserAccessMissing, ", "),
			"effect", "git tokens cannot be issued or revoked; set the named variables and restart to enable")
	}
	// Read-only Files API (T0307): tree/preview/history/raw over the
	// provisioned repository, gated by the shared project read surface —
	// a project's files are exactly as visible as the project itself. The
	// reader needs the service token (all provider reads authenticate with
	// it); the routes register either way — disabled, they answer 503
	// naming the missing keys, like the git-token surface.
	filesAPI := fileshttp.New(fileshttp.Deps{
		Projects: projectAPI.Service(),
		Files:    filesReaderService(gitCfg, pool),
		Missing:  gitCfg.FilesMissing(),
	})
	filesAPI.Register(v1)
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
	// The registry instance is the one loaded above — shared with the
	// push-ingestion webhook receiver.
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
	// Project schema profiles (T0213): namespaced, versioned JSON Schema
	// extensions of the official base schemas. The persisted profile rows
	// are re-registered into the runtime registry at startup (LoadAll) —
	// the registry each validation run and each object create resolves
	// against, so profiles survive restarts. The same service instance is
	// shared with the RSG service below: the object create path resolves
	// schema refs against exactly the profiles these routes register.
	profileSvc := schemaprofiles.NewService(schemaprofiles.Deps{
		Store:    persistence.NewSchemaProfileStore(pool),
		Projects: projectAPI.Service(),
		Schemas:  reg,
	})
	// The profile load is a background job, not a startup gate: the API
	// must start (and report "not ready") while PostgreSQL is down. The
	// loader retries forever while the store is unreachable (ErrStore) and
	// returns ErrCorruption immediately when a persisted row is broken —
	// profileFatal turns that into a process exit, never a half-loaded
	// registry. Until the load completes, profile-dependent validation
	// fails closed (rsg.resolveSchemaRef refuses refs whose schema the
	// runtime registry does not hold yet).
	profileFatal := make(chan error, 1)
	go func() {
		if err := runSchemaProfileLoad(ctx, profileSvc, logger); err != nil && !errors.Is(err, context.Canceled) {
			profileFatal <- err
		}
	}()
	schemaprofilesAPI := schemaprofileshttp.New(schemaprofileshttp.Deps{Service: profileSvc})
	schemaprofilesAPI.Register(v1)
	rsgSvc := rsg.NewService(rsg.Deps{
		Projects:       projectAPI.Service(),
		Branches:       branches.NewService(persistence.NewBranchStore(pool)),
		States:         states.NewService(stateStore, appvalidation.NewGuard(rsgvalidation.NewValidator(reg), persistence.NewValidationTxProbe())),
		Latest:         stateStore,
		Objects:        persistence.NewScientificObjectStore(pool),
		Relations:      persistence.NewRelationStore(pool),
		Queries:        persistence.NewRSGQueryStore(pool),
		Profiles:       persistence.NewProfileStore(pool),
		SchemaProfiles: profileSvc,
		Authz:          authz.NewMatrixEngine(),
		Schemas:        reg,
		// The transactional outbox (T1001): every scientific-state write
		// records its domain events in the same transaction; cmd/worker
		// publishes them into research_events.
		Events: events.Recorder{},
	})
	rsgAPI := rsghttp.New(rsghttp.Deps{Service: rsgSvc})
	rsgAPI.Register(v1)
	// Provenance graph projection (T0505): read-only graph + lineage
	// routes over the rebuildable provenance_edges projection (migration
	// 00043). Reads run the same project visibility gate as every other
	// project read; the pgx adapter lives in provenancehttp because
	// T0505's scope excludes internal/persistence (L1, recorded in the
	// task result).
	provenanceAPI := provenancehttp.New(provenancehttp.Deps{
		Store: provenancehttp.NewProjectionStore(pool),
		Gate:  projectAPI.Service(),
	})
	provenanceAPI.Register(v1)
	// Organization/project policy (T0603): read/write routes for the
	// versioned governance policy, sharing the v1 guard. The production
	// adapters are the same pgx stores the org/project surfaces use.
	policyAPI := policyhttp.New(policyhttp.Deps{
		Store:    persistence.NewPolicyStore(pool),
		Orgs:     orgStore,
		Projects: persistence.NewProjectStore(pool),
	})
	policyAPI.Register(v1)
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
	case err := <-profileFatal:
		slog.Error("post-api: schema profile load failed permanently", "error", err)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if serr := srv.Shutdown(shutdownCtx); serr != nil {
			slog.Error("post-api: shutdown after schema profile load failure failed", "error", serr)
		}
		return exitRuntime
	}
	return exitOK
}

// schemaProfileLoadBackoff is the initial retry delay of the profile load
// loop (doubling up to a minute); a package variable so main_test can
// shrink it instead of waiting out real backoffs.
var schemaProfileLoadBackoff = 5 * time.Second

// runSchemaProfileLoad loads the persisted schema profiles into the
// runtime registry, retrying while the store is unreachable. Two error
// classes: ErrStore (database down — retry forever, the API keeps serving
// and /readyz reports the database truth) and ErrCorruption (a persisted
// row is broken — returned immediately so the caller fails fast rather
// than serving a registry that diverges from the database rows). A
// canceled context ends the loop (the process is shutting down).
func runSchemaProfileLoad(ctx context.Context, svc *schemaprofiles.Service, logger *slog.Logger) error {
	backoff := schemaProfileLoadBackoff
	for {
		err := svc.LoadAll(ctx)
		if err == nil {
			logger.Info("post-api: schema profiles loaded into the runtime registry")
			return nil
		}
		if errors.Is(err, schemaprofiles.ErrCorruption) {
			return err
		}
		logger.Warn("post-api: schema profile load failed — retrying",
			"error", err, "retry_in", backoff.String())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
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

// gitAccessService wires the T0304 user-credential service when the
// configuration is complete, nil otherwise (the routes answer 503 naming
// the missing keys — a disabled feature is a visible one).
func gitAccessService(cfg *gitprovider.Config, pool *pgxpool.Pool) *gitprovider.UserAccess {
	if !cfg.UserAccessEnabled() {
		return nil
	}
	return gitprovider.NewUserAccess(
		gitprovider.NewGiteaUserAccess(*cfg),
		gitprovider.NewUserAccessStore(pool),
		cfg.BaseURL)
}

// filesReaderService wires the T0307 read-only files service when the
// service token is configured, nil otherwise (the routes answer 503
// naming the missing keys — a disabled feature is a visible one).
func filesReaderService(cfg *gitprovider.Config, pool *pgxpool.Pool) *gitprovider.FilesReader {
	if len(cfg.FilesMissing()) > 0 {
		return nil
	}
	return gitprovider.NewFilesReader(
		gitprovider.NewGiteaAdapter(*cfg),
		gitprovider.NewUserAccessStore(pool))
}

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
