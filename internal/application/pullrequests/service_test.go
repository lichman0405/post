package pullrequests

import (
	"context"
	"errors"
	"testing"

	"github.com/lichman0405/post/internal/domain"
)

// fakeRepo implements Repository for service-level tests: it records the
// validated params the service derives, so the tests prove the service
// checks the shape and the docs/43 map and maps outcomes — not the
// adapter.
type fakeRepo struct {
	createIn  CreatePullRequestParams
	createOut domain.PullRequest
	createErr error

	getProjectID string
	getNumber    int64
	getOut       domain.PullRequest
	getErr       error

	listOut []domain.PullRequest
	listErr error

	setProjectID       string
	setNumber          int64
	setExpected, setTo domain.PullRequestState
	setOut             domain.PullRequest
	setErr             error

	refreshProjectID string
	refreshNumber    int64
	refreshOut       domain.PullRequest
	refreshErr       error
}

func (f *fakeRepo) CreatePullRequest(_ context.Context, in CreatePullRequestParams) (domain.PullRequest, error) {
	f.createIn = in
	return f.createOut, f.createErr
}

func (f *fakeRepo) GetPullRequest(_ context.Context, projectID string, number int64) (domain.PullRequest, error) {
	f.getProjectID, f.getNumber = projectID, number
	return f.getOut, f.getErr
}

func (f *fakeRepo) ListPullRequests(_ context.Context, projectID string) ([]domain.PullRequest, error) {
	return f.listOut, f.listErr
}

func (f *fakeRepo) SetPullRequestState(_ context.Context, projectID string, number int64, expected, to domain.PullRequestState) (domain.PullRequest, error) {
	f.setProjectID, f.setNumber, f.setExpected, f.setTo = projectID, number, expected, to
	// Mirror the adapter: the returned row carries the new state.
	f.setOut.State = to
	return f.setOut, f.setErr
}

func (f *fakeRepo) RefreshProposedState(_ context.Context, projectID string, number int64) (domain.PullRequest, error) {
	f.refreshProjectID, f.refreshNumber = projectID, number
	return f.refreshOut, f.refreshErr
}

func validCreateParams() CreatePullRequestParams {
	return CreatePullRequestParams{
		ProjectID:      "11111111-1111-1111-1111-111111111111",
		SourceBranchID: "22222222-2222-2222-2222-222222222222",
		TargetBranchID: "33333333-3333-3333-3333-333333333333",
		Title:          "Propose the screening protocol",
		Body:           "Context.",
		CreatedBy:      "44444444-4444-4444-4444-444444444444",
	}
}

func TestCreateValidatesShapeAndForwards(t *testing.T) {
	ctx := context.Background()
	repo := &fakeRepo{createOut: domain.PullRequest{ID: "pr-1", Number: 7, State: domain.PullRequestStateOpen}}
	svc := NewService(repo)
	pr, err := svc.Create(ctx, validCreateParams())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if pr.Number != 7 || pr.State != domain.PullRequestStateOpen {
		t.Fatalf("Create returned %+v, want the repo's value", pr)
	}
	if repo.createIn.Title != validCreateParams().Title {
		t.Fatalf("Create forwarded %+v", repo.createIn)
	}

	cases := map[string]func(*CreatePullRequestParams){
		"empty project":    func(p *CreatePullRequestParams) { p.ProjectID = "" },
		"empty source":     func(p *CreatePullRequestParams) { p.SourceBranchID = "" },
		"empty target":     func(p *CreatePullRequestParams) { p.TargetBranchID = "" },
		"same branches":    func(p *CreatePullRequestParams) { p.SourceBranchID = p.TargetBranchID },
		"blank title":      func(p *CreatePullRequestParams) { p.Title = "  " },
		"oversized title":  func(p *CreatePullRequestParams) { p.Title = string(make([]byte, 201)) },
		"oversized body":   func(p *CreatePullRequestParams) { p.Body = string(make([]byte, 50_001)) },
		"empty created_by": func(p *CreatePullRequestParams) { p.CreatedBy = "" },
	}
	for name, mutate := range cases {
		in := validCreateParams()
		mutate(&in)
		if _, err := svc.Create(ctx, in); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: Create error = %v, want ErrValidation", name, err)
		}
	}
}

