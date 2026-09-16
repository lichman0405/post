package merge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/lichman0405/post/internal/application/diffs"
	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/relations"
	"github.com/lichman0405/post/internal/application/sciobjects"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/integrity"
	"github.com/lichman0405/post/internal/rsg/manifest"
	rsgmerge "github.com/lichman0405/post/internal/rsg/merge"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// defaultAttempts bounds the optimistic retry: the plan is computed from the
// three states (which the write transaction re-verifies), while the version
// counters the operation summary names are read optimistically. A counter
// that moved between the read and the write rolls the whole merge back and
// is simply re-read. Two attempts cover a single concurrent writer, which is
// what this race is; a third is cheap insurance against a busy object.
const defaultAttempts = 3

// Deps wires the merge service. Every port is required except Git: with no
// provider merge adapter the database truth still commits and the saga
// records that the Git half has not happened (see the package doc).
type Deps struct {
	Store     StorePort
	Diffs     DiffPort
	Plans     PlanPort
	Commits   CommitPort
	Objects   ObjectWriter
	Relations RelationWriter
	Projects  ProjectGate
	Authz     Authz
	// Checks re-runs the PR's integrity review server-side (docs/22 §7).
	// Nil is NOT a legitimate wiring for the command: a merge that cannot
	// re-run its validation must refuse, not skip it (see requireIntegrity).
	Checks IntegrityChecker
	// Policies reads the policy in force and Rules evaluates one typed
	// question against it. Both are required for the same reason.
	Policies PolicyReader
	Rules    RuleEvaluator
	// Events is the outbox recorder. Required: the merge's domain event is
	// part of the merge, not an afterthought.
	Events EventRecorder
	// Git performs the provider-side PR merge (T0409's adapter). Nil is a
	// legitimate wiring: the saga then leaves the step pending and
	// retryable.
	Git GitMerger
	// RefGuard is the platform policy over the target ref update. The zero
	// value fails closed (no identity may advance main).
	RefGuard RefGuard
	// MaxAttempts overrides defaultAttempts when positive.
	MaxAttempts int
}

// Service orchestrates one Research PR merge: it plans the merge (the pure
// engine, internal/rsg/merge), runs the plan inside one state commit, records
// the merge and its carried conflicts in that same transaction, and drives
// the Git saga that follows.
//
// It owns the input validation and the authorization (ActionMergeMain), and
// it refuses to merge anything a human has not decided: the plan it executes
// is the engine's, and the engine blocks on an undecided conflict rather
// than choosing.
type Service struct {
	store       StorePort
	diffs       DiffPort
	plans       PlanPort
	commits     CommitPort
	objects     ObjectWriter
	relations   RelationWriter
	projects    ProjectGate
	authz       Authz
	checks      IntegrityChecker
	policies    PolicyReader
	rules       RuleEvaluator
	events      EventRecorder
	git         GitMerger
	guard       RefGuard
	maxAttempts int
}

// NewService wires the service.
//
// Every optional port is normalized first (see absentToNil): a port that was not
// wired is nil here, whichever of the two spellings of "nothing" the
// composition root used. The distinction matters for exactly one field — Git,
// the one port a deployment may legitimately leave out — and a typed nil
// pointer there is a boot-time panic inside the merge transaction rather than
// a recorded unfinished step.
func NewService(d Deps) *Service {
	attempts := d.MaxAttempts
	if attempts <= 0 {
		attempts = defaultAttempts
	}
	return &Service{
		store:       absentToNil(d.Store),
		diffs:       absentToNil(d.Diffs),
		plans:       absentToNil(d.Plans),
		commits:     absentToNil(d.Commits),
		objects:     absentToNil(d.Objects),
		relations:   absentToNil(d.Relations),
		projects:    absentToNil(d.Projects),
		authz:       absentToNil(d.Authz),
		checks:      absentToNil(d.Checks),
		policies:    absentToNil(d.Policies),
		rules:       absentToNil(d.Rules),
		events:      absentToNil(d.Events),
		git:         absentToNil(d.Git),
		guard:       d.RefGuard,
		maxAttempts: attempts,
	}
}

// absentToNil turns a port that carries no implementation into a nil
// interface.
//
// The case it exists for is the TYPED nil. `Deps.Git` is an interface, and a
// nil pointer assigned to it is NOT a nil interface — it holds a type with a
// nil value:
//
//	var bridge *mergeGitBridge       // nil
//	merge.Deps{Git: bridge}          // Git != nil; calling it panics
//
// The service decides what to do from a nil check (an unwired Git adapter
// makes the saga record its step as not-done instead of claiming the ref
// moved), so a typed nil used to slip past the check and panic on the first
// merge — after PostgreSQL had already accepted the state. Normalizing here
// makes "not wired" one thing, however the composition root spells it.
func absentToNil[T any](port T) T {
	if portIsNil(port) {
		var zero T
		return zero
	}
	return port
}

// portIsNil reports whether a port value holds no implementation. The
// reflect kinds are the ones a nil pointer can be hidden in; a non-pointer
// port (the RefGuard value type) is never "absent".
func portIsNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Func, reflect.Slice, reflect.Chan, reflect.Interface, reflect.UnsafePointer:
		return rv.IsNil()
	}
	return false
}

// Input names one merge request.
type Input struct {
	ProjectID string
	// Number is the per-project PR number.
	Number int64
	// Message is the state commit's message. Empty falls back to a
	// generated one naming the PR and the branch pair.
	Message string
	// IdempotencyKey is the request's Idempotency-Key (docs/22 §3, which
	// lists merge among the governed commands), nil when the request
	// carried none. A key that already created a merge replays it: the
	// same call returns the merge the first one produced, forever, without
	// re-planning, re-gating or writing anything.
	IdempotencyKey *string
}

