package forks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/gitprovider"
)

// Deps carries the collaborators. Authz is required: without an engine the
// service refuses every request (ErrStore) rather than assuming a class.
type Deps struct {
	Projects     ProjectGate
	Branches     BranchReader
	BranchWriter BranchCreator
	Forks        StorePort
	Repos        RepoProvisioner
	Imports      ContentImporter
	PullRequests PullRequestOpener
	Authz        authz.Engine
}

// Service implements the external contribution path.
type Service struct {
	projects ProjectGate
	branches BranchReader
	creates  BranchCreator
	links    StorePort
	repos    RepoProvisioner
	imports  ContentImporter
	prs      PullRequestOpener
	authz    authz.Engine
}

// NewService builds the service on the ports.
func NewService(deps Deps) *Service {
	return &Service{
		projects: deps.Projects,
		branches: deps.Branches,
		creates:  deps.BranchWriter,
		links:    deps.Forks,
		repos:    deps.Repos,
		imports:  deps.Imports,
		prs:      deps.PullRequests,
		authz:    deps.Authz,
	}
}

// ForkRequest is one external fork request.
type ForkRequest struct {
	// ProjectID is the project being forked (the parent). It must be
	// readable by the actor and, when the actor is not a member, public —
	// the condition the create_branch cell names for that class.
	ProjectID string
	// Name optionally names the fork project; empty derives one from the
	// parent. The fork's SLUG is never caller-supplied — see forkSlug.
	Name string
	// Purpose optionally states what the fork is for; empty derives one.
	Purpose string
	// Visibility optionally sets the fork project's visibility; empty
	// means private. Fail closed: the fork's content is the forker's own
	// until they publish it, and publishing is a governance action
	// (publish_private_to_public is the owner's), not a side effect of
	// forking.
	Visibility domain.ProjectVisibility
	// SourceBranchID names the branch of the parent project to fork; empty
	// means the parent's canonical line (main). A contributor forks the
	// line they mean to build on — a parent research branch is a research
	// line like any other — and the import measures the copy against that
	// line's own fork point, so the branch's evidence is the branch's own
	// divergence rather than the whole tree.
	SourceBranchID string
	// BranchName optionally names the branch the copy lands on in the
	// fork's project; empty means "fork/<source branch name>". The copy
	// never lands on the fork's own main: that is the fork's canonical
	// line, seeded at provisioning, and the contribution is a distinct
	// research path (docs/09 §3).
	BranchName string
}

// ImportOutcome records what a content copy did.
type ImportOutcome struct {
	// SourceSHA is the parent commit that was copied — the fork point the
	// lineage row records.
	SourceSHA string
	// TargetHeadSHA is the fork branch's head after the copy.
	TargetHeadSHA string
	// Recorded is false when the copy's delivery had already been
	// recorded (the provider's own push webhook arrived first, or an
	// earlier attempt got this far): the content is there either way, and
	// the delivery key (repository, ref, after) is what makes the second
	// record a no-op rather than a second fact.
	Recorded bool
}

// ForkResult is one fork outcome.
type ForkResult struct {
	// Fork is the lineage row — the one that exists, whether this call
	// created it or found it.
	Fork Fork
	// Project and Branch are the fork project and the fork branch the
	// copy landed on.
	Project domain.Project
	Branch  domain.Branch
	// AlreadyForked reports that (parent, actor) already had a fork, so
	// this call created no project, branch or lineage row and wrote no
	// audit row or event.
	AlreadyForked bool
	// Imported reports whether THIS call ran the content copy. It is
	// false when the fork already carried its content (Fork.ForkedSHA
	// names the commit) — a repeated request never copies twice.
	Imported bool
	// Import describes the copy, when one ran.
	Import ImportOutcome
}

