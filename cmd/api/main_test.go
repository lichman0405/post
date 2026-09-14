package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/schemaprofiles"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

func TestRunWithoutConfigurationFailsClosedNamingPOSTEnv(t *testing.T) {
	// The API must refuse to start on a missing configuration and name the
	// offending variable (T0006 fail-closed requirement). The test scrubs
	// the layer so LoadFromCwd cannot find a layer file either.
	t.Setenv("POST_ENV", "")
	if code := run([]string{}); code != exitConfig {
		t.Fatalf("run without configuration = %d, want %d (config failure)", code, exitConfig)
	}
}

func TestRunVersionFlagPrintsAndExitsZero(t *testing.T) {
	if code := run([]string{"-version"}); code != exitOK {
		t.Fatalf("run -version = %d, want 0", code)
	}
}

// TestHealthSurfaceReportsTruthfulReadinessWithPostgresDown exercises the
// real wired handler: liveness answers 200 while PostgreSQL is unreachable,
// readiness answers 503 not_ready with the per-dependency truth — the
// "interesting case" of the T0006 acceptance criteria, shown here at the
// handler level and again in the process-level smoke.
func TestHealthSurfaceReportsTruthfulReadinessWithPostgresDown(t *testing.T) {
	// A lazy pool pointed at a port nobody listens on: connection refused.
	pool, err := openLazyTestPool("127.0.0.1:1")
	if err != nil {
		t.Fatalf("lazy pool: %v", err)
	}
	defer pool.Close()

	// A redis client pointed at the same closed port (no redis running).
	redisClient := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer redisClient.Close()

	h := newHealthHandler(pool, redisClient)

	// Liveness: must not depend on any downstream service.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz with both deps down = %d, want 200 (liveness only)", rec.Code)
	}
	var live map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &live); err != nil {
		t.Fatalf("healthz body: %v", err)
	}
	if live["status"] != "ok" {
		t.Errorf("healthz status = %v, want ok", live["status"])
	}

	// Readiness: PostgreSQL down => 503 not_ready, never a crash, never 200.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with postgres down = %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}
	var ready map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &ready); err != nil {
		t.Fatalf("readyz body: %v", err)
	}
	if ready["status"] != "not_ready" {
		t.Errorf("readyz status = %v, want not_ready", ready["status"])
	}
	checks, _ := ready["checks"].(map[string]any)
	pg, _ := checks["postgresql"].(map[string]any)
	if pg["status"] != "down" {
		t.Errorf("postgresql check = %v, want down", pg)
	}
	// The public body carries no probe detail: it would disclose the database
	// host, port, name and user to an unauthenticated caller. The detail is
	// logged for the operator instead.
	if _, present := pg["detail"]; present {
		t.Errorf("readyz exposed probe detail to an unauthenticated caller: %v", pg)
	}
	rawReady, err := json.Marshal(ready)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawReady), "connection refused") || strings.Contains(string(rawReady), "user=") {
		t.Errorf("readyz body carries internal topology: %s", rawReady)
	}
	rd, _ := checks["redis"].(map[string]any)
	if rd["status"] != "down" {
		t.Errorf("redis check = %v, want down", rd)
	}
}

// openLazyTestPool mirrors the production pool settings without pulling in
// the full config (the DSN builder is covered by the smoke run).
func openLazyTestPool(hostPort string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig("postgres://test:test@" + hostPort + "/post?sslmode=disable")
	if err != nil {
		return nil, err
	}
	cfg.MinConns = 0
	cfg.MaxConns = 8
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute
	return pgxpool.NewWithConfig(context.Background(), cfg)
}

