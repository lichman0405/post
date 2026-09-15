package assetshttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/rights"
)

// Task T0704 required test "publish preview tests" — the transport half.
//
// The surface is one route, and these tests pin the four things it can do
// besides serve a preview: refuse an anonymous write (the guard's own rule,
// inherited by registering on the v1 subtree), refuse a caller who cannot
// read the project, refuse a body that is not JSON, and refuse to build a
// preview out of a state it could not read. Everything the route answers
// with when it CAN answer is internal/assets' business and is pinned there;
// what is pinned here is the wiring — that the handler hands the gate's
// candidate and the project id to the preview, and answers with what the
// preview said.

// fakeState is the state reader: canned state or a canned failure, recording
// the candidate it was asked to resolve.
type fakeState struct {
	state    assets.CurrentState
	err      error
	got      assets.PublishCandidate
	requests int
}

func (f *fakeState) ResolvePreviewState(_ context.Context, c assets.PublishCandidate) (assets.CurrentState, error) {
	f.requests++
	f.got = c
	if f.err != nil {
		return assets.CurrentState{}, f.err
	}
	return f.state, nil
}

// fakeGate is the project read gate: a canned project or error, recording
// the reader and the project it was handed.
type fakeGate struct {
	err       error
	gotReader projects.Reader
	gotID     string
	calls     int
}

func (f *fakeGate) Get(_ context.Context, r projects.Reader, projectID string) (domain.Project, error) {
	f.calls++
	f.gotReader, f.gotID = r, projectID
	if f.err != nil {
		return domain.Project{}, f.err
	}
	return domain.Project{ID: projectID}, nil
}

// testProjectID is the project every request below is addressed to.
const testProjectID = "11111111-1111-4111-8111-111111111111"

// previewServer is the composed surface: the real auth guard (so the
// session/CSRF rule is the production one, not a stub) over a mux carrying
// only this package's route.
type previewServer struct {
	ts     *httptest.Server
	client *http.Client
	csrf   string
	state  *fakeState
	gate   *fakeGate
}

func newPreviewServer(t *testing.T, state *fakeState, gate *fakeGate) *previewServer {
	t.Helper()
	authAPI := authhttp.New(authhttp.Deps{
		Users:    memstore.NewUsers(),
		Sessions: memstore.NewSessions(),
		Limiter:  memstore.NewLimiter(),
		Cfg: authn.Config{
			WebOrigin:          "http://web.test",
			SessionTTL:         time.Hour,
			LoginLimitPerEmail: 1000,
			LoginLimitPerIP:    10000,
			LoginWindow:        time.Minute,
			// Every case in this file signs up from the same address, and
			// the real budget (authn.DefaultSignupPerIP) is small on purpose
			// — raise it here so a case tests the route, not the limiter.
			SignupLimitPerIP: 10000,
		},
	})
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	New(Deps{State: state, Projects: gate}).Register(mux)
	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Post(ts.URL+"/api/v1/auth/signup", "application/json",
		strings.NewReader(`{"email":"preview@example.com","password":"long-enough-password-1","handle":"previewer","display_name":"Previewer"}`))
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("signup = %d, want 201", resp.StatusCode)
	}
	var payload struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode signup: %v", err)
	}
	return &previewServer{ts: ts, client: client, csrf: payload.CSRFToken, state: state, gate: gate}
}

// previewURL is the route under test, spelled exactly as the contract
// spells it (specs/api/openapi.yaml: /projects/{projectId}/assets:publish-preview).
func previewURL(projectID string) string {
	return "/api/v1/projects/" + projectID + "/assets:publish-preview"
}

// post sends the body as the web app sends it (JSON content type, CSRF
// token attached) and returns the response.
func (s *previewServer) post(t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.ts.URL+url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", s.csrf)
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

// decodeJSON reads a response body into a map, failing loudly on a body
// that is not JSON.
func decodeJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s: %v", resp.Request.URL.Path, err)
	}
	return out
}

// errorEnvelope decodes a refusal (authhttp's flat code/message/request_id)
// and fails when there is none — a refusal without a code is not one a
// client can act on. It reads the body, so call it once per response.
func errorEnvelope(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	envelope := decodeJSON(t, resp)
	if code, _ := envelope["code"].(string); code == "" {
		t.Fatalf("no error code in the response body %v", envelope)
	}
	return envelope
}

// errorCode is the code of a refusal.
func errorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	code, _ := errorEnvelope(t, resp)["code"].(string)
	return code
}