// Fork creates the actor's own fork of a public project and imports the
// parent's canonical line into it.
//
// The matrix cell this resolves is create_branch for the actor's class:
// a non-member's cell is external_fork_only, and the two conditions it
// names are (a) the project being forked is public and (b) the fork is
// the actor's OWN space. Both hold by construction here — authorizeCreate-
// Branch refuses a non-public parent, and the fork project is created as
// the actor's personal project with the actor as its owner, so the fork
// grants no access to the parent and needs none.
//
// The operation is idempotent against the canonical state, not against a
// remembered request key: a repeated request finds the fork (the derived
// slug makes a second project impossible, the lineage's unique key makes a
// second lineage row impossible) and returns it, and the copy runs only
// while the lineage's forked_sha is still NULL.
func (s *Service) Fork(ctx context.Context, actor domain.User, in ForkRequest) (ForkResult, error) {
	if actor.ID == "" {
		return ForkResult{}, fmt.Errorf("%w: forking requires an authenticated actor", ErrForbidden)
	}
	if in.ProjectID == "" {
		return ForkResult{}, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	// Checked before any row is created: a fork project cannot be deleted
	// (nothing disappears), so a request that cannot finish must not start.
	if err := validateForkBranchName(in.BranchName); err != nil {
		return ForkResult{}, err
	}
	// The read gate first: a project the actor may not read answers the
	// existence-hiding not-found, which is exactly how a non-member is
	// refused on a private project before any condition is resolved.
	parent, err := s.projects.Get(ctx, projects.Reader{UserID: actor.ID, Authenticated: true}, in.ProjectID)
	if err != nil {
		return ForkResult{}, mapProjectError(err)
	}
	role, err := s.membershipRole(ctx, actor, parent.ID)
	if err != nil {
		return ForkResult{}, err
	}
	if err := s.authorizeCreateBranch(ctx, parent, role); err != nil {
		return ForkResult{}, err
	}
	// The line being forked, resolved BEFORE anything is written: a source
	// branch that does not exist (or belongs to another project — the read
	// is project-scoped and answers the same) must not leave a half-built
	// fork behind, because a project row cannot be deleted.
	source, err := s.sourceBranch(ctx, parent, in.SourceBranchID)
	if err != nil {
		return ForkResult{}, err
	}

	fork, forkProject, forkBranch, already, err := s.ensureFork(ctx, actor, parent, in, source)
	if err != nil {
		return ForkResult{}, err
	}
	result := ForkResult{Fork: fork, Project: forkProject, Branch: forkBranch, AlreadyForked: already}
	if fork.ForkedSHA != nil {
		// The fork carries its content already. Nothing to copy: the
		// recorded fork point is the row's.
		return result, nil
	}

	// The copy. What arrives is a commit in the fork's own repository,
	// inspected and recorded by the same ingestion every push goes
	// through (docs/16 §4.1: a non-push path that brings content in runs
	// the same inspection, so the branch's semantic flag is derived from
	// evidence rather than from a default).
	//
	// The line the copy is taken of is the LINEAGE's, not the request's:
	// a repeated request that names no source branch must complete the
	// fork the row describes rather than import a different line into it.
	line, err := s.branches.Get(ctx, fork.ParentProjectID, fork.SourceBranchID)
	if err != nil {
		return ForkResult{}, mapBranchError(err)
	}
	outcome, err := s.imports.Import(ctx, gitprovider.ForkImportRequest{
		SourceProjectID: fork.ParentProjectID,
		TargetProjectID: fork.ForkProjectID,
		SourceRef:       line.GitRef,
		TargetBranch:    forkBranch.Name,
		// The copy lands the fork branch on the parent's content, which is
		// a state transition of that branch — recorded in the forker's
		// name, because the fork is this request's (T0817).
		ActorID: actor.ID,
	})
	if err != nil {
		// The lineage row stands (a fork exists); the content copy is the
		// step that did not happen, and a repeated request retries it —
		// which is why the copy is guarded by forked_sha and not by this
		// call's own progress.
		return ForkResult{}, fmt.Errorf("%w: importing %s into the fork: %v", ErrStore, line.GitRef, err)
	}
	set, err := s.links.SetForkSHA(ctx, fork.ForkProjectID, outcome.SourceSHA)
	if err != nil {
		return ForkResult{}, err
	}
	if !set {
		// The cell moved between this request's claim and this write: a
		// concurrent request of the actor's own imported too, and its
		// value is the recorded one. Report the row as it stands rather
		// than this call's copy — the two copies are the same content by
		// the delivery key, so the difference is only which arrived first.
		current, ok, rerr := s.links.ForkOfProject(ctx, fork.ForkProjectID)
		if rerr != nil {
			return ForkResult{}, rerr
		}
		if !ok {
			return ForkResult{}, fmt.Errorf("%w: the fork of project %s by %s disappeared between its claim and its import", ErrStore, parent.ID, actor.ID)
		}
		fork = current
	} else {
		sha := outcome.SourceSHA
		fork.ForkedSHA = &sha
	}
	result.Fork = fork
	result.Imported = true
	result.Import = ImportOutcome{
		SourceSHA:     outcome.SourceSHA,
		TargetHeadSHA: outcome.TargetHeadSHA,
		Recorded:      outcome.Inserted,
	}
	return result, nil
}

// OpenPRRequest is one proposal to a project.
type OpenPRRequest struct {
	// ProjectID is the project the contribution proposes to. For an
	// external contribution it is the parent project, not the fork.
	ProjectID string
	// SourceBranchID names the branch the proposed changes live on. For a
	// non-member it must be a branch of the actor's own fork of ProjectID
	// — the condition the open_pr cell names for that class.
	SourceBranchID string
	// TargetBranchID names the branch the proposal merges into (the
	// parent's main, docs/09 §3).
	TargetBranchID string
	Title          string
	Body           string
	// CreationKey is the proposal's Idempotency-Key
	// (specs/api/openapi.yaml, components.parameters.IdempotencyKey):
	// empty when the caller sent none. A non-empty key is unique per
	// project — a repeated request is answered with the proposal the
	// first one opened, never with a second (migration 00089).
	CreationKey string
}

// OpenExternalPR resolves the open_pr cell for the actor and proposes
// through the EXISTING pull-request path: the same rows, the same reviews,
// the same semantic gates. The cell for a non-member is allow_from_fork,
// and the condition is resolved against the lineage — the source branch
// must belong to a fork of this project that THIS actor forked. The
// database re-checks the same fact for any insert path
// (pull_request_fork_gate, 00086), so the condition cannot be lost by a
// caller that does not come through here.
func (s *Service) OpenExternalPR(ctx context.Context, actor domain.User, in OpenPRRequest) (domain.PullRequest, error) {
	if actor.ID == "" {
		return domain.PullRequest{}, fmt.Errorf("%w: opening a pull request requires an authenticated actor", ErrForbidden)
	}
	if in.ProjectID == "" || in.SourceBranchID == "" || in.TargetBranchID == "" {
		return domain.PullRequest{}, fmt.Errorf("%w: project_id, source_branch_id and target_branch_id are required", ErrValidation)
	}
	project, err := s.projects.Get(ctx, projects.Reader{UserID: actor.ID, Authenticated: true}, in.ProjectID)
	if err != nil {
		return domain.PullRequest{}, mapProjectError(err)
	}
	role, err := s.membershipRole(ctx, actor, project.ID)
	if err != nil {
		return domain.PullRequest{}, err
	}
	if err := s.authorizeOpenPR(ctx, actor, project, role, in.SourceBranchID); err != nil {
		return domain.PullRequest{}, err
	}
	pr, err := s.prs.Create(ctx, pullrequests.CreatePullRequestParams{
		ProjectID:      project.ID,
		SourceBranchID: in.SourceBranchID,
		TargetBranchID: in.TargetBranchID,
		Title:          in.Title,
		Body:           in.Body,
		CreatedBy:      actor.ID,
		CreationKey:    in.CreationKey,
	})
	if err != nil {
		return domain.PullRequest{}, mapPullRequestError(err)
	}
	return pr, nil
}

// Lineage returns a project's forks, newest first. Each row carries the
// canonical relation name ('forked_from', docs/44) — the lineage is a
// stored fact, not something a reader infers from the table's name — and
// the fork point the import recorded.
//
// The read runs the project's read gate first: a derived fact is never
// more visible than its subject (docs/12 §3), so a project the caller may
// not read answers the existence-hiding not-found rather than an empty
// lineage.
func (s *Service) Lineage(ctx context.Context, reader projects.Reader, projectID string) ([]Fork, error) {
	if projectID == "" {
		return nil, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	if _, err := s.projects.Get(ctx, reader, projectID); err != nil {
		return nil, mapProjectError(err)
	}
	list, err := s.links.ListForks(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return list, nil
}

// ensureFork makes the fork exist and reports it, with already=true when
// it was there before this call. source is the parent line being forked,
// resolved by the caller before this ran (a request that cannot be served
// must not start creating rows).
//
// The order is fixed by the schema and by what each step needs:
//
//	project → repository → branch → lineage
//
// The FIRST step is the compare-and-swap. The fork project's slug is
// derived from the parent and the actor (forkSlug) and personal project
// slugs are globally unique (projects_personal_slug_idx, 00019), so two
// requests for the same (parent, actor) cannot both insert a project: the
// loser is answered ErrSlugTaken and reads back what the winner is
// building. That is why the slug is derived rather than caller-supplied —
// a caller-chosen slug would let a repeated request create a SECOND
// project before the lineage's unique key refused it, and a project row
// cannot be deleted (nothing disappears; state only evolves).
//
// The loose end that compare-and-swap has is that the name it arbitrates
// on is not a secret: it is computable from public facts (the parent's
// slug and id, the actor's handle and id), so a third party can register
// "<parent slug>-<handle>-<pair digest>" before the actor forks, and the
// actor's own older project can already hold it. Either way the name is
// gone for good (nothing is deleted), so a fork that only ever tried the
// derived name would be deadlocked by a name its own owner cannot free —
// and reporting that as "a fork of yours is being created" would be a
// claim no row supports. On a lost insert the holder's creator therefore
// decides:
//
//   - held by ANOTHER actor (forkProjectOverTakenName): provably not this
//     pair's fork project, because a fork project is created by the actor
//     who forks. The fork moves to the pair's reserved name (forkSlugReserve)
//     and succeeds there.
//   - held by THIS actor with a lineage row: the pair's fork, at whichever
//     name it was created — answered, nothing written (the repeat path).
//   - held by THIS actor with no lineage row: a fork request of theirs
//     between its project insert and its lineage insert, or one of their own
//     projects that merely holds the name. The record cannot tell them
//     apart, so neither is claimed and no second project is created
//     (ErrForkSlugTaken) — creating one would be the single way to break
//     "one fork project per pair", which is why this is where the fork
//     stops rather than escalates.
func (s *Service) ensureFork(ctx context.Context, actor domain.User, parent domain.Project, in ForkRequest, source domain.Branch) (Fork, domain.Project, domain.Branch, bool, error) {
	forkProject, _, err := s.projects.Create(ctx, actor, forkCreateInput(parent, in, forkSlug(parent.Slug, actor, parent.ID)))
	if errors.Is(err, projects.ErrSlugTaken) {
		// The pair's own fork — if one is recorded — is the answer, whether
		// it was created at the derived name or at the reserved one.
		existing, existingProject, existingBranch, ok, rerr := s.recordedFork(ctx, actor, parent.ID)
		if rerr != nil {
			return Fork{}, domain.Project{}, domain.Branch{}, false, rerr
		}
		if ok {
			return existing, existingProject, existingBranch, true, nil
		}
		forkProject, err = s.forkProjectOverTakenName(ctx, actor, parent, in)
		if errors.Is(err, errReservedNameTaken) {
			// The reserved name is taken too. A fork that appeared while this
			// request ran is still the answer; otherwise the pair's fork
			// cannot be created under either of the names derived for it, and
			// that is the fact to report — no request of this pair is
			// described as being in flight.
			existing, existingProject, existingBranch, ok, rerr := s.recordedFork(ctx, actor, parent.ID)
			if rerr != nil {
				return Fork{}, domain.Project{}, domain.Branch{}, false, rerr
			}
			if ok {
				return existing, existingProject, existingBranch, true, nil
			}
			return Fork{}, domain.Project{}, domain.Branch{}, false, fmt.Errorf(
				"%w: neither %s nor the reserved name %s is available for a fork of %s by %s, and no fork of that pair is recorded",
				ErrForkSlugTaken, forkSlug(parent.Slug, actor, parent.ID),
				forkSlugReserve(parent.Slug, actor, parent.ID), parent.ID, actor.ID)
		}
	}
	if err != nil {
		return Fork{}, domain.Project{}, domain.Branch{}, false, mapProjectError(err)
	}

	// The fork's repository must exist before anything can be imported
	// into it. Provisioning is the ordinary path — the fork project is a
	// project like any other, in the actor's own space — and it is
	// idempotent.
	if err := s.repos.Provision(ctx, forkProject.ID); err != nil {
		return Fork{}, domain.Project{}, domain.Branch{}, false, fmt.Errorf("%w: provisioning the fork's repository: %v", ErrStore, err)
	}
	branchName, err := forkBranchName(in.BranchName, source)
	if err != nil {
		return Fork{}, domain.Project{}, domain.Branch{}, false, err
	}
	forkBranch, err := s.creates.CreateBranch(ctx, actor, forkProject.ID, rsg.CreateBranchInput{
		Name:       branchName,
		Visibility: branchVisibility(forkProject.Visibility),
		Purpose:    forkBranchPurpose(parent, source),
	})
	if err != nil {
		return Fork{}, domain.Project{}, domain.Branch{}, false, mapBranchError(err)
	}
	fork, inserted, err := s.links.ClaimFork(ctx, ClaimRequest{
		ForkProjectID:   forkProject.ID,
		ParentProjectID: parent.ID,
		ActorID:         actor.ID,
		SourceBranchID:  source.ID,
		ForkBranchID:    forkBranch.ID,
		Audit:           forkAudit(actor, parent, forkProject, source, forkBranch),
	})
	if err != nil {
		return Fork{}, domain.Project{}, domain.Branch{}, false, err
	}
	if !inserted {
		// The unique key refused the claim for a pair this request has
		// just created a project for. With derived slugs the project
		// insert above would have lost first, so this is an invariant
		// violation, not a flow: report it rather than return a fork
		// whose identity this call cannot vouch for.
		return Fork{}, domain.Project{}, domain.Branch{}, false, fmt.Errorf(
			"%w: the lineage of (%s, %s) was already claimed while this request was creating project %s",
			ErrStore, parent.ID, actor.ID, forkProject.ID)
	}
	return fork, forkProject, forkBranch, false, nil
}

// loadFork reads the rows a repeated request reports, without writing
// anything: the fork project (through the read gate — the actor owns it)
// and the fork branch.
func (s *Service) loadFork(ctx context.Context, actor domain.User, fork Fork) (domain.Project, domain.Branch, error) {
	project, err := s.projects.Get(ctx, projects.Reader{UserID: actor.ID, Authenticated: true}, fork.ForkProjectID)
	if err != nil {
		return domain.Project{}, domain.Branch{}, mapProjectError(err)
	}
	branch, err := s.branches.Get(ctx, fork.ForkProjectID, fork.ForkBranchID)
	if err != nil {
		return domain.Project{}, domain.Branch{}, mapBranchError(err)
	}
	return project, branch, nil
}

// recordedFork answers the fork the (parent, actor) pair has, when one is
// recorded: the lineage row and the rows a repeated request reports. ok is
// false when the pair has no fork, which is what turns a lost slug insert
// from a race into a decision (ensureFork).
func (s *Service) recordedFork(ctx context.Context, actor domain.User, parentID string) (Fork, domain.Project, domain.Branch, bool, error) {
	fork, ok, err := s.links.FindFork(ctx, parentID, actor.ID)
	if err != nil {
		return Fork{}, domain.Project{}, domain.Branch{}, false, err
	}
	if !ok {
		return Fork{}, domain.Project{}, domain.Branch{}, false, nil
	}
	project, branch, err := s.loadFork(ctx, actor, fork)
	if err != nil {
		return Fork{}, domain.Project{}, domain.Branch{}, false, err
	}
	return fork, project, branch, true, nil
}

// errReservedNameTaken reports that the fork's reserved name is taken as
// well as its derived one. It is an internal signal to ensureFork, which
// turns it into the request's answer after one last look at the lineage; it
// never reaches a caller.
var errReservedNameTaken = errors.New("forks: the fork's reserved name is taken as well")

// forkProjectOverTakenName creates the fork project when the derived name
// is held by a project this pair did not create, and reports the fact when
// it is held by one the pair might be building.
//
// The holder's creator is the whole decision, and it is sound because
// creating a project records its creator: a project created by another
// actor cannot be this pair's fork project, so no concurrent request of
// this pair is the holder and moving to the reserved name cannot produce a
// second project for the pair. The reserved name is derived from the pair
// alone (forkSlugReserve), so two requests of one pair that both reach this
// function derive the SAME name: the unique index still arbitrates, exactly
// as it does on the derived name, and one of them wins.
//
// A holder created by THIS actor is deliberately not escalated past. The
// fork project of a pair is created before its lineage row, so a holder
// created by this actor and a request of theirs in flight look identical in
// the record — and escalating on that ambiguity is the one way to end up
// with two fork projects for one pair (the second wins the reserved name
// while the first is between its writes, and the first's lineage claim then
// refuses it, leaving a project row that cannot be deleted). So the
// ambiguity is answered, not resolved: ErrForkSlugTaken states what is true
// of the record rather than asserting a fork that may not exist.
func (s *Service) forkProjectOverTakenName(ctx context.Context, actor domain.User, parent domain.Project, in ForkRequest) (domain.Project, error) {
	derived := forkSlug(parent.Slug, actor, parent.ID)
	holder, held, err := s.links.PersonalProjectCreator(ctx, derived)
	if err != nil {
		return domain.Project{}, err
	}
	if !held {
		// The insert lost to a name no project answers to. A NULL-organization
		// insert can only violate the personal-slug index, and its holder
		// cannot be deleted, so this is not a state this code can act on:
		// refuse rather than guess.
		return domain.Project{}, fmt.Errorf("%w: %s is taken but no project answers to it", ErrStore, derived)
	}
	if holder == actor.ID {
		return domain.Project{}, fmt.Errorf(
			"%w: project %s already exists and belongs to %s, and no fork of %s by them is recorded — either it is a fork request of theirs still in flight (a retry will find the fork once it finishes) or one of their own projects holding the name; the record cannot tell the two apart",
			ErrForkSlugTaken, derived, actor.ID, parent.ID)
	}
	reserved := forkSlugReserve(parent.Slug, actor, parent.ID)
	project, _, err := s.projects.Create(ctx, actor, forkCreateInput(parent, in, reserved))
	if err == nil {
		return project, nil
	}
	if !errors.Is(err, projects.ErrSlugTaken) {
		return domain.Project{}, err
	}
	return domain.Project{}, fmt.Errorf("%w: %s", errReservedNameTaken, reserved)
}

// forkCreateInput is the project-create call a fork makes, under whichever
// of its two derived names is being tried. The fork's SLUG is derived and
// never caller-supplied (forkSlug); the rest of the shape is the caller's,
// with the fail-closed defaults.
func forkCreateInput(parent domain.Project, in ForkRequest, slug string) projects.CreateProjectInput {
	return projects.CreateProjectInput{
		Slug:       slug,
		Name:       forkName(parent, in.Name),
		Purpose:    forkPurpose(parent, in.Purpose),
		Visibility: forkVisibility(in.Visibility),
	}
}

// membershipRole returns the actor's role in projectID, nil when they hold
// no membership. The read is the projects service's (it runs the project
// read gate first), so "no role" is distinguished from "cannot see the
// project" — the latter answers ErrProjectNotFound and never reaches the
// matrix.
func (s *Service) membershipRole(ctx context.Context, actor domain.User, projectID string) (*domain.ProjectRole, error) {
	membership, err := s.projects.GetMembership(ctx, actor, projectID)
	switch {
	case err == nil:
		role := membership.Role
		return &role, nil
	case errors.Is(err, projects.ErrMemberNotFound):
		return nil, nil
	default:
		return nil, mapProjectError(err)
	}
}

// authorizeCreateBranch resolves the create_branch cell for the fork.
//
// A member's cell is a plain allow (contributor and above may create
// branches); a non-member's is external_fork_only, which permits the
// branch creation the fork performs — a branch in the actor's OWN project,
// not in the parent — only while the project being forked is public. The
// read gate above has already refused an unreadable parent, so the check
// is the condition stated where it is resolved rather than inferred from
// the gate's current behaviour.
func (s *Service) authorizeCreateBranch(ctx context.Context, parent domain.Project, role *domain.ProjectRole) error {
	if s.authz == nil {
		return fmt.Errorf("%w: no policy engine configured", ErrStore)
	}
	decision, err := s.authz.Authorize(ctx, authz.Request{
		Action: authz.ActionCreateBranch,
		Class:  authz.ClassOf(true, role, false),
	})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrStore, err)
	}
	switch {
	case decision.Permits():
		return nil
	case decision.Verdict == authz.VerdictExternalForkOnly:
		if parent.Visibility != domain.VisibilityPublic {
			return fmt.Errorf("%w: create_branch is external_fork_only for a non-member, and project %s is not public",
				ErrForbidden, parent.ID)
		}
		return nil
	default:
		return fmt.Errorf("%w: create_branch on project %s is %s for this actor", ErrForbidden, parent.ID, decision.Verdict)
	}
}

// authorizeOpenPR resolves the open_pr cell for the proposal.
//
// A member's cell is a plain allow; a non-member's is allow_from_fork,
// and the condition is the lineage: the source branch must belong to a
// fork of THIS project that THIS actor forked. A branch of the parent
// itself, of an unrelated project, or of somebody else's fork all answer
// ErrForbidden — the count of projects does not grow, and the second
// person to fork a popular project cannot propose from the first one's
// work.
func (s *Service) authorizeOpenPR(ctx context.Context, actor domain.User, project domain.Project, role *domain.ProjectRole, sourceBranchID string) error {
	if s.authz == nil {
		return fmt.Errorf("%w: no policy engine configured", ErrStore)
	}
	decision, err := s.authz.Authorize(ctx, authz.Request{
		Action: authz.ActionOpenPR,
		Class:  authz.ClassOf(true, role, false),
	})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrStore, err)
	}
	switch {
	case decision.Permits():
		return nil
	case decision.Verdict == authz.VerdictAllowFromFork:
		fork, ok, ferr := s.links.ForkOfBranch(ctx, sourceBranchID)
		if ferr != nil {
			return ferr
		}
		if !ok || fork.ParentProjectID != project.ID || fork.ForkedBy != actor.ID {
			return fmt.Errorf("%w: open_pr is allow_from_fork for a non-member: branch %s must belong to the fork of project %s that this actor forked",
				ErrForbidden, sourceBranchID, project.ID)
		}
		return nil
	default:
		return fmt.Errorf("%w: open_pr on project %s is %s for this actor", ErrForbidden, project.ID, decision.Verdict)
	}
}

