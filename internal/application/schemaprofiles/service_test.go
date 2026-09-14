package schemaprofiles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

// fakeStore is an in-memory ProfileStore reproducing the adapter's
// invariants (per-(project, schema_id, version) uniqueness, newest-first
// ordering, append-only rows) so the service rules are testable without
// PostgreSQL. The real adapter is exercised by the integration suite.
type fakeStore struct {
	rows     []domain.ProjectSchemaProfile
	audits   []domain.AuditEntry
	nextID   int
	projects map[string]bool // visible project ids
	members  map[[2]string]domain.ProjectRole
	failWith error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		projects: map[string]bool{},
		members:  map[[2]string]domain.ProjectRole{},
	}
}

// seedProject makes the project visible to the gate's reads and grants the
// actor a membership role.
func (s *fakeStore) seedProject(projectID string) { s.projects[projectID] = true }

func (s *fakeStore) seedMember(projectID, userID string, role domain.ProjectRole) {
	s.members[[2]string{projectID, userID}] = role
}

func (s *fakeStore) RegisterProfile(ctx context.Context, p domain.ProjectSchemaProfile, audit domain.AuditEntry) (domain.ProjectSchemaProfile, error) {
	if s.failWith != nil {
		return domain.ProjectSchemaProfile{}, s.failWith
	}
	if !s.projects[p.ProjectID] {
		return domain.ProjectSchemaProfile{}, ErrProjectNotFound
	}
	for _, row := range s.rows {
		if row.ProjectID == p.ProjectID && row.SchemaID == p.SchemaID && row.Version == p.Version {
			return domain.ProjectSchemaProfile{}, ErrProfileVersionExists
		}
	}
	s.nextID++
	p.ID = fmt.Sprintf("00000000-0000-4000-8000-%012d", s.nextID)
	p.CreatedAt = time.Date(2026, 1, 1, 0, 0, s.nextID, 0, time.UTC)
	s.rows = append(s.rows, p)
	s.audits = append(s.audits, audit)
	return p, nil
}

func (s *fakeStore) GetProfile(ctx context.Context, projectID, schemaID, version string) (domain.ProjectSchemaProfile, error) {
	if s.failWith != nil {
		return domain.ProjectSchemaProfile{}, s.failWith
	}
	for _, row := range s.rows {
		if row.ProjectID == projectID && row.SchemaID == schemaID && row.Version == version {
			return row, nil
		}
	}
	return domain.ProjectSchemaProfile{}, ErrProfileNotFound
}

func (s *fakeStore) GetLatestProfile(ctx context.Context, projectID, schemaID string) (domain.ProjectSchemaProfile, error) {
	if s.failWith != nil {
		return domain.ProjectSchemaProfile{}, s.failWith
	}
	var best *domain.ProjectSchemaProfile
	for i := range s.rows {
		row := s.rows[i]
		if row.ProjectID != projectID || row.SchemaID != schemaID {
			continue
		}
		if best == nil || row.CreatedAt.After(best.CreatedAt) {
			best = &row
		}
	}
	if best == nil {
		return domain.ProjectSchemaProfile{}, ErrProfileNotFound
	}
	return *best, nil
}

