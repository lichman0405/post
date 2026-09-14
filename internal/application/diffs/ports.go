package diffs

import (
	"context"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/manifest"
)

// Ports (docs/52: application orchestrates against ports; adapters live
// in internal/persistence). The diff needs exactly two read surfaces,
// exercised three times: the state rows (their project, recorded git ref
// and manifest format) and the lineage snapshots — every object and
// relation version whose state_id is the state or one of its ancestors.
// There is deliberately no project read: a diff is a pure function of the
// three states' recorded content, and the project row is mutable.

// StatePort resolves the states being diffed. The production
// implementation is persistence.StateStore; a missing state reports
// states.ErrStateNotFound (the port contract, so the service can map it).
type StatePort interface {
	GetState(ctx context.Context, stateID string) (domain.ProjectState, error)
}

// SnapshotPort reads a state's lineage snapshot as raw diff input rows
// (payloads in their stored form — the diff package canonicalizes). The
// production implementation is persistence.ManifestStore; the blob refs
// of the snapshot are accepted and ignored (the V1 diff covers objects
// and relations, internal/rsg/diff's package doc).
type SnapshotPort interface {
	GetManifestSnapshot(ctx context.Context, stateID string) (manifest.Snapshot, error)
}
