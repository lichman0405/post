package gitprovider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The external fork's content import (T0804).
//
// A fork is not a copy of rows — it is a copy of CONTENT, and the platform
// has exactly one rule about content that arrives through the Git side
// (docs/16 §4): whatever brings content in has to run the same inspection a
// push runs, so the branch's semantic completeness flag is derived from
// real evidence rather than from a default.
//
// So the import does not write an ingestion record of its own shape. It
// inspects the copied content with the SAME ingester the push webhook
// feeds (PushIngester.Inspect — one implementation of the check, so the
// two paths cannot classify differently), records the delivery against the
// fork's repository and ref, and copies the parent's commit into the
// fork's repository over the git protocol — which is what makes the
// content real on the provider side. The row that lands is
// indistinguishable from one a real push would have produced. The flag
// therefore derives exactly as docs/16 §4.1 requires: unparseable content
// marks the branch unstructured_changes and the PR gate refuses it,
// because a check genuinely ran and genuinely failed — not because the
// import guessed.
//
// What is inspected is the copy's DIVERGENCE, not the copy's whole tree.
// A fork branch's tree is the parent's tree; diffed against the fork
// branch's own empty history every path in it is "new", including the
// platform's project bootstrap file, which no ingestion wrote and no later
// push can remove — the branch would be marked unstructured_changes for
// good, the outcome docs/16 §4.1 forbids. With a recorded fork point
// (git_branch_refs.fork_sha) the copy contributes what the line it copies
// diverged by: that state's content diffed against the copied commit. With
// none — a line created without a base state's commit, or the repository's
// default branch — no content baseline exists, and the copy contributes the
// source line's OWN recorded evidence at the copied commit instead
// (IngestStore.SourceLineEvidence: the walk the line's own completeness
// flag is derived by).
//
// That is the invariant this file exists to keep: at the moment of the
// import the copied branch's flag is never CLEANER than the source line's.
// Push an unparseable file to a line and the line is marked
// unstructured_changes — and so is everything forked from it, instead of a
// copy that carries the same content and merges it. Neither mark is a life
// sentence: it is ordinary evidence, and a later push whose record for the
// same path is nearer to the head clears it, which is the one rule that
// clears it anywhere.
//
// The no-fork-point route answers "no baseline" with evidence, never with a
// default value, and it does not mark the copy unstructured_changes
// wholesale either: a clean line's copy carries that line's manifest
// records and comes out clean, so a legitimate fork is not locked behind a
// mark only a real push could clear (docs/16 §4.1). What it cannot cover is
// content the platform never ingested — a source head no delivery recorded
// (a webhook that has not arrived, a ref adopted outside the platform): the
// walk finds no row and contributes nothing, exactly as the source line's
// own flag is derived from that same absence.
//
// The delivery is idempotent in the same place every other delivery is:
// the ingestion row's dedupe key (gitea_repo_id, git_ref, after_sha).
// Import records the row BEFORE the copy lands, so the provider's own push
// webhook for this very copy — production's normal case, the push is a
// real push to a real repository — collapses onto the same row and cannot
// record a whole-tree inspection of its own. A retried import collapses
// onto it too.

// ForkRepoRef is the provider-side identity of one project's repository,
// read from the canonical provision row. The project id travels with it
// because the repository coordinates are never identity (ADR-003).
type ForkRepoRef struct {
	ProjectID string
	Owner     string
	Name      string
	RepoID    int64
}

// ForkImportStore is the canonical-store port the fork importer reads.
// The concrete adapter is *PGForkImportStore in this package.
type ForkImportStore interface {
	// ForkRepo returns the repository a project is provisioned as,
	// ErrRepoNotProvisioned when the project has no provision row yet.
	ForkRepo(ctx context.Context, projectID string) (ForkRepoRef, error)
	// SourceForkPoint returns the provider-side commit the ref gitRef in
	// projectID diverged from — git_branch_refs.fork_sha, the semantic
	// fork point T0303's ref syncer records from the base state's commit.
	// Empty when none is recorded: the branch was created without a base
	// state's commit (the syncer's default-branch fallback records no
	// fork point) or has no ref record yet. Empty is NOT "this line is
	// the baseline and a copy of it contributes nothing" — it is "no
	// content baseline exists", and Import answers it with the source
	// line's own recorded evidence rather than with a default.
	SourceForkPoint(ctx context.Context, projectID, gitRef string) (string, error)
}

// PGForkImportStore reads the provision rows the fork import needs. Plain
// pgx, like the ingestion store beside it: the columns belong to this
// package.
type PGForkImportStore struct {
	pool *pgxpool.Pool
}

// NewForkImportStore builds the store on pool.
func NewForkImportStore(pool *pgxpool.Pool) *PGForkImportStore {
	return &PGForkImportStore{pool: pool}
}

