package relations

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/relationcatalog"
)

// fakeRepo is a scripted Repository: every method fails the test if called
// with input the test did not expect, so the service's validation surface
// is proven without a database.
type fakeRepo struct {
	t         *testing.T
	createFn  func(ctx context.Context, in CreateRelationParams) (domain.Relation, domain.RelationVersion, error)
	versionFn func(ctx context.Context, relationID string, expected int, in VersionParams) (domain.RelationVersion, error)
}

func (f *fakeRepo) CreateRelation(ctx context.Context, in CreateRelationParams) (domain.Relation, domain.RelationVersion, error) {
	if f.createFn == nil {
		f.t.Fatal("CreateRelation called without a script")
	}
	return f.createFn(ctx, in)
}

func (f *fakeRepo) CreateVersion(ctx context.Context, relationID string, expected int, in VersionParams) (domain.RelationVersion, error) {
	if f.versionFn == nil {
		f.t.Fatal("CreateVersion called without a script")
	}
	return f.versionFn(ctx, relationID, expected, in)
}

func (f *fakeRepo) GetRelation(ctx context.Context, relationID string) (domain.Relation, error) {
	f.t.Fatal("GetRelation called without a script")
	return domain.Relation{}, nil
}

func (f *fakeRepo) GetVersion(ctx context.Context, relationID string, versionNo int) (domain.RelationVersion, error) {
	f.t.Fatal("GetVersion called without a script")
	return domain.RelationVersion{}, nil
}

func (f *fakeRepo) GetLatestVersion(ctx context.Context, relationID string) (domain.RelationVersion, error) {
	f.t.Fatal("GetLatestVersion called without a script")
	return domain.RelationVersion{}, nil
}

func (f *fakeRepo) ListVersions(ctx context.Context, relationID string) ([]domain.RelationVersion, error) {
	f.t.Fatal("ListVersions called without a script")
	return nil, nil
}

func (f *fakeRepo) ListVersionsByType(ctx context.Context, projectID string, relationType string) ([]domain.RelationVersion, error) {
	f.t.Fatal("ListVersionsByType called without a script")
	return nil, nil
}

func (f *fakeRepo) ListVersionsByTypes(ctx context.Context, projectID string, relationTypes []string) ([]domain.RelationVersion, error) {
	f.t.Fatal("ListVersionsByTypes called without a script")
	return nil, nil
}

func validParams() VersionParams {
	return VersionParams{
		StateID:               "state-1",
		RelationType:          "depends_on",
		SourceObjectVersionID: "source-version-1",
		TargetObjectVersionID: "target-version-1",
		Payload:               json.RawMessage(`{"scope":"v1"}`),
		CreatedBy:             "alice-1",
	}
}

// TestServiceCreateRelationCatalogValidation: the type catalog is the write
// gate. A type outside the catalog fails explicitly with
// *UnknownRelationTypeError and code RSG_VALIDATION_FAILED — before the
// repository is ever called; a dependency type and a provenance type pass
// through.
func TestServiceCreateRelationCatalogValidation(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		typ  string
		want error
	}{
		{"dependency type accepted", "depends_on", nil},
		{"provenance type accepted", "derived_from", nil},
		{"weak type accepted", "related_to", nil},
		{"unknown type rejected", "friend_of", &UnknownRelationTypeError{Type: "friend_of"}},
		{"namespaced extension rejected in V1", "materials:synthesized_from", &UnknownRelationTypeError{Type: "materials:synthesized_from"}},
		{"empty type rejected", "", ErrValidation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepo{t: t}
			repo.createFn = func(ctx context.Context, in CreateRelationParams) (domain.Relation, domain.RelationVersion, error) {
				return domain.Relation{ID: "r1"}, domain.RelationVersion{ID: "rv1", RelationID: "r1", VersionNo: 1}, nil
			}
			svc := NewService(repo)
			in := CreateRelationParams{ProjectID: "p1", Version: validParams()}
			in.Version.RelationType = tc.typ
			_, _, err := svc.CreateRelation(ctx, in)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("CreateRelation(%q): %v, want success", tc.typ, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("CreateRelation(%q): success, want error", tc.typ)
			}
			if want, ok := tc.want.(*UnknownRelationTypeError); ok {
				var got *UnknownRelationTypeError
				if !errors.As(err, &got) {
					t.Fatalf("CreateRelation(%q) err = %v, want *UnknownRelationTypeError", tc.typ, err)
				}
				if got.Type != want.Type || got.Code() != CodeRSGValidationFailed {
					t.Errorf("unknown-type error = %+v (code %s), want type %q code %s", got, got.Code(), want.Type, CodeRSGValidationFailed)
				}
			} else if !errors.Is(err, tc.want) {
				t.Fatalf("CreateRelation(%q) err = %v, want %v", tc.typ, err, tc.want)
			}
		})
	}
}

