// Package discussions' unit tests: the command's own rules, exercised
// against fakes. They are deliberately the cheap half of the suite — the
// integration and e2e tests run the same rules on real PostgreSQL — and
// they exist for the two things only a unit test can state directly:
//
//  1. the STRUCTURAL claim behind "a comment does not change scientific
//     state": the comment path has no state-committing machinery to call.
//     TestCommentPathHasNoStateCommitReachable walks this package's own
//     Deps, every port interface it declares, and the package's import
//     graph, and refuses a port method that commits state and an import of
//     the package that owns commits. The behavioural half (a comment calls
//     nothing but the thread port) is asserted by counting fake calls;
//  2. the promotion shape rules and the fail-closed authorization, one
//     refusal per rule, each asserting that nothing was written.
package discussions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
)

// --- fakes ---------------------------------------------------------------

type fakeProjects struct {
	project   domain.Project
	getErr    error
	member    *domain.ProjectMembership
	memberErr error
	gets      int
	members   int
}

func (f *fakeProjects) Get(_ context.Context, _ projects.Reader, projectID string) (domain.Project, error) {
	f.gets++
	if f.getErr != nil {
		return domain.Project{}, f.getErr
	}
	if f.project.ID != projectID {
		return domain.Project{}, projects.ErrProjectNotFound
	}
	return f.project, nil
}

func (f *fakeProjects) GetMembership(_ context.Context, _ domain.User, projectID string) (domain.ProjectMembership, error) {
	f.members++
	if f.memberErr != nil {
		return domain.ProjectMembership{}, f.memberErr
	}
	if f.member == nil || f.member.ProjectID != projectID {
		return domain.ProjectMembership{}, projects.ErrMemberNotFound
	}
	return *f.member, nil
}

type fakeThreads struct {
	threads  []domain.DiscussionThread
	comments []domain.DiscussionComment
	target   bool
	err      error
	creates  int
}

func (f *fakeThreads) CreateThreadWithComment(_ context.Context, t domain.DiscussionThread, c domain.DiscussionComment) (domain.DiscussionThread, domain.DiscussionComment, error) {
	f.creates++
	if f.err != nil {
		return domain.DiscussionThread{}, domain.DiscussionComment{}, f.err
	}
	t.ID = fmt.Sprintf("thread-%d", len(f.threads)+1)
	c.ID = fmt.Sprintf("comment-%d", len(f.comments)+1)
	c.ThreadID = t.ID
	f.threads = append(f.threads, t)
	f.comments = append(f.comments, c)
	return t, c, nil
}

func (f *fakeThreads) GetThread(_ context.Context, projectID, threadID string) (domain.DiscussionThread, error) {
	for _, t := range f.threads {
		if t.ID == threadID && t.ProjectID == projectID {
			return t, nil
		}
	}
	return domain.DiscussionThread{}, ErrThreadNotFound
}