// WrittenVersion is one version row the merge created in the accepted state:
// the plan's change it realizes and the row that carries the content.
type WrittenVersion struct {
	TargetKind domain.ConflictResolutionTargetKind
	TargetID   string
	VersionID  string
	VersionNo  int
}

// Result is one completed merge.
type Result struct {
	// Number is the merged PR's per-project number.
	Number int64
	// Merge is the merge record, as stored (including the Git saga's outcome
	// after the step ran).
	Merge domain.SemanticMerge
	// State and Commit are the accepted state this merge committed and the
	// transition that recorded it.
	State  domain.ProjectState
	Commit domain.StateCommit
	// Plan is the plan that was executed; Merge.Plan is its canonical bytes.
	Plan *rsgmerge.Plan
	// Written lists the version rows the merge created, in plan order.
	Written []WrittenVersion
	// SourceBranchClosed reports whether the merge also closed the source
	// branch (docs/43: active → merged). It is false when the source branch
	// had moved past the state this merge accepted — see closeSourceBranch.
	SourceBranchClosed bool
	// Replayed reports that this result is a replay: the Idempotency-Key
	// already created this merge, so nothing was planned, gated or written
	// on this call and State/Commit/Plan/Written are empty. Merge is the
	// stored record, and its ResultStateID/PlanDigest/GitState answer what
	// the first call produced.
	Replayed bool
}

// Merge executes one Research PR merge: plan, gate, commit, then the Git
// saga.
//
// The database truth is one transaction — the accepted state, the merge
// record, the conflicts it carries, its Idempotency-Key ledger entry, its
// audit row, its domain event, the PR's transition to merged and the source
// branch's close either all land or none do. The Git half follows the
// commit (docs/08: Git is the repository/file truth beside PostgreSQL's
// semantic truth) and is recorded on the merge row, so a failed Git step is
// visible and retryable instead of silently divergent.
//
// The order of the refusals is the contract's:
//
//  1. authorization (ActionMergeMain) before anything is read;
//  2. the Idempotency-Key replay — a replay is a read, and running the
//     gates again would make the answer depend on state the first call
//     never decided;
//  3. the PR's own readiness and the plan (T0406: only a merge_ready PR
//     with an executable plan merges);
//  4. the policy in force (fail-closed, see requirePolicy);
//  5. the integrity review, re-run server-side (docs/22 §7).
func (s *Service) Merge(ctx context.Context, actor domain.User, in Input) (*Result, error) {
	if err := validateInput(in, actor); err != nil {
		return nil, err
	}
	if err := s.requireMerge(ctx, actor, in.ProjectID); err != nil {
		return nil, err
	}
	if replayed, err := s.replay(ctx, in); err != nil || replayed != nil {
		return replayed, err
	}
	prepared, err := s.prepare(ctx, in)
	if err != nil {
		return nil, err
	}
	if err := s.requirePolicy(ctx, actor, in.ProjectID); err != nil {
		return nil, err
	}
	if err := s.requireIntegrity(ctx, in); err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt < s.maxAttempts; attempt++ {
		res, err := s.commit(ctx, actor, prepared)
		if err == nil {
			s.runGitStep(ctx, res)
			return res, nil
		}
		var moved *versionHeadsMovedError
		if errors.As(err, &moved) {
			// The states did not move (that would be *StaleMergeError and is
			// not retried); only a version counter did, which the next pass
			// re-reads. Nothing was written: the transaction rolled back.
			lastErr = err
			continue
		}
		return nil, err
	}
	return nil, fmt.Errorf("%w: the merge's version heads kept moving (%v); nothing was written, retry the merge",
		ErrStore, lastErr)
}

// replay answers a repeated Idempotency-Key from the ledger. It returns nil
// when the key is unused (or absent) — the caller then proceeds with a
// fresh merge.
//
// A replay runs BEFORE the gates on purpose: the merge the key produced is
// committed history, and re-deciding it against today's policy, today's
// integrity review or today's branch heads would make the same call answer
// differently over time. That is exactly what docs/22 §3 forbids.
//
// The one thing a replay does do is finish the Git half when the first
// attempt could not: the merge row records the provider step's outcome, and
// a client retrying a request whose provider step failed is the retry the
// saga was built for. Nothing is written when the step already finished
// (`updated` is terminal, migration 00069).
func (s *Service) replay(ctx context.Context, in Input) (*Result, error) {
	if in.IdempotencyKey == nil {
		return nil, nil
	}
	stored, err := s.store.LookupMergeCreation(ctx, in.ProjectID, *in.IdempotencyKey)
	if err != nil {
		return nil, fmt.Errorf("%w: read the merge ledger: %v", ErrStore, err)
	}
	if stored == nil {
		return nil, nil
	}
	res := &Result{Number: in.Number, Merge: *stored, Replayed: true}
	if stored.GitState != domain.GitStateUpdated {
		s.retryGitStep(ctx, res)
	}
	return res, nil
}

