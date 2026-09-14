package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/gitprovider"
	"github.com/lichman0405/post/internal/worker"
)

// The T0303 job-side wiring in the API process: the worker.Handler for
// BranchRefJobType, the enqueue-side job builder, and the startup sweep.
// Fakes stand in for the provider and the canonical store — the real ones
// run in the integration suite against live Gitea + PostgreSQL.

// fakeRefPort is a stub GitPort answering with a fixed branch ref.
type fakeRefPort struct {
	branch    gitprovider.BranchRef
	branchErr error
}

func (f *fakeRefPort) EnsureRepository(context.Context, gitprovider.RepositorySpec) (gitprovider.Repository, error) {
	return gitprovider.Repository{}, gitprovider.ErrNotFound
}

func (f *fakeRefPort) GetRepository(context.Context, string, string) (gitprovider.Repository, error) {
	return gitprovider.Repository{}, gitprovider.ErrNotFound
}

func (f *fakeRefPort) EnsureWebhook(context.Context, gitprovider.WebhookSpec) (gitprovider.Webhook, error) {
	return gitprovider.Webhook{}, gitprovider.ErrNotFound
}

func (f *fakeRefPort) EnsureBranch(_ context.Context, _ gitprovider.BranchSpec) (gitprovider.BranchRef, error) {
	if f.branchErr != nil {
		return gitprovider.BranchRef{}, f.branchErr
	}
	return f.branch, nil
}

func (f *fakeRefPort) GetBranch(context.Context, gitprovider.Repository, string) (gitprovider.BranchRef, error) {
	return f.branch, nil
}

func (f *fakeRefPort) DeleteBranch(context.Context, gitprovider.Repository, string) error {
	return nil
}

// The main-protection methods (T0302) are unreachable from the ref-sync
// tests but the port contract requires them.
func (f *fakeRefPort) EnsureInitialMain(context.Context, gitprovider.Repository) (string, error) {
	return "", gitprovider.ErrNotFound
}

func (f *fakeRefPort) EnsureMainProtection(context.Context, gitprovider.Repository, gitprovider.MainProtectionSpec) (gitprovider.MainProtection, error) {
	return gitprovider.MainProtection{}, gitprovider.ErrNotFound
}

func (f *fakeRefPort) GetMainProtection(context.Context, gitprovider.Repository) (gitprovider.MainProtection, error) {
	return gitprovider.MainProtection{}, gitprovider.ErrNotFound
}

// The push-ingestion methods (T0305) are unreachable from the ref-sync
// tests but the port contract requires them.
func (f *fakeRefPort) ChangedFiles(context.Context, gitprovider.Repository, string, string) ([]gitprovider.FileChange, error) {
	return nil, gitprovider.ErrNotFound
}

func (f *fakeRefPort) ReadFile(context.Context, gitprovider.Repository, string, string) ([]byte, error) {
	return nil, gitprovider.ErrNotFound
}

// fakeRefSyncStore is the canonical-store fake: SyncBranchRef invokes the
// sync function (the store contract) and records its outcome.
type fakeRefSyncStore struct {
	skipped  bool
	calls    []string
	fnErr    error
	recorded bool
}

func (f *fakeRefSyncStore) BranchRefBacklog(context.Context) ([]gitprovider.PendingBranchRef, error) {
	return nil, nil
}

func (f *fakeRefSyncStore) SyncBranchRef(_ context.Context, branchID string, fn func(gitprovider.PendingBranchRef) (*gitprovider.BranchRefRecord, error)) (bool, bool, error) {
	f.calls = append(f.calls, branchID)
	if f.skipped {
		return false, true, nil
	}
	rec, err := fn(gitprovider.PendingBranchRef{
		BranchID: branchID,
		GitRef:   "refs/heads/feature-x",
		Name:     "feature-x",
		Owner:    "post-git-svc",
		Repo:     "p-" + branchID,
		ForkSHA:  refsyncTestHeadSHA,
	})
	if err != nil {
		f.fnErr = err
		return false, false, err
	}
	f.recorded = rec != nil
	return true, false, nil
}

