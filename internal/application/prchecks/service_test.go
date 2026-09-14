package prchecks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/integrity"
	"github.com/lichman0405/post/internal/rsg/manifest"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

func strPtr(s string) *string { return &s }

func hashOf(payload string) string {
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// fixtures build the well-formed PR facts the service assembles: project
// proj-1 in org-1, PR #7 proposing source branch src (chain s1→s2) onto
// target branch main (chain m1→m2 rooted at the genesis state), base m2,
// proposed s2, dataset v1 unchanged across the lineages, material v1 new,
// both pinned to policy-1.

var (
	genesis = domain.ProjectState{ID: "state-genesis", ProjectID: "proj-1"}
	m1      = domain.ProjectState{ID: "state-m1", ProjectID: "proj-1", BranchID: strPtr("branch-main"), ParentStateID: strPtr("state-genesis")}
	m2      = domain.ProjectState{ID: "state-m2", ProjectID: "proj-1", BranchID: strPtr("branch-main"), ParentStateID: strPtr("state-m1")}
	s1      = domain.ProjectState{ID: "state-s1", ProjectID: "proj-1", BranchID: strPtr("branch-src"), ParentStateID: strPtr("state-m2")}
	s2      = domain.ProjectState{ID: "state-s2", ProjectID: "proj-1", BranchID: strPtr("branch-src"), ParentStateID: strPtr("state-s1")}
	c1      = domain.StateCommit{ID: "commit-1", ProjectID: "proj-1", BranchID: "branch-src", BaseStateID: strPtr("state-m2"), ResultStateID: "state-s1", ActorID: "u-alice", Via: domain.ViaAPI, Message: "first"}
	c2      = domain.StateCommit{ID: "commit-2", ProjectID: "proj-1", BranchID: "branch-src", BaseStateID: strPtr("state-s1"), ResultStateID: "state-s2", ActorID: "u-alice", Via: domain.ViaAPI, Message: "second"}
	policy1 = domain.PolicyVersion{ID: "policy-1", Scope: domain.PolicyScope{ProjectID: "proj-1"}, Version: "1"}
	// o1 is a state of a THIRD branch: it exists in the project, is on no
	// chain of this PR, and is no head. Corrupt rows pin their base or
	// proposed here — "exists and belongs to the project" is all the
	// boundary resolution used to ask, which is what made the empty-chain
	// pins accept themselves.
	o1 = domain.ProjectState{ID: "state-o1", ProjectID: "proj-1", BranchID: strPtr("branch-other"), ParentStateID: strPtr("state-m2")}
)

func datasetVersion(id, objectID string, pin *string, payload string) manifest.ObjectVersion {
	return manifest.ObjectVersion{
		ID: id, ObjectID: objectID, ObjectType: "dataset", VersionNo: 1,
		StateID: "state-s2", BranchID: strPtr("branch-src"),
		SchemaRef: manifest.SchemaRef{ID: "https://open-rd.example/schemas/dataset.schema.json", Version: "1"},
		Title:     "Measurements", LifecycleState: "active",
		Payload:            json.RawMessage(payload),
		VisibilityPolicyID: pin,
		IntegrityHash:      hashOf(payload),
		CreatedBy:          "u-alice",
		CreatedAt:          time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC),
	}
}

func materialVersion(id, objectID string, pin *string) manifest.ObjectVersion {
	payload := `{}`
	return manifest.ObjectVersion{
		ID: id, ObjectID: objectID, ObjectType: "material", VersionNo: 1,
		StateID: "state-s2", BranchID: strPtr("branch-src"),
		SchemaRef: manifest.SchemaRef{ID: "https://open-rd.example/schemas/material.schema.json", Version: "1"},
		Title:     "Alloy", LifecycleState: "active",
		Payload:            json.RawMessage(payload),
		VisibilityPolicyID: pin,
		IntegrityHash:      hashOf(payload),
		CreatedBy:          "u-alice",
		CreatedAt:          time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC),
	}
}

// fakeStateReader serves the chains, commits and single-state reads.
type fakeStateReader struct {
	states  map[string][]domain.ProjectState
	commits map[string][]domain.StateCommit
	byID    map[string]domain.ProjectState
}

func (f *fakeStateReader) ListStates(_ context.Context, branchID string) ([]domain.ProjectState, error) {
	return f.states[branchID], nil
}

func (f *fakeStateReader) ListCommits(_ context.Context, branchID string) ([]domain.StateCommit, error) {
	return f.commits[branchID], nil
}

func (f *fakeStateReader) GetState(_ context.Context, stateID string) (domain.ProjectState, error) {
	state, ok := f.byID[stateID]
	if !ok {
		return domain.ProjectState{}, states.ErrStateNotFound
	}
	return state, nil
}