func (f *fakeThreads) ListThreads(_ context.Context, projectID string, kind domain.DiscussionTargetKind, targetID string) ([]domain.DiscussionThread, error) {
	out := []domain.DiscussionThread{}
	for _, t := range f.threads {
		if t.ProjectID == projectID && t.TargetKind == kind && t.TargetID == targetID {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeThreads) CreateComment(_ context.Context, c domain.DiscussionComment) (domain.DiscussionComment, error) {
	f.creates++
	if f.err != nil {
		return domain.DiscussionComment{}, f.err
	}
	c.ID = fmt.Sprintf("comment-%d", len(f.comments)+1)
	f.comments = append(f.comments, c)
	return c, nil
}

func (f *fakeThreads) GetComment(_ context.Context, projectID, commentID string) (domain.DiscussionComment, error) {
	for _, c := range f.comments {
		if c.ID == commentID && c.ProjectID == projectID {
			return c, nil
		}
	}
	return domain.DiscussionComment{}, ErrCommentNotFound
}

func (f *fakeThreads) ListComments(_ context.Context, threadID string) ([]domain.DiscussionComment, error) {
	out := []domain.DiscussionComment{}
	for _, c := range f.comments {
		if c.ThreadID == threadID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeThreads) DeleteComment(_ context.Context, projectID, commentID, deletedBy string) (domain.DiscussionComment, error) {
	for i, c := range f.comments {
		if c.ID == commentID && c.ProjectID == projectID {
			if c.Deleted() {
				return domain.DiscussionComment{}, ErrCommentDeleted
			}
			if c.CreatedBy != deletedBy {
				return domain.DiscussionComment{}, ErrForbidden
			}
			now := time.Now().UTC()
			f.comments[i].DeletedAt, f.comments[i].DeletedBy = &now, &deletedBy
			return f.comments[i], nil
		}
	}
	return domain.DiscussionComment{}, ErrCommentNotFound
}

func (f *fakeThreads) TargetExists(_ context.Context, _ string, _ domain.DiscussionTargetKind, _ string) (bool, error) {
	return f.target, f.err
}

type fakePromotions struct {
	rows     []domain.DiscussionPromotion
	audits   []domain.AuditEntry
	issues   []domain.Issue
	records  int
	issueErr error
}

func (f *fakePromotions) PromoteToIssue(_ context.Context, in IssueProposal, promotion domain.DiscussionPromotion, audit domain.AuditEntry) (domain.Issue, domain.DiscussionPromotion, error) {
	f.records++
	f.audits = append(f.audits, audit)
	if f.issueErr != nil {
		return domain.Issue{}, domain.DiscussionPromotion{}, f.issueErr
	}
	issue := domain.Issue{
		ID: "issue-1", ProjectID: in.ProjectID, Number: 1, IssueType: in.IssueType,
		Title: in.Title, Body: in.Body, State: domain.IssueOpen, CreatedBy: in.CreatedBy,
	}
	promotion.ID, promotion.Ref = "promotion-1", domain.PromotionRef(domain.PromotionIssue, issue.ID)
	f.issues, f.rows = append(f.issues, issue), append(f.rows, promotion)
	return issue, promotion, nil
}

func (f *fakePromotions) RecordPromotion(_ context.Context, promotion domain.DiscussionPromotion, audit domain.AuditEntry) (domain.DiscussionPromotion, error) {
	f.records++
	f.audits = append(f.audits, audit)
	promotion.ID = fmt.Sprintf("promotion-%d", len(f.rows)+1)
	f.rows = append(f.rows, promotion)
	return promotion, nil
}

func (f *fakePromotions) GetPromotion(_ context.Context, projectID, promotionID string) (domain.DiscussionPromotion, error) {
	for _, p := range f.rows {
		if p.ID == promotionID && p.ProjectID == projectID {
			return p, nil
		}
	}
	return domain.DiscussionPromotion{}, ErrPromotionNotFound
}

func (f *fakePromotions) ListPromotionsByRef(_ context.Context, projectID, ref string) ([]domain.DiscussionPromotion, error) {
	out := []domain.DiscussionPromotion{}
	for _, p := range f.rows {
		if p.ProjectID == projectID && p.Ref == ref {
			out = append(out, p)
		}
	}
	return out, nil
}

// fakeHypotheses is the RSG write path as the command sees it: one method,
// and no way to commit anything else.
type fakeHypotheses struct {
	calls  int
	in     rsg.CreateObjectInput
	actor  domain.User
	branch string
	err    error
}

func (f *fakeHypotheses) CreateObject(_ context.Context, actor domain.User, _, branchID string, in rsg.CreateObjectInput) (rsg.ObjectResult, error) {
	f.calls, f.in, f.actor, f.branch = f.calls+1, in, actor, branchID
	if f.err != nil {
		return rsg.ObjectResult{}, f.err
	}
	return rsg.ObjectResult{
		Object:  domain.ScientificObject{ID: "object-1", ProjectID: "p1", ObjectType: "hypothesis", CurrentVersionNo: 1},
		Version: domain.ScientificObjectVersion{ID: "version-1", ObjectID: "object-1", VersionNo: 1, StateID: "state-9", BranchID: &branchID},
	}, nil
}

type fakeEvidence struct {
	calls  int
	in     rsg.CreateEvidenceAssertionInput
	branch string
	err    error
}

func (f *fakeEvidence) CreateEvidenceAssertion(_ context.Context, _ domain.User, _, branchID string, in rsg.CreateEvidenceAssertionInput) (rsg.EvidenceAssertionResult, error) {
	f.calls, f.in, f.branch = f.calls+1, in, branchID
	if f.err != nil {
		return rsg.EvidenceAssertionResult{}, f.err
	}
	return rsg.EvidenceAssertionResult{Assertion: rsg.EvidenceAssertionRow{
		ID: "assertion-1", ProjectID: "p1", StateID: "state-9", ReviewState: "unreviewed",
		ReasoningNote: in.ReasoningNote, RelationType: in.Relation,
	}}, nil
}

type fakeForkGate struct {
	owned bool
	calls int
}

func (f *fakeForkGate) OwnedFork(_ context.Context, _, _ string) (bool, error) {
	f.calls++
	return f.owned, nil
}

// --- the fixture ---------------------------------------------------------

type fixture struct {
	cmd       *Command
	projects  *fakeProjects
	threads   *fakeThreads
	promos    *fakePromotions
	hypo      *fakeHypotheses
	evidence  *fakeEvidence
	forks     *fakeForkGate
	owner     domain.User
	outsider  domain.User
	projectID string
}

// newFixture wires the command the way the surfaces do, with the REAL
// authorization matrix (so the tests assert on the shipped decision) and a
// non-member actor to drive the fail-closed paths.
func newFixture(opts ...func(*fixture)) *fixture {
	f := &fixture{
		projects: &fakeProjects{
			project: domain.Project{ID: "p1", Slug: "project-1", Visibility: domain.VisibilityPublic},
			member:  &domain.ProjectMembership{ProjectID: "p1", UserID: "owner-1", Role: domain.ProjectRoleOwner},
		},
		threads:   &fakeThreads{target: true},
		promos:    &fakePromotions{},
		hypo:      &fakeHypotheses{},
		evidence:  &fakeEvidence{},
		forks:     &fakeForkGate{},
		owner:     domain.User{ID: "owner-1"},
		outsider:  domain.User{ID: "outsider-1"},
		projectID: "p1",
	}
	for _, opt := range opts {
		opt(f)
	}
	f.cmd = NewCommand(Deps{
		Projects:   f.projects,
		Threads:    f.threads,
		Promotions: f.promos,
		Hypotheses: f.hypo,
		Evidence:   f.evidence,
		Authz:      authz.NewMatrixEngine(),
		ForkGate:   f.forks,
	})
	return f
}

// openThreads seeds one thread with one live comment authored by alice.
func (f *fixture) openThreads(t *testing.T) (domain.DiscussionThread, domain.DiscussionComment) {
	t.Helper()
	res, err := f.cmd.OpenThread(context.Background(), f.owner, OpenThreadParams{
		ProjectID: f.projectID, TargetKind: domain.DiscussionTargetProject,
		TargetID: f.projectID, Body: "shall we try a lower temperature?",
	})
	if err != nil {
		t.Fatalf("open thread: %v", err)
	}
	return res.Thread, res.Comments[0]
}

// --- 1. the structural claim --------------------------------------------

// TestCommentPathHasNoStateCommitReachable is the unit half of acceptance
// criterion 2: the comment path cannot change scientific state because
// there is no state-committing machinery within reach of it. It states that
// three ways over the shipped source:
//
//  1. every field of Deps, walked by reflection, has a method set without a
//     state-committing method;
//  2. every port interface this package declares does the same;
//  3. no production file of this package imports the package that owns
//     commits (internal/application/states) — a port could not smuggle one
//     in even through an unmatched type.
//
// A fourth, behavioural, assertion follows in
// TestCommentPathCallsNothingButTheThreadPort.
func TestCommentPathHasNoStateCommitReachable(t *testing.T) {
	refused := func(name string) bool {
		// Commit is the operation under test; anything naming a commit is
		// refused too, so a rename cannot slip past.
		return strings.Contains(strings.ToLower(name), "commit")
	}

	depsType := reflect.TypeOf(Deps{})
	seen := 0
	for i := 0; i < depsType.NumField(); i++ {
		field := depsType.Field(i)
		if field.Type.Kind() != reflect.Interface {
			continue
		}
		seen++
		for j := 0; j < field.Type.NumMethod(); j++ {
			if name := field.Type.Method(j).Name; refused(name) {
				t.Errorf("Deps.%s exposes %s: a state-committing method reachable from the discussion command", field.Name, name)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no interface field walked: the instrument measured nothing")
	}

	// Every port interface the application package declares.
	for _, port := range []any{
		(*ProjectAccessPort)(nil), (*ForkGate)(nil), (*ThreadPort)(nil),
		(*PromotionPort)(nil), (*HypothesisPort)(nil), (*EvidencePort)(nil),
	} {
		typ := reflect.TypeOf(port).Elem()
		for j := 0; j < typ.NumMethod(); j++ {
			if name := typ.Method(j).Name; refused(name) {
				t.Errorf("port %s exposes %s", typ.Name(), name)
			}
		}
	}

	// The import graph of the package's production files: walk the directory
	// and ParseFile each non-test source, so no call is the deprecated
	// parser.ParseDir (staticcheck SA1019 since Go 1.25).
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("list the package directory: %v", err)
	}
	fset := token.NewFileSet()
	files := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files++
		for _, imp := range parsed.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.HasSuffix(path, "/internal/application/states") ||
				strings.HasSuffix(path, "/internal/application/validation") {
				t.Errorf("%s imports %s: the state-committing path is reachable from this package", name, path)
			}
		}
	}
	if files < 3 {
		t.Fatalf("parsed %d production files: the import instrument measured something other than this package", files)
	}
}

// TestCommentPathCallsNothingButTheThreadPort is the behavioural half: a
// thread and a comment run through the command and the creating ports are
// untouched, so "commenting writes nothing but the conversation" is a count
// of zero calls, not a promise.
func TestCommentPathCallsNothingButTheThreadPort(t *testing.T) {
	f := newFixture()
	thread, _ := f.openThreads(t)
	if _, err := f.cmd.AddComment(context.Background(), f.owner, AddCommentParams{
		ProjectID: f.projectID, ThreadID: thread.ID, Body: "and what about 280K?",
	}); err != nil {
		t.Fatalf("add comment: %v", err)
	}
	if f.hypo.calls != 0 || f.evidence.calls != 0 || f.promos.records != 0 {
		t.Fatalf("a comment reached a creating port: objects=%d evidence=%d promotions=%d",
			f.hypo.calls, f.evidence.calls, f.promos.records)
	}
}

// --- 2. the conversation's own rules ------------------------------------

func TestOpenThreadShapeRules(t *testing.T) {
	f := newFixture()
	cases := []struct {
		name string
		in   OpenThreadParams
	}{
		{"blank project", OpenThreadParams{TargetKind: domain.DiscussionTargetProject, TargetID: "p1", Body: "x"}},
		{"unknown kind", OpenThreadParams{ProjectID: "p1", TargetKind: "document", TargetID: "p1", Body: "x"}},
		{"blank target", OpenThreadParams{ProjectID: "p1", TargetKind: domain.DiscussionTargetProject, Body: "x"}},
		{"blank body", OpenThreadParams{ProjectID: "p1", TargetKind: domain.DiscussionTargetProject, TargetID: "p1", Body: "   "}},
		{"oversized body", OpenThreadParams{ProjectID: "p1", TargetKind: domain.DiscussionTargetProject, TargetID: "p1",
			Body: strings.Repeat("x", domain.MaxDiscussionBodyLen+1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.cmd.OpenThread(context.Background(), f.owner, tc.in)
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("err = %v, want ErrValidation", err)
			}
		})
	}
	if f.threads.creates != 0 {
		t.Fatalf("a refused thread was written: %d creates", f.threads.creates)
	}

	// A project-kind target must name the project itself: anything else is
	// not-found, never a thread about something the project does not have.
	if _, err := f.cmd.OpenThread(context.Background(), f.owner, OpenThreadParams{
		ProjectID: "p1", TargetKind: domain.DiscussionTargetProject, TargetID: "p2", Body: "x",
	}); !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("foreign project target err = %v, want ErrTargetNotFound", err)
	}

	// An invisible project is not-found (existence hiding, docs/45).
	hidden := newFixture(func(f *fixture) { f.projects.getErr = projects.ErrProjectNotFound })
	if _, err := hidden.cmd.OpenThread(context.Background(), hidden.owner, OpenThreadParams{
		ProjectID: "p1", TargetKind: domain.DiscussionTargetProject, TargetID: "p1", Body: "x",
	}); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("invisible project err = %v, want ErrProjectNotFound", err)
	}
}

