package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/relations"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/sciobjects"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/application/validation"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	"github.com/lichman0405/post/internal/rsg/semantics"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// Task T0208: V1 Scientific Object Domain Services — over a REAL
// PostgreSQL. Proves the required test "object type tests" and the write
// authorization acceptance criterion:
//
//   - every one of the eleven V1 object types is created AND versioned
//     through the rsg service (all eleven: first version + next version,
//     persisted as two append-only version rows each, every write as one
//     state commit on the branch);
//   - the relation command validates the catalog and the version-pinned
//     endpoints (unknown type, missing endpoint, foreign-project endpoint);
//   - every object/relation write refuses a non-member — and a member the
//     matrix denies (viewer) — with the exact same outcome whether the
//     target exists or not, and the denial precedes any target lookup so
//     nothing is written and no existence leaks (acceptance: 拒绝时不泄漏
//     对象是否存在);
//   - claim atomicity stays advisory: a compound-looking claim statement
//     commits with the CLAIM_MAY_BE_COMPOUND hint, never a failure.

// rsgFixture seeds three users, one private project owned by alice (carol
// is a viewer, bob is no member at all) and one public project, and wires
// the rsg service over the real stores, the real matrix engine and the
// real commit guard — the same composition cmd/api/main.go uses.
type rsgFixture struct {
	svc       *rsg.Service
	pool      *pgxpool.Pool
	alice     domain.User
	bob       domain.User
	carol     domain.User
	project   domain.Project
	publicP   domain.Project
	branch    string
	publicRef string
}

const rsgIntegrationTaskID = "T0208"

