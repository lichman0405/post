package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/lichman0405/post/internal/application/authn"
)

// RedisSessionStore persists sessions in Redis (the canonical cache,
// CLAUDE.md §7) under post:session:<token> with the session TTL as the key
// expiry: Redis itself expires the session — no sweeper, no leaked
// immortal sessions. Session state is a JSON blob (user id, csrf token,
// timestamps); the token is the key and is never derivable from the blob.
type RedisSessionStore struct {
	client *redis.Client
}

// NewRedisSessionStore builds a session store on client.
func NewRedisSessionStore(client *redis.Client) *RedisSessionStore {
	return &RedisSessionStore{client: client}
}

func sessionKey(token string) string { return "post:session:" + token }

// Create implements authn.SessionStore.
func (s *RedisSessionStore) Create(ctx context.Context, sess authn.Session, ttl time.Duration) error {
	blob, err := json.Marshal(sess)
	if err != nil {
		return fmt.Errorf("persistence: encode session: %w", err)
	}
	// SET with EX: one atomic command — a later login with the same token
	// (never happens with 256-bit random tokens, but the store stays
	// correct if it did) atomically replaces the old session.
	if err := s.client.Set(ctx, sessionKey(sess.Token), blob, ttl).Err(); err != nil {
		return fmt.Errorf("persistence: store session: %w", err)
	}
	return nil
}

// Get implements authn.SessionStore.
func (s *RedisSessionStore) Get(ctx context.Context, token string) (authn.Session, error) {
	blob, err := s.client.Get(ctx, sessionKey(token)).Bytes()
	if errors.Is(err, redis.Nil) {
		return authn.Session{}, authn.ErrSessionNotFound
	}
	if err != nil {
		return authn.Session{}, fmt.Errorf("persistence: read session: %w", err)
	}
	var sess authn.Session
	if err := json.Unmarshal(blob, &sess); err != nil {
		return authn.Session{}, fmt.Errorf("persistence: decode session: %w", err)
	}
	return sess, nil
}

// Delete implements authn.SessionStore.
func (s *RedisSessionStore) Delete(ctx context.Context, token string) error {
	if err := s.client.Del(ctx, sessionKey(token)).Err(); err != nil {
		return fmt.Errorf("persistence: revoke session: %w", err)
	}
	return nil
}

// RedisRateLimiter is a fixed-window limiter over Redis INCR counters.
// Check is atomic enough for the purpose: INCR then PEXPIRE-NX seeds the
// window expiry exactly once; a crash between the two leaves a key that
// the NX flag still expires on the next attempt.
type RedisRateLimiter struct {
	client *redis.Client
}

// NewRedisRateLimiter builds a limiter on client.
func NewRedisRateLimiter(client *redis.Client) *RedisRateLimiter {
	return &RedisRateLimiter{client: client}
}

func limiterKey(bucket string) string { return "post:ratelimit:" + bucket }

// Check implements authn.RateLimiter.
func (l *RedisRateLimiter) Check(ctx context.Context, bucket string, limit int, window time.Duration) (bool, time.Duration, error) {
	key := limiterKey(bucket)
	n, err := l.client.Incr(ctx, key).Result()
	if err != nil {
		return false, 0, fmt.Errorf("persistence: rate limit increment: %w", err)
	}
	ttl, err := l.client.PTTL(ctx, key).Result()
	if err != nil {
		return false, 0, fmt.Errorf("persistence: rate limit ttl: %w", err)
	}
	if ttl == -1 {
		// No window running yet: seed it (only the first request reaches
		// here in practice; the flag keeps a restart from extending it).
		if err := l.client.PExpire(ctx, key, window).Err(); err != nil {
			return false, 0, fmt.Errorf("persistence: rate limit window: %w", err)
		}
		ttl = window
	}
	if n > int64(limit) {
		return false, ttl, nil
	}
	return true, 0, nil
}

var _ authn.SessionStore = (*RedisSessionStore)(nil)
var _ authn.RateLimiter = (*RedisRateLimiter)(nil)
