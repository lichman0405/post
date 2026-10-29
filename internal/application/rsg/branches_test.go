package rsg

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

// newBranchListService wires only the ports ListBranches touches: the
// project read gate and the branch port. Everything else stays nil — the
// read must not need them, and a nil port would fail loudly if it did.
func newBranchListService(t *testing.T, projects *fakeProjects, branchRepo *fakeBranches) *Service {
	t.Helper()
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	return NewService(Deps{
		Projects: projects,
		Branches: branchRepo,
		Authz:    authz.NewMatrixEngine(),
		Schemas:  reg,
	})
}

func TestListBranchesOrdersMainFirstAndMapsFields(t *testing.T) {
	purpose := "CO2 isotherm path"
	created := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	head := "state-1"
	branchRepo := &fakeBranches{list: []domain.Branch{
		{ID: "branch-2", ProjectID: "proj-1", Name: "co2-path", Purpose: &purpose,
			Visibility: domain.BranchVisibilityPrivate, Lifecycle: domain.BranchLifecycleActive,
			BaseStateID: &head, CreatedAt: created},
		{ID: "branch-main", ProjectID: "proj-1", Name: "main",
			Visibility: domain.BranchVisibilityPublic, Lifecycle: domain.BranchLifecycleActive,
			BaseStateID: &head, CreatedAt: created.Add(-time.Hour)},
		{ID: "branch-3", ProjectID: "proj-1", Name: "abandoned",
			Visibility: domain.BranchVisibilityPublic, Lifecycle: domain.BranchLifecycleAborted,
			CreatedAt: created.Add(time.Hour)},
	}}
	projectsRepo := &fakeProjects{}
	svc := newBranchListService(t, projectsRepo, branchRepo)

	list, err := svc.ListBranches(context.Background(), projects.Reader{UserID: "u-1", Authenticated: true}, "proj-1")
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	if projectsRepo.getCalls != 1 {
		t.Fatalf("project read gate calls = %d, want 1", projectsRepo.getCalls)
	}
	if list.ProjectID != "proj-1" {
		t.Fatalf("project id = %q", list.ProjectID)
	}
	if len(list.Branches) != 3 {
		t.Fatalf("branches = %d, want 3", len(list.Branches))
	}
	// Order: main first, then the active path, then the closed one.
	got := []string{list.Branches[0].Name, list.Branches[1].Name, list.Branches[2].Name}
	want := []string{"main", "co2-path", "abandoned"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	if !list.Branches[0].IsMain {
		t.Fatalf("first branch is not marked main")
	}
	path := list.Branches[1]
	if path.Purpose != purpose || path.Visibility != "private" || path.HeadStateID != head {
		t.Fatalf("path branch = %#v", path)
	}
	if path.IsMain {
		t.Fatalf("a research path is marked main")
	}
	if list.Branches[2].Lifecycle != "aborted" || list.Branches[2].HeadStateID != "" {
		t.Fatalf("closed branch = %#v (a headless branch reports no fabricated head)", list.Branches[2])
	}
}

func TestListBranchesRunsProjectReadGateFirst(t *testing.T) {
	branchRepo := &fakeBranches{list: []domain.Branch{{ID: "branch-main", ProjectID: "proj-1", Name: "main"}}}
	projectsRepo := &fakeProjects{getErr: projects.ErrProjectNotFound}
	svc := newBranchListService(t, projectsRepo, branchRepo)

	_, err := svc.ListBranches(context.Background(), projects.Reader{}, "proj-1")
	if !errors.Is(err, projects.ErrProjectNotFound) {
		t.Fatalf("err = %v, want ErrProjectNotFound", err)
	}
	if branchRepo.listCalls != 0 {
		t.Fatalf("branch list read %d times after a denied project read", branchRepo.listCalls)
	}
}

func TestListBranchesStoreFailureFailsClosed(t *testing.T) {
	branchRepo := &fakeBranches{listErr: branches.ErrValidation}
	svc := newBranchListService(t, &fakeProjects{}, branchRepo)

	_, err := svc.ListBranches(context.Background(), projects.Reader{UserID: "u-1", Authenticated: true}, "proj-1")
	if err == nil {
		t.Fatal("a failing branch store must not answer an empty list")
	}
	if !errors.Is(err, branches.ErrValidation) {
		t.Fatalf("err = %v, want the branch store's validation error", err)
	}
}

func TestListBranchesRejectsEmptyProjectID(t *testing.T) {
	svc := newBranchListService(t, &fakeProjects{}, &fakeBranches{})
	_, err := svc.ListBranches(context.Background(), projects.Reader{}, "")
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
}
