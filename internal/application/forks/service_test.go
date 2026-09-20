package forks_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/forks"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/gitprovider"
)

// ---------------------------------------------------------------- fixtures

const (
	parentID   = "11111111-1111-4111-8111-111111111111"
	forkID     = "22222222-2222-4222-8222-222222222222"
	otherID    = "33333333-3333-4333-8333-333333333333"
	mainID     = "44444444-4444-4444-8444-444444444444"
	researchID = "55555555-5555-4555-8555-555555555555"
	forkBrID   = "66666666-6666-4666-8666-666666666666"
	actorID    = "77777777-7777-4777-8777-777777777777"
	otherActor = "88888888-8888-4888-8888-888888888888"
)

func publicParent() domain.Project {
	return domain.Project{ID: parentID, Slug: "mof-gas", Name: "MOF gas separation", Visibility: domain.VisibilityPublic}
}

func privateParent() domain.Project {
	return domain.Project{ID: parentID, Slug: "mof-gas", Name: "MOF gas separation", Visibility: domain.VisibilityPrivate}
}

func mainBranch() domain.Branch {
	return domain.Branch{ID: mainID, ProjectID: parentID, Name: domain.MainBranchName, GitRef: "refs/heads/main", Lifecycle: domain.BranchLifecycleActive}
}

func researchBranch() domain.Branch {
	return domain.Branch{ID: researchID, ProjectID: parentID, Name: "feature-x", GitRef: "refs/heads/feature-x", Lifecycle: domain.BranchLifecycleActive}
}

func actor() domain.User { return domain.User{ID: actorID, Handle: "curie", DisplayName: "Marie"} }

// forkSlugFor spells out the rule the fork applies to the slug: the parent's
// slug, the actor's handle, and the digest of the pair (parent id, actor id)
// — sha256 over the two, NUL-separated, first four bytes in hex. The tests
// below assert on the VALUE the fork inserts, so the expectation is derived
// here from the rule rather than by calling the derivation under test.
//
// It is the SHORT-name spelling: the fixtures' "mof-gas" and "curie" leave
// the name far inside the 64-character bound, so the shortening that the
// long-name tests measure does not bite here.
func forkSlugFor(parentSlug, handle, parentID, userID string) string {
	sum := sha256.Sum256([]byte(parentID + "\x00" + userID))
	return parentSlug + "-" + handle + "-" + hex.EncodeToString(sum[:4])
}

// ---------------------------------------------------------------- fakes

type fakeProjects struct {
	project       domain.Project
	getErr        error
	membership    *domain.ProjectMembership
	memberErr     error
	createProject domain.Project
	createErr     error
	// createErrFor fails the create of ONE slug, which is what a name
	// somebody else registered first looks like to the fork service.
	createErrFor map[string]error
	createCalls  []projects.CreateProjectInput
	getCalls     int
	memberCalls  int
}

func (f *fakeProjects) Get(_ context.Context, _ projects.Reader, _ string) (domain.Project, error) {
	f.getCalls++
	if f.getErr != nil {
		return domain.Project{}, f.getErr
	}
	return f.project, nil
}

func (f *fakeProjects) GetMembership(_ context.Context, _ domain.User, _ string) (domain.ProjectMembership, error) {
	f.memberCalls++
	if f.memberErr != nil {
		return domain.ProjectMembership{}, f.memberErr
	}
	if f.membership == nil {
		return domain.ProjectMembership{}, projects.ErrMemberNotFound
	}
	return *f.membership, nil
}

func (f *fakeProjects) Create(_ context.Context, _ domain.User, in projects.CreateProjectInput) (domain.Project, domain.ProjectMembership, error) {
	f.createCalls = append(f.createCalls, in)
	if err, ok := f.createErrFor[in.Slug]; ok {
		return domain.Project{}, domain.ProjectMembership{}, err
	}
	if f.createErr != nil {
		return domain.Project{}, domain.ProjectMembership{}, f.createErr
	}
	created := f.createProject
	created.Slug = in.Slug
	created.Name = in.Name
	created.Purpose = in.Purpose
	created.Visibility = in.Visibility
	return created, domain.ProjectMembership{ProjectID: created.ID, UserID: actorID, Role: domain.ProjectRoleOwner}, nil
}

type fakeBranches struct {
	byID     map[string]domain.Branch
	list     []domain.Branch
	getErr   error
	listErr  error
	getCalls []string
}

func (f *fakeBranches) Get(_ context.Context, projectID, branchID string) (domain.Branch, error) {
	f.getCalls = append(f.getCalls, projectID+"/"+branchID)
	if f.getErr != nil {
		return domain.Branch{}, f.getErr
	}
	branch, ok := f.byID[branchID]
	if !ok || branch.ProjectID != projectID {
		return domain.Branch{}, branches.ErrBranchNotFound
	}
	return branch, nil
}

func (f *fakeBranches) List(_ context.Context, _ string) ([]domain.Branch, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.list, nil
}

type fakeBranchCreator struct {
	branch     domain.Branch
	err        error
	projectIDs []string
	inputs     []rsg.CreateBranchInput
}

func (f *fakeBranchCreator) CreateBranch(_ context.Context, _ domain.User, projectID string, in rsg.CreateBranchInput) (domain.Branch, error) {
	f.projectIDs = append(f.projectIDs, projectID)
	f.inputs = append(f.inputs, in)
	if f.err != nil {
		return domain.Branch{}, f.err
	}
	out := f.branch
	out.ProjectID = projectID
	out.Name = in.Name
	return out, nil
}

type fakeForks struct {
	claim         forks.Fork
	claimInserted bool
	claimErr      error
	claims        []forks.ClaimRequest

	find       forks.Fork
	findOK     bool
	findErr    error
	projectRow forks.Fork
	projectOK  bool
	projectErr error
	branchRow  forks.Fork
	branchOK   bool
	branchErr  error

	list    []forks.Fork
	listErr error

	setOK    bool
	setErr   error
	setCalls [][2]string

	// slugCreator/slugHeld are what the personal-slug read answers: whether
	// a project holds the slug the fork derived, and who created it.
	slugCreator string
	slugHeld    bool
	slugErr     error
}

func (f *fakeForks) ClaimFork(_ context.Context, req forks.ClaimRequest) (forks.Fork, bool, error) {
	f.claims = append(f.claims, req)
	if f.claimErr != nil {
		return forks.Fork{}, false, f.claimErr
	}
	// A real insert echoes the request back (the row IS the request); the
	// fixture only pins the cells a test wants to state.
	out := f.claim
	out.ForkProjectID = req.ForkProjectID
	out.ParentProjectID = req.ParentProjectID
	out.ForkedBy = req.ActorID
	out.SourceBranchID = req.SourceBranchID
	out.ForkBranchID = req.ForkBranchID
	if out.RelationType == "" {
		out.RelationType = "forked_from"
	}
	return out, f.claimInserted, nil
}

