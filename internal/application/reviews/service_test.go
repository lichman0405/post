package reviews

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
)

const (
	projID = "11111111-1111-1111-1111-111111111111"
	prNo   = int64(3)
)

// fakeRepo implements Repository for service-level tests: it records the
// derived params (including the hook-resolved responsibility) so the
// tests prove the service validates, authorizes and maps — not the
// adapter.
type fakeRepo struct {
	submitIn  SubmitReviewParams
	submitOut domain.Review
	submitErr error

	listProjectID string
	listNumber    int64
	listOut       []domain.Review
	listErr       error
}

func (f *fakeRepo) SubmitReview(_ context.Context, in SubmitReviewParams) (domain.Review, error) {
	f.submitIn = in
	return f.submitOut, f.submitErr
}

func (f *fakeRepo) ListReviews(_ context.Context, projectID string, number int64) ([]domain.Review, error) {
	f.listProjectID, f.listNumber = projectID, number
	return f.listOut, f.listErr
}

// fakeGate implements ProjectGate: a configured membership role, or the
// two non-member outcomes.
type fakeGate struct {
	role *domain.ProjectRole
	err  error
}

func (g *fakeGate) GetMembership(_ context.Context, _ domain.User, _ string) (domain.ProjectMembership, error) {
	if g.err != nil {
		return domain.ProjectMembership{}, g.err
	}
	if g.role == nil {
		return domain.ProjectMembership{}, projects.ErrMemberNotFound
	}
	return domain.ProjectMembership{Role: *g.role}, nil
}

// fakeResponsibility implements ResponsibilityGate: a configured label
// or error.
type fakeResponsibility struct {
	label string
	err   error
	seen  []string
}

func (f *fakeResponsibility) ReviewResponsibility(_ context.Context, projectID, userID string) (string, error) {
	f.seen = append(f.seen, projectID+"|"+userID)
	return f.label, f.err
}

// failingEngine is an authz.Engine that always errors: the service must
// fail closed on it.
type failingEngine struct{}

func (failingEngine) Authorize(_ context.Context, _ authz.Request) (authz.Decision, error) {
	return authz.Decision{}, errors.New("engine down")
}

func roleOf(r domain.ProjectRole) *domain.ProjectRole { return &r }

func validInput() SubmitReviewInput {
	return SubmitReviewInput{
		Kind:     domain.ReviewKindScientific,
		Decision: domain.ReviewDecisionChangesRequested,
		Body:     "the protocol section needs the sampling rationale",
	}
}

func actor() domain.User {
	return domain.User{ID: "99999999-9999-9999-9999-999999999999", Handle: "reviewer"}
}

// newService wires the service with the real matrix engine (the
// canonical row), a fake project gate and the given hook. The hook
// conversion normalizes a typed nil *fakeResponsibility to a nil
// interface — the "no hook wired" shape.
func newService(repo *fakeRepo, gate *fakeGate, hook *fakeResponsibility, engine authz.Engine) *Service {
	if engine == nil {
		engine = authz.NewMatrixEngine()
	}
	var hookGate ResponsibilityGate
	if hook != nil {
		hookGate = hook
	}
	return NewService(Deps{Repo: repo, Projects: gate, Authz: engine, Responsibility: hookGate})
}

func TestSubmitReviewValidatesShape(t *testing.T) {
	ctx := context.Background()
	repo := &fakeRepo{}
	svc := newService(repo, &fakeGate{role: roleOf(domain.ProjectRoleMaintainer)}, nil, nil)

	cases := []struct {
		name string
		mut  func(*SubmitReviewInput)
	}{
		{"unknown kind", func(in *SubmitReviewInput) { in.Kind = "rights" }},
		{"empty kind", func(in *SubmitReviewInput) { in.Kind = "" }},
		{"unknown decision", func(in *SubmitReviewInput) { in.Decision = "commented" }},
		{"oversized body", func(in *SubmitReviewInput) { in.Body = strings.Repeat("x", 50_001) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validInput()
			tc.mut(&in)
			if _, err := svc.SubmitReview(ctx, actor(), projID, prNo, in); !errors.Is(err, ErrValidation) {
				t.Fatalf("err = %v, want ErrValidation", err)
			}
			if repo.submitIn != (SubmitReviewParams{}) {
				t.Fatalf("repo called on invalid input: %+v", repo.submitIn)
			}
		})
	}

	if _, err := svc.SubmitReview(ctx, domain.User{}, projID, prNo, validInput()); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty actor: err = %v, want ErrValidation", err)
	}
}

