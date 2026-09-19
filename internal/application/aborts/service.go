package aborts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/application/sciobjects"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// MinIdempotencyKeyLen is the Idempotency-Key bound the contract fixes
// (specs/api/openapi.yaml components.parameters.IdempotencyKey: required,
// minLength 8). The command checks it itself, not only the transport: the
// contract's requirements are server-side requirements, and a command
// reachable from anything but the HTTP route must hold to the same ones.
const MinIdempotencyKeyLen = 8

// The reason-code token shape. Shape only — never a vocabulary: see
// AbortRecord.ReasonCode in internal/domain for the ruling and its cost.
var reasonCodeRe = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// Bounds on the free-text fields, matching migration 00100's CHECK
// constraints so that a value the command admits is a value the database
// admits, and a value it refuses is refused with a domain message rather
// than a constraint violation.
const (
	// MaxExplanationLen is the human explanation's bound (docs/46:7
	// requires the field; the bound is this command's, and migration
	// 00100's abort_explanation_shape enforces the same one).
	MaxExplanationLen = 4096
	// MaxReplacementRefLen bounds docs/46:7's replacement/superseding ref.
	MaxReplacementRefLen = 512
	// maxAbortPRTitleLen is the bound domain.ValidPullRequestTitle enforces
	// (200). The generated proposal title is built from the object type,
	// the object's title and the reason code, so it can exceed it; the
	// builder truncates on a rune boundary rather than letting the PR
	// adapter refuse a title this command constructed.
	maxAbortPRTitleLen = 200
)

// manifestVersion is the manifest format version every scientific-state
// commit is written under (the RSG write path's value, same constant).
const manifestVersion = "v1"

// Service is the abort-proposal use case.
type Service struct {
	members  Membership
	authz    Authz
	objects  Objects
	branches Branches
	prs      PullRequests
	commits  Commits
	events   EventRecorder
	// now is the clock the abort's decision time is derived from. It is a
	// field so a test can pin it; production leaves it nil and gets
	// time.Now (docs/23 §3: the time is server-derived, never caller-
	// supplied).
	now func() time.Time
}

// Deps carries the adapters a Service is wired over.
type Deps struct {
	// Members resolves the actor's project membership (the production
	// value is *projects.Service).
	Members Membership
	// Authz is the permission-matrix engine (authz.NewMatrixEngine()).
	Authz Authz
	// Objects is the version log (the production value is
	// *persistence.ScientificObjectStore).
	Objects Objects
	// Branches forks the proposal branch (the production value is
	// *branches.Service).
	Branches Branches
	// PullRequests opens the proposal (the production value is
	// *pullrequests.Service).
	PullRequests PullRequests
	// Commits runs the state transition (the production value is
	// *states.Service).
	Commits Commits
	// Events is the transactional outbox recorder (events.Recorder{}).
	// Required: the abort's domain event is part of the abort, not an
	// afterthought.
	Events EventRecorder
}

// NewService wires the service.
func NewService(d Deps) *Service {
	return &Service{
		members:  d.Members,
		authz:    d.Authz,
		objects:  d.Objects,
		branches: d.Branches,
		prs:      d.PullRequests,
		commits:  d.Commits,
		events:   d.Events,
	}
}

// Actor is the aborting principal: the authenticated user, and whether the
// request arrived as a platform agent rather than a human session.
//
// The flag is a field of a value the TRANSPORT builds from the principal,
// never a field of the request body: a caller that could set it could
// clear it, and the only thing the flag does here is refuse.
type Actor struct {
	// User is the authenticated user. An agent acts AS a user — the
	// token's owner — so this is never empty.
	User domain.User
	// IsAgent reports whether the request arrived as a platform agent
	// (MCP/API), not as a human session.
	IsAgent bool
}

// Input is one abort-proposal request.
//
// The four data fields are specs/mcp/tools.json:23's arguments
// (project_id, object_version_ref, reason_code, explanation) plus the
// optional replacement ref docs/46:7 names and the Idempotency-Key the
// contract's other write routes require. The HTTP body schema and the key
// parameter are this task's design (the contract declares the route and
// neither): see the transport package for the names it renders and this
// task's RESULT for why.
type Input struct {
	// ProjectID is the research boundary the object lives in.
	ProjectID string
	// ObjectID names the container object whose version is aborted.
	ObjectID string
	// ObjectVersionRef names the version the abort is about: the version
	// row's own id. The transport strips the platform's
	// `object_version:<uuid>` ref spelling before this field is filled
	// (the same strip the publish and evidence surfaces do), so the value
	// here is a bare uuid.
	ObjectVersionRef string
	// ReasonCode is docs/46:7's "reason code". An OPEN token in V1 — the
	// command checks its shape and stores it as given; no enumeration
	// exists anywhere in this build, and none is invented here.
	ReasonCode string
	// Explanation is docs/46:7's "human explanation". Required, never
	// empty: it is the part of an abort no machine can reconstruct.
	Explanation string
	// ReplacementRef is docs/46:7's optional
	// "replacement/superseding ref". Empty means none was given and is
	// stored as NULL, never as an empty string.
	ReplacementRef string
	// IdempotencyKey is the contract-required key this request carries.
	// It does not create a ledger: the version row the request appends
	// holds it (migration 00100), so the state itself is the idempotency
	// record.
	IdempotencyKey string
}

