package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	appcontribution "github.com/lichman0405/post/internal/application/contribution"
	"github.com/lichman0405/post/internal/contribution"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

// Task T0803: Open Contribution Opportunity — over a REAL PostgreSQL,
// with the real store. Proves the three requirements and the acceptance
// criterion end to end:
//
//   - maintainer marks an issue / research need open for contribution:
//     the title is snapshotted from the target, the metadata rides
//     along, and a mark is never born public;
//   - difficulty/capability metadata: validated, persisted, editable
//     while internal, frozen once publicized;
//   - agent suggestions require approval: an agent's MarkOpen is
//     refused, Suggest lands as suggested, only a maintainer's Approve
//     makes it open — and the approve is stamped;
//   - "Agent 不能自动 publicize opportunity": the service refuses agent
//     actors on Publicize, the store is the only path the database
//     admits (the session flag migration 00062's guard requires), the
//     publicize is audited, and raw SQL cannot publicize, un-publicize
//     or drift a publicized row's metadata.
//
// The guard probes (raw SQL) matter beyond the service path: the
// acceptance criterion must hold for ANY write path, not just the happy
// one.

const oppTaskID = "T0803"

// oppFixture seeds ONE database holding TWO private projects — the home
// project the cases act in, and a second project that exists to supply
// genuinely foreign rows — plus three users (a maintainer, a
// contributor, an agent identity), and wires the opportunity service over
// the real store.
//
// Both projects MUST be in the same database. A cross-project assertion
// is only about the project scope if the foreign target really exists in
// another project of the same database: a second database (or a second
// fixture) would make every foreign id merely unknown, which the "target
// does not exist" path already rejects — the cases would then pass
// without the guard's project scope ever being reached.
type oppFixture struct {
	svc  *appcontribution.Service
	pool *pgxpool.Pool

	aliceID string // maintainer
	bobID   string // contributor
	agentID string // the platform agent identity

	projectID  string // the home project
	issueID    string
	questionID string

	// The second project of the same database: its issue and research
	// question are real targets the home project must not be able to
	// borrow, and it owns opportunities of its own that the home project
	// must not be able to read.
	otherProjectID  string
	otherIssueID    string
	otherQuestionID string
}

// seedOppProject seeds one organization, one private project, one issue
// and one research question (object + current version on a main branch
// state) into pool, returning the ids of the project and its two
// targets. Calling it twice seeds two projects of the SAME database.
func seedOppProject(t *testing.T, ctx context.Context, pool *pgxpool.Pool, slug, authorID string) (projectID, issueID, questionID string) {
	t.Helper()
	var orgID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO organizations (slug, name) VALUES ($1, $1) RETURNING id`, slug).Scan(&orgID); err != nil {
		t.Fatalf("seed org %s: %v", slug, err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO projects (organization_id, slug, name, purpose, visibility, created_by)
		VALUES ($1, $2, $2, 'fixture', 'private', $3) RETURNING id`,
		orgID, slug, authorID).Scan(&projectID); err != nil {
		t.Fatalf("seed project %s: %v", slug, err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO issues (project_id, number, issue_type, title, body, created_by)
		VALUES ($1, 1, 'task', 'Curate the benchmark dataset', 'body', $2) RETURNING id`,
		projectID, authorID).Scan(&issueID); err != nil {
		t.Fatalf("seed issue of %s: %v", slug, err)
	}

	// The research question target: object + one version on a main
	// branch state (the store reads the CURRENT version's title).
	var branchID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO branches (project_id, name, visibility, git_ref, created_by)
		VALUES ($1, 'main', 'private', 'refs/heads/main', $2) RETURNING id`,
		projectID, authorID).Scan(&branchID); err != nil {
		t.Fatalf("seed branch of %s: %v", slug, err)
	}
	var stateID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO project_states (project_id, branch_id, state_hash, manifest_version)
		VALUES ($1, $2, $3, 'm1') RETURNING id`,
		projectID, branchID, slug+"-state-1").Scan(&stateID); err != nil {
		t.Fatalf("seed state of %s: %v", slug, err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO scientific_objects (project_id, object_type, created_by)
		VALUES ($1, 'research_question', $2) RETURNING id`,
		projectID, authorID).Scan(&questionID); err != nil {
		t.Fatalf("seed question of %s: %v", slug, err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, schema_id, schema_version, title,
			 lifecycle_state, payload, integrity_hash, created_by)
		VALUES ($1, 1, $2, 'core/research_question', '1.0', 'Why does the alloy harden?',
		        'active', '{}'::jsonb, $4, $3)`,
		questionID, stateID, authorID, slug+"-ih"); err != nil {
		t.Fatalf("seed question version of %s: %v", slug, err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE scientific_objects SET current_version_no = 1 WHERE id = $1`, questionID); err != nil {
		t.Fatalf("seed question head of %s: %v", slug, err)
	}
	return projectID, issueID, questionID
}

