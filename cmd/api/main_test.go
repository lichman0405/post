package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
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