// TestRunStartsWithPostgresDownAndServesHealthTruthfully exercises the
// STARTUP PATH of the T0006 lazy-start contract end to end: run() must
// reach ListenAndServe and answer /healthz 200 and /readyz 503 not_ready
// while PostgreSQL is unreachable — a down database must never keep the
// API from starting. The schema-profile load (T0213) runs as a background
// retry loop precisely because of this contract: under the old behavior
// (a synchronous LoadAll in run()) this test is RED — run() returns
// exitRuntime before the listener ever opens.
func TestRunStartsWithPostgresDownAndServesHealthTruthfully(t *testing.T) {
	// A free loopback port for the API listener (released again before
	// run() binds it).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a free port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	// Minimal configuration: every REQUIRED variable set; every dependency
	// pointed at a port that refuses connections; the optional surfaces
	// blanked so a host environment cannot leak into the run.
	for k, v := range map[string]string{
		"POST_ENV":                  "test",
		"POST_API_ADDR":             addr,
		"POST_DB_HOST":              "127.0.0.1",
		"POST_DB_PORT":              "1",
		"POST_DB_USER":              "postgres",
		"POST_DB_PASSWORD":          "test",
		"POST_DB_NAME":              "post",
		"POST_DB_SSLMODE":           "disable",
		"POST_REDIS_ADDR":           "127.0.0.1:1",
		"POST_BLOB_ACCESS_KEY":      "test",
		"POST_BLOB_SECRET_KEY":      "test",
		"POST_BLOB_ENDPOINT":        "",
		"POST_BLOB_BUCKET":          "",
		"POST_BLOB_USE_TLS":         "",
		"POST_GITEA_TOKEN":          "test",
		"POST_GITEA_BASE_URL":       "",
		"POST_GITEA_WEBHOOK_URL":    "",
		"POST_GITEA_ADMIN_USER":     "",
		"POST_GITEA_ADMIN_PASSWORD": "",
		"POST_OIDC_ISSUER":          "",
		"POST_OIDC_CLIENT_ID":       "",
		"POST_OIDC_CLIENT_SECRET":   "",
		"POST_AUTH_SESSION_TTL":     "",
		"POST_AUTH_LOGIN_PER_EMAIL": "",
		"POST_AUTH_LOGIN_PER_IP":    "",
		"POST_AUTH_LOGIN_WINDOW":    "",
		"POST_AUTH_SIGNUP_PER_IP":   "",
	} {
		t.Setenv(k, v)
	}

	done := make(chan int, 1)
	go func() { done <- run([]string{}) }()

	// Poll until the API answers liveness. A run() that exits early — the
	// old synchronous-load behavior, or a config problem — fails fast with
	// the real exit code instead of polluting the assertion with a timeout.
	base := "http://" + addr
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("healthz with postgres down = %d (body %s), want 200 — liveness must not depend on PostgreSQL", resp.StatusCode, body)
			}
			break
		}
		select {
		case code := <-done:
			t.Fatalf("run() exited with %d before /healthz ever answered — the API must start (and answer liveness) while PostgreSQL is down", code)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("/healthz never answered (last error: %v) — run() neither served nor exited", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Readiness tells the truth: PostgreSQL down => 503 not_ready, never a
	// crash, never 200 — from the real process, not just the handler.
	resp, err := http.Get(base + "/readyz")
	if err != nil {
		t.Fatalf("readyz: %v", err)
	}
	readyBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz with postgres down = %d (body %s), want 503", resp.StatusCode, readyBody)
	}
	var ready map[string]any
	if err := json.Unmarshal(readyBody, &ready); err != nil {
		t.Fatalf("readyz body: %v", err)
	}
	if ready["status"] != "not_ready" {
		t.Errorf("readyz status = %v, want not_ready", ready["status"])
	}
	if checks, _ := ready["checks"].(map[string]any); checks != nil {
		if pg, _ := checks["postgresql"].(map[string]any); pg["status"] != "down" {
			t.Errorf("postgresql check = %v, want down", pg)
		}
	}

	// The background profile load is retrying against the dead database
	// right now — and must keep doing so: a process that exits here is the
	// fail-fast the load must never take for an unreachable store.
	select {
	case code := <-done:
		t.Fatalf("run() exited with %d while PostgreSQL was down — the profile load must retry in the background, never exit the process", code)
	case <-time.After(2 * time.Second):
	}

	// Clean shutdown: SIGTERM to this very process — run()'s
	// NotifyContext was registered long before the listener opened, so the
	// signal is consumed and run() returns exitOK.
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("run() after SIGTERM = %d, want %d", code, exitOK)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run() did not return after SIGTERM")
	}
}

// schemaProfileLoaderFake scripts ListAllProfiles for the loader loop
// tests; the service's other ports are unused by LoadAll and stubbed.
type schemaProfileLoaderFake struct {
	attempts int
	failures int // fail ListAllProfiles this many times before succeeding
	err      error
	rows     []domain.ProjectSchemaProfile
}

func (f *schemaProfileLoaderFake) ListAllProfiles(ctx context.Context) ([]domain.ProjectSchemaProfile, error) {
	f.attempts++
	if f.attempts <= f.failures {
		return nil, f.err
	}
	return f.rows, nil
}

func (f *schemaProfileLoaderFake) RegisterProfile(ctx context.Context, p domain.ProjectSchemaProfile, audit domain.AuditEntry) (domain.ProjectSchemaProfile, error) {
	return domain.ProjectSchemaProfile{}, errors.New("RegisterProfile: unused in loader tests")
}

func (f *schemaProfileLoaderFake) GetProfile(ctx context.Context, projectID, schemaID, version string) (domain.ProjectSchemaProfile, error) {
	return domain.ProjectSchemaProfile{}, errors.New("GetProfile: unused in loader tests")
}

