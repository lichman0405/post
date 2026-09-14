package rsg

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
)

// Overview unit tests (T0212): the summary aggregation rules over fakes —
// the question/finding caps and the accepted-first ordering, the attention
// derivation from the graph's own state signals, the branch split, the
// current-main assembly, the empty flag, the visibility gate ordering and
// fail-closed store errors. The e2e over real PostgreSQL lives in
// tests/integration/overview_page_test.go.

// overviewFakeProfiles is a ProfilePort over a canned handle map.
type overviewFakeProfiles struct {
	handles map[string]string
	err     error
}

func (f *overviewFakeProfiles) GetByUserID(_ context.Context, userID string) (domain.Profile, error) {
	if f.err != nil {
		return domain.Profile{}, f.err
	}
	if h, ok := f.handles[userID]; ok {
		return domain.Profile{User: domain.User{ID: userID, Handle: h}}, nil
	}
	return domain.Profile{}, errors.New("no profile")
}

// overviewProject builds a canned project with the fields the summary
// carries.
func overviewProject() domain.Project {
	return domain.Project{
		ID: qP, Slug: "rsg-project", Name: "RSG Project",
		Purpose: "Screen MOFs for CO2 capture", Visibility: domain.VisibilityPrivate,
		MainFrozen: true, ActivityStatus: "active",
	}
}

// overviewObjects builds the research slice the overview aggregates:
// three root questions, one sub-question, five findings with a spread of
// assessments, one contested finding, one unresolved question, and a
// hypothesis addressing a question.
func overviewObjects() ([]ObjectQueryRow, []RelationQueryRow) {
	objects := []ObjectQueryRow{
		outlineObj("q1", "research_question", "q1-v1", "Does X adsorb?", map[string]any{
			"statement": "Does the material adsorb CO2?", "question_state": "open",
		}),
		outlineObj("q2", "research_question", "q2-v1", "Is it stable?", map[string]any{
			"statement": "Is it stable under cycling?", "question_state": "partially_answered",
		}),
		outlineObj("q3", "research_question", "q3-v1", "Does it scale?", map[string]any{
			"statement": "Does synthesis scale?", "question_state": "unresolved",
		}),
		outlineObj("q4", "research_question", "q4-v1", "What pressure?", map[string]any{
			"statement": "At which pressure does uptake peak?", "question_state": "open",
			"parent_question_id": "q1",
		}),
		outlineObj("h1", "hypothesis", "h1-v1", "Uptake scales with pressure", map[string]any{
			"statement": "Uptake scales with pressure",
		}),
		outlineObj("f-accepted", "finding", "f-accepted-v1", "Uptake peaks at 30 bar", map[string]any{
			"statement": "Uptake peaks at 30 bar", "finding_type": "trend", "assessment": "accepted",
		}),
		outlineObj("f-prelim", "finding", "f-prelim-v1", "Capacity grows with pore size", map[string]any{
			"statement": "Capacity grows with pore size", "finding_type": "trend", "assessment": "preliminary",
		}),
		outlineObj("f-contested", "finding", "f-contested-v1", "Degrades in humid air", map[string]any{
			"statement": "Degrades in humid air", "finding_type": "trend", "assessment": "contested",
		}),
		outlineObj("f-unresolved", "finding", "f-unresolved-v1", "Mechanism is unclear", map[string]any{
			"statement": "Mechanism is unclear", "finding_type": "trend", "assessment": "unresolved",
		}),
		outlineObj("f-superseded", "finding", "f-superseded-v1", "Old claim", map[string]any{
			"statement": "An earlier claim", "finding_type": "trend", "assessment": "superseded",
		}),
		outlineObj("f-6", "finding", "f-6-v1", "Sixth finding", map[string]any{
			"statement": "Sixth", "finding_type": "trend", "assessment": "preliminary",
		}),
	}
	relations := []RelationQueryRow{
		outlineRel("r1", "addresses_question",
			queryEndpoint("h1-v1", "h1", "hypothesis", qP), queryEndpoint("q1-v1", "q1", "research_question", qP)),
		outlineRel("r2", "addresses_question",
			queryEndpoint("f-accepted-v1", "f-accepted", "finding", qP), queryEndpoint("q1-v1", "q1", "research_question", qP)),
	}
	return objects, relations
}