// TestServiceCreateRelationShapeValidation: identity facts and version
// content shape fail before the repository is called.
func TestServiceCreateRelationShapeValidation(t *testing.T) {
	ctx := context.Background()
	svc := NewService(&fakeRepo{t: t})

	cases := []struct {
		name string
		mut  func(*CreateRelationParams)
	}{
		{"project_id required", func(in *CreateRelationParams) { in.ProjectID = "" }},
		{"state_id required", func(in *CreateRelationParams) { in.Version.StateID = "" }},
		{"source required", func(in *CreateRelationParams) { in.Version.SourceObjectVersionID = "" }},
		{"target required", func(in *CreateRelationParams) { in.Version.TargetObjectVersionID = "" }},
		{"created_by required", func(in *CreateRelationParams) { in.Version.CreatedBy = "" }},
		{"payload must be an object", func(in *CreateRelationParams) { in.Version.Payload = json.RawMessage(`[1,2]`) }},
		{"payload must be JSON", func(in *CreateRelationParams) { in.Version.Payload = json.RawMessage(`{`) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := CreateRelationParams{ProjectID: "p1", Version: validParams()}
			tc.mut(&in)
			_, _, err := svc.CreateRelation(ctx, in)
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("err = %v, want ErrValidation", err)
			}
		})
	}
}

// TestServiceCreateRelationPayloadNormalization: an empty payload is the
// legitimate "no metadata yet" case and is normalized to the empty object.
func TestServiceCreateRelationPayloadNormalization(t *testing.T) {
	ctx := context.Background()
	repo := &fakeRepo{t: t}
	var seen json.RawMessage
	repo.createFn = func(ctx context.Context, in CreateRelationParams) (domain.Relation, domain.RelationVersion, error) {
		seen = in.Version.Payload
		return domain.Relation{ID: "r1"}, domain.RelationVersion{ID: "rv1", RelationID: "r1", VersionNo: 1}, nil
	}
	in := CreateRelationParams{ProjectID: "p1", Version: validParams()}
	in.Version.Payload = nil
	if _, _, err := NewService(repo).CreateRelation(ctx, in); err != nil {
		t.Fatalf("CreateRelation with empty payload: %v", err)
	}
	if string(seen) != `{}` {
		t.Errorf("payload = %s, want {}", seen)
	}
}