// retryGitStep re-drives the Git half of a stored merge from the plan the
// merge itself recorded. The plan's source/target refs are the triple the
// human decided, so the retry advances exactly the refs the first attempt
// planned and never a newer pair. A plan that cannot be decoded leaves the
// row as it is: the saga stays visibly unfinished, which is better than
// re-driving a step whose inputs are unknown.
func (s *Service) retryGitStep(ctx context.Context, res *Result) {
	var plan rsgmerge.Plan
	if len(res.Merge.Plan) == 0 {
		return
	}
	if err := json.Unmarshal(res.Merge.Plan, &plan); err != nil {
		return
	}
	res.Plan = &plan
	s.runGitStep(ctx, res)
}

// requirePolicy evaluates the governance policy in force for the project.
// It is fail-closed on every input (see PolicyRefusedError): an unreadable
// policy refuses, an absent `main_protected` rule refuses (silence is not a
// permission — docs/09 §3 protects main structurally, so a document that
// does not mention it cannot waive that), and a rule set to false refuses
// too. Only an explicit true proceeds.
func (s *Service) requirePolicy(ctx context.Context, actor domain.User, projectID string) error {
	if s.policies == nil || s.rules == nil {
		return fmt.Errorf("%w: merge service not fully wired", ErrStore)
	}
	refuse := func(found, value bool, reason string, cause error) error {
		return &PolicyRefusedError{
			Rule: domain.RuleMainProtected, Found: found, Bool: value,
			Reason: reason, Err: cause,
		}
	}
	effective, err := s.policies.EffectivePolicy(ctx, actor, projectID)
	if err != nil {
		return refuse(false, false, "the policy in force could not be read, and an unreadable policy is not a permissive one", err)
	}
	decision, err := s.rules.Evaluate(ctx, effective.Effective, policy.Query{Rule: domain.RuleMainProtected})
	if err != nil {
		return refuse(false, false, "the policy in force cannot be evaluated, and an unevaluable policy is not a permissive one", err)
	}
	if !decision.Found {
		return refuse(false, false,
			"the policy in force does not set "+domain.RuleMainProtected+
				"; an absent rule is not a permission — main is protected by the platform (docs/09 §3), and a policy that is silent about it cannot unprotect it", nil)
	}
	if !decision.Bool {
		return refuse(true, false,
			"the policy in force sets "+domain.RuleMainProtected+
				" to false; no policy in this build may waive the frozen-main guarantee, so the merge refuses rather than obeying", nil)
	}
	return nil
}

// requireIntegrity re-runs the PR's integrity review server-side and
// refuses on any BLOCKING failure (docs/22 §7: a command never trusts a
// client-supplied precheck; internal/rsg/integrity assigns the refusal to
// the merge governance). Warnings are reported by the review page and do
// not stop the merge — the severity is the check declaration's, not this
// function's.
//
// A check that cannot run is a refusal, not a pass: an unreviewable PR is
// not a reviewed one.
func (s *Service) requireIntegrity(ctx context.Context, in Input) error {
	if s.checks == nil {
		return fmt.Errorf("%w: merge service not fully wired", ErrStore)
	}
	report, err := s.checks.CheckPullRequest(ctx, in.ProjectID, in.Number)
	if err != nil {
		return err
	}
	for _, failure := range report.Failures() {
		if failure.Severity == integrity.SeverityBlocking {
			return &GateRefused{Report: report}
		}
	}
	return nil
}

// prepared carries everything the plan is, plus the facts the write
// transaction re-verifies. It is computed once: the plan is a function of the
// three pinned states, and a state that moved makes the plan invalid (a
// different triple, which a human has not decided) rather than recomputable.
type prepared struct {
	input  Input
	plan   *rsgmerge.Plan
	planJS []byte
	digest string

	pr            domain.PullRequest
	sourceBranch  domain.Branch
	targetBranch  domain.Branch
	baseStateID   string
	sourceStateID string
	targetStateID string
}

// prepare reads the merge's inputs and computes the plan. It refuses a PR
// that is not merge_ready, a branch that is not active and a plan the engine
// blocked — the last with *BlockedError, which carries the plan.
func (s *Service) prepare(ctx context.Context, in Input) (*prepared, error) {
	if s.store == nil || s.diffs == nil || s.plans == nil || s.commits == nil ||
		s.objects == nil || s.relations == nil {
		return nil, fmt.Errorf("%w: merge service not fully wired", ErrStore)
	}
	pr, err := s.store.GetPullRequest(ctx, in.ProjectID, in.Number)
	if err != nil {
		if errors.Is(err, ErrPullRequestNotFound) {
			return nil, ErrPullRequestNotFound
		}
		return nil, fmt.Errorf("%w: read pull request: %v", ErrStore, err)
	}
	// docs/43: `merged` is reachable only from merge_ready, and the merge
	// engine is the transition that lands it. A PR in any other state is
	// refused here, before anything is read or decided.
	if pr.State != domain.PullRequestStateMergeReady {
		return nil, &NotMergeableError{Number: pr.Number, State: pr.State}
	}
	source, err := s.branch(ctx, in.ProjectID, pr.SourceBranchID)
	if err != nil {
		return nil, err
	}
	target, err := s.branch(ctx, in.ProjectID, pr.TargetBranchID)
	if err != nil {
		return nil, err
	}
	// A closed research path neither merges nor accepts a merge (docs/43).
	for _, b := range []domain.Branch{source, target} {
		if b.Lifecycle != domain.BranchLifecycleActive {
			return nil, &BranchNotActiveError{BranchID: b.ID}
		}
	}
	if target.BaseStateID == nil {
		return nil, fmt.Errorf("%w: target branch %s has no head state to advance", ErrValidation, target.ID)
	}
	inputs, err := s.diffs.Inputs(ctx, diffs.Params{
		ProjectID:     in.ProjectID,
		BaseStateID:   pr.BaseStateID,
		SourceStateID: pr.ProposedStateID,
		TargetStateID: *target.BaseStateID,
	})
	if err != nil {
		return nil, err
	}
	recorded, err := s.plans.Plan(ctx, in.ProjectID, pr.BaseStateID, pr.ProposedStateID, *target.BaseStateID)
	if err != nil {
		return nil, err
	}
	plan, err := rsgmerge.Merge(rsgmerge.Inputs{
		ProjectID: in.ProjectID,
		Diff:      inputs,
		Decisions: decisionsOf(recorded),
		// docs/09 §9: the engine withholds a private source's changes from a
		// public target rather than publishing them. The publication itself
		// is the Publication Gate's (T0704/T0705).
		SourceVisibility: source.Visibility,
		TargetVisibility: target.Visibility,
	})
	if err != nil {
		return nil, fmt.Errorf("merge: plan: %w", err)
	}
	if !plan.Executable {
		return nil, &BlockedError{Plan: plan}
	}
	planJS, err := plan.CanonicalJSON()
	if err != nil {
		return nil, fmt.Errorf("%w: render the plan: %v", ErrStore, err)
	}
	sum := sha256.Sum256(planJS)
	return &prepared{
		input:         in,
		plan:          plan,
		planJS:        planJS,
		digest:        hex.EncodeToString(sum[:]),
		pr:            pr,
		sourceBranch:  source,
		targetBranch:  target,
		baseStateID:   pr.BaseStateID,
		sourceStateID: pr.ProposedStateID,
		targetStateID: *target.BaseStateID,
	}, nil
}

