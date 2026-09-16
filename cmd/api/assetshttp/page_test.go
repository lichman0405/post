package assetshttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/domain"
)

// Task T0709 required test "asset page tests" — the transport half (the
// rules are internal/assets': page_test.go and browse_test.go there).
//
// What is pinned here is what only the composed route can be asked:
//
//   - GET /api/v1/assets/{assetId} is reachable ANONYMOUSLY (the contract's
//     `security: []`, specs/api/openapi.yaml), and it answers the model's
//     page and nothing else — no field of the response is the transport's
//     own filtering (the raw body is searched for the private names).
//   - the ONE 404: an unknown pid, an asset whose project this caller may
//     not read, an asset with no version this caller may see, and a version
//     the caller may not see all answer the same code and the same message.
//     A page that told those apart would be an oracle for what exists.
//   - the version query parameter: a version label renders THAT version, a
//     label that is not one is refused 400, and a private one is the same
//     404 as everything else.
//   - GET /api/v1/assets (the browse list, mounted here because the contract
//     has no path for it — see page.go) filters by the closed four-type set
//     and refuses a type outside it with 400 rather than an empty list.
//
// The fakes are the two ports the surface asks: the page reader and the
// membership read. The rest of the composition is the production one: the
// real auth guard over the real mux (newAssetsServer).

// ---- fakes -----------------------------------------------------------------

// fakePages is the page reader: canned state (or failure) per pid, and the
// rows the browse list reads.
type fakePages struct {
	states map[assets.PID]assets.PageState
	err    error
	rows   []assets.BrowseRowState
	gotPID assets.PID
	gotFil *assets.Type
	reads  int
}

func (f *fakePages) LoadAssetPage(_ context.Context, pid assets.PID) (assets.PageState, bool, error) {
	f.reads++
	f.gotPID = pid
	if f.err != nil {
		return assets.PageState{}, false, f.err
	}
	state, ok := f.states[pid]
	return state, ok, nil
}

