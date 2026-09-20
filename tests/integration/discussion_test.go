// Task T0811 required tests — the discussion surface over real PostgreSQL.
//
// The unit suites pin the command's decisions
// (internal/application/discussions: the per-kind field rules, the write
// authorization, the tombstone and target rules) and the transport's wiring
// (cmd/api/discussionhttp: the routes, the session guard, the envelope). This
// file pins what only a real database and the whole composed path can settle,
// each over real rows:
//
//  1. A THREAD EXISTS ON ALL THREE SURFACES. A project, a published knowledge
//     object (by PID) and a research pull request (by number) each carry a
//     conversation through the real route, and the thread is readable back by
//     id and by its target filter. The target rule is fail-closed in both
//     directions: a target that does not exist, and a target that exists in
//     ANOTHER project, are answered the one not-found.
//
//  2. A COMMENT DOES NOT MOVE SCIENTIFIC STATE. This is the acceptance the
//     architectural ruling asks for, and it is asserted as COUNTS, not as
//     "no error": after opening threads, writing comments and withdrawing
//     one, the rows in scientific_object_versions, relation_versions,
//     state_commits and project_states are exactly the rows that were there
//     before, and the Contribution Ledger has not grown either. The counter is
//     proven live in the same test twice over: the ledger is drained through
//     the real dispatcher and projector first (so a state commit in this world
//     WOULD land a contribution_events row), and the positive control at the
//     end runs a HYPOTHESIS promotion, which does move the counts. An
//     instrument that cannot see movement cannot witness stillness.
//
//  3. ALL THREE PROMOTION PATHS WORK, each through the surface that owns the
//     created object: an Issue row (issues), a Hypothesis by a real state
//     commit (a new scientific_object_version, object_type hypothesis) and an
//     external-evidence proposal stored review_state='unreviewed'.
//
//  4. PROVENANCE IS A ROW, NOT A RELATION. The promotion records the comment
//     and its author, the created object can be traced back through the
//     reverse read by promoted_ref, and relation_versions gains NO row — a
//     discussion has no version to be an endpoint of.
//
//  5. AUTHORIZATION IS FAIL-CLOSED. An anonymous promotion is refused before
//     the command runs; a signed-in non-member may COMMENT on a project it
//     may read but may not PROMOTE; a member whose role the matrix denies is
//     refused too; and a project the actor may not read answers the
//     existence-hiding not-found to every route, promotion included.
//
//  6. A WITHDRAWAL IS A TOMBSTONE. The body stops being served, the row
//     stays and still holds the text, the tombstone names who withdrew it and
//     when, a second withdrawal is refused, withdrawing somebody else's
//     comment is refused, and a withdrawn comment cannot be promoted.
//
//  7. THE PER-KIND FIELD RULES REFUSE RATHER THAN IGNORE: a declaration that
//     belongs to another kind is refused, not silently dropped, and the
//     evidence write path's own refusal of an unnamed literature unit is not
//     swallowed by the promotion.
//
// The composition is cmd/api/main.go's (newKnowledgeWorldFor registers the
// discussion surface the way main.go does), so these tests cannot pass
// against a world production does not build.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	appcontribution "github.com/lichman0405/post/internal/application/contribution"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/contribution"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
)

const discussionTaskID = "T0811"

// --------------------------------------------------------------------------
// The world

// discussionWorld is the discussion surface as production wires it, over one
// public project (a review-passed, published object, plus the question the
// hypothesis promotion answers) and one private project (the existence-hiding
// case).
type discussionWorld struct {
	*knowledgeWorld

	// pub is the public project every ordinary case runs in; alice owns it.
	pub *knowledgeProject
	// secret is a private project in the same organization. Bob and carol
	// are not members of either project unless a case adds them.
	secret *knowledgeProject

	// questionID is the research_question object the hypothesis promotion
	// answers — migration 00040 resolves hypothesis.question_id against an
	// existing research_question of the same project.
	questionID string
	// questionVersion is that question's version: a version of the project,
	// which is what the evidence side of an assertion must be.
	questionVersion string
	// versionID is the published object version a knowledge thread hangs on,
	// and pid is the publication's identifier (the target's own addressing
	// scheme).
	versionID string
	pid       string
	// prNumber is the number of the research pull request the publication's
	// reviews were approved on (seedReviewedVersion seeds it), which is the
	// PR target's addressing scheme.
	prNumber int64
	// relationsBefore is the project's relation_versions count as the fixture
	// leaves it, so a case can assert a promotion added no edge without
	// assuming the world started empty.
	relationsBefore int
}

func newDiscussionWorld(t *testing.T, ctx context.Context) *discussionWorld {
	t.Helper()
	w := newKnowledgeWorldFor(t, ctx, discussionTaskID)
	w.signups(t)

	pub := w.newProject(t, ctx, "disc-alpha", "public")
	secret := w.newProject(t, ctx, "disc-omega", "private")

	questionID, questionVersion, _ := w.seedVersion(t, ctx, pub.id, pub.probe,
		"What is the CO2 uptake of MOF-5 at 298 K")
	// The knowledge target: a version whose reviews are approved and which
	// is then published on the contract's route (the ordinary product path
	// for both), so the thread hangs on a REAL publication.
	_, versionID, _ := w.seedReviewedVersion(t, ctx, pub, "MOF-5 CO2 uptake at 298 K", 1, true)
	pid := w.mustKnowledgePublish(t, w.alice, pub.id,
		knowledgePublishBody(t, versionID, "v1.0", ""), "").PID

	// One relation, created through the RSG write path, so relation_versions
	// holds a row of this project BEFORE any case runs. Without it "the
	// relation counter did not move" would be a statement about an empty
	// table, which the empty table satisfies whatever the write does.
	if _, err := w.svc.CreateRelation(ctx, domain.User{ID: w.aliceID}, pub.id, pub.probe, rsg.CreateRelationInput{
		RelationType:          "addresses_question",
		SourceObjectVersionID: versionID,
		TargetObjectVersionID: questionVersion,
		Payload:               json.RawMessage(`{"note":"the fixture's one relation"}`),
	}); err != nil {
		t.Fatalf("seed the fixture's relation: %v", err)
	}

	return &discussionWorld{
		knowledgeWorld:  w,
		pub:             pub,
		secret:          secret,
		questionID:      questionID,
		questionVersion: questionVersion,
		versionID:       versionID,
		pid:             pid,
		prNumber:        1,
		relationsBefore: countRows(t, ctx, w.pool,
			`SELECT count(*) FROM relation_versions rv JOIN relations r ON r.id = rv.relation_id
			  WHERE r.project_id = $1`, pub.id),
	}
}

// addMember grants a project role by writing the membership row directly, the
// recipe the permission suites use (tests/integration/project_shell_test.go,
// asset_governance_test.go): V1 has no product route that adds a member, and
// what this task needs is the MEMBERSHIP, not the route that would have made
// it.
func (d *discussionWorld) addMember(t *testing.T, ctx context.Context, projectID, userID, role string) {
	t.Helper()
	if _, err := d.pool.Exec(ctx,
		`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, $3)`,
		projectID, userID, role); err != nil {
		t.Fatalf("add %s as %s to %s: %v", userID, role, projectID, err)
	}
}

