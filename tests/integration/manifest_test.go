package integration

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/manifests"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/sqlc"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rsg/manifest"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// Task T0206: RSG Manifest 导出与 hash — over a REAL PostgreSQL, with the
// same store composition the API uses. Proves both acceptance criteria and
// the lineage rule end to end:
//
//   - 同一 state 多次导出 hash 相同: two exports of the same state through
//     the real service agree on state_hash and on every hash-input byte;
//     an attachment committed in a LATER state never moves an earlier
//     state's hash (the attachment belongs to the state it was created in,
//     never to the owning version — re-exporting the earlier state yields
//     the identical manifest);
//   - 任何 semantic change 改变 hash: a new object version, a new object, a
//     new relation, a blob attachment and a recorded git ref each move the
//     hash, and the manifest reflects exactly the change;
//   - purity: the manifest never reads mutable project data — updating the
//     project's repository id after the state was written changes nothing,
//     and the git ref is the state's own recorded commit sha
//     (project_states.git_commit_sha), null when the state has none;
//   - lineage: a version committed on a forked branch is not part of the
//     ancestor branch head's manifest, and vice versa;
//   - the exported document validates against the canonical rsg-manifest
//     schema.

const manifestTaskID = "T0206"

// manifestFixture seeds one private project owned by alice and wires the
// rsg service (the write path) plus the manifests service (the export
// path) over the real stores — the composition cmd/api/main.go uses, with
// ManifestStore as the snapshot adapter.
type manifestFixture struct {
	export    *manifests.Service
	svc       *rsg.Service
	statesSvc *states.Service
	pool      *pgxpool.Pool
	alice     domain.User
	project   domain.Project
	branch    string
	genesis   string
}

func newManifestFixture(t *testing.T, ctx context.Context) *manifestFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), manifestTaskID)
	alice, err := persistence.NewCredentialStore(pool).CreateWithPassword(
		ctx, "manifest-alice@example.com", "hash", "manifest-alice", "Alice")
	if err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	orgStore := persistence.NewOrgStore(pool)
	org, _, err := orgStore.CreateOrganization(ctx, domain.Organization{
		Slug: "manifest-fixture", Name: "Manifest Fixture",
	}, alice.ID, todayUTC())
	if err != nil {
		t.Fatalf("create fixture org: %v", err)
	}
	projectStore := persistence.NewProjectStore(pool)
	project, _, err := projectStore.CreateProject(ctx, domain.Project{
		OrganizationID:  &org.ID,
		Slug:            "manifest-project",
		Name:            "Manifest Project",
		Purpose:         "fixture purpose",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, alice.ID)
	if err != nil {
		t.Fatalf("create fixture project: %v", err)
	}

	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	stateStore := persistence.NewStateStore(pool)
	statesSvc := states.NewService(stateStore, newCommitGuard(t))
	svc := rsg.NewService(rsg.Deps{
		Projects:  projects.NewService(projectStore, orgStore, authz.NewMatrixEngine()),
		Branches:  branches.NewService(persistence.NewBranchStore(pool)),
		States:    statesSvc,
		Latest:    stateStore,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
		Events:    events.Recorder{},
	})
	branch, err := svc.CreateBranch(ctx, alice, project.ID, rsg.CreateBranchInput{
		Name:       "main",
		BaseRef:    "",
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}
	var genesis string
	if err := pool.QueryRow(ctx,
		`SELECT id FROM project_states WHERE project_id = $1 AND parent_state_id IS NULL`, project.ID).Scan(&genesis); err != nil {
		t.Fatalf("read genesis state: %v", err)
	}

	return &manifestFixture{
		export: manifests.NewService(
			stateStore,
			persistence.NewManifestStore(pool),
		),
		svc:       svc,
		statesSvc: statesSvc,
		pool:      pool,
		alice:     alice,
		project:   project,
		branch:    branch.ID,
		genesis:   genesis,
	}
}

// exportHead exports the manifest of stateID through the real service and
// requires it to verify its own hash.
func (f *manifestFixture) exportHead(t *testing.T, ctx context.Context, stateID string) *manifest.Manifest {
	t.Helper()
	m, err := f.export.Export(ctx, stateID)
	if err != nil {
		t.Fatalf("Export(%s): %v", stateID, err)
	}
	if !m.VerifyHash() {
		t.Fatalf("exported manifest of %s does not verify its hash", stateID)
	}
	return m
}

// createObject writes one object on the main branch through the rsg
// service and returns the head state and the version ids it produced.
func (f *manifestFixture) createObject(t *testing.T, ctx context.Context, objectType, payload string) (head string, versionID string) {
	t.Helper()
	res, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, f.branch, rsg.CreateObjectInput{
		ObjectType: objectType,
		Payload:    json.RawMessage(payload),
	})
	if err != nil {
		t.Fatalf("CreateObject: %v", err)
	}
	return res.Version.StateID, res.Version.ID
}