func (f *fakeForks) FindFork(context.Context, string, string) (forks.Fork, bool, error) {
	return f.find, f.findOK, f.findErr
}

func (f *fakeForks) ForkOfProject(context.Context, string) (forks.Fork, bool, error) {
	return f.projectRow, f.projectOK, f.projectErr
}

func (f *fakeForks) ForkOfBranch(context.Context, string) (forks.Fork, bool, error) {
	return f.branchRow, f.branchOK, f.branchErr
}

func (f *fakeForks) ListForks(context.Context, string) ([]forks.Fork, error) {
	return f.list, f.listErr
}

func (f *fakeForks) SetForkSHA(_ context.Context, forkProjectID, sha string) (bool, error) {
	f.setCalls = append(f.setCalls, [2]string{forkProjectID, sha})
	return f.setOK, f.setErr
}

func (f *fakeForks) PersonalProjectCreator(context.Context, string) (string, bool, error) {
	return f.slugCreator, f.slugHeld, f.slugErr
}

type fakeRepos struct {
	err error
	ids []string
}

func (f *fakeRepos) Provision(_ context.Context, projectID string) error {
	f.ids = append(f.ids, projectID)
	return f.err
}

type fakeImporter struct {
	result gitprovider.ForkImportResult
	err    error
	reqs   []gitprovider.ForkImportRequest
}

func (f *fakeImporter) Import(_ context.Context, in gitprovider.ForkImportRequest) (gitprovider.ForkImportResult, error) {
	f.reqs = append(f.reqs, in)
	if f.err != nil {
		return gitprovider.ForkImportResult{}, f.err
	}
	return f.result, nil
}

type fakePRs struct {
	pr   domain.PullRequest
	err  error
	reqs []pullrequests.CreatePullRequestParams
}

func (f *fakePRs) Create(_ context.Context, in pullrequests.CreatePullRequestParams) (domain.PullRequest, error) {
	f.reqs = append(f.reqs, in)
	if f.err != nil {
		return domain.PullRequest{}, f.err
	}
	return f.pr, nil
}

// harness wires the fakes with the REAL policy engine: the matrix cells the
// fork path resolves are the canonical table's, not a stub's.
type harness struct {
	projects *fakeProjects
	branches *fakeBranches
	creator  *fakeBranchCreator
	links    *fakeForks
	repos    *fakeRepos
	imports  *fakeImporter
	prs      *fakePRs
	svc      *forks.Service
}

func newHarness(t *testing.T, opts ...func(*harness)) *harness {
	t.Helper()
	h := &harness{
		projects: &fakeProjects{
			project:       publicParent(),
			createProject: domain.Project{ID: forkID, CreatedBy: actorID},
		},
		branches: &fakeBranches{
			byID: map[string]domain.Branch{mainID: mainBranch(), researchID: researchBranch()},
			list: []domain.Branch{mainBranch(), researchBranch()},
		},
		creator: &fakeBranchCreator{branch: domain.Branch{ID: forkBrID, GitRef: "refs/heads/fork/main", Lifecycle: domain.BranchLifecycleActive}},
		links: &fakeForks{
			claim:         forks.Fork{ForkProjectID: forkID, ParentProjectID: parentID, ForkedBy: actorID, RelationType: "forked_from", SourceBranchID: mainID, ForkBranchID: forkBrID},
			claimInserted: true,
			setOK:         true,
		},
		repos:   &fakeRepos{},
		imports: &fakeImporter{result: gitprovider.ForkImportResult{SourceSHA: "sourcesha", TargetHeadSHA: "sourcesha", Inserted: true}},
		prs:     &fakePRs{pr: domain.PullRequest{ID: "pr-1", Number: 1, ProjectID: parentID, State: domain.PullRequestStateOpen}},
	}
	for _, opt := range opts {
		opt(h)
	}
	h.svc = forks.NewService(forks.Deps{
		Projects:     h.projects,
		Branches:     h.branches,
		BranchWriter: h.creator,
		Forks:        h.links,
		Repos:        h.repos,
		Imports:      h.imports,
		PullRequests: h.prs,
		Authz:        authz.NewMatrixEngine(),
	})
	return h
}

// ------------------------------------------------------------------ tests

// TestForkCreatesTheForkAndImportsTheLine is the happy path of criterion 1
// and 8: a non-member forks a public project, the fork project is derived
// (slug, name, private visibility) in the actor's own space, the branch the
// copy lands on is not the fork's main, the copy runs against the parent's
// main ref, and the recorded fork point is the copied commit.
func TestForkCreatesTheForkAndImportsTheLine(t *testing.T) {
	h := newHarness(t)
	res, err := h.svc.Fork(context.Background(), actor(), forks.ForkRequest{ProjectID: parentID})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if res.AlreadyForked || !res.Imported {
		t.Fatalf("AlreadyForked=%v Imported=%v, want false/true", res.AlreadyForked, res.Imported)
	}
	if res.Fork.ForkedSHA == nil || *res.Fork.ForkedSHA != "sourcesha" {
		t.Fatalf("ForkedSHA=%v, want sourcesha", res.Fork.ForkedSHA)
	}
	if len(h.projects.createCalls) != 1 {
		t.Fatalf("project creates = %d, want 1", len(h.projects.createCalls))
	}
	created := h.projects.createCalls[0]
	if want := forkSlugFor("mof-gas", "curie", parentID, actorID); created.Slug != want {
		t.Fatalf("derived slug = %q, want %q", created.Slug, want)
	}
	if created.Visibility != domain.VisibilityPrivate {
		t.Fatalf("fork visibility = %q, want private (fail closed)", created.Visibility)
	}
	if len(h.repos.ids) != 1 || h.repos.ids[0] != forkID {
		t.Fatalf("provisioned projects = %v, want [%s]", h.repos.ids, forkID)
	}
	if len(h.creator.inputs) != 1 {
		t.Fatalf("branch creates = %d, want 1", len(h.creator.inputs))
	}
	if got := h.creator.inputs[0].Name; got != "fork/main" {
		t.Fatalf("fork branch = %q, want fork/main", got)
	}
	if h.creator.projectIDs[0] != forkID {
		t.Fatalf("branch created in %s, want the fork project %s", h.creator.projectIDs[0], forkID)
	}
	if len(h.imports.reqs) != 1 {
		t.Fatalf("imports = %d, want 1", len(h.imports.reqs))
	}
	imp := h.imports.reqs[0]
	if imp.SourceProjectID != parentID || imp.TargetProjectID != forkID {
		t.Fatalf("import projects = %s -> %s, want %s -> %s", imp.SourceProjectID, imp.TargetProjectID, parentID, forkID)
	}
	if imp.SourceRef != "refs/heads/main" {
		t.Fatalf("import source ref = %q, want refs/heads/main", imp.SourceRef)
	}
	if imp.TargetBranch != "fork/main" {
		t.Fatalf("import target branch = %q, want fork/main", imp.TargetBranch)
	}
	if len(h.links.setCalls) != 1 || h.links.setCalls[0] != [2]string{forkID, "sourcesha"} {
		t.Fatalf("SetForkSHA calls = %v, want [(fork sha)]", h.links.setCalls)
	}
	// The lineage claim carries the audit entry (docs/53: row, audit and
	// event are one transaction's facts) and names the parent as its scope.
	if len(h.links.claims) != 1 {
		t.Fatalf("claims = %d, want 1", len(h.links.claims))
	}
	audit := h.links.claims[0].Audit
	if audit.Action != "project.forked" || audit.ProjectID != parentID || audit.ActorID != actorID {
		t.Fatalf("audit = %+v, want project.forked on the parent by the actor", audit)
	}
	if md, ok := audit.Metadata.(map[string]any); !ok || md["relation_type"] != "forked_from" {
		t.Fatalf("audit metadata = %#v, want relation_type forked_from", audit.Metadata)
	}
	if h.links.claims[0].SourceBranchID != mainID || h.links.claims[0].ForkBranchID != forkBrID {
		t.Fatalf("claim branches = %s/%s, want %s/%s", h.links.claims[0].SourceBranchID, h.links.claims[0].ForkBranchID, mainID, forkBrID)
	}
}

