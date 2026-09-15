package integration

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/sqlc"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

// Task T0605: Release Manifest Builder — ReleaseStore.ListReleaseReviews
// over a REAL PostgreSQL, with the same store composition the API uses.
// Proves the review-record read end to end:
//
//   - the record of a released main state carries exactly the reviews of
//     the PRs that proposed states inside its lineage AND targeted main —
//     a PR proposing a state outside the lineage (an unmerged research
//     head) contributes nothing, and neither does a PR targeting another
//     branch even when its proposed state is inside the lineage;
//   - rows group by pull request, oldest review first;
//   - a state with no PRs at all yields an empty record (the release gate
//     refuses it — the store reads, it does not judge);
//   - a malformed id is an error, never a silent empty record.

const releaseStoreTaskID = "T0605"

// releaseStoreFixture seeds one private project owned by alice, its main
// branch and the state chain the release reads, and wires ReleaseStore
// over the real pool. PR/review rows are seeded through the canonical
// queries (the PR domain service is a later task; this task reads the
// rows it will write).
type releaseStoreFixture struct {
	pool  *pgxpool.Pool
	store *persistence.ReleaseStore
	alice domain.User
	// bob is the second reviewer: migration 00061's unique key allows
	// one decision per reviewer per kind per head, so a second
	// same-kind decision needs a second reviewer.
	bob domain.User
	// project is the seeded project the PRs belong to.
	project domain.Project
	// mainBranch is the project's main branch id.
	mainBranch string
	// genesis is the project's genesis state (lineage root).
	genesis string
	// head is the state committed on main — the released state.
	head string
	// researchHead is a state committed on a research branch forked from
	// head — outside head's lineage.
	researchHead string
	// researchBranch is the branch researchHead was committed on.
	researchBranch string
}

