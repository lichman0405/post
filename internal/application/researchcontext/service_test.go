// The flow's unit tests: the order of each command, what a refusal costs, and
// which facts a replay must NOT re-decide.
//
// The authorizer is the REAL matrix engine (internal/authz), not a stub: the
// question "who may confirm a draft" is answered by the permission matrix, and
// a fake engine would be a second copy of that answer, written by the same
// person as the code under test. Everything else is faked, because everything
// else is a database or another service and is covered by the e2e.
package researchcontext

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
)

const (
	testActor   = "11111111-2222-4333-8444-555555555555"
	testOther   = "99999999-8888-4777-8666-555555555555"
	testSearch  = "22222222-3333-4444-8555-666666666666"
	testDraft   = "33333333-4444-4555-8666-777777777777"
	testProject = "44444444-5555-4666-8777-888888888888"
	testBranch  = "55555555-6666-4777-8888-999999999999"
	testState   = "66666666-7777-4888-8999-000000000000"
	testCommit  = "77777777-8888-4999-8000-111111111111"
	testObject  = "88888888-9999-4000-8111-222222222222"
	testVersion = "99999999-0000-4111-8222-333333333333"
)

const (
	refA = "asset:AST-0001@2"
	refB = "knowledge:KNW-0007@1"
)

// ---------------------------------------------------------------------------
// Fakes

type fakeSearches struct {
	rec SearchRecord
	err error
	got string
}

func (f *fakeSearches) GetSearchRecord(_ context.Context, searchID string) (SearchRecord, error) {
	f.got = searchID
	if f.err != nil {
		return SearchRecord{}, f.err
	}
	return f.rec, nil
}

// fakeDrafts is an in-memory DraftStore: the two unique keys are enforced the
// way 00134 enforces them, so a test can drive the races the real store
// resolves.
type fakeDrafts struct {
	rows       []Draft
	seq        int
	err        error
	confirmErr error

	inserted  []Draft
	confirmed []ConfirmWrite
}

func (f *fakeDrafts) LookupBySearch(_ context.Context, searchID string) (*Draft, error) {
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.rows {
		if f.rows[i].SearchID == searchID {
			row := f.rows[i]
			return &row, nil
		}
	}
	return nil, nil
}

func (f *fakeDrafts) LookupByStartKey(_ context.Context, actorID, key string) (*Draft, error) {
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.rows {
		if f.rows[i].CreatedBy == actorID && f.rows[i].IdempotencyKey == key {
			row := f.rows[i]
			return &row, nil
		}
	}
	return nil, nil
}

func (f *fakeDrafts) Get(_ context.Context, draftID string) (Draft, error) {
	if f.err != nil {
		return Draft{}, f.err
	}
	for _, row := range f.rows {
		if row.ID == draftID {
			return row, nil
		}
	}
	return Draft{}, ErrDraftNotFound
}

func (f *fakeDrafts) Insert(_ context.Context, d Draft) (Draft, error) {
	if f.err != nil {
		return Draft{}, f.err
	}
	for _, row := range f.rows {
		if row.SearchID == d.SearchID {
			return Draft{}, ErrAlreadyStarted
		}
		if row.CreatedBy == d.CreatedBy && row.IdempotencyKey == d.IdempotencyKey {
			return Draft{}, ErrIdempotencyConflict
		}
	}
	f.seq++
	d.ID = testDraft
	// 00134's DEFAULT 'draft': the store's insert does not write the status,
	// the column supplies it. A fake that left it empty would let the
	// service's own reads disagree with the production row.
	if d.Status == "" {
		d.Status = DraftStatusDraft
	}
	f.rows = append(f.rows, d)
	f.inserted = append(f.inserted, d)
	return d, nil
}

