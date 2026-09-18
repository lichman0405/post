package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/prdiff"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/application/responsibilities"
	"github.com/lichman0405/post/internal/application/reviews"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence"
)

// Task T0604: Scientific Responsibility / Reviewer Routing — over a REAL
// PostgreSQL, with the same composition cmd/api/main.go wires: the
// responsibilities service (rules + assignments + the reviewer-responsibility
// resolver + the required-review calculation) over the real stores, and the
// reviews service over the real review store with the resolver and the
// routing both attached. Nothing here is constructed state: every PR walks
// the docs/43 machine through the product's own services.
//
// The acceptance criteria each map to a section of this file:
//
//   - "protocol 变更自动要求对应 reviewer，规则驱动" → the rule written in
//     the project's own data decides the required label; rewriting the rule
//     changes the requirement without a code change, and the calculation
//     reads the real three-way diff (object type, schema id, payload
//     domain);
//   - "责任标签不改变 authz 判定，逐条证明" → the full action × class matrix
//     is compared cell by cell before and after a maximal set of
//     responsibility data exists, and the cells that must not move
//     (merge main by a viewer holding every label) are asserted directly;
//   - "生产 wiring 不再为 nil" → the conditional submit_scientific_review
//     verdict now resolves through the REAL resolver: a label holder is
//     allowed, a member without one is refused, an outsider cannot even
//     learn the project exists;
//   - "reviews.responsibility 填写真实标签" → the recorded label is the one
//     the resolved rule routed the change to (not merely a label the
//     reviewer happens to hold);
//   - "open → merge_ready 真实产品路径" → the whole machine is walked
//     through the services, and the database's transition guard proves the
//     shortcut (review_required → merge_ready) is refused;
//   - "缺配置不自动 approve" → no rules at all: approvals record and the
//     proposal never advances, with the refusal naming the missing routing;
//   - "release_min_reviewers 真的被消费" → the same approvals advance the
//     proposal with the rule absent and do NOT with it set to 2;
//   - "重复与并发" → a repeated decision is refused and does not inflate the
//     count; two concurrent approvals advance exactly once, with one audit
//     row and one advanced event.

// routingFixture is the diff fixture (private project owned by alice, the
// rsg write path, the T0401 diff engine) plus the PR service and the
// T0604 stack, wired as cmd/api wires it.
type routingFixture struct {
	*diffFixture
	prs     *pullrequests.Service
	routing *responsibilities.Service
	reviews *reviews.Service
}

func newRoutingFixture(t *testing.T, ctx context.Context) *routingFixture {
	t.Helper()
	d := newDiffFixture(t, ctx)
	projectStore := persistence.NewProjectStore(d.pool)
	orgStore := persistence.NewOrgStore(d.pool)
	projectSvc := projects.NewService(projectStore, orgStore, authz.NewMatrixEngine())
	prStore := persistence.NewPullRequestStore(d.pool)
	branchStore := persistence.NewBranchStore(d.pool)
	routingSvc := responsibilities.NewService(responsibilities.Deps{
		Rules:     persistence.NewResponsibilityStore(d.pool),
		Projects:  projectStore,
		Members:   projectSvc,
		PRs:       prStore,
		Branches:  branchStore,
		Diffs:     prdiff.NewService(prStore, branchStore, d.diffs),
		Policies:  persistence.NewPolicyStore(d.pool),
		Evaluator: policy.NewRuleEvaluator(),
	})
	return &routingFixture{
		diffFixture: d,
		prs:         pullrequests.NewService(prStore),
		routing:     routingSvc,
		reviews: reviews.NewService(reviews.Deps{
			Repo:           persistence.NewReviewStore(d.pool),
			Projects:       projectSvc,
			Authz:          authz.NewMatrixEngine(),
			Responsibility: routingSvc,
			Routing:        routingSvc,
		}),
	}
}

