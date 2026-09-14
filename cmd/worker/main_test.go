package main

import (
	"sync"
	"testing"
)

// TestShutdownOrderCancelThenJoinThenClose pins the shutdown contract
// run() composes: cancel the root context first (the dispatcher observes
// ctx.Done and exits), join the dispatcher goroutine, and only then close
// the database pool. A regression that closes before the join — the T1001
// review finding — fails here deterministically.
func TestShutdownOrderCancelThenJoinThenClose(t *testing.T) {
	var mu sync.Mutex
	var steps []string
	record := func(s string) {
		mu.Lock()
		steps = append(steps, s)
		mu.Unlock()
	}

	stopped := make(chan struct{})
	stop := func() {
		record("stop")
		close(stopped)
	}

	// The join models dispatcherWG.Wait(): it returns only once the
	// context is cancelled (the dispatcher's Run observes ctx.Done).
	release := make(chan struct{})
	join := func() {
		<-stopped
		<-release
		record("join")
	}

	closePool := func() { record("close") }

	done := make(chan struct{})
	go func() {
		shutdown(stop, join, closePool)
		close(done)
	}()
	close(release)
	<-done

	mu.Lock()
	defer mu.Unlock()
	want := []string{"stop", "join", "close"}
	if len(steps) != len(want) {
		t.Fatalf("steps = %v, want %v", steps, want)
	}
	for i := range want {
		if steps[i] != want[i] {
			t.Fatalf("steps = %v, want %v", steps, want)
		}
	}
}