// Result is one abort proposal, as recorded.
type Result struct {
	ProjectID string `json:"project_id"`
	ObjectID  string `json:"object_id"`
	// AbortedVersionID and AbortedVersionNo name the version the abort is
	// about — the one whose lifecycle the proposal moves. It is NOT
	// modified: the abort appends.
	AbortedVersionID string `json:"aborted_version_id"`
	AbortedVersionNo int    `json:"aborted_version_no"`
	// VersionID and VersionNo name the version row the abort appended:
	// the proposal branch's head, lifecycle_state 'aborted'.
	VersionID      string `json:"version_id"`
	VersionNo      int    `json:"version_no"`
	LifecycleState string `json:"lifecycle_state"`
	// BranchID and BranchName name the proposal branch the version was
	// committed to. The name is derived from the object and the
	// Idempotency-Key, so it is stable across replays.
	BranchID   string `json:"branch_id"`
	BranchName string `json:"branch_name"`
	// PullRequestNumber is the Research PR's per-project number. It is 0
	// only in the replay case where the first attempt committed the
	// version and died before opening the PR (see replay()).
	PullRequestNumber int64 `json:"pull_request_number,omitempty"`
	// PRState is the proposal's lifecycle state as read back, "" when no
	// PR was found.
	PRState string `json:"pull_request_state,omitempty"`
	// The record docs/46:7 requires, echoed as stored.
	ReasonCode     string    `json:"reason_code"`
	Explanation    string    `json:"explanation"`
	ReplacementRef string    `json:"replacement_ref,omitempty"`
	DecidedBy      string    `json:"decided_by"`
	DecidedAt      time.Time `json:"decided_at"`
	// Replayed reports that this call found the version an earlier
	// request with the same key appended and wrote nothing: no second
	// version, no second audit row, no second event.
	Replayed bool `json:"replayed"`
}

// AbortProposal runs one abort proposal.
//
// The order of the steps IS the design, so it is stated once here and not
// repeated:
//
//  1. shape. Nothing is read.
//  2. the agent backstop. Nothing is read — the actor value alone decides.
//  3. authorization (the matrix). Membership is resolved, then the
//     decision; an unknown project answers "not a member", the matrix
//     denies that class, and the caller cannot tell the two apart.
//  4. the idempotency read: has this key already produced a version?
//  5. the target reads: object, version, main line.
//  6. the proposal: fork a branch off main's head, append the aborted
//     version (with the audit and the event in the same transaction), open
//     the PR.
//
// Steps 1-3 all precede step 4, so no refusal of any kind depends on
// whether the project or the object exists. Step 4 precedes step 5, so a
// replay is answered from what the first request recorded even though the
// named version is no longer in a state the fresh path would accept (it is
// aborted by then — that is the point).
func (s *Service) AbortProposal(ctx context.Context, actor Actor, in Input) (Result, error) {
	if err := requireShape(in); err != nil {
		return Result{}, err
	}
	if err := s.requireActor(actor); err != nil {
		return Result{}, err
	}
	if err := s.requireAgent(actor); err != nil {
		return Result{}, err
	}
	if err := s.requireAbort(ctx, actor.User.ID, in.ProjectID, actor.IsAgent); err != nil {
		return Result{}, err
	}
	if s.objects == nil || s.branches == nil || s.prs == nil || s.commits == nil {
		return Result{}, fmt.Errorf("%w: abort command not fully wired", ErrStore)
	}

	obj, err := s.objects.GetObject(ctx, in.ObjectID)
	if err != nil {
		return Result{}, mapObjectReadError(err)
	}
	if obj.ProjectID != in.ProjectID {
		// An object of another project answers exactly as an unknown one
		// (docs/45: never leak a foreign project's entity existence).
		return Result{}, ErrObjectNotFound
	}

	// The idempotency read, before anything else can refuse: the state
	// itself is the record.
	existing, err := s.objects.GetVersionByAbortRequestKey(ctx, in.ObjectID, in.IdempotencyKey)
	switch {
	case err == nil:
		return s.replay(ctx, in, existing)
	case errors.Is(err, sciobjects.ErrVersionNotFound):
		// No version carries this key yet: this request is the first.
	default:
		return Result{}, fmt.Errorf("%w: %v", ErrStore, err)
	}

	return s.propose(ctx, actor, in, obj)
}

