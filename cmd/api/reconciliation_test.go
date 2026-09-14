package main

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/gitprovider"
)

// The T0309 job-side wiring in the API process: the sweep loop around the
// reconciler — one pass at startup, one per tick, survives a failing
// store, stops with the context. The store is a counting fake; the pass
// policy itself is covered by the reconciler unit tests and the
// integration suite.

// countingReconcilerStore counts pass attempts (GitStates is read once per
// pass) and can fail them.
type countingReconcilerStore struct {
	mu    sync.Mutex
	count int
	err   error
}

func (s *countingReconcilerStore) BeginRun(context.Context) (string, error) {
	return "run-1", nil
}

func (s *countingReconcilerStore) FinishRun(context.Context, gitprovider.RunSummary) (int, error) {
	return 0, nil
}

func (s *countingReconcilerStore) MustExistRefs(context.Context) ([]gitprovider.CheckedRef, error) {
	return nil, nil
}

func (s *countingReconcilerStore) MustBeGoneRefs(context.Context) ([]gitprovider.CheckedRef, error) {
	return nil, nil
}

func (s *countingReconcilerStore) GitStates(context.Context) ([]gitprovider.GitStateCheck, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count++
	return nil, s.err
}

func (s *countingReconcilerStore) MappingViolations(context.Context) ([]gitprovider.MappingViolation, error) {
	return nil, nil
}

func (s *countingReconcilerStore) ProvisionedRepos(context.Context) ([]gitprovider.ProvisionedRepo, error) {
	return nil, nil
}

func (s *countingReconcilerStore) BranchNames(context.Context, string) (map[string]bool, error) {
	return nil, nil
}

func (s *countingReconcilerStore) RecordFinding(context.Context, string, gitprovider.NewFinding) (bool, error) {
	return false, nil
}

func (s *countingReconcilerStore) ResolveFindings(context.Context, []gitprovider.FindingKey, []gitprovider.FindingKind) (int, error) {
	return 0, nil
}

func (s *countingReconcilerStore) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

func (s *countingReconcilerStore) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// TestRunReconciliationSweepBootPassAndTick: the loop sweeps once
// immediately at startup (the boot pass checks anything drifted while the
// API was down), again on the ticker, and stops with the context.
func TestRunReconciliationSweepBootPassAndTick(t *testing.T) {
	oldInterval := reconciliationInterval
	reconciliationInterval = 20 * time.Millisecond
	t.Cleanup(func() { reconciliationInterval = oldInterval })

	store := &countingReconcilerStore{}
	rec := gitprovider.NewReconciler(nil, store) // canonical-only pass: no provider wired
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		runReconciliationSweep(ctx, rec, slog.New(slog.DiscardHandler))
		close(done)
	}()

	waitFor(t, func() bool { return store.calls() >= 2 })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reconciliation sweep did not stop with the context")
	}
}

// TestRunReconciliationSweepFailureIsNotFatal: a failing store pass is
// logged and the loop keeps running — the next tick retries (a crashed
// pass leaves its run row unfinished, visible evidence, never silence).
func TestRunReconciliationSweepFailureIsNotFatal(t *testing.T) {
	oldInterval := reconciliationInterval
	reconciliationInterval = 20 * time.Millisecond
	t.Cleanup(func() { reconciliationInterval = oldInterval })

	store := &countingReconcilerStore{}
	store.setErr(context.DeadlineExceeded)
	rec := gitprovider.NewReconciler(nil, store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runReconciliationSweep(ctx, rec, slog.New(slog.DiscardHandler))

	// The boot pass fails, the loop survives, and the next tick retries.
	waitFor(t, func() bool { return store.calls() >= 2 })
	cancel()
}
