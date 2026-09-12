package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestQueue starts an in-process Redis-protocol server (miniredis) and
// returns a real go-redis client plus a queue over it. The client speaks the
// real Redis wire protocol over TCP; only the server lives in-process.
func newTestQueue(t *testing.T) (*miniredis.Miniredis, *redis.Client, *RedisQueue) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	// miniredis supports blocking commands down to a 1s granularity.
	q := NewRedisQueue(client, "post-test").WithPollTimeout(time.Second)
	return mr, client, q
}

// runLoop starts the loop in the background and returns a stop func.
func runLoop(t *testing.T, l *Loop) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := l.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("loop did not stop")
		}
	}
}

func TestWorkerConsumesAndCompletesARealJob(t *testing.T) {
	_, _, q := newTestQueue(t)
	var completed atomic.Int64
	l := NewLoop(q, WithLogger(slog.New(slog.DiscardHandler)))
	l.Register("smoke", func(_ context.Context, job Job) error {
		completed.Add(1)
		return nil
	})
	stop := runLoop(t, l)
	defer stop()

	err := q.Enqueue(context.Background(), Job{
		ID:            "job-1",
		Type:          "smoke",
		CorrelationID: "corr-1",
		Payload:       json.RawMessage(`{"ref":"rsg/1"}`),
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	waitFor(t, func() bool { return completed.Load() == 1 }, "job completion")

	// The completed job is marked seen (idempotency) and every list is drained.
	seen, err := q.IsSeen(context.Background(), "job-1")
	if err != nil || !seen {
		t.Fatalf("completed job must be marked seen: seen=%v err=%v", seen, err)
	}
	jobs, processing, dead, err := q.Depth(context.Background())
	if err != nil {
		t.Fatalf("Depth: %v", err)
	}
	if jobs != 0 || processing != 0 || dead != 0 {
		t.Errorf("queue not drained: jobs=%d processing=%d dead=%d", jobs, processing, dead)
	}

	// Duplicate delivery of a completed ID must not execute again.
	if err := q.Enqueue(context.Background(), Job{ID: "job-1", Type: "smoke"}); err != nil {
		t.Fatalf("Enqueue dup: %v", err)
	}
	waitFor(t, func() bool {
		jobs, _, _, _ := q.Depth(context.Background())
		return jobs == 0
	}, "duplicate consumed")
	if completed.Load() != 1 {
		t.Errorf("duplicate job executed again: completed=%d, want 1", completed.Load())
	}
}

func TestWorkerRetriesThenDeadLetters(t *testing.T) {
	_, client, q := newTestQueue(t)
	var attempts atomic.Int64
	l := NewLoop(q, WithLogger(slog.New(slog.DiscardHandler)), WithMaxAttempts(3))
	l.Register("flaky", func(_ context.Context, job Job) error {
		attempts.Add(1)
		return errors.New("boom")
	})
	stop := runLoop(t, l)
	defer stop()

	if err := q.Enqueue(context.Background(), Job{ID: "job-2", Type: "flaky"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	waitFor(t, func() bool {
		_, _, dead, _ := q.Depth(context.Background())
		return dead == 1
	}, "dead-letter")

	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3 (max attempts)", got)
	}

	// The dead-letter entry carries the recorded error.
	raw, err := client.LIndex(context.Background(), "post-test:queue:dead", 0).Result()
	if err != nil {
		t.Fatalf("LIndex: %v", err)
	}
	var dead Job
	if err := json.Unmarshal([]byte(raw), &dead); err != nil {
		t.Fatalf("dead entry not a job: %v", err)
	}
	if dead.Error == "" || dead.Attempts != 3 {
		t.Errorf("dead entry = %+v, want recorded error and attempts=3", dead)
	}
}

func TestRecoverSweepsStaleProcessingEntriesBack(t *testing.T) {
	_, client, q := newTestQueue(t)

	// Simulate a crash: a job was moved to processing and never acked.
	if err := q.Enqueue(context.Background(), Job{ID: "job-3", Type: "smoke"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := client.BRPopLPush(context.Background(),
		"post-test:queue:jobs", "post-test:queue:processing", time.Second).Err(); err != nil {
		t.Fatalf("BRPopLPush: %v", err)
	}

	var completed atomic.Int64
	l := NewLoop(q, WithLogger(slog.New(slog.DiscardHandler)))
	l.Register("smoke", func(_ context.Context, job Job) error {
		completed.Add(1)
		return nil
	})
	stop := runLoop(t, l)
	defer stop()

	waitFor(t, func() bool { return completed.Load() == 1 }, "recovered job completion")
	jobs, processing, dead, _ := q.Depth(context.Background())
	if jobs != 0 || processing != 0 || dead != 0 {
		t.Errorf("queue not drained after recovery: jobs=%d processing=%d dead=%d", jobs, processing, dead)
	}
}

func TestUnknownJobTypeIsDeadLettered(t *testing.T) {
	_, _, q := newTestQueue(t)
	l := NewLoop(q, WithLogger(slog.New(slog.DiscardHandler)))
	stop := runLoop(t, l)
	defer stop()

	if err := q.Enqueue(context.Background(), Job{ID: "job-4", Type: "nope"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitFor(t, func() bool {
		_, _, dead, _ := q.Depth(context.Background())
		return dead == 1
	}, "dead-letter of unknown type")
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