// commit runs one attempt: read the version heads, build the operation
// summary, and run the plan inside one state commit.
func (s *Service) commit(ctx context.Context, actor domain.User, p *prepared) (*Result, error) {
	objectIDs, relationIDs := materializedContainers(p.plan)
	heads, err := s.store.VersionHeads(ctx, objectIDs, relationIDs)
	if err != nil {
		return nil, fmt.Errorf("%w: read version heads: %v", ErrStore, err)
	}
	operations, err := operationsFor(p.plan, heads)
	if err != nil {
		return nil, err
	}
	var (
		written      []WrittenVersion
		mergeRow     domain.SemanticMerge
		branchClosed bool
	)
	write := func(ctx context.Context, tx states.Transaction, stateID string) error {
		// The fence (T0402 review minor #2): everything the plan was
		// computed from is re-read ON THIS TRANSACTION and under a row
		// lock, so a concurrent merge cannot commit between the read and
		// this write. A difference is a refusal, not a re-plan — the human
		// decided THIS triple.
		scope, err := s.store.LockMergeScope(ctx, tx, LockScopeParams{
			ProjectID:      p.input.ProjectID,
			PRNumber:       p.input.Number,
			SourceBranchID: p.sourceBranch.ID,
			TargetBranchID: p.targetBranch.ID,
		})
		if err != nil {
			return err
		}
		if err := verifyScope(scope, p, stateID); err != nil {
			return err
		}
		locked, err := s.store.LockVersionHeads(ctx, tx, objectIDs, relationIDs)
		if err != nil {
			return err
		}
		if err := heads.Equal(locked); err != nil {
			return err // *versionHeadsMovedError → the caller retries
		}
		written, err = s.materialize(ctx, tx, stateID, p, heads, actor.ID)
		if err != nil {
			return err
		}
		mergeRow, err = s.store.WriteMerge(ctx, tx, s.mergeParams(p, stateID, actor.ID, written))
		if err != nil {
			return err
		}
		// The event is recorded on this transaction, after the merge row
		// exists: nothing is committed unless the event is written with it
		// (docs/53).
		if err := s.recordMergeEvent(ctx, tx, p, actor.ID, mergeRow.ID, stateID); err != nil {
			return err
		}
		if err := s.store.MarkPullRequestMerged(ctx, tx, p.input.ProjectID, p.pr.ID); err != nil {
			return err
		}
		branchClosed, err = s.closeSourceBranch(ctx, tx, p, scope)
		return err
	}
	message := p.input.Message
	if strings.TrimSpace(message) == "" {
		message = fmt.Sprintf("merge PR #%d (%s → %s)", p.pr.Number, p.sourceBranch.Name, p.targetBranch.Name)
	}
	targetHead := p.targetStateID
	state, commit, err := s.commits.Commit(ctx, states.CommitParams{
		ProjectID:       p.input.ProjectID,
		BranchID:        p.targetBranch.ID,
		ActorID:         actor.ID,
		Via:             domain.ViaAPI,
		Message:         message,
		Operations:      operations,
		BaseStateID:     &targetHead,
		ManifestVersion: ManifestVersion,
		// The merge into main is held to the main gate — the strictest
		// commit gate (docs/09 §3: frozen main holds accepted research
		// state, provenance completeness included). A merge into another
		// research branch is held to the PR gate, which is the standard a
		// reviewed proposal has met.
		Gate: gateFor(p.targetBranch),
		// This transition IS the Research PR merge — the one path docs/09
		// §3 leaves open onto a frozen main (T0601). The declaration is
		// what keeps the freeze from forbidding evolution: the same commit
		// would be refused with MAIN_FROZEN_DIRECT_WRITE_FORBIDDEN if it
		// arrived through any other path, and only this package sets it.
		ResearchPRMerge: true,
	}, write)
	if err != nil {
		return nil, unwrapWriteError(err)
	}
	return &Result{
		Number:             p.input.Number,
		Merge:              mergeRow,
		State:              state,
		Commit:             commit,
		Plan:               p.plan,
		Written:            written,
		SourceBranchClosed: branchClosed,
	}, nil
}

