package rsg

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/relations"
	"github.com/lichman0405/post/internal/application/sciobjects"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	"github.com/lichman0405/post/internal/rsg/semantics"
)

// The fakes model exactly the slices the service composes, with the same
// outcome vocabulary as the production adapters. The object fake keeps an
// in-memory log with the real CAS semantics, and counts its reads — the
// no-leak tests assert the denial happens before any object lookup.

type fakeProjects struct {
	role       *domain.ProjectRole
	membership error // projects.ErrMemberNotFound for readable-no-role
	getErr     error
	getCalls   int
}

func (f *fakeProjects) GetMembership(ctx context.Context, actor domain.User, projectID string) (domain.ProjectMembership, error) {
	if f.membership != nil {
		return domain.ProjectMembership{}, f.membership
	}
	role := domain.ProjectRoleViewer
	if f.role != nil {
		role = *f.role
	}
	return domain.ProjectMembership{ProjectID: projectID, UserID: actor.ID, Role: role, CreatedAt: time.Now()}, nil
}

func (f *fakeProjects) Get(ctx context.Context, r projects.Reader, projectID string) (domain.Project, error) {
	f.getCalls++
	if f.getErr != nil {
		return domain.Project{}, f.getErr
	}
	return domain.Project{ID: projectID, Visibility: domain.VisibilityPublic}, nil
}

type fakeBranches struct {
	branch  domain.Branch
	getErr  error
	err     error // Create error override
	created bool
	list    []domain.Branch
	listErr error
}

func (f *fakeBranches) Create(ctx context.Context, in branches.CreateBranchParams) (domain.Branch, error) {
	if f.err != nil {
		return domain.Branch{}, f.err
	}
	f.created = true
	b := domain.Branch{ID: "branch-1", ProjectID: in.ProjectID, Name: in.Name, BaseStateID: &in.BaseStateID, CreatedBy: in.CreatedBy}
	if b.ID == "" {
		b = f.branch
	}
	return b, nil
}

func (f *fakeBranches) Get(ctx context.Context, projectID, branchID string) (domain.Branch, error) {
	if f.getErr != nil {
		return domain.Branch{}, f.getErr
	}
	return domain.Branch{ID: branchID, ProjectID: projectID, Name: "main"}, nil
}

func (f *fakeBranches) List(ctx context.Context, projectID string) ([]domain.Branch, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.list != nil {
		return f.list, nil
	}
	return []domain.Branch{{ID: "branch-1", ProjectID: projectID, Name: "main"}}, nil
}

// fakeTx is a states.Transaction stub: the object fake never issues SQL on
// it (only the production adapters do).
type fakeTx struct{}