// fakeManifestReader serves the two lineages.
type fakeManifestReader struct {
	snaps map[string]manifest.Snapshot
}

func (f *fakeManifestReader) GetManifestSnapshot(_ context.Context, stateID string) (manifest.Snapshot, error) {
	return f.snaps[stateID], nil
}

// fakePolicyLister serves per-scope policy histories and records the
// scopes asked for.
type fakePolicyLister struct {
	byScope map[domain.PolicyScope][]domain.PolicyVersion
	calls   []domain.PolicyScope
}

func (f *fakePolicyLister) List(_ context.Context, scope domain.PolicyScope) ([]domain.PolicyVersion, error) {
	f.calls = append(f.calls, scope)
	return f.byScope[scope], nil
}

// fakePRReader serves one PR row.
type fakePRReader struct {
	pr  domain.PullRequest
	err error
}

func (f *fakePRReader) GetPullRequest(_ context.Context, _ string, _ int64) (domain.PullRequest, error) {
	return f.pr, f.err
}

// fakeBranchHeadReader serves the branches' heads (the empty-chain
// boundary read) and records the branch ids asked for.
type fakeBranchHeadReader struct {
	heads map[string]domain.ProjectState
	errs  map[string]error
	calls []string
}

func (f *fakeBranchHeadReader) GetBranchHead(_ context.Context, branchID string) (domain.ProjectState, error) {
	f.calls = append(f.calls, branchID)
	if err := f.errs[branchID]; err != nil {
		return domain.ProjectState{}, err
	}
	head, ok := f.heads[branchID]
	if !ok {
		return domain.ProjectState{}, branches.ErrBranchNotFound
	}
	return head, nil
}

// fakeProjectReader serves one project row.
type fakeProjectReader struct {
	project domain.Project
	err     error
}

func (f *fakeProjectReader) GetProject(_ context.Context, _ string) (domain.Project, error) {
	return f.project, f.err
}

// newTestService wires the service over a happy fixture. The returned
// branchHead fake is the empty-chain boundary read: each branch's head is
// where the chain currently points, which for a branch with no states of
// its own is its fork point.
func newTestService(t *testing.T) (*Service, *fakePolicyLister, *fakeStateReader, *fakeBranchHeadReader) {
	t.Helper()
	branchHeads := &fakeBranchHeadReader{heads: map[string]domain.ProjectState{
		"branch-main": m2,
		"branch-src":  s2,
	}}
	stateReader := &fakeStateReader{
		states: map[string][]domain.ProjectState{
			"branch-main": {m1, m2},
			"branch-src":  {s1, s2},
		},
		commits: map[string][]domain.StateCommit{
			"branch-src": {c1, c2},
		},
		byID: map[string]domain.ProjectState{
			genesis.ID: genesis, m1.ID: m1, m2.ID: m2, s1.ID: s1, s2.ID: s2, o1.ID: o1,
		},
	}
	datasetV1 := datasetVersion("ov-d1", "obj-dataset", strPtr("policy-1"),
		`{"purpose":"store measurements","blob_ids":["blob-1"]}`)
	materialV1 := materialVersion("ov-m1", "obj-material", strPtr("policy-1"))
	manifests := &fakeManifestReader{snaps: map[string]manifest.Snapshot{
		m2.ID: {
			ObjectVersions: []manifest.ObjectVersion{datasetV1},
		},
		s2.ID: {
			ObjectVersions: []manifest.ObjectVersion{datasetV1, materialV1},
			BlobRefs:       []manifest.BlobRef{{ID: "blob-1", Hash: "hash-1"}},
		},
	}}
	policies := &fakePolicyLister{byScope: map[domain.PolicyScope][]domain.PolicyVersion{
		{ProjectID: "proj-1"}: {policy1},
	}}
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	svc := NewService(Deps{
		PRs: &fakePRReader{pr: domain.PullRequest{
			ID: "pr-1", ProjectID: "proj-1", Number: 7,
			SourceBranchID: "branch-src", TargetBranchID: "branch-main",
			BaseStateID: m2.ID, ProposedStateID: s2.ID,
			Title: "proposal", State: domain.PullRequestStateReviewRequired, CreatedBy: "u-alice",
		}},
		Projects: &fakeProjectReader{project: domain.Project{ID: "proj-1", OrganizationID: strPtr("org-1")}},
		States:   stateReader,
		Branches: branchHeads,
		Manifest: manifests,
		Policies: policies,
		Engine:   integrity.New(reg),
	})
	return svc, policies, stateReader, branchHeads
}

