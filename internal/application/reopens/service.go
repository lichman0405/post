package reopens

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

// MinIdempotencyKeyLen is the Idempotency-Key bound the contract fixes for
// the governed write routes (specs/api/openapi.yaml
// components.parameters.IdempotencyKey: required, minLength 8). The reopen
// route is not in the contract (see the package doc), so this command holds to
// the platform's existing bound rather than inventing a laxer one: the key is
// what makes a repeated request a replay instead of a second transition, and
// that property does not depend on which route reached the command.
const MinIdempotencyKeyLen = 8

// The reason-code token shape. Shape only — never a vocabulary: the abort
// record's ReasonCode is an OPEN token in this build (see
// AbortRecord.ReasonCode / ReopenRecord in internal/domain), and the reopen
// record is the same decision applied to the reverse edge (the task book:
// "reopen 按同一形状记录 reason/explanation 即可——沿用它的决定，不要另立一套").
var reasonCodeRe = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// Bounds on the free-text fields, matching migration 00123's CHECK constraints
// so that a value the command admits is a value the database admits, and a
// value it refuses is refused with a domain message rather than a constraint
// violation.
const (
	// MaxExplanationLen is the human explanation's bound, the abort
	// explanation's bound and migration 00123's reopen_explanation_shape.
	MaxExplanationLen = 4096
	// maxReopenPRTitleLen is the bound domain.ValidPullRequestTitle enforces
	// (200). See abortPRTitle's twin below.
	maxReopenPRTitleLen = 200
)

// manifestVersion is the manifest format version every scientific-state commit
// is written under (the RSG write path's value, same constant).
const manifestVersion = "v1"

// Service is the reopen-proposal use case.
type Service struct {
	members  Membership
	authz    Authz
	objects  Objects
	branches Branches
	prs      PullRequests
	commits  Commits
	events   EventRecorder
	// now is the clock the reopen's decision time is derived from. It is a
	// field so a test can pin it; production leaves it nil and gets time.Now
	// (docs/23 §3: the time is server-derived, never caller-supplied).
	now func() time.Time
}

