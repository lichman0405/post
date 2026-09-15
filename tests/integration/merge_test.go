package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/lichman0405/post/internal/application/diffs"
	"github.com/lichman0405/post/internal/application/merge"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/application/relations"
	"github.com/lichman0405/post/internal/application/resolutions"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence"
	rsgconflict "github.com/lichman0405/post/internal/rsg/conflict"
	rsgmerge "github.com/lichman0405/post/internal/rsg/merge"
)

// Task T0406: Semantic Merge Engine — over a REAL PostgreSQL, with the same
// store composition the API uses. Proves the task's four acceptance
// criteria end to end:
//
//   - safe merge 结果 deterministic: a clean three-way merge lands, and the
//     plan the service executed re-derives byte-for-byte from the same three
//     states and the same decisions (its stored digest is that re-derivation's
//     sha256);
//   - 科学冲突不自动 winner: both sides moving the same scientific field is
//     refused — no decision, no merge, nothing written, and no change is
//     materialized for it;
//   - 科学冲突允许在 main 里保持 contested/unresolved: a decided keep-both /
//     unresolved conflict is CARRIED into the accepted state (both versions
//     named, the human decider recorded) instead of being resolved into a
//     winner, and all seven docs/09 §8 actions are storable and have a
//     defined effect;
//   - merge 只改变 accepted state, private source → public target 不公开:
//     a private source's changes are withheld from a public target, and the
//     merge writes nothing at all.

// mergeMainGateClaim is a claim payload complete for the MAIN gate: the
// type's schema fields plus the docs/08 domain fields the gate ladder makes
// blocking at main (subject_ref, property, scope, assessment). The merge is
// the last chance to complete them, so a merge fixture that wants a merge to
// LAND has to write main-gate-complete content.
func mergeMainGateClaim(statement string) string {
	return `{"statement":"` + statement + `","claim_type":"descriptive",` +
		`"subject_ref":"material-0001","property":"band_gap",` +
		`"scope":{"system":"fixture"},"assessment":"preliminary"}`
}

// mergeMainGateFinding is the finding sibling of mergeMainGateClaim.
func mergeMainGateFinding(statement, claimVersionID string) string {
	return `{"statement":"` + statement + `","finding_type":"observation",` +
		`"claim_version_refs":["` + claimVersionID + `"],"assessment":"preliminary"}`
}

// mergeProtocolPayload is a protocol complete for the main gate (domain,
// steps, parameters, requirements, plus the schema's purpose) whose only
// movable content is one numeric parameter. Both sides moving it to
// different numbers is docs/09 §7's "Protocol 冲突数值折中" — the case the
// classifier calls a scientific conflict, and the case a merge must never
// average into a value neither branch proposed.
func mergeProtocolPayload(temperature string) string {
	return `{"purpose":"annealing protocol","domain":"materials",` +
		`"steps":[{"id":"s1","action":"heat"}],` +
		`"parameters":{"temperature":"` + temperature + `"},` +
		`"requirements":["dry glovebox"]}`
}

// mergeFixture is the diff fixture plus the merge stack: the PR service (to
// drive a proposal to merge_ready), the resolution store (the human plan) and
// the merge service itself, wired exactly like cmd/api wires it — every port
// is a real adapter, and the provider-side Git merger is absent because
// T0409 owns that adapter.
type mergeFixture struct {
	*diffFixture
	prs        *pullrequests.Service
	resolution *resolutions.Service
	merges     *merge.Service
	mergeStore *persistence.SemanticMergeStore
	branches   *persistence.BranchStore
}

// newMergeFixture builds the stack over a private project. projectVisibility
// and targetVisibility pick the pair the docs/09 §9 publication rule reads
// (a public branch only exists inside a public project, docs/12 §3).
func newMergeFixture(t *testing.T, ctx context.Context, projectVisibility domain.ProjectVisibility, targetVisibility domain.BranchVisibility) *mergeFixture {
	t.Helper()
	d := newDiffFixtureWithVisibility(t, ctx, projectVisibility, targetVisibility)
	t.Helper()
	projectStore := persistence.NewProjectStore(d.pool)
	orgStore := persistence.NewOrgStore(d.pool)
	projectSvc := projects.NewService(projectStore, orgStore, authz.NewMatrixEngine())
	resolutionSvc := resolutions.NewService(d.diffs, resolutions.NewPGStore(d.pool), projectSvc, authz.NewMatrixEngine())
	store := persistence.NewSemanticMergeStore(d.pool)
	return &mergeFixture{
		diffFixture: d,
		prs:         pullrequests.NewService(persistence.NewPullRequestStore(d.pool)),
		resolution:  resolutionSvc,
		mergeStore:  store,
		branches:    persistence.NewBranchStore(d.pool),
		merges: merge.NewService(merge.Deps{
			Store:     store,
			Diffs:     d.diffs,
			Plans:     resolutionSvc,
			Commits:   d.statesSvc,
			Objects:   persistence.NewScientificObjectStore(d.pool),
			Relations: persistence.NewRelationStore(d.pool),
			Projects:  projectSvc,
			Authz:     authz.NewMatrixEngine(),
			// Git is nil on purpose: the provider-side merge adapter is
			// T0409's, so the saga must record the step as not-done instead
			// of claiming the ref moved.
		}),
	}
}

// fork creates a research branch off main's current head and returns it.
func (f *mergeFixture) fork(t *testing.T, ctx context.Context, name string, visibility domain.BranchVisibility) domain.Branch {
	t.Helper()
	branch, err := f.svc.CreateBranch(ctx, f.alice, f.project.ID, rsg.CreateBranchInput{
		Name:       name,
		BaseRef:    *f.head(t, ctx, f.main),
		Visibility: visibility,
	})
	if err != nil {
		t.Fatalf("create %s branch: %v", name, err)
	}
	return branch
}

// openPR opens a proposal and drives it through the docs/43 review machine
// to merge_ready — the only state the merge engine accepts.
func (f *mergeFixture) openPR(t *testing.T, ctx context.Context, sourceBranchID, title string) domain.PullRequest {
	t.Helper()
	pr, err := f.prs.Create(ctx, pullrequests.CreatePullRequestParams{
		ProjectID:      f.project.ID,
		SourceBranchID: sourceBranchID,
		TargetBranchID: f.main,
		Title:          title,
		CreatedBy:      f.alice.ID,
	})
	if err != nil {
		t.Fatalf("create pull request: %v", err)
	}
	if _, err := f.prs.RequestReview(ctx, f.project.ID, pr.Number); err != nil {
		t.Fatalf("request review: %v", err)
	}
	if _, err := f.prs.SetState(ctx, f.project.ID, pr.Number, domain.PullRequestStateApproved); err != nil {
		t.Fatalf("approve: %v", err)
	}
	ready, err := f.prs.SetState(ctx, f.project.ID, pr.Number, domain.PullRequestStateMergeReady)
	if err != nil {
		t.Fatalf("mark merge ready: %v", err)
	}
	return ready
}

// refresh re-pins the PR's proposed head to the source branch's current head
// (the explicit head refresh — the only path that moves it).
func (f *mergeFixture) refresh(t *testing.T, ctx context.Context, number int64) domain.PullRequest {
	t.Helper()
	pr, err := f.prs.RefreshProposed(ctx, f.project.ID, number)
	if err != nil {
		t.Fatalf("refresh proposed head: %v", err)
	}
	return pr
}