func newOppFixture(t *testing.T, ctx context.Context) *oppFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), oppTaskID)
	seedUser := func(handle string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (handle, display_name) VALUES ($1, $1) RETURNING id`, handle).Scan(&id); err != nil {
			t.Fatalf("seed user %s: %v", handle, err)
		}
		return id
	}
	alice := seedUser("opp-alice")
	bob := seedUser("opp-bob")
	agent := seedUser("opp-agent")

	homeProject, homeIssue, homeQuestion := seedOppProject(t, ctx, pool, "opp-project", alice)
	otherProject, otherIssue, otherQuestion := seedOppProject(t, ctx, pool, "opp-other-project", alice)

	return &oppFixture{
		svc:  appcontribution.NewService(contribution.NewOpportunityStore(pool)),
		pool: pool,

		aliceID: alice,
		bobID:   bob,
		agentID: agent,

		projectID:  homeProject,
		issueID:    homeIssue,
		questionID: homeQuestion,

		otherProjectID:  otherProject,
		otherIssueID:    otherIssue,
		otherQuestionID: otherQuestion,
	}
}

func (f *oppFixture) alice() contribution.Actor {
	return contribution.Actor{UserID: f.aliceID}
}

func (f *oppFixture) bob() contribution.Actor {
	return contribution.Actor{UserID: f.bobID}
}

func (f *oppFixture) agent() contribution.Actor {
	return contribution.Actor{UserID: f.agentID, IsAgent: true}
}

func (f *oppFixture) markParams() appcontribution.MarkParams {
	return appcontribution.MarkParams{
		ProjectID:            f.projectID,
		TargetType:           contribution.TargetIssue,
		TargetID:             f.issueID,
		Description:          "Curate rows; contact the maintainer first.",
		Difficulty:           contribution.DifficultyIntermediate,
		RequiredCapabilities: []string{"data-curation", "python"},
		By:                   f.alice(),
	}
}

// isPgErr reports whether err is a Postgres error with the given code.
func isPgErr(t *testing.T, err error, code string) bool {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Errorf("want SQLSTATE %s, got non-pg error: %v", code, err)
		return false
	}
	if pgErr.Code != code {
		t.Errorf("want SQLSTATE %s, got %s (%s)", code, pgErr.Code, pgErr.Message)
		return false
	}
	return true
}

func TestMarkOpenSnapshotsTargetAndMetadata(t *testing.T) {
	ctx := testCtx(t)
	f := newOppFixture(t, ctx)

	op, err := f.svc.MarkOpen(ctx, f.markParams())
	if err != nil {
		t.Fatalf("MarkOpen: %v", err)
	}
	if op.State != contribution.OpportunityStateOpen {
		t.Errorf("state = %s, want open", op.State)
	}
	if op.Visibility != contribution.OpportunityVisibilityInternal {
		t.Errorf("visibility = %s, want internal — a mark is never born public", op.Visibility)
	}
	if op.Title != "Curate the benchmark dataset" {
		t.Errorf("title = %q, want the issue's title snapshotted", op.Title)
	}
	if op.SuggestedBy != nil || op.ApprovedBy != nil || op.PublicizedBy != nil || op.PublicizedAt != nil {
		t.Errorf("a mark must not carry suggestion/approval/publicize stamps: %+v", op)
	}
	if op.CreatedBy != f.aliceID {
		t.Errorf("created_by = %s, want the maintainer", op.CreatedBy)
	}
	if op.Difficulty != contribution.DifficultyIntermediate {
		t.Errorf("difficulty = %s", op.Difficulty)
	}
	if len(op.RequiredCapabilities) != 2 || op.RequiredCapabilities[0] != "data-curation" {
		t.Errorf("capabilities = %v", op.RequiredCapabilities)
	}

	// The row is readable back through the service.
	got, err := f.svc.Get(ctx, f.projectID, op.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != op.Title || got.State != op.State {
		t.Errorf("Get returned %+v, want the created row", got)
	}

	// A research question target works too, title from its current
	// version.
	rq, err := f.svc.MarkOpen(ctx, appcontribution.MarkParams{
		ProjectID:            f.projectID,
		TargetType:           contribution.TargetResearchQuestion,
		TargetID:             f.questionID,
		Difficulty:           contribution.DifficultyAdvanced,
		RequiredCapabilities: []string{"materials-modeling"},
		By:                   f.alice(),
	})
	if err != nil {
		t.Fatalf("MarkOpen research_question: %v", err)
	}
	if rq.Title != "Why does the alloy harden?" {
		t.Errorf("research question title = %q, want the current version's title", rq.Title)
	}
}

func TestAgentMarkRefusedSuggestNeedsApproval(t *testing.T) {
	ctx := testCtx(t)
	f := newOppFixture(t, ctx)

	// An agent cannot mark directly — that would bypass the approval
	// requirement.
	_, err := f.svc.MarkOpen(ctx, f.markParamsWith(appcontribution.MarkParams{By: f.agent()}))
	var agentErr *appcontribution.AgentNotPermittedError
	if !errors.As(err, &agentErr) {
		t.Fatalf("agent MarkOpen: got %v, want *AgentNotPermittedError", err)
	}
	if agentErr.Code() != appcontribution.CodeAgentMarkDenied {
		t.Errorf("agent mark code = %s", agentErr.Code())
	}

	// The agent's path is Suggest: lands as suggested, never open.
	suggested, err := f.svc.Suggest(ctx, appcontribution.SuggestParams{
		ProjectID:            f.projectID,
		TargetType:           contribution.TargetIssue,
		TargetID:             f.issueID,
		Description:          "I can prepare the dataset.",
		Difficulty:           contribution.DifficultyBeginner,
		RequiredCapabilities: []string{"data-curation"},
		By:                   f.agent(),
	})
	if err != nil {
		t.Fatalf("Suggest: %v", err)
	}
	if suggested.State != contribution.OpportunityStateSuggested {
		t.Errorf("suggestion state = %s, want suggested", suggested.State)
	}
	if suggested.Visibility != contribution.OpportunityVisibilityInternal {
		t.Errorf("suggestion visibility = %s, want internal", suggested.Visibility)
	}
	if suggested.SuggestedBy == nil || *suggested.SuggestedBy != f.agentID {
		t.Errorf("suggested_by = %v, want the agent", suggested.SuggestedBy)
	}
	if suggested.ApprovedBy != nil {
		t.Errorf("a fresh suggestion must not carry approved_by")
	}

	// Not visible on the open network, and not open yet.
	public, err := f.svc.ListPublic(ctx)
	if err != nil {
		t.Fatalf("ListPublic: %v", err)
	}
	if len(public) != 0 {
		t.Errorf("ListPublic = %d rows, want 0 — a suggestion is never public", len(public))
	}

	// An agent cannot approve (its own or anyone's suggestion).
	_, err = f.svc.Approve(ctx, f.projectID, suggested.ID, f.agent())
	if !errors.As(err, &agentErr) {
		t.Fatalf("agent Approve: got %v, want *AgentNotPermittedError", err)
	}
	if agentErr.Code() != appcontribution.CodeAgentApproveDenied {
		t.Errorf("agent approve code = %s", agentErr.Code())
	}

	// The maintainer's approval makes it real and is stamped.
	approved, err := f.svc.Approve(ctx, f.projectID, suggested.ID, f.alice())
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if approved.State != contribution.OpportunityStateOpen {
		t.Errorf("approved state = %s, want open", approved.State)
	}
	if approved.ApprovedBy == nil || *approved.ApprovedBy != f.aliceID {
		t.Errorf("approved_by = %v, want the maintainer", approved.ApprovedBy)
	}
	if approved.SuggestedBy == nil || *approved.SuggestedBy != f.agentID {
		t.Errorf("suggested_by must survive approval: %v", approved.SuggestedBy)
	}
	if approved.Visibility != contribution.OpportunityVisibilityInternal {
		t.Errorf("approval must not change visibility: %s", approved.Visibility)
	}
}

func TestRejectAndCloseLifecycle(t *testing.T) {
	ctx := testCtx(t)
	f := newOppFixture(t, ctx)

	suggested, err := f.svc.Suggest(ctx, appcontribution.SuggestParams{
		ProjectID:  f.projectID,
		TargetType: contribution.TargetIssue,
		TargetID:   f.issueID,
		Difficulty: contribution.DifficultyBeginner,
		By:         f.bob(),
	})
	if err != nil {
		t.Fatalf("Suggest: %v", err)
	}
	rejected, err := f.svc.Reject(ctx, f.projectID, suggested.ID, f.alice())
	if err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if rejected.State != contribution.OpportunityStateClosed {
		t.Errorf("rejected state = %s, want closed", rejected.State)
	}
	if rejected.ApprovedBy != nil {
		t.Errorf("a rejected suggestion must not carry approved_by")
	}

	// Closed is terminal: no re-approval, no close-again.
	_, err = f.svc.Approve(ctx, f.projectID, suggested.ID, f.alice())
	var transErr *appcontribution.TransitionError
	if !errors.As(err, &transErr) {
		t.Fatalf("Approve on closed: got %v, want *TransitionError", err)
	}

	// The target is free again: re-marking after close starts a new
	// lifecycle round (the closed row stays as history).
	op, err := f.svc.MarkOpen(ctx, f.markParams())
	if err != nil {
		t.Fatalf("MarkOpen after close: %v", err)
	}
	closed, err := f.svc.Close(ctx, f.projectID, op.ID, f.alice())
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if closed.State != contribution.OpportunityStateClosed {
		t.Errorf("closed state = %s", closed.State)
	}
	_, err = f.svc.Close(ctx, f.projectID, op.ID, f.alice())
	if !errors.As(err, &transErr) {
		t.Fatalf("Close on closed: got %v, want *TransitionError", err)
	}

	// Both lifecycle rounds are project history.
	all, err := f.svc.List(ctx, f.projectID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("List = %d rows, want 2 (the rejected suggestion and the closed mark)", len(all))
	}
}

func TestOneActiveOpportunityPerTarget(t *testing.T) {
	ctx := testCtx(t)
	f := newOppFixture(t, ctx)

	if _, err := f.svc.MarkOpen(ctx, f.markParams()); err != nil {
		t.Fatalf("MarkOpen: %v", err)
	}
	// A second mark (or suggestion) on the same target is refused.
	_, err := f.svc.MarkOpen(ctx, f.markParams())
	if !errors.Is(err, contribution.ErrTargetAlreadyActive) {
		t.Fatalf("second MarkOpen: got %v, want ErrTargetAlreadyActive", err)
	}
	_, err = f.svc.Suggest(ctx, appcontribution.SuggestParams{
		ProjectID:  f.projectID,
		TargetType: contribution.TargetIssue,
		TargetID:   f.issueID,
		Difficulty: contribution.DifficultyBeginner,
		By:         f.agent(),
	})
	if !errors.Is(err, contribution.ErrTargetAlreadyActive) {
		t.Fatalf("Suggest on active target: got %v, want ErrTargetAlreadyActive", err)
	}

	// The database itself refuses the duplicate for any write path: the
	// partial unique index is the unconditional backstop.
	_, err = f.pool.Exec(ctx, `
		INSERT INTO contribution_opportunities
		    (project_id, target_type, target_id, title, difficulty, state, created_by)
		VALUES ($1, 'issue', $2, 'Dup', 'beginner', 'open', $3)`,
		f.projectID, f.issueID, f.aliceID)
	isPgErr(t, err, "23505")
}

func TestPublicizeExplicitAuditedAndAgentRefused(t *testing.T) {
	ctx := testCtx(t)
	f := newOppFixture(t, ctx)

	op, err := f.svc.MarkOpen(ctx, f.markParams())
	if err != nil {
		t.Fatalf("MarkOpen: %v", err)
	}

	// An agent never publicizes — not the open row, not any row.
	_, err = f.svc.Publicize(ctx, f.projectID, op.ID, f.agent())
	var agentErr *appcontribution.AgentNotPermittedError
	if !errors.As(err, &agentErr) {
		t.Fatalf("agent Publicize: got %v, want *AgentNotPermittedError", err)
	}
	if agentErr.Code() != appcontribution.CodeAgentPublicizeDenied {
		t.Errorf("agent publicize code = %s", agentErr.Code())
	}
	// The refusal must not have changed the row.
	still, err := f.svc.Get(ctx, f.projectID, op.ID)
	if err != nil {
		t.Fatalf("Get after refused publicize: %v", err)
	}
	if still.Visibility != contribution.OpportunityVisibilityInternal {
		t.Errorf("visibility moved on a refused publicize: %s", still.Visibility)
	}

	// The maintainer's publicize is the explicit widening: stamped and
	// audited in one transaction.
	publicized, err := f.svc.Publicize(ctx, f.projectID, op.ID, f.alice())
	if err != nil {
		t.Fatalf("Publicize: %v", err)
	}
	if publicized.Visibility != contribution.OpportunityVisibilityPublic {
		t.Errorf("visibility = %s, want public", publicized.Visibility)
	}
	if publicized.PublicizedBy == nil || *publicized.PublicizedBy != f.aliceID {
		t.Errorf("publicized_by = %v, want the maintainer", publicized.PublicizedBy)
	}
	if publicized.PublicizedAt == nil {
		t.Errorf("publicized_at must be stamped")
	}

	// The audit record exists with the actor and the action.
	var audited int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM audit_log
		WHERE action = 'contribution.opportunity.publicized'
		  AND target_ref = $1 AND actor_id = $2`,
		"contribution_opportunity:"+op.ID, f.aliceID).Scan(&audited); err != nil {
		t.Fatalf("probe audit: %v", err)
	}
	if audited != 1 {
		t.Errorf("audit rows for the publicize = %d, want 1", audited)
	}

	// Visibility is terminal: a second publicize is refused.
	_, err = f.svc.Publicize(ctx, f.projectID, op.ID, f.alice())
	var pubErr *contribution.PublicizeError
	if !errors.As(err, &pubErr) || !pubErr.AlreadyPublic {
		t.Fatalf("second Publicize: got %v, want *PublicizeError{AlreadyPublic}", err)
	}

	// The row is on the open network listing.
	public, err := f.svc.ListPublic(ctx)
	if err != nil {
		t.Fatalf("ListPublic: %v", err)
	}
	if len(public) != 1 || public[0].ID != op.ID {
		t.Errorf("ListPublic = %+v, want exactly the publicized row", public)
	}

	// Closing a publicized row keeps it public as history and removes it
	// from the active listing.
	if _, err := f.svc.Close(ctx, f.projectID, op.ID, f.alice()); err != nil {
		t.Fatalf("Close publicized: %v", err)
	}
	public, err = f.svc.ListPublic(ctx)
	if err != nil {
		t.Fatalf("ListPublic: %v", err)
	}
	if len(public) != 0 {
		t.Errorf("ListPublic after close = %d rows, want 0 (closed rows are history, not active listings)", len(public))
	}
	closed, err := f.svc.Get(ctx, f.projectID, op.ID)
	if err != nil {
		t.Fatalf("Get closed: %v", err)
	}
	if closed.Visibility != contribution.OpportunityVisibilityPublic {
		t.Errorf("closing must not change visibility: %s", closed.Visibility)
	}
}