func newRSGFixture(t *testing.T, ctx context.Context) *rsgFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), rsgIntegrationTaskID)
	cred := persistence.NewCredentialStore(pool)
	seed := func(email, handle, name string) domain.User {
		u, err := cred.CreateWithPassword(ctx, email, "hash", handle, name)
		if err != nil {
			t.Fatalf("seed user %s: %v", handle, err)
		}
		return u
	}
	alice := seed("rsg-alice@example.com", "rsg-alice", "Alice")
	bob := seed("rsg-bob@example.com", "rsg-bob", "Bob")
	carol := seed("rsg-carol@example.com", "rsg-carol", "Carol")

	orgStore := persistence.NewOrgStore(pool)
	org, _, err := orgStore.CreateOrganization(ctx, domain.Organization{
		Slug: "rsg-fixture", Name: "RSG Fixture",
	}, alice.ID, todayUTC())
	if err != nil {
		t.Fatalf("create fixture org: %v", err)
	}
	projectStore := persistence.NewProjectStore(pool)
	project, _, err := projectStore.CreateProject(ctx, domain.Project{
		OrganizationID:  &org.ID,
		Slug:            "rsg-project",
		Name:            "RSG Project",
		Purpose:         "fixture purpose",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, alice.ID)
	if err != nil {
		t.Fatalf("create fixture project: %v", err)
	}
	publicP, _, err := projectStore.CreateProject(ctx, domain.Project{
		OrganizationID:  &org.ID,
		Slug:            "rsg-public-project",
		Name:            "RSG Public Project",
		Purpose:         "public fixture purpose",
		Visibility:      domain.VisibilityPublic,
		ProvisionStatus: domain.ProvisionPending,
	}, alice.ID)
	if err != nil {
		t.Fatalf("create public fixture project: %v", err)
	}
	// carol is a viewer of the private project; bob has no membership.
	if _, err := pool.Exec(ctx, `
		INSERT INTO project_memberships (project_id, user_id, role)
		VALUES ($1, $2, 'viewer')`, project.ID, carol.ID); err != nil {
		t.Fatalf("seed viewer membership: %v", err)
	}

	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	guard := validation.NewGuard(rsgvalidation.NewValidator(reg), persistence.NewValidationTxProbe())
	stateStore := persistence.NewStateStore(pool)
	svc := rsg.NewService(rsg.Deps{
		Projects:  projects.NewService(projectStore, orgStore, authz.NewMatrixEngine()),
		Branches:  branches.NewService(persistence.NewBranchStore(pool)),
		States:    states.NewService(stateStore, guard),
		Latest:    stateStore,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Queries:   persistence.NewRSGQueryStore(pool),
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
		Events:    events.Recorder{},
	})

	// The first branch bootstraps the genesis root through the real
	// service (BaseRef "" on a project with no state yet).
	branch, err := svc.CreateBranch(ctx, alice, project.ID, rsg.CreateBranchInput{
		Name:       "main",
		BaseRef:    "",
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}
	publicRef, err := svc.CreateBranch(ctx, alice, publicP.ID, rsg.CreateBranchInput{
		Name:       "main",
		BaseRef:    "",
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create public-project branch: %v", err)
	}

	return &rsgFixture{
		svc:       svc,
		pool:      pool,
		alice:     alice,
		bob:       bob,
		carol:     carol,
		project:   project,
		publicP:   publicP,
		branch:    branch.ID,
		publicRef: publicRef.ID,
	}
}

// TestEveryObjectTypeCreatesAndVersions is the required "object type
// tests": all eleven V1 object types go through create (version 1) and
// version (version 2) against the real database, each write leaving one
// state commit, two append-only version rows and a merged payload.
func TestEveryObjectTypeCreatesAndVersions(t *testing.T) {
	ctx := testCtx(t)
	f := newRSGFixture(t, ctx)

	types := []struct {
		objectType string
		payload    string
		patch      string
		mergeKeys  []string
	}{
		{"research_question", `{"statement":"What MOFs maximize CO2 uptake at 298 K?"}`, `{"objective":"screen MOFs"}`, []string{"statement", "objective"}},
		// The hypothesis question_id must name a REAL research question:
		// migration 00040's reference guard refuses a dangling reference
		// at commit. The question this test created above supplies the id
		// (the %s placeholder is filled in the loop below).
		{"hypothesis", `{"statement":"MOF-5 outperforms ZIF-8 at low pressure","question_id":"%s"}`, `{"confidence":"tentative"}`, []string{"statement", "question_id", "confidence"}},
		{"material", `{"name":"MOF-5"}`, `{"formula":"Zn4O(BDC)3"}`, []string{"name", "formula"}},
		{"sample", `{"name":"MOF-5 sample A"}`, `{"batch":"b1"}`, []string{"name", "batch"}},
		{"experiment", `{"name":"N2 isotherm run 1"}`, `{"temperature_k":298}`, []string{"name", "temperature_k"}},
		{"calculation", `{"name":"GCMC screening"}`, `{"method":"GCMC"}`, []string{"name", "method"}},
		{"dataset", `{"name":"isotherm series"}`, `{"units":"mmol/g"}`, []string{"name", "units"}},
		{"protocol", `{"name":"synthesis protocol"}`, `{"version_note":"v1.1"}`, []string{"name", "version_note"}},
		// A compound-looking statement: atomicity is a HINT, never a refusal.
		{"claim", `{"statement":"MOF-5 has high CO2 uptake and ZIF-8 has low uptake.","claim_type":"quantitative"}`, `{"scope":"298 K"}`, []string{"statement", "claim_type", "scope"}},
		{"finding", `{"name":"uptake finding"}`, `{"summary":"screened 50 MOFs"}`, []string{"name", "summary"}},
		{"external_reference", `{"canonical_url":"https://doi.org/10.5555/example"}`, `{"title":"a reference paper"}`, []string{"canonical_url", "title"}},
	}
	if len(types) != 11 {
		t.Fatalf("the table must carry all eleven V1 types, has %d", len(types))
	}

	compoundClaimHintSeen := false
	questionID := ""
	for _, tt := range types {
		t.Run(tt.objectType, func(t *testing.T) {
			payload := tt.payload
			if tt.objectType == "hypothesis" {
				if questionID == "" {
					t.Fatal("hypothesis runs before the research question that must supply its question_id")
				}
				payload = fmt.Sprintf(tt.payload, questionID)
			}
			res, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, f.branch, rsg.CreateObjectInput{
				ObjectType: tt.objectType,
				Payload:    json.RawMessage(payload),
			})
			if err != nil {
				t.Fatalf("CreateObject: %v", err)
			}
			if tt.objectType == "research_question" {
				questionID = res.Object.ID
			}
			if res.Object.ObjectType != tt.objectType || res.Object.ProjectID != f.project.ID {
				t.Errorf("object = %+v", res.Object)
			}
			if res.Version.VersionNo != 1 || res.Object.CurrentVersionNo != 1 {
				t.Fatalf("first version = %d, current = %d, want 1/1", res.Version.VersionNo, res.Object.CurrentVersionNo)
			}
			if res.Version.ID == "" || res.Version.StateID == "" {
				t.Fatalf("version id/state id empty: %+v", res.Version)
			}
			if res.Version.Title == "" {
				t.Errorf("title empty for %s (payload %s)", tt.objectType, tt.payload)
			}
			// The claim's compound statement is advisory only (acceptance:
			// Claim atomicity 只做提示不硬 NLP 判断).
			if tt.objectType == "claim" {
				for _, h := range res.Hints {
					if h.Code == semantics.HintClaimCompound {
						compoundClaimHintSeen = true
					}
				}
			}

			next, err := f.svc.CreateObjectVersion(ctx, f.alice, f.project.ID, f.branch, res.Object.ID, rsg.CreateObjectVersionInput{
				ExpectedVersion: 1,
				Patch:           json.RawMessage(tt.patch),
			})
			if err != nil {
				t.Fatalf("CreateObjectVersion: %v", err)
			}
			if next.Version.VersionNo != 2 || next.Version.ObjectID != res.Object.ID {
				t.Fatalf("next version = %d on %s, want 2 on %s", next.Version.VersionNo, next.Version.ObjectID, res.Object.ID)
			}
			var merged map[string]any
			if err := json.Unmarshal(next.Version.Payload, &merged); err != nil {
				t.Fatalf("merged payload not JSON: %v", err)
			}
			for _, key := range tt.mergeKeys {
				if _, ok := merged[key]; !ok {
					t.Errorf("merged payload lacks %q: %v", key, merged)
				}
			}
			// The head moved: the object read now answers version 2 with the
			// merged payload, and two append-only rows exist.
			got, err := f.svc.GetObject(ctx, projects.Reader{UserID: f.alice.ID, Authenticated: true}, f.project.ID, f.branch, res.Object.ID)
			if err != nil {
				t.Fatalf("GetObject: %v", err)
			}
			if got.Object.CurrentVersionNo != 2 || got.Version.VersionNo != 2 {
				t.Fatalf("read back version = %d, current = %d, want 2/2", got.Version.VersionNo, got.Object.CurrentVersionNo)
			}
			var count int
			if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM scientific_object_versions WHERE object_id = $1`, res.Object.ID).Scan(&count); err != nil {
				t.Fatalf("count versions: %v", err)
			}
			if count != 2 {
				t.Errorf("version rows = %d, want 2 (append-only log)", count)
			}
		})
	}

	// Every create/version landed as a state commit on the branch: 22
	// object-version-created operations (11 types × 2), 22 commit rows.
	var commits, versions int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM state_commits WHERE project_id = $1`, f.project.ID).Scan(&commits); err != nil {
		t.Fatalf("count commits: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM scientific_object_versions v JOIN scientific_objects o ON o.id = v.object_id WHERE o.project_id = $1`, f.project.ID).Scan(&versions); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if commits != 22 {
		t.Errorf("state commits = %d, want 22 (one per create/version)", commits)
	}
	if versions != 22 {
		t.Errorf("version rows = %d, want 22 (two per type)", versions)
	}
	// The compound claim's advisory hint fired on the real service path
	// (acceptance: Claim atomicity 只做提示不硬 NLP 判断).
	if !compoundClaimHintSeen {
		t.Errorf("compound claim: no %s hint on any claim create", semantics.HintClaimCompound)
	}
}

// TestRelationCreateValidatesCatalogAndEndpoints: relation writes land as
// version-pinned edges in one commit; an unknown catalog type, a missing
// endpoint and a foreign-project endpoint are each refused with their
// stable typed error.
func TestRelationCreateValidatesCatalogAndEndpoints(t *testing.T) {
	ctx := testCtx(t)
	f := newRSGFixture(t, ctx)

	src, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, f.branch, rsg.CreateObjectInput{
		ObjectType: "material", Payload: json.RawMessage(`{"name":"MOF-5"}`),
	})
	if err != nil {
		t.Fatalf("create source object: %v", err)
	}
	dst, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, f.branch, rsg.CreateObjectInput{
		ObjectType: "material", Payload: json.RawMessage(`{"name":"ZIF-8"}`),
	})
	if err != nil {
		t.Fatalf("create target object: %v", err)
	}
	// An object in the OTHER (public) project: a relation in the private
	// project may not pin it.
	foreign, err := f.svc.CreateObject(ctx, f.alice, f.publicP.ID, f.publicRef, rsg.CreateObjectInput{
		ObjectType: "material", Payload: json.RawMessage(`{"name":"UIO-66"}`),
	})
	if err != nil {
		t.Fatalf("create foreign object: %v", err)
	}

	rel, err := f.svc.CreateRelation(ctx, f.alice, f.project.ID, f.branch, rsg.CreateRelationInput{
		RelationType:          "derived_from",
		SourceObjectVersionID: src.Version.ID,
		TargetObjectVersionID: dst.Version.ID,
	})
	if err != nil {
		t.Fatalf("CreateRelation: %v", err)
	}
	if rel.Version.VersionNo != 1 || rel.Relation.CurrentVersionNo != 1 {
		t.Fatalf("relation version = %d, current = %d, want 1/1", rel.Version.VersionNo, rel.Relation.CurrentVersionNo)
	}
	if rel.Version.SourceObjectVersionID != src.Version.ID || rel.Version.TargetObjectVersionID != dst.Version.ID {
		t.Errorf("pinned endpoints = %s/%s, want %s/%s",
			rel.Version.SourceObjectVersionID, rel.Version.TargetObjectVersionID, src.Version.ID, dst.Version.ID)
	}

	_, err = f.svc.CreateRelation(ctx, f.alice, f.project.ID, f.branch, rsg.CreateRelationInput{
		RelationType:          "not_in_catalog",
		SourceObjectVersionID: src.Version.ID,
		TargetObjectVersionID: dst.Version.ID,
	})
	var unknown *relations.UnknownRelationTypeError
	if !errors.As(err, &unknown) || unknown.Type != "not_in_catalog" {
		t.Fatalf("unknown type: err = %v, want *UnknownRelationTypeError", err)
	}

	_, err = f.svc.CreateRelation(ctx, f.alice, f.project.ID, f.branch, rsg.CreateRelationInput{
		RelationType:          "derived_from",
		SourceObjectVersionID: "99999999-9999-4999-8999-999999999999",
		TargetObjectVersionID: dst.Version.ID,
	})
	var missing *relations.ReferencedVersionNotFoundError
	if !errors.As(err, &missing) || missing.Side != "source" {
		t.Fatalf("missing endpoint: err = %v, want *ReferencedVersionNotFoundError{side: source}", err)
	}

	_, err = f.svc.CreateRelation(ctx, f.alice, f.project.ID, f.branch, rsg.CreateRelationInput{
		RelationType:          "derived_from",
		SourceObjectVersionID: src.Version.ID,
		TargetObjectVersionID: foreign.Version.ID,
	})
	if !errors.As(err, &missing) || missing.Side != "target" {
		t.Fatalf("foreign endpoint: err = %v, want *ReferencedVersionNotFoundError{side: target}", err)
	}
}

// TestWriteDenialPrecedesLookupAndHidesExistence is the authorization
// acceptance criterion: for a non-member the write refuses with the same
// outcome whether the branch/object exists or not (private project: the
// project itself is hidden; public project: a flat forbidden); for a
// viewer the matrix denies with a flat forbidden before any target lookup
// — and no denied attempt writes anything.
func TestWriteDenialPrecedesLookupAndHidesExistence(t *testing.T) {
	ctx := testCtx(t)
	f := newRSGFixture(t, ctx)

	obj, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, f.branch, rsg.CreateObjectInput{
		ObjectType: "material", Payload: json.RawMessage(`{"name":"MOF-5"}`),
	})
	if err != nil {
		t.Fatalf("seed object: %v", err)
	}

	var objectsBefore int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM scientific_objects WHERE project_id = $1`, f.project.ID).Scan(&objectsBefore); err != nil {
		t.Fatalf("count objects: %v", err)
	}

	// bob is no member of the private project: denied with the project
	// itself hidden — identical for an existing and a nonexistent branch.
	_, errExisting := f.svc.CreateObject(ctx, f.bob, f.project.ID, f.branch, rsg.CreateObjectInput{
		ObjectType: "material", Payload: json.RawMessage(`{"name":"intrusion"}`),
	})
	_, errMissing := f.svc.CreateObject(ctx, f.bob, f.project.ID, "99999999-9999-4999-8999-999999999999", rsg.CreateObjectInput{
		ObjectType: "material", Payload: json.RawMessage(`{"name":"intrusion"}`),
	})
	if !errors.Is(errExisting, projects.ErrProjectNotFound) {
		t.Fatalf("non-member on existing branch: err = %v, want ErrProjectNotFound", errExisting)
	}
	if !errors.Is(errMissing, projects.ErrProjectNotFound) {
		t.Fatalf("non-member on missing branch: err = %v, want the same ErrProjectNotFound (existence must not leak)", errMissing)
	}

	// carol is a viewer: the matrix denies scientific-state writes with a
	// flat forbidden — identical for an existing and a nonexistent object.
	_, errExisting = f.svc.CreateObjectVersion(ctx, f.carol, f.project.ID, f.branch, obj.Object.ID, rsg.CreateObjectVersionInput{
		ExpectedVersion: 1, Patch: json.RawMessage(`{"name":"renamed"}`),
	})
	_, errMissing = f.svc.CreateObjectVersion(ctx, f.carol, f.project.ID, f.branch, "99999999-9999-4999-8999-999999999999", rsg.CreateObjectVersionInput{
		ExpectedVersion: 1, Patch: json.RawMessage(`{"name":"renamed"}`),
	})
	if !errors.Is(errExisting, rsg.ErrForbidden) {
		t.Fatalf("viewer on existing object: err = %v, want ErrForbidden", errExisting)
	}
	if !errors.Is(errMissing, rsg.ErrForbidden) {
		t.Fatalf("viewer on missing object: err = %v, want the same ErrForbidden (existence must not leak)", errMissing)
	}

	// The public project is readable, so the non-member is told "forbidden"
	// — again without learning whether the branch exists.
	_, errPublic := f.svc.CreateObject(ctx, f.bob, f.publicP.ID, f.publicRef, rsg.CreateObjectInput{
		ObjectType: "material", Payload: json.RawMessage(`{"name":"intrusion"}`),
	})
	if !errors.Is(errPublic, rsg.ErrForbidden) {
		t.Fatalf("non-member on public project: err = %v, want ErrForbidden", errPublic)
	}

	// Relations refuse a viewer the same way, before any endpoint lookup.
	_, errRel := f.svc.CreateRelation(ctx, f.carol, f.project.ID, f.branch, rsg.CreateRelationInput{
		RelationType:          "derived_from",
		SourceObjectVersionID: obj.Version.ID,
		TargetObjectVersionID: "99999999-9999-4999-8999-999999999999",
	})
	if !errors.Is(errRel, rsg.ErrForbidden) {
		t.Fatalf("viewer on relation: err = %v, want ErrForbidden", errRel)
	}

	// None of the denied attempts wrote anything.
	var objectsAfter int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM scientific_objects WHERE project_id = $1`, f.project.ID).Scan(&objectsAfter); err != nil {
		t.Fatalf("count objects: %v", err)
	}
	if objectsAfter != objectsBefore {
		t.Errorf("denied attempts wrote: objects %d -> %d", objectsBefore, objectsAfter)
	}
}