// ForkRepo implements ForkImportStore.
func (s *PGForkImportStore) ForkRepo(ctx context.Context, projectID string) (ForkRepoRef, error) {
	var out ForkRepoRef
	err := s.pool.QueryRow(ctx,
		`SELECT project_id::text, owner, name, gitea_repo_id
		   FROM git_repository_provisions WHERE project_id = $1`,
		projectID).Scan(&out.ProjectID, &out.Owner, &out.Name, &out.RepoID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ForkRepoRef{}, fmt.Errorf("%w: project %s", ErrRepoNotProvisioned, projectID)
	}
	if err != nil {
		return ForkRepoRef{}, err
	}
	return out, nil
}

// SourceForkPoint implements ForkImportStore: the recorded content
// baseline of one ref. A branch with no ref row yet — or one created
// without a base state's commit — answers empty rather than erroring; the
// import then carries the source line's own evidence, which is a read of
// the canonical record and not a default (see Import).
func (s *PGForkImportStore) SourceForkPoint(ctx context.Context, projectID, gitRef string) (string, error) {
	var forkSHA *string
	err := s.pool.QueryRow(ctx,
		`SELECT r.fork_sha
		   FROM branches b
		   JOIN git_branch_refs r ON r.branch_id = b.id
		  WHERE b.project_id = $1 AND b.git_ref = $2`,
		projectID, gitRef).Scan(&forkSHA)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if forkSHA == nil {
		return "", nil
	}
	return *forkSHA, nil
}

// ForkImportRequest names one fork's content copy. Every field is an
// identity the caller already holds; the repository coordinates, the
// commit and the pre-import head are all resolved server-side.
type ForkImportRequest struct {
	// SourceProjectID / TargetProjectID are the parent project and the
	// fork project. Content travels from the source's repository into the
	// target's.
	SourceProjectID string
	TargetProjectID string
	// SourceRef is the ref in the source repository to import
	// (refs/heads/<name>). Empty means the source repository's default
	// branch — the parent's accepted line, which is what a fork is of.
	SourceRef string
	// TargetBranch is the branch name the content lands on in the fork
	// project's repository (the semantic branch the caller has already
	// created there: the ingestion resolves the branch row from
	// refs/heads/<TargetBranch>).
	TargetBranch string
	// ActorID is the user whose request ran the import — the forker. It is
	// the imported transition's actor (T0817): the copy lands the branch on
	// the parent's content, which is a state transition like any other, and
	// a transition names who it was made for. Required — the platform never
	// records a transition on nobody's behalf.
	ActorID string
}

// ForkImportResult is the outcome of one import: the commit that was
// copied, the head the target ref carried before it, and whether this call
// wrote the ingestion row (false when the delivery was already recorded —
// a retry, or the provider's own webhook having arrived first).
type ForkImportResult struct {
	// SourceSHA is the parent-side commit the fork was taken at — the
	// value the lineage row records as the fork point.
	SourceSHA string
	// BeforeSHA is the target ref's head before the import, ZerosSHA when
	// the ref did not exist. It is the delivery's `before`, so the head
	// pointer advance is guarded against a concurrent push exactly as a
	// webhook's is.
	BeforeSHA string
	// TargetHeadSHA is the target ref's head after the import (normally
	// SourceSHA; the read-back keeps a concurrent push honest).
	TargetHeadSHA string
	// Inserted reports whether this call created the ingestion record.
	Inserted bool
}

// ForkImporter performs the content half of an external fork.
type ForkImporter struct {
	port   GitPort
	store  ForkImportStore
	ingest *PushIngester
}

// NewForkImporter wires the importer. ingest is the SAME ingester the push
// webhook is served by — the inspection must not have a second
// implementation, or the flag could be derived two ways.
func NewForkImporter(port GitPort, store ForkImportStore, ingest *PushIngester) *ForkImporter {
	return &ForkImporter{port: port, store: store, ingest: ingest}
}

