package branches

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
)

// fakeRepo implements Repository for service-level tests: it records the
// validated params the service derives, so the tests prove the service
// computes the git ref and maps outcomes — not the adapter.
type fakeRepo struct {
	createCalled bool
	createIn     CreateBranchParams
	createOut    domain.Branch
	createErr    error

	getInProjectID, getInBranchID string
	getOut                        domain.Branch
	getErr                        error

	listErr error

	lifecycleProjectID, lifecycleBranchID string
	lifecycleTo                           domain.BranchLifecycle
	lifecycleOut                          domain.Branch
	lifecycleErr                          error

	headOut domain.ProjectState
	headErr error
}

func (f *fakeRepo) CreateBranch(_ context.Context, in CreateBranchParams) (domain.Branch, error) {
	f.createCalled = true
	f.createIn = in
	return f.createOut, f.createErr
}

func (f *fakeRepo) GetBranch(_ context.Context, projectID, branchID string) (domain.Branch, error) {
	f.getInProjectID, f.getInBranchID = projectID, branchID
	return f.getOut, f.getErr
}

func (f *fakeRepo) ListBranches(context.Context, string) ([]domain.Branch, error) {
	return nil, f.listErr
}

func (f *fakeRepo) SetBranchLifecycle(_ context.Context, projectID, branchID string, to domain.BranchLifecycle) (domain.Branch, error) {
	f.lifecycleProjectID, f.lifecycleBranchID, f.lifecycleTo = projectID, branchID, to
	return f.lifecycleOut, f.lifecycleErr
}

func (f *fakeRepo) GetBranchHead(context.Context, string) (domain.ProjectState, error) {
	return f.headOut, f.headErr
}

func validCreateParams() CreateBranchParams {
	return CreateBranchParams{
		ProjectID:   "11111111-1111-1111-1111-111111111111",
		Name:        "feature/screening",
		Visibility:  "",
		BaseStateID: "22222222-2222-2222-2222-222222222222",
		CreatedBy:   "33333333-3333-3333-3333-333333333333",
	}
}

func TestCreateDerivesGitRefAndForwards(t *testing.T) {
	ctx := context.Background()
	repo := &fakeRepo{createOut: domain.Branch{ID: "branch-1", Name: "feature/screening"}}
	svc := NewService(repo)
	branch, err := svc.Create(ctx, validCreateParams())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if branch.ID != "branch-1" {
		t.Fatalf("Create returned %+v, want the repo's value", branch)
	}
	if !repo.createCalled {
		t.Fatal("repo.CreateBranch not called")
	}
	// The git ref is derived by the service, 1:1 with the name — never
	// caller-supplied (T0303 publishes refs/heads/<name>).
	if repo.createIn.GitRef != "refs/heads/feature/screening" {
		t.Errorf("GitRef = %q, want refs/heads/feature/screening", repo.createIn.GitRef)
	}
}

func TestCreateRejectsInvalidInputs(t *testing.T) {
	ctx := context.Background()
	purpose := "screen MOFs"
	cases := []struct {
		name   string
		mutate func(*CreateBranchParams)
	}{
		{"empty project", func(p *CreateBranchParams) { p.ProjectID = "" }},
		{"empty name", func(p *CreateBranchParams) { p.Name = "" }},
		{"invalid name", func(p *CreateBranchParams) { p.Name = "has space" }},
		{"reserved name", func(p *CreateBranchParams) { p.Name = "HEAD" }},
		{"invalid visibility", func(p *CreateBranchParams) { p.Visibility = "internal" }},
		{"blank purpose", func(p *CreateBranchParams) { blank := "   "; p.Purpose = &blank }},
		{"oversized purpose", func(p *CreateBranchParams) {
			huge := strings.Repeat("a", 2001)
			p.Purpose = &huge
		}},
		{"empty base state", func(p *CreateBranchParams) { p.BaseStateID = "" }},
		{"empty creator", func(p *CreateBranchParams) { p.CreatedBy = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepo{}
			svc := NewService(repo)
			in := validCreateParams()
			in.Purpose = &purpose
			tc.mutate(&in)
			if _, err := svc.Create(ctx, in); !errors.Is(err, ErrValidation) {
				t.Fatalf("Create error = %v, want ErrValidation", err)
			}
			if repo.createCalled {
				t.Error("repo.CreateBranch called for invalid input")
			}
		})
	}
}