// fork creates a research branch off main's current head.
func (f *routingFixture) fork(t *testing.T, ctx context.Context, name string) domain.Branch {
	t.Helper()
	branch, err := f.svc.CreateBranch(ctx, f.alice, f.project.ID, rsg.CreateBranchInput{
		Name:       name,
		BaseRef:    *f.head(t, ctx, f.main),
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create %s branch: %v", name, err)
	}
	return branch
}

// openPR opens the proposal (state `open`) and returns it.
func (f *routingFixture) openPR(t *testing.T, ctx context.Context, sourceBranchID, title string) domain.PullRequest {
	t.Helper()
	pr, err := f.prs.Create(ctx, pullrequests.CreatePullRequestParams{
		ProjectID:      f.project.ID,
		SourceBranchID: sourceBranchID,
		TargetBranchID: f.main,
		Title:          title,
		Body:           "proposal context",
		CreatedBy:      f.alice.ID,
	})
	if err != nil {
		t.Fatalf("create pull request: %v", err)
	}
	if pr.State != domain.PullRequestStateOpen {
		t.Fatalf("new PR state = %s, want open", pr.State)
	}
	return pr
}

// requestReview moves open -> review_required through the PR service.
func (f *routingFixture) requestReview(t *testing.T, ctx context.Context, number int64) {
	t.Helper()
	if _, err := f.prs.RequestReview(ctx, f.project.ID, number); err != nil {
		t.Fatalf("RequestReview #%d: %v", number, err)
	}
}

// prState reads the PR's current machine state.
func (f *routingFixture) prState(t *testing.T, ctx context.Context, number int64) domain.PullRequestState {
	t.Helper()
	pr, err := f.prs.Get(ctx, f.project.ID, number)
	if err != nil {
		t.Fatalf("Get PR #%d: %v", number, err)
	}
	return pr.State
}

// seedUser creates one user and, when role is not empty, seeds the
// membership row directly (the member-management API is a later task; the
// canonical state is what the gates read).
func (f *routingFixture) seedUser(t *testing.T, ctx context.Context, email, handle, role string) domain.User {
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

// addRule writes one Research Owners rule through the service (owner-only),
// so the audit row and the table row land together as production writes them.
func (f *routingFixture) addRule(t *testing.T, ctx context.Context, kind domain.ResearchOwnerMatchKind, value, label string) domain.ResearchOwnerRule {
	t.Helper()
	rule, err := f.routing.AddRule(ctx, f.alice, f.project.ID, responsibilities.AddRuleInput{
		MatchKind:      kind,
		MatchValue:     value,
		Responsibility: label,
	})
	if err != nil {
		t.Fatalf("AddRule %s=%q -> %q: %v", kind, value, label, err)
	}
	return rule
}

// assign records one responsibility assignment through the service.
func (f *routingFixture) assign(t *testing.T, ctx context.Context, userID, label string) {
	t.Helper()
	if _, err := f.routing.Assign(ctx, f.alice, f.project.ID, userID, label); err != nil {
		t.Fatalf("Assign %s -> %q: %v", userID, label, err)
	}
}

// submit records one review through the wired service and returns the error
// (nil on success) so callers can assert refusals.
func (f *routingFixture) submit(ctx context.Context, actor domain.User, number int64, kind domain.ReviewKind, decision domain.ReviewDecision) (domain.Review, error) {
	return f.reviews.SubmitReview(ctx, actor, f.project.ID, number, reviews.SubmitReviewInput{
		Kind:     kind,
		Decision: decision,
		Body:     "review reasoning",
	})
}

// mustApprove records one approving review and fails the test on error.
func (f *routingFixture) mustApprove(t *testing.T, ctx context.Context, actor domain.User, number int64, kind domain.ReviewKind) domain.Review {
	t.Helper()
	review, err := f.submit(ctx, actor, number, kind, domain.ReviewDecisionApproved)
	if err != nil {
		t.Fatalf("SubmitReview %s/approved: %v", kind, err)
	}
	return review
}

// required computes the proposal's required-review calculation through the
// real service (real PR row, real diff, real rules, real policy).
func (f *routingFixture) required(t *testing.T, ctx context.Context, number int64) domain.RequiredReviews {
	t.Helper()
	required, err := f.routing.RequiredReviews(ctx, f.project.ID, number)
	if err != nil {
		t.Fatalf("RequiredReviews #%d: %v", number, err)
	}
	return required
}

// progress re-evaluates the calculation against the reviews the product
// recorded (the same pure function the store's projection runs).
func (f *routingFixture) progress(t *testing.T, ctx context.Context, number int64) domain.ReviewProgress {
	t.Helper()
	recorded, err := f.reviews.List(ctx, f.project.ID, number)
	if err != nil {
		t.Fatalf("ListReviews #%d: %v", number, err)
	}
	return domain.EvaluateRequiredReviews(f.required(t, ctx, number), recorded)
}

// requirementLabels renders the calculation's requirements as an
// order-independent set of "kind|responsibility".
func requirementLabels(required domain.RequiredReviews) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, req := range required.Requirements {
		sig := string(req.Kind) + "|" + req.Responsibility
		if !seen[sig] {
			seen[sig] = true
			out = append(out, sig)
		}
	}
	return out
}

// responsibilityOf reads the label straight off the stored review row — the
// column the acceptance criterion names.
func (f *routingFixture) responsibilityOf(t *testing.T, ctx context.Context, reviewID string) string {
	t.Helper()
	var label string
	if err := f.pool.QueryRow(ctx, `SELECT responsibility FROM reviews WHERE id = $1::uuid`, reviewID).Scan(&label); err != nil {
		t.Fatalf("read reviews.responsibility of %s: %v", reviewID, err)
	}
	return label
}

// reviewEvent is one pull_request.reviewed outbox row as this suite reads it.
type reviewEvent struct {
	Responsibility string
	StateBefore    string
	StateAfter     string
	Advanced       bool
}

// reviewEvents reads the proposal's review events, oldest first.
func (f *routingFixture) reviewEvents(t *testing.T, ctx context.Context, number int64) []reviewEvent {
	t.Helper()
	rows, err := f.pool.Query(ctx, `
		SELECT payload->>'responsibility', payload->>'state_before', payload->>'state_after', payload->>'advanced'
		  FROM outbox_events
		 WHERE project_id = $1::uuid AND event_type = 'pull_request.reviewed' AND payload->>'pull_request_number' = $2
		 ORDER BY created_at, id`,
		f.project.ID, fmt.Sprint(number))
	if err != nil {
		t.Fatalf("query review events: %v", err)
	}
	defer rows.Close()
	var out []reviewEvent
	for rows.Next() {
		var e reviewEvent
		var advanced string
		if err := rows.Scan(&e.Responsibility, &e.StateBefore, &e.StateAfter, &advanced); err != nil {
			t.Fatalf("scan review event: %v", err)
		}
		e.Advanced = advanced == "true"
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate review events: %v", err)
	}
	return out
}

// auditActionsMatching counts the project's audit rows carrying this action.
func (f *routingFixture) auditActionsMatching(t *testing.T, ctx context.Context, action string) []auditSQLRow {
	t.Helper()
	var out []auditSQLRow
	for _, row := range sqlAuditRows(t, ctx, f.pool, "project", f.project.ID) {
		if row.Action == action {
			out = append(out, row)
		}
	}
	return out
}

// seedPolicy inserts one project policy version directly (the write surface
// is T0603's; the calculation only reads what is in force).
func (f *routingFixture) seedPolicy(t *testing.T, ctx context.Context, version, doc string) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO policy_versions (project_id, version, policy_json, created_by) VALUES ($1,$2,$3::jsonb,$4) RETURNING id`,
		f.project.ID, version, doc, f.alice.ID).Scan(&id); err != nil {
		t.Fatalf("insert policy %s: %v", version, err)
	}
	return id
}

// protocolPayload is a protocol document complete enough for the schema and
// the merge gate's needs; only the declared subject area moves between cases.
func protocolPayload(subjectArea string) string {
	return `{"purpose":"annealing protocol","domain":"` + subjectArea + `",` +
		`"steps":[{"id":"s1","action":"heat"}],` +
		`"parameters":{"temperature":"300 K"},` +
		`"requirements":["dry glovebox"]}`
}

// TestReviewRoutingRoutesTheProtocolChangeToItsResearchOwner is the first
// acceptance criterion end to end: the change's routing comes from the
// project's own rule data, the label is what the resolved rule asked for,
// and the proposal walks open → review_required → merge_ready through the
// product's own services (no constructed state) — with the database's
// transition guard proving the machine cannot be shortcut.
func TestReviewRoutingRoutesTheProtocolChangeToItsResearchOwner(t *testing.T) {
	ctx := testCtx(t)
	f := newRoutingFixture(t, ctx)
	f.createObject(t, ctx, f.main, "claim", `{"statement":"main's accepted claim"}`)

	// The project's routing data: a protocol change belongs to
	// "Experimental Reviewer".
	f.addRule(t, ctx, domain.ResearchOwnerMatchObjectType, "protocol", "Experimental Reviewer")

	feature := f.fork(t, ctx, "protocol-proposal")
	f.createObjectOn(t, ctx, feature.ID, "protocol", protocolPayload("materials"))
	pr := f.openPR(t, ctx, feature.ID, "Propose the annealing protocol")
	f.requestReview(t, ctx, pr.Number)
	if got := f.prState(t, ctx, pr.Number); got != domain.PullRequestStateReviewRequired {
		t.Fatalf("state = %s, want review_required", got)
	}

	// The calculation reads the real diff: the protocol change routes to
	// the rule's label and the proposal needs its integrity judgment too.
	required := f.required(t, ctx, pr.Number)
	got := requirementLabels(required)
	if len(got) != 2 || got[0] != "integrity|" || got[1] != "scientific|Experimental Reviewer" {
		t.Fatalf("requirements = %v, want [integrity|, scientific|Experimental Reviewer]", got)
	}
	if len(required.Unrouted) != 0 {
		t.Fatalf("unrouted = %v, want none", required.Unrouted)
	}
	if !required.Satisfiable() {
		t.Fatalf("calculation = %+v, want satisfiable", required)
	}
	if len(required.RoutedLabels) != 1 || required.RoutedLabels[0] != "Experimental Reviewer" {
		t.Fatalf("routed labels = %v, want [Experimental Reviewer]", required.RoutedLabels)
	}

	// carol is a viewer holding two labels: one the routing did NOT ask for
	// ("Data Reviewer") and one it did. The review must be recorded under
	// the routed one — the criterion "reviews.responsibility 与解析出的规则
	// 一致", and the reason the label is not simply the reviewer's first.
	carol := f.seedUser(t, ctx, "routing-carol@example.com", "routing-carol", "viewer")
	bob := f.seedUser(t, ctx, "routing-bob@example.com", "routing-bob", "maintainer")
	f.assign(t, ctx, carol.ID, "Data Reviewer")
	f.assign(t, ctx, carol.ID, "Experimental Reviewer")
	f.assign(t, ctx, bob.ID, "Experimental Reviewer")

	// First dimension: records, and the proposal is still not approved —
	// the integrity judgment is missing.
	sci := f.mustApprove(t, ctx, carol, pr.Number, domain.ReviewKindScientific)
	if got := f.responsibilityOf(t, ctx, sci.ID); got != "Experimental Reviewer" {
		t.Fatalf("reviews.responsibility = %q, want the routed label Experimental Reviewer", got)
	}
	if got := f.prState(t, ctx, pr.Number); got != domain.PullRequestStateReviewRequired {
		t.Fatalf("state after one of two dimensions = %s, want review_required", got)
	}
	before := f.progress(t, ctx, pr.Number)
	if before.Satisfied || len(before.Missing) != 1 {
		t.Fatalf("progress = %+v, want one missing requirement", before)
	}

	// Second dimension: the calculation is satisfied and the PR advances
	// review_required -> approved -> merge_ready in the same transaction as
	// the review row.
	integrity := f.mustApprove(t, ctx, bob, pr.Number, domain.ReviewKindIntegrity)
	if got := f.responsibilityOf(t, ctx, integrity.ID); got != "Experimental Reviewer" {
		t.Fatalf("integrity review responsibility = %q, want the routed label", got)
	}
	if got := f.prState(t, ctx, pr.Number); got != domain.PullRequestStateMergeReady {
		t.Fatalf("state = %s, want merge_ready (the only state T0406's merge accepts)", got)
	}
	after := f.progress(t, ctx, pr.Number)
	if !after.Satisfied {
		t.Fatalf("progress = %+v, want satisfied", after)
	}

	// The governance record of the advance, written in the same transaction.
	completed := f.auditActionsMatching(t, ctx, domain.ActionPullRequestReviewCompleted)
	if len(completed) != 1 {
		t.Fatalf("pull_request.review_completed audit rows = %d, want 1", len(completed))
	}
	var summary map[string]any
	if completed[0].AfterSummary == nil {
		t.Fatal("the advance audit row carries no after_summary")
	}
	if err := json.Unmarshal([]byte(*completed[0].AfterSummary), &summary); err != nil {
		t.Fatalf("decode after_summary: %v", err)
	}
	if summary["state"] != string(domain.PullRequestStateMergeReady) {
		t.Fatalf("audit after_summary.state = %v, want merge_ready", summary["state"])
	}
	if summary["approvals"] != float64(2) {
		t.Fatalf("audit approvals = %v, want 2", summary["approvals"])
	}

	// Both submissions are research events; exactly one of them is the
	// advance (the second submission is what satisfied the calculation).
	events := f.reviewEvents(t, ctx, pr.Number)
	if len(events) != 2 {
		t.Fatalf("review events = %d, want 2 (one per submission)", len(events))
	}
	advanced := 0
	for _, e := range events {
		if e.Advanced {
			advanced++
			if e.StateBefore != string(domain.PullRequestStateReviewRequired) || e.StateAfter != string(domain.PullRequestStateMergeReady) {
				t.Fatalf("advance event = %+v, want review_required -> merge_ready", e)
			}
		}
	}
	if advanced != 1 {
		t.Fatalf("advanced events = %d, want exactly 1", advanced)
	}

	// The rule is DATA: writing a different rule for the same object type
	// changes the required reviewer with no code change, and a change whose
	// type no rule matches stays unrouted. Both are asserted on a second,
	// independent proposal.
	rules, err := f.routing.ListRules(ctx, f.alice, f.project.ID)
	if err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("rules = %+v, want the one rule written above", rules)
	}
	if removed, err := f.routing.RemoveRule(ctx, f.alice, f.project.ID, rules[0].ID); err != nil || !removed {
		t.Fatalf("RemoveRule = %v, %v; want the rule removed", removed, err)
	}
	f.addRule(t, ctx, domain.ResearchOwnerMatchObjectType, "protocol", "IP Reviewer")

	feature2 := f.fork(t, ctx, "protocol-proposal-2")
	f.createObjectOn(t, ctx, feature2.ID, "protocol", protocolPayload("catalysis"))
	pr2 := f.openPR(t, ctx, feature2.ID, "Second protocol proposal")
	f.requestReview(t, ctx, pr2.Number)
	required2 := f.required(t, ctx, pr2.Number)
	if got := requirementLabels(required2); len(got) != 2 || got[1] != "scientific|IP Reviewer" {
		t.Fatalf("requirements after the rule rewrite = %v, want the new label", got)
	}

	// A claim change is routed by nothing now: the calculation is
	// unsatisfiable and names the unrouted change.
	feature3 := f.fork(t, ctx, "claim-proposal")
	f.createObjectOn(t, ctx, feature3.ID, "claim", `{"statement":"a claim nobody owns"}`)
	pr3 := f.openPR(t, ctx, feature3.ID, "Claim proposal")
	f.requestReview(t, ctx, pr3.Number)
	required3 := f.required(t, ctx, pr3.Number)
	if len(required3.Unrouted) != 1 {
		t.Fatalf("unrouted = %v, want the claim change", required3.Unrouted)
	}
	if required3.Satisfiable() {
		t.Fatalf("calculation for an unrouted change = %+v, want unsatisfiable", required3)
	}

	// The machine cannot be shortcut: the database's docs/43 guard refuses
	// review_required -> merge_ready, which is why the projection walks
	// through approved. (The product path above reached merge_ready through
	// both steps; this proves the direct write is refused.)
	_, err = f.pool.Exec(ctx, `UPDATE pull_requests SET state = 'merge_ready' WHERE project_id = $1::uuid AND number = $2`,
		f.project.ID, pr2.Number)
	if got := sqlState(t, err); got != "P0001" {
		t.Fatalf("raw review_required -> merge_ready SQLSTATE = %s, want P0001 (illegal transition)", got)
	}
}

// TestResponsibilityNeverWidensAuthz is the second acceptance criterion:
// responsibility labels are project data, and the authorization matrix
// never reads them. Every action × class cell is compared before and after
// a maximal set of responsibility data exists, and the cells that must not
// move are asserted on the product surfaces too.
func TestResponsibilityNeverWidensAuthz(t *testing.T) {
	ctx := testCtx(t)
	f := newRoutingFixture(t, ctx)
	f.createObject(t, ctx, f.main, "claim", `{"statement":"main's accepted claim"}`)
	feature := f.fork(t, ctx, "authz-proposal")
	f.createObjectOn(t, ctx, feature.ID, "protocol", protocolPayload("materials"))
	pr := f.openPR(t, ctx, feature.ID, "Authz invariance")
	f.requestReview(t, ctx, pr.Number)

	viewer := f.seedUser(t, ctx, "authz-viewer@example.com", "authz-viewer", "viewer")
	outsider := f.seedUser(t, ctx, "authz-outsider@example.com", "authz-outsider", "")

	// Baseline: the whole matrix, cell by cell.
	engine := authz.NewMatrixEngine()
	type cell struct {
		action authz.Action
		class  authz.ActorClass
	}
	cells := make([]cell, 0, len(authz.Actions())*len(authz.ActorClasses()))
	before := map[cell]authz.Decision{}
	for _, action := range authz.Actions() {
		for _, class := range authz.ActorClasses() {
			c := cell{action, class}
			decision, err := engine.Authorize(ctx, authz.Request{Action: action, Class: class})
			if err != nil {
				t.Fatalf("Authorize(%s, %s): %v", action, class, err)
			}
			cells = append(cells, c)
			before[c] = decision
		}
	}
	if len(cells) == 0 {
		t.Fatal("the matrix exposed no action × class cells to compare")
	}

	// Every documented responsibility label, held by a viewer — and one
	// held by a user who is not a member of the project at all — plus a
	// rule routing the proposal's own change to one of them.
	for _, label := range []string{
		"Experimental Reviewer", "Computational Reviewer", "Data Reviewer", "Project Lead", "IP Reviewer",
	} {
		f.assign(t, ctx, viewer.ID, label)
	}
	f.assign(t, ctx, outsider.ID, "Project Lead")
	f.addRule(t, ctx, domain.ResearchOwnerMatchObjectType, "protocol", "Project Lead")

	// Item by item: not one verdict moved.
	for _, c := range cells {
		decision, err := engine.Authorize(ctx, authz.Request{Action: c.action, Class: c.class})
		if err != nil {
			t.Fatalf("Authorize(%s, %s) after the responsibility writes: %v", c.action, c.class, err)
		}
		if decision != before[c] {
			t.Fatalf("verdict for (%s, %s) changed from %+v to %+v after responsibility writes",
				c.action, c.class, before[c], decision)
		}
	}

	// The cells that must not move, asserted by name: a viewer holding every
	// responsibility label still cannot merge main, and the merge surfaces'
	// classes are untouched.
	for _, class := range []authz.ActorClass{authz.ActorViewer} {
		decision, err := engine.Authorize(ctx, authz.Request{Action: authz.ActionMergeMain, Class: class})
		if err != nil {
			t.Fatalf("Authorize(merge_main, %s): %v", class, err)
		}
		if !decision.Denies() {
			t.Fatalf("merge_main verdict for %s = %+v, want denied even holding every responsibility label", class, decision)
		}
	}

	// And the label data does not smuggle the outsider in through the
	// product path either: the resolver refuses to answer for a non-member
	// (existence-hiding), and the submission is refused at the membership
	// gate before the responsibility is ever consulted — the label is not
	// what decides an outsider's access.
	if _, err := f.routing.Responsibilities(ctx, outsider, f.project.ID); !errors.Is(err, responsibilities.ErrProjectNotFound) {
		t.Fatalf("resolver for a non-member = %v, want ErrProjectNotFound", err)
	}
	if _, err := f.submit(ctx, outsider, pr.Number, domain.ReviewKindIntegrity, domain.ReviewDecisionApproved); !errors.Is(err, projects.ErrProjectNotFound) {
		t.Fatalf("non-member submission while holding a label = %v, want ErrProjectNotFound", err)
	}

	// Holding a label does not open the CONFIGURATION either. The private
	// project answers the outsider with the existence-hiding not-found on
	// both the read and the write — never "forbidden", which would confirm
	// that the project exists (docs/45) — while a member who can already
	// see the project and merely lacks the owner role gets the role refusal.
	if _, err := f.routing.ListRules(ctx, outsider, f.project.ID); !errors.Is(err, responsibilities.ErrProjectNotFound) {
		t.Fatalf("outsider ListRules = %v, want ErrProjectNotFound", err)
	}
	if _, err := f.routing.AddRule(ctx, outsider, f.project.ID, responsibilities.AddRuleInput{
		MatchKind: domain.ResearchOwnerMatchObjectType, MatchValue: "protocol", Responsibility: "Project Lead",
	}); !errors.Is(err, responsibilities.ErrProjectNotFound) {
		t.Fatalf("outsider AddRule = %v, want ErrProjectNotFound (no existence leak)", err)
	}
	if _, err := f.routing.AddRule(ctx, viewer, f.project.ID, responsibilities.AddRuleInput{
		MatchKind: domain.ResearchOwnerMatchObjectType, MatchValue: "protocol", Responsibility: "Project Lead",
	}); !errors.Is(err, responsibilities.ErrForbidden) {
		t.Fatalf("viewer AddRule = %v, want ErrForbidden", err)
	}
	// Reading the configuration is a member read: the viewer may.
	if _, err := f.routing.ListRules(ctx, viewer, f.project.ID); err != nil {
		t.Fatalf("viewer ListRules: %v", err)
	}

	// The viewer's labels DO resolve the conditional review verdict — the
	// one thing responsibility is for. This is the positive half: the
	// invariance above is not "labels do nothing", it is "labels decide
	// review attribution and nothing else".
	if _, err := f.submit(ctx, viewer, pr.Number, domain.ReviewKindIntegrity, domain.ReviewDecisionComment); err != nil {
		t.Fatalf("viewer holding a responsibility was refused a review: %v", err)
	}
}

// TestReviewRoutingProductionWiringResolvesTheConditionalVerdict is the
// third acceptance criterion: the reviewer-responsibility hook is no longer
// nil in production. The conditional submit_scientific_review verdict now
// resolves through the project's own responsibility data — allowed when the
// actor holds one, refused when they do not.
func TestReviewRoutingProductionWiringResolvesTheConditionalVerdict(t *testing.T) {
	ctx := testCtx(t)
	f := newRoutingFixture(t, ctx)
	f.createObject(t, ctx, f.main, "claim", `{"statement":"main's accepted claim"}`)
	feature := f.fork(t, ctx, "wiring-proposal")
	f.createObjectOn(t, ctx, feature.ID, "protocol", protocolPayload("materials"))
	pr := f.openPR(t, ctx, feature.ID, "Wiring")
	f.requestReview(t, ctx, pr.Number)

	carol := f.seedUser(t, ctx, "wiring-carol@example.com", "wiring-carol", "viewer")
	bob := f.seedUser(t, ctx, "wiring-bob@example.com", "wiring-bob", "maintainer")
	dave := f.seedUser(t, ctx, "wiring-dave@example.com", "wiring-dave", "")

	// viewer with no responsibility: the condition is unmet — refused, not
	// permitted by default.
	if _, err := f.submit(ctx, carol, pr.Number, domain.ReviewKindIntegrity, domain.ReviewDecisionComment); !errors.Is(err, reviews.ErrForbidden) {
		t.Fatalf("viewer without a responsibility = %v, want ErrForbidden", err)
	}

	// maintainer: unconditional allow, and with no label the recorded
	// responsibility is the empty one (attribution is a fact about the
	// reviewer; a maintainer is not thereby a responsible reviewer).
	bobReview, err := f.submit(ctx, bob, pr.Number, domain.ReviewKindScientific, domain.ReviewDecisionComment)
	if err != nil {
		t.Fatalf("maintainer submission: %v", err)
	}
	if got := f.responsibilityOf(t, ctx, bobReview.ID); got != "" {
		t.Fatalf("maintainer without a label recorded responsibility %q, want empty", got)
	}

	// The owner assigns carol a responsibility: the conditional verdict now
	// resolves, and the label is recorded on the row.
	f.assign(t, ctx, carol.ID, "Data Reviewer")
	carolReview, err := f.submit(ctx, carol, pr.Number, domain.ReviewKindIntegrity, domain.ReviewDecisionComment)
	if err != nil {
		t.Fatalf("viewer holding a responsibility: %v", err)
	}
	if got := f.responsibilityOf(t, ctx, carolReview.ID); got != "Data Reviewer" {
		t.Fatalf("recorded responsibility = %q, want Data Reviewer", got)
	}

	// Removing the responsibility closes the condition again: the resolver
	// is consulted on every submission, never cached into a role.
	if removed, err := f.routing.Unassign(ctx, f.alice, f.project.ID, carol.ID, "Data Reviewer"); err != nil || !removed {
		t.Fatalf("Unassign = %v, %v; want the assignment removed", removed, err)
	}
	if _, err := f.submit(ctx, carol, pr.Number, domain.ReviewKindIntegrity, domain.ReviewDecisionApproved); !errors.Is(err, reviews.ErrForbidden) {
		t.Fatalf("viewer after the responsibility was removed = %v, want ErrForbidden", err)
	}

	// A non-member still cannot learn the project exists (docs/45).
	if _, err := f.submit(ctx, dave, pr.Number, domain.ReviewKindScientific, domain.ReviewDecisionComment); !errors.Is(err, projects.ErrProjectNotFound) {
		t.Fatalf("non-member submission = %v, want ErrProjectNotFound", err)
	}

	// None of this advanced the proposal: comments are not approvals, and
	// the required calculation was never satisfied.
	if got := f.prState(t, ctx, pr.Number); got != domain.PullRequestStateReviewRequired {
		t.Fatalf("state = %s, want review_required", got)
	}
}

// TestReviewRoutingWithoutRulesNeverApproves is the "missing configuration"
// criterion: a project with no Research Owners rules at all records the
// reviews and never advances — absence of configuration is never a pass.
func TestReviewRoutingWithoutRulesNeverApproves(t *testing.T) {
	ctx := testCtx(t)
	f := newRoutingFixture(t, ctx)
	f.createObject(t, ctx, f.main, "claim", `{"statement":"main's accepted claim"}`)
	feature := f.fork(t, ctx, "unconfigured-proposal")
	f.createObjectOn(t, ctx, feature.ID, "protocol", protocolPayload("materials"))
	pr := f.openPR(t, ctx, feature.ID, "Unconfigured project")
	f.requestReview(t, ctx, pr.Number)

	// No rules, no assignments — but a maintainer, who may review freely.
	bob := f.seedUser(t, ctx, "norules-bob@example.com", "norules-bob", "maintainer")
	f.mustApprove(t, ctx, bob, pr.Number, domain.ReviewKindScientific)
	f.mustApprove(t, ctx, bob, pr.Number, domain.ReviewKindIntegrity)

	required := f.required(t, ctx, pr.Number)
	if len(required.Unrouted) != 1 {
		t.Fatalf("unrouted = %v, want the change named (no rule routes it)", required.Unrouted)
	}
	if len(required.Requirements) != 0 || required.Satisfiable() {
		t.Fatalf("calculation = %+v, want nothing required and nothing satisfiable", required)
	}
	progress := f.progress(t, ctx, pr.Number)
	if progress.Satisfied {
		t.Fatalf("progress = %+v, want unsatisfied", progress)
	}
	if progress.Reason == "" {
		t.Fatal("progress carries no reason for the refusal")
	}
	if got := f.prState(t, ctx, pr.Number); got != domain.PullRequestStateReviewRequired {
		t.Fatalf("state = %s, want review_required: missing routing must never approve", got)
	}

	// The reviews themselves are still recorded — a human judgment is a
	// fact about the head whatever the project's configuration says — and
	// the events carry advanced=false.
	recorded, err := f.reviews.List(ctx, f.project.ID, pr.Number)
	if err != nil {
		t.Fatalf("ListReviews: %v", err)
	}
	if len(recorded) != 2 {
		t.Fatalf("recorded reviews = %d, want 2", len(recorded))
	}
	for _, e := range f.reviewEvents(t, ctx, pr.Number) {
		if e.Advanced {
			t.Fatalf("event = %+v, want no advance while the routing is missing", e)
		}
	}
	if rows := f.auditActionsMatching(t, ctx, domain.ActionPullRequestReviewCompleted); len(rows) != 0 {
		t.Fatalf("advance audit rows = %d, want none", len(rows))
	}
}

// TestReviewRoutingConsumesReleaseMinReviewers is the policy criterion: the
// `release_min_reviewers` value is read from the project's policy and
// really bounds the number of distinct approving reviewers a merge into the
// released lineage needs. The same approvals that suffice without the rule
// do not suffice with it set to 2.
func TestReviewRoutingConsumesReleaseMinReviewers(t *testing.T) {
	ctx := testCtx(t)
	f := newRoutingFixture(t, ctx)
	f.createObject(t, ctx, f.main, "claim", `{"statement":"main's accepted claim"}`)
	f.addRule(t, ctx, domain.ResearchOwnerMatchObjectType, "protocol", "Experimental Reviewer")
	carol := f.seedUser(t, ctx, "release-carol@example.com", "release-carol", "viewer")
	bob := f.seedUser(t, ctx, "release-bob@example.com", "release-bob", "viewer")
	f.assign(t, ctx, carol.ID, "Experimental Reviewer")
	f.assign(t, ctx, bob.ID, "Experimental Reviewer")

	// Without the rule: one reviewer signing both dimensions satisfies the
	// calculation, because nothing bounds the count.
	first := f.fork(t, ctx, "release-proposal-1")
	f.createObjectOn(t, ctx, first.ID, "protocol", protocolPayload("materials"))
	pr1 := f.openPR(t, ctx, first.ID, "Release-bound proposal 1")
	f.requestReview(t, ctx, pr1.Number)
	if required := f.required(t, ctx, pr1.Number); required.MinApprovals != 0 || !required.ReleaseMerge {
		t.Fatalf("calculation = %+v, want a release merge with no minimum", required)
	}
	f.mustApprove(t, ctx, carol, pr1.Number, domain.ReviewKindScientific)
	f.mustApprove(t, ctx, carol, pr1.Number, domain.ReviewKindIntegrity)
	if got := f.prState(t, ctx, pr1.Number); got != domain.PullRequestStateMergeReady {
		t.Fatalf("state without the policy = %s, want merge_ready", got)
	}

	// With the policy in force: the identical approvals leave the proposal
	// at review_required — the calculation is satisfied but the minimum is
	// not reached.
	f.seedPolicy(t, ctx, "v1", `{"`+domain.RuleReleaseMinReviewers+`": 2}`)
	second := f.fork(t, ctx, "release-proposal-2")
	f.createObjectOn(t, ctx, second.ID, "protocol", protocolPayload("catalysis"))
	pr2 := f.openPR(t, ctx, second.ID, "Release-bound proposal 2")
	f.requestReview(t, ctx, pr2.Number)
	required := f.required(t, ctx, pr2.Number)
	if !required.ReleaseMerge || required.MinApprovals != 2 {
		t.Fatalf("calculation = %+v, want the policy's minimum of 2 on a release merge", required)
	}
	f.mustApprove(t, ctx, carol, pr2.Number, domain.ReviewKindScientific)
	f.mustApprove(t, ctx, carol, pr2.Number, domain.ReviewKindIntegrity)
	progress := f.progress(t, ctx, pr2.Number)
	if progress.Approvals != 1 || progress.RequiredApprovals != 2 {
		t.Fatalf("progress = %+v, want 1 of the policy's 2 approvals", progress)
	}
	if got := f.prState(t, ctx, pr2.Number); got != domain.PullRequestStateReviewRequired {
		t.Fatalf("state with 1 of 2 required reviewers = %s, want review_required", got)
	}

	// The second person's approval reaches the minimum and the proposal
	// advances exactly once.
	f.mustApprove(t, ctx, bob, pr2.Number, domain.ReviewKindIntegrity)
	if got := f.prState(t, ctx, pr2.Number); got != domain.PullRequestStateMergeReady {
		t.Fatalf("state with 2 of 2 required reviewers = %s, want merge_ready", got)
	}
	if rows := f.auditActionsMatching(t, ctx, domain.ActionPullRequestReviewCompleted); len(rows) != 2 {
		t.Fatalf("advance audit rows = %d, want 2 (one per proposal)", len(rows))
	}
}

// TestReviewRoutingDuplicateAndConcurrentApprovals is the concurrency
// criterion: a repeated decision is refused by the database and cannot
// inflate the count the policy bounds, and two approvals racing for the
// same head advance the proposal exactly once — one audit row, one advance
// event, no second state transition.
func TestReviewRoutingDuplicateAndConcurrentApprovals(t *testing.T) {
	ctx := testCtx(t)
	f := newRoutingFixture(t, ctx)
	f.createObject(t, ctx, f.main, "claim", `{"statement":"main's accepted claim"}`)
	f.addRule(t, ctx, domain.ResearchOwnerMatchObjectType, "protocol", "Experimental Reviewer")
	// The minimum makes the count observable: with it set, the number of
	// distinct approving reviewers decides the state, so an inflated count
	// would show up as a premature advance.
	f.seedPolicy(t, ctx, "v1", `{"`+domain.RuleReleaseMinReviewers+`": 2}`)

	carol := f.seedUser(t, ctx, "dup-carol@example.com", "dup-carol", "viewer")
	bob := f.seedUser(t, ctx, "dup-bob@example.com", "dup-bob", "viewer")
	dave := f.seedUser(t, ctx, "dup-dave@example.com", "dup-dave", "viewer")
	for _, u := range []domain.User{carol, bob, dave} {
		f.assign(t, ctx, u.ID, "Experimental Reviewer")
	}

	feature := f.fork(t, ctx, "concurrent-proposal")
	f.createObjectOn(t, ctx, feature.ID, "protocol", protocolPayload("materials"))
	pr := f.openPR(t, ctx, feature.ID, "Concurrent approvals")
	f.requestReview(t, ctx, pr.Number)

	// A repeated decision for the same dimension is refused by the unique
	// key (one decision per person per kind per head).
	f.mustApprove(t, ctx, carol, pr.Number, domain.ReviewKindScientific)
	if _, err := f.submit(ctx, carol, pr.Number, domain.ReviewKindScientific, domain.ReviewDecisionApproved); !errors.Is(err, reviews.ErrAlreadyReviewed) {
		t.Fatalf("repeated scientific decision = %v, want ErrAlreadyReviewed", err)
	}
	if progress := f.progress(t, ctx, pr.Number); progress.Approvals != 1 {
		t.Fatalf("progress = %+v, want the repeated decision counted once", progress)
	}
	if got := f.prState(t, ctx, pr.Number); got != domain.PullRequestStateReviewRequired {
		t.Fatalf("state = %s, want review_required", got)
	}

	// Two reviewers approve the missing dimension at the same time. The PR
	// row lock serializes them, so exactly one of them sees a satisfied
	// calculation and advances the proposal.
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i, reviewer := range []domain.User{bob, dave} {
		wg.Add(1)
		go func(i int, reviewer domain.User) {
			defer wg.Done()
			_, results[i] = f.submit(ctx, reviewer, pr.Number, domain.ReviewKindIntegrity, domain.ReviewDecisionApproved)
		}(i, reviewer)
	}
	wg.Wait()
	for i, err := range results {
		if err != nil {
			t.Fatalf("concurrent approval %d: %v", i, err)
		}
	}

	if got := f.prState(t, ctx, pr.Number); got != domain.PullRequestStateMergeReady {
		t.Fatalf("state after the concurrent approvals = %s, want merge_ready", got)
	}
	recorded, err := f.reviews.List(ctx, f.project.ID, pr.Number)
	if err != nil {
		t.Fatalf("ListReviews: %v", err)
	}
	if len(recorded) != 3 {
		t.Fatalf("recorded reviews = %d, want 3 (one refused duplicate plus three approvals)", len(recorded))
	}
	if rows := f.auditActionsMatching(t, ctx, domain.ActionPullRequestReviewCompleted); len(rows) != 1 {
		t.Fatalf("advance audit rows = %d, want exactly 1", len(rows))
	}
	events := f.reviewEvents(t, ctx, pr.Number)
	if len(events) != 3 {
		t.Fatalf("review events = %d, want 3 (one per accepted submission)", len(events))
	}
	advanced := 0
	for _, e := range events {
		if e.Advanced {
			advanced++
		}
	}
	if advanced != 1 {
		t.Fatalf("advanced events = %d, want exactly 1", advanced)
	}
}

// TestReviewRoutingEveryAcceptedSubmissionWritesItsEvent walks the review
// LOOP of docs/43 (changes_requested → review_required → …) and pins the two
// things a submission must always leave behind:
//
//   - one pull_request.reviewed event per ACCEPTED submission — the event is
//     the research record of the judgment, not a reward for advancing, so a
//     submission the projection decides against still emits exactly one;
//   - a payload whose state_before/state_after are the states the submission
//     actually spanned. A changes_requested decision moves the PR out of
//     review_required inside its own transaction, so that submission reports
//     review_required → changes_requested — not the review_required →
//     review_required its own pre-move snapshot would show.
//
// The third step is the reachable path where both used to fail at once: the
// calculation is already satisfied when the PR re-enters review_required, so
// the changes_requested decision that closes the loop is also the submission
// whose projection has nothing left to advance.
func TestReviewRoutingEveryAcceptedSubmissionWritesItsEvent(t *testing.T) {
	ctx := testCtx(t)
	f := newRoutingFixture(t, ctx)
	f.createObject(t, ctx, f.main, "claim", `{"statement":"main's accepted claim"}`)
	f.addRule(t, ctx, domain.ResearchOwnerMatchObjectType, "protocol", "Experimental Reviewer")

	carol := f.seedUser(t, ctx, "loop-carol@example.com", "loop-carol", "viewer")
	bob := f.seedUser(t, ctx, "loop-bob@example.com", "loop-bob", "viewer")
	f.assign(t, ctx, carol.ID, "Experimental Reviewer")
	f.assign(t, ctx, bob.ID, "Experimental Reviewer")

	feature := f.fork(t, ctx, "review-loop")
	f.createObjectOn(t, ctx, feature.ID, "protocol", protocolPayload("materials"))
	pr := f.openPR(t, ctx, feature.ID, "Review loop")
	f.requestReview(t, ctx, pr.Number)

	submit := func(actor domain.User, kind domain.ReviewKind, decision domain.ReviewDecision) {
		t.Helper()
		if _, err := f.submit(ctx, actor, pr.Number, kind, decision); err != nil {
			t.Fatalf("submit %s/%s by %s: %v", kind, decision, actor.Handle, err)
		}
	}

	// (1) carol asks for changes. The submission itself moves the PR
	// review_required → changes_requested.
	submit(carol, domain.ReviewKindScientific, domain.ReviewDecisionChangesRequested)
	if got := f.prState(t, ctx, pr.Number); got != domain.PullRequestStateChangesRequested {
		t.Fatalf("state after the changes_requested decision = %s, want changes_requested", got)
	}

	// (2)+(3) The dimensions are recorded while the PR is in
	// changes_requested: the calculation becomes satisfied (the routed
	// scientific review by bob, the proposal integrity review by carol) but
	// the machine does not move — docs/43 advances a proposal out of
	// review_required, and the PR is not there.
	submit(bob, domain.ReviewKindScientific, domain.ReviewDecisionApproved)
	submit(carol, domain.ReviewKindIntegrity, domain.ReviewDecisionApproved)
	if p := f.progress(t, ctx, pr.Number); !p.Satisfied {
		t.Fatalf("progress = %+v, want the calculation satisfied", p)
	}
	if got := f.prState(t, ctx, pr.Number); got != domain.PullRequestStateChangesRequested {
		t.Fatalf("state with a satisfied calculation in changes_requested = %s, want changes_requested", got)
	}

	// (4) The author re-enters review: changes_requested → review_required.
	// The head does not move (only RefreshProposed moves it), so the
	// approvals recorded above still stand for the same proposal.
	f.requestReview(t, ctx, pr.Number)
	if required := f.required(t, ctx, pr.Number); !required.Satisfiable() || required.HeadStateID == "" {
		t.Fatalf("calculation after re-entering review = %+v, want a head and a satisfiable routing", required)
	}

	// (5) A second changes_requested decision, now in review_required, with
	// the calculation already satisfied.
	submit(bob, domain.ReviewKindIntegrity, domain.ReviewDecisionChangesRequested)
	if got := f.prState(t, ctx, pr.Number); got != domain.PullRequestStateChangesRequested {
		t.Fatalf("state after the second changes_requested decision = %s, want changes_requested", got)
	}

	// Every accepted submission has exactly one event: the outbox count
	// equals the review-row count.
	recorded, err := f.reviews.List(ctx, f.project.ID, pr.Number)
	if err != nil {
		t.Fatalf("ListReviews: %v", err)
	}
	if len(recorded) != 4 {
		t.Fatalf("recorded reviews = %d, want 4 (two decisions, two approvals)", len(recorded))
	}
	events := f.reviewEvents(t, ctx, pr.Number)
	if len(events) != len(recorded) {
		t.Fatalf("pull_request.reviewed events = %d, reviews = %d; want one event per accepted submission",
			len(events), len(recorded))
	}

	// Each payload reports the states its own submission spanned.
	got := map[string]int{}
	for _, e := range events {
		if e.Advanced {
			t.Fatalf("event = %+v, want no advance: the calculation was satisfied from changes_requested, never from review_required", e)
		}
		got[e.StateBefore+" -> "+e.StateAfter]++
	}
	want := map[string]int{
		// the two changes_requested decisions, each moving the row itself
		"review_required -> changes_requested": 2,
		// the two approvals, recorded while the row was already there
		"changes_requested -> changes_requested": 2,
	}
	if len(got) != len(want) {
		t.Fatalf("event payload shapes = %v, want exactly %v", got, want)
	}
	for pair, n := range want {
		if got[pair] != n {
			t.Fatalf("events reporting %q = %d, want %d (all events: %v)", pair, got[pair], n, got)
		}
	}

	// Nothing advanced: no PR review_completed audit row, and the state is
	// the one the changes_requested decision left.
	if rows := f.auditActionsMatching(t, ctx, domain.ActionPullRequestReviewCompleted); len(rows) != 0 {
		t.Fatalf("advance audit rows = %d, want none", len(rows))
	}
	if got := f.prState(t, ctx, pr.Number); got != domain.PullRequestStateChangesRequested {
		t.Fatalf("final state = %s, want changes_requested", got)
	}
}

// createObjectOn writes one object on the branch through the rsg service and
// returns the new version id.
func (f *routingFixture) createObjectOn(t *testing.T, ctx context.Context, branch, objectType, payload string) string {
	t.Helper()
	_, versionID := f.createObject(t, ctx, branch, objectType, payload)
	return versionID
}