// TestGetObjectVisibility: the read is exactly as visible as its project —
// a member reads, a non-member of the private project gets the
// not-found shape, and an object of another project answers not-found
// even for its owner.
func TestGetObjectVisibility(t *testing.T) {
	ctx := testCtx(t)
	f := newRSGFixture(t, ctx)

	obj, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, f.branch, rsg.CreateObjectInput{
		ObjectType: "material", Payload: json.RawMessage(`{"name":"MOF-5"}`),
	})
	if err != nil {
		t.Fatalf("create object: %v", err)
	}

	_, err = f.svc.GetObject(ctx, projects.Reader{UserID: f.bob.ID, Authenticated: true}, f.project.ID, f.branch, obj.Object.ID)
	if !errors.Is(err, projects.ErrProjectNotFound) {
		t.Fatalf("non-member read: err = %v, want ErrProjectNotFound", err)
	}
	_, err = f.svc.GetObject(ctx, projects.Reader{UserID: f.alice.ID, Authenticated: true}, f.publicP.ID, f.publicRef, obj.Object.ID)
	if !errors.Is(err, sciobjects.ErrObjectNotFound) {
		t.Fatalf("cross-project read: err = %v, want ErrObjectNotFound", err)
	}
	got, err := f.svc.GetObject(ctx, projects.Reader{UserID: f.carol.ID, Authenticated: true}, f.project.ID, f.branch, obj.Object.ID)
	if err != nil {
		t.Fatalf("viewer read: %v", err)
	}
	if got.Object.ID != obj.Object.ID || got.Version.VersionNo != 1 {
		t.Errorf("viewer read = %+v", got)
	}
}