const refsyncTestBranchID = "b1f0f895-8b6d-4b16-9c0e-1f2a3b4c5d6e"
const refsyncTestHeadSHA = "0123456789abcdef0123456789abcdef01234567"

// TestBranchRefSyncHandlerSyncsJob: a valid payload reaches the syncer,
// which performs the full policy through the port and the store — the
// handler returns nil (job done, no retry).
func TestBranchRefSyncHandlerSyncsJob(t *testing.T) {
	port := &fakeRefPort{branch: gitprovider.BranchRef{Name: "feature-x", HeadSHA: "0123456789abcdef0123456789abcdef01234567"}}
	store := &fakeRefSyncStore{}
	handler := newBranchRefSyncHandler(gitprovider.NewBranchRefSyncer(port, store))

	payload, err := json.Marshal(gitprovider.BranchRefJobPayload{BranchID: refsyncTestBranchID})
	if err != nil {
		t.Fatal(err)
	}
	if err := handler(t.Context(), worker.Job{ID: "job-1", Type: gitprovider.BranchRefJobType, Payload: payload}); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(store.calls) != 1 || store.calls[0] != refsyncTestBranchID {
		t.Errorf("store calls = %v, want one call with the payload's branch id", store.calls)
	}
	if !store.recorded {
		t.Error("no branch-ref record produced")
	}
}

// TestBranchRefSyncHandlerPropagatesSyncFailure: a sync failure surfaces to
// the loop (which retries), never swallowed — and an already-synced skip is
// still a success.
func TestBranchRefSyncHandlerPropagatesSyncFailure(t *testing.T) {
	// Redelivered job on a synced branch: the store skips, done.
	skipped := &fakeRefSyncStore{skipped: true}
	handler := newBranchRefSyncHandler(gitprovider.NewBranchRefSyncer(&fakeRefPort{}, skipped))
	payload, _ := json.Marshal(gitprovider.BranchRefJobPayload{BranchID: refsyncTestBranchID})
	if err := handler(t.Context(), worker.Job{ID: "job-1", Type: gitprovider.BranchRefJobType, Payload: payload}); err != nil {
		t.Fatalf("skipped job: %v", err)
	}

	// A provider outage inside the sync function surfaces through the
	// handler for the loop's retry policy — the job errors, the store
	// records the failed attempt (its fnErr), nothing is "recorded".
	store := &fakeRefSyncStore{}
	handler2 := newBranchRefSyncHandler(gitprovider.NewBranchRefSyncer(
		&fakeRefPort{branchErr: gitprovider.ErrUnavailable}, store))
	err := handler2(t.Context(), worker.Job{ID: "job-2", Type: gitprovider.BranchRefJobType, Payload: payload})
	if !errors.Is(err, gitprovider.ErrUnavailable) {
		t.Errorf("handler error = %v, want ErrUnavailable from the syncer", err)
	}
	if store.fnErr == nil || !errors.Is(store.fnErr, gitprovider.ErrUnavailable) {
		t.Errorf("store fnErr = %v, want the provider failure recorded", store.fnErr)
	}
	if store.recorded {
		t.Error("a failed sync recorded a branch-ref outcome")
	}
}

// TestBranchRefSyncHandlerRejectsBadJobs: a malformed payload or an empty
// branch id is a permanent failure — the error names the job and the
// syncer is never reached (the loop dead-letters these).
func TestBranchRefSyncHandlerRejectsBadJobs(t *testing.T) {
	store := &fakeRefSyncStore{}
	handler := newBranchRefSyncHandler(gitprovider.NewBranchRefSyncer(&fakeRefPort{}, store))

	cases := []struct {
		name    string
		payload []byte
		want    string
	}{
		{"bad json", []byte(`{`), "bad payload"},
		{"empty branch id", []byte(`{"branch_id":""}`), "empty branch_id"},
		{"missing branch id", []byte(`{}`), "empty branch_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := handler(t.Context(), worker.Job{ID: "job-x", Type: gitprovider.BranchRefJobType, Payload: tc.payload})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "job-x") {
				t.Errorf("error %q does not name the job (dead-letter inspection)", err)
			}
		})
	}
	if len(store.calls) != 0 {
		t.Errorf("rejected jobs reached the syncer: %v", store.calls)
	}
}