// sourceBranch resolves the parent line a fork is taken of: the named
// branch, or the parent's canonical line (the branch named main,
// domain.MainBranchName) when the caller names none. Both are read, never
// assumed — a branch is created like any other — and the read is
// project-scoped, so a branch of another project answers not-found. A
// project without a main has nothing to fork (ErrNoSourceBranch).
func (s *Service) sourceBranch(ctx context.Context, parent domain.Project, branchID string) (domain.Branch, error) {
	if strings.TrimSpace(branchID) != "" {
		branch, err := s.branches.Get(ctx, parent.ID, strings.TrimSpace(branchID))
		if err != nil {
			return domain.Branch{}, mapBranchError(err)
		}
		return branch, nil
	}
	list, err := s.branches.List(ctx, parent.ID)
	if err != nil {
		return domain.Branch{}, mapBranchError(err)
	}
	for _, branch := range list {
		if branch.Name == domain.MainBranchName {
			return branch, nil
		}
	}
	return domain.Branch{}, fmt.Errorf("%w: project %s has no %s branch", ErrNoSourceBranch, parent.ID, domain.MainBranchName)
}

// forkSlug derives the fork project's slug from the parent and the actor.
//
// Derived, not caller-supplied, because the slug is the fork's
// idempotency key: a personal project's slug is globally unique (00019),
// so a repeated request cannot create a second project — the insert
// itself is the compare-and-swap, before any other row exists to be
// duplicated.
//
// The shape carries the pair's digest ALWAYS, not only when the plain
// "<parent slug>-<actor handle>" overflows the 64-character bound
// (maxForkSlug, the domain's own). The long-name case is still what the
// shortening is for — the parent's slug and the actor's handle are both
// cut around forkPairDigest(parentID, actorID), so the result is a valid
// project slug at any handle length the domain accepts and still tells
// apart two long-named parents for one actor and two actors whose handles
// share a long prefix.
//
// What the digest adds on the SHORT names is the reason it is
// unconditional. A personal-slug index is global, but a parent's slug is
// not: a project's slug is unique per organization (00019's
// projects_personal_slug_idx covers only organization_id IS NULL), so two
// organizations may each hold a "mof-curie" — and with the plain shape,
// one actor forking both derives the SAME name twice. The second fork
// then loses the insert to the fork of the FIRST one, which is the
// actor's own project: forkProjectOverTakenName answers ErrForkSlugTaken
// for a holder this actor created, because the record cannot tell an
// in-flight fork of theirs from a project that merely holds the name. A
// slug is not editable and a project row is not deletable
// (internal/application/projects/settings.go: UpdateSettingsInput has no
// name/slug), so that answer would be PERMANENT: a legal fork of a
// readable project, refused forever by a name the actor cannot free.
// Deriving from the pair (parent id, actor id) makes two same-named
// parents two different names, so the deadlock cannot arise, and the
// "held by another actor" escalation to forkSlugReserve keeps its
// meaning: it is about a name somebody else registered, not about the
// actor's own earlier fork.
func forkSlug(parentSlug string, actor domain.User, parentID string) string {
	return forkSlugDigest(slugToken(parentSlug), forkHandle(actor), forkPairDigest(parentID, actor.ID), "")
}