// depFor finds a private_dependencies entry by the ref it names.
func depFor(t *testing.T, deps []any, ref string) map[string]any {
	t.Helper()
	for _, raw := range deps {
		dep, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("private_dependencies entry %v is not an object", raw)
		}
		if dep["ref"] == ref {
			return dep
		}
	}
	return nil
}

// stateFor builds the state a preview resolves against: the stored asset of
// the project, one answer per ref, and the pins and blobs the test wants.
func stateFor(asset *assets.StoredAsset, refs []assets.StoredRef, pins []assets.StoredPin, blobs []assets.StoredBlob) assets.CurrentState {
	return assets.CurrentState{Asset: asset, Refs: refs, Pins: pins, Blobs: blobs}
}

// previewBody builds a request body: a candidate the gate accepts, with the
// parts a test needs to change.
func previewBody(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	rightsRaw, err := rights.New().Marshal()
	if err != nil {
		t.Fatalf("rights.New().Marshal: %v", err)
	}
	manifest := map[string]any{
		"version":    1,
		"asset_type": "dataset",
		"metadata": map[string]any{
			"purpose":       "measure the thing",
			"data_type":     "table",
			"blob_ids":      []any{"a value"},
			"access_level":  "restricted",
			"quality_notes": "checked",
		},
	}
	hash := manifestHash(t, manifest)
	body := map[string]any{
		"asset_pid":      "01j9z6k3m4n5p6q7r8s9t0v1w2",
		"asset_type":     "dataset",
		"version":        "1.0",
		"manifest":       manifest,
		"rights":         json.RawMessage(rightsRaw),
		"origin_refs":    []any{"release:0f4d2c1b-9a87-4653-8b21-7e6f5d4c3b2a"},
		"visibility":     "public",
		"integrity_hash": hash,
		"creator_ids":    []any{"22222222-2222-4222-8222-222222222222"},
	}
	if mutate != nil {
		mutate(body)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return string(raw)
}

// manifestHash renders the canonical hash of a manifest built as a Go map —
// the same hash the gate requires the candidate to carry.
func manifestHash(t *testing.T, manifest map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	m, err := assets.ParseManifest(raw)
	if err != nil {
		t.Fatalf("the fixture manifest must pass the gate's own parser: %v", err)
	}
	hash, err := m.Hash()
	if err != nil {
		t.Fatalf("manifest hash: %v", err)
	}
	return hash
}

// TestPreviewServesTheImpact is the route's happy path: an authenticated
// caller who can read the project gets the preview of the candidate it
// sent, computed over the state the reader answered with.
func TestPreviewServesTheImpact(t *testing.T) {
	releaseRef := "release:0f4d2c1b-9a87-4653-8b21-7e6f5d4c3b2a"
	state := &fakeState{state: stateFor(
		&assets.StoredAsset{
			ID:              "33333333-3333-4333-8333-333333333333",
			PID:             "01j9z6k3m4n5p6q7r8s9t0v1w2",
			Type:            assets.TypeDataset,
			Title:           "A dataset",
			OriginProjectID: testProjectID,
		},
		[]assets.StoredRef{{
			Ref:               assets.OriginRef(releaseRef),
			Resolved:          true,
			ProjectID:         testProjectID,
			ProjectVisibility: assets.VisibilityPrivate,
		}},
		nil,
		nil,
	)}
	gate := &fakeGate{}
	srv := newPreviewServer(t, state, gate)

	resp := srv.post(t, previewURL(testProjectID), previewBody(t, nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, readBody(t, resp))
	}
	body := decodeJSON(t, resp)

	// The identity half: the route's project and the caller's candidate.
	if body["project_id"] != testProjectID {
		t.Errorf("project_id = %v, want the route's project", body["project_id"])
	}
	if body["version"] != "1.0" || body["target_visibility"] != "public" {
		t.Errorf("version/visibility = %v/%v, want 1.0/public", body["version"], body["target_visibility"])
	}
	// The five lists are present as arrays, even the empty ones: a client
	// must not have to tell "absent" from "empty".
	for _, key := range []string{"objects", "metadata", "blobs", "refs", "dependencies",
		"private_dependencies", "publish_blockers", "rights_blockers"} {
		if _, ok := body[key].([]any); !ok {
			t.Errorf("%s = %v, want an array", key, body[key])
		}
	}
	// The ref resolved into the publishing project's own private state, so
	// it is named and does not block (internal/assets' rule, asserted here
	// only to prove the state the reader answered with reached the model).
	// The manifest's blob_id is deliberately not asserted on: which entries
	// a preview names is internal/assets' business.
	deps, _ := body["private_dependencies"].([]any)
	dep := depFor(t, deps, releaseRef)
	if dep == nil {
		t.Fatalf("private_dependencies = %v, want the project's own ref named", deps)
	}
	if dep["blocking"] != false {
		t.Errorf("private_dependencies entry = %v, want the publishing project's own ref, not blocking", dep)
	}
	// The gate saw the route's project and the caller's identity.
	if gate.gotID != testProjectID {
		t.Errorf("the project gate ran for %q, want %q", gate.gotID, testProjectID)
	}
	if !gate.gotReader.Authenticated || gate.gotReader.UserID == "" {
		t.Errorf("the project gate got reader %+v, want the authenticated caller", gate.gotReader)
	}
	// The reader saw the candidate as the client sent it.
	if state.requests != 1 {
		t.Fatalf("the state reader ran %d times, want once", state.requests)
	}
	if state.got.Version != "1.0" || string(state.got.AssetPID) != "01j9z6k3m4n5p6q7r8s9t0v1w2" ||
		state.got.Visibility != assets.VisibilityPublic || len(state.got.OriginRefs) != 1 {
		t.Errorf("the state reader got %+v, want the candidate the client sent", state.got)
	}
}

// TestPreviewRequiresASession: this is a write method on the shared v1
// subtree, so the guard refuses it before routing, and the handler refuses
// it again as a backstop. Either way the state is never read.
func TestPreviewRequiresASession(t *testing.T) {
	state := &fakeState{}
	srv := newPreviewServer(t, state, &fakeGate{})

	// The same request WITHOUT the session cookie (a fresh client) and
	// without a CSRF token.
	anon := &http.Client{}
	req, err := http.NewRequest(http.MethodPost, srv.ts.URL+previewURL(testProjectID),
		strings.NewReader(previewBody(t, nil)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := anon.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for an anonymous preview", resp.StatusCode)
	}
	if state.requests != 0 {
		t.Errorf("the state reader ran %d times for an anonymous request, want 0", state.requests)
	}
}

// TestPreviewRejectsAProjectTheCallerCannotRead: the existence-hiding 404 of
// every other project read, answered before the body is read at all.
func TestPreviewRejectsAProjectTheCallerCannotRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"hidden or missing", projects.ErrProjectNotFound, http.StatusNotFound, projects.CodeProjectNotFound},
		{"forbidden", projects.ErrForbidden, http.StatusForbidden, projects.CodeProjectForbidden},
		{"store down", projects.ErrStore, http.StatusServiceUnavailable, CodePreviewUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := &fakeState{}
			srv := newPreviewServer(t, state, &fakeGate{err: tc.err})

			resp := srv.post(t, previewURL(testProjectID), previewBody(t, nil))
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			if got := errorCode(t, resp); got != tc.code {
				t.Errorf("error code = %s, want %s", got, tc.code)
			}
			if state.requests != 0 {
				t.Errorf("the state reader ran %d times behind a refused project, want 0", state.requests)
			}
		})
	}
}

