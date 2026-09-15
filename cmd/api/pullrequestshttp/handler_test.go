package pullrequestshttp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/prchecks"
	"github.com/lichman0405/post/internal/application/prdiff"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/diff"
	"github.com/lichman0405/post/internal/rsg/integrity"
	"github.com/lichman0405/post/internal/rsg/manifest"
)

// fakePRs serves canned list/get outcomes.
type fakePRs struct {
	list  []domain.PullRequest
	pr    domain.PullRequest
	err   error
	gotID string
	gotN  int64
}

func (f *fakePRs) List(_ context.Context, projectID string) ([]domain.PullRequest, error) {
	f.gotID = projectID
	return f.list, f.err
}

func (f *fakePRs) Get(_ context.Context, projectID string, number int64) (domain.PullRequest, error) {
	f.gotID, f.gotN = projectID, number
	return f.pr, f.err
}

// fakeChecks serves canned report outcomes.
type fakeChecks struct {
	report integrity.Report
	err    error
	gotID  string
	gotN   int64
}

func (f *fakeChecks) CheckPullRequest(_ context.Context, projectID string, number int64) (integrity.Report, error) {
	f.gotID, f.gotN = projectID, number
	return f.report, f.err
}

// fakeDiff serves a canned diff document.
type fakeDiff struct {
	document *diff.Diff
	err      error
	gotID    string
	gotN     int64
}

func (f *fakeDiff) PullRequestDiff(_ context.Context, projectID string, number int64) (*diff.Diff, error) {
	f.gotID, f.gotN = projectID, number
	return f.document, f.err
}

// fakeProjectGate is the ProjectReader the handler runs before anything.
type fakeProjectGate struct {
	err error
}

func (f *fakeProjectGate) Get(_ context.Context, _ projects.Reader, projectID string) (domain.Project, error) {
	if f.err != nil {
		return domain.Project{}, f.err
	}
	return domain.Project{ID: projectID}, nil
}

func newTestServer(prs PullRequests, checks CheckRunner, gate *fakeProjectGate) *httptest.Server {
	mux := http.NewServeMux()
	New(Deps{PullRequests: prs, Checks: checks, Projects: gate}).Register(mux)
	return httptest.NewServer(mux)
}

// newDiffServer wires the diff endpoint (the other routes stay unset: the
// diff service is the only dependency this surface needs).
func newDiffServer(diffRunner DiffRunner, gate ProjectReader) *httptest.Server {
	mux := http.NewServeMux()
	New(Deps{Diff: diffRunner, Projects: gate}).Register(mux)
	return httptest.NewServer(mux)
}

func testPR() domain.PullRequest {
	return domain.PullRequest{
		ID: "pr-1", ProjectID: "proj-1", Number: 7,
		SourceBranchID: "branch-src", TargetBranchID: "branch-main",
		BaseStateID: "state-m2", ProposedStateID: "state-s2",
		Title: "proposal", Body: "body", State: domain.PullRequestStateReviewRequired,
		CreatedBy: "u-alice", CreatedAt: time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC),
	}
}

func get(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// errorEnvelope is the wire failure shape (cmd/api/authhttp): the handler
// tests assert the code and the client-safe message it carries.
type errorEnvelope struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return out
}

func TestHandleList(t *testing.T) {
	prs := &fakePRs{list: []domain.PullRequest{testPR()}}
	ts := newTestServer(prs, &fakeChecks{}, &fakeProjectGate{})
	defer ts.Close()

	resp := get(t, ts.URL+"/api/v1/projects/proj-1/pull-requests")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := decode[[]prPayload](t, resp)
	if len(got) != 1 || got[0].Number != 7 || got[0].State != string(domain.PullRequestStateReviewRequired) {
		t.Fatalf("payload = %+v", got)
	}
	if got[0].BaseStateID != "state-m2" || got[0].ProposedStateID != "state-s2" {
		t.Fatalf("state pins = %s→%s", got[0].BaseStateID, got[0].ProposedStateID)
	}
	if prs.gotID != "proj-1" {
		t.Fatalf("service called with project %q", prs.gotID)
	}
}

