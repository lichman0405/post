package integration

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/diffs"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/sciobjects"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
	rsgdiff "github.com/lichman0405/post/internal/rsg/diff"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// Task T0401: Research State Diff 引擎 — over a REAL PostgreSQL, with the
// same store composition the API uses. Proves both acceptance criteria
// end to end:
//
//   - 能稳定列出 create/update/abort/relation changes: a three-branch
//     scenario (base = fork point, source = feature head, target = main
//     head) lists exactly the source-side changes with their kinds — the
//     updated claim (target moved it too, field info recorded), the
//     aborted hypothesis, the created finding, the created relation — and
//     never lists the target-only experiment or the untouched relation;
//   - golden diff 可重复: two diffs of the same three states render the
//     same canonical bytes, and the change list is the stable
//     identity-sorted order whatever the store's row order.

const diffTaskID = "T0401"

// diffFixture seeds one private project owned by alice and wires the rsg
// service (the write path) plus the diffs service (the read path) over
// the real stores — the composition cmd/api/main.go uses, with
// ManifestStore as the snapshot adapter (the diff shares the manifest's
// lineage snapshot queries).
type diffFixture struct {
	diffs     *diffs.Service
	svc       *rsg.Service
	statesSvc *states.Service
	pool      *pgxpool.Pool
	alice     domain.User
	project   domain.Project
	main      string
	feature   string
	// base is the state the feature branch forked from — the merge base
	// of the three-way diff.
	base string
}

func newDiffFixture(t *testing.T, ctx context.Context) *diffFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), diffTaskID)
	alice, err := persistence.NewCredentialStore(pool).CreateWithPassword(
		ctx, "diff-alice@example.com", "hash", "diff-alice", "Alice")
	if err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	orgStore := persistence.NewOrgStore(pool)
	org, _, err := orgStore.CreateOrganization(ctx, domain.Organization{
		Slug: "diff-fixture", Name: "Diff Fixture",
	}, alice.ID, todayUTC())
	if err != nil {
		t.Fatalf("create fixture org: %v", err)
	}
	projectStore := persistence.NewProjectStore(pool)
	project, _, err := projectStore.CreateProject(ctx, domain.Project{
		OrganizationID:  &org.ID,
		Slug:            "diff-project",
		Name:            "Diff Project",
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
	})
	main, err := svc.CreateBranch(ctx, alice, project.ID, rsg.CreateBranchInput{
		Name:       "main",
		BaseRef:    "",
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	return &diffFixture{
		diffs: diffs.NewService(
			stateStore,
			persistence.NewManifestStore(pool),
		),
		svc:       svc,
		statesSvc: statesSvc,
		pool:      pool,
		alice:     alice,
		project:   project,
		main:      main.ID,
	}
}

// createObject writes one object on the branch through the rsg service and
// returns the object id and its version id.
func (f *diffFixture) createObject(t *testing.T, ctx context.Context, branch, objectType, payload string) (string, string) {
	t.Helper()
	res, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, branch, rsg.CreateObjectInput{
		ObjectType: objectType,
		Payload:    json.RawMessage(payload),
	})
	if err != nil {
		t.Fatalf("CreateObject: %v", err)
	}
	return res.Object.ID, res.Version.ID
}

// updateObject appends version expected+1 to the object on the branch
// through the rsg service.
func (f *diffFixture) updateObject(t *testing.T, ctx context.Context, branch, objectID string, expected int, patch string) {
	t.Helper()
	if _, err := f.svc.CreateObjectVersion(ctx, f.alice, f.project.ID, branch, objectID, rsg.CreateObjectVersionInput{
		ExpectedVersion: expected,
		Patch:           json.RawMessage(patch),
	}); err != nil {
		t.Fatalf("CreateObjectVersion: %v", err)
	}
}

