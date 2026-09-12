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
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("persistence: parse config: %w", err)
	}
	// Modest, explicit defaults; tuned further once SLO work lands (docs/27).
	cfg.MaxConns = 8
	cfg.MinConns = 1
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("persistence: create pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("persistence: ping: %w", err)
	}
	return pool, nil
}
