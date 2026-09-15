package prdiff

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/diffs"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/diff"
	"github.com/lichman0405/post/internal/rsg/manifest"
)

// fakePRs serves one canned PR row.
type fakePRs struct {
	pr    domain.PullRequest
	err   error
	gotID string
	gotN  int64
}

func (f *fakePRs) GetPullRequest(_ context.Context, projectID string, number int64) (domain.PullRequest, error) {
	f.gotID, f.gotN = projectID, number
	return f.pr, f.err
}

// fakeHeads serves one canned branch head.
type fakeHeads struct {
	state     domain.ProjectState
	err       error
	gotBranch string
}

func (f *fakeHeads) GetBranchHead(_ context.Context, branchID string) (domain.ProjectState, error) {
	f.gotBranch = branchID
	return f.state, f.err
}

// fakeDiff records the params it was called with and returns a canned
// outcome. It is the seam that proves WHICH three states the service
// resolved — the assertion this package's whole job rests on.
type fakeDiff struct {
	calls []diffs.Params
	out   *diff.Diff
	err   error
}

func (f *fakeDiff) Diff(_ context.Context, in diffs.Params) (*diff.Diff, error) {
	f.calls = append(f.calls, in)
	return f.out, f.err
}

func testPR() domain.PullRequest {
	return domain.PullRequest{
		ID: "pr-1", ProjectID: "proj-1", Number: 7,
		SourceBranchID: "branch-src", TargetBranchID: "branch-main",
		BaseStateID: "state-base", ProposedStateID: "state-proposed",
		State: domain.PullRequestStateReviewRequired,
	}
}

func newService(prs *fakePRs, heads *fakeHeads, differ *fakeDiff) *Service {
	return NewService(prs, heads, differ)
}

// TestPullRequestDiffResolvesTheThreeStates is the core contract: the
// base is the PR's OWN pin (never the target's current head), the source
// is the pinned proposal, and the target is the target branch's current
// head.
func TestPullRequestDiffResolvesTheThreeStates(t *testing.T) {
	prs := &fakePRs{pr: testPR()}
	heads := &fakeHeads{state: domain.ProjectState{ID: "state-target-head", ProjectID: "proj-1"}}
	differ := &fakeDiff{out: &diff.Diff{FormatVersion: diff.FormatV1}}
	svc := newService(prs, heads, differ)

	d, err := svc.PullRequestDiff(context.Background(), "proj-1", 7)
	if err != nil {
		t.Fatalf("PullRequestDiff: %v", err)
	}
	if d == nil || d.FormatVersion != diff.FormatV1 {
		t.Fatalf("diff = %+v", d)
	}
	if len(differ.calls) != 1 {
		t.Fatalf("diff engine calls = %d, want 1", len(differ.calls))
	}
	got := differ.calls[0]
	want := diffs.Params{
		ProjectID:     "proj-1",
		BaseStateID:   "state-base",
		SourceStateID: "state-proposed",
		TargetStateID: "state-target-head",
	}
	if got != want {
		t.Fatalf("diff params = %+v, want %+v", got, want)
	}
	if prs.gotID != "proj-1" || prs.gotN != 7 {
		t.Fatalf("PR read = %q/#%d", prs.gotID, prs.gotN)
	}
	if heads.gotBranch != "branch-main" {
		t.Fatalf("head read = branch %q, want branch-main", heads.gotBranch)
	}
}

// TestPullRequestDiffBaseDoesNotDrift: even when the target's head equals
// nothing the PR pinned, the base stays the PR's own BaseStateID — the
// read never re-derives it from the target branch.
func TestPullRequestDiffBaseDoesNotDrift(t *testing.T) {
	prs := &fakePRs{pr: testPR()}
	// The target branch has moved far past the PR's base.
	heads := &fakeHeads{state: domain.ProjectState{ID: "state-target-moved-9"}}
	differ := &fakeDiff{out: &diff.Diff{}}
	svc := newService(prs, heads, differ)

	if _, err := svc.PullRequestDiff(context.Background(), "proj-1", 7); err != nil {
		t.Fatalf("PullRequestDiff: %v", err)
	}
	if differ.calls[0].BaseStateID != "state-base" {
		t.Fatalf("base = %q, want the PR's own pin state-base", differ.calls[0].BaseStateID)
	}
	if differ.calls[0].TargetStateID != "state-target-moved-9" {
		t.Fatalf("target = %q, want the target branch's current head", differ.calls[0].TargetStateID)
	}
}