// TestForkRefusesAPrivateParentForANonMember is criterion 1's second half:
// the create_branch cell a non-member resolves is external_fork_only, whose
// condition is that the project is public. The read gate refuses first, so
// the answer is the existence-hiding not-found.
func TestForkRefusesAPrivateParentForANonMember(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.projects.project = privateParent()
		h.projects.getErr = projects.ErrProjectNotFound
	})
	_, err := h.svc.Fork(context.Background(), actor(), forks.ForkRequest{ProjectID: parentID})
	if !errors.Is(err, forks.ErrProjectNotFound) {
		t.Fatalf("Fork on a private project = %v, want ErrProjectNotFound", err)
	}
	if len(h.projects.createCalls) != 0 || len(h.imports.reqs) != 0 {
		t.Fatalf("a refused fork wrote something: creates=%d imports=%d", len(h.projects.createCalls), len(h.imports.reqs))
	}
}

// TestForkRefusesUnreadablePrivateProjectEvenAsMemberRead is the engine's
// own refusal path: a project the actor cannot read never reaches the
// matrix, and the answer carries no existence signal.
func TestForkRefusesForbiddenProjectRead(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.projects.getErr = projects.ErrForbidden })
	_, err := h.svc.Fork(context.Background(), actor(), forks.ForkRequest{ProjectID: parentID})
	if !errors.Is(err, forks.ErrForbidden) {
		t.Fatalf("Fork = %v, want ErrForbidden", err)
	}
	if h.projects.memberCalls != 0 {
		t.Fatalf("membership was read after a refused project read")
	}
}

// TestForkRefusesAnonymous is criterion 4: the anonymous class denies all
// three cells, and the refusal happens before anything is read or written.
func TestForkRefusesAnonymous(t *testing.T) {
	h := newHarness(t)
	_, err := h.svc.Fork(context.Background(), domain.User{}, forks.ForkRequest{ProjectID: parentID})
	if !errors.Is(err, forks.ErrForbidden) {
		t.Fatalf("anonymous Fork = %v, want ErrForbidden", err)
	}
	if h.projects.getCalls != 0 || h.projects.memberCalls != 0 || len(h.imports.reqs) != 0 {
		t.Fatalf("an anonymous fork touched the stack: projects=%d members=%d imports=%d",
			h.projects.getCalls, h.projects.memberCalls, len(h.imports.reqs))
	}
}

// TestForkRefusesAViewer locks the deny cell: viewer is a role, not a
// non-member, and its create_branch verdict is deny — not
// external_fork_only. A viewer therefore cannot fork the project they
// watch.
func TestForkRefusesAViewer(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.projects.membership = &domain.ProjectMembership{ProjectID: parentID, UserID: actorID, Role: domain.ProjectRoleViewer}
	})
	_, err := h.svc.Fork(context.Background(), actor(), forks.ForkRequest{ProjectID: parentID})
	if !errors.Is(err, forks.ErrForbidden) {
		t.Fatalf("viewer Fork = %v, want ErrForbidden", err)
	}
	if len(h.projects.createCalls) != 0 {
		t.Fatalf("a viewer's fork created a project")
	}
}

// TestForkByAMemberIsAllowedAndStillForks is the plain-allow cell: a
// contributor's create_branch is allow, so the fork runs without the
// public-project condition (the condition belongs to the non-member cell).
func TestForkByAMemberIsAllowed(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.projects.project = privateParent()
		h.projects.membership = &domain.ProjectMembership{ProjectID: parentID, UserID: actorID, Role: domain.ProjectRoleContributor}
	})
	res, err := h.svc.Fork(context.Background(), actor(), forks.ForkRequest{ProjectID: parentID})
	if err != nil {
		t.Fatalf("member Fork: %v", err)
	}
	if !res.Imported {
		t.Fatalf("member fork did not import the line")
	}
}

// TestForkRepeatedIsANoOp is criterion 9's service half: the second request
// finds the lineage row (the derived slug collides) and writes nothing — no
// second project, no second import, no second claim.
func TestForkRepeatedIsANoOp(t *testing.T) {
	sha := "sourcesha"
	h := newHarness(t, func(h *harness) {
		h.projects.createErr = projects.ErrSlugTaken
		h.links.findOK = true
		h.links.find = forks.Fork{ForkProjectID: forkID, ParentProjectID: parentID, ForkedBy: actorID, RelationType: "forked_from", SourceBranchID: mainID, ForkBranchID: forkBrID, ForkedSHA: &sha}
		h.branches.byID[forkBrID] = domain.Branch{ID: forkBrID, ProjectID: forkID, Name: "fork/main", GitRef: "refs/heads/fork/main", Lifecycle: domain.BranchLifecycleActive}
	})
	res, err := h.svc.Fork(context.Background(), actor(), forks.ForkRequest{ProjectID: parentID})
	if err != nil {
		t.Fatalf("repeated Fork: %v", err)
	}
	if !res.AlreadyForked || res.Imported {
		t.Fatalf("AlreadyForked=%v Imported=%v, want true/false", res.AlreadyForked, res.Imported)
	}
	if len(h.links.claims) != 0 || len(h.imports.reqs) != 0 || len(h.links.setCalls) != 0 {
		t.Fatalf("a repeated fork wrote: claims=%d imports=%d set=%d", len(h.links.claims), len(h.imports.reqs), len(h.links.setCalls))
	}
}