func overviewBranches() []domain.Branch {
	return []domain.Branch{
		{
			ID: "main-1", ProjectID: qP, Name: "main", Lifecycle: domain.BranchLifecycleActive,
			BaseStateID: strPtr("state-main"), CreatedAt: time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC),
		},
		{
			ID: "feat-1", ProjectID: qP, Name: "feature/screening",
			Purpose: strPtr("screen a new linker set"), Lifecycle: domain.BranchLifecycleActive,
			BaseStateID: strPtr("state-feat"), CreatedAt: time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC),
		},
		{
			ID: "merged-1", ProjectID: qP, Name: "feature/old", Lifecycle: domain.BranchLifecycleMerged,
			BaseStateID: strPtr("state-merged"), CreatedAt: time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC),
		},
		{
			ID: "aborted-1", ProjectID: qP, Name: "feature/dead", Lifecycle: domain.BranchLifecycleAborted,
			BaseStateID: strPtr("state-aborted"), CreatedAt: time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC),
		},
	}
}

func overviewServiceWith(objects []ObjectQueryRow, relations []RelationQueryRow, branchesList []domain.Branch, states *fakeStates) *Service {
	port := &queryFakePort{objects: objects, relations: relations}
	return &Service{
		projects: &queryFakeProjects{outcomes: map[string]error{}, project: overviewProject()},
		queries:  port,
		branches: &fakeBranches{list: branchesList},
		states:   states,
	}
}

func TestProjectOverviewAggregates(t *testing.T) {
	objects, relations := overviewObjects()
	states := &fakeStates{
		head: domain.ProjectState{
			ID: "state-main", ProjectID: qP, StateHash: "abc123",
			CreatedAt: time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC),
		},
		commits: []domain.StateCommit{
			{Message: "first question", Via: domain.ViaWeb, ActorID: "user-1", CreatedAt: time.Date(2026, 9, 1, 9, 1, 0, 0, time.UTC)},
			{Message: "recorded screening finding", Via: domain.ViaMCP, ActorID: "user-2", CreatedAt: time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)},
		},
	}
	svc := overviewServiceWith(objects, relations, overviewBranches(), states)
	ov, err := svc.ProjectOverview(context.Background(), projects.Reader{}, qP)
	if err != nil {
		t.Fatalf("ProjectOverview: %v", err)
	}

	// Project facts ride through.
	if ov.ProjectSlug != "rsg-project" || ov.Purpose != "Screen MOFs for CO2 capture" {
		t.Errorf("project facts = %q %q", ov.ProjectSlug, ov.Purpose)
	}
	if !ov.MainFrozen || ov.Visibility != "private" || ov.ActivityStatus != "active" {
		t.Errorf("header facts = frozen:%v vis:%q activity:%q", ov.MainFrozen, ov.Visibility, ov.ActivityStatus)
	}

	// Key questions: the tree's roots in display order (q1 with its child
	// nested), all three roots present, count 4 total.
	if ov.TotalQuestions != 4 {
		t.Errorf("TotalQuestions = %d, want 4", ov.TotalQuestions)
	}
	if len(ov.KeyQuestions) != 3 {
		t.Fatalf("key questions = %d, want 3", len(ov.KeyQuestions))
	}
	if ov.KeyQuestions[0].ObjectID != "q1" || len(ov.KeyQuestions[0].Children) != 1 || ov.KeyQuestions[0].Children[0].ObjectID != "q4" {
		t.Errorf("first root = %+v, want q1 with nested q4", ov.KeyQuestions[0])
	}

	// Key findings: accepted first, then preliminary, then contested/
	// unresolved, then superseded — capped at 5 with the total at 6.
	if ov.TotalFindings != 6 {
		t.Errorf("TotalFindings = %d, want 6", ov.TotalFindings)
	}
	if len(ov.KeyFindings) != 5 {
		t.Fatalf("key findings = %d, want 5 (cap)", len(ov.KeyFindings))
	}
	wantOrder := []string{"f-accepted", "f-prelim", "f-6", "f-contested", "f-unresolved"}
	for i, want := range wantOrder {
		if ov.KeyFindings[i].ObjectID != want {
			t.Errorf("key findings[%d] = %s, want %s", i, ov.KeyFindings[i].ObjectID, want)
		}
	}
	// The superseded finding sank below the cap — never on the key list.
	for _, f := range ov.KeyFindings {
		if f.ObjectID == "f-superseded" {
			t.Errorf("superseded finding on the key list")
		}
	}

	// Attention: the contested and unresolved findings, and the unresolved
	// question (walked from the tree) — findings first, sorted by title.
	wantAttention := []struct{ kind, objectID, signal string }{
		{"finding", "f-contested", "contested"},
		{"finding", "f-unresolved", "unresolved"},
		{"question", "q3", "unresolved"},
	}
	if ov.AttentionTotal != len(wantAttention) {
		t.Fatalf("attention total = %d, want %d", ov.AttentionTotal, len(wantAttention))
	}
	if len(ov.NeedsAttention) != len(wantAttention) {
		t.Fatalf("attention = %+v, want %d rows", ov.NeedsAttention, len(wantAttention))
	}
	for i, want := range wantAttention {
		got := ov.NeedsAttention[i]
		if got.Kind != want.kind || got.ObjectID != want.objectID || got.Signal != want.signal {
			t.Errorf("attention[%d] = %+v, want %+v", i, got, want)
		}
	}

	// Branches: the active non-main path, the closed counts; main kept out
	// of the paths list.
	if len(ov.Branches.Active) != 1 || ov.Branches.Active[0].Name != "feature/screening" ||
		ov.Branches.Active[0].Purpose != "screen a new linker set" || ov.Branches.Active[0].HeadStateID != "state-feat" {
		t.Errorf("active branches = %+v", ov.Branches.Active)
	}
	if ov.Branches.Merged != 1 || ov.Branches.Aborted != 1 {
		t.Errorf("closed counts = merged:%d aborted:%d, want 1/1", ov.Branches.Merged, ov.Branches.Aborted)
	}

	// Current main: the branch row's head id, the head state's hash, and
	// the NEWEST commit (the history's tail).
	if ov.CurrentMain.BranchID != "main-1" || ov.CurrentMain.HeadStateID != "state-main" || ov.CurrentMain.StateHash != "abc123" {
		t.Errorf("current main = %+v", ov.CurrentMain)
	}
	if !ov.CurrentMain.LatestCommit.Present || ov.CurrentMain.LatestCommit.Message != "recorded screening finding" ||
		ov.CurrentMain.LatestCommit.Via != string(domain.ViaMCP) {
		t.Errorf("latest commit = %+v", ov.CurrentMain.LatestCommit)
	}

	if ov.Empty {
		t.Errorf("Empty = true for a populated project")
	}
}

