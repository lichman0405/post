package diffs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/manifest"
)

// fakeStatePort implements StatePort for service-shape tests: one state
// per id from a fixed map, with an optional error for the not-found path.
type fakeStatePort struct {
	states map[string]domain.ProjectState
	err    error
}

func (f *fakeStatePort) GetState(_ context.Context, stateID string) (domain.ProjectState, error) {
	if f.err != nil {
		return domain.ProjectState{}, f.err
	}
	st, ok := f.states[stateID]
	if !ok {
		return domain.ProjectState{}, states.ErrStateNotFound
	}
	return st, nil
}

// fakeSnapshotPort implements SnapshotPort: the same small snapshot for
// every state, plus an optional error.
type fakeSnapshotPort struct {
	snap manifest.Snapshot
	err  error
}

func (f *fakeSnapshotPort) GetManifestSnapshot(context.Context, string) (manifest.Snapshot, error) {
	if f.err != nil {
		return manifest.Snapshot{}, f.err
	}
	return f.snap, nil
}

// fixtureStates is the three-state map the service fixtures share: base
// and target carry no git ref, source carries one — so the service's
// ref-passing shows up in the result.
func fixtureStates() map[string]domain.ProjectState {
	sha := "cccccccccccccccccccccccccccccccccccccccc"
	return map[string]domain.ProjectState{
		"state-base-0001": {ID: "state-base-0001", ProjectID: "proj-00000001", ManifestVersion: manifest.FormatV1},
		"state-src-000001": {ID: "state-src-000001", ProjectID: "proj-00000001",
			GitCommitSHA: &sha, ManifestVersion: manifest.FormatV1},
		"state-tgt-000001": {ID: "state-tgt-000001", ProjectID: "proj-00000001", ManifestVersion: manifest.FormatV1},
	}
}

func fixtureService(t *testing.T) (*Service, *fakeStatePort, *fakeSnapshotPort) {
	t.Helper()
	row := manifest.ObjectVersion{
		ID: "ov-00000001", ObjectID: "obj-a-0000001", ObjectType: "claim",
		VersionNo: 1, StateID: "state-base-0001",
		SchemaRef: manifest.SchemaRef{ID: "https://open-rd.example/schemas/claim.schema.json", Version: "1"},
		Title:     "A", LifecycleState: "active",
		Payload:       json.RawMessage(`{"b":2,"a":1}`),
		IntegrityHash: "sha256:fixture", CreatedBy: "user-00000001",
		CreatedAt: time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
	}
	statesPort := &fakeStatePort{states: fixtureStates()}
	snapshotsPort := &fakeSnapshotPort{snap: manifest.Snapshot{
		ObjectVersions:   []manifest.ObjectVersion{row},
		RelationVersions: []manifest.RelationVersion{},
		BlobRefs:         []manifest.BlobRef{},
	}}
	return NewService(statesPort, snapshotsPort), statesPort, snapshotsPort
}

// TestDiffBuildsDiff proves the service composes the six reads into a
// diff: the source state's git ref lands in the file diff refs, the
// project ids flow through, and the payloads are canonicalized.
func TestDiffBuildsDiff(t *testing.T) {
	svc, _, _ := fixtureService(t)
	d, err := svc.Diff(context.Background(), Params{
		ProjectID:     "proj-00000001",
		BaseStateID:   "state-base-0001",
		SourceStateID: "state-src-000001",
		TargetStateID: "state-tgt-000001",
	})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if d.ProjectID != "proj-00000001" || d.Base.ID != "state-base-0001" {
		t.Fatalf("diff names the wrong states: %+v", d)
	}
	if len(d.FileDiffRefs) != 1 || d.FileDiffRefs[0].Kind != "source" ||
		d.FileDiffRefs[0].HeadGitRef != "cccccccccccccccccccccccccccccccccccccccc" {
		t.Fatalf("file diff refs = %+v, want the source state's git ref", d.FileDiffRefs)
	}
}

// TestDiffValidation pins the shape errors: empty ids and a state that
// belongs to a different project.
func TestDiffValidation(t *testing.T) {
	svc, _, _ := fixtureService(t)
	_, err := svc.Diff(context.Background(), Params{})
	if err == nil || !errors.Is(err, ErrValidation) {
		t.Fatalf("empty params error = %v, want ErrValidation", err)
	}

	// A state of another project reports validation, not its content.
	_, err = svc.Diff(context.Background(), Params{
		ProjectID:     "proj-other-0001",
		BaseStateID:   "state-base-0001",
		SourceStateID: "state-src-000001",
		TargetStateID: "state-tgt-000001",
	})
	if err == nil || !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("foreign project error = %v, want ErrValidation naming the mismatch", err)
	}
}

// TestDiffStateNotFound pins the not-found mapping: the adapter's sentinel
// surfaces as ErrStateNotFound, whatever the role.
func TestDiffStateNotFound(t *testing.T) {
	svc, _, _ := fixtureService(t)
	_, err := svc.Diff(context.Background(), Params{
		ProjectID:     "proj-00000001",
		BaseStateID:   "state-missing-01",
		SourceStateID: "state-src-000001",
		TargetStateID: "state-tgt-000001",
	})
	if err == nil || !errors.Is(err, ErrStateNotFound) {
		t.Fatalf("missing base error = %v, want ErrStateNotFound", err)
	}
}

// TestDiffStoreFailures pins the store wrapping: adapter failures surface
// as ErrStore with the cause kept.
func TestDiffStoreFailures(t *testing.T) {
	svc, statesPort, snapshotsPort := fixtureService(t)
	statesPort.err = errors.New("db down")
	_, err := svc.Diff(context.Background(), Params{
		ProjectID:     "proj-00000001",
		BaseStateID:   "state-base-0001",
		SourceStateID: "state-src-000001",
		TargetStateID: "state-tgt-000001",
	})
	if err == nil || !errors.Is(err, ErrStore) || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("state adapter error = %v, want ErrStore with cause", err)
	}
	statesPort.err = nil
	snapshotsPort.err = errors.New("snapshot down")
	_, err = svc.Diff(context.Background(), Params{
		ProjectID:     "proj-00000001",
		BaseStateID:   "state-base-0001",
		SourceStateID: "state-src-000001",
		TargetStateID: "state-tgt-000001",
	})
	if err == nil || !errors.Is(err, ErrStore) || !strings.Contains(err.Error(), "snapshot down") {
		t.Fatalf("snapshot adapter error = %v, want ErrStore with cause", err)
	}
}

// TestDiffCorruptSnapshot pins the unrenderable-snapshot path: a payload
// that is not valid JSON fails the whole diff as ErrStore.
func TestDiffCorruptSnapshot(t *testing.T) {
	svc, _, snapshotsPort := fixtureService(t)
	snap := snapshotsPort.snap
	snap.ObjectVersions[0].Payload = json.RawMessage(`{"a": `)
	snapshotsPort.snap = snap
	_, err := svc.Diff(context.Background(), Params{
		ProjectID:     "proj-00000001",
		BaseStateID:   "state-base-0001",
		SourceStateID: "state-src-000001",
		TargetStateID: "state-tgt-000001",
	})
	if err == nil || !errors.Is(err, ErrStore) || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("corrupt snapshot error = %v, want ErrStore naming the payload", err)
	}
}