// Deps carries the adapters a Service is wired over.
type Deps struct {
	// Members resolves the actor's project membership (the production value
	// is persistence.NewProjectStore(pool), as the freeze and abort commands
	// are wired).
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
	// Required: the reopen's domain event is part of the reopen, not an
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

// Actor is the reopening principal: the authenticated user, and whether the
// request arrived as a platform agent rather than a human session.
//
// The flag is a field of a value the TRANSPORT builds from the principal,
// never a field of the request body: a caller that could set it could clear
// it, and the only thing the flag does here is refuse.
type Actor struct {
	// User is the authenticated user. An agent acts AS a user — the token's
	// owner — so this is never empty.
	User domain.User
	// IsAgent reports whether the request arrived as a platform agent
	// (MCP/API), not as a human session.
	IsAgent bool
}

// Input is one reopen-proposal request.
//
// The field set mirrors the abort command's Input, minus the replacement ref:
// docs/46:7 makes "replacement/superseding ref" part of what an ABORT records
// and says nothing of the kind about a reopen, so no such column exists
// (migration 00123) and no such field is accepted. The task book's "沿用它的
// 决定，不要另立一套" is the reason the other two record fields are here at
// all; see the package doc for the judgement and its cost.
type Input struct {
	// ProjectID is the research boundary the object lives in.
	ProjectID string
	// ObjectID names the container object whose version is reopened.
	ObjectID string
	// ObjectVersionRef names the version the reopen is about: the version row
	// that CARRIES the abort (the object's current version, lifecycle
	// 'aborted'). The transport strips the platform's `object_version:<uuid>`
	// ref spelling before this field is filled, so the value here is a bare
	// uuid.
	ObjectVersionRef string
	// ReasonCode is the reopen record's reason code. An OPEN token in V1 —
	// the command checks its shape and stores it as given; no enumeration
	// exists anywhere in this build, and none is invented here.
	ReasonCode string
	// Explanation is the reopen record's human explanation. Required, never
	// empty: it is the part of the decision no machine can reconstruct.
	Explanation string
	// IdempotencyKey is the key this request carries. It does not create a
	// ledger: the version row the request appends holds it (migration 00123),
	// so the state itself is the idempotency record.
	IdempotencyKey string
}

// Result is one reopen proposal, as recorded.
type Result struct {
	ProjectID string `json:"project_id"`
	ObjectID  string `json:"object_id"`
	// AbortedVersionID and AbortedVersionNo name the version the reopen is
	// about — the row that carries the abort, whose lifecycle the proposal
	// moves. It is NOT modified: the reopen appends. (The field names are the
	// abort command's, and they mean the same thing here: this is the aborted
	// version, which is what a reopen reopens.)
	AbortedVersionID string `json:"aborted_version_id"`
	AbortedVersionNo int    `json:"aborted_version_no"`
	// VersionID and VersionNo name the version row the reopen appended: the
	// proposal branch's head, lifecycle_state 'reopened'.
	VersionID      string `json:"version_id"`
	VersionNo      int    `json:"version_no"`
	LifecycleState string `json:"lifecycle_state"`
	// BranchID and BranchName name the proposal branch the version was
	// committed to. The name is derived from the object and the
	// Idempotency-Key, so it is stable across replays.
	BranchID   string `json:"branch_id"`
	BranchName string `json:"branch_name"`
	// PullRequestNumber is the Research PR's per-project number. It is 0 only
	// in the replay case where the first attempt committed the version and
	// died before opening the PR (see replay()).
	PullRequestNumber int64 `json:"pull_request_number,omitempty"`
	// PRState is the proposal's lifecycle state as read back, "" when no PR
	// was found.
	PRState string `json:"pull_request_state,omitempty"`
	// The reopen record, echoed as stored.
	ReasonCode  string    `json:"reason_code"`
	Explanation string    `json:"explanation"`
	DecidedBy   string    `json:"decided_by"`
	DecidedAt   time.Time `json:"decided_at"`
	// Replayed reports that this call found the version an earlier request
	// with the same key appended and wrote nothing: no second version, no
	// second audit row, no second event.
	Replayed bool `json:"replayed"`
}

// ReopenProposal runs one reopen proposal.
//
// The order of the steps IS the design, and it is the abort command's order
// (internal/application/aborts/service.go:202-222) because every reason it
// holds there holds here:
//
//  1. shape. Nothing is read.
//  2. the agent backstop. Nothing is read — the actor value alone decides.
//  3. authorization (the matrix). Membership is resolved, then the decision;
//     an unknown project answers "not a member", the matrix denies that class,
//     and the caller cannot tell the two apart.
//  4. the idempotency read: has this key already produced a version?
//  5. the target reads: object, version, lifecycle, main line.
//  6. the proposal: fork a branch off main's head, append the reopened version
//     (with the audit and the event in the same transaction), open the PR.
//
// Steps 1-3 all precede step 4, so no refusal of any kind depends on whether
// the project or the object exists. Step 4 precedes step 5, so a replay is
// answered from what the first request recorded even though the named version
// is no longer in a state the fresh path would accept (it is 'reopened' by
// then — that is the point).
func (s *Service) ReopenProposal(ctx context.Context, actor Actor, in Input) (Result, error) {
	if err := requireShape(in); err != nil {
		return Result{}, err
	}
	if err := s.requireActor(actor); err != nil {
		return Result{}, err
	}
	if err := s.requireAgent(actor); err != nil {
		return Result{}, err
	}
	if err := s.requireReopen(ctx, actor.User.ID, in.ProjectID, actor.IsAgent); err != nil {
		return Result{}, err
	}
	if s.objects == nil || s.branches == nil || s.prs == nil || s.commits == nil {
		return Result{}, fmt.Errorf("%w: reopen command not fully wired", ErrStore)
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

	// The idempotency read, before anything else can refuse: the state itself
	// is the record.
	existing, err := s.objects.GetVersionByReopenRequestKey(ctx, in.ObjectID, in.IdempotencyKey)
	switch {
	case err == nil:
		return s.replay(ctx, in, existing)
	case errors.Is(err, sciobjects.ErrVersionNotFound):
		// No version carries this key yet: this request is the first.
	default:
		return Result{}, fmt.Errorf("%w: %v", ErrStore, err)
	}

	// The project-wide proposal index, checked BEFORE the append. The Research
	// PR's creation key is indexed per PROJECT (migration 00089) and is shared
	// with the abort command, while this command's own key index is per object
	// (00123) — so a key that already names a proposal this request did not open
	// is a refusal (ErrIdempotencyKeyInUse), and refusing it HERE is what keeps
	// a refused request from leaving a version row, an audit row and an event
	// behind: the append is the point of no return, and nothing in this build
	// deletes a version (migration 00014's append-only trigger; "nothing
	// disappears").
	//
	// This does not weaken the single-winner property: two concurrent requests
	// carrying one key both read nothing here, and the compare-and-swap on the
	// object's version counter — not this read — is what decides between them.
	// The window it leaves open is the truly concurrent cross-object collision,
	// where both requests pass this read and one of them loses at the PR
	// creation; openProposal re-checks the row it is handed for exactly that
	// case, and the failure it reports there is the same refusal.
	if _, err := s.prs.GetByCreationKey(ctx, in.ProjectID, in.IdempotencyKey); err == nil {
		return Result{}, &IdempotencyKeyInUseError{}
	} else if !errors.Is(err, pullrequests.ErrPullRequestNotFound) {
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
		// Not the object's current version. Reopening an older aborted version
		// while a newer one is current would leave the object's live head
		// where it is — a reopen that reopened nothing — and it is the shape
		// docs/43:10's "aborted → reopened" edge does not have.
		return Result{}, ErrVersionNotFound
	}
	if named.LifecycleState != domain.LifecycleAborted {
		// The state machine, in ONE check (docs/43:10's "active → aborted →
		// reopened → active"). FAIL-CLOSED, named in the task result: only a
		// version in lifecycle 'aborted' may be reopened. 'active' is an
		// object that was never aborted (refused; criterion 3(a)); 'reopened'
		// is an object already reopened (refused; criterion 3(b)); and
		// 'superseded' is terminal for this command — all three would be a
		// reopen of something that is not in the state the edge leaves.
		return Result{}, ErrVersionNotFound
	}

	mainLine, err := s.mainBranch(ctx, in.ProjectID)
	if err != nil {
		return Result{}, err
	}
	if named.BranchID == nil || *named.BranchID != mainLine.ID {
		// docs/09:9-10 governs main's scientific state: "即便 Owner 也只能经
		// PR merge". A version living on another branch is not this command's
		// subject — and that includes the aborted version sitting on the
		// ABORT's proposal branch, which main does not carry until that PR
		// merges. FAIL-CLOSED: a version with no branch at all (a row written
		// before branch resolution existed) is refused too, rather than
		// assumed to be main's.
		return Result{}, ErrNotMainObject
	}

	rec := domain.ReopenRecord{
		ReasonCode:  in.ReasonCode,
		Explanation: in.Explanation,
		DecidedBy:   actor.User.ID,
		DecidedAt:   s.clock(),
	}

	branch, err := s.proposalBranch(ctx, actor, in, mainLine)
	if err != nil {
		return Result{}, err
	}

	version, err := s.appendReopenVersion(ctx, actor, in, obj, named, branch, rec)
	if err != nil {
		// The append may have lost a race to an identical concurrent request
		// that carried the same key. If the key now names a version, that
		// request won and this one replays it; otherwise the failure is real.
		if replay, rerr := s.replayIfRecorded(ctx, in); rerr == nil {
			return replay, nil
		}
		return Result{}, err
	}

	pr, err := s.openProposal(ctx, actor, in, obj, named, mainLine, branch, rec)
	if err != nil {
		// A key held by a proposal this request did not open is NOT replayable,
		// and must not reach the fallback below: the append above already wrote
		// this object's version under this very key, so the fallback's read
		// would find it and answer 201 ("replayed") for a request that was
		// refused. Only the outcomes the append's race can produce are
		// eligible.
		//
		// This is the concurrent half of the check ReopenProposal makes before
		// the append: two requests that both found the key free, for two
		// different objects, meet again here, and the one whose PR creation
		// loses gets the refusal. Its appended row is left on its own proposal
		// branch — it can neither be deleted (the log is append-only) nor
		// reported as a proposal (a replay of it names no PR, see replay) —
		// and main carries nothing either way.
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

// requireAgent is the domain backstop: an agent never reopens a main object,
// whatever the matrix says. It reads the actor value alone, so it is resolved
// before any lookup, and it is deliberately the FIRST of the two lines — so
// the wire can name which one stopped the caller.
func (s *Service) requireAgent(actor Actor) error {
	if actor.IsAgent {
		return &AgentNotPermittedError{Action: "reopen_main_object"}
	}
	return nil
}

// requireReopen is the authorization step: the actor's class in this project
// against the permission matrix's reopen_main_object row
// (internal/authz/matrix.go; specs/policies/permissions-matrix.csv:15 —
// anonymous/non-member/viewer/contributor deny, maintainer/owner via_pr,
// agent proposal_only, CELL FOR CELL the abort row at :14, which is the
// owner's 2026-09-21 ruling for this task). It resolves the membership FIRST
// and only then the decision, and it maps an unknown project to an unknown
// membership, so a denial never discloses whether the project exists. Every
// lookup of the object happens after it.
//
// The conditional verdicts are resolved here, because a conditional verdict
// that nobody resolves is a denial with extra steps (authz.Verdict.Permits is
// true for `allow` alone):
//
//   - via_pr: PERMITTED, and this command is the resolution. The route creates
//     the proposal and the PR; it never writes main, and the reopen becomes
//     effective only when a human merges the PR. That is docs/09:9-10's rule
//     spelled as a verdict — and it is the reason the reopen's cell is
//     `via_pr` rather than `allow`.
//   - proposal_only (the agent cell): REFUSED. The matrix does not permit it
//     (`Permits` is false and it is not via_pr), and this command is not a
//     proposal an agent may make — the domain backstop above refuses it too.
//     The two lines are independent on purpose: this one is the matrix's own
//     answer, and the test suite exercises it with the backstop bypassed.
//   - every other conditional form: REFUSED. Nothing in this build resolves
//     them for this action, and the safe default for an unresolved condition
//     is refusal.
func (s *Service) requireReopen(ctx context.Context, actorID, projectID string, isAgent bool) error {
	if s.members == nil || s.authz == nil {
		return fmt.Errorf("%w: reopen command not fully wired", ErrStore)
	}
	membership, err := s.members.GetMembership(ctx, projectID, actorID)
	var role *domain.ProjectRole
	switch {
	case err == nil:
		r := membership.Role
		role = &r
	case errors.Is(err, projects.ErrMemberNotFound):
		// Not a member — or no such project. The two are the same answer on
		// purpose: the matrix denies the non-member class, so an unknown
		// project is refused by the step that refuses a stranger, and the
		// caller cannot tell them apart.
		role = nil
	case errors.Is(err, projects.ErrProjectNotFound):
		role = nil
	default:
		return fmt.Errorf("%w: %v", ErrStore, err)
	}
	decision, err := s.authz.Authorize(ctx, authz.Request{
		Action: authz.ActionReopenMainObject,
		Class:  authz.ClassOf(true, role, isAgent),
	})
	if err != nil {
		return fmt.Errorf("%w: authorize reopen: %v", ErrStore, err)
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
	// A project without main is a project without an accepted state; there is
	// nothing to propose a reopen against.
	return domain.Branch{}, ErrNotMainObject
}

// proposalBranch forks the proposal branch off main's head.
//
// The name is DERIVED from the object and the Idempotency-Key rather than
// generated, and that is what makes concurrent identical requests collide
// instead of forking two branches: the second request's insert fails with
// ErrBranchNameTaken, and the command adopts the branch the first one forked
// (a taken name here means this key already forked it — no other caller can
// produce this name). The `reopen/` prefix keeps the reopen's proposals out of
// the abort's `abort/` namespace, so an operator reading the branch list can
// tell the two transitions apart.
func (s *Service) proposalBranch(ctx context.Context, actor Actor, in Input, main domain.Branch) (domain.Branch, error) {
	if main.BaseStateID == nil || *main.BaseStateID == "" {
		// A proposal forks an existing state; main without a head has nothing
		// to fork.
		return domain.Branch{}, ErrNotMainObject
	}
	name := proposalBranchName(in.ObjectID, in.IdempotencyKey)
	purpose := "reopen proposal for scientific object " + in.ObjectID
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

// appendReopenVersion runs the proposal's state transition: one commit that
// appends the reopened version and, in the SAME transaction, its audit row and
// its domain event.
//
// docs/53's rule is the reason for the shape: the audit record of a high-risk
// action is part of the action (docs/26 lists abort/reopen together), so a
// version row in lifecycle 'reopened' either commits with the record of who
// decided it and why, or does not exist. There is no "change the state, then
// record it" path.
func (s *Service) appendReopenVersion(ctx context.Context, actor Actor, in Input, obj domain.ScientificObject, named domain.ScientificObjectVersion, branch domain.Branch, rec domain.ReopenRecord) (domain.ScientificObjectVersion, error) {
	if s.events == nil {
		return domain.ScientificObjectVersion{}, fmt.Errorf("%w: no outbox recorder configured", ErrStore)
	}
	versionNo := named.VersionNo + 1
	params := states.CommitParams{
		ProjectID:  in.ProjectID,
		BranchID:   branch.ID,
		ActorID:    actor.User.ID,
		Via:        domain.ViaAPI,
		Message:    fmt.Sprintf("reopen %s %q (reason code %s)", obj.ObjectType, named.Title, rec.ReasonCode),
		Operations: []domain.StateOperation{objectOperation(in.ObjectID, obj.ObjectType, versionNo)},
		// The commit is the proposal's first transition, so it forks the
		// branch's head — which is main's head, the state the branch was
		// created from.
		BaseStateID:     branch.BaseStateID,
		ManifestVersion: manifestVersion,
		// GateDraft, the gate every branch commit in this build is held to —
		// including the abort proposal's (internal/application/aborts/
		// service.go:529). The ladder is progressive and the stricter rungs
		// are applied where they belong: the merge that lands this on main
		// re-runs the gate at GateMain inside ITS transaction, over this very
		// row. Naming GatePR here would not add a check the merge does not
		// already make; it would only make THIS command refuse branches the
		// rest of the build accepts.
		Gate: rsgvalidation.GateDraft,
	}
	visibility := eventVisibility(branch.Visibility)

	var version domain.ScientificObjectVersion
	write := func(ctx context.Context, tx states.Transaction, stateID string) error {
		v, werr := s.objects.AppendReopenVersionInTx(ctx, tx, ReopenWriteParams{
			ObjectID:          in.ObjectID,
			ExpectedVersionNo: obj.CurrentVersionNo,
			Version: sciobjects.VersionParams{
				StateID:        stateID,
				BranchID:       &branch.ID,
				SchemaID:       named.SchemaID,
				SchemaVersion:  named.SchemaVersion,
				Title:          named.Title,
				LifecycleState: domain.LifecycleReopened,
				// The payload is the ABORTED version's, byte for byte: the
				// research content is what the abort left it as, and a reopen
				// restores the object to its research life without rewriting a
				// byte of it (docs/46:11 keeps the abort history; nothing in a
				// reopen recreates content — it moves the lifecycle).
				Payload:            named.Payload,
				VisibilityPolicyID: named.VisibilityPolicyID,
				CreatedBy:          actor.User.ID,
				Reopen:             &rec,
				ReopenRequestKey:   in.IdempotencyKey,
			},
			Audit: auditEntry(actor, in, obj, named, versionNo, rec),
		})
		if werr != nil {
			return werr
		}
		version = v
		evt, eerr := reopenedEvent(in.ProjectID, in.ObjectID, obj.ObjectType, stateID, branch.ID,
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

// openProposal opens the Research PR that carries the proposal. The creation
// key is the request's Idempotency-Key, so the adapter's own migration-00089
// replay answers a repeat inside its own transaction (PullRequests.Create) —
// this call cannot open a second proposal for one key.
//
// The PR is opened AFTER the version commit, and the order is forced: a PR
// pins its proposed state from the source branch's head at creation and
// migration 00051's fixity guard freezes it there. Opening the PR first would
// pin it to the branch's base — a proposal whose head is its own base,
// proposing nothing.
//
// The row the call comes back with is CHECKED, not trusted, exactly as the
// abort command checks it: it must name the branch this call forked. The
// adapter's creation key index is per PROJECT (migration 00089) and is SHARED
// with the abort command, while this command's own key index is per OBJECT
// (00123) — so one key used for two objects in one project hands this call the
// FIRST object's proposal, or an abort proposal. Returning it would tell the
// caller their reopen was proposed when nothing proposes it: the branch they
// hold is not what that PR moves. The comparison needs no read — the branch
// this call just forked and the row it was handed are both in hand — so
// nothing is added ahead of the write and the single-winner compare-and-swap
// this command's idempotency rests on is untouched.
func (s *Service) openProposal(ctx context.Context, actor Actor, in Input, obj domain.ScientificObject, named domain.ScientificObjectVersion, main, branch domain.Branch, rec domain.ReopenRecord) (domain.PullRequest, error) {
	pr, err := s.prs.Create(ctx, pullrequests.CreatePullRequestParams{
		ProjectID:      in.ProjectID,
		SourceBranchID: branch.ID,
		TargetBranchID: main.ID,
		Title:          reopenPRTitle(obj, named, rec),
		Body:           reopenPRBody(obj, named, rec),
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

// reopenPRTitle renders the proposal's one-line summary. It names the object
// and the reason code — the two things a reviewer scanning a list needs — and
// is truncated to the contract's bound rather than refused, because a generated
// title must not be the thing that fails an otherwise valid reopen.
func reopenPRTitle(obj domain.ScientificObject, named domain.ScientificObjectVersion, rec domain.ReopenRecord) string {
	title := fmt.Sprintf("Reopen %s %q (reason code %s)", obj.ObjectType, named.Title, rec.ReasonCode)
	return truncateRunes(title, maxReopenPRTitleLen)
}

// reopenPRBody renders the reopen record into the PR's context, field by field
// and labelled, so the reviewer decides on the record itself rather than on a
// summary of it. It states what a merge will and will not do, because that is
// the decision the reviewer is taking: the object returns to research life and
// the abort stays where it is.
func reopenPRBody(obj domain.ScientificObject, named domain.ScientificObjectVersion, rec domain.ReopenRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Proposes reopening %s %s.\n\n", obj.ObjectType, obj.ID)
	fmt.Fprintf(&b, "Aborted version being reopened: v%d (%s)\n", named.VersionNo, named.ID)
	fmt.Fprintf(&b, "Reason code: %s\n", rec.ReasonCode)
	fmt.Fprintf(&b, "Decided by: %s\n", rec.DecidedBy)
	fmt.Fprintf(&b, "Decided at: %s\n", rec.DecidedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "\nExplanation:\n%s\n", rec.Explanation)
	b.WriteString("\nMerging this proposal moves the object's lifecycle to 'reopened' on main. " +
		"The abort it reverses is NOT removed: it stays in the version log exactly as it was written " +
		"(docs/46:11 — a reopen creates a new transition and keeps the abort history).\n")
	return b.String()
}

// truncateRunes cuts s to at most n runes, so a multi-byte title is never split
// mid-rune.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// replay answers a repeated request from what the first one recorded: the
// version row the key names, plus the proposal it opened when one is readable.
// Nothing is written — no second version, no second audit row, no second event.
func (s *Service) replay(ctx context.Context, in Input, version domain.ScientificObjectVersion) (Result, error) {
	out := resultFromRecorded(in.ProjectID, version)

	// The version the reopen is about: one number below the row the key names,
	// by construction (appendReopenVersion writes versionNo+1). It is read
	// rather than inferred so the replay reports the same id the first answer
	// did.
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
		// The key names a proposal in this project, but not THIS object's (the
		// creation key is indexed per project by migration 00089, this
		// command's key per object by 00123 — ErrIdempotencyKeyInUse). Answer
		// with what is recorded and leave the proposal unnamed, exactly as the
		// case below: this object's recorded reopen has no proposal of its own,
		// and naming somebody else's would report it as proposed. FAIL-CLOSED:
		// a version row that records no branch names no proposal either.
	case errors.Is(err, pullrequests.ErrPullRequestNotFound):
		// The first attempt committed the version and died before opening the
		// proposal (the gap documented in this task's result). The replay
		// answers with what is recorded rather than opening a PR on a branch
		// the caller can no longer see the head of.
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
	version, err := s.objects.GetVersionByReopenRequestKey(ctx, in.ObjectID, in.IdempotencyKey)
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
// The refusal is an AUTHORIZATION outcome, not a validation one: the matrix's
// anonymous cell for reopen_main_object is `deny`
// (specs/policies/permissions-matrix.csv:15), so an actor-less call is refused
// by the same rule that refuses a stranger, with the same code, and — like
// every other refusal here — before anything is read. The transport resolves
// the principal before the command is reached, so this is the second line, not
// the only one.
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
		// Shape only. There is no list of reason codes in this build and none
		// is invented here: the value is the CALLER's and the record requires
		// the field be recorded rather than taken from a closed set. The cost
		// of the open set — V1 cannot break reopens down by reason — is named
		// in the task result rather than paid for by guessing a vocabulary.
		return fmt.Errorf("%w: reason_code must be 1..64 characters of [a-z0-9_]", ErrValidation)
	}
	explanation := strings.TrimSpace(in.Explanation)
	if explanation == "" {
		return fmt.Errorf("%w: explanation is required — a reopen is a governance decision and this command requires the record to carry a human explanation, which is not a field a machine can fill in", ErrValidation)
	}
	if len(in.Explanation) > MaxExplanationLen {
		return fmt.Errorf("%w: explanation must be at most %d characters", ErrValidation, MaxExplanationLen)
	}
	if in.IdempotencyKey == "" {
		return fmt.Errorf("%w: Idempotency-Key is required on a reopen proposal: it is what makes a repeated request return the proposal it already created instead of proposing a second reopen", ErrValidation)
	}
	if len(in.IdempotencyKey) < MinIdempotencyKeyLen {
		return fmt.Errorf("%w: Idempotency-Key must be at least %d characters (specs/api/openapi.yaml components.parameters.IdempotencyKey)", ErrValidation, MinIdempotencyKeyLen)
	}
	return nil
}

// proposalBranchName derives the proposal branch's name from the object and the
// request key: `reopen/<object uuid>-<8 hex of sha256(key)>`.
//
// Derivation rather than generation is the concurrency mechanism (see
// proposalBranch): two requests carrying one key propose one branch. The object
// id keeps the name readable, and the key hash keeps two different keys for one
// object on two branches.
func proposalBranchName(objectID, idempotencyKey string) string {
	sum := sha256.Sum256([]byte(objectID + "\x00" + idempotencyKey))
	return "reopen/" + strings.ToLower(objectID) + "-" + hex.EncodeToString(sum[:4])
}

// objectOperation builds the commit operation summary for the version the
// reopen appends: the entity is the object id and the version number the log
// position, which is what the PR gate's commit_linkage check matches against
// the rows as written.
func objectOperation(objectID, objectType string, versionNo int) domain.StateOperation {
	return domain.StateOperation{
		Kind:      domain.OperationObjectVersionCreated,
		EntityID:  objectID,
		VersionNo: versionNo,
	}
}

// auditEntry renders the reopen's audit row (docs/26 lists abort/reopen among
// the highest-risk actions that must be audited; docs/53 makes the row part of
// the action). It names the object and the version pair, the branch, the record
// this command requires, and the Idempotency-Key of the request that produced
// it.
//
// The free-text explanation IS here, unlike in the event payload: the audit log
// is the governance record and is not fanned out to subscribers, while the
// event vocabulary carries identity and reference only (docs/52's payload
// rule). The two surfaces keep their own standards.
func auditEntry(actor Actor, in Input, obj domain.ScientificObject, named domain.ScientificObjectVersion, versionNo int, rec domain.ReopenRecord) domain.AuditEntry {
	return domain.AuditEntry{
		ActorID:   actor.User.ID,
		Action:    domain.ActionScientificObjectReopened,
		TargetRef: "object:" + in.ObjectID,
		ProjectID: in.ProjectID,
		BeforeSummary: map[string]any{
			"object_id":       in.ObjectID,
			"version_no":      named.VersionNo,
			"lifecycle_state": string(named.LifecycleState),
		},
		AfterSummary: map[string]any{
			"object_id":          in.ObjectID,
			"object_type":        obj.ObjectType,
			"aborted_version_id": named.ID,
			"aborted_version_no": named.VersionNo,
			"version_no":         versionNo,
			"lifecycle_state":    string(domain.LifecycleReopened),
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
		},
		Metadata: map[string]any{
			"reopen_proposal": true,
			"reason_code":     rec.ReasonCode,
		},
	}
}

// resultFrom renders a fresh proposal. It starts from the same renderer the
// replay uses — so the two answers are the same answer — and adds what only the
// fresh path knows (the branch it just forked, the PR it just opened).
func resultFrom(projectID string, version, named domain.ScientificObjectVersion, branch domain.Branch, pr domain.PullRequest, rec domain.ReopenRecord) Result {
	out := resultFromRecorded(projectID, version)
	out.AbortedVersionID = named.ID
	out.AbortedVersionNo = named.VersionNo
	out.BranchID = branch.ID
	out.BranchName = branch.Name
	out.ReasonCode = rec.ReasonCode
	out.Explanation = rec.Explanation
	out.DecidedBy = rec.DecidedBy
	out.DecidedAt = rec.DecidedAt
	out.PullRequestNumber = pr.Number
	out.PRState = string(pr.State)
	out.Replayed = false
	return out
}

// resultFromRecorded renders a result from the version row alone. Every field
// it fills comes from what the database holds, never from the request that
// produced it, so a replay reports the recorded facts rather than the caller's
// re-statement of them.
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
	if version.Reopen != nil {
		out.AbortedVersionNo = version.VersionNo - 1
		out.ReasonCode = version.Reopen.ReasonCode
		out.Explanation = version.Reopen.Explanation
		out.DecidedBy = version.Reopen.DecidedBy
		out.DecidedAt = version.Reopen.DecidedAt
	}
	return out
}

// mapObjectReadError keeps the expected outcomes and turns everything else into
// ErrStore with the cause kept for the log.
func mapObjectReadError(err error) error {
	if errors.Is(err, sciobjects.ErrObjectNotFound) {
		return ErrObjectNotFound
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}

// mapCommitError keeps the domain outcomes the commit can raise and turns
// everything else into ErrStore. A lost compare-and-swap is ErrConflict: the
// object's version log moved between the read and the append and the request
// neither won nor replayed, so the caller may re-read and retry.
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
// ErrBranchNotActive is the one that matters at this call site: a branch that
// closed between the fork and the proposal is a conflict with what the command
// read, not a server failure.
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