// attachBlobTo is a WriteFunc that records one blob row attached to
// versionID inside the commit transaction — the same append-only shape the
// blob attach domain service will produce. The attachment records the state
// it is created in (the stateID the commit is producing; 00035), so a later
// attachment never leaks into earlier states' manifests. fill/size
// distinguish the blob (blobs are unique per (content_hash, size_bytes)).
func (f *manifestFixture) attachBlobTo(versionID string, fill byte, size int64, record *string) states.WriteFunc {
	return func(ctx context.Context, tx states.Transaction, stateID string) error {
		q := sqlc.New(tx)
		media := "text/plain"
		blob, err := q.CreateBlob(ctx, sqlc.CreateBlobParams{
			ContentHash: "sha256:" + string40(fill),
			SizeBytes:   size,
			MediaType:   &media,
			StorageKey:  "test/t0206/blob.dat",
			CreatedBy:   parseUUIDOrDie(f.alice.ID),
		})
		if err != nil {
			return err
		}
		if err := q.AttachBlob(ctx, sqlc.AttachBlobParams{
			BlobID:                    blob.ID,
			ScientificObjectVersionID: parseUUIDOrDie(versionID),
			AttachmentRole:            "data",
			AccessLevel:               "open",
			StateID:                   parseUUIDOrDie(stateID),
		}); err != nil {
			return err
		}
		if record != nil {
			*record = pgUUIDTextTest(blob.ID)
		}
		return nil
	}
}

// setProjectRepoID pins the project's external repository id directly. The
// projects row is ordinary mutable state; the manifest must not read it at
// all (a state's hash is a pure function of the state's recorded content),
// so the test uses this mutation to prove a later project update moves
// nothing. The state-side git half (git_commit_sha) is append-only: it
// enters only through a state transition (CommitParams.GitCommitSHA),
// never an UPDATE.
func (f *manifestFixture) setProjectRepoID(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := f.pool.Exec(ctx,
		`UPDATE projects SET git_repository_external_id = 'repo-ext-t0206' WHERE id = $1`, f.project.ID); err != nil {
		t.Fatalf("set project git repository id: %v", err)
	}
}

// string40 renders a 40-hex-char content address for fixtures.
func string40(r byte) string {
	b := make([]byte, 40)
	for i := range b {
		b[i] = r
	}
	return string(b)
}

// TestManifestExportSameStateHashesSame proves the first acceptance
// criterion over the real stack: exporting the same state twice derives
// the same state hash and identical hash-input bytes, on a populated state
// and on the genesis root alike. generated_at is the only field allowed to
// move, and it is not hash input.
func TestManifestExportSameStateHashesSame(t *testing.T) {
	ctx := testCtx(t)
	f := newManifestFixture(t, ctx)

	head, _ := f.createObject(t, ctx, "experiment", `{"name":"E1","z":1,"a":[1,2]}`)
	states := []struct {
		name string
		id   string
	}{
		{"genesis", f.genesis},
		{"populated", head},
	}
	for _, st := range states {
		t.Run(st.name, func(t *testing.T) {
			m1 := f.exportHead(t, ctx, st.id)
			m2 := f.exportHead(t, ctx, st.id)
			if m1.StateHash != m2.StateHash {
				t.Fatalf("state hash moved between exports: %s != %s", m1.StateHash, m2.StateHash)
			}
			c1, err := m1.ContentCanonicalJSON()
			if err != nil {
				t.Fatalf("content 1: %v", err)
			}
			c2, err := m2.ContentCanonicalJSON()
			if err != nil {
				t.Fatalf("content 2: %v", err)
			}
			if string(c1) != string(c2) {
				t.Fatalf("hash-input bytes differ between exports:\n%s\n%s", c1, c2)
			}
		})
	}
}

