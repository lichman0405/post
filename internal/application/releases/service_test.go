package releases

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/manifests"
	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/manifest"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

// fakeStatePort implements StatePort.
type fakeStatePort struct {
	state domain.ProjectState
	err   error
}

func (f *fakeStatePort) GetState(context.Context, string) (domain.ProjectState, error) {
	return f.state, f.err
}

// fakeManifestPort implements StateManifestPort.
type fakeManifestPort struct {
	m   *manifest.Manifest
	err error
}

func (f *fakeManifestPort) Export(context.Context, string) (*manifest.Manifest, error) {
	return f.m, f.err
}

// fakeProjectPort implements ProjectPort.
type fakeProjectPort struct {
	project domain.Project
	err     error
}

func (f *fakeProjectPort) GetProject(context.Context, string) (domain.Project, error) {
	return f.project, f.err
}

// fakeBranchPort implements MainBranchPort.
type fakeBranchPort struct {
	branches []domain.Branch
	err      error
}

func (f *fakeBranchPort) ListBranches(context.Context, string) ([]domain.Branch, error) {
	return f.branches, f.err
}

// fakePolicyPort implements PolicyVersionPort.
type fakePolicyPort struct {
	versions map[string]domain.PolicyVersion
	err      error
}

func (f *fakePolicyPort) GetVersion(_ context.Context, id string) (domain.PolicyVersion, error) {
	if f.err != nil {
		return domain.PolicyVersion{}, f.err
	}
	v, ok := f.versions[id]
	if !ok {
		return domain.PolicyVersion{}, policy.ErrPolicyNotFound
	}
	return v, nil
}

// fakeReviewPort implements ReleaseReviewPort.
type fakeReviewPort struct {
	records []ReviewRecord
	err     error
}

func (f *fakeReviewPort) ListReleaseReviews(context.Context, string, string) ([]ReviewRecord, error) {
	return f.records, f.err
}

// fakeSchemaPort implements SchemaRegistryPort.
type fakeSchemaPort struct {
	schemas map[schemareg.Ref]*schemareg.Schema
}

func (f *fakeSchemaPort) Get(ref schemareg.Ref) (*schemareg.Schema, bool) {
	s, ok := f.schemas[ref]
	return s, ok
}

// fixtureService wires a service over the happy-path fakes with a fixed
// clock: a project under organization org-00000001, main branch
// branch-00000001, the state committed on it, both policy versions, the
// fixture review record and the fixture schema registrations.
func fixtureService(t *testing.T) (*Service, *fakeStatePort, *fakeProjectPort) {
	t.Helper()
	mainBranch := "branch-00000001"
	org := "org-00000001"
	statesPort := &fakeStatePort{state: domain.ProjectState{
		ID:              "state-00000001",
		ProjectID:       "proj-00000001",
		BranchID:        &mainBranch,
		ManifestVersion: manifest.FormatV1,
	}}
	manifestsPort := &fakeManifestPort{m: fixtureStateManifest(t)}
	projectsPort := &fakeProjectPort{project: domain.Project{
		ID:             "proj-00000001",
		OrganizationID: &org,
	}}
	branchesPort := &fakeBranchPort{branches: []domain.Branch{
		{ID: mainBranch, ProjectID: "proj-00000001", Name: domain.MainBranchName},
	}}
	policiesPort := &fakePolicyPort{versions: map[string]domain.PolicyVersion{
		"policy-00000002": {
			ID:        "policy-00000002",
			Scope:     domain.PolicyScope{OrganizationID: org},
			Version:   "v2",
			Policy:    policyFixture(`{"main_protected":true,"release_min_reviewers":2}`),
			CreatedBy: "user-00000002",
			CreatedAt: fixedTime(9, 0),
		},
		"policy-00000003": {
			ID:        "policy-00000003",
			Scope:     domain.PolicyScope{ProjectID: "proj-00000001"},
			Version:   "v1",
			Policy:    policyFixture(`{"release_min_reviewers":3}`),
			CreatedBy: "user-00000001",
			CreatedAt: fixedTime(10, 0),
		},
	}}
	reviewsPort := &fakeReviewPort{records: fixtureReviews()}
	schemasPort := &fakeSchemaPort{schemas: map[schemareg.Ref]*schemareg.Schema{
		{ID: "https://open-rd.example/schemas/experiment.schema.json", Version: "1"}: {Ref: schemareg.Ref{ID: "https://open-rd.example/schemas/experiment.schema.json", Version: "1"}, ContentHash: "eeee"},
		{ID: "https://open-rd.example/schemas/hypothesis.schema.json", Version: "1"}: {Ref: schemareg.Ref{ID: "https://open-rd.example/schemas/hypothesis.schema.json", Version: "1"}, ContentHash: "hhhh"},
	}}
	svc := NewService(statesPort, manifestsPort, projectsPort, branchesPort, policiesPort, reviewsPort, schemasPort)
	svc.now = func() time.Time { return fixedTime(18, 0) }
	return svc, statesPort, projectsPort
}

