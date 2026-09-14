package validation

import (
	"context"
	"errors"
	"testing"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// fakeSnapshotRepo is a SnapshotRepository over canned values.
type fakeSnapshotRepo struct {
	branchProject string
	branchErr     error

	headState domain.ProjectState
	headErr   error

	states    []domain.ProjectState
	commits   []domain.StateCommit
	objects   map[string][]domain.ScientificObjectVersion
	relations map[string][]domain.RelationVersion
}

func (f *fakeSnapshotRepo) BranchProject(context.Context, string) (string, error) {
	return f.branchProject, f.branchErr
}
func (f *fakeSnapshotRepo) GetBranchHead(context.Context, string) (domain.ProjectState, error) {
	return f.headState, f.headErr
}
func (f *fakeSnapshotRepo) ListStates(context.Context, string) ([]domain.ProjectState, error) {
	return f.states, nil
}
func (f *fakeSnapshotRepo) ListCommits(context.Context, string) ([]domain.StateCommit, error) {
	return f.commits, nil
}
func (f *fakeSnapshotRepo) ListStateObjectVersions(_ context.Context, stateID string) ([]domain.ScientificObjectVersion, error) {
	return f.objects[stateID], nil
}
func (f *fakeSnapshotRepo) ListStateRelationVersions(_ context.Context, stateID string) ([]domain.RelationVersion, error) {
	return f.relations[stateID], nil
}

func newTestService(t *testing.T, repo SnapshotRepository) *Service {
	t.Helper()
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	return NewService(repo, rsgvalidation.NewValidator(reg))
}

func TestValidateBranchRejectsBadInputs(t *testing.T) {
	svc := newTestService(t, &fakeSnapshotRepo{})
	ctx := context.Background()
	if _, err := svc.ValidateBranch(ctx, "", "b", rsgvalidation.GatePR); !errors.Is(err, ErrValidation) {
		t.Errorf("empty project: %v, want ErrValidation", err)
	}
	if _, err := svc.ValidateBranch(ctx, "p", "", rsgvalidation.GatePR); !errors.Is(err, ErrValidation) {
		t.Errorf("empty branch: %v, want ErrValidation", err)
	}
	if _, err := svc.ValidateBranch(ctx, "p", "b", "gold"); !errors.Is(err, ErrValidation) {
		t.Errorf("unknown gate: %v, want ErrValidation", err)
	}
}

func TestValidateBranchHidesForeignProject(t *testing.T) {
	svc := newTestService(t, &fakeSnapshotRepo{branchProject: "other-project"})
	if _, err := svc.ValidateBranch(context.Background(), "proj-1", "b", rsgvalidation.GatePR); !errors.Is(err, ErrBranchNotFound) {
		t.Fatalf("foreign branch: %v, want ErrBranchNotFound (never leak another project's branch)", err)
	}
}

func TestValidateBranchEmptyBranchWarns(t *testing.T) {
	svc := newTestService(t, &fakeSnapshotRepo{
		branchProject: "proj-1",
		headErr:       ErrStateNotFound,
		objects:       map[string][]domain.ScientificObjectVersion{},
		relations:     map[string][]domain.RelationVersion{},
	})
	report, err := svc.ValidateBranch(context.Background(), "proj-1", "b", rsgvalidation.GatePR)
	if err != nil {
		t.Fatalf("ValidateBranch: %v", err)
	}
	if report.Verdict != rsgvalidation.VerdictPassWithWarn {
		t.Fatalf("verdict = %s, want pass_with_warnings (empty branch)", report.Verdict)
	}
	if report.Gate != rsgvalidation.GatePR {
		t.Errorf("report gate = %s, want pr", report.Gate)
	}
}

func TestValidateBranchAssemblesSnapshot(t *testing.T) {
	branch := "br-1"
	now := timeNow()
	st1 := domain.ProjectState{ID: "st-1", BranchID: &branch, ParentStateID: strPtrOf("st-gen")}
	st2 := domain.ProjectState{ID: "st-2", BranchID: &branch, ParentStateID: strPtrOf("st-1")}
	payload := rsgPayload(`{"statement":"s","question_id":"q","hypothesis_type":"t","scope":{"d":1}}`)
	version := domain.ScientificObjectVersion{
		ID: "v-1", ObjectID: "object-1", VersionNo: 1, StateID: "st-1",
		SchemaID: schemareg.CanonicalNamespace + "hypothesis.schema.json", SchemaVersion: "1",
		Title: "H1", LifecycleState: domain.LifecycleActive, Payload: payload,
		IntegrityHash: rsgHash(payload), CreatedBy: "alice", CreatedAt: now,
	}
	c1 := domain.StateCommit{
		ID: "c-1", BranchID: branch, BaseStateID: strPtrOf("st-gen"), ResultStateID: "st-1",
		ActorID: "alice", OperationSummary: rsgOps(t, domain.StateOperation{
			Kind: domain.OperationObjectVersionCreated, EntityID: "object-1", VersionNo: 1,
		}),
	}
	repo := &fakeSnapshotRepo{
		branchProject: "proj-1",
		headState:     st2,
		states:        []domain.ProjectState{st1, st2},
		commits:       []domain.StateCommit{c1},
		// The same version is reported by two states: the assembler must
		// dedupe by row id, not double-validate it.
		objects: map[string][]domain.ScientificObjectVersion{
			"st-1": {version},
			"st-2": {version},
		},
		relations: map[string][]domain.RelationVersion{},
	}
	svc := newTestService(t, repo)
	report, err := svc.ValidateBranch(context.Background(), "proj-1", branch, rsgvalidation.GateMain)
	if err != nil {
		t.Fatalf("ValidateBranch: %v", err)
	}
	if report.Blocked() {
		t.Fatalf("verdict = %s, want pass; explanation:\n%s", report.Verdict, report.Explanation)
	}
	// The dedupe keeps exactly one result per object version per check.
	count := 0
	for _, res := range report.Results {
		if res.Check == rsgvalidation.CheckSchemaTyped && res.Subject != "" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("schema_typed results for the version = %d, want 1 (deduped)", count)
	}
}

func TestValidateBranchWithFacts(t *testing.T) {
	repo := &fakeSnapshotRepo{
		branchProject: "proj-1",
		headErr:       ErrStateNotFound,
		objects:       map[string][]domain.ScientificObjectVersion{},
		relations:     map[string][]domain.RelationVersion{},
	}
	svc := newTestService(t, repo)
	ctx := context.Background()

	// Without facts the release gate blocks.
	report, err := svc.ValidateBranch(ctx, "proj-1", "b", rsgvalidation.GateRelease)
	if err != nil {
		t.Fatalf("ValidateBranch: %v", err)
	}
	if !report.Blocked() {
		t.Fatalf("release without facts: verdict = %s, want blocked", report.Verdict)
	}

	// With complete facts over the empty branch, only branch_empty warns.
	report, err = svc.ValidateBranchWithFacts(ctx, "proj-1", "b", rsgvalidation.GateRelease,
		&rsgvalidation.ReleaseFacts{ReviewApproved: true, RightsSnapshot: true, FromMainBranch: true}, nil)
	if err != nil {
		t.Fatalf("ValidateBranchWithFacts: %v", err)
	}
	if report.Verdict != rsgvalidation.VerdictPassWithWarn {
		t.Fatalf("release with facts: verdict = %s, want pass_with_warnings", report.Verdict)
	}
}

func strPtrOf(s string) *string { return &s }