func (s *fakeStore) ListProfiles(ctx context.Context, projectID string) ([]domain.ProjectSchemaProfile, error) {
	if s.failWith != nil {
		return nil, s.failWith
	}
	var out []domain.ProjectSchemaProfile
	for _, row := range s.rows {
		if row.ProjectID == projectID {
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (s *fakeStore) ListAllProfiles(ctx context.Context) ([]domain.ProjectSchemaProfile, error) {
	if s.failWith != nil {
		return nil, s.failWith
	}
	return append([]domain.ProjectSchemaProfile(nil), s.rows...), nil
}

// Get implements the ProjectGate read half: the visibility-aware project
// read (visible = seeded, existence hiding otherwise).
func (s *fakeStore) Get(ctx context.Context, r projects.Reader, projectID string) (domain.Project, error) {
	if s.failWith != nil {
		return domain.Project{}, s.failWith
	}
	if !s.projects[projectID] {
		return domain.Project{}, projects.ErrProjectNotFound
	}
	return domain.Project{ID: projectID}, nil
}

// GetMembership implements the ProjectGate membership half with the
// projects application's sentinels.
func (s *fakeStore) GetMembership(ctx context.Context, actor domain.User, projectID string) (domain.ProjectMembership, error) {
	if s.failWith != nil {
		return domain.ProjectMembership{}, s.failWith
	}
	role, ok := s.members[[2]string{projectID, actor.ID}]
	if !ok {
		return domain.ProjectMembership{}, projects.ErrMemberNotFound
	}
	return domain.ProjectMembership{ProjectID: projectID, UserID: actor.ID, Role: role}, nil
}

var (
	_ ProfileStore = (*fakeStore)(nil)
	_ ProjectGate  = (*fakeStore)(nil)
)

// experimentBase is the canonical experiment schema ref every test extends.
var experimentBase = schemareg.Ref{
	ID:      schemareg.CanonicalNamespace + "experiment.schema.json",
	Version: schemareg.CanonicalV1,
}

// hypothesisBase is the canonical hypothesis schema ref — the "different
// base" arm of the same-name base-pin tests.
var hypothesisBase = schemareg.Ref{
	ID:      schemareg.CanonicalNamespace + "hypothesis.schema.json",
	Version: schemareg.CanonicalV1,
}

// newTestService wires a service over a fresh canonical registry and the
// fake gate/store.
func newTestService(t *testing.T) (*Service, *fakeStore, *schemareg.Registry) {
	t.Helper()
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	store := newFakeStore()
	return NewService(Deps{Store: store, Projects: store, Schemas: reg}), store, reg
}

func testUser(id string) domain.User { return domain.User{ID: id, Handle: "u-" + id} }

// seedMaintainer registers a visible project and an owner actor (the
// maintainer-or-above gate passes).
func seedMaintainer(t *testing.T, store *fakeStore, projectID, userID string) {
	t.Helper()
	store.seedProject(projectID)
	store.seedMember(projectID, userID, domain.ProjectRoleOwner)
}

// registerInput builds a minimal registration request for the experiment
// base.
func registerInput(custom map[string]any, required ...string) RegisterInput {
	return RegisterInput{
		Name:       "catalysis",
		Version:    "1",
		Base:       Ref{ID: experimentBase.ID, Version: experimentBase.Version},
		Properties: custom,
		Required:   required,
	}
}

// TestRegisterGeneratesMergedProfile proves the generation contract: the
// profile document is the base schema's properties merged with the custom
// additions (extension by construction), the base's required array and
// strictness survive, the authoritative type travels with the base, and
// the registry is the one authority that the generated document is a valid
// JSON Schema.
func TestRegisterGeneratesMergedProfile(t *testing.T) {
	ctx := context.Background()
	svc, store, reg := newTestService(t)
	seedMaintainer(t, store, "p-1", "alice")

	profile, err := svc.Register(ctx, testUser("alice"), "p-1", registerInput(
		map[string]any{"catalyst": map[string]any{"type": "string"}},
		"catalyst",
	))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if profile.SchemaID != "project:p-1:catalysis" || profile.Version != "1" {
		t.Fatalf("profile ref = %s v%s, want project:p-1:catalysis v1", profile.SchemaID, profile.Version)
	}
	if profile.BaseSchemaID != experimentBase.ID || profile.BaseSchemaVersion != experimentBase.Version {
		t.Errorf("base pin = %s v%s, want the experiment base", profile.BaseSchemaID, profile.BaseSchemaVersion)
	}
	if profile.Content == "" || profile.ContentHash == "" || profile.CreatedBy != "alice" {
		t.Fatalf("profile facts incomplete: %+v", profile)
	}
	sum := sha256.Sum256([]byte(profile.Content))
	if hex.EncodeToString(sum[:]) != profile.ContentHash {
		t.Errorf("stored content does not re-hash to its content hash")
	}

	var doc map[string]any
	if err := json.Unmarshal([]byte(profile.Content), &doc); err != nil {
		t.Fatalf("profile content is not JSON: %v", err)
	}
	if doc["$id"] != "project:p-1:catalysis" {
		t.Errorf("$id = %v, want project:p-1:catalysis", doc["$id"])
	}
	if doc["additionalProperties"] != false {
		t.Errorf("additionalProperties = %v, want false (strictness preserved)", doc["additionalProperties"])
	}
	props, _ := doc["properties"].(map[string]any)
	// The base's own definition survives verbatim: the authoritative type
	// const and a base field that is NOT in the custom set.
	typeDef, ok := props["type"].(map[string]any)
	if !ok || typeDef["const"] != "experiment" {
		t.Errorf("properties.type.const = %v, want the base's experiment const", props["type"])
	}
	if _, ok := props["objective"]; !ok {
		t.Errorf("base property objective missing from the merged profile")
	}
	if _, ok := props["catalyst"]; !ok {
		t.Errorf("custom property catalyst missing from the merged profile")
	}
	required := doc["required"].([]any)
	if !containsString(required, "objective") || !containsString(required, "catalyst") {
		t.Errorf("required = %v, want the base's objective and the custom catalyst", required)
	}

	// The registry compiled it: the ref resolves and the document actually
	// validates what the base demands and what the profile adds.
	ref := schemareg.Ref{ID: profile.SchemaID, Version: profile.Version}
	if _, err := reg.Lookup(ref); err != nil {
		t.Fatalf("registry Lookup(%s): %v", ref, err)
	}
	payload := experimentDoc(map[string]any{"objective": "screen MOFs", "catalyst": "Ni"})
	if err := reg.Validate(ref, payload); err != nil {
		t.Errorf("generated profile rejects a satisfying document: %v", err)
	}
	// The custom required field is enforced by the generated profile.
	if err := reg.Validate(ref, experimentDoc(map[string]any{"objective": "screen MOFs"})); err == nil {
		t.Errorf("generated profile accepted a document missing the required custom field catalyst")
	}
	// Unknown fields are still refused (strictness preserved), except
	// through the base's own escape hatch (metadata/conditions).
	if err := reg.Validate(ref, experimentDoc(map[string]any{"objective": "x", "catalyst": "Ni", "spurious": true})); err == nil {
		t.Errorf("generated profile accepted a spurious field (additionalProperties must stay false)")
	}
	hatch := experimentDoc(map[string]any{"objective": "x", "catalyst": "Ni",
		"metadata": map[string]any{"arbitrary": map[string]any{"anything": 1}}})
	if err := reg.Validate(ref, hatch); err != nil {
		t.Errorf("escape hatch closed: arbitrary metadata rejected: %v", err)
	}

	// The registration wrote exactly one audit entry naming the profile.
	if len(store.audits) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(store.audits))
	}
	a := store.audits[0]
	if a.Action != domain.ActionSchemaProfileRegistered || a.ActorID != "alice" || a.ProjectID != "p-1" {
		t.Errorf("audit entry = %+v", a)
	}
}

// experimentDoc assembles a payload the way the validator does: the
// server-authoritative facts plus the caller's content fields.
func experimentDoc(content map[string]any) []byte {
	doc := map[string]any{
		"id":              "11111111-1111-4111-8111-111111111111",
		"type":            "experiment",
		"version":         1,
		"project_id":      "p-1",
		"title":           "Catalysis run",
		"lifecycle_state": "active",
		"schema_ref":      map[string]any{"id": "project:p-1:catalysis", "version": "1"},
		"created_by":      "alice",
		"created_at":      "2026-01-01T00:00:00Z",
	}
	for k, v := range content {
		doc[k] = v
	}
	b, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return b
}

func containsString(list []any, want string) bool {
	for _, v := range list {
		if s, ok := v.(string); ok && s == want {
			return true
		}
	}
	return false
}

// TestRegisterRejectsInvalidInput: the shape rules refuse what could
// weaken the base or break the registry — a custom field that redefines a
// base field, a required name that names no property, a property def that
// is not an object, a bad name, a bad version label.
func TestRegisterRejectsInvalidInput(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newTestService(t)
	seedMaintainer(t, store, "p-1", "alice")
	actor := testUser("alice")

	cases := []struct {
		name string
		in   RegisterInput
	}{
		{"redefines base field", registerInput(map[string]any{"objective": map[string]any{"type": "string"}})},
		{"required names nothing", registerInput(nil, "ghost")},
		{"non-object definition", registerInput(map[string]any{"catalyst": "string"})},
		{"bad name", RegisterInput{Name: "Bad Name", Version: "1", Base: Ref{ID: experimentBase.ID, Version: experimentBase.Version}}},
		{"bad version", RegisterInput{Name: "ok_name", Version: "v/1", Base: Ref{ID: experimentBase.ID, Version: experimentBase.Version}}},
		{"empty base id", RegisterInput{Name: "ok_name", Version: "1", Base: Ref{ID: "", Version: "1"}}},
		{"empty base version", RegisterInput{Name: "ok_name", Version: "1", Base: Ref{ID: experimentBase.ID}}},
	}
	for _, tc := range cases {
		if _, err := svc.Register(ctx, actor, "p-1", tc.in); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: err = %v, want ErrValidation", tc.name, err)
		}
	}
}

