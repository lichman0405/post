package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/lichman0405/post/internal/gitprovider"
	"github.com/lichman0405/post/internal/worker"
)

// The T0301 job-side wiring in the API process: the worker.Handler for
// ProvisionJobType, the enqueue-side job builder, and the startup sweep.
// Fakes stand in for the provider and the canonical store — the real ones
// run in the integration suite against live Gitea + PostgreSQL.

// fakeProvisionPort is a stub GitPort answering with a fixed repository.
type fakeProvisionPort struct{}

func (f *fakeProvisionPort) EnsureRepository(_ context.Context, spec gitprovider.RepositorySpec) (gitprovider.Repository, error) {
	return gitprovider.Repository{Owner: "svc", Name: spec.Name, ID: 1}, nil
}

func (f *fakeProvisionPort) GetRepository(context.Context, string, string) (gitprovider.Repository, error) {
	return gitprovider.Repository{}, gitprovider.ErrNotFound
}

func (f *fakeProvisionPort) EnsureWebhook(_ context.Context, spec gitprovider.WebhookSpec) (gitprovider.Webhook, error) {
	return gitprovider.Webhook{ID: 9, Active: true}, nil
}

func (f *fakeProvisionPort) EnsureInitialMain(_ context.Context, repo gitprovider.Repository) (string, error) {
	return "sha-seed", nil
}

func (f *fakeProvisionPort) EnsureMainProtection(_ context.Context, repo gitprovider.Repository, _ gitprovider.MainProtectionSpec) (gitprovider.MainProtection, error) {
	return gitprovider.MainProtection{
		RuleName:              "main",
		DirectPushBlocked:     true,
		ForcePushBlocked:      true,
		MergeWhitelistEnabled: true,
		MergeWhitelist:        []string{"svc"},
	}, nil
}

func (f *fakeProvisionPort) GetMainProtection(context.Context, gitprovider.Repository) (gitprovider.MainProtection, error) {
	return gitprovider.MainProtection{}, gitprovider.ErrNotFound
}

// The branch-ref methods (T0303) are unreachable from the provisioning
// tests but the port contract requires them.
func (f *fakeProvisionPort) EnsureBranch(context.Context, gitprovider.BranchSpec) (gitprovider.BranchRef, error) {
	return gitprovider.BranchRef{}, gitprovider.ErrNotFound
}

func (f *fakeProvisionPort) GetBranch(context.Context, gitprovider.Repository, string) (gitprovider.BranchRef, error) {
	return gitprovider.BranchRef{}, gitprovider.ErrNotFound
}

// ListBranches (T0309) is unreachable from the provisioning tests but the
// port contract requires it.
func (f *fakeProvisionPort) ListBranches(context.Context, gitprovider.Repository) ([]gitprovider.BranchRef, error) {
	return nil, gitprovider.ErrNotFound
}

func (f *fakeProvisionPort) DeleteBranch(context.Context, gitprovider.Repository, string) error {
	return nil
}

// ImportBranch (T0804) is unreachable from the provisioning tests.
func (f *fakeProvisionPort) ImportBranch(context.Context, gitprovider.ImportBranchSpec) (gitprovider.BranchRef, error) {
	return gitprovider.BranchRef{}, gitprovider.ErrNotFound
}

// The push-ingestion methods (T0305) are unreachable from the provisioning
// tests but the port contract requires them.
func (f *fakeProvisionPort) ChangedFiles(context.Context, gitprovider.Repository, string, string) ([]gitprovider.FileChange, error) {
	return nil, gitprovider.ErrNotFound
}

func (f *fakeProvisionPort) ReadFile(context.Context, gitprovider.Repository, string, string) ([]byte, error) {
	return nil, gitprovider.ErrNotFound
}

// fakeProvisionStore is the canonical-store fake: backlog lists are
// scripted, Provision invokes the provisioning function (the store
// contract) and records its outcome.
type fakeProvisionStore struct {
	backlog  []gitprovider.PendingProject
	listErr  error
	skipped  bool
	calls    []string
	fnErr    error
	recorded bool
}

