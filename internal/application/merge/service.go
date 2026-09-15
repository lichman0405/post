package merge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lichman0405/post/internal/application/diffs"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/relations"
	"github.com/lichman0405/post/internal/application/sciobjects"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
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
	git         GitMerger
	guard       RefGuard
	maxAttempts int
}

// NewService wires the service.
func NewService(d Deps) *Service {
	attempts := d.MaxAttempts
	if attempts <= 0 {
		attempts = defaultAttempts
	}
	return &Service{
		store:       d.Store,
		diffs:       d.Diffs,
		plans:       d.Plans,
		commits:     d.Commits,
		objects:     d.Objects,
		relations:   d.Relations,
		projects:    d.Projects,
		authz:       d.Authz,
		git:         d.Git,
		guard:       d.RefGuard,
		maxAttempts: attempts,
	}
}

// Input names one merge request.
type Input struct {
	ProjectID string
	// Number is the per-project PR number.
	Number int64
	// Message is the state commit's message. Empty falls back to a
	// generated one naming the PR and the branch pair.
	Message string
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
}

// Merge executes one Research PR merge: plan, commit, then the Git saga.
//
// The database truth is one transaction — the accepted state, the merge
// record, the conflicts it carries, the PR's transition to merged and the
// source branch's close either all land or none do. The Git half follows the
// commit (docs/08: Git is the repository/file truth beside PostgreSQL's
// semantic truth) and is recorded on the merge row, so a failed Git step is
// visible and retryable instead of silently divergent.
func (s *Service) Merge(ctx context.Context, actor domain.User, in Input) (*Result, error) {
	if err := validateInput(in, actor); err != nil {
		return nil, err
	}
	if err := s.requireMerge(ctx, actor, in.ProjectID); err != nil {
		return nil, err
	}
	prepared, err := s.prepare(ctx, in)
	if err != nil {
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
	if s.git == nil {
		step.Error = "no provider merge adapter is wired (T0409 owns it): the platform database truth is committed, the Git ref update has not happened"
	} else {
		result, err := s.git.MergePullRequest(ctx, GitMergeRequest{
			ProjectID: res.Merge.ProjectID,
			Number:    res.Number,
			TargetRef: deref(res.Merge.GitRef),
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

// deref reads an optional string, "" when nil.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
