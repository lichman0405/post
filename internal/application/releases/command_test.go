package releases

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// The command tests reuse the builder fixture (fixtureService) for the
// manifest half and wire fakes for the command's own ports — authz
// resolution, the policy-in-force read, the branch head, the gate and
// the release store. The authz engine is the real matrix engine: the
// command's denial behavior must match the production matrix.

// fakeAuthzProjectPort implements ProjectAuthzPort.
type fakeAuthzProjectPort struct {
	project    domain.Project
	membership domain.ProjectMembership
	projectErr error
	memberErr  error
}

func (f *fakeAuthzProjectPort) GetProject(context.Context, string) (domain.Project, error) {
	if f.projectErr != nil {
		return domain.Project{}, f.projectErr
	}
	return f.project, nil
}

func (f *fakeAuthzProjectPort) GetMembership(context.Context, string, string) (domain.ProjectMembership, error) {
	if f.memberErr != nil {
		return domain.ProjectMembership{}, f.memberErr
	}
	return f.membership, nil
}

// fakeLatestPolicyPort implements PolicyLatestPort: the policy in force
// per scope, answering policy.ErrPolicyNotFound for a scope without one.
type fakeLatestPolicyPort struct {
	org     *domain.PolicyVersion
	project *domain.PolicyVersion
	err     error
}

func (f *fakeLatestPolicyPort) Latest(_ context.Context, scope domain.PolicyScope) (domain.PolicyVersion, error) {
	if f.err != nil {
		return domain.PolicyVersion{}, f.err
	}
	if scope.OrganizationID != "" {
		if f.org == nil {
			return domain.PolicyVersion{}, policy.ErrPolicyNotFound
		}
		return *f.org, nil
	}
	if f.project == nil {
		return domain.PolicyVersion{}, policy.ErrPolicyNotFound
	}
	return *f.project, nil
}

// fakeHeadPort implements BranchHeadPort.
type fakeHeadPort struct {
	head domain.ProjectState
	err  error
}

func (f *fakeHeadPort) GetBranchHead(context.Context, string) (domain.ProjectState, error) {
	return f.head, f.err
}

// fakeGatePort implements ReleaseGatePort: it echoes the configured
// report — the command's job is to run it and obey the verdict.
type fakeGatePort struct {
	report rsgvalidation.Report
	err    error
}

func (f *fakeGatePort) ValidateBranchWithFacts(context.Context, string, string, rsgvalidation.Gate, *rsgvalidation.ReleaseFacts, *rsgvalidation.AssetFacts) (rsgvalidation.Report, error) {
	return f.report, f.err
}

// fakeReleaseStorePort implements ReleaseStorePort with the same
// semantics the persistence store guarantees: keyed replay, version
// conflict on a fresh create, newest-first list, project-scoped get.
type fakeReleaseStorePort struct {
	releases  []domain.Release
	creations map[string]string // "<projectID>/<key>" -> release id
	err       error
}

func (f *fakeReleaseStorePort) CreateRelease(_ context.Context, r domain.Release, _ domain.AuditEntry, key *string) (domain.Release, error) {
	if f.err != nil {
		return domain.Release{}, f.err
	}
	if f.creations == nil {
		f.creations = map[string]string{}
	}
	if key != nil {
		if id, ok := f.creations[r.ProjectID+"/"+*key]; ok {
			for _, stored := range f.releases {
				if stored.ID == id {
					return stored, nil
				}
			}
		}
	}
	for _, stored := range f.releases {
		if stored.ProjectID == r.ProjectID && stored.Version == r.Version {
			return domain.Release{}, ErrVersionTaken
		}
	}
	r.ID = "release-0000000" + string(rune('1'+len(f.releases)))
	f.releases = append(f.releases, r)
	if key != nil {
		f.creations[r.ProjectID+"/"+*key] = r.ID
	}
	return r, nil
}