// TestForkRetriesAnInterruptedImport is criterion 9's other half: the
// lineage row exists but its fork point is still NULL (the copy did not
// finish), so a repeated request completes it — the guard is the recorded
// state, not a remembered request.
func TestForkRetriesAnInterruptedImport(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.projects.createErr = projects.ErrSlugTaken
		h.links.findOK = true
		h.links.find = forks.Fork{ForkProjectID: forkID, ParentProjectID: parentID, ForkedBy: actorID, RelationType: "forked_from", SourceBranchID: mainID, ForkBranchID: forkBrID}
		h.branches.byID[forkBrID] = domain.Branch{ID: forkBrID, ProjectID: forkID, Name: "fork/main", GitRef: "refs/heads/fork/main", Lifecycle: domain.BranchLifecycleActive}
	})
	res, err := h.svc.Fork(context.Background(), actor(), forks.ForkRequest{ProjectID: parentID})
	if err != nil {
		t.Fatalf("retry Fork: %v", err)
	}
	if !res.AlreadyForked || !res.Imported {
		t.Fatalf("AlreadyForked=%v Imported=%v, want true/true", res.AlreadyForked, res.Imported)
	}
	if len(h.imports.reqs) != 1 || h.imports.reqs[0].TargetBranch != "fork/main" {
		t.Fatalf("retry imports = %+v, want one import onto fork/main", h.imports.reqs)
	}
	if len(h.links.setCalls) != 1 {
		t.Fatalf("retry did not record the fork point: %v", h.links.setCalls)
	}
}

// TestForkCopyFailureLeavesTheForkWithoutAForkPoint: the import failing is
// a store failure, and the recorded state (forked_sha NULL) is what makes
// the next request try again.
func TestForkCopyFailureIsAStoreFailure(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.imports.err = errors.New("provider unreachable") })
	_, err := h.svc.Fork(context.Background(), actor(), forks.ForkRequest{ProjectID: parentID})
	if !errors.Is(err, forks.ErrStore) {
		t.Fatalf("Fork with a failing import = %v, want ErrStore", err)
	}
	if len(h.links.setCalls) != 0 {
		t.Fatalf("a failed import recorded a fork point: %v", h.links.setCalls)
	}
}

// TestForkRefusesALostClaimLoudly: the derived slug is what makes the
// project insert the compare-and-swap, so a lost lineage claim after a
// winning project insert is an invariant violation, not a flow to answer
// with a fork this call cannot vouch for.
func TestForkRefusesALostClaimLoudly(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.links.claimInserted = false })
	_, err := h.svc.Fork(context.Background(), actor(), forks.ForkRequest{ProjectID: parentID})
	if !errors.Is(err, forks.ErrStore) {
		t.Fatalf("lost claim = %v, want ErrStore", err)
	}
	if len(h.imports.reqs) != 0 {
		t.Fatalf("a fork whose claim was lost imported content")
	}
}

// TestForkNeedsAPolicyEngine: without an engine the service refuses rather
// than assuming a class (fail closed).
func TestForkNeedsAPolicyEngine(t *testing.T) {
	h := newHarness(t)
	h.svc = forks.NewService(forks.Deps{Projects: h.projects, Branches: h.branches, BranchWriter: h.creator, Forks: h.links, Repos: h.repos, Imports: h.imports, PullRequests: h.prs})
	if _, err := h.svc.Fork(context.Background(), actor(), forks.ForkRequest{ProjectID: parentID}); !errors.Is(err, forks.ErrStore) {
		t.Fatalf("Fork without an engine = %v, want ErrStore", err)
	}
}

// TestForkRefusesTheForkMainAsTheCopyTarget: the copy never lands on the
// fork's own main (docs/09 §3), and the refusal happens before any row is
// created — a fork project cannot be deleted.
func TestForkRefusesTheForkMainAsTheCopyTarget(t *testing.T) {
	h := newHarness(t)
	_, err := h.svc.Fork(context.Background(), actor(), forks.ForkRequest{ProjectID: parentID, BranchName: "main"})
	if !errors.Is(err, forks.ErrValidation) {
		t.Fatalf("Fork onto main = %v, want ErrValidation", err)
	}
	if len(h.projects.createCalls) != 0 || len(h.creator.inputs) != 0 {
		t.Fatalf("a refused branch name still created rows")
	}
}

// TestForkOfAParentBranchUsesThatBranchAndItsOwnName is the research-line
// fork: naming a source branch forks THAT line, the copy lands on
// fork/<that name>, and the import is asked for the branch's own ref (its
// divergence is measured against its own recorded fork point, which is what
// keeps a plain fork free of the parent's whole tree).
func TestForkOfAParentBranch(t *testing.T) {
	h := newHarness(t)
	res, err := h.svc.Fork(context.Background(), actor(), forks.ForkRequest{ProjectID: parentID, SourceBranchID: researchID})
	if err != nil {
		t.Fatalf("Fork of a branch: %v", err)
	}
	if res.Fork.SourceBranchID != researchID {
		t.Fatalf("lineage source branch = %s, want %s", res.Fork.SourceBranchID, researchID)
	}
	if h.links.claims[0].SourceBranchID != researchID {
		t.Fatalf("claim source branch = %s, want %s", h.links.claims[0].SourceBranchID, researchID)
	}
	if got := h.creator.inputs[0].Name; got != "fork/feature-x" {
		t.Fatalf("fork branch = %q, want fork/feature-x", got)
	}
	if got := h.imports.reqs[0].SourceRef; got != "refs/heads/feature-x" {
		t.Fatalf("import source ref = %q, want refs/heads/feature-x", got)
	}
}

// TestForkOfABranchOfAnotherProjectIsNotFound: the branch read is
// project-scoped, so a branch of another project is indistinguishable from
// an unknown one.
func TestForkOfAForeignBranchIsNotFound(t *testing.T) {
	h := newHarness(t)
	foreign := domain.Branch{ID: researchID, ProjectID: otherID, Name: "feature-x", GitRef: "refs/heads/feature-x"}
	h.branches.byID[researchID] = foreign
	_, err := h.svc.Fork(context.Background(), actor(), forks.ForkRequest{ProjectID: parentID, SourceBranchID: researchID})
	if !errors.Is(err, forks.ErrBranchNotFound) {
		t.Fatalf("Fork of a foreign branch = %v, want ErrBranchNotFound", err)
	}
	if len(h.projects.createCalls) != 0 {
		t.Fatalf("a fork of a foreign branch created a project")
	}
}