func TestCommentingRequiresReadingTheTarget(t *testing.T) {
	f := newFixture()
	thread, _ := f.openThreads(t)

	// A thread of another project is not-found, not forbidden.
	if _, err := f.cmd.AddComment(context.Background(), f.owner, AddCommentParams{
		ProjectID: "p2", ThreadID: thread.ID, Body: "x",
	}); !errors.Is(err, ErrProjectNotFound) && !errors.Is(err, ErrThreadNotFound) {
		t.Fatalf("foreign project comment err = %v, want not-found", err)
	}

	// The blank and oversized bodies are refused before anything is read.
	for _, body := range []string{"", "  ", strings.Repeat("x", domain.MaxDiscussionBodyLen+1)} {
		if _, err := f.cmd.AddComment(context.Background(), f.owner, AddCommentParams{
			ProjectID: f.projectID, ThreadID: thread.ID, Body: body,
		}); !errors.Is(err, ErrValidation) {
			t.Fatalf("body %q err = %v, want ErrValidation", body, err)
		}
	}
	if _, err := f.cmd.AddComment(context.Background(), f.owner, AddCommentParams{
		ProjectID: f.projectID, ThreadID: thread.ID, Body: strings.Repeat("x", domain.MaxDiscussionBodyLen),
	}); err != nil {
		t.Fatalf("body at the bound: %v", err)
	}
}

