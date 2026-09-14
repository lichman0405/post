package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/lichman0405/post/internal/config"
	"github.com/lichman0405/post/internal/gitprovider"
)

// The T0309 Git ↔ RSG reconciliation loop: runs one verification pass at
// startup and then every reconciliationInterval — the background job of
// docs/16 §5 that checks drift between the canonical store and the
// GitProvider. Every drift is a high-severity finding (a row in
// git_reconciliation_findings plus a system audit row) carrying a repair
// proposal; the reconciler NEVER repairs — the proposal is recorded, not
// applied (acceptance criterion "repair proposal 不静默修").
//
// Failure policy, per pass: a canonical-store failure aborts the pass and
// the next tick retries; a provider failure is recorded on the run row
// (provider_error) and only the provider-side checks are skipped — a down
// provider is not mistaken for missing refs.

// reconciliationInterval is how often the drift check runs. Any drift is
// high severity (docs/16 §5), so the window stays small — the same cadence
// family as the main-protection sweep. A var so tests shrink the tick.
var reconciliationInterval = 5 * time.Minute

// reconciliationTimeout bounds one pass: the provider-side checks are one
// call per ref, and a provider outage must not hold the loop forever —
// the next tick retries anyway.
const reconciliationTimeout = 2 * time.Minute

// runReconciliationSweep runs one pass immediately (the boot pass checks
// anything drifted while the API was down) and then on the ticker until
// ctx ends.
func runReconciliationSweep(ctx context.Context, rec *gitprovider.Reconciler, log *slog.Logger) {
	pass := func(ctx context.Context) {
		cctx, cancel := context.WithTimeout(ctx, reconciliationTimeout)
		defer cancel()
		sum, err := rec.Reconcile(cctx)
		if err != nil {
			log.Error("git reconciliation: pass failed (retried next tick)",
				"error", config.RedactForOutput(err.Error()))
			return
		}
		attrs := []any{
			"refs_checked", sum.RefsChecked,
			"states_checked", sum.StatesChecked,
			"mapping_violations", sum.MappingViolations,
			"repositories_checked", sum.RepositoriesChecked,
			"findings_opened", sum.FindingsOpened,
			"findings_resolved", sum.FindingsResolved,
			"findings_open", sum.FindingsOpen,
		}
		if sum.ProviderError != "" {
			attrs = append(attrs, "provider_error", config.RedactForOutput(sum.ProviderError))
		}
		log.Info("git reconciliation: pass complete", attrs...)
		if sum.FindingsOpened > 0 {
			log.Error("git reconciliation: drift detected — high-severity findings recorded (repair proposals NOT applied)",
				"opened", sum.FindingsOpened, "run_id", sum.RunID)
		}
	}

	pass(ctx)
	ticker := time.NewTicker(reconciliationInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pass(ctx)
		}
	}
}