func (f *fakeDrafts) Confirm(_ context.Context, in ConfirmWrite) (Draft, error) {
	if f.confirmErr != nil {
		return Draft{}, f.confirmErr
	}
	if f.err != nil {
		return Draft{}, f.err
	}
	for i := range f.rows {
		if f.rows[i].ID != in.DraftID {
			continue
		}
		if f.rows[i].Status != DraftStatusDraft {
			return Draft{}, ErrNotConfirmable
		}
		for j := range f.rows {
			if j != i && f.rows[j].ConfirmIdempotencyKey != nil &&
				f.rows[j].ConfirmedBy != nil && *f.rows[j].ConfirmedBy == in.ActorID &&
				*f.rows[j].ConfirmIdempotencyKey == in.Key {
				return Draft{}, ErrIdempotencyConflict
			}
		}
		f.rows[i].Status = DraftStatusConfirmed
		key := in.Key
		by := in.ActorID
		f.rows[i].ConfirmIdempotencyKey = &key
		f.rows[i].ConfirmedBy = &by
		f.rows[i].InitialBranchID = &in.BranchID
		f.rows[i].InitialStateID = &in.StateID
		f.rows[i].InitialCommitID = &in.CommitID
		f.rows[i].QuestionObjectID = &in.ObjectID
		f.rows[i].QuestionVersionID = &in.VersionID
		f.confirmed = append(f.confirmed, in)
		return f.rows[i], nil
	}
	return Draft{}, ErrNotConfirmable
}

type fakeProjects struct {
	project  domain.Project
	memberOf map[string]domain.ProjectRole
	memberEr map[string]error
	createEr error

	created []projects.CreateProjectInput
	creates int
}

func (f *fakeProjects) Create(_ context.Context, actor domain.User, in projects.CreateProjectInput) (domain.Project, domain.ProjectMembership, error) {
	f.creates++
	f.created = append(f.created, in)
	if f.createEr != nil {
		return domain.Project{}, domain.ProjectMembership{}, f.createEr
	}
	p := f.project
	p.Slug, p.Name, p.Purpose, p.Visibility = in.Slug, in.Name, in.Purpose, in.Visibility
	if p.ID == "" {
		p.ID = testProject
	}
	return p, domain.ProjectMembership{ProjectID: p.ID, UserID: actor.ID, Role: domain.ProjectRoleOwner}, nil
}

func (f *fakeProjects) GetMembership(_ context.Context, actor domain.User, projectID string) (domain.ProjectMembership, error) {
	if err := f.memberEr[actor.ID]; err != nil {
		return domain.ProjectMembership{}, err
	}
	role, ok := f.memberOf[actor.ID]
	if !ok {
		return domain.ProjectMembership{}, projects.ErrMemberNotFound
	}
	return domain.ProjectMembership{ProjectID: projectID, UserID: actor.ID, Role: role}, nil
}

func (f *fakeProjects) Get(_ context.Context, _ projects.Reader, projectID string) (domain.Project, error) {
	p := f.project
	if p.ID == "" {
		p.ID = projectID
	}
	return p, nil
}

type fakeStates struct {
	branchErr error
	objectErr error

	branches []rsg.CreateBranchInput
	objects  []rsg.CreateObjectInput
	branchOf string
}

func (f *fakeStates) CreateBranch(_ context.Context, _ domain.User, _ string, in rsg.CreateBranchInput) (domain.Branch, error) {
	f.branches = append(f.branches, in)
	if f.branchErr != nil {
		return domain.Branch{}, f.branchErr
	}
	return domain.Branch{ID: testBranch, ProjectID: testProject, Name: in.Name, Purpose: in.Purpose}, nil
}

func (f *fakeStates) CreateObject(_ context.Context, _ domain.User, _, branchID string, in rsg.CreateObjectInput) (rsg.ObjectResult, error) {
	f.objects = append(f.objects, in)
	f.branchOf = branchID
	if f.objectErr != nil {
		return rsg.ObjectResult{}, f.objectErr
	}
	return rsg.ObjectResult{
		Object:  domain.ScientificObject{ID: testObject, ObjectType: in.ObjectType},
		Version: domain.ScientificObjectVersion{ID: testVersion, ObjectID: testObject, StateID: testState, VersionNo: 1},
	}, nil
}

type fakeCommits struct {
	err   error
	calls int
	got   [2]string
}

func (f *fakeCommits) CommitForState(_ context.Context, branchID, stateID string) (domain.StateCommit, error) {
	f.calls++
	f.got = [2]string{branchID, stateID}
	if f.err != nil {
		return domain.StateCommit{}, f.err
	}
	return domain.StateCommit{ID: testCommit, BranchID: branchID, ResultStateID: stateID}, nil
}

// ---------------------------------------------------------------------------
// Fixtures