func TestCreateMapsStoreFailure(t *testing.T) {
	ctx := context.Background()
	repo := &fakeRepo{createErr: errors.New("db down")}
	_, err := NewService(repo).Create(ctx, validCreateParams())
	if !errors.Is(err, ErrStore) {
		t.Fatalf("Create error = %v, want ErrStore", err)
	}
}

func TestGetValidatesAndMaps(t *testing.T) {
	ctx := context.Background()
	repo := &fakeRepo{getOut: domain.PullRequest{ID: "pr-1"}}
	svc := NewService(repo)
	pr, err := svc.Get(ctx, "11111111-1111-1111-1111-111111111111", 3)
	if err != nil || pr.ID != "pr-1" {
		t.Fatalf("Get = %+v, %v", pr, err)
	}
	if repo.getProjectID == "" || repo.getNumber != 3 {
		t.Fatalf("Get forwarded (%q, %d)", repo.getProjectID, repo.getNumber)
	}
	if _, err := svc.Get(ctx, "", 3); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty project: error = %v, want ErrValidation", err)
	}
	if _, err := svc.Get(ctx, "p", 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("zero number: error = %v, want ErrValidation", err)
	}
	repo.getErr = ErrPullRequestNotFound
	if _, err := svc.Get(ctx, "p", 1); !errors.Is(err, ErrPullRequestNotFound) {
		t.Fatalf("not found passthrough: error = %v", err)
	}
	repo.getErr = errors.New("db down")
	if _, err := svc.Get(ctx, "p", 1); !errors.Is(err, ErrStore) {
		t.Fatalf("store failure: error = %v, want ErrStore", err)
	}
}

func TestSetStateEnforcesTransitionMap(t *testing.T) {
	ctx := context.Background()

	// The full happy chain open → review_required → approved →
	// merge_ready → merged, one step per call; each call re-reads the
	// current state from the fake and CASes on it.
	steps := []domain.PullRequestState{
		domain.PullRequestStateReviewRequired,
		domain.PullRequestStateApproved,
		domain.PullRequestStateMergeReady,
		domain.PullRequestStateMerged,
	}
	repo := &fakeRepo{getOut: domain.PullRequest{ID: "pr-1", State: domain.PullRequestStateOpen}}
	svc := NewService(repo)
	current := domain.PullRequestStateOpen
	for _, to := range steps {
		pr, err := svc.SetState(ctx, "p", 1, to)
		if err != nil {
			t.Fatalf("SetState(%s -> %s): %v", current, to, err)
		}
		if repo.setExpected != current || repo.setTo != to {
			t.Fatalf("CAS forwarded (expected=%s, to=%s), want (%s, %s)", repo.setExpected, repo.setTo, current, to)
		}
		current = to
		repo.getOut.State = current
		if pr.State != current {
			t.Fatalf("SetState returned state %s, want %s", pr.State, current)
		}
	}

	// Skipping the machine is refused before the store is touched.
	repo.getOut.State = domain.PullRequestStateOpen
	repo.setTo = ""
	_, err := svc.SetState(ctx, "p", 1, domain.PullRequestStateMerged)
	var te *TransitionError
	if !errors.As(err, &te) || te.From != domain.PullRequestStateOpen || te.To != domain.PullRequestStateMerged {
		t.Fatalf("open -> merged error = %v, want TransitionError", err)
	}
	if repo.setTo != "" {
		t.Fatalf("store was called for an illegal transition (%s)", repo.setTo)
	}

	// open is not a transition target.
	if _, err := svc.SetState(ctx, "p", 1, domain.PullRequestStateOpen); !errors.Is(err, ErrValidation) {
		t.Fatalf("to=open error = %v, want ErrValidation", err)
	}
	// An unknown state value is refused too.
	if _, err := svc.SetState(ctx, "p", 1, "draft"); !errors.Is(err, ErrValidation) {
		t.Fatalf("to=draft error = %v, want ErrValidation", err)
	}

	// A CAS conflict surfaces as *StateConflictError, not a store error.
	repo.getOut.State = domain.PullRequestStateOpen
	repo.setErr = &StateConflictError{Number: 1, Current: domain.PullRequestStateReviewRequired}
	_, err = svc.SetState(ctx, "p", 1, domain.PullRequestStateReviewRequired)
	var sc *StateConflictError
	if !errors.As(err, &sc) {
		t.Fatalf("CAS conflict error = %v, want StateConflictError", err)
	}
	repo.setErr = nil
}

