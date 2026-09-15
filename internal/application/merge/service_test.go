package merge

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lichman0405/post/internal/application/diffs"
	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/relations"
	"github.com/lichman0405/post/internal/application/sciobjects"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/rsg/diff"
	"github.com/lichman0405/post/internal/rsg/integrity"
	"github.com/lichman0405/post/internal/rsg/manifest"
	rsgmerge "github.com/lichman0405/post/internal/rsg/merge"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// These unit tests cover what an integration test cannot stage on demand: the
// ORDER of the service's steps (a refusal that must precede every read), the
// retry policy (a moved version counter is retried, a moved state is not), and
// the fence — a commit whose locked target head is not the state being created
// must fail with *StaleMergeError and write nothing. The integration suite
// (tests/integration/merge_test.go) runs the same code against a real
// PostgreSQL; this file pins the branches a real database will not produce at
// will.

var (
	fixtureClock = time.Date(2026, 2, 10, 9, 30, 0, 0, time.UTC)

	projID       = "11111111-1111-4111-8111-111111111111"
	mainBranchID = "22222222-2222-4222-8222-222222222222"
	featBranchID = "33333333-3333-4333-8333-333333333333"
	claimID      = "44444444-4444-4444-8444-444444444444"
	claimV1      = "44444444-0001-4001-8001-000000000001"
	claimV2      = "44444444-0002-4002-8002-000000000002"
	claimV3      = "44444444-0003-4003-8003-000000000003"
	safeObjectID = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	baseStateID  = "55555555-5555-4555-8555-555555555555"
	srcStateID   = "66666666-6666-4666-8666-666666666666"
	tgtStateID   = "77777777-7777-4777-8777-777777777777"
	otherStateID = "ffffffff-0000-4000-8000-000000000000"
	resultState  = "88888888-8888-4888-8888-888888888888"
	actorID      = "99999999-9999-4999-8999-999999999999"
	prID         = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	mergeID      = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	mainSHA      = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	mergeSHA     = "aabbccddeeff00112233445566778899aabbccdd"
	mergeService = "post-merge-service"
)

func strptr(s string) *string { return &s }

// objRow builds one manifest object version, the way persistence hands it to
// the engine.
func objRow(id, objectID string, versionNo int, stateID, title, statement string) manifest.ObjectVersion {
	return manifest.ObjectVersion{
		ID:             id,
		ObjectID:       objectID,
		ObjectType:     "claim",
		VersionNo:      versionNo,
		StateID:        stateID,
		SchemaRef:      manifest.SchemaRef{ID: "https://open-rd.example/schemas/claim.schema.json", Version: "1"},
		Title:          title,
		LifecycleState: "active",
		Payload:        json.RawMessage(`{"statement":"` + statement + `","claim_type":"descriptive"}`),
		IntegrityHash:  "sha256:fixture",
		CreatedBy:      actorID,
		CreatedAt:      fixtureClock,
	}
}

// safeInputs is a triple whose one source-side change is a NEW object: nothing
// contests it, so a merge over this triple executes with no human decision at
// all. Every test starts from a merge that would otherwise succeed.
func safeInputs() diff.Inputs {
	return diff.Inputs{
		ProjectID: projID,
		Base:      diff.StateRef{ID: baseStateID},
		Source:    diff.StateRef{ID: srcStateID, GitRef: strptr(mainSHA)},
		Target:    diff.StateRef{ID: tgtStateID, GitRef: strptr(mainSHA)},
		SourceSnapshot: manifest.Snapshot{ObjectVersions: []manifest.ObjectVersion{
			objRow("ffffffff-ffff-4fff-8fff-ffffffffffff", safeObjectID, 1, srcStateID, "new finding", "found"),
		}},
	}
}

// conflictedInputs adds the scientific conflict: one claim whose statement both
// sides moved. That change is the only thing blocking, so this triple's plan is
// executable exactly when a human has decided the claim's conflicts.
func conflictedInputs() diff.Inputs {
	in := safeInputs()
	in.SourceSnapshot.ObjectVersions = append(in.SourceSnapshot.ObjectVersions, claimRow(claimV2, 2, srcStateID, "beta"))
	in.BaseSnapshot = manifest.Snapshot{ObjectVersions: []manifest.ObjectVersion{claimRow(claimV1, 1, baseStateID, "alpha")}}
	in.TargetSnapshot = manifest.Snapshot{ObjectVersions: []manifest.ObjectVersion{claimRow(claimV3, 3, tgtStateID, "gamma")}}
	return in
}

// claimOnlyInputs is the same contested claim with nothing else in the triple:
// once the human carries the conflict, nothing would land at all.
func claimOnlyInputs() diff.Inputs {
	in := conflictedInputs()
	in.SourceSnapshot.ObjectVersions = []manifest.ObjectVersion{claimRow(claimV2, 2, srcStateID, "beta")}
	return in
}

// claimRow builds one version of the contested claim (the title and the payload
// both move with the statement, which is why this change carries two conflicts).
func claimRow(id string, versionNo int, stateID, statement string) manifest.ObjectVersion {
	return objRow(id, claimID, versionNo, stateID, statement, statement)
}

// fakeTx is a states.Transaction stub: the fakes issue no SQL on it.
type fakeTx struct{}

func (fakeTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (fakeTx) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, nil }
func (fakeTx) QueryRow(context.Context, string, ...any) pgx.Row        { return nil }

// fakeDiffs serves the engine inputs the test composed.
type fakeDiffs struct {
	inputs diff.Inputs
	err    error
	calls  int
}

func (f *fakeDiffs) Inputs(context.Context, diffs.Params) (diff.Inputs, error) {
	f.calls++
	if f.err != nil {
		return diff.Inputs{}, f.err
	}
	return f.inputs, nil
}

// fakePlans serves the recorded human decisions.
type fakePlans struct {
	decisions []domain.ConflictResolution
	calls     int
}

func (f *fakePlans) Plan(context.Context, string, string, string, string) ([]domain.ConflictResolution, error) {
	f.calls++
	return f.decisions, nil
}

// fakeCommits runs the write callback the way persistence.StateStore does —
// result state row, head CAS, then the callback with the new state id — and
// counts the attempts.
type fakeCommits struct {
	attempts int
	failErr  error
	// gate records the gate ladder the merge asked the commit to enforce.
	gate rsgvalidation.Gate
}

func (f *fakeCommits) Commit(ctx context.Context, in states.CommitParams, write states.WriteFunc) (domain.ProjectState, domain.StateCommit, error) {
	f.attempts++
	f.gate = in.Gate
	if f.failErr != nil {
		return domain.ProjectState{}, domain.StateCommit{}, f.failErr
	}
	if err := write(ctx, fakeTx{}, resultState); err != nil {
		return domain.ProjectState{}, domain.StateCommit{}, err
	}
	return domain.ProjectState{ID: resultState, ProjectID: in.ProjectID, BranchID: strptr(in.BranchID)},
		domain.StateCommit{ProjectID: in.ProjectID, BranchID: in.BranchID, ResultStateID: resultState}, nil
}

// fakeStore is the persistence surface, with the call counts a refusal must
// leave at zero.
type fakeStore struct {
	pr     domain.PullRequest
	source domain.Branch
	target domain.Branch

	// heads is the optimistic prediction read; lockedHeads what the write
	// transaction sees under its row locks. lockedHeadsSeq, when set, is served
	// one entry per locked read (its last entry repeats), which is how a
	// counter that moved once and then settled is staged.
	heads          VersionHeads
	lockedHeads    VersionHeads
	lockedHeadsSeq []VersionHeads
	lockedHeadsN   int

	lockedScope    MergeScope
	lockedScopeErr error

	getPRCalls      int
	lockScopeCalls  int
	lockHeadsCalls  int
	writeMergeCalls int
	markPRCalls     int
	markBranchCalls int
	ledgerReads     int
	ledgerKeys      []string
	// ledger is the merge a previous call with the same Idempotency-Key
	// stored; nil means the key is unused.
	ledger   *domain.SemanticMerge
	gitSteps []GitStepParams
	mergeRow domain.SemanticMerge
	// mergeParams records what the merge asked the store to write, which is
	// where the carried conflicts and the plan digest live (the stored row
	// itself has no carried-conflict column: they are their own table).
	mergeParams []WriteMergeParams
}