// forkSlugReserve is the name a fork falls back to when its derived name is
// held by a project this pair did not create (forkProjectOverTakenName).
//
// It is derived from the pair and nothing else — the same parent, the same
// actor, the same value every time — so two concurrent requests of one pair
// derive the SAME reserved name: whoever inserts first wins it and the
// other is answered by the lineage. A random or open-ended name would let
// both requests insert, and a fork project row cannot be deleted (nothing
// disappears; state only evolves).
func forkSlugReserve(parentSlug string, actor domain.User, parentID string) string {
	return forkSlugDigest(slugToken(parentSlug), forkHandle(actor), forkPairDigest(parentID, actor.ID), forkReserveMark)
}

// forkReserveMark separates the reserved name from the derived one for the
// same pair: the two shapes never coincide, so trying the reserved name
// after the derived one can actually succeed.
const forkReserveMark = "-2"

// forkHandle is the actor's part of a fork's slug: their handle, or their
// id when the handle carries nothing usable.
func forkHandle(actor domain.User) string {
	if handle := slugToken(actor.Handle); handle != "" {
		return handle
	}
	// No handle to name the fork by: the actor's id, which is stable and
	// unique, in its compact form.
	return slugToken(strings.ReplaceAll(actor.ID, "-", ""))
}

// forkPairDigest reduces the pair (parent id, actor id) to eight hex
// characters. BOTH ids go in, and both are needed whatever the name's
// length:
//
//   - the parent's id, because a slug is unique per organization and two
//     organizations may hold the same one — the plain
//     "<parent slug>-<handle>" would collide for one actor forking both
//     (see forkSlug);
//   - the actor's id, because two actors' handles can be cut to the same
//     prefix by the shortening below.
//
// A digest collision — 32 bits, and it would have to land on the same pair
// of name parts as well — costs a fork its derived name, not its
// identity: it escalates to the reserved name or is answered with
// ErrForkSlugTaken, and the number of forks per pair is decided by the
// slug's unique index, never by this digest.
func forkPairDigest(parentID, actorID string) string {
	sum := sha256.Sum256([]byte(parentID + "\x00" + actorID))
	return hex.EncodeToString(sum[:4])
}

