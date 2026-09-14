package diffs

import (
	"context"
	"errors"
	"fmt"

	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/diff"
	"github.com/lichman0405/post/internal/rsg/manifest"
)

// Service orchestrates the three-way diff against the two read ports. It
// owns input validation (the port trusts, the service verifies) and the
// project-membership check of the three states; it does NOT authorize —
// consumers resolve visibility before calling it (package doc).
type Service struct {
	states    StatePort
	snapshots SnapshotPort
}

// NewService wires the diff service.
func NewService(states StatePort, snapshots SnapshotPort) *Service {
	return &Service{states: states, snapshots: snapshots}
}

// Params names the three states of the diff: the merge base, the source
// (the proposed head) and the target (the target branch's current head).
type Params struct {
	ProjectID     string
	BaseStateID   string
	SourceStateID string
	TargetStateID string
}

// Diff computes the three-way Research State Diff of the named states:
// the change list (objects created/updated/aborted/reopened, relations
// changed), the summary categories, and the recorded file-level git refs.
// The three states must exist and belong to the same project — a state of
// another project reports ErrValidation, never its content. The same
// inputs always render the same diff (internal/rsg/diff's canonical
// serialization).
func (s *Service) Diff(ctx context.Context, in Params) (*diff.Diff, error) {
	if err := validateParams(in); err != nil {
		return nil, err
	}
	base, err := s.readState(ctx, in.BaseStateID)
	if err != nil {
		return nil, err
	}
	source, err := s.readState(ctx, in.SourceStateID)
	if err != nil {
		return nil, err
	}
	target, err := s.readState(ctx, in.TargetStateID)
	if err != nil {
		return nil, err
	}
	for _, st := range []struct {
		role  string
		state domain.ProjectState
	}{{"base", base}, {"source", source}, {"target", target}} {
		if st.state.ProjectID != in.ProjectID {
			return nil, fmt.Errorf("%w: %s state %s does not belong to project %s", ErrValidation, st.role, st.state.ID, in.ProjectID)
		}
	}
	baseSnap, err := s.readSnapshot(ctx, in.BaseStateID)
	if err != nil {
		return nil, err
	}
	sourceSnap, err := s.readSnapshot(ctx, in.SourceStateID)
	if err != nil {
		return nil, err
	}
	targetSnap, err := s.readSnapshot(ctx, in.TargetStateID)
	if err != nil {
		return nil, err
	}
	d, err := diff.Compute(diff.Inputs{
		ProjectID:      in.ProjectID,
		Base:           diff.StateRef{ID: base.ID, GitRef: base.GitCommitSHA},
		Source:         diff.StateRef{ID: source.ID, GitRef: source.GitCommitSHA},
		Target:         diff.StateRef{ID: target.ID, GitRef: target.GitCommitSHA},
		BaseSnapshot:   baseSnap,
		SourceSnapshot: sourceSnap,
		TargetSnapshot: targetSnap,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStore, err)
	}
	return d, nil
}

// readState resolves one state, mapping the adapter's not-found sentinel
// to this package's vocabulary.
func (s *Service) readState(ctx context.Context, stateID string) (domain.ProjectState, error) {
	state, err := s.states.GetState(ctx, stateID)
	if err != nil {
		if errors.Is(err, states.ErrStateNotFound) {
			return domain.ProjectState{}, ErrStateNotFound
		}
		return domain.ProjectState{}, fmt.Errorf("%w: read state: %v", ErrStore, err)
	}
	return state, nil
}

// readSnapshot reads one state's lineage snapshot.
func (s *Service) readSnapshot(ctx context.Context, stateID string) (manifest.Snapshot, error) {
	snap, err := s.snapshots.GetManifestSnapshot(ctx, stateID)
	if err != nil {
		return manifest.Snapshot{}, fmt.Errorf("%w: read snapshot of state %s: %v", ErrStore, stateID, err)
	}
	return snap, nil
}

// validateParams checks the request's shape: the project and the three
// state ids are required. The state/project membership is checked after
// the reads — it needs the state rows.
func validateParams(in Params) error {
	if in.ProjectID == "" {
		return fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	if in.BaseStateID == "" {
		return fmt.Errorf("%w: base_state_id is required", ErrValidation)
	}
	if in.SourceStateID == "" {
		return fmt.Errorf("%w: source_state_id is required", ErrValidation)
	}
	if in.TargetStateID == "" {
		return fmt.Errorf("%w: target_state_id is required", ErrValidation)
	}
	return nil
}