// propose runs the fresh path: resolve the named version and the main line,
// then create the proposal.
func (s *Service) propose(ctx context.Context, actor Actor, in Input, obj domain.ScientificObject) (Result, error) {
	named, err := s.objects.GetVersionByID(ctx, in.ObjectVersionRef)
	if err != nil {
		if errors.Is(err, sciobjects.ErrVersionNotFound) {
			return Result{}, ErrVersionNotFound
		}
		return Result{}, fmt.Errorf("%w: %v", ErrStore, err)
	}
	if named.ObjectID != in.ObjectID {
		// A version of another object answers as an unknown one.
		return Result{}, ErrVersionNotFound
	}
	if named.VersionNo != obj.CurrentVersionNo {
		// Not the object's current version. Aborting a superseded version
		// is 'superseded', not 'aborted', and aborting an old version
		// while a newer one is current would leave the object's live head
		// active — an abort that did not abort anything.
		return Result{}, ErrVersionNotFound
	}
	if named.LifecycleState != domain.LifecycleActive {
		// FAIL-CLOSED, named in the task result: only a version in
		// lifecycle 'active' may be aborted. 'aborted' and 'superseded'
		// are terminal for this command, and 'reopened' is refused because
		// nothing in this build produces it (docs/46:11's reopen is
		// T0610); admitting it is a one-line widening when T0610 lands.
		return Result{}, ErrVersionNotFound
	}

	mainLine, err := s.mainBranch(ctx, in.ProjectID)
	if err != nil {
		return Result{}, err
	}
	if named.BranchID == nil || *named.BranchID != mainLine.ID {
		// docs/46:9 governs "main 中对象": the version named must be one
		// the main line carries. A version living on another branch is not
		// this command's subject. FAIL-CLOSED: a version with no branch at
		// all (a row written before branch resolution existed) is refused
		// too, rather than assumed to be main's.
		return Result{}, ErrNotMainObject
	}

	rec := domain.AbortRecord{
		ReasonCode:     in.ReasonCode,
		Explanation:    in.Explanation,
		ReplacementRef: in.ReplacementRef,
		DecidedBy:      actor.User.ID,
		DecidedAt:      s.clock(),
	}

	branch, err := s.proposalBranch(ctx, actor, in, mainLine)
	if err != nil {
		return Result{}, err
	}

	version, err := s.appendAbortVersion(ctx, actor, in, obj, named, branch, rec)
	if err != nil {
		// The append may have lost a race to an identical concurrent
		// request that carried the same key. If the key now names a
		// version, that request won and this one replays it; otherwise
		// the failure is real.
		if replay, rerr := s.replayIfRecorded(ctx, in); rerr == nil {
			return replay, nil
		}
		return Result{}, err
	}

	pr, err := s.openProposal(ctx, actor, in, obj, named, mainLine, branch, rec)
	if err != nil {
		// A key held by a proposal this request did not open is NOT
		// replayable, and must not reach the fallback below: the append
		// above already wrote this object's version under this very key, so
		// the fallback's read would find it and answer 201 ("replayed") for
		// a request that was refused. Only the outcomes the append's race
		// can produce are eligible.
		if errors.Is(err, ErrIdempotencyKeyInUse) {
			return Result{}, err
		}
		if replay, rerr := s.replayIfRecorded(ctx, in); rerr == nil {
			return replay, nil
		}
		return Result{}, err
	}

	return resultFrom(in.ProjectID, version, named, branch, pr, rec), nil
}

// requireAgent is the domain backstop: an agent never aborts a main object,
// whatever the matrix says. It reads the actor value alone, so it is
// resolved before any lookup, and it is deliberately the FIRST of the two
// lines — so the wire can name which one stopped the caller (the
// internal/application/mainfreeze and contribution shape).
func (s *Service) requireAgent(actor Actor) error {
	if actor.IsAgent {
		return &AgentNotPermittedError{Action: "abort_main_object"}
	}
	return nil
}

// requireAbort is the authorization step: the actor's class in this project
// against the permission matrix's abort_main_object row
// (internal/authz/matrix.go:121-129; specs/policies/permissions-matrix.csv:14
// — anonymous/non-member/viewer/contributor deny, maintainer/owner via_pr,
// agent proposal_only). It resolves the membership FIRST and only then the
// decision, and it maps an unknown project to an unknown membership, so a
// denial never discloses whether the project exists (the
// releases.ErrForbidden rule). Every lookup of the object happens after it.
//
// The conditional verdicts are resolved here, because a conditional verdict
// that nobody resolves is a denial with extra steps (authz.Verdict.Permits
// is true for `allow` alone):
//
//   - via_pr: PERMITTED, and this command is the resolution. The route
//     creates the proposal and the PR; it never writes main, and the abort
//     becomes effective only when a human merges the PR. That is
//     docs/46:9's "branch → PR → merge" spelled as a verdict.
//   - proposal_only (the agent cell): REFUSED. The matrix does not permit
//     it (`Permits` is false and it is not via_pr), and this command is
//     not a proposal an agent may make — the domain backstop above refuses
//     it too. The two lines are independent on purpose: this one is the
//     matrix's own answer, and the test suite exercises it with the
//     backstop bypassed.
//   - every other conditional form: REFUSED. Nothing in this build
//     resolves them for this action, and the safe default for an
//     unresolved condition is refusal.
func (s *Service) requireAbort(ctx context.Context, actorID, projectID string, isAgent bool) error {
	if s.members == nil || s.authz == nil {
		return fmt.Errorf("%w: abort command not fully wired", ErrStore)
	}
	membership, err := s.members.GetMembership(ctx, projectID, actorID)
	var role *domain.ProjectRole
	switch {
	case err == nil:
		r := membership.Role
		role = &r
	case errors.Is(err, projects.ErrMemberNotFound):
		// Not a member — or no such project. The two are the same answer
		// on purpose: the matrix denies the non-member class, so an
		// unknown project is refused by the step that refuses a stranger,
		// and the caller cannot tell them apart.
		role = nil
	case errors.Is(err, projects.ErrProjectNotFound):
		role = nil
	default:
		return fmt.Errorf("%w: %v", ErrStore, err)
	}
	decision, err := s.authz.Authorize(ctx, authz.Request{
		Action: authz.ActionAbortMainObject,
		Class:  authz.ClassOf(true, role, isAgent),
	})
	if err != nil {
		return fmt.Errorf("%w: authorize abort: %v", ErrStore, err)
	}
	switch decision.Verdict {
	case authz.VerdictAllow, authz.VerdictViaPR:
		return nil
	default:
		return ErrForbidden
	}
}