func newServiceForTest(searches *fakeSearches, drafts *fakeDrafts, proj *fakeProjects, states *fakeStates, commits *fakeCommits) *Service {
	if searches == nil {
		searches = &fakeSearches{rec: SearchRecord{ID: testSearch, ActorID: testActor, SelectedRefs: []string{refA, refB}}}
	}
	if drafts == nil {
		drafts = &fakeDrafts{}
	}
	if proj == nil {
		proj = &fakeProjects{project: domain.Project{ID: testProject, Visibility: domain.VisibilityPrivate},
			memberOf: map[string]domain.ProjectRole{testActor: domain.ProjectRoleMaintainer}}
	}
	if states == nil {
		states = &fakeStates{}
	}
	if commits == nil {
		commits = &fakeCommits{}
	}
	return New(Deps{
		SearchRecords: searches, DraftStore: drafts, Projects: proj,
		StateWriter: states, CommitReader: commits, Authz: authz.NewMatrixEngine(),
	})
}

func startInput() StartInput {
	return StartInput{
		SearchID:         testSearch,
		IdempotencyKey:   "start-key-0001",
		Name:             "MOF uptake",
		Slug:             "mof-uptake",
		ResearchQuestion: "Which Mg-MOF-76 samples reproduce the reported CO2 uptake?",
		ReferencedRefs:   []string{refA},
		CandidateRefs:    []string{refB},
	}
}

func actor() domain.User { return domain.User{ID: testActor} }

func mustStart(t *testing.T, svc *Service, in StartInput) StartResult {
	t.Helper()
	res, err := svc.Start(context.Background(), actor(), in)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return res
}

// ---------------------------------------------------------------------------
// Start

// TestStartWritesAProjectAndADraftAndNoState: the acceptance rule one layer
// down — the flow has no way to write state, and the objects that would carry
// it are never touched.
func TestStartWritesAProjectAndADraftAndNoState(t *testing.T) {
	drafts := &fakeDrafts{}
	proj := &fakeProjects{project: domain.Project{ID: testProject}}
	states := &fakeStates{}
	svc := newServiceForTest(nil, drafts, proj, states, nil)

	res := mustStart(t, svc, startInput())

	if res.Replayed {
		t.Error("a first start answered replayed=true")
	}
	if proj.creates != 1 {
		t.Fatalf("project creates = %d, want 1", proj.creates)
	}
	if len(drafts.inserted) != 1 {
		t.Fatalf("drafts inserted = %d, want 1", len(drafts.inserted))
	}
	if len(states.branches) != 0 || len(states.objects) != 0 {
		t.Fatalf("start wrote RSG state: %d branches, %d objects", len(states.branches), len(states.objects))
	}
	if res.Draft.ProjectID != testProject {
		t.Errorf("draft project = %q, want the created project", res.Draft.ProjectID)
	}
	if res.Draft.Status != DraftStatusDraft {
		t.Errorf("status = %q, want draft", res.Draft.Status)
	}
	if res.Draft.CreatedBy != testActor {
		t.Errorf("created_by = %q, want the actor", res.Draft.CreatedBy)
	}
}

// TestStartRefusesAnUngroundedRefBeforeItCostsAnything: the ref check runs
// before the project is created, so a caller's mistake costs no project — and
// the refusal names the ref.
func TestStartRefusesAnUngroundedRefBeforeItCostsAnything(t *testing.T) {
	proj := &fakeProjects{project: domain.Project{ID: testProject}}
	svc := newServiceForTest(nil, &fakeDrafts{}, proj, &fakeStates{}, nil)

	in := startInput()
	in.ReferencedRefs = []string{refA, "asset:AST-9999@1"}
	_, err := svc.Start(context.Background(), actor(), in)

	if !errors.Is(err, ErrUngroundedRef) {
		t.Fatalf("err = %v, want ErrUngroundedRef", err)
	}
	var ungrounded *UngroundedRefError
	if !errors.As(err, &ungrounded) || ungrounded.Ref != "asset:AST-9999@1" {
		t.Errorf("err = %v, want the offending ref named", err)
	}
	if proj.creates != 0 {
		t.Errorf("project creates = %d, want 0: an ungrounded ref must cost nothing", proj.creates)
	}
}