// TestRegisterBaseQualification: the base must be an official (canonical
// namespace), registered, typed schema — an extension can never extend an
// extension, an unknown schema, or the untyped core shape.
func TestRegisterBaseQualification(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newTestService(t)
	seedMaintainer(t, store, "p-1", "alice")
	actor := testUser("alice")

	// A non-canonical id is refused: profiles extend official schemas only.
	_, err := svc.Register(ctx, actor, "p-1", RegisterInput{
		Name: "x", Version: "1",
		Base: Ref{ID: "project:p-9:other", Version: "1"},
	})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("non-canonical base: err = %v, want ErrValidation", err)
	}
	// An unknown canonical schema is refused.
	_, err = svc.Register(ctx, actor, "p-1", RegisterInput{
		Name: "x", Version: "1",
		Base: Ref{ID: schemareg.CanonicalNamespace + "no-such.schema.json", Version: "1"},
	})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("unknown base: err = %v, want ErrValidation", err)
	}
	// The core-scientific-object schema declares no type const: refused.
	_, err = svc.Register(ctx, actor, "p-1", RegisterInput{
		Name: "x", Version: "1",
		Base: Ref{ID: schemareg.CanonicalNamespace + "core-scientific-object.schema.json", Version: "1"},
	})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("untyped base: err = %v, want ErrValidation", err)
	}
}