// policyFixture parses a policy document the way the persistence layer
// delivers it.
func policyFixture(doc string) domain.Policy {
	p, err := domain.PolicyFromJSON(json.RawMessage(doc))
	if err != nil {
		panic(err)
	}
	return p
}

// fixtureBuildParams is the happy-path BuildParams of the fixture service.
func fixtureBuildParams() BuildParams {
	org := "policy-00000002"
	project := "policy-00000003"
	return BuildParams{
		ProjectID:              "proj-00000001",
		StateID:                "state-00000001",
		Version:                "  v1.0.0  ",
		OrgPolicyVersionID:     &org,
		ProjectPolicyVersionID: &project,
	}
}

// TestBuildAssemblesVerifiedManifest proves the service composes every
// read into a manifest whose hash verifies, whose version is the trimmed
// input, whose policy documents are the pinned canonical forms and whose
// schema pins carry the registry hashes.
func TestBuildAssemblesVerifiedManifest(t *testing.T) {
	svc, _, _ := fixtureService(t)
	m, err := svc.Build(context.Background(), fixtureBuildParams())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !m.VerifyHash() {
		t.Fatalf("built manifest does not verify")
	}
	if m.Version != "v1.0.0" {
		t.Fatalf("version = %q, want the trimmed input", m.Version)
	}
	if m.Policy.Organization.Version != "v2" || m.Policy.Project.Version != "v1" {
		t.Fatalf("policy pins wrong: %+v", m.Policy)
	}
	if !strings.Contains(string(m.Policy.Organization.Document), `"main_protected":true`) {
		t.Fatalf("org policy document not pinned canonically: %s", m.Policy.Organization.Document)
	}
	if len(m.Schemas) != 2 || m.Schemas[0].ContentHash != "eeee" {
		t.Fatalf("schema pins wrong: %+v", m.Schemas)
	}
	if len(m.Reviews) != 2 {
		t.Fatalf("review records wrong: %+v", m.Reviews)
	}
	if m.State.StateHash == "" || len(m.State.Content) == 0 {
		t.Fatalf("state pin empty")
	}
}

// TestBuildDeterministicAcrossRenders builds the same release twice
// through the service: the manifest hash must not move (acceptance:
// manifest deterministic).
func TestBuildDeterministicAcrossRenders(t *testing.T) {
	svc, _, _ := fixtureService(t)
	m1, err := svc.Build(context.Background(), fixtureBuildParams())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	m2, err := svc.Build(context.Background(), fixtureBuildParams())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if m1.ManifestHash != m2.ManifestHash {
		t.Fatalf("hash moved between service renders: %s != %s", m1.ManifestHash, m2.ManifestHash)
	}
}