func newFakeStore() *fakeStore {
	heads := VersionHeads{Objects: map[string]int{safeObjectID: 1}, Relations: map[string]int{}}
	s := &fakeStore{
		pr: domain.PullRequest{
			ID: prID, ProjectID: projID, Number: 7,
			SourceBranchID: featBranchID, TargetBranchID: mainBranchID,
			BaseStateID: baseStateID, ProposedStateID: srcStateID,
			State: domain.PullRequestStateMergeReady,
		},
		source: domain.Branch{ID: featBranchID, ProjectID: projID, Name: "feature",
			Visibility: domain.BranchVisibilityPublic, Lifecycle: domain.BranchLifecycleActive,
			GitRef: "refs/heads/feature", BaseStateID: strptr(srcStateID)},
		target: domain.Branch{ID: mainBranchID, ProjectID: projID, Name: "main",
			Visibility: domain.BranchVisibilityPublic, Lifecycle: domain.BranchLifecycleActive,
			GitRef: "refs/heads/main", BaseStateID: strptr(tgtStateID)},
		heads:       heads,
		lockedHeads: heads,
		mergeRow: domain.SemanticMerge{
			ID: mergeID, ProjectID: projID, PullRequestID: prID, ResultStateID: resultState,
			ActorID: actorID, GitRef: strptr("refs/heads/main"), GitState: domain.GitStatePending,
		},
	}
	s.lockedScope = s.currentScope(resultState)
	return s
}

// currentScope is the scope a transaction sees when nothing moved: the locked
// target head is the state this commit is creating. That is not a convenience —
// persistence.StateStore.CommitState advances the branch head BEFORE the write
// callback runs, so this IS what an honest transaction sees (see verifyScope).
func (f *fakeStore) currentScope(stateID string) MergeScope {
	target := f.target
	target.BaseStateID = strptr(stateID)
	return MergeScope{PullRequest: f.pr, Source: f.source, Target: target}
}

func (f *fakeStore) GetPullRequest(context.Context, string, int64) (domain.PullRequest, error) {
	f.getPRCalls++
	return f.pr, nil
}

func (f *fakeStore) GetBranch(_ context.Context, _ string, branchID string) (domain.Branch, error) {
	if branchID == mainBranchID {
		return f.target, nil
	}
	return f.source, nil
}

func (f *fakeStore) VersionHeads(context.Context, []string, []string) (VersionHeads, error) {
	return f.heads, nil
}

func (f *fakeStore) LockMergeScope(_ context.Context, _ states.Transaction, _ LockScopeParams) (MergeScope, error) {
	f.lockScopeCalls++
	if f.lockedScopeErr != nil {
		return MergeScope{}, f.lockedScopeErr
	}
	return f.lockedScope, nil
}

func (f *fakeStore) LockVersionHeads(context.Context, states.Transaction, []string, []string) (VersionHeads, error) {
	f.lockHeadsCalls++
	if len(f.lockedHeadsSeq) == 0 {
		return f.lockedHeads, nil
	}
	i := f.lockedHeadsN
	if i >= len(f.lockedHeadsSeq) {
		i = len(f.lockedHeadsSeq) - 1
	}
	f.lockedHeadsN++
	return f.lockedHeadsSeq[i], nil
}

func (f *fakeStore) WriteMerge(_ context.Context, _ states.Transaction, in WriteMergeParams) (domain.SemanticMerge, error) {
	f.writeMergeCalls++
	f.mergeParams = append(f.mergeParams, in)
	row := f.mergeRow
	row.Applied, row.KeptTarget = in.Applied, in.KeptTarget
	row.Carried, row.Aborted, row.Withheld = in.Carried, in.Aborted, in.Withheld
	row.Plan, row.PlanDigest, row.PlanVersion = in.PlanJSON, in.PlanDigest, in.PlanVersion
	row.GitRef, row.GitState = in.GitRef, in.GitState
	row.SourceBranchID = in.SourceBranchID
	f.mergeRow = row
	return row, nil
}

func (f *fakeStore) MarkPullRequestMerged(context.Context, states.Transaction, string, string) error {
	f.markPRCalls++
	return nil
}

func (f *fakeStore) MarkBranchMerged(context.Context, states.Transaction, string) error {
	f.markBranchCalls++
	return nil
}

func (f *fakeStore) CompleteGitStep(_ context.Context, in GitStepParams) (domain.SemanticMerge, error) {
	f.gitSteps = append(f.gitSteps, in)
	f.mergeRow.GitState, f.mergeRow.GitError = in.State, in.Error
	if in.SHA != "" {
		f.mergeRow.GitSHA = strptr(in.SHA)
	}
	return f.mergeRow, nil
}

func (f *fakeStore) RecordGitAttempt(_ context.Context, in GitStepParams) (domain.SemanticMerge, error) {
	f.gitSteps = append(f.gitSteps, in)
	f.mergeRow.GitState, f.mergeRow.GitError = in.State, in.Error
	f.mergeRow.GitAttempts++
	return f.mergeRow, nil
}

func (f *fakeStore) GetMergeByPullRequest(context.Context, string, string) (domain.SemanticMerge, error) {
	return f.mergeRow, nil
}

func (f *fakeStore) ListCarriedConflicts(context.Context, string) ([]domain.SemanticMergeConflict, error) {
	return nil, nil
}

// LookupMergeCreation answers the Idempotency-Key ledger read. ledger is what
// a previous call with the same key stored — nil means the key is unused, which
// is what every test that does not stage a replay sees.
func (f *fakeStore) LookupMergeCreation(_ context.Context, _, key string) (*domain.SemanticMerge, error) {
	f.ledgerReads++
	f.ledgerKeys = append(f.ledgerKeys, key)
	if f.ledger == nil {
		return nil, nil
	}
	row := *f.ledger
	return &row, nil
}

// fakeChecks stands for prchecks.Service: the integrity review the command
// re-runs server-side. The report is staged, so a test can hand the merge a
// blocking failure without building one.
type fakeChecks struct {
	report integrity.Report
	err    error
	calls  int
}

func (f *fakeChecks) CheckPullRequest(context.Context, string, int64) (integrity.Report, error) {
	f.calls++
	if f.err != nil {
		return integrity.Report{}, f.err
	}
	return f.report, nil
}

// blockingReport is one BLOCKING failure — the shape the merge must refuse.
// The result is a FAILURE (Passed=false at blocking severity), which is what
// Report.Failures reports and what the command refuses on.
func blockingReport() integrity.Report {
	return integrity.Report{Kind: "integrity", Verdict: integrity.VerdictBlocked,
		Explanation: "1 check blocked the proposal: schema_registered",
		Results: []integrity.Result{{
			Dimension: integrity.DimensionSchema, Check: integrity.CheckSchemaRegistered,
			Severity: integrity.SeverityBlocking, Passed: false,
			Subject: "claim v1 (obj-fixture)", Detail: "the claim carries no evidence",
			Why: "a claim without evidence is not reviewable",
		}}}
}

// warningReport is the same shape at WARNING severity: reported by the review
// page, never a reason to refuse the merge.
func warningReport() integrity.Report {
	report := blockingReport()
	report.Verdict = integrity.VerdictPassWithWarn
	report.Results[0].Severity = integrity.SeverityWarning
	return report
}

// fakePolicies stands for policy.Service.EffectivePolicy.
type fakePolicies struct {
	effective domain.EffectivePolicy
	err       error
	calls     int
}

func (f *fakePolicies) EffectivePolicy(context.Context, domain.User, string) (domain.EffectivePolicy, error) {
	f.calls++
	if f.err != nil {
		return domain.EffectivePolicy{}, f.err
	}
	return f.effective, nil
}

// fakeRules stands for policy.NewRuleEvaluator(): one typed question answered
// against the policy document. found/value stage the two fail-closed cases
// (an absent rule and a rule set to false).
type fakeRules struct {
	found bool
	value bool
	err   error
	calls int
}

func (f *fakeRules) Evaluate(context.Context, domain.Policy, policy.Query) (policy.Decision, error) {
	f.calls++
	if f.err != nil {
		return policy.Decision{}, f.err
	}
	return policy.Decision{Found: f.found, Bool: f.value}, nil
}

// fakeEvents is the outbox recorder: it records what the merge emitted inside
// its transaction, which is where the domain event has to be written.
type fakeEvents struct {
	events []events.Event
	err    error
}

func (f *fakeEvents) Record(_ context.Context, _ events.DBTX, e events.Event) error {
	if f.err != nil {
		return f.err
	}
	f.events = append(f.events, e)
	return nil
}

// fakeObjects and fakeRelations are the append-only writers; they count the
// version rows the merge created, which is what "nothing was written" means.
type fakeObjects struct {
	written int
	ids     []string
}

func (f *fakeObjects) CreateVersionInTx(_ context.Context, _ states.Transaction, objectID string, expected int, in sciobjects.VersionParams) (domain.ScientificObjectVersion, error) {
	f.written++
	f.ids = append(f.ids, objectID)
	return domain.ScientificObjectVersion{
		ID: "v-" + objectID, ObjectID: objectID, VersionNo: expected + 1,
		StateID: in.StateID, Title: in.Title, Payload: in.Payload, CreatedBy: in.CreatedBy,
	}, nil
}