func TestProjectOverviewCapsQuestionsAndAttention(t *testing.T) {
	// Seven root questions, all unresolved: the key list caps at 5 and
	// the attention list caps at 5 with the total at 7.
	var objects []ObjectQueryRow
	for _, id := range []string{"qa", "qb", "qc", "qd", "qe", "qf", "qg"} {
		objects = append(objects, outlineObj(id, "research_question", id+"-v1", strings.ToUpper(id), map[string]any{
			"question_state": "unresolved",
		}))
	}
	svc := overviewServiceWith(objects, nil, nil, &fakeStates{})
	ov, err := svc.ProjectOverview(context.Background(), projects.Reader{}, qP)
	if err != nil {
		t.Fatalf("ProjectOverview: %v", err)
	}
	if len(ov.KeyQuestions) != 5 || ov.TotalQuestions != 7 {
		t.Errorf("questions = %d/%d, want 5/7", len(ov.KeyQuestions), ov.TotalQuestions)
	}
	if len(ov.NeedsAttention) != 5 || ov.AttentionTotal != 7 {
		t.Errorf("attention = %d/%d, want 5/7", len(ov.NeedsAttention), ov.AttentionTotal)
	}
}

func TestProjectOverviewEmptyProject(t *testing.T) {
	// No objects at all, and no branches: the empty state with no main.
	svc := overviewServiceWith(nil, nil, []domain.Branch{}, &fakeStates{})
	ov, err := svc.ProjectOverview(context.Background(), projects.Reader{}, qP)
	if err != nil {
		t.Fatalf("ProjectOverview: %v", err)
	}
	if !ov.Empty {
		t.Errorf("Empty = false for an empty project")
	}
	if ov.CurrentMain.BranchID != "" {
		t.Errorf("current main = %+v, want none", ov.CurrentMain)
	}
}

func TestProjectOverviewMainWithoutCommits(t *testing.T) {
	// Main exists with a head but no commits of its own: the commit slot
	// reports not-present, the summary still builds.
	branches := []domain.Branch{{
		ID: "main-1", ProjectID: qP, Name: "main", Lifecycle: domain.BranchLifecycleActive,
		BaseStateID: strPtr("state-main"),
	}}
	svc := overviewServiceWith(nil, nil, branches, &fakeStates{head: domain.ProjectState{ID: "state-main", ProjectID: qP}})
	ov, err := svc.ProjectOverview(context.Background(), projects.Reader{}, qP)
	if err != nil {
		t.Fatalf("ProjectOverview: %v", err)
	}
	if ov.CurrentMain.HeadStateID != "state-main" {
		t.Errorf("head = %q, want state-main", ov.CurrentMain.HeadStateID)
	}
	if ov.CurrentMain.LatestCommit.Present {
		t.Errorf("latest commit present for a branch with no commits")
	}
	if !ov.Empty {
		t.Errorf("Empty = false for a project with no objects")
	}
}