// TestManifestExportSemanticChangesMoveHash proves the second acceptance
// criterion over the real stack: each semantic transition — a new object
// version, a new object, a new relation, a blob attachment, a recorded git
// ref — moves the state hash, and the manifest reflects exactly the
// change. Every exported manifest verifies its hash and validates against
// the canonical schema.
func TestManifestExportSemanticChangesMoveHash(t *testing.T) {
	ctx := testCtx(t)
	f := newManifestFixture(t, ctx)
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	validate := func(t *testing.T, m *manifest.Manifest) {
		t.Helper()
		doc, err := m.CanonicalJSON()
		if err != nil {
			t.Fatalf("CanonicalJSON: %v", err)
		}
		if err := reg.Validate(schemareg.Ref{ID: "https://open-rd.example/schemas/rsg-manifest.schema.json", Version: schemareg.CanonicalV1}, doc); err != nil {
			t.Fatalf("exported manifest does not satisfy the rsg-manifest schema: %v\n%s", err, doc)
		}
	}

	// S1: one object (version 1).
	s1, ov1 := f.createObject(t, ctx, "experiment", `{"name":"E1","z":1,"a":[1,2]}`)
	m1 := f.exportHead(t, ctx, s1)
	validate(t, m1)
	if len(m1.ObjectVersions) != 1 || m1.ObjectVersions[0].ID != ov1 {
		t.Fatalf("S1 manifest object versions = %+v, want exactly %s", m1.ObjectVersions, ov1)
	}
	if string(m1.ObjectVersions[0].Payload) != `{"a":[1,2],"name":"E1","z":1}` {
		t.Fatalf("payload not canonicalized over the real stack: %s", m1.ObjectVersions[0].Payload)
	}
	if len(m1.SchemaRefs) != 1 || m1.SchemaRefs[0] != "https://open-rd.example/schemas/experiment.schema.json@1" {
		t.Fatalf("schema refs = %v", m1.SchemaRefs)
	}
	if len(m1.RelationVersions) != 0 || len(m1.BlobRefs) != 0 || m1.GitRef != nil {
		t.Fatalf("S1 manifest has phantom members: rel=%d blob=%d git=%+v", len(m1.RelationVersions), len(m1.BlobRefs), m1.GitRef)
	}

	// S2: a second version of the same object — the lineage holds both.
	ver2, err := f.svc.CreateObjectVersion(ctx, f.alice, f.project.ID, f.branch,
		m1.ObjectVersions[0].ObjectID, rsg.CreateObjectVersionInput{
			ExpectedVersion: 1,
			Patch:           json.RawMessage(`{"temperature_k":300.0}`),
		})
	if err != nil {
		t.Fatalf("CreateObjectVersion: %v", err)
	}
	s2 := ver2.Version.StateID
	m2 := f.exportHead(t, ctx, s2)
	validate(t, m2)
	if m2.StateHash == m1.StateHash {
		t.Fatalf("hash did not move for a new object version: %s", m2.StateHash)
	}
	if len(m2.ObjectVersions) != 2 {
		t.Fatalf("S2 manifest object versions = %d, want both versions", len(m2.ObjectVersions))
	}

	// S3: a second object.
	s3, ov3 := f.createObject(t, ctx, "hypothesis", `{"statement":"h1"}`)
	m3 := f.exportHead(t, ctx, s3)
	validate(t, m3)
	if m3.StateHash == m2.StateHash || len(m3.ObjectVersions) != 3 {
		t.Fatalf("new object: hash=%s versions=%d, want a moved hash and 3 versions", m3.StateHash, len(m3.ObjectVersions))
	}

	// S4: a relation between the new object and the experiment's v2.
	rel, err := f.svc.CreateRelation(ctx, f.alice, f.project.ID, f.branch, rsg.CreateRelationInput{
		RelationType:          "derived_from",
		SourceObjectVersionID: ov3,
		TargetObjectVersionID: ver2.Version.ID,
		Payload:               json.RawMessage(`{"scope":"preliminary"}`),
	})
	if err != nil {
		t.Fatalf("CreateRelation: %v", err)
	}
	s4 := rel.Version.StateID
	m4 := f.exportHead(t, ctx, s4)
	validate(t, m4)
	if m4.StateHash == m3.StateHash {
		t.Fatalf("hash did not move for a new relation: %s", m4.StateHash)
	}
	if len(m4.RelationVersions) != 1 || m4.RelationVersions[0].SourceObjectVersionID != ov3 {
		t.Fatalf("S4 manifest relation versions = %+v", m4.RelationVersions)
	}

	// S5: a blob attached to the experiment's FIRST version — the manifest
	// gains a blob ref while the version set stays identical. The attachment
	// records S5 as the state it was created in (00035).
	var blobID string
	state5, _, err := f.statesSvc.Commit(ctx, states.CommitParams{
		ProjectID: f.project.ID,
		BranchID:  f.branch,
		ActorID:   f.alice.ID,
		Via:       domain.ViaWeb,
		Message:   "attach the raw data",
		Operations: []domain.StateOperation{
			{Kind: domain.OperationBlobAttached, EntityID: "blob-attached-in-callback"},
		},
		BaseStateID:     &s4,
		ManifestVersion: "v1",
		Gate:            rsgvalidation.GateDraft,
	}, f.attachBlobTo(ov1, 'b', 3, &blobID))
	if err != nil {
		t.Fatalf("blob attach Commit: %v", err)
	}
	s5 := state5.ID
	m5 := f.exportHead(t, ctx, s5)
	validate(t, m5)
	if m5.StateHash == m4.StateHash {
		t.Fatalf("hash did not move for a blob attachment: %s", m5.StateHash)
	}
	if len(m5.BlobRefs) != 1 || m5.BlobRefs[0].ID != blobID {
		t.Fatalf("S5 manifest blob refs = %+v, want exactly %s", m5.BlobRefs, blobID)
	}
	if len(m5.ObjectVersions) != len(m4.ObjectVersions) {
		t.Fatalf("blob attachment changed the version set: %d != %d", len(m5.ObjectVersions), len(m4.ObjectVersions))
	}
	if m5.GitRef != nil {
		t.Fatalf("S5 manifest git ref = %+v, want null (no commit sha recorded)", m5.GitRef)
	}

	// The attachment is S5's own member, not the owning version's: the
	// blob is attached to S1's version, but exporting S4 (or S1) must yield
	// exactly the manifest it yielded before — no blob ref, and the hash
	// unchanged. A later attach must never move an earlier state's hash.
	m4b := f.exportHead(t, ctx, s4)
	validate(t, m4b)
	if m4b.StateHash != m4.StateHash {
		t.Fatalf("re-exporting S4 after the S5 attach moved its hash: %s != %s", m4b.StateHash, m4.StateHash)
	}
	if len(m4b.BlobRefs) != 0 {
		t.Fatalf("S4 manifest gained the later state's blob ref: %+v", m4b.BlobRefs)
	}
	m1b := f.exportHead(t, ctx, s1)
	validate(t, m1b)
	if m1b.StateHash != m1.StateHash {
		t.Fatalf("re-exporting S1 after the S5 attach moved its hash: %s != %s", m1b.StateHash, m1.StateHash)
	}
	if len(m1b.BlobRefs) != 0 {
		t.Fatalf("S1 manifest gained the later state's blob ref: %+v", m1b.BlobRefs)
	}

	// Mutable project data contributes nothing: updating the project's
	// repository id after the state was written moves no hash and renders no
	// git ref (the manifest never reads the project row — the hash is a pure
	// function of the state's recorded content).
	f.setProjectRepoID(t, ctx)
	m5b := f.exportHead(t, ctx, s5)
	validate(t, m5b)
	if m5b.StateHash != m5.StateHash {
		t.Fatalf("project repository id moved the hash: %s != %s", m5b.StateHash, m5.StateHash)
	}
	if m5b.GitRef != nil {
		t.Fatalf("project repository id rendered a git ref: %+v", m5b.GitRef)
	}

	// S6: the GitProvider-side transition records its commit sha on the
	// state (CommitParams.GitCommitSHA — the path the git compat writer
	// uses; project_states is append-only, so the sha can never be edited
	// in place). The state's own recorded sha alone renders the git_ref
	// (never the project's repository id), and the hash moves.
	sha := string40('c')
	state6, _, err := f.statesSvc.Commit(ctx, states.CommitParams{
		ProjectID: f.project.ID,
		BranchID:  f.branch,
		ActorID:   f.alice.ID,
		Via:       domain.ViaGitCompat,
		Message:   "record the git compat commit",
		Operations: []domain.StateOperation{
			{Kind: domain.OperationBlobAttached, EntityID: "git-compat-data-attached"},
		},
		BaseStateID:     &s5,
		GitCommitSHA:    &sha,
		ManifestVersion: "v1",
		Gate:            rsgvalidation.GateDraft,
	}, f.attachBlobTo(ov3, 'd', 4, nil))
	if err != nil {
		t.Fatalf("git compat Commit: %v", err)
	}
	s6 := state6.ID
	m6 := f.exportHead(t, ctx, s6)
	validate(t, m6)
	if m6.StateHash == m5.StateHash {
		t.Fatalf("hash did not move for the git ref: %s", m6.StateHash)
	}
	if m6.GitRef == nil || *m6.GitRef != sha {
		t.Fatalf("git ref not rendered from the state's recorded commit sha: %+v", m6.GitRef)
	}
	// S6 also attached the second blob — its isolated effect was proven in
	// the S4→S5 step; here the version set stays untouched.
	if len(m6.BlobRefs) != 2 {
		t.Fatalf("S6 manifest blob refs = %+v, want both attachments", m6.BlobRefs)
	}
	if len(m6.ObjectVersions) != len(m5.ObjectVersions) {
		t.Fatalf("git compat commit changed the version set: %d != %d", len(m6.ObjectVersions), len(m5.ObjectVersions))
	}
	// The S6 attachment belongs to S6 alone: re-exporting S5 yields its
	// own unchanged manifest (still exactly S5's blob), and S4 stays
	// empty and unchanged.
	m5c := f.exportHead(t, ctx, s5)
	validate(t, m5c)
	if m5c.StateHash != m5.StateHash {
		t.Fatalf("re-exporting S5 after the S6 attach moved its hash: %s != %s", m5c.StateHash, m5.StateHash)
	}
	if len(m5c.BlobRefs) != 1 || m5c.BlobRefs[0].ID != blobID {
		t.Fatalf("S5 manifest changed after the S6 attach: %+v", m5c.BlobRefs)
	}
	m4c := f.exportHead(t, ctx, s4)
	validate(t, m4c)
	if m4c.StateHash != m4.StateHash || len(m4c.BlobRefs) != 0 {
		t.Fatalf("S4 manifest moved after the S6 attach: hash=%s refs=%+v", m4c.StateHash, m4c.BlobRefs)
	}
}

