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

	"github.com/lichman0405/post/cmd/api/aborthttp"
	"github.com/lichman0405/post/cmd/api/assetshttp"
	"github.com/lichman0405/post/cmd/api/attestationhttp"
	"github.com/lichman0405/post/cmd/api/audithttp"
	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/conflicthttp"
	"github.com/lichman0405/post/cmd/api/discussionhttp"
	"github.com/lichman0405/post/cmd/api/evidencehttp"
	"github.com/lichman0405/post/cmd/api/explorehttp"
	"github.com/lichman0405/post/cmd/api/feedshttp"
	"github.com/lichman0405/post/cmd/api/fileshttp"
	"github.com/lichman0405/post/cmd/api/forkshttp"
	"github.com/lichman0405/post/cmd/api/freezehttp"
	"github.com/lichman0405/post/cmd/api/gittokenshttp"
	"github.com/lichman0405/post/cmd/api/inboxhttp"
	"github.com/lichman0405/post/cmd/api/knowledgehttp"
	"github.com/lichman0405/post/cmd/api/mergegit"
	"github.com/lichman0405/post/cmd/api/mergehttp"
	"github.com/lichman0405/post/cmd/api/milestonehttp"
	"github.com/lichman0405/post/cmd/api/notificationshttp"
	"github.com/lichman0405/post/cmd/api/orgshttp"
	"github.com/lichman0405/post/cmd/api/policyhttp"
	"github.com/lichman0405/post/cmd/api/profilehttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/cmd/api/provenancehttp"
	"github.com/lichman0405/post/cmd/api/pullrequestshttp"
	"github.com/lichman0405/post/cmd/api/releasehttp"
	"github.com/lichman0405/post/cmd/api/researchprofilehttp"
	"github.com/lichman0405/post/cmd/api/reviewhttp"
	"github.com/lichman0405/post/cmd/api/rsghttp"
	"github.com/lichman0405/post/cmd/api/schemaprofileshttp"
	"github.com/lichman0405/post/cmd/api/searchhttp"
	"github.com/lichman0405/post/cmd/api/subscriptionshttp"
	"github.com/lichman0405/post/cmd/api/templateshttp"
	"github.com/lichman0405/post/cmd/api/validationhttp"
	"github.com/lichman0405/post/cmd/api/webhookshttp"
	"github.com/lichman0405/post/internal/application/aborts"
	"github.com/lichman0405/post/internal/application/assetmetadata"
	"github.com/lichman0405/post/internal/application/assetpublish"
	"github.com/lichman0405/post/internal/application/attestations"
	"github.com/lichman0405/post/internal/application/audit"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	appcontribution "github.com/lichman0405/post/internal/application/contribution"
	"github.com/lichman0405/post/internal/application/dependencyimpact"
	"github.com/lichman0405/post/internal/application/diffs"
	"github.com/lichman0405/post/internal/application/discussions"
	"github.com/lichman0405/post/internal/application/evidencegraph"
	"github.com/lichman0405/post/internal/application/feeds"
	"github.com/lichman0405/post/internal/application/forks"
	"github.com/lichman0405/post/internal/application/knowledgepublish"
	"github.com/lichman0405/post/internal/application/mainfreeze"
	"github.com/lichman0405/post/internal/application/manifests"
	"github.com/lichman0405/post/internal/application/merge"
	"github.com/lichman0405/post/internal/application/milestones"
	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/prchecks"
	"github.com/lichman0405/post/internal/application/prdiff"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/application/releases"
	"github.com/lichman0405/post/internal/application/researchcontext"
	"github.com/lichman0405/post/internal/application/resolutions"
	"github.com/lichman0405/post/internal/application/responsibilities"
	"github.com/lichman0405/post/internal/application/reviews"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/schemaprofiles"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/application/templates"
	appvalidation "github.com/lichman0405/post/internal/application/validation"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/config"
	"github.com/lichman0405/post/internal/contribution"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/gitprovider"
	"github.com/lichman0405/post/internal/health"
	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/rsg/integrity"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
	"github.com/lichman0405/post/internal/search/answer"
	"github.com/lichman0405/post/internal/search/ranking"
	"github.com/lichman0405/post/internal/search/retrieval"
	"github.com/lichman0405/post/internal/security"
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
	// The two provider-side pieces the merge command needs (T0409): the
	// adapter whose pull-request merge is the ONLY way main advances, and the
	// service login the ref guard accepts for that update. They are resolved
	// in this block (and carried out of it) because they come from the
	// provisioning configuration; the merge service itself is assembled later,
	// with the other governance commands.
	var (
		mergeAdapter *gitprovider.GiteaAdapter
		mergeLogin   string
		// The external-contribution path's provider halves (T0410): the
		// provisioner that makes the fork project's repository exist, and the
		// importer that copies the parent's commit into it. Carried out of
		// this block for the same reason as the merge adapter — they come
		// from the provisioning configuration — because the fork service is
		// assembled later, with the other application services. Both stay nil
		// without provisioning; the fork route itself still registers (it is
		// part of the product API) but answers 503 naming the missing keys,
		// so a disabled deployment never writes a project row and never calls
		// a nil adapter.
		forkProvisioner forks.RepoProvisioner
		forkImporter    forks.ContentImporter
	)
	if gitCfg.ProvisioningEnabled() {
		provisioningStore := gitprovider.NewProvisionStore(pool)
		giteaAdapter := gitprovider.NewGiteaAdapter(*gitCfg)
		mergeAdapter = giteaAdapter
		// The controlled service identity (docs/16 §3): the account the
		// adapter authenticates as, and the login EnsureMainProtection
		// prepends to main's merge whitelist — so the guard's accepted actor
		// and the branch rule's only permitted merger are the same identity by
		// construction, not by two configuration keys agreeing.
		//
		// Resolved once, with a bounded timeout: a provider that is slow to
		// answer must not hold startup open (the same lazy-dependency policy
		// PostgreSQL's pool and the profile load follow). If it cannot be
		// resolved the login stays empty and the ref guard fails closed — the
		// command commits the platform truth and records the Git step as
		// unfinished rather than accepting an update it cannot attribute.
		ownerCtx, cancelOwner := context.WithTimeout(ctx, gitIdentityTimeout)
		owner, ownerErr := giteaAdapter.Owner(ownerCtx)
		cancelOwner()
		if ownerErr != nil {
			// Redacted by construction: the error names the provider, never
			// the token.
			slog.Warn("post-api: could not resolve the Git service identity — merges record an unfinished Git step until it resolves",
				"error", ownerErr)
		} else {
			mergeLogin = owner
		}
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
		// The fork halves (T0410). The importer takes the SAME ingester the
		// webhook is served by, on purpose: the copy the fork lands is
		// inspected by the one push-inspection implementation (T0305), so
		// the fork's branch is judged by the same rules as any other push.
		forkProvisioner = provisioner
		forkImporter = gitprovider.NewForkImporter(giteaAdapter, gitprovider.NewForkImportStore(pool), pushIngester)
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
	// Edge hardening (T1106, docs/23 §7): the response-header set and the
	// shared rate-limit budgets. Loaded fail-closed like every other
	// configuration block — an unparseable budget refuses to start rather
	// than silently running with a different policy than the operator
	// wrote. The budgets are logged on one line at startup so the running
	// policy is visible without reading the loader.
	securityCfg, err := securityLoader().Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "post-api: security configuration error:\n%v\n", err)
		return exitConfig
	}
	// One limiter instance, two consumers: the login service (per-email and
	// per-IP brute-force budgets) and the edge middleware (the coarse
	// per-class budgets). They share the Redis keyspace but never a bucket —
	// the middleware namespaces its keys "edge:".
	rateLimiter := persistence.NewRedisRateLimiter(redisClient)
	// Audit (T0110): one store writes auth events and serves the Activity
	// reads; the state-changing stores append audit rows inside their own
	// transactions. The auth surface records through it best-effort (the
	// sessions live in Redis, there is no PostgreSQL transaction to join).
	auditStore := persistence.NewAuditStore(pool)
	authAPI := authhttp.New(authhttp.Deps{
		Users:      persistence.NewCredentialStore(pool),
		Sessions:   persistence.NewRedisSessionStore(redisClient),
		Limiter:    rateLimiter,
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
	// Research Profile / Organization Profile (T0808): the two public read
	// models docs/05 §4 lists as network entity pages. Registered on the same
	// guarded v1 mux as everything else; both routes are GETs, and the guard
	// lets anonymous reads through, so the two pages are public while every
	// write in the subtree still needs a session + CSRF token.
	researchProfileAPI := researchprofilehttp.New(researchprofilehttp.Deps{
		Reader: persistence.NewResearchProfileStore(pool),
	})
	researchProfileAPI.Register(v1)
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
	// Signed webhooks (T1006): the endpoint registry + delivery log. The
	// worker owns delivery itself (events.FanOut / events.Deliverer over
	// the same database); the API only manages what the owner controls.
	webhooksAPI := webhookshttp.New(webhookshttp.Deps{Store: events.NewWebhookStore(pool)})
	v1.Handle("/api/v1/webhooks", webhooksAPI.Routes())
	v1.Handle("/api/v1/webhooks/", webhooksAPI.Routes())
	// Follow/watch (T1002): a user's subscriptions to a project, asset,
	// knowledge object, person or organization, with their event filters and
	// channels. The API only manages the subscription; the worker's
	// events.SubscriptionFanOut decides delivery, re-resolving the
	// subscriber's access to the target against live state for every event —
	// so a permission change stops the flow without anyone touching the
	// subscription.
	subscriptionsAPI := subscriptionshttp.New(subscriptionshttp.Deps{Store: events.NewSubscriptionStore(pool)})
	v1.Handle("/api/v1/subscriptions", subscriptionsAPI.Routes())
	v1.Handle("/api/v1/subscriptions/", subscriptionsAPI.Routes())
	// Research inbox (T1003): the web channel's read surface over the
	// deliveries the fan-out above writes — aggregated into entries, with
	// read/unread and a deep link to each entry's target. It reads the
	// same store as the subscription API (one owner of
	// subscription_deliveries), so the rows it lists and the rows the
	// fan-out withdraws can never disagree.
	inboxAPI := inboxhttp.New(inboxhttp.Deps{Store: events.NewSubscriptionStore(pool)})
	v1.Handle("/api/v1/inbox", inboxAPI.Routes())
	v1.Handle("/api/v1/inbox/", inboxAPI.Routes())
	// Email notification settings (T1005): how often the account's email
	// notifications arrive (immediate/daily/weekly). The setting is the
	// account's own — the routes take no user id — and the worker's digest
	// sender reads the same row through the same store.
	notificationsAPI := notificationshttp.New(notificationshttp.Deps{Store: events.NewNotificationStore(pool)})
	v1.Handle("/api/v1/notifications/preferences", notificationsAPI.Routes())
	v1.Handle("/api/v1/notifications/", notificationsAPI.Routes())
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
	// Pull requests (T0402/T0403): the per-project PR read surface plus the
	// machine integrity review (internal/rsg/integrity). The check service
	// assembles the PR's snapshot through the same stores the validation
	// gate reads from — reads only, nothing stored; re-running over the
	// same PR derives the same report (the pins are fixed, the rows
	// append-only).
	// The three-way diff use case (T0401): one instance serves both the PR
	// page's diff read (T0408, through the prdiff resolution below) and the
	// conflict resolution surface (T0407) — the engine is stateless and the
	// ports are the same read stores. The third port is the proposal read
	// (T0817) that lets the SOURCE side of a triple live in another project
	// when a pull request of this one proposes it — the external fork's
	// shape, and the only foreign source any of these callers may read.
	pullRequestStore := persistence.NewPullRequestStore(pool)
	diffSvc := diffs.NewService(stateStore, persistence.NewManifestStore(pool), pullRequestStore)
	// One check service, two consumers: the PR page's review read and the
	// merge command's server-side re-run (docs/22 §7). The merge must run the
	// SAME review the human read — a second instance would be a second
	// assembly of the snapshot.
	checksSvc := prchecks.NewService(prchecks.Deps{
		PRs:      persistence.NewPullRequestStore(pool),
		Projects: persistence.NewProjectStore(pool),
		States:   stateStore,
		// The branch's own head is the boundary of a chain with no
		// states of its own — read from the branch row, never from the
		// PR under review.
		Branches: persistence.NewBranchStore(pool),
		Manifest: persistence.NewManifestStore(pool),
		Policies: persistence.NewPolicyStore(pool),
		Engine:   integrity.New(reg),
		// The PR first screen's dependency impact line (T1007): the same
		// analysis's read surface, with the caller's own access applied per
		// affected project. The WRITE side of the analysis is not here — it
		// runs in cmd/worker off the published event log, because 「上游变更
		// 触发」 names no user a request could authenticate (docs/19 §3), and
		// no route in this binary triggers it.
		Impact: dependencyimpact.NewService(
			persistence.NewDependencyImpactStore(pool),
			dependencyimpact.ProjectsGate(projectAPI.Service()),
		),
	})
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
	// One state-commit service, shared by every path that lands a state
	// transition (the RSG writes and the merge): the gate is the same
	// instance, so a transition cannot be validated one way here and another
	// way there.
	stateSvc := states.NewService(stateStore, appvalidation.NewGuard(rsgvalidation.NewValidator(reg), persistence.NewValidationTxProbe()))
	branchSvc := branches.NewService(persistence.NewBranchStore(pool))
	rsgSvc := rsg.NewService(rsg.Deps{
		Projects:       projectAPI.Service(),
		Branches:       branchSvc,
		States:         stateSvc,
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
		// The external fork lineage (T0804): write_scientific_state is
		// own_fork_only for an authenticated non-member, and this is the
		// read that resolves it — a write is permitted exactly when the
		// project is the actor's own fork (project_forks, 00086). Wired
		// unconditionally: without it the conditional path fails closed,
		// which would refuse a non-member's fork write even in their own
		// fork.
		ForkGate: persistence.NewForkStore(pool),
		// The evidence network (T0806): the write side of docs/10 §7 — the
		// evidence assertion command resolves both version pins through this
		// adapter, refuses a target that is not published (and a cross-project
		// assertion on a publication whose audience is not the network), and
		// records the row inside the state commit. It also serves the read
		// the published knowledge document renders. Optional by contract: a
		// nil Evidence makes the command fail closed.
		Evidence: persistence.NewEvidenceStore(pool),
	})
	// The RSG surface is mounted further down, after the two graph
	// projections below: its object detail page renders their tabs, so it is
	// constructed with the very stores their JSON routes read through (T0507
	// — one adapter per graph, one answer per graph).
	// Pull requests (T0402, T0408; the open route is T0410). One command
	// instance serves the reads and the open: `Create` is the same
	// pullrequests.Service the list and detail endpoints read through, so a
	// proposal opened over the route and one read back from the list are the
	// same rows through the same adapter.
	//
	// The open route's command is the external-contribution service
	// (T0804/T0410). Opening a pull request is its own governance action —
	// specs/policies/permissions-matrix.csv gives it its own cell
	// (`open_pr`: deny / allow_from_fork / deny / allow / allow / allow /
	// allow) — and that service is the one place the cell is resolved
	// against a real project, a real membership and, for a non-member, the
	// fork lineage. It proposes through the very same pull-request command
	// below, so an external contribution is an ordinary proposal
	// (specs/api/openapi.yaml: "Open pull request with RSG diff").
	//
	// Assembled HERE, after the RSG service, because the fork command
	// creates the fork's branch through the canonical RSG service — the fork
	// branch is an ordinary branch with an ordinary genesis state. The two
	// provider ports (Repos, Imports) are the ones carried out of the
	// provisioning block above and are nil without provisioning, exactly like
	// every other provider dependency: the only method that reaches them is
	// Fork, which is the route below.
	prSvc := pullrequests.NewService(persistence.NewPullRequestStore(pool))
	forksSvc := forks.NewService(forks.Deps{
		Projects:     projectAPI.Service(),
		Branches:     branchSvc,
		BranchWriter: rsgSvc,
		Forks:        persistence.NewForkStore(pool),
		Repos:        forkProvisioner,
		Imports:      forkImporter,
		PullRequests: prSvc,
		// Reviews is the same pull-request service read through its review
		// surface: sending an EXISTING proposal into review (T0411) is the
		// pull-request state machine's move, driven from here because the
		// authorization for it is the open_pr cell this service already
		// resolves. Without it the route would fail closed with 503 — the
		// service refuses rather than moving a row nobody authorized.
		Reviews: prSvc,
		Authz:   authz.NewMatrixEngine(),
	})
	pullrequestsAPI := pullrequestshttp.New(pullrequestshttp.Deps{
		PullRequests: prSvc,
		Create:       forksSvc,
		// Sending a proposal into review (T0411) is a governance action,
		// so it is authorized where every other open_pr decision is:
		// the forks service resolves the SAME cell against the PR's
		// project (including the non-member's fork lineage) and drives
		// the pull-request command that moves the row.
		Review: forksSvc,
		Checks: checksSvc,
		// The PR's Research State Diff (T0408): the PR's own fixed base,
		// its proposed head and the target branch's current head, computed
		// by the T0401 engine. The base is never re-derived from the target
		// — it does not drift as main advances.
		Diff: prdiff.NewService(
			persistence.NewPullRequestStore(pool),
			persistence.NewBranchStore(pool),
			diffSvc,
		),
		// PRs and their check reports are exactly as visible as their
		// project: the same project-read gate every other project read
		// runs (T0106 read matrix).
		Projects: projectAPI.Service(),
	})
	pullrequestsAPI.Register(v1)
	// Fork surface (T0814): the external-contribution entry —
	// POST /api/v1/projects/{projectId}/forks. It is the entry of the same
	// graph the open route above consumes: a non-member's fork is what
	// makes their later proposal legitimate (open_pr = allow_from_fork),
	// and the lineage row this route writes is the fact that the open route
	// reads back. The service instance is shared with the open route on
	// purpose — one command, whose idempotent repeat and whose lineage are
	// the same rows for both.
	//
	// The project reader is the same reader every project read uses, so the
	// route's public-parent condition (a fork of a non-PUBLIC parent may
	// not be created public) is answered by the one visibility rule rather
	// than a second copy of it.
	forksAPI := forkshttp.New(forkshttp.Deps{
		Forks:    forksSvc,
		Projects: projectAPI.Service(),
		Missing:  gitCfg.Missing,
	})
	forksAPI.Register(v1)
	// Provenance graph projection (T0505): read-only graph + lineage
	// routes over the rebuildable provenance_edges projection (migration
	// 00043). Reads run the same project visibility gate as every other
	// project read; the pgx adapter lives in provenancehttp because
	// T0505's scope excludes internal/persistence (L1, recorded in the
	// task result).
	provenanceStore := provenancehttp.NewProjectionStore(pool)
	provenanceAPI := provenancehttp.New(provenancehttp.Deps{
		Store: provenanceStore,
		Gate:  projectAPI.Service(),
	})
	provenanceAPI.Register(v1)
	// Evidence graph projection (T0506): the read over evidence_assertions
	// grouped by the exact target version each assertion pins, plus the
	// hypothesis page's two separate sections. It is not the surface above
	// and shares nothing with it — different table (evidence_assertions, not
	// the rebuildable provenance_edges projection), different relation
	// semantics (why this evidence bears on that proposition, not where it
	// came from: CLAUDE.md §9 invariant 10), and its own route prefix. Reads
	// run the same project visibility gate as every other project read; the
	// per-target query is the one the schema already carried.
	evidenceGraphSvc := evidencegraph.New(evidencegraph.Deps{
		Objects:    persistence.NewScientificObjectStore(pool),
		Assertions: persistence.NewEvidenceGraphStore(pool),
		Relations:  persistence.NewRelationStore(pool),
	})
	evidenceGraphAPI := evidencehttp.New(evidencehttp.Deps{
		Service: evidenceGraphSvc,
		Gate:    projectAPI.Service(),
	})
	evidenceGraphAPI.Register(v1)
	// Scientific Object Detail surface (T0210), mounted here so its graph
	// tabs render the two projections above through the SAME readers their
	// JSON routes serve (T0507): the page and the API cannot disagree about
	// a project's provenance or evidence, because there is one adapter per
	// graph and both go through it. Read-only, no write verb of its own.
	rsgAPI := rsghttp.New(rsghttp.Deps{
		Service:    rsgSvc,
		Provenance: provenanceStore,
		Evidence:   evidenceGraphSvc,
	})
	rsgAPI.Register(v1)
	// Scientific Conflict Resolution surface (T0407): the conflict view
	// read (report + evidence + recorded decisions) and the resolution
	// plan write over the three-way base/source/target triple. The reads
	// run the same project read gate every other project read runs; the
	// writes authorize through the same membership + matrix shape the RSG
	// write path uses. The resolution store is the task-scoped pgx adapter
	// (internal/application/resolutions, see its package doc).
	resolutionSvc := resolutions.NewService(
		diffSvc,
		resolutions.NewPGStore(pool),
		projectAPI.Service(),
		authz.NewMatrixEngine(),
	)
	conflictAPI := conflicthttp.New(conflicthttp.Deps{
		Viewer:   resolutionSvc,
		Saver:    resolutionSvc,
		Projects: projectAPI.Service(),
	})
	conflictAPI.Register(v1)
	// Organization/project policy (T0603): read/write routes for the
	// versioned governance policy, sharing the v1 guard. The production
	// adapters are the same pgx stores the org/project surfaces use.
	policyAPI := policyhttp.New(policyhttp.Deps{
		Store:    persistence.NewPolicyStore(pool),
		Orgs:     orgStore,
		Projects: persistence.NewProjectStore(pool),
	})
	policyAPI.Register(v1)
	// Scientific responsibility and Research Owners routing (T0604,
	// docs/04 §3): the project's routing rules and responsibility
	// assignments (migration 00084), the resolver the conditional
	// submit_scientific_review verdict hangs on, and the required-review
	// calculation the review projection evaluates. The labels and the
	// rules are project data; holding one grants no access — internal/authz
	// never reads them.
	responsibilitySvc := responsibilities.NewService(responsibilities.Deps{
		Rules:    persistence.NewResponsibilityStore(pool),
		Projects: persistence.NewProjectStore(pool),
		Members:  projectAPI.Service(),
		PRs:      persistence.NewPullRequestStore(pool),
		Branches: persistence.NewBranchStore(pool),
		// The same diff use case the PR page reads: what a proposal
		// changes is what the routing applies to.
		Diffs: prdiff.NewService(
			persistence.NewPullRequestStore(pool),
			persistence.NewBranchStore(pool),
			diffSvc,
		),
		Policies:  persistence.NewPolicyStore(pool),
		Evaluator: policy.NewRuleEvaluator(),
	})
	// PR reviews (T0404/T0604): per-dimension scientific/integrity review
	// submissions, plus the list read the PR page's review section renders
	// (T0408). Authorization of the submission runs the
	// submit_scientific_review matrix row over the projects membership
	// gate, and the conditional verdict is resolved by the Research Owners
	// resolver above (allowed for a member holding a responsibility in the
	// project, refused otherwise — never permission by default). The
	// submission carries the required-review calculation into the store,
	// which advances the PR review_required -> approved -> merge_ready when
	// the recorded reviews satisfy it (docs/43; T0409's merge accepts only
	// merge_ready). The list read runs the same project read gate every
	// other project read runs.
	reviewSvc := reviews.NewService(reviews.Deps{
		Repo:           persistence.NewReviewStore(pool),
		Projects:       projectAPI.Service(),
		Authz:          authz.NewMatrixEngine(),
		Responsibility: responsibilitySvc,
		Routing:        responsibilitySvc,
	})
	reviewAPI := reviewhttp.New(reviewhttp.Deps{
		Service:  reviewSvc,
		Projects: projectAPI.Service(),
	})
	reviewAPI.Register(v1)
	// Immutable releases (T0606): the release command composes the T0605
	// manifest builder with its own authorization (ActionCreateRelease),
	// the policy in force (pinned by id), the server-side release gate
	// and the append-only release store (row + idempotency ledger + audit
	// + release.published event, one transaction). The read routes are
	// exactly as visible as the project (projectAPI.Service()); the
	// create is the only write — no update/delete route exists.
	releaseStore := persistence.NewReleaseStore(pool)
	policyStore := persistence.NewPolicyStore(pool)
	branchStore := persistence.NewBranchStore(pool)
	releaseBuilder := releases.NewService(
		stateStore,
		manifests.NewService(stateStore, persistence.NewManifestStore(pool)),
		persistence.NewProjectStore(pool),
		branchStore,
		policyStore,
		releaseStore,
		reg,
	)
	releaseCommand := releases.NewCommand(
		releaseBuilder,
		persistence.NewProjectStore(pool),
		policyStore,
		branchStore,
		stateStore,
		appvalidation.NewService(
			persistence.NewValidationSnapshotRepository(stateStore),
			rsgvalidation.NewValidator(reg),
		),
		releaseStore,
		authz.NewMatrixEngine(),
	)
	releaseAPI := releasehttp.New(releasehttp.Deps{
		Command:  releaseCommand,
		Projects: projectAPI.Service(),
	})
	releaseAPI.Register(v1)
	// Publication impact preview (T0704): the read-only half of docs/23
	// §4's highest-risk operation — one route, POST
	// .../assets:publish-preview, that says who would see what if the
	// proposed publish were executed. The pure model lives in
	// internal/assets (the same gate the publish command runs); the state
	// reader is internal/persistence.AssetStateStore (moved there by T0705,
	// which is the second caller), over the canonical queries of
	// internal/persistence/queries/asset_preview.sql. The preview writes
	// nothing; the publish itself is the route beside it.
	//
	// Publication governance (T0705): POST .../assets:publish, the write
	// docs/23 §4 calls the platform's highest-risk operation. The command
	// owns every decision it needs — the actor's membership class against
	// the permission matrix (publish_private_to_public: only the project
	// owner's cell is `allow` in V1; a maintainer's is `conditional`, and
	// an unresolved condition is a refusal, internal/authz default deny,
	// issue #237), the policy in force (read through the owning service and
	// evaluated through the typed rule surface, exactly as the merge does),
	// the server-side re-run of the impact preview, and the gate itself —
	// and the store runs them over ONE transaction, so the version row, its
	// create (when the publish makes the asset), the idempotency ledger
	// entry, the audit row and the research event commit together or not at
	// all.
	publishCommand := assetpublish.NewCommand(assetpublish.Deps{
		Members:  persistence.NewProjectStore(pool),
		Policies: policyAPI.Service(),
		Rules:    policy.NewRuleEvaluator(),
		Store:    persistence.NewAssetPublishStore(pool, rsgvalidation.NewValidator(reg)),
		Authz:    authz.NewMatrixEngine(),
	})
	// Asset fork/derive (T0708): POST .../assets:derive, the write docs/11 §5
	// defines as "创建新的 Asset/Object identity，保留 lineage". The command
	// decides the same authorization a publish does (the TARGET project's
	// membership class against publish_private_to_public — no new action, no
	// matrix edit), and the store's transaction adds the two decisions that
	// are about the PARENT version: whether this caller may read it at all
	// (the asset page's own ruler: a public project, or a member of it) and
	// what its stored rights declaration says about derivatives. Both are
	// re-made inside the transaction, beside the impact preview the publish
	// also runs, and the new asset row, its version row, the lineage edge,
	// the idempotency ledger entry, the audit row (which carries the rights
	// verdict and the caller's confirmation) and the research event commit
	// together or not at all.
	//
	// The membership adapter is the same *persistence.ProjectStore the
	// publish above is wired over, and it is handed to the STORE as well as
	// to the command: the parent read gate inside the transaction asks the
	// same question the authorization outside it does, and there is one
	// implementation of "is this caller a member" in this build.
	projectStore := persistence.NewProjectStore(pool)
	deriveCommand := assets.NewDeriveCommand(assets.DeriveDeps{
		Members:  projectStore,
		Policies: policyAPI.Service(),
		Rules:    policy.NewRuleEvaluator(),
		Store:    persistence.NewAssetDeriveStore(pool, rsgvalidation.NewValidator(reg), projectStore),
		Authz:    authz.NewMatrixEngine(),
	})
	assetsAPI := assetshttp.New(assetshttp.Deps{
		State:    assetshttp.NewPostgresStateStore(pool),
		Projects: projectAPI.Service(),
		Publish:  publishCommand,
		Derive:   deriveCommand,
		// The asset hub's reads (T0709). Pages is the read-only page reader;
		// Members is the SAME project service the gate above is — the
		// membership question is one the project surface already answers
		// (GetMembership re-runs its own read gate first), and a second
		// implementation of "is this caller a member" would be a second
		// answer to it.
		Pages:   persistence.NewAssetPageStore(pool),
		Members: projectAPI.Service(),
		// The project-side dependency read (T0707): the other end of the
		// same asset_dependencies table the publish above writes, keyed by
		// the project instead of the asset.
		Dependencies: persistence.NewProjectDependencyStore(pool),
		// The asset metadata revision (T0706). docs/11 §4 makes an asset's
		// description/keywords/cover/contact/documentation independently
		// revisable with the audit kept and no new scientific version; the
		// command revises the research_assets row in place and the store
		// writes the audit_log row in the SAME transaction, under the asset
		// row lock, with the before/after pair read from the row it is about
		// to overwrite. The role gate is the default-deny maintainer rule
		// the project settings surface uses — the permission matrix has no
		// asset-metadata row, and specs/ is not a Worker's to edit.
		Metadata: assetmetadata.NewCommand(assetmetadata.Deps{
			Members: persistence.NewProjectStore(pool),
			Store:   persistence.NewAssetMetadataStore(pool),
		}),
	})
	assetsAPI.Register(v1)
	// Knowledge publication (T0805): the missing publish path for
	// knowledge_publications — the table has existed since migration 00010
	// with no writer, while the event name
	// (knowledge.version_published, specs/events/event-types.yaml) and the
	// public read route (GET /knowledge/{knowledgeId}) were already in
	// place. The pair mirrors the asset publish step for step: the preview
	// is a PROPOSAL (specs/mcp/tools.json: knowledge.publish_preview, args
	// knowledge_version_ref + rights) and the publish is the human
	// governance action (publish_private_to_public in the permission
	// matrix: only the owner's cell is `allow` in V1, a maintainer's is
	// `conditional` and an unresolved condition is a refusal — issue #237).
	//
	// 发布不等于公开: publishing records a STATE and widens nothing. The
	// visibility axis is the one that already existed,
	// scientific_object_versions.visibility_policy_id (migration 00005),
	// and it is the version's OWN axis that the public read judges by —
	// not the owning project's preset (knowledgepublish.AudienceFor).
	//
	// The review middle cell of docs/43's publication state machine
	// (private candidate → publication_review → published) is the EXISTING
	// research-PR review record: the same ListReleaseReviews lineage read
	// the release gate makes, re-run inside the publish transaction, so
	// there is no path to `published` that did not go through review.
	knowledgePublishStore := persistence.NewKnowledgePublishStore(pool)
	knowledgePublishCommand := knowledgepublish.NewCommand(knowledgepublish.Deps{
		Members: persistence.NewProjectStore(pool),

		Store: knowledgePublishStore,
		Authz: authz.NewMatrixEngine(),
	})
	knowledgeAPI := knowledgehttp.New(knowledgehttp.Deps{
		Publish: knowledgePublishCommand,
		Read:    knowledgePublishStore,
		// The read gate and the membership read are the SAME project
		// service the asset page uses: GetMembership re-runs the project
		// read gate first, and a second implementation of "is this caller
		// a member" would be a second answer to it.
		Projects: projectAPI.Service(),
		Members:  projectAPI.Service(),
		// The published version's origin/network evidence state (T0806),
		// served by the same read the evidence write path uses: one adapter
		// answers both "may this assertion land" and "what does this
		// published version carry", so the two cannot disagree about what the
		// rows say.
		Evidence: persistence.NewEvidenceStore(pool),
	})
	knowledgeAPI.Register(v1)
	// Private evidence / public attestation (T0812): the governance write
	// that lets a project state, in public, that it validated a PUBLIC
	// version somebody else's project published — "we validated this, this
	// way, and the result was this" — without the private work underneath
	// it coming along. docs/12 §2 逐字: "Private Project：project/RSG/private
	// blobs 默认不可见；可显式 Publish Asset/Knowledge/Attestation", and
	// docs/22 §28 lists create attestation among the high-risk commands.
	//
	// The privacy is STRUCTURAL, not a rendering rule (migration 00120):
	// the table has no column a reasoning note, a citation or an excerpt
	// could be written into, and the public read
	// (ResolvePublicAttestation) has no column for the attesting project,
	// the basis state or the internal review — so the projection cannot
	// disclose what it was never handed. What it publishes is the
	// attestation itself and nothing else: the attesting project's
	// visibility, the basis state's visibility and the visibility of
	// anything in them are all untouched (发布不等于公开, L3-20260916-1,
	// from the other end).
	//
	// The matrix row is the same one the publication evaluates —
	// publish_private_to_public — and the human backstop in front of it is
	// the command's own: an agent may prepare an attestation and may not
	// issue one.
	attestationStore := persistence.NewAttestationStore(pool)
	attestationCommand := attestations.NewCommand(attestations.Deps{
		Members: persistence.NewProjectStore(pool),
		Store:   attestationStore,
		Authz:   authz.NewMatrixEngine(),
	})
	attestationAPI := attestationhttp.New(attestationhttp.Deps{
		Attest: attestationCommand,
		Read:   attestationStore,
	})
	attestationAPI.Register(v1)
	// The Explore index (T0802): the six dimensions of docs/05 §6 in one
	// anonymous read. Three of its six sections are the platform's EXISTING
	// public reads, not new ones — the public project list, the asset hub's
	// browse list (the same BuildBrowse that renders /assets) and the
	// open-network contribution view — so this surface adds no disclosure
	// rule of its own: it aggregates answers other surfaces already give and
	// keeps the newest rows of each (internal/application/explore).
	//
	// The remaining three (published knowledge, people, organizations) get
	// their reads from explorehttp's own store; the contribution service is
	// constructed here because the open-network view of an opportunity is
	// the contribution application service's answer, and nothing else on
	// this process wires it yet.
	exploreAPI := explorehttp.New(explorehttp.Deps{
		Reader: explorehttp.Sources{
			ProjectSource:      explorehttp.ProjectSource{Service: projectAPI.Service()},
			AssetSource:        explorehttp.AssetSource{Pages: persistence.NewAssetPageStore(pool)},
			ContributionSource: explorehttp.ContributionSource{Service: appcontribution.NewService(contribution.NewOpportunityStore(pool))},
			Store:              explorehttp.NewStore(pool),
		},
	})
	exploreAPI.Register(v1)
	// Public syndication feeds (T1004, docs/18 §4): the three anonymous
	// RSS/Atom routes — a project's, an asset's and a knowledge object's
	// published output — on the same guarded v1 mux, where a read flows
	// unauthenticated (未登录可订阅) and the model
	// (internal/application/feeds) decides what may be rendered. A private
	// project's feed does not exist at all: BuildFeed answers the same 404
	// for "not public", "unknown" and "nothing published".
	//
	// The base for every absolute link in a document is the configured web
	// origin (POST_WEB_ORIGIN) — the pages a feed entry points at are the
	// web app's — and never the request's Host, which a client controls. A
	// deployment whose origin is unusable disables the surface rather than
	// serving documents with broken links: the routes register either way
	// and answer 503 naming the deployment problem (a disabled feature is a
	// visible one, like the git-token and files surfaces above).
	feedsAPI := feedshttp.New(feedshttp.Deps{
		Service: feedService(persistence.NewFeedStore(pool), authCfg.WebOrigin, logger),
	})
	feedsAPI.Register(v1)
	// Official project templates (T0214): the catalog and the
	// create-from-template path. The templates service orchestrates the
	// SAME owning-service instances the direct routes use (projectAPI /
	// profileSvc / policyAPI / rsgSvc), so a project created from a
	// template behaves identically to one assembled by hand — the template
	// applies its defaults once and never controls the project afterwards.
	templatesAPI := templateshttp.New(templateshttp.Deps{
		Service: templates.NewService(templates.Deps{
			Catalog:       templates.Official(),
			Store:         persistence.NewTemplateStore(pool),
			Projects:      projectAPI.Service(),
			ProjectReader: projectAPI.Service(),
			Profiles:      profileSvc,
			Policy:        policyAPI.Service(),
			MapSeeds:      rsgSvc,
			// A default that fails reports a stable sentence to the caller
			// and logs its cause here (docs/45).
			Logger: logger,
		}),
	})
	templatesAPI.Register(v1)
	// Project milestones (T0609): the research-timeline surface —
	// separate from releases (no release is required to record one, and
	// recording one never touches the project lifecycle — no forced
	// "completed" terminal state). The create reuses ActionCreateRelease
	// (the milestones package doc records why); the read routes are
	// exactly as visible as the project (projectAPI.Service()).
	milestoneCommand := milestones.NewCommand(
		persistence.NewProjectStore(pool),
		releaseStore,
		persistence.NewMilestoneStore(pool),
		authz.NewMatrixEngine(),
	)
	milestoneAPI := milestonehttp.New(milestonehttp.Deps{
		Command:  milestoneCommand,
		Projects: projectAPI.Service(),
	})
	milestoneAPI.Register(v1)
	// Discussions (T0811): the conversation a project, a knowledge object or
	// a research PR carries, and the promotion of one comment into a
	// proposed research object. The three tables of 00104 are the whole of
	// the ordinary write path — a thread, a comment and a tombstone touch
	// nothing else — while a promotion goes through the command that owns
	// the object it creates: rsgSvc for a Hypothesis (a real state commit)
	// and for an external-evidence proposal, the store's own transaction for
	// an Issue. A promotion is authorized as write_scientific_state, the
	// same action the RSG writes evaluate, with the own_fork_only cell
	// resolved by the same fork lineage read; the discussion surface adds no
	// action to the matrix.
	discussionsAPI := discussionhttp.New(discussionhttp.Deps{
		Command: discussions.NewCommand(discussions.Deps{
			Projects:   projectAPI.Service(),
			Threads:    persistence.NewDiscussionStore(pool),
			Promotions: persistence.NewDiscussionStore(pool),
			Hypotheses: rsgSvc,
			Evidence:   rsgSvc,
			Authz:      authz.NewMatrixEngine(),
			ForkGate:   persistence.NewForkStore(pool),
		}),
	})
	discussionsAPI.Register(v1)
	// Research PR merge (T0406/T0409): the governance command that advances a
	// project's frozen main, and the only write path that ever does. The
	// service plans the merge (the pure engine), re-runs the PR's integrity
	// review and evaluates the governance policy server-side before it
	// commits, writes the accepted state through the shared state-commit
	// service, and records the merge, its idempotency ledger entry, its audit
	// row and its domain event in one transaction. The Git half then runs
	// through the provider merge bridge below — never a push: main advances
	// by a provider-side pull request merge, which is what T0302's branch
	// protection and the ref guard accept.
	//
	// Git and RefGuard are wired together and only when a provider is
	// configured. Without one, mergeAdapter is nil: the command still commits
	// the platform's own truth and records the Git step as unfinished
	// (retryable) instead of pretending the ref moved — and the guard's zero
	// value fails closed, so no identity would be accepted for main anyway.
	var mergeGit merge.GitMerger
	refGuard := gitprovider.RefGuard{MergeService: mergeLogin}
	if mergeAdapter != nil {
		mergeGit = mergegit.New(mergeAdapter, gitprovider.NewUserAccessStore(pool))
	}
	mergeSvc := merge.NewService(merge.Deps{
		Store:     persistence.NewSemanticMergeStore(pool),
		Diffs:     diffSvc,
		Plans:     resolutionSvc,
		Commits:   stateSvc,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		// The abort reader (T0602): when the plan materializes a version
		// the abort lifecycle moved, the merge copies docs/46:7's record
		// onto the row it writes rather than landing an 'aborted' version
		// with nobody's name on it. Wiring it is not optional in the sense
		// that matters: without it the merge REFUSES such a plan instead of
		// writing a record-less abort.
		Aborts: persistence.NewScientificObjectStore(pool),
		// The reopen reader (T0610): the same rule for the reverse edge —
		// a 'reopened' version the plan materializes onto main carries the
		// record of WHO decided the reopen and WHEN, because the row the
		// merge writes is stamped with the MERGING actor and the deciding
		// actor exists nowhere else (internal/application/merge/ports.go).
		// Deliberately asymmetric with Aborts above: no specification
		// mandates a reopen record (docs/46:11 says only that a reopen
		// creates a new transition and keeps the abort history), so an
		// unwired or record-less case copies nothing rather than refusing
		// a plan no sentence forbids.
		Reopens:  persistence.NewScientificObjectStore(pool),
		Projects: projectAPI.Service(),
		Authz:    authz.NewMatrixEngine(),
		// The fork lineage (T0817): the merge reads a source branch out of
		// its own project only when the lineage says that project is the PR
		// author's fork of the PR's project — the same triple 00086's
		// pull_request_fork_gate enforces on the row. Without it the merge
		// refuses every cross-project source rather than reading a foreign
		// project's branch on the say-so of a PR row.
		Forks:  persistence.NewForkStore(pool),
		Checks: checksSvc,
		// The policy in force is read through the owning service (T0603) and
		// evaluated through the typed rule surface: the merge asks a question
		// (main_protected?) and never reads policy_json itself.
		Policies: policyAPI.Service(),
		Rules:    policy.NewRuleEvaluator(),
		// The transactional outbox (T1001), the same recorder the RSG writes
		// use: pull_request.merged commits with the merge or not at all.
		Events:   events.Recorder{},
		Git:      mergeGit,
		RefGuard: refGuard,
	})
	mergeAPI := mergehttp.New(mergehttp.Deps{
		Command:  mergeSvc,
		Projects: projectAPI.Service(),
		// The collection's last path segment has ONE remainder owner and it
		// is this route (Go's ServeMux cannot match a suffix inside a
		// segment, and a second remainder registration panics), so the other
		// verb on that segment — ":request-review", T0411 — is served by the
		// pull-request surface's handler, dispatched from here.
		ReviewRequest: pullrequestsAPI.RequestReviewHandler(),
	})
	mergeAPI.Register(v1)
	// Freeze main governance (T0601). The two halves of one rule live in
	// different layers on purpose: the freeze COMMAND (internal/application/
	// mainfreeze) is the only writer of projects.main_frozen, and the
	// REFUSALS are where main is written — the state store declines any
	// semantic commit onto a frozen main that does not carry the Research PR
	// merge declaration it alone can set, and the push ingestion declines a
	// delivery that carries a direct refs/heads/main push. Wiring the
	// command without those two would leave the flag reported and not
	// enforced, which is exactly the hole this task closes.
	//
	// The membership port is the raw project store (as the publish command
	// is wired) rather than the projects service: it reports "no membership"
	// for a stranger AND for a project that does not exist, which is what
	// lets the command answer a permission-class refusal to both without
	// disclosing which one it was.
	freezeCommand := mainfreeze.NewCommand(mainfreeze.Deps{
		Members:  persistence.NewProjectStore(pool),
		Policies: policyAPI.Service(),
		Rules:    policy.NewRuleEvaluator(),
		Store:    persistence.NewMainFreezeStore(pool),
		Authz:    authz.NewMatrixEngine(),
	})
	freezeAPI := freezehttp.New(freezehttp.Deps{Command: freezeCommand})
	freezeAPI.Register(v1)
	// Abort proposals (T0602): the command behind
	// POST /projects/{projectId}/objects/{objectId}:abort-proposal. It never
	// writes main — docs/46:9 requires branch → PR → merge for a main
	// object, so the command forks a proposal branch off main's head,
	// appends the aborted version (with docs/46:7's record, the audit row
	// and the scientific_object.aborted event in one transaction), and opens
	// the Research PR. The merge above is what makes the abort effective.
	//
	// The membership port is the raw project store, as the freeze command is
	// wired: it answers "no membership" for a stranger AND for a project that
	// does not exist, which is what lets the command refuse both with the
	// same permission-class outcome without disclosing which one it was.
	abortSvc := aborts.NewService(aborts.Deps{
		Members:      persistence.NewProjectStore(pool),
		Authz:        authz.NewMatrixEngine(),
		Objects:      persistence.NewScientificObjectStore(pool),
		Branches:     branchSvc,
		PullRequests: prSvc,
		Commits:      stateSvc,
		Events:       events.Recorder{},
	})
	abortAPI := aborthttp.New(aborthttp.Deps{Command: abortSvc})
	abortAPI.Register(v1)
	// Evidence-backed search (T0906): POST /api/v1/search runs the whole
	// pipeline — resolve the actor's scope, plan, retrieve, rank, answer —
	// and records the search (query plan, selected refs, citations) before
	// it answers (docs/22 §8; the record is what /search/{searchId}:start-project
	// addresses later).
	//
	// Two steps are deliberately left unwired, and both absences are the
	// supported state the search packages document rather than a gap:
	//
	//   * the PLANNER has no provider. planner.New refuses a nil provider,
	//     so no planner is constructed at all: planning is skipped, the
	//     retrieval runs on the question alone, and the record's plan column
	//     is null — which says "this deployment planned nothing", not "the
	//     plan failed". A provider adapter is what a deployment that has
	//     answered "may a question leave the platform" would add.
	//   * the EMBEDDER is nil, so the vector signal does not run and the
	//     vector half of the corpus is not compared. The retrieval REPORTS
	//     this (SignalReport.Skipped = no_embedder) and the answer's
	//     limitations carry it, because "the vector signal did not run" and
	//     "no stored vector matched" are different statements about the
	//     corpus and only one of them is true here.
	//
	// The answer generator is built with no provider for the same reason, and
	// that is a state it accepts: an answer model that is absent costs the
	// written summary, never the structured result underneath it.
	retrievalStore, err := retrieval.NewSQLStore(pool)
	if err != nil {
		slog.Error("post-api: retrieval store setup failed", "error", err)
		return exitRuntime
	}
	rankingStore, err := ranking.NewSQLStore(pool)
	if err != nil {
		slog.Error("post-api: ranking store setup failed", "error", err)
		return exitRuntime
	}
	searcher, err := retrieval.NewRetriever(retrievalStore, nil)
	if err != nil {
		slog.Error("post-api: retriever setup failed", "error", err)
		return exitRuntime
	}
	ranker, err := ranking.NewRanker(rankingStore)
	if err != nil {
		slog.Error("post-api: ranker setup failed", "error", err)
		return exitRuntime
	}
	answerer, err := answer.New(answer.Deps{Logger: logger})
	if err != nil {
		slog.Error("post-api: answer generator setup failed", "error", err)
		return exitRuntime
	}
	// One SearchRecordStore for both surfaces: the search mounts it as its
	// writer and the Draft Research Context flow reads the same records
	// through the same object, so "the search a draft was started from" is
	// one table read by one adapter rather than two views of it.
	searchRecords := persistence.NewSearchRecordStore(pool)
	// The confirmation's two persistence roles are one object: the draft
	// store owns the 00134 rows, and the commit reader reads the RSG
	// transition log (state_commits) the confirmation names.
	draftStore := persistence.NewResearchContextStore(pool)
	researchContext := researchcontext.New(researchcontext.Deps{
		SearchRecords: searchRecords,
		DraftStore:    draftStore,
		Projects:      projectAPI.Service(),
		StateWriter:   rsgSvc,
		CommitReader:  draftStore,
		Authz:         authz.NewMatrixEngine(),
	})
	searchAPI := searchhttp.New(searchhttp.Deps{
		Scope:     persistence.NewProjectStore(pool),
		Retriever: searcher,
		Ranker:    ranker,
		Answerer:  answerer,
		Records:   searchRecords,
		Drafts:    researchContext,
		// The project a start creates is provision-pending, exactly like one
		// created by POST /api/v1/projects, and it is provisioned the same
		// way: the same job (newProvisionJob) on the same queue. Best effort
		// for the same reason the project surface is — the row is the truth
		// and the API's startup sweep back-fills anything a failed enqueue
		// skipped (enqueuePendingProvisioning, above).
		ProvisionProject: func(ctx context.Context, projectID string) error {
			correlationID := "start-project"
			if cid, ok := observability.FromContext(ctx); ok {
				correlationID = string(cid)
			}
			job, err := newProvisionJob(projectID, correlationID)
			if err != nil {
				return err
			}
			return provisioningQueue.Enqueue(ctx, job)
		},
	})
	searchAPI.Register(v1)
	mux.Handle("/api/v1/", authAPI.Guard(v1))

	// The edge chain, outermost first (T1106):
	//   1. security.Headers stamps the secure response-header set on every
	//      response — including the ones the limiter refuses below, so a
	//      429 carries the same CSP and nosniff as a 200;
	//   2. observability assigns the correlation id, which the limiter's
	//      own error envelope reports as request_id;
	//   3. security.RateLimit guards the WHOLE tree — not just /api/v1 —
	//      because the two routes that bypass the /api/v1 guard are the
	//      ones that most need a budget: the provider push receiver
	//      (POST /api/v1/git/hooks/gitea, authenticated by HMAC, outside
	//      the session guard by design) and the scaffold enqueue endpoint
	//      (POST /internal/jobs). The only exemption is the pair of
	//      liveness/readiness probes (security.ExemptProbePaths).
	securityCfg.SessionCookie = authhttp.CookieSession
	handler := security.Headers()(
		observability.Middleware(logger)(
			security.RateLimit(rateLimiter, *securityCfg)(mux)))
	slog.Info("post-api: edge rate limiting active", "policy", securityCfg.Describe())

	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           handler,
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

// securityLoader resolves the edge-hardening configuration environment
// (injectable, same reason as authnLoader).
var securityLoader = func() security.Loader { return security.Loader{} }

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

// feedService wires the T1004 public feeds when the configured web origin is
// a usable base for the absolute links every feed document is built from
// (scheme://host[:port], no path), nil otherwise — the same "disabled is
// visible" shape as the two helpers above, because a feed whose links cannot
// be built must not be served at all: a subscriber that stores
// http:///assets/... learns nothing, and a 404 there would tell every reader
// the feed does not exist.
//
// The nil it returns is a NIL INTERFACE (not a typed nil pointer), which is
// what feedshttp.Deps.Service's nil check requires.
func feedService(reader feeds.Reader, webOrigin string, logger *slog.Logger) feedshttp.Service {
	svc, err := feeds.NewService(reader, feeds.Config{BaseURL: webOrigin})
	if err != nil {
		logger.Warn("post-api: public feeds disabled",
			"error", err,
			"effect", "the feed routes answer 503; set POST_WEB_ORIGIN to the deployment's public web origin and restart")
		return nil
	}
	return svc
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