func TestSetStateNamedFlows(t *testing.T) {
	ctx := context.Background()
	repo := &fakeRepo{}
	svc := NewService(repo)

	// RequestReview/Close/Abort/MarkMerged drive their docs/43 targets
	// through the same CAS.
	states := map[string]domain.PullRequestState{
		"review": domain.PullRequestStateReviewRequired,
		"close":  domain.PullRequestStateClosed,
		"abort":  domain.PullRequestStateAborted,
		"merge":  domain.PullRequestStateMerged,
	}
	for name, want := range states {
		repo.getOut.State = domain.PullRequestStateOpen
		repo.setTo = ""
		var err error
		switch name {
		case "review":
			_, err = svc.RequestReview(ctx, "p", 1)
		case "close":
			_, err = svc.Close(ctx, "p", 1)
		case "abort":
			_, err = svc.Abort(ctx, "p", 1)
		case "merge":
			repo.getOut.State = domain.PullRequestStateMergeReady
			_, err = svc.MarkMerged(ctx, "p", 1)
		}
		if err != nil || repo.setTo != want {
			t.Errorf("%s: err=%v setTo=%s, want %s", name, err, repo.setTo, want)
		}
	}
	// MarkMerged from open is an illegal transition (merge_ready →
	// merged only).
	repo.getOut.State = domain.PullRequestStateOpen
	repo.setTo = ""
	if _, err := svc.MarkMerged(ctx, "p", 1); err == nil {
		t.Fatal("MarkMerged from open succeeded, want TransitionError")
	}
}

func TestRefreshProposedValidatesAndMaps(t *testing.T) {
	ctx := context.Background()
	repo := &fakeRepo{refreshOut: domain.PullRequest{ID: "pr-1", ProposedStateID: "state-9"}}
	svc := NewService(repo)
	pr, err := svc.RefreshProposed(ctx, "p", 2)
	if err != nil || pr.ProposedStateID != "state-9" {
		t.Fatalf("RefreshProposed = %+v, %v", pr, err)
	}
	if repo.refreshProjectID != "p" || repo.refreshNumber != 2 {
		t.Fatalf("RefreshProposed forwarded (%q, %d)", repo.refreshProjectID, repo.refreshNumber)
	}
	if _, err := svc.RefreshProposed(ctx, "", 2); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty project: error = %v, want ErrValidation", err)
	}
	if _, err := svc.RefreshProposed(ctx, "p", 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("zero number: error = %v, want ErrValidation", err)
	}
	repo.refreshErr = &TerminalError{Number: 2, State: domain.PullRequestStateMerged}
	_, err = svc.RefreshProposed(ctx, "p", 2)
	var te *TerminalError
	if !errors.As(err, &te) {
		t.Fatalf("terminal refresh error = %v, want TerminalError", err)
	}
	repo.refreshErr = errors.New("db down")
	if _, err := svc.RefreshProposed(ctx, "p", 2); !errors.Is(err, ErrStore) {
		t.Fatalf("store failure: error = %v, want ErrStore", err)
	}
}