// plan re-derives the merge plan straight from the engine over the real
// inputs — the reference the service's own result is compared against.
// It is deliberately NOT the service's code path: the point is that two
// independent derivations of the same triple agree.
func (f *mergeFixture) plan(t *testing.T, ctx context.Context, base, source, target string, srcVis, tgtVis domain.BranchVisibility) *rsgmerge.Plan {
	t.Helper()
	inputs, err := f.diffs.Inputs(ctx, diffs.Params{
		ProjectID:     f.project.ID,
		BaseStateID:   base,
		SourceStateID: source,
		TargetStateID: target,
	})
	if err != nil {
		t.Fatalf("diff inputs: %v", err)
	}
	recorded, err := f.resolution.Plan(ctx, f.project.ID, base, source, target)
	if err != nil {
		t.Fatalf("read resolution plan: %v", err)
	}
	decisions := make([]rsgmerge.Decision, 0, len(recorded))
	for _, r := range recorded {
		var other string
		if r.OtherObjectID != nil {
			other = *r.OtherObjectID
		}
		decisions = append(decisions, rsgmerge.Decision{
			TargetKind: r.TargetKind, TargetID: r.TargetID, Code: r.Code,
			Fields: r.Fields, PayloadKeys: r.PayloadKeys, OtherObjectID: other,
			Kind: r.Kind, DecidedBy: r.DecidedBy, Note: r.Note,
		})
	}
	plan, err := rsgmerge.Merge(rsgmerge.Inputs{
		ProjectID:        f.project.ID,
		Diff:             inputs,
		Decisions:        decisions,
		SourceVisibility: srcVis,
		TargetVisibility: tgtVis,
	})
	if err != nil {
		t.Fatalf("plan merge: %v", err)
	}
	return plan
}

// conflictsOf returns EVERY classified conflict the T0405 detector reports
// for one object of the triple. A change carries more than one when more than
// one divergence is classified (a moved statement diverges the derived title
// as well as the payload), and the merge is only unblocked once the humans
// decided all of them — so the tests must decide the whole list.
func (f *mergeFixture) conflictsOf(t *testing.T, ctx context.Context, base, source, target, objectID string) []rsgconflict.Conflict {
	t.Helper()
	report, err := f.diffs.Conflicts(ctx, diffs.Params{
		ProjectID:     f.project.ID,
		BaseStateID:   base,
		SourceStateID: source,
		TargetStateID: target,
	})
	if err != nil {
		t.Fatalf("detect conflicts: %v", err)
	}
	for _, v := range report.ObjectVerdicts {
		if v.ObjectID != objectID {
			continue
		}
		if len(v.Conflicts) == 0 {
			t.Fatalf("verdict for %s carries no conflict: %+v", objectID, v)
		}
		return v.Conflicts
	}
	t.Fatalf("report has no verdict for object %s", objectID)
	return nil
}

// conflictWithCategory picks the one conflict of a change the detector
// classified as want — the conflict an assertion about a specific docs/09 §6
// class is made on. It fails rather than returning a neighbouring conflict:
// asserting the taxonomy against whatever happens to be first is how a test
// ends up claiming a category it never checked.
func conflictWithCategory(t *testing.T, conflicts []rsgconflict.Conflict, want rsgconflict.ConflictCategory) rsgconflict.Conflict {
	t.Helper()
	for _, c := range conflicts {
		if c.Category == want {
			return c
		}
	}
	t.Fatalf("no %s conflict among %+v", want, conflicts)
	return rsgconflict.Conflict{}
}

// decide records one human decision for EVERY conflict of a change. One
// decision per conflict is what the classifier key demands: a decision covers
// exactly the conflict whose (code, fields, payload_keys) it names, so a
// change with two conflicts needs two decisions before it can merge.
func (f *mergeFixture) decide(t *testing.T, ctx context.Context, triple [3]string, targetKind domain.ConflictResolutionTargetKind, targetID string, conflicts []rsgconflict.Conflict, kind domain.ResolutionKind, note string) {
	t.Helper()
	decisions := make([]resolutions.Decision, 0, len(conflicts))
	for _, c := range conflicts {
		decisions = append(decisions, resolutions.Decision{
			TargetKind:  targetKind,
			TargetID:    targetID,
			Code:        c.Code,
			Fields:      c.Fields,
			PayloadKeys: c.PayloadKeys,
			Kind:        kind,
			Note:        note,
		})
	}
	if _, err := f.resolution.Save(ctx, f.alice, resolutions.SaveInput{
		ProjectID:     f.project.ID,
		BaseStateID:   triple[0],
		SourceStateID: triple[1],
		TargetStateID: triple[2],
		Decisions:     decisions,
	}); err != nil {
		t.Fatalf("save %s decision: %v", kind, err)
	}
}