// TestForkWithoutAMainBranchIsRefused: a project whose canonical line does
// not exist has nothing to fork.
func TestForkWithoutAMainBranchIsRefused(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.branches.list = []domain.Branch{researchBranch()}
		h.branches.byID = map[string]domain.Branch{researchID: researchBranch()}
	})
	_, err := h.svc.Fork(context.Background(), actor(), forks.ForkRequest{ProjectID: parentID})
	if !errors.Is(err, forks.ErrNoSourceBranch) {
		t.Fatalf("Fork without main = %v, want ErrNoSourceBranch", err)
	}
}

// ------------------------------------------------------ external proposals

// TestOpenExternalPRFromOwnFork is criterion 3's positive half: a
// non-member's open_pr cell is allow_from_fork, the condition is the
// lineage (their OWN fork of THIS project), and the proposal goes through
// the EXISTING pull-request path — the same Create an internal proposal
// uses.
func TestOpenExternalPRFromOwnFork(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.links.branchOK = true
		h.links.branchRow = forks.Fork{ForkProjectID: forkID, ParentProjectID: parentID, ForkedBy: actorID, RelationType: "forked_from", SourceBranchID: mainID, ForkBranchID: forkBrID}
	})
	pr, err := h.svc.OpenExternalPR(context.Background(), actor(), forks.OpenPRRequest{
		ProjectID: parentID, SourceBranchID: forkBrID, TargetBranchID: mainID, Title: "Add a MOF", Body: "why",
	})
	if err != nil {
		t.Fatalf("OpenExternalPR: %v", err)
	}
	if pr.Number != 1 {
		t.Fatalf("PR = %+v, want the path's own result", pr)
	}
	if len(h.prs.reqs) != 1 {
		t.Fatalf("PR creates = %d, want 1", len(h.prs.reqs))
	}
	got := h.prs.reqs[0]
	if got.ProjectID != parentID || got.SourceBranchID != forkBrID || got.TargetBranchID != mainID || got.CreatedBy != actorID {
		t.Fatalf("PR params = %+v, want the external pair", got)
	}
}

// TestOpenExternalPRRefusedOutsideTheOwnFork covers the three shapes the
// allow_from_fork condition refuses, none of which may create a PR.
func TestOpenExternalPRRefusedOutsideTheOwnFork(t *testing.T) {
	cases := []struct {
		name string
		row  forks.Fork
		ok   bool
	}{
		{"not a fork at all", forks.Fork{}, false},
		{"a fork of another project", forks.Fork{ForkProjectID: forkID, ParentProjectID: otherID, ForkedBy: actorID}, true},
		{"somebody else's fork", forks.Fork{ForkProjectID: forkID, ParentProjectID: parentID, ForkedBy: otherActor}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(h *harness) {
				h.links.branchOK = tc.ok
				h.links.branchRow = tc.row
			})
			_, err := h.svc.OpenExternalPR(context.Background(), actor(), forks.OpenPRRequest{
				ProjectID: parentID, SourceBranchID: forkBrID, TargetBranchID: mainID, Title: "Add a MOF", Body: "why",
			})
			if !errors.Is(err, forks.ErrForbidden) {
				t.Fatalf("OpenExternalPR = %v, want ErrForbidden", err)
			}
			if len(h.prs.reqs) != 0 {
				t.Fatalf("a refused proposal reached the PR path")
			}
		})
	}
}

// TestOpenExternalPRRefusesAnonymous is criterion 4's second cell.
func TestOpenExternalPRRefusesAnonymous(t *testing.T) {
	h := newHarness(t)
	_, err := h.svc.OpenExternalPR(context.Background(), domain.User{}, forks.OpenPRRequest{
		ProjectID: parentID, SourceBranchID: forkBrID, TargetBranchID: mainID, Title: "t", Body: "b",
	})
	if !errors.Is(err, forks.ErrForbidden) {
		t.Fatalf("anonymous OpenExternalPR = %v, want ErrForbidden", err)
	}
	if h.projects.getCalls != 0 || len(h.prs.reqs) != 0 {
		t.Fatalf("an anonymous proposal touched the stack")
	}
}

// TestOpenExternalPRByAMemberNeedsNoFork: a member's open_pr cell is a
// plain allow, so the lineage is not consulted at all.
func TestOpenExternalPRByAMemberNeedsNoFork(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.projects.membership = &domain.ProjectMembership{ProjectID: parentID, UserID: actorID, Role: domain.ProjectRoleMaintainer}
	})
	if _, err := h.svc.OpenExternalPR(context.Background(), actor(), forks.OpenPRRequest{
		ProjectID: parentID, SourceBranchID: researchID, TargetBranchID: mainID, Title: "t", Body: "b",
	}); err != nil {
		t.Fatalf("member OpenExternalPR: %v", err)
	}
	if h.links.branchOK {
		t.Fatalf("the lineage was consulted for a member's own branch")
	}
}

// TestOpenExternalPRRefusesAViewer: viewer's open_pr cell is deny, so a
// viewer cannot propose from anywhere.
func TestOpenExternalPRRefusesAViewer(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.projects.membership = &domain.ProjectMembership{ProjectID: parentID, UserID: actorID, Role: domain.ProjectRoleViewer}
	})
	_, err := h.svc.OpenExternalPR(context.Background(), actor(), forks.OpenPRRequest{
		ProjectID: parentID, SourceBranchID: researchID, TargetBranchID: mainID, Title: "t", Body: "b",
	})
	if !errors.Is(err, forks.ErrForbidden) {
		t.Fatalf("viewer OpenExternalPR = %v, want ErrForbidden", err)
	}
}

// TestOpenExternalPRKeepsDomainOutcomes: the existing path's answers pass
// through unchanged — a caller of an external proposal sees what a caller
// of an internal one sees.
func TestOpenExternalPRKeepsDomainOutcomes(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.links.branchOK = true
		h.links.branchRow = forks.Fork{ForkProjectID: forkID, ParentProjectID: parentID, ForkedBy: actorID}
		h.prs.err = pullrequests.ErrBranchNotActive
	})
	_, err := h.svc.OpenExternalPR(context.Background(), actor(), forks.OpenPRRequest{
		ProjectID: parentID, SourceBranchID: forkBrID, TargetBranchID: mainID, Title: "t", Body: "b",
	})
	if !errors.Is(err, pullrequests.ErrBranchNotActive) {
		t.Fatalf("OpenExternalPR = %v, want the path's own outcome", err)
	}
}

// ---------------------------------------------------------------- lineage