// mainBranch resolves the project's main branch.
func (s *Service) mainBranch(ctx context.Context, projectID string) (domain.Branch, error) {
	list, err := s.branches.List(ctx, projectID)
	if err != nil {
		return domain.Branch{}, fmt.Errorf("%w: %v", ErrStore, err)
	}
	for _, b := range list {
		if b.IsMain() {
			return b, nil
		}
	}
	// A project without main is a project without an accepted state; there
	// is nothing to propose an abort against.
	return domain.Branch{}, ErrNotMainObject
}

// proposalBranch forks the proposal branch off main's head.
//
// The name is DERIVED from the object and the Idempotency-Key rather than
// generated, and that is what makes concurrent identical requests collide
// instead of forking two branches: the second request's insert fails with
// ErrBranchNameTaken, and the command adopts the branch the first one
// forked (a taken name here means this key already forked it — no other
// caller can produce this name).
func (s *Service) proposalBranch(ctx context.Context, actor Actor, in Input, main domain.Branch) (domain.Branch, error) {
	if main.BaseStateID == nil || *main.BaseStateID == "" {
		// A proposal forks an existing state; main without a head has
		// nothing to fork.
		return domain.Branch{}, ErrNotMainObject
	}
	name := proposalBranchName(in.ObjectID, in.IdempotencyKey)
	purpose := "abort proposal for scientific object " + in.ObjectID
	branch, err := s.branches.Create(ctx, branches.CreateBranchParams{
		ProjectID:   in.ProjectID,
		Name:        name,
		BaseStateID: *main.BaseStateID,
		CreatedBy:   actor.User.ID,
		Purpose:     &purpose,
	})
	if err == nil {
		return branch, nil
	}
	if errors.Is(err, branches.ErrBranchNameTaken) {
		adopted, aerr := s.branchByName(ctx, in.ProjectID, name)
		if aerr != nil {
			return domain.Branch{}, aerr
		}
		return adopted, nil
	}
	return domain.Branch{}, fmt.Errorf("%w: fork proposal branch: %v", ErrStore, err)
}

// branchByName re-reads the project's branches and returns the named one.
func (s *Service) branchByName(ctx context.Context, projectID, name string) (domain.Branch, error) {
	list, err := s.branches.List(ctx, projectID)
	if err != nil {
		return domain.Branch{}, fmt.Errorf("%w: %v", ErrStore, err)
	}
	for _, b := range list {
		if b.Name == name {
			return b, nil
		}
	}
	return domain.Branch{}, fmt.Errorf("%w: proposal branch %q is taken but not readable", ErrStore, name)
}

