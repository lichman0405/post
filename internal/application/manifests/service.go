package manifests

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/rsg/manifest"
)

// Service orchestrates the manifest export against the two read ports.
// It owns input validation (the port trusts, the service verifies); it
// does NOT authorize — consumers resolve visibility before calling it
// (package doc).
type Service struct {
	states    StatePort
	snapshots SnapshotPort
	// now is the export timestamp source; the zero value means
	// time.Now. Tests inject a fixed clock.
	now func() time.Time
}

// NewService wires the export service.
func NewService(states StatePort, snapshots SnapshotPort) *Service {
	return &Service{states: states, snapshots: snapshots}
}

// Export renders the canonical manifest of the state, hash included: the
// state, the full lineage snapshot (object/relation versions, blob refs,
// schema/policy refs) and the state's recorded git ref, serialized
// canonically and content-addressed. Exporting the same state twice
// derives the same state hash — generated_at is the only field that moves,
// and it is not part of the hash (internal/rsg/manifest). The export reads
// nothing outside the state's recorded content: the hash never depends on
// mutable project data.
func (s *Service) Export(ctx context.Context, stateID string) (*manifest.Manifest, error) {
	if stateID == "" {
		return nil, fmt.Errorf("%w: state_id is required", ErrValidation)
	}
	state, err := s.states.GetState(ctx, stateID)
	if err != nil {
		if isStateNotFound(err) {
			return nil, ErrStateNotFound
		}
		return nil, fmt.Errorf("%w: read state: %v", ErrStore, err)
	}
	snap, err := s.snapshots.GetManifestSnapshot(ctx, state.ID)
	if err != nil {
		return nil, fmt.Errorf("%w: read snapshot of state %s: %v", ErrStore, state.ID, err)
	}
	m, err := manifest.Build(state, snap, s.exportTime())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStore, err)
	}
	return m, nil
}

// exportTime is the generated_at source: the injected clock, or the wall
// clock.
func (s *Service) exportTime() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// isStateNotFound maps the state adapter's not-found sentinel to this
// package's vocabulary (docs/45): the port contract says a missing state
// reports states.ErrStateNotFound.
func isStateNotFound(err error) bool {
	return errors.Is(err, states.ErrStateNotFound)
}