func (f *schemaProfileLoaderFake) GetLatestProfile(ctx context.Context, projectID, schemaID string) (domain.ProjectSchemaProfile, error) {
	return domain.ProjectSchemaProfile{}, errors.New("GetLatestProfile: unused in loader tests")
}

func (f *schemaProfileLoaderFake) ListProfiles(ctx context.Context, projectID string) ([]domain.ProjectSchemaProfile, error) {
	return nil, errors.New("ListProfiles: unused in loader tests")
}

func (f *schemaProfileLoaderFake) Get(ctx context.Context, r projects.Reader, projectID string) (domain.Project, error) {
	return domain.Project{}, errors.New("Get: unused in loader tests")
}

func (f *schemaProfileLoaderFake) GetMembership(ctx context.Context, actor domain.User, projectID string) (domain.ProjectMembership, error) {
	return domain.ProjectMembership{}, errors.New("GetMembership: unused in loader tests")
}

// newLoaderTestService wires a schemaprofiles.Service over the scripted
// store and a fresh canonical registry — the same ports the production
// run() wiring uses.
func newLoaderTestService(t *testing.T, store *schemaProfileLoaderFake) *schemaprofiles.Service {
	t.Helper()
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	return schemaprofiles.NewService(schemaprofiles.Deps{Store: store, Projects: store, Schemas: reg})
}

func loaderTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestRunSchemaProfileLoadRetriesUnreachableStoreThenSucceeds: ErrStore is
// the retryable class — the loader keeps trying while the database is down
// and returns nil once the store answers, so a transient outage never ends
// the process.
func TestRunSchemaProfileLoadRetriesUnreachableStoreThenSucceeds(t *testing.T) {
	oldBackoff := schemaProfileLoadBackoff
	schemaProfileLoadBackoff = time.Millisecond
	t.Cleanup(func() { schemaProfileLoadBackoff = oldBackoff })

	store := &schemaProfileLoaderFake{
		failures: 2,
		err:      fmt.Errorf("postgres down: %w", schemaprofiles.ErrStore),
	}
	svc := newLoaderTestService(t, store)
	if err := runSchemaProfileLoad(context.Background(), svc, loaderTestLogger()); err != nil {
		t.Fatalf("runSchemaProfileLoad: %v", err)
	}
	if store.attempts != 3 {
		t.Errorf("attempts = %d, want 3 (two failures, one success)", store.attempts)
	}
}

// TestRunSchemaProfileLoadSucceedsImmediately: a reachable store loads on
// the first pass and the loop ends.
func TestRunSchemaProfileLoadSucceedsImmediately(t *testing.T) {
	store := &schemaProfileLoaderFake{}
	svc := newLoaderTestService(t, store)
	if err := runSchemaProfileLoad(context.Background(), svc, loaderTestLogger()); err != nil {
		t.Fatalf("runSchemaProfileLoad: %v", err)
	}
	if store.attempts != 1 {
		t.Errorf("attempts = %d, want 1", store.attempts)
	}
}

// TestRunSchemaProfileLoadFailsFastOnCorruption: ErrCorruption is NOT
// retryable — a broken persisted row ends the loop immediately so run()
// exits rather than serving a registry that diverges from the database
// rows. The fake models the corruption the service actually detects: a row
// whose content does not re-hash to its stored hash.
func TestRunSchemaProfileLoadFailsFastOnCorruption(t *testing.T) {
	store := &schemaProfileLoaderFake{rows: []domain.ProjectSchemaProfile{{
		ProjectID:   "p-1",
		SchemaID:    "project:p-1:catalysis",
		Version:     "1",
		Content:     `{"type":"object"}`,
		ContentHash: "0000000000000000000000000000000000000000000000000000000000000000",
	}}}
	svc := newLoaderTestService(t, store)
	err := runSchemaProfileLoad(context.Background(), svc, loaderTestLogger())
	if !errors.Is(err, schemaprofiles.ErrCorruption) {
		t.Fatalf("err = %v, want ErrCorruption", err)
	}
	if store.attempts != 1 {
		t.Errorf("attempts = %d, want 1 (corruption must not retry)", store.attempts)
	}
}

// TestRunSchemaProfileLoadStopsOnCancel: a canceled context (shutdown)
// ends the retry loop — the goroutine in run() then skips the fatal
// channel and the process shuts down normally.
func TestRunSchemaProfileLoadStopsOnCancel(t *testing.T) {
	oldBackoff := schemaProfileLoadBackoff
	schemaProfileLoadBackoff = time.Millisecond
	t.Cleanup(func() { schemaProfileLoadBackoff = oldBackoff })

	store := &schemaProfileLoaderFake{
		failures: 1 << 30,
		err:      fmt.Errorf("postgres down: %w", schemaprofiles.ErrStore),
	}
	svc := newLoaderTestService(t, store)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runSchemaProfileLoad(ctx, svc, loaderTestLogger()); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