// gateFor picks the commit gate the merge is held to.
func gateFor(target domain.Branch) rsgvalidation.Gate {
	if target.IsMain() {
		return rsgvalidation.GateMain
	}
	return rsgvalidation.GatePR
}

// closeSourceBranch closes the source branch as merged when the state this
// merge accepted IS the branch's head — its research path has arrived in the
// accepted state, and docs/43's active → merged transition is exactly that.
//
// When the branch has moved past the accepted state, it stays active: closing
// it would make history immutable over work that never merged, and the
// branch is free to propose its newer work as another PR. The merge reports
// which of the two happened (Result.SourceBranchClosed) instead of deciding
// silently.
//
// A failure to close the branch the merge DID accept fails the merge: the
// branch's close and the acceptance it records are one atomic fact.
func (s *Service) closeSourceBranch(ctx context.Context, tx states.Transaction, p *prepared, scope MergeScope) (bool, error) {
	if p.sourceBranch.IsMain() {
		return false, nil
	}
	// The locked re-read is the source branch's truth for this transaction.
	if scope.Source.BaseStateID == nil || *scope.Source.BaseStateID != p.sourceStateID {
		return false, nil
	}
	if err := s.store.MarkBranchMerged(ctx, tx, p.sourceBranch.ID); err != nil {
		return false, err
	}
	return true, nil
}

// materialize writes the plan's materialized changes into the accepted state,
// in the plan's order (objects first: a relation's endpoint may have to be
// re-pinned onto a version this merge just wrote). Each change copies the
// SOURCE version's content verbatim — the engine never synthesizes a third
// value, and this is where that promise is kept.
func (s *Service) materialize(ctx context.Context, tx states.Transaction, stateID string, p *prepared, heads VersionHeads, actorID string) ([]WrittenVersion, error) {
	objects := objectVersionsOf(p.plan)
	relatns := relationVersionsOf(p.plan)
	branchID := p.targetBranch.ID
	out := make([]WrittenVersion, 0, len(p.plan.Materialized()))
	writtenByObject := make(map[string]string)
	for _, c := range p.plan.Materialized() {
		switch c.TargetKind {
		case domain.ConflictResolutionTargetObject:
			src, ok := objects[c.TargetID]
			if !ok {
				return nil, fmt.Errorf("%w: the plan materializes object %s but its source version is not in the diff", ErrStore, c.TargetID)
			}
			v, err := s.objects.CreateVersionInTx(ctx, tx, c.TargetID, heads.Objects[c.TargetID], sciobjects.VersionParams{
				StateID:        stateID,
				BranchID:       &branchID,
				SchemaID:       src.SchemaRef.ID,
				SchemaVersion:  src.SchemaRef.Version,
				Title:          src.Title,
				LifecycleState: domain.LifecycleState(src.LifecycleState),
				Payload:        src.Payload,
				// The rights policy travels with the content: the merge
				// never widens or drops it (docs/12 §3).
				VisibilityPolicyID: src.VisibilityPolicyID,
				CreatedBy:          actorID,
			})
			if err != nil {
				return nil, err
			}
			writtenByObject[c.TargetID] = v.ID
			out = append(out, WrittenVersion{
				TargetKind: c.TargetKind, TargetID: c.TargetID,
				VersionID: v.ID, VersionNo: v.VersionNo,
			})
		case domain.ConflictResolutionTargetRelation:
			src, ok := relatns[c.TargetID]
			if !ok {
				return nil, fmt.Errorf("%w: the plan materializes relation %s but its source version is not in the diff", ErrStore, c.TargetID)
			}
			sourcePin, err := repin(c, src.SourceObjectVersionID, writtenByObject)
			if err != nil {
				return nil, err
			}
			targetPin, err := repin(c, src.TargetObjectVersionID, writtenByObject)
			if err != nil {
				return nil, err
			}
			v, err := s.relations.AppendRelationVersionInTx(ctx, tx, c.TargetID, heads.Relations[c.TargetID], relations.VersionParams{
				StateID:               stateID,
				RelationType:          src.RelationType,
				SourceObjectVersionID: sourcePin,
				TargetObjectVersionID: targetPin,
				Payload:               src.Payload,
				CreatedBy:             actorID,
			})
			if err != nil {
				return nil, err
			}
			out = append(out, WrittenVersion{
				TargetKind: c.TargetKind, TargetID: c.TargetID,
				VersionID: v.ID, VersionNo: v.VersionNo,
			})
		}
	}
	return out, nil
}

// repin resolves one relation endpoint: a pin the plan rewrote points at an
// object whose merged version this merge has just written, so the edge pins
// that version. Every other pin stays exactly as the source authored it.
func repin(c rsgmerge.Change, pin string, writtenByObject map[string]string) (string, error) {
	objectID, ok := c.EndpointRewrites[pin]
	if !ok {
		return pin, nil
	}
	versionID, ok := writtenByObject[objectID]
	if !ok {
		return "", fmt.Errorf("%w: relation %s re-pins endpoint %s onto object %s, which this merge did not write",
			ErrStore, c.TargetID, pin, objectID)
	}
	return versionID, nil
}

