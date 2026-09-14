package manifests

import (
	"context"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/manifest"
)

// Ports (docs/52: application orchestrates against ports; adapters live in
// internal/persistence). The export needs exactly two reads: the state row
// (its project, git commit sha and manifest format) and the lineage
// snapshot — every object and relation version whose state_id is the state
// or one of its ancestors, plus the blob attachments whose own state_id and
// owning version are in the lineage. There is deliberately no project read:
// the manifest is a pure function of the state's recorded content, and the
// project row is mutable (its git repository id may change after the state
// was written — deriving the hash through it would let a later project
// update move an earlier state's hash).

// StatePort resolves the state being exported. The production
// implementation is persistence.StateStore; a missing state reports
// states.ErrStateNotFound (the port contract, so the service can map it).
type StatePort interface {
	GetState(ctx context.Context, stateID string) (domain.ProjectState, error)
}

// SnapshotPort reads the state's lineage snapshot as raw manifest input
// rows (payloads in their stored form — the manifest package
// canonicalizes). The production implementation is
// persistence.ManifestStore.
type SnapshotPort interface {
	GetManifestSnapshot(ctx context.Context, stateID string) (manifest.Snapshot, error)
}