func (f *fakeProvisionStore) ProvisioningBacklog(context.Context) ([]gitprovider.PendingProject, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.backlog, nil
}

func (f *fakeProvisionStore) Provision(_ context.Context, projectID string, fn func(gitprovider.PendingProject) (*gitprovider.ProvisionRecord, error)) (bool, bool, error) {
	f.calls = append(f.calls, projectID)
	if f.skipped {
		return false, true, nil
	}
	rec, err := fn(gitprovider.PendingProject{ID: projectID, Slug: "slug", Name: "Name"})
	if err != nil {
		f.fnErr = err
		return false, false, err
	}
	f.recorded = rec != nil
	return true, false, nil
}

func newProvisioningTestQueue(t *testing.T) *worker.RedisQueue {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return worker.NewRedisQueue(client, provisioningQueuePrefix)
}

const provisionTestProjectID = "c9f0f895-8b6d-4b16-9c0e-1f2a3b4c5d6e"

// TestProvisioningHandlerProvisionsJob: a valid payload reaches the
// provisioner, which performs the full policy through the port and the
// store — the handler returns nil (job done, no retry).
func TestProvisioningHandlerProvisionsJob(t *testing.T) {
	port := &fakeProvisionPort{}
	store := &fakeProvisionStore{}
	handler := newProvisioningHandler(gitprovider.NewProvisioner(port, store, "http://host/x"))

	payload, err := json.Marshal(gitprovider.ProvisionJobPayload{ProjectID: provisionTestProjectID})
	if err != nil {
		t.Fatal(err)
	}
	if err := handler(t.Context(), worker.Job{ID: "job-1", Type: gitprovider.ProvisionJobType, Payload: payload}); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(store.calls) != 1 || store.calls[0] != provisionTestProjectID {
		t.Errorf("store calls = %v, want one call with the payload's project id", store.calls)
	}
	if !store.recorded {
		t.Error("no provision record produced")
	}
}

// TestProvisioningHandlerPropagatesProvisioningFailure: a provisioning
// failure surfaces to the loop (which retries), never swallowed — and an
// already-provisioned skip is still a success.
func TestProvisioningHandlerPropagatesProvisioningFailure(t *testing.T) {
	port := &fakeProvisionPort{}

	// Redelivered job on a provisioned project: the store skips, done.
	skipped := &fakeProvisionStore{skipped: true}
	handler := newProvisioningHandler(gitprovider.NewProvisioner(port, skipped, "http://host/x"))
	payload, _ := json.Marshal(gitprovider.ProvisionJobPayload{ProjectID: provisionTestProjectID})
	if err := handler(t.Context(), worker.Job{ID: "job-1", Type: gitprovider.ProvisionJobType, Payload: payload}); err != nil {
		t.Fatalf("skipped job: %v", err)
	}

	// A project id that cannot become a repository name: the provisioner's
	// conflict surfaces through the handler.
	store := &fakeProvisionStore{}
	handler2 := newProvisioningHandler(gitprovider.NewProvisioner(port, store, "http://host/x"))
	err := handler2(t.Context(), worker.Job{ID: "job-2", Type: gitprovider.ProvisionJobType, Payload: []byte(`{"project_id":"not-a-uuid"}`)})
	if err == nil || !errors.Is(err, gitprovider.ErrConflict) {
		t.Errorf("handler error = %v, want ErrConflict from the provisioner", err)
	}
}

// TestProvisioningHandlerRejectsBadJobs: a malformed payload or an empty
// project id is a permanent failure — the error names the job and the
// provisioner is never reached (the loop dead-letters these).
func TestProvisioningHandlerRejectsBadJobs(t *testing.T) {
	port := &fakeProvisionPort{}
	store := &fakeProvisionStore{}
	handler := newProvisioningHandler(gitprovider.NewProvisioner(port, store, "http://host/x"))

	cases := []struct {
		name    string
		payload []byte
		want    string
	}{
		{"bad json", []byte(`{`), "bad payload"},
		{"empty project id", []byte(`{"project_id":""}`), "empty project_id"},
		{"missing project id", []byte(`{}`), "empty project_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := handler(t.Context(), worker.Job{ID: "job-x", Type: gitprovider.ProvisionJobType, Payload: tc.payload})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "job-x") {
				t.Errorf("error %q does not name the job (dead-letter inspection)", err)
			}
		})
	}
	if len(store.calls) != 0 {
		t.Errorf("rejected jobs reached the provisioner: %v", store.calls)
	}
}

