package manifests

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/manifest"
)

// fakeStatePort implements StatePort for service-shape tests.
type fakeStatePort struct {
	state domain.ProjectState
	err   error
}

func (f *fakeStatePort) GetState(context.Context, string) (domain.ProjectState, error) {
	return f.state, f.err
}

// fakeSnapshotPort implements SnapshotPort.
type fakeSnapshotPort struct {
	snap manifest.Snapshot
	err  error
}

func (f *fakeSnapshotPort) GetManifestSnapshot(context.Context, string) (manifest.Snapshot, error) {
	return f.snap, f.err
}

// fixtureService wires a service over the fakes with a fixed clock. The
// state carries a recorded git commit sha — the service deliberately has no
// project read (the manifest is a pure function of the state's recorded
// content), so the fixture has no project fake to wire.
func fixtureService(t *testing.T) (*Service, *fakeStatePort, *fakeSnapshotPort) {
	t.Helper()
	state := domain.ProjectState{
		ID:              "state-00000001",
		ProjectID:       "proj-00000001",
		GitCommitSHA:    strptr("abcdef0123456789abcdef0123456789abcdef01"),
		ManifestVersion: manifest.FormatV1,
	}
	snap := manifest.Snapshot{
		ObjectVersions: []manifest.ObjectVersion{
			{
				ID: "ov-00000001", ObjectID: "obj-00000001", ObjectType: "experiment",
				VersionNo: 1, StateID: "state-00000001",
				SchemaRef: manifest.SchemaRef{ID: "https://open-rd.example/schemas/experiment.schema.json", Version: "1"},
				Title:     "E1", LifecycleState: "active",
				Payload:       []byte(`{"b":2,"a":1}`),
				IntegrityHash: "sha256:1111", CreatedBy: "user-00000001",
				CreatedAt: time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
			},
		},
		RelationVersions: []manifest.RelationVersion{},
		BlobRefs:         []manifest.BlobRef{},
	}
	statesPort := &fakeStatePort{state: state}
	snapshotsPort := &fakeSnapshotPort{snap: snap}
	svc := NewService(statesPort, snapshotsPort)
	svc.now = func() time.Time {
		return time.Date(2026, 1, 15, 14, 0, 0, 0, time.UTC)
	}
	return svc, statesPort, snapshotsPort
}

func strptr(s string) *string { return &s }

// TestExportBuildsVerifiedManifest proves the service composes the two
// reads into a manifest whose hash verifies and whose payloads are
// canonical.
func TestExportBuildsVerifiedManifest(t *testing.T) {
	svc, _, _ := fixtureService(t)
	m, err := svc.Export(context.Background(), "state-00000001")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if m.StateID != "state-00000001" || m.ProjectID != "proj-00000001" {
		t.Fatalf("manifest names the wrong state/project: %+v", m)
	}
	if !m.VerifyHash() {
		t.Fatalf("exported manifest must verify its hash")
	}
	if string(m.ObjectVersions[0].Payload) != `{"a":1,"b":2}` {
		t.Fatalf("payload not canonicalized: %s", m.ObjectVersions[0].Payload)
	}
	if m.GitRef == nil || *m.GitRef != "abcdef0123456789abcdef0123456789abcdef01" {
		t.Fatalf("git ref not rendered from the state's recorded commit sha: %+v", m.GitRef)
	}
}

// TestExportRepeatedIsStable proves two exports of the same state agree on
// the hash even when the clock moves (the injected clock is fixed; the
// hash must not depend on it at all).
func TestExportRepeatedIsStable(t *testing.T) {
	svc, _, _ := fixtureService(t)
	m1, err := svc.Export(context.Background(), "state-00000001")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	svc.now = func() time.Time {
		return time.Date(2026, 1, 15, 20, 0, 0, 0, time.UTC)
	}
	m2, err := svc.Export(context.Background(), "state-00000001")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if m1.StateHash != m2.StateHash {
		t.Fatalf("hash moved between exports: %s != %s", m1.StateHash, m2.StateHash)
	}
}

// TestExportValidation refuses an empty state id before any read.
func TestExportValidation(t *testing.T) {
	svc, _, _ := fixtureService(t)
	_, err := svc.Export(context.Background(), "")
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("want ErrValidation, got %v", err)
	}
}

// TestExportMapsOutcomes maps the adapter outcomes: a missing state is the
// manifests not-found; snapshot failures and a foreign manifest format are
// store failures.
func TestExportMapsOutcomes(t *testing.T) {
	svc, statesPort, snapshotsPort := fixtureService(t)

	statesPort.err = states.ErrStateNotFound
	if _, err := svc.Export(context.Background(), "state-00000001"); !errors.Is(err, ErrStateNotFound) {
		t.Fatalf("want ErrStateNotFound, got %v", err)
	}
	statesPort.err = nil

	snapshotsPort.err = errors.New("db down")
	if _, err := svc.Export(context.Background(), "state-00000001"); !errors.Is(err, ErrStore) {
		t.Fatalf("want ErrStore, got %v", err)
	}
	snapshotsPort.err = nil

	// A state written under a format this exporter does not speak is a
	// store-level refusal, not a silent wrong-format export.
	statesPort.state.ManifestVersion = "v9"
	if _, err := svc.Export(context.Background(), "state-00000001"); !errors.Is(err, ErrStore) {
		t.Fatalf("want ErrStore for foreign manifest version, got %v", err)
	}
}