// mergeParams assembles the merge record.
func (s *Service) mergeParams(p *prepared, resultStateID, actorID string, written []WrittenVersion) WriteMergeParams {
	summary := p.plan.Summary
	gitRef := gitRefOf(p.targetBranch)
	return WriteMergeParams{
		ProjectID:        p.input.ProjectID,
		PullRequestID:    p.pr.ID,
		SourceBranchID:   p.sourceBranch.ID,
		TargetBranchID:   p.targetBranch.ID,
		BaseStateID:      p.baseStateID,
		SourceStateID:    p.sourceStateID,
		TargetStateID:    p.targetStateID,
		ResultStateID:    resultStateID,
		ActorID:          actorID,
		PlanVersion:      p.plan.FormatVersion,
		PlanJSON:         p.planJS,
		PlanDigest:       p.digest,
		Applied:          summary.Applied,
		KeptTarget:       summary.KeptTarget,
		Carried:          summary.Carried,
		Aborted:          summary.Aborted,
		Withheld:         summary.Withheld,
		CarriedConflicts: carriedOf(p.plan),
		GitRef:           gitRef,
		GitState:         domain.GitStatePending,
		IdempotencyKey:   p.input.IdempotencyKey,
		Audit:            s.auditEntry(p, actorID, resultStateID),
	}
}

// auditEntry renders the merge's audit row (docs/22, docs/26: governance
// actions are audited). The store appends it on the commit transaction and
// fills in the target ref, which it can only name once the insert has
// assigned the merge id.
//
// Note what the summary names and what it deliberately does not: the merge
// record id, the PR, the accepted state and the plan digest. The
// research-event vocabulary (specs/events/event-types.yaml, what subscribers
// route on) and the audit-action vocabulary (the Activity page's actions)
// are different namespaces on purpose — a governance action can exist
// without a research event and vice versa, so one is never derived from the
// other's spellings.
func (s *Service) auditEntry(p *prepared, actorID, resultStateID string) domain.AuditEntry {
	return domain.AuditEntry{
		ActorID:   actorID,
		Via:       domain.ViaSession,
		Action:    domain.ActionPullRequestMerged,
		ProjectID: p.input.ProjectID,
		AfterSummary: map[string]any{
			"pull_request_id":     p.pr.ID,
			"pull_request_number": p.pr.Number,
			"source_branch_id":    p.sourceBranch.ID,
			"target_branch_id":    p.targetBranch.ID,
			"base_state_id":       p.baseStateID,
			"source_state_id":     p.sourceStateID,
			"target_state_id":     p.targetStateID,
			"state_id":            resultStateID,
			"plan_version":        p.plan.FormatVersion,
			"plan_digest":         p.digest,
			"applied":             p.plan.Summary.Applied,
			"kept_target":         p.plan.Summary.KeptTarget,
			"carried":             p.plan.Summary.Carried,
			"aborted":             p.plan.Summary.Aborted,
			"withheld":            p.plan.Summary.Withheld,
		},
	}
}

// gitRefOf names the provider ref the saga will advance: the target branch's
// recorded ref, or the canonical refs/heads/<name> when the branch row has
// none recorded yet (the projection T0303 maintains).
func gitRefOf(target domain.Branch) *string {
	if target.GitRef != "" {
		ref := target.GitRef
		return &ref
	}
	if target.Name == "" {
		return nil
	}
	ref := "refs/heads/" + target.Name
	return &ref
}

// carriedOf renders the plan's carried conflicts as store rows.
func carriedOf(plan *rsgmerge.Plan) []CarriedConflict {
	out := make([]CarriedConflict, 0, len(plan.Carried))
	for _, c := range plan.Carried {
		out = append(out, CarriedConflict{
			TargetKind:      c.TargetKind,
			TargetID:        c.TargetID,
			Code:            c.Code,
			Category:        string(c.Category),
			Fields:          c.Fields,
			PayloadKeys:     c.PayloadKeys,
			OtherObjectID:   optional(c.OtherObjectID),
			Detail:          c.Detail,
			Decision:        c.Decision,
			DecidedBy:       c.DecidedBy,
			Note:            c.Note,
			SourceVersionID: c.SourceVersionID,
			TargetVersionID: optional(c.TargetVersionID),
		})
	}
	return out
}

// decisionsOf transcribes the recorded human decisions into the engine's
// input. Nothing is derived: the kind is what the human chose, and the
// decider is the human's id (never an agent).
func decisionsOf(recorded []domain.ConflictResolution) []rsgmerge.Decision {
	out := make([]rsgmerge.Decision, 0, len(recorded))
	for _, r := range recorded {
		var other string
		if r.OtherObjectID != nil {
			other = *r.OtherObjectID
		}
		out = append(out, rsgmerge.Decision{
			TargetKind:    r.TargetKind,
			TargetID:      r.TargetID,
			Code:          r.Code,
			Fields:        r.Fields,
			PayloadKeys:   r.PayloadKeys,
			OtherObjectID: other,
			Kind:          r.Kind,
			DecidedBy:     r.DecidedBy,
			Note:          r.Note,
		})
	}
	return out
}

// materializedContainers lists the containers the plan writes a version of,
// in plan order.
func materializedContainers(plan *rsgmerge.Plan) (objects, relations []string) {
	for _, c := range plan.Materialized() {
		switch c.TargetKind {
		case domain.ConflictResolutionTargetObject:
			objects = append(objects, c.TargetID)
		case domain.ConflictResolutionTargetRelation:
			relations = append(relations, c.TargetID)
		}
	}
	return objects, relations
}