// appendAbortVersion runs the proposal's state transition: one commit that
// appends the aborted version and, in the SAME transaction, its audit row
// and its domain event.
//
// docs/53's rule is the reason for the shape: the audit record of a
// high-risk action is part of the action, so a version row in lifecycle
// 'aborted' either commits with the record of who decided it and why, or
// does not exist. There is no "change the state, then record it" path.
func (s *Service) appendAbortVersion(ctx context.Context, actor Actor, in Input, obj domain.ScientificObject, named domain.ScientificObjectVersion, branch domain.Branch, rec domain.AbortRecord) (domain.ScientificObjectVersion, error) {
	if s.events == nil {
		return domain.ScientificObjectVersion{}, fmt.Errorf("%w: no outbox recorder configured", ErrStore)
	}
	versionNo := named.VersionNo + 1
	params := states.CommitParams{
		ProjectID:  in.ProjectID,
		BranchID:   branch.ID,
		ActorID:    actor.User.ID,
		Via:        domain.ViaAPI,
		Message:    fmt.Sprintf("abort %s %q (reason code %s)", obj.ObjectType, named.Title, rec.ReasonCode),
		Operations: []domain.StateOperation{objectOperation(in.ObjectID, obj.ObjectType, versionNo)},
		// The commit is the proposal's first transition, so it forks the
		// branch's head — which is main's head, the state the branch was
		// created from.
		BaseStateID:     branch.BaseStateID,
		ManifestVersion: manifestVersion,
		// GateDraft, the gate every branch commit in this build is held to
		// (internal/application/rsg/service.go:263, :366, :451 — including
		// the object creation whose branch is likewise about to be
		// proposed). The ladder is progressive and the stricter rungs are
		// applied where they belong: the merge that lands this on main
		// re-runs the gate at GateMain inside ITS transaction
		// (internal/application/merge/service.go:585/:608), over this very
		// row. Naming GatePR here would not add a check the merge does not
		// already make; it would only make THIS command refuse branches the
		// rest of the build accepts — a payload that passed the main gate
		// would fail a command that has not changed it.
		Gate: rsgvalidation.GateDraft,
	}
	visibility := eventVisibility(branch.Visibility)

	var version domain.ScientificObjectVersion
	write := func(ctx context.Context, tx states.Transaction, stateID string) error {
		v, werr := s.objects.AppendAbortVersionInTx(ctx, tx, AbortWriteParams{
			ObjectID:          in.ObjectID,
			ExpectedVersionNo: obj.CurrentVersionNo,
			Version: sciobjects.VersionParams{
				StateID:        stateID,
				BranchID:       &branch.ID,
				SchemaID:       named.SchemaID,
				SchemaVersion:  named.SchemaVersion,
				Title:          named.Title,
				LifecycleState: domain.LifecycleAborted,
				// The payload is the aborted version's, byte for byte:
				// an abort moves the lifecycle and nothing else, so the
				// content address of the version it aborts is untouched
				// (docs/46: the correction appends, it never rewrites).
				Payload:            named.Payload,
				VisibilityPolicyID: named.VisibilityPolicyID,
				CreatedBy:          actor.User.ID,
				Abort:              &rec,
				AbortRequestKey:    in.IdempotencyKey,
			},
			Audit: auditEntry(actor, in, obj, named, versionNo, rec),
		})
		if werr != nil {
			return werr
		}
		version = v
		evt, eerr := abortedEvent(in.ProjectID, in.ObjectID, obj.ObjectType, stateID, branch.ID,
			actor.User.ID, named.VersionNo, v.VersionNo, rec, visibility)
		if eerr != nil {
			return eerr
		}
		return s.events.Record(ctx, tx, evt)
	}
	if _, _, err := s.commits.Commit(ctx, params, write); err != nil {
		return domain.ScientificObjectVersion{}, mapCommitError(err)
	}
	return version, nil
}

// openProposal opens the Research PR that carries the proposal. The
// creation key is the request's Idempotency-Key, so the adapter's own
// migration-00089 replay answers a repeat inside its own transaction
// (PullRequests.Create) — this call cannot open a second proposal for one
// key.
//
// The PR is opened AFTER the version commit, and the order is forced: a PR
// pins its proposed state from the source branch's head at creation and
// migration 00051's fixity guard freezes it there. Opening the PR first
// would pin it to the branch's base — a proposal whose head is its own
// base, proposing nothing.
//
// The row the call comes back with is CHECKED, not trusted: it must be the
// proposal this call forked, i.e. its source branch must be the branch
// above. The adapter's replay answers a known creation key with the row the
// earlier request opened, and that index is per PROJECT (migration 00089)
// while the abort's own key index is per OBJECT (00100) — so one key used
// for two objects in one project hands this call the FIRST object's
// proposal. Returning it would tell the second caller their abort was
// proposed when nothing proposes it: the branch they hold is not what that
// PR moves, and the merge would land the first object's abort only. The
// comparison needs no read — the branch this call just forked and the row
// it was handed are both in hand — so nothing is added ahead of the write
// and the single-winner compare-and-swap this command's idempotency rests
// on is untouched.
func (s *Service) openProposal(ctx context.Context, actor Actor, in Input, obj domain.ScientificObject, named domain.ScientificObjectVersion, main, branch domain.Branch, rec domain.AbortRecord) (domain.PullRequest, error) {
	pr, err := s.prs.Create(ctx, pullrequests.CreatePullRequestParams{
		ProjectID:      in.ProjectID,
		SourceBranchID: branch.ID,
		TargetBranchID: main.ID,
		Title:          abortPRTitle(obj, named, rec),
		Body:           abortPRBody(obj, named, rec),
		CreatedBy:      actor.User.ID,
		CreationKey:    in.IdempotencyKey,
	})
	if err != nil {
		return domain.PullRequest{}, mapPullRequestError(err)
	}
	if pr.SourceBranchID != branch.ID {
		return domain.PullRequest{}, &IdempotencyKeyInUseError{}
	}
	return pr, nil
}

// abortPRTitle renders the proposal's one-line summary. It names the object
// and the reason code — the two things a reviewer scanning a list needs — and
// is truncated to the contract's bound rather than refused, because a
// generated title must not be the thing that fails an otherwise valid abort.
func abortPRTitle(obj domain.ScientificObject, named domain.ScientificObjectVersion, rec domain.AbortRecord) string {
	title := fmt.Sprintf("Abort %s %q (reason code %s)", obj.ObjectType, named.Title, rec.ReasonCode)
	return truncateRunes(title, maxAbortPRTitleLen)
}

