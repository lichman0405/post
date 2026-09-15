package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/application/reviews"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence"
)

// Task T0404: Scientific Review 模型 — over a REAL PostgreSQL, with the
// same store composition cmd/api/main.go wires (reviews service over the
// review store, the projects membership gate and the matrix engine).
// Proves the task requirements and both acceptance criteria end to end:
//
//   - 分维度: scientific and integrity decisions are separate review
//     records for the same reviewer and the same head — one person can
//     record different review kinds (acceptance), while a second decision
//     for the SAME kind on the SAME head is refused (23505);
//   - 权限正确: the submit_scientific_review matrix row over the real
//     membership gate — maintainer allowed, viewer conditional on the
//     reviewer-responsibility hook, private-project non-member answered
//     with the existence-hiding project-not-found (docs/45);
//   - the reviewer-responsibility hook resolves the conditional verdict
//     and its label is recorded on the row; a missing hook, an empty
//     label or a hook error fails closed (docs/12);
//   - reviewed_state_id pins every review to the head it judged: a
//     refreshed head opens a new round, and the changes_requested
//     projection moves review_required -> changes_requested atomically;
//   - the database guards back every application rule: the kind/decision
//     CHECKs (23514), the uniqueness (23505), the reviewed_state_id FK
//     (23503), and terminal PRs refuse further review records.

// labelGate is the fake reviewer-responsibility hook for these tests —
// the production rule-based resolver lands with T0604; until then
// production wires nil and the conditional verdict fails closed.
type labelGate struct {
	label string
	err   error
}

func (g *labelGate) ReviewResponsibility(_ context.Context, _, _ string) (string, error) {
	return g.label, g.err
}

// reviewSvc builds the production-shaped service over the fixture's
// stores. hook may be nil (the production composition until T0604).
func reviewSvc(f *prFixture, hook reviews.ResponsibilityGate) *reviews.Service {
	return reviews.NewService(reviews.Deps{
		Repo:     persistence.NewReviewStore(f.pool),
		Projects: projects.NewService(persistence.NewProjectStore(f.pool), persistence.NewOrgStore(f.pool), authz.NewMatrixEngine()),
		Authz:    authz.NewMatrixEngine(),
		// Responsibility: the reviewer-responsibility hook (task
		// requirement) — the production resolver lands with T0604.
		Responsibility: hook,
	})
}

// seedReviewUser creates one user and seeds the membership role directly
// in PostgreSQL (the member-management API lands with T0109; the tests
// seed canonical state so the engine's role columns are what is
// exercised here).
func seedReviewUser(t *testing.T, ctx context.Context, f *prFixture, email, handle, role string) domain.User {
	t.Helper()
	user, err := persistence.NewCredentialStore(f.pool).CreateWithPassword(ctx, email, "hash", handle, handle)
	if err != nil {
		t.Fatalf("seed %s: %v", handle, err)
	}
	if role != "" {
		if _, err := f.pool.Exec(ctx,
			`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, $3)`,
			f.project.ID, user.ID, role); err != nil {
			t.Fatalf("seed %s membership: %v", handle, err)
		}
	}
	return user
}

// mustReview submits one review and fails the test on error.
func mustReview(t *testing.T, ctx context.Context, svc *reviews.Service, actor domain.User, projectID string, number int64, kind domain.ReviewKind, decision domain.ReviewDecision) domain.Review {
	t.Helper()
	review, err := svc.SubmitReview(ctx, actor, projectID, number, reviews.SubmitReviewInput{
		Kind:     kind,
		Decision: decision,
		Body:     "review reasoning",
	})
	if err != nil {
		t.Fatalf("SubmitReview %s/%s: %v", kind, decision, err)
	}
	return review
}

func prState(t *testing.T, ctx context.Context, f *prFixture, number int64) domain.PullRequestState {
	t.Helper()
	pr, err := f.prsvc.Get(ctx, f.project.ID, number)
	if err != nil {
		t.Fatalf("Get PR #%d: %v", number, err)
	}
	return pr.State
}

