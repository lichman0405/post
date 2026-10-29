// Command web-v2-harness is the isolated real-services API for the WEB-01
// acceptance harness (docs/70 §6.1-6.4, G23).
//
// It composes the PRODUCTION authentication surface (cmd/api/authhttp) over a
// real PostgreSQL database and a real Redis, exactly as cmd/api wires it:
// credential store, Redis session store, Redis rate limiter, audit store,
// the session/CSRF guard and its CORS allow-list. The only difference from
// cmd/api is what it deliberately does NOT do:
//
//   - it seeds no account for the journeys that CREATE one. The first
//     account in those journeys is created by the browser through the
//     product's own /login sign-up form; a harness that pre-created it
//     through HTTP or SQL would make the login journey prove nothing
//     (docs/70 §6.2).
//   - its ONE seeded fixture is the WEB-05 browse project (fixture.go): the
//     research map, the outline list and the object page read research state
//     that no web surface can author yet, and docs/70 §6.3 allows a
//     synthetic seed for exactly that. It is PRIVATE, so it is exactly as
//     visible as its one member and cannot alter another journey's directory;
//     the journey signs that member in through the real /login form. The
//     fixture proves no creation step.
//   - it serves the auth surface, the project read surface, the scientific
//     object surface (the research outline, the object detail and the object
//     page's content negotiation) and /healthz + /readyz. T1303 (WEB-02)
//     needs the home workbench to list the signed-in account's projects, so
//     the production project API is mounted over the same real database;
//     later journey slices mount their own product surfaces here as the
//     features land.
//
// Isolation: every run creates its own database `webv2_<task>_<run id>`,
// migrates it, and drops it on exit. Redis sessions are issued random
// tokens; this process records the tokens it created and deletes only those
// on shutdown, so a run never touches another run's (or the dev stack's)
// keys. The database name and run id travel in the READY line so the runner
// can put them in the evidence record.
//
// Failure policy is docs/70 §6.4: a missing PostgreSQL, Redis, web origin or
// listener is a FAILED start with the reason on stderr — never a skip. The
// process prints exactly one `READY {...}` line on stdout once the API is
// listening, and the runner refuses to drive a browser until it has that
// line.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/orgshttp"
	"github.com/lichman0405/post/cmd/api/profilehttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/cmd/api/rsghttp"
	"github.com/lichman0405/post/cmd/api/validationhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/states"
	appvalidation "github.com/lichman0405/post/internal/application/validation"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

const (
	// harnessTaskID names the databases this harness creates. It is the
	// task id, not the WEB id, because the database namespace is the one
	// testdb uses for every other database-scoped suite.
	harnessTaskID = "T1302"

	defaultRedis = "127.0.0.1:6379"
)

// ready describes the running API. It is the machine-readable half of the
// READY line; the runner parses it to build the web app against the API and
// to record the environment in the evidence.
type ready struct {
	RunID      string `json:"run_id"`
	Task       string `json:"task"`
	Commit     string `json:"commit"`
	APIOrigin  string `json:"api_origin"`
	WebOrigin  string `json:"web_origin"`
	Database   string `json:"database"`
	RedisAddr  string `json:"redis_addr"`
	RedisDB    int    `json:"redis_db"`
	StartedAt  string `json:"started_at"`
	HarnessPID int    `json:"harness_pid"`
	// Web05ProjectSlug names the browse fixture this run seeded, so a
	// reviewer reading the run record can tell which project the screenshots
	// show. The account's address is deliberately absent: the READY line is
	// copied into the evidence, and the evidence must not record credentials
	// (docs/70 §6.4).
	Web05ProjectSlug string `json:"web05_project_slug,omitempty"`
}

func main() {
	preflight := flag.Bool("preflight", false, "check PostgreSQL/Redis/web-origin reachability and exit")
	flag.Parse()

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "web-v2-harness: REFUSING TO RUN: %v\n", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *preflight {
		if err := preflightOnly(ctx, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "web-v2-harness: PREFLIGHT FAILED: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("PREFLIGHT OK postgres=%s redis=%s web_origin=%s\n",
			redactDSN(cfg.adminURL), cfg.redisAddr, cfg.webOrigin)
		return
	}

	if err := run(ctx, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "web-v2-harness: %v\n", err)
		os.Exit(1)
	}
}