func TestProjectOverviewResolvesCommitActor(t *testing.T) {
	branches := []domain.Branch{{
		ID: "main-1", ProjectID: qP, Name: "main", Lifecycle: domain.BranchLifecycleActive,
		BaseStateID: strPtr("state-main"),
	}}
	states := &fakeStates{
		head:    domain.ProjectState{ID: "state-main", ProjectID: qP},
		commits: []domain.StateCommit{{Message: "m", ActorID: "user-1", Via: domain.ViaAPI}},
	}
	svc := overviewServiceWith(nil, nil, branches, states)
	svc.profiles = &overviewFakeProfiles{handles: map[string]string{"user-1": "alice"}}
	ov, err := svc.ProjectOverview(context.Background(), projects.Reader{}, qP)
	if err != nil {
		t.Fatalf("ProjectOverview: %v", err)
	}
	if ov.CurrentMain.LatestCommit.ActorID != "alice" {
		t.Errorf("actor = %q, want alice (the profile handle)", ov.CurrentMain.LatestCommit.ActorID)
	}

	// An unresolvable actor stays the raw id — never an error.
	svc.profiles = &overviewFakeProfiles{handles: map[string]string{}}
	ov, err = svc.ProjectOverview(context.Background(), projects.Reader{}, qP)
	if err != nil {
		t.Fatalf("ProjectOverview with unresolvable actor: %v", err)
	}
	if ov.CurrentMain.LatestCommit.ActorID != "user-1" {
		t.Errorf("actor = %q, want the raw id user-1", ov.CurrentMain.LatestCommit.ActorID)
	}
}

func TestProjectOverviewVisibilityGateFirst(t *testing.T) {
	gate := &queryFakeProjects{outcomes: map[string]error{qP: projects.ErrProjectNotFound}}
	port := &queryFakePort{err: errors.New("store must not be reached")}
	branches := &fakeBranches{listErr: errors.New("branch store must not be reached")}
	svc := &Service{projects: gate, queries: port, branches: branches, states: &fakeStates{}}
	_, err := svc.ProjectOverview(context.Background(), projects.Reader{}, qP)
	if !errors.Is(err, projects.ErrProjectNotFound) {
		t.Fatalf("err = %v, want ErrProjectNotFound", err)
	}
	if port.objectCalls != 0 {
		t.Errorf("query port consulted %d times before the gate", port.objectCalls)
	}
	if len(gate.calls) != 1 || gate.calls[0] != qP {
		t.Errorf("gate calls = %v, want [%s]", gate.calls, qP)
	}
}

func TestProjectOverviewStoreFailureFailsClosed(t *testing.T) {
	// The branch list is the first store read after the outline; its
	// failure must abort, not produce a partial summary.
	svc := overviewServiceWith(nil, nil, nil, &fakeStates{})
	svc.branches = &fakeBranches{listErr: errors.New("branch store down")}
	_, err := svc.ProjectOverview(context.Background(), projects.Reader{}, qP)
	if err == nil || !strings.Contains(err.Error(), "branch store down") {
		t.Fatalf("err = %v, want the branch store failure", err)
	}

	// The main head fetch failing aborts too.
	states := &fakeStates{headErr: errors.New("state store down")}
	svc = overviewServiceWith(nil, nil, overviewBranches(), states)
	_, err = svc.ProjectOverview(context.Background(), projects.Reader{}, qP)
	if err == nil || !strings.Contains(err.Error(), "state store down") {
		t.Fatalf("err = %v, want the state store failure", err)
	}

	// And the commit list failing aborts.
	states = &fakeStates{head: domain.ProjectState{ID: "state-main"}, commitsErr: errors.New("commit store down")}
	svc = overviewServiceWith(nil, nil, overviewBranches(), states)
	_, err = svc.ProjectOverview(context.Background(), projects.Reader{}, qP)
	if err == nil || !strings.Contains(err.Error(), "commit store down") {
		t.Fatalf("err = %v, want the commit store failure", err)
	}
}