// TestLineageRunsTheReadGate: the lineage is a derived fact and is never
// more visible than its subject.
func TestLineageRunsTheReadGate(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.projects.getErr = projects.ErrProjectNotFound })
	if _, err := h.svc.Lineage(context.Background(), projects.Reader{UserID: actorID, Authenticated: true}, parentID); !errors.Is(err, forks.ErrProjectNotFound) {
		t.Fatalf("Lineage = %v, want ErrProjectNotFound", err)
	}
}

// TestLineageReturnsTheStoredRows: the rows are returned as stored — the
// relation name travels with them rather than being implied by the table's
// name.
func TestLineageReturnsTheStoredRows(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.links.list = []forks.Fork{{ForkProjectID: forkID, ParentProjectID: parentID, ForkedBy: actorID, RelationType: "forked_from"}}
	})
	rows, err := h.svc.Lineage(context.Background(), projects.Reader{UserID: actorID, Authenticated: true}, parentID)
	if err != nil {
		t.Fatalf("Lineage: %v", err)
	}
	if len(rows) != 1 || rows[0].RelationType != "forked_from" || rows[0].ForkProjectID != forkID {
		t.Fatalf("Lineage = %+v, want the stored row", rows)
	}
}

// ------------------------------------------------------------------ slug

// TestForkSlugIsDerivedFromParentAndActor pins the idempotency key: the
// same (parent, actor) always derives the same slug, and two different
// actors never collide.
func TestForkSlugIsDerivedFromParentAndActor(t *testing.T) {
	h := newHarness(t)
	if _, err := h.svc.Fork(context.Background(), actor(), forks.ForkRequest{ProjectID: parentID, Name: "My fork"}); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if got := h.projects.createCalls[0].Name; got != "My fork" {
		t.Fatalf("fork name = %q, want the caller's", got)
	}
	if want := forkSlugFor("mof-gas", "curie", parentID, actorID); h.projects.createCalls[0].Slug != want {
		t.Fatalf("slug = %q, want %q", h.projects.createCalls[0].Slug, want)
	}
	other := actor()
	other.ID = otherActor
	other.Handle = "faraday"
	h2 := newHarness(t)
	if _, err := h2.svc.Fork(context.Background(), other, forks.ForkRequest{ProjectID: parentID}); err != nil {
		t.Fatalf("second actor Fork: %v", err)
	}
	if want := forkSlugFor("mof-gas", "faraday", parentID, otherActor); h2.projects.createCalls[0].Slug != want {
		t.Fatalf("second slug = %q, want %q", h2.projects.createCalls[0].Slug, want)
	}
}

// TestForkSlugTellsTwoSameNamedParentsApart is the lock-out this rule exists
// to close. A project's slug is unique per ORGANIZATION (00019: the personal
// index covers organization_id IS NULL only), so two organizations may each
// hold a "mof-curie" — and one actor may fork both. Under a name derived
// from the parent's SLUG alone both forks would derive the same string, the
// second insert would lose to the fork of the FIRST (a project the actor
// themselves created), and forkProjectOverTakenName answers that holder with
// ErrForkSlugTaken because the record cannot tell an in-flight fork from a
// project that merely holds the name. A slug is not editable and a project
// row is not deletable, so the second fork would be refused forever.
//
// The digest of the pair (parent id, actor id) is what separates them: same
// slug, different parents, two names.
func TestForkSlugTellsTwoSameNamedParentsApart(t *testing.T) {
	const secondParent = "99999999-9999-4999-8999-999999999999"
	h := newHarness(t)
	if _, err := h.svc.Fork(context.Background(), actor(), forks.ForkRequest{ProjectID: parentID}); err != nil {
		t.Fatalf("first fork: %v", err)
	}
	first := h.projects.createCalls[0].Slug

	// The second parent carries the SAME slug and a different id, which is
	// what an organization-scoped slug collision looks like to this service.
	other := publicParent()
	other.ID = secondParent
	h2 := newHarness(t, func(h *harness) {
		h.projects.project = other
		h.branches.byID[mainID] = domain.Branch{ID: mainID, ProjectID: secondParent, Name: domain.MainBranchName, GitRef: "refs/heads/main"}
		h.branches.list = []domain.Branch{h.branches.byID[mainID]}
	})
	if _, err := h2.svc.Fork(context.Background(), actor(), forks.ForkRequest{ProjectID: secondParent}); err != nil {
		t.Fatalf("second fork of a same-named parent: %v", err)
	}
	second := h2.projects.createCalls[0].Slug

	if first == second {
		t.Fatalf("two same-named parents derived the same fork slug %q — the second fork would be permanently locked out", first)
	}
	for _, tc := range []struct{ got, want string }{
		{first, forkSlugFor("mof-gas", "curie", parentID, actorID)},
		{second, forkSlugFor("mof-gas", "curie", secondParent, actorID)},
	} {
		if tc.got != tc.want {
			t.Errorf("slug = %q, want %q (the pair's own digest)", tc.got, tc.want)
		}
	}
}

