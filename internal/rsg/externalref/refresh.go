package externalref

import (
	"context"
	"time"

	"github.com/lichman0405/post/internal/domain"
)

// Refresher runs ONE manual refresh of a live identity's upstream
// metadata (docs/19 §5: V1 is manual refresh — an operator, a user or a
// future scheduled job calls Refresh; automatic monitoring is a later
// iteration). The refresh contract is the task's acceptance criteria in
// one function:
//
//   - a refresh NEVER mutates an existing snapshot — it either produces
//     a NEW snapshot (upstream changed since the head) or reports the
//     head as still current;
//   - an unchanged upstream therefore never grows the log, and a
//     changed upstream only ever appends: history is history.
//
// Refresher is storage-free: it returns the snapshot to persist (or the
// head, unchanged) and the caller — the consuming API task — owns the
// store seam, exactly as T0208 assigned transport concerns to the
// consuming API task.
type Refresher struct {
	fetch Fetcher
	now   func() time.Time // seam for tests; nil means time.Now
}

// NewRefresher wires the refresher on the upstream fetcher (the V1
// adapter is DoiFetcher).
func NewRefresher(fetch Fetcher) *Refresher {
	return &Refresher{fetch: fetch}
}

// WithClock injects the time source (tests).
func (r *Refresher) WithClock(now func() time.Time) *Refresher {
	r.now = now
	return r
}

// RefreshResult is one refresh outcome.
type RefreshResult struct {
	// Snapshot is the snapshot to persist when Changed, or the head
	// snapshot itself (byte-for-byte untouched) when not.
	Snapshot Snapshot
	// Changed reports whether upstream moved since head: true means the
	// caller appends Snapshot as a new immutable row; false means the
	// head is still current and nothing may be written.
	Changed bool
}

// Refresh fetches the upstream metadata for the identity and compares it
// with the head snapshot. head nil means "no snapshot yet": the first
// refresh always reports a change. An upstream that is unreachable or
// unusable fails the refresh — a failed refresh never fabricates a
// snapshot, so the stored history only ever holds facts actually
// observed.
func (r *Refresher) Refresh(ctx context.Context, ref Ref, externalReferenceID string, head *Snapshot) (RefreshResult, error) {
	if r.fetch == nil {
		return RefreshResult{}, errf(ErrUpstreamUnavailable, "no upstream fetcher configured")
	}
	if err := validateRef(ref); err != nil {
		return RefreshResult{}, err
	}
	meta, err := r.fetch.Fetch(ctx, ref)
	if err != nil {
		return RefreshResult{}, err
	}
	snap, err := BuildSnapshot(externalReferenceID, meta, r.now)
	if err != nil {
		return RefreshResult{}, err
	}
	if head != nil && SameContent(*head, snap) {
		// Upstream still matches what the head pinned. Return the head
		// itself — untouched — so the caller reports it without any
		// write: a refresh that saw nothing new never grows the log.
		return RefreshResult{Snapshot: *head, Changed: false}, nil
	}
	return RefreshResult{Snapshot: snap, Changed: true}, nil
}

// validateRef checks the ref's identity facts before any network call:
// a refresh is never attempted on a nameless source, and a source_type
// outside the schema's canonical enum is refused HERE rather than
// failing at the database or keying an identity the schema does not
// admit (the identity guard would reject the write anyway; refusing
// early keeps a bogus ref from ever reaching the network).
func validateRef(ref Ref) error {
	if ref.SourceType == "" {
		return errf(ErrNotResolvable, "source_type is required")
	}
	if !domain.ValidExternalReferenceSourceType(ref.SourceType) {
		return errf(ErrNotResolvable, "source_type %q is not a canonical external reference source type", ref.SourceType)
	}
	if ref.ExternalIdentifier == "" {
		return errf(ErrNotResolvable, "external_identifier is required")
	}
	return nil
}
