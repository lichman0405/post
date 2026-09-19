package sciobjects

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/domain"
)

// fakeRepo is the in-memory test double for the Repository port. It mirrors
// the compare-and-swap semantics (CreateVersion fails with
// *VersionConflictError when expected != current) so the service's error
// mapping and validation rules are exercised against a store that behaves
// like the real one.
type fakeRepo struct {
	mu        sync.Mutex
	objects   map[string]domain.ScientificObject
	versions  map[string][]domain.ScientificObjectVersion
	abortKeys map[string]string // objectID \x00 requestKey -> version id
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		objects:  map[string]domain.ScientificObject{},
		versions: map[string][]domain.ScientificObjectVersion{},
	}
}

func (f *fakeRepo) CreateObject(ctx context.Context, in CreateObjectParams) (domain.ScientificObject, domain.ScientificObjectVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	obj := domain.ScientificObject{
		ID:               "obj-1",
		ProjectID:        in.ProjectID,
		ObjectType:       in.ObjectType,
		CurrentVersionNo: 1,
		CreatedBy:        in.CreatedBy,
		CreatedAt:        time.Now(),
	}
	v1 := domain.ScientificObjectVersion{
		ID: "ver-1", ObjectID: obj.ID, VersionNo: 1,
		StateID: in.Version.StateID, BranchID: in.Version.BranchID,
		SchemaID: in.Version.SchemaID, SchemaVersion: in.Version.SchemaVersion,
		Title: in.Version.Title, LifecycleState: in.Version.LifecycleState,
		Payload: in.Version.Payload, VisibilityPolicyID: in.Version.VisibilityPolicyID,
		IntegrityHash: "hash-1", CreatedBy: in.Version.CreatedBy, CreatedAt: obj.CreatedAt,
	}
	f.objects[obj.ID] = obj
	f.versions[obj.ID] = []domain.ScientificObjectVersion{v1}
	return obj, v1, nil
}

func (f *fakeRepo) CreateVersion(ctx context.Context, objectID string, expected int, in VersionParams) (domain.ScientificObjectVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	obj, ok := f.objects[objectID]
	if !ok {
		return domain.ScientificObjectVersion{}, ErrObjectNotFound
	}
	if expected != obj.CurrentVersionNo {
		return domain.ScientificObjectVersion{}, &VersionConflictError{
			ObjectID: objectID, Expected: expected, Actual: obj.CurrentVersionNo,
		}
	}
	obj.CurrentVersionNo++
	f.objects[objectID] = obj
	v := domain.ScientificObjectVersion{
		ID: "ver-next", ObjectID: objectID, VersionNo: obj.CurrentVersionNo,
		StateID: in.StateID, BranchID: in.BranchID,
		SchemaID: in.SchemaID, SchemaVersion: in.SchemaVersion,
		Title: in.Title, LifecycleState: in.LifecycleState,
		Payload: in.Payload, VisibilityPolicyID: in.VisibilityPolicyID,
		IntegrityHash: "hash-next", CreatedBy: in.CreatedBy, CreatedAt: time.Now(),
		Abort: in.Abort,
	}
	f.versions[objectID] = append(f.versions[objectID], v)
	if in.AbortRequestKey != "" {
		// The adapter's unique index reads (object_id, request_key): the
		// fake mirrors it as an index of the same pair so a second
		// request with one key cannot resolve to two versions.
		if f.abortKeys == nil {
			f.abortKeys = map[string]string{}
		}
		f.abortKeys[objectID+"\x00"+in.AbortRequestKey] = v.ID
	}
	return v, nil
}

func (f *fakeRepo) GetObject(ctx context.Context, objectID string) (domain.ScientificObject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	obj, ok := f.objects[objectID]
	if !ok {
		return domain.ScientificObject{}, ErrObjectNotFound
	}
	return obj, nil
}

func (f *fakeRepo) GetVersion(ctx context.Context, objectID string, versionNo int) (domain.ScientificObjectVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.versions[objectID] {
		if v.VersionNo == versionNo {
			return v, nil
		}
	}
	return domain.ScientificObjectVersion{}, ErrVersionNotFound
}

func (f *fakeRepo) GetVersionByID(ctx context.Context, versionID string) (domain.ScientificObjectVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, log := range f.versions {
		for _, v := range log {
			if v.ID == versionID {
				return v, nil
			}
		}
	}
	return domain.ScientificObjectVersion{}, ErrVersionNotFound
}

func (f *fakeRepo) GetVersionByAbortRequestKey(ctx context.Context, objectID, requestKey string) (domain.ScientificObjectVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if requestKey == "" {
		return domain.ScientificObjectVersion{}, ErrVersionNotFound
	}
	versionID, ok := f.abortKeys[objectID+"\x00"+requestKey]
	if !ok {
		return domain.ScientificObjectVersion{}, ErrVersionNotFound
	}
	for _, v := range f.versions[objectID] {
		if v.ID == versionID {
			return v, nil
		}
	}
	return domain.ScientificObjectVersion{}, ErrVersionNotFound
}