func TestPullRequestDiffValidation(t *testing.T) {
	differ := &fakeDiff{}
	t.Run("empty project", func(t *testing.T) {
		svc := newService(&fakePRs{}, &fakeHeads{}, differ)
		if _, err := svc.PullRequestDiff(context.Background(), "", 7); !errors.Is(err, ErrValidation) {
			t.Fatalf("err = %v, want ErrValidation", err)
		}
	})
	t.Run("non-positive number", func(t *testing.T) {
		svc := newService(&fakePRs{}, &fakeHeads{}, differ)
		if _, err := svc.PullRequestDiff(context.Background(), "proj-1", 0); !errors.Is(err, ErrValidation) {
			t.Fatalf("err = %v, want ErrValidation", err)
		}
	})
	t.Run("unwired service fails closed", func(t *testing.T) {
		svc := NewService(nil, nil, nil)
		if _, err := svc.PullRequestDiff(context.Background(), "proj-1", 7); !errors.Is(err, ErrStore) {
			t.Fatalf("err = %v, want ErrStore", err)
		}
	})
	// A validation failure never reaches the diff engine.
	if len(differ.calls) != 0 {
		t.Fatalf("diff engine called %d times on invalid input", len(differ.calls))
	}
}

func TestPullRequestDiffErrorMapping(t *testing.T) {
	t.Run("missing PR", func(t *testing.T) {
		svc := newService(&fakePRs{err: pullrequests.ErrPullRequestNotFound}, &fakeHeads{}, &fakeDiff{})
		if _, err := svc.PullRequestDiff(context.Background(), "proj-1", 7); !errors.Is(err, ErrPullRequestNotFound) {
			t.Fatalf("err = %v, want ErrPullRequestNotFound", err)
		}
	})
	t.Run("PR store failure", func(t *testing.T) {
		svc := newService(&fakePRs{err: errors.New("connection reset")}, &fakeHeads{}, &fakeDiff{})
		if _, err := svc.PullRequestDiff(context.Background(), "proj-1", 7); !errors.Is(err, ErrStore) {
			t.Fatalf("err = %v, want ErrStore", err)
		}
	})
	t.Run("target branch has no head state", func(t *testing.T) {
		svc := newService(&fakePRs{pr: testPR()}, &fakeHeads{err: branches.ErrStateNotFound}, &fakeDiff{})
		if _, err := svc.PullRequestDiff(context.Background(), "proj-1", 7); !errors.Is(err, ErrStateNotFound) {
			t.Fatalf("err = %v, want ErrStateNotFound", err)
		}
	})
	t.Run("unknown target branch", func(t *testing.T) {
		svc := newService(&fakePRs{pr: testPR()}, &fakeHeads{err: branches.ErrBranchNotFound}, &fakeDiff{})
		if _, err := svc.PullRequestDiff(context.Background(), "proj-1", 7); !errors.Is(err, ErrStore) {
			t.Fatalf("err = %v, want ErrStore", err)
		}
	})
	t.Run("diff reports a missing state", func(t *testing.T) {
		differ := &fakeDiff{err: fmt.Errorf("%w: state gone", diffs.ErrStateNotFound)}
		svc := newService(&fakePRs{pr: testPR()}, &fakeHeads{state: domain.ProjectState{ID: "s-t"}}, differ)
		if _, err := svc.PullRequestDiff(context.Background(), "proj-1", 7); !errors.Is(err, ErrStateNotFound) {
			t.Fatalf("err = %v, want ErrStateNotFound", err)
		}
	})
	t.Run("diff reports stored-data corruption as a store failure", func(t *testing.T) {
		differ := &fakeDiff{err: fmt.Errorf("%w: base belongs to another project", diffs.ErrValidation)}
		svc := newService(&fakePRs{pr: testPR()}, &fakeHeads{state: domain.ProjectState{ID: "s-t"}}, differ)
		err := func() error { _, e := svc.PullRequestDiff(context.Background(), "proj-1", 7); return e }()
		if !errors.Is(err, ErrStore) || errors.Is(err, ErrStateNotFound) {
			t.Fatalf("err = %v, want ErrStore (not a caller error, not a missing state)", err)
		}
	})
	t.Run("diff engine failure", func(t *testing.T) {
		differ := &fakeDiff{err: fmt.Errorf("%w: snapshot unreadable", diffs.ErrStore)}
		svc := newService(&fakePRs{pr: testPR()}, &fakeHeads{state: domain.ProjectState{ID: "s-t"}}, differ)
		if _, err := svc.PullRequestDiff(context.Background(), "proj-1", 7); !errors.Is(err, ErrStore) {
			t.Fatalf("err = %v, want ErrStore", err)
		}
	})
}