func TestCheckPullRequestHappyPath(t *testing.T) {
	svc, policies, _, _ := newTestService(t)
	report, err := svc.CheckPullRequest(context.Background(), "proj-1", 7)
	if err != nil {
		t.Fatalf("CheckPullRequest: %v", err)
	}
	if report.Blocked() {
		t.Fatalf("happy path blocked:\n%s", report.Explanation)
	}
	if report.Kind != "integrity" || report.ProjectID != "proj-1" || report.PRNumber != 7 {
		t.Fatalf("report address = %q/%s/#%d, want integrity/proj-1/#7", report.Kind, report.ProjectID, report.PRNumber)
	}
	if report.BaseStateID != m2.ID || report.ProposedStateID != s2.ID {
		t.Fatalf("report pins = %s→%s, want %s→%s", report.BaseStateID, report.ProposedStateID, m2.ID, s2.ID)
	}
	if report.ComputedAt == "" {
		t.Fatal("report lacks the computed_at stamp")
	}
	if _, err := time.Parse(time.RFC3339, report.ComputedAt); err != nil {
		t.Fatalf("computed_at is not RFC3339: %v", err)
	}
	// The org policy and the project policy were both listed.
	if len(policies.calls) != 2 {
		t.Fatalf("policy scopes asked = %v, want org + project", policies.calls)
	}
}

func TestCheckPullRequestValidation(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	if _, err := svc.CheckPullRequest(context.Background(), "", 7); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty project: err = %v, want ErrValidation", err)
	}
	if _, err := svc.CheckPullRequest(context.Background(), "proj-1", 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("zero number: err = %v, want ErrValidation", err)
	}
}

func TestCheckPullRequestNotFound(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	svc.prs = &fakePRReader{err: pullrequests.ErrPullRequestNotFound}
	_, err := svc.CheckPullRequest(context.Background(), "proj-1", 999)
	if !errors.Is(err, ErrPullRequestNotFound) {
		t.Fatalf("err = %v, want ErrPullRequestNotFound", err)
	}
}

func TestCheckPullRequestBlocked(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	svc.prs = &fakePRReader{pr: domain.PullRequest{
		ID: "pr-1", ProjectID: "proj-1", Number: 7,
		SourceBranchID: "branch-src", TargetBranchID: "branch-main",
		// A base pin outside the target chain and outside every boundary.
		BaseStateID: "state-elsewhere", ProposedStateID: s2.ID,
		Title: "proposal", State: domain.PullRequestStateReviewRequired, CreatedBy: "u-alice",
	}}
	report, err := svc.CheckPullRequest(context.Background(), "proj-1", 7)
	if err != nil {
		t.Fatalf("CheckPullRequest: %v", err)
	}
	if !report.Blocked() {
		t.Fatalf("corrupt base pin did not block:\n%s", report.Explanation)
	}
}

// wantBlockedOn runs the check and requires the report to block on exactly
// the given blocking checks — no more, no fewer.
func wantBlockedOn(t *testing.T, svc *Service, checks ...integrity.CheckID) integrity.Report {
	t.Helper()
	report, err := svc.CheckPullRequest(context.Background(), "proj-1", 7)
	if err != nil {
		t.Fatalf("CheckPullRequest: %v", err)
	}
	failures := report.BlockingFailures()
	if len(failures) != len(checks) {
		t.Fatalf("blocking failures = %+v, want exactly %v (verdict %s):\n%s", failures, checks, report.Verdict, report.Explanation)
	}
	for i, want := range checks {
		if failures[i].Check != want {
			t.Fatalf("blocking failure %d = %s, want %s (all: %+v):\n%s", i, failures[i].Check, want, failures, report.Explanation)
		}
	}
	return report
}

// pinPR replaces the PR row the service reads (the corrupt row shape: the
// pins are the only tell, every foreign key still holds).
func pinPR(svc *Service, base, proposed string) {
	svc.prs = &fakePRReader{pr: domain.PullRequest{
		ID: "pr-1", ProjectID: "proj-1", Number: 7,
		SourceBranchID: "branch-src", TargetBranchID: "branch-main",
		BaseStateID: base, ProposedStateID: proposed,
		Title: "proposal", State: domain.PullRequestStateReviewRequired, CreatedBy: "u-alice",
	}}
}