func TestReviewRecordsPerDimensionPinsHeadAndProjectsState(t *testing.T) {
	ctx := testCtx(t)
	f := newPRFixture(t, ctx)
	f.commit(t, ctx, f.main.ID, "claim", `{"statement":"alpha"}`)
	feature := f.newFeature(t, ctx, "feature")
	head1 := f.commit(t, ctx, feature.ID, "hypothesis", `{"question_ref":"q1"}`)
	pr := f.openPR(t, ctx, feature.ID, "Per-dimension review")
	if _, err := f.prsvc.RequestReview(ctx, f.project.ID, pr.Number); err != nil {
		t.Fatalf("RequestReview: %v", err)
	}
	bob := seedReviewUser(t, ctx, f, "review-bob@example.com", "review-bob", "maintainer")
	svc := reviewSvc(f, &labelGate{label: "Experimental Reviewer"})

	// One person records different review kinds for the same head
	// (acceptance "一人不同 review kind 可记录"): scientific
	// changes_requested, then integrity comment — two rows.
	r1 := mustReview(t, ctx, svc, bob, f.project.ID, pr.Number, domain.ReviewKindScientific, domain.ReviewDecisionChangesRequested)
	if r1.Kind != domain.ReviewKindScientific || r1.Decision != domain.ReviewDecisionChangesRequested ||
		r1.ReviewedStateID != head1 || r1.Responsibility != "Experimental Reviewer" {
		t.Fatalf("scientific review = %+v, want kind=scientific decision=changes_requested head=%s responsibility set", r1, head1)
	}
	// The fact-based projection: review_required -> changes_requested,
	// atomically with the review row.
	if got := prState(t, ctx, f, pr.Number); got != domain.PullRequestStateChangesRequested {
		t.Fatalf("state after changes_requested = %s, want changes_requested", got)
	}
	r2 := mustReview(t, ctx, svc, bob, f.project.ID, pr.Number, domain.ReviewKindIntegrity, domain.ReviewDecisionComment)
	if r2.ReviewedStateID != head1 {
		t.Fatalf("integrity review head = %s, want %s", r2.ReviewedStateID, head1)
	}
	if got := prState(t, ctx, f, pr.Number); got != domain.PullRequestStateChangesRequested {
		t.Fatalf("comment moved the machine to %s, want changes_requested", got)
	}

	// A second decision for a kind the reviewer ALREADY recorded for this
	// head is the duplicate the unique key refuses (one decision per
	// person per kind per head).
	_, err := svc.SubmitReview(ctx, bob, f.project.ID, pr.Number, reviews.SubmitReviewInput{
		Kind:     domain.ReviewKindScientific,
		Decision: domain.ReviewDecisionComment,
		Body:     "still reviewing",
	})
	if !errors.Is(err, reviews.ErrAlreadyReviewed) {
		t.Fatalf("duplicate scientific decision error = %v, want ErrAlreadyReviewed", err)
	}

	list, err := svc.List(ctx, f.project.ID, pr.Number)
	if err != nil {
		t.Fatalf("ListReviews: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("review count = %d, want 2", len(list))
	}
	for i, got := range list {
		if got.ReviewedStateID != head1 {
			t.Fatalf("review[%d] head = %s, want %s", i, got.ReviewedStateID, head1)
		}
	}
	if list[0].Kind != domain.ReviewKindScientific || list[1].Kind != domain.ReviewKindIntegrity {
		t.Fatalf("list order wrong: %+v", list)
	}

	// The head advances; the explicit refresh opens a NEW round: the same
	// person may decide the same kind again, pinned to the new head —
	// stale round-1 decisions never leak into round 2.
	if _, err := f.prsvc.RequestReview(ctx, f.project.ID, pr.Number); err != nil {
		t.Fatalf("re-request review: %v", err)
	}
	head2 := f.commit(t, ctx, feature.ID, "finding", `{"summary":"found"}`)
	refreshed, err := f.prsvc.RefreshProposed(ctx, f.project.ID, pr.Number)
	if err != nil {
		t.Fatalf("RefreshProposed: %v", err)
	}
	if refreshed.ProposedStateID != head2 {
		t.Fatalf("refreshed head = %s, want %s", refreshed.ProposedStateID, head2)
	}
	r4 := mustReview(t, ctx, svc, bob, f.project.ID, pr.Number, domain.ReviewKindScientific, domain.ReviewDecisionApproved)
	if r4.ReviewedStateID != head2 {
		t.Fatalf("round-2 review head = %s, want %s", r4.ReviewedStateID, head2)
	}
	// An approval on the new head records but does NOT project (deciding
	// when the dimensions add up to an approval is T0604's required-
	// review calculation — deliberately not invented here).
	if got := prState(t, ctx, f, pr.Number); got != domain.PullRequestStateReviewRequired {
		t.Fatalf("state after approval = %s, want review_required", got)
	}
	list, err = svc.List(ctx, f.project.ID, pr.Number)
	if err != nil {
		t.Fatalf("ListReviews round 2: %v", err)
	}
	if len(list) != 3 || list[2].ReviewedStateID != head2 {
		t.Fatalf("round-2 list = %+v, want 3 reviews with the last pinned to %s", list, head2)
	}
}

func TestReviewPermissionsOverRealStores(t *testing.T) {
	ctx := testCtx(t)
	f := newPRFixture(t, ctx)
	f.commit(t, ctx, f.main.ID, "claim", `{"statement":"alpha"}`)
	feature := f.newFeature(t, ctx, "feature")
	f.commit(t, ctx, feature.ID, "hypothesis", `{"question_ref":"q1"}`)
	pr := f.openPR(t, ctx, feature.ID, "Permissions")
	if _, err := f.prsvc.RequestReview(ctx, f.project.ID, pr.Number); err != nil {
		t.Fatalf("RequestReview: %v", err)
	}
	bob := seedReviewUser(t, ctx, f, "perm-bob@example.com", "perm-bob", "maintainer")
	carol := seedReviewUser(t, ctx, f, "perm-carol@example.com", "perm-carol", "viewer")
	dave := seedReviewUser(t, ctx, f, "perm-dave@example.com", "perm-dave", "")

	// maintainer: unconditional allow — records even without a hook,
	// with an empty responsibility label.
	noHook := reviewSvc(f, nil)
	r := mustReview(t, ctx, noHook, bob, f.project.ID, pr.Number, domain.ReviewKindScientific, domain.ReviewDecisionComment)
	if r.Responsibility != "" {
		t.Fatalf("maintainer review responsibility = %q, want empty (no hook)", r.Responsibility)
	}

	// viewer: conditional — without a hook the condition cannot resolve,
	// and the verdict fails closed (docs/12).
	_, err := noHook.SubmitReview(ctx, carol, f.project.ID, pr.Number, reviews.SubmitReviewInput{
		Kind:     domain.ReviewKindIntegrity,
		Decision: domain.ReviewDecisionComment,
	})
	if !errors.Is(err, reviews.ErrForbidden) {
		t.Fatalf("viewer without hook error = %v, want ErrForbidden", err)
	}

	// viewer + hook resolving a label: allowed, the label recorded (the
	// reviewer is attributable under that responsibility).
	withHook := reviewSvc(f, &labelGate{label: "Domain Expert"})
	r = mustReview(t, ctx, withHook, carol, f.project.ID, pr.Number, domain.ReviewKindIntegrity, domain.ReviewDecisionComment)
	if r.Responsibility != "Domain Expert" {
		t.Fatalf("viewer review responsibility = %q, want the hook's label", r.Responsibility)
	}

	// viewer + hook resolving no label: the condition is unmet.
	_, err = reviewSvc(f, &labelGate{}).SubmitReview(ctx, carol, f.project.ID, pr.Number, reviews.SubmitReviewInput{
		Kind:     domain.ReviewKindIntegrity,
		Decision: domain.ReviewDecisionApproved,
	})
	if !errors.Is(err, reviews.ErrForbidden) {
		t.Fatalf("viewer with empty label error = %v, want ErrForbidden", err)
	}

	// viewer + hook error: the condition is unknowable — fails closed as
	// a store failure, never as a permission.
	_, err = reviewSvc(f, &labelGate{err: errors.New("resolver down")}).SubmitReview(ctx, carol, f.project.ID, pr.Number, reviews.SubmitReviewInput{
		Kind:     domain.ReviewKindIntegrity,
		Decision: domain.ReviewDecisionApproved,
	})
	if !errors.Is(err, reviews.ErrStore) {
		t.Fatalf("viewer with hook error = %v, want ErrStore", err)
	}

	// A non-member of the private project cannot even learn it exists:
	// the membership gate answers the existence-hiding project-not-found
	// before any role is decided (docs/45).
	_, err = withHook.SubmitReview(ctx, dave, f.project.ID, pr.Number, reviews.SubmitReviewInput{
		Kind:     domain.ReviewKindScientific,
		Decision: domain.ReviewDecisionComment,
	})
	if !errors.Is(err, projects.ErrProjectNotFound) {
		t.Fatalf("non-member error = %v, want ErrProjectNotFound", err)
	}
}

func TestReviewDatabaseGuardsAndTerminalRefusal(t *testing.T) {
	ctx := testCtx(t)
	f := newPRFixture(t, ctx)
	f.commit(t, ctx, f.main.ID, "claim", `{"statement":"alpha"}`)
	feature := f.newFeature(t, ctx, "feature")
	head1 := f.commit(t, ctx, feature.ID, "hypothesis", `{"question_ref":"q1"}`)
	pr := f.openPR(t, ctx, feature.ID, "Guards")
	if _, err := f.prsvc.RequestReview(ctx, f.project.ID, pr.Number); err != nil {
		t.Fatalf("RequestReview: %v", err)
	}
	bob := seedReviewUser(t, ctx, f, "guard-bob@example.com", "guard-bob", "maintainer")
	svc := reviewSvc(f, &labelGate{label: "Experimental Reviewer"})

	// The decision vocabulary is exactly approved/changes_requested/
	// comment (migration 00061): the pre-T0404 'commented' spelling and
	// every other word are rejected BY THE DATABASE, not just the service.
	_, err := f.pool.Exec(ctx, `
		INSERT INTO reviews (pull_request_id, reviewer_id, review_kind, decision, reviewed_state_id)
		VALUES ($1, $2, 'scientific', 'commented', $3)`,
		pr.ID, bob.ID, head1)
	if got := sqlState(t, err); got != "23514" {
		t.Fatalf("raw 'commented' insert SQLSTATE = %s, want 23514 (decision CHECK)", got)
	}
	// The kind vocabulary is scientific/integrity (docs/09 §5): rights
	// and ip are not review kinds written by this surface.
	_, err = f.pool.Exec(ctx, `
		INSERT INTO reviews (pull_request_id, reviewer_id, review_kind, decision, reviewed_state_id)
		VALUES ($1, $2, 'rights', 'approved', $3)`,
		pr.ID, bob.ID, head1)
	if got := sqlState(t, err); got != "23514" {
		t.Fatalf("raw 'rights' insert SQLSTATE = %s, want 23514 (kind CHECK)", got)
	}
	// The uniqueness is a database fact: one decision per person per kind
	// per head, even for a raw write.
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO reviews (pull_request_id, reviewer_id, review_kind, decision, reviewed_state_id)
		VALUES ($1, $2, 'scientific', 'approved', $3)`,
		pr.ID, bob.ID, head1); err != nil {
		t.Fatalf("raw first insert: %v", err)
	}
	_, err = f.pool.Exec(ctx, `
		INSERT INTO reviews (pull_request_id, reviewer_id, review_kind, decision, reviewed_state_id)
		VALUES ($1, $2, 'scientific', 'comment', $3)`,
		pr.ID, bob.ID, head1)
	if got := sqlState(t, err); got != "23505" {
		t.Fatalf("raw duplicate insert SQLSTATE = %s, want 23505 (unique per PR/reviewer/kind/head)", got)
	}
	// reviewed_state_id must name a real state: the FK guards the pin.
	_, err = f.pool.Exec(ctx, `
		INSERT INTO reviews (pull_request_id, reviewer_id, review_kind, decision, reviewed_state_id)
		VALUES ($1, $2, 'scientific', 'comment', '00000000-0000-0000-0000-000000000000')`,
		pr.ID, bob.ID)
	if got := sqlState(t, err); got != "23503" {
		t.Fatalf("raw dangling reviewed_state_id SQLSTATE = %s, want 23503 (FK)", got)
	}

	// A closed proposal accepts no further review records.
	if _, err := f.prsvc.Close(ctx, f.project.ID, pr.Number); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err = svc.SubmitReview(ctx, bob, f.project.ID, pr.Number, reviews.SubmitReviewInput{
		Kind:     domain.ReviewKindIntegrity,
		Decision: domain.ReviewDecisionComment,
	})
	var term *pullrequests.TerminalError
	if !errors.As(err, &term) || term.State != domain.PullRequestStateClosed {
		t.Fatalf("review on closed PR error = %v, want TerminalError(closed)", err)
	}

	// Merged is terminal the same way.
	pr2 := f.openPR(t, ctx, feature.ID, "Merged guard")
	if _, err := f.prsvc.RequestReview(ctx, f.project.ID, pr2.Number); err != nil {
		t.Fatalf("RequestReview pr2: %v", err)
	}
	if _, err := f.prsvc.SetState(ctx, f.project.ID, pr2.Number, domain.PullRequestStateApproved); err != nil {
		t.Fatalf("approved: %v", err)
	}
	if _, err := f.prsvc.SetState(ctx, f.project.ID, pr2.Number, domain.PullRequestStateMergeReady); err != nil {
		t.Fatalf("merge_ready: %v", err)
	}
	if _, err := f.prsvc.MarkMerged(ctx, f.project.ID, pr2.Number); err != nil {
		t.Fatalf("MarkMerged: %v", err)
	}
	_, err = svc.SubmitReview(ctx, bob, f.project.ID, pr2.Number, reviews.SubmitReviewInput{
		Kind:     domain.ReviewKindScientific,
		Decision: domain.ReviewDecisionApproved,
	})
	if !errors.As(err, &term) || term.State != domain.PullRequestStateMerged {
		t.Fatalf("review on merged PR error = %v, want TerminalError(merged)", err)
	}
}