// TestNewProvisionJob: the enqueue-side job carries the type, a generated
// id, the correlation id and identity-only payload.
func TestNewProvisionJob(t *testing.T) {
	job, err := newProvisionJob(provisionTestProjectID, "corr-1")
	if err != nil {
		t.Fatalf("newProvisionJob: %v", err)
	}
	if job.Type != gitprovider.ProvisionJobType {
		t.Errorf("type = %q", job.Type)
	}
	if job.ID == "" {
		t.Error("job id is empty")
	}
	if job.CorrelationID != "corr-1" {
		t.Errorf("correlation id = %q", job.CorrelationID)
	}
	var payload gitprovider.ProvisionJobPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.ProjectID != provisionTestProjectID {
		t.Errorf("payload project_id = %q", payload.ProjectID)
	}
}

// TestEnqueuePendingProvisioning: the startup sweep turns the provisioning
// backlog ('pending' and 'failed' rows) into queued jobs — one per project.
func TestEnqueuePendingProvisioning(t *testing.T) {
	q := newProvisioningTestQueue(t)
	store := &fakeProvisionStore{backlog: []gitprovider.PendingProject{
		{ID: provisionTestProjectID, Slug: "a", Name: "A"},
		{ID: "22222222-3333-4333-8444-555555555555", Slug: "b", Name: "B"},
	}}
	enqueuePendingProvisioning(t.Context(), store, q, slog.New(slog.DiscardHandler))

	var got []worker.Job
	for i := 0; i < 2; i++ {
		job, _, err := q.Next(t.Context())
		if err != nil {
			t.Fatalf("queue Next %d: %v", i, err)
		}
		got = append(got, job)
	}
	if len(got) != 2 {
		t.Fatalf("queued jobs = %d, want 2", len(got))
	}
	seen := map[string]string{}
	for _, job := range got {
		if job.Type != gitprovider.ProvisionJobType {
			t.Errorf("job type = %q", job.Type)
		}
		var payload gitprovider.ProvisionJobPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			t.Errorf("payload: %v", err)
		}
		seen[payload.ProjectID] = job.CorrelationID
	}
	if seen[provisionTestProjectID] != "startup-sweep" {
		t.Errorf("sweep job correlation = %q, want startup-sweep", seen[provisionTestProjectID])
	}
	if len(seen) != 2 {
		t.Errorf("swept projects = %v, want both pending ids", seen)
	}
	if jobs, _, _, _ := q.Depth(t.Context()); jobs != 0 {
		t.Errorf("queue depth after drain = %d, want 0", jobs)
	}
}

// TestEnqueuePendingProvisioningEmptyAndFailingStore: an empty backlog
// enqueues nothing, a failing store logs and enqueues nothing (no panic —
// the next API start retries).
func TestEnqueuePendingProvisioningEmptyAndFailingStore(t *testing.T) {
	q := newProvisioningTestQueue(t)
	enqueuePendingProvisioning(t.Context(), &fakeProvisionStore{}, q, slog.New(slog.DiscardHandler))
	if jobs, _, _, _ := q.Depth(t.Context()); jobs != 0 {
		t.Errorf("empty backlog enqueued %d jobs", jobs)
	}

	enqueuePendingProvisioning(t.Context(), &fakeProvisionStore{listErr: errors.New("pg down")}, q, slog.New(slog.DiscardHandler))
	if jobs, _, _, _ := q.Depth(t.Context()); jobs != 0 {
		t.Errorf("failing store enqueued %d jobs", jobs)
	}
}