// TestStartRefusesASearchTheCallerDidNotRun: another actor's record and an
// unknown id produce the SAME error, so neither can be told from the other.
func TestStartRefusesASearchTheCallerDidNotRun(t *testing.T) {
	other := &fakeSearches{rec: SearchRecord{ID: testSearch, ActorID: testOther, SelectedRefs: []string{refA}}}
	svc := newServiceForTest(other, &fakeDrafts{}, nil, nil, nil)
	if _, err := svc.Start(context.Background(), actor(), startInput()); !errors.Is(err, ErrSearchNotOwned) {
		t.Fatalf("err = %v, want ErrSearchNotOwned", err)
	}

	missing := &fakeSearches{err: ErrSearchNotFound}
	svc = newServiceForTest(missing, &fakeDrafts{}, nil, nil, nil)
	if _, err := svc.Start(context.Background(), actor(), startInput()); !errors.Is(err, ErrSearchNotFound) {
		t.Fatalf("err = %v, want ErrSearchNotFound", err)
	}
}

// TestStartReplaysTheSearchesOwnDraft: the same search and the same key answer
// the first call's draft without creating a second project.
func TestStartReplaysTheSearchesOwnDraft(t *testing.T) {
	drafts := &fakeDrafts{}
	proj := &fakeProjects{project: domain.Project{ID: testProject}}
	svc := newServiceForTest(nil, drafts, proj, &fakeStates{}, nil)

	first := mustStart(t, svc, startInput())
	second := mustStart(t, svc, startInput())

	if !second.Replayed {
		t.Error("the second call did not answer as a replay")
	}
	if second.Draft.ID != first.Draft.ID {
		t.Errorf("replay draft = %q, want the first call's %q", second.Draft.ID, first.Draft.ID)
	}
	if proj.creates != 1 {
		t.Errorf("project creates = %d, want 1: a replay creates nothing", proj.creates)
	}
	if len(drafts.inserted) != 1 {
		t.Errorf("drafts inserted = %d, want 1", len(drafts.inserted))
	}
}

// TestStartRefusesADifferentKeyForTheSameSearch: answering with the existing
// draft would hand the caller a project it did not ask for under a key it did
// not use — the contract's 409.
func TestStartRefusesADifferentKeyForTheSameSearch(t *testing.T) {
	drafts := &fakeDrafts{}
	proj := &fakeProjects{project: domain.Project{ID: testProject}}
	svc := newServiceForTest(nil, drafts, proj, &fakeStates{}, nil)

	mustStart(t, svc, startInput())
	in := startInput()
	in.IdempotencyKey = "start-key-0002"
	if _, err := svc.Start(context.Background(), actor(), in); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("err = %v, want ErrAlreadyStarted", err)
	}
	if proj.creates != 1 {
		t.Errorf("project creates = %d, want 1", proj.creates)
	}
}

// TestStartRefusesAKeyThatNamesAnotherDraft: one key names one creation.
func TestStartRefusesAKeyThatNamesAnotherDraft(t *testing.T) {
	drafts := &fakeDrafts{}
	proj := &fakeProjects{project: domain.Project{ID: testProject}}
	svc := newServiceForTest(nil, drafts, proj, &fakeStates{}, nil)

	mustStart(t, svc, startInput())

	// A second search, the same key.
	searches := &fakeSearches{rec: SearchRecord{ID: "other-search", ActorID: testActor, SelectedRefs: []string{refA}}}
	svc = New(Deps{
		SearchRecords: searches, DraftStore: drafts, Projects: proj,
		StateWriter: &fakeStates{}, CommitReader: &fakeCommits{}, Authz: authz.NewMatrixEngine(),
	})
	in := startInput()
	in.SearchID = "other-search"
	if _, err := svc.Start(context.Background(), actor(), in); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("err = %v, want ErrIdempotencyConflict", err)
	}
	if proj.creates != 1 {
		t.Errorf("project creates = %d, want 1", proj.creates)
	}
}