// TestManifestExportLineageIsPerBranch proves the lineage rule over the
// real stack: the snapshot is the state chain, not the project. A version
// committed on a forked branch is not part of the ancestor branch head's
// manifest, and vice versa.
func TestManifestExportLineageIsPerBranch(t *testing.T) {
	ctx := testCtx(t)
	f := newManifestFixture(t, ctx)

	// S1 on main: object A.
	s1, a1 := f.createObject(t, ctx, "experiment", `{"name":"A"}`)
	// Fork feature at S1 and commit object B there.
	feature, err := f.svc.CreateBranch(ctx, f.alice, f.project.ID, rsg.CreateBranchInput{
		Name:       "feature",
		BaseRef:    s1,
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("fork feature branch: %v", err)
	}
	res, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, feature.ID, rsg.CreateObjectInput{
		ObjectType: "hypothesis",
		Payload:    json.RawMessage(`{"statement":"B"}`),
	})
	if err != nil {
		t.Fatalf("CreateObject on feature: %v", err)
	}
	s2 := res.Version.StateID

	// The main head (S1) does not see the fork's object…
	m1 := f.exportHead(t, ctx, s1)
	if len(m1.ObjectVersions) != 1 || m1.ObjectVersions[0].ID != a1 {
		t.Fatalf("main head manifest = %+v, want exactly object A", m1.ObjectVersions)
	}
	// …and the fork head sees its own object plus the ancestor's.
	m2 := f.exportHead(t, ctx, s2)
	if len(m2.ObjectVersions) != 2 {
		t.Fatalf("fork head manifest holds %d versions, want A and B", len(m2.ObjectVersions))
	}
	var hasA, hasB bool
	for _, ov := range m2.ObjectVersions {
		hasA = hasA || ov.ID == a1
		hasB = hasB || ov.ID == res.Version.ID
	}
	if !hasA || !hasB {
		t.Fatalf("fork head manifest must hold the ancestor version and its own: hasA=%v hasB=%v", hasA, hasB)
	}

	// Committing object C on main must not leak the fork's object B either.
	s3, c3 := f.createObject(t, ctx, "experiment", `{"name":"C"}`)
	m3 := f.exportHead(t, ctx, s3)
	if len(m3.ObjectVersions) != 2 {
		t.Fatalf("main manifest after fork = %d versions, want A and C", len(m3.ObjectVersions))
	}
	for _, ov := range m3.ObjectVersions {
		if ov.ID == res.Version.ID {
			t.Fatalf("main manifest leaked the fork's object %s", ov.ID)
		}
		if ov.ID != a1 && ov.ID != c3 {
			t.Fatalf("main manifest holds a foreign version %s", ov.ID)
		}
	}
}