type fakeRelations struct{ written int }

func (f *fakeRelations) AppendRelationVersionInTx(_ context.Context, _ states.Transaction, relationID string, expected int, in relations.VersionParams) (domain.RelationVersion, error) {
	f.written++
	return domain.RelationVersion{
		ID: "relv-" + relationID, RelationID: relationID, VersionNo: expected + 1, StateID: in.StateID,
	}, nil
}

// fakeProjects answers the membership the merge authorizes against.
type fakeProjects struct {
	membership domain.ProjectMembership
	err        error
	calls      int
}

func (f *fakeProjects) GetMembership(context.Context, domain.User, string) (domain.ProjectMembership, error) {
	f.calls++
	if f.err != nil {
		return domain.ProjectMembership{}, f.err
	}
	return f.membership, nil
}

// fakeAuthz records the request and answers with the configured verdict.
type fakeAuthz struct {
	decision authz.Decision
	last     authz.Request
	calls    int
}

func (f *fakeAuthz) Authorize(_ context.Context, req authz.Request) (authz.Decision, error) {
	f.calls++
	f.last = req
	return f.decision, nil
}

// fakeGit is the provider-side merger: it returns the sha and the identity the
// RefGuard then judges.
type fakeGit struct {
	sha   string
	actor string
	err   error
	calls int
	// requests records what the saga asked the provider-side adapter to
	// merge, which is the only place the ref PAIR is observable.
	requests []GitMergeRequest
}

func (f *fakeGit) MergePullRequest(_ context.Context, in GitMergeRequest) (GitMergeResult, error) {
	f.calls++
	f.requests = append(f.requests, in)
	if f.err != nil {
		return GitMergeResult{}, f.err
	}
	return GitMergeResult{SHA: f.sha, Actor: f.actor}, nil
}

// harness is one wired service plus the fakes behind it.
type harness struct {
	svc       *Service
	store     *fakeStore
	diffs     *fakeDiffs
	plans     *fakePlans
	commits   *fakeCommits
	objects   *fakeObjects
	relations *fakeRelations
	projects  *fakeProjects
	authz     *fakeAuthz
	checks    *fakeChecks
	policies  *fakePolicies
	rules     *fakeRules
	events    *fakeEvents
	git       *fakeGit
}

func newHarness(t *testing.T, opts ...func(*harness)) *harness {
	t.Helper()
	h := &harness{
		store:     newFakeStore(),
		diffs:     &fakeDiffs{inputs: safeInputs()},
		plans:     &fakePlans{},
		commits:   &fakeCommits{},
		objects:   &fakeObjects{},
		relations: &fakeRelations{},
		projects: &fakeProjects{membership: domain.ProjectMembership{
			ProjectID: projID, UserID: actorID, Role: domain.ProjectRoleMaintainer,
		}},
		authz: &fakeAuthz{decision: authz.Decision{Verdict: authz.VerdictAllow}},
		// The governance half of the command, staged to PERMIT: an empty
		// integrity report, a readable policy and the explicit
		// main_protected=true rule the merge requires. Every test that
		// asserts a refusal stages the failing half itself; leaving these
		// unwired would only prove the fail-closed path.
		checks:   &fakeChecks{},
		policies: &fakePolicies{},
		rules:    &fakeRules{found: true, value: true},
		events:   &fakeEvents{},
	}
	for _, opt := range opts {
		opt(h)
	}
	h.svc = h.service(0)
	return h
}

// service wires the harness's fakes into one service; attempts > 0 overrides
// the retry bound. The Git port is left untyped-nil absent when the harness has
// no adapter — the "no adapter wired" path (the pending step) is what is being
// exercised, and NewService normalizes a typed nil to a nil interface.
func (h *harness) service(attempts int) *Service {
	d := Deps{
		Store: h.store, Diffs: h.diffs, Plans: h.plans, Commits: h.commits,
		Objects: h.objects, Relations: h.relations, Projects: h.projects, Authz: h.authz,
		Checks: h.checks, Policies: h.policies, Rules: h.rules, Events: h.events,
		RefGuard: RefGuard{MergeService: mergeService}, MaxAttempts: attempts,
	}
	if h.git != nil {
		d.Git = h.git
	}
	return NewService(d)
}

// merge runs one merge as a maintainer.
func (h *harness) merge(t *testing.T) (*Result, error) {
	t.Helper()
	return h.svc.Merge(context.Background(), domain.User{ID: actorID}, Input{ProjectID: projID, Number: 7})
}

// decideAll records one decision per conflict of the blocked plan, with the
// given resolution kind — the transcription T0407's store performs.
func decideAll(t *testing.T, blocked *BlockedError, kind domain.ResolutionKind) []domain.ConflictResolution {
	t.Helper()
	var out []domain.ConflictResolution
	for _, c := range blocked.Plan.Changes {
		for _, cfl := range c.Conflicts {
			out = append(out, domain.ConflictResolution{
				ProjectID:     projID,
				BaseStateID:   baseStateID,
				SourceStateID: srcStateID,
				TargetStateID: tgtStateID,
				TargetKind:    c.TargetKind,
				TargetID:      c.TargetID,
				Code:          cfl.Code,
				Fields:        cfl.Fields,
				PayloadKeys:   cfl.PayloadKeys,
				Kind:          kind,
				DecidedBy:     actorID,
				Note:          "kept open",
			})
		}
	}
	if len(out) == 0 {
		t.Fatalf("the blocked plan reported no conflict to decide: %+v", blocked.Plan.Changes)
	}
	return out
}

// blockedPlan returns the plan the service refuses a triple with, so a test can
// decide exactly the conflicts the service reported.
func blockedPlan(t *testing.T, h *harness) *BlockedError {
	t.Helper()
	_, err := h.merge(t)
	var blocked *BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("the merge should have been blocked first: %v", err)
	}
	return blocked
}

// TestUnwiredServiceRefuses keeps the default-deny shape: an unconfigured
// service refuses at call time instead of committing a merge of nothing.
func TestUnwiredServiceRefuses(t *testing.T) {
	svc := NewService(Deps{})
	if _, err := svc.Merge(context.Background(), domain.User{ID: actorID}, Input{ProjectID: projID, Number: 7}); err == nil {
		t.Fatalf("an unwired merge service merged a PR")
	}
}

// TestTypedNilGitMergerIsUnwiredNotPanicking pins the composition-root trap
// T0409 was warned about: Deps.Git is an interface, and the nil *fakeGit a
// deployment hands it is NOT a nil interface — it is a non-nil interface
// holding a nil pointer. Without NewService's normalization that value passes
// the "no adapter wired" check, and the first merge panics inside runGitStep,
// AFTER PostgreSQL has accepted the state. The merge must instead complete its
// database half and record the Git step as pending, exactly as it does with an
// untyped nil.
func TestTypedNilGitMergerIsUnwiredNotPanicking(t *testing.T) {
	h := newHarness(t)
	var typedNil *fakeGit // nil pointer, non-nil interface once assigned
	h.svc = NewService(Deps{
		Store: h.store, Diffs: h.diffs, Plans: h.plans, Commits: h.commits,
		Objects: h.objects, Relations: h.relations, Projects: h.projects, Authz: h.authz,
		Checks: h.checks, Policies: h.policies, Rules: h.rules, Events: h.events,
		Git:      typedNil,
		RefGuard: RefGuard{MergeService: mergeService},
	})
	if h.svc.git != nil {
		t.Fatalf("a typed-nil adapter was accepted as a wired GitMerger")
	}

	res, err := h.merge(t)
	if err != nil {
		t.Fatalf("merge with a typed-nil Git adapter = %v", err)
	}
	if res.Merge.GitState != domain.GitStatePending {
		t.Fatalf("merge row state = %q, want pending (the Git half did not happen)", res.Merge.GitState)
	}
	// The nil adapter is not counted on purpose: calling it would dereference
	// the nil receiver and panic, so this test finishing IS the proof it was
	// never reached.
	if len(h.store.gitSteps) != 1 || h.store.gitSteps[0].Error == "" {
		t.Fatalf("recorded git steps = %+v, want one pending step with a reason", h.store.gitSteps)
	}
}