type config struct {
	adminURL  string
	redisAddr string
	redisDB   int
	webOrigin string
	runID     string
	commit    string
	addr      string
	// The WEB-05 fixture owner's credentials. Both sides derive the same
	// values from the run id, so the values are per-run, synthetic, and never
	// a constant someone could sign in with against another run's database.
	web05Email    string
	web05Password string
}

func loadConfig() (config, error) {
	c := config{
		adminURL:  envDefault("WEB_V2_POSTGRES_ADMIN_URL", os.Getenv("POSTGRES_TEST_ADMIN_URL")),
		redisAddr: envDefault("WEB_V2_REDIS_ADDR", envDefault("POST_REDIS_ADDR", defaultRedis)),
		webOrigin: strings.TrimSuffix(envDefault("WEB_V2_WEB_ORIGIN", ""), "/"),
		runID:     envDefault("WEB_V2_RUN_ID", ""),
		commit:    envDefault("WEB_V2_COMMIT", "unknown"),
		addr:      envDefault("WEB_V2_API_ADDR", "127.0.0.1:0"),
	}
	// No built-in DSN: a credential-bearing default in Go source is a
	// finding wait to happen (gosec G101) and a silent fallback could point
	// a run at the wrong database. The shell runner supplies the documented
	// dev value; the harness requires one to be named.
	if c.adminURL == "" {
		return c, errors.New("configuration error: WEB_V2_POSTGRES_ADMIN_URL (or POSTGRES_TEST_ADMIN_URL) must name the test database admin DSN; there is no built-in default")
	}
	db, err := envInt("WEB_V2_REDIS_DB", 15)
	if err != nil {
		return c, err
	}
	c.redisDB = db

	if c.runID == "" {
		id, err := randomHex(6)
		if err != nil {
			return c, fmt.Errorf("generate run id: %w", err)
		}
		c.runID = time.Now().UTC().Format("20060102T150405Z") + "-" + id
	}
	// The WEB-05 fixture's synthetic credentials, derived from the run id the
	// same way journey-web05.mjs derives them. Env overrides exist for a
	// hand-driven run; the runner sets both sides from one run id.
	c.web05Email = envDefault("WEB_V2_WEB05_EMAIL",
		"webv2-web05-"+strings.ToLower(c.runID)+"@example.test")
	c.web05Password = envDefault("WEB_V2_WEB05_PASSWORD", "webv2-synthetic-"+c.runID+"-password")
	adminURL, err := url.Parse(c.adminURL)
	if err != nil {
		// url.Parse errors quote the input, password included; report the
		// redacted form instead of wrapping the error.
		return c, fmt.Errorf("WEB_V2_POSTGRES_ADMIN_URL %s is not a valid URL", redactDSN(c.adminURL))
	}
	if adminURL.Scheme == "" || adminURL.Host == "" {
		return c, fmt.Errorf("WEB_V2_POSTGRES_ADMIN_URL %s must be an absolute URL (scheme://host/database)", redactDSN(c.adminURL))
	}
	if c.webOrigin == "" {
		return c, errors.New("WEB_V2_WEB_ORIGIN is required: the API's CORS allow-list and CSRF origin check are bound to the browser origin, and a guessed value would make the browser journey fail for the wrong reason")
	}
	u, err := url.Parse(c.webOrigin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return c, fmt.Errorf("WEB_V2_WEB_ORIGIN %q is not an origin", c.webOrigin)
	}
	if u.Path != "" {
		return c, fmt.Errorf("WEB_V2_WEB_ORIGIN %q must be scheme://host[:port] with no path", c.webOrigin)
	}
	return c, nil
}