// mergeCounts returns the row counts a merge must not disturb when it is
// refused: accepted states, state commits, merge records and object version
// rows.
func (f *mergeFixture) mergeCounts(t *testing.T, ctx context.Context) [4]int {
	t.Helper()
	var counts [4]int
	row := f.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM project_states),
		       (SELECT count(*) FROM state_commits),
		       (SELECT count(*) FROM semantic_merges),
		       (SELECT count(*) FROM scientific_object_versions)`)
	if err := row.Scan(&counts[0], &counts[1], &counts[2], &counts[3]); err != nil {
		t.Fatalf("count merge tables: %v", err)
	}
	return counts
}

// objectVersionStates lists the states the object's versions were created in,
// oldest first — the read that says whether a merge wrote a version of it.
func (f *mergeFixture) objectVersionStates(t *testing.T, ctx context.Context, objectID string) []string {
	t.Helper()
	rows, err := f.pool.Query(ctx, `
		SELECT state_id::text FROM scientific_object_versions
		WHERE object_id = $1 ORDER BY version_no`, objectID)
	if err != nil {
		t.Fatalf("list object version states: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var state string
		if err := rows.Scan(&state); err != nil {
			t.Fatalf("scan object version state: %v", err)
		}
		out = append(out, state)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read object version states: %v", err)
	}
	return out
}

// TestMergeAppliesSafeChangesDeterministically is acceptance criterion one
// over the real stack: a three-way merge with no conflicts lands, the accepted
// state carries the source-side changes, and the plan is reproducible — the
// stored digest is the sha256 of the canonical plan the same triple and the
// same decisions re-derive.
func TestMergeAppliesSafeChangesDeterministically(t *testing.T) {
	ctx := testCtx(t)
	f := newMergeFixture(t, ctx, domain.VisibilityPrivate, domain.BranchVisibilityPrivate)

	// main: the claim the feature will update, and a second claim main will
	// move on its own (the target-side movement a three-way merge sees).
	claim, claimV1 := f.createObject(t, ctx, f.main, "claim", mergeMainGateClaim("alpha"))
	side, _ := f.createObject(t, ctx, f.main, "claim", mergeMainGateClaim("side"))

	feature := f.fork(t, ctx, "feature", domain.BranchVisibilityPrivate)

	// feature: update the claim, and create a finding citing its v1 pin.
	f.updateObject(t, ctx, feature.ID, claim, 1, mergeMainGateClaim("beta"))
	finding, findingV1 := f.createObject(t, ctx, feature.ID, "finding",
		mergeMainGateFinding("found", claimV1))

	// The proposal pins the fork point; feature's work lands after it.
	pr := f.openPR(t, ctx, feature.ID, "feature work")
	pr = f.refresh(t, ctx, pr.Number)

	// main moves after the proposal exists: the target head the merge must
	// apply on top of is not the PR's base.
	f.updateObject(t, ctx, f.main, side, 1, mergeMainGateClaim("side-2"))
	targetHead := *f.head(t, ctx, f.main)

	// The plan is deterministic: two independent derivations of the same
	// triple and the same (empty) decision set are byte-identical.
	planA := f.plan(t, ctx, pr.BaseStateID, pr.ProposedStateID, targetHead,
		domain.BranchVisibilityPrivate, domain.BranchVisibilityPrivate)
	planB := f.plan(t, ctx, pr.BaseStateID, pr.ProposedStateID, targetHead,
		domain.BranchVisibilityPrivate, domain.BranchVisibilityPrivate)
	bytesA, err := planA.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonical plan: %v", err)
	}
	bytesB, err := planB.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonical plan: %v", err)
	}
	if string(bytesA) != string(bytesB) {
		t.Fatalf("two plans of the same triple drifted:\nfirst:  %s\nsecond: %s", bytesA, bytesB)
	}
	if !planA.Executable || len(planA.Materialized()) != 2 {
		t.Fatalf("plan = executable %v, materialized %d; want an executable plan writing the claim and the finding: %+v",
			planA.Executable, len(planA.Materialized()), planA.Changes)
	}
	if planA.Summary.Applied != 2 || planA.Summary.Withheld != 0 {
		t.Fatalf("summary = %+v, want two applied changes and nothing withheld", planA.Summary)
	}
	sum := sha256.Sum256(bytesA)
	wantDigest := hex.EncodeToString(sum[:])

	// The merge itself.
	res, err := f.merges.Merge(ctx, f.alice, merge.Input{ProjectID: f.project.ID, Number: pr.Number})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	// The executed plan is the one re-derived above. The digest is over the
	// canonical bytes and must match exactly; the column is JSONB, so the
	// stored JSON is compared structurally — Postgres normalizes whitespace
	// and key order on the way in, and that normalization is the only
	// difference. (The audit path is: recompute the plan from the stored
	// triple and the recorded decisions, hash it, compare with the stored
	// digest.)
	if res.Merge.PlanDigest != wantDigest {
		t.Fatalf("stored plan digest = %s, want %s (the re-derived plan of the same triple)", res.Merge.PlanDigest, wantDigest)
	}
	if diff := planJSONDifference(t, res.Merge.Plan, bytesA); diff != "" {
		t.Fatalf("stored plan differs from the re-derived plan: %s", diff)
	}

	// The merge advanced main and recorded the exact triple it applied on.
	stored, err := f.mergeStore.GetMergeByPullRequest(ctx, f.project.ID, pr.ID)
	if err != nil {
		t.Fatalf("read the merge record: %v", err)
	}
	if stored.ResultStateID != res.State.ID || stored.TargetStateID != targetHead {
		t.Fatalf("merge record = result %s target %s, want result %s target %s",
			stored.ResultStateID, stored.TargetStateID, res.State.ID, targetHead)
	}
	if stored.ActorID != f.alice.ID || stored.PlanVersion != rsgmerge.FormatV1 {
		t.Fatalf("merge record actor/version = %s/%s, want %s/%s",
			stored.ActorID, stored.PlanVersion, f.alice.ID, rsgmerge.FormatV1)
	}
	if stored.GitState != domain.GitStatePending {
		t.Fatalf("git saga state = %q, want pending (no provider merge adapter is wired: T0409)", stored.GitState)
	}
	if stored.GitAttempts != 1 || stored.GitError == "" {
		t.Fatalf("git saga = attempts %d error %q, want one recorded attempt naming the missing adapter",
			stored.GitAttempts, stored.GitError)
	}
	if !res.SourceBranchClosed {
		t.Fatalf("source branch was not closed: the merge accepted exactly its head")
	}

	// main's head is the accepted state, on main, built on the locked target
	// head.
	head, err := f.statesSvc.GetBranchHead(ctx, f.main)
	if err != nil {
		t.Fatalf("main head: %v", err)
	}
	if head.ID != res.State.ID || res.State.BranchID == nil || *res.State.BranchID != f.main {
		t.Fatalf("accepted state = %s on branch %v, want %s on main %s", res.State.ID, res.State.BranchID, res.State.ID, f.main)
	}
	if res.Commit.BranchID != f.main {
		t.Fatalf("state commit branch = %s, want main", res.Commit.BranchID)
	}

	// The merged content: the claim's v2 content (the source's wording) is an
	// APPENDED v3 in the accepted state, and the finding's content is an
	// appended v2 — the merge copies the source version verbatim, and it never
	// re-points the source's own rows at main (nothing disappears, state only
	// evolves). So the claim carries the fork's v1, the source's v2 and the
	// merge's v3; the finding carries the source's v1 and the merge's v2.
	wantClaim := f.objectVersionStates(t, ctx, claim)
	if len(wantClaim) != 3 {
		t.Fatalf("claim version states = %v, want the fork's v1, the source's v2 and the merge's v3", wantClaim)
	}
	if wantClaim[2] != res.State.ID {
		t.Fatalf("merged claim version lives in state %s, want the accepted state %s", wantClaim[2], res.State.ID)
	}
	findingStates := f.objectVersionStates(t, ctx, finding)
	if len(findingStates) != 2 || findingStates[1] != res.State.ID {
		t.Fatalf("finding version states = %v, want the source's v1 and the merge's v2 in the accepted state %s", findingStates, res.State.ID)
	}
	var payload []byte
	if err := f.pool.QueryRow(ctx, `
		SELECT payload FROM scientific_object_versions
		WHERE object_id = $1 AND state_id = $2`, claim, res.State.ID).Scan(&payload); err != nil {
		t.Fatalf("read the merged claim payload: %v", err)
	}
	var merged map[string]any
	if err := json.Unmarshal(payload, &merged); err != nil {
		t.Fatalf("decode the merged claim payload: %v", err)
	}
	if merged["statement"] != "beta" {
		t.Fatalf("merged claim statement = %v, want the source's beta (the merge copies the source version verbatim)", merged["statement"])
	}
	if findingV1 == "" {
		t.Fatalf("the finding's source version id is empty")
	}

	// The proposal and the research path it came from are closed: docs/43's
	// merge_ready → merged and active → merged.
	after, err := f.prs.Get(ctx, f.project.ID, pr.Number)
	if err != nil {
		t.Fatalf("read the merged PR: %v", err)
	}
	if after.State != domain.PullRequestStateMerged || after.MergedAt == nil {
		t.Fatalf("PR state = %s (merged_at %v), want merged with a timestamp", after.State, after.MergedAt)
	}
	source, err := f.branches.GetBranch(ctx, f.project.ID, feature.ID)
	if err != nil {
		t.Fatalf("read the source branch: %v", err)
	}
	if source.Lifecycle != domain.BranchLifecycleMerged {
		t.Fatalf("source branch lifecycle = %s, want merged", source.Lifecycle)
	}

	// A second merge of the same PR is refused: a PR merges once (its state
	// machine is already past merge_ready), so main cannot advance twice.
	if _, err := f.merges.Merge(ctx, f.alice, merge.Input{ProjectID: f.project.ID, Number: pr.Number}); err == nil {
		t.Fatalf("second merge of the same PR succeeded; a PR merges once")
	} else if !errors.Is(err, merge.ErrStore) {
		var notMergeable *merge.NotMergeableError
		if !errors.As(err, &notMergeable) {
			t.Fatalf("second merge error = %v, want *merge.NotMergeableError", err)
		}
	}
	after2, err := f.statesSvc.GetBranchHead(ctx, f.main)
	if err != nil || after2.ID != res.State.ID {
		t.Fatalf("main head after the refused second merge = %v (%v), want the accepted state %s", after2.ID, err, res.State.ID)
	}
}

// TestMergeRefusesScientificConflictWithoutDecision is acceptance criterion
// two over the real stack: both sides moving the same scientific content is a
// conflict the engine refuses to resolve by itself. docs/09 §7 forbids an
// automatic winner for a Protocol's conflicting numbers and for a scientific
// conclusion; this test walks both forbidden classes through the real
// three-way diff, and asserts the merge picks neither — without a human
// decision the merge is blocked, nothing is materialized, and the target
// branch does not move.
func TestMergeRefusesScientificConflictWithoutDecision(t *testing.T) {
	ctx := testCtx(t)
	f := newMergeFixture(t, ctx, domain.VisibilityPrivate, domain.BranchVisibilityPrivate)

	// The protocol's numbers are the "Protocol 冲突数值折中" case; the claim's
	// statement is the "科学结论" case. Both are forbidden automatic.
	proto, _ := f.createObject(t, ctx, f.main, "protocol", mergeProtocolPayload("300 K"))
	claim, _ := f.createObject(t, ctx, f.main, "claim", mergeMainGateClaim("alpha"))
	feature := f.fork(t, ctx, "feature", domain.BranchVisibilityPrivate)
	pr := f.openPR(t, ctx, feature.ID, "contested protocol and claim")

	// Both sides move the same content: the source first, then the target.
	f.updateObject(t, ctx, feature.ID, proto, 1, mergeProtocolPayload("350 K"))
	f.updateObject(t, ctx, feature.ID, claim, 1, mergeMainGateClaim("beta"))
	pr = f.refresh(t, ctx, pr.Number)
	f.updateObject(t, ctx, f.main, proto, 2, mergeProtocolPayload("400 K"))
	f.updateObject(t, ctx, f.main, claim, 2, mergeMainGateClaim("gamma"))
	targetHead := *f.head(t, ctx, f.main)

	triple := [3]string{pr.BaseStateID, pr.ProposedStateID, targetHead}
	// The taxonomy of docs/09 §6, read off the detector the merge reuses: the
	// diverging protocol parameters are a scientific conflict, the diverging
	// claim statement is a knowledge conflict (Claim assessment conflict), and
	// the statement's divergence shows up once more in the derived title as an
	// attribute conflict. The merge must not decide any of them.
	protoConflicts := f.conflictsOf(t, ctx, triple[0], triple[1], triple[2], proto)
	protoConflict := conflictWithCategory(t, protoConflicts, rsgconflict.CategoryScientific)
	if protoConflict.Code != rsgconflict.CodeScientificFieldDiverges {
		t.Fatalf("protocol conflict code = %q, want %q: %+v", protoConflict.Code, rsgconflict.CodeScientificFieldDiverges, protoConflict)
	}
	claimConflicts := f.conflictsOf(t, ctx, triple[0], triple[1], triple[2], claim)
	claimConflict := conflictWithCategory(t, claimConflicts, rsgconflict.CategoryKnowledge)
	if claimConflict.Code != rsgconflict.CodeKnowledgeFieldDiverges {
		t.Fatalf("claim conflict code = %q, want %q: %+v", claimConflict.Code, rsgconflict.CodeKnowledgeFieldDiverges, claimConflict)
	}

	plan := f.plan(t, ctx, triple[0], triple[1], triple[2],
		domain.BranchVisibilityPrivate, domain.BranchVisibilityPrivate)
	if plan.Executable {
		t.Fatalf("plan over undecided scientific conflicts is executable: %+v", plan.Summary)
	}
	if len(plan.Materialized()) != 0 {
		t.Fatalf("plan materializes %d change(s) although every change is conflicted", len(plan.Materialized()))
	}
	// Neither side's value won anywhere in the plan: the changes stay
	// blocked/undecided and the summary counts no applied change.
	for _, want := range []string{proto, claim} {
		seen := false
		for _, c := range plan.Changes {
			if c.TargetID != want {
				continue
			}
			seen = true
			if c.Effect != rsgmerge.EffectBlocked || c.Resolution != rsgmerge.ResolutionUndecided {
				t.Fatalf("conflicted change for %s = effect %q resolution %q, want blocked/undecided (never a winner)",
					want, c.Effect, c.Resolution)
			}
			if c.Materialize {
				t.Fatalf("change for %s materializes without a decision", want)
			}
		}
		if !seen {
			t.Fatalf("the plan has no change for the conflicted object %s: %+v", want, plan.Changes)
		}
	}
	if plan.Summary.Applied != 0 || plan.Summary.Carried != 0 {
		t.Fatalf("summary = %+v, want nothing applied and nothing carried: the target's values must not be overwritten by an undecided merge", plan.Summary)
	}

	before := f.mergeCounts(t, ctx)
	_, err := f.merges.Merge(ctx, f.alice, merge.Input{ProjectID: f.project.ID, Number: pr.Number})
	var blocked *merge.BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("merge over an undecided scientific conflict = %v, want *merge.BlockedError", err)
	}
	if blocked.Code() != merge.CodeMergeBlocked {
		t.Fatalf("blocked code = %s, want %s", blocked.Code(), merge.CodeMergeBlocked)
	}
	var undecided bool
	for _, b := range blocked.Plan.Blockers {
		if b.Code == rsgmerge.CodeConflictUndecided {
			undecided = true
		}
	}
	if !undecided {
		t.Fatalf("blockers = %+v, want %s among them", blocked.Plan.Blockers, rsgmerge.CodeConflictUndecided)
	}

	// A refused merge writes nothing at all: no accepted state, no merge
	// record, no version row — and main's head and the PR's state are where
	// they were.
	if after := f.mergeCounts(t, ctx); after != before {
		t.Fatalf("refused merge wrote rows: states/commits/merges/versions %v → %v", before, after)
	}
	if head, err := f.statesSvc.GetBranchHead(ctx, f.main); err != nil || head.ID != targetHead {
		t.Fatalf("main head after the refused merge = %v (%v), want %s", head.ID, err, targetHead)
	}
	after, err := f.prs.Get(ctx, f.project.ID, pr.Number)
	if err != nil {
		t.Fatalf("read the PR: %v", err)
	}
	if after.State != domain.PullRequestStateMergeReady {
		t.Fatalf("PR state after the refused merge = %s, want merge_ready (untouched)", after.State)
	}
	// No fourth version of either object exists: the refused merge wrote
	// nothing. And the newest version of each object is still the TARGET's —
	// main's 400 K, main's gamma — so the source's value did not win either.
	if got := f.objectVersionStates(t, ctx, proto); len(got) != 3 {
		t.Fatalf("protocol version states = %v, want exactly the fork, the source's v2 and main's v3", got)
	}
	if got := f.objectVersionStates(t, ctx, claim); len(got) != 3 {
		t.Fatalf("claim version states = %v, want exactly the fork, the source's v2 and main's v3", got)
	}
	if got := f.latestPayloadField(t, ctx, proto, "parameters", "temperature"); got != "400 K" {
		t.Fatalf("the newest protocol temperature = %v, want the target's 400 K (no source value, no average)", got)
	}
	if got := f.latestPayloadField(t, ctx, claim, "", "statement"); got != "gamma" {
		t.Fatalf("the newest claim statement = %v, want the target's gamma (no source value, no synthesis)", got)
	}
}

// planJSONDifference compares a stored plan against the engine's canonical
// bytes structurally, returning "" when they carry the same JSON value and a
// short report when they do not. It exists because the plan is stored in a
// JSONB column: Postgres normalizes whitespace and reorders object keys, so a
// byte comparison would fail on a stored plan that is in fact the same value.
func planJSONDifference(t *testing.T, stored []byte, canonical []byte) string {
	t.Helper()
	var want, got any
	if err := json.Unmarshal(canonical, &want); err != nil {
		t.Fatalf("decode the canonical plan: %v", err)
	}
	if err := json.Unmarshal(stored, &got); err != nil {
		t.Fatalf("decode the stored plan: %v", err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("re-marshal the canonical plan: %v", err)
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("re-marshal the stored plan: %v", err)
	}
	if string(wantJSON) == string(gotJSON) {
		return ""
	}
	return "\nstored:     " + string(gotJSON) + "\nre-derived: " + string(wantJSON)
}

// latestPayloadField reads one field of an object's newest version payload.
// outer names a nested object to descend into ("" for the top level).
func (f *mergeFixture) latestPayloadField(t *testing.T, ctx context.Context, objectID, outer, field string) any {
	t.Helper()
	var payload []byte
	err := f.pool.QueryRow(ctx, `
		SELECT payload FROM scientific_object_versions
		WHERE object_id = $1 ORDER BY version_no DESC LIMIT 1`, objectID).Scan(&payload)
	if err != nil {
		t.Fatalf("read the newest payload of %s: %v", objectID, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatalf("decode the newest payload of %s: %v", objectID, err)
	}
	if outer == "" {
		return doc[field]
	}
	nested, ok := doc[outer].(map[string]any)
	if !ok {
		t.Fatalf("payload of %s has no object at %q: %v", objectID, outer, doc)
	}
	return nested[field]
}

// TestMergeCarriesContestedConflictIntoAcceptedState is acceptance criterion
// three over the real stack: a conflict the human decided to keep open is
// CARRIED into main — the merge lands the proposal's other work, writes no
// winner for the conflicted object, and records the open disagreement (both
// versions, the deciding human) as a fact of the accepted state.
func TestMergeCarriesContestedConflictIntoAcceptedState(t *testing.T) {
	ctx := testCtx(t)
	f := newMergeFixture(t, ctx, domain.VisibilityPrivate, domain.BranchVisibilityPrivate)

	// The claim is the version the finding pins; it is not moved, so it is
	// not a change of the proposal at all.
	_, claimV1 := f.createObject(t, ctx, f.main, "claim", mergeMainGateClaim("alpha"))
	proto, _ := f.createObject(t, ctx, f.main, "protocol", mergeProtocolPayload("300 K"))
	feature := f.fork(t, ctx, "feature", domain.BranchVisibilityPrivate)
	pr := f.openPR(t, ctx, feature.ID, "contested protocol plus a finding")

	// The conflicted protocol: both sides moved it.
	f.updateObject(t, ctx, feature.ID, proto, 1, mergeProtocolPayload("350 K"))
	// …and a change that is safe to merge on its own, so the proposal has
	// something to accept besides the open conflict.
	f.createObject(t, ctx, feature.ID, "finding", mergeMainGateFinding("found", claimV1))
	pr = f.refresh(t, ctx, pr.Number)
	f.updateObject(t, ctx, f.main, proto, 2, mergeProtocolPayload("400 K"))
	targetHead := *f.head(t, ctx, f.main)

	triple := [3]string{pr.BaseStateID, pr.ProposedStateID, targetHead}
	conflicts := f.conflictsOf(t, ctx, triple[0], triple[1], triple[2], proto)
	conflict := conflictWithCategory(t, conflicts, rsgconflict.CategoryScientific)

	// The human keeps the conflict open instead of choosing a side: neither
	// 350 K nor 400 K wins, and no averaged 375 K is invented.
	f.decide(t, ctx, triple, domain.ConflictResolutionTargetObject, proto, conflicts,
		domain.ResolutionKeepBoth, "both readings stand until the follow-up experiment reports")

	plan := f.plan(t, ctx, triple[0], triple[1], triple[2],
		domain.BranchVisibilityPrivate, domain.BranchVisibilityPrivate)
	if !plan.Executable {
		t.Fatalf("plan with a keep-both decision is not executable: %+v", plan.Blockers)
	}
	if len(plan.Carried) != 1 || plan.Carried[0].Decision != domain.ResolutionKeepBoth {
		t.Fatalf("plan carried = %+v, want exactly the keep-both conflict", plan.Carried)
	}
	if plan.Summary.Applied != 1 || plan.Summary.Carried != 1 {
		t.Fatalf("summary = %+v, want one applied change and one carried conflict", plan.Summary)
	}

	res, err := f.merges.Merge(ctx, f.alice, merge.Input{ProjectID: f.project.ID, Number: pr.Number})
	if err != nil {
		t.Fatalf("merge with a keep-both decision: %v", err)
	}
	if res.Merge.Applied != 1 || res.Merge.Carried != 1 {
		t.Fatalf("merge record applied/carried = %d/%d, want 1/1", res.Merge.Applied, res.Merge.Carried)
	}

	// The carried conflict is a row of the accepted state, naming BOTH
	// versions and the human who kept them.
	carried, err := f.mergeStore.ListCarriedConflicts(ctx, res.Merge.ID)
	if err != nil {
		t.Fatalf("list carried conflicts: %v", err)
	}
	if len(carried) != 1 {
		t.Fatalf("carried conflicts = %+v, want exactly one", carried)
	}
	c := carried[0]
	if c.ResultStateID != res.State.ID || c.TargetID != proto {
		t.Fatalf("carried conflict = state %s target %s, want %s/%s", c.ResultStateID, c.TargetID, res.State.ID, proto)
	}
	if c.Kind != domain.ResolutionKeepBoth || c.DecidedBy != f.alice.ID {
		t.Fatalf("carried conflict kind/decider = %s/%s, want %s/%s", c.Kind, c.DecidedBy, domain.ResolutionKeepBoth, f.alice.ID)
	}
	if c.Category != string(conflict.Category) || c.Code != conflict.Code {
		t.Fatalf("carried conflict classifier key = %s/%s, want the detector's %s/%s",
			c.Category, c.Code, conflict.Category, conflict.Code)
	}
	if c.SourceVersionID == "" || c.TargetVersionID == nil || *c.TargetVersionID == "" {
		t.Fatalf("carried conflict names versions %q/%v; both sides must be named", c.SourceVersionID, c.TargetVersionID)
	}
	if *c.TargetVersionID == c.SourceVersionID {
		t.Fatalf("carried conflict names the same version on both sides (%s)", c.SourceVersionID)
	}

	// Main kept the contested object as it was: no version of the protocol was
	// written by the merge, so nothing was resolved into a winner, and the
	// target's own reading (400 K) is still the newest one.
	states := f.objectVersionStates(t, ctx, proto)
	if len(states) != 3 {
		t.Fatalf("protocol version states = %v, want the merge to have written no version of the disputed protocol", states)
	}
	for _, s := range states {
		if s == res.State.ID {
			t.Fatalf("the merge wrote a protocol version into the accepted state: the conflict was resolved, not carried")
		}
	}
	if got := f.latestPayloadField(t, ctx, proto, "parameters", "temperature"); got != "400 K" {
		t.Fatalf("the newest protocol temperature = %v, want the target's 400 K: keeping both sides means no winner and no average", got)
	}
	// The proposal's other work did land: the finding's source version is
	// followed by the version the merge appended into the accepted state.
	findingStates := f.objectVersionStates(t, ctx, f.objectIDOfFinding(t, ctx, res.State.ID))
	if len(findingStates) != 2 || findingStates[1] != res.State.ID {
		t.Fatalf("finding version states = %v, want the merge's version in the accepted state %s", findingStates, res.State.ID)
	}
	if head, err := f.statesSvc.GetBranchHead(ctx, f.main); err != nil || head.ID != res.State.ID {
		t.Fatalf("main head = %v (%v), want the accepted state %s", head.ID, err, res.State.ID)
	}
}

// objectIDOfFinding returns the id of the finding version the accepted state
// carries — the only object version in that state that is not the claim.
func (f *mergeFixture) objectIDOfFinding(t *testing.T, ctx context.Context, stateID string) string {
	t.Helper()
	var objectID string
	err := f.pool.QueryRow(ctx, `
		SELECT DISTINCT object_id::text FROM scientific_object_versions
		WHERE state_id = $1 AND schema_id LIKE '%finding%'`, stateID).Scan(&objectID)
	if err != nil {
		t.Fatalf("find the merged finding: %v", err)
	}
	return objectID
}

// TestMergeStoresEveryResolutionAction is acceptance criterion three's other
// half: every docs/09 §8 action the human can take is expressible — the store
// takes it (the migration widens the CHECK to the full vocabulary) and the
// engine has a defined effect for it — including the two that keep the
// conflict unresolved in main.
func TestMergeStoresEveryResolutionAction(t *testing.T) {
	ctx := testCtx(t)
	f := newMergeFixture(t, ctx, domain.VisibilityPrivate, domain.BranchVisibilityPrivate)

	// The claim is the version the finding pins; it is not moved, so it is
	// not a change of the proposal at all.
	_, claimV1 := f.createObject(t, ctx, f.main, "claim", mergeMainGateClaim("alpha"))
	proto, _ := f.createObject(t, ctx, f.main, "protocol", mergeProtocolPayload("300 K"))
	feature := f.fork(t, ctx, "feature", domain.BranchVisibilityPrivate)
	pr := f.openPR(t, ctx, feature.ID, "every action")

	f.updateObject(t, ctx, feature.ID, proto, 1, mergeProtocolPayload("350 K"))
	f.createObject(t, ctx, feature.ID, "finding", mergeMainGateFinding("found", claimV1))
	pr = f.refresh(t, ctx, pr.Number)
	f.updateObject(t, ctx, f.main, proto, 2, mergeProtocolPayload("400 K"))
	targetHead := *f.head(t, ctx, f.main)

	triple := [3]string{pr.BaseStateID, pr.ProposedStateID, targetHead}
	conflicts := f.conflictsOf(t, ctx, triple[0], triple[1], triple[2], proto)
	if len(conflicts) != 1 {
		t.Fatalf("protocol change carries %d conflicts (%+v), want the single scientific one", len(conflicts), conflicts)
	}

	cases := []struct {
		kind domain.ResolutionKind
		// wantEffect is the effect the engine plans for the conflicted
		// change; wantBlocker the blocker code when the action stops the
		// merge for now.
		wantEffect  rsgmerge.Effect
		wantBlocker string
	}{
		{domain.ResolutionAcceptSource, rsgmerge.EffectApply, ""},
		{domain.ResolutionAcceptTarget, rsgmerge.EffectKeepTarget, ""},
		{domain.ResolutionKeepBoth, rsgmerge.EffectCarryBoth, ""},
		{domain.ResolutionExplicitCoexistence, rsgmerge.EffectCarryBoth, ""},
		{domain.ResolutionUnresolved, rsgmerge.EffectCarryBoth, ""},
		{domain.ResolutionAbortChange, rsgmerge.EffectAbortProposed, ""},
		{domain.ResolutionValidationBranch, rsgmerge.EffectBlocked, rsgmerge.CodeValidationBranch},
		{domain.ResolutionRequestEvidence, rsgmerge.EffectBlocked, rsgmerge.CodeMoreEvidence},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			// Storing the decision is the first half: before the migration
			// widened the CHECK, four of these seven actions had no row they
			// could be written to.
			f.decide(t, ctx, triple, domain.ConflictResolutionTargetObject, proto, conflicts, tc.kind, "decided by a human")
			recorded, err := f.resolution.Plan(ctx, f.project.ID, triple[0], triple[1], triple[2])
			if err != nil {
				t.Fatalf("read back the recorded plan: %v", err)
			}
			if len(recorded) != 1 || recorded[0].Kind != tc.kind {
				t.Fatalf("recorded plan = %+v, want the %s decision", recorded, tc.kind)
			}
			if recorded[0].DecidedBy != f.alice.ID {
				t.Fatalf("recorded decider = %s, want the human %s (an agent never decides a scientific conflict)",
					recorded[0].DecidedBy, f.alice.ID)
			}

			// The second half: the engine has a defined effect for it, and
			// the effect is never "the merge invented an outcome".
			plan := f.plan(t, ctx, triple[0], triple[1], triple[2],
				domain.BranchVisibilityPrivate, domain.BranchVisibilityPrivate)
			var got *rsgmerge.Change
			for i := range plan.Changes {
				if plan.Changes[i].TargetID == proto {
					got = &plan.Changes[i]
				}
			}
			if got == nil {
				t.Fatalf("the plan has no change for the conflicted protocol")
			}
			if got.Effect != tc.wantEffect {
				t.Fatalf("change effect = %q, want %q (%s)", got.Effect, tc.wantEffect, tc.kind)
			}
			if tc.wantBlocker != "" {
				if !got.Blocked || got.Block != tc.wantBlocker {
					t.Fatalf("change blocked/block = %v/%q, want true/%s", got.Blocked, got.Block, tc.wantBlocker)
				}
				if plan.Executable {
					t.Fatalf("plan is executable although %s asks for %s", tc.kind, tc.wantBlocker)
				}
				return
			}
			if got.Blocked {
				t.Fatalf("change blocked = %q, want the %s effect to run", got.Block, tc.kind)
			}
			if tc.wantEffect == rsgmerge.EffectCarryBoth {
				if len(plan.Carried) != 1 || plan.Carried[0].Decision != tc.kind {
					t.Fatalf("carried = %+v, want one %s conflict kept open", plan.Carried, tc.kind)
				}
				if got.Materialize {
					t.Fatalf("%s materialized a version; keeping both sides means writing neither winner", tc.kind)
				}
			}
			// Whatever the action, the proposal's other change still lands:
			// the human's decision is about the conflict, not about the merge.
			if !plan.Executable {
				t.Fatalf("plan is not executable although the %s decision resolves every conflict: %+v", tc.kind, plan.Blockers)
			}
		})
	}
}

// TestMergeWithholdsPrivateSourceFromPublicTarget is acceptance criterion
// four's second half over the real stack: a private source branch's changes
// are NOT published by the merge into a public target. docs/09 §9 defers the
// private → public step to the independent Publication Gate (T0704/T0705), so
// the merge withholds the changes, refuses to run, and writes nothing.
func TestMergeWithholdsPrivateSourceFromPublicTarget(t *testing.T) {
	ctx := testCtx(t)
	// The target is public and the source private: the pair the publication
	// rule reads. Branch visibility is the branch's own (docs/09 §9), so a
	// public main beside a private research path is the case under test —
	// inside a public project, because a public branch only exists in one
	// (docs/12 §3).
	f := newMergeFixture(t, ctx, domain.VisibilityPublic, domain.BranchVisibilityPublic)

	f.createObject(t, ctx, f.main, "claim", mergeMainGateClaim("alpha"))
	feature := f.fork(t, ctx, "feature", domain.BranchVisibilityPrivate)
	pr := f.openPR(t, ctx, feature.ID, "private work heading for a public target")

	f.createObject(t, ctx, feature.ID, "claim", mergeMainGateClaim("private work"))
	pr = f.refresh(t, ctx, pr.Number)
	targetHead := *f.head(t, ctx, f.main)

	plan := f.plan(t, ctx, pr.BaseStateID, pr.ProposedStateID, targetHead,
		domain.BranchVisibilityPrivate, domain.BranchVisibilityPublic)
	if plan.Executable {
		t.Fatalf("plan publishes a private source's change into a public target: %+v", plan.Summary)
	}
	if len(plan.Withheld) != 1 || plan.Withheld[0].Reason != rsgmerge.ReasonPublicationGate {
		t.Fatalf("withheld = %+v, want the change withheld for %s", plan.Withheld, rsgmerge.ReasonPublicationGate)
	}
	if len(plan.Materialized()) != 0 {
		t.Fatalf("plan materializes %d change(s) although every change is withheld", len(plan.Materialized()))
	}

	before := f.mergeCounts(t, ctx)
	_, err := f.merges.Merge(ctx, f.alice, merge.Input{ProjectID: f.project.ID, Number: pr.Number})
	var blocked *merge.BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("private → public merge = %v, want *merge.BlockedError", err)
	}
	if after := f.mergeCounts(t, ctx); after != before {
		t.Fatalf("the withheld merge wrote rows: states/commits/merges/versions %v → %v", before, after)
	}
	publicState, err := f.statesSvc.GetBranchHead(ctx, f.main)
	if err != nil || publicState.ID != targetHead {
		t.Fatalf("public target head = %v (%v), want the untouched %s", publicState.ID, err, targetHead)
	}
	// Nothing of the private branch's content reached the public target's
	// state: the merge did not publish it, and it did not pretend to merge it.
	states := f.objectVersionStates(t, ctx, f.publicClaimID(t, ctx, feature.ID))
	for _, s := range states {
		if s == publicState.ID {
			t.Fatalf("the private branch's claim is a member of the public target's state %s", publicState.ID)
		}
	}
	after, err := f.prs.Get(ctx, f.project.ID, pr.Number)
	if err != nil {
		t.Fatalf("read the PR: %v", err)
	}
	if after.State != domain.PullRequestStateMergeReady {
		t.Fatalf("PR state after the withheld merge = %s, want merge_ready", after.State)
	}
}

// publicClaimID returns the id of the object the branch's head state created.
func (f *mergeFixture) publicClaimID(t *testing.T, ctx context.Context, branchID string) string {
	t.Helper()
	head, err := f.statesSvc.GetBranchHead(ctx, branchID)
	if err != nil {
		t.Fatalf("read the source head: %v", err)
	}
	var objectID string
	if err := f.pool.QueryRow(ctx, `
		SELECT object_id::text FROM scientific_object_versions
		WHERE state_id = $1`, head.ID).Scan(&objectID); err != nil {
		t.Fatalf("find the source-side object: %v", err)
	}
	return objectID
}

// TestMergeAppendsRelationVersionOnTheRealStack is the merge's RELATION half
// on a real PostgreSQL. The object half is covered above; the relation half
// writes through AppendRelationVersionInTx, whose only real caller is the
// merge itself — so without this test the whole relation write path was
// exercised against a fake and by nothing else. Here the proposal carries a
// relation change through the real plan and the real transaction, and the
// assertion is on the rows: the version is APPENDED (v2 in the accepted
// state), the container's counter advances, and the source's own v1 is left
// where it was (nothing disappears; state only evolves).
func TestMergeAppendsRelationVersionOnTheRealStack(t *testing.T) {
	ctx := testCtx(t)
	f := newMergeFixture(t, ctx, domain.VisibilityPrivate, domain.BranchVisibilityPrivate)

	_, claimV1 := f.createObject(t, ctx, f.main, "claim", mergeMainGateClaim("alpha"))
	_, findingV1 := f.createObject(t, ctx, f.main, "finding", mergeMainGateFinding("found", claimV1))
	// A relation main already has, which the proposal does not touch: it must
	// not move, and it is what makes "the appended version" unambiguous.
	inPlace := f.createRelation(t, ctx, f.main, "supports", findingV1, claimV1)

	feature := f.fork(t, ctx, "feature", domain.BranchVisibilityPrivate)
	pr := f.openPR(t, ctx, feature.ID, "an edge main does not have yet")

	sourceRel := f.createRelation(t, ctx, feature.ID, "supports", findingV1, claimV1)
	pr = f.refresh(t, ctx, pr.Number)
	targetHead := *f.head(t, ctx, f.main)

	// The plan the merge will execute carries the relation change and
	// materializes it — otherwise the assertions below would be about a merge
	// that never intended to write the relation.
	plan := f.plan(t, ctx, pr.BaseStateID, pr.ProposedStateID, targetHead,
		domain.BranchVisibilityPrivate, domain.BranchVisibilityPrivate)
	var planned *rsgmerge.Change
	for i := range plan.Changes {
		if plan.Changes[i].TargetKind == domain.ConflictResolutionTargetRelation && plan.Changes[i].TargetID == sourceRel {
			planned = &plan.Changes[i]
		}
	}
	if planned == nil {
		t.Fatalf("the plan has no change for the proposal's relation %s: %+v", sourceRel, plan.Changes)
	}
	if !planned.Materialize {
		t.Fatalf("relation change = %+v, want it materialized (a non-conflicting edge the proposal adds)", planned)
	}
	if planned.Effect != rsgmerge.EffectApply {
		t.Fatalf("relation change effect = %q, want %q", planned.Effect, rsgmerge.EffectApply)
	}

	before := f.relationHead(t, ctx, sourceRel)
	if before != 1 {
		t.Fatalf("source relation version before the merge = %d, want 1 (created on the branch)", before)
	}

	res, err := f.merges.Merge(ctx, f.alice, merge.Input{ProjectID: f.project.ID, Number: pr.Number})
	if err != nil {
		t.Fatalf("merge carrying a relation change: %v", err)
	}

	// The row the merge appended: v2 of the same relation, in the accepted
	// state, carrying the proposal's type and pins.
	row := f.relationVersionRow(t, ctx, sourceRel, 2)
	if row.StateID != res.State.ID {
		t.Fatalf("appended relation version lives in state %s, want the accepted state %s", row.StateID, res.State.ID)
	}
	if row.RelationType != "supports" {
		t.Fatalf("appended relation version type = %q, want the proposal's supports", row.RelationType)
	}
	if row.SourceObjectVersionID != findingV1 || row.TargetObjectVersionID != claimV1 {
		t.Fatalf("appended relation version pins = %s/%s, want the proposal's %s/%s",
			row.SourceObjectVersionID, row.TargetObjectVersionID, findingV1, claimV1)
	}
	// The counter advanced, and the log is the whole story: v1 stays on the
	// branch that proposed it, v2 is the accepted state's.
	if after := f.relationHead(t, ctx, sourceRel); after != 2 {
		t.Fatalf("source relation head after the merge = %d, want 2", after)
	}
	if got := f.relationVersionStates(t, ctx, sourceRel); len(got) != 2 || got[0] == res.State.ID || got[1] != res.State.ID {
		t.Fatalf("relation version states = %v, want the proposal's v1 then the accepted state %s", got, res.State.ID)
	}
	// The relation main already had is untouched: a merge writes what the
	// proposal changed and nothing else.
	if got := f.relationHead(t, ctx, inPlace); got != 1 {
		t.Fatalf("untouched relation head = %d, want it left at 1", got)
	}
	if got := f.relationVersionStates(t, ctx, inPlace); len(got) != 1 {
		t.Fatalf("untouched relation version states = %v, want exactly its single version", got)
	}
	// The merge counted it: the record's applied count is the number of
	// changes the plan said it would materialize — here exactly one, the edge
	// (the objects the proposal's edge joins were created on main, not by this
	// proposal).
	if res.Merge.Applied != len(plan.Materialized()) {
		t.Fatalf("merge record applied = %d, want the %d change(s) the plan materialized",
			res.Merge.Applied, len(plan.Materialized()))
	}
	if res.Merge.Applied != 1 {
		t.Fatalf("merge record applied = %d, want exactly the one relation change", res.Merge.Applied)
	}
}

// relationVersionRow is one relation version as stored.
type relationVersionRow struct {
	StateID               string
	RelationType          string
	SourceObjectVersionID string
	TargetObjectVersionID string
}

// relationHead reads a relation's current version counter.
func (f *mergeFixture) relationHead(t *testing.T, ctx context.Context, relationID string) int {
	t.Helper()
	var head int
	if err := f.pool.QueryRow(ctx, `
		SELECT current_version_no FROM relations WHERE id = $1`, relationID).Scan(&head); err != nil {
		t.Fatalf("read relation head: %v", err)
	}
	return head
}

// relationVersionStates lists the state ids of a relation's versions in order.
func (f *mergeFixture) relationVersionStates(t *testing.T, ctx context.Context, relationID string) []string {
	t.Helper()
	rows, err := f.pool.Query(ctx, `
		SELECT state_id::text FROM relation_versions
		WHERE relation_id = $1 ORDER BY version_no`, relationID)
	if err != nil {
		t.Fatalf("list relation version states: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var state string
		if err := rows.Scan(&state); err != nil {
			t.Fatalf("scan relation version state: %v", err)
		}
		out = append(out, state)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read relation version states: %v", err)
	}
	return out
}

// relationVersionRow reads one stored relation version.
func (f *mergeFixture) relationVersionRow(t *testing.T, ctx context.Context, relationID string, versionNo int) relationVersionRow {
	t.Helper()
	var row relationVersionRow
	if err := f.pool.QueryRow(ctx, `
		SELECT state_id::text, relation_type, source_object_version_id::text, target_object_version_id::text
		FROM relation_versions WHERE relation_id = $1 AND version_no = $2`,
		relationID, versionNo).Scan(&row.StateID, &row.RelationType, &row.SourceObjectVersionID, &row.TargetObjectVersionID); err != nil {
		t.Fatalf("read relation version %d: %v", versionNo, err)
	}
	return row
}

// TestAppendRelationVersionRefusesAStaleExpectation drives the relation write
// the merge uses straight at the store, on a real transaction, with the two
// expectations a concurrent merge makes possible. A fresh expectation appends
// (the control: the call itself works), a stale one reports the CAS conflict
// with the actual head — the outcome the merge's optimistic retry recognizes
// — and an unknown relation is still reported as missing rather than as a
// version conflict (the read after zero rows must distinguish the two).
func TestAppendRelationVersionRefusesAStaleExpectation(t *testing.T) {
	ctx := testCtx(t)
	f := newMergeFixture(t, ctx, domain.VisibilityPrivate, domain.BranchVisibilityPrivate)

	_, claimV1 := f.createObject(t, ctx, f.main, "claim", mergeMainGateClaim("alpha"))
	_, findingV1 := f.createObject(t, ctx, f.main, "finding", mergeMainGateFinding("found", claimV1))
	rel := f.createRelation(t, ctx, f.main, "supports", findingV1, claimV1)
	head := *f.head(t, ctx, f.main)

	store := persistence.NewRelationStore(f.pool)
	params := relations.VersionParams{
		StateID:               head,
		RelationType:          "supports",
		SourceObjectVersionID: findingV1,
		TargetObjectVersionID: claimV1,
		Payload:               json.RawMessage(`{}`),
		// The merge writes the version as the actor it runs for; the store
		// requires it (a relation version without an author would not be a
		// record of who moved the edge).
		CreatedBy: f.alice.ID,
	}

	// Each attempt runs in its own transaction and is rolled back: the
	// rollback is registered with the test, not left to the happy path, so a
	// failing assertion cannot strand an open transaction and wedge the pool
	// teardown (which is exactly what it did the first time this was written).
	begin := func() pgx.Tx {
		t.Helper()
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		return tx
	}

	// 1. The fresh expectation (the head as read) appends version 2. This is
	// the merge's own call shape, and it proves the two refusals below are
	// about the expectation, not about the statement.
	tx := begin()
	v, err := store.AppendRelationVersionInTx(ctx, tx, rel, 1, params)
	if err != nil {
		t.Fatalf("append with a fresh expectation: %v", err)
	}
	if v.VersionNo != 2 {
		t.Fatalf("appended version no = %d, want 2", v.VersionNo)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// 2. A stale expectation — what a concurrent merge that advanced the edge
	// first would leave behind — is a version conflict naming the real head,
	// not a store failure and not a silent second write.
	tx = begin()
	_, err = store.AppendRelationVersionInTx(ctx, tx, rel, 0, params)
	var conflict *relations.VersionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("append with a stale expectation = %v, want *relations.VersionConflictError", err)
	}
	if conflict.Expected != 0 || conflict.Actual != 1 {
		t.Fatalf("conflict = expected %d actual %d, want 0/1 (the head the merge would re-read)",
			conflict.Expected, conflict.Actual)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// 3. The same zero-row CAS for a relation that does not exist reports the
	// missing container, so a caller can tell "deleted" from "moved".
	tx = begin()
	_, err = store.AppendRelationVersionInTx(ctx, tx, "00000000-0000-0000-0000-000000000000", 0, params)
	if !errors.Is(err, relations.ErrRelationNotFound) {
		t.Fatalf("append to an unknown relation = %v, want %v", err, relations.ErrRelationNotFound)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
}

// TestSemanticMergeRecordCountsAreImmutable is the schema half: the guard's
// comment promises that the database truth (states, plan, counts, actor) is
// fixed once written and only the Git saga columns move — so a merge record
// can never drift away from the state it claims to have produced. Each case
// below changes exactly one of those columns and must be refused; delete any
// one of them from the guard's comparison and its case starts passing, which
// is what makes this test evidence rather than decoration. The saga update
// the platform really makes is asserted to still work, so the guard is not
// simply refusing every update.
func TestSemanticMergeRecordCountsAreImmutable(t *testing.T) {
	ctx := testCtx(t)
	f := newMergeFixture(t, ctx, domain.VisibilityPrivate, domain.BranchVisibilityPrivate)

	claim, _ := f.createObject(t, ctx, f.main, "claim", mergeMainGateClaim("alpha"))
	feature := f.fork(t, ctx, "feature", domain.BranchVisibilityPrivate)
	pr := f.openPR(t, ctx, feature.ID, "counts stay put")
	f.updateObject(t, ctx, feature.ID, claim, 1, mergeMainGateClaim("beta"))
	pr = f.refresh(t, ctx, pr.Number)

	res, err := f.merges.Merge(ctx, f.alice, merge.Input{ProjectID: f.project.ID, Number: pr.Number})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	mergeID := res.Merge.ID

	// What the merge recorded, read back: the reference values the refused
	// updates must leave untouched.
	before := f.mergeCountRow(t, ctx, mergeID)
	if before.Applied != 1 {
		t.Fatalf("recorded applied_count = %d, want the one change the merge applied", before.Applied)
	}

	// The saga surface still moves: recording a failed Git step is what the
	// platform does after the merge transaction commits.
	failed := domain.GitStateFailed
	if _, err := f.mergeStore.RecordGitAttempt(ctx, merge.GitStepParams{
		MergeID: mergeID,
		State:   failed,
		Error:   "provider refused the merge",
	}); err != nil {
		t.Fatalf("record a failed Git attempt: %v", err)
	}
	saga := f.mergeCountRow(t, ctx, mergeID)
	if saga.GitState != string(failed) || saga.GitAttempts != before.GitAttempts+1 {
		t.Fatalf("saga after the recorded attempt = %s/%d, want failed/%d",
			saga.GitState, saga.GitAttempts, before.GitAttempts+1)
	}

	// Every database-truth column the guard lists is refused, one statement
	// each. The expressions genuinely change the value, so a no-op update
	// cannot make a case pass by accident.
	cases := []struct {
		column string
		set    string
	}{
		{"applied_count", "applied_count + 1"},
		{"kept_target_count", "kept_target_count + 1"},
		{"carried_count", "carried_count + 1"},
		{"aborted_count", "aborted_count + 1"},
		{"withheld_count", "withheld_count + 1"},
		{"created_at", "created_at + interval '1 second'"},
		{"plan", `'{"tampered":true}'::jsonb`},
		{"plan_digest", "'tampered'"},
		{"actor_id", "(SELECT id FROM users WHERE id <> actor_id LIMIT 1)"},
	}
	for _, tc := range cases {
		t.Run(tc.column, func(t *testing.T) {
			_, err := f.pool.Exec(ctx, "UPDATE semantic_merges SET "+tc.column+" = "+tc.set+" WHERE id = $1", mergeID)
			isPgErr(t, err, "P0001")
		})
	}

	// After nine refusals the row still says what the merge said.
	after := f.mergeCountRow(t, ctx, mergeID)
	if after != saga {
		t.Fatalf("merge row after the refused updates = %+v, want the recorded %+v", after, saga)
	}
}

// mergeCountRow is the merge record's database truth as stored, plus the saga
// fields the platform is allowed to move.
type mergeCountRow struct {
	Applied     int
	KeptTarget  int
	Carried     int
	Aborted     int
	Withheld    int
	PlanDigest  string
	ActorID     string
	CreatedAt   string
	GitState    string
	GitError    string
	GitAttempts int
}

// mergeCountRow reads one merge record's columns.
func (f *mergeFixture) mergeCountRow(t *testing.T, ctx context.Context, mergeID string) mergeCountRow {
	t.Helper()
	var row mergeCountRow
	if err := f.pool.QueryRow(ctx, `
		SELECT applied_count, kept_target_count, carried_count, aborted_count, withheld_count,
		       plan_digest, actor_id::text, created_at::text, git_state, git_error, git_attempts
		FROM semantic_merges WHERE id = $1`, mergeID).Scan(
		&row.Applied, &row.KeptTarget, &row.Carried, &row.Aborted, &row.Withheld,
		&row.PlanDigest, &row.ActorID, &row.CreatedAt, &row.GitState, &row.GitError, &row.GitAttempts); err != nil {
		t.Fatalf("read the merge record: %v", err)
	}
	return row
}