// createRelation writes one typed relation on the branch through the rsg
// service.
func (f *diffFixture) createRelation(t *testing.T, ctx context.Context, branch, relationType, sourceVersionID, targetVersionID string) string {
	t.Helper()
	res, err := f.svc.CreateRelation(ctx, f.alice, f.project.ID, branch, rsg.CreateRelationInput{
		RelationType:          relationType,
		SourceObjectVersionID: sourceVersionID,
		TargetObjectVersionID: targetVersionID,
		Payload:               json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateRelation: %v", err)
	}
	return res.Relation.ID
}

// abortObject appends a version in lifecycle 'aborted' to the object as
// one state commit — the abort path the V1 write API does not expose yet,
// written through the same transaction surface a future AbortObject
// command will use. The commit records a git ref (the git_compat push
// shape), so the source head carries one for the file-diff-ref half.
func (f *diffFixture) abortObject(t *testing.T, ctx context.Context, branch, objectID string, expected int) string {
	t.Helper()
	objects := persistence.NewScientificObjectStore(f.pool)
	var aborted domain.ScientificObjectVersion
	sha := "cccccccccccccccccccccccccccccccccccccccc"
	_, _, err := f.statesSvc.Commit(ctx, states.CommitParams{
		ProjectID: f.project.ID,
		BranchID:  branch,
		ActorID:   f.alice.ID,
		Via:       domain.ViaGitCompat,
		Message:   "abort hypothesis (git push)",
		Operations: []domain.StateOperation{{
			Kind:      domain.OperationObjectVersionCreated,
			EntityID:  objectID,
			VersionNo: expected + 1,
		}},
		BaseStateID:     f.head(t, ctx, branch),
		GitCommitSHA:    &sha,
		ManifestVersion: "v1",
		Gate:            rsgvalidation.GateDraft,
	}, func(ctx context.Context, tx states.Transaction, stateID string) error {
		var err error
		aborted, err = objects.CreateVersionInTx(ctx, tx, objectID, expected, sciobjects.VersionParams{
			StateID:        stateID,
			BranchID:       &branch,
			SchemaID:       "https://open-rd.example/schemas/hypothesis.schema.json",
			SchemaVersion:  "1",
			Title:          "aborted hypothesis",
			LifecycleState: domain.LifecycleAborted,
			Payload:        json.RawMessage(`{"question_ref":"q1"}`),
			CreatedBy:      f.alice.ID,
		})
		return err
	})
	if err != nil {
		t.Fatalf("abort commit: %v", err)
	}
	return aborted.ID
}

// head returns the branch's current head state id.
func (f *diffFixture) head(t *testing.T, ctx context.Context, branch string) *string {
	t.Helper()
	state, err := f.statesSvc.GetBranchHead(ctx, branch)
	if err != nil {
		t.Fatalf("GetBranchHead: %v", err)
	}
	return &state.ID
}

// computeDiff runs the diffs service over the three named states.
func (f *diffFixture) computeDiff(t *testing.T, ctx context.Context, base, source, target string) *rsgdiff.Diff {
	t.Helper()
	d, err := f.diffs.Diff(ctx, diffs.Params{
		ProjectID:     f.project.ID,
		BaseStateID:   base,
		SourceStateID: source,
		TargetStateID: target,
	})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	return d
}

// TestDiffListsStableChanges is the first acceptance criterion over the
// real stack: the three-way scenario lists exactly the source-side
// changes with their kinds, in stable identity order.
func TestDiffListsStableChanges(t *testing.T) {
	ctx := testCtx(t)
	f := newDiffFixture(t, ctx)

	// main: claim C, hypothesis H, relation R (C supports H).
	cObject, cV1 := f.createObject(t, ctx, f.main, "claim", `{"statement":"alpha"}`)
	hObject, hV1 := f.createObject(t, ctx, f.main, "hypothesis", `{"question_ref":"q1"}`)
	rRelation := f.createRelation(t, ctx, f.main, "supports", cV1, hV1)
	_ = rRelation

	// feature forks main's head — that state is the merge base.
	feature, err := f.svc.CreateBranch(ctx, f.alice, f.project.ID, rsg.CreateBranchInput{
		Name:       "feature",
		BaseRef:    *f.head(t, ctx, f.main),
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create feature branch: %v", err)
	}
	f.feature = feature.ID
	f.base = *feature.BaseStateID

	// main (target) moves first: experiment E created — the target-side
	// movement the three-way diff must record without listing E as a
	// source-side change. (Order matters for the fixtures: two identical
	// "update C to v2" transitions from the SAME parent state would
	// collide on the state content hash by design — the identical
	// transition IS the same state — so the two updates commit from
	// different parents.)
	f.createObject(t, ctx, f.main, "experiment", `{"design":"d"}`)

	// feature: update C (v2), create finding F, create relation R2, then
	// abort H last — the abort commit records the git ref, so the source
	// head carries it (the git_compat push shape).
	f.updateObject(t, ctx, f.feature, cObject, 1, `{"statement":"beta"}`)
	fObject, fV1 := f.createObject(t, ctx, f.feature, "finding", `{"summary":"found"}`)
	f.createRelation(t, ctx, f.feature, "supports", fV1, fV1)
	f.abortObject(t, ctx, f.feature, hObject, 1)

	// main updates C differently — the object's version log is one global
	// log per object (versions carry the branch they were created on), so
	// main's update appends v3 after feature's v2, from a different parent
	// state: the two versions are distinct rows.
	f.updateObject(t, ctx, f.main, cObject, 2, `{"statement":"gamma"}`)

	// The PR diff: base = fork point, source = feature head, target = main head.
	sourceHead := *f.head(t, ctx, f.feature)
	targetHead := *f.head(t, ctx, f.main)
	d := f.computeDiff(t, ctx, f.base, sourceHead, targetHead)

	// Exactly three object changes, identity-sorted: C, F, H.
	if len(d.ObjectChanges) != 3 {
		t.Fatalf("object changes = %+v, want exactly [C, F, H]", d.ObjectChanges)
	}
	byID := map[string]rsgdiff.ObjectChange{}
	for _, c := range d.ObjectChanges {
		byID[c.ObjectID] = c
	}
	ids := make([]string, 0, 3)
	for _, c := range d.ObjectChanges {
		ids = append(ids, c.ObjectID)
	}
	if ids[0] > ids[1] || ids[1] > ids[2] {
		t.Fatalf("object changes not identity-sorted: %v", ids)
	}

	c := byID[cObject]
	if c.Kind != rsgdiff.ChangeUpdated {
		t.Fatalf("claim kind = %q, want updated", c.Kind)
	}
	if !c.TargetMoved {
		t.Fatalf("claim TargetMoved = false; main updated it since the fork")
	}
	if !containsField(c.ChangedFields, "payload") || !containsField(c.TargetChangedFields, "payload") {
		t.Fatalf("claim field info = %v / %v, want payload moves on both sides", c.ChangedFields, c.TargetChangedFields)
	}
	if c.BaseVersion == nil || c.SourceVersion.ID == "" || c.TargetVersion == nil {
		t.Fatalf("claim heads missing: base=%v source=%v target=%v", c.BaseVersion, c.SourceVersion.ID, c.TargetVersion)
	}
	if c.TargetVersion.ID == c.SourceVersion.ID {
		t.Fatalf("claim source and target heads are the same row %s; the two updates must be distinct versions", c.SourceVersion.ID)
	}

	h := byID[hObject]
	if h.Kind != rsgdiff.ChangeAborted {
		t.Fatalf("hypothesis kind = %q, want aborted", h.Kind)
	}
	if h.SourceVersion.LifecycleState != string(domain.LifecycleAborted) {
		t.Fatalf("hypothesis source head lifecycle = %q, want aborted", h.SourceVersion.LifecycleState)
	}

	fl := byID[fObject]
	if fl.Kind != rsgdiff.ChangeCreated || fl.BaseVersion != nil {
		t.Fatalf("finding = %+v, want created with nil base", fl)
	}
	for _, c := range d.ObjectChanges {
		if c.ObjectType == "experiment" {
			t.Fatalf("target-only experiment listed as source-side change: %+v", c)
		}
	}

	// Relations: only R2 (created); R itself did not move on feature.
	if len(d.RelationChanges) != 1 {
		t.Fatalf("relation changes = %+v, want exactly one", d.RelationChanges)
	}
	if d.RelationChanges[0].Kind != rsgdiff.ChangeCreated {
		t.Fatalf("relation change kind = %q, want created", d.RelationChanges[0].Kind)
	}
	if d.RelationChanges[0].RelationID == rRelation {
		t.Fatalf("untouched relation R listed as a change: %+v", d.RelationChanges[0])
	}

	// Summary categories match the scenario.
	s := d.Summary
	if s.ObjectsCreated != 1 || s.ObjectsUpdated != 1 || s.ObjectsAborted != 1 || s.RelationsCreated != 1 {
		t.Fatalf("summary = %+v, want created=1 updated=1 aborted=1 relations_created=1", s)
	}

	// File diff refs: the abort commit recorded a git ref on the source
	// head; base and target have none — the source side renders with an
	// empty base end.
	if len(d.FileDiffRefs) != 1 || d.FileDiffRefs[0].Kind != "source" ||
		d.FileDiffRefs[0].HeadGitRef != "cccccccccccccccccccccccccccccccccccccccc" {
		t.Fatalf("file diff refs = %+v, want the source side only", d.FileDiffRefs)
	}
}

// TestDiffGoldenReproducible is the second acceptance criterion over the
// real stack: diffing the same three states twice yields the same
// canonical bytes, timestamps included.
func TestDiffGoldenReproducible(t *testing.T) {
	ctx := testCtx(t)
	f := newDiffFixture(t, ctx)

	cObject, cV1 := f.createObject(t, ctx, f.main, "claim", `{"statement":"alpha"}`)
	hObject, hV1 := f.createObject(t, ctx, f.main, "hypothesis", `{"question_ref":"q1"}`)
	f.createRelation(t, ctx, f.main, "supports", cV1, hV1)

	feature, err := f.svc.CreateBranch(ctx, f.alice, f.project.ID, rsg.CreateBranchInput{
		Name:       "feature",
		BaseRef:    *f.head(t, ctx, f.main),
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create feature branch: %v", err)
	}
	base := *feature.BaseStateID
	f.updateObject(t, ctx, feature.ID, cObject, 1, `{"statement":"beta"}`)
	f.abortObject(t, ctx, feature.ID, hObject, 1)

	sourceHead := *f.head(t, ctx, feature.ID)
	targetHead := *f.head(t, ctx, f.main)

	d1 := f.computeDiff(t, ctx, base, sourceHead, targetHead)
	d2 := f.computeDiff(t, ctx, base, sourceHead, targetHead)
	b1, err := d1.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	b2, err := d2.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if string(b1) != string(b2) {
		t.Fatalf("two diffs of the same states drifted:\nfirst: %s\nsecond: %s", b1, b2)
	}
	if len(d1.ObjectChanges) != 2 {
		t.Fatalf("object changes = %+v, want the updated claim and the aborted hypothesis", d1.ObjectChanges)
	}
}

// containsField reports whether the changed-fields list carries the field.
func containsField(fields []string, name string) bool {
	for _, f := range fields {
		if f == name {
			return true
		}
	}
	return false
}

// TestDiffCrossProjectRefused pins the service's boundary: a state of
// another project reports ErrValidation, never its content.
func TestDiffCrossProjectRefused(t *testing.T) {
	ctx := testCtx(t)
	f := newDiffFixture(t, ctx)

	// A second project with its own state.
	alice2, err := persistence.NewCredentialStore(f.pool).CreateWithPassword(
		ctx, "diff-bob@example.com", "hash", "diff-bob", "Bob")
	if err != nil {
		t.Fatalf("seed bob: %v", err)
	}
	orgStore := persistence.NewOrgStore(f.pool)
	org2, _, err := orgStore.CreateOrganization(ctx, domain.Organization{
		Slug: "diff-fixture-2", Name: "Diff Fixture 2",
	}, alice2.ID, todayUTC())
	if err != nil {
		t.Fatalf("create org 2: %v", err)
	}
	project2, _, err := persistence.NewProjectStore(f.pool).CreateProject(ctx, domain.Project{
		OrganizationID:  &org2.ID,
		Slug:            "diff-project-2",
		Name:            "Diff Project 2",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, alice2.ID)
	if err != nil {
		t.Fatalf("create project 2: %v", err)
	}
	main2, err := f.svc.CreateBranch(ctx, alice2, project2.ID, rsg.CreateBranchInput{
		Name:       "main",
		BaseRef:    "",
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create branch 2: %v", err)
	}
	state2, err := f.statesSvc.GetBranchHead(ctx, main2.ID)
	if err != nil {
		t.Fatalf("GetBranchHead 2: %v", err)
	}

	fbase := *f.head(t, ctx, f.main)
	_, err = f.diffs.Diff(ctx, diffs.Params{
		ProjectID:     f.project.ID,
		BaseStateID:   fbase,
		SourceStateID: fbase,
		TargetStateID: state2.ID, // a state of project 2
	})
	if err == nil || !errors.Is(err, diffs.ErrValidation) {
		t.Fatalf("cross-project diff error = %v, want diffs.ErrValidation", err)
	}
}
