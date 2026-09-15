package contribution

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/contribution"
)

// fakeRepo implements contribution.Repository for service-level tests:
// it records the validated params the service derives, so the tests
// prove the service checks the shape and the state machine, maps
// outcomes, and refuses agent actors — not the adapter.
type fakeRepo struct {
	createIn  contribution.CreateOpportunityParams
	createOut contribution.ContributionOpportunity
	createErr error

	getProjectID, getID string
	getOut              contribution.ContributionOpportunity
	getErr              error

	listProjectID string
	listOut       []contribution.ContributionOpportunity
	listErr       error

	listPublicOut []contribution.ContributionOpportunity
	listPublicErr error

	setProjectID, setID        string
	setExpected, setTo         contribution.OpportunityState
	setOut                     contribution.ContributionOpportunity
	setErr                     error
	setBy                      contribution.Actor
	publicizeProjectID, pubID  string
	publicizeOut               contribution.ContributionOpportunity
	publicizeErr               error
	publicizeBy                contribution.Actor
	updateProjectID, updateID  string
	updatePatch                contribution.MetadataPatch
	updateOut                  contribution.ContributionOpportunity
	updateErr                  error
	calledSet, calledPublicize bool
}

func (f *fakeRepo) CreateOpportunity(_ context.Context, in contribution.CreateOpportunityParams) (contribution.ContributionOpportunity, error) {
	f.createIn = in
	return f.createOut, f.createErr
}

func (f *fakeRepo) GetOpportunity(_ context.Context, projectID, id string) (contribution.ContributionOpportunity, error) {
	f.getProjectID, f.getID = projectID, id
	return f.getOut, f.getErr
}

func (f *fakeRepo) ListOpportunities(_ context.Context, projectID string) ([]contribution.ContributionOpportunity, error) {
	f.listProjectID = projectID
	return f.listOut, f.listErr
}

func (f *fakeRepo) ListPublicOpportunities(_ context.Context) ([]contribution.ContributionOpportunity, error) {
	return f.listPublicOut, f.listPublicErr
}

func (f *fakeRepo) SetOpportunityState(_ context.Context, projectID, id string, expected, to contribution.OpportunityState, by contribution.Actor) (contribution.ContributionOpportunity, error) {
	f.calledSet = true
	f.setProjectID, f.setID, f.setExpected, f.setTo, f.setBy = projectID, id, expected, to, by
	// Mirror the adapter: the returned row carries the new state, and the
	// suggested -> open move earns the approval stamp.
	f.setOut.State = to
	if expected == contribution.OpportunityStateSuggested && to == contribution.OpportunityStateOpen {
		byID := by.UserID
		f.setOut.ApprovedBy = &byID
	}
	return f.setOut, f.setErr
}

func (f *fakeRepo) PublicizeOpportunity(_ context.Context, projectID, id string, by contribution.Actor) (contribution.ContributionOpportunity, error) {
	f.calledPublicize = true
	f.publicizeProjectID, f.pubID, f.publicizeBy = projectID, id, by
	return f.publicizeOut, f.publicizeErr
}

func (f *fakeRepo) UpdateOpportunityMetadata(_ context.Context, projectID, id string, patch contribution.MetadataPatch) (contribution.ContributionOpportunity, error) {
	f.updateProjectID, f.updateID, f.updatePatch = projectID, id, patch
	return f.updateOut, f.updateErr
}

const (
	testProjectID = "11111111-1111-1111-1111-111111111111"
	testTargetID  = "22222222-2222-2222-2222-222222222222"
	testAliceID   = "33333333-3333-3333-3333-333333333333"
	testAgentID   = "44444444-4444-4444-4444-444444444444"
	testOpID      = "55555555-5555-5555-5555-555555555555"
)

func validMarkParams() MarkParams {
	return MarkParams{
		ProjectID:            testProjectID,
		TargetType:           contribution.TargetIssue,
		TargetID:             testTargetID,
		Description:          "Curate rows.",
		Difficulty:           contribution.DifficultyIntermediate,
		RequiredCapabilities: []string{"data-curation", "python"},
		By:                   contribution.Actor{UserID: testAliceID},
	}
}