// forkSlugDigest renders the fork's name: as much of the actor's handle as
// it needs (the handle is what names the fork to its owner), as much of the
// parent's slug as is then left, the pair's digest and the reserve mark
// when this is the reserved name. Every fork name is rendered here, short
// ones included — the digest is what makes two same-named parents two
// names (forkSlug), and shortening is what keeps the result inside the
// bound for the long ones.
//
// The budget is computed from the tail that must fit — not guessed — and
// the handle is cut to it, so the result is at most maxForkSlug characters
// for every handle domain.ValidHandle allows, a 64-character one included,
// and always at least one character of the parent's slug survives.
func forkSlugDigest(head, handle, digest, mark string) string {
	tail := "-" + digest + mark
	room := maxForkSlug - len(tail) - 1 // the dash that joins head and handle
	// The handle keeps the room it needs, one character short of all of it:
	// the parent's slug keeps at least one character, so a shortened fork
	// name still says what it is a fork OF.
	actorRoom := room - 1
	if len(handle) > actorRoom {
		handle = handle[:actorRoom]
	}
	parentRoom := room - len(handle)
	if len(head) > parentRoom {
		head = head[:parentRoom]
	}
	// head is a slug token, so if it survived at all it starts with a letter
	// or digit: trimming a dash it may now end with cannot empty it.
	return strings.Trim(head, "-") + "-" + handle + tail
}