func TestCommentWithdrawalRules(t *testing.T) {
	f := newFixture()
	_, comment := f.openThreads(t)

	// Only the author may withdraw: the comment path is the author's own.
	other := domain.User{ID: "another-member"}
	if _, err := f.cmd.DeleteComment(context.Background(), other, f.projectID, comment.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("another actor's withdrawal err = %v, want ErrForbidden", err)
	}

	withdrawn, err := f.cmd.DeleteComment(context.Background(), f.owner, f.projectID, comment.ID)
	if err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if !withdrawn.Deleted() || withdrawn.Body != comment.Body {
		t.Fatalf("tombstone = %+v, want the row with its body and a DeletedAt", withdrawn)
	}
	// Withdrawing twice is a conflict, and the thread read still renders the
	// row (with its body — withholding is the transport's job).
	if _, err := f.cmd.DeleteComment(context.Background(), f.owner, f.projectID, comment.ID); !errors.Is(err, ErrCommentDeleted) {
		t.Fatalf("second withdrawal err = %v, want ErrCommentDeleted", err)
	}
	res, err := f.cmd.GetThread(context.Background(), projects.Reader{UserID: f.owner.ID, Authenticated: true}, f.projectID, comment.ThreadID)
	if err != nil {
		t.Fatalf("read thread: %v", err)
	}
	if len(res.Comments) != 1 || !res.Comments[0].Deleted() {
		t.Fatalf("thread read = %+v, want the withdrawn comment (nothing disappears)", res.Comments)
	}
}

