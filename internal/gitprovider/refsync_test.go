package gitprovider_test

import (
	"context"
	"errors"
	"testing"

	"github.com/lichman0405/post/internal/gitprovider"
)

// fakeRefStore is a scripted BranchRefStore: it invokes the sync function
// the store contract passes in (recording the branch), honours the skipped
// flag, and can fail outright (a store-level failure).
type fakeRefStore struct {
	row      gitprovider.PendingBranchRef
	skipped  bool
	storeErr error
	calls    []string
	record   *gitprovider.BranchRefRecord
	fnErr    error
}

func (f *fakeRefStore) BranchRefBacklog(context.Context) ([]gitprovider.PendingBranchRef, error) {
	return nil, nil
}

func (f *fakeRefStore) SyncBranchRef(_ context.Context, branchID string, fn func(gitprovider.PendingBranchRef) (*gitprovider.BranchRefRecord, error)) (bool, bool, error) {
	f.calls = append(f.calls, branchID)
	if f.storeErr != nil {
		return false, false, f.storeErr
	}
	if f.skipped {
		return false, true, nil
	}
	p := f.row
	if p.BranchID == "" {
		p.BranchID = branchID
	}
	rec, err := fn(p)
	if err != nil {
		f.fnErr = err
		return false, false, err
	}
	f.record = rec
	return true, false, nil
}

const testBranchID = "b1f0f895-8b6d-4b16-9c0e-1f2a3b4c5d6e"
const testHeadSHA = "0123456789abcdef0123456789abcdef01234567"

// pendingCreate is a create-direction work item (fork point from the
// canonical store: the base state's git commit).
func pendingCreate(forkSHA string) gitprovider.PendingBranchRef {
	return gitprovider.PendingBranchRef{
		BranchID: testBranchID,
		GitRef:   "refs/heads/feature-x",
		Name:     "feature-x",
		Owner:    "post-git-svc",
		Repo:     "p-" + testBranchID,
		ForkSHA:  forkSHA,
	}
}

// pendingClose is a close-direction work item (lifecycle terminal).
func pendingClose() gitprovider.PendingBranchRef {
	p := pendingCreate("")
	p.Close = true
	return p
}

func newTestSyncer(port gitprovider.GitPort, store gitprovider.BranchRefStore) *gitprovider.BranchRefSyncer {
	return gitprovider.NewBranchRefSyncer(port, store)
}

// TestBranchRefSyncCreatesFromBaseSHA: the semantic fork point (the base
// state's git_commit_sha) is the ref's fork — no repository read, the
// record carries the sha back for the canonical store.
func TestBranchRefSyncCreatesFromBaseSHA(t *testing.T) {
	port := &fakePort{branch: gitprovider.BranchRef{Name: "feature-x", HeadSHA: testHeadSHA}}
	store := &fakeRefStore{row: pendingCreate(testHeadSHA)}
	err := newTestSyncer(port, store).Sync(t.Context(), testBranchID)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(port.branchs) != 1 {
		t.Fatalf("EnsureBranch calls = %d, want 1", len(port.branchs))
	}
	got := port.branchs[0]
	if got.Name != "feature-x" || got.ForkRef != testHeadSHA {
		t.Errorf("EnsureBranch spec = %+v, want Name=feature-x ForkRef=%s", got, testHeadSHA)
	}
	if got.Repository.Owner != "post-git-svc" || got.Repository.Name != "p-"+testBranchID {
		t.Errorf("EnsureBranch repository = %+v", got.Repository)
	}
	if port.getRepoSet {
		t.Error("a recorded fork point must not re-read the repository")
	}
	if store.record == nil || store.record.HeadSHA != testHeadSHA || store.record.ForkSHA != testHeadSHA {
		t.Errorf("record = %+v, want head and fork = %s", store.record, testHeadSHA)
	}
}