// TestSubmitReviewPermissionMatrix walks the submit_scientific_review
// row: maintainer/owner allowed; viewer/contributor/non-member only
// through the responsibility hook; every unresolved shape refused.
func TestSubmitReviewPermissionMatrix(t *testing.T) {
	ctx := context.Background()

	labelHook := &fakeResponsibility{label: "Experimental Reviewer"}

	cases := []struct {
		name    string
		role    *domain.ProjectRole
		gateErr error
		hook    *fakeResponsibility
		want    error
		wantLbl string
	}{
		{"maintainer allowed without hook", roleOf(domain.ProjectRoleMaintainer), nil, nil, nil, ""},
		{"owner allowed without hook", roleOf(domain.ProjectRoleOwner), nil, nil, nil, ""},
		{"maintainer allowed, hook label recorded", roleOf(domain.ProjectRoleMaintainer), nil, labelHook, nil, "Experimental Reviewer"},
		{"maintainer allowed despite hook error", roleOf(domain.ProjectRoleMaintainer), nil, &fakeResponsibility{err: errors.New("resolver down")}, nil, ""},
		{"viewer without hook refused", roleOf(domain.ProjectRoleViewer), nil, nil, ErrForbidden, ""},
		{"viewer without label refused", roleOf(domain.ProjectRoleViewer), nil, &fakeResponsibility{}, ErrForbidden, ""},
		{"viewer with label allowed", roleOf(domain.ProjectRoleViewer), nil, labelHook, nil, "Experimental Reviewer"},
		{"viewer hook error fails closed", roleOf(domain.ProjectRoleViewer), nil, &fakeResponsibility{err: errors.New("resolver down")}, ErrStore, ""},
		{"contributor with label allowed", roleOf(domain.ProjectRoleContributor), nil, labelHook, nil, "Experimental Reviewer"},
		{"non-member with label allowed", nil, nil, labelHook, nil, "Experimental Reviewer"},
		{"non-member without hook refused", nil, nil, nil, ErrForbidden, ""},
		{"unknown role class denied", roleOf("architect"), nil, labelHook, ErrForbidden, ""},
		{"project read denied propagates", roleOf(domain.ProjectRoleMaintainer), projects.ErrProjectNotFound, nil, projects.ErrProjectNotFound, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepo{submitOut: domain.Review{ID: "r-1"}}
			svc := newService(repo, &fakeGate{role: tc.role, err: tc.gateErr}, tc.hook, nil)
			_, err := svc.SubmitReview(ctx, actor(), projID, prNo, validInput())
			if tc.want == nil {
				if err != nil {
					t.Fatalf("err = %v, want success", err)
				}
				if repo.submitIn.Responsibility != tc.wantLbl {
					t.Fatalf("recorded responsibility = %q, want %q", repo.submitIn.Responsibility, tc.wantLbl)
				}
				if repo.submitIn.ReviewerID != actor().ID || repo.submitIn.ProjectID != projID || repo.submitIn.Number != prNo {
					t.Fatalf("recorded identity wrong: %+v", repo.submitIn)
				}
			} else {
				if !errors.Is(err, tc.want) {
					t.Fatalf("err = %v, want %v", err, tc.want)
				}
				if repo.submitIn != (SubmitReviewParams{}) {
					t.Fatalf("repo called on refused submission: %+v", repo.submitIn)
				}
			}
		})
	}
}

