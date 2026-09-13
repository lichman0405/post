package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/relations"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rsg/relationcatalog"
)

// Task T0203: the typed relation repository
// (persistence.RelationStore implementing relations.Repository) over a REAL
// PostgreSQL. Proves the three requirements and both acceptance criteria:
//
//   - version-pinned endpoints: source and target are foreign keys to
//     scientific_object_versions, so a write naming a non-existent version
//     is rejected (stable *ReferencedVersionNotFoundError naming the side),
//     never stored, and leaves no half-written relation behind;
//   - relation type catalog validation: every write goes through the
//     service, which rejects a type outside the V1 catalog (docs/44) with
//     *UnknownRelationTypeError / RSG_VALIDATION_FAILED;
//   - immutable versions: create relation + v1 atomically, append the next
//     version guarded by expected_version (a compare-and-swap on
//     relations.current_version_no, migration 00025), no repository update
//     path, and the database rejects UPDATE/DELETE of version rows itself
//     (migrations 00014/00015);
//   - dependency/provenance types are queryable: ListVersionsByType for a
//     concrete type and ListVersionsByTypes for the catalog's
//     DependencyTypes() / ProvenanceTypes() families, scoped to the
//     project.
//
// Scientific objects are seeded with direct SQL: the object repository
// arrives with the parallel T0202 delivery, and this task must stay
// verifiable on its own baseline.
const relationTaskID = "T0203"

// relationFixture seeds the store with alice, an organization, two
// projects (each with one project state) and scientific object versions
// created by direct SQL. The raw pool is kept for direct-SQL probes
// against the same database the store writes.
type relationFixture struct {
	service      *relations.Service
	store        *persistence.RelationStore
	pool         *pgxpool.Pool
	alice        domain.User
	project      domain.Project
	otherProject domain.Project
	stateID      string
	otherStateID string
	// sourceV1 / targetV1 are version 1 rows of two objects in project;
	// otherV1 / otherV2 are versions in otherProject.
	sourceV1 string
	targetV1 string
	otherV1  string
	otherV2  string
}

func newRelationFixture(t *testing.T, ctx context.Context) *relationFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), relationTaskID)
	alice, err := persistence.NewCredentialStore(pool).CreateWithPassword(
		ctx, "rel-alice@example.com", "hash", "rel-alice", "Alice")
	if err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	orgStore := persistence.NewOrgStore(pool)
	org, _, err := orgStore.CreateOrganization(ctx, domain.Organization{
		Slug: "rel-fixture", Name: "Relation Fixture",
	}, alice.ID, todayUTC())
	if err != nil {
		t.Fatalf("create fixture org: %v", err)
	}
	orgID := org.ID
	newProject := func(slug, name string) domain.Project {
		t.Helper()
		project, _, err := persistence.NewProjectStore(pool).CreateProject(ctx, domain.Project{
			OrganizationID:  &orgID,
			Slug:            slug,
			Name:            name,
			Purpose:         "fixture purpose",
			Visibility:      domain.VisibilityPrivate,
			ProvisionStatus: domain.ProvisionPending,
		}, alice.ID)
		if err != nil {
			t.Fatalf("create fixture project %s: %v", slug, err)
		}
		return project
	}
	project := newProject("rel-project", "Relation Project")
	otherProject := newProject("rel-other-project", "Relation Other Project")
	newState := func(projectID, hash string) string {
		t.Helper()
		var stateID string
		if err := pool.QueryRow(ctx, `
			INSERT INTO project_states (project_id, state_hash, manifest_version)
			VALUES ($1, $2, $3) RETURNING id`,
			projectID, hash, "v1").Scan(&stateID); err != nil {
			t.Fatalf("seed project state: %v", err)
		}
		return stateID
	}
	stateID := newState(project.ID, "rel-state-hash-1")
	otherStateID := newState(otherProject.ID, "rel-other-state-hash-1")
	// One object version per endpoint, seeded directly (no object
	// repository on this baseline).
	newObjectVersion := func(projectID, stateID, title string) string {
		t.Helper()
		var objectID, versionID string
		if err := pool.QueryRow(ctx, `
			INSERT INTO scientific_objects (project_id, object_type, created_by)
			VALUES ($1, 'experiment', $2) RETURNING id`,
			projectID, alice.ID).Scan(&objectID); err != nil {
			t.Fatalf("seed scientific object: %v", err)
		}
		if err := pool.QueryRow(ctx, `
			INSERT INTO scientific_object_versions
				(object_id, version_no, state_id, schema_id, schema_version,
				 title, lifecycle_state, payload, integrity_hash, created_by)
			VALUES ($1, 1, $2, 'core/experiment', '1.0', $3, 'active',
			        '{}'::jsonb, 'seeded-hash', $4) RETURNING id`,
			objectID, stateID, title, alice.ID).Scan(&versionID); err != nil {
			t.Fatalf("seed scientific object version: %v", err)
		}
		return versionID
	}
	fx := &relationFixture{
		store:        persistence.NewRelationStore(pool),
		pool:         pool,
		alice:        alice,
		project:      project,
		otherProject: otherProject,
		stateID:      stateID,
		otherStateID: otherStateID,
	}
	fx.service = relations.NewService(fx.store)
	fx.sourceV1 = newObjectVersion(project.ID, stateID, "Fixture source")
	fx.targetV1 = newObjectVersion(project.ID, stateID, "Fixture target")
	fx.otherV1 = newObjectVersion(otherProject.ID, otherStateID, "Fixture other source")
	fx.otherV2 = newObjectVersion(otherProject.ID, otherStateID, "Fixture other target")
	return fx
}

