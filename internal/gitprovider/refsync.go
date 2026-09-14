package gitprovider

import (
	"context"
	"errors"
	"fmt"
)

// The branch-ref sync application service (T0303): one semantic branch
// (branches.git_ref = refs/heads/<name>, derived 1:1 from the name) → one
// provider-side ref, created when the branch row appears and deleted when
// the semantic lifecycle closes (docs/43: active → merged | aborted —
// both terminal, both delete the ref after recording its final head).
//
// All policy lives here; the provider calls go through GitPort and every
// canonical-store fact through BranchRefStore. The mapping rows are
// maintained by migration 00031's triggers — branch creation and the
// mapping can never drift apart — and the backlog
// (sync_state IN ('pending','failed','closing')) is the work list, exactly
// like provisioning's provision_status: a boot sweep plus redelivered jobs
// re-attempt, with no hot retry loop. The database additionally enforces
// the whole state machine for ANY update path (00031's
// git_branch_ref_guard).

// BranchRefJobType is the Redis job type the API's startup sweep enqueues
// for every backlog row (consumed by the loop wired in cmd/api/main.go).
// A branch-creation API (a later task) enqueues the same type on create;
// until then the sweep is the only enqueue path — and the only one needed,
// because the backlog is derived from the canonical rows, never from
// events.
const BranchRefJobType = "branch-ref-sync"

// BranchRefJobPayload is the payload of a branch-ref-sync job. It carries
// identity only (docs/52 §17): the branch id — the ref name, fork point,
// owner and repository are all derived server-side from the canonical
// store.
type BranchRefJobPayload struct {
	BranchID string `json:"branch_id"`
}

// PendingBranchRef is the work item the store derives for one backlog row:
// everything the syncer needs to name the provider call. Direction is the
// row's own state: Close means the semantic lifecycle is terminal
// (close_requested_at is set) and the ref must be DELETED; otherwise the
// ref must EXIST.
type PendingBranchRef struct {
	// BranchID is the canonical branches.id.
	BranchID string
	// GitRef is the derived ref the provider must carry (refs/heads/<name>).
	GitRef string
	// Name is the branch name (the ref without the refs/heads/ prefix).
	Name string
	// Owner and Repo name the provisioned provider repository
	// (git_repository_provisions). Rows whose project is not provisioned
	// yet never enter the backlog.
	Owner string
	Repo  string
	// ForkSHA is the base state's git_commit_sha when recorded (T0305
	// fills those on ingestion); empty falls back to the repository's
	// default branch — the pre-T0305 path, where every branch forks the
	// accepted head.
	ForkSHA string
	// Close is the direction: true = delete the ref (lifecycle terminal),
	// false = ensure the ref exists.
	Close bool
}

// BranchRefRecord is the outcome of one successful sync step: the
// provider-side facts the store commits. On close, HeadSHA is the final
// head recorded BEFORE the deletion ("" when the ref was already gone) —
// the reconstruction pointer for the closed path.
type BranchRefRecord struct {
	BranchID string
	ForkSHA  string
	HeadSHA  string
}

// BranchRefStore is the canonical-store port the syncer needs. The
// concrete adapter is *PGBranchRefStore in this package.
type BranchRefStore interface {
	// BranchRefBacklog lists every mapping row awaiting sync: pending
	// (never attempted), failed (last attempt failed) and closing (the
	// semantic branch is terminal and the ref must be deleted). The work
	// list the boot sweep and redelivered jobs derive from.
	BranchRefBacklog(ctx context.Context) ([]PendingBranchRef, error)
	// SyncBranchRef serializes one sync attempt per branch: the row is
	// locked, already-synced rows are skipped, fn does the provider work,
	// and its outcome is committed atomically with the state transition
	// (see *PGBranchRefStore.SyncBranchRef).
	SyncBranchRef(ctx context.Context, branchID string, fn func(PendingBranchRef) (*BranchRefRecord, error)) (synced, skipped bool, err error)
}

// BranchRefSyncer keeps semantic branches and their provider refs
// consistent: creates missing refs, adopts existing ones (recording the
// actual head — the ref is the truth), and deletes refs of closed
// branches after recording their final head.
type BranchRefSyncer struct {
	port  GitPort
	store BranchRefStore
}

// NewBranchRefSyncer wires the syncer.
func NewBranchRefSyncer(port GitPort, store BranchRefStore) *BranchRefSyncer {
	return &BranchRefSyncer{port: port, store: store}
}

// Sync runs one sync attempt for one branch through the store's
// compare-and-swap. Idempotent: an already-synced branch is skipped, and
// every provider-side step is itself idempotent (EnsureBranch adopts,
// DeleteBranch tolerates a missing ref), so job redelivery never fails on
// work that is already done. A failed row stays in the store's backlog —
// the boot sweep (or a redelivered job) re-attempts it.
func (s *BranchRefSyncer) Sync(ctx context.Context, branchID string) error {
	_, _, err := s.store.SyncBranchRef(ctx, branchID, func(p PendingBranchRef) (*BranchRefRecord, error) {
		return s.sync(ctx, p)
	})
	return err
}

// sync performs the provider work for one branch while the store holds the
// row lock. It returns the record to commit; an error aborts and the store
// records the row as failed.
func (s *BranchRefSyncer) sync(ctx context.Context, p PendingBranchRef) (*BranchRefRecord, error) {
	repo := Repository{Owner: p.Owner, Name: p.Repo}
	if p.Close {
		// Close direction (merged/aborted, docs/43): record the final head,
		// then delete. The read is best-effort — a transient read failure
		// retries the whole step, a missing ref is the goal already
		// achieved — and the delete is unconditional: it runs even when the
		// read found nothing (a closed semantic branch must not keep
		// accepting pushes) and an ErrNotFound from it is still a success —
		// the ref does not exist, which is exactly the goal. A redelivered
		// close job therefore never fails on work that is already done.
		var head string
		ref, err := s.port.GetBranch(ctx, repo, p.Name)
		switch {
		case err == nil:
			head = ref.HeadSHA
		case errors.Is(err, ErrNotFound):
			// Already gone: the row keeps its last recorded head.
		default:
			return nil, err
		}
		if err := s.port.DeleteBranch(ctx, repo, p.Name); err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		return &BranchRefRecord{BranchID: p.BranchID, HeadSHA: head}, nil
	}

	// Create direction: fork from the semantic fork point when the
	// canonical store recorded one (the base state's git_commit_sha),
	// otherwise from the repository's default branch — the pre-T0305
	// fallback, where every branch forks the accepted head.
	forkRef := p.ForkSHA
	if forkRef == "" {
		repoInfo, err := s.port.GetRepository(ctx, p.Owner, p.Repo)
		if err != nil {
			return nil, err
		}
		forkRef = repoInfo.DefaultBranch
		if forkRef == "" {
			return nil, fmt.Errorf("%w: repository %s/%s has no refs to fork from (the initial commit arrives with T0302)",
				ErrNotFound, p.Owner, p.Repo)
		}
	}
	ref, err := s.port.EnsureBranch(ctx, BranchSpec{
		Repository: repo,
		Name:       p.Name,
		ForkRef:    forkRef,
	})
	if err != nil {
		return nil, err
	}
	// ForkSHA stays the SEMANTIC fork point: the record carries it only
	// when the canonical store had one. The default-branch fallback is not
	// a sha — the store leaves fork_sha NULL, meaning "forked the
	// repository's default branch".
	return &BranchRefRecord{BranchID: p.BranchID, ForkSHA: p.ForkSHA, HeadSHA: ref.HeadSHA}, nil
}