// TestBuildRefusesNonMainState: a state committed on a research branch is
// not an accepted main snapshot — no release manifest exists for it.
func TestBuildRefusesNonMainState(t *testing.T) {
	svc, statesPort, _ := fixtureService(t)
	research := "branch-00000002"
	statesPort.state.BranchID = &research
	if _, err := svc.Build(context.Background(), fixtureBuildParams()); !errors.Is(err, ErrNotMainState) {
		t.Fatalf("Build error = %v, want ErrNotMainState", err)
	}
}

// TestBuildRefusesGenesisState: the genesis root predates branches — it
// is not on main either.
func TestBuildRefusesGenesisState(t *testing.T) {
	svc, statesPort, _ := fixtureService(t)
	statesPort.state.BranchID = nil
	if _, err := svc.Build(context.Background(), fixtureBuildParams()); !errors.Is(err, ErrNotMainState) {
		t.Fatalf("Build error = %v, want ErrNotMainState", err)
	}
}

// TestBuildHidesForeignState: a state of another project reports "not
// found", never a foreign-entity detail (docs/45).
func TestBuildHidesForeignState(t *testing.T) {
	svc, statesPort, _ := fixtureService(t)
	statesPort.state.ProjectID = "proj-00000099"
	if _, err := svc.Build(context.Background(), fixtureBuildParams()); !errors.Is(err, ErrStateNotFound) {
		t.Fatalf("Build error = %v, want ErrStateNotFound", err)
	}
}

// TestBuildMapsStoreSentinels: missing state, missing project and missing
// policy each map onto this package's vocabulary.
func TestBuildMapsStoreSentinels(t *testing.T) {
	svc, statesPort, projectsPort := fixtureService(t)

	statesPort.state = domain.ProjectState{}
	statesPort.err = states.ErrStateNotFound
	if _, err := svc.Build(context.Background(), fixtureBuildParams()); !errors.Is(err, ErrStateNotFound) {
		t.Fatalf("Build error = %v, want ErrStateNotFound", err)
	}
	statesPort.err = nil
	mainBranch := "branch-00000001"
	statesPort.state = domain.ProjectState{ID: "state-00000001", ProjectID: "proj-00000001", BranchID: &mainBranch}

	projectsPort.err = projects.ErrProjectNotFound
	if _, err := svc.Build(context.Background(), fixtureBuildParams()); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("Build error = %v, want ErrProjectNotFound", err)
	}
	projectsPort.err = nil

	params := fixtureBuildParams()
	missing := "policy-00000099"
	params.OrgPolicyVersionID = &missing
	if _, err := svc.Build(context.Background(), params); !errors.Is(err, ErrPolicyNotFound) {
		t.Fatalf("Build error = %v, want ErrPolicyNotFound", err)
	}
}

// TestBuildRefusesForeignPolicyScope: a policy version of another
// organization or another project is "not found" (existence-hiding), even
// though the row exists.
func TestBuildRefusesForeignPolicyScope(t *testing.T) {
	svc, _, _ := fixtureService(t)
	// A real row that belongs to a different organization.
	svc.policies.(*fakePolicyPort).versions["policy-00000099"] = domain.PolicyVersion{
		ID:        "policy-00000099",
		Scope:     domain.PolicyScope{OrganizationID: "org-00000099"},
		Version:   "v1",
		Policy:    policyFixture(`{"main_protected":true}`),
		CreatedBy: "user-00000099",
		CreatedAt: fixedTime(9, 0),
	}
	params := fixtureBuildParams()
	foreign := "policy-00000099"
	params.OrgPolicyVersionID = &foreign
	if _, err := svc.Build(context.Background(), params); !errors.Is(err, ErrPolicyNotFound) {
		t.Fatalf("org-scope mismatch: Build error = %v, want ErrPolicyNotFound", err)
	}

	// And a project-scoped row of another project.
	svc.policies.(*fakePolicyPort).versions["policy-00000098"] = domain.PolicyVersion{
		ID:        "policy-00000098",
		Scope:     domain.PolicyScope{ProjectID: "proj-00000099"},
		Version:   "v1",
		Policy:    policyFixture(`{"release_min_reviewers":3}`),
		CreatedBy: "user-00000099",
		CreatedAt: fixedTime(9, 0),
	}
	params = fixtureBuildParams()
	foreignProject := "policy-00000098"
	params.ProjectPolicyVersionID = &foreignProject
	if _, err := svc.Build(context.Background(), params); !errors.Is(err, ErrPolicyNotFound) {
		t.Fatalf("project-scope mismatch: Build error = %v, want ErrPolicyNotFound", err)
	}
}