func (f *fakeReleaseStorePort) LookupCreation(_ context.Context, projectID, key string) (*domain.Release, error) {
	if f.err != nil {
		return nil, f.err
	}
	id, ok := f.creations[projectID+"/"+key]
	if !ok {
		return nil, nil
	}
	for i := range f.releases {
		if f.releases[i].ID == id {
			return &f.releases[i], nil
		}
	}
	return nil, ErrReleaseNotFound
}

func (f *fakeReleaseStorePort) ListReleases(_ context.Context, projectID string) ([]domain.Release, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []domain.Release
	for _, r := range f.releases {
		if r.ProjectID == projectID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeReleaseStorePort) GetRelease(_ context.Context, projectID, releaseID string) (domain.Release, error) {
	if f.err != nil {
		return domain.Release{}, f.err
	}
	for _, r := range f.releases {
		if r.ProjectID == projectID && r.ID == releaseID {
			return r, nil
		}
	}
	return domain.Release{}, ErrReleaseNotFound
}

// fixtureCommand wires the command over the builder fixture and the
// happy-path command fakes: owner membership on proj-00000001, main
// branch with the fixture state as head, both policies in force, a
// passing gate and an empty store.
func fixtureCommand(t *testing.T) (*Command, *fakeAuthzProjectPort, *fakeLatestPolicyPort, *fakeHeadPort, *fakeGatePort, *fakeReleaseStorePort) {
	t.Helper()
	svc, _, _ := fixtureService(t)
	org := "org-00000001"
	projectsPort := &fakeAuthzProjectPort{
		project: domain.Project{ID: "proj-00000001", OrganizationID: &org},
		membership: domain.ProjectMembership{
			ProjectID: "proj-00000001",
			UserID:    "user-00000001",
			Role:      domain.ProjectRoleOwner,
		},
	}
	policiesPort := &fakeLatestPolicyPort{
		org: &domain.PolicyVersion{
			ID:        "policy-00000002",
			Scope:     domain.PolicyScope{OrganizationID: org},
			Version:   "v2",
			Policy:    policyFixture(`{"main_protected":true,"release_min_reviewers":2}`),
			CreatedBy: "user-00000002",
			CreatedAt: fixedTime(9, 0),
		},
		project: &domain.PolicyVersion{
			ID:        "policy-00000003",
			Scope:     domain.PolicyScope{ProjectID: "proj-00000001"},
			Version:   "v1",
			Policy:    policyFixture(`{"release_min_reviewers":3}`),
			CreatedBy: "user-00000001",
			CreatedAt: fixedTime(10, 0),
		},
	}
	mainBranch := "branch-00000001"
	branchesPort := &fakeBranchPort{branches: []domain.Branch{
		{ID: mainBranch, ProjectID: "proj-00000001", Name: domain.MainBranchName},
	}}
	headsPort := &fakeHeadPort{head: domain.ProjectState{
		ID: "state-00000001", ProjectID: "proj-00000001", BranchID: &mainBranch,
	}}
	gatePort := &fakeGatePort{report: rsgvalidation.Report{
		Gate:    rsgvalidation.GateRelease,
		Verdict: rsgvalidation.VerdictPass,
	}}
	storePort := &fakeReleaseStorePort{}
	cmd := NewCommand(svc, projectsPort, policiesPort, branchesPort, headsPort, gatePort, storePort, authz.NewMatrixEngine())
	return cmd, projectsPort, policiesPort, headsPort, gatePort, storePort
}

func fixtureCreateParams() CreateReleaseParams {
	key := "key-create-v1"
	return CreateReleaseParams{
		ProjectID:      "proj-00000001",
		Version:        " v1.0.0 ",
		IdempotencyKey: &key,
	}
}

// TestCreateStoresVerifiedRelease: the happy path stores a release whose
// id came from the store, whose version is trimmed, whose title defaults
// to the version and whose manifest verifies against its hash.
func TestCreateStoresVerifiedRelease(t *testing.T) {
	cmd, _, _, _, _, storePort := fixtureCommand(t)
	release, err := cmd.Create(context.Background(), domain.User{ID: "user-00000001"}, fixtureCreateParams())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if release.ID == "" {
		t.Fatalf("stored release has no id")
	}
	if release.Version != "v1.0.0" {
		t.Fatalf("version = %q, want the trimmed input", release.Version)
	}
	if release.Title != "v1.0.0" {
		t.Fatalf("title = %q, want the version default", release.Title)
	}
	if release.StateID != "state-00000001" || release.CreatedBy != "user-00000001" {
		t.Fatalf("release row wrong: %+v", release)
	}
	if release.PolicyVersionID == nil || *release.PolicyVersionID != "policy-00000003" ||
		release.OrgPolicyVersionID == nil || *release.OrgPolicyVersionID != "policy-00000002" {
		t.Fatalf("policy pins wrong: %+v", release)
	}
	if release.ManifestHash == "" {
		t.Fatalf("manifest hash empty")
	}
	var m ReleaseManifest
	if err := json.Unmarshal(release.Manifest, &m); err != nil {
		t.Fatalf("stored manifest does not parse: %v", err)
	}
	if !m.VerifyHash() || m.ManifestHash != release.ManifestHash {
		t.Fatalf("stored manifest does not verify")
	}
	if len(storePort.releases) != 1 {
		t.Fatalf("store rows = %d, want 1", len(storePort.releases))
	}
}

// TestCreateSameVersionConflicts: the same version under a different key
// — or no key — is a conflict, never a silent overwrite (acceptance: 同
// version 重复 idempotent/conflict).
func TestCreateSameVersionConflicts(t *testing.T) {
	cmd, _, _, _, _, _ := fixtureCommand(t)
	key := "key-create-v1"
	if _, err := cmd.Create(context.Background(), domain.User{ID: "user-00000001"}, CreateReleaseParams{
		ProjectID: "proj-00000001", Version: "v1.0.0", IdempotencyKey: &key,
	}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	otherKey := "key-create-v1-again"
	if _, err := cmd.Create(context.Background(), domain.User{ID: "user-00000001"}, CreateReleaseParams{
		ProjectID: "proj-00000001", Version: "v1.0.0", IdempotencyKey: &otherKey,
	}); !errors.Is(err, ErrVersionTaken) {
		t.Fatalf("different key: Create error = %v, want ErrVersionTaken", err)
	}
	if _, err := cmd.Create(context.Background(), domain.User{ID: "user-00000001"}, CreateReleaseParams{
		ProjectID: "proj-00000001", Version: "v1.0.0",
	}); !errors.Is(err, ErrVersionTaken) {
		t.Fatalf("no key: Create error = %v, want ErrVersionTaken", err)
	}
}

// TestCreateReplayReturnsFirstRow: the same key replays the first
// create's row before any snapshot resolution — main may have moved and
// the gate may now block; the replay still answers the stored release.
func TestCreateReplayReturnsFirstRow(t *testing.T) {
	cmd, _, _, headsPort, gatePort, _ := fixtureCommand(t)
	params := fixtureCreateParams()
	first, err := cmd.Create(context.Background(), domain.User{ID: "user-00000001"}, params)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	// The project has moved on: main's head changed and the gate now
	// refuses. The replay must not care.
	headsPort.head.ID = "state-00000002"
	gatePort.report = rsgvalidation.Report{Gate: rsgvalidation.GateRelease, Verdict: rsgvalidation.VerdictBlocked}
	replayed, err := cmd.Create(context.Background(), domain.User{ID: "user-00000001"}, params)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replayed.ID != first.ID || replayed.ManifestHash != first.ManifestHash {
		t.Fatalf("replay row = %+v, want the first create's row %+v", replayed, first)
	}
}

// TestCreateRefusesWhenGateBlocks: a blocking report refuses the create
// with ErrReleaseGate carrying the full report (docs/22 §7: the command
// re-runs the gate, it never trusts a precheck).
func TestCreateRefusesWhenGateBlocks(t *testing.T) {
	cmd, _, _, _, gatePort, _ := fixtureCommand(t)
	gatePort.report = rsgvalidation.Report{
		Gate:        rsgvalidation.GateRelease,
		Verdict:     rsgvalidation.VerdictBlocked,
		Explanation: "no rights snapshot",
	}
	_, err := cmd.Create(context.Background(), domain.User{ID: "user-00000001"}, fixtureCreateParams())
	if !errors.Is(err, ErrReleaseGate) {
		t.Fatalf("Create error = %v, want ErrReleaseGate", err)
	}
	var refused *GateRefused
	if !errors.As(err, &refused) || refused.Report.Verdict != rsgvalidation.VerdictBlocked {
		t.Fatalf("Create error = %v, want a GateRefused carrying the report", err)
	}
}

// TestCreateForbiddenForViewerAndOutsider: a viewer membership and a
// complete outsider both answer ErrForbidden — and the denial precedes
// any target lookup (existence hiding, docs/45).
func TestCreateForbiddenForViewerAndOutsider(t *testing.T) {
	cmd, projectsPort, _, _, _, _ := fixtureCommand(t)
	projectsPort.membership.Role = domain.ProjectRoleViewer
	if _, err := cmd.Create(context.Background(), domain.User{ID: "user-00000001"}, fixtureCreateParams()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer: Create error = %v, want ErrForbidden", err)
	}
	projectsPort.memberErr = projects.ErrMemberNotFound
	if _, err := cmd.Create(context.Background(), domain.User{ID: "user-00000009"}, fixtureCreateParams()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("outsider: Create error = %v, want ErrForbidden", err)
	}
}

// TestCreateHidesMissingProject: an invisible project answers
// "not found", never "forbidden".
func TestCreateHidesMissingProject(t *testing.T) {
	cmd, projectsPort, _, _, _, _ := fixtureCommand(t)
	projectsPort.memberErr = projects.ErrProjectNotFound
	if _, err := cmd.Create(context.Background(), domain.User{ID: "user-00000001"}, fixtureCreateParams()); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("Create error = %v, want ErrProjectNotFound", err)
	}
}

// TestCreateValidatesInputShape: the version label and the title bound
// are checked before anything else runs.
func TestCreateValidatesInputShape(t *testing.T) {
	cmd, _, _, _, _, _ := fixtureCommand(t)
	for _, bad := range []CreateReleaseParams{
		{ProjectID: "", Version: "v1"},
		{ProjectID: "proj-00000001", Version: ""},
		{ProjectID: "proj-00000001", Version: "has space"},
		{ProjectID: "proj-00000001", Version: "v1", Title: strings.Repeat("t", 201)},
	} {
		if _, err := cmd.Create(context.Background(), domain.User{ID: "user-00000001"}, bad); !errors.Is(err, ErrValidation) {
			t.Fatalf("params %+v: Create error = %v, want ErrValidation", bad, err)
		}
	}
}

// TestCreateNotMainState: a project without a main branch (or main
// without a head) has no accepted snapshot to release.
func TestCreateNotMainState(t *testing.T) {
	cmd, _, _, headsPort, _, _ := fixtureCommand(t)
	headsPort.err = errors.New("no head")
	if _, err := cmd.Create(context.Background(), domain.User{ID: "user-00000001"}, fixtureCreateParams()); !errors.Is(err, ErrNotMainState) {
		t.Fatalf("Create error = %v, want ErrNotMainState", err)
	}
}

// TestCreateNoPolicyPinsNothing: a scope without a policy contributes no
// pin — the release's policy fields stay nil and the command hands the
// gate the nil-pin facts (the gate then refuses on release_rights, as
// the e2e proves with the real gate).
func TestCreateNoPolicyPinsNothing(t *testing.T) {
	cmd, _, policiesPort, _, gatePort, _ := fixtureCommand(t)
	policiesPort.org = nil
	policiesPort.project = nil
	gatePort.report = rsgvalidation.Report{Gate: rsgvalidation.GateRelease, Verdict: rsgvalidation.VerdictBlocked}
	if _, err := cmd.Create(context.Background(), domain.User{ID: "user-00000001"}, fixtureCreateParams()); !errors.Is(err, ErrReleaseGate) {
		t.Fatalf("Create error = %v, want ErrReleaseGate", err)
	}
}

// TestListReturnsNewestFirst: the command passes the store's list
// through.
func TestListReturnsNewestFirst(t *testing.T) {
	cmd, _, _, _, _, storePort := fixtureCommand(t)
	storePort.releases = []domain.Release{
		{ID: "release-00000002", ProjectID: "proj-00000001", Version: "v2"},
		{ID: "release-00000001", ProjectID: "proj-00000001", Version: "v1"},
	}
	releases, err := cmd.List(context.Background(), "proj-00000001")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(releases) != 2 || releases[0].Version != "v2" || releases[1].Version != "v1" {
		t.Fatalf("List = %+v, want newest first", releases)
	}
}

// TestGetMapsNotFound: an unknown release id answers ErrReleaseNotFound.
func TestGetMapsNotFound(t *testing.T) {
	cmd, _, _, _, _, _ := fixtureCommand(t)
	if _, err := cmd.Get(context.Background(), "proj-00000001", "release-00000099"); !errors.Is(err, ErrReleaseNotFound) {
		t.Fatalf("Get error = %v, want ErrReleaseNotFound", err)
	}
}

// TestManifestExportsStoredSnapshotOnly: the export renders from the
// stored row alone — moving main's head afterwards changes nothing
// (acceptance: 旧 release 不受 current state 改变).
func TestManifestExportsStoredSnapshotOnly(t *testing.T) {
	cmd, _, _, headsPort, _, storePort := fixtureCommand(t)
	release, err := cmd.Create(context.Background(), domain.User{ID: "user-00000001"}, fixtureCreateParams())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	first, err := cmd.Manifest(context.Background(), "proj-00000001", release.ID)
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	headsPort.head.ID = "state-00000099"
	second, err := cmd.Manifest(context.Background(), "proj-00000001", release.ID)
	if err != nil {
		t.Fatalf("Manifest after head move: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("manifest changed after the current state moved")
	}
	var m ReleaseManifest
	if err := json.Unmarshal(second, &m); err != nil {
		t.Fatalf("export does not parse: %v", err)
	}
	if !m.VerifyHash() {
		t.Fatalf("export does not verify")
	}
	if len(storePort.releases) != 1 {
		t.Fatalf("store rows = %d, want 1", len(storePort.releases))
	}
}

// TestManifestRefusesTamperedRow: a stored row whose content no longer
// verifies against its hash is refused, never served.
func TestManifestRefusesTamperedRow(t *testing.T) {
	cmd, _, _, _, _, storePort := fixtureCommand(t)
	release, err := cmd.Create(context.Background(), domain.User{ID: "user-00000001"}, fixtureCreateParams())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Tamper with the stored snapshot row after the fact.
	tampered := string(storePort.releases[0].Manifest)
	tampered = strings.Replace(tampered, `"v1.0.0"`, `"v1.0.1"`, 1)
	storePort.releases[0].Manifest = json.RawMessage(tampered)
	if _, err := cmd.Manifest(context.Background(), "proj-00000001", release.ID); !errors.Is(err, ErrStore) {
		t.Fatalf("Manifest error = %v, want ErrStore", err)
	}
}

// TestManifestRejectsForeignRelease: a release of another project is
// "not found" (the project boundary hides it, docs/45).
func TestManifestRejectsForeignRelease(t *testing.T) {
	cmd, _, _, _, _, _ := fixtureCommand(t)
	if _, err := cmd.Manifest(context.Background(), "proj-00000099", "release-00000001"); !errors.Is(err, ErrReleaseNotFound) {
		t.Fatalf("Manifest error = %v, want ErrReleaseNotFound", err)
	}
}