// Import copies one commit from the parent project's repository into the
// fork project's branch and runs the push inspection over the copy.
//
// The order is fixed and deliberate: the source commit and the fork
// project's current head are read, the delivery's evidence is resolved and
// RECORDED, and only then does the copy land. Recording first is what keeps the
// provider's own webhook for the copy (production's normal case) from
// inspecting the copy's whole tree — a diff against the fork's empty
// history that blames the fork branch for the platform's own bootstrap
// file — because that delivery carries the same (repository, ref, after)
// and collapses onto the row written here. A copy that fails after the row
// landed leaves the recorded head ahead of the provider's ref: visible
// drift, healed by retrying the request (the row insert is a no-op, the
// copy re-runs).
func (f *ForkImporter) Import(ctx context.Context, in ForkImportRequest) (ForkImportResult, error) {
	if in.SourceProjectID == "" || in.TargetProjectID == "" || in.TargetBranch == "" || in.ActorID == "" {
		return ForkImportResult{}, fmt.Errorf("%w: the fork import needs both projects, the target branch and the actor it is made for", ErrConflict)
	}
	source, err := f.store.ForkRepo(ctx, in.SourceProjectID)
	if err != nil {
		return ForkImportResult{}, err
	}
	target, err := f.store.ForkRepo(ctx, in.TargetProjectID)
	if err != nil {
		return ForkImportResult{}, err
	}
	sourceRepo := Repository{Owner: source.Owner, Name: source.Name, ID: source.RepoID}
	targetRepo := Repository{Owner: target.Owner, Name: target.Name, ID: target.RepoID}

	// The commit to import and the ref it is on: the named ref, or the
	// source repository's default branch. A parent repository always
	// carries one (provisioning seeds main), so an empty answer is a
	// repository the fork cannot be taken from.
	sourceRef := in.SourceRef
	if sourceRef == "" {
		info, err := f.port.GetRepository(ctx, source.Owner, source.Name)
		if err != nil {
			return ForkImportResult{}, err
		}
		if info.DefaultBranch == "" {
			return ForkImportResult{}, fmt.Errorf("%w: the project being forked has no branch to fork",
				ErrNotFound)
		}
		sourceRef = "refs/heads/" + info.DefaultBranch
	}
	ref, err := f.port.GetBranch(ctx, sourceRepo, strings.TrimPrefix(sourceRef, "refs/heads/"))
	if err != nil {
		return ForkImportResult{}, err
	}
	sourceSHA := ref.HeadSHA

	// The line the copied commit DIVERGED from: the source ref's recorded
	// fork point, when there is one. Empty means no content baseline
	// exists — the line was created without a base state's commit, or the
	// ref carries no record — and the copy's evidence is then the source
	// line's own (IngestCopy). It is never a licence to attribute nothing.
	baseline, err := f.store.SourceForkPoint(ctx, in.SourceProjectID, sourceRef)
	if err != nil {
		return ForkImportResult{}, err
	}

	// The pre-import head of the target ref: the delivery's before. A
	// missing ref is a creation — zeros, the same value the provider sends
	// for a ref's first push.
	beforeSHA := ZerosSHA
	if ref, err := f.port.GetBranch(ctx, targetRepo, in.TargetBranch); err == nil {
		// An empty head is the adapter's "the ref carries no commit" shape
		// (it answers ErrNotFound for that, and the guard keeps a provider
		// that answered neither from recording an empty before).
		if ref.HeadSHA != "" {
			beforeSHA = ref.HeadSHA
		}
	} else if !errors.Is(err, ErrNotFound) {
		return ForkImportResult{}, err
	}

	// Record the copy's delivery: the same PushEvent shape a webhook
	// carries, its evidence resolved against the source line (IngestCopy),
	// recorded against the fork's repository and ref. The store writes the
	// ingestion row, the change rows, the pushed-head state, the head
	// pointer and the semantic flag in one transaction, so the flag derives
	// from evidence that exists the moment the import is observable.
	inserted, err := f.ingest.IngestCopy(ctx, PushEvent{
		Ref:          "refs/heads/" + in.TargetBranch,
		Before:       beforeSHA,
		After:        sourceSHA,
		RepositoryID: target.RepoID,
		Owner:        target.Owner,
		Name:         target.Name,
		// The pusher is the platform's fork service, not a person: the
		// copy was made by the platform on the forker's behalf, and the
		// name says so rather than borrowing the forker's provider login
		// (there is none — T0304's per-user tokens are not what this path
		// uses).
		Pusher:       forkImportPusher,
		TotalCommits: 1,
		DeliveryID:   forkImportDeliveryID(target.RepoID, sourceSHA),
	}, CopySource{
		Repository: sourceRepo,
		Ref:        sourceRef,
		ForkPoint:  baseline,
	}, ForkImportTransition{
		ActorID: in.ActorID,
		Message: forkImportMessage(in, sourceRef, sourceSHA),
	})
	if err != nil {
		return ForkImportResult{}, err
	}

	imported, err := f.port.ImportBranch(ctx, ImportBranchSpec{
		Source:    sourceRepo,
		SourceSHA: sourceSHA,
		Target:    targetRepo,
		Name:      in.TargetBranch,
	})
	if err != nil {
		return ForkImportResult{}, err
	}
	return ForkImportResult{
		SourceSHA:     sourceSHA,
		BeforeSHA:     beforeSHA,
		TargetHeadSHA: imported.HeadSHA,
		Inserted:      inserted,
	}, nil
}

// forkImportPusher is the pusher recorded on an imported delivery: the
// platform's own service identity, never a person's provider login.
const forkImportPusher = "post-fork-service"

// forkImportMessage is the commit message of the imported transition: what
// arrived, where it came from, and which project it was copied out of. The
// three facts are the ones a reader walking the branch's history back to its
// root needs (docs/09 §2: a state commit names actor, channel, message and
// the base → result pair), and none of them is inferred later from the
// branch name.
func forkImportMessage(in ForkImportRequest, sourceRef, sourceSHA string) string {
	return fmt.Sprintf("fork import: %s@%s from project %s",
		sourceRef, sourceSHA, in.SourceProjectID)
}

// forkImportDeliveryID is the delivery id of an imported copy. Deliveries
// are correlation only — the dedupe key is (repository, ref, after) — and
// this one is derived from exactly those facts so a retried import reads
// as the same delivery in the records rather than as a new one.
func forkImportDeliveryID(giteaRepoID int64, after string) string {
	return fmt.Sprintf("fork-import-%d-%s", giteaRepoID, after)
}