// TestBuildValidatesVersionShape refuses release version strings outside
// the bounded label.
func TestBuildValidatesVersionShape(t *testing.T) {
	svc, _, _ := fixtureService(t)
	for _, bad := range []string{"", "   ", "has space", "x/y", strings.Repeat("v", 65)} {
		params := fixtureBuildParams()
		params.Version = bad
		if _, err := svc.Build(context.Background(), params); !errors.Is(err, ErrValidation) {
			t.Fatalf("version %q: Build error = %v, want ErrValidation", bad, err)
		}
	}
}

// TestBuildRefusesUnregisteredSchema: a snapshot referencing a schema the
// registry does not know cannot be pinned — no silent partial manifest.
func TestBuildRefusesUnregisteredSchema(t *testing.T) {
	svc, _, _ := fixtureService(t)
	state := fixtureStateManifest(t)
	ov := state.ObjectVersions
	ov[0].SchemaRef = manifest.SchemaRef{ID: "https://open-rd.example/schemas/unknown.schema.json", Version: "1"}
	state.ObjectVersions = ov
	// Rebuild the export with the unknown ref so the export stays
	// self-consistent (the state hash re-derives).
	rebuilt := rebuildStateManifest(t, state)
	svc.manifests = &fakeManifestPort{m: rebuilt}
	if _, err := svc.Build(context.Background(), fixtureBuildParams()); !errors.Is(err, ErrStore) {
		t.Fatalf("Build error = %v, want ErrStore", err)
	}
}

// rebuildStateManifest re-renders an export fixture through the manifest
// builder so tampered fields stay consistent with the state hash.
func rebuildStateManifest(t *testing.T, m *manifest.Manifest) *manifest.Manifest {
	t.Helper()
	var snap manifest.Snapshot
	for _, ov := range m.ObjectVersions {
		snap.ObjectVersions = append(snap.ObjectVersions, manifest.ObjectVersion{
			ID: ov.ID, ObjectID: ov.ObjectID, ObjectType: ov.ObjectType,
			VersionNo: ov.VersionNo, StateID: ov.StateID, BranchID: ov.BranchID,
			SchemaRef: ov.SchemaRef, Title: ov.Title, LifecycleState: ov.LifecycleState,
			Payload: ov.Payload, VisibilityPolicyID: ov.VisibilityPolicyID,
			IntegrityHash: ov.IntegrityHash, CreatedBy: ov.CreatedBy, CreatedAt: ov.CreatedAt,
		})
	}
	snap.RelationVersions = m.RelationVersions
	snap.BlobRefs = m.BlobRefs
	rebuilt, err := manifest.Build(domain.ProjectState{
		ID:              m.StateID,
		ProjectID:       m.ProjectID,
		GitCommitSHA:    m.GitRef,
		ManifestVersion: manifest.FormatV1,
	}, snap, fixedTime(14, 0))
	if err != nil {
		t.Fatalf("manifest.Build: %v", err)
	}
	return rebuilt
}