// TestPreviewRejectsANonJSONBody: the one thing the transport refuses by
// itself. A candidate the publish gate refuses is NOT this case — it is
// answered 200 with the refusals listed (TestPreviewAnswersARefusedCandidate).
func TestPreviewRejectsANonJSONBody(t *testing.T) {
	state := &fakeState{}
	srv := newPreviewServer(t, state, &fakeGate{})

	resp := srv.post(t, previewURL(testProjectID), `{"asset_pid": `)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if got := errorCode(t, resp); got != CodePreviewValidationFailed {
		t.Errorf("error code = %s, want %s", got, CodePreviewValidationFailed)
	}
	if state.requests != 0 {
		t.Errorf("the state reader ran %d times for a malformed body, want 0", state.requests)
	}
}

// TestPreviewAnswersARefusedCandidate is the design decision this surface
// would be tempting to get wrong: a candidate the publish gate refuses is
// answered 200, with the refusals in the body. A preview exists so a human
// can decide, and "this cannot execute, here is why, and here is what it
// would have exposed anyway" is the answer that decision needs; a 400 would
// say nothing about the impact.
func TestPreviewAnswersARefusedCandidate(t *testing.T) {
	state := &fakeState{state: stateFor(&assets.StoredAsset{
		PID:             "01j9z6k3m4n5p6q7r8s9t0v1w2",
		Type:            assets.TypeDataset,
		OriginProjectID: testProjectID,
	}, []assets.StoredRef{{
		Ref:               assets.OriginRef("release:0f4d2c1b-9a87-4653-8b21-7e6f5d4c3b2a"),
		Resolved:          true,
		ProjectID:         testProjectID,
		ProjectVisibility: assets.VisibilityPublic,
	}}, nil, nil)}
	srv := newPreviewServer(t, state, &fakeGate{})

	// The same candidate with its rights declaration replaced by one this
	// platform does not read, and no creators.
	body := previewBody(t, func(b map[string]any) {
		b["rights"] = map[string]any{"version": 99}
		b["creator_ids"] = []any{}
	})
	resp := srv.post(t, previewURL(testProjectID), body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 even for a candidate that cannot execute (body %s)",
			resp.StatusCode, readBody(t, resp))
	}
	decoded := decodeJSON(t, resp)
	if decoded["publishable"] != false {
		t.Errorf("publishable = %v, want false", decoded["publishable"])
	}
	rightsBlockers, _ := decoded["rights_blockers"].([]any)
	if len(rightsBlockers) == 0 {
		t.Errorf("rights_blockers = %v, want the rights model's refusal", rightsBlockers)
	}
	blockers, _ := decoded["publish_blockers"].([]any)
	if len(blockers) == 0 {
		t.Errorf("publish_blockers = %v, want the gate's refusal (no contributors)", blockers)
	}
}