// preflightOnly answers the question the runner must ask before it builds
// anything: are the real services reachable? Unreachable is a failure with
// the DSN/address named, never a skip.
func preflightOnly(ctx context.Context, cfg config) error {
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(pingCtx, cfg.adminURL)
	if err != nil {
		return fmt.Errorf("postgres unreachable at %s: %w", redactDSN(cfg.adminURL), err)
	}
	if err := conn.Ping(pingCtx); err != nil {
		_ = conn.Close(context.Background())
		return fmt.Errorf("postgres unreachable at %s: %w", redactDSN(cfg.adminURL), err)
	}
	_ = conn.Close(context.Background())

	client := redis.NewClient(&redis.Options{Addr: cfg.redisAddr, DB: cfg.redisDB})
	defer client.Close()
	if err := client.Ping(pingCtx).Err(); err != nil {
		return fmt.Errorf("redis unreachable at %s db %d: %w", cfg.redisAddr, cfg.redisDB, err)
	}
	return nil
}

func run(ctx context.Context, cfg config) error {
	if err := preflightOnly(ctx, cfg); err != nil {
		return err
	}

	name, err := databaseName(cfg.runID)
	if err != nil {
		return err
	}
	if err := createDatabase(ctx, cfg.adminURL, name); err != nil {
		return err
	}
	dropped := false
	drop := func() {
		if dropped {
			return
		}
		dropped = true
		dropCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := dropDatabase(dropCtx, cfg.adminURL, name); err != nil {
			fmt.Fprintf(os.Stderr, "web-v2-harness: could not drop database %s: %v\n", name, err)
		}
	}
	defer drop()

	dbURL, err := withDatabase(cfg.adminURL, name)
	if err != nil {
		return err
	}
	if _, err := persistence.Migrate(ctx, dbURL); err != nil {
		return fmt.Errorf("migrate isolated database %s: %w", name, err)
	}
	pool, err := persistence.Open(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("open isolated database %s: %w", name, err)
	}
	defer pool.Close()

	redisClient := redis.NewClient(&redis.Options{Addr: cfg.redisAddr, DB: cfg.redisDB})
	defer redisClient.Close()
	pingCtx, cancelPing := context.WithTimeout(ctx, 5*time.Second)
	defer cancelPing()
	if err := redisClient.Ping(pingCtx).Err(); err != nil {
		return fmt.Errorf("redis unreachable at %s db %d: %w", cfg.redisAddr, cfg.redisDB, err)
	}

	sessions := &trackingSessions{inner: persistence.NewRedisSessionStore(redisClient), tokens: map[string]struct{}{}}
	defer sessions.cleanup()

	authAPI := authhttp.New(authhttp.Deps{
		Users:    persistence.NewCredentialStore(pool),
		Sessions: sessions,
		Limiter:  persistence.NewRedisRateLimiter(redisClient),
		Cfg: authn.Config{
			WebOrigin:          cfg.webOrigin,
			SessionTTL:         time.Hour,
			LoginLimitPerEmail: 1000,
			LoginLimitPerIP:    10000,
			LoginWindow:        time.Minute,
			SignupLimitPerIP:   10000,
		},
		// The harness serves plain HTTP on loopback, exactly like the dev
		// topology; a Secure cookie would never come back.
		Secure: false,
		Audit:  persistence.NewAuditStore(pool),
	})

	mux := http.NewServeMux()
	authAPI.Register(mux)

	// The project surface (WEB-02's home lists the account's projects; the
	// journey also follows the work entry into /projects). Composed exactly
	// as cmd/api composes it — pgx ProjectStore, pgx OrgStore as the
	// organization gate, the production policy matrix — with no provisioning
	// queue: this harness has no worker, and the create path's job enqueue is
	// optional by design (the project row is the source of truth and the API's
	// startup sweep back-fills a missing queue).
	orgStore := persistence.NewOrgStore(pool)
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: persistence.NewProjectStore(pool),
		Orgs:  orgStore,
		Authz: authz.NewMatrixEngine(),
	})
	mux.Handle("/api/v1/projects", projectAPI.Routes())
	mux.Handle("/api/v1/projects/", projectAPI.Routes())

	// The organization surface (T1304 / WEB-03, G03): creation, the member
	// list, invites, role changes and end-of-affiliation, composed exactly as
	// cmd/api composes them (pgx OrgStore, production handlers and guard).
	// The journey drives all of it through the web UI; no step calls it.
	orgAPI := orgshttp.New(orgshttp.Deps{Store: orgStore})
	mux.Handle("/api/v1/organizations", orgAPI.Routes())
	mux.Handle("/api/v1/organizations/", orgAPI.Routes())

	// The public profile read (T0102): the member page resolves a membership
	// row's user id to the handle the reader recognises. The API's payload is
	// the public one; the journey never reads it directly.
	profileAPI := profilehttp.New(profilehttp.Deps{
		Profiles: persistence.NewProfileStore(pool),
	})
	profileAPI.Register(mux)

	// The scientific-object surface (T1306 / WEB-05): the research outline,
	// the object detail read (with its version log and resolved relations)
	// and the object page's HTML/JSON content negotiation, composed exactly
	// as cmd/api composes it — the schema registry, the ONE state-commit
	// service every RSG write lands through, and the production matrix.
	// rsghttp's two optional graph readers stay nil: this harness serves no
	// provenance or evidence tab, and the surface's contract is that an
	// unwired reader says the graph could not be read — never that it is
	// empty (rsghttp.Deps).
	reg, err := schemareg.New()
	if err != nil {
		return fmt.Errorf("load the schema registry: %w", err)
	}
	stateStore := persistence.NewStateStore(pool)
	rsgSvc := rsg.NewService(rsg.Deps{
		Projects:  projectAPI.Service(),
		Branches:  branches.NewService(persistence.NewBranchStore(pool)),
		States:    states.NewService(stateStore, appvalidation.NewGuard(rsgvalidation.NewValidator(reg), persistence.NewValidationTxProbe())),
		Latest:    stateStore,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Queries:   persistence.NewRSGQueryStore(pool),
		Profiles:  persistence.NewProfileStore(pool),
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
		Events:    events.Recorder{},
	})
	rsgAPI := rsghttp.New(rsghttp.Deps{Service: rsgSvc})
	rsgAPI.Register(mux)

	// The progressive validation gate (T1307 / WEB-06a): the authoring
	// surface checks a branch against the pr gate before a reader claims it
	// is ready for review, and renders the report's own missing-field rows.
	// Composed exactly as cmd/api composes it — the snapshot repository over
	// the state store, the canonical schema registry, and the same project
	// read gate as every other read.
	validationAPI := validationhttp.New(validationhttp.Deps{
		Validator: appvalidation.NewService(
			persistence.NewValidationSnapshotRepository(persistence.NewStateStore(pool)),
			rsgvalidation.NewValidator(reg),
		),
		Projects: projectAPI.Service(),
	})
	validationAPI.Register(mux)

	fixture, err := seedWeb05(ctx, pool, projectAPI.Service(), rsgSvc, cfg.web05Email, cfg.web05Password)
	if err != nil {
		return fmt.Errorf("seed the WEB-05 browse fixture: %w", err)
	}

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"service": "post-api",
			"status":  "ok",
			"version": "web-v2-harness",
		})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		checkCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		checks := map[string]any{}
		status := "ready"
		code := http.StatusOK
		if err := pool.Ping(checkCtx); err != nil {
			checks["postgres"] = map[string]string{"status": "down"}
			status, code = "not_ready", http.StatusServiceUnavailable
		} else {
			checks["postgres"] = map[string]string{"status": "up"}
		}
		if err := redisClient.Ping(checkCtx).Err(); err != nil {
			checks["redis"] = map[string]string{"status": "down"}
			status, code = "not_ready", http.StatusServiceUnavailable
		} else {
			checks["redis"] = map[string]string{"status": "up"}
		}
		writeJSON(w, code, map[string]any{"status": status, "checks": checks})
	})

	guarded := authAPI.Guard(mux)

	listener, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.addr, err)
	}
	server := &http.Server{Handler: guarded, ReadHeaderTimeout: 15 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutCtx)
	}()

	origin := "http://" + listener.Addr().String()
	line := ready{
		RunID:            cfg.runID,
		Task:             harnessTaskID,
		Commit:           cfg.commit,
		APIOrigin:        origin,
		WebOrigin:        cfg.webOrigin,
		Database:         name,
		RedisAddr:        cfg.redisAddr,
		RedisDB:          cfg.redisDB,
		StartedAt:        time.Now().UTC().Format(time.RFC3339Nano),
		HarnessPID:       os.Getpid(),
		Web05ProjectSlug: fixture.ProjectSlug,
	}
	blob, err := json.Marshal(line)
	if err != nil {
		return err
	}
	fmt.Printf("READY %s\n", blob)
	fmt.Fprintf(os.Stderr, "web-v2-harness: listening on %s (database %s, redis db %d, web origin %s)\n",
		origin, name, cfg.redisDB, cfg.webOrigin)

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	return nil
}