// TestBranchRefSyncFallsBackToDefaultBranch: no recorded git_commit_sha
// (the pre-T0305 path) — the branch forks the repository's default branch,
// and the record leaves the semantic fork point empty (the store keeps
// fork_sha NULL: "forked the default branch").
func TestBranchRefSyncFallsBackToDefaultBranch(t *testing.T) {
	port := &fakePort{
		branch:     gitprovider.BranchRef{Name: "feature-x", HeadSHA: testHeadSHA},
		getRepoSet: true,
		getRepo:    gitprovider.Repository{Owner: "post-git-svc", Name: "p-" + testBranchID, DefaultBranch: "main"},
	}
	store := &fakeRefStore{row: pendingCreate("")}
	err := newTestSyncer(port, store).Sync(t.Context(), testBranchID)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(port.branchs) != 1 || port.branchs[0].ForkRef != "main" {
		t.Errorf("EnsureBranch spec = %+v, want ForkRef=main (the default branch)", port.branchs)
	}
	if store.record == nil || store.record.ForkSHA != "" {
		t.Errorf("record = %+v, want empty semantic fork point (default-branch fallback)", store.record)
	}
}

// TestBranchRefSyncEmptyRepositoryFails: the repository has no refs at all
// (T0301 provisions with auto_init false) — ErrNotFound surfaces and the
// row stays retryable until the initial commit lands (T0302).
func TestBranchRefSyncEmptyRepositoryFails(t *testing.T) {
	port := &fakePort{
		getRepoSet: true,
		getRepo:    gitprovider.Repository{Owner: "post-git-svc", Name: "p-" + testBranchID}, // DefaultBranch ""
	}
	store := &fakeRefStore{row: pendingCreate("")}
	err := newTestSyncer(port, store).Sync(t.Context(), testBranchID)
	if !errors.Is(err, gitprovider.ErrNotFound) {
		t.Errorf("Sync error = %v, want ErrNotFound", err)
	}
	if len(port.branchs) != 0 {
		t.Errorf("an empty repository must not be asked to create a branch: %+v", port.branchs)
	}
}

// TestBranchRefSyncCloseRecordsHeadBeforeDelete: the close strategy — the
// final head is read (and recorded) BEFORE the ref is deleted.
func TestBranchRefSyncCloseRecordsHeadBeforeDelete(t *testing.T) {
	port := &fakePort{branch: gitprovider.BranchRef{Name: "feature-x", HeadSHA: testHeadSHA}}
	store := &fakeRefStore{row: pendingClose()}
	err := newTestSyncer(port, store).Sync(t.Context(), testBranchID)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(port.branchGot) != 1 || port.branchGot[0] != "feature-x" {
		t.Errorf("GetBranch calls = %v, want [feature-x]", port.branchGot)
	}
	if len(port.deleted) != 1 || port.deleted[0] != "feature-x" {
		t.Errorf("DeleteBranch calls = %v, want [feature-x]", port.deleted)
	}
	if len(port.branchs) != 0 {
		t.Errorf("close direction must never create a ref: %+v", port.branchs)
	}
	if store.record == nil || store.record.HeadSHA != testHeadSHA {
		t.Errorf("record = %+v, want the final head %s recorded before deletion", store.record, testHeadSHA)
	}
}

// TestBranchRefSyncCloseAlreadyGoneStillDeletes: a missing ref is the goal
// already achieved, and the delete still runs — idempotent, and covering a
// ref that appears between the read and the delete.
func TestBranchRefSyncCloseAlreadyGoneStillDeletes(t *testing.T) {
	port := &fakePort{getBranchErr: gitprovider.ErrNotFound}
	store := &fakeRefStore{row: pendingClose()}
	err := newTestSyncer(port, store).Sync(t.Context(), testBranchID)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(port.deleted) != 1 {
		t.Errorf("DeleteBranch calls = %d, want 1 (idempotent delete)", len(port.deleted))
	}
	if store.record == nil || store.record.HeadSHA != "" {
		t.Errorf("record = %+v, want empty head (the row keeps its last recorded one)", store.record)
	}
}