// abortPRBody renders the record docs/46:7 requires into the PR's context,
// field by field and labelled, so the reviewer decides on the record itself
// rather than on a summary of it. The replacement ref is printed only when
// there is one — "none" and "the empty string" are different facts and the
// body keeps them apart the same way the stored row does.
func abortPRBody(obj domain.ScientificObject, named domain.ScientificObjectVersion, rec domain.AbortRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Proposes aborting %s %s.\n\n", obj.ObjectType, obj.ID)
	fmt.Fprintf(&b, "Aborted version: v%d (%s)\n", named.VersionNo, named.ID)
	fmt.Fprintf(&b, "Reason code: %s\n", rec.ReasonCode)
	fmt.Fprintf(&b, "Decided by: %s\n", rec.DecidedBy)
	fmt.Fprintf(&b, "Decided at: %s\n", rec.DecidedAt.UTC().Format(time.RFC3339))
	if rec.ReplacementRef != "" {
		fmt.Fprintf(&b, "Replacement/superseding ref: %s\n", rec.ReplacementRef)
	}
	fmt.Fprintf(&b, "\nExplanation:\n%s\n", rec.Explanation)
	b.WriteString("\nMerging this proposal moves the object's lifecycle to 'aborted' on main. " +
		"Nothing is deleted: the aborted version's content is unchanged, and the abort is recorded as a new version " +
		"(docs/46).\n")
	return b.String()
}

// truncateRunes cuts s to at most n runes, so a multi-byte title is never
// split mid-rune.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// replay answers a repeated request from what the first one recorded: the
// version row the key names, plus the proposal it opened when one is
// readable. Nothing is written — no second version, no second audit row,
// no second event.
func (s *Service) replay(ctx context.Context, in Input, version domain.ScientificObjectVersion) (Result, error) {
	out := resultFromRecorded(in.ProjectID, version)

	// The version the abort is about: one number below the row the key
	// names, by construction (appendAbortVersion writes versionNo+1). It is
	// read rather than inferred so the replay reports the same id the first
	// answer did.
	if version.VersionNo > 1 {
		if aborted, err := s.objects.GetVersion(ctx, in.ObjectID, version.VersionNo-1); err == nil {
			out.AbortedVersionID = aborted.ID
		} else if !errors.Is(err, sciobjects.ErrVersionNotFound) {
			return Result{}, fmt.Errorf("%w: %v", ErrStore, err)
		}
	}
	// The proposal branch's name, when the row still names one.
	if out.BranchID != "" {
		if branch, err := s.branchByID(ctx, in.ProjectID, out.BranchID); err == nil {
			out.BranchName = branch.Name
		} else if !errors.Is(err, branches.ErrBranchNotFound) {
			return Result{}, err
		}
	}
	pr, err := s.prs.GetByCreationKey(ctx, in.ProjectID, in.IdempotencyKey)
	switch {
	case err == nil && pr.SourceBranchID != "" && pr.SourceBranchID == out.BranchID:
		out.PullRequestNumber = pr.Number
		out.PRState = string(pr.State)
	case err == nil:
		// The key names a proposal in this project, but not THIS object's:
		// the proposal the key names was not forked from the branch the
		// recorded version sits on (the creation key is indexed per project
		// by migration 00089, the abort key per object by 00100 —
		// ErrIdempotencyKeyInUse). Answer with what is recorded and leave
		// the proposal unnamed, exactly as the case below: this object's
		// recorded abort has no proposal of its own, and naming somebody
		// else's would report it as proposed. FAIL-CLOSED: a version row
		// that records no branch names no proposal either.
	case errors.Is(err, pullrequests.ErrPullRequestNotFound):
		// The first attempt committed the version and died before opening
		// the proposal (the gap documented in this task's result). The
		// replay answers with what is recorded rather than opening a PR on
		// a branch the caller can no longer see the head of.
	default:
		return Result{}, fmt.Errorf("%w: %v", ErrStore, err)
	}
	return out, nil
}

// branchByID resolves one of the project's branches by id.
func (s *Service) branchByID(ctx context.Context, projectID, branchID string) (domain.Branch, error) {
	list, err := s.branches.List(ctx, projectID)
	if err != nil {
		return domain.Branch{}, fmt.Errorf("%w: %v", ErrStore, err)
	}
	for _, b := range list {
		if b.ID == branchID {
			return b, nil
		}
	}
	return domain.Branch{}, branches.ErrBranchNotFound
}

// replayIfRecorded re-reads the key after a failed write and returns the
// recorded result when a concurrent (or earlier) request with this key got
// there first. It returns an error when the key still names nothing, so the
// caller reports the original failure.
func (s *Service) replayIfRecorded(ctx context.Context, in Input) (Result, error) {
	version, err := s.objects.GetVersionByAbortRequestKey(ctx, in.ObjectID, in.IdempotencyKey)
	if err != nil {
		return Result{}, err
	}
	return s.replay(ctx, in, version)
}

// clock reads the service's time source.
func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

// requireActor refuses a request that arrives with no resolved principal.
//
// The refusal is an AUTHORIZATION outcome, not a validation one: the
// matrix's anonymous cell for abort_main_object is `deny`
// (specs/policies/permissions-matrix.csv:14), so an actor-less call is
// refused by the same rule that refuses a stranger, with the same code, and
// — like every other refusal here — before anything is read. The transport
// resolves the principal before the command is reached, so this is the
// second line, not the only one.
func (s *Service) requireActor(actor Actor) error {
	if actor.User.ID == "" {
		return ErrForbidden
	}
	return nil
}