// operationsFor builds the commit's operation summary: one op per version the
// merge writes, naming the entity and the exact version number. The gate's
// commit_linkage check matches the summary against the rows as written, in
// both directions, so this must name the versions the writes produce — which
// are the heads read above plus one.
func operationsFor(plan *rsgmerge.Plan, heads VersionHeads) ([]domain.StateOperation, error) {
	ops := make([]domain.StateOperation, 0, len(plan.Materialized()))
	for _, c := range plan.Materialized() {
		switch c.TargetKind {
		case domain.ConflictResolutionTargetObject:
			head, ok := heads.Objects[c.TargetID]
			if !ok {
				return nil, &MissingEntityError{Kind: "object", ID: c.TargetID}
			}
			ops = append(ops, domain.StateOperation{
				Kind:      domain.OperationObjectVersionCreated,
				EntityID:  c.TargetID,
				VersionNo: head + 1,
				Detail:    objectDetail(c.ObjectType),
			})
		case domain.ConflictResolutionTargetRelation:
			head, ok := heads.Relations[c.TargetID]
			if !ok {
				return nil, &MissingEntityError{Kind: "relation", ID: c.TargetID}
			}
			ops = append(ops, domain.StateOperation{
				Kind:      domain.OperationRelationVersionCreated,
				EntityID:  c.TargetID,
				VersionNo: head + 1,
			})
		}
	}
	return ops, nil
}

// objectDetail renders the operation's detail for an object version, in the
// same shape the RSG service writes (the object type).
func objectDetail(objectType string) []byte {
	if objectType == "" {
		return nil
	}
	detail, err := json.Marshal(map[string]string{"object_type": objectType})
	if err != nil {
		return nil
	}
	return detail
}

// objectVersionsOf indexes the plan's diff by object id.
func objectVersionsOf(plan *rsgmerge.Plan) map[string]manifest.ObjectVersion {
	out := make(map[string]manifest.ObjectVersion)
	if plan.Report == nil || plan.Report.Diff == nil {
		return out
	}
	for _, c := range plan.Report.Diff.ObjectChanges {
		out[c.ObjectID] = c.SourceVersion
	}
	return out
}

// relationVersionsOf indexes the plan's diff by relation id.
func relationVersionsOf(plan *rsgmerge.Plan) map[string]manifest.RelationVersion {
	out := make(map[string]manifest.RelationVersion)
	if plan.Report == nil || plan.Report.Diff == nil {
		return out
	}
	for _, c := range plan.Report.Diff.RelationChanges {
		out[c.RelationID] = c.SourceVersion
	}
	return out
}

// verifyScope compares the transaction's locked truth against the plan's
// inputs, and it runs from INSIDE the commit's write callback — that is, after
// the branch head compare-and-swap has already moved the target branch from
// the head the plan was computed over to the state this transaction is
// creating (persistence.StateStore.CommitState orders: result state row → head
// CAS → the semantic writes → the commit row).
//
// That ordering is why the target check is an equality against stateID: the
// head CAS is the fence for the planned target head — it only fires while the
// row still equals p.targetStateID, so a concurrent merge that advanced main
// in the window fails this commit before this callback ever runs — and what is
// left to check here is that the row this transaction advanced is the row the
// lock read. Locking one branch and committing on another is exactly the
// mistake the lock exists to prevent, and it would otherwise slip through.
//
// The PR and the source branch are not touched by the CAS, so their checks are
// the plan's own facts, read under the same lock.
func verifyScope(scope MergeScope, p *prepared, stateID string) error {
	switch {
	case scope.PullRequest.ID != p.pr.ID:
		return &StaleMergeError{Reason: fmt.Sprintf("pull request %s is not the one planned", scope.PullRequest.ID)}
	case scope.PullRequest.State != domain.PullRequestStateMergeReady:
		return &StaleMergeError{Reason: fmt.Sprintf("the pull request is %s, not merge_ready", scope.PullRequest.State)}
	case scope.PullRequest.ProposedStateID != p.sourceStateID:
		return &StaleMergeError{Reason: "the proposed state moved (refresh the proposal and decide again)"}
	case scope.PullRequest.BaseStateID != p.baseStateID:
		return &StaleMergeError{Reason: "the pull request's base state moved"}
	case scope.Target.ID != p.targetBranch.ID:
		return &StaleMergeError{Reason: "the target branch is not the one planned"}
	case scope.Target.BaseStateID == nil:
		return &StaleMergeError{Reason: "the target branch has no head state"}
	case *scope.Target.BaseStateID != stateID:
		return &StaleMergeError{Reason: "the target branch advanced since the plan was computed"}
	case scope.Source.Lifecycle != domain.BranchLifecycleActive:
		return &StaleMergeError{Reason: "the source branch left the active lifecycle"}
	case scope.Target.Lifecycle != domain.BranchLifecycleActive:
		return &StaleMergeError{Reason: "the target branch left the active lifecycle"}
	}
	return nil
}

// requireMerge authorizes one merge: resolve the caller's membership/role,
// then evaluate ActionMergeMain (maintainer and above, docs/12). The denial
// happens before any PR, branch or state is read, so it never discloses
// whether they exist.
func (s *Service) requireMerge(ctx context.Context, actor domain.User, projectID string) error {
	if s.projects == nil || s.authz == nil {
		return fmt.Errorf("%w: merge service not fully wired", ErrStore)
	}
	membership, err := s.projects.GetMembership(ctx, actor, projectID)
	var role *domain.ProjectRole
	switch {
	case err == nil:
		r := membership.Role
		role = &r
	case errors.Is(err, projects.ErrMemberNotFound):
		role = nil
	default:
		return err
	}
	decision, err := s.authz.Authorize(ctx, authz.Request{
		Action: authz.ActionMergeMain,
		Class:  authz.ClassOf(true, role, false),
	})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrStore, err)
	}
	if !decision.Permits() {
		return ErrForbidden
	}
	return nil
}