// createParams builds valid creation input against the fixture: a
// depends_on edge (the dependency type) from sourceV1 to targetV1.
func (fx *relationFixture) createParams(payload any) relations.CreateRelationParams {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(fmt.Sprintf("relationFixture: marshal payload: %v", err))
	}
	return relations.CreateRelationParams{
		ProjectID: fx.project.ID,
		Version: relations.VersionParams{
			StateID:               fx.stateID,
			RelationType:          "depends_on",
			SourceObjectVersionID: fx.sourceV1,
			TargetObjectVersionID: fx.targetV1,
			Payload:               raw,
			CreatedBy:             fx.alice.ID,
		},
	}
}

// wantRelationHash is the sha256 hex digest the store must have computed for
// payload.
func wantRelationHash(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// assertRelationVersionEqual compares two version values field by field
// (the domain type carries json.RawMessage, which is not comparable as a
// struct).
func assertRelationVersionEqual(t *testing.T, got, want domain.RelationVersion) {
	t.Helper()
	if got.ID != want.ID || got.RelationID != want.RelationID || got.VersionNo != want.VersionNo ||
		got.StateID != want.StateID || got.RelationType != want.RelationType ||
		got.SourceObjectVersionID != want.SourceObjectVersionID ||
		got.TargetObjectVersionID != want.TargetObjectVersionID ||
		got.IntegrityHash != want.IntegrityHash ||
		got.CreatedBy != want.CreatedBy || !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("version mismatch:\ngot  %+v\nwant %+v", got, want)
	}
	if !bytes.Equal(got.Payload, want.Payload) {
		t.Errorf("payload mismatch:\ngot  %s\nwant %s", got.Payload, want.Payload)
	}
}