// TestReleaseFactsAssembled: the happy path yields all three facts true.
func TestReleaseFactsAssembled(t *testing.T) {
	svc, _, _ := fixtureService(t)
	facts, err := svc.ReleaseFacts(context.Background(), FactsParams{
		ProjectID:              "proj-00000001",
		StateID:                "state-00000001",
		OrgPolicyVersionID:     fixtureBuildParams().OrgPolicyVersionID,
		ProjectPolicyVersionID: fixtureBuildParams().ProjectPolicyVersionID,
	})
	if err != nil {
		t.Fatalf("ReleaseFacts: %v", err)
	}
	if !facts.ReviewApproved || !facts.RightsSnapshot || !facts.FromMainBranch {
		t.Fatalf("facts = %+v, want all true", facts)
	}
}

// TestReleaseFactsReportedNotEnforced: a research-branch state yields
// false facts, not an error — the release gate refuses, the fact
// assembler reports.
func TestReleaseFactsReportedNotEnforced(t *testing.T) {
	svc, statesPort, _ := fixtureService(t)
	research := "branch-00000002"
	statesPort.state.BranchID = &research
	facts, err := svc.ReleaseFacts(context.Background(), FactsParams{
		ProjectID: "proj-00000001",
		StateID:   "state-00000001",
	})
	if err != nil {
		t.Fatalf("ReleaseFacts: %v", err)
	}
	if facts.FromMainBranch {
		t.Fatalf("facts = %+v, want FromMainBranch false", facts)
	}
}

// TestReleaseFactsRequireBothReviewKinds: an approved scientific review
// alone does not approve the record — integrity must be approved too
// (docs/09 §5: the dimensions are recorded separately).
func TestReleaseFactsRequireBothReviewKinds(t *testing.T) {
	svc, _, _ := fixtureService(t)
	svc.reviews = &fakeReviewPort{records: []ReviewRecord{{
		PullRequestNumber: 1,
		ProposedStateID:   "state-00000001",
		Reviews: []Review{{
			ID: "rev-00000001", ReviewerID: "user-00000002",
			ReviewKind: "scientific", Decision: "approved",
			CreatedAt: fixedTime(14, 0),
		}},
	}}}
	facts, err := svc.ReleaseFacts(context.Background(), FactsParams{
		ProjectID: "proj-00000001",
		StateID:   "state-00000001",
	})
	if err != nil {
		t.Fatalf("ReleaseFacts: %v", err)
	}
	if facts.ReviewApproved {
		t.Fatalf("facts = %+v, want ReviewApproved false without an approved integrity review", facts)
	}
}

// TestReleaseFactsRightsSnapshotFollowsPins: with no policy versions
// named, the rights fact is false — the release gate refuses.
func TestReleaseFactsRightsSnapshotFollowsPins(t *testing.T) {
	svc, _, _ := fixtureService(t)
	facts, err := svc.ReleaseFacts(context.Background(), FactsParams{
		ProjectID: "proj-00000001",
		StateID:   "state-00000001",
	})
	if err != nil {
		t.Fatalf("ReleaseFacts: %v", err)
	}
	if facts.RightsSnapshot {
		t.Fatalf("facts = %+v, want RightsSnapshot false without policy pins", facts)
	}
}

// TestBuildAllowsPolicyWithoutOrg: a personal project pins only its
// project policy — the org half stays null.
func TestBuildAllowsPolicyWithoutOrg(t *testing.T) {
	svc, _, projectsPort := fixtureService(t)
	projectsPort.project.OrganizationID = nil
	params := fixtureBuildParams()
	params.OrgPolicyVersionID = nil
	m, err := svc.Build(context.Background(), params)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if m.Policy.Organization != nil || m.Policy.Project == nil {
		t.Fatalf("policy pin wrong for a personal project: %+v", m.Policy)
	}
}

// TestBuildMapsManifestErrors: an export failure maps onto this package's
// vocabulary, not the manifests package's.
func TestBuildMapsManifestErrors(t *testing.T) {
	svc, _, _ := fixtureService(t)
	svc.manifests = &fakeManifestPort{err: manifests.ErrStore}
	if _, err := svc.Build(context.Background(), fixtureBuildParams()); !errors.Is(err, ErrStore) {
		t.Fatalf("Build error = %v, want ErrStore", err)
	}
}