// TestMergeRefusesWhenTheIntegrityReviewBlocks: the command re-runs the
// review server-side (docs/22 §7) and refuses on a BLOCKING failure, carrying
// the complete report — the same shape releases.GateRefused uses. Nothing is
// written: no state, no merge row, no audit row, no event.
func TestMergeRefusesWhenTheIntegrityReviewBlocks(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.checks.report = blockingReport() })
	_, err := h.merge(t)

	var refused *GateRefused
	if !errors.As(err, &refused) {
		t.Fatalf("merge of a blocking PR = %v, want *GateRefused", err)
	}
	if !errors.Is(err, ErrGateRefused) {
		t.Fatalf("err = %v, want it to wrap ErrGateRefused", err)
	}
	if got := refused.Code(); got != CodeIntegrityBlocked {
		t.Fatalf("code = %q, want %q", got, CodeIntegrityBlocked)
	}
	if len(refused.Report.Results) != 1 || refused.Report.Results[0].Check != integrity.CheckSchemaRegistered {
		t.Fatalf("the refusal carries report %+v, want the COMPLETE report (every result)", refused.Report)
	}
	if refused.Report.Explanation == "" {
		t.Fatalf("the refusal carries no explanation to show the caller")
	}
	if h.store.writeMergeCalls != 0 || h.commits.attempts != 0 {
		t.Fatalf("a refused merge wrote: merge=%d commits=%d", h.store.writeMergeCalls, h.commits.attempts)
	}
	if len(h.events.events) != 0 {
		t.Fatalf("a refused merge emitted %d event(s)", len(h.events.events))
	}
}

// TestMergeRunsDespiteAWarningSeverityFailure: the severity belongs to the
// check declaration, not to this command. A warning is reported by the review
// page and does not stop main from advancing.
func TestMergeRunsDespiteAWarningSeverityFailure(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.checks.report = warningReport() })
	res, err := h.merge(t)
	if err != nil {
		t.Fatalf("merge with a warning-severity failure = %v", err)
	}
	if res.Merge.GitState != domain.GitStatePending {
		t.Fatalf("merge row = %q, want the merge to have run", res.Merge.GitState)
	}
	if h.checks.calls != 1 {
		t.Fatalf("integrity runs = %d, want exactly one", h.checks.calls)
	}
}

// TestMergeRefusesWhenTheIntegrityReviewCannotRun: an unreviewable PR is not a
// reviewed one. A check that fails to run refuses the merge, and the failure is
// not dressed up as a gate refusal (the report does not exist).
func TestMergeRefusesWhenTheIntegrityReviewCannotRun(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.checks.err = errors.New("checks: store unavailable") })
	_, err := h.merge(t)
	if err == nil || errors.Is(err, ErrGateRefused) {
		t.Fatalf("merge whose review could not run = %v, want the check's own failure", err)
	}
	if h.store.writeMergeCalls != 0 {
		t.Fatalf("an unreviewable merge wrote %d merge row(s)", h.store.writeMergeCalls)
	}
}

// TestMainProtectedPolicyFailsClosed: every way the governance policy can
// decline to permit the merge refuses it — an unreadable policy, an
// unevaluable rule, an ABSENT rule (silence is not a permission) and a rule
// set to false (no policy in this build waives the frozen-main guarantee).
// Nothing is written in any of the four cases.
func TestMainProtectedPolicyFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(*harness)
		found bool
		value bool
	}{
		{"unreadable policy", func(h *harness) { h.policies.err = errors.New("policy: unavailable") }, false, false},
		{"unevaluable rule", func(h *harness) { h.rules.err = errors.New("policy: unknown rule kind") }, false, false},
		{"absent rule", func(h *harness) { h.rules.found = false }, false, false},
		{"rule set to false", func(h *harness) { h.rules.value = false }, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hp := newHarness(t)
			tc.apply(hp)
			_, err := hp.merge(t)

			var refused *PolicyRefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("merge under %s = %v, want *PolicyRefusedError", tc.name, err)
			}
			if !errors.Is(err, ErrPolicyRefused) {
				t.Fatalf("err = %v, want it to wrap ErrPolicyRefused", err)
			}
			if refused.Code() != CodePolicyRefused {
				t.Fatalf("code = %q, want %q", refused.Code(), CodePolicyRefused)
			}
			if refused.Rule != domain.RuleMainProtected {
				t.Fatalf("refused on rule %q, want %q", refused.Rule, domain.RuleMainProtected)
			}
			if refused.Found != tc.found || refused.Bool != tc.value {
				t.Fatalf("refusal reports found=%v bool=%v, want found=%v bool=%v",
					refused.Found, refused.Bool, tc.found, tc.value)
			}
			if refused.Reason == "" {
				t.Fatalf("the refusal carries no reason for the caller")
			}
			if hp.store.writeMergeCalls != 0 || hp.commits.attempts != 0 {
				t.Fatalf("a policy-refused merge wrote: merge=%d commits=%d", hp.store.writeMergeCalls, hp.commits.attempts)
			}
		})
	}
}

// TestPolicyPrecedesTheWriteButFollowsThePlan: the plan is computed before the
// policy is read (a plan is a pure function of states a human already pinned),
// and the policy is read before anything is written. The ordering matters
// because the plan is what the audit row and the event describe.
func TestPolicyPrecedesTheWriteButFollowsThePlan(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.rules.value = false })
	_, err := h.merge(t)
	if !errors.Is(err, ErrPolicyRefused) {
		t.Fatalf("merge = %v, want ErrPolicyRefused", err)
	}
	if h.plans.calls != 1 || h.diffs.calls != 1 {
		t.Fatalf("the plan was not computed before the policy read: plans=%d diffs=%d", h.plans.calls, h.diffs.calls)
	}
	if h.store.writeMergeCalls != 0 {
		t.Fatalf("the policy refusal came after the merge row was written")
	}
}

// TestMergeReplaysAnIdempotencyKey: the same key returns the merge the first
// call produced, writes nothing, and does not re-run a single gate — a replay
// is a read. The merge the key names is what comes back, not a re-decision of
// it against today's policy.
func TestMergeReplaysAnIdempotencyKey(t *testing.T) {
	h := newHarness(t)
	key := "merge-key-0001"
	h.store.ledger = &domain.SemanticMerge{
		ID: mergeID, ProjectID: projID, PullRequestID: prID, ResultStateID: resultState,
		ActorID: actorID, GitRef: strptr("refs/heads/main"), GitState: domain.GitStateUpdated,
		GitSHA: strptr(mergeSHA),
	}
	res, err := h.svc.Merge(context.Background(), domain.User{ID: actorID},
		Input{ProjectID: projID, Number: 7, IdempotencyKey: &key})
	if err != nil {
		t.Fatalf("replay = %v", err)
	}
	if !res.Replayed || res.Merge.ID != mergeID {
		t.Fatalf("replay result = %+v, want the stored merge %s marked replayed", res, mergeID)
	}
	if h.store.writeMergeCalls != 0 || h.commits.attempts != 0 {
		t.Fatalf("a replay wrote: merge=%d commits=%d", h.store.writeMergeCalls, h.commits.attempts)
	}
	if h.diffs.calls != 0 || h.plans.calls != 0 || h.checks.calls != 0 || h.policies.calls != 0 {
		t.Fatalf("a replay re-ran the gates: diffs=%d plans=%d checks=%d policies=%d",
			h.diffs.calls, h.plans.calls, h.checks.calls, h.policies.calls)
	}
	if h.store.ledgerReads != 1 || h.store.ledgerKeys[0] != key {
		t.Fatalf("ledger reads = %d keys=%v, want exactly one read of %q", h.store.ledgerReads, h.store.ledgerKeys, key)
	}
	if len(h.store.gitSteps) != 0 {
		t.Fatalf("a replay whose Git step already finished re-drove it: %+v", h.store.gitSteps)
	}
}

// TestMergeKeyAbsentSkipsTheLedger: without an Idempotency-Key there is
// nothing to replay, so the ledger is never read and the merge runs normally.
// The key is optional at this layer — the transport is what makes it required
// on the route the contract marks it so.
func TestMergeKeyAbsentSkipsTheLedger(t *testing.T) {
	h := newHarness(t)
	res, err := h.merge(t)
	if err != nil {
		t.Fatalf("merge without a key = %v", err)
	}
	if res.Replayed {
		t.Fatalf("a keyless merge was reported as a replay")
	}
	if h.store.ledgerReads != 0 {
		t.Fatalf("ledger reads = %d, want 0 without a key", h.store.ledgerReads)
	}
	if h.store.mergeParams[0].IdempotencyKey != nil {
		t.Fatalf("the keyless merge wrote a ledger key: %q", *h.store.mergeParams[0].IdempotencyKey)
	}
}