func TestMarkOpenValidatesShapeAndForwards(t *testing.T) {
	ctx := context.Background()
	repo := &fakeRepo{createOut: contribution.ContributionOpportunity{
		ID: testOpID, State: contribution.OpportunityStateOpen,
		Visibility: contribution.OpportunityVisibilityInternal,
	}}
	svc := NewService(repo)
	op, err := svc.MarkOpen(ctx, validMarkParams())
	if err != nil || op.ID != testOpID {
		t.Fatalf("MarkOpen = %+v, %v", op, err)
	}
	if repo.createIn.State != contribution.OpportunityStateOpen ||
		repo.createIn.By.UserID != testAliceID ||
		repo.createIn.Difficulty != contribution.DifficultyIntermediate {
		t.Fatalf("MarkOpen forwarded %+v", repo.createIn)
	}

	cases := map[string]func(*MarkParams){
		"empty project":      func(p *MarkParams) { p.ProjectID = "" },
		"unknown target":     func(p *MarkParams) { p.TargetType = "paper" },
		"empty target id":    func(p *MarkParams) { p.TargetID = "" },
		"unknown difficulty": func(p *MarkParams) { p.Difficulty = "expert" },
		"bad capabilities":   func(p *MarkParams) { p.RequiredCapabilities = []string{"UPPER"} },
		"oversized body":     func(p *MarkParams) { p.Description = strings.Repeat("x", 50_001) },
		"empty actor":        func(p *MarkParams) { p.By.UserID = "" },
	}
	for name, mutate := range cases {
		in := validMarkParams()
		mutate(&in)
		if _, err := svc.MarkOpen(ctx, in); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: MarkOpen error = %v, want ErrValidation", name, err)
		}
	}
}

func TestMarkOpenRefusesAgent(t *testing.T) {
	ctx := context.Background()
	repo := &fakeRepo{}
	svc := NewService(repo)

	in := validMarkParams()
	in.By = contribution.Actor{UserID: testAgentID, IsAgent: true}
	_, err := svc.MarkOpen(ctx, in)
	var agentErr *AgentNotPermittedError
	if !errors.As(err, &agentErr) {
		t.Fatalf("agent MarkOpen error = %v, want *AgentNotPermittedError", err)
	}
	if agentErr.Action != "mark" || agentErr.Code() != CodeAgentMarkDenied {
		t.Errorf("agent mark error = %+v, want action mark / %s", agentErr, CodeAgentMarkDenied)
	}
	if repo.createIn.ProjectID != "" {
		t.Fatalf("store was called for an agent mark: %+v", repo.createIn)
	}
}

func TestMarkOpenMapsStoreOutcomes(t *testing.T) {
	ctx := context.Background()
	repo := &fakeRepo{createErr: errors.New("db down")}
	svc := NewService(repo)
	if _, err := svc.MarkOpen(ctx, validMarkParams()); !errors.Is(err, ErrStore) {
		t.Fatalf("store failure: error = %v, want ErrStore", err)
	}
	repo.createErr = contribution.ErrTargetAlreadyActive
	if _, err := svc.MarkOpen(ctx, validMarkParams()); !errors.Is(err, contribution.ErrTargetAlreadyActive) {
		t.Fatalf("duplicate passthrough: error = %v", err)
	}
}

func TestSuggestCreatesSuggestedAndAllowsAgent(t *testing.T) {
	ctx := context.Background()
	repo := &fakeRepo{createOut: contribution.ContributionOpportunity{
		ID: testOpID, State: contribution.OpportunityStateSuggested,
	}}
	svc := NewService(repo)
	suggested, err := svc.Suggest(ctx, SuggestParams{
		ProjectID:  testProjectID,
		TargetType: contribution.TargetResearchQuestion,
		TargetID:   testTargetID,
		Difficulty: contribution.DifficultyBeginner,
		By:         contribution.Actor{UserID: testAgentID, IsAgent: true},
	})
	if err != nil || suggested.State != contribution.OpportunityStateSuggested {
		t.Fatalf("Suggest = %+v, %v", suggested, err)
	}
	if repo.createIn.State != contribution.OpportunityStateSuggested ||
		repo.createIn.By.UserID != testAgentID || !repo.createIn.By.IsAgent {
		t.Fatalf("Suggest forwarded %+v", repo.createIn)
	}

	// The suggestion path validates the same shape as a mark.
	if _, err := svc.Suggest(ctx, SuggestParams{By: contribution.Actor{UserID: testAgentID, IsAgent: true}}); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty suggestion: error = %v, want ErrValidation", err)
	}
}

