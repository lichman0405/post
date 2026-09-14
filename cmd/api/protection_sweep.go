package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/lichman0405/post/internal/config"
	"github.com/lichman0405/post/internal/gitprovider"
)

// The T0302 platform-layer enforcement loop: runs the main-protection
// sweep once at startup and then every protectionSweepInterval. The Gitea
// rule is layer one; this loop is layer two — a rule an operator removed
// or drifted in the provider UI is re-applied by the platform on the next
// pass, without a restart or a scheduler. Failures are per-repository and
// logged redacted (the adapter errors never carry credentials); a store
// failure aborts the pass and the next tick retries.

// protectionSweepInterval is how often every provisioned repository's main
// rule is re-verified (docs/16 §3: drift on main is a high-severity
// condition, so the window stays small). A var so tests shrink the tick.
var protectionSweepInterval = 10 * time.Minute

// protectionSweepTimeout bounds one pass: a provider outage must not hold
// the loop forever, and the next tick retries anyway.
const protectionSweepTimeout = 30 * time.Second

// runMainProtectionSweep sweeps immediately (the boot pass heals anything
// drifted while the API was down) and then on the ticker until ctx ends.
func runMainProtectionSweep(ctx context.Context, sweeper *gitprovider.ProtectionSweeper, log *slog.Logger) {
	sweep := func(ctx context.Context) {
		cctx, cancel := context.WithTimeout(ctx, protectionSweepTimeout)
		defer cancel()
		ensured, failures, err := sweeper.Sweep(cctx)
		if err != nil {
			log.Error("main protection sweep: store pass failed (retried next tick)",
				"error", config.RedactForOutput(err.Error()))
			return
		}
		for _, f := range failures {
			log.Error("main protection sweep: repository failed (retried next tick)",
				"error", config.RedactForOutput(f.Error()))
		}
		if len(failures) > 0 || ensured > 0 {
			log.Info("main protection sweep: pass complete",
				"ensured", ensured, "failed", len(failures))
		}
	}

	sweep(ctx)
	ticker := time.NewTicker(protectionSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep(ctx)
		}
	}
}