// --- 3. the promotion shape rules ---------------------------------------

func TestPromoteShapeRulesAreExclusive(t *testing.T) {
	cases := []struct {
		name string
		in   PromoteParams
	}{
		{"unknown kind", PromoteParams{Kind: "epic"}},
		{"issue with a branch", PromoteParams{Kind: domain.PromotionIssue, IssueType: "question", Title: "x", BranchID: "b1"}},
		{"issue without a type", PromoteParams{Kind: domain.PromotionIssue, Title: "x"}},
		{"issue type too long", PromoteParams{Kind: domain.PromotionIssue, IssueType: strings.Repeat("x", domain.MaxPromotionTitleLen+1), Title: "x"}},
		{"title too long", PromoteParams{Kind: domain.PromotionIssue, IssueType: "question", Title: strings.Repeat("x", domain.MaxPromotionTitleLen+1)}},
		{"issue type on a hypothesis", PromoteParams{Kind: domain.PromotionHypothesis, IssueType: "question",
			BranchID: "b1", Hypothesis: &HypothesisProposal{QuestionID: "q1"}}},
		{"hypothesis with a title", PromoteParams{Kind: domain.PromotionHypothesis, Title: "x",
			BranchID: "b1", Hypothesis: &HypothesisProposal{QuestionID: "q1"}}},
		{"hypothesis without a branch", PromoteParams{Kind: domain.PromotionHypothesis, Hypothesis: &HypothesisProposal{QuestionID: "q1"}}},
		{"hypothesis without a question", PromoteParams{Kind: domain.PromotionHypothesis, BranchID: "b1", Hypothesis: &HypothesisProposal{}}},
		{"evidence without its pins", PromoteParams{Kind: domain.PromotionExternalEvidence, BranchID: "b1",
			Evidence: &EvidenceProposal{Relation: "supports", EvidenceType: "experimental"}}},
		{"evidence without relation/type", PromoteParams{Kind: domain.PromotionExternalEvidence, BranchID: "b1",
			Evidence: &EvidenceProposal{TargetVersionRef: "v1", EvidenceVersionRef: "v2"}}},
		{"evidence without a branch", PromoteParams{Kind: domain.PromotionExternalEvidence,
			Evidence: &EvidenceProposal{TargetVersionRef: "v1", EvidenceVersionRef: "v2", Relation: "supports", EvidenceType: "experimental"}}},
		{"evidence with a title", PromoteParams{Kind: domain.PromotionExternalEvidence, Title: "x", BranchID: "b1",
			Evidence: &EvidenceProposal{TargetVersionRef: "v1", EvidenceVersionRef: "v2", Relation: "supports", EvidenceType: "experimental"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			thread, comment := f.openThreads(t)
			in := tc.in
			in.ProjectID, in.ThreadID, in.CommentID = f.projectID, thread.ID, comment.ID
			_, err := f.cmd.Promote(context.Background(), f.owner, in)
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("err = %v, want ErrValidation", err)
			}
			if f.promos.records != 0 || f.hypo.calls != 0 || f.evidence.calls != 0 {
				t.Fatalf("a refused promotion wrote something: promotions=%d objects=%d evidence=%d",
					f.promos.records, f.hypo.calls, f.evidence.calls)
			}
		})
	}
}

// TestPromotionIsFailClosed covers acceptance criterion 5 at the command
// level: the promotion runs the shipped matrix for write_scientific_state,
// an actor the matrix does not permit is refused, and the conditional
// own_fork_only cell is resolved by the fork gate — never assumed.
func TestPromotionIsFailClosed(t *testing.T) {
	issue := PromoteParams{Kind: domain.PromotionIssue, IssueType: "question", Title: "promote me"}

	// A viewer may read and may not promote (the matrix's viewer cell).
	viewer := newFixture(func(f *fixture) {
		f.projects.member = &domain.ProjectMembership{ProjectID: "p1", UserID: "outsider-1", Role: domain.ProjectRoleViewer}
	})
	thread, comment := viewer.openThreads(t)
	in := issue
	in.ProjectID, in.ThreadID, in.CommentID = "p1", thread.ID, comment.ID
	if _, err := viewer.cmd.Promote(context.Background(), viewer.outsider, in); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer promotion err = %v, want ErrForbidden", err)
	}

	// A non-member answers the conditional cell: without a fork gate the
	// command refuses (fail closed, no assumption), and with one that says
	// "not your fork" it refuses too.
	outsider := newFixture(func(f *fixture) { f.projects.memberErr = projects.ErrMemberNotFound })
	outsider.cmd = NewCommand(Deps{
		Projects: outsider.projects, Threads: outsider.threads, Promotions: outsider.promos,
		Hypotheses: outsider.hypo, Evidence: outsider.evidence,
		Authz: authz.NewMatrixEngine(), ForkGate: nil,
	})
	thread, comment = outsider.openThreads(t)
	in.ProjectID, in.ThreadID, in.CommentID = "p1", thread.ID, comment.ID
	if _, err := outsider.cmd.Promote(context.Background(), outsider.outsider, in); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-member promotion without a fork gate err = %v, want ErrForbidden", err)
	}

	notFork := newFixture(func(f *fixture) {
		f.projects.memberErr = projects.ErrMemberNotFound
		f.forks.owned = false
	})
	thread, comment = notFork.openThreads(t)
	in.ProjectID, in.ThreadID, in.CommentID = "p1", thread.ID, comment.ID
	if _, err := notFork.cmd.Promote(context.Background(), notFork.outsider, in); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-member promotion of a foreign project err = %v, want ErrForbidden", err)
	}
	if notFork.forks.calls == 0 {
		t.Fatal("the conditional cell was decided without asking the fork gate")
	}
	if notFork.promos.records != 0 {
		t.Fatalf("a refused promotion recorded %d rows", notFork.promos.records)
	}
}

