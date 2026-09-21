package diffs

import (
	"context"
	"errors"
	"fmt"

	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/conflict"
	"github.com/lichman0405/post/internal/rsg/diff"
	"github.com/lichman0405/post/internal/rsg/manifest"
)

// Service orchestrates the three-way diff against the read ports. It
// owns input validation (the port trusts, the service verifies) and the
// project-membership check of the three states; it does NOT authorize —
// consumers resolve visibility before calling it (package doc).
type Service struct {
	states    StatePort
	snapshots SnapshotPort
	proposals ProposalPort
}

// NewService wires the diff service. proposals may be nil, and a nil one
// is not a licence: without it no source state of another project is
// admissible at all (see admitsForeignSource), so every caller keeps the
// same-project-only behaviour the three-state check has always had.
func NewService(states StatePort, snapshots SnapshotPort, proposals ProposalPort) *Service {
	return &Service{states: states, snapshots: snapshots, proposals: proposals}
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
// The three states must exist and belong to the same project, with the
// one exception the external fork's cross-project proposal makes —
// readInputs states it — so a state of an unrelated project reports
// ErrValidation, never its content. The same inputs always render the
// same diff (internal/rsg/diff's canonical serialization).
func (s *Service) Diff(ctx context.Context, in Params) (*diff.Diff, error) {
	inputs, err := s.readInputs(ctx, in)
	if err != nil {
		return nil, err
	}
	d, err := diff.Compute(inputs)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStore, err)
	}
	return d, nil
}

// Conflicts computes the three-way diff of the named states and
// classifies every source-side change with the semantic conflict detector
// (internal/rsg/conflict, T0405): safe changes are marked auto_mergeable,
// conflicts are classified into the docs/09 §6 taxonomy. The report
// carries the diff itself, so consumers get the change list and the
// verdicts from one call. Authorization and shape checks are the same as
// Diff's (package doc).
func (s *Service) Conflicts(ctx context.Context, in Params) (*conflict.Report, error) {
	inputs, err := s.readInputs(ctx, in)
	if err != nil {
		return nil, err
	}
	r, err := conflict.Detect(inputs)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStore, err)
	}
	return r, nil
}

// Inputs composes the engine inputs of the named triple without computing
// anything: the three states' refs and their lineage snapshots. Callers
// that run an engine of their own over the same triple (the merge engine,
// which recomputes the conflict report rather than trusting a copy) get
// exactly the inputs Diff and Conflicts use, with the same validation and
// the same project-membership check.
func (s *Service) Inputs(ctx context.Context, in Params) (diff.Inputs, error) {
	return s.readInputs(ctx, in)
}

// readInputs performs the shared front half of Diff and Conflicts: the
// shape validation, the three state reads, the project-membership check
// and the three snapshot reads, composed into the engine's inputs.
func (s *Service) readInputs(ctx context.Context, in Params) (diff.Inputs, error) {
	if err := validateParams(in); err != nil {
		return diff.Inputs{}, err
	}
	base, err := s.readState(ctx, in.BaseStateID)
	if err != nil {
		return diff.Inputs{}, err
	}
	source, err := s.readState(ctx, in.SourceStateID)
	if err != nil {
		return diff.Inputs{}, err
	}
	target, err := s.readState(ctx, in.TargetStateID)
	if err != nil {
		return diff.Inputs{}, err
	}
	for _, st := range []struct {
		role  string
		state domain.ProjectState
	}{{"base", base}, {"source", source}, {"target", target}} {
		if st.state.ProjectID == in.ProjectID {
			continue
		}
		if st.role != "source" {
			return diff.Inputs{}, fmt.Errorf("%w: %s state %s does not belong to project %s", ErrValidation, st.role, st.state.ID, in.ProjectID)
		}
		// The source side is the one that may sit in another project, and
		// only in the external fork's shape (docs/04 §2): the state is the
		// head a pull request of THIS project proposes, so the project is
		// already being asked to review it and its content is already on
		// that proposal's diff. Anything else — a foreign project's state
		// nobody proposed here — keeps the same answer it always had.
		ok, err := s.admitsForeignSource(ctx, in.ProjectID, st.state.ID)
		if err != nil {
			return diff.Inputs{}, err
		}
		if !ok {
			return diff.Inputs{}, fmt.Errorf("%w: source state %s does not belong to project %s and no pull request of that project proposes it", ErrValidation, st.state.ID, in.ProjectID)
		}
	}
	baseSnap, err := s.readSnapshot(ctx, in.BaseStateID)
	if err != nil {
		return diff.Inputs{}, err
	}
	sourceSnap, err := s.readSnapshot(ctx, in.SourceStateID)
	if err != nil {
		return diff.Inputs{}, err
	}
	targetSnap, err := s.readSnapshot(ctx, in.TargetStateID)
	if err != nil {
		return diff.Inputs{}, err
	}
	return diff.Inputs{
		ProjectID:      in.ProjectID,
		Base:           diff.StateRef{ID: base.ID, GitRef: base.GitCommitSHA},
		Source:         diff.StateRef{ID: source.ID, GitRef: source.GitCommitSHA},
		Target:         diff.StateRef{ID: target.ID, GitRef: target.GitCommitSHA},
		BaseSnapshot:   baseSnap,
		SourceSnapshot: sourceSnap,
		TargetSnapshot: targetSnap,
	}, nil
}

// admitsForeignSource asks the one question that can make a source state
// of another project readable: whether a pull request of projectID
// proposes it. Fail closed on every input — no proposal port wired, a
// failed read and a false answer all report the state as unreadable.
func (s *Service) admitsForeignSource(ctx context.Context, projectID, stateID string) (bool, error) {
	if s.proposals == nil {
		return false, nil
	}
	ok, err := s.proposals.ProposesToProject(ctx, projectID, stateID)
	if err != nil {
		return false, fmt.Errorf("%w: read the proposal of state %s: %v", ErrStore, stateID, err)
	}
	return ok, nil
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