// TestMergeRecordsItsAuditRowAndDomainEvent: the merge, its audit row and its
// domain event are one unit — the audit row is handed to the same transaction
// that writes the merge, and the event goes through the outbox recorder in that
// transaction. The two vocabularies are the point: the audit action
// (domain.ActionPullRequestMerged) and the event type
// (specs/events/event-types.yaml's pull_request.merged) name the same act in the
// two registries the platform keeps, and neither registry derives from the
// other.
func TestMergeRecordsItsAuditRowAndDomainEvent(t *testing.T) {
	h := newHarness(t)
	res, err := h.merge(t)
	if err != nil {
		t.Fatalf("merge = %v", err)
	}
	if len(h.store.mergeParams) != 1 {
		t.Fatalf("merge writes = %d, want 1", len(h.store.mergeParams))
	}
	audit := h.store.mergeParams[0].Audit
	if audit.Action != domain.ActionPullRequestMerged {
		t.Fatalf("audit action = %q, want %q", audit.Action, domain.ActionPullRequestMerged)
	}
	if audit.ActorID != actorID || audit.ProjectID != projID {
		t.Fatalf("audit row = %+v, want the actor and the project", audit)
	}
	if len(h.events.events) != 1 {
		t.Fatalf("domain events = %d, want exactly 1", len(h.events.events))
	}
	e := h.events.events[0]
	// specs/events/event-types.yaml spells the event pull_request.merged.
	// docs/18 §2 names the same event in prose as "pr.merged" — the
	// machine-readable vocabulary wins, because it is what every consumer
	// switches on (pull_request.opened / pull_request.reviewed are the same
	// pair of spellings one line above it in the same list).
	if e.EventType != eventPullRequestMerged {
		t.Fatalf("event type = %q, want %q", e.EventType, eventPullRequestMerged)
	}
	if e.Visibility != events.VisibilityPublic {
		t.Fatalf("event visibility = %q, want the public target branch's own visibility", e.Visibility)
	}
	if e.ProjectID != projID || e.ActorID != actorID {
		t.Fatalf("event = %+v, want the project and the actor", e)
	}
	var payload struct {
		MergeID           string `json:"merge_id"`
		PullRequestID     string `json:"pull_request_id"`
		PullRequestNumber int64  `json:"pull_request_number"`
		StateID           string `json:"state_id"`
		TargetBranchID    string `json:"target_branch_id"`
	}
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		t.Fatalf("event payload %s is not JSON: %v", e.Payload, err)
	}
	if payload.MergeID != res.Merge.ID {
		t.Fatalf("payload merge_id = %q, want %q", payload.MergeID, res.Merge.ID)
	}
	if payload.PullRequestNumber != 7 || payload.PullRequestID != prID {
		t.Fatalf("payload = %+v, want the PR it merged", payload)
	}
	if payload.StateID != resultState || payload.TargetBranchID != mainBranchID {
		t.Fatalf("payload = %+v, want the accepted state and the target branch", payload)
	}
}

// TestMergeFailsWhenTheEventCannotBeRecorded: the outbox write is part of the
// merge's transaction (docs/53) — an event that cannot be recorded fails the
// merge rather than being dropped.
func TestMergeFailsWhenTheEventCannotBeRecorded(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.events.err = errors.New("outbox: insert refused") })
	if _, err := h.merge(t); err == nil {
		t.Fatalf("a merge whose domain event could not be recorded reported success")
	}
}

// TestValidationPrecedesEveryRead: a malformed request is refused on its shape,
// before the authorization and before any read.
func TestValidationPrecedesEveryRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   Input
		user domain.User
	}{
		{"no project", Input{Number: 7}, domain.User{ID: actorID}},
		{"no number", Input{ProjectID: projID}, domain.User{ID: actorID}},
		{"no actor", Input{ProjectID: projID, Number: 7}, domain.User{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if _, err := h.svc.Merge(context.Background(), tc.user, tc.in); !errors.Is(err, ErrValidation) {
				t.Fatalf("merge of %+v = %v, want ErrValidation", tc.in, err)
			}
			if h.authz.calls != 0 || h.store.getPRCalls != 0 {
				t.Fatalf("the shape refusal authorized/read: authz=%d pr=%d", h.authz.calls, h.store.getPRCalls)
			}
		})
	}
}

// TestRefusalPrecedesEveryRead: an actor who may not merge is refused before
// the PR, the branches and the states are read — a refusal must not disclose
// whether the PR exists.
func TestRefusalPrecedesEveryRead(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.authz.decision = authz.Decision{Verdict: authz.VerdictDeny, Reason: "maintainer and above"}
	})
	_, err := h.merge(t)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("merge as a denied actor = %v, want ErrForbidden", err)
	}
	if h.store.getPRCalls != 0 || h.diffs.calls != 0 || h.plans.calls != 0 {
		t.Fatalf("the refusal read state first: pr=%d diffs=%d plans=%d",
			h.store.getPRCalls, h.diffs.calls, h.plans.calls)
	}
	if h.commits.attempts != 0 || h.store.writeMergeCalls != 0 {
		t.Fatalf("the denied merge wrote: commits=%d merges=%d", h.commits.attempts, h.store.writeMergeCalls)
	}
}

// TestMergeAuthorizesMergeMain: the merge is the ActionMergeMain gate, and the
// role the membership resolved is the one the policy engine sees.
func TestMergeAuthorizesMergeMain(t *testing.T) {
	h := newHarness(t)
	if _, err := h.merge(t); err != nil {
		t.Fatalf("merge as a maintainer = %v", err)
	}
	if h.authz.last.Action != authz.ActionMergeMain {
		t.Fatalf("authorized action = %q, want %q", h.authz.last.Action, authz.ActionMergeMain)
	}
	role := h.projects.membership.Role
	if h.authz.last.Class != authz.ClassOf(true, &role, false) {
		t.Fatalf("actor class = %v, want the maintainer's", h.authz.last.Class)
	}
}

// TestMergeRefusesANonMember: a non-member gets a refusal, not a membership
// oracle — the class the engine sees is the actor's, not the project's.
func TestMergeRefusesANonMember(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.projects.err = projects.ErrMemberNotFound
		h.authz.decision = authz.Decision{Verdict: authz.VerdictDeny, Reason: "not a member"}
	})
	_, err := h.merge(t)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("merge by a non-member = %v, want ErrForbidden", err)
	}
	if h.authz.last.Class != authz.ClassOf(true, nil, false) {
		t.Fatalf("actor class = %v, want the class of an authenticated actor with no role", h.authz.last.Class)
	}
	if h.store.getPRCalls != 0 {
		t.Fatalf("the non-member's refusal read the PR: %d calls", h.store.getPRCalls)
	}
}

// TestMergeRefusesAPullRequestThatIsNotMergeReady: docs/43's `merged` is
// reachable from merge_ready only, and the merge is the transition that lands
// it. Every other state is refused before the plan is computed — the review
// machine cannot be skipped by calling the merge service directly.
func TestMergeRefusesAPullRequestThatIsNotMergeReady(t *testing.T) {
	for _, state := range []domain.PullRequestState{
		domain.PullRequestStateOpen,
		domain.PullRequestStateReviewRequired,
		domain.PullRequestStateChangesRequested,
		domain.PullRequestStateApproved,
		domain.PullRequestStateMerged,
	} {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			h.store.pr.State = state
			_, err := h.merge(t)
			var notMergeable *NotMergeableError
			if !errors.As(err, &notMergeable) {
				t.Fatalf("merge of a %s PR = %v, want *NotMergeableError", state, err)
			}
			if h.diffs.calls != 0 || h.commits.attempts != 0 {
				t.Fatalf("a %s PR reached the plan/commit: diffs=%d commits=%d", state, h.diffs.calls, h.commits.attempts)
			}
		})
	}
}

// TestMergeRefusesANonActiveBranch: a closed research path neither merges nor
// accepts a merge (docs/43).
func TestMergeRefusesANonActiveBranch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*harness)
	}{
		{"source merged", func(h *harness) { h.store.source.Lifecycle = domain.BranchLifecycleMerged }},
		{"source aborted", func(h *harness) { h.store.source.Lifecycle = domain.BranchLifecycleAborted }},
		{"target merged", func(h *harness) { h.store.target.Lifecycle = domain.BranchLifecycleMerged }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.mutate)
			_, err := h.merge(t)
			var notActive *BranchNotActiveError
			if !errors.As(err, &notActive) {
				t.Fatalf("merge with a %s branch = %v, want *BranchNotActiveError", tc.name, err)
			}
			if h.commits.attempts != 0 {
				t.Fatalf("the merge committed anyway: %d attempts", h.commits.attempts)
			}
		})
	}
}

