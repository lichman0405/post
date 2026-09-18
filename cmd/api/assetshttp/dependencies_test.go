package assetshttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/assets"
)

// Task T0707 required test "asset dependency tests" — the transport half
// (the rules are internal/assets': project_dependency_test.go there, and the
// recording rule is usage_test.go).
//
// What only the composed route can be asked:
//
//   - GET /api/v1/projects/{projectId}/dependencies is reachable by a member,
//     by a non-member of a public project, and anonymously — and the
//     membership bit is what decides whether the project's private
//     declarations are rendered.
//   - a project this caller may not read answers the project surface's
//     existence-hiding 404 BEFORE any row is read, so the route cannot be
//     used to learn that a private project exists.
//   - a failed read is answered unavailable (503), never as an empty list:
//     "this project depends on nothing" is a claim, and a read that did not
//     finish has no claim to make.
//   - the route is not in the contract (specs/api/openapi.yaml), which is
//     why it is mounted on the v1 mux the way provenancehttp mounts its own
//     projection walk — asserted against the file itself, with a control
//     proving the file was read.
//
// The fakes are the three ports this surface asks for this route: the
// dependency reader, the project gate and the membership read. The rest of
// the composition is the production one (newAssetsServer).

// ---- fakes -----------------------------------------------------------------

// fakeDependencies is the dependency reader: canned rows (or failure), and
// it records whether it was asked at all.
type fakeDependencies struct {
	rows  []assets.ProjectDependencyState
	err   error
	gotID string
	calls int
}

func (f *fakeDependencies) ListProjectDependencies(_ context.Context, projectID string) ([]assets.ProjectDependencyState, error) {
	f.calls++
	f.gotID = projectID
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

// ---- fixtures --------------------------------------------------------------

const (
	// depTestProject is the project the fixture's rows belong to.
	depTestProject = "77777777-7777-4777-8777-777777777777"
	// depTestUsedPID is the pid of the used asset; depTestSecretName is the
	// title of a version the network cannot open, and depTestPrivateNote a
	// declaration that is private.
	depTestUsedPID    = assets.PID("01j9z6k3m4n5p6q7r8s9t0v1w9")
	depTestSecretName = "Secret upstream benchmark"
)

// depTestRows is the fixture: one public declaration, one private one, and
// one row pointing at a version the network cannot open — so the transport
// test can see both what the model renders and what it must not.
func depTestRows(t *testing.T) []assets.ProjectDependencyState {
	t.Helper()
	pin, ok := assets.NewDependencyPin(depTestUsedPID, "1.0")
	if !ok {
		t.Fatal("NewDependencyPin refused a valid pair")
	}
	hiddenPin, ok := assets.NewDependencyPin(depTestUsedPID, "2.0")
	if !ok {
		t.Fatal("NewDependencyPin refused a valid pair")
	}
	base := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)
	return []assets.ProjectDependencyState{
		{
			Pin: string(pin), Title: "Open upstream dataset", Type: assets.TypeDataset,
			VersionVisibility: assets.VisibilityPublic, VersionProjectVisibility: assets.VisibilityPublic,
			DependencyType:    string(assets.DependencyTypeDependsOn),
			VisibilityOfUsage: assets.VisibilityPublic, CreatedAt: base,
		},
		{
			Pin: string(pin), Title: "Open upstream dataset", Type: assets.TypeDataset,
			VersionVisibility: assets.VisibilityPublic, VersionProjectVisibility: assets.VisibilityPublic,
			DependencyType:    string(assets.DependencyTypeReferences),
			VisibilityOfUsage: assets.VisibilityPrivate, CreatedAt: base.Add(time.Minute),
		},
		{
			Pin: string(hiddenPin), Title: depTestSecretName, Type: assets.TypeDataset,
			VersionVisibility: assets.VisibilityPrivate, VersionProjectVisibility: assets.VisibilityPublic,
			DependencyType:    string(assets.DependencyTypeDependsOn),
			VisibilityOfUsage: assets.VisibilityPublic, CreatedAt: base.Add(2 * time.Minute),
		},
	}
}

// ---- server ----------------------------------------------------------------

// dependencyServer is the composed surface with the dependency reader wired.
type dependencyServer struct {
	*previewServer
	anon    *http.Client
	deps    *fakeDependencies
	members *fakeMembers
	gate    *fakeGate
}