// TestRelationRepositoryCreateRelationAndV1: the creation transaction
// produces the relation row and its version 1 together, the head pointer
// starts at 1, endpoints are pinned to the exact object versions given,
// the payload round-trips and the integrity hash is the server-computed
// sha256 of the exact stored payload bytes.
func TestRelationRepositoryCreateRelationAndV1(t *testing.T) {
	ctx := testCtx(t)
	fx := newRelationFixture(t, ctx)

	payload := map[string]any{"scope": "v1", "note": "first"}
	rel, v1, err := fx.service.CreateRelation(ctx, fx.createParams(payload))
	if err != nil {
		t.Fatalf("create relation: %v", err)
	}
	if rel.ID == "" || rel.ProjectID != fx.project.ID {
		t.Errorf("relation = %+v, want fixture identity", rel)
	}
	if rel.CurrentVersionNo != 1 {
		t.Errorf("relation current_version_no = %d, want 1", rel.CurrentVersionNo)
	}
	if v1.RelationID != rel.ID || v1.VersionNo != 1 {
		t.Errorf("v1 = %+v, want relation %s version 1", v1, rel.ID)
	}
	if v1.RelationType != "depends_on" {
		t.Errorf("v1 relation_type = %q, want depends_on", v1.RelationType)
	}
	// The endpoints are pinned to the exact object versions the fixture
	// seeded — not the objects, not a string copy of them.
	if v1.SourceObjectVersionID != fx.sourceV1 || v1.TargetObjectVersionID != fx.targetV1 {
		t.Errorf("v1 endpoints = (%s -> %s), want (%s -> %s)",
			v1.SourceObjectVersionID, v1.TargetObjectVersionID, fx.sourceV1, fx.targetV1)
	}
	wantRaw, _ := json.Marshal(payload)
	var gotPayload, wantPayload any
	if err := json.Unmarshal(v1.Payload, &gotPayload); err != nil {
		t.Fatalf("v1 payload is not JSON: %v", err)
	}
	if err := json.Unmarshal(wantRaw, &wantPayload); err != nil {
		t.Fatalf("want payload is not JSON: %v", err)
	}
	if fmt.Sprint(gotPayload) != fmt.Sprint(wantPayload) {
		t.Errorf("v1 payload = %s, want semantic %s", v1.Payload, wantRaw)
	}
	if v1.IntegrityHash != wantRelationHash(v1.Payload) {
		t.Errorf("v1 integrity hash = %s, want sha256(stored payload) = %s", v1.IntegrityHash, wantRelationHash(v1.Payload))
	}

	// Every read path returns the same row.
	gotRel, err := fx.store.GetRelation(ctx, rel.ID)
	if err != nil {
		t.Fatalf("get relation: %v", err)
	}
	if gotRel.ID != rel.ID || gotRel.ProjectID != rel.ProjectID || gotRel.CurrentVersionNo != 1 {
		t.Errorf("GetRelation = %+v, want %+v", gotRel, rel)
	}
	gotV1, err := fx.store.GetVersion(ctx, rel.ID, 1)
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	assertRelationVersionEqual(t, gotV1, v1)
	latest, err := fx.store.GetLatestVersion(ctx, rel.ID)
	if err != nil {
		t.Fatalf("get latest: %v", err)
	}
	assertRelationVersionEqual(t, latest, v1)
	log, err := fx.store.ListVersions(ctx, rel.ID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(log) != 1 {
		t.Fatalf("ListVersions = %+v, want exactly one version", log)
	}
	assertRelationVersionEqual(t, log[0], v1)

	// A value that cannot name a relation is ErrRelationNotFound, never a
	// 500.
	if _, err := fx.store.GetRelation(ctx, "not-a-uuid"); !errors.Is(err, relations.ErrRelationNotFound) {
		t.Errorf("GetRelation(not-a-uuid) err = %v, want ErrRelationNotFound", err)
	}
}

// TestRelationRepositoryRejectsUnknownRelationType: the type catalog is
// the write gate. A type outside the V1 catalog fails explicitly with
// *UnknownRelationTypeError and code RSG_VALIDATION_FAILED, and no row is
// written; dependency and provenance types pass.
func TestRelationRepositoryRejectsUnknownRelationType(t *testing.T) {
	ctx := testCtx(t)
	fx := newRelationFixture(t, ctx)

	for _, tc := range []struct {
		name string
		typ  string
		want error
	}{
		{"dependency type accepted", "depends_on", nil},
		{"provenance type accepted", "derived_from", nil},
		{"unknown type rejected", "friend_of", &relations.UnknownRelationTypeError{Type: "friend_of"}},
		{"namespaced extension rejected in V1", "materials:synthesized_from", &relations.UnknownRelationTypeError{Type: "materials:synthesized_from"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := fx.createParams(map[string]any{"t": tc.typ})
			in.Version.RelationType = tc.typ
			_, _, err := fx.service.CreateRelation(ctx, in)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("CreateRelation(%q): %v, want success", tc.typ, err)
				}
				return
			}
			var got *relations.UnknownRelationTypeError
			if !errors.As(err, &got) {
				t.Fatalf("CreateRelation(%q) err = %v, want *UnknownRelationTypeError", tc.typ, err)
			}
			if got.Type != tc.typ || got.Code() != relations.CodeRSGValidationFailed {
				t.Errorf("unknown-type error = %+v (code %s), want type %q code %s",
					got, got.Code(), tc.typ, relations.CodeRSGValidationFailed)
			}
		})
	}

	// The rejected writes left nothing behind.
	var count int
	if err := fx.pool.QueryRow(ctx, `SELECT count(*) FROM relations WHERE project_id = $1`, fx.project.ID).Scan(&count); err != nil {
		t.Fatalf("count relations: %v", err)
	}
	if count != 2 { // only the two "accepted" writes above
		t.Errorf("relations in project = %d, want 2 (rejected types must not create rows)", count)
	}
}