func TestApproveRefusesAgentAndStampsMaintainer(t *testing.T) {
	ctx := context.Background()
	repo := &fakeRepo{getOut: contribution.ContributionOpportunity{
		ID: testOpID, State: contribution.OpportunityStateSuggested,
		SuggestedBy: strPtr(testAgentID),
	}}
	svc := NewService(repo)

	_, err := svc.Approve(ctx, testProjectID, testOpID, contribution.Actor{UserID: testAgentID, IsAgent: true})
	var agentErr *AgentNotPermittedError
	if !errors.As(err, &agentErr) {
		t.Fatalf("agent Approve error = %v, want *AgentNotPermittedError", err)
	}
	if agentErr.Action != "approve" || agentErr.Code() != CodeAgentApproveDenied {
		t.Errorf("agent approve error = %+v, want action approve / %s", agentErr, CodeAgentApproveDenied)
	}
	if repo.calledSet {
		t.Fatal("store was called for an agent approve")
	}

	approved, err := svc.Approve(ctx, testProjectID, testOpID, contribution.Actor{UserID: testAliceID})
	if err != nil || approved.State != contribution.OpportunityStateOpen {
		t.Fatalf("Approve = %+v, %v", approved, err)
	}
	if repo.setExpected != contribution.OpportunityStateSuggested ||
		repo.setTo != contribution.OpportunityStateOpen ||
		repo.setBy.UserID != testAliceID {
		t.Fatalf("Approve CAS forwarded (expected=%s, to=%s, by=%s)",
			repo.setExpected, repo.setTo, repo.setBy.UserID)
	}
	if approved.ApprovedBy == nil || *approved.ApprovedBy != testAliceID {
		t.Errorf("Approve returned %+v, want the approval stamped", approved)
	}
}

func TestLifecycleMovesAndRefusals(t *testing.T) {
	ctx := context.Background()

	t.Run("reject", func(t *testing.T) {
		repo := &fakeRepo{getOut: contribution.ContributionOpportunity{
			ID: testOpID, State: contribution.OpportunityStateSuggested,
		}}
		rejected, err := NewService(repo).Reject(ctx, testProjectID, testOpID, contribution.Actor{UserID: testAliceID})
		if err != nil || rejected.State != contribution.OpportunityStateClosed {
			t.Fatalf("Reject = %+v, %v", rejected, err)
		}
		if repo.setExpected != contribution.OpportunityStateSuggested ||
			repo.setTo != contribution.OpportunityStateClosed {
			t.Fatalf("Reject CAS forwarded (expected=%s, to=%s)", repo.setExpected, repo.setTo)
		}
	})

	t.Run("close", func(t *testing.T) {
		repo := &fakeRepo{getOut: contribution.ContributionOpportunity{
			ID: testOpID, State: contribution.OpportunityStateOpen,
		}}
		closed, err := NewService(repo).Close(ctx, testProjectID, testOpID, contribution.Actor{UserID: testAliceID})
		if err != nil || closed.State != contribution.OpportunityStateClosed {
			t.Fatalf("Close = %+v, %v", closed, err)
		}
	})

	t.Run("wrong current state", func(t *testing.T) {
		// Approve on a row that is no longer suggested: the machine map
		// holds against the actual row state.
		repo := &fakeRepo{getOut: contribution.ContributionOpportunity{
			ID: testOpID, State: contribution.OpportunityStateClosed,
		}}
		_, err := NewService(repo).Approve(ctx, testProjectID, testOpID, contribution.Actor{UserID: testAliceID})
		var te *TransitionError
		if !errors.As(err, &te) || te.From != contribution.OpportunityStateClosed || te.To != contribution.OpportunityStateOpen {
			t.Fatalf("Approve on closed: error = %v, want TransitionError{closed -> open}", err)
		}
		if repo.calledSet {
			t.Fatal("store was called for a refused transition")
		}
	})

	t.Run("unresolved actor", func(t *testing.T) {
		repo := &fakeRepo{getOut: contribution.ContributionOpportunity{
			ID: testOpID, State: contribution.OpportunityStateSuggested,
		}}
		if _, err := NewService(repo).Approve(ctx, testProjectID, testOpID, contribution.Actor{}); !errors.Is(err, ErrValidation) {
			t.Fatalf("unresolved actor: error = %v, want ErrValidation", err)
		}
	})

	t.Run("cas conflict passthrough", func(t *testing.T) {
		repo := &fakeRepo{getOut: contribution.ContributionOpportunity{
			ID: testOpID, State: contribution.OpportunityStateSuggested,
		}, setErr: &contribution.StateConflictError{ID: testOpID, Current: contribution.OpportunityStateClosed}}
		_, err := NewService(repo).Approve(ctx, testProjectID, testOpID, contribution.Actor{UserID: testAliceID})
		var sc *contribution.StateConflictError
		if !errors.As(err, &sc) {
			t.Fatalf("CAS conflict: error = %v, want *StateConflictError", err)
		}
	})
}