func TestCreateMapsStoreOutcomes(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		err    error
		wantIs error
	}{
		{"name taken passes through", ErrBranchNameTaken, ErrBranchNameTaken},
		{"base state missing passes through", ErrBaseStateNotFound, ErrBaseStateNotFound},
		{"public in private passes through", ErrPublicBranchInPrivateProject, ErrPublicBranchInPrivateProject},
		{"project missing passes through", projects.ErrProjectNotFound, projects.ErrProjectNotFound},
		{"adapter failure becomes ErrStore", errors.New("connection refused"), ErrStore},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepo{createErr: tc.err}
			svc := NewService(repo)
			if _, err := svc.Create(ctx, validCreateParams()); !errors.Is(err, tc.wantIs) {
				t.Fatalf("Create error = %v, want %v", err, tc.wantIs)
			}
		})
	}
}

func TestGetValidatesAndForwards(t *testing.T) {
	ctx := context.Background()
	repo := &fakeRepo{getOut: domain.Branch{ID: "b"}}
	svc := NewService(repo)
	for _, tc := range []struct {
		name      string
		projectID string
		branchID  string
	}{
		{"empty project", "", "b"},
		{"empty branch", "p", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.Get(ctx, tc.projectID, tc.branchID); !errors.Is(err, ErrValidation) {
				t.Fatalf("Get error = %v, want ErrValidation", err)
			}
		})
	}
	branch, err := svc.Get(ctx, "p", "b")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if branch.ID != "b" {
		t.Errorf("Get returned %+v, want the repo's value", branch)
	}
}

func TestMergeAndAbortForwardLifecycle(t *testing.T) {
	ctx := context.Background()
	repo := &fakeRepo{lifecycleOut: domain.Branch{ID: "b", Lifecycle: domain.BranchLifecycleMerged}}
	svc := NewService(repo)
	branch, err := svc.Merge(ctx, "p", "b")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if repo.lifecycleTo != domain.BranchLifecycleMerged || repo.lifecycleProjectID != "p" || repo.lifecycleBranchID != "b" {
		t.Errorf("Merge forwarded %q/%q → %q, want p/b → merged", repo.lifecycleProjectID, repo.lifecycleBranchID, repo.lifecycleTo)
	}
	if branch.Lifecycle != domain.BranchLifecycleMerged {
		t.Errorf("Merge returned lifecycle %q, want merged", branch.Lifecycle)
	}

	repo.lifecycleTo = ""
	repo.lifecycleOut = domain.Branch{ID: "b", Lifecycle: domain.BranchLifecycleAborted}
	if _, err := svc.Abort(ctx, "p", "b"); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if repo.lifecycleTo != domain.BranchLifecycleAborted {
		t.Errorf("Abort forwarded %q, want aborted", repo.lifecycleTo)
	}
}

func TestLifecycleMapsStoreOutcomes(t *testing.T) {
	ctx := context.Background()
	t.Run("not active passes through", func(t *testing.T) {
		closed := &NotActiveError{BranchID: "b", Lifecycle: "merged"}
		repo := &fakeRepo{lifecycleErr: closed}
		svc := NewService(repo)
		_, err := svc.Merge(ctx, "p", "b")
		var na *NotActiveError
		if !errors.As(err, &na) || na != closed {
			t.Fatalf("Merge error = %v, want the *NotActiveError unchanged", err)
		}
		if closed.Code() != CodeBranchNotActive {
			t.Errorf("NotActiveError code = %q, want %q", closed.Code(), CodeBranchNotActive)
		}
	})
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"main protected passes through", ErrMainProtected},
		{"not found passes through", ErrBranchNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepo{lifecycleErr: tc.err}
			svc := NewService(repo)
			if _, err := svc.Merge(ctx, "p", "b"); !errors.Is(err, tc.err) {
				t.Fatalf("Merge error = %v, want %v", err, tc.err)
			}
		})
	}
	t.Run("adapter failure becomes ErrStore", func(t *testing.T) {
		repo := &fakeRepo{lifecycleErr: errors.New("connection refused")}
		svc := NewService(repo)
		if _, err := svc.Merge(ctx, "p", "b"); !errors.Is(err, ErrStore) {
			t.Fatalf("Merge error = %v, want ErrStore", err)
		}
	})
}

func TestGetHeadValidatesAndMaps(t *testing.T) {
	ctx := context.Background()
	repo := &fakeRepo{}
	svc := NewService(repo)
	if _, err := svc.GetHead(ctx, ""); !errors.Is(err, ErrValidation) {
		t.Fatalf("GetHead(empty) error = %v, want ErrValidation", err)
	}
	repo.headErr = ErrStateNotFound
	if _, err := svc.GetHead(ctx, "b"); !errors.Is(err, ErrStateNotFound) {
		t.Fatalf("GetHead error = %v, want ErrStateNotFound", err)
	}
}
