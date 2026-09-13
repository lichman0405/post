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

	"github.com/lichman0405/post/internal/application/sciobjects"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

// Task T0202: the scientific object repository (persistence.ScientificObjectStore
// implementing sciobjects.Repository) over a REAL PostgreSQL. Proves the
// three requirements and both acceptance criteria:
//
//   - create object + v1 in one atomic transaction;
//   - create the next version guarded by expected_version — a compare-and-swap
//     on scientific_objects.current_version_no (migration 00024);
//   - version payloads are immutable: no repository update path (the port
//     surface has none), the database rejects UPDATE/DELETE of version rows
//     itself (migrations 00014/00015), and old content reads back unchanged;
//   - concurrent expected-version conflicts all return the SAME stable code
//     EXPECTED_VERSION_MISMATCH (docs/45), whatever the interleaving.
//
// NB the append-only triggers are exercised on THIS task's own rows (not
// only the catalog-level table scan of the T0013 test), so the acceptance
// criterion is tied to the task's data, and the conflict paths are
// exercised adversarially: contention, stale expectations, expectations
// above the head, and unknown objects.
const sciObjectTaskID = "T0202"

// sciObjectFixture seeds the store with alice, an organization, a project
// and one project state (state_id is NOT NULL on version rows). The raw
// pool is kept for direct-SQL probes against the same database the store
// writes.
type sciObjectFixture struct {
	store   *persistence.ScientificObjectStore
	pool    *pgxpool.Pool
	alice   domain.User
	project domain.Project
	stateID string
}