// newDependencyServer composes the surface with the real guard over a mux
// carrying this package's routes. projectErr is what the project read gate
// answers; memberOf names the projects the signed-in caller is a member of.
func newDependencyServer(t *testing.T, projectErr, memberErr error, memberOf ...string) *dependencyServer {
	t.Helper()
	deps := &fakeDependencies{rows: depTestRows(t)}
	members := &fakeMembers{memberOf: map[string]bool{}, err: memberErr}
	for _, id := range memberOf {
		members.memberOf[id] = true
	}
	gate := &fakeGate{err: projectErr}
	inner := newAssetsServer(t, Deps{State: &fakeState{}, Projects: gate, Members: members, Dependencies: deps})
	jar, _ := cookiejar.New(nil)
	return &dependencyServer{
		previewServer: inner,
		deps:          deps,
		members:       members,
		gate:          gate,
		anon: &http.Client{
			Jar:           jar,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// dependenciesURL is the route under test.
func dependenciesURL(projectID string) string {
	return "/api/v1/projects/" + projectID + "/dependencies"
}

// get performs one GET as the given client and returns the response.
func (s *dependencyServer) get(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()
	resp, err := client.Get(s.ts.URL + url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

// ---- the route -------------------------------------------------------------

// TestProjectDependenciesRouteServesAMemberTheWholeList: a member of the
// project reads the project's own declarations, private ones included, and
// the transport filters nothing of what the model decided.
func TestProjectDependenciesRouteServesAMemberTheWholeList(t *testing.T) {
	server := newDependencyServer(t, nil, nil, depTestProject)
	resp := server.get(t, server.client, dependenciesURL(depTestProject))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200: %s", dependenciesURL(depTestProject), resp.StatusCode, rawBody(t, resp))
	}
	body := rawBody(t, resp)
	var answer struct {
		Dependencies []struct {
			Pin               string `json:"pin"`
			URL               string `json:"url"`
			DependencyType    string `json:"dependency_type"`
			ImpactAnalysis    bool   `json:"impact_analysis"`
			VisibilityOfUsage string `json:"visibility_of_usage"`
			Title             string `json:"title"`
		} `json:"dependencies"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("decode answer: %v: %s", err, body)
	}
	// Two of the three fixture rows survive: the private declaration is the
	// member's to see, the unopenable version never is.
	if len(answer.Dependencies) != 2 {
		t.Fatalf("a member saw %d dependencies, want 2 (the unopenable row is dropped for everyone): %s", len(answer.Dependencies), body)
	}
	if answer.Dependencies[0].VisibilityOfUsage != string(assets.VisibilityPublic) ||
		answer.Dependencies[1].VisibilityOfUsage != string(assets.VisibilityPrivate) {
		t.Errorf("a member's list = %+v, want the public declaration then the private one", answer.Dependencies)
	}
	if !answer.Dependencies[0].ImpactAnalysis || answer.Dependencies[1].ImpactAnalysis {
		t.Errorf("impact_analysis does not follow the stored type: %+v", answer.Dependencies)
	}
	if want := "/assets/" + string(depTestUsedPID) + "/1.0"; answer.Dependencies[0].URL != want {
		t.Errorf("url = %q, want %q", answer.Dependencies[0].URL, want)
	}
	if strings.Contains(body, depTestSecretName) {
		t.Errorf("the member's answer names a version the network cannot open: %s", body)
	}
}

// TestProjectDependenciesRouteHidesPrivateDeclarationsFromANonMember: the
// same project, read by someone who may read it but is not inside it — the
// public declarations and nothing else, with no trace of the private one.
func TestProjectDependenciesRouteHidesPrivateDeclarationsFromANonMember(t *testing.T) {
	server := newDependencyServer(t, nil, nil) // signed in, member of nothing
	resp := server.get(t, server.client, dependenciesURL(depTestProject))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200: %s", dependenciesURL(depTestProject), resp.StatusCode, rawBody(t, resp))
	}
	body := rawBody(t, resp)
	if strings.Contains(body, string(assets.VisibilityPrivate)) {
		t.Errorf("a non-member's answer mentions a private declaration: %s", body)
	}
	var answer struct {
		Dependencies []struct {
			DependencyType    string `json:"dependency_type"`
			VisibilityOfUsage string `json:"visibility_of_usage"`
		} `json:"dependencies"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("decode answer: %v: %s", err, body)
	}
	if len(answer.Dependencies) != 1 {
		t.Fatalf("a non-member saw %d dependencies, want the one public declaration: %s", len(answer.Dependencies), body)
	}
	if answer.Dependencies[0].DependencyType != string(assets.DependencyTypeDependsOn) {
		t.Errorf("the rendered declaration is %q, want the public depends_on row", answer.Dependencies[0].DependencyType)
	}

	// The anonymous caller is a non-member too, and is answered the same
	// (a public project is readable by anyone, docs/12 §2).
	anon := newDependencyServer(t, nil, nil)
	anonResp := anon.get(t, anon.anon, dependenciesURL(depTestProject))
	if anonResp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous GET %s = %d, want 200", dependenciesURL(depTestProject), anonResp.StatusCode)
	}
	anonBody := rawBody(t, anonResp)
	if strings.Contains(anonBody, string(assets.VisibilityPrivate)) || strings.Contains(anonBody, depTestSecretName) {
		t.Errorf("the anonymous answer leaks: %s", anonBody)
	}
	// The anonymous caller's membership was not consulted: there is no user
	// to read one for (page.go's viewer).
	if anon.members.calls != 0 {
		t.Errorf("the anonymous read resolved a membership %d times, want 0", anon.members.calls)
	}
}

// TestProjectDependenciesRouteRefusesAProjectTheCallerMayNotRead: the
// project read gate runs FIRST and relays the project surface's own
// refusals — a hidden project is the same 404 with the same code as a
// nonexistent one (projects: hidden and missing are one answer), and a
// caller the project surface would refuse is refused the same way. A
// refused read touches no row, so the route cannot be an oracle for what a
// project depends on.
func TestProjectDependenciesRouteRefusesAProjectTheCallerMayNotRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"hidden or missing", projects.ErrProjectNotFound, http.StatusNotFound, projects.CodeProjectNotFound},
		{"forbidden", projects.ErrForbidden, http.StatusForbidden, projects.CodeProjectForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newDependencyServer(t, tc.err, nil, depTestProject)
			resp := server.get(t, server.client, dependenciesURL(depTestProject))
			if resp.StatusCode != tc.status {
				t.Fatalf("GET with gate error %v = %d, want %d: %s", tc.err, resp.StatusCode, tc.status, rawBody(t, resp))
			}
			body := rawBody(t, resp)
			if !strings.Contains(body, tc.code) {
				t.Errorf("the refusal's code is not the project surface's own (%s): %s", tc.code, body)
			}
			// Withheld, not merely unrendered: nothing was read.
			if server.deps.calls != 0 {
				t.Errorf("the dependency reader was asked %d times for a project the caller may not read", server.deps.calls)
			}
			if server.members.calls != 0 {
				t.Errorf("the membership read was consulted %d times for a project the caller may not read", server.members.calls)
			}
		})
	}
}