// TestForkSlugStaysWithinTheBound: a long parent slug and a long handle
// must still derive a valid project slug — the personal-slug unique index
// is 64 characters wide, and a derived value the domain refuses would make
// a fork impossible — and two long parents must not derive the same slug
// for one actor.
//
// The handle lengths below are the ones the bound actually bites at: a
// 20-character parent slug with a 54-character handle is already 75
// characters before shortening, and a 64-character handle — the longest
// domain.ValidHandle allows — leaves no room for the parent's slug at all.
// Both ends are measured, not assumed: the pre-fix derivation answered 65
// characters for the 54- and 57-character handles (the actor's part was
// kept whole and the parent's part was clamped to one character, which was
// then dropped by the trim).
func TestForkSlugStaysWithinTheBound(t *testing.T) {
	long := domain.Project{ID: parentID, Slug: strings.Repeat("a", 80), Name: "Long", Visibility: domain.VisibilityPublic}
	for _, handleLen := range []int{5, 20, 54, 55, 56, 57, 61, 64} {
		handle := strings.Repeat("h", handleLen)
		if !domain.ValidHandle(handle) {
			t.Fatalf("the fixture's %d-character handle is not one the domain allows", handleLen)
		}
		user := actor()
		user.Handle = handle
		h := newHarness(t, func(h *harness) { h.projects.project = long })
		if _, err := h.svc.Fork(context.Background(), user, forks.ForkRequest{ProjectID: parentID}); err != nil {
			t.Fatalf("handle %d: Fork: %v", handleLen, err)
		}
		got := h.projects.createCalls[0].Slug
		if len(got) > 64 {
			t.Errorf("handle %d: derived slug %q is %d characters, want <= 64", handleLen, got, len(got))
		}
		if !domain.ValidProjectSlug(got) {
			t.Errorf("handle %d: derived slug %q is not a valid project slug", handleLen, got)
		}
		t.Logf("handle %d chars: slug %q (%d chars, full handle kept: %v)",
			handleLen, got, len(got), strings.Contains(got, handle))
		// Both parts survive the shortening: the parent's slug keeps its
		// first character and the handle keeps its first 40, which is what
		// tells two forks apart in a name that had to be cut.
		if !strings.HasPrefix(got, "a") {
			t.Errorf("handle %d: derived slug %q does not name the parent it is a fork of", handleLen, got)
		}
		keep := handleLen
		if keep > 40 {
			keep = 40
		}
		if !strings.Contains(got, handle[:keep]) {
			t.Errorf("handle %d: derived slug %q does not carry the actor's handle", handleLen, got)
		}
		// A second actor with the same long handle: one parent, two actors,
		// two slugs — the actor's id is in the digest for exactly this.
		other := user
		other.ID = otherActor
		h2 := newHarness(t, func(h *harness) { h.projects.project = long })
		if _, err := h2.svc.Fork(context.Background(), other, forks.ForkRequest{ProjectID: parentID}); err != nil {
			t.Fatalf("handle %d: second actor Fork: %v", handleLen, err)
		}
		if h2.projects.createCalls[0].Slug == got {
			t.Errorf("handle %d: two actors derived the same slug %q", handleLen, got)
		}
	}
	// The grid the bound was measured on — parent slug length against handle
	// length, at the lengths where the derivation overflowed before — run
	// through the service so the value asserted is the one the fork inserts.
	for _, parentLen := range []int{3, 7, 20, 80} {
		for _, handleLen := range []int{54, 55, 56, 57, 60, 61, 64} {
			user := actor()
			user.Handle = strings.Repeat("h", handleLen)
			parent := long
			parent.Slug = strings.Repeat("a", parentLen)
			h := newHarness(t, func(h *harness) { h.projects.project = parent })
			if _, err := h.svc.Fork(context.Background(), user, forks.ForkRequest{ProjectID: parentID}); err != nil {
				t.Fatalf("parent %d, handle %d: Fork: %v", parentLen, handleLen, err)
			}
			got := h.projects.createCalls[0].Slug
			if len(got) > 64 || !domain.ValidProjectSlug(got) {
				t.Errorf("parent %d, handle %d: derived slug %q is %d characters (valid: %v)",
					parentLen, handleLen, got, len(got), domain.ValidProjectSlug(got))
			}
			t.Logf("parent slug %d chars, handle %d chars: %q (%d chars)", parentLen, handleLen, got, len(got))
		}
	}
	// A different parent with the same long slug must derive a different
	// value for the same actor: the digest of the parent's id is what
	// separates them. Measured at the longest handle, where both names are
	// shortened.
	user := actor()
	user.Handle = strings.Repeat("h", 64)
	other := long
	other.ID = otherID
	first := newHarness(t, func(h *harness) { h.projects.project = long })
	if _, err := first.svc.Fork(context.Background(), user, forks.ForkRequest{ProjectID: parentID}); err != nil {
		t.Fatalf("first parent Fork: %v", err)
	}
	second := newHarness(t, func(h *harness) {
		h.projects.project = other
		h.branches.byID[mainID] = domain.Branch{ID: mainID, ProjectID: otherID, Name: domain.MainBranchName, GitRef: "refs/heads/main"}
		h.branches.list = []domain.Branch{h.branches.byID[mainID]}
	})
	if _, err := second.svc.Fork(context.Background(), user, forks.ForkRequest{ProjectID: otherID}); err != nil {
		t.Fatalf("second parent Fork: %v", err)
	}
	got, been := first.projects.createCalls[0].Slug, second.projects.createCalls[0].Slug
	if got == been {
		t.Fatalf("two parents derived the same slug %q", got)
	}
	for _, slug := range []string{got, been} {
		if !domain.ValidProjectSlug(slug) || len(slug) > 64 {
			t.Errorf("derived slug %q is not a valid project slug within the bound", slug)
		}
	}
}

// TestForkMovesToTheReservedNameWhenAnotherProjectHoldsTheName is F1's
// first half. The name a fork derives is derivable by anyone — it is the
// parent's slug and the actor's handle — so another account can register it
// first, and personal-slug uniqueness is global (00019) with nothing ever
// deleted: a fork that only ever tried that one name would be permanently
// deadlocked by a name its own actor cannot free. It is not:
//
//	a project created by ANOTHER actor cannot be this pair's fork project
//	(fork projects are created by the actor who forks), so the fork moves
//	to the pair's reserved name and completes there — the holder is not
//	touched, and nothing is written as if the fork already existed.
func TestForkMovesToTheReservedNameWhenAnotherProjectHoldsTheName(t *testing.T) {
	want := forkSlugFor("mof-gas", "curie", parentID, actorID)
	h := newHarness(t, func(h *harness) {
		h.projects.createErrFor = map[string]error{want: projects.ErrSlugTaken}
		h.links.slugHeld = true
		h.links.slugCreator = otherActor
	})
	res, err := h.svc.Fork(context.Background(), actor(), forks.ForkRequest{ProjectID: parentID})
	if err != nil {
		t.Fatalf("Fork past a taken name: %v", err)
	}
	if res.AlreadyForked || !res.Imported {
		t.Fatalf("Fork = %+v, want a fresh fork whose content was imported", res)
	}
	if len(h.projects.createCalls) != 2 {
		t.Fatalf("project creates = %+v, want the derived name and then the reserved one", h.projects.createCalls)
	}
	derived, reserved := h.projects.createCalls[0].Slug, h.projects.createCalls[1].Slug
	if derived != want {
		t.Fatalf("first slug = %q, want the derived %q", derived, want)
	}
	if !strings.HasPrefix(reserved, derived) || !strings.HasSuffix(reserved, "-2") {
		t.Fatalf("reserved slug = %q, want the derived name %q carrying the reserve mark", reserved, derived)
	}
	if !domain.ValidProjectSlug(reserved) {
		t.Fatalf("reserved slug %q is not a valid project slug", reserved)
	}
	// The reserved name changes the name and nothing else: it is the same
	// fork request, not a different one.
	if a, b := h.projects.createCalls[0], h.projects.createCalls[1]; a.Name != b.Name || a.Purpose != b.Purpose || a.Visibility != b.Visibility {
		t.Fatalf("the reserved name changed more than the slug: %+v vs %+v", a, b)
	}
	// The fork then runs the ordinary path: provisioned once, one branch,
	// one lineage claim, one import.
	if len(h.repos.ids) != 1 || len(h.creator.inputs) != 1 || len(h.links.claims) != 1 || len(h.imports.reqs) != 1 {
		t.Fatalf("the escalated fork did not run the ordinary path: repos=%v branches=%d claims=%d imports=%d",
			h.repos.ids, len(h.creator.inputs), len(h.links.claims), len(h.imports.reqs))
	}
	// The fork's own project is the one that was created under the reserved
	// name, and the lineage names it.
	if h.links.claims[0].ForkProjectID != forkID {
		t.Fatalf("the lineage claimed project %s, want the one that was created", h.links.claims[0].ForkProjectID)
	}
}

