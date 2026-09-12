package persistence

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Open creates a pgx connection pool for url (postgres://user:pass@host/db)
// and verifies connectivity with a ping. Callers own the pool and must call
// pool.Close() when done.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := open(ctx, url, 1)
	if err != nil {
		return nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("persistence: ping: %w", err)
	}
	return pool, nil
}

// OpenLazy creates a pgx connection pool without connecting. It is for
// processes that must start while the database is down (the API, which
// reports database reachability through /readyz instead of refusing to
// start): the pool acquires connections on demand, and a readiness ping
// fails per-request until the database is reachable. Callers own the pool
// and must call pool.Close() when done.
func OpenLazy(ctx context.Context, url string) (*pgxpool.Pool, error) {
	// MinConns=0: the pool never tries to hold a connection in the
	// background, so a down database cannot generate reconnect churn.
	return open(ctx, url, 0)
}

func open(ctx context.Context, url string, minConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("persistence: parse config: %w", err)
	}
	// Modest, explicit defaults; tuned further once SLO work lands (docs/27).
	cfg.MaxConns = 8
	cfg.MinConns = minConns
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("persistence: create pool: %w", err)
	}
	return pool, nil
}