// trackingSessions wraps the production Redis session store and remembers
// the tokens this run created, so shutdown revokes exactly this run's
// sessions and nothing else in the shared Redis instance.
type trackingSessions struct {
	inner  authn.SessionStore
	mu     sync.Mutex
	tokens map[string]struct{}
}

func (t *trackingSessions) Create(ctx context.Context, s authn.Session, ttl time.Duration) error {
	if err := t.inner.Create(ctx, s, ttl); err != nil {
		return err
	}
	t.mu.Lock()
	t.tokens[s.Token] = struct{}{}
	t.mu.Unlock()
	return nil
}

func (t *trackingSessions) Get(ctx context.Context, token string) (authn.Session, error) {
	return t.inner.Get(ctx, token)
}

func (t *trackingSessions) Delete(ctx context.Context, token string) error {
	err := t.inner.Delete(ctx, token)
	t.mu.Lock()
	delete(t.tokens, token)
	t.mu.Unlock()
	return err
}

func (t *trackingSessions) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t.mu.Lock()
	tokens := make([]string, 0, len(t.tokens))
	for token := range t.tokens {
		tokens = append(tokens, token)
	}
	t.mu.Unlock()
	for _, token := range tokens {
		if err := t.inner.Delete(ctx, token); err != nil {
			fmt.Fprintf(os.Stderr, "web-v2-harness: could not revoke session from this run: %v\n", err)
		}
	}
}