// TestNewBranchRefSyncJob: the enqueue-side job carries the type, a
// generated id, the correlation id and identity-only payload.
func TestNewBranchRefSyncJob(t *testing.T) {
	job, err := newBranchRefSyncJob(refsyncTestBranchID, "corr-1")
	if err != nil {
		t.Fatalf("newBranchRefSyncJob: %v", err)
	}
	if job.Type != gitprovider.BranchRefJobType {
		t.Errorf("type = %q", job.Type)
	}
	if job.ID == "" {
		t.Error("job id is empty")
	}
	if job.CorrelationID != "corr-1" {
		t.Errorf("correlation id = %q", job.CorrelationID)
	}
	var payload gitprovider.BranchRefJobPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.BranchID != refsyncTestBranchID {
		t.Errorf("payload branch_id = %q", payload.BranchID)
	}
}

// fakeRefBacklogStore is the backlog side of the store fake (the sweep only
// reads BranchRefBacklog).
type fakeRefBacklogStore struct {
	backlog []gitprovider.PendingBranchRef
	listErr error
}

func (f *fakeRefBacklogStore) BranchRefBacklog(context.Context) ([]gitprovider.PendingBranchRef, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.backlog, nil
}

func (f *fakeRefBacklogStore) SyncBranchRef(context.Context, string, func(gitprovider.PendingBranchRef) (*gitprovider.BranchRefRecord, error)) (bool, bool, error) {
	return false, true, nil
}

// TestEnqueuePendingBranchRefs: the startup sweep turns the sync backlog
// ('pending', 'failed' and 'closing' rows) into queued jobs — one per
// branch.
func TestEnqueuePendingBranchRefs(t *testing.T) {
	q := newProvisioningTestQueue(t)
	store := &fakeRefBacklogStore{backlog: []gitprovider.PendingBranchRef{
		{BranchID: refsyncTestBranchID, GitRef: "refs/heads/a", Name: "a"},
		{BranchID: "22222222-3333-4333-8444-555555555555", GitRef: "refs/heads/b", Name: "b", Close: true},
	}}
	enqueuePendingBranchRefs(t.Context(), store, q, slog.New(slog.DiscardHandler))

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
		if job.Type != gitprovider.BranchRefJobType {
			t.Errorf("job type = %q", job.Type)
		}
		var payload gitprovider.BranchRefJobPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			t.Errorf("payload: %v", err)
		}
		seen[payload.BranchID] = job.CorrelationID
	}
	if seen[refsyncTestBranchID] != "startup-sweep" {
		t.Errorf("sweep job correlation = %q, want startup-sweep", seen[refsyncTestBranchID])
	}
	if len(seen) != 2 {
		t.Errorf("swept branches = %v, want both backlog ids", seen)
	}
	if jobs, _, _, _ := q.Depth(t.Context()); jobs != 0 {
		t.Errorf("queue depth after drain = %d, want 0", jobs)
	}
}

// TestEnqueuePendingBranchRefsEmptyAndFailingStore: an empty backlog
// enqueues nothing, a failing store logs and enqueues nothing (no panic —
// the next API start retries).
func TestEnqueuePendingBranchRefsEmptyAndFailingStore(t *testing.T) {
	q := newProvisioningTestQueue(t)
	enqueuePendingBranchRefs(t.Context(), &fakeRefBacklogStore{}, q, slog.New(slog.DiscardHandler))
	if jobs, _, _, _ := q.Depth(t.Context()); jobs != 0 {
		t.Errorf("empty backlog enqueued %d jobs", jobs)
	}

	enqueuePendingBranchRefs(t.Context(), &fakeRefBacklogStore{listErr: errors.New("pg down")}, q, slog.New(slog.DiscardHandler))
	if jobs, _, _, _ := q.Depth(t.Context()); jobs != 0 {
		t.Errorf("failing store enqueued %d jobs", jobs)
	}
}