// TestBlockedPlanIsRefusedWithThePlan: an undecided scientific conflict does not
// merge. The refusal carries the engine's plan, so the caller can show the human
// exactly what is undecided, and nothing is written — no state, no version row,
// no merge record. The service never picks a winner to make the merge succeed.
func TestBlockedPlanIsRefusedWithThePlan(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.diffs.inputs = conflictedInputs() })
	_, err := h.merge(t)
	var blocked *BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("merge over an undecided conflict = %v, want *BlockedError", err)
	}
	if blocked.Code() != CodeMergeBlocked {
		t.Fatalf("blocked code = %s, want %s", blocked.Code(), CodeMergeBlocked)
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
	var conflicted *rsgmerge.Change
	for i := range blocked.Plan.Changes {
		if blocked.Plan.Changes[i].TargetID == claimID {
			conflicted = &blocked.Plan.Changes[i]
		}
	}
	if conflicted == nil {
		t.Fatalf("the undecided claim is not in the plan: %+v", blocked.Plan.Changes)
	}
	if conflicted.Effect != rsgmerge.EffectBlocked || conflicted.Resolution != rsgmerge.ResolutionUndecided {
		t.Fatalf("conflicted change = %q/%q, want blocked/undecided", conflicted.Effect, conflicted.Resolution)
	}
	if conflicted.Materialize {
		t.Fatalf("the blocked change is marked materialize: %+v", conflicted)
	}
	if len(conflicted.Conflicts) == 0 {
		t.Fatalf("the blocked change carries no conflict, so the refusal names nothing to decide: %+v", conflicted)
	}
	if h.commits.attempts != 0 || h.objects.written != 0 || h.store.writeMergeCalls != 0 {
		t.Fatalf("the blocked merge wrote: commits=%d versions=%d merges=%d",
			h.commits.attempts, h.objects.written, h.store.writeMergeCalls)
	}
}

// TestDecidedConflictMergesWithoutAWinner is the same conflicted triple with the
// human's decision recorded: the merge lands, and the contested claim still gets
// NO version row — the conflict travels into the accepted state as a carried
// record naming both versions, instead of being resolved into one winner. The
// decisions are built from the conflicts the service's OWN blocked plan
// reported, which is exactly what the resolution store transcribes.
func TestDecidedConflictMergesWithoutAWinner(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.diffs.inputs = conflictedInputs() })
	blocked := blockedPlan(t, h)
	h.plans.decisions = decideAll(t, blocked, domain.ResolutionKeepBoth)
	wantCarried := 0
	for _, c := range blocked.Plan.Changes {
		if c.TargetID == claimID {
			wantCarried = len(c.Conflicts)
		}
	}

	res, err := h.merge(t)
	if err != nil {
		t.Fatalf("merge with every conflict decided keep-both: %v", err)
	}
	if res.Merge.Applied != 1 || res.Merge.Carried != 1 {
		t.Fatalf("merge record applied/carried = %d/%d, want the safe change applied and the conflict carried",
			res.Merge.Applied, res.Merge.Carried)
	}
	if h.objects.written != 1 || h.objects.ids[0] != safeObjectID {
		t.Fatalf("versions written = %v, want the safe object only — a carried conflict writes no version",
			h.objects.ids)
	}
	for _, w := range res.Written {
		if w.TargetID == claimID {
			t.Fatalf("the contested claim was materialized: %+v", w)
		}
	}
	if res.Merge.PlanDigest == "" || len(res.Merge.Plan) == 0 {
		t.Fatalf("the merge record carries no plan: digest %q, %d bytes", res.Merge.PlanDigest, len(res.Merge.Plan))
	}
	// The carried rows are what "no winner" means on the wire: the two versions
	// that stay, and the human who decided to keep them open. The change carries
	// two conflicts (the title diverged too, so its attribute conflict rides
	// along with the knowledge one) while Summary.Carried counts CHANGES — both
	// facts are the engine's, and neither is smoothed into the other here.
	if h.store.writeMergeCalls != 1 {
		t.Fatalf("merge records written = %d, want 1", h.store.writeMergeCalls)
	}
	carried := h.store.mergeParams[0].CarriedConflicts
	if len(carried) != wantCarried || wantCarried == 0 {
		t.Fatalf("carried conflicts = %d, want the %d the contested change reported", len(carried), wantCarried)
	}
	for _, c := range carried {
		if c.TargetID != claimID || c.DecidedBy != actorID || c.Decision != domain.ResolutionKeepBoth {
			t.Fatalf("carried conflict = %+v, want the claim's conflict kept by %s", c, actorID)
		}
		if c.TargetVersionID == nil || c.SourceVersionID == "" {
			t.Fatalf("the carried conflict does not name both versions: %+v", c)
		}
		if c.Code == "" || c.Category == "" || c.Detail == "" {
			t.Fatalf("the carried conflict does not say what it is: %+v", c)
		}
	}
	if h.store.markPRCalls != 1 {
		t.Fatalf("PR merges recorded = %d, want 1", h.store.markPRCalls)
	}
}

// TestMergeRefusesWhenNothingWouldLand: a merge whose only change is carried
// writes nothing, and a state commit claiming a merge happened would be a lie.
// The engine's CodeNothingToMerge blocker is the refusal, and it fires even with
// every conflict decided — a decision is not a change to merge.
func TestMergeRefusesWhenNothingWouldLand(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.diffs.inputs = claimOnlyInputs() })
	h.plans.decisions = decideAll(t, blockedPlan(t, h), domain.ResolutionKeepBoth)

	_, err := h.merge(t)
	var blocked *BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("a merge whose every change is carried = %v, want *BlockedError", err)
	}
	var nothing bool
	for _, b := range blocked.Plan.Blockers {
		if b.Code == rsgmerge.CodeNothingToMerge {
			nothing = true
		}
	}
	if !nothing {
		t.Fatalf("blockers = %+v, want %s", blocked.Plan.Blockers, rsgmerge.CodeNothingToMerge)
	}
	if h.commits.attempts != 0 || h.store.writeMergeCalls != 0 || h.store.markPRCalls != 0 {
		t.Fatalf("the nothing-to-merge refusal wrote: commits=%d merges=%d pr=%d",
			h.commits.attempts, h.store.writeMergeCalls, h.store.markPRCalls)
	}
}

// TestMergeRefusesAWithheldOnlyPlan: a private source beside a public target
// withholds every change (docs/09 §9 — publishing is the Publication Gate's, not
// the merge's). With nothing left to land the merge refuses, and it never writes
// the private content into a public state.
func TestMergeRefusesAWithheldOnlyPlan(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.store.source.Visibility = domain.BranchVisibilityPrivate
		h.store.target.Visibility = domain.BranchVisibilityPublic
	})
	_, err := h.merge(t)
	var blocked *BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("a private-to-public merge of source-only changes = %v, want *BlockedError", err)
	}
	if blocked.Plan.Summary.Withheld != 1 {
		t.Fatalf("plan withheld = %d, want the private change withheld", blocked.Plan.Summary.Withheld)
	}
	if h.objects.written != 0 || h.commits.attempts != 0 {
		t.Fatalf("the publication-gated merge published: versions=%d commits=%d", h.objects.written, h.commits.attempts)
	}
}

// TestMergeRetriesWhenAVersionCounterMoves: the operation summary names
// {entity, version_no} pairs predicted from an unlocked read. A counter that
// moved rolls the transaction back and is simply re-read — the merge retries,
// and it still commits exactly once.
func TestMergeRetriesWhenAVersionCounterMoves(t *testing.T) {
	h := newHarness(t)
	h.store.lockedHeadsSeq = []VersionHeads{
		{Objects: map[string]int{safeObjectID: 2}, Relations: map[string]int{}},
		h.store.heads,
	}

	res, err := h.merge(t)
	if err != nil {
		t.Fatalf("merge with one rolled-back attempt = %v", err)
	}
	if h.commits.attempts != 2 || h.store.lockHeadsCalls != 2 {
		t.Fatalf("attempts/locked reads = %d/%d, want 2/2 (one rollback, one commit)",
			h.commits.attempts, h.store.lockHeadsCalls)
	}
	if h.store.writeMergeCalls != 1 || h.objects.written != 1 || h.store.markPRCalls != 1 {
		t.Fatalf("committed writes = merges %d / versions %d / PR %d, want exactly one of each",
			h.store.writeMergeCalls, h.objects.written, h.store.markPRCalls)
	}
	if res.Merge.Applied != 1 {
		t.Fatalf("applied = %d, want the one source change", res.Merge.Applied)
	}
}

// TestMergeGivesUpAfterMaxAttempts: the retry is bounded. A counter that keeps
// moving past the bound is a store failure the caller can retry, never a merge
// that quietly commits over a stale prediction.
func TestMergeGivesUpAfterMaxAttempts(t *testing.T) {
	h := newHarness(t)
	h.svc = h.service(2)
	h.store.lockedHeads = VersionHeads{Objects: map[string]int{safeObjectID: 9}, Relations: map[string]int{}}

	_, err := h.merge(t)
	if !errors.Is(err, ErrStore) {
		t.Fatalf("a permanently moved counter = %v, want ErrStore (bounded retry)", err)
	}
	if h.commits.attempts != 2 {
		t.Fatalf("attempts = %d, want the configured bound of 2", h.commits.attempts)
	}
	if h.store.writeMergeCalls != 0 || h.objects.written != 0 {
		t.Fatalf("the abandoned merge wrote: merges=%d versions=%d", h.store.writeMergeCalls, h.objects.written)
	}
}