func TestPublicizeRefusesAgentAndMaps(t *testing.T) {
	ctx := context.Background()
	repo := &fakeRepo{}
	svc := NewService(repo)

	_, err := svc.Publicize(ctx, testProjectID, testOpID, contribution.Actor{UserID: testAgentID, IsAgent: true})
	var agentErr *AgentNotPermittedError
	if !errors.As(err, &agentErr) {
		t.Fatalf("agent Publicize error = %v, want *AgentNotPermittedError", err)
	}
	if agentErr.Action != "publicize" || agentErr.Code() != CodeAgentPublicizeDenied {
		t.Errorf("agent publicize error = %+v, want action publicize / %s", agentErr, CodeAgentPublicizeDenied)
	}
	if repo.calledPublicize {
		t.Fatal("store was called for an agent publicize")
	}

	// An unresolved actor is refused too.
	if _, err := svc.Publicize(ctx, testProjectID, testOpID, contribution.Actor{}); !errors.Is(err, ErrValidation) {
		t.Fatalf("unresolved actor: error = %v, want ErrValidation", err)
	}
	if repo.calledPublicize {
		t.Fatal("store was called for an unresolved actor")
	}

	// Store outcomes pass through: not-publicizable and store failure.
	repo.publicizeErr = &contribution.PublicizeError{ID: testOpID, State: contribution.OpportunityStateSuggested}
	_, err = svc.Publicize(ctx, testProjectID, testOpID, contribution.Actor{UserID: testAliceID})
	var pubErr *contribution.PublicizeError
	if !errors.As(err, &pubErr) {
		t.Fatalf("not publicizable: error = %v, want *PublicizeError", err)
	}
	repo.publicizeErr = errors.New("db down")
	if _, err := svc.Publicize(ctx, testProjectID, testOpID, contribution.Actor{UserID: testAliceID}); !errors.Is(err, ErrStore) {
		t.Fatalf("store failure: error = %v, want ErrStore", err)
	}
}