func (f *fakeRepo) GetLatestVersion(ctx context.Context, objectID string) (domain.ScientificObjectVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	log := f.versions[objectID]
	if len(log) == 0 {
		return domain.ScientificObjectVersion{}, ErrVersionNotFound
	}
	return log[len(log)-1], nil
}

func (f *fakeRepo) ListVersions(ctx context.Context, objectID string) ([]domain.ScientificObjectVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.ScientificObjectVersion(nil), f.versions[objectID]...), nil
}

func validParams(t *testing.T) (CreateObjectParams, VersionParams) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"statement": "x"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	base := VersionParams{
		StateID:        "state-1",
		SchemaID:       "https://open-rd.example/schemas/hypothesis.schema.json",
		SchemaVersion:  "1",
		Title:          "H1",
		LifecycleState: domain.LifecycleActive,
		Payload:        raw,
		CreatedBy:      "user-1",
	}
	return CreateObjectParams{
		ProjectID: "proj-1", ObjectType: "hypothesis", CreatedBy: "user-1", Version: base,
	}, base
}

// TestServiceCreateObjectValidation: the shape rules fire before the store
// is touched.
func TestServiceCreateObjectValidation(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newFakeRepo())
	good, goodV := validParams(t)

	cases := []struct {
		name   string
		mutate func(*CreateObjectParams)
	}{
		{"project missing", func(p *CreateObjectParams) { p.ProjectID = "" }},
		{"object type missing", func(p *CreateObjectParams) { p.ObjectType = "" }},
		{"creator missing", func(p *CreateObjectParams) { p.CreatedBy = "" }},
		{"state missing", func(p *CreateObjectParams) { p.Version.StateID = "" }},
		{"schema ref missing", func(p *CreateObjectParams) { p.Version.SchemaID = "" }},
		{"title missing", func(p *CreateObjectParams) { p.Version.Title = "" }},
		{"lifecycle invalid", func(p *CreateObjectParams) { p.Version.LifecycleState = "zombie" }},
		{"version creator missing", func(p *CreateObjectParams) { p.Version.CreatedBy = "" }},
		{"payload empty", func(p *CreateObjectParams) { p.Version.Payload = nil }},
		{"payload invalid json", func(p *CreateObjectParams) { p.Version.Payload = []byte("{nope") }},
		{"payload not an object", func(p *CreateObjectParams) { p.Version.Payload = []byte(`[1,2]`) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := good
			in.Version = goodV
			tc.mutate(&in)
			if _, _, err := svc.CreateObject(ctx, in); !errors.Is(err, ErrValidation) {
				t.Fatalf("err = %v, want ErrValidation", err)
			}
		})
	}

	if _, _, err := svc.CreateObject(ctx, good); err != nil {
		t.Fatalf("valid create: %v", err)
	}
}

// TestServiceCreateVersionErrorMapping: the store's outcomes surface with
// their expected types — the conflict keeps its code and numbers, an
// unknown object stays ErrObjectNotFound, and a below-1 expectation is
// validation.
func TestServiceCreateVersionErrorMapping(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newFakeRepo())
	create, vp := validParams(t)

	obj, _, err := svc.CreateObject(ctx, create)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	v2, err := svc.CreateVersion(ctx, obj.ID, 1, vp)
	if err != nil {
		t.Fatalf("expected=1: %v", err)
	}
	if v2.VersionNo != 2 {
		t.Errorf("v2 = %+v, want version 2", v2)
	}

	_, err = svc.CreateVersion(ctx, obj.ID, 1, vp)
	var conflict *VersionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("stale expected=1 err = %v, want *VersionConflictError", err)
	}
	if conflict.Code() != CodeVersionConflict || conflict.Expected != 1 || conflict.Actual != 2 {
		t.Errorf("conflict = %+v code %q, want expected 1 actual 2 code %q", conflict, conflict.Code(), CodeVersionConflict)
	}

	if _, err := svc.CreateVersion(ctx, "missing", 1, vp); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("unknown object err = %v, want ErrObjectNotFound", err)
	}
	if _, err := svc.CreateVersion(ctx, obj.ID, 0, vp); !errors.Is(err, ErrValidation) {
		t.Errorf("expected=0 err = %v, want ErrValidation", err)
	}
	if _, err := svc.CreateVersion(ctx, obj.ID, 2, VersionParams{StateID: "state-1", Title: "T", SchemaID: "s", SchemaVersion: "1", LifecycleState: domain.LifecycleActive, Payload: []byte(`{"a":1}`), CreatedBy: "user-1"}); err != nil {
		t.Errorf("valid next create: %v", err)
	}
}