func TestPublicizeRequiresOpenAndMetadataFrozen(t *testing.T) {
	ctx := testCtx(t)
	f := newOppFixture(t, ctx)

	// A suggested row cannot be publicized.
	suggested, err := f.svc.Suggest(ctx, appcontribution.SuggestParams{
		ProjectID:  f.projectID,
		TargetType: contribution.TargetIssue,
		TargetID:   f.issueID,
		Difficulty: contribution.DifficultyBeginner,
		By:         f.bob(),
	})
	if err != nil {
		t.Fatalf("Suggest: %v", err)
	}
	_, err = f.svc.Publicize(ctx, f.projectID, suggested.ID, f.alice())
	var pubErr *contribution.PublicizeError
	if !errors.As(err, &pubErr) || pubErr.AlreadyPublic {
		t.Fatalf("Publicize suggested: got %v, want *PublicizeError (not open)", err)
	}

	op, err := f.svc.MarkOpen(ctx, f.markParamsWith(appcontribution.MarkParams{
		TargetID: f.questionID, TargetType: contribution.TargetResearchQuestion,
	}))
	if err != nil {
		t.Fatalf("MarkOpen: %v", err)
	}
	// Metadata is editable while internal.
	advanced := contribution.DifficultyAdvanced
	updated, err := f.svc.UpdateMetadata(ctx, f.projectID, op.ID, contribution.MetadataPatch{
		Difficulty:           &advanced,
		RequiredCapabilities: &[]string{"materials-modeling", "simulation"},
	})
	if err != nil {
		t.Fatalf("UpdateMetadata: %v", err)
	}
	if updated.Difficulty != contribution.DifficultyAdvanced {
		t.Errorf("difficulty = %s after update", updated.Difficulty)
	}

	if _, err := f.svc.Publicize(ctx, f.projectID, op.ID, f.alice()); err != nil {
		t.Fatalf("Publicize: %v", err)
	}
	// Frozen once public: the service path refuses, and the database
	// refuses any other path (the guard).
	beginner := contribution.DifficultyBeginner
	_, err = f.svc.UpdateMetadata(ctx, f.projectID, op.ID, contribution.MetadataPatch{
		Difficulty: &beginner,
	})
	if !errors.Is(err, contribution.ErrPublicizedFrozen) {
		t.Fatalf("UpdateMetadata publicized: got %v, want ErrPublicizedFrozen", err)
	}
	_, err = f.pool.Exec(ctx, `
		UPDATE contribution_opportunities SET description = 'drifted' WHERE id = $1`, op.ID)
	isPgErr(t, err, "P0001")
}

