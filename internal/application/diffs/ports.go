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
//
// ProposalPort (T0817) is the one addition, and it is not a project read:
// it answers whether THIS project is already being asked to review a
// state, which is what the external fork's cross-project source side
// needs (docs/04 §2). It is consulted for the source state only, and only
// after the state's own project has been found to differ from the diff's
// project — see Service.readInputs.

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

// ProposalPort answers the one project-boundary question a diff cannot
// answer from the three state rows: whether stateID is a state the
// project projectID is ALREADY being asked to review — the head a pull
// request of that project proposes (pull_requests.proposed_state_id).
//
// It exists for exactly one shape, the external fork (docs/04 §2, T0804):
// a contributor's proposal has its source state in the contributor's own
// fork project, so the triple's source is foreign while its base and
// target are the project's own. The question is deliberately about the
// PROPOSAL and not about fork lineage, because the widening has to be
// bounded by exposure: a state some pull request of this project proposes
// is content this project's members are already shown on that proposal's
// diff, while "a fork of this project holds it" would admit content of a
// fork the caller may not read.
//
// A false answer is not a failure: it is an unreadable source, reported
// as the validation failure a foreign state has always been. The
// production implementation is persistence.PullRequestStore.
type ProposalPort interface {
	// ProposesToProject reports whether a pull request of projectID
	// proposes stateID. It is a decision-shaped read: an id that cannot
	// name a row answers false, and only a query failure is an error.
	ProposesToProject(ctx context.Context, projectID, stateID string) (bool, error)
}