// TestRelationRepositoryRejectsNonexistentEndpointVersions (acceptance:
// 禁止引用不存在版本): an edge whose source or target names a scientific
// object version that does not exist is rejected with a stable
// *ReferencedVersionNotFoundError naming the side — never stored, never a
// half-written relation. Nonexistent project/state are validation
// failures, also without rows.
func TestRelationRepositoryRejectsNonexistentEndpointVersions(t *testing.T) {
	ctx := testCtx(t)
	fx := newRelationFixture(t, ctx)
	bogus := "00000000-0000-0000-0000-0000000000ab"

	// One valid edge first: the probes below must leave exactly this row
	// set untouched.
	kept, _, err := fx.service.CreateRelation(ctx, fx.createParams(map[string]any{"kept": true}))
	if err != nil {
		t.Fatalf("create relation: %v", err)
	}

	countRelations := func() int {
		t.Helper()
		var n int
		if err := fx.pool.QueryRow(ctx, `SELECT count(*) FROM relations WHERE project_id = $1`, fx.project.ID).Scan(&n); err != nil {
			t.Fatalf("count relations: %v", err)
		}
		return n
	}

	for _, tc := range []struct {
		name string
		mut  func(in *relations.CreateRelationParams)
		want string // expected error code
	}{
		{"dangling source version", func(in *relations.CreateRelationParams) { in.Version.SourceObjectVersionID = bogus }, "OBJECT_VERSION_NOT_FOUND"},
		{"dangling target version", func(in *relations.CreateRelationParams) { in.Version.TargetObjectVersionID = bogus }, "OBJECT_VERSION_NOT_FOUND"},
		{"nonexistent project", func(in *relations.CreateRelationParams) { in.ProjectID = "00000000-0000-0000-0000-0000000000cd" }, "VALIDATION_FAILED"},
		{"nonexistent state", func(in *relations.CreateRelationParams) { in.Version.StateID = "00000000-0000-0000-0000-0000000000ef" }, "VALIDATION_FAILED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := fx.createParams(map[string]any{"probe": tc.name})
			tc.mut(&in)
			before := countRelations()
			_, _, err := fx.service.CreateRelation(ctx, in)
			if err == nil {
				t.Fatalf("CreateRelation succeeded, want rejection")
			}
			var ref *relations.ReferencedVersionNotFoundError
			if errors.As(err, &ref) {
				if ref.Code() != tc.want {
					t.Errorf("err = %v, want code %s", err, tc.want)
				}
				switch {
				case strings.Contains(tc.name, "source") && ref.Side != "source":
					t.Errorf("dangling source reported side %q, want source", ref.Side)
				case strings.Contains(tc.name, "target") && ref.Side != "target":
					t.Errorf("dangling target reported side %q, want target", ref.Side)
				}
				if ref.VersionID != bogus {
					t.Errorf("dangling version id = %q, want %q", ref.VersionID, bogus)
				}
			} else if !errors.Is(err, relations.ErrValidation) {
				t.Fatalf("err = %v, want *ReferencedVersionNotFoundError or ErrValidation", err)
			}
			if after := countRelations(); after != before {
				t.Errorf("failed write left rows: before %d after %d", before, after)
			}
		})
	}

	// The DB layer itself pins the endpoints: a direct INSERT naming a
	// non-existent source version cannot bypass the repository's mapping.
	_, err = fx.pool.Exec(ctx, `
		INSERT INTO relation_versions
			(relation_id, version_no, state_id, relation_type,
			 source_object_version_id, target_object_version_id,
			 payload, integrity_hash, created_by)
		VALUES ($1, 9, $2, 'depends_on', $3, $4, '{}'::jsonb, 'x', $5)`,
		kept.ID, fx.stateID, bogus, fx.targetV1, fx.alice.ID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("direct dangling-source insert err = %v, want 23503 foreign_key_violation", err)
	}

	// CreateVersion is guarded the same way: a new version with a dangling
	// endpoint is rejected and the version log is untouched.
	params := fx.createParams(map[string]any{"v2": true}).Version
	params.SourceObjectVersionID = bogus
	if _, err := fx.service.CreateVersion(ctx, kept.ID, 1, params); err == nil {
		t.Fatal("CreateVersion with dangling source succeeded, want rejection")
	}
	log, err := fx.store.ListVersions(ctx, kept.ID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(log) != 1 {
		t.Errorf("version log length = %d, want 1 (failed v2 must not append)", len(log))
	}
}