func TestHandleGet(t *testing.T) {
	prs := &fakePRs{pr: testPR()}
	ts := newTestServer(prs, &fakeChecks{}, &fakeProjectGate{})
	defer ts.Close()

	resp := get(t, ts.URL+"/api/v1/projects/proj-1/pull-requests/7")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := decode[prPayload](t, resp)
	if got.Number != 7 || got.Title != "proposal" {
		t.Fatalf("payload = %+v", got)
	}
	if prs.gotN != 7 {
		t.Fatalf("service called with number %d", prs.gotN)
	}
}

func TestHandleGetNotFound(t *testing.T) {
	prs := &fakePRs{err: pullrequests.ErrPullRequestNotFound}
	ts := newTestServer(prs, &fakeChecks{}, &fakeProjectGate{})
	defer ts.Close()

	resp := get(t, ts.URL+"/api/v1/projects/proj-1/pull-requests/999")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHandleGetNonNumericNumberIs404(t *testing.T) {
	prs := &fakePRs{}
	ts := newTestServer(prs, &fakeChecks{}, &fakeProjectGate{})
	defer ts.Close()

	resp := get(t, ts.URL+"/api/v1/projects/proj-1/pull-requests/checks")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (a non-numeric segment names nothing)", resp.StatusCode)
	}
	if prs.gotN != 0 {
		t.Fatalf("service called with number %d, want none", prs.gotN)
	}
}

func TestHandleChecks(t *testing.T) {
	report := integrity.Report{
		Kind: "integrity", ProjectID: "proj-1", PRNumber: 7,
		BaseStateID: "state-m2", ProposedStateID: "state-s2",
		Verdict: integrity.VerdictBlocked,
		Results: []integrity.Result{{
			Dimension: integrity.DimensionProvenance, Check: integrity.CheckBaseOnTargetChain,
			Severity: integrity.SeverityBlocking, Passed: false,
			Detail: "base state state-elsewhere is not on the target branch's chain",
		}},
		Explanation: "integrity check for PR #7: blocked",
		ComputedAt:  "2026-09-15T09:00:00Z",
	}
	checks := &fakeChecks{report: report}
	ts := newTestServer(&fakePRs{}, checks, &fakeProjectGate{})
	defer ts.Close()

	resp := get(t, ts.URL+"/api/v1/projects/proj-1/pull-requests/7/checks")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := decode[integrity.Report](t, resp)
	if got.Verdict != integrity.VerdictBlocked || len(got.Results) != 1 {
		t.Fatalf("report = %+v", got)
	}
	if checks.gotID != "proj-1" || checks.gotN != 7 {
		t.Fatalf("checks called with %q/#%d", checks.gotID, checks.gotN)
	}
}

func TestHandleChecksMapsErrors(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		checks := &fakeChecks{err: prchecks.ErrPullRequestNotFound}
		ts := newTestServer(&fakePRs{}, checks, &fakeProjectGate{})
		defer ts.Close()
		resp := get(t, ts.URL+"/api/v1/projects/proj-1/pull-requests/7/checks")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})
	t.Run("validation", func(t *testing.T) {
		// The service's validation error as it really arrives at the
		// handler: wrapped, with the package prefix in the text. The wire
		// carries the stable code and the fixed user-facing line — never
		// the dependency text (docs/45).
		checks := &fakeChecks{err: fmt.Errorf("%w: project_id is required", prchecks.ErrValidation)}
		ts := newTestServer(&fakePRs{}, checks, &fakeProjectGate{})
		defer ts.Close()
		resp := get(t, ts.URL+"/api/v1/projects/proj-1/pull-requests/7/checks")
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
		got := decode[errorEnvelope](t, resp)
		if got.Code != pullrequests.CodeValidation {
			t.Fatalf("code = %q, want %q", got.Code, pullrequests.CodeValidation)
		}
		if strings.Contains(got.Message, "prchecks") {
			t.Fatalf("message leaks the service error: %q", got.Message)
		}
		if got.Message != "invalid project or pull request number" {
			t.Fatalf("message = %q, want the fixed user-facing line", got.Message)
		}
	})
	t.Run("store failure", func(t *testing.T) {
		checks := &fakeChecks{err: prchecks.ErrStore}
		ts := newTestServer(&fakePRs{}, checks, &fakeProjectGate{})
		defer ts.Close()
		resp := get(t, ts.URL+"/api/v1/projects/proj-1/pull-requests/7/checks")
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", resp.StatusCode)
		}
	})
}