// TestVersionConflictPassthrough: a stale expected_version loses the CAS
// against the real append-only log with the stable conflict outcome.
func TestVersionConflictPassthrough(t *testing.T) {
	ctx := testCtx(t)
	f := newRSGFixture(t, ctx)

	obj, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, f.branch, rsg.CreateObjectInput{
		ObjectType: "material", Payload: json.RawMessage(`{"name":"MOF-5"}`),
	})
	if err != nil {
		t.Fatalf("create object: %v", err)
	}
	if _, err := f.svc.CreateObjectVersion(ctx, f.alice, f.project.ID, f.branch, obj.Object.ID, rsg.CreateObjectVersionInput{
		ExpectedVersion: 1, Patch: json.RawMessage(`{"formula":"Zn4O(BDC)3"}`),
	}); err != nil {
		t.Fatalf("version 2: %v", err)
	}
	// A caller that still expects version 1 loses the compare-and-swap.
	_, err = f.svc.CreateObjectVersion(ctx, f.alice, f.project.ID, f.branch, obj.Object.ID, rsg.CreateObjectVersionInput{
		ExpectedVersion: 1, Patch: json.RawMessage(`{"formula":"Zn4O(BDC)3"}`),
	})
	var conflict *sciobjects.VersionConflictError
	if !errors.As(err, &conflict) || conflict.Actual != 2 {
		t.Fatalf("stale expectation: err = %v, want *VersionConflictError{Actual: 2}", err)
	}
}