// TestMergeDoesNotRetryAStaleScope is the fence, and the reason the service
// separates the two failures: a version counter is a re-readable number, but a
// STATE that moved means the plan was computed over a triple nobody decided. The
// locked target head here is not the state this transaction is creating, so
// another writer advanced the branch: the merge refuses with *StaleMergeError,
// tries exactly once, and writes nothing.
func TestMergeDoesNotRetryAStaleScope(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.store.lockedScope = h.store.currentScope(otherStateID)
	})
	_, err := h.merge(t)
	var stale *StaleMergeError
	if !errors.As(err, &stale) {
		t.Fatalf("merge whose locked target head is not the committed state = %v, want *StaleMergeError", err)
	}
	if stale.Code() != CodeMergeStale {
		t.Fatalf("stale code = %s, want %s", stale.Code(), CodeMergeStale)
	}
	if h.commits.attempts != 1 {
		t.Fatalf("commit attempts = %d, want exactly 1: a moved state is never retried", h.commits.attempts)
	}
	if h.store.writeMergeCalls != 0 || h.objects.written != 0 || h.relations.written != 0 {
		t.Fatalf("the refused merge wrote: merges=%d objects=%d relations=%d",
			h.store.writeMergeCalls, h.objects.written, h.relations.written)
	}
	if h.store.markPRCalls != 0 || h.store.markBranchCalls != 0 {
		t.Fatalf("the refused merge closed the PR/branch: pr=%d branch=%d", h.store.markPRCalls, h.store.markBranchCalls)
	}
	if len(h.store.gitSteps) != 0 {
		t.Fatalf("the refused merge ran a Git step: %+v", h.store.gitSteps)
	}
}

// TestMergeRefusesAProposedStateThatMoved is the same fence one fact out: the
// PR's proposed head moved between the plan and the write (someone refreshed the
// proposal), so the plan is over a triple nobody decided.
func TestMergeRefusesAProposedStateThatMoved(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		scope := h.store.currentScope(resultState)
		scope.PullRequest.ProposedStateID = otherStateID
		h.store.lockedScope = scope
	})
	_, err := h.merge(t)
	var stale *StaleMergeError
	if !errors.As(err, &stale) {
		t.Fatalf("merge over a refreshed proposal = %v, want *StaleMergeError", err)
	}
	if h.commits.attempts != 1 || h.store.writeMergeCalls != 0 {
		t.Fatalf("attempts/writes = %d/%d, want 1/0", h.commits.attempts, h.store.writeMergeCalls)
	}
}

// TestMergeRefusesAPullRequestThatLeftMergeReady is the same fence for the PR:
// the in-transaction read finds it elsewhere, which is what the PR row's CAS
// does on the real store.
func TestMergeRefusesAPullRequestThatLeftMergeReady(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		scope := h.store.currentScope(resultState)
		scope.PullRequest.State = domain.PullRequestStateMerged
		h.store.lockedScope = scope
	})
	_, err := h.merge(t)
	var stale *StaleMergeError
	if !errors.As(err, &stale) {
		t.Fatalf("merge whose PR left merge_ready = %v, want *StaleMergeError", err)
	}
	if h.commits.attempts != 1 || h.store.writeMergeCalls != 0 {
		t.Fatalf("attempts/writes = %d/%d, want 1/0", h.commits.attempts, h.store.writeMergeCalls)
	}
}

// TestMergeRefusesWhenTheCommitGateRefuses: the gate ladder runs inside the
// commit transaction (states.Service.RequireCommitGate). A refusal there is the
// merge's refusal, and nothing of the merge is recorded.
func TestMergeRefusesWhenTheCommitGateRefuses(t *testing.T) {
	h := newHarness(t)
	h.commits.failErr = errors.New("commit gate refused the accepted state")
	_, err := h.merge(t)
	if err == nil {
		t.Fatalf("a merge whose commit was refused reported success")
	}
	if h.store.writeMergeCalls != 0 || len(h.store.gitSteps) != 0 {
		t.Fatalf("the refused commit was recorded: merges=%d git steps=%d", h.store.writeMergeCalls, len(h.store.gitSteps))
	}
}

// TestMergeLeavesTheGitStepPendingWithoutAnAdapter: with no provider merge
// adapter wired (T0409 owns it) the database truth still commits and the saga
// records that the Git half has NOT happened, with the reason. The record says
// "pending"; it never claims a ref update.
func TestMergeLeavesTheGitStepPendingWithoutAnAdapter(t *testing.T) {
	h := newHarness(t) // no Git adapter wired
	res, err := h.merge(t)
	if err != nil {
		t.Fatalf("merge without a Git adapter = %v", err)
	}
	if res.Merge.GitState != domain.GitStatePending || res.Merge.GitSHA != nil {
		t.Fatalf("merge row = state %q sha %v, want pending with no sha", res.Merge.GitState, res.Merge.GitSHA)
	}
	if len(h.store.gitSteps) != 1 {
		t.Fatalf("git steps = %+v, want exactly one recorded attempt", h.store.gitSteps)
	}
	step := h.store.gitSteps[0]
	if step.State != domain.GitStatePending || step.Error == "" || step.MergeID != res.Merge.ID {
		t.Fatalf("recorded git step = %+v, want pending with a reason on the merge row", step)
	}
	if res.Merge.GitAttempts != 1 {
		t.Fatalf("git attempts = %d, want the pending step recorded once", res.Merge.GitAttempts)
	}
}

// TestMergeRecordsAFailedGitStepWhenTheProviderFails: a provider error is a
// failed saga step with its reason, and the database truth is not rolled back
// over it.
func TestMergeRecordsAFailedGitStepWhenTheProviderFails(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.git = &fakeGit{err: errors.New("gitea: 502 bad gateway")}
	})
	res, err := h.merge(t)
	if err != nil {
		t.Fatalf("a merge whose provider call failed = %v", err)
	}
	if res.Merge.GitState != domain.GitStateFailed || res.Merge.GitError == "" {
		t.Fatalf("merge row = state %q error %q, want failed with the provider's reason",
			res.Merge.GitState, res.Merge.GitError)
	}
	if h.store.markPRCalls != 1 || h.commits.attempts != 1 {
		t.Fatalf("the committed database truth was undone: pr=%d commits=%d", h.store.markPRCalls, h.commits.attempts)
	}
}

// TestMergeRecordsAFailedGitStepWhenTheGuardRefuses: the platform layer judges
// the update the provider performed. An update main's guard refuses is a FAILED
// saga step, never a legitimate merge — this is "main may only be advanced by
// the merge service identity" applied to the merge service itself.
func TestMergeRecordsAFailedGitStepWhenTheGuardRefuses(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.git = &fakeGit{sha: mergeSHA, actor: "some-other-identity"}
	})
	res, err := h.merge(t)
	if err != nil {
		t.Fatalf("merge whose Git update the guard refuses = %v", err)
	}
	if len(h.store.gitSteps) != 1 {
		t.Fatalf("git steps = %+v, want one", h.store.gitSteps)
	}
	step := h.store.gitSteps[0]
	if step.State != domain.GitStateFailed || step.Error == "" {
		t.Fatalf("recorded git step = %+v, want failed with the guard's refusal", step)
	}
	if res.Merge.GitState != domain.GitStateFailed || res.Merge.GitSHA != nil {
		t.Fatalf("merge row = state %q sha %v, want failed with no sha (the ref did not legitimately move)",
			res.Merge.GitState, res.Merge.GitSHA)
	}
}

// TestMergeRefusesToAdvanceMainWithNoMergeIdentity: the guard fails closed on an
// empty MergeService, so a platform wired without the merge identity records a
// failed step instead of treating an anonymous main update as legitimate.
func TestMergeRefusesToAdvanceMainWithNoMergeIdentity(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.git = &fakeGit{sha: mergeSHA, actor: mergeService}
	})
	h.svc = NewService(Deps{
		Store: h.store, Diffs: h.diffs, Plans: h.plans, Commits: h.commits,
		Objects: h.objects, Relations: h.relations, Projects: h.projects, Authz: h.authz,
		Checks: h.checks, Policies: h.policies, Rules: h.rules, Events: h.events,
		Git: h.git, // no RefGuard merge identity configured
	})
	res, err := h.merge(t)
	if err != nil {
		t.Fatalf("merge with an unconfigured ref guard = %v", err)
	}
	if res.Merge.GitState != domain.GitStateFailed || res.Merge.GitSHA != nil {
		t.Fatalf("merge row = state %q sha %v, want failed: an empty guard must fail closed",
			res.Merge.GitState, res.Merge.GitSHA)
	}
}