func TestUpdateMetadataValidatesPatchAndMaps(t *testing.T) {
	ctx := context.Background()
	repo := &fakeRepo{updateOut: contribution.ContributionOpportunity{
		ID: testOpID, Title: "new title",
	}}
	svc := NewService(repo)

	title := "Curate the dataset v2"
	op, err := svc.UpdateMetadata(ctx, testProjectID, testOpID, contribution.MetadataPatch{Title: &title})
	if err != nil || op.Title != "new title" {
		t.Fatalf("UpdateMetadata = %+v, %v", op, err)
	}
	if repo.updateProjectID != testProjectID || repo.updateID != testOpID ||
		repo.updatePatch.Title == nil || *repo.updatePatch.Title != title {
		t.Fatalf("UpdateMetadata forwarded (%q, %q, %+v)", repo.updateProjectID, repo.updateID, repo.updatePatch)
	}

	// The patch's shape is the service's call: empty patch, bad title,
	// bad difficulty — all refused without touching the store.
	repo.updatePatch = contribution.MetadataPatch{}
	_, err = svc.UpdateMetadata(ctx, testProjectID, testOpID, contribution.MetadataPatch{})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("empty patch: error = %v, want ErrValidation", err)
	}
	bad := " "
	_, err = svc.UpdateMetadata(ctx, testProjectID, testOpID, contribution.MetadataPatch{Title: &bad})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("blank title: error = %v, want ErrValidation", err)
	}
	var badDiff contribution.Difficulty = "expert"
	_, err = svc.UpdateMetadata(ctx, testProjectID, testOpID, contribution.MetadataPatch{Difficulty: &badDiff})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("bad difficulty: error = %v, want ErrValidation", err)
	}
	if repo.updatePatch.Title != nil || repo.updatePatch.Difficulty != nil {
		t.Fatal("store was called for a refused patch")
	}

	// Store outcomes pass through: frozen and terminal.
	repo.updateErr = contribution.ErrPublicizedFrozen
	_, err = svc.UpdateMetadata(ctx, testProjectID, testOpID, contribution.MetadataPatch{Title: &title})
	if !errors.Is(err, contribution.ErrPublicizedFrozen) {
		t.Fatalf("frozen passthrough: error = %v", err)
	}
	repo.updateErr = &contribution.TerminalError{ID: testOpID, State: contribution.OpportunityStateClosed}
	_, err = svc.UpdateMetadata(ctx, testProjectID, testOpID, contribution.MetadataPatch{Title: &title})
	var te *contribution.TerminalError
	if !errors.As(err, &te) {
		t.Fatalf("terminal passthrough: error = %v", err)
	}
}

func TestGetAndLists(t *testing.T) {
	ctx := context.Background()
	repo := &fakeRepo{getOut: contribution.ContributionOpportunity{ID: testOpID}}
	svc := NewService(repo)

	op, err := svc.Get(ctx, testProjectID, testOpID)
	if err != nil || op.ID != testOpID {
		t.Fatalf("Get = %+v, %v", op, err)
	}
	if repo.getProjectID != testProjectID || repo.getID != testOpID {
		t.Fatalf("Get forwarded (%q, %q)", repo.getProjectID, repo.getID)
	}
	if _, err := svc.Get(ctx, "", testOpID); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty project: error = %v, want ErrValidation", err)
	}
	repo.getErr = contribution.ErrOpportunityNotFound
	if _, err := svc.Get(ctx, testProjectID, "unknown"); !errors.Is(err, contribution.ErrOpportunityNotFound) {
		t.Fatalf("not found passthrough: error = %v", err)
	}
	repo.getErr = errors.New("db down")
	if _, err := svc.Get(ctx, testProjectID, testOpID); !errors.Is(err, ErrStore) {
		t.Fatalf("store failure: error = %v, want ErrStore", err)
	}

	if _, err := svc.List(ctx, ""); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty project list: error = %v, want ErrValidation", err)
	}
	repo.listOut = []contribution.ContributionOpportunity{{ID: testOpID}}
	all, err := svc.List(ctx, testProjectID)
	if err != nil || len(all) != 1 {
		t.Fatalf("List = %+v, %v", all, err)
	}
	repo.listErr = errors.New("db down")
	if _, err := svc.List(ctx, testProjectID); !errors.Is(err, ErrStore) {
		t.Fatalf("list store failure: error = %v, want ErrStore", err)
	}

	repo.listPublicOut = []contribution.ContributionOpportunity{{ID: testOpID, Visibility: contribution.OpportunityVisibilityPublic}}
	public, err := svc.ListPublic(ctx)
	if err != nil || len(public) != 1 || public[0].Visibility != contribution.OpportunityVisibilityPublic {
		t.Fatalf("ListPublic = %+v, %v", public, err)
	}
	repo.listPublicErr = errors.New("db down")
	if _, err := svc.ListPublic(ctx); !errors.Is(err, ErrStore) {
		t.Fatalf("public list store failure: error = %v, want ErrStore", err)
	}
}

func strPtr(s string) *string { return &s }