// TestStartDefaultsVisibilityToPrivateAndDerivesPurpose: the contract's
// visibility is optional and an absent one must not widen anything; the
// project surface requires a purpose the start route's contract does not
// carry, and the question is the honest one.
func TestStartDefaultsVisibilityToPrivateAndDerivesPurpose(t *testing.T) {
	proj := &fakeProjects{project: domain.Project{ID: testProject}}
	svc := newServiceForTest(nil, &fakeDrafts{}, proj, &fakeStates{}, nil)
	mustStart(t, svc, startInput())

	if got := proj.created[0].Visibility; got != domain.VisibilityPrivate {
		t.Errorf("visibility = %q, want private (nothing becomes public by omission)", got)
	}
	if got := proj.created[0].Purpose; got != startInput().ResearchQuestion {
		t.Errorf("purpose = %q, want the research question", got)
	}

	proj2 := &fakeProjects{project: domain.Project{ID: testProject}}
	svc = newServiceForTest(nil, &fakeDrafts{}, proj2, &fakeStates{}, nil)
	in := startInput()
	in.Visibility = domain.VisibilityPublic
	mustStart(t, svc, in)
	if got := proj2.created[0].Visibility; got != domain.VisibilityPublic {
		t.Errorf("visibility = %q, want the requested public", got)
	}
}

// TestStartRefusesTheRequestsItCannotActOn: the shape checks, each of which
// costs no row.
func TestStartRefusesTheRequestsItCannotActOn(t *testing.T) {
	cases := map[string]func(*StartInput){
		"no actor":       func(in *StartInput) {},
		"no search":      func(in *StartInput) { in.SearchID = "" },
		"short key":      func(in *StartInput) { in.IdempotencyKey = "short" },
		"no name":        func(in *StartInput) { in.Name = "  " },
		"no slug":        func(in *StartInput) { in.Slug = "" },
		"tiny question":  func(in *StartInput) { in.ResearchQuestion = "co" },
		"bad visibility": func(in *StartInput) { in.Visibility = "unlisted" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			proj := &fakeProjects{project: domain.Project{ID: testProject}}
			svc := newServiceForTest(nil, &fakeDrafts{}, proj, &fakeStates{}, nil)
			in := startInput()
			mutate(&in)
			who := actor()
			if name == "no actor" {
				who = domain.User{}
			}
			if _, err := svc.Start(context.Background(), who, in); !errors.Is(err, ErrValidation) {
				t.Fatalf("err = %v, want ErrValidation", err)
			}
			if proj.creates != 0 {
				t.Errorf("project creates = %d, want 0", proj.creates)
			}
		})
	}
}

// TestStartPassesTheProjectSurfacesOwnRefusalsThrough: a taken slug is the
// project surface's outcome and must stay recognizable as such — the transport
// answers its code, not a generic 503.
func TestStartPassesTheProjectSurfacesOwnRefusalsThrough(t *testing.T) {
	proj := &fakeProjects{createEr: projects.ErrSlugTaken}
	svc := newServiceForTest(nil, &fakeDrafts{}, proj, &fakeStates{}, nil)
	if _, err := svc.Start(context.Background(), actor(), startInput()); !errors.Is(err, projects.ErrSlugTaken) {
		t.Fatalf("err = %v, want the project surface's ErrSlugTaken", err)
	}
}

// ---------------------------------------------------------------------------
// Confirm

func confirmedFixture(t *testing.T, role domain.ProjectRole) (*Service, *fakeDrafts, *fakeStates, *fakeCommits) {
	t.Helper()
	drafts := &fakeDrafts{}
	proj := &fakeProjects{
		project:  domain.Project{ID: testProject, Visibility: domain.VisibilityPublic},
		memberOf: map[string]domain.ProjectRole{testActor: role},
	}
	states := &fakeStates{}
	commits := &fakeCommits{}
	svc := newServiceForTest(nil, drafts, proj, states, commits)
	mustStart(t, svc, startInput())
	return svc, drafts, states, commits
}