func (fakeTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (fakeTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return nil, nil
}
func (fakeTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return nil
}

type fakeStates struct {
	head       domain.ProjectState
	headErr    error
	commitErr  error
	committed  int
	stateID    string
	genesisErr error
	commits    []domain.StateCommit
	commitsErr error
}

func (f *fakeStates) Commit(ctx context.Context, in states.CommitParams, write states.WriteFunc) (domain.ProjectState, domain.StateCommit, error) {
	if f.commitErr != nil {
		return domain.ProjectState{}, domain.StateCommit{}, f.commitErr
	}
	if err := write(ctx, fakeTx{}, f.stateID); err != nil {
		return domain.ProjectState{}, domain.StateCommit{}, err
	}
	f.committed++
	return domain.ProjectState{ID: f.stateID, ProjectID: in.ProjectID, BranchID: &in.BranchID}, domain.StateCommit{ProjectID: in.ProjectID, BranchID: in.BranchID}, nil
}

func (f *fakeStates) CreateInitialState(ctx context.Context, in states.CreateInitialStateParams) (domain.ProjectState, error) {
	if f.genesisErr != nil {
		return domain.ProjectState{}, f.genesisErr
	}
	return domain.ProjectState{ID: "genesis-1", ProjectID: in.ProjectID}, nil
}

func (f *fakeStates) GetBranchHead(ctx context.Context, branchID string) (domain.ProjectState, error) {
	if f.headErr != nil {
		return domain.ProjectState{}, f.headErr
	}
	return f.head, nil
}

func (f *fakeStates) ListCommits(ctx context.Context, branchID string) ([]domain.StateCommit, error) {
	return f.commits, f.commitsErr
}

type fakeLatest struct {
	state domain.ProjectState
	err   error
}

func (f *fakeLatest) GetLatestState(ctx context.Context, projectID string) (domain.ProjectState, error) {
	if f.err != nil {
		return domain.ProjectState{}, f.err
	}
	return f.state, nil
}

// fakeObjects keeps the in-memory version log with the real CAS outcome
// vocabulary, and counts reads so the tests can prove the authorization
// refusal happens before any object state is touched.
type fakeObjects struct {
	objects     map[string]domain.ScientificObject
	versions    map[string][]domain.ScientificObjectVersion
	byID        map[string]domain.ScientificObjectVersion
	objectCalls int
	readCalls   int
	createErr   error
}

func newFakeObjects() *fakeObjects {
	return &fakeObjects{
		objects:  map[string]domain.ScientificObject{},
		versions: map[string][]domain.ScientificObjectVersion{},
		byID:     map[string]domain.ScientificObjectVersion{},
	}
}

func (f *fakeObjects) GetObject(ctx context.Context, objectID string) (domain.ScientificObject, error) {
	f.objectCalls++
	o, ok := f.objects[objectID]
	if !ok {
		return domain.ScientificObject{}, sciobjects.ErrObjectNotFound
	}
	return o, nil
}

func (f *fakeObjects) GetLatestVersion(ctx context.Context, objectID string) (domain.ScientificObjectVersion, error) {
	f.readCalls++
	vs := f.versions[objectID]
	if len(vs) == 0 {
		return domain.ScientificObjectVersion{}, sciobjects.ErrVersionNotFound
	}
	return vs[len(vs)-1], nil
}

func (f *fakeObjects) GetVersionByID(ctx context.Context, versionID string) (domain.ScientificObjectVersion, error) {
	f.readCalls++
	v, ok := f.byID[versionID]
	if !ok {
		return domain.ScientificObjectVersion{}, sciobjects.ErrVersionNotFound
	}
	return v, nil
}

func (f *fakeObjects) GetVersion(ctx context.Context, objectID string, versionNo int) (domain.ScientificObjectVersion, error) {
	f.readCalls++
	vs := f.versions[objectID]
	for _, v := range vs {
		if v.VersionNo == versionNo {
			return v, nil
		}
	}
	return domain.ScientificObjectVersion{}, sciobjects.ErrVersionNotFound
}

func (f *fakeObjects) ListVersions(ctx context.Context, objectID string) ([]domain.ScientificObjectVersion, error) {
	f.readCalls++
	return f.versions[objectID], nil
}

func (f *fakeObjects) CreateObjectInTx(ctx context.Context, tx states.Transaction, in CreateObjectInTxParams) (domain.ScientificObject, domain.ScientificObjectVersion, error) {
	if f.createErr != nil {
		return domain.ScientificObject{}, domain.ScientificObjectVersion{}, f.createErr
	}
	obj := domain.ScientificObject{ID: in.ObjectID, ProjectID: in.ProjectID, ObjectType: in.ObjectType, CurrentVersionNo: 1, CreatedBy: in.CreatedBy, CreatedAt: time.Now()}
	v := domain.ScientificObjectVersion{
		ID: in.ObjectID + "-v1", ObjectID: in.ObjectID, VersionNo: 1, StateID: in.Version.StateID,
		SchemaID: in.Version.SchemaID, SchemaVersion: in.Version.SchemaVersion, Title: in.Version.Title,
		LifecycleState: in.Version.LifecycleState, Payload: in.Version.Payload, CreatedBy: in.CreatedBy, CreatedAt: time.Now(),
	}
	f.objects[in.ObjectID] = obj
	f.versions[in.ObjectID] = []domain.ScientificObjectVersion{v}
	f.byID[v.ID] = v
	return obj, v, nil
}

func (f *fakeObjects) CreateVersionInTx(ctx context.Context, tx states.Transaction, objectID string, expected int, in sciobjects.VersionParams) (domain.ScientificObjectVersion, error) {
	o, ok := f.objects[objectID]
	if !ok {
		return domain.ScientificObjectVersion{}, sciobjects.ErrObjectNotFound
	}
	if o.CurrentVersionNo != expected {
		return domain.ScientificObjectVersion{}, &sciobjects.VersionConflictError{ObjectID: objectID, Expected: expected, Actual: o.CurrentVersionNo}
	}
	v := domain.ScientificObjectVersion{
		ID: objectID + "-v" + string(rune('1'+len(f.versions[objectID]))), ObjectID: objectID,
		VersionNo: expected + 1, StateID: in.StateID, SchemaID: in.SchemaID, SchemaVersion: in.SchemaVersion,
		Title: in.Title, LifecycleState: in.LifecycleState, Payload: in.Payload, CreatedBy: in.CreatedBy, CreatedAt: time.Now(),
	}
	f.versions[objectID] = append(f.versions[objectID], v)
	f.byID[v.ID] = v
	o.CurrentVersionNo = expected + 1
	f.objects[objectID] = o
	return v, nil
}

type fakeRelations struct {
	created   int
	createErr error
}

func (f *fakeRelations) CreateRelationInTx(ctx context.Context, tx states.Transaction, in CreateRelationInTxParams) (domain.Relation, domain.RelationVersion, error) {
	if f.createErr != nil {
		return domain.Relation{}, domain.RelationVersion{}, f.createErr
	}
	f.created++
	rel := domain.Relation{ID: in.RelationID, ProjectID: in.ProjectID, CurrentVersionNo: 1, CreatedAt: time.Now()}
	v := domain.RelationVersion{
		ID: in.RelationID + "-v1", RelationID: in.RelationID, VersionNo: 1, StateID: in.Version.StateID,
		RelationType: in.Version.RelationType, SourceObjectVersionID: in.Version.SourceObjectVersionID,
		TargetObjectVersionID: in.Version.TargetObjectVersionID, Payload: in.Version.Payload, CreatedBy: in.Version.CreatedBy, CreatedAt: time.Now(),
	}
	return rel, v, nil
}

func (f *fakeRelations) ListVersionsForObject(ctx context.Context, projectID, objectID string) ([]ObjectRelationVersion, error) {
	return nil, nil
}

func newTestService(t *testing.T, projects *fakeProjects, objects *fakeObjects) *Service {
	t.Helper()
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	sts := &fakeStates{head: domain.ProjectState{ID: "head-1"}, stateID: "state-1"}
	return NewService(Deps{
		Projects:  projects,
		Branches:  &fakeBranches{},
		States:    sts,
		Latest:    &fakeLatest{state: domain.ProjectState{ID: "latest-1"}},
		Objects:   objects,
		Relations: &fakeRelations{},
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
	})
}

func ownerActor() domain.User {
	return domain.User{ID: "owner-1", Handle: "owner", Email: "owner@example.test"}
}

func memberProject() *fakeProjects {
	role := domain.ProjectRoleOwner
	return &fakeProjects{role: &role}
}

func TestCreateObjectRequiresScientificStateWrite(t *testing.T) {
	svc := newTestService(t, memberProject(), newFakeObjects())
	_, err := svc.CreateObject(context.Background(), ownerActor(), "project-1", "branch-1", CreateObjectInput{
		ObjectType: "material",
		Payload:    json.RawMessage(`{"name":"MOF-5"}`),
	})
	if err != nil {
		t.Fatalf("CreateObject: %v", err)
	}
}

func TestCreateObjectUnknownTypeRefused(t *testing.T) {
	svc := newTestService(t, memberProject(), newFakeObjects())
	_, err := svc.CreateObject(context.Background(), ownerActor(), "project-1", "branch-1", CreateObjectInput{
		ObjectType: "wormhole",
		Payload:    json.RawMessage(`{}`),
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("unknown object type: err = %v, want ErrValidation", err)
	}
}

func TestCreateObjectPayloadMustBeObject(t *testing.T) {
	svc := newTestService(t, memberProject(), newFakeObjects())
	_, err := svc.CreateObject(context.Background(), ownerActor(), "project-1", "branch-1", CreateObjectInput{
		ObjectType: "material",
		Payload:    json.RawMessage(`[1,2]`),
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("array payload: err = %v, want ErrValidation", err)
	}
}

func TestCreateObjectClaimAtomicityIsHintOnly(t *testing.T) {
	svc := newTestService(t, memberProject(), newFakeObjects())
	res, err := svc.CreateObject(context.Background(), ownerActor(), "project-1", "branch-1", CreateObjectInput{
		ObjectType: "claim",
		Payload:    json.RawMessage(`{"statement":"The sample is phase-pure. Its density matches the literature."}`),
	})
	if err != nil {
		t.Fatalf("CreateObject(claim): %v — atomicity must never hard-fail the write", err)
	}
	if len(res.Hints) != 1 || res.Hints[0].Code != semantics.HintClaimCompound {
		t.Fatalf("compound claim hints = %+v, want exactly %s", res.Hints, semantics.HintClaimCompound)
	}
}

func TestCreateObjectHardSemanticFailure(t *testing.T) {
	svc := newTestService(t, memberProject(), newFakeObjects())
	res, err := svc.CreateObject(context.Background(), ownerActor(), "project-1", "branch-1", CreateObjectInput{
		ObjectType: "research_question",
		Payload:    json.RawMessage(`{"statement":"What holds MOF-5 together?"}`),
	})
	if err != nil {
		t.Fatalf("CreateObject(research_question): %v", err)
	}
	// The self-parent rule is mechanically decidable and reachable on a
	// version write (the object id is caller-known there): the service
	// refuses a patch that makes the question its own parent.
	_, err = svc.CreateObjectVersion(context.Background(), ownerActor(), "project-1", "branch-1", res.Object.ID,
		CreateObjectVersionInput{ExpectedVersion: 1, Patch: json.RawMessage(`{"parent_question_id":"` + res.Object.ID + `"}`)})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("self-parent research question: err = %v, want ErrValidation", err)
	}
}

// The task's acceptance criterion in its own words: any relation/object
// write command by a caller who is not a project member (or whom the
// matrix denies) is refused, and the refusal must not disclose whether the
// object exists. The service refuses before ANY object lookup, so the
// denial is identical for an existing and a nonexistent object.
func TestWriteDenialHidesObjectExistence(t *testing.T) {
	objects := newFakeObjects()
	// An object that exists, in the fake store.
	objects.objects["existing-1"] = domain.ScientificObject{ID: "existing-1", ProjectID: "project-1", ObjectType: "material", CurrentVersionNo: 1}
	objects.versions["existing-1"] = []domain.ScientificObjectVersion{{ID: "existing-1-v1", ObjectID: "existing-1", VersionNo: 1}}

	for _, denied := range []struct {
		name   string
		gates  *fakeProjects
		expect error
	}{
		// Non-member of a private project: the project read gate hides the
		// project itself (the same 404 shape as an unknown project).
		{"non-member of a private project", &fakeProjects{membership: projects.ErrProjectNotFound}, projects.ErrProjectNotFound},
		// Non-member of a public project: readable, no role — the matrix
		// column authenticated_nonmember is denied (OwnForkOnly is a
		// conditional this site does not resolve).
		{"non-member of a public project", &fakeProjects{membership: projects.ErrMemberNotFound}, ErrForbidden},
		// Viewer: the matrix denies write_scientific_state for the viewer
		// column outright.
		{"viewer member", &fakeProjects{role: rolePtr(domain.ProjectRoleViewer)}, ErrForbidden},
	} {
		t.Run(denied.name, func(t *testing.T) {
			svc := newTestService(t, denied.gates, objects)
			before := objects.objectCalls + objects.readCalls

			_, existingErr := svc.CreateObjectVersion(context.Background(), ownerActor(), "project-1", "branch-1", "existing-1",
				CreateObjectVersionInput{ExpectedVersion: 1, Patch: json.RawMessage(`{"name":"x"}`)})
			_, missingErr := svc.CreateObjectVersion(context.Background(), ownerActor(), "project-1", "branch-1", "nonexistent-1",
				CreateObjectVersionInput{ExpectedVersion: 1, Patch: json.RawMessage(`{"name":"x"}`)})

			if !errors.Is(existingErr, denied.expect) {
				t.Fatalf("denied write on an EXISTING object: err = %v, want %v", existingErr, denied.expect)
			}
			if !errors.Is(missingErr, denied.expect) {
				t.Fatalf("denied write on a NONEXISTENT object: err = %v, want %v", missingErr, denied.expect)
			}
			if existingErr.Error() != missingErr.Error() {
				t.Fatalf("denials differ by existence: %q vs %q", existingErr, missingErr)
			}
			if after := objects.objectCalls + objects.readCalls; after != before {
				t.Fatalf("denial touched object state: read calls went from %d to %d (the refusal must precede any lookup)", before, after)
			}
		})
	}
}

func TestRelationWriteDenialHidesEndpointExistence(t *testing.T) {
	objects := newFakeObjects()
	objects.byID["existing-v1"] = domain.ScientificObjectVersion{ID: "existing-v1", ObjectID: "existing-1", VersionNo: 1}

	svc := newTestService(t, &fakeProjects{membership: projects.ErrProjectNotFound}, objects)
	before := objects.readCalls
	_, existingErr := svc.CreateRelation(context.Background(), ownerActor(), "project-1", "branch-1", CreateRelationInput{
		RelationType:          "derived_from",
		SourceObjectVersionID: "existing-v1",
		TargetObjectVersionID: "missing-v1",
	})
	_, missingErr := svc.CreateRelation(context.Background(), ownerActor(), "project-1", "branch-1", CreateRelationInput{
		RelationType:          "derived_from",
		SourceObjectVersionID: "missing-v1",
		TargetObjectVersionID: "also-missing-v1",
	})
	if !errors.Is(existingErr, projects.ErrProjectNotFound) || !errors.Is(missingErr, projects.ErrProjectNotFound) {
		t.Fatalf("denied relation writes: existing=%v missing=%v, want ErrProjectNotFound for both", existingErr, missingErr)
	}
	if after := objects.readCalls; after != before {
		t.Fatalf("denial touched endpoint lookups: read calls went from %d to %d", before, after)
	}
}

func TestCreateBranchRequiresMember(t *testing.T) {
	for _, denied := range []*fakeProjects{
		{membership: projects.ErrProjectNotFound}, // non-member, private
		{membership: projects.ErrMemberNotFound},  // non-member, public
		{role: rolePtr(domain.ProjectRoleViewer)}, // viewer
	} {
		svc := newTestService(t, denied, newFakeObjects())
		_, err := svc.CreateBranch(context.Background(), ownerActor(), "project-1", CreateBranchInput{Name: "main"})
		if err == nil || (errors.Is(err, ErrForbidden) == false && errors.Is(err, projects.ErrProjectNotFound) == false) {
			t.Fatalf("branch create by denied caller: err = %v, want a denial", err)
		}
	}
	contrib := rolePtr(domain.ProjectRoleContributor)
	svc := newTestService(t, &fakeProjects{role: contrib}, newFakeObjects())
	if _, err := svc.CreateBranch(context.Background(), ownerActor(), "project-1", CreateBranchInput{Name: "main"}); err != nil {
		t.Fatalf("contributor branch create: %v", err)
	}
}

func TestCreateBranchSeedsGenesisWhenProjectHasNoState(t *testing.T) {
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	sts := &fakeStates{head: domain.ProjectState{ID: "head-1"}, stateID: "state-1"}
	branches := &fakeBranches{}
	svc := NewService(Deps{
		Projects:  memberProject(),
		Branches:  branches,
		States:    sts,
		Latest:    &fakeLatest{err: states.ErrStateNotFound},
		Objects:   newFakeObjects(),
		Relations: &fakeRelations{},
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
	})
	branch, err := svc.CreateBranch(context.Background(), ownerActor(), "project-1", CreateBranchInput{Name: "main"})
	if err != nil {
		t.Fatalf("first branch: %v", err)
	}
	if branch.BaseStateID == nil || *branch.BaseStateID != "genesis-1" {
		t.Fatalf("first branch base state = %v, want the seeded genesis root", branch.BaseStateID)
	}
}

func TestCreateObjectVersionAppendsAndMerges(t *testing.T) {
	objects := newFakeObjects()
	svc := newTestService(t, memberProject(), objects)
	created, err := svc.CreateObject(context.Background(), ownerActor(), "project-1", "branch-1", CreateObjectInput{
		ObjectType: "material",
		Payload:    json.RawMessage(`{"name":"MOF-5","formula":"old"}`),
	})
	if err != nil {
		t.Fatalf("CreateObject: %v", err)
	}
	res, err := svc.CreateObjectVersion(context.Background(), ownerActor(), "project-1", "branch-1", created.Object.ID,
		CreateObjectVersionInput{ExpectedVersion: 1, Patch: json.RawMessage(`{"formula":"Zn4O(BDC)3"}`)})
	if err != nil {
		t.Fatalf("CreateObjectVersion: %v", err)
	}
	if res.Version.VersionNo != 2 {
		t.Fatalf("new version number = %d, want 2", res.Version.VersionNo)
	}
	if res.Version.ID == created.Version.ID {
		t.Fatalf("version identity did not change: %s", res.Version.ID)
	}
	var payload map[string]any
	if err := json.Unmarshal(res.Version.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	// Shallow merge: the patch replaced formula, kept name.
	if payload["name"] != "MOF-5" || payload["formula"] != "Zn4O(BDC)3" {
		t.Fatalf("merged payload = %v, want name kept and formula replaced", payload)
	}
}

func TestCreateObjectVersionConflictPassthrough(t *testing.T) {
	objects := newFakeObjects()
	svc := newTestService(t, memberProject(), objects)
	created, err := svc.CreateObject(context.Background(), ownerActor(), "project-1", "branch-1", CreateObjectInput{
		ObjectType: "material", Payload: json.RawMessage(`{"name":"MOF-5"}`),
	})
	if err != nil {
		t.Fatalf("CreateObject: %v", err)
	}
	_, err = svc.CreateObjectVersion(context.Background(), ownerActor(), "project-1", "branch-1", created.Object.ID,
		CreateObjectVersionInput{ExpectedVersion: 7, Patch: json.RawMessage(`{"name":"x"}`)})
	var conflict *sciobjects.VersionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("stale expectation: err = %v, want *sciobjects.VersionConflictError", err)
	}
	if conflict.Expected != 7 || conflict.Actual != 1 {
		t.Fatalf("conflict = %+v, want expected 7 actual 1", conflict)
	}
}

func TestCreateObjectVersionForeignObjectHidden(t *testing.T) {
	objects := newFakeObjects()
	// The same object id, but in another project.
	objects.objects["foreign-1"] = domain.ScientificObject{ID: "foreign-1", ProjectID: "other-project", ObjectType: "material", CurrentVersionNo: 1}
	objects.versions["foreign-1"] = []domain.ScientificObjectVersion{{ID: "foreign-1-v1", ObjectID: "foreign-1", VersionNo: 1}}
	svc := newTestService(t, memberProject(), objects)
	_, err := svc.CreateObjectVersion(context.Background(), ownerActor(), "project-1", "branch-1", "foreign-1",
		CreateObjectVersionInput{ExpectedVersion: 1, Patch: json.RawMessage(`{}`)})
	if !errors.Is(err, sciobjects.ErrObjectNotFound) {
		t.Fatalf("foreign-project object: err = %v, want ErrObjectNotFound (same shape as unknown)", err)
	}
}

func TestCreateRelationValidatesCatalogAndEndpoints(t *testing.T) {
	objects := newFakeObjects()
	svc := newTestService(t, memberProject(), objects)
	// Unknown catalog type.
	_, err := svc.CreateRelation(context.Background(), ownerActor(), "project-1", "branch-1", CreateRelationInput{
		RelationType: "wormhole", SourceObjectVersionID: "a", TargetObjectVersionID: "b",
	})
	var unknown *relations.UnknownRelationTypeError
	if !errors.As(err, &unknown) {
		t.Fatalf("unknown relation type: err = %v, want UnknownRelationTypeError", err)
	}
	// A missing endpoint names its side.
	_, err = svc.CreateRelation(context.Background(), ownerActor(), "project-1", "branch-1", CreateRelationInput{
		RelationType: "derived_from", SourceObjectVersionID: "missing-v1", TargetObjectVersionID: "b",
	})
	var missing *relations.ReferencedVersionNotFoundError
	if !errors.As(err, &missing) || missing.Side != "source" {
		t.Fatalf("missing endpoint: err = %v, want ReferencedVersionNotFoundError on the source side", err)
	}
	// An endpoint of another project reports the same "not found" outcome
	// (never leak a foreign project's state).
	objects.byID["foreign-v1"] = domain.ScientificObjectVersion{ID: "foreign-v1", ObjectID: "foreign-obj", VersionNo: 1}
	objects.objects["foreign-obj"] = domain.ScientificObject{ID: "foreign-obj", ProjectID: "other-project", ObjectType: "material"}
	_, err = svc.CreateRelation(context.Background(), ownerActor(), "project-1", "branch-1", CreateRelationInput{
		RelationType: "derived_from", SourceObjectVersionID: "foreign-v1", TargetObjectVersionID: "b",
	})
	if !errors.As(err, &missing) || missing.Side != "source" {
		t.Fatalf("foreign endpoint: err = %v, want the same ReferencedVersionNotFoundError", err)
	}
}

func TestCreateRelationHappyPath(t *testing.T) {
	objects := newFakeObjects()
	rels := &fakeRelations{}
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	svc := NewService(Deps{
		Projects: memberProject(), Branches: &fakeBranches{},
		States:  &fakeStates{head: domain.ProjectState{ID: "head-1"}, stateID: "state-1"},
		Latest:  &fakeLatest{state: domain.ProjectState{ID: "latest-1"}},
		Objects: objects, Relations: rels,
		Authz: authz.NewMatrixEngine(), Schemas: reg,
	})
	created, err := svc.CreateObject(context.Background(), ownerActor(), "project-1", "branch-1", CreateObjectInput{
		ObjectType: "material", Payload: json.RawMessage(`{"name":"MOF-5"}`),
	})
	if err != nil {
		t.Fatalf("CreateObject: %v", err)
	}
	res, err := svc.CreateRelation(context.Background(), ownerActor(), "project-1", "branch-1", CreateRelationInput{
		RelationType:          "derived_from",
		SourceObjectVersionID: created.Version.ID,
		TargetObjectVersionID: created.Version.ID,
	})
	if err != nil {
		t.Fatalf("CreateRelation: %v", err)
	}
	if rels.created != 1 || res.Version.VersionNo != 1 {
		t.Fatalf("relation write: created=%d version=%d, want one v1", rels.created, res.Version.VersionNo)
	}
}

func TestGetObjectReadGateAndForeignProjectHidden(t *testing.T) {
	objects := newFakeObjects()
	// The read gate denies: the project itself is hidden (docs/45).
	svc := newTestService(t, &fakeProjects{getErr: projects.ErrProjectNotFound}, objects)
	_, err := svc.GetObject(context.Background(), projects.Reader{UserID: "x", Authenticated: true}, "project-1", "branch-1", "obj-1")
	if !errors.Is(err, projects.ErrProjectNotFound) {
		t.Fatalf("denied read: err = %v, want ErrProjectNotFound", err)
	}
	// A foreign-project object reads as unknown.
	objects.objects["foreign-1"] = domain.ScientificObject{ID: "foreign-1", ProjectID: "other-project", ObjectType: "material", CurrentVersionNo: 1}
	objects.versions["foreign-1"] = []domain.ScientificObjectVersion{{ID: "foreign-1-v1", ObjectID: "foreign-1", VersionNo: 1}}
	svc = newTestService(t, memberProject(), objects)
	_, err = svc.GetObject(context.Background(), projects.Reader{UserID: "x", Authenticated: true}, "project-1", "branch-1", "foreign-1")
	if !errors.Is(err, sciobjects.ErrObjectNotFound) {
		t.Fatalf("foreign object read: err = %v, want ErrObjectNotFound", err)
	}
}

func TestUnwiredEngineFailsClosed(t *testing.T) {
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	svc := NewService(Deps{
		Projects: memberProject(), Branches: &fakeBranches{},
		States:  &fakeStates{head: domain.ProjectState{ID: "head-1"}, stateID: "state-1"},
		Latest:  &fakeLatest{state: domain.ProjectState{ID: "latest-1"}},
		Objects: newFakeObjects(), Relations: &fakeRelations{},
		Authz: nil, Schemas: reg,
	})
	_, err = svc.CreateObject(context.Background(), ownerActor(), "project-1", "branch-1", CreateObjectInput{
		ObjectType: "material", Payload: json.RawMessage(`{}`),
	})
	if !errors.Is(err, ErrStore) {
		t.Fatalf("nil engine: err = %v, want ErrStore (fail closed)", err)
	}
}

func rolePtr(r domain.ProjectRole) *domain.ProjectRole { return &r }

// fakeProfileResolver returns one scripted profile row — enough to drive
// resolveSchemaRef's profile arm without the real store.
type fakeProfileResolver struct {
	profile domain.ProjectSchemaProfile
	err     error
}

func (f *fakeProfileResolver) GetLatestProfile(ctx context.Context, projectID, schemaID string) (domain.ProjectSchemaProfile, error) {
	if f.err != nil {
		return domain.ProjectSchemaProfile{}, f.err
	}
	return f.profile, nil
}

// TestResolveSchemaRefRefusesNotYetLoadedProfile: a profile row can exist
// for this project while its schema is not yet in the runtime registry —
// the startup profile load is a background job, so this is a normal early
// state. Resolution must fail closed NAMING that state, never answer the
// bogus "governs type \"\"" verdict about a schema this registry has never
// seen (the old behavior read TypeConst off an unregistered ref).
func TestResolveSchemaRefRefusesNotYetLoadedProfile(t *testing.T) {
	svc := newTestService(t, memberProject(), newFakeObjects())
	svc.schemaProfiles = &fakeProfileResolver{profile: domain.ProjectSchemaProfile{
		ProjectID: "project-1",
		SchemaID:  "project:project-1:custom_material",
		Version:   "1",
	}}
	_, err := svc.CreateObject(context.Background(), ownerActor(), "project-1", "branch-1", CreateObjectInput{
		ObjectType: "material",
		SchemaRef:  "project:project-1:custom_material",
		Payload:    json.RawMessage(`{"name":"MOF-5"}`),
	})
	if err == nil {
		t.Fatal("CreateObject with a not-yet-loaded profile: err = nil, want ErrValidation (fail closed)")
	}
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	if !strings.Contains(err.Error(), "not yet loaded") {
		t.Errorf("err does not name the not-yet-loaded state: %v", err)
	}
	if strings.Contains(err.Error(), `governs type ""`) {
		t.Errorf("err carries the bogus empty-type verdict: %v", err)
	}
}