func TestProjectGate(t *testing.T) {
	t.Run("denied read hides the project", func(t *testing.T) {
		prs := &fakePRs{}
		ts := newTestServer(prs, &fakeChecks{}, &fakeProjectGate{err: projects.ErrProjectNotFound})
		defer ts.Close()
		resp := get(t, ts.URL+"/api/v1/projects/secret/pull-requests")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
		if prs.gotID != "" {
			t.Fatalf("service reached before the gate; project %q", prs.gotID)
		}
	})
	t.Run("missing project gate fails closed", func(t *testing.T) {
		mux := http.NewServeMux()
		New(Deps{PullRequests: &fakePRs{}, Checks: &fakeChecks{}}).Register(mux)
		ts := httptest.NewServer(mux)
		defer ts.Close()
		resp := get(t, ts.URL+"/api/v1/projects/proj-1/pull-requests")
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", resp.StatusCode)
		}
	})
}

// testDiff builds a small but real diff document through the engine, so
// the handler test asserts over engine output rather than a hand-rolled
// map.
func testDiff(t *testing.T) *diff.Diff {
	t.Helper()
	base := []byte(`{"statement":"base"}`)
	source := []byte(`{"statement":"proposed"}`)
	gitBase, gitSource := "c0ffee01", "c0ffee02"
	document, err := diff.Compute(diff.Inputs{
		ProjectID: "proj-1",
		Base:      diff.StateRef{ID: "state-m2", GitRef: &gitBase},
		Source:    diff.StateRef{ID: "state-s2", GitRef: &gitSource},
		Target:    diff.StateRef{ID: "state-m3"},
		BaseSnapshot: manifest.Snapshot{ObjectVersions: []manifest.ObjectVersion{{
			ID: "v1", ObjectID: "obj-1", ObjectType: "claim", VersionNo: 1,
			StateID: "state-m2", Title: "base claim", LifecycleState: "active", Payload: base,
		}}},
		SourceSnapshot: manifest.Snapshot{ObjectVersions: []manifest.ObjectVersion{{
			ID: "v2", ObjectID: "obj-1", ObjectType: "claim", VersionNo: 2,
			StateID: "state-s2", Title: "proposed claim", LifecycleState: "active", Payload: source,
		}}},
		TargetSnapshot: manifest.Snapshot{},
	})
	if err != nil {
		t.Fatalf("diff.Compute: %v", err)
	}
	return document
}

func TestHandleDiff(t *testing.T) {
	document := testDiff(t)
	diffs := &fakeDiff{document: document}
	ts := newDiffServer(diffs, &fakeProjectGate{})
	defer ts.Close()

	resp := get(t, ts.URL+"/api/v1/projects/proj-1/pull-requests/7/diff")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	// The wire document IS the engine's canonical serialization — same
	// bytes, so the field order and the payload encoding are the
	// engine's, not the handler's.
	want, err := document.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if strings.TrimSpace(string(raw)) != strings.TrimSpace(string(want)) {
		t.Fatalf("body is not the canonical document:\n got %s\nwant %s", raw, want)
	}
	var got diff.Diff
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(got.ObjectChanges) != 1 || got.ObjectChanges[0].Kind != diff.ChangeUpdated {
		t.Fatalf("document = %+v", got)
	}
	// The engine emits one entry per side that has any git material at
	// all, source first; here the base carries a ref and the target state
	// does not, so both sides appear and the target's head ref is empty
	// (the empty-tree convention the engine documents).
	if len(got.FileDiffRefs) != 2 {
		t.Fatalf("file diff refs = %+v, want one entry per side", got.FileDiffRefs)
	}
	if got.FileDiffRefs[0].Kind != "source" || got.FileDiffRefs[0].HeadGitRef != "c0ffee02" {
		t.Fatalf("source refs = %+v, want base c0ffee01 → head c0ffee02", got.FileDiffRefs[0])
	}
	if got.FileDiffRefs[1].Kind != "target" || got.FileDiffRefs[1].HeadGitRef != "" {
		t.Fatalf("target refs = %+v, want the base ref with an empty head", got.FileDiffRefs[1])
	}
	if diffs.gotID != "proj-1" || diffs.gotN != 7 {
		t.Fatalf("diff service called with %q/#%d", diffs.gotID, diffs.gotN)
	}
}