// TestRegisterMaintainerGate: registration answers the maintainer-or-above
// gate with the settings surface's stable outcomes — a viewer and a
// non-member both get ErrForbidden (nothing disclosed), a hidden project
// gets ErrProjectNotFound (existence hiding).
func TestRegisterMaintainerGate(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newTestService(t)
	store.seedProject("p-1")
	store.seedMember("p-1", "alice", domain.ProjectRoleOwner)
	store.seedMember("p-1", "carol", domain.ProjectRoleViewer)

	if _, err := svc.Register(ctx, testUser("carol"), "p-1", registerInput(nil)); !errors.Is(err, ErrForbidden) {
		t.Errorf("viewer: err = %v, want ErrForbidden", err)
	}
	if _, err := svc.Register(ctx, testUser("bob"), "p-1", registerInput(nil)); !errors.Is(err, ErrForbidden) {
		t.Errorf("non-member: err = %v, want ErrForbidden", err)
	}
	// Authorization answers BEFORE input shape: a viewer with a malformed
	// payload gets the same ErrForbidden as with a valid one — validation
	// discloses nothing about a project the caller may not touch (docs/45).
	if _, err := svc.Register(ctx, testUser("carol"), "p-1", RegisterInput{Name: "Bad Name", Version: "v/1", Base: Ref{}}); !errors.Is(err, ErrForbidden) {
		t.Errorf("viewer with malformed payload: err = %v, want ErrForbidden (authorization first)", err)
	}
	// A project the actor may not read at all hides its existence.
	if _, err := svc.Register(ctx, testUser("bob"), "p-2", registerInput(nil)); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("hidden project: err = %v, want ErrProjectNotFound", err)
	}
	// Maintainer and owner both pass (distinct names: versions are
	// immutable, one name+version registers once).
	for _, role := range []domain.ProjectRole{domain.ProjectRoleMaintainer, domain.ProjectRoleOwner} {
		store.seedMember("p-1", "dana", role)
		in := registerInput(nil)
		in.Name = "profile_" + string(role)
		if _, err := svc.Register(ctx, testUser("dana"), "p-1", in); err != nil {
			t.Errorf("%s: err = %v, want nil", role, err)
		}
	}
}

// TestRegisterVersionImmutability: an id+version pair is never overwritten
// — a second registration under the same name+version refuses with
// ErrProfileVersionExists even when the content is identical (the row is
// append-only; new content takes a new version). A different version lands.
func TestRegisterVersionImmutability(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newTestService(t)
	seedMaintainer(t, store, "p-1", "alice")
	actor := testUser("alice")

	if _, err := svc.Register(ctx, actor, "p-1", registerInput(map[string]any{"catalyst": map[string]any{"type": "string"}})); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	// Same id+version, identical content — the registry no-ops but the
	// append-only row refuses.
	if _, err := svc.Register(ctx, actor, "p-1", registerInput(map[string]any{"catalyst": map[string]any{"type": "string"}})); !errors.Is(err, ErrProfileVersionExists) {
		t.Errorf("identical re-registration: err = %v, want ErrProfileVersionExists", err)
	}
	// Same id+version, different content — the registry itself refuses.
	if _, err := svc.Register(ctx, actor, "p-1", registerInput(map[string]any{"solvent": map[string]any{"type": "string"}})); !errors.Is(err, ErrProfileVersionExists) {
		t.Errorf("conflicting re-registration: err = %v, want ErrProfileVersionExists", err)
	}
	// A new version with new content lands beside the old one.
	v2, err := svc.Register(ctx, actor, "p-1", RegisterInput{
		Name:       "catalysis",
		Version:    "2",
		Base:       Ref{ID: experimentBase.ID, Version: experimentBase.Version},
		Properties: map[string]any{"catalyst": map[string]any{"type": "string"}, "solvent": map[string]any{"type": "string"}},
	})
	if err != nil {
		t.Fatalf("second version: %v", err)
	}
	if v2.Version != "2" {
		t.Errorf("v2 label = %s, want 2", v2.Version)
	}
}

