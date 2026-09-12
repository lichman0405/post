package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisQueue is the V1 job queue: Redis lists with the reliable-queue
// pattern (BRPOPLPUSH into a processing list, explicit ack, recovery sweep
// on start). Redis is the async/cache store per docs/20 §9; this is the
// minimal queue the job loop needs — a heavier Redis-backed queue library
// is an L1 decision left to the event tasks.
type RedisQueue struct {
	client *redis.Client
	prefix string
	poll   time.Duration
}

// NewRedisQueue builds a queue on client, namespacing every key under
// prefix (run-specific isolation per docs/66 §3). Default prefix "post".
func NewRedisQueue(client *redis.Client, prefix string) *RedisQueue {
	if prefix == "" {
		prefix = "post"
	}
	return &RedisQueue{client: client, prefix: prefix, poll: 5 * time.Second}
}

// WithPollTimeout sets how long Next blocks for a job (default 5s); shorter
// polls make shutdown more responsive.
func (q *RedisQueue) WithPollTimeout(d time.Duration) *RedisQueue {
	if d > 0 {
		q.poll = d
	}
	return q
}

func (q *RedisQueue) jobs() string       { return q.prefix + ":queue:jobs" }
func (q *RedisQueue) processing() string { return q.prefix + ":queue:processing" }
func (q *RedisQueue) dead() string       { return q.prefix + ":queue:dead" }
func (q *RedisQueue) seen(id string) string {
	return q.prefix + ":worker:seen:" + id
}

// Enqueue appends a job to the queue (producers use this; the loop uses it
// to schedule retries).
func (q *RedisQueue) Enqueue(ctx context.Context, job Job) error {
	payload, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("worker: encode job %q: %w", job.ID, err)
	}
	return q.client.RPush(ctx, q.jobs(), payload).Err()
}

// Next atomically moves the next job from the queue to the processing list
// (BRPOPLPUSH) and returns it. redis.Nil means no job arrived within the
// poll window.
func (q *RedisQueue) Next(ctx context.Context) (Job, string, error) {
	raw, err := q.client.BRPopLPush(ctx, q.jobs(), q.processing(), q.poll).Result()
	if err != nil {
		return Job{}, "", err
	}
	var job Job
	if err := json.Unmarshal([]byte(raw), &job); err != nil {
		// A malformed entry can never be processed: park it for inspection
		// and keep the loop alive.
		_ = q.client.LRem(ctx, q.processing(), 1, raw).Err()
		_ = q.client.RPush(ctx, q.dead(), raw).Err()
		return Job{}, "", fmt.Errorf("worker: malformed queue entry: %w", err)
	}
	return job, raw, nil
}

// Ack removes the in-flight copy from the processing list.
func (q *RedisQueue) Ack(ctx context.Context, raw string) error {
	return q.client.LRem(ctx, q.processing(), 1, raw).Err()
}

// IsSeen reports whether job id already completed (the idempotency mark).
func (q *RedisQueue) IsSeen(ctx context.Context, id string) (bool, error) {
	n, err := q.client.Exists(ctx, q.seen(id)).Result()
	return n > 0, err
}

// MarkSeen records the completion of job id. The mark is written only after
// a successful run (see Loop.process); it expires so the seen set cannot
// grow without bound.
func (q *RedisQueue) MarkSeen(ctx context.Context, id string, ttl time.Duration) error {
	return q.client.Set(ctx, q.seen(id), "1", ttl).Err()
}

// DeadLetter parks a permanently failed job with its error recorded.
func (q *RedisQueue) DeadLetter(ctx context.Context, job Job) error {
	payload, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("worker: encode dead-letter %q: %w", job.ID, err)
	}
	return q.client.RPush(ctx, q.dead(), payload).Err()
}

// Recover sweeps stale in-flight entries back onto the queue. It runs once
// at loop start: entries left in the processing list by a crash are the
// only delivery loss risk of the reliable-queue pattern, and this closes
// it. Completed-then-lost entries are skipped via the idempotency mark.
func (q *RedisQueue) Recover(ctx context.Context) error {
	for {
		if _, err := q.client.RPopLPush(ctx, q.processing(), q.jobs()).Result(); err != nil {
			if errors.Is(err, redis.Nil) {
				return nil
			}
			return err
		}
		// One stale entry recovered; continue sweeping.
	}
}

// Depth reports the current queue lengths (jobs, processing, dead).
func (q *RedisQueue) Depth(ctx context.Context) (jobs, processing, dead int64, err error) {
	jobs, err = q.client.LLen(ctx, q.jobs()).Result()
	if err != nil {
		return 0, 0, 0, err
	}
	processing, err = q.client.LLen(ctx, q.processing()).Result()
	if err != nil {
		return 0, 0, 0, err
	}
	dead, err = q.client.LLen(ctx, q.dead()).Result()
	return jobs, processing, dead, err
}