func TestDatabaseGuardRefusesEveryRawPath(t *testing.T) {
	ctx := testCtx(t)
	f := newOppFixture(t, ctx)

	op, err := f.svc.MarkOpen(ctx, f.markParams())
	if err != nil {
		t.Fatalf("MarkOpen: %v", err)
	}

	// Raw publicize without the session flag: refused.
	_, err = f.pool.Exec(ctx, `
		UPDATE contribution_opportunities
		   SET visibility = 'public', publicized_by = $2, publicized_at = now()
		 WHERE id = $1`, op.ID, f.aliceID)
	isPgErr(t, err, "P0001")

	// Raw publicize with the flag but without the stamps: refused. The
	// flag is set for real here (its own statement inside an explicit
	// transaction — set_config(is_local=true) is transaction-scoped), so
	// the refusal provably comes from the missing stamps, not from a
	// missing flag.
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`SELECT set_config('post.co_publicize', 'on', true)`); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	_, err = tx.Exec(ctx, `
		UPDATE contribution_opportunities SET visibility = 'public' WHERE id = $1`, op.ID)
	isPgErr(t, err, "P0001")
	_ = tx.Rollback(ctx)

	// Raw illegal state transitions: refused.
	_, err = f.pool.Exec(ctx, `
		UPDATE contribution_opportunities SET state = 'suggested' WHERE id = $1`, op.ID)
	isPgErr(t, err, "P0001")

	// Raw identity drift: refused.
	_, err = f.pool.Exec(ctx, `
		UPDATE contribution_opportunities SET created_by = $2 WHERE id = $1`, op.ID, f.bobID)
	isPgErr(t, err, "P0001")

	// Raw un-publish of a publicized row: refused (visibility is
	// terminal).
	pub, err := f.svc.Publicize(ctx, f.projectID, op.ID, f.alice())
	if err != nil {
		t.Fatalf("Publicize: %v", err)
	}
	_, err = f.pool.Exec(ctx, `
		UPDATE contribution_opportunities SET visibility = 'internal' WHERE id = $1`, pub.ID)
	isPgErr(t, err, "P0001")

	// A row is never born public: raw INSERT with visibility public is
	// refused.
	_, err = f.pool.Exec(ctx, `
		INSERT INTO contribution_opportunities
		    (project_id, target_type, target_id, title, difficulty, state, visibility, created_by)
		VALUES ($1, 'issue', $2, 'Born public', 'beginner', 'open', 'public', $3)`,
		f.projectID, f.issueID, f.aliceID)
	isPgErr(t, err, "P0001")

	// The state machine holds against a second database: even with the
	// flag and the stamps, only an open row can be publicized.
	suggested, err := f.svc.Suggest(ctx, appcontribution.SuggestParams{
		ProjectID:  f.projectID,
		TargetType: contribution.TargetResearchQuestion,
		TargetID:   f.questionID,
		Difficulty: contribution.DifficultyBeginner,
		By:         f.bob(),
	})
	if err != nil {
		t.Fatalf("Suggest: %v", err)
	}
	tx2, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tx2.Exec(ctx,
		`SELECT set_config('post.co_publicize', 'on', true)`); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	_, err = tx2.Exec(ctx, `
		UPDATE contribution_opportunities
		   SET visibility = 'public', publicized_by = $2, publicized_at = now()
		 WHERE id = $1`, suggested.ID, f.aliceID)
	isPgErr(t, err, "P0001")
	_ = tx2.Rollback(ctx)
}

