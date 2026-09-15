package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/lichman0405/post/internal/config"
	"github.com/lichman0405/post/internal/gitprovider"
	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/worker"
)

// The T0301 provisioning pipeline: project creation enqueues a
// project-provision job, a worker loop in this process consumes it and
// provisions the GitProvider repository + push webhook through
// internal/gitprovider.
//
// The loop runs inside the API because cmd/worker is not a product job
// consumer yet (it only runs the scaffold "smoke" job); the queue prefix
// below isolates the two loops so neither can steal or dead-letter the
// other's jobs. When cmd/worker grows real handlers, this loop and its
// prefix fold into it (see T0301 RESULT follow_up_issues).

// provisioningQueuePrefix namespaces the API-owned job queue, separate
// from cmd/worker's "post" scaffold queue (docs/66 §3 run isolation).
const provisioningQueuePrefix = "post-api"

// gitIdentityTimeout bounds the one provider call the API makes while
// starting up (T0409): resolving the service identity the merge's ref guard
// accepts for main. A provider that is slow or down must delay startup by at
// most this, never hold it open — the merge command fails that half closed
// and records an unfinished Git step until an operator restarts with the
// provider reachable.
const gitIdentityTimeout = 5 * time.Second

// newProvisioningHandler builds the worker.Handler for ProvisionJobType:
// the payload carries the project id (identity only, docs/52 §17), the
// provisioner derives everything else. A malformed payload is a
// permanent failure — the retry loop dead-letters it for inspection.
func newProvisioningHandler(p *gitprovider.Provisioner) worker.Handler {
	return func(ctx context.Context, job worker.Job) error {
		var payload gitprovider.ProvisionJobPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return fmt.Errorf("provision job %s: bad payload: %w", job.ID, err)
		}
		if payload.ProjectID == "" {
			return fmt.Errorf("provision job %s: empty project_id", job.ID)
		}
		return p.Provision(ctx, payload.ProjectID)
	}
}

// newProvisionJob builds the enqueue-side job for one project.
func newProvisionJob(projectID, correlationID string) (worker.Job, error) {
	id, err := observability.NewRandomID()
	if err != nil {
		return worker.Job{}, err
	}
	payload, err := json.Marshal(gitprovider.ProvisionJobPayload{ProjectID: projectID})
	if err != nil {
		return worker.Job{}, err
	}
	return worker.Job{
		ID:            id,
		Type:          gitprovider.ProvisionJobType,
		CorrelationID: correlationID,
		Payload:       payload,
	}, nil
}

// enqueuePendingProvisioning back-fills jobs for every project in the
// provisioning backlog ('pending' plus 'failed', see ProvisioningBacklog):
// a project created while Redis (or the enqueue path) was down would
// otherwise wait forever, and a project whose last attempt failed would
// stay wedged — provision_status is the work list (00019), so a startup
// sweep makes provisioning self-healing without a scheduler. Best effort:
// a failure here is logged; the sweep runs again on the next API start.
func enqueuePendingProvisioning(ctx context.Context, store gitprovider.ProvisionStore, queue *worker.RedisQueue, log *slog.Logger) {
	backlog, err := store.ProvisioningBacklog(ctx)
	if err != nil {
		log.Error("provisioning: backlog sweep failed (projects may wait until the next API start)",
			"error", config.RedactForOutput(err.Error()))
		return
	}
	for _, p := range backlog {
		job, err := newProvisionJob(p.ID, "startup-sweep")
		if err != nil {
			log.Error("provisioning: cannot build sweep job", "project_id", p.ID, "error", err)
			continue
		}
		if err := queue.Enqueue(ctx, job); err != nil {
			log.Error("provisioning: sweep enqueue failed (retried on the next API start)",
				"project_id", p.ID, "error", config.RedactForOutput(err.Error()))
			return
		}
	}
	if len(backlog) > 0 {
		log.Info("provisioning: backlog sweep enqueued", "count", len(backlog))
	}
}