// TestConfirmFormsTheInitialStateAndRecordsTheTransition: the branch is main,
// the object is the research question, and every id the draft records is the
// one the write path produced — the trail the acceptance asks for.
func TestConfirmFormsTheInitialStateAndRecordsTheTransition(t *testing.T) {
	svc, drafts, states, commits := confirmedFixture(t, domain.ProjectRoleMaintainer)

	res, err := svc.Confirm(context.Background(), actor(), ConfirmInput{DraftID: testDraft, IdempotencyKey: "confirm-key-01"})
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	if len(states.branches) != 1 {
		t.Fatalf("branches created = %d, want 1", len(states.branches))
	}
	branch := states.branches[0]
	if branch.Name != domain.MainBranchName {
		t.Errorf("branch name = %q, want %q", branch.Name, domain.MainBranchName)
	}
	if branch.BaseRef != "" {
		t.Errorf("BaseRef = %q, want empty: the project has no state, and genesis is what the transition path makes for it", branch.BaseRef)
	}
	if branch.Visibility != domain.BranchVisibility(domain.VisibilityPublic) {
		t.Errorf("branch visibility = %q, want the project's public", branch.Visibility)
	}

	if len(states.objects) != 1 {
		t.Fatalf("objects created = %d, want 1", len(states.objects))
	}
	obj := states.objects[0]
	if obj.ObjectType != "research_question" {
		t.Errorf("object type = %q, want research_question", obj.ObjectType)
	}
	if states.branchOf != testBranch {
		t.Errorf("the object was written to branch %q, want the branch the confirmation just created", states.branchOf)
	}
	var payload map[string]string
	if err := json.Unmarshal(obj.Payload, &payload); err != nil {
		t.Fatalf("the research question payload is not JSON: %s", obj.Payload)
	}
	if payload["statement"] != startInput().ResearchQuestion {
		t.Errorf("statement = %q, want the draft's question", payload["statement"])
	}
	if payload["question_state"] != "open" {
		t.Errorf("question_state = %q, want open", payload["question_state"])
	}

	if commits.calls != 1 || commits.got[0] != testBranch || commits.got[1] != testState {
		t.Errorf("commit read = %d %v, want one read of (branch, state)", commits.calls, commits.got)
	}
	if len(drafts.confirmed) != 1 {
		t.Fatalf("confirm writes = %d, want 1", len(drafts.confirmed))
	}
	w := drafts.confirmed[0]
	if w.BranchID != testBranch || w.StateID != testState || w.CommitID != testCommit ||
		w.ObjectID != testObject || w.VersionID != testVersion {
		t.Errorf("recorded confirmation = %+v, want the ids the write path produced", w)
	}
	if res.StateCommitID != testCommit || res.BranchID != testBranch {
		t.Errorf("result = %+v, want the recorded ids", res)
	}
	if res.Draft.Status != DraftStatusConfirmed {
		t.Errorf("draft status = %q, want confirmed", res.Draft.Status)
	}
	if res.Replayed {
		t.Error("a first confirmation answered replayed=true")
	}
}

// TestConfirmIsWritableOnceAndReplayableByItsKey: the same key answers the
// recorded confirmation and writes no second state; a different key is the
// contract's 409.
func TestConfirmIsWritableOnceAndReplayableByItsKey(t *testing.T) {
	svc, _, states, _ := confirmedFixture(t, domain.ProjectRoleMaintainer)
	in := ConfirmInput{DraftID: testDraft, IdempotencyKey: "confirm-key-01"}

	first, err := svc.Confirm(context.Background(), actor(), in)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	second, err := svc.Confirm(context.Background(), actor(), in)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !second.Replayed {
		t.Error("the replay did not answer as one")
	}
	if second.StateCommitID != first.StateCommitID || second.BranchID != first.BranchID {
		t.Errorf("replay answered %+v, want the first call's ids", second)
	}
	if len(states.branches) != 1 || len(states.objects) != 1 {
		t.Errorf("a replay wrote state again: %d branches, %d objects", len(states.branches), len(states.objects))
	}

	if _, err := svc.Confirm(context.Background(), actor(), ConfirmInput{DraftID: testDraft, IdempotencyKey: "confirm-key-02"}); !errors.Is(err, ErrNotConfirmable) {
		t.Fatalf("err = %v, want ErrNotConfirmable", err)
	}
	if len(states.branches) != 1 {
		t.Errorf("a refused confirmation wrote a branch")
	}
}