// TestVersioningKeepsOldContentValid: after v2 lands, the v1 content is
// still registered and still validates the exact document shape v1
// required — a new version never rewrites history (docs/21 §8).
func TestVersioningKeepsOldContentValid(t *testing.T) {
	ctx := context.Background()
	svc, store, reg := newTestService(t)
	seedMaintainer(t, store, "p-1", "alice")
	actor := testUser("alice")

	v1, err := svc.Register(ctx, actor, "p-1", registerInput(map[string]any{"catalyst": map[string]any{"type": "string"}}, "catalyst"))
	if err != nil {
		t.Fatalf("v1 Register: %v", err)
	}
	if _, err := svc.Register(ctx, actor, "p-1", RegisterInput{
		Name:       "catalysis",
		Version:    "2",
		Base:       Ref{ID: experimentBase.ID, Version: experimentBase.Version},
		Properties: map[string]any{"catalyst": map[string]any{"type": "string"}, "solvent": map[string]any{"type": "string"}},
		Required:   []string{"catalyst"},
	}); err != nil {
		t.Fatalf("v2 Register: %v", err)
	}

	// The v1 document shape (no solvent — v1 never knew it) still passes
	// against the pinned v1 schema.
	v1doc := experimentDoc(map[string]any{"objective": "x", "catalyst": "Ni"})
	if err := reg.Validate(schemareg.Ref{ID: v1.SchemaID, Version: "1"}, v1doc); err != nil {
		t.Errorf("v1 document rejected against pinned v1: %v", err)
	}
	// v2's new field is only known to v2.
	if err := reg.Validate(schemareg.Ref{ID: v1.SchemaID, Version: "1"}, experimentDoc(map[string]any{"objective": "x", "catalyst": "Ni", "solvent": "MeOH"})); err == nil {
		t.Errorf("v1 schema accepted a solvent field it never declared")
	}
	if err := reg.Validate(schemareg.Ref{ID: v1.SchemaID, Version: "2"}, experimentDoc(map[string]any{"objective": "x", "catalyst": "Ni", "solvent": "MeOH"})); err != nil {
		t.Errorf("v2 schema rejected its own new field: %v", err)
	}
}

// TestReadsRunTheProjectGate: reads are exactly as visible as their
// project — a hidden project answers ErrProjectNotFound for Get, GetVersion
// and List alike.
func TestReadsRunTheProjectGate(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newTestService(t)
	seedMaintainer(t, store, "p-1", "alice")
	if _, err := svc.Register(ctx, testUser("alice"), "p-1", registerInput(nil)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	reader := projects.Reader{UserID: "bob", Authenticated: true}
	if _, err := svc.Get(ctx, reader, "p-2", "catalysis"); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("Get on hidden project: err = %v, want ErrProjectNotFound", err)
	}
	if _, err := svc.GetVersion(ctx, reader, "p-2", "catalysis", "1"); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("GetVersion on hidden project: err = %v, want ErrProjectNotFound", err)
	}
	if _, err := svc.List(ctx, reader, "p-2"); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("List on hidden project: err = %v, want ErrProjectNotFound", err)
	}
	// A visible project reads: newest version via Get, any age via
	// GetVersion, newest first via List.
	if got, err := svc.Get(ctx, reader, "p-1", "catalysis"); err != nil || got.Version != "1" {
		t.Errorf("Get = %+v, %v", got, err)
	}
	if got, err := svc.GetVersion(ctx, reader, "p-1", "catalysis", "1"); err != nil || got.Version != "1" {
		t.Errorf("GetVersion = %+v, %v", got, err)
	}
	if list, err := svc.List(ctx, reader, "p-1"); err != nil || len(list) != 1 {
		t.Errorf("List = %d rows, %v, want 1", len(list), err)
	}
}

// TestGetLatestProfileNoSilentFallback: the object-create resolution never
// falls back to a canonical schema — an unknown profile id answers
// ErrProfileNotFound.
func TestGetLatestProfileNoSilentFallback(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newTestService(t)
	seedMaintainer(t, store, "p-1", "alice")
	if _, err := svc.GetLatestProfile(ctx, "p-1", "project:p-1:ghost"); !errors.Is(err, ErrProfileNotFound) {
		t.Errorf("unknown profile: err = %v, want ErrProfileNotFound", err)
	}
}

