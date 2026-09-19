// Task T0809 required test "credit dispute" — credit attribution and credit
// disputes over a REAL PostgreSQL (docs/66 §3: task/run-scoped namespaced
// database, no mocks), the REAL permission matrix, and the REAL ledger
// projection.
//
// docs/13 §2 is the attribution half ("Asset/Finding/Release 可声明
// creators/major contributors/method designer 等高层 credit。它可以被纠正，
// 但 correction 作为新 event，旧 attribution 和 dispute history 保留。")
// and docs/13 §3 the dispute half ("Contributor 可发起 dispute，附带 ledger
// evidence；Maintainer/Organization governance 处理。不得直接改原事件。
// Dispute 本身不参与公共 reputation 排名直到 resolution."). This file is
// the acceptance for both, and it is written to make each claim FAIL if the
// code stops holding it:
//
//	1. "修正不改 ledger 原事件" — a correction APPENDS a new declaration and
//	   the ledger's rows are byte-identical across it, read back field by
//	   field rather than by count; the earlier declaration is still
//	   readable; and the database refuses an UPDATE/DELETE/TRUNCATE of
//	   either the ledger rows or the declaration chain.
//	2. "dispute history 可查" — the open and the close of a dispute each
//	   produce exactly one ledger row, both carrying the same
//	   credit_dispute:<id> ref and the two different actors, so the history
//	   is read out of the append-only ledger rather than out of the
//	   mutable current-state row; the current state stays readable beside
//	   it.
//	3. the authorization re-uses the matrix's submit_scientific_review row
//	   and fails closed: contributor+ may open, maintainer/owner may declare
//	   and decide, viewer and non-member may do neither, an agent may do
//	   nothing — and nothing is written by any refusal.
//	4. migration 00103's guards refuse the writes that would make the
//	   record dishonest: rewriting a declaration, diverging a target_ref
//	   from its kind, inventing a credit role, re-opening a closed dispute,
//	   editing a claim, closing without a resolution.
//
// The commands are driven through the production command
// (credit.Command over persistence.CreditStore and authz.NewMatrixEngine),
// the history through the production projector
// (application/contribution.LedgerProjector over the production LedgerStore
// and the real outbox dispatcher). Nothing here re-implements a rule the
// product owns.

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	appcontribution "github.com/lichman0405/post/internal/application/contribution"
	"github.com/lichman0405/post/internal/application/credit"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/contribution"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

const creditTaskID = "T0809"

// creditWorld is the composed surface: one migrated database, the
// production credit command over the production store, the production
// ledger projection over the real dispatcher, and the fixture's ids.
type creditWorld struct {
	ctx        context.Context
	pool       *pgxpool.Pool
	store      *persistence.CreditStore
	cmd        *credit.Command
	projector  *appcontribution.LedgerProjector
	dispatcher *events.Dispatcher

	ownerID       string // owner: declare, open, close
	maintainerID  string // maintainer: declare, open, close
	contributorID string // contributor: open only
	viewerID      string // viewer: nothing
	outsiderID    string // authenticated, no membership: nothing
	orgID         string // an organization, to prove a credit party is not only a person

	projectID string
	// foreignProjectID is a second project the SAME owner owns: a credit
	// request authorized in it must not be able to write the first
	// project's target.
	foreignProjectID string

	assetPID string
	assetID  string
}

// newCreditWorld migrates a namespaced database and seeds the fixture: five
// users, one organization, two projects, their memberships and one research
// asset with a minted pid (the identity a credit is declared against).
func newCreditWorld(t *testing.T) *creditWorld {
	t.Helper()
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), creditTaskID)

	w := &creditWorld{ctx: ctx, pool: pool}
	w.ownerID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO users (handle, display_name) VALUES ('credit-owner', 'Owner') RETURNING id`)
	w.maintainerID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO users (handle, display_name) VALUES ('credit-maintainer', 'Maintainer') RETURNING id`)
	w.contributorID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO users (handle, display_name) VALUES ('credit-contributor', 'Contributor') RETURNING id`)
	w.viewerID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO users (handle, display_name) VALUES ('credit-viewer', 'Viewer') RETURNING id`)
	w.outsiderID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO users (handle, display_name) VALUES ('credit-outsider', 'Outsider') RETURNING id`)
	w.orgID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO organizations (slug, name) VALUES ('credit-lab', 'Credit Lab') RETURNING id`)

	w.projectID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO projects (slug, name, purpose, visibility, created_by)
		 VALUES ('credit-lab', 'Credit Lab', 'T0809 fixture', 'public', $1) RETURNING id`, w.ownerID)
	w.foreignProjectID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO projects (slug, name, purpose, visibility, created_by)
		 VALUES ('credit-other', 'Credit Other', 'T0809 fixture', 'public', $1) RETURNING id`, w.ownerID)
	for _, m := range []struct {
		project, user, role string
	}{
		{w.projectID, w.ownerID, "owner"},
		{w.projectID, w.maintainerID, "maintainer"},
		{w.projectID, w.contributorID, "contributor"},
		{w.projectID, w.viewerID, "viewer"},
		{w.foreignProjectID, w.ownerID, "owner"},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, $3)`,
			m.project, m.user, m.role); err != nil {
			t.Fatalf("seed membership %s: %v", m.role, err)
		}
	}

	pid, err := assets.NewPID()
	if err != nil {
		t.Fatalf("mint pid: %v", err)
	}
	w.assetPID = string(pid)
	w.assetID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO research_assets (asset_type, slug, title, origin_project_id, pid)
		 VALUES ('dataset', 'credit-subject', 'Credit Subject', $1, $2) RETURNING id`,
		w.projectID, w.assetPID)

	w.store = persistence.NewCreditStore(pool)
	w.cmd = credit.NewCommand(credit.Deps{
		Members: persistence.NewProjectStore(pool),
		Store:   w.store,
		Authz:   authz.NewMatrixEngine(),
	})
	w.projector = appcontribution.NewLedgerProjector(
		contribution.NewLedgerStore(pool), appcontribution.WithLedgerLogger(outboxTestLogger()))
	w.dispatcher = events.NewDispatcher(pool, events.WithLogger(outboxTestLogger()))
	return w
}