// TestRelationRepositoryCreateVersionExpectedSemantics: sequential
// expected_version behaviour — a correct expectation appends the next
// number, a stale one and an over-optimistic one both fail with the typed
// conflict, an unknown relation is ErrRelationNotFound, and failed
// attempts never advance the head.
func TestRelationRepositoryCreateVersionExpectedSemantics(t *testing.T) {
	ctx := testCtx(t)
	fx := newRelationFixture(t, ctx)

	rel, _, err := fx.service.CreateRelation(ctx, fx.createParams(map[string]any{"n": 1}))
	if err != nil {
		t.Fatalf("create relation: %v", err)
	}

	// expected = current → next version.
	v2, err := fx.service.CreateVersion(ctx, rel.ID, 1, fx.createParams(map[string]any{"n": 2}).Version)
	if err != nil {
		t.Fatalf("create version with expected=1: %v", err)
	}
	if v2.VersionNo != 2 {
		t.Errorf("v2 version_no = %d, want 2", v2.VersionNo)
	}

	// Stale expectation (the head already moved past 1).
	var conflict *relations.VersionConflictError
	_, err = fx.service.CreateVersion(ctx, rel.ID, 1, fx.createParams(map[string]any{"n": 3}).Version)
	if !errors.As(err, &conflict) {
		t.Fatalf("stale expected=1 err = %v, want *VersionConflictError", err)
	}
	if conflict.Expected != 1 || conflict.Actual != 2 || conflict.RelationID != rel.ID {
		t.Errorf("stale conflict = %+v, want expected 1 actual 2", conflict)
	}
	if conflict.Code() != relations.CodeVersionConflict || relations.CodeVersionConflict != "EXPECTED_VERSION_MISMATCH" {
		t.Errorf("conflict code = %q, want %q (docs/45)", conflict.Code(), "EXPECTED_VERSION_MISMATCH")
	}

	// Over-optimistic expectation.
	_, err = fx.service.CreateVersion(ctx, rel.ID, 7, fx.createParams(map[string]any{"n": 4}).Version)
	if !errors.As(err, &conflict) {
		t.Fatalf("expected=7 err = %v, want *VersionConflictError", err)
	}
	if conflict.Actual != 2 {
		t.Errorf("expected=7 conflict actual = %d, want 2", conflict.Actual)
	}

	// Unknown relation.
	_, err = fx.service.CreateVersion(ctx, "00000000-0000-0000-0000-000000000001", 1, fx.createParams(map[string]any{"n": 5}).Version)
	if !errors.Is(err, relations.ErrRelationNotFound) {
		t.Errorf("unknown relation err = %v, want ErrRelationNotFound", err)
	}

	// The head did not move through any of the failed attempts.
	head, err := fx.store.GetRelation(ctx, rel.ID)
	if err != nil {
		t.Fatalf("get relation: %v", err)
	}
	if head.CurrentVersionNo != 2 {
		t.Errorf("current_version_no = %d, want 2 (failed attempts must not advance it)", head.CurrentVersionNo)
	}
	log, err := fx.store.ListVersions(ctx, rel.ID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(log) != 2 {
		t.Errorf("version log length = %d, want 2", len(log))
	}
}

// TestRelationRepositoryConcurrentExpectedVersionConflict: N writers race
// the same expectation; exactly one wins, every loser gets the SAME stable
// conflict code, and the loser errors carry the winner's version as the
// fresh head. Repeated rounds prove it is not a fluke of one interleaving.
func TestRelationRepositoryConcurrentExpectedVersionConflict(t *testing.T) {
	ctx := testCtx(t)
	fx := newRelationFixture(t, ctx)

	rel, _, err := fx.service.CreateRelation(ctx, fx.createParams(map[string]any{"round": 0}))
	if err != nil {
		t.Fatalf("create relation: %v", err)
	}

	const writers = 8
	const rounds = 3
	for round := 0; round < rounds; round++ {
		expected := 1 + round // the head at the start of this round
		errs := make(chan error, writers)
		var wg sync.WaitGroup
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				_, err := fx.service.CreateVersion(ctx, rel.ID, expected,
					fx.createParams(map[string]any{"round": round + 1, "writer": w}).Version)
				errs <- err
			}(w)
		}
		wg.Wait()
		close(errs)

		wins, conflicts := 0, 0
		for err := range errs {
			switch {
			case err == nil:
				wins++
			default:
				var conflict *relations.VersionConflictError
				if !errors.As(err, &conflict) {
					t.Fatalf("round %d: loser err = %v, want *VersionConflictError (raw storage errors must never surface)", round, err)
				}
				conflicts++
				if conflict.RelationID != rel.ID || conflict.Expected != expected || conflict.Actual != expected+1 {
					t.Errorf("round %d: conflict = %+v, want relation %s expected %d actual %d",
						round, conflict, rel.ID, expected, expected+1)
				}
				if conflict.Code() != relations.CodeVersionConflict {
					t.Errorf("round %d: conflict code = %q, want the one stable code %q",
						round, conflict.Code(), relations.CodeVersionConflict)
				}
			}
		}
		if wins != 1 {
			t.Errorf("round %d: winners = %d, want exactly 1", round, wins)
		}
		if conflicts != writers-1 {
			t.Errorf("round %d: conflicts = %d, want %d", round, conflicts, writers-1)
		}
	}

	// The log advanced exactly once per round and every row is distinct.
	head, err := fx.store.GetRelation(ctx, rel.ID)
	if err != nil {
		t.Fatalf("get relation: %v", err)
	}
	if head.CurrentVersionNo != 1+rounds {
		t.Errorf("current_version_no = %d, want %d", head.CurrentVersionNo, 1+rounds)
	}
	log, err := fx.store.ListVersions(ctx, rel.ID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(log) != 1+rounds {
		t.Fatalf("version log length = %d, want %d", len(log), 1+rounds)
	}
	for i, v := range log {
		if v.VersionNo != i+1 {
			t.Errorf("log[%d].version_no = %d, want %d", i, v.VersionNo, i+1)
		}
	}
}