func TestTargetExistenceGuardAtCommit(t *testing.T) {
	ctx := testCtx(t)
	f := newOppFixture(t, ctx)

	// A nonexistent target fails at COMMIT (the deferred constraint
	// trigger), not at insert time.
	foreign := "00000000-0000-0000-0000-000000000099"
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO contribution_opportunities
		    (project_id, target_type, target_id, title, difficulty, state, created_by)
		VALUES ($1, 'issue', $2, 'Ghost target', 'beginner', 'open', $3)`,
		f.projectID, foreign, f.aliceID); err != nil {
		t.Fatalf("insert (should defer): %v", err)
	}
	err = tx.Commit(ctx)
	isPgErr(t, err, "P0001")
	_ = tx.Rollback(ctx)

	// Positive control for the borrowing cases below: the second
	// project's issue is a REAL target — addressed by the project that
	// owns it, the very same row goes through the very same insert path
	// and is accepted. The closed row it leaves behind is not "active",
	// so it does not collide with the borrowing inserts.
	control, err := f.svc.MarkOpen(ctx, f.markParamsWith(appcontribution.MarkParams{
		ProjectID:  f.otherProjectID,
		TargetID:   f.otherIssueID,
		TargetType: contribution.TargetIssue,
		Difficulty: contribution.DifficultyBeginner,
	}))
	if err != nil {
		t.Fatalf("MarkOpen in the other project on its own issue: %v", err)
	}
	if control.ProjectID != f.otherProjectID || control.TargetID != f.otherIssueID {
		t.Fatalf("control opportunity = (project %s, target %s), want (%s, %s)",
			control.ProjectID, control.TargetID, f.otherProjectID, f.otherIssueID)
	}
	if _, err := f.svc.Close(ctx, f.otherProjectID, control.ID, f.alice()); err != nil {
		t.Fatalf("Close control opportunity: %v", err)
	}

	// A target of ANOTHER project is refused the same way — no foreign
	// target borrows. This is the case that pins the guard's project
	// scope: the id exists (control above), the insert is
	// indistinguishable from a legal one except for the project it names.
	for _, tc := range []struct {
		name       string
		targetType contribution.OpportunityTargetType
		targetID   string
	}{
		{"issue", contribution.TargetIssue, f.otherIssueID},
		{"research_question", contribution.TargetResearchQuestion, f.otherQuestionID},
	} {
		tx, err = f.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO contribution_opportunities
			    (project_id, target_type, target_id, title, difficulty, state, created_by)
			VALUES ($1, $2, $3, 'Foreign target', 'beginner', 'open', $4)`,
			f.projectID, string(tc.targetType), tc.targetID, f.aliceID); err != nil {
			t.Fatalf("insert of a foreign %s target (should defer): %v", tc.name, err)
		}
		commitErr := tx.Commit(ctx)
		if commitErr == nil {
			t.Errorf("borrowing the other project's %s target: commit succeeded, want P0001 "+
				"(the target exists in another project of this database)", tc.name)
			_ = tx.Rollback(ctx)
			continue
		}
		isPgErr(t, commitErr, "P0001")
		_ = tx.Rollback(ctx)
	}

	// A non-research_question object is not a research-need target.
	var datasetID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO scientific_objects (project_id, object_type, created_by)
		VALUES ($1, 'dataset', $2) RETURNING id`, f.projectID, f.aliceID).Scan(&datasetID); err != nil {
		t.Fatalf("seed dataset: %v", err)
	}
	_, err = f.svc.MarkOpen(ctx, appcontribution.MarkParams{
		ProjectID:  f.projectID,
		TargetType: contribution.TargetResearchQuestion,
		TargetID:   datasetID,
		Difficulty: contribution.DifficultyBeginner,
		By:         f.alice(),
	})
	if !errors.Is(err, contribution.ErrTargetNotFound) {
		t.Fatalf("MarkOpen on a dataset as research_question: got %v, want ErrTargetNotFound", err)
	}

	// And the service maps a missing target for an issue the same way.
	_, err = f.svc.MarkOpen(ctx, appcontribution.MarkParams{
		ProjectID:  f.projectID,
		TargetType: contribution.TargetIssue,
		TargetID:   foreign,
		Difficulty: contribution.DifficultyBeginner,
		By:         f.alice(),
	})
	if !errors.Is(err, contribution.ErrTargetNotFound) {
		t.Fatalf("MarkOpen on a ghost issue: got %v, want ErrTargetNotFound", err)
	}
}

func TestValidationAndForeignReads(t *testing.T) {
	ctx := testCtx(t)
	f := newOppFixture(t, ctx)

	bad := f.markParams()
	bad.TargetType = "paper"
	_, err := f.svc.MarkOpen(ctx, bad)
	if !errors.Is(err, appcontribution.ErrValidation) {
		t.Fatalf("unknown target type: got %v, want ErrValidation", err)
	}
	bad = f.markParams()
	bad.Difficulty = "impossible"
	_, err = f.svc.MarkOpen(ctx, bad)
	if !errors.Is(err, appcontribution.ErrValidation) {
		t.Fatalf("unknown difficulty: got %v, want ErrValidation", err)
	}
	bad = f.markParams()
	bad.RequiredCapabilities = []string{"UPPER"}
	_, err = f.svc.MarkOpen(ctx, bad)
	if !errors.Is(err, appcontribution.ErrValidation) {
		t.Fatalf("bad capability shape: got %v, want ErrValidation", err)
	}
	bad = f.markParams()
	bad.RequiredCapabilities = []string{"a", "a"}
	_, err = f.svc.MarkOpen(ctx, bad)
	if !errors.Is(err, appcontribution.ErrValidation) {
		t.Fatalf("duplicate capabilities: got %v, want ErrValidation", err)
	}

	op, err := f.svc.MarkOpen(ctx, f.markParams())
	if err != nil {
		t.Fatalf("MarkOpen: %v", err)
	}

	// The second project (same database) holds an opportunity of its own.
	// That is what makes the assertions below about scoping: a project
	// that does not exist answers "not found" and "empty" for free, so
	// probing an unknown id proves nothing about a project boundary.
	otherOp, err := f.svc.MarkOpen(ctx, f.markParamsWith(appcontribution.MarkParams{
		ProjectID:  f.otherProjectID,
		TargetID:   f.otherIssueID,
		TargetType: contribution.TargetIssue,
		Difficulty: contribution.DifficultyBeginner,
	}))
	if err != nil {
		t.Fatalf("MarkOpen in the other project: %v", err)
	}

	// A foreign project never leaks the row's existence: the row is there
	// (its own project holds it), and it is still not found through the
	// other project's id — in either direction.
	_, err = f.svc.Get(ctx, f.otherProjectID, op.ID)
	if !errors.Is(err, contribution.ErrOpportunityNotFound) {
		t.Fatalf("Get of the home row through the other project: got %v, want ErrOpportunityNotFound", err)
	}
	_, err = f.svc.Get(ctx, f.projectID, otherOp.ID)
	if !errors.Is(err, contribution.ErrOpportunityNotFound) {
		t.Fatalf("Get of the other project's row through the home project: got %v, want ErrOpportunityNotFound", err)
	}
	_, err = f.svc.Close(ctx, f.otherProjectID, op.ID, f.alice())
	if !errors.Is(err, contribution.ErrOpportunityNotFound) {
		t.Fatalf("Close of the home row through the other project: got %v, want ErrOpportunityNotFound", err)
	}
	// The refused foreign Close left the row alone.
	after, err := f.svc.Get(ctx, f.projectID, op.ID)
	if err != nil {
		t.Fatalf("Get after the refused foreign Close: %v", err)
	}
	if after.State != contribution.OpportunityStateOpen {
		t.Errorf("state after the refused foreign Close = %s, want open", after.State)
	}

	// The project list is scoped: the other project sees exactly its own
	// row — neither the home project's row nor nothing at all.
	all, err := f.svc.List(ctx, f.otherProjectID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 1 || all[0].ID != otherOp.ID || all[0].ProjectID != f.otherProjectID {
		t.Errorf("List of the other project = %v, want exactly [%s]", oppIDs(all), otherOp.ID)
	}
	home, err := f.svc.List(ctx, f.projectID)
	if err != nil {
		t.Fatalf("List (home): %v", err)
	}
	if len(home) != 1 || home[0].ID != op.ID {
		t.Errorf("List of the home project = %v, want exactly [%s]", oppIDs(home), op.ID)
	}
}

// oppIDs renders the ids of a list of opportunities for failure messages.
func oppIDs(ops []contribution.ContributionOpportunity) []string {
	ids := make([]string, len(ops))
	for i, op := range ops {
		ids[i] = op.ID
	}
	return ids
}

// markParamsWith copies the default mark params and overlays non-zero
// fields of over.
func (f *oppFixture) markParamsWith(over appcontribution.MarkParams) appcontribution.MarkParams {
	p := f.markParams()
	if over.ProjectID != "" {
		p.ProjectID = over.ProjectID
	}
	if over.TargetType != "" {
		p.TargetType = over.TargetType
	}
	if over.TargetID != "" {
		p.TargetID = over.TargetID
	}
	if over.Description != "" {
		p.Description = over.Description
	}
	if over.Difficulty != "" {
		p.Difficulty = over.Difficulty
	}
	if over.RequiredCapabilities != nil {
		p.RequiredCapabilities = over.RequiredCapabilities
	}
	if over.By.UserID != "" {
		p.By = over.By
	}
	return p
}