// target is the canonical ref a credit declaration and a dispute are about.
func (w *creditWorld) target() string { return "asset:" + w.assetPID }

// actor renders one of the fixture's users as the command's Actor.
func (w *creditWorld) actor(id string, isAgent bool) credit.Actor {
	return credit.Actor{User: domain.User{ID: id}, IsAgent: isAgent}
}

// projectEvents drains the outbox into research_events and projects the
// result into contribution_events — the two hops the production worker
// chain performs, driven once per step so a test can look between them.
func (w *creditWorld) projectEvents(t *testing.T) {
	t.Helper()
	if _, err := w.dispatcher.RunOnce(w.ctx); err != nil {
		t.Fatalf("publish outbox: %v", err)
	}
	if _, err := w.projector.RunOnce(w.ctx); err != nil {
		t.Fatalf("project ledger: %v", err)
	}
}

// declare drives the production command for one declaration.
func (w *creditWorld) declare(t *testing.T, actorID string, isAgent bool, projectID string, parties ...credit.PartyInput) (contribution.CreditAttribution, error) {
	t.Helper()
	return w.cmd.DeclareAttribution(w.ctx, w.actor(actorID, isAgent), credit.DeclareParams{
		ProjectID: projectID,
		TargetRef: w.target(),
		Parties:   parties,
	})
}

// open drives the production command for one dispute.
func (w *creditWorld) open(t *testing.T, actorID string, isAgent bool, projectID, claim string, evidence ...string) (contribution.CreditDispute, error) {
	t.Helper()
	return w.cmd.OpenDispute(w.ctx, w.actor(actorID, isAgent), credit.OpenDisputeParams{
		ProjectID:    projectID,
		TargetRef:    w.target(),
		Claim:        claim,
		EvidenceRefs: evidence,
	})
}

// close drives the production command for one decision.
func (w *creditWorld) close(t *testing.T, actorID string, isAgent bool, projectID, disputeID string, outcome contribution.DisputeState, resolution string) (contribution.CreditDispute, error) {
	t.Helper()
	return w.cmd.CloseDispute(w.ctx, w.actor(actorID, isAgent), credit.CloseDisputeParams{
		ProjectID:  projectID,
		DisputeID:  disputeID,
		Outcome:    outcome,
		Resolution: resolution,
	})
}

// creditLedgerRow is one contribution_events row as these tests inspect it.
type creditLedgerRow struct {
	ID               string
	ResearchEventID  string
	ActorID          string
	ProjectID        string
	EventType        string
	RoleCodes        []string
	ObjectRefs       []string
	AcceptedContext  bool
	ReleasedContext  bool
	OccurredAt       time.Time
	OrganizationAtTm *string
}

// ledgerRows reads every ledger row, in a stable order, from the DATABASE
// rather than through the projection under test.
func (w *creditWorld) ledgerRows(t *testing.T) []creditLedgerRow {
	t.Helper()
	rows, err := w.pool.Query(w.ctx,
		`SELECT id, research_event_id, actor_id, project_id, event_type, role_codes, object_refs,
		        accepted_context, released_context, occurred_at, organization_id_at_time
		   FROM contribution_events ORDER BY occurred_at, id`)
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	defer rows.Close()
	out := []creditLedgerRow{}
	for rows.Next() {
		var (
			r                     creditLedgerRow
			refs                  []byte
			org                   *string
			occurred              time.Time
			researchEvent, actor  *string
			project               *string
			id, eventType         string
			roles                 []string
			accepted, releasedCtx bool
		)
		if err := rows.Scan(&id, &researchEvent, &actor, &project, &eventType, &roles, &refs,
			&accepted, &releasedCtx, &occurred, &org); err != nil {
			t.Fatalf("scan the ledger: %v", err)
		}
		r.ID, r.EventType, r.RoleCodes = id, eventType, roles
		r.ObjectRefs = decodeRefs(t, refs)
		r.AcceptedContext, r.ReleasedContext = accepted, releasedCtx
		r.OccurredAt = occurred
		r.OrganizationAtTm = org
		if researchEvent != nil {
			r.ResearchEventID = *researchEvent
		}
		if actor != nil {
			r.ActorID = *actor
		}
		if project != nil {
			r.ProjectID = *project
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	return out
}

// ledgerSnapshot renders the ledger as one canonical string, so a claim
// that it did not move is checked against EVERY field rather than against a
// count — a rewrite that changed an actor or a ref would keep the count
// identical.
func (w *creditWorld) ledgerSnapshot(t *testing.T) string {
	t.Helper()
	rows := w.ledgerRows(t)
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, fmt.Sprintf(
			"id=%s research_event=%s event=%s actor=%s project=%s roles=%v refs=%v accepted=%t released=%t at=%s org=%v",
			r.ID, r.ResearchEventID, r.EventType, r.ActorID, r.ProjectID, r.RoleCodes,
			r.ObjectRefs, r.AcceptedContext, r.ReleasedContext, r.OccurredAt.UTC().Format(time.RFC3339Nano), r.OrganizationAtTm))
	}
	return strings.Join(parts, "\n")
}

func decodeRefs(t *testing.T, raw []byte) []string {
	t.Helper()
	var refs []string
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, &refs); err != nil {
		t.Fatalf("decode object_refs %s: %v", raw, err)
	}
	return refs
}

// rowsFor returns the ledger rows of one event type, in order.
func rowsFor(rows []creditLedgerRow, eventType string) []creditLedgerRow {
	out := []creditLedgerRow{}
	for _, r := range rows {
		if r.EventType == eventType {
			out = append(out, r)
		}
	}
	return out
}