// --- 4. the promotion paths ---------------------------------------------

// TestPromoteToIssueRecordsThePromotingActor pins the provenance split: the
// record names who promoted (the authorization's subject) while the comment
// keeps its own author, and the audit row names the act.
func TestPromoteToIssueRecordsThePromotingActor(t *testing.T) {
	f := newFixture(func(f *fixture) {
		// The comment is written by a contributor; the owner promotes it.
		f.projects.member = &domain.ProjectMembership{ProjectID: "p1", UserID: "contributor-1", Role: domain.ProjectRoleContributor}
	})
	contributor := domain.User{ID: "contributor-1"}
	thread, comment, err := f.openThreadsAs(t, contributor)
	if err != nil {
		t.Fatalf("open thread: %v", err)
	}
	res, err := f.cmd.Promote(context.Background(), f.owner, PromoteParams{
		ProjectID: f.projectID, ThreadID: thread.ID, CommentID: comment.ID,
		Kind: domain.PromotionIssue, IssueType: "question", Title: "a question",
	})
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if res.Issue == nil || res.Issue.Title != "a question" || res.Issue.Body != comment.Body {
		t.Fatalf("issue = %+v, want the promoted comment as its body", res.Issue)
	}
	if res.Origin.Promotion.PromotedBy != f.owner.ID {
		t.Fatalf("promoted_by = %q, want the promoting actor %q", res.Origin.Promotion.PromotedBy, f.owner.ID)
	}
	if res.Origin.Comment.CreatedBy != contributor.ID {
		t.Fatalf("origin comment author = %q, want %q", res.Origin.Comment.CreatedBy, contributor.ID)
	}
	if res.Origin.Promotion.Ref != domain.PromotionRef(domain.PromotionIssue, "issue-1") {
		t.Fatalf("ref = %q", res.Origin.Promotion.Ref)
	}
	if len(f.promos.audits) != 1 || f.promos.audits[0].Action != domain.ActionDiscussionPromoted ||
		f.promos.audits[0].ActorID != f.owner.ID {
		t.Fatalf("audit rows = %+v, want one discussion.promoted by the promoter", f.promos.audits)
	}

	// A title is optional: the comment's first line becomes one.
	f2 := newFixture()
	thread2, comment2 := f2.openThreads(t)
	res2, err := f2.cmd.Promote(context.Background(), f2.owner, PromoteParams{
		ProjectID: f2.projectID, ThreadID: thread2.ID, CommentID: comment2.ID,
		Kind: domain.PromotionIssue, IssueType: "question",
	})
	if err != nil {
		t.Fatalf("promote without a title: %v", err)
	}
	if res2.Issue == nil || res2.Issue.Title == "" {
		t.Fatalf("issue = %+v, want a title derived from the comment", res2.Issue)
	}

	// A comment carrying no usable first line has no title to fall back on.
	f3 := newFixture()
	thread3, comment3 := f3.openThreads(t)
	f3.threads.comments[len(f3.threads.comments)-1].Body = "   "
	if _, err := f3.cmd.Promote(context.Background(), f3.owner, PromoteParams{
		ProjectID: f3.projectID, ThreadID: thread3.ID, CommentID: comment3.ID,
		Kind: domain.PromotionIssue, IssueType: "question",
	}); !errors.Is(err, ErrValidation) {
		t.Fatalf("untitled promotion err = %v, want ErrValidation", err)
	}
}