// TestPullRequestDiffEndToEndOverRealEngine runs the service over the
// real diff engine with in-memory snapshots: the PR's pinned base and
// proposed head plus a target that moved concurrently. It proves the
// resolution is wired to real inputs, not only to a fake's argument
// capture.
func TestPullRequestDiffEndToEndOverRealEngine(t *testing.T) {
	pr := testPR()
	pr.BaseStateID = "state-base"
	pr.ProposedStateID = "state-proposed"

	snapshots := map[string]manifest.Snapshot{
		"state-base": {ObjectVersions: []manifest.ObjectVersion{{
			ID: "v1", ObjectID: "obj-1", ObjectType: "claim", VersionNo: 1,
			StateID: "state-base", Title: "base claim", LifecycleState: "active",
			Payload: []byte(`{"statement":"base"}`),
		}}},
		"state-proposed": {ObjectVersions: []manifest.ObjectVersion{{
			ID: "v2", ObjectID: "obj-1", ObjectType: "claim", VersionNo: 2,
			StateID: "state-proposed", Title: "proposed claim", LifecycleState: "active",
			Payload: []byte(`{"statement":"proposed"}`),
		}}},
		"state-target-head": {ObjectVersions: []manifest.ObjectVersion{{
			ID: "v3", ObjectID: "obj-1", ObjectType: "claim", VersionNo: 2,
			StateID: "state-target-head", Title: "target claim", LifecycleState: "active",
			Payload: []byte(`{"statement":"target"}`),
		}}},
	}
	engine := &engineStub{snapshots: snapshots}
	svc := NewService(&fakePRs{pr: pr}, &fakeHeads{state: domain.ProjectState{ID: "state-target-head"}}, engine)

	d, err := svc.PullRequestDiff(context.Background(), "proj-1", 7)
	if err != nil {
		t.Fatalf("PullRequestDiff: %v", err)
	}
	if d.Base.ID != "state-base" || d.Source.ID != "state-proposed" || d.Target.ID != "state-target-head" {
		t.Fatalf("diff pins = %s/%s/%s", d.Base.ID, d.Source.ID, d.Target.ID)
	}
	if len(d.ObjectChanges) != 1 {
		t.Fatalf("object changes = %d, want 1", len(d.ObjectChanges))
	}
	change := d.ObjectChanges[0]
	if change.Kind != diff.ChangeUpdated || !change.TargetMoved {
		t.Fatalf("change = %+v, want an updated change with target_moved", change)
	}
}

// engineStub is a different (three-state) stub with a fixed snapshot
// table: it runs the REAL engine, so the assertion above is over real
// engine output rather than a hand-built document.
type engineStub struct {
	snapshots map[string]manifest.Snapshot
}

func (e *engineStub) Diff(_ context.Context, in diffs.Params) (*diff.Diff, error) {
	return diff.Compute(diff.Inputs{
		ProjectID:      in.ProjectID,
		Base:           diff.StateRef{ID: in.BaseStateID},
		Source:         diff.StateRef{ID: in.SourceStateID},
		Target:         diff.StateRef{ID: in.TargetStateID},
		BaseSnapshot:   e.snapshots[in.BaseStateID],
		SourceSnapshot: e.snapshots[in.SourceStateID],
		TargetSnapshot: e.snapshots[in.TargetStateID],
	})
}