func (f *fakePages) ListBrowseAssets(_ context.Context, filter *assets.Type) ([]assets.BrowseRowState, error) {
	f.reads++
	f.gotFil = filter
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

// fakeMembers is the membership read: the caller is a member of exactly the
// projects named here and of nothing else (projects.ErrMemberNotFound is
// "no role", not a failure).
type fakeMembers struct {
	memberOf map[string]bool
	err      error
	calls    int
}

func (f *fakeMembers) GetMembership(_ context.Context, _ domain.User, projectID string) (domain.ProjectMembership, error) {
	f.calls++
	if f.err != nil {
		return domain.ProjectMembership{}, f.err
	}
	if !f.memberOf[projectID] {
		return domain.ProjectMembership{}, projects.ErrMemberNotFound
	}
	return domain.ProjectMembership{ProjectID: projectID}, nil
}

// ---- fixtures --------------------------------------------------------------

const (
	pageTestProject     = "11111111-1111-4111-8111-111111111111"
	pageTestOtherPid    = assets.PID("01j9z6k3m4n5p6q7r8s9t0v1w3")
	pageTestSecretPid   = assets.PID("01j9z6k3m4n5p6q7r8s9t0v1w4")
	pageTestSecretLab   = "Secret Zeolite Lab"
	pageTestSecretTitle = "Secret zeolite dataset"
	pageTestOldVersion  = "1.0"
	pageTestNewVersion  = "2.0"
	pageTestHiddenThing = "3.0"
)

// pageTestManifest is a manifest the platform's own parser reads (dataset
// with its required keys), with the given dependency pins.
func pageTestManifest(t *testing.T, pins ...string) json.RawMessage {
	t.Helper()
	rawPins := make([]any, 0, len(pins))
	for _, pin := range pins {
		rawPins = append(rawPins, pin)
	}
	doc, err := json.Marshal(map[string]any{
		"version":         1,
		"asset_type":      string(assets.TypeDataset),
		"dependency_pins": rawPins,
		"metadata": map[string]any{
			"purpose":       "screen MOF water stability",
			"data_type":     "tabular",
			"blob_ids":      []any{"blobs/open-1"},
			"access_level":  "restricted",
			"quality_notes": "curated",
		},
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if _, err := assets.ParseManifest(doc); err != nil {
		t.Fatalf("the fixture manifest is not readable by ParseManifest: %v", err)
	}
	return doc
}

// pageTestVersion builds one stored version row.
func pageTestVersion(t *testing.T, id, label string, visibility assets.Visibility, at time.Time, state *pageTestState, pins ...string) assets.PageVersionState {
	t.Helper()
	return assets.PageVersionState{
		ID:            id,
		Version:       label,
		Visibility:    visibility,
		IntegrityHash: "sha256:" + strings.Repeat("c", 64),
		PublishedBy:   state.userID,
		PublishedAt:   at,
		Manifest:      pageTestManifest(t, pins...),
		RightsJSON:    json.RawMessage(`{"license":{"id":"CC-BY-4.0"},"visibility":{"metadata":"public","data_access":"restricted"}}`),
		OriginRefs:    []string{"project:" + pageTestProject},
	}
}

// pageTestState carries the ids the fixture needs to build rows and pins.
type pageTestState struct{ userID string }

// pageTestPageState is the state the reader answers with: one dataset in
// pageTestProject with a public 1.0, a public 2.0 that pins another asset's
// PRIVATE version, and a private 3.0; one public usage by a private project;
// one public and one private event.
func pageTestPageState(t *testing.T, projectVisibility assets.Visibility) assets.PageState {
	t.Helper()
	state := &pageTestState{userID: "44444444-4444-4444-8444-444444444444"}
	hiddenPin, _ := assets.NewDependencyPin(pageTestSecretPid, "0.9")
	base := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	return assets.PageState{
		Asset: assets.PageAssetState{
			ID:              "55555555-5555-4555-8555-555555555555",
			PID:             sampleAssetPID,
			Type:            assets.TypeDataset,
			Title:           "MOF water-stability screening set",
			Slug:            "mof-water-stability",
			OriginProjectID: pageTestProject,
			CreatedAt:       base,
		},
		Project: assets.PageProjectState{
			ID:         pageTestProject,
			Name:       "Open MOF Lab",
			Slug:       "open-lab",
			Visibility: projectVisibility,
		},
		Versions: []assets.PageVersionState{
			pageTestVersion(t, "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa", pageTestOldVersion, assets.VisibilityPublic, base, state),
			pageTestVersion(t, "aaaaaaaa-2222-4222-8222-aaaaaaaaaaaa", pageTestNewVersion, assets.VisibilityPublic, base.Add(time.Minute), state, string(hiddenPin)),
			pageTestVersion(t, "aaaaaaaa-3333-4333-8333-aaaaaaaaaaaa", pageTestHiddenThing, assets.VisibilityPrivate, base.Add(2*time.Minute), state),
		},
		Users: map[string]assets.PageUser{
			state.userID: {UserID: state.userID, Handle: "privacy-alice", DisplayName: "Alice"},
		},
		Pins: map[assets.DependencyPin]assets.PagePinState{
			hiddenPin: {
				Pin: hiddenPin, Resolved: true,
				Visibility: assets.VisibilityPrivate, ProjectVisibility: assets.VisibilityPrivate,
				Title: pageTestSecretTitle, Type: assets.TypeDataset, PID: pageTestSecretPid, Version: "0.9",
			},
		},
		Refs: map[assets.OriginRef]assets.StoredRef{
			assets.OriginRef("project:" + pageTestProject): {
				Ref: assets.OriginRef("project:" + pageTestProject), Resolved: true,
				ProjectID: pageTestProject, ProjectVisibility: projectVisibility,
			},
		},
		Usages: []assets.PageUsageState{
			{
				AssetVersionID: "aaaaaaaa-2222-4222-8222-aaaaaaaaaaaa",
				ProjectID:      "66666666-6666-4666-8666-666666666666",
				ProjectName:    pageTestSecretLab, ProjectSlug: "secret-lab",
				ProjectVisibility: assets.VisibilityPrivate,
				VisibilityOfUsage: assets.VisibilityPublic,
				DependencyType:    "reuses", CreatedAt: base,
			},
		},
		Events: []assets.PageEventState{
			{Type: "research_asset.version_published", Visibility: assets.VisibilityPublic, ActorID: state.userID, AssetVersion: pageTestNewVersion, OccurredAt: base.Add(time.Minute)},
			{Type: "research_asset.version_published", Visibility: assets.VisibilityPrivate, ActorID: state.userID, AssetVersion: pageTestHiddenThing, OccurredAt: base.Add(2 * time.Minute)},
		},
	}
}

// pageTestBrowseRows is the browse fixture: one asset of each type, with the
// second row's project private.
func pageTestBrowseRows() []assets.BrowseRowState {
	base := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	row := func(pid assets.PID, ty assets.Type, title string, visibility assets.Visibility, at time.Time) assets.BrowseRowState {
		return assets.BrowseRowState{
			PID: pid, Type: ty, Title: title, Slug: strings.ReplaceAll(strings.ToLower(title), " ", "-"),
			OriginProjectID: pageTestProject, OriginProjectName: "Open MOF Lab",
			OriginProjectSlug: "open-lab", OriginProjectVisibility: visibility,
			PublicVersions: 1, LatestVersion: "1.0", LatestPublishedAt: at,
		}
	}
	return []assets.BrowseRowState{
		row(sampleAssetPID, assets.TypeDataset, "MOF water-stability screening set", assets.VisibilityPublic, base),
		row(pageTestOtherPid, assets.TypeBenchmark, "A public benchmark", assets.VisibilityPrivate, base.Add(time.Minute)),
		row("01j9z6k3m4n5p6q7r8s9t0v1w6", assets.TypeProtocol, "Synthesis protocol", assets.VisibilityPublic, base.Add(2*time.Minute)),
	}
}

// sampleAssetPID is the pid the page fixture is stored under (a pid the
// assets package's own parser accepts: cuid2 shape, internal/assets/url.go).
const sampleAssetPID = assets.PID("01j9z6k3m4n5p6q7r8s9t0v1w2")

// ---- server ----------------------------------------------------------------

// pageServer is the composed read surface with its two fakes. It embeds
// the preview suite's composition (previewServer) so there is one
// construction of the auth harness for every route suite, and adds the
// anonymous client the public read needs: the preview suite's client is
// signed in, and a page that were only reachable signed in would be a page
// the contract's `security: []` does not have.
type pageServer struct {
	*previewServer
	anon    *http.Client
	pages   *fakePages
	members *fakeMembers
}

// newPageServer composes the surface: the real guard (so the anonymous read
// is the production one — the guard lets unauthenticated GETs through and
// the handler decides) over a mux carrying this package's routes.
func newPageServer(t *testing.T, projectErr error, memberOf ...string) *pageServer {
	t.Helper()
	pages := &fakePages{
		states: map[assets.PID]assets.PageState{
			sampleAssetPID: pageTestPageState(t, assets.VisibilityPublic),
		},
		rows: pageTestBrowseRows(),
	}
	members := &fakeMembers{memberOf: map[string]bool{}}
	for _, id := range memberOf {
		members.memberOf[id] = true
	}
	gate := &fakeGate{err: projectErr}
	inner := newAssetsServer(t, Deps{State: &fakeState{}, Projects: gate, Pages: pages, Members: members})
	jar, _ := cookiejar.New(nil)
	return &pageServer{
		previewServer: inner,
		pages:         pages,
		members:       members,
		anon: &http.Client{
			Jar:           jar,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// get performs one GET as the given client and returns the response.
func (s *pageServer) get(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()
	resp, err := client.Get(s.ts.URL + url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

// rawBody reads a response body as a string, failing loudly.
func rawBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

// assetPageURL is the route under test, spelled exactly as the contract
// spells it (specs/api/openapi.yaml: /assets/{assetId}).
func assetPageURL(pid assets.PID) string { return "/api/v1/assets/" + string(pid) }

// ---- the page route --------------------------------------------------------

// TestAssetPageIsServedAnonymouslyAndCompletely: the contract's path is a
// public read (`security: []`), so no session is involved; the response is
// the model's page; and the raw body carries none of the fixture's private
// entities — the transport does not add a field or leak one the model
// withheld.
func TestAssetPageIsServedAnonymouslyAndCompletely(t *testing.T) {
	server := newPageServer(t, nil)
	resp := server.get(t, server.anon, assetPageURL(sampleAssetPID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous GET %s = %d, want 200 (the contract's security: [])", assetPageURL(sampleAssetPID), resp.StatusCode)
	}
	body := rawBody(t, resp)
	var page map[string]any
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	// The eleven items of docs/42 §Asset Page, by the keys the model renders
	// them under. The model's own test walks the spec sentence; this one
	// checks the wire carries what the model built.
	for _, block := range []string{
		"asset", "version", "origin", "rights", "creators",
		"metadata", "dependencies", "lineage", "used_by", "versions", "events",
	} {
		if _, ok := page[block]; !ok {
			t.Errorf("the response has no %q block: %s", block, body)
		}
	}
	version, _ := page["version"].(map[string]any)
	if version["version"] != pageTestNewVersion {
		t.Errorf("rendered version = %v, want the newest public one %q", version["version"], pageTestNewVersion)
	}
	if version["url"] != "/assets/"+string(sampleAssetPID)+"/"+pageTestNewVersion {
		t.Errorf("version url = %v, want the version's own URL", version["url"])
	}
	// The private entities the reader resolved are NOT on the wire: the
	// transport does not filter, so this asserts the model's decision
	// survives serialization (the raw body is what leaks).
	for _, secret := range []string{pageTestHiddenThing, string(pageTestSecretPid), "0.9", pageTestSecretLab, "secret-lab", pageTestSecretTitle} {
		if strings.Contains(body, secret) {
			t.Errorf("the page leaks %q: %s", secret, body)
		}
	}
	// The public facts ARE there, so the absence above is not the absence of
	// a page.
	if !strings.Contains(body, "MOF water-stability screening set") || !strings.Contains(body, "Open MOF Lab") {
		t.Errorf("the page is missing its public content: %s", body)
	}
}

// TestAssetPageAnswersOneIndistinguishable404 is the existence-hiding rule
// on this surface: an unknown pid, an asset whose project the caller may not
// read, and an asset with nothing the caller may see all answer with the
// same status, the same code and the same message. Anything else is an
// oracle for which pids exist and which private projects have published.
func TestAssetPageAnswersOneIndistinguishable404(t *testing.T) {
	refusal := func(t *testing.T, resp *http.Response) (int, string, string) {
		t.Helper()
		status := resp.StatusCode
		envelope := decodeJSON(t, resp)
		code, _ := envelope["code"].(string)
		message, _ := envelope["message"].(string)
		return status, code, message
	}

	// (a) a pid the reader has no row for.
	server := newPageServer(t, nil)
	unknown := assets.PID("01j9z6k3m4n5p6q7r8s9t0v1w8")
	status, code, message := refusal(t, server.get(t, server.anon, assetPageURL(unknown)))

	// (b) an asset whose project the caller may not read: the project gate
	// answers the way the project surface answers a denied read.
	hidden := newPageServer(t, projects.ErrProjectNotFound)
	if s, c, m := refusal(t, hidden.get(t, hidden.anon, assetPageURL(sampleAssetPID))); s != status || c != code || m != message {
		t.Errorf("a refused project answers %d %s %q, want the same as an unknown pid (%d %s %q)", s, c, m, status, code, message)
	}

	// (c) an asset whose every version is private, read by a non-member.
	empty := newPageServer(t, nil)
	state := pageTestPageState(t, assets.VisibilityPublic)
	for i := range state.Versions {
		state.Versions[i].Visibility = assets.VisibilityPrivate
	}
	empty.pages.states[sampleAssetPID] = state
	if s, c, m := refusal(t, empty.get(t, empty.anon, assetPageURL(sampleAssetPID))); s != status || c != code || m != message {
		t.Errorf("an all-private asset answers %d %s %q, want the same as an unknown pid (%d %s %q)", s, c, m, status, code, message)
	}

	// (d) a segment that is not a pid at all: the slug form of the same URL.
	// It cannot resolve, so it is the same 404 rather than a 400 that would
	// tell a client which shape is a real identity.
	if s, c, m := refusal(t, server.get(t, server.anon, "/api/v1/assets/mof-water-stability")); s != status || c != code || m != message {
		t.Errorf("a slug segment answers %d %s %q, want the same as an unknown pid (%d %s %q)", s, c, m, status, code, message)
	}

	if status != http.StatusNotFound || code != CodeAssetNotFound {
		t.Errorf("the shared refusal = %d %s, want 404 %s", status, code, CodeAssetNotFound)
	}
	if strings.Contains(message, "project") || strings.Contains(message, "private") {
		t.Errorf("the refusal message %q names a reason; the cases must be indistinguishable", message)
	}
}

// TestAssetPageVersionParameter: the version query renders the version it
// names (the URL /assets/{pid}/{version} is a persistent address), a value
// that is not a version label is refused 400 rather than looked up, and a
// version the caller may not see is the same 404 as everything else — never
// "that version exists but is private".
func TestAssetPageVersionParameter(t *testing.T) {
	server := newPageServer(t, nil)

	resp := server.get(t, server.anon, assetPageURL(sampleAssetPID)+"?version="+pageTestOldVersion)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET with version=%s = %d, want 200", pageTestOldVersion, resp.StatusCode)
	}
	var page map[string]any
	if err := json.Unmarshal([]byte(rawBody(t, resp)), &page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	version, _ := page["version"].(map[string]any)
	if version["version"] != pageTestOldVersion {
		t.Errorf("version = %v, want the requested %q", version["version"], pageTestOldVersion)
	}

	// A label that cannot be stored is refused before any read.
	before := server.pages.reads
	resp = server.get(t, server.anon, assetPageURL(sampleAssetPID)+"?version=not%20a%20version")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("GET with a malformed version = %d, want 400", resp.StatusCode)
	} else if code := errorCode(t, resp); code != CodeAssetPageValidationFailed {
		t.Errorf("malformed version code = %q, want %q — the page route's own code, not the list's",
			code, CodeAssetPageValidationFailed)
	}
	if server.pages.reads != before {
		t.Errorf("the reader ran %d times for a malformed label, want no read at all", server.pages.reads-before)
	}

	// A version the caller may not see: the same 404 as an unknown pid.
	private := server.get(t, server.anon, assetPageURL(sampleAssetPID)+"?version="+pageTestHiddenThing)
	if private.StatusCode != http.StatusNotFound {
		t.Errorf("GET with a private version = %d, want 404", private.StatusCode)
	} else if code := errorCode(t, private); code != CodeAssetNotFound {
		t.Errorf("private version code = %q, want %q", code, CodeAssetNotFound)
	}
	// …and the same as a version that does not exist at all.
	missing := server.get(t, server.anon, assetPageURL(sampleAssetPID)+"?version=99.0")
	if missing.StatusCode != http.StatusNotFound || errorCode(t, missing) != CodeAssetNotFound {
		t.Errorf("a nonexistent version = %d, want 404 %s", missing.StatusCode, CodeAssetNotFound)
	}
}

// TestAssetPageMembershipIsReadFromTheProjectService: a member of the
// asset's project renders the private version and the project's identity; a
// caller who is not a member of it gets neither. The bit comes from the
// membership port (the project service in production) and never from the
// request — there is no parameter a client could send to become a member.
func TestAssetPageMembershipIsReadFromTheProjectService(t *testing.T) {
	member := newPageServer(t, nil, pageTestProject)
	resp := member.get(t, member.client, assetPageURL(sampleAssetPID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("member GET = %d, want 200", resp.StatusCode)
	}
	body := rawBody(t, resp)
	var page map[string]any
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	version, _ := page["version"].(map[string]any)
	if version["version"] != pageTestHiddenThing {
		t.Errorf("member renders version %v, want their own newest %q", version["version"], pageTestHiddenThing)
	}
	if !strings.Contains(body, pageTestProject) {
		t.Errorf("member's page does not name their own project: %s", body)
	}

	// The same authenticated caller, not a member of that project: the
	// public view, and the private version is nowhere on the wire.
	stranger := newPageServer(t, nil, "77777777-7777-4777-8777-777777777777")
	resp = stranger.get(t, stranger.client, assetPageURL(sampleAssetPID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("non-member GET = %d, want 200 for a public version", resp.StatusCode)
	}
	strangerBody := rawBody(t, resp)
	// The private VERSION is what must be absent, and it is the assertion
	// that costs something: this project is public, so its identity is
	// rendered (and must be — that is the row above), while the private
	// version's label, its metadata and its event are not on the wire.
	if strings.Contains(strangerBody, pageTestHiddenThing) {
		t.Errorf("a non-member's page carries the private version: %s", strangerBody)
	}
	var strangerPage map[string]any
	if err := json.Unmarshal([]byte(strangerBody), &strangerPage); err != nil {
		t.Fatalf("decode: %v", err)
	}
	versions, _ := strangerPage["versions"].([]any)
	if len(versions) != 2 {
		t.Errorf("a non-member sees %d versions, want the 2 public ones: %s", len(versions), strangerBody)
	}
}

// TestAssetPageReadsTheStateOnce: one request is one read. A surface that
// read the state per block would answer a page assembled from two different
// instants, which is the thing the reader's whole-state contract exists to
// prevent.
func TestAssetPageReadsTheStateOnce(t *testing.T) {
	server := newPageServer(t, nil)
	before := server.pages.reads
	if resp := server.get(t, server.anon, assetPageURL(sampleAssetPID)); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if got := server.pages.reads - before; got != 1 {
		t.Errorf("one request read the state %d times, want 1", got)
	}
	if server.pages.gotPID != sampleAssetPID {
		t.Errorf("the reader was asked for %q, want the pid in the path", server.pages.gotPID)
	}
}

// TestAssetPageUnavailableStateIsNotAnEmptyPage: a read failure is a 503 for
// the whole page, never a page built over a state nobody finished reading —
// "no usages, no events" is a claim about the repository, and a failed read
// has no answer to give.
func TestAssetPageUnavailableStateIsNotAnEmptyPage(t *testing.T) {
	server := newPageServer(t, nil)
	server.pages.err = errors.New("connection refused")
	resp := server.get(t, server.anon, assetPageURL(sampleAssetPID))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET with a failing reader = %d, want 503", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != CodeAssetPageUnavailable {
		t.Errorf("code = %q, want %q", code, CodeAssetPageUnavailable)
	}

	// A failing project gate is the same: no page, and never a masked 404
	// that would say "this asset does not exist" about a project nobody
	// could read.
	broken := newPageServer(t, projects.ErrStore)
	resp = broken.get(t, broken.anon, assetPageURL(sampleAssetPID))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET with a failing gate = %d, want 503", resp.StatusCode)
	} else if code := errorCode(t, resp); code != CodeAssetPageUnavailable {
		t.Errorf("gate failure code = %q, want %q", code, CodeAssetPageUnavailable)
	}

	// A failing membership read is a 503 too: silently answering "not a
	// member" would downgrade a member's view, which is a wrong page nobody
	// would report.
	unreadable := newPageServer(t, nil)
	unreadable.members.err = errors.New("connection refused")
	resp = unreadable.get(t, unreadable.client, assetPageURL(sampleAssetPID))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET with a failing membership read = %d, want 503", resp.StatusCode)
	}
}

// ---- the browse route ------------------------------------------------------

// TestAssetBrowseFiltersByTheClosedTypeSet: the list is anonymous, it
// carries the four V1 types, and each one selects its own rows. A type
// outside the set is refused 400 — an ignored filter would return an empty
// list, which says "there are no such assets".
func TestAssetBrowseFiltersByTheClosedTypeSet(t *testing.T) {
	server := newPageServer(t, nil)
	for _, ty := range assets.AllTypes() {
		resp := server.get(t, server.anon, "/api/v1/assets?type="+string(ty))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET ?type=%s = %d, want 200", ty, resp.StatusCode)
		}
		var list struct {
			Type   *string `json:"type"`
			Types  []string
			Assets []struct {
				PID  string `json:"pid"`
				Type string `json:"type"`
			} `json:"assets"`
		}
		if err := json.Unmarshal([]byte(rawBody(t, resp)), &list); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		if list.Type == nil || *list.Type != string(ty) {
			t.Errorf("?type=%s echoed %v, want the filter that was applied", ty, list.Type)
		}
		if len(list.Types) != 4 {
			t.Errorf("?type=%s carried %d types, want the closed set of 4", ty, len(list.Types))
		}
		for _, item := range list.Assets {
			if item.Type != string(ty) {
				t.Errorf("?type=%s returned a %s asset (%s)", ty, item.Type, item.PID)
			}
		}
		// The filter reached the reader as well: the model filters too, but
		// the store is what keeps the list cheap, and a handler that dropped
		// the parameter would be correct only by accident.
		if server.pages.gotFil == nil || *server.pages.gotFil != ty {
			t.Errorf("the read behind ?type=%s got %v, want the requested type", ty, server.pages.gotFil)
		}
	}

	// An unrecognised type: 400, no read, and a message that names the set.
	before := server.pages.reads
	resp := server.get(t, server.anon, "/api/v1/assets?type=model")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET ?type=model = %d, want 400", resp.StatusCode)
	}
	envelope := errorEnvelope(t, resp)
	if code, _ := envelope["code"].(string); code != CodeAssetListValidationFailed {
		t.Errorf("code = %q, want %q", code, CodeAssetListValidationFailed)
	}
	message, _ := envelope["message"].(string)
	for _, ty := range assets.AllTypes() {
		if !strings.Contains(message, string(ty)) {
			t.Errorf("the refusal message %q does not offer %q, so a client cannot learn the set", message, ty)
		}
	}
	if server.pages.reads != before {
		t.Errorf("an unrecognised type ran %d reads, want none", server.pages.reads-before)
	}
}

// TestAssetBrowseWithholdsPrivateProjectsAndCounts: the list is one answer
// for every caller, so it names a project only when that project is public,
// and it carries no number about anything a reader cannot open.
func TestAssetBrowseWithholdsPrivateProjectsAndCounts(t *testing.T) {
	server := newPageServer(t, nil)
	resp := server.get(t, server.anon, "/api/v1/assets")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous GET /api/v1/assets = %d, want 200", resp.StatusCode)
	}
	body := rawBody(t, resp)
	if !strings.Contains(body, "MOF water-stability screening set") {
		t.Errorf("the list is missing the public assets: %s", body)
	}
	// The private-project row is still listed (the asset is public) but
	// without its project.
	var list struct {
		Assets []map[string]any `json:"assets"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	var withheld, named int
	for _, item := range list.Assets {
		switch item["origin_project"].(type) {
		case nil:
			withheld++
		case map[string]any:
			named++
		default:
			t.Errorf("origin_project = %v, want null or an object", item["origin_project"])
		}
	}
	if withheld != 1 || named != 2 {
		t.Errorf("origin_project: %d withheld, %d named, want 1 and 2 (the private project only)", withheld, named)
	}
	// The page's own API path is not a filter: the IDs and the version URLs
	// are what a reader clicks through.
	for _, item := range list.Assets {
		pid, _ := item["pid"].(string)
		if item["url"] != "/assets/"+pid {
			t.Errorf("item %s url = %v, want its persistent URL", pid, item["url"])
		}
		if item["latest_url"] != "/assets/"+pid+"/"+item["latest_version"].(string) {
			t.Errorf("item %s latest_url = %v, want the version URL", pid, item["latest_url"])
		}
	}
}
