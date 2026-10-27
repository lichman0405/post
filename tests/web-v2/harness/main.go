// Command web-v2-harness is the isolated real-services API for the WEB-01
// acceptance harness (docs/70 §6.1-6.4, G23).
//
// It composes the PRODUCTION authentication surface (cmd/api/authhttp) over a
// real PostgreSQL database and a real Redis, exactly as cmd/api wires it:
// credential store, Redis session store, Redis rate limiter, audit store,
// the session/CSRF guard and its CORS allow-list. The only difference from
// cmd/api is what it deliberately does NOT do:
//
//   - it seeds no accounts. The first account in the journey is created by
//     the browser through the product's own /login sign-up form; a harness
//     that pre-created it through HTTP or SQL would make the login journey
//     prove nothing (docs/70 §6.2).
//   - it seeds no projects, membership or other business state.
//   - it serves only the auth surface plus /healthz and /readyz. The journey
//     in this task is U01's login/session slice; later journey slices mount
//     their own product surfaces here as the features land.
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
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/persistence"
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
		RunID:      cfg.runID,
		Task:       harnessTaskID,
		Commit:     cfg.commit,
		APIOrigin:  origin,
		WebOrigin:  cfg.webOrigin,
		Database:   name,
		RedisAddr:  cfg.redisAddr,
		RedisDB:    cfg.redisDB,
		StartedAt:  time.Now().UTC().Format(time.RFC3339Nano),
		HarnessPID: os.Getpid(),
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