// TestPreviewRefusesADroppedRef: the reader's contract failure. A state that
// does not answer for a canonical ref is this service's defect, and it is
// answered 500 rather than rendered as "that ref does not exist" — a
// preview that reported another project's entity as nonexistent would be
// wrong in the direction that matters.
func TestPreviewRefusesADroppedRef(t *testing.T) {
	state := &fakeState{state: stateFor(&assets.StoredAsset{
		PID:             "01j9z6k3m4n5p6q7r8s9t0v1w2",
		Type:            assets.TypeDataset,
		OriginProjectID: testProjectID,
	}, nil, nil, nil)}
	srv := newPreviewServer(t, state, &fakeGate{})

	resp := srv.post(t, previewURL(testProjectID), previewBody(t, nil))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for a state that dropped a ref (body %s)",
			resp.StatusCode, readBody(t, resp))
	}
	if got := errorCode(t, resp); got != "INTERNAL_ERROR" {
		t.Errorf("error code = %s, want INTERNAL_ERROR", got)
	}
}

// TestPreviewReportsAStateReadFailure: no state, no preview. The reader's
// failure is answered 503 naming nothing internal, never an empty impact —
// "nothing is private here" is precisely the wrong answer to give when
// nobody managed to look.
func TestPreviewReportsAStateReadFailure(t *testing.T) {
	state := &fakeState{err: errors.New("connection refused")}
	srv := newPreviewServer(t, state, &fakeGate{})

	resp := srv.post(t, previewURL(testProjectID), previewBody(t, nil))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", resp.StatusCode, readBody(t, resp))
	}
	envelope := errorEnvelope(t, resp)
	if envelope["code"] != CodePreviewUnavailable {
		t.Errorf("error code = %v, want %s", envelope["code"], CodePreviewUnavailable)
	}
	if msg, _ := envelope["message"].(string); strings.Contains(msg, "connection refused") {
		t.Errorf("the response leaks the store's own error: %q", msg)
	}
}

// TestPreviewRouteIsTheContractPath pins the path and the method: the
// contract fixes them (specs/api/openapi.yaml), so a near miss must not
// silently 404 — and a GET must not quietly answer, because this route's
// input is a document.
func TestPreviewRouteIsTheContractPath(t *testing.T) {
	state := &fakeState{}
	srv := newPreviewServer(t, state, &fakeGate{})

	for _, path := range []string{
		"/api/v1/projects/" + testProjectID + "/assets/publish-preview",
		"/api/v1/projects/" + testProjectID + "/assets:publish-preview/",
		"/api/v1/projects/" + testProjectID + "/assets:publish",
	} {
		req, err := http.NewRequest(http.MethodPost, srv.ts.URL+path, strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", srv.csrf)
		resp, err := srv.client.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("POST %s answered 200: the route must be the contract's path only", path)
		}
	}
	if state.requests != 0 {
		t.Errorf("the state reader ran %d times for a wrong path, want 0", state.requests)
	}
}

// readBody reads a response body for a failure message.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "(unreadable)"
	}
	return string(raw)
}