// TestConfirmNeedsMaintainerAndAbove is the permission rule the acceptance
// names: owner and maintainer may, everyone below may not — and a caller who
// may not, a caller who is not a member, and a draft that does not exist all
// get the SAME answer.
func TestConfirmNeedsMaintainerAndAbove(t *testing.T) {
	allowed := []domain.ProjectRole{domain.ProjectRoleOwner, domain.ProjectRoleMaintainer}
	denied := []domain.ProjectRole{"", domain.ProjectRoleContributor, domain.ProjectRoleViewer}

	for _, role := range allowed {
		t.Run("allow "+string(role), func(t *testing.T) {
			svc, _, _, _ := confirmedFixture(t, role)
			if _, err := svc.Confirm(context.Background(), actor(), ConfirmInput{DraftID: testDraft, IdempotencyKey: "confirm-key-01"}); err != nil {
				t.Fatalf("role %q was refused: %v", role, err)
			}
		})
	}
	for _, role := range denied {
		t.Run("deny "+string(role), func(t *testing.T) {
			// memberOf is empty for the "" case: the actor is then a
			// non-member, which must be denied the same way.
			memberOf := map[string]domain.ProjectRole{}
			if role != "" {
				memberOf[testActor] = role
			}
			svc, _, states, _ := confirmedFixtureWith(t, memberOf)
			_, err := svc.Confirm(context.Background(), actor(), ConfirmInput{DraftID: testDraft, IdempotencyKey: "confirm-key-01"})
			if !errors.Is(err, ErrDraftNotFound) {
				t.Fatalf("role %q err = %v, want ErrDraftNotFound", role, err)
			}
			if len(states.branches) != 0 {
				t.Errorf("role %q wrote a branch", role)
			}
		})
	}

	// A draft that does not exist answers exactly what a denied caller is
	// answered: nothing about a draft's existence is disclosed.
	svc, _, _, _ := confirmedFixture(t, domain.ProjectRoleOwner)
	missing, err := svc.Confirm(context.Background(), actor(), ConfirmInput{DraftID: "not-a-draft", IdempotencyKey: "confirm-key-01"})
	if !errors.Is(err, ErrDraftNotFound) {
		t.Fatalf("missing draft err = %v, want ErrDraftNotFound", err)
	}
	deniedSvc, _, _, _ := confirmedFixtureWith(t, map[string]domain.ProjectRole{})
	deniedRes, derr := deniedSvc.Confirm(context.Background(), actor(), ConfirmInput{DraftID: testDraft, IdempotencyKey: "confirm-key-01"})
	if !errors.Is(derr, ErrDraftNotFound) {
		t.Fatalf("non-member err = %v, want ErrDraftNotFound", derr)
	}
	if missing.Draft.ID != "" || deniedRes.Draft.ID != "" {
		t.Error("a refused confirmation returned a draft")
	}
}

// TestConfirmRefusesTheRequestsItCannotActOn: the shape checks.
func TestConfirmRefusesTheRequestsItCannotActOn(t *testing.T) {
	cases := map[string]ConfirmInput{
		"no draft":  {DraftID: "", IdempotencyKey: "confirm-key-01"},
		"short key": {DraftID: testDraft, IdempotencyKey: "short"},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			svc, _, states, _ := confirmedFixture(t, domain.ProjectRoleOwner)
			if _, err := svc.Confirm(context.Background(), actor(), in); !errors.Is(err, ErrValidation) {
				t.Fatalf("err = %v, want ErrValidation", err)
			}
			if len(states.branches) != 0 {
				t.Errorf("a refused confirmation wrote a branch")
			}
		})
	}
}

// TestConfirmReportsAStateWriteFailureAsOne: a real failure of the RSG write
// path is the store's, not a permission answer — unless the draft was
// confirmed meanwhile, in which case the caller gets the same answer a
// confirmed draft gets.
func TestConfirmReportsAStateWriteFailureAsOne(t *testing.T) {
	svc, _, _, _ := confirmedFixture(t, domain.ProjectRoleOwner)
	// Break the state writer under the service.
	states := &fakeStates{branchErr: errors.New("postgres is down")}
	svc.states = states
	if _, err := svc.Confirm(context.Background(), actor(), ConfirmInput{DraftID: testDraft, IdempotencyKey: "confirm-key-01"}); !errors.Is(err, ErrStore) {
		t.Fatalf("err = %v, want ErrStore", err)
	}
}

// confirmedFixtureWith is confirmedFixture with an explicit membership map, so
// the denials can be driven.
func confirmedFixtureWith(t *testing.T, memberOf map[string]domain.ProjectRole) (*Service, *fakeDrafts, *fakeStates, *fakeCommits) {
	t.Helper()
	drafts := &fakeDrafts{}
	proj := &fakeProjects{
		project:  domain.Project{ID: testProject, Visibility: domain.VisibilityPrivate},
		memberOf: memberOf,
	}
	states := &fakeStates{}
	commits := &fakeCommits{}
	svc := newServiceForTest(nil, drafts, proj, states, commits)
	mustStart(t, svc, startInput())
	return svc, drafts, states, commits
}