// TestBranchRefSyncCloseDeleteNotFoundIsSuccess: a port that reports an
// already-absent ref as ErrNotFound from DeleteBranch is still a success —
// "the ref must not exist" is the goal, and the delete still ran (the
// unconditional delete covers a ref appearing between the read and the
// delete).
func TestBranchRefSyncCloseDeleteNotFoundIsSuccess(t *testing.T) {
	port := &fakePort{getBranchErr: gitprovider.ErrNotFound, deleteErr: gitprovider.ErrNotFound}
	store := &fakeRefStore{row: pendingClose()}
	err := newTestSyncer(port, store).Sync(t.Context(), testBranchID)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(port.deleted) != 1 {
		t.Errorf("DeleteBranch calls = %d, want 1 (the unconditional delete)", len(port.deleted))
	}
	if store.record == nil || store.record.HeadSHA != "" {
		t.Errorf("record = %+v, want empty head (the row keeps its last recorded one)", store.record)
	}
}

// TestBranchRefSyncCloseReadFailureRetries: a transient read failure must
// not delete a ref whose head was never recorded — the whole step retries.
func TestBranchRefSyncCloseReadFailureRetries(t *testing.T) {
	port := &fakePort{getBranchErr: gitprovider.ErrUnavailable}
	store := &fakeRefStore{row: pendingClose()}
	err := newTestSyncer(port, store).Sync(t.Context(), testBranchID)
	if !errors.Is(err, gitprovider.ErrUnavailable) {
		t.Errorf("Sync error = %v, want ErrUnavailable", err)
	}
	if len(port.deleted) != 0 {
		t.Errorf("DeleteBranch ran %d times after a failed head read", len(port.deleted))
	}
}

// TestBranchRefSyncCloseDeleteFailurePropagates: the deletion itself can
// fail — the error surfaces, the store marks the row failed, the boot
// sweep retries.
func TestBranchRefSyncCloseDeleteFailurePropagates(t *testing.T) {
	port := &fakePort{
		branch:    gitprovider.BranchRef{Name: "feature-x", HeadSHA: testHeadSHA},
		deleteErr: gitprovider.ErrUnavailable,
	}
	store := &fakeRefStore{row: pendingClose()}
	err := newTestSyncer(port, store).Sync(t.Context(), testBranchID)
	if !errors.Is(err, gitprovider.ErrUnavailable) {
		t.Errorf("Sync error = %v, want ErrUnavailable", err)
	}
}

// TestBranchRefSyncProviderFailurePropagates: a create-direction provider
// outage surfaces as its sentinel.
func TestBranchRefSyncProviderFailurePropagates(t *testing.T) {
	port := &fakePort{branchErr: gitprovider.ErrUnavailable}
	store := &fakeRefStore{row: pendingCreate(testHeadSHA)}
	err := newTestSyncer(port, store).Sync(t.Context(), testBranchID)
	if !errors.Is(err, gitprovider.ErrUnavailable) {
		t.Errorf("Sync error = %v, want ErrUnavailable", err)
	}
}

// TestBranchRefSyncSkippedIsNoop: an already-synced branch (store skip)
// never touches the provider.
func TestBranchRefSyncSkippedIsNoop(t *testing.T) {
	port := &fakePort{}
	store := &fakeRefStore{skipped: true}
	err := newTestSyncer(port, store).Sync(t.Context(), testBranchID)
	if err != nil {
		t.Fatalf("Sync (skipped) = %v, want nil", err)
	}
	if len(port.branchs) != 0 || len(port.deleted) != 0 {
		t.Errorf("skipped sync touched the provider: creates=%d deletes=%d", len(port.branchs), len(port.deleted))
	}
}

// TestBranchRefSyncStoreFailurePropagates: a store-level failure surfaces
// untouched.
func TestBranchRefSyncStoreFailurePropagates(t *testing.T) {
	port := &fakePort{}
	store := &fakeRefStore{storeErr: errors.New("pg down")}
	err := newTestSyncer(port, store).Sync(t.Context(), testBranchID)
	if err == nil || err.Error() != "pg down" {
		t.Errorf("Sync error = %v, want the store failure", err)
	}
}