// TestManifestExportErrors proves the error outcomes over the real stack:
// an unknown state reports ErrStateNotFound (the port contract mapped by
// the service), an empty id is a validation outcome before any read.
func TestManifestExportErrors(t *testing.T) {
	ctx := testCtx(t)
	f := newManifestFixture(t, ctx)

	if _, err := f.export.Export(ctx, "99999999-9999-9999-9999-999999999999"); !errors.Is(err, manifests.ErrStateNotFound) {
		t.Fatalf("unknown state: want ErrStateNotFound, got %v", err)
	}
	if _, err := f.export.Export(ctx, ""); !errors.Is(err, manifests.ErrValidation) {
		t.Fatalf("empty state id: want ErrValidation, got %v", err)
	}

	// The snapshot adapter itself: a malformed id is an error (never a
	// silent empty manifest — an empty snapshot would look like a valid
	// export), and an unknown-but-valid UUID is an empty lineage, not an
	// error (the caller's state read reports not-found).
	store := persistence.NewManifestStore(f.pool)
	if _, err := store.GetManifestSnapshot(ctx, "not-a-uuid"); err == nil {
		t.Fatalf("snapshot of a malformed state id: want an error, got nil")
	}
	snap, err := store.GetManifestSnapshot(ctx, "99999999-9999-9999-9999-999999999999")
	if err != nil {
		t.Fatalf("snapshot of an unknown state: %v", err)
	}
	if len(snap.ObjectVersions) != 0 || len(snap.RelationVersions) != 0 || len(snap.BlobRefs) != 0 {
		t.Fatalf("snapshot of an unknown state must be empty, got %+v", snap)
	}
}