// --------------------------------------------------------------------------
// Wire shapes

type discussionThreadWire struct {
	ID            string     `json:"id"`
	ProjectID     string     `json:"project_id"`
	TargetType    string     `json:"target_type"`
	TargetID      string     `json:"target_id"`
	CreatedBy     string     `json:"created_by"`
	CreatedAt     time.Time  `json:"created_at"`
	CommentCount  int64      `json:"comment_count"`
	LastCommentAt *time.Time `json:"last_comment_at"`
}

// discussionCommentWire is the wire comment. Body is a POINTER because the
// tombstone's whole meaning is that the field is null: a `string` here would
// read a withheld body as "" and the test could not tell the two apart.
type discussionCommentWire struct {
	ID        string     `json:"id"`
	ThreadID  string     `json:"thread_id"`
	ProjectID string     `json:"project_id"`
	Body      *string    `json:"body"`
	CreatedBy string     `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
	Deleted   bool       `json:"deleted"`
	DeletedAt *time.Time `json:"deleted_at"`
	DeletedBy *string    `json:"deleted_by"`
}

type discussionThreadReadWire struct {
	Thread   discussionThreadWire    `json:"thread"`
	Comments []discussionCommentWire `json:"comments"`
}

type discussionThreadListWire struct {
	Threads []discussionThreadWire `json:"threads"`
}

type discussionPromotionWire struct {
	ID           string    `json:"id"`
	ProjectID    string    `json:"project_id"`
	ThreadID     string    `json:"thread_id"`
	CommentID    string    `json:"comment_id"`
	PromotedKind string    `json:"promoted_kind"`
	PromotedRef  string    `json:"promoted_ref"`
	PromotedBy   string    `json:"promoted_by"`
	PromotedAt   time.Time `json:"promoted_at"`
}

// discussionPromoteWire is the 201 of one promotion: the provenance, plus
// exactly one kind-specific payload.
type discussionPromoteWire struct {
	Promotion discussionPromotionWire `json:"promotion"`
	Thread    discussionThreadWire    `json:"thread"`
	Comment   discussionCommentWire   `json:"comment"`
	Issue     *struct {
		ID        string `json:"id"`
		Number    int64  `json:"number"`
		IssueType string `json:"issue_type"`
		Title     string `json:"title"`
		Body      string `json:"body"`
		State     string `json:"state"`
	} `json:"issue"`
	ScientificObject *struct {
		ID             string `json:"id"`
		ObjectType     string `json:"object_type"`
		VersionID      string `json:"version_id"`
		VersionNo      int    `json:"version_no"`
		StateID        string `json:"state_id"`
		Title          string `json:"title"`
		LifecycleState string `json:"lifecycle_state"`
	} `json:"scientific_object"`
	EvidenceAssertion *struct {
		ID             string `json:"id"`
		StateID        string `json:"state_id"`
		Relation       string `json:"relation_type"`
		EvidenceType   string `json:"evidence_type"`
		ReasoningNote  string `json:"reasoning_note"`
		ReviewState    string `json:"review_state"`
		EvidenceOrigin string `json:"evidence_origin"`
		CreatedBy      string `json:"created_by"`
	} `json:"evidence_assertion"`
}

type discussionPromotionListWire struct {
	Promotions []discussionPromoteWire `json:"promotions"`
}

// --------------------------------------------------------------------------
// Requests

func (d *discussionWorld) threadsURL(projectID string) string {
	return "/api/v1/projects/" + projectID + "/discussions"
}

func (d *discussionWorld) threadURL(projectID, threadID string) string {
	return d.threadsURL(projectID) + "/" + threadID
}

func (d *discussionWorld) commentsURL(projectID, threadID string) string {
	return d.threadURL(projectID, threadID) + "/comments"
}

func (d *discussionWorld) commentURL(projectID, threadID, commentID string) string {
	return d.commentsURL(projectID, threadID) + "/" + commentID
}

func (d *discussionWorld) openThread(t *testing.T, uc *testUserClient, projectID, targetType, targetID, body string) *http.Response {
	t.Helper()
	return uc.do(t, http.MethodPost, d.threadsURL(projectID), fmt.Sprintf(
		`{"target_type":%q,"target_id":%q,"body":%q}`, targetType, targetID, body))
}

// mustOpenThread requires the 201 and returns the thread with its opening
// comment.
func (d *discussionWorld) mustOpenThread(t *testing.T, uc *testUserClient, projectID, targetType, targetID, body string) discussionThreadReadWire {
	t.Helper()
	resp := d.openThread(t, uc, projectID, targetType, targetID, body)
	mustStatus(t, resp, http.StatusCreated)
	return decodeFlow[discussionThreadReadWire](t, resp)
}

func (d *discussionWorld) listThreads(t *testing.T, uc *testUserClient, projectID, targetType, targetID string) *http.Response {
	t.Helper()
	return uc.do(t, http.MethodGet, fmt.Sprintf("%s?target_type=%s&target_id=%s",
		d.threadsURL(projectID), targetType, targetID), "")
}

// mustListTheads requires the 200 and decodes the list.
func (d *discussionWorld) mustListThreads(t *testing.T, uc *testUserClient, projectID, targetType, targetID string) discussionThreadListWire {
	t.Helper()
	resp := d.listThreads(t, uc, projectID, targetType, targetID)
	mustStatus(t, resp, http.StatusOK)
	return decodeFlow[discussionThreadListWire](t, resp)
}

// mustReadThread requires the 200 and decodes the thread with its comments.
func (d *discussionWorld) mustReadThread(t *testing.T, uc *testUserClient, projectID, threadID string) discussionThreadReadWire {
	t.Helper()
	resp := uc.do(t, http.MethodGet, d.threadURL(projectID, threadID), "")
	mustStatus(t, resp, http.StatusOK)
	return decodeFlow[discussionThreadReadWire](t, resp)
}

func (d *discussionWorld) addComment(t *testing.T, uc *testUserClient, projectID, threadID, body string) *http.Response {
	t.Helper()
	return uc.do(t, http.MethodPost, d.commentsURL(projectID, threadID),
		fmt.Sprintf(`{"body":%q}`, body))
}

// mustComment requires the 201 and returns the stored comment.
func (d *discussionWorld) mustComment(t *testing.T, uc *testUserClient, projectID, threadID, body string) discussionCommentWire {
	t.Helper()
	resp := d.addComment(t, uc, projectID, threadID, body)
	mustStatus(t, resp, http.StatusCreated)
	return decodeFlow[discussionCommentWire](t, resp)
}

func (d *discussionWorld) withdrawComment(t *testing.T, uc *testUserClient, projectID, threadID, commentID string) *http.Response {
	t.Helper()
	return uc.do(t, http.MethodDelete, d.commentURL(projectID, threadID, commentID), "")
}

func (d *discussionWorld) promote(t *testing.T, uc *testUserClient, projectID, threadID, commentID, body string) *http.Response {
	t.Helper()
	return uc.do(t, http.MethodPost, d.commentURL(projectID, threadID, commentID)+"/promotions", body)
}

// mustPromote requires the 201 and returns the promotion with its created
// object.
func (d *discussionWorld) mustPromote(t *testing.T, uc *testUserClient, projectID, threadID, commentID, body string) discussionPromoteWire {
	t.Helper()
	resp := d.promote(t, uc, projectID, threadID, commentID, body)
	mustStatus(t, resp, http.StatusCreated)
	return decodeFlow[discussionPromoteWire](t, resp)
}

// mustPromotionsForRef requires the 200 of the reverse provenance read: what
// produced the object this ref names.
func (d *discussionWorld) mustPromotionsForRef(t *testing.T, uc *testUserClient, projectID, ref string) discussionPromotionListWire {
	t.Helper()
	resp := uc.do(t, http.MethodGet, d.threadsURL(projectID)+"/promotions?ref="+ref, "")
	mustStatus(t, resp, http.StatusOK)
	return decodeFlow[discussionPromotionListWire](t, resp)
}

// mustPromotionByID requires the 200 of the provenance read by promotion id.
func (d *discussionWorld) mustPromotionByID(t *testing.T, uc *testUserClient, projectID, promotionID string) discussionPromoteWire {
	t.Helper()
	resp := uc.do(t, http.MethodGet, d.threadsURL(projectID)+"/promotions/"+promotionID, "")
	mustStatus(t, resp, http.StatusOK)
	return decodeFlow[discussionPromoteWire](t, resp)
}

// hypothesisBody is the promotion declaration the hypothesis cases send.
func (d *discussionWorld) hypothesisBody(branchID string) string {
	return fmt.Sprintf(`{"kind":"hypothesis","branch_id":%q,"hypothesis":{"question_id":%q}}`,
		branchID, d.questionID)
}

// threadComment is the shape every promotion case starts from: a thread whose
// second comment is the proposal.
type threadComment struct {
	threadID  string
	commentID string
	body      string
}

// proposalThread opens a thread on the project target and appends one
// comment by the given author, returning the pair the promotion routes need.
func (d *discussionWorld) proposalThread(t *testing.T, uc *testUserClient, body string) threadComment {
	t.Helper()
	opened := d.mustOpenThread(t, uc, d.pub.id, "project", d.pub.id, "opening the conversation")
	comment := d.mustComment(t, uc, d.pub.id, opened.Thread.ID, body)
	return threadComment{threadID: opened.Thread.ID, commentID: comment.ID, body: body}
}

// --------------------------------------------------------------------------
// Row counts

// stateCounts is the scientific-state surface a plain comment must leave
// untouched. The tables are read as one struct so a case compares one
// snapshot with another and cannot compare a table it forgot to read.
type stateCounts struct {
	objectVersions     int
	relationVersions   int
	stateCommits       int
	projectStates      int
	contributionEvents int
}

// projectVersions counts the project's object versions. The join is not
// decoration: scientific_object_versions carries no project_id of its own
// (00005) — the project lives on the object — so a version count per project
// is a join, and writing the query the other way is a compile-time-valid
// mistake the database refuses.
func (d *discussionWorld) projectVersions(t *testing.T, ctx context.Context) int {
	t.Helper()
	return countRows(t, ctx, d.pool,
		`SELECT count(*) FROM scientific_object_versions v
		   JOIN scientific_objects o ON o.id = v.object_id
		  WHERE o.project_id = $1`, d.pub.id)
}

func readStateCounts(t *testing.T, ctx context.Context, d *discussionWorld) stateCounts {
	t.Helper()
	return stateCounts{
		objectVersions:     countRows(t, ctx, d.pool, `SELECT count(*) FROM scientific_object_versions`),
		relationVersions:   countRows(t, ctx, d.pool, `SELECT count(*) FROM relation_versions`),
		stateCommits:       countRows(t, ctx, d.pool, `SELECT count(*) FROM state_commits`),
		projectStates:      countRows(t, ctx, d.pool, `SELECT count(*) FROM project_states`),
		contributionEvents: countRows(t, ctx, d.pool, `SELECT count(*) FROM contribution_events`),
	}
}

// drain runs the two hops a state commit travels to reach the Contribution
// Ledger: the real outbox publisher, then the real projection (repeated until
// a pass projects nothing). It is what makes the ledger counter non-vacuous —
// without it a commit's ledger row would simply not exist yet, and "the count
// did not move" would be true of every write in the world.
func (d *discussionWorld) drain(t *testing.T, ctx context.Context) {
	t.Helper()
	dispatcher := events.NewDispatcher(d.pool, events.WithLogger(outboxTestLogger()))
	if _, err := dispatcher.RunOnce(ctx); err != nil {
		t.Fatalf("outbox publish pass: %v", err)
	}
	projector := appcontribution.NewLedgerProjector(
		contribution.NewLedgerStore(d.pool), appcontribution.WithLedgerLogger(outboxTestLogger()))
	for {
		batch, err := projector.RunOnce(ctx)
		if err != nil {
			t.Fatalf("ledger projection pass: %v", err)
		}
		if batch.Projected == 0 {
			return
		}
	}
}

// --------------------------------------------------------------------------
// 1. The three surfaces

func TestDiscussionThreadsOnProjectKnowledgeAndPR(t *testing.T) {
	ctx := testCtx(t)
	d := newDiscussionWorld(t, ctx)

	// (a) the project itself: the target id is the project's own uuid.
	project := d.mustOpenThread(t, d.bob, d.pub.id, "project", d.pub.id,
		"should the uptake campaign be re-run at 273 K?")
	if project.Thread.TargetType != "project" || project.Thread.TargetID != d.pub.id {
		t.Errorf("project thread target = %s/%s, want project/%s",
			project.Thread.TargetType, project.Thread.TargetID, d.pub.id)
	}
	if project.Thread.CreatedBy != d.bobID {
		t.Errorf("project thread created_by = %s, want bob %s", project.Thread.CreatedBy, d.bobID)
	}
	if len(project.Comments) != 1 || project.Comments[0].Body == nil ||
		*project.Comments[0].Body != "should the uptake campaign be re-run at 273 K?" {
		t.Errorf("opening comment = %+v, want the text it was opened with", project.Comments)
	}

	// (b) a published knowledge object: the target id is the PID, the
	// identity the public read resolves — not an internal version id.
	knowledge := d.mustOpenThread(t, d.alice, d.pub.id, "knowledge", d.pid,
		"the isotherm in figure 2 needs an error bar")
	if knowledge.Thread.TargetType != "knowledge" || knowledge.Thread.TargetID != d.pid {
		t.Errorf("knowledge thread target = %s/%s, want knowledge/%s",
			knowledge.Thread.TargetType, knowledge.Thread.TargetID, d.pid)
	}

	// (c) a research pull request: the target id is the per-project number
	// every PR route displays.
	pr := d.mustOpenThread(t, d.alice, d.pub.id, "pull_request",
		fmt.Sprintf("%d", d.prNumber), "the pin is stale after the rebase")
	if pr.Thread.TargetType != "pull_request" || pr.Thread.TargetID != fmt.Sprintf("%d", d.prNumber) {
		t.Errorf("PR thread target = %s/%s, want pull_request/%d",
			pr.Thread.TargetType, pr.Thread.TargetID, d.prNumber)
	}

	// The threads are readable back by id, with their comments.
	read := d.mustReadThread(t, d.bob, d.pub.id, knowledge.Thread.ID)
	if read.Thread.ID != knowledge.Thread.ID || len(read.Comments) != 1 {
		t.Errorf("thread read = %+v, want the thread and its one comment", read)
	}

	// The list is filtered by target: the knowledge thread lists, the
	// project's thread does not.
	listed := d.mustListThreads(t, d.bob, d.pub.id, "knowledge", d.pid)
	if len(listed.Threads) != 1 || listed.Threads[0].ID != knowledge.Thread.ID {
		t.Errorf("knowledge target list = %+v, want exactly the knowledge thread", listed.Threads)
	}
	if listed.Threads[0].CommentCount != 1 {
		t.Errorf("listed thread comment_count = %d, want 1", listed.Threads[0].CommentCount)
	}
	// A second comment moves the list's count — the list read is live.
	d.mustComment(t, d.alice, d.pub.id, knowledge.Thread.ID, "second thought")
	listed = d.mustListThreads(t, d.bob, d.pub.id, "knowledge", d.pid)
	if listed.Threads[0].CommentCount != 2 {
		t.Errorf("listed thread comment_count = %d after a second comment, want 2", listed.Threads[0].CommentCount)
	}

	// The target rule is fail-closed: a target that does not exist and a
	// target of ANOTHER project answer the same not-found, so a thread is
	// never a way to learn that a foreign object exists.
	for _, tc := range []struct {
		name, projectID, targetType, targetID string
	}{
		{"unknown knowledge pid", d.pub.id, "knowledge", "00000000000000000000000000"},
		{"knowledge pid of another project", d.secret.id, "knowledge", d.pid},
		{"PR number of another project", d.secret.id, "pull_request", fmt.Sprintf("%d", d.prNumber)},
		{"project target that is not this project", d.pub.id, "project", d.secret.id},
		{"unknown PR number", d.pub.id, "pull_request", "424242"},
	} {
		resp := d.openThread(t, d.alice, tc.projectID, tc.targetType, tc.targetID, "into the void")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404: %s", tc.name, resp.StatusCode, readAll(t, resp))
			continue
		}
		mustEnvelope(t, resp, "DISCUSSION_TARGET_NOT_FOUND")
	}
}

// --------------------------------------------------------------------------
// 2. A comment does not move scientific state

func TestDiscussionCommentsDoNotMoveScientificState(t *testing.T) {
	ctx := testCtx(t)
	d := newDiscussionWorld(t, ctx)

	// The ledger is drained FIRST, so the counter is live: whatever this
	// world committed to reach its seeded state has already landed its
	// contribution_events rows, and there is no lag left to explain a
	// still-standing count.
	d.drain(t, ctx)
	before := readStateCounts(t, ctx, d)
	if before.contributionEvents == 0 {
		t.Fatal("the ledger is empty after draining a seeded world — " +
			"a count that cannot move cannot witness that nothing moved")
	}
	if before.objectVersions == 0 || before.relationVersions == 0 || before.stateCommits == 0 {
		t.Fatalf("the world has no scientific rows to hold still: %+v", before)
	}

	// The whole conversation path: open threads on all three surfaces, write
	// comments by two actors, withdraw one.
	opened := d.mustOpenThread(t, d.bob, d.pub.id, "project", d.pub.id, "should we re-run at 273 K?")
	d.mustComment(t, d.alice, d.pub.id, opened.Thread.ID, "yes, and at 283 K too")
	third := d.mustComment(t, d.bob, d.pub.id, opened.Thread.ID, "the isotherm is already at 283 K")
	resp := d.withdrawComment(t, d.bob, d.pub.id, opened.Thread.ID, third.ID)
	mustStatus(t, resp, http.StatusOK)

	knowledge := d.mustOpenThread(t, d.alice, d.pub.id, "knowledge", d.pid, "figure 2 error bar")
	d.mustComment(t, d.bob, d.pub.id, knowledge.Thread.ID, "agreed")
	pr := d.mustOpenThread(t, d.alice, d.pub.id, "pull_request", fmt.Sprintf("%d", d.prNumber), "pin is stale")
	d.mustComment(t, d.bob, d.pub.id, pr.Thread.ID, "rebase again")

	// The dispatcher and the projector run again: if any of those writes had
	// produced a domain event, this is where it would become a ledger row.
	d.drain(t, ctx)
	after := readStateCounts(t, ctx, d)
	if after != before {
		t.Errorf("scientific state moved while only discussion rows were written:\nbefore: %+v\nafter:  %+v", before, after)
	}
	// The rows the conversation DID write, named so the equality above
	// cannot be satisfied by the conversation not having happened at all:
	// three threads (project, knowledge, PR) and their seven comments (three
	// on the project thread, two on each of the others).
	if n := countRows(t, ctx, d.pool,
		`SELECT count(*) FROM discussion_threads WHERE project_id = $1`, d.pub.id); n != 3 {
		t.Errorf("discussion_threads rows = %d, want the 3 threads this test opened", n)
	}
	if n := countRows(t, ctx, d.pool,
		`SELECT count(*) FROM discussion_comments WHERE project_id = $1`, d.pub.id); n != 7 {
		t.Errorf("discussion_comments rows = %d, want the 7 comments this test wrote", n)
	}

	// The positive control: a promotion to a Hypothesis commits state through
	// the RSG write path, and the SAME counters see it move. Without this,
	// "nothing moved" would be indistinguishable from "nothing is counted".
	proposal := d.proposalThread(t, d.alice, "MOF-5 maximizes CO2 uptake at 298 K")
	d.mustPromote(t, d.alice, d.pub.id, proposal.threadID, proposal.commentID,
		d.hypothesisBody(d.pub.probe))
	d.drain(t, ctx)
	control := readStateCounts(t, ctx, d)
	if control.objectVersions <= after.objectVersions {
		t.Errorf("scientific_object_versions did not grow on a hypothesis promotion: %d then %d — "+
			"the counter cannot see movement, so it cannot witness stillness",
			after.objectVersions, control.objectVersions)
	}
	if control.stateCommits <= after.stateCommits {
		t.Errorf("state_commits did not grow on a hypothesis promotion: %d then %d",
			after.stateCommits, control.stateCommits)
	}
	if control.projectStates <= after.projectStates {
		t.Errorf("project_states did not grow on a hypothesis promotion: %d then %d",
			after.projectStates, control.projectStates)
	}
	// A promotion is a record, never an RSG relation: the promoted object is
	// not an endpoint of anything.
	if control.relationVersions != after.relationVersions {
		t.Errorf("relation_versions moved on a promotion: %d then %d — "+
			"promotion provenance is a row, not a relation", after.relationVersions, control.relationVersions)
	}
	// ...and a comment, which is not a state commit, still leaves the ledger
	// where it was.
	ledgerAfterControl := control.contributionEvents
	tail := d.mustComment(t, d.alice, d.pub.id, proposal.threadID, "one more thought")
	if tail.Body == nil {
		t.Fatal("the comment after the control was not served with its body")
	}
	d.drain(t, ctx)
	if final := readStateCounts(t, ctx, d); final != control {
		t.Errorf("a comment moved scientific state after the control:\ncontrol: %+v\nfinal:   %+v", control, final)
	}
	_ = ledgerAfterControl
}

// --------------------------------------------------------------------------
// 3/4. The three promotion paths and their provenance

func TestDiscussionPromotionToIssue(t *testing.T) {
	ctx := testCtx(t)
	d := newDiscussionWorld(t, ctx)

	// alice writes the proposal, bob (a contributor of the project) promotes
	// it: the promotion's actor and the comment's author are different
	// people, which is what makes the provenance read worth asserting.
	d.addMember(t, ctx, d.pub.id, d.bobID, "contributor")
	proposal := d.proposalThread(t, d.alice, "add a 273 K isotherm to the campaign plan")
	got := d.mustPromote(t, d.bob, d.pub.id, proposal.threadID, proposal.commentID,
		`{"kind":"issue","issue_type":"research_task","title":"273 K isotherm"}`)

	if got.Issue == nil {
		t.Fatalf("promotion to an Issue answered %+v, want an issue payload", got)
	}
	if got.Issue.Body != proposal.body {
		t.Errorf("issue body = %q, want the promoted comment %q", got.Issue.Body, proposal.body)
	}
	if got.Issue.IssueType != "research_task" || got.Issue.Title != "273 K isotherm" {
		t.Errorf("issue = %+v, want issue_type research_task and the requested title", got.Issue)
	}
	if got.Issue.State != "open" {
		t.Errorf("issue state = %q, want the schema's default open", got.Issue.State)
	}
	if got.Promotion.PromotedKind != "issue" {
		t.Errorf("promoted_kind = %q, want issue", got.Promotion.PromotedKind)
	}
	// The ref names the created issue; the promoting actor and the comment's
	// author are two different recorded facts.
	if got.Promotion.PromotedRef != "issue:"+got.Issue.ID {
		t.Errorf("promoted_ref = %q, want issue:%s", got.Promotion.PromotedRef, got.Issue.ID)
	}
	if got.Promotion.PromotedBy != d.bobID {
		t.Errorf("promoted_by = %s, want bob %s", got.Promotion.PromotedBy, d.bobID)
	}
	if got.Comment.CreatedBy != d.aliceID {
		t.Errorf("origin comment author = %s, want alice %s", got.Comment.CreatedBy, d.aliceID)
	}
	// The issue is a real row of the surface that owns it, created by the
	// promoting actor.
	if n := countRows(t, ctx, d.pool,
		`SELECT count(*) FROM issues WHERE id = $1 AND created_by = $2`, got.Issue.ID, d.bobID); n != 1 {
		t.Errorf("issues rows for the created issue = %d, want 1 created by bob", n)
	}
	// The provenance is a row, not a relation.
	if n := countRows(t, ctx, d.pool,
		`SELECT count(*) FROM discussion_promotions WHERE promoted_ref = $1`, got.Promotion.PromotedRef); n != 1 {
		t.Errorf("discussion_promotions rows for the ref = %d, want 1", n)
	}
	if n := countRows(t, ctx, d.pool,
		`SELECT count(*) FROM relation_versions rv JOIN relations r ON r.id = rv.relation_id
		  WHERE r.project_id = $1`, d.pub.id); n != d.relationsBefore {
		t.Errorf("relation_versions rows in the project after an Issue promotion = %d, want the %d it already had — "+
			"a comment has no version to be an endpoint of", n, d.relationsBefore)
	}
	// And the promotion is audited.
	if n := countRows(t, ctx, d.pool,
		`SELECT count(*) FROM audit_log WHERE action = 'discussion.promoted' AND target_ref = $1`,
		got.Promotion.PromotedRef); n != 1 {
		t.Errorf("audit_log rows for the promotion = %d, want 1", n)
	}

	// The reverse read: what produced this object?
	list := d.mustPromotionsForRef(t, d.bob, d.pub.id, got.Promotion.PromotedRef)
	if len(list.Promotions) != 1 {
		t.Fatalf("promotions for %s = %d, want 1", got.Promotion.PromotedRef, len(list.Promotions))
	}
	if list.Promotions[0].Promotion.ID != got.Promotion.ID ||
		list.Promotions[0].Comment.ID != proposal.commentID ||
		list.Promotions[0].Thread.ID != proposal.threadID {
		t.Errorf("reverse read = %+v, want the promotion of comment %s in thread %s",
			list.Promotions[0], proposal.commentID, proposal.threadID)
	}
	// ...and the same answer read by id, which is the provenance an issue
	// page links to.
	byID := d.mustPromotionByID(t, d.bob, d.pub.id, got.Promotion.ID)
	if byID.Promotion.ID != got.Promotion.ID || byID.Thread.ID != proposal.threadID ||
		byID.Comment.ID != proposal.commentID {
		t.Errorf("promotion by id = %+v, want the same promotion in thread %s", byID, proposal.threadID)
	}
}

func TestDiscussionPromotionToHypothesisCommitsState(t *testing.T) {
	ctx := testCtx(t)
	d := newDiscussionWorld(t, ctx)

	proposal := d.proposalThread(t, d.alice, "MOF-5 maximizes CO2 uptake at 298 K")
	before := d.projectVersions(t, ctx)

	got := d.mustPromote(t, d.alice, d.pub.id, proposal.threadID, proposal.commentID,
		d.hypothesisBody(d.pub.probe))

	if got.ScientificObject == nil {
		t.Fatalf("promotion to a Hypothesis answered %+v, want a scientific_object payload", got)
	}
	obj := got.ScientificObject
	if obj.ObjectType != "hypothesis" {
		t.Errorf("object_type = %q, want hypothesis", obj.ObjectType)
	}
	if obj.VersionNo != 1 {
		t.Errorf("version_no = %d, want the first version a new object starts at", obj.VersionNo)
	}
	if obj.StateID == "" {
		t.Error("the promotion answered no state — a hypothesis is created by a real state commit")
	}
	if got.Promotion.PromotedRef != "hypothesis:"+obj.ID {
		t.Errorf("promoted_ref = %q, want hypothesis:%s", got.Promotion.PromotedRef, obj.ID)
	}

	// It IS a state commit: one new version of the project, and the version
	// the promotion answered is the one the commit wrote.
	after := d.projectVersions(t, ctx)
	if after != before+1 {
		t.Errorf("scientific_object_versions = %d, want %d — the RSG write path commits state", after, before+1)
	}
	if n := countRows(t, ctx, d.pool,
		`SELECT count(*) FROM scientific_object_versions WHERE id = $1 AND state_id = $2 AND branch_id = $3`,
		obj.VersionID, obj.StateID, d.pub.probe); n != 1 {
		t.Errorf("the version the promotion answered is not the one the commit wrote in the branch (%s/%s/%s)",
			obj.VersionID, obj.StateID, d.pub.probe)
	}
	// The hypothesis answers the question it declared, and the statement is
	// the promoted comment's own words.
	var payload struct {
		Statement  string `json:"statement"`
		QuestionID string `json:"question_id"`
	}
	var raw []byte
	if err := d.pool.QueryRow(ctx,
		`SELECT payload FROM scientific_object_versions WHERE id = $1`, obj.VersionID).Scan(&raw); err != nil {
		t.Fatalf("read the committed payload: %v", err)
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode the committed payload: %v: %s", err, raw)
	}
	if payload.Statement != proposal.body {
		t.Errorf("committed statement = %q, want the promoted comment %q", payload.Statement, proposal.body)
	}
	if payload.QuestionID != d.questionID {
		t.Errorf("committed question_id = %q, want %q", payload.QuestionID, d.questionID)
	}

	// The state commit reached the Contribution Ledger: the promotion is a
	// contribution-making act, and the ledger sees it arrive.
	d.drain(t, ctx)
	if n := countRows(t, ctx, d.pool,
		`SELECT count(*) FROM contribution_events ce
		   JOIN research_events re ON re.id = ce.research_event_id
		  WHERE re.project_id = $1 AND re.actor_id = $2`, d.pub.id, d.aliceID); n == 0 {
		t.Error("the promoted state commit produced no contribution_events row — the ledger did not see it")
	}
	list := d.mustPromotionsForRef(t, d.alice, d.pub.id, got.Promotion.PromotedRef)
	if len(list.Promotions) != 1 || list.Promotions[0].Comment.CreatedBy != d.aliceID {
		t.Errorf("reverse provenance = %+v, want alice's comment", list.Promotions)
	}
}

func TestDiscussionPromotionToExternalEvidenceIsAProposal(t *testing.T) {
	ctx := testCtx(t)
	d := newDiscussionWorld(t, ctx)

	proposal := d.proposalThread(t, d.alice, "the 2019 dataset replicates our CO2 uptake value at 298 K")
	beforeAssertions := countRows(t, ctx, d.pool, `SELECT count(*) FROM evidence_assertions`)
	beforeRelations := countRows(t, ctx, d.pool, `SELECT count(*) FROM relation_versions`)

	// The refs are spelled the way specs/schemas/evidence-assertion.schema.json
	// spells them (the platform's `object_version:` prefix): the promotion
	// route accepts the same reference spelling every other surface accepts.
	got := d.mustPromote(t, d.alice, d.pub.id, proposal.threadID, proposal.commentID,
		fmt.Sprintf(`{"kind":"external_evidence","branch_id":%q,"evidence":{`+
			`"target_version_ref":"object_version:%s","evidence_version_ref":"object_version:%s",`+
			`"relation":"supports","evidence_type":"experimental"}}`,
			d.pub.probe, d.versionID, d.questionVersion))

	if got.EvidenceAssertion == nil {
		t.Fatalf("promotion to external evidence answered %+v, want an evidence_assertion payload", got)
	}
	assertion := got.EvidenceAssertion
	// THE POINT OF THE SHAPE: the created assertion is a PROPOSAL — it is
	// stored unreviewed, and no separate proposal table exists.
	if assertion.ReviewState != "unreviewed" {
		t.Errorf("review_state = %q, want unreviewed — the assertion IS the pending proposal", assertion.ReviewState)
	}
	if assertion.Relation != "supports" || assertion.EvidenceType != "experimental" {
		t.Errorf("assertion = %+v, want relation supports and evidence_type experimental", assertion)
	}
	if assertion.ReasoningNote != proposal.body {
		t.Errorf("reasoning_note = %q, want the promoted comment %q", assertion.ReasoningNote, proposal.body)
	}
	if assertion.EvidenceOrigin != "internal" {
		t.Errorf("evidence_origin = %q, want internal (both versions are this project's)", assertion.EvidenceOrigin)
	}
	if assertion.CreatedBy != d.aliceID {
		t.Errorf("assertion created_by = %s, want alice %s", assertion.CreatedBy, d.aliceID)
	}
	if n := countRows(t, ctx, d.pool,
		`SELECT count(*) FROM evidence_assertions WHERE id = $1 AND review_state = 'unreviewed'`,
		assertion.ID); n != 1 {
		t.Errorf("evidence_assertions rows for the created assertion = %d, want 1 unreviewed", n)
	}
	if after := countRows(t, ctx, d.pool, `SELECT count(*) FROM evidence_assertions`); after != beforeAssertions+1 {
		t.Errorf("evidence_assertions = %d, want %d", after, beforeAssertions+1)
	}
	if got.Promotion.PromotedRef != "external_evidence:"+assertion.ID {
		t.Errorf("promoted_ref = %q, want external_evidence:%s", got.Promotion.PromotedRef, assertion.ID)
	}
	// An assertion is not an RSG relation: the promotion wrote exactly one
	// assertion row and no edge at all.
	if n := countRows(t, ctx, d.pool, `SELECT count(*) FROM relation_versions`); n != beforeRelations {
		t.Errorf("relation_versions = %d, want the %d the project already had — an assertion is not an RSG relation",
			n, beforeRelations)
	}
	list := d.mustPromotionsForRef(t, d.alice, d.pub.id, got.Promotion.PromotedRef)
	if len(list.Promotions) != 1 || list.Promotions[0].Comment.ID != proposal.commentID {
		t.Fatalf("promotions for %s = %+v, want the promotion of comment %s",
			got.Promotion.PromotedRef, list.Promotions, proposal.commentID)
	}
	// The promoted comment is the assertion's note: the proposal's words
	// survive into the row the reviewers will read.
	var storedNote string
	if err := d.pool.QueryRow(ctx,
		`SELECT reasoning_note FROM evidence_assertions WHERE id = $1`, assertion.ID).Scan(&storedNote); err != nil {
		t.Fatalf("read the stored assertion: %v", err)
	}
	if storedNote != proposal.body {
		t.Errorf("stored reasoning_note = %q, want the promoted comment %q", storedNote, proposal.body)
	}
}

// --------------------------------------------------------------------------
// 5. Authorization is fail-closed

func TestDiscussionPromotionAuthorizationIsFailClosed(t *testing.T) {
	ctx := testCtx(t)
	d := newDiscussionWorld(t, ctx)

	// A public project's thread, opened by bob (a non-member who may read
	// it), whose comment bob may write and may NOT promote.
	opened := d.mustOpenThread(t, d.bob, d.pub.id, "project", d.pub.id, "a stranger's opening")
	comment := d.mustComment(t, d.bob, d.pub.id, opened.Thread.ID, "MOF-5 maximizes CO2 uptake at 298 K")
	body := d.hypothesisBody(d.pub.probe)

	// Reading the target is the permission commenting it needs: a signed-in
	// non-member of a PUBLIC project comments (answered 201 above).
	// Promoting is a write of scientific state and is refused.
	resp := d.promote(t, d.bob, d.pub.id, opened.Thread.ID, comment.ID, body)
	mustStatus(t, resp, http.StatusForbidden)
	mustEnvelope(t, resp, "DISCUSSION_FORBIDDEN")
	if n := countRows(t, ctx, d.pool, `SELECT count(*) FROM discussion_promotions`); n != 0 {
		t.Errorf("discussion_promotions rows after a refused promotion = %d, want 0", n)
	}

	// Anonymous: refused before the command runs.
	anon := newTestUserClient(d.ts.URL)
	resp = anon.do(t, http.MethodPost, d.commentURL(d.pub.id, opened.Thread.ID, comment.ID)+"/promotions", body)
	mustStatus(t, resp, http.StatusUnauthorized)
	mustEnvelope(t, resp, "AUTH_UNAUTHENTICATED")

	// A member whose ROLE the matrix denies is refused too: `viewer` has no
	// write_scientific_state cell, and a conditional cell is not an
	// allowance.
	d.addMember(t, ctx, d.pub.id, d.carolID, "viewer")
	resp = d.promote(t, d.carol, d.pub.id, opened.Thread.ID, comment.ID, body)
	mustStatus(t, resp, http.StatusForbidden)
	mustEnvelope(t, resp, "DISCUSSION_FORBIDDEN")

	// A project the actor may not read answers the existence-hiding
	// not-found to every route, promotion included — the same answer a
	// project that does not exist gets.
	secretThread := d.mustOpenThread(t, d.alice, d.secret.id, "project", d.secret.id, "inside omega")
	secretComment := d.mustComment(t, d.alice, d.secret.id, secretThread.Thread.ID, "inner remark")
	resp = d.promote(t, d.bob, d.secret.id, secretThread.Thread.ID, secretComment.ID, body)
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, "DISCUSSION_PROJECT_NOT_FOUND")
	resp = d.addComment(t, d.bob, d.secret.id, secretThread.Thread.ID, "let me in")
	mustStatus(t, resp, http.StatusNotFound)

	// The owner of the project CAN promote: the refusals above are the
	// matrix cell, not a blanket block on the route.
	got := d.mustPromote(t, d.alice, d.pub.id, opened.Thread.ID, comment.ID,
		d.hypothesisBody(d.pub.probe))
	if got.Promotion.PromotedBy != d.aliceID {
		t.Errorf("promoted_by = %s, want alice %s", got.Promotion.PromotedBy, d.aliceID)
	}
	if got.Comment.CreatedBy != d.bobID {
		t.Errorf("the promoted comment's author = %s, want bob %s — the promotion must not rewrite who proposed it",
			got.Comment.CreatedBy, d.bobID)
	}
}

// --------------------------------------------------------------------------
// 6. A withdrawal is a tombstone

func TestDiscussionCommentWithdrawalIsATombstone(t *testing.T) {
	ctx := testCtx(t)
	d := newDiscussionWorld(t, ctx)

	opened := d.mustOpenThread(t, d.bob, d.pub.id, "project", d.pub.id, "opening")
	comment := d.mustComment(t, d.bob, d.pub.id, opened.Thread.ID, "MOF-5 maximizes CO2 uptake at 298 K")
	alices := d.mustComment(t, d.alice, d.pub.id, opened.Thread.ID, "alice's own remark")

	// Somebody else's comment cannot be withdrawn.
	resp := d.withdrawComment(t, d.alice, d.pub.id, opened.Thread.ID, comment.ID)
	mustStatus(t, resp, http.StatusForbidden)
	mustEnvelope(t, resp, "DISCUSSION_FORBIDDEN")

	// The author's own withdrawal answers the tombstone: body withheld, who
	// and when recorded.
	resp = d.withdrawComment(t, d.bob, d.pub.id, opened.Thread.ID, comment.ID)
	mustStatus(t, resp, http.StatusOK)
	tombstone := decodeFlow[discussionCommentWire](t, resp)
	if tombstone.Body != nil {
		t.Errorf("withdrawn comment body = %q, want null (the body is withheld, not blanked)", *tombstone.Body)
	}
	if !tombstone.Deleted || tombstone.DeletedAt == nil || tombstone.DeletedBy == nil ||
		*tombstone.DeletedBy != d.bobID {
		t.Errorf("tombstone = %+v, want deleted with bob as the withdrawer", tombstone)
	}
	if tombstone.ID != comment.ID {
		t.Errorf("tombstone id = %s, want the withdrawn comment %s", tombstone.ID, comment.ID)
	}

	// The ROW is still there and still holds the text: nothing disappears.
	var stored string
	if err := d.pool.QueryRow(ctx,
		`SELECT body FROM discussion_comments WHERE id = $1`, comment.ID).Scan(&stored); err != nil {
		t.Fatalf("the withdrawn comment's row is gone: %v", err)
	}
	if stored != "MOF-5 maximizes CO2 uptake at 298 K" {
		t.Errorf("stored body = %q, want the original text (a withdrawal is not an erasure)", stored)
	}

	// The thread read serves the tombstone beside the live comment.
	read := d.mustReadThread(t, d.bob, d.pub.id, opened.Thread.ID)
	byID := map[string]discussionCommentWire{}
	for _, c := range read.Comments {
		byID[c.ID] = c
	}
	if len(read.Comments) != 3 {
		t.Fatalf("thread comments = %d, want 3 (the opening, the withdrawn one and alice's)", len(read.Comments))
	}
	if got := byID[comment.ID]; got.Body != nil || !got.Deleted {
		t.Errorf("withdrawn comment served as %+v, want a null body", got)
	}
	if got := byID[alices.ID]; got.Body == nil || *got.Body != "alice's own remark" || got.Deleted {
		t.Errorf("live comment served as %+v, want its body intact", got)
	}

	// A second withdrawal is refused — the tombstone is already there.
	resp = d.withdrawComment(t, d.bob, d.pub.id, opened.Thread.ID, comment.ID)
	mustStatus(t, resp, http.StatusConflict)
	mustEnvelope(t, resp, "DISCUSSION_COMMENT_DELETED")

	// And the withdrawn proposal cannot be promoted: an author who took a
	// proposal back must not have it turned into a research object anyway.
	resp = d.promote(t, d.alice, d.pub.id, opened.Thread.ID, comment.ID, d.hypothesisBody(d.pub.probe))
	mustStatus(t, resp, http.StatusConflict)
	mustEnvelope(t, resp, "DISCUSSION_COMMENT_DELETED")
	if n := countRows(t, ctx, d.pool, `SELECT count(*) FROM discussion_promotions`); n != 0 {
		t.Errorf("discussion_promotions rows = %d, want 0", n)
	}

	// A promotion naming a thread the comment does not live in is not
	// found: the route's two ids must belong together.
	other := d.mustOpenThread(t, d.bob, d.pub.id, "project", d.pub.id, "another thread")
	resp = d.promote(t, d.alice, d.pub.id, other.Thread.ID, alices.ID, d.hypothesisBody(d.pub.probe))
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, "DISCUSSION_COMMENT_NOT_FOUND")
}

// --------------------------------------------------------------------------
// 7. The per-kind field rules refuse rather than ignore

func TestDiscussionPromotionShapeRulesAreExclusive(t *testing.T) {
	ctx := testCtx(t)
	d := newDiscussionWorld(t, ctx)
	proposal := d.proposalThread(t, d.alice, "a proposal with a point")

	for _, tc := range []struct {
		name string
		body string
	}{
		{"an Issue may not declare a branch",
			`{"kind":"issue","issue_type":"research_task","branch_id":"11111111-1111-4111-8111-111111111111"}`},
		{"an Issue may not carry a hypothesis",
			`{"kind":"issue","issue_type":"research_task","hypothesis":{"question_id":"11111111-1111-4111-8111-111111111111"}}`},
		{"an Issue needs its type", `{"kind":"issue","title":"no type"}`},
		{"a hypothesis may not take a title",
			fmt.Sprintf(`{"kind":"hypothesis","branch_id":%q,"title":"named","hypothesis":{"question_id":%q}}`, d.pub.probe, d.questionID)},
		{"a hypothesis must declare its proposal",
			fmt.Sprintf(`{"kind":"hypothesis","branch_id":%q}`, d.pub.probe)},
		{"a hypothesis may not carry evidence",
			fmt.Sprintf(`{"kind":"hypothesis","branch_id":%q,"hypothesis":{"question_id":%q},"evidence":{"target_version_ref":"x","evidence_version_ref":"y","relation":"supports","evidence_type":"experimental"}}`, d.pub.probe, d.questionID)},
		{"external evidence may not take a title",
			fmt.Sprintf(`{"kind":"external_evidence","branch_id":%q,"title":"named","evidence":{"target_version_ref":%q,"evidence_version_ref":%q,"relation":"supports","evidence_type":"experimental"}}`, d.pub.probe, d.versionID, d.questionVersion)},
		{"external evidence must name both versions",
			fmt.Sprintf(`{"kind":"external_evidence","branch_id":%q,"evidence":{"target_version_ref":%q,"relation":"supports","evidence_type":"experimental"}}`, d.pub.probe, d.versionID)},
		{"an unknown kind is refused", `{"kind":"release","title":"no"}`},
	} {
		resp := d.promote(t, d.alice, d.pub.id, proposal.threadID, proposal.commentID, tc.body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400: %s", tc.name, resp.StatusCode, readAll(t, resp))
			continue
		}
		mustEnvelope(t, resp, "DISCUSSION_VALIDATION_FAILED")
	}

	// A blank reasoning_note is NOT a declaration: it means "the comment's
	// own words are the note", so a LITERATURE assertion — which the evidence
	// write path refuses when nothing names the evidence unit (docs/10 §6) —
	// is accepted with the comment's text, and the refusal that would have
	// fired on an empty note cannot fire here. A promotion therefore cannot
	// produce an assertion that names no unit.
	literature := d.mustPromote(t, d.alice, d.pub.id, proposal.threadID, proposal.commentID,
		fmt.Sprintf(`{"kind":"external_evidence","branch_id":%q,"evidence":{`+
			`"target_version_ref":%q,"evidence_version_ref":%q,`+
			`"relation":"supports","evidence_type":"literature","reasoning_note":"   "}}`,
			d.pub.probe, d.versionID, d.questionVersion))
	if literature.EvidenceAssertion == nil || literature.EvidenceAssertion.EvidenceType != "literature" {
		t.Fatalf("a literature promotion answered %+v, want an evidence_assertion payload", literature)
	}
	if literature.EvidenceAssertion.ReasoningNote != proposal.body {
		t.Errorf("literature assertion note = %q, want the promoted comment %q",
			literature.EvidenceAssertion.ReasoningNote, proposal.body)
	}

	// Nothing was written by any of the refusals: exactly the one accepted
	// promotion above is in the tables.
	if n := countRows(t, ctx, d.pool, `SELECT count(*) FROM discussion_promotions`); n != 1 {
		t.Errorf("discussion_promotions rows = %d, want the 1 accepted promotion and no more", n)
	}
	if n := countRows(t, ctx, d.pool, `SELECT count(*) FROM issues WHERE project_id = $1`, d.pub.id); n != 0 {
		t.Errorf("issues rows after refusals = %d, want 0 — every Issue promotion here was refused", n)
	}
	if n := countRows(t, ctx, d.pool, `SELECT count(*) FROM evidence_assertions`); n != 1 {
		t.Errorf("evidence_assertions rows = %d, want the 1 accepted promotion and no more", n)
	}
}

// TestDiscussionUnknownCommentAndThreadAreNotFound pins the not-found
// answers of the routes themselves: a thread id that does not exist, a
// comment id that does not exist, and a promotion id that does not exist are
// each the one not-found — a discussion is never an existence oracle for
// anything.
func TestDiscussionUnknownIDsAreNotFound(t *testing.T) {
	ctx := testCtx(t)
	d := newDiscussionWorld(t, ctx)
	opened := d.mustOpenThread(t, d.alice, d.pub.id, "project", d.pub.id, "opening")

	missing := "11111111-1111-4111-8111-111111111111"
	resp := d.get(t, d.alice, d.threadURL(d.pub.id, missing))
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, "DISCUSSION_THREAD_NOT_FOUND")

	resp = d.addComment(t, d.alice, d.pub.id, missing, "into the void")
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, "DISCUSSION_THREAD_NOT_FOUND")

	resp = d.withdrawComment(t, d.alice, d.pub.id, opened.Thread.ID, missing)
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, "DISCUSSION_COMMENT_NOT_FOUND")

	resp = d.promote(t, d.alice, d.pub.id, opened.Thread.ID, missing, d.hypothesisBody(d.pub.probe))
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, "DISCUSSION_COMMENT_NOT_FOUND")

	resp = d.promotionByID(t, d.alice, d.pub.id, missing)
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, "DISCUSSION_PROMOTION_NOT_FOUND")

	// A thread of another project is not readable through this project's
	// route either.
	resp = d.get(t, d.alice, d.threadURL(d.secret.id, opened.Thread.ID))
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, "DISCUSSION_THREAD_NOT_FOUND")

	// The thread is still exactly as it was: none of the refusals wrote.
	if n := countRows(t, ctx, d.pool,
		`SELECT count(*) FROM discussion_comments WHERE thread_id = $1`, opened.Thread.ID); n != 1 {
		t.Errorf("comments in the thread = %d, want the opening one only", n)
	}
	_ = ctx
}

// get is the plain read (reads take no CSRF token, but sending one is
// harmless — a browser does).
func (d *discussionWorld) get(t *testing.T, uc *testUserClient, path string) *http.Response {
	t.Helper()
	return uc.do(t, http.MethodGet, path, "")
}

// promotionByID is the provenance read by promotion id.
func (d *discussionWorld) promotionByID(t *testing.T, uc *testUserClient, projectID, promotionID string) *http.Response {
	t.Helper()
	return uc.do(t, http.MethodGet, d.threadsURL(projectID)+"/promotions/"+promotionID, "")
}