// TestForkRefusesAccuratelyWhenTheActorOwnsTheName is F1's second half, the
// case where the fork stops. The name is held by a project the ACTOR
// created, and no lineage row exists: that is either a fork request of
// theirs between its two writes (a project insert and a lineage insert are
// two statements) or one of their own projects that merely holds the name.
// The record cannot tell them apart, so:
//
//   - no second project is created — escalating past this ambiguity is the
//     one way a pair could end up with two fork projects, one of them a row
//     that could never be deleted;
//   - and the answer says what is true of the record instead of reporting a
//     fork that may not exist.
func TestForkRefusesAccuratelyWhenTheActorOwnsTheName(t *testing.T) {
	want := forkSlugFor("mof-gas", "curie", parentID, actorID)
	h := newHarness(t, func(h *harness) {
		h.projects.createErrFor = map[string]error{want: projects.ErrSlugTaken}
		h.links.slugHeld = true
		h.links.slugCreator = actorID
	})
	_, err := h.svc.Fork(context.Background(), actor(), forks.ForkRequest{ProjectID: parentID})
	if !errors.Is(err, forks.ErrForkSlugTaken) {
		t.Fatalf("Fork = %v, want ErrForkSlugTaken", err)
	}
	if len(h.projects.createCalls) != 1 {
		t.Fatalf("project creates = %+v, want 1: escalating here can create a SECOND fork project for one pair", h.projects.createCalls)
	}
	if len(h.repos.ids)+len(h.creator.inputs)+len(h.links.claims)+len(h.imports.reqs)+len(h.links.setCalls) != 0 {
		t.Fatalf("a refused fork wrote something: repos=%v branches=%d claims=%d imports=%d set=%d",
			h.repos.ids, len(h.creator.inputs), len(h.links.claims), len(h.imports.reqs), len(h.links.setCalls))
	}
	msg := err.Error()
	if !strings.Contains(msg, want) {
		t.Errorf("the refusal does not name the slug it lost to: %q", msg)
	}
	if strings.Contains(msg, "already being created") {
		t.Errorf("the refusal claims a fork is being created, which no row shows: %q", msg)
	}
}

// TestForkRefusesWhenBothDerivedAndReservedNamesAreTaken: the escalation is
// bounded at one reserved name (a deterministic function of the pair, which
// is what keeps concurrent requests of one pair on the same name). When a
// third party holds that one too, the fork is refused with both names
// stated — and with no claim about a fork that does not exist.
func TestForkRefusesWhenBothDerivedAndReservedNamesAreTaken(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.projects.createErr = projects.ErrSlugTaken
		h.links.slugHeld = true
		h.links.slugCreator = otherActor
	})
	_, err := h.svc.Fork(context.Background(), actor(), forks.ForkRequest{ProjectID: parentID})
	if !errors.Is(err, forks.ErrForkSlugTaken) {
		t.Fatalf("Fork = %v, want ErrForkSlugTaken", err)
	}
	if len(h.projects.createCalls) != 2 {
		t.Fatalf("project creates = %+v, want exactly the two derived names", h.projects.createCalls)
	}
	msg := err.Error()
	for _, slug := range []string{h.projects.createCalls[0].Slug, h.projects.createCalls[1].Slug} {
		if !strings.Contains(msg, slug) {
			t.Errorf("the refusal does not name %s: %q", slug, msg)
		}
	}
	if len(h.links.claims)+len(h.imports.reqs) != 0 {
		t.Fatalf("a refused fork wrote something: claims=%d imports=%d", len(h.links.claims), len(h.imports.reqs))
	}
}

// TestForkReportsAStoreFailureWhenTheHolderCannotBeRead: the read that
// explains a lost insert can itself fail, and the outcome is then unknown —
// a store failure, not a fork and not a permission answer. A lost insert
// against a name no project answers to is the same: the state this code
// would have to act on does not exist, so it refuses rather than guesses.
func TestForkReportsAStoreFailureWhenTheHolderCannotBeRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*harness)
	}{
		{"the read failed", func(h *harness) { h.links.slugErr = errors.New("database unreachable") }},
		{"no project answers to the name", func(h *harness) { h.links.slugHeld = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(h *harness) {
				h.projects.createErrFor = map[string]error{forkSlugFor("mof-gas", "curie", parentID, actorID): projects.ErrSlugTaken}
				tc.set(h)
			})
			_, err := h.svc.Fork(context.Background(), actor(), forks.ForkRequest{ProjectID: parentID})
			if !errors.Is(err, forks.ErrStore) {
				t.Fatalf("Fork = %v, want ErrStore", err)
			}
			if len(h.projects.createCalls) != 1 {
				t.Fatalf("project creates = %+v, want 1", h.projects.createCalls)
			}
		})
	}
}

// TestForkFallsBackToTheActorIDWhenTheHandleIsUnusable: a handle that
// reduces to nothing still derives a slug.
func TestForkFallsBackToTheActorIDForSlug(t *testing.T) {
	h := newHarness(t)
	bare := domain.User{ID: actorID, Handle: "!!!"}
	if _, err := h.svc.Fork(context.Background(), bare, forks.ForkRequest{ProjectID: parentID}); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	slug := h.projects.createCalls[0].Slug
	if !strings.HasPrefix(slug, "mof-gas-") || strings.HasSuffix(slug, "mof-gas-") {
		t.Fatalf("slug = %q, want mof-gas-<something>", slug)
	}
}

// TestForkNeedsAProject: the request's shape is checked before anything is
// read.
func TestForkNeedsAProject(t *testing.T) {
	h := newHarness(t)
	if _, err := h.svc.Fork(context.Background(), actor(), forks.ForkRequest{}); !errors.Is(err, forks.ErrValidation) {
		t.Fatalf("Fork without a project = %v, want ErrValidation", err)
	}
	if h.projects.getCalls != 0 {
		t.Fatalf("a shapeless request read the project")
	}
}

// TestOpenExternalPRNeedsThePair: the request's shape is checked before
// anything is read.
func TestOpenExternalPRNeedsThePair(t *testing.T) {
	h := newHarness(t)
	if _, err := h.svc.OpenExternalPR(context.Background(), actor(), forks.OpenPRRequest{ProjectID: parentID}); !errors.Is(err, forks.ErrValidation) {
		t.Fatalf("OpenExternalPR without a branch pair = %v, want ErrValidation", err)
	}
}