func TestHandleDiffMapsErrors(t *testing.T) {
	t.Run("unknown pull request", func(t *testing.T) {
		ts := newDiffServer(&fakeDiff{err: prdiff.ErrPullRequestNotFound}, &fakeProjectGate{})
		defer ts.Close()
		resp := get(t, ts.URL+"/api/v1/projects/proj-1/pull-requests/999/diff")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
		got := decode[errorEnvelope](t, resp)
		if got.Code != pullrequests.CodePullRequestNotFound {
			t.Fatalf("code = %q, want %q", got.Code, pullrequests.CodePullRequestNotFound)
		}
	})
	t.Run("a compared state is missing", func(t *testing.T) {
		ts := newDiffServer(&fakeDiff{err: fmt.Errorf("%w: state gone", prdiff.ErrStateNotFound)}, &fakeProjectGate{})
		defer ts.Close()
		resp := get(t, ts.URL+"/api/v1/projects/proj-1/pull-requests/7/diff")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
		got := decode[errorEnvelope](t, resp)
		if got.Code != prdiff.CodeStateNotFound {
			t.Fatalf("code = %q, want %q", got.Code, prdiff.CodeStateNotFound)
		}
	})
	t.Run("validation never leaks the service error", func(t *testing.T) {
		ts := newDiffServer(&fakeDiff{err: fmt.Errorf("%w: project_id is required", prdiff.ErrValidation)}, &fakeProjectGate{})
		defer ts.Close()
		resp := get(t, ts.URL+"/api/v1/projects/proj-1/pull-requests/7/diff")
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
		got := decode[errorEnvelope](t, resp)
		if got.Code != pullrequests.CodeValidation {
			t.Fatalf("code = %q, want %q", got.Code, pullrequests.CodeValidation)
		}
		if strings.Contains(got.Message, "prdiff") {
			t.Fatalf("message leaks the service error: %q", got.Message)
		}
	})
	t.Run("store failure", func(t *testing.T) {
		ts := newDiffServer(&fakeDiff{err: prdiff.ErrStore}, &fakeProjectGate{})
		defer ts.Close()
		resp := get(t, ts.URL+"/api/v1/projects/proj-1/pull-requests/7/diff")
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", resp.StatusCode)
		}
	})
	t.Run("missing diff wiring fails closed", func(t *testing.T) {
		ts := newDiffServer(nil, &fakeProjectGate{})
		defer ts.Close()
		resp := get(t, ts.URL+"/api/v1/projects/proj-1/pull-requests/7/diff")
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", resp.StatusCode)
		}
	})
	t.Run("non-numeric number is 404 and reaches no service", func(t *testing.T) {
		diffs := &fakeDiff{}
		ts := newDiffServer(diffs, &fakeProjectGate{})
		defer ts.Close()
		resp := get(t, ts.URL+"/api/v1/projects/proj-1/pull-requests/all/diff")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
		if diffs.gotN != 0 || diffs.gotID != "" {
			t.Fatalf("diff service reached with %q/#%d", diffs.gotID, diffs.gotN)
		}
	})
}

func TestHandleDiffRunsTheProjectGateFirst(t *testing.T) {
	diffs := &fakeDiff{document: testDiff(t)}
	ts := newDiffServer(diffs, &fakeProjectGate{err: projects.ErrProjectNotFound})
	defer ts.Close()

	resp := get(t, ts.URL+"/api/v1/projects/secret/pull-requests/7/diff")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if diffs.gotID != "" {
		t.Fatalf("diff service reached before the gate; project %q", diffs.gotID)
	}
}

func TestHandleDiffWithoutGateFailsClosed(t *testing.T) {
	mux := http.NewServeMux()
	New(Deps{Diff: &fakeDiff{document: testDiff(t)}}).Register(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp := get(t, ts.URL+"/api/v1/projects/proj-1/pull-requests/7/diff")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}