// TestResolveBoundaries covers the two resolutions the application layer
// hands the engine: a chain with states roots at its first state's parent,
// a chain without states roots at the BRANCH ROW's head. The empty-chain
// rule is what the corrupt-pin probes below attack: resolving the boundary
// from the PR under review would make the pin equal to its own boundary
// and pass for any value.
func TestResolveBoundaries(t *testing.T) {
	t.Run("non-empty chains resolve their roots' parents", func(t *testing.T) {
		svc, _, _, branchHeads := newTestService(t)
		report, err := svc.CheckPullRequest(context.Background(), "proj-1", 7)
		if err != nil {
			t.Fatalf("CheckPullRequest: %v", err)
		}
		if report.Blocked() {
			t.Fatalf("happy fixture blocked:\n%s", report.Explanation)
		}
		if len(branchHeads.calls) != 0 {
			t.Fatalf("non-empty chains read branch heads %v, want none", branchHeads.calls)
		}
	})

	t.Run("an empty chain takes its own branch's head, not the PR's pin", func(t *testing.T) {
		// Both shapes at once, each pin naming the branch's own head:
		// branch-main's head is m2 (the base pin) and branch-src's is s2
		// (the proposed pin).
		svc, _, stateReader, branchHeads := newTestService(t)
		stateReader.states["branch-main"] = nil
		stateReader.states["branch-src"] = nil
		stateReader.commits["branch-src"] = nil
		report, err := svc.CheckPullRequest(context.Background(), "proj-1", 7)
		if err != nil {
			t.Fatalf("CheckPullRequest: %v", err)
		}
		if report.Blocked() {
			t.Fatalf("pins equal to the branches' heads blocked:\n%s", report.Explanation)
		}
		if len(branchHeads.calls) != 2 {
			t.Fatalf("head reads = %v, want one per empty chain (branch-main, branch-src)", branchHeads.calls)
		}
	})

	t.Run("an empty source chain rejects a proposed pin on a third branch's state", func(t *testing.T) {
		// The probe the review reproduced: a raw-INSERTed PR whose source
		// branch has no states of its own and whose proposed pin names a
		// state of an unrelated branch. Nothing but the pin is wrong —
		// every foreign key holds — so the pin is the only tell, and it
		// must block on proposed_on_source_chain.
		svc, _, stateReader, _ := newTestService(t)
		stateReader.states["branch-src"] = nil
		stateReader.commits["branch-src"] = nil
		pinPR(svc, m2.ID, o1.ID)
		wantBlockedOn(t, svc, integrity.CheckProposedOnSourceChain)
	})

	t.Run("an empty target chain rejects a base pin on a third branch's state", func(t *testing.T) {
		svc, _, stateReader, _ := newTestService(t)
		stateReader.states["branch-main"] = nil
		pinPR(svc, o1.ID, s2.ID)
		wantBlockedOn(t, svc, integrity.CheckBaseOnTargetChain)
	})

	t.Run("an empty target chain rejects a base pin that is not the head", func(t *testing.T) {
		// The pin names a state the project owns and the PR's own source
		// branch holds: still not branch-main's head, so still no boundary.
		svc, _, stateReader, _ := newTestService(t)
		stateReader.states["branch-main"] = nil
		stateReader.byID[o1.ID] = o1
		pinPR(svc, s1.ID, s2.ID)
		wantBlockedOn(t, svc, integrity.CheckBaseOnTargetChain)
	})

	t.Run("an unreadable head blocks instead of passing the pin", func(t *testing.T) {
		// The branch row's head pointer names no state: the boundary is
		// unknown, so the pin is unverifiable — a failure, never a pass.
		svc, _, stateReader, branchHeads := newTestService(t)
		stateReader.states["branch-main"] = nil
		branchHeads.errs = map[string]error{"branch-main": branches.ErrStateNotFound}
		wantBlockedOn(t, svc, integrity.CheckBaseOnTargetChain)
	})

	t.Run("an unknown branch blocks instead of passing the pin", func(t *testing.T) {
		svc, _, stateReader, branchHeads := newTestService(t)
		stateReader.states["branch-src"] = nil
		stateReader.commits["branch-src"] = nil
		branchHeads.errs = map[string]error{"branch-src": branches.ErrBranchNotFound}
		wantBlockedOn(t, svc, integrity.CheckProposedOnSourceChain)
	})

	t.Run("a head read failure is a store error, not a verdict", func(t *testing.T) {
		// An unreachable database must not be reported as a blocked
		// proposal: the caller has to be able to tell "this row is corrupt"
		// from "we could not ask".
		svc, _, stateReader, branchHeads := newTestService(t)
		stateReader.states["branch-main"] = nil
		branchHeads.errs = map[string]error{"branch-main": errors.New("connection refused")}
		_, err := svc.CheckPullRequest(context.Background(), "proj-1", 7)
		if !errors.Is(err, ErrStore) {
			t.Fatalf("err = %v, want ErrStore", err)
		}
	})

	t.Run("a base pin on another project's state is not a boundary", func(t *testing.T) {
		svc, _, stateReader, _ := newTestService(t)
		stateReader.states["branch-main"] = nil
		stateReader.byID["state-foreign"] = domain.ProjectState{ID: "state-foreign", ProjectID: "proj-other"}
		pinPR(svc, "state-foreign", s2.ID)
		wantBlockedOn(t, svc, integrity.CheckBaseOnTargetChain)
	})
}