func createDatabase(ctx context.Context, adminURL, name string) error {
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return fmt.Errorf("connect for CREATE DATABASE: %w", err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return fmt.Errorf("CREATE DATABASE %s: %w", name, err)
	}
	return nil
}

func dropDatabase(ctx context.Context, adminURL, name string) error {
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	_, _ = conn.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
	_, err = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize())
	return err
}

func withDatabase(raw, name string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	u.Path = "/" + name
	return u.String(), nil
}

// databaseName is deterministic from the run id and safe for PostgreSQL:
// lower-case letters, digits and underscores, at most 63 bytes.
func databaseName(runID string) (string, error) {
	clean := strings.ToLower(runID)
	var b strings.Builder
	for _, r := range clean {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := "webv2_" + strings.ToLower(harnessTaskID) + "_" + b.String()
	if len(name) > 63 {
		name = name[:63]
	}
	if len(name) < 10 {
		return "", fmt.Errorf("run id %q produced an unusable database name", runID)
	}
	return name, nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	// Atoi rejects trailing garbage ("15abc"), which Sscanf would accept as
	// 15 — a silently wrong Redis namespace is worse than a failed start.
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not an integer", key, v)
	}
	return n, nil
}

// redactDSN keeps the user and host and drops the password: diagnostics may
// name the service they could not reach, never the credential.
func redactDSN(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparsable DSN>"
	}
	if u.User != nil {
		u.User = url.UserPassword(u.User.Username(), "***")
	}
	return u.String()
}