// branch reads one project-scoped branch, mapping the adapter's not-found
// sentinel to this package's vocabulary.
func (s *Service) branch(ctx context.Context, projectID, branchID string) (domain.Branch, error) {
	b, err := s.store.GetBranch(ctx, projectID, branchID)
	if err != nil {
		if errors.Is(err, ErrBranchNotFound) {
			return domain.Branch{}, ErrBranchNotFound
		}
		return domain.Branch{}, fmt.Errorf("%w: read branch: %v", ErrStore, err)
	}
	return b, nil
}

// unwrapWriteError keeps the outcomes the write callback decided on. The
// states service wraps a callback failure in *states.CommitWriteError and
// every other failure in its own store error; the callback's domain outcome
// (a stale plan, a refused PR, a moved version head) is what the caller
// needs, so it is unwrapped and passed through.
func unwrapWriteError(err error) error {
	var writeErr *states.CommitWriteError
	if errors.As(err, &writeErr) {
		if writeErr.Err != nil {
			return writeErr.Err
		}
	}
	return err
}

// validateInput checks the request's shape.
func validateInput(in Input, actor domain.User) error {
	if in.ProjectID == "" {
		return fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	if in.Number < 1 {
		return fmt.Errorf("%w: number must be positive", ErrValidation)
	}
	if actor.ID == "" {
		return fmt.Errorf("%w: the merging actor is required", ErrValidation)
	}
	return nil
}

// optional turns "" into nil (the store's NULL).
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// runGitStep drives the Git half of the saga: the provider-side PR merge,
// checked against the platform's ref policy, recorded on the merge row. It
// never fails the merge — the database truth is already committed — and it
// never records a legitimate update it cannot justify.
func (s *Service) runGitStep(ctx context.Context, res *Result) {
	step := GitStepParams{MergeID: res.Merge.ID, Ref: res.Merge.GitRef, State: domain.GitStatePending}
	// The provider-side merge names TWO refs: the target it advances and the
	// source branch it merges from. The target ref is recorded on the merge
	// row (resolved from the locked branch row when the merge was planned);
	// the source ref is resolved here, by the branch id the merge recorded.
	// Resolving it here rather than at plan time is what makes the RETRY path
	// (replay → retryGitStep) name the same pair: a stored merge whose Git
	// step failed has no branch rows in hand, and the id is stable while the
	// ref name it maps to is the one T0303 maintains.
	sourceRef, sourceErr := s.sourceRefOf(ctx, res.Merge)
	switch {
	case s.git == nil:
		step.Error = "no provider merge adapter is wired (T0409 owns it): the platform database truth is committed, the Git ref update has not happened"
	case sourceErr != nil:
		// Without the source ref the saga cannot name the PR it must merge:
		// fail the step rather than merging an unnamed pair.
		step.State = domain.GitStateFailed
		step.Error = "resolve the merge's source ref: " + sourceErr.Error()
	default:
		result, err := s.git.MergePullRequest(ctx, GitMergeRequest{
			ProjectID: res.Merge.ProjectID,
			Number:    res.Number,
			TargetRef: deref(res.Merge.GitRef),
			SourceRef: deref(sourceRef),
			TargetSHA: deref(res.Plan.Target.GitRef),
			SourceSHA: deref(res.Plan.Source.GitRef),
		})
		switch {
		case err != nil:
			step.State = domain.GitStateFailed
			step.Error = err.Error()
		default:
			// The platform layer judges the update the provider just
			// performed: main may only be advanced by the merge-service
			// identity (gitprovider.RefGuard). An update the guard refuses
			// is recorded as a failed step, never as a legitimate merge —
			// that refusal is exactly the "no direct write to main" rule,
			// applied to ourselves.
			if err := s.guard.Check(RefUpdate{
				Ref:      deref(res.Merge.GitRef),
				OldSHA:   deref(res.Plan.Target.GitRef),
				NewSHA:   result.SHA,
				Actor:    result.Actor,
				ViaMerge: true,
			}); err != nil {
				step.State = domain.GitStateFailed
				step.Error = err.Error()
			} else {
				step.State = domain.GitStateUpdated
				step.SHA = result.SHA
			}
		}
	}
	if step.State == domain.GitStateUpdated {
		if updated, err := s.store.CompleteGitStep(ctx, step); err == nil {
			res.Merge = updated
		}
		return
	}
	if updated, err := s.store.RecordGitAttempt(ctx, step); err == nil {
		res.Merge = updated
	}
}

// sourceRefOf resolves the provider ref of the branch a merge came FROM — the
// head the provider-side pull request names. It is read from the branch row
// by id, so a re-driven Git step (a client retrying its Idempotency-Key)
// resolves exactly the ref the first attempt would have.
//
// An empty id (a merge row written by hand, or a schema that predates the
// column) resolves to nil, and a branch that is gone is an error: the caller
// records a failed step rather than merging some other branch's pair.
func (s *Service) sourceRefOf(ctx context.Context, m domain.SemanticMerge) (*string, error) {
	if m.SourceBranchID == "" {
		return nil, nil
	}
	b, err := s.store.GetBranch(ctx, m.ProjectID, m.SourceBranchID)
	if err != nil {
		return nil, err
	}
	return gitRefOf(b), nil
}

// deref reads an optional string, "" when nil.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