// TestLoadAllRestoresTheRegistry: rows persisted by one process load into
// a fresh registry of another process — identical re-registration is a
// no-op and every persisted profile resolves afterwards (the restart
// convergence the Register ordering comment promises).
func TestLoadAllRestoresTheRegistry(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newTestService(t)
	seedMaintainer(t, store, "p-1", "alice")
	if _, err := svc.Register(ctx, testUser("alice"), "p-1", registerInput(map[string]any{"catalyst": map[string]any{"type": "string"}})); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// A fresh registry (a new process) + the same persisted rows.
	freshReg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	svc2 := NewService(Deps{Store: store, Projects: store, Schemas: freshReg})
	if err := svc2.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if _, err := freshReg.Lookup(schemareg.Ref{ID: "project:p-1:catalysis", Version: "1"}); err != nil {
		t.Errorf("persisted profile not registered after LoadAll: %v", err)
	}
	// The load is idempotent (registry no-op on identical content).
	if err := svc2.LoadAll(ctx); err != nil {
		t.Errorf("second LoadAll: %v", err)
	}
}

// TestLoadAllRefusesCorruption: corruption and unavailability are two
// different error classes. A stored content that does not re-hash to its
// stored hash, that conflicts with the registry's immutable content, or
// that does not compile as a JSON Schema is ErrCorruption — fail-fast,
// never retryable. (An unreachable store is ErrStore — retryable — and
// covered by TestStoreFailureFailsClosed.)
func TestLoadAllRefusesCorruption(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newTestService(t)
	seedMaintainer(t, store, "p-1", "alice")
	if _, err := svc.Register(ctx, testUser("alice"), "p-1", registerInput(map[string]any{"catalyst": map[string]any{"type": "string"}})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Same rows, fresh registry, tampered hash on the first row.
	freshReg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	tampered := append([]domain.ProjectSchemaProfile(nil), store.rows...)
	tampered[0].ContentHash = "0000000000000000000000000000000000000000000000000000000000000000"
	tamperedStore := &fakeStore{rows: tampered, projects: store.projects, members: store.members}
	svc2 := NewService(Deps{Store: tamperedStore, Projects: tamperedStore, Schemas: freshReg})
	if err := svc2.LoadAll(ctx); !errors.Is(err, ErrCorruption) {
		t.Errorf("LoadAll with tampered hash: err = %v, want ErrCorruption", err)
	}

	// Same rows, fresh registry, tampered content (hash recomputed so the
	// integrity check passes, but the registry holds different content).
	freshReg2, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	svc3 := NewService(Deps{Store: store, Projects: store, Schemas: freshReg2})
	conflicting := append([]domain.ProjectSchemaProfile(nil), store.rows...)
	conflicting[0].Content = `{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"project:p-1:catalysis","type":"object","properties":{},"required":[]}`
	sum := sha256.Sum256([]byte(conflicting[0].Content))
	conflicting[0].ContentHash = hex.EncodeToString(sum[:])
	// First load the tampered content into the registry...
	conflictingStore := &fakeStore{rows: conflicting, projects: store.projects, members: store.members}
	if err := NewService(Deps{Store: conflictingStore, Projects: conflictingStore, Schemas: freshReg2}).LoadAll(ctx); err != nil {
		t.Fatalf("seed conflicting content: %v", err)
	}
	// ...then the honest rows load against the registry that now holds
	// different content at the same ref: corruption, not a conflict.
	if err := svc3.LoadAll(ctx); !errors.Is(err, ErrCorruption) {
		t.Errorf("LoadAll against conflicting registry content: err = %v, want ErrCorruption", err)
	}

	// Content that does not compile as a JSON Schema (hash recomputed so
	// the integrity check passes) is corruption too: the load must never
	// put a non-schema into the runtime registry.
	freshReg3, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	invalid := append([]domain.ProjectSchemaProfile(nil), store.rows...)
	invalid[0].SchemaID = "project:p-1:broken"
	invalid[0].Content = `{"type":"object","properties":{"x":{"type":123}}}`
	invalidSum := sha256.Sum256([]byte(invalid[0].Content))
	invalid[0].ContentHash = hex.EncodeToString(invalidSum[:])
	invalidStore := &fakeStore{rows: invalid, projects: store.projects, members: store.members}
	if err := NewService(Deps{Store: invalidStore, Projects: invalidStore, Schemas: freshReg3}).LoadAll(ctx); !errors.Is(err, ErrCorruption) {
		t.Errorf("LoadAll with non-schema content: err = %v, want ErrCorruption", err)
	}
}

// TestStoreFailureFailsClosed: a broken store never answers a guessed
// outcome — reads and writes wrap as ErrStore. ErrCorruption passes
// through untouched: a port that signals corruption must never be
// downgraded into the retryable class.
func TestStoreFailureFailsClosed(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newTestService(t)
	seedMaintainer(t, store, "p-1", "alice")
	store.failWith = errors.New("boom")
	if _, err := svc.Register(ctx, testUser("alice"), "p-1", registerInput(nil)); !errors.Is(err, ErrStore) {
		t.Errorf("Register under store failure: err = %v, want ErrStore", err)
	}
	if _, err := svc.GetLatestProfile(ctx, "p-1", "project:p-1:catalysis"); !errors.Is(err, ErrStore) {
		t.Errorf("GetLatestProfile under store failure: err = %v, want ErrStore", err)
	}
	store.failWith = fmt.Errorf("tampered row: %w", ErrCorruption)
	if err := svc.LoadAll(ctx); !errors.Is(err, ErrCorruption) {
		t.Errorf("LoadAll under store-signaled corruption: err = %v, want ErrCorruption (never downgraded to ErrStore)", err)
	}
}

// TestServiceWithoutRegistryFailsClosed: a service wired without a
// registry refuses registration and loading rather than guessing.
func TestServiceWithoutRegistryFailsClosed(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	seedMaintainer(t, store, "p-1", "alice")
	svc := NewService(Deps{Store: store, Projects: store, Schemas: nil})
	if _, err := svc.Register(ctx, testUser("alice"), "p-1", registerInput(nil)); !errors.Is(err, ErrStore) {
		t.Errorf("Register without registry: err = %v, want ErrStore (fail closed)", err)
	}
	if err := svc.LoadAll(ctx); !errors.Is(err, ErrStore) {
		t.Errorf("LoadAll without registry: err = %v, want ErrStore (fail closed)", err)
	}
}

// TestServiceWithoutStoreFailsClosed: a service wired without a store
// refuses every call with ErrStore instead of panicking on the nil port
// (the T0209 unwired-port rule).
func TestServiceWithoutStoreFailsClosed(t *testing.T) {
	ctx := context.Background()
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	gate := newFakeStore()
	seedMaintainer(t, gate, "p-1", "alice")
	svc := NewService(Deps{Store: nil, Projects: gate, Schemas: reg})

	if _, err := svc.Register(ctx, testUser("alice"), "p-1", registerInput(nil)); !errors.Is(err, ErrStore) {
		t.Errorf("Register without store: err = %v, want ErrStore (fail closed)", err)
	}
	reader := projects.Reader{UserID: "alice", Authenticated: true}
	if _, err := svc.Get(ctx, reader, "p-1", "catalysis"); !errors.Is(err, ErrStore) {
		t.Errorf("Get without store: err = %v, want ErrStore (fail closed)", err)
	}
	if _, err := svc.GetVersion(ctx, reader, "p-1", "catalysis", "1"); !errors.Is(err, ErrStore) {
		t.Errorf("GetVersion without store: err = %v, want ErrStore (fail closed)", err)
	}
	if _, err := svc.List(ctx, reader, "p-1"); !errors.Is(err, ErrStore) {
		t.Errorf("List without store: err = %v, want ErrStore (fail closed)", err)
	}
	if _, err := svc.GetLatestProfile(ctx, "p-1", "project:p-1:catalysis"); !errors.Is(err, ErrStore) {
		t.Errorf("GetLatestProfile without store: err = %v, want ErrStore (fail closed)", err)
	}
	if err := svc.LoadAll(ctx); !errors.Is(err, ErrStore) {
		t.Errorf("LoadAll without store: err = %v, want ErrStore (fail closed)", err)
	}
}

// TestRegisterRefusesOverlongName: the name bound (64 characters) is
// enforced HERE, before the registry or the database ever see the name —
// a longer token would otherwise register into the in-memory registry and
// then die on the database CHECK (char_length(schema_id) <= 512), leaving
// a registry entry with no row behind it.
func TestRegisterRefusesOverlongName(t *testing.T) {
	ctx := context.Background()
	svc, store, reg := newTestService(t)
	seedMaintainer(t, store, "p-1", "alice")
	overlong := strings.Repeat("a", 65)
	_, err := svc.Register(ctx, testUser("alice"), "p-1", RegisterInput{
		Name:    overlong,
		Version: "1",
		Base:    Ref{ID: experimentBase.ID, Version: experimentBase.Version},
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("over-long name: err = %v, want ErrValidation", err)
	}
	// Nothing reached the registry or the store.
	if len(store.rows) != 0 {
		t.Errorf("store rows = %d, want 0 (refused before persistence)", len(store.rows))
	}
	if _, err := reg.Lookup(schemareg.Ref{ID: "project:p-1:" + overlong, Version: "1"}); err == nil {
		t.Errorf("over-long name reached the registry")
	}
}

// TestRegisterRequiresSameBaseAcrossVersions: one name, one base — a new
// version pinned to a different base would silently change which object
// type the name governs, breaking existing objects that pin the schema id.
func TestRegisterRequiresSameBaseAcrossVersions(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newTestService(t)
	seedMaintainer(t, store, "p-1", "alice")
	actor := testUser("alice")

	if _, err := svc.Register(ctx, actor, "p-1", registerInput(nil)); err != nil {
		t.Fatalf("v1 Register: %v", err)
	}
	// v2 of the same name pinned to a different base: refused.
	_, err := svc.Register(ctx, actor, "p-1", RegisterInput{
		Name:       "catalysis",
		Version:    "2",
		Base:       Ref{ID: hypothesisBase.ID, Version: hypothesisBase.Version},
		Properties: map[string]any{"catalyst": map[string]any{"type": "string"}},
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("v2 on a different base: err = %v, want ErrValidation", err)
	}
	if !strings.Contains(err.Error(), experimentBase.ID) || !strings.Contains(err.Error(), hypothesisBase.ID) {
		t.Errorf("refusal does not name both bases: %v", err)
	}
	if len(store.rows) != 1 {
		t.Errorf("store rows = %d, want 1 (the refused v2 must not persist)", len(store.rows))
	}
	// A different name may extend a different base — the rule is per name.
	if _, err := svc.Register(ctx, actor, "p-1", RegisterInput{
		Name:       "thesis",
		Version:    "1",
		Base:       Ref{ID: hypothesisBase.ID, Version: hypothesisBase.Version},
		Properties: map[string]any{"h": map[string]any{"type": "string"}},
	}); err != nil {
		t.Fatalf("different name, different base: %v", err)
	}
}

// TestRegisterRefusesUnsupportedBaseKeywords: the profile generator carries
// an exact set of top-level keywords; a base using anything else ($defs,
// $ref, if/then, allOf, ...) is refused instead of generating a profile
// that silently drops the keyword's meaning. The registry hard-refuses
// fabricated canonical ids (only shipped schemas are canonical), so the
// refusing function is exercised directly with a registered schema — the
// full-path refusal for a canonical base cannot be fabricated at runtime
// by construction, and resolveBase's canonical-namespace refusal is
// covered by TestRegisterBaseQualification.
func TestRegisterRefusesUnsupportedBaseKeywords(t *testing.T) {
	_, _, reg := newTestService(t)

	// A schema that compiles fine but uses a top-level keyword the
	// generated profile could not carry.
	weirdID := "project:p-9:weird"
	weirdDoc := map[string]any{
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"$id":                  weirdID,
		"title":                "Weird",
		"description":          "a base the generator cannot carry",
		"type":                 "object",
		"required":             []any{"type"},
		"properties":           map[string]any{"type": map[string]any{"const": "weird"}},
		"additionalProperties": false,
		"$defs": map[string]any{
			"shared": map[string]any{"type": "string"},
		},
	}
	content, err := json.Marshal(weirdDoc)
	if err != nil {
		t.Fatalf("marshal weird base: %v", err)
	}
	weird, err := reg.Register(weirdID, "1", content)
	if err != nil {
		t.Fatalf("register weird base: %v", err)
	}

	_, err = generateProfileDoc(weird, "project:p-1:weird_profile", "weird_profile", nil, nil)
	if err == nil {
		t.Fatal("base with $defs: err = nil, want a refusal naming the unsupported keyword")
	}
	if !strings.Contains(err.Error(), "$defs") {
		t.Errorf("refusal does not name the offending keyword: %v", err)
	}

	// The same document without the unsupported keyword generates fine —
	// the refusal is about the keyword, not the base itself.
	supported := map[string]any{
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"$id":                  "project:p-9:carriable",
		"title":                "Weird",
		"description":          "a base the generator can carry",
		"type":                 "object",
		"required":             []any{"type"},
		"properties":           map[string]any{"type": map[string]any{"const": "weird"}},
		"additionalProperties": false,
	}
	supportedContent, err := json.Marshal(supported)
	if err != nil {
		t.Fatalf("marshal supported base: %v", err)
	}
	supportedSchema, err := reg.Register("project:p-9:carriable", "1", supportedContent)
	if err != nil {
		t.Fatalf("register supported base: %v", err)
	}
	doc, err := generateProfileDoc(supportedSchema, "project:p-1:carriable_profile", "carriable_profile", nil, nil)
	if err != nil {
		t.Fatalf("supported base refused: %v", err)
	}
	if doc["additionalProperties"] != false {
		t.Errorf("additionalProperties = %v, want false", doc["additionalProperties"])
	}
}