func newSciObjectFixture(t *testing.T, ctx context.Context) *sciObjectFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), sciObjectTaskID)
	alice, err := persistence.NewCredentialStore(pool).CreateWithPassword(
		ctx, "sciobj-alice@example.com", "hash", "sciobj-alice", "Alice")
	if err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	orgStore := persistence.NewOrgStore(pool)
	org, _, err := orgStore.CreateOrganization(ctx, domain.Organization{
		Slug: "sciobj-fixture", Name: "SciObj Fixture",
	}, alice.ID, todayUTC())
	if err != nil {
		t.Fatalf("create fixture org: %v", err)
	}
	orgID := org.ID
	project, _, err := persistence.NewProjectStore(pool).CreateProject(ctx, domain.Project{
		OrganizationID:  &orgID,
		Slug:            "sciobj-project",
		Name:            "SciObj Project",
		Purpose:         "fixture purpose",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, alice.ID)
	if err != nil {
		t.Fatalf("create fixture project: %v", err)
	}
	var stateID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO project_states (project_id, state_hash, manifest_version)
		VALUES ($1, $2, $3) RETURNING id`,
		project.ID, "sciobj-state-hash-1", "v1").Scan(&stateID); err != nil {
		t.Fatalf("seed project state: %v", err)
	}
	return &sciObjectFixture{
		store:   persistence.NewScientificObjectStore(pool),
		pool:    pool,
		alice:   alice,
		project: project,
		stateID: stateID,
	}
}

// createParams builds valid creation input against the fixture. Payload is
// the type-specific scientific content; the server-computed hash is
// verified against it in the tests.
func (fx *sciObjectFixture) createParams(payload any) sciobjects.CreateObjectParams {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(fmt.Sprintf("sciObjectFixture: marshal payload: %v", err))
	}
	return sciobjects.CreateObjectParams{
		ProjectID:  fx.project.ID,
		ObjectType: "experiment",
		CreatedBy:  fx.alice.ID,
		Version: sciobjects.VersionParams{
			StateID:        fx.stateID,
			SchemaID:       "https://open-rd.example/schemas/experiment.schema.json",
			SchemaVersion:  "1",
			Title:          "Fixture experiment",
			LifecycleState: domain.LifecycleActive,
			Payload:        raw,
			CreatedBy:      fx.alice.ID,
		},
	}
}

// wantHash is the sha256 hex digest the store must have computed for
// payload.
func wantHash(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// assertVersionEqual compares two version values field by field (the domain
// type carries json.RawMessage, which is not comparable as a struct).
func assertVersionEqual(t *testing.T, got, want domain.ScientificObjectVersion) {
	t.Helper()
	if got.ID != want.ID || got.ObjectID != want.ObjectID || got.VersionNo != want.VersionNo ||
		got.StateID != want.StateID || got.SchemaID != want.SchemaID ||
		got.SchemaVersion != want.SchemaVersion || got.Title != want.Title ||
		got.LifecycleState != want.LifecycleState || got.IntegrityHash != want.IntegrityHash ||
		got.CreatedBy != want.CreatedBy || !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("version mismatch:\ngot  %+v\nwant %+v", got, want)
	}
	if !bytes.Equal(got.Payload, want.Payload) {
		t.Errorf("payload mismatch:\ngot  %s\nwant %s", got.Payload, want.Payload)
	}
	if (got.BranchID == nil) != (want.BranchID == nil) ||
		(got.BranchID != nil && *got.BranchID != *want.BranchID) {
		t.Errorf("branch mismatch: got %v want %v", got.BranchID, want.BranchID)
	}
	if (got.VisibilityPolicyID == nil) != (want.VisibilityPolicyID == nil) ||
		(got.VisibilityPolicyID != nil && *got.VisibilityPolicyID != *want.VisibilityPolicyID) {
		t.Errorf("visibility policy mismatch: got %v want %v", got.VisibilityPolicyID, want.VisibilityPolicyID)
	}
}

// TestScientificObjectRepositoryCreateObjectAndV1: the creation transaction
// produces the object row and its version 1 together, the head pointer
// starts at 1, the payload round-trips byte-identically and the integrity
// hash is the server-computed sha256 of the exact stored payload bytes.
func TestScientificObjectRepositoryCreateObjectAndV1(t *testing.T) {
	ctx := testCtx(t)
	fx := newSciObjectFixture(t, ctx)

	payload := map[string]any{"objective": "measure X", "samples": []any{"s1"}}
	obj, v1, err := fx.store.CreateObject(ctx, fx.createParams(payload))
	if err != nil {
		t.Fatalf("create object: %v", err)
	}
	if obj.ID == "" || obj.ProjectID != fx.project.ID || obj.ObjectType != "experiment" {
		t.Errorf("object = %+v, want fixture identity", obj)
	}
	if obj.CurrentVersionNo != 1 {
		t.Errorf("object current_version_no = %d, want 1", obj.CurrentVersionNo)
	}
	if v1.ObjectID != obj.ID || v1.VersionNo != 1 {
		t.Errorf("v1 = %+v, want object %s version 1", v1, obj.ID)
	}
	// The payload is stored in PostgreSQL's canonical jsonb form (key
	// order and whitespace are normalized), so the assertion is semantic
	// equality with the input — and the hash must pin the STORED bytes:
	// sha256 of the read payload always equals the stored hash.
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
	if v1.IntegrityHash != wantHash(v1.Payload) {
		t.Errorf("v1 integrity hash = %s, want sha256(stored payload) = %s", v1.IntegrityHash, wantHash(v1.Payload))
	}

	// Every read path returns the same row.
	gotObj, err := fx.store.GetObject(ctx, obj.ID)
	if err != nil {
		t.Fatalf("get object: %v", err)
	}
	if gotObj != obj {
		t.Errorf("GetObject = %+v, want %+v", gotObj, obj)
	}
	gotV1, err := fx.store.GetVersion(ctx, obj.ID, 1)
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	assertVersionEqual(t, gotV1, v1)
	latest, err := fx.store.GetLatestVersion(ctx, obj.ID)
	if err != nil {
		t.Fatalf("get latest: %v", err)
	}
	assertVersionEqual(t, latest, v1)
	log, err := fx.store.ListVersions(ctx, obj.ID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(log) != 1 {
		t.Fatalf("ListVersions = %+v, want exactly one version", log)
	}
	assertVersionEqual(t, log[0], v1)

	// A value that cannot name an object is ErrObjectNotFound, never a 500.
	if _, err := fx.store.GetObject(ctx, "not-a-uuid"); !errors.Is(err, sciobjects.ErrObjectNotFound) {
		t.Errorf("GetObject(not-a-uuid) err = %v, want ErrObjectNotFound", err)
	}
}

// TestScientificObjectRepositoryCreateVersionExpectedSemantics: sequential
// expected_version behaviour — a correct expectation appends the next
// number, a stale one and an over-optimistic one both fail with the typed
// conflict, an unknown object is ErrObjectNotFound, and a version below 1
// is a validation failure at the service.
func TestScientificObjectRepositoryCreateVersionExpectedSemantics(t *testing.T) {
	ctx := testCtx(t)
	fx := newSciObjectFixture(t, ctx)
	svc := sciobjects.NewService(fx.store)

	obj, _, err := fx.store.CreateObject(ctx, fx.createParams(map[string]any{"n": 1}))
	if err != nil {
		t.Fatalf("create object: %v", err)
	}

	// expected = current → next version.
	v2, err := fx.store.CreateVersion(ctx, obj.ID, 1, fx.createParams(map[string]any{"n": 2}).Version)
	if err != nil {
		t.Fatalf("create version with expected=1: %v", err)
	}
	if v2.VersionNo != 2 {
		t.Errorf("v2 version_no = %d, want 2", v2.VersionNo)
	}

	// Stale expectation (the head already moved past 1).
	var conflict *sciobjects.VersionConflictError
	_, err = fx.store.CreateVersion(ctx, obj.ID, 1, fx.createParams(map[string]any{"n": 3}).Version)
	if !errors.As(err, &conflict) {
		t.Fatalf("stale expected=1 err = %v, want *VersionConflictError", err)
	}
	if conflict.Expected != 1 || conflict.Actual != 2 || conflict.ObjectID != obj.ID {
		t.Errorf("stale conflict = %+v, want expected 1 actual 2", conflict)
	}
	if conflict.Code() != sciobjects.CodeVersionConflict || sciobjects.CodeVersionConflict != "EXPECTED_VERSION_MISMATCH" {
		t.Errorf("conflict code = %q, want %q (docs/45)", conflict.Code(), "EXPECTED_VERSION_MISMATCH")
	}

	// Over-optimistic expectation.
	_, err = fx.store.CreateVersion(ctx, obj.ID, 7, fx.createParams(map[string]any{"n": 4}).Version)
	if !errors.As(err, &conflict) {
		t.Fatalf("expected=7 err = %v, want *VersionConflictError", err)
	}
	if conflict.Actual != 2 {
		t.Errorf("expected=7 conflict actual = %d, want 2", conflict.Actual)
	}

	// Unknown object.
	_, err = fx.store.CreateVersion(ctx, "00000000-0000-0000-0000-000000000001", 1, fx.createParams(map[string]any{"n": 5}).Version)
	if !errors.Is(err, sciobjects.ErrObjectNotFound) {
		t.Errorf("unknown object err = %v, want ErrObjectNotFound", err)
	}

	// The head did not move through any of the failed attempts.
	head, err := fx.store.GetObject(ctx, obj.ID)
	if err != nil {
		t.Fatalf("get object: %v", err)
	}
	if head.CurrentVersionNo != 2 {
		t.Errorf("current_version_no = %d, want 2 (failed attempts must not advance it)", head.CurrentVersionNo)
	}
	log, err := fx.store.ListVersions(ctx, obj.ID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(log) != 2 {
		t.Errorf("version log length = %d, want 2", len(log))
	}

	// Service-level validation: expected below 1 is rejected before the
	// store is touched.
	_, err = svc.CreateVersion(ctx, obj.ID, 0, fx.createParams(map[string]any{"n": 6}).Version)
	if !errors.Is(err, sciobjects.ErrValidation) {
		t.Errorf("expected=0 via service err = %v, want ErrValidation", err)
	}
}

// TestScientificObjectRepositoryConcurrentExpectedVersionConflict: N
// writers race the same expectation; exactly one wins, every loser gets
// the SAME stable conflict code, and the loser errors carry the winner's
// version as the fresh head. Repeated rounds prove it is not a fluke of
// one interleaving.
func TestScientificObjectRepositoryConcurrentExpectedVersionConflict(t *testing.T) {
	ctx := testCtx(t)
	fx := newSciObjectFixture(t, ctx)

	obj, _, err := fx.store.CreateObject(ctx, fx.createParams(map[string]any{"round": 0}))
	if err != nil {
		t.Fatalf("create object: %v", err)
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
				_, err := fx.store.CreateVersion(ctx, obj.ID, expected,
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
				var conflict *sciobjects.VersionConflictError
				if !errors.As(err, &conflict) {
					t.Fatalf("round %d: loser err = %v, want *VersionConflictError (raw storage errors must never surface)", round, err)
				}
				conflicts++
				if conflict.ObjectID != obj.ID || conflict.Expected != expected || conflict.Actual != expected+1 {
					t.Errorf("round %d: conflict = %+v, want object %s expected %d actual %d",
						round, conflict, obj.ID, expected, expected+1)
				}
				if conflict.Code() != sciobjects.CodeVersionConflict {
					t.Errorf("round %d: conflict code = %q, want the one stable code %q",
						round, conflict.Code(), sciobjects.CodeVersionConflict)
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
	head, err := fx.store.GetObject(ctx, obj.ID)
	if err != nil {
		t.Fatalf("get object: %v", err)
	}
	if head.CurrentVersionNo != 1+rounds {
		t.Errorf("current_version_no = %d, want %d", head.CurrentVersionNo, 1+rounds)
	}
	log, err := fx.store.ListVersions(ctx, obj.ID)
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

// TestScientificObjectRepositoryOldVersionContentUnchanged: after a second
// version lands, version 1 reads back byte-identical (payload, title,
// lifecycle, hash, timestamps), the two rows are distinct, and the
// database itself rejects any UPDATE/DELETE of a version row.
func TestScientificObjectRepositoryOldVersionContentUnchanged(t *testing.T) {
	ctx := testCtx(t)
	fx := newSciObjectFixture(t, ctx)

	payloadV1 := map[string]any{"content": "first", "detail": []any{1, 2, 3}}
	obj, v1, err := fx.store.CreateObject(ctx, fx.createParams(payloadV1))
	if err != nil {
		t.Fatalf("create object: %v", err)
	}
	payloadV2 := map[string]any{"content": "second"}
	v2Params := fx.createParams(payloadV2).Version
	v2Params.Title = "Fixture experiment, take two"
	v2Params.LifecycleState = domain.LifecycleSuperseded
	v2, err := fx.store.CreateVersion(ctx, obj.ID, 1, v2Params)
	if err != nil {
		t.Fatalf("create v2: %v", err)
	}

	// Version 1 is untouched.
	again, err := fx.store.GetVersion(ctx, obj.ID, 1)
	if err != nil {
		t.Fatalf("re-read v1: %v", err)
	}
	assertVersionEqual(t, again, v1)
	// Old version content is unchanged: the stored (jsonb-canonical) form
	// is what version 1 holds, and it stays byte-identical across reads —
	// nothing in the v2 write can have touched it.
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
	if again.IntegrityHash != wantHash(again.Payload) {
		t.Errorf("v1 integrity hash = %s, want %s", again.IntegrityHash, wantHash(again.Payload))
	}
	if v2.VersionNo != 2 || string(v2.Payload) == string(v1.Payload) {
		t.Errorf("v2 = %+v, want a distinct second version", v2)
	}

	// The database rejects an UPDATE of the old row itself (the 00014
	// append-only guard), so no client — this repository included — can
	// rewrite history.
	_, err = fx.pool.Exec(ctx,
		`UPDATE scientific_object_versions SET title = 'tampered' WHERE id = $1`, v1.ID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "P0001" {
		t.Fatalf("direct UPDATE of a version row err = %v, want P0001 append-only rejection", err)
	}
	if !strings.Contains(pgErr.Message, "append-only") {
		t.Errorf("UPDATE rejection message = %q, want it to name the append-only guard", pgErr.Message)
	}
	// DELETE is rejected the same way.
	_, err = fx.pool.Exec(ctx,
		`DELETE FROM scientific_object_versions WHERE id = $1`, v1.ID)
	if !errors.As(err, &pgErr) || pgErr.Code != "P0001" {
		t.Fatalf("direct DELETE of a version row err = %v, want P0001 append-only rejection", err)
	}

	// And the rejected tampering changed nothing.
	last, err := fx.store.GetVersion(ctx, obj.ID, 1)
	if err != nil {
		t.Fatalf("re-read v1 after rejection: %v", err)
	}
	assertVersionEqual(t, last, v1)
}
