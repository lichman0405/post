package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/lichman0405/post/internal/config"
	"github.com/lichman0405/post/internal/gitprovider"
	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/worker"
)

// The T0303 branch-ref pipeline: the git_branch_refs backlog
// (sync_state IN ('pending','failed','closing')) is swept at startup into
// branch-ref-sync jobs, and a worker loop in this process keeps every
// semantic branch's provider ref consistent with its lifecycle — created
// on branch creation (via migration 00031's trigger, whatever path
// inserted the row), deleted once the branch is merged or aborted.
//
// The loop runs inside the API on the same queue and prefix as T0301's
// provisioning loop (one loop, two job types): the queue prefix isolates
// both from the scaffold's smoke jobs, and the same fold-into-cmd/worker
// note applies when cmd/worker grows real handlers.

// newBranchRefSyncHandler builds the worker.Handler for BranchRefJobType:
// the payload carries the branch id (identity only, docs/52 §17), the
// syncer derives everything else. A malformed payload is a permanent
// failure — the retry loop dead-letters it for inspection.
func newBranchRefSyncHandler(s *gitprovider.BranchRefSyncer) worker.Handler {
	return func(ctx context.Context, job worker.Job) error {
		var payload gitprovider.BranchRefJobPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return fmt.Errorf("branch-ref-sync job %s: bad payload: %w", job.ID, err)
		}
		if payload.BranchID == "" {
			return fmt.Errorf("branch-ref-sync job %s: empty branch_id", job.ID)
		}
		return s.Sync(ctx, payload.BranchID)
	}
}

// newBranchRefSyncJob builds the enqueue-side job for one branch.
func newBranchRefSyncJob(branchID, correlationID string) (worker.Job, error) {
	id, err := observability.NewRandomID()
	if err != nil {
		return worker.Job{}, err
	}
	payload, err := json.Marshal(gitprovider.BranchRefJobPayload{BranchID: branchID})
	if err != nil {
		return worker.Job{}, err
	}
	return worker.Job{
		ID:            id,
		Type:          gitprovider.BranchRefJobType,
		CorrelationID: correlationID,
		Payload:       payload,
	}, nil
}

// enqueuePendingBranchRefs back-fills jobs for every branch in the sync
// backlog ('pending' plus 'failed' plus 'closing', see BranchRefBacklog):
// a branch created while Redis (or the enqueue path) was down would
// otherwise wait forever, and a branch whose last attempt failed would
// stay wedged — git_branch_refs.sync_state is the work list, so a startup
// sweep makes ref sync self-healing without a scheduler. Best effort: a
// failure here is logged; the sweep runs again on the next API start.
func enqueuePendingBranchRefs(ctx context.Context, store gitprovider.BranchRefStore, queue *worker.RedisQueue, log *slog.Logger) {
	backlog, err := store.BranchRefBacklog(ctx)
	if err != nil {
		log.Error("branch-ref: backlog sweep failed (branches may wait until the next API start)",
			"error", config.RedactForOutput(err.Error()))
		return
	}
	for _, b := range backlog {
		job, err := newBranchRefSyncJob(b.BranchID, "startup-sweep")
		if err != nil {
			log.Error("branch-ref: cannot build sweep job", "branch_id", b.BranchID, "error", err)
			continue
		}
		if err := queue.Enqueue(ctx, job); err != nil {
			log.Error("branch-ref: sweep enqueue failed (retried on the next API start)",
				"branch_id", b.BranchID, "error", config.RedactForOutput(err.Error()))
			return
		}
	}
	if len(backlog) > 0 {
		log.Info("branch-ref: backlog sweep enqueued", "count", len(backlog))
	}
}