// countIn counts rows of one table of one project directly, so an assertion
// about what a REFUSAL did or did not write does not depend on the reader
// under test.
func (w *creditWorld) countIn(t *testing.T, table, projectID string) int {
	t.Helper()
	var n int
	if err := w.pool.QueryRow(w.ctx,
		`SELECT count(*) FROM `+table+` WHERE project_id = $1`, projectID).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// ---------------------------------------------------------------------------
// 1. "修正不改 ledger 原事件"
// ---------------------------------------------------------------------------

// TestCreditAttributionCorrectionAppendsAndLeavesTheLedgerIntact is the
// first acceptance: docs/13 §2's "它可以被纠正，但 correction 作为新 event，
// 旧 attribution ... 保留".
//
// It is written so that each half fails independently: the ledger is
// snapshotted field-by-field before and after the correction (a rewrite
// that preserved the row count would still fail), the earlier declaration
// is read back after the correction, and the database is asked directly to
// rewrite both records.
func TestCreditAttributionCorrectionAppendsAndLeavesTheLedgerIntact(t *testing.T) {
	w := newCreditWorld(t)

	// A real ledger row exists before the correction: the dispute opened
	// below is projected through the production dispatcher and projector,
	// so "the ledger was not touched" is a statement about rows a producer
	// actually wrote.
	dispute, err := w.open(t, w.contributorID, false, w.projectID,
		"The declared creator list omits a contributor.", "object:"+w.assetID)
	if err != nil {
		t.Fatalf("open dispute: %v", err)
	}
	w.projectEvents(t)
	before := w.ledgerSnapshot(t)
	if !strings.Contains(before, string(contribution.RefCreditDispute)+":"+dispute.ID) {
		t.Fatalf("the opening's ledger row does not carry the dispute ref:\n%s", before)
	}

	// The first declaration: alice-style creator + the organization as a
	// major contributor.
	first, err := w.declare(t, w.ownerID, false, w.projectID,
		credit.PartyInput{Kind: domain.PartyUser, ID: w.contributorID, Role: contribution.CreditRoleCreator},
		credit.PartyInput{Kind: domain.PartyOrganization, ID: w.orgID, Role: contribution.CreditRoleMajorContributor},
	)
	if err != nil {
		t.Fatalf("declare credit: %v", err)
	}
	if first.Ordinal != 1 {
		t.Fatalf("first declaration ordinal = %d, want 1", first.Ordinal)
	}

	// The correction: a NEW declaration, with the creator list corrected.
	second, err := w.declare(t, w.ownerID, false, w.projectID,
		credit.PartyInput{Kind: domain.PartyUser, ID: w.maintainerID, Role: contribution.CreditRoleCreator},
		credit.PartyInput{Kind: domain.PartyOrganization, ID: w.orgID, Role: contribution.CreditRoleMajorContributor},
	)
	if err != nil {
		t.Fatalf("correct the credit: %v", err)
	}
	if second.Ordinal != 2 {
		t.Fatalf("correction ordinal = %d, want 2 (a correction APPENDS)", second.Ordinal)
	}
	if second.ID == first.ID {
		t.Fatal("the correction reused the first statement's row — it must be a new record")
	}

	// The acceptance, in the order it is worded: the ledger did not move...
	after := w.ledgerSnapshot(t)
	if after != before {
		t.Fatalf("the ledger changed across a credit correction:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
	// ...and the OLD attribution is still readable, item by item.
	chain, err := w.store.AttributionHistory(w.ctx, w.projectID, w.target())
	if err != nil {
		t.Fatalf("read attribution history: %v", err)
	}
	if len(chain) != 2 {
		t.Fatalf("attribution history has %d declarations, want 2 (the corrected one is kept)", len(chain))
	}
	if chain[0].ID != first.ID || chain[1].ID != second.ID {
		t.Fatalf("chain order = [%s %s], want [%s %s] (oldest first)", chain[0].ID, chain[1].ID, first.ID, second.ID)
	}
	if want := []contribution.CreditParty{
		{Party: contribution.PartyRef{Kind: "user", ID: w.contributorID}, Role: contribution.CreditRoleCreator, Position: 0},
		{Party: contribution.PartyRef{Kind: "organization", ID: w.orgID}, Role: contribution.CreditRoleMajorContributor, Position: 0},
	}; !sameParties(chain[0].Parties, want) {
		t.Fatalf("the corrected-away declaration reads back as %+v, want %+v — the old attribution must stay readable", chain[0].Parties, want)
	}
	current, err := w.store.CurrentAttribution(w.ctx, w.projectID, w.target())
	if err != nil {
		t.Fatalf("read current attribution: %v", err)
	}
	if current == nil || current.ID != second.ID {
		t.Fatalf("current attribution = %+v, want the greatest ordinal (%s)", current, second.ID)
	}

	// The database is asked directly, not through the store: a correction
	// could not have been an UPDATE even if a future caller wanted one.
	for _, stmt := range []string{
		`UPDATE credit_attribution_statements SET ordinal = 99 WHERE id = $1`,
		`DELETE FROM credit_attribution_statements WHERE id = $1`,
	} {
		if _, err := w.pool.Exec(w.ctx, stmt, first.ID); err == nil {
			t.Fatalf("the declaration chain accepted %q — it must be append-only", stmt)
		}
	}
	if _, err := w.pool.Exec(w.ctx,
		`UPDATE contribution_events SET actor_id = $1`, w.outsiderID); err == nil {
		t.Fatal("contribution_events accepted an UPDATE — the ledger the correction must not touch is rewritable")
	}
}

// sameParties compares two party lists on the identity, the role and the
// position, ignoring the display facts a reader may or may not have filled
// in.
func sameParties(got, want []contribution.CreditParty) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i].Party != want[i].Party || got[i].Role != want[i].Role || got[i].Position != want[i].Position {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// 2. "dispute history 可查"
// ---------------------------------------------------------------------------

// TestCreditDisputeHistoryIsQueryable is the second acceptance. The history
// is read out of the append-only LEDGER — the open and the close as two
// rows carrying the same dispute ref and the two different actors — and the
// current state is read beside it, so neither answer depends on the other.
func TestCreditDisputeHistoryIsQueryable(t *testing.T) {
	w := newCreditWorld(t)

	evidence := []string{"object:" + w.assetID, "state:" + w.foreignProjectID}
	opened, err := w.open(t, w.contributorID, false, w.projectID,
		"The creator list attributes my contribution to someone else.", evidence...)
	if err != nil {
		t.Fatalf("open dispute: %v", err)
	}
	if opened.State != contribution.DisputeStateOpen {
		t.Fatalf("a raised dispute is %q, want open", opened.State)
	}
	if opened.ResolvedAt != nil || opened.Resolution != "" {
		t.Fatalf("a raised dispute carries a decision: %+v", opened)
	}
	w.projectEvents(t)

	// The open half reaches the ledger: one row, the opener as its actor,
	// the dispute as its ref, and the evidence the claim was attached with.
	openRows := rowsFor(w.ledgerRows(t), "credit.dispute_opened")
	if len(openRows) != 1 {
		t.Fatalf("credit.dispute_opened rows = %d, want 1", len(openRows))
	}
	wantRef := string(contribution.RefCreditDispute) + ":" + opened.ID
	if got := openRows[0].ObjectRefs; len(got) != 1 || got[0] != wantRef {
		t.Fatalf("the opening's object_refs = %v, want [%s]", got, wantRef)
	}
	if openRows[0].ActorID != w.contributorID {
		t.Fatalf("the opening's actor = %s, want the contributor %s", openRows[0].ActorID, w.contributorID)
	}
	if []string(openRows[0].RoleCodes) != nil && len(openRows[0].RoleCodes) != 0 {
		t.Fatalf("the opening claims contribution roles %v — a dispute is not a credit role", openRows[0].RoleCodes)
	}
	if openRows[0].AcceptedContext || openRows[0].ReleasedContext {
		t.Fatalf("the opening claims a context flag: %+v", openRows[0])
	}
	assertEvidenceInOutbox(t, w, opened.ID, evidence)

	// Still open: the ledger says so, and so does the row.
	if closed := rowsFor(w.ledgerRows(t), "credit.dispute_resolved"); len(closed) != 0 {
		t.Fatalf("a resolution was recorded before any decision: %+v", closed)
	}

	// The decision.
	closed, err := w.close(t, w.maintainerID, false, w.projectID, opened.ID,
		contribution.DisputeStateResolved, "The ledger evidence supports the claim; the declaration is corrected.")
	if err != nil {
		t.Fatalf("close dispute: %v", err)
	}
	if closed.State != contribution.DisputeStateResolved || closed.ResolvedAt == nil {
		t.Fatalf("closed dispute = %+v, want state resolved with a resolved_at", closed)
	}
	// The claim is what it was: a resolution answers the claim, it does not
	// edit it (docs/13 §3's "不得直接改原事件").
	if closed.Claim != opened.Claim || closed.OpenedBy != opened.OpenedBy || closed.OpenedAt != opened.OpenedAt {
		t.Fatalf("closing rewrote the dispute's identity or claim:\nopen  %+v\nclose %+v", opened, closed)
	}
	w.projectEvents(t)

	// The close half reaches the ledger: one row, the DECIDER as its actor,
	// the same dispute ref — the pair is the history.
	ledger := w.ledgerRows(t)
	closeRows := rowsFor(ledger, "credit.dispute_resolved")
	if len(closeRows) != 1 {
		t.Fatalf("credit.dispute_resolved rows = %d, want 1", len(closeRows))
	}
	if got := closeRows[0].ObjectRefs; len(got) != 1 || got[0] != wantRef {
		t.Fatalf("the resolution's object_refs = %v, want [%s] (both halves join on the dispute)", got, wantRef)
	}
	if closeRows[0].ActorID != w.maintainerID {
		t.Fatalf("the resolution's actor = %s, want the deciding maintainer %s", closeRows[0].ActorID, w.maintainerID)
	}
	if openRows[0].ID == closeRows[0].ID {
		t.Fatal("the resolution reused the opening's ledger row — the history must be two rows")
	}

	// The whole history of one dispute, as a reader reconstructs it: the
	// two events in order, with the two actors, joined by the ref.
	history := []creditLedgerRow{}
	for _, r := range ledger {
		for _, ref := range r.ObjectRefs {
			if ref == wantRef {
				history = append(history, r)
			}
		}
	}
	if len(history) != 2 {
		t.Fatalf("ledger rows about dispute %s = %d, want 2 (open + close)", opened.ID, len(history))
	}
	if history[0].EventType != "credit.dispute_opened" || history[1].EventType != "credit.dispute_resolved" {
		t.Fatalf("history order = %q, %q; want the opening first", history[0].EventType, history[1].EventType)
	}
	if history[0].ActorID == history[1].ActorID {
		t.Fatal("the history records one actor for both halves — the two facts are attributable to different people")
	}

	// Re-projecting is harmless and changes nothing: the ledger is the
	// history, not a cache the next pass may rewrite.
	snapshot := w.ledgerSnapshot(t)
	w.projectEvents(t)
	if again := w.ledgerSnapshot(t); again != snapshot {
		t.Fatalf("re-projecting rewrote the ledger:\n--- before ---\n%s\n--- after ---\n%s", snapshot, again)
	}

	// The current state, read beside the history: the row says what the
	// dispute IS, the ledger says what happened to it.
	current, err := w.store.GetDispute(w.ctx, w.projectID, opened.ID)
	if err != nil {
		t.Fatalf("read the dispute: %v", err)
	}
	if current.State != contribution.DisputeStateResolved || current.Resolution == "" {
		t.Fatalf("the current state = %+v, want resolved with a resolution", current)
	}
	// The decision's reasoning is recorded verbatim, and it is NOT in the
	// ledger's row: the event carries identity and references, the row
	// carries the prose (see persistence/credit_store.go).
	if !strings.Contains(current.Resolution, "the declaration is corrected") {
		t.Fatalf("the resolution reads back as %q", current.Resolution)
	}

	// A second dispute about the same target is a new row: a decided
	// dispute is not re-opened (test 4 drives the refusal).
	second, err := w.open(t, w.contributorID, false, w.projectID, "A second, narrower claim.")
	if err != nil {
		t.Fatalf("open a second dispute: %v", err)
	}
	if second.ID == opened.ID {
		t.Fatal("the second dispute reused the first row")
	}
	listed, err := w.store.ListTargetDisputes(w.ctx, w.projectID, w.target())
	if err != nil {
		t.Fatalf("list disputes about the target: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("disputes about the target = %d, want 2", len(listed))
	}
	if listed[0].ID != second.ID {
		t.Fatalf("the listing is not newest-first: %s, %s", listed[0].ID, listed[1].ID)
	}
	// A dispute of another project is not readable through this one.
	if _, err := w.store.GetDispute(w.ctx, w.foreignProjectID, opened.ID); !errors.Is(err, credit.ErrDisputeNotFound) {
		t.Fatalf("reading a foreign project's dispute = %v, want ErrDisputeNotFound", err)
	}
}

// assertEvidenceInOutbox proves the evidence a claim is attached with is
// recorded on the event, which is where docs/13 §3's "附带 ledger evidence"
// lands: 00011's credit_disputes has no column for it, and the outbox
// payload is what the dispatcher copies into research_events verbatim.
func assertEvidenceInOutbox(t *testing.T, w *creditWorld, disputeID string, want []string) {
	t.Helper()
	var raw []byte
	if err := w.pool.QueryRow(w.ctx,
		`SELECT payload FROM outbox_events WHERE event_type = 'credit.dispute_opened' ORDER BY id DESC LIMIT 1`).Scan(&raw); err != nil {
		t.Fatalf("read the opening's outbox payload: %v", err)
	}
	var payload struct {
		DisputeID    string   `json:"dispute_id"`
		TargetRef    string   `json:"target_ref"`
		EvidenceRefs []string `json:"evidence_refs"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode the opening's payload: %v", err)
	}
	if payload.DisputeID != disputeID {
		t.Fatalf("the payload names dispute %q, want %s", payload.DisputeID, disputeID)
	}
	if payload.TargetRef != w.target() {
		t.Fatalf("the payload's target_ref = %q, want %q", payload.TargetRef, w.target())
	}
	if !reflect.DeepEqual(payload.EvidenceRefs, want) {
		t.Fatalf("the payload's evidence = %v, want %v", payload.EvidenceRefs, want)
	}
}

// ---------------------------------------------------------------------------
// 3. the authorization, re-using the matrix's submit_scientific_review row
// ---------------------------------------------------------------------------

// TestCreditDisputeAuthorization proves the permission shape the task
// derives from the specs: 发起 only Contributor and above (docs/13 §3), 处理
// only Maintainer/Owner (same sentence), agent never (docs/60 §2) — and
// every refusal writes nothing.
func TestCreditDisputeAuthorization(t *testing.T) {
	w := newCreditWorld(t)
	parties := []credit.PartyInput{{Kind: domain.PartyUser, ID: w.contributorID, Role: contribution.CreditRoleCreator}}

	type row struct {
		who       string
		userID    string
		agent     bool
		declareOK bool
		openOK    bool
	}
	cases := []row{
		{who: "owner", userID: w.ownerID, declareOK: true, openOK: true},
		{who: "maintainer", userID: w.maintainerID, declareOK: true, openOK: true},
		{who: "contributor", userID: w.contributorID, declareOK: false, openOK: true},
		{who: "viewer", userID: w.viewerID, declareOK: false, openOK: false},
		{who: "authenticated non-member", userID: w.outsiderID, declareOK: false, openOK: false},
		{who: "agent acting as the owner", userID: w.ownerID, agent: true, declareOK: false, openOK: false},
	}
	for _, c := range cases {
		t.Run(c.who, func(t *testing.T) {
			declarationsBefore := w.countIn(t, "credit_attribution_statements", w.projectID)
			disputesBefore := w.countIn(t, "credit_disputes", w.projectID)
			auditsBefore := w.countIn(t, "audit_log", w.projectID)
			// effects counts the writes this subtest PERMITS — a
			// declaration, a dispute, a decision — so the audit delta can
			// be held to exactly those and nothing else.
			effects := 0

			_, declareErr := w.declare(t, c.userID, c.agent, w.projectID, parties...)
			if c.declareOK && declareErr != nil {
				t.Fatalf("%s declaring credit: %v, want permitted", c.who, declareErr)
			}
			if !c.declareOK {
				if !errors.Is(declareErr, credit.ErrForbidden) && !errors.Is(declareErr, credit.ErrAgentNotPermitted) {
					t.Fatalf("%s declaring credit = %v, want a refusal", c.who, declareErr)
				}
			}

			dispute, openErr := w.open(t, c.userID, c.agent, w.projectID, "A claim from "+c.who)
			if c.openOK && openErr != nil {
				t.Fatalf("%s opening a dispute: %v, want permitted", c.who, openErr)
			}
			if !c.openOK {
				if !errors.Is(openErr, credit.ErrForbidden) && !errors.Is(openErr, credit.ErrAgentNotPermitted) {
					t.Fatalf("%s opening a dispute = %v, want a refusal", c.who, openErr)
				}
			}

			// Nothing a refusal touched may have been written.
			wantDeclarations := declarationsBefore
			if c.declareOK {
				wantDeclarations++
				effects++
			}
			if got := w.countIn(t, "credit_attribution_statements", w.projectID); got != wantDeclarations {
				t.Fatalf("%s left %d declarations, want %d", c.who, got, wantDeclarations)
			}
			wantDisputes := disputesBefore
			if c.openOK {
				wantDisputes++
				effects++
			}
			if got := w.countIn(t, "credit_disputes", w.projectID); got != wantDisputes {
				t.Fatalf("%s left %d disputes, want %d", c.who, got, wantDisputes)
			}

			// Deciding is Maintainer/Organization governance: an open
			// dispute may only be closed by maintainer or owner, and never
			// by an agent.
			if c.openOK {
				closed, closeErr := w.close(t, c.userID, c.agent, w.projectID, dispute.ID,
					contribution.DisputeStateResolved, "decided by "+c.who)
				_, decideOK := decideAllowed(c.who, c.agent)
				if decideOK && closeErr != nil {
					t.Fatalf("%s deciding a dispute: %v, want permitted", c.who, closeErr)
				}
				if !decideOK {
					if !errors.Is(closeErr, credit.ErrForbidden) && !errors.Is(closeErr, credit.ErrAgentNotPermitted) {
						t.Fatalf("%s deciding a dispute = %v, want a refusal", c.who, closeErr)
					}
					stored, err := w.store.GetDispute(w.ctx, w.projectID, dispute.ID)
					if err != nil {
						t.Fatalf("read the dispute a refusal targeted: %v", err)
					}
					if stored.State != contribution.DisputeStateOpen {
						t.Fatalf("%s closed a dispute they may not decide: state = %q", c.who, stored.State)
					}
				} else {
					if closed.State != contribution.DisputeStateResolved {
						t.Fatalf("decided dispute state = %q", closed.State)
					}
					effects++
				}
			}

			// The audit delta is exactly the permitted effects: a refusal
			// is not an act and must not be audited as one, and a
			// permitted act must not be silent.
			if got := w.countIn(t, "audit_log", w.projectID) - auditsBefore; got != effects {
				t.Fatalf("%s wrote %d audit rows for %d permitted effects — a refusal must not be audited as an act",
					c.who, got, effects)
			}
		})
	}

	// A contributor may NOT decide: the row's conditional cell resolves to
	// "may open" for them and to nothing more. Driven once, explicitly,
	// because it is the boundary the whole reuse turns on.
	t.Run("contributor cannot decide", func(t *testing.T) {
		dispute, err := w.open(t, w.contributorID, false, w.projectID, "A claim to be decided elsewhere.")
		if err != nil {
			t.Fatalf("open dispute: %v", err)
		}
		if _, err := w.close(t, w.contributorID, false, w.projectID, dispute.ID,
			contribution.DisputeStateRejected, "self-service rejection"); !errors.Is(err, credit.ErrForbidden) {
			t.Fatalf("a contributor closing their own dispute = %v, want ErrForbidden", err)
		}
	})
}

// decideAllowed encodes which of the actors above may decide a dispute:
// maintainer and owner, never an agent, never anyone below maintainer.
func decideAllowed(who string, isAgent bool) (string, bool) {
	if isAgent {
		return who, false
	}
	return who, who == "owner" || who == "maintainer"
}

// TestCreditTargetScopeFailsClosed proves the two ways a credit operation
// can be aimed at the wrong thing are refused identically, so neither can
// be used as an oracle: a ref no row carries, and a real row of a project
// the caller's authorization was not resolved against.
func TestCreditTargetScopeFailsClosed(t *testing.T) {
	w := newCreditWorld(t)

	// A ref of the right SHAPE naming nothing at all.
	if _, err := w.cmd.DeclareAttribution(w.ctx, w.actor(w.ownerID, false), credit.DeclareParams{
		ProjectID: w.projectID,
		TargetRef: "asset:0123456789abcdefghjkmnpqrs",
		Parties:   []credit.PartyInput{{Kind: domain.PartyUser, ID: w.contributorID, Role: contribution.CreditRoleCreator}},
	}); !errors.Is(err, credit.ErrTargetNotFound) {
		t.Fatalf("declaring credit for an unknown pid = %v, want ErrTargetNotFound", err)
	}

	// The fixture's asset belongs to projectID. The same owner, authorized
	// in the FOREIGN project, must not be able to declare its credit there.
	if _, err := w.cmd.DeclareAttribution(w.ctx, w.actor(w.ownerID, false), credit.DeclareParams{
		ProjectID: w.foreignProjectID,
		TargetRef: w.target(),
		Parties:   []credit.PartyInput{{Kind: domain.PartyUser, ID: w.contributorID, Role: contribution.CreditRoleCreator}},
	}); !errors.Is(err, credit.ErrTargetNotFound) {
		t.Fatalf("declaring credit for another project's asset = %v, want ErrTargetNotFound", err)
	}
	if _, err := w.cmd.OpenDispute(w.ctx, w.actor(w.ownerID, false), credit.OpenDisputeParams{
		ProjectID: w.foreignProjectID,
		TargetRef: w.target(),
		Claim:     "cross-project claim",
	}); !errors.Is(err, credit.ErrTargetNotFound) {
		t.Fatalf("disputing another project's asset = %v, want ErrTargetNotFound", err)
	}

	// And a release ref whose value is not a uuid is a malformed request,
	// not a lookup.
	if _, err := w.cmd.DeclareAttribution(w.ctx, w.actor(w.ownerID, false), credit.DeclareParams{
		ProjectID: w.projectID,
		TargetRef: "release:not-a-uuid",
		Parties:   []credit.PartyInput{{Kind: domain.PartyUser, ID: w.contributorID, Role: contribution.CreditRoleCreator}},
	}); !errors.Is(err, credit.ErrValidation) {
		t.Fatalf("declaring credit for a malformed release ref = %v, want ErrValidation", err)
	}
	// A kind docs/13 §2 does not name is not a target at all.
	if _, err := w.cmd.DeclareAttribution(w.ctx, w.actor(w.ownerID, false), credit.DeclareParams{
		ProjectID: w.projectID,
		TargetRef: "project:" + w.projectID,
		Parties:   []credit.PartyInput{{Kind: domain.PartyUser, ID: w.contributorID, Role: contribution.CreditRoleCreator}},
	}); !errors.Is(err, credit.ErrValidation) {
		t.Fatalf("declaring credit for a project ref = %v, want ErrValidation", err)
	}
	// An identity kind with no table behind it is refused before the write.
	if _, err := w.cmd.DeclareAttribution(w.ctx, w.actor(w.ownerID, false), credit.DeclareParams{
		ProjectID: w.projectID,
		TargetRef: w.target(),
		Parties:   []credit.PartyInput{{Kind: domain.PartyProject, ID: w.projectID, Role: contribution.CreditRoleCreator}},
	}); !errors.Is(err, credit.ErrValidation) {
		t.Fatalf("declaring a project as a credit party = %v, want ErrValidation", err)
	}
	// A party that names no row of its kind.
	if _, err := w.cmd.DeclareAttribution(w.ctx, w.actor(w.ownerID, false), credit.DeclareParams{
		ProjectID: w.projectID,
		TargetRef: w.target(),
		Parties: []credit.PartyInput{{
			Kind: domain.PartyOrganization, ID: "11111111-2222-3333-4444-555555555555",
			Role: contribution.CreditRoleCreator,
		}},
	}); !errors.Is(err, credit.ErrPartyNotFound) {
		t.Fatalf("declaring a nonexistent organization = %v, want ErrPartyNotFound", err)
	}
	if got := w.countIn(t, "credit_attribution_statements", w.projectID); got != 0 {
		t.Fatalf("%d declarations survived the refusals — a refused write must leave nothing", got)
	}
}

// ---------------------------------------------------------------------------
// 4. the guards: the writes that would make the record dishonest
// ---------------------------------------------------------------------------

// TestCreditDisputeGuardsRefuseIllegalWrites drives the DATABASE directly,
// without the command, for the writes that would make the record
// dishonest. Each is a statement the command would never send, which is
// the point: the guard is what makes the rule true for every write path,
// not just the one this task wrote.
func TestCreditDisputeGuardsRefuseIllegalWrites(t *testing.T) {
	w := newCreditWorld(t)

	open, err := w.open(t, w.contributorID, false, w.projectID, "An honest claim.")
	if err != nil {
		t.Fatalf("open dispute: %v", err)
	}
	declared, err := w.declare(t, w.ownerID, false, w.projectID,
		credit.PartyInput{Kind: domain.PartyUser, ID: w.contributorID, Role: contribution.CreditRoleCreator})
	if err != nil {
		t.Fatalf("declare credit: %v", err)
	}

	closed, err := w.close(t, w.maintainerID, false, w.projectID, open.ID,
		contribution.DisputeStateRejected, "The evidence does not support the claim.")
	if err != nil {
		t.Fatalf("close dispute: %v", err)
	}
	if closed.State != contribution.DisputeStateRejected {
		t.Fatalf("closed dispute state = %q, want rejected", closed.State)
	}

	// A dispute is born open, and it is born undecided: a row inserted as
	// resolved would have no opening and no credit.dispute_opened event.
	if _, err := w.pool.Exec(w.ctx,
		`INSERT INTO credit_disputes (project_id, opened_by, target_ref, claim, state, resolution, resolved_at)
		 VALUES ($1, $2, $3, 'born decided', 'resolved', 'because', now())`,
		w.projectID, w.contributorID, w.target()); err == nil {
		t.Fatal("credit_disputes accepted a dispute born resolved — it must be born open and undecided")
	}
	// The claim is immutable: a resolution answers the claim, it does not
	// edit it (docs/13 §3).
	if _, err := w.pool.Exec(w.ctx,
		`UPDATE credit_disputes SET claim = 'rewritten' WHERE id = $1`, open.ID); err == nil {
		t.Fatal("credit_disputes accepted a rewritten claim")
	}
	// The opener is immutable too: the record of who raised the claim is
	// what the credit.dispute_opened event is about.
	if _, err := w.pool.Exec(w.ctx,
		`UPDATE credit_disputes SET opened_by = $1 WHERE id = $2`, w.outsiderID, open.ID); err == nil {
		t.Fatal("credit_disputes accepted a rewritten opener")
	}
	// A closed dispute is terminal: not re-opened, not re-decided.
	for _, stmt := range []string{
		`UPDATE credit_disputes SET state = 'open', resolution = NULL, resolved_at = NULL WHERE id = $1`,
		`UPDATE credit_disputes SET state = 'resolved', resolution = 'again', resolved_at = now() WHERE id = $1`,
		`UPDATE credit_disputes SET resolution = 'quietly reworded' WHERE id = $1`,
	} {
		if _, err := w.pool.Exec(w.ctx, stmt, open.ID); err == nil {
			t.Fatalf("credit_disputes accepted %q on a closed dispute", stmt)
		}
	}
	// The command refuses it too, and writes no second event.
	if _, err := w.close(t, w.maintainerID, false, w.projectID, open.ID,
		contribution.DisputeStateResolved, "second thoughts"); !errors.Is(err, credit.ErrDisputeClosed) {
		t.Fatalf("re-deciding a closed dispute = %v, want ErrDisputeClosed", err)
	}
	w.projectEvents(t)
	if rows := rowsFor(w.ledgerRows(t), "credit.dispute_resolved"); len(rows) != 1 {
		t.Fatalf("credit.dispute_resolved rows = %d, want 1 — the refused decision must not be recorded", len(rows))
	}

	// The declaration chain is append-only, and its own shape is checked by
	// the database: the kind and the ref cannot disagree, a role outside
	// the two admitted ones cannot be written (00103's header records the
	// L3 decision that the 等 of docs/13 §2 stays open until ratified).
	for _, stmt := range []struct {
		why  string
		sql  string
		args []any
	}{
		{"rewrite a declaration", `UPDATE credit_attribution_statements SET target_ref = 'asset:other' WHERE id = $1`, []any{declared.ID}},
		{"delete a declaration", `DELETE FROM credit_attribution_statements WHERE id = $1`, []any{declared.ID}},
		{"truncate the declaration chain", `TRUNCATE credit_attribution_statements`, nil},
		{"rewrite a party", `UPDATE credit_attribution_parties SET role = 'major_contributor' WHERE statement_id = $1`, []any{declared.ID}},
		{"delete a party", `DELETE FROM credit_attribution_parties WHERE statement_id = $1`, []any{declared.ID}},
		{"truncate the party rows", `TRUNCATE credit_attribution_parties`, nil},
		{"split a target_ref from its kind",
			`INSERT INTO credit_attribution_statements (project_id, target_kind, target_ref, ordinal, recorded_by)
			 VALUES ($1, 'asset', 'finding:' || gen_random_uuid(), 9, $2)`, []any{w.projectID, w.ownerID}},
		{"invent a credit role",
			`INSERT INTO credit_attribution_parties (statement_id, role, party_kind, party_id, "position")
			 VALUES ($1, 'method_designer', 'user', $2, 1)`, []any{declared.ID, w.contributorID}},
		{"reuse an ordinal",
			`INSERT INTO credit_attribution_statements (project_id, target_kind, target_ref, ordinal, recorded_by)
			 VALUES ($1, 'asset', $2, 1, $3)`, []any{w.projectID, w.target(), w.ownerID}},
	} {
		if _, err := w.pool.Exec(w.ctx, stmt.sql, stmt.args...); err == nil {
			t.Fatalf("the database accepted %q", stmt.why)
		}
	}

	// The audit rows the two real acts wrote are append-only like every
	// other audit row (docs/26 §5 puts credit dispute in the high-risk
	// tier), and they are still readable.
	if _, err := w.pool.Exec(w.ctx, `UPDATE audit_log SET action = 'rewritten'`); err == nil {
		t.Fatal("audit_log accepted an UPDATE")
	}
	var audits int
	if err := w.pool.QueryRow(w.ctx,
		`SELECT count(*) FROM audit_log WHERE action = ANY($1)`,
		[]string{
			contribution.AuditActionCreditAttributionDeclared,
			contribution.AuditActionCreditDisputeOpened,
			contribution.AuditActionCreditDisputeResolved,
		}).Scan(&audits); err != nil {
		t.Fatalf("read the credit audit rows: %v", err)
	}
	if audits != 3 {
		t.Fatalf("credit audit rows = %d, want 3 (declare + open + close)", audits)
	}

	// The two guards themselves are installed at head, so a database that
	// skipped 00103 cannot pass this test by having no guard at all.
	for table, trigger := range map[string]string{
		"credit_attribution_statements": "credit_attribution_statements_append_only",
		"credit_attribution_parties":    "credit_attribution_parties_append_only",
		"credit_disputes":               "credit_disputes_state_guard",
	} {
		var n int
		if err := w.pool.QueryRow(w.ctx,
			`SELECT count(*) FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
			  WHERE NOT t.tgisinternal AND c.relname = $1 AND t.tgname = $2 AND t.tgenabled = 'O'`,
			table, trigger).Scan(&n); err != nil {
			t.Fatalf("read the trigger catalog: %v", err)
		}
		if n != 1 {
			t.Fatalf("trigger %s on %s is not installed and enabled", trigger, table)
		}
	}
}

// TestCreditDisputeVocabularyMatchesTheSpecs pins the vocabularies this
// task introduced against their spec sources, over a real connection (the
// CHECKs are the database's copy of them), so the Go side and the schema
// cannot drift apart.
func TestCreditDisputeVocabularyMatchesTheSpecs(t *testing.T) {
	w := newCreditWorld(t)

	for _, role := range contribution.CreditRoles() {
		if !contribution.ValidCreditRole(role) {
			t.Fatalf("CreditRoles() returned %q, which ValidCreditRole refuses", role)
		}
	}
	// The exact set, not just membership: widening it is a product decision
	// and must fail here first.
	if got := contribution.CreditRoles(); !reflect.DeepEqual(got, []contribution.CreditRole{
		contribution.CreditRoleCreator, contribution.CreditRoleMajorContributor,
	}) {
		t.Fatalf("CreditRoles() = %v", got)
	}
	// docs/13 §2's three targets, and the canonical ref round-trip.
	for _, kind := range []contribution.CreditTargetKind{
		contribution.CreditTargetAsset, contribution.CreditTargetRelease, contribution.CreditTargetFinding,
	} {
		ref, ok := contribution.NewCreditTargetRef(kind, "value")
		if !ok {
			t.Fatalf("NewCreditTargetRef(%q, value) refused", kind)
		}
		backKind, backValue, ok := contribution.ParseCreditTargetRef(ref)
		if !ok || backKind != kind || backValue != "value" {
			t.Fatalf("ParseCreditTargetRef(%q) = %q, %q, %t", ref, backKind, backValue, ok)
		}
	}
	for _, bad := range []string{"", "asset", "asset:", ":value", "asset:a:b", "project:x", "object:x"} {
		if _, _, ok := contribution.ParseCreditTargetRef(bad); ok {
			t.Fatalf("ParseCreditTargetRef(%q) accepted a ref that is not a credit target", bad)
		}
	}
	// The dispute machine: open is not terminal, both decisions are, and
	// nothing outside the three states is a state.
	if contribution.DisputeStateOpen.Terminal() {
		t.Fatal("open must not be terminal")
	}
	if !contribution.DisputeStateResolved.Terminal() || !contribution.DisputeStateRejected.Terminal() {
		t.Fatal("a decision must be terminal")
	}
	for _, s := range []contribution.DisputeState{"", "closed", "OPEN", "decided"} {
		if contribution.ValidDisputeState(s) {
			t.Fatalf("ValidDisputeState(%q) = true", s)
		}
	}
	// The ledger's own ref kind for a dispute, and the check the command
	// uses on the evidence refs.
	if !contribution.ValidLedgerRefKind(contribution.RefCreditDispute) {
		t.Fatal("RefCreditDispute is not a valid ledger ref kind")
	}
	if contribution.ValidLedgerRefKind("credit_title") {
		t.Fatal("ValidLedgerRefKind accepted a kind the projection does not produce")
	}
	// Evidence that is not a ledger ref is refused by the command.
	if _, err := w.open(t, w.contributorID, false, w.projectID, "claim", "credit_title:invented"); !errors.Is(err, credit.ErrValidation) {
		t.Fatalf("an invented evidence ref = %v, want ErrValidation", err)
	}
	if _, err := w.open(t, w.contributorID, false, w.projectID, "claim", "object"); !errors.Is(err, credit.ErrValidation) {
		t.Fatalf("a kindless evidence ref = %v, want ErrValidation", err)
	}
	// Duplicates collapse, order is the caller's.
	opened, err := w.open(t, w.contributorID, false, w.projectID, "claim",
		"state:"+w.projectID, "object:"+w.assetID, "state:"+w.projectID)
	if err != nil {
		t.Fatalf("open dispute with evidence: %v", err)
	}
	if !reflect.DeepEqual(opened.EvidenceRefs, []string{"state:" + w.projectID, "object:" + w.assetID}) {
		t.Fatalf("evidence refs = %v, want the caller's order with duplicates collapsed", opened.EvidenceRefs)
	}
	// A claim or a resolution that says nothing is not a claim or a
	// resolution.
	if _, err := w.open(t, w.contributorID, false, w.projectID, "   "); !errors.Is(err, credit.ErrValidation) {
		t.Fatalf("an empty claim = %v, want ErrValidation", err)
	}
	if _, err := w.close(t, w.maintainerID, false, w.projectID, opened.ID, contribution.DisputeStateResolved, "  "); !errors.Is(err, credit.ErrValidation) {
		t.Fatalf("an empty resolution = %v, want ErrValidation", err)
	}
	if _, err := w.close(t, w.maintainerID, false, w.projectID, opened.ID, contribution.DisputeStateOpen, "still open"); !errors.Is(err, credit.ErrValidation) {
		t.Fatalf("closing with outcome open = %v, want ErrValidation", err)
	}
}