// TestRelationRepositoryOldVersionContentUnchanged: after a second version
// lands, version 1 reads back byte-identical (payload, endpoints, type,
// hash, timestamps), the two rows are distinct, and the database itself
// rejects any UPDATE/DELETE of a version row.
func TestRelationRepositoryOldVersionContentUnchanged(t *testing.T) {
	ctx := testCtx(t)
	fx := newRelationFixture(t, ctx)

	payloadV1 := map[string]any{"content": "first", "detail": []any{1, 2, 3}}
	rel, v1, err := fx.service.CreateRelation(ctx, fx.createParams(payloadV1))
	if err != nil {
		t.Fatalf("create relation: %v", err)
	}
	payloadV2 := map[string]any{"content": "second"}
	v2Params := fx.createParams(payloadV2).Version
	v2Params.RelationType = "supersedes"
	v2, err := fx.service.CreateVersion(ctx, rel.ID, 1, v2Params)
	if err != nil {
		t.Fatalf("create v2: %v", err)
	}

	// Version 1 is untouched.
	again, err := fx.store.GetVersion(ctx, rel.ID, 1)
	if err != nil {
		t.Fatalf("re-read v1: %v", err)
	}
	assertRelationVersionEqual(t, again, v1)
	wantRaw, _ := json.Marshal(payloadV1)
	var gotPayload, wantPayload any
	if err := json.Unmarshal(again.Payload, &gotPayload); err != nil {
		t.Fatalf("v1 payload is not JSON: %v", err)
	}
	if err := json.Unmarshal(wantRaw, &wantPayload); err != nil {
		t.Fatalf("want payload is not JSON: %v", err)
	}
	if fmt.Sprint(gotPayload) != fmt.Sprint(wantPayload) {
		t.Errorf("v1 payload = %s, want semantic %s (old version content must stay unchanged)", again.Payload, wantRaw)
	}
	if again.IntegrityHash != wantRelationHash(again.Payload) {
		t.Errorf("v1 integrity hash = %s, want %s", again.IntegrityHash, wantRelationHash(again.Payload))
	}
	if v2.VersionNo != 2 || string(v2.Payload) == string(v1.Payload) {
		t.Errorf("v2 = %+v, want a distinct second version", v2)
	}

	// The database rejects an UPDATE of the old row itself (the 00014
	// append-only guard), so no client — this repository included — can
	// rewrite history.
	_, err = fx.pool.Exec(ctx,
		`UPDATE relation_versions SET payload = '{"tampered":true}'::jsonb WHERE id = $1`, v1.ID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "P0001" {
		t.Fatalf("direct UPDATE of a version row err = %v, want P0001 append-only rejection", err)
	}
	if !strings.Contains(pgErr.Message, "append-only") {
		t.Errorf("UPDATE rejection message = %q, want it to name the append-only guard", pgErr.Message)
	}
	// DELETE is rejected the same way.
	_, err = fx.pool.Exec(ctx,
		`DELETE FROM relation_versions WHERE id = $1`, v1.ID)
	if !errors.As(err, &pgErr) || pgErr.Code != "P0001" {
		t.Fatalf("direct DELETE of a version row err = %v, want P0001 append-only rejection", err)
	}

	// And the rejected tampering changed nothing.
	last, err := fx.store.GetVersion(ctx, rel.ID, 1)
	if err != nil {
		t.Fatalf("re-read v1 after rejection: %v", err)
	}
	assertRelationVersionEqual(t, last, v1)
}

// TestRelationRepositoryListByTypeAndCategory (acceptance:
// dependency/provenance type 可查询): relations of the dependency type
// (depends_on) and of provenance types are queryable by concrete type and
// by catalog family, scoped to the project — relations of another project
// never leak into the result.
func TestRelationRepositoryListByTypeAndCategory(t *testing.T) {
	ctx := testCtx(t)
	fx := newRelationFixture(t, ctx)

	newRelation := func(in relations.CreateRelationParams) domain.Relation {
		t.Helper()
		rel, _, err := fx.service.CreateRelation(ctx, in)
		if err != nil {
			t.Fatalf("create relation %s: %v", in.Version.RelationType, err)
		}
		return rel
	}
	// Project A: two depends_on edges, one derived_from (provenance), one
	// supports (knowledge — must NOT appear in dependency/provenance
	// queries).
	dep1 := newRelation(fx.createParams(map[string]any{"kind": "dep1"}))
	dep2 := newRelation(fx.createParams(map[string]any{"kind": "dep2"}))
	prov := fx.createParams(map[string]any{"kind": "prov"})
	prov.Version.RelationType = "derived_from"
	newRelation(prov)
	know := fx.createParams(map[string]any{"kind": "know"})
	know.Version.RelationType = "supports"
	newRelation(know)
	// Project B: a depends_on edge on the other project's own versions.
	other := relations.CreateRelationParams{
		ProjectID: fx.otherProject.ID,
		Version: relations.VersionParams{
			StateID:               fx.otherStateID,
			RelationType:          "depends_on",
			SourceObjectVersionID: fx.otherV1,
			TargetObjectVersionID: fx.otherV2,
			Payload:               json.RawMessage(`{"kind":"other"}`),
			CreatedBy:             fx.alice.ID,
		},
	}
	newRelation(other)

	byType, err := fx.service.ListVersionsByType(ctx, fx.project.ID, "depends_on")
	if err != nil {
		t.Fatalf("ListVersionsByType(depends_on): %v", err)
	}
	if len(byType) != 2 {
		t.Fatalf("ListVersionsByType(depends_on) = %d rows, want 2 (the other project's edge must not leak)", len(byType))
	}
	for _, v := range byType {
		if v.RelationType != "depends_on" {
			t.Errorf("row %s type = %q, want depends_on", v.ID, v.RelationType)
		}
		if v.RelationID != dep1.ID && v.RelationID != dep2.ID {
			t.Errorf("row %s relation = %s, want one of the project's own dependency edges", v.ID, v.RelationID)
		}
		if v.SourceObjectVersionID != fx.sourceV1 || v.TargetObjectVersionID != fx.targetV1 {
			t.Errorf("row %s endpoints = (%s -> %s), want the pinned fixture versions",
				v.ID, v.SourceObjectVersionID, v.TargetObjectVersionID)
		}
	}

	// The dependency family (catalog.DependencyTypes()) is queryable as a
	// set and returns exactly the dependency edges.
	dep, err := fx.service.ListVersionsByTypes(ctx, fx.project.ID, relationcatalog.DependencyTypes())
	if err != nil {
		t.Fatalf("ListVersionsByTypes(DependencyTypes): %v", err)
	}
	if len(dep) != 2 {
		t.Fatalf("dependency family = %d rows, want 2", len(dep))
	}
	for _, v := range dep {
		if v.RelationType != "depends_on" {
			t.Errorf("dependency row %s type = %q, want depends_on", v.ID, v.RelationType)
		}
	}

	// The provenance family covers depends_on and derived_from — and not
	// the knowledge edge.
	provRows, err := fx.service.ListVersionsByTypes(ctx, fx.project.ID, relationcatalog.ProvenanceTypes())
	if err != nil {
		t.Fatalf("ListVersionsByTypes(ProvenanceTypes): %v", err)
	}
	if len(provRows) != 3 {
		t.Fatalf("provenance family = %d rows, want 3 (2 depends_on + 1 derived_from)", len(provRows))
	}
	seen := map[string]int{}
	for _, v := range provRows {
		seen[v.RelationType]++
		if v.RelationType == "supports" {
			t.Error("knowledge edge appeared in the provenance family query")
		}
	}
	if seen["depends_on"] != 2 || seen["derived_from"] != 1 {
		t.Errorf("provenance family composition = %v, want depends_on x2 + derived_from x1", seen)
	}

	// Querying an unknown type returns empty — no error, no rows: no write
	// through the port can store such a type.
	none, err := fx.service.ListVersionsByType(ctx, fx.project.ID, "friend_of")
	if err != nil {
		t.Fatalf("ListVersionsByType(friend_of): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("unknown type returned %d rows, want 0", len(none))
	}
}