// TestServiceCreateVersionExpectedAndConflictMapping: expected < 1 is a
// validation error; a store conflict passes through as
// *VersionConflictError with its code; unknown relation/version endpoints
// pass through as their sentinels; anything else becomes ErrStore.
func TestServiceCreateVersionExpectedAndConflictMapping(t *testing.T) {
	ctx := context.Background()

	repo := &fakeRepo{t: t}
	svc := NewService(repo)
	if _, err := svc.CreateVersion(ctx, "r1", 0, validParams()); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected=0 err = %v, want ErrValidation", err)
	}

	repo.versionFn = func(ctx context.Context, relationID string, expected int, in VersionParams) (domain.RelationVersion, error) {
		return domain.RelationVersion{}, &VersionConflictError{RelationID: relationID, Expected: expected, Actual: 3}
	}
	_, err := svc.CreateVersion(ctx, "r1", 2, validParams())
	var conflict *VersionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("conflict err = %v, want *VersionConflictError", err)
	}
	if conflict.Code() != CodeVersionConflict || conflict.Actual != 3 {
		t.Errorf("conflict = %+v (code %s), want actual 3 code %s", conflict, conflict.Code(), CodeVersionConflict)
	}

	repo.versionFn = func(ctx context.Context, relationID string, expected int, in VersionParams) (domain.RelationVersion, error) {
		return domain.RelationVersion{}, ErrRelationNotFound
	}
	if _, err := svc.CreateVersion(ctx, "r1", 1, validParams()); !errors.Is(err, ErrRelationNotFound) {
		t.Fatalf("unknown relation err = %v, want ErrRelationNotFound", err)
	}

	repo.versionFn = func(ctx context.Context, relationID string, expected int, in VersionParams) (domain.RelationVersion, error) {
		return domain.RelationVersion{}, &ReferencedVersionNotFoundError{Side: "source", VersionID: "v9"}
	}
	_, err = svc.CreateVersion(ctx, "r1", 1, validParams())
	var ref *ReferencedVersionNotFoundError
	if !errors.As(err, &ref) || ref.Code() != CodeObjectVersionNotFound {
		t.Fatalf("dangling endpoint err = %v, want *ReferencedVersionNotFoundError with code %s", err, CodeObjectVersionNotFound)
	}

	repo.versionFn = func(ctx context.Context, relationID string, expected int, in VersionParams) (domain.RelationVersion, error) {
		return domain.RelationVersion{}, errors.New("adapter exploded")
	}
	if _, err := svc.CreateVersion(ctx, "r1", 1, validParams()); !errors.Is(err, ErrStore) {
		t.Fatalf("adapter failure err = %v, want ErrStore", err)
	}
}

// TestServiceListValidation: the category query validates its identity
// facts and short-circuits an empty type set without touching the store.
func TestServiceListValidation(t *testing.T) {
	ctx := context.Background()
	svc := NewService(&fakeRepo{t: t})

	if _, err := svc.ListVersionsByType(ctx, "", "depends_on"); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty project err = %v, want ErrValidation", err)
	}
	if _, err := svc.ListVersionsByTypes(ctx, "p1", nil); err != nil {
		t.Fatalf("empty type set err = %v, want nil", err)
	}
}

// TestCatalogCoversTheRequirementTypes: the acceptance criterion's
// dependency/provenance families are real catalog members — depends_on is
// the dependency edge and provenance types are queryable as a set.
func TestCatalogCoversTheRequirementTypes(t *testing.T) {
	if _, ok := relationcatalog.Lookup("depends_on"); !ok {
		t.Fatal("depends_on missing from the catalog")
	}
	dep := relationcatalog.DependencyTypes()
	if len(dep) == 0 || dep[0] != "depends_on" {
		t.Fatalf("DependencyTypes() = %v, want depends_on first", dep)
	}
	for _, typ := range dep {
		e, ok := relationcatalog.Lookup(typ)
		if !ok || !e.DependencyInference {
			t.Errorf("dependency type %q: lookup ok=%v inference=%v", typ, ok, e.DependencyInference)
		}
	}
	prov := relationcatalog.ProvenanceTypes()
	if len(prov) == 0 {
		t.Fatal("ProvenanceTypes() is empty")
	}
	if _, ok := relationcatalog.Lookup("derived_from"); !ok {
		t.Fatal("derived_from missing from the catalog")
	}
	if e, _ := relationcatalog.Lookup("related_to"); e.Category != relationcatalog.CategoryWeak {
		t.Errorf("related_to category = %s, want weak", e.Category)
	}
}