// TestMergeNamesTheProviderRefPair: the provider-side merge needs BOTH refs —
// the target it advances and the branch it merges from — and the saga hands
// the adapter the pair the plan was computed over, pinned to the two states'
// commits. A merge that named only the target could not open, let alone
// merge, the provider's pull request.
func TestMergeNamesTheProviderRefPair(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.git = &fakeGit{sha: mergeSHA, actor: mergeService}
	})
	if _, err := h.merge(t); err != nil {
		t.Fatalf("merge = %v", err)
	}
	if len(h.git.requests) != 1 {
		t.Fatalf("provider merge calls = %d, want 1", len(h.git.requests))
	}
	got := h.git.requests[0]
	if got.TargetRef != "refs/heads/main" {
		t.Errorf("target ref = %q, want main's ref (the ref the merge advances)", got.TargetRef)
	}
	if got.SourceRef != "refs/heads/feature" {
		t.Errorf("source ref = %q, want the source branch's ref (the PR's head)", got.SourceRef)
	}
	// Both sides of the harness triple sit on the same recorded commit, so
	// the pins are that commit on both sides — the point is that the pins come
	// from the PLAN's states, not from the branch rows' current heads.
	if got.SourceSHA != mainSHA || got.TargetSHA != mainSHA {
		t.Errorf("pins = source %q target %q, want the planned states' commits (%q)",
			got.SourceSHA, got.TargetSHA, mainSHA)
	}
}

// TestMergeRetryNamesTheSameRefPair: the retry path (a client repeating its
// Idempotency-Key after a failed Git step) rebuilds the request from the merge
// ROW alone — there are no branch rows in hand at that point. It must name the
// same pair the first attempt did, or a retry would advance a different ref
// than the one the humans decided.
func TestMergeRetryNamesTheSameRefPair(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.git = &fakeGit{err: errors.New("gitea: 502 bad gateway")}
	})
	key := "merge-key-retry-0001"
	first, err := h.svc.Merge(context.Background(), domain.User{ID: actorID},
		Input{ProjectID: projID, Number: 7, IdempotencyKey: &key})
	if err != nil {
		t.Fatalf("first merge = %v", err)
	}
	if first.Merge.GitState != domain.GitStateFailed {
		t.Fatalf("first merge git state = %q, want failed so the retry has work to do", first.Merge.GitState)
	}
	// The ledger now holds the merge the first call created, Git step and all
	// (the real store's LookupMergeCreation returns exactly this row).
	stored := h.store.mergeRow
	h.store.ledger = &stored
	// The provider is reachable again on the retry — the same adapter
	// instance the service was wired with, since that is what a real
	// redeployment-free retry uses.
	h.git.err, h.git.sha, h.git.actor = nil, mergeSHA, mergeService

	res, err := h.svc.Merge(context.Background(), domain.User{ID: actorID},
		Input{ProjectID: projID, Number: 7, IdempotencyKey: &key})
	if err != nil {
		t.Fatalf("retry = %v", err)
	}
	if !res.Replayed {
		t.Fatalf("the retry was not reported as a replay of %s", key)
	}
	if len(h.git.requests) != 2 {
		t.Fatalf("provider calls = %d, want the first attempt plus the retry", len(h.git.requests))
	}
	got := h.git.requests[1]
	if got.TargetRef != "refs/heads/main" || got.SourceRef != "refs/heads/feature" {
		t.Fatalf("retry ref pair = %q -> %q, want the pair the first attempt named (refs/heads/feature -> refs/heads/main)",
			got.SourceRef, got.TargetRef)
	}
	if res.Merge.GitState != domain.GitStateUpdated || res.Merge.GitSHA == nil || *res.Merge.GitSHA != mergeSHA {
		t.Fatalf("merge row after the retry = state %q sha %v, want the completed step", res.Merge.GitState, res.Merge.GitSHA)
	}
	if h.store.writeMergeCalls != 1 {
		t.Fatalf("write merge calls = %d, want the retry to have written nothing new", h.store.writeMergeCalls)
	}
}

// TestMergeRecordsTheGitStepWhenTheGuardAllowsIt is the same step with the
// merge-service identity: the saga completes with the provider's sha.
func TestMergeRecordsTheGitStepWhenTheGuardAllowsIt(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.git = &fakeGit{sha: mergeSHA, actor: mergeService}
	})
	res, err := h.merge(t)
	if err != nil {
		t.Fatalf("merge with a well-behaved Git adapter = %v", err)
	}
	if res.Merge.GitState != domain.GitStateUpdated {
		t.Fatalf("merge row state = %q, want updated", res.Merge.GitState)
	}
	if res.Merge.GitSHA == nil || *res.Merge.GitSHA != mergeSHA {
		t.Fatalf("merge row sha = %v, want %s", res.Merge.GitSHA, mergeSHA)
	}
	if len(h.store.gitSteps) != 1 || h.store.gitSteps[0].State != domain.GitStateUpdated {
		t.Fatalf("recorded git steps = %+v, want exactly one updated step", h.store.gitSteps)
	}
}

// TestMergeClosesTheSourceBranchOnlyWhenItStillIsTheAcceptedState: closing a
// branch freezes its history, so a branch that moved past the state this merge
// accepted stays active (docs/43) — and the result reports which of the two
// happened instead of choosing silently.
func TestMergeClosesTheSourceBranchOnlyWhenItStillIsTheAcceptedState(t *testing.T) {
	h := newHarness(t)
	res, err := h.merge(t)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if !res.SourceBranchClosed || h.store.markBranchCalls != 1 {
		t.Fatalf("source branch close = %v (%d calls), want the branch closed",
			res.SourceBranchClosed, h.store.markBranchCalls)
	}

	moved := newHarness(t, func(h *harness) {
		scope := h.store.currentScope(resultState)
		scope.Source.BaseStateID = strptr(otherStateID)
		h.store.lockedScope = scope
	})
	res, err = moved.merge(t)
	if err != nil {
		t.Fatalf("merge of a branch that moved on: %v", err)
	}
	if res.SourceBranchClosed || moved.store.markBranchCalls != 0 {
		t.Fatalf("a source branch that moved past the accepted state was closed (%v, %d calls)",
			res.SourceBranchClosed, moved.store.markBranchCalls)
	}
}

// TestMergeSkipsTheCloseWhenTheSourceIsMain: main is not a research proposal's
// path, so accepting work into it never closes it (docs/43's active → merged is
// a property of the branch a PR proposes FROM).
func TestMergeSkipsTheCloseWhenTheSourceIsMain(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.store.source = h.store.target })
	if _, err := h.merge(t); err != nil {
		t.Fatalf("merge whose source branch is main = %v", err)
	}
	if h.store.markBranchCalls != 0 {
		t.Fatalf("the merge closed main: %d calls", h.store.markBranchCalls)
	}
}

// TestMergeCarriesTheGateOfTheTargetBranch: a merge into main is held to the
// main gate (docs/09 §3, frozen main), a merge into another research branch to
// the PR gate. The gate decides whether the accepted state is even legal, so it
// cannot be picked by the caller.
func TestMergeCarriesTheGateOfTheTargetBranch(t *testing.T) {
	main := newHarness(t)
	if _, err := main.merge(t); err != nil {
		t.Fatalf("merge into main: %v", err)
	}
	if main.commits.gate != gateFor(main.store.target) {
		t.Fatalf("main merge gate = %q, want the target branch's", main.commits.gate)
	}

	side := newHarness(t, func(h *harness) {
		// A PR into another research branch: the target is not main.
		h.store.target.Name = "release-candidate"
		h.store.target.GitRef = "refs/heads/release-candidate"
		h.store.pr.TargetBranchID = mainBranchID
	})
	if _, err := side.merge(t); err != nil {
		t.Fatalf("merge into a research branch: %v", err)
	}
	if side.commits.gate == main.commits.gate {
		t.Fatalf("a merge into a non-main branch used the main gate")
	}
}

// The fakes must keep implementing the ports they stand for: a port that grows a
// method fails to compile here rather than at some call site's wiring.
var (
	_ StorePort      = (*fakeStore)(nil)
	_ DiffPort       = (*fakeDiffs)(nil)
	_ PlanPort       = (*fakePlans)(nil)
	_ CommitPort     = (*fakeCommits)(nil)
	_ ObjectWriter   = (*fakeObjects)(nil)
	_ RelationWriter = (*fakeRelations)(nil)
	_ ProjectGate    = (*fakeProjects)(nil)
	_ Authz          = (*fakeAuthz)(nil)
	_ GitMerger      = (*fakeGit)(nil)
)
