package prchecks

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/lichman0405/post/internal/application/dependencyimpact"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/manifest"
)

// TestChangedObjects: the proposal's changed objects, derived from the two
// manifests the PR pins.
//
// The three facts the test fixes are the three ways a manifest pair can
// differ and mean "this object changed": a version the proposal adds (a new
// object, or a new version of an existing one), the same version re-recorded
// at a different lifecycle position (an abort that does not re-version), and
// two versions of one object in one proposal (one changed object, asked
// about once — the walk is over objects).
func TestChangedObjects(t *testing.T) {
	const (
		proto     = "11111111-1111-4111-8111-111111111111"
		exper     = "22222222-2222-4222-8222-222222222222"
		dataset   = "33333333-3333-4333-8333-333333333333"
		untouched = "44444444-4444-4444-8444-444444444444"
	)
	base := manifest.Snapshot{ObjectVersions: []manifest.ObjectVersion{
		{ID: "v-proto-1", ObjectID: proto, ObjectType: "protocol", VersionNo: 1, LifecycleState: "active"},
		{ID: "v-exper-1", ObjectID: exper, ObjectType: "experiment", VersionNo: 1, LifecycleState: "active"},
		{ID: "v-dataset-1", ObjectID: dataset, ObjectType: "dataset", VersionNo: 1, LifecycleState: "active"},
		{ID: "v-untouched-1", ObjectID: untouched, ObjectType: "claim", VersionNo: 1, LifecycleState: "active"},
	}}
	proposed := manifest.Snapshot{ObjectVersions: []manifest.ObjectVersion{
		{ID: "v-proto-1", ObjectID: proto, ObjectType: "protocol", VersionNo: 1, LifecycleState: "active"},
		{ID: "v-proto-2", ObjectID: proto, ObjectType: "protocol", VersionNo: 2, LifecycleState: "active"},
		// The same version the base had, now aborted.
		{ID: "v-exper-1", ObjectID: exper, ObjectType: "experiment", VersionNo: 1, LifecycleState: "aborted"},
		{ID: "v-dataset-1", ObjectID: dataset, ObjectType: "dataset", VersionNo: 1, LifecycleState: "active"},
		{ID: "v-untouched-1", ObjectID: untouched, ObjectType: "claim", VersionNo: 1, LifecycleState: "active"},
	}}

	got := changedObjects(base, proposed)
	// Sorted by object id, so the answer is the same request every time the
	// screen renders.
	want := dependencyimpact.Subjects{
		{Kind: dependencyimpact.SubjectObject, ID: proto, VersionNo: 2},
		{Kind: dependencyimpact.SubjectObject, ID: exper, VersionNo: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changedObjects = %+v, want %+v", got, want)
	}

	// An unchanged proposal changes nothing: the empty subject list is what
	// makes PullRequestImpact answer without running (and without
	// authorizing) anything.
	if got := changedObjects(base, base); len(got) != 0 {
		t.Errorf("changedObjects(base, base) = %+v, want none", got)
	}
	// A version whose container is unset names no object to walk from.
	if got := changedObjects(manifest.Snapshot{}, manifest.Snapshot{
		ObjectVersions: []manifest.ObjectVersion{{ID: "x", ObjectType: "claim", VersionNo: 1}},
	}); len(got) != 0 {
		t.Errorf("changedObjects with no object id = %+v, want none", got)
	}
}

// fakeManifest supplies the PR's two pinned states. The PR reader itself is
// the existing test double (service_test.go's fakePRReader), used through
// newImpactService so the two test files cannot drift into two fakes for one
// port.
type fakeManifest map[string]manifest.Snapshot

func (f fakeManifest) GetManifestSnapshot(_ context.Context, stateID string) (manifest.Snapshot, error) {
	snap, ok := f[stateID]
	if !ok {
		return manifest.Snapshot{}, errors.New("no such state")
	}
	return snap, nil
}

// recordingImpact is the analysis read surface, recorded: the service layer
// must hand it the changed objects and the CALLER's reader, and show the
// caller exactly what it answers.
type recordingImpact struct {
	report   dependencyimpact.Report
	err      error
	gotSubj  dependencyimpact.Subjects
	gotRead  projects.Reader
	calledAt int
}

func (r *recordingImpact) Read(_ context.Context, reader projects.Reader, subjects dependencyimpact.Subjects) (dependencyimpact.Report, error) {
	r.calledAt++
	r.gotRead = reader
	r.gotSubj = subjects
	return r.report, r.err
}

// TestPullRequestImpactAsksAboutTheChangedObjects: the screen's answer is
// the analysis's answer about exactly the objects the proposal changes, with
// the caller's own reader — the composition, not a second walk.
func TestPullRequestImpactAsksAboutTheChangedObjects(t *testing.T) {
	const (
		proto = "11111111-1111-4111-8111-111111111111"
		exper = "22222222-2222-4222-8222-222222222222"
	)
	impact := &recordingImpact{report: dependencyimpact.Report{Groups: []dependencyimpact.SubjectImpacts{
		{Subject: dependencyimpact.Subject{Kind: dependencyimpact.SubjectObject, ID: proto}},
	}}}
	svc := NewService(Deps{
		PRs: &fakePRReader{pr: domain.PullRequest{
			ProjectID: "proj", Number: 7, BaseStateID: "base", ProposedStateID: "proposed",
		}},
		Manifest: fakeManifest{
			"base": manifest.Snapshot{ObjectVersions: []manifest.ObjectVersion{
				{ID: "v1", ObjectID: proto, ObjectType: "protocol", VersionNo: 1, LifecycleState: "active"},
			}},
			"proposed": manifest.Snapshot{ObjectVersions: []manifest.ObjectVersion{
				{ID: "v1", ObjectID: proto, ObjectType: "protocol", VersionNo: 1, LifecycleState: "active"},
				{ID: "v2", ObjectID: proto, ObjectType: "protocol", VersionNo: 2, LifecycleState: "active"},
				{ID: "v3", ObjectID: exper, ObjectType: "experiment", VersionNo: 1, LifecycleState: "active"},
			}},
		},
		Impact: impact,
	})
	reader := projects.Reader{UserID: "u1", Authenticated: true}
	report, err := svc.PullRequestImpact(context.Background(), reader, "proj", 7)
	if err != nil {
		t.Fatalf("PullRequestImpact: %v", err)
	}
	if !reflect.DeepEqual(report, impact.report) {
		t.Errorf("report = %+v, want the analysis's own answer %+v", report, impact.report)
	}
	if impact.calledAt != 1 {
		t.Fatalf("the analysis was called %d times, want once", impact.calledAt)
	}
	if !impact.gotRead.Authenticated || impact.gotRead.UserID != "u1" {
		t.Errorf("the analysis was handed reader %+v, want the caller's", impact.gotRead)
	}
	want := dependencyimpact.Subjects{
		{Kind: dependencyimpact.SubjectObject, ID: proto, VersionNo: 2},
		{Kind: dependencyimpact.SubjectObject, ID: exper, VersionNo: 1},
	}
	if !reflect.DeepEqual(impact.gotSubj, want) {
		t.Errorf("subjects = %+v, want the changed objects %+v", impact.gotSubj, want)
	}
}

// TestPullRequestImpactFailsClosed: no analysis wired, an unknown PR, and a
// subject the caller may not read each produce an error rather than an empty
// report — a screen that renders an empty impact line would be reporting
// "nothing is affected" on the strength of a failure.
func TestPullRequestImpactFailsClosed(t *testing.T) {
	reader := projects.Reader{UserID: "u1", Authenticated: true}
	prs := &fakePRReader{pr: domain.PullRequest{ProjectID: "proj", Number: 7, BaseStateID: "base", ProposedStateID: "proposed"}}
	manifests := fakeManifest{"base": {}, "proposed": {ObjectVersions: []manifest.ObjectVersion{
		{ID: "v1", ObjectID: "obj", ObjectType: "claim", VersionNo: 1, LifecycleState: "active"},
	}}}

	t.Run("no analysis wired", func(t *testing.T) {
		svc := NewService(Deps{PRs: prs, Manifest: manifests})
		if _, err := svc.PullRequestImpact(context.Background(), reader, "proj", 7); err == nil {
			t.Fatal("an unwired analysis answered without an error")
		}
	})
	t.Run("unknown pull request", func(t *testing.T) {
		svc := NewService(Deps{PRs: &fakePRReader{err: pullrequests.ErrPullRequestNotFound}, Manifest: manifests, Impact: &recordingImpact{}})
		if _, err := svc.PullRequestImpact(context.Background(), reader, "proj", 7); !errors.Is(err, ErrPullRequestNotFound) {
			t.Fatalf("err = %v, want ErrPullRequestNotFound", err)
		}
	})
	t.Run("unreadable subject", func(t *testing.T) {
		svc := NewService(Deps{PRs: prs, Manifest: manifests,
			Impact: &recordingImpact{err: dependencyimpact.ErrSubjectNotFound}})
		_, err := svc.PullRequestImpact(context.Background(), reader, "proj", 7)
		if !errors.Is(err, ErrPullRequestNotFound) {
			t.Fatalf("err = %v, want the existence-hiding not-found of this package", err)
		}
		if errors.Is(err, ErrStore) {
			t.Error("a subject the caller may not read is reported as a store failure")
		}
	})
	t.Run("malformed arguments", func(t *testing.T) {
		svc := NewService(Deps{PRs: prs, Manifest: manifests, Impact: &recordingImpact{}})
		if _, err := svc.PullRequestImpact(context.Background(), reader, "", 7); !errors.Is(err, ErrValidation) {
			t.Errorf("empty project = %v, want ErrValidation", err)
		}
		if _, err := svc.PullRequestImpact(context.Background(), reader, "proj", 0); !errors.Is(err, ErrValidation) {
			t.Errorf("PR number 0 = %v, want ErrValidation", err)
		}
	})
}

// TestPullRequestImpactWithNoChangesDoesNotWalk: a proposal that changes
// nothing asks the analysis nothing, and answers an empty report — the
// screen's "nothing downstream is affected", reached without a query.
func TestPullRequestImpactWithNoChangesDoesNotWalk(t *testing.T) {
	impact := &recordingImpact{}
	svc := NewService(Deps{
		PRs:      &fakePRReader{pr: domain.PullRequest{ProjectID: "proj", Number: 7, BaseStateID: "s", ProposedStateID: "s"}},
		Manifest: fakeManifest{"s": {ObjectVersions: []manifest.ObjectVersion{{ID: "v1", ObjectID: "obj", VersionNo: 1, LifecycleState: "active"}}}},
		Impact:   impact,
	})
	report, err := svc.PullRequestImpact(context.Background(), projects.Reader{UserID: "u", Authenticated: true}, "proj", 7)
	if err != nil {
		t.Fatalf("PullRequestImpact: %v", err)
	}
	if len(report.Groups) != 0 {
		t.Errorf("report = %+v, want an empty answer", report)
	}
	if impact.calledAt != 0 {
		t.Errorf("the analysis was called %d times for a proposal that changes nothing", impact.calledAt)
	}
}
