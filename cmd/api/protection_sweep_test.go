package main

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/gitprovider"
)

// TestRunMainProtectionSweepBootPassAndTick: the loop sweeps once
// immediately at startup (the boot pass heals anything drifted while the
// API was down), again on the ticker, and stops with the context.
func TestRunMainProtectionSweepBootPassAndTick(t *testing.T) {
	oldInterval := protectionSweepInterval
	protectionSweepInterval = 20 * time.Millisecond
	t.Cleanup(func() { protectionSweepInterval = oldInterval })

	sweeper, store := newSweepTestDoubles()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		runMainProtectionSweep(ctx, sweeper, slog.New(slog.DiscardHandler))
		close(done)
	}()

	waitFor(t, func() bool { return store.calls() >= 2 })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sweep loop did not stop with the context")
	}
}

// TestRunMainProtectionSweepFailureIsNotFatal: a failing store pass is
// logged and the loop keeps running — the next tick retries.
func TestRunMainProtectionSweepFailureIsNotFatal(t *testing.T) {
	oldInterval := protectionSweepInterval
	protectionSweepInterval = 20 * time.Millisecond
	t.Cleanup(func() { protectionSweepInterval = oldInterval })

	sweeper, store := newSweepTestDoubles()
	store.setErr(context.DeadlineExceeded)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runMainProtectionSweep(ctx, sweeper, slog.New(slog.DiscardHandler))

	// The boot pass fails, the loop survives, and the next tick retries.
	waitFor(t, func() bool { return store.calls() >= 2 })
	cancel()
}

// waitFor polls cond until it holds or the deadline expires.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !cond() {
		t.Fatal("condition not reached within 5s")
	}
}

// newSweepTestDoubles wires a counting fake store around a sweeper whose
// port always succeeds.
func newSweepTestDoubles() (*gitprovider.ProtectionSweeper, *countingProtectionStore) {
	store := &countingProtectionStore{}
	return gitprovider.NewProtectionSweeper(&noopProtectionPort{}, store), store
}

type countingProtectionStore struct {
	mu    sync.Mutex
	count int
	err   error
}

func (s *countingProtectionStore) ProvisionedRepositories(context.Context) ([]gitprovider.Repository, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count++
	return nil, s.err
}

func (s *countingProtectionStore) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

func (s *countingProtectionStore) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

type noopProtectionPort struct{}

func (noopProtectionPort) EnsureMainProtection(_ context.Context, _ gitprovider.Repository, _ gitprovider.MainProtectionSpec) (gitprovider.MainProtection, error) {
	return gitprovider.MainProtection{}, nil
}

func (noopProtectionPort) GetMainProtection(context.Context, gitprovider.Repository) (gitprovider.MainProtection, error) {
	return gitprovider.MainProtection{}, gitprovider.ErrNotFound
}