// TestPromoteToHypothesisGoesThroughTheRSPath pins that the command does not
// create scientific objects itself: it hands the RSG write path a payload,
// and the created object's identity comes back from there.
func TestPromoteToHypothesisGoesThroughTheRSGPath(t *testing.T) {
	f := newFixture()
	thread, comment := f.openThreads(t)
	res, err := f.cmd.Promote(context.Background(), f.owner, PromoteParams{
		ProjectID: f.projectID, ThreadID: thread.ID, CommentID: comment.ID,
		Kind: domain.PromotionHypothesis, BranchID: "branch-1",
		Hypothesis: &HypothesisProposal{QuestionID: "question-1"},
	})
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if f.hypo.calls != 1 || f.hypo.branch != "branch-1" {
		t.Fatalf("object calls = %d on branch %q, want one on branch-1", f.hypo.calls, f.hypo.branch)
	}
	if f.hypo.in.ObjectType != hypothesisObjectType {
		t.Fatalf("object type = %q, want %q", f.hypo.in.ObjectType, hypothesisObjectType)
	}
	// The statement is the comment, and the question is the caller's — the
	// two facts the schema requires.
	var payload map[string]string
	if err := json.Unmarshal(f.hypo.in.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload["statement"] != comment.Body || payload["question_id"] != "question-1" {
		t.Fatalf("payload = %v, want the comment's words and the named question", payload)
	}
	if res.Object == nil || res.Object.Object.ID != "object-1" {
		t.Fatalf("result = %+v, want the RSG path's object", res)
	}
	if res.Origin.Promotion.Ref != domain.PromotionRef(domain.PromotionHypothesis, "object-1") {
		t.Fatalf("ref = %q", res.Origin.Promotion.Ref)
	}
	// An RSG refusal is the caller's validation error, not a store failure.
	f.hypo.err = rsg.ErrValidation
	if _, err := f.cmd.Promote(context.Background(), f.owner, PromoteParams{
		ProjectID: f.projectID, ThreadID: thread.ID, CommentID: comment.ID,
		Kind: domain.PromotionHypothesis, BranchID: "branch-1",
		Hypothesis: &HypothesisProposal{QuestionID: "question-1"},
	}); !errors.Is(err, ErrValidation) {
		t.Fatalf("RSG validation refusal err = %v, want ErrValidation", err)
	}
}

// TestPromoteToEvidenceIsAProposal pins the third kind: the assertion is
// created by the evidence write path, the reasoning note falls back to the
// comment's own words, and the promotion names the assertion.
func TestPromoteToEvidenceIsAProposal(t *testing.T) {
	f := newFixture()
	thread, comment := f.openThreads(t)
	res, err := f.cmd.Promote(context.Background(), f.owner, PromoteParams{
		ProjectID: f.projectID, ThreadID: thread.ID, CommentID: comment.ID,
		Kind: domain.PromotionExternalEvidence, BranchID: "branch-1",
		Evidence: &EvidenceProposal{
			TargetVersionRef: "version-1", EvidenceVersionRef: "version-2",
			Relation: "supports", EvidenceType: "experimental",
			Directness: "direct", InferenceNature: "causal",
		},
	})
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if f.evidence.calls != 1 || f.evidence.branch != "branch-1" {
		t.Fatalf("evidence calls = %d on branch %q", f.evidence.calls, f.evidence.branch)
	}
	if f.evidence.in.ReasoningNote != comment.Body {
		t.Fatalf("reasoning note = %q, want the comment's words %q", f.evidence.in.ReasoningNote, comment.Body)
	}
	if res.Assertion == nil || res.Assertion.Assertion.ReviewState != "unreviewed" {
		t.Fatalf("assertion = %+v, want the unreviewed proposal", res.Assertion)
	}
	if res.Origin.Promotion.Ref != domain.PromotionRef(domain.PromotionExternalEvidence, "assertion-1") {
		t.Fatalf("ref = %q", res.Origin.Promotion.Ref)
	}
}

// TestPromotionRefusalsBeforeTheObjectExists pins the order the other way
// round: a promotion whose comment cannot be used is refused before any
// creator runs, so a refusal can never leave an object behind.
func TestPromotionRefusalsBeforeTheObjectExists(t *testing.T) {
	f := newFixture()
	thread, comment := f.openThreads(t)

	// A withdrawn comment cannot be promoted.
	if _, err := f.cmd.DeleteComment(context.Background(), f.owner, f.projectID, comment.ID); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if _, err := f.cmd.Promote(context.Background(), f.owner, PromoteParams{
		ProjectID: f.projectID, ThreadID: thread.ID, CommentID: comment.ID,
		Kind: domain.PromotionHypothesis, BranchID: "branch-1",
		Hypothesis: &HypothesisProposal{QuestionID: "question-1"},
	}); !errors.Is(err, ErrCommentDeleted) {
		t.Fatalf("withdrawn comment promotion err = %v, want ErrCommentDeleted", err)
	}

	// A comment of another thread is not found here (the path names both).
	// It is a separate fixture: the first comment is withdrawn by now, and
	// the tombstone check runs first.
	g := newFixture()
	first, err := g.cmd.OpenThread(context.Background(), g.owner, OpenThreadParams{
		ProjectID: g.projectID, TargetKind: domain.DiscussionTargetProject, TargetID: g.projectID, Body: "first thread",
	})
	if err != nil {
		t.Fatalf("first thread: %v", err)
	}
	second, err := g.cmd.OpenThread(context.Background(), g.owner, OpenThreadParams{
		ProjectID: g.projectID, TargetKind: domain.DiscussionTargetProject, TargetID: g.projectID, Body: "another thread",
	})
	if err != nil {
		t.Fatalf("second thread: %v", err)
	}
	if _, err := g.cmd.Promote(context.Background(), g.owner, PromoteParams{
		ProjectID: g.projectID, ThreadID: second.Thread.ID, CommentID: first.Comments[0].ID,
		Kind: domain.PromotionHypothesis, BranchID: "branch-1",
		Hypothesis: &HypothesisProposal{QuestionID: "question-1"},
	}); !errors.Is(err, ErrCommentNotFound) {
		t.Fatalf("comment of another thread err = %v, want ErrCommentNotFound", err)
	}
	if g.hypo.calls != 0 || g.promos.records != 0 {
		t.Fatalf("a refused promotion reached a creator: objects=%d promotions=%d", g.hypo.calls, g.promos.records)
	}

	if f.hypo.calls != 0 || f.promos.records != 0 {
		t.Fatalf("a refused promotion reached a creator: objects=%d promotions=%d", f.hypo.calls, f.promos.records)
	}
}

// --- 5. the reads --------------------------------------------------------

func TestPromotionReadsRunTheProjectGate(t *testing.T) {
	f := newFixture()
	thread, comment := f.openThreads(t)
	res, err := f.cmd.Promote(context.Background(), f.owner, PromoteParams{
		ProjectID: f.projectID, ThreadID: thread.ID, CommentID: comment.ID,
		Kind: domain.PromotionIssue, IssueType: "question", Title: "a question",
	})
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	r := projects.Reader{UserID: f.owner.ID, Authenticated: true}

	origin, err := f.cmd.Promotion(context.Background(), r, f.projectID, res.Origin.Promotion.ID)
	if err != nil {
		t.Fatalf("promotion read: %v", err)
	}
	if origin.Thread.ID != thread.ID || origin.Comment.ID != comment.ID {
		t.Fatalf("origin = %+v, want the thread and comment it came from", origin)
	}
	byRef, err := f.cmd.PromotionsForRef(context.Background(), r, f.projectID, res.Origin.Promotion.Ref)
	if err != nil {
		t.Fatalf("promotions for ref: %v", err)
	}
	if len(byRef) != 1 || byRef[0].Promotion.ID != res.Origin.Promotion.ID {
		t.Fatalf("by ref = %+v", byRef)
	}

	// A ref that is not "<kind>:<id>" is a validation error, and an unknown
	// promotion id is not-found.
	if _, err := f.cmd.PromotionsForRef(context.Background(), r, f.projectID, "issue"); !errors.Is(err, ErrValidation) {
		t.Fatalf("malformed ref err = %v, want ErrValidation", err)
	}
	if _, err := f.cmd.Promotion(context.Background(), r, f.projectID, "nope"); !errors.Is(err, ErrPromotionNotFound) {
		t.Fatalf("unknown promotion err = %v, want ErrPromotionNotFound", err)
	}

	// The gate runs first: a reader who may not read the project learns
	// nothing (existence hiding).
	hidden := newFixture(func(f *fixture) { f.projects.getErr = projects.ErrProjectNotFound })
	if _, err := hidden.cmd.Promotion(context.Background(), projects.Reader{}, "p1", "promotion-1"); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("hidden project read err = %v, want ErrProjectNotFound", err)
	}
}