// TestProjectDependenciesRouteAnswersUnavailableRatherThanEmpty: a read
// that failed must not be rendered as "this project depends on nothing", and
// neither must a membership read that failed be downgraded to "not a member"
// (which would show a member the network's view of their own project).
func TestProjectDependenciesRouteAnswersUnavailableRatherThanEmpty(t *testing.T) {
	readFailure := newDependencyServer(t, nil, nil, depTestProject)
	readFailure.deps.err = errors.New("connection reset by peer")
	resp := readFailure.get(t, readFailure.client, dependenciesURL(depTestProject))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET with a failed read = %d, want 503: %s", resp.StatusCode, rawBody(t, resp))
	}
	body := rawBody(t, resp)
	if !strings.Contains(body, CodeAssetDependenciesUnavailable) {
		t.Errorf("the failure's code is not %s: %s", CodeAssetDependenciesUnavailable, body)
	}
	if strings.Contains(body, `"dependencies"`) {
		t.Errorf("a failed read answered a list: %s", body)
	}

	memberFailure := newDependencyServer(t, nil, errors.New("membership store down"), depTestProject)
	resp = memberFailure.get(t, memberFailure.client, dependenciesURL(depTestProject))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET with a failed membership read = %d, want 503: %s", resp.StatusCode, rawBody(t, resp))
	}
	if memberFailure.deps.calls != 0 {
		t.Errorf("the dependency reader was consulted %d times after the membership read failed", memberFailure.deps.calls)
	}
}

// TestProjectDependenciesRouteIsNotInTheContract: the route is mounted on
// the v1 mux without a contract path, the way provenancehttp mounts its
// projection walk — because the contract has no path for a project's
// dependency list and adding one is a product decision, not an
// implementation's.
//
// The assertion is against the file itself, and it carries its own control:
// the same read must find the paths the contract DOES declare, so a spec
// that failed to open (or an empty read) cannot make this test pass by
// finding nothing.
func TestProjectDependenciesRouteIsNotInTheContract(t *testing.T) {
	path := filepath.Join("..", "..", "..", "specs", "api", "openapi.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the contract at %s: %v", path, err)
	}
	spec := string(raw)
	for _, control := range []string{
		"/assets/{assetId}",
		"/projects/{projectId}/assets:publish",
	} {
		if !strings.Contains(spec, control) {
			t.Fatalf("the contract does not declare %q, so this read is not reading the contract: %d bytes", control, len(spec))
		}
	}
	if strings.Contains(spec, "/dependencies") {
		t.Error("the contract now declares a dependencies path; the route's mount comment in dependencies.go is out of date")
	}
}