func newReleaseStoreFixture(t *testing.T, ctx context.Context) *releaseStoreFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), releaseStoreTaskID)
	alice, err := persistence.NewCredentialStore(pool).CreateWithPassword(
		ctx, "release-alice@example.com", "hash", "release-alice", "Alice")
	if err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	bob, err := persistence.NewCredentialStore(pool).CreateWithPassword(
		ctx, "release-bob@example.com", "hash", "release-bob", "Bob")
	if err != nil {
		t.Fatalf("seed bob: %v", err)
	}
	orgStore := persistence.NewOrgStore(pool)
	org, _, err := orgStore.CreateOrganization(ctx, domain.Organization{
		Slug: "release-fixture", Name: "Release Fixture",
	}, alice.ID, todayUTC())
	if err != nil {
		t.Fatalf("create fixture org: %v", err)
	}
	projectStore := persistence.NewProjectStore(pool)
	project, _, err := projectStore.CreateProject(ctx, domain.Project{
		OrganizationID:  &org.ID,
		Slug:            "release-project",
		Name:            "Release Project",
		Purpose:         "fixture purpose",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, alice.ID)
	if err != nil {
		t.Fatalf("create fixture project: %v", err)
	}

	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	stateStore := persistence.NewStateStore(pool)
	statesSvc := states.NewService(stateStore, newCommitGuard(t))
	svc := rsg.NewService(rsg.Deps{
		Projects:  projects.NewService(projectStore, orgStore, authz.NewMatrixEngine()),
		Branches:  branches.NewService(persistence.NewBranchStore(pool)),
		States:    statesSvc,
		Latest:    stateStore,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
		Events:    events.Recorder{},
	})
	mainBranch, err := svc.CreateBranch(ctx, alice, project.ID, rsg.CreateBranchInput{
		Name:       "main",
		BaseRef:    "",
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create main branch: %v", err)
	}
	var genesis string
	if err := pool.QueryRow(ctx,
		`SELECT id FROM project_states WHERE project_id = $1 AND parent_state_id IS NULL`, project.ID).Scan(&genesis); err != nil {
		t.Fatalf("read genesis state: %v", err)
	}
	res, err := svc.CreateObject(ctx, alice, project.ID, mainBranch.ID, rsg.CreateObjectInput{
		ObjectType: "experiment",
		Payload:    []byte(`{"temperature_k":273.15}`),
	})
	if err != nil {
		t.Fatalf("CreateObject on main: %v", err)
	}
	head := res.Version.StateID

	researchBranch, err := svc.CreateBranch(ctx, alice, project.ID, rsg.CreateBranchInput{
		Name:       "research-path",
		BaseRef:    head,
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create research branch: %v", err)
	}
	researchRes, err := svc.CreateObject(ctx, alice, project.ID, researchBranch.ID, rsg.CreateObjectInput{
		ObjectType: "experiment",
		Payload:    []byte(`{"temperature_k":300.0}`),
	})
	if err != nil {
		t.Fatalf("CreateObject on research branch: %v", err)
	}

	return &releaseStoreFixture{
		pool:           pool,
		store:          persistence.NewReleaseStore(pool),
		alice:          alice,
		bob:            bob,
		project:        project,
		mainBranch:     mainBranch.ID,
		genesis:        genesis,
		head:           head,
		researchHead:   researchRes.Version.StateID,
		researchBranch: researchBranch.ID,
	}
}

// createPR seeds one pull_request row through the canonical query and
// returns its id.
func (f *releaseStoreFixture) createPR(t *testing.T, ctx context.Context, number int64, sourceBranch, targetBranch, baseState, proposedState string) string {
	t.Helper()
	q := sqlc.New(f.pool)
	row, err := q.CreatePullRequest(ctx, sqlc.CreatePullRequestParams{
		ProjectID:       parseUUIDOrDie(f.project.ID),
		Number:          number,
		SourceBranchID:  parseUUIDOrDie(sourceBranch),
		TargetBranchID:  parseUUIDOrDie(targetBranch),
		BaseStateID:     parseUUIDOrDie(baseState),
		ProposedStateID: parseUUIDOrDie(proposedState),
		Title:           "fixture PR",
		Body:            "",
		CreatedBy:       parseUUIDOrDie(f.alice.ID),
	})
	if err != nil {
		t.Fatalf("CreatePullRequest: %v", err)
	}
	return pgUUIDTextTest(row.ID)
}

// addReview seeds one review row on a PR. reviewedState is the head the
// review evaluates (migration 00061: reviews pin reviewed_state_id to
// the PR's proposed head; the fixture passes it explicitly).
func (f *releaseStoreFixture) addReview(t *testing.T, ctx context.Context, prID, reviewerID, reviewedState, kind, decision string) {
	t.Helper()
	q := sqlc.New(f.pool)
	if _, err := q.CreateReview(ctx, sqlc.CreateReviewParams{
		PullRequestID:   parseUUIDOrDie(prID),
		ReviewerID:      parseUUIDOrDie(reviewerID),
		ReviewKind:      kind,
		Decision:        decision,
		ReviewedStateID: parseUUIDOrDie(reviewedState),
		Responsibility:  "",
		Body:            "",
	}); err != nil {
		t.Fatalf("CreateReview: %v", err)
	}
}

// TestListReleaseReviewsLineageAndTargetFilter is the end-to-end record
// read: only PRs that targeted main AND proposed a state inside the
// released lineage contribute.
func TestListReleaseReviewsLineageAndTargetFilter(t *testing.T) {
	ctx := context.Background()
	f := newReleaseStoreFixture(t, ctx)

	// PR 1: research-path → main, proposing head — head IS its own
	// lineage member, so its reviews are the release's review record.
	pr1 := f.createPR(t, ctx, 1, f.researchBranch, f.mainBranch, f.genesis, f.head)
	f.addReview(t, ctx, pr1, f.alice.ID, f.head, "scientific", "approved")
	f.addReview(t, ctx, pr1, f.alice.ID, f.head, "integrity", "approved")
	// The second scientific decision needs a second reviewer: migration
	// 00061 refuses two same-kind decisions by one person on one head.
	f.addReview(t, ctx, pr1, f.bob.ID, f.head, "scientific", "changes_requested")

	// PR 2: research-path → main, proposing researchHead — outside the
	// released head's lineage (it is a state of a forked branch): its
	// review must NOT appear.
	pr2 := f.createPR(t, ctx, 2, f.researchBranch, f.mainBranch, f.head, f.researchHead)
	f.addReview(t, ctx, pr2, f.alice.ID, f.researchHead, "scientific", "approved")

	// PR 3: research-path → research-path, proposing head — head is in
	// the lineage but the PR did not target main: its review must NOT
	// appear.
	pr3 := f.createPR(t, ctx, 3, f.researchBranch, f.researchBranch, f.genesis, f.head)
	f.addReview(t, ctx, pr3, f.alice.ID, f.head, "scientific", "approved")

	// PR 4: research-path → main, proposing genesis — the root of the
	// lineage: it appears too, and groups separately.
	pr4 := f.createPR(t, ctx, 4, f.researchBranch, f.mainBranch, f.genesis, f.genesis)
	f.addReview(t, ctx, pr4, f.alice.ID, f.genesis, "integrity", "comment")

	records, err := f.store.ListReleaseReviews(ctx, f.head, f.mainBranch)
	if err != nil {
		t.Fatalf("ListReleaseReviews: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2 (PRs 1 and 4): %+v", len(records), records)
	}
	// Ordered by PR number.
	if records[0].PullRequestNumber != 1 || records[1].PullRequestNumber != 4 {
		t.Fatalf("record order wrong: %+v", records)
	}
	if records[0].ProposedStateID != f.head || records[1].ProposedStateID != f.genesis {
		t.Fatalf("proposed state ids wrong: %+v", records)
	}
	if len(records[0].Reviews) != 3 {
		t.Fatalf("PR 1 review count = %d, want 3", len(records[0].Reviews))
	}
	// The three reviews arrive in insertion order (scientific,
	// integrity, scientific); the query orders them by (created_at, id)
	// — all at now(), so the order is id order, which is the order
	// above only by chance; assert the SET instead of the order.
	kinds := map[string]int{}
	for _, r := range records[0].Reviews {
		kinds[r.ReviewKind+"|"+r.Decision]++
	}
	if kinds["scientific|approved"] != 1 || kinds["integrity|approved"] != 1 || kinds["scientific|changes_requested"] != 1 {
		t.Fatalf("PR 1 review set wrong: %+v", records[0].Reviews)
	}
	if len(records[1].Reviews) != 1 || records[1].Reviews[0].ReviewKind != "integrity" {
		t.Fatalf("PR 4 reviews wrong: %+v", records[1])
	}
}

// TestListReleaseReviewsEmptyLineage: a state with no PRs at all yields an
// empty record, not an error — the release gate is what refuses it.
func TestListReleaseReviewsEmptyLineage(t *testing.T) {
	ctx := context.Background()
	f := newReleaseStoreFixture(t, ctx)
	records, err := f.store.ListReleaseReviews(ctx, f.head, f.mainBranch)
	if err != nil {
		t.Fatalf("ListReleaseReviews: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("records = %+v, want none", records)
	}
}

// TestListReleaseReviewsMalformedIDs: a malformed id is an error, never a
// silent empty record.
func TestListReleaseReviewsMalformedIDs(t *testing.T) {
	ctx := context.Background()
	f := newReleaseStoreFixture(t, ctx)
	if _, err := f.store.ListReleaseReviews(ctx, "not-a-uuid", f.mainBranch); err == nil {
		t.Fatalf("expected an error for a malformed state id")
	}
	if _, err := f.store.ListReleaseReviews(ctx, f.head, "not-a-uuid"); err == nil {
		t.Fatalf("expected an error for a malformed main branch id")
	}
}