// requireShape validates the request before anything is read.
func requireShape(in Input) error {
	if strings.TrimSpace(in.ProjectID) == "" {
		return fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	if !domain.ValidUUID(in.ProjectID) {
		return fmt.Errorf("%w: project_id must be a uuid", ErrValidation)
	}
	if !domain.ValidUUID(in.ObjectID) {
		return fmt.Errorf("%w: object_id must name a scientific object (a uuid)", ErrValidation)
	}
	if !domain.ValidUUID(in.ObjectVersionRef) {
		return fmt.Errorf("%w: object_version_ref must name an object version (object_version:<uuid>), not %q", ErrValidation, in.ObjectVersionRef)
	}
	if !reasonCodeRe.MatchString(in.ReasonCode) {
		// Shape only. There is no list of reason codes in this build and
		// none is invented here: the value is the CALLER's, the
		// specification requires the field be recorded rather than taken
		// from a closed set (specs/mcp/tools.json:23 passes it as an
		// argument; docs/46:7 requires the record), and the cost of the
		// open set — V1 cannot break aborts down by reason — is named in
		// the task result rather than paid for by guessing a vocabulary.
		return fmt.Errorf("%w: reason_code must be 1..64 characters of [a-z0-9_]", ErrValidation)
	}
	explanation := strings.TrimSpace(in.Explanation)
	if explanation == "" {
		return fmt.Errorf("%w: explanation is required — docs/46:7 requires every abort to record a human explanation, and it is not a field a machine can fill in", ErrValidation)
	}
	if len(in.Explanation) > MaxExplanationLen {
		return fmt.Errorf("%w: explanation must be at most %d characters", ErrValidation, MaxExplanationLen)
	}
	if len(in.ReplacementRef) > MaxReplacementRefLen {
		return fmt.Errorf("%w: replacement_ref must be at most %d characters", ErrValidation, MaxReplacementRefLen)
	}
	if in.IdempotencyKey == "" {
		return fmt.Errorf("%w: Idempotency-Key is required on an abort proposal: it is what makes a repeated request return the proposal it already created instead of proposing a second abort", ErrValidation)
	}
	if len(in.IdempotencyKey) < MinIdempotencyKeyLen {
		return fmt.Errorf("%w: Idempotency-Key must be at least %d characters (specs/api/openapi.yaml)", ErrValidation, MinIdempotencyKeyLen)
	}
	return nil
}

// proposalBranchName derives the proposal branch's name from the object and
// the request key: `abort/<object uuid>-<8 hex of sha256(key)>`.
//
// Derivation rather than generation is the concurrency mechanism (see
// proposalBranch): two requests carrying one key propose one branch. The
// object id keeps the name readable — an operator looking at the branch
// list can see which object a proposal is about — and the key hash keeps
// two different keys for one object on two branches.
func proposalBranchName(objectID, idempotencyKey string) string {
	sum := sha256.Sum256([]byte(objectID + "\x00" + idempotencyKey))
	return "abort/" + strings.ToLower(objectID) + "-" + hex.EncodeToString(sum[:4])
}

// objectOperation builds the commit operation summary for the version the
// abort appends: the entity is the object id and the version number the log
// position, which is what the PR gate's commit_linkage check matches
// against the rows as written.
func objectOperation(objectID, objectType string, versionNo int) domain.StateOperation {
	return domain.StateOperation{
		Kind:      domain.OperationObjectVersionCreated,
		EntityID:  objectID,
		VersionNo: versionNo,
	}
}

// auditEntry renders the abort's audit row (docs/26 lists abort among the
// highest-risk actions that must be audited; docs/53 makes the row part of
// the action). It names the object and the version pair, the branch, the
// record docs/46:7 requires, and the Idempotency-Key of the request that
// produced it.
//
// The free-text explanation IS here, unlike in the event payload: the audit
// log is the governance record and is not fanned out to subscribers, while
// the event vocabulary carries identity and reference only (docs/52's
// payload rule). The two surfaces keep their own standards.
func auditEntry(actor Actor, in Input, obj domain.ScientificObject, named domain.ScientificObjectVersion, versionNo int, rec domain.AbortRecord) domain.AuditEntry {
	after := map[string]any{
		"object_id":          in.ObjectID,
		"object_type":        obj.ObjectType,
		"aborted_version_id": named.ID,
		"aborted_version_no": named.VersionNo,
		"version_no":         versionNo,
		"lifecycle_state":    string(domain.LifecycleAborted),
		"reason_code":        rec.ReasonCode,
		"explanation":        rec.Explanation,
		"decided_by":         rec.DecidedBy,
		"decided_at":         rec.DecidedAt.UTC().Format(time.RFC3339Nano),
		"idempotency_key":    in.IdempotencyKey,
		// Always false when it is written — the backstop above refuses an
		// agent before the store is reached — and recorded anyway so the
		// row states the fact rather than leaving it to be inferred from
		// the absence of a refusal.
		"actor_is_agent": actor.IsAgent,
	}
	// The replacement ref is set only when the caller gave one, so the row
	// distinguishes "no replacement" from "a replacement that is nothing"
	// exactly as the version row's NULL does.
	if rec.ReplacementRef != "" {
		after["replacement_ref"] = rec.ReplacementRef
	}
	return domain.AuditEntry{
		ActorID:   actor.User.ID,
		Action:    domain.ActionScientificObjectAborted,
		TargetRef: "object:" + in.ObjectID,
		ProjectID: in.ProjectID,
		BeforeSummary: map[string]any{
			"object_id":       in.ObjectID,
			"version_no":      named.VersionNo,
			"lifecycle_state": string(named.LifecycleState),
		},
		AfterSummary: after,
		Metadata: map[string]any{
			"abort_proposal": true,
			"reason_code":    rec.ReasonCode,
		},
	}
}

// resultFrom renders a fresh proposal. It starts from the same renderer the
// replay uses — so the two answers are the same answer — and adds what only
// the fresh path knows (the branch it just forked, the PR it just opened).
func resultFrom(projectID string, version, named domain.ScientificObjectVersion, branch domain.Branch, pr domain.PullRequest, rec domain.AbortRecord) Result {
	out := resultFromRecorded(projectID, version)
	out.AbortedVersionID = named.ID
	out.AbortedVersionNo = named.VersionNo
	out.BranchID = branch.ID
	out.BranchName = branch.Name
	out.ReasonCode = rec.ReasonCode
	out.Explanation = rec.Explanation
	out.ReplacementRef = rec.ReplacementRef
	out.DecidedBy = rec.DecidedBy
	out.DecidedAt = rec.DecidedAt
	out.PullRequestNumber = pr.Number
	out.PRState = string(pr.State)
	out.Replayed = false
	return out
}

// resultFromRecorded renders a result from the version row alone. Every
// field it fills comes from what the database holds, never from the request
// that produced it, so a replay reports the recorded facts rather than the
// caller's re-statement of them.
func resultFromRecorded(projectID string, version domain.ScientificObjectVersion) Result {
	out := Result{
		ProjectID:      projectID,
		ObjectID:       version.ObjectID,
		VersionID:      version.ID,
		VersionNo:      version.VersionNo,
		LifecycleState: string(version.LifecycleState),
		Replayed:       true,
	}
	if version.BranchID != nil {
		out.BranchID = *version.BranchID
	}
	if version.Abort != nil {
		out.AbortedVersionNo = version.VersionNo - 1
		out.ReasonCode = version.Abort.ReasonCode
		out.Explanation = version.Abort.Explanation
		out.ReplacementRef = version.Abort.ReplacementRef
		out.DecidedBy = version.Abort.DecidedBy
		out.DecidedAt = version.Abort.DecidedAt
	}
	return out
}

// mapObjectReadError keeps the expected outcomes and turns everything else
// into ErrStore with the cause kept for the log.
func mapObjectReadError(err error) error {
	if errors.Is(err, sciobjects.ErrObjectNotFound) {
		return ErrObjectNotFound
	}
	// Everything else — sciobjects.ErrStore included, since "the store
	// failed" is already what this says — is a store failure with the cause
	// kept for the log.
	return fmt.Errorf("%w: %v", ErrStore, err)
}

// mapCommitError keeps the domain outcomes the commit can raise and turns
// everything else into ErrStore. A lost compare-and-swap is ErrConflict:
// the object's version log moved between the read and the append and the
// request neither won nor replayed, so the caller may re-read and retry.
func mapCommitError(err error) error {
	var conflict *sciobjects.VersionConflictError
	switch {
	case errors.As(err, &conflict):
		return fmt.Errorf("%w: %v", ErrConflict, err)
	case errors.Is(err, sciobjects.ErrVersionNotFound), errors.Is(err, sciobjects.ErrObjectNotFound):
		return ErrVersionNotFound
	case errors.Is(err, sciobjects.ErrValidation):
		return fmt.Errorf("%w: %v", ErrValidation, err)
	case errors.Is(err, states.ErrValidation):
		return fmt.Errorf("%w: %v", ErrValidation, err)
	case errors.Is(err, states.ErrBranchNotFound):
		return fmt.Errorf("%w: %v", ErrStore, err)
	default:
		return fmt.Errorf("%w: %v", ErrStore, err)
	}
}

// mapPullRequestError keeps the domain outcomes the PR adapter can raise.
// ErrBranchNotActive is the one that matters at this call site: a branch
// that closed between the fork and the proposal is a conflict with what the
// command read, not a server failure.
func mapPullRequestError(err error) error {
	switch {
	case errors.Is(err, pullrequests.ErrValidation):
		return fmt.Errorf("%w: %v", ErrValidation, err)
	case errors.Is(err, pullrequests.ErrBranchNotFound),
		errors.Is(err, pullrequests.ErrBranchNotActive),
		errors.Is(err, pullrequests.ErrBranchHeadMissing):
		return fmt.Errorf("%w: %v", ErrConflict, err)
	default:
		return fmt.Errorf("%w: %v", ErrStore, err)
	}
}