func TestSubmitReviewEngineFailuresFailClosed(t *testing.T) {
	ctx := context.Background()
	repo := &fakeRepo{}

	// No engine wired: state unknowable, refused as a store failure.
	svc := NewService(Deps{Repo: repo, Projects: &fakeGate{role: roleOf(domain.ProjectRoleMaintainer)}})
	if _, err := svc.SubmitReview(ctx, actor(), projID, prNo, validInput()); !errors.Is(err, ErrStore) {
		t.Fatalf("no engine: err = %v, want ErrStore", err)
	}
	// Engine failing: same fail-closed outcome.
	svc = newService(repo, &fakeGate{role: roleOf(domain.ProjectRoleMaintainer)}, nil, failingEngine{})
	if _, err := svc.SubmitReview(ctx, actor(), projID, prNo, validInput()); !errors.Is(err, ErrStore) {
		t.Fatalf("failing engine: err = %v, want ErrStore", err)
	}
	// No project gate: the class is unknowable, refused.
	svc = NewService(Deps{Repo: repo, Authz: authz.NewMatrixEngine()})
	if _, err := svc.SubmitReview(ctx, actor(), projID, prNo, validInput()); !errors.Is(err, ErrStore) {
		t.Fatalf("no project gate: err = %v, want ErrStore", err)
	}
}

// TestSubmitReviewMapsStoreOutcomes: the expected domain outcomes pass
// through unwrapped; anything else becomes ErrStore.
func TestSubmitReviewMapsStoreOutcomes(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name         string
		err          error
		want         error
		wantTerminal bool
	}{
		{"already reviewed", ErrAlreadyReviewed, ErrAlreadyReviewed, false},
		{"pull request not found", pullrequests.ErrPullRequestNotFound, pullrequests.ErrPullRequestNotFound, false},
		{"terminal PR", &pullrequests.TerminalError{Number: prNo, State: domain.PullRequestStateMerged}, nil, true},
		{"store failure", errors.New("connection refused"), ErrStore, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepo{submitErr: tc.err}
			svc := newService(repo, &fakeGate{role: roleOf(domain.ProjectRoleMaintainer)}, nil, nil)
			_, err := svc.SubmitReview(ctx, actor(), projID, prNo, validInput())
			if tc.wantTerminal {
				var term *pullrequests.TerminalError
				if !errors.As(err, &term) {
					t.Fatalf("err = %v, want TerminalError", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestSubmitReviewOnePersonManyKinds: the acceptance "一人不同 review
// kind 可记录" at the service level — the same reviewer submitting
// different dimensions is never refused, and each carries its own
// decision.
func TestSubmitReviewOnePersonManyKinds(t *testing.T) {
	ctx := context.Background()
	repo := &fakeRepo{}
	svc := newService(repo, &fakeGate{role: roleOf(domain.ProjectRoleMaintainer)}, nil, nil)

	scientific := validInput()
	scientific.Kind = domain.ReviewKindScientific
	scientific.Decision = domain.ReviewDecisionApproved
	if _, err := svc.SubmitReview(ctx, actor(), projID, prNo, scientific); err != nil {
		t.Fatalf("scientific review: %v", err)
	}
	first := repo.submitIn

	integrity := validInput()
	integrity.Kind = domain.ReviewKindIntegrity
	integrity.Decision = domain.ReviewDecisionChangesRequested
	if _, err := svc.SubmitReview(ctx, actor(), projID, prNo, integrity); err != nil {
		t.Fatalf("integrity review: %v", err)
	}
	if repo.submitIn.Kind != domain.ReviewKindIntegrity || repo.submitIn.Decision != domain.ReviewDecisionChangesRequested {
		t.Fatalf("second submission wrong: %+v", repo.submitIn)
	}
	if first.ReviewerID != repo.submitIn.ReviewerID || first.Kind == repo.submitIn.Kind {
		t.Fatalf("dimensions not kept separate: %+v vs %+v", first, repo.submitIn)
	}
}

func TestListValidatesAndForwards(t *testing.T) {
	ctx := context.Background()
	repo := &fakeRepo{listOut: []domain.Review{{ID: "r-1"}}}
	svc := newService(repo, &fakeGate{role: roleOf(domain.ProjectRoleMaintainer)}, nil, nil)

	reviews, err := svc.List(ctx, projID, prNo)
	if err != nil || len(reviews) != 1 {
		t.Fatalf("List = %+v, %v", reviews, err)
	}
	if _, err := svc.List(ctx, "", prNo); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty project: err = %v, want ErrValidation", err)
	}
	if _, err := svc.List(ctx, projID, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("zero number: err = %v, want ErrValidation", err)
	}
	repo.listErr = errors.New("boom")
	if _, err := svc.List(ctx, projID, prNo); !errors.Is(err, ErrStore) {
		t.Fatalf("store failure: err = %v, want ErrStore", err)
	}
}