// TestThreadReadScopesToTheProject pins that a read of another project's
// thread answers not-found rather than the thread.
func TestThreadReadScopesToTheProject(t *testing.T) {
	f := newFixture()
	thread, _ := f.openThreads(t)
	r := projects.Reader{UserID: f.owner.ID, Authenticated: true}
	if _, err := f.cmd.GetThread(context.Background(), r, "p2", thread.ID); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("foreign project thread read err = %v, want ErrProjectNotFound", err)
	}
	// With the project visible but the thread not of it, the thread itself
	// is not-found (the store's own scope).
	f.projects.project = domain.Project{ID: "p2", Visibility: domain.VisibilityPublic}
	if _, err := f.cmd.GetThread(context.Background(), r, "p2", thread.ID); !errors.Is(err, ErrThreadNotFound) {
		t.Fatalf("thread of another project err = %v, want ErrThreadNotFound", err)
	}
}

// openThreadsAs is openThreads for a named actor.
func (f *fixture) openThreadsAs(t *testing.T, actor domain.User) (domain.DiscussionThread, domain.DiscussionComment, error) {
	t.Helper()
	res, err := f.cmd.OpenThread(context.Background(), actor, OpenThreadParams{
		ProjectID: f.projectID, TargetKind: domain.DiscussionTargetProject,
		TargetID: f.projectID, Body: "a contributor's idea",
	})
	if err != nil {
		return domain.DiscussionThread{}, domain.DiscussionComment{}, err
	}
	return res.Thread, res.Comments[0], nil
}