// maxForkSlug is the project slug bound (domain.ValidProjectSlug).
const maxForkSlug = 64

// slugToken reduces a value to the slug alphabet: lowercase letters,
// digits and dashes, with no leading or trailing dash. Anything else
// becomes a dash, and runs of dashes collapse, so a handle or a slug that
// already follows the rules is returned unchanged.
func slugToken(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r == '-' || r == '.' || r == '_' || r == ' ':
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// forkName derives the fork project's name when the caller states none.
func forkName(parent domain.Project, name string) string {
	if strings.TrimSpace(name) != "" {
		return truncate(name, 200)
	}
	return truncate(strings.TrimSpace(parent.Name)+" (fork)", 200)
}

// forkPurpose derives the fork project's purpose when the caller states
// none: the fork's reason to exist is to propose back to the parent, and
// the stated purpose records which project that is.
func forkPurpose(parent domain.Project, purpose string) string {
	if strings.TrimSpace(purpose) != "" {
		return truncate(purpose, 4000)
	}
	return truncate(fmt.Sprintf(
		"External fork of %s (project %s). Changes are made here and proposed back to the parent project as a research pull request.",
		strings.TrimSpace(parent.Name), parent.Slug), 4000)
}

// forkVisibility applies the fail-closed default: only the canonical
// public value widens it, everything else (including "unset") is private.
func forkVisibility(v domain.ProjectVisibility) domain.ProjectVisibility {
	if v == domain.VisibilityPublic {
		return domain.VisibilityPublic
	}
	return domain.VisibilityPrivate
}

// forkBranchName derives the branch the copy lands on. It is never the
// fork's own main: main is the fork's canonical line (seeded at
// provisioning, docs/09 §3) and the contribution is a separate research
// path, which is also what keeps the import off a protected ref.
func forkBranchName(name string, source domain.Branch) (string, error) {
	if err := validateForkBranchName(name); err != nil {
		return "", err
	}
	if trimmed := strings.TrimSpace(name); trimmed != "" {
		return trimmed, nil
	}
	return "fork/" + source.Name, nil
}

// validateForkBranchName refuses the names the copy may not land on. The
// fork's main is its canonical line, seeded at provisioning and protected
// on the provider side (docs/09 §3: main advances only through a Research
// PR merge) — an import that targeted it would be a direct write to a
// protected ref and would rewrite the line the fork's own proposals are
// measured against.
func validateForkBranchName(name string) error {
	if strings.TrimSpace(name) == domain.MainBranchName {
		return fmt.Errorf("%w: a fork's copy never lands on the fork's own %s (docs/09 §3); name the contribution branch instead",
			ErrValidation, domain.MainBranchName)
	}
	return nil
}

// branchVisibility mirrors the fork project's own visibility — the branch
// preset rule (docs/09 §1): a private fork gets private branches, and a
// fork its owner chose to make public gets public ones.
func branchVisibility(project domain.ProjectVisibility) domain.BranchVisibility {
	if project == domain.VisibilityPublic {
		return domain.BranchVisibilityPublic
	}
	return domain.BranchVisibilityPrivate
}

// forkBranchPurpose states why the branch exists, so the fork's research
// path reads as one from its first row.
func forkBranchPurpose(parent domain.Project, source domain.Branch) *string {
	purpose := truncate(fmt.Sprintf(
		"Fork of %s@%s: imported from project %s and proposed back as a research pull request.",
		parent.Slug, source.Name, parent.Slug), 500)
	return &purpose
}

// truncate cuts s to at most n bytes on a rune boundary and trims the
// result, so a derived value never fails the domain's shape checks because
// of a long parent name. A partial rune at the cut is dropped: the tail
// must decode as whole runes, which a byte-wise cut does not guarantee
// (cutting "研研研研" at 10 bytes leaves three runes plus the first byte of
// a fourth).
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for len(cut) > 0 {
		r, size := utf8.DecodeLastRuneInString(cut)
		if r != utf8.RuneError || size > 1 {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return strings.TrimSpace(cut)
}

// mapProjectError keeps the projects service's outcomes in this package's
// vocabulary: a project the caller may not read stays an existence-hiding
// not-found, a refusal stays a refusal, and the fork's own names for a lost
// slug (ErrForkSlugTaken, decided on the row's creator, not by the projects
// service) pass through unchanged — anything else is a store failure.
func mapProjectError(err error) error {
	switch {
	case err == nil,
		errors.Is(err, ErrProjectNotFound),
		errors.Is(err, ErrForbidden),
		errors.Is(err, ErrForkSlugTaken),
		errors.Is(err, ErrStore):
		return err
	case errors.Is(err, projects.ErrProjectNotFound):
		return ErrProjectNotFound
	case errors.Is(err, projects.ErrForbidden):
		return ErrForbidden
	case errors.Is(err, projects.ErrValidation):
		return fmt.Errorf("%w: %v", ErrValidation, err)
	default:
		return fmt.Errorf("%w: %v", ErrStore, err)
	}
}

// mapBranchError keeps the branch service's not-found (a branch that does
// not exist, or belongs to another project) and turns anything else into a
// store failure.
func mapBranchError(err error) error {
	switch {
	case err == nil,
		errors.Is(err, ErrValidation),
		errors.Is(err, ErrStore),
		errors.Is(err, ErrBranchNotFound):
		return err
	case errors.Is(err, ErrNoSourceBranch):
		return err
	case errors.Is(err, branches.ErrBranchNotFound):
		return fmt.Errorf("%w: %v", ErrBranchNotFound, err)
	default:
		return fmt.Errorf("%w: %v", ErrStore, err)
	}
}

// mapPullRequestError passes the existing pull-request path's domain
// outcomes through unchanged — the caller of an external proposal sees the
// same answers an internal one does — and reports anything else as a store
// failure.
//
// ErrBranchUnstructuredChanges is one of the passed-through outcomes, and
// it is the one this function exists for as much as the others: an
// external contributor's fork branch is just as capable of carrying
// unparseable content as an internal one (docs/16 §4), the database gate
// refuses the insert either way (00042), and the contributor is the one
// who has to fill the semantics in. Folding it into ErrStore would tell
// them the platform is down.
func mapPullRequestError(err error) error {
	switch {
	case err == nil,
		errors.Is(err, pullrequests.ErrValidation),
		errors.Is(err, pullrequests.ErrBranchNotFound),
		errors.Is(err, pullrequests.ErrBranchNotActive),
		errors.Is(err, pullrequests.ErrBranchHeadMissing),
		errors.Is(err, pullrequests.ErrBranchUnstructuredChanges),
		errors.Is(err, projects.ErrProjectNotFound),
		errors.Is(err, ErrForbidden):
		return err
	default:
		// Both verbs are %w here, and only here: the contribution goes
		// through the SAME PR service and the same database gates as an
		// internal proposal, and the gate that refuses a proposal from an
		// incomplete import (P0001) has to stay readable to the caller —
		// "store failure" alone cannot say whether the branch's evidence or
		// the database was the problem.
		return fmt.Errorf("%w: %w", ErrStore, err)
	}
}
