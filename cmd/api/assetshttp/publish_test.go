package assetshttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/assetpublish"
	"github.com/lichman0405/post/internal/assets"
)

// Task T0705 required test "asset publish tests" — the transport half.
//
// The route is one governed write, and what is pinned here is the wiring
// around it: that the contract's path is the only path, that the guard's
// session rule applies, that the Idempotency-Key comes off the HEADER (and
// only from there), that the actor handed to the command is the session's
// user with the agent flag false, that the project is NOT read-gated here,
// and that every outcome the command can report is answered with a status
// and a code a client can branch on.
//
// One response gets more than that: a refusal by the server-side impact
// re-check answers with the COMPLETE preview (docs/23 §4 — the caller has
// to see which dependency blocked it), and the assertions below are about
// that body being the report rather than a sentence about one.

// fakePublish is the publish command: what it was handed, and what it
// answers.
type fakePublish struct {
	version assetpublish.PublishedVersion
	err     error

	got   assetpublish.PublishParams
	actor assetpublish.Actor
	calls int
}

func (f *fakePublish) Publish(_ context.Context, actor assetpublish.Actor, in assetpublish.PublishParams) (assetpublish.PublishedVersion, error) {
	f.calls++
	f.actor, f.got = actor, in
	if f.err != nil {
		return assetpublish.PublishedVersion{}, f.err
	}
	return f.version, nil
}

// publishFake recovers the fake from a Deps field, for the shared harness.
func publishFake(cmd PublishCommand) *fakePublish {
	f, _ := cmd.(*fakePublish)
	return f
}

// publishURL is the route under test, spelled exactly as the contract
// spells it (specs/api/openapi.yaml: /projects/{projectId}/assets:publish).
func publishURL(projectID string) string {
	return "/api/v1/projects/" + projectID + "/assets:publish"
}

// newPublishServer composes the surface with the publish command wired and
// nothing else interesting.
func newPublishServer(t *testing.T, cmd *fakePublish) *previewServer {
	t.Helper()
	return newAssetsServer(t, Deps{State: &fakeState{}, Projects: &fakeGate{}, Publish: cmd})
}

// postWithHeaders sends the body with the extra headers a case needs.
func (s *previewServer) postWithHeaders(t *testing.T, url, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.ts.URL+url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", s.csrf)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

// publishBody is a publish request body: the same candidate the preview
// suite sends, plus the display fields a create needs.
func publishBody(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	return previewBody(t, mutate)
}

// storedVersion is what the fake command answers with on success.
func storedVersion() assetpublish.PublishedVersion {
	return assetpublish.PublishedVersion{
		ID:            "22222222-2222-4222-8222-222222222222",
		AssetID:       "33333333-3333-4333-8333-333333333333",
		AssetPID:      "01j9z6k3m4n5p6q7r8s9t0v1w2",
		Version:       "1.0",
		Visibility:    assets.VisibilityPublic,
		IntegrityHash: "sha256:abc",
		OriginRefs:    []string{"release:44444444-4444-4444-8444-444444444444"},
		PublishedBy:   "55555555-5555-4555-8555-555555555555",
		PublishedAt:   time.Date(2026, 9, 16, 10, 30, 0, 0, time.UTC),
		Manifest:      json.RawMessage(`{"version":1}`),
		RightsJSON:    json.RawMessage(`{"visibility":{"data_access":"open"}}`),
	}
}

// ---------------------------------------------------------------------------

// TestPublishRouteIsTheContractPath: the route exists where the contract
// puts it, and only there.
func TestPublishRouteIsTheContractPath(t *testing.T) {
	srv := newPublishServer(t, &fakePublish{version: storedVersion()})
	srv.post(t, publishURL(testProjectID), `{}`).Body.Close()

	for _, path := range []string{
		"/api/v1/projects/" + testProjectID + "/assets/publish",
		"/api/v1/projects/" + testProjectID + "/assets:publish/",
		"/api/v1/projects/assets:publish",
	} {
		resp := srv.post(t, path, `{}`)
		resp.Body.Close()
		if resp.StatusCode == http.StatusCreated {
			t.Errorf("POST %s answered 201: the route must be the contract's path only", path)
		}
	}
	if srv.publish.calls != 1 {
		t.Errorf("the command ran %d times, want the one contract-path call", srv.publish.calls)
	}
}

// TestPublishRequiresASession: the route is a write, so the v1 guard's
// session rule applies before it, and the handler's own backstop answers
// the same way.
func TestPublishRequiresASession(t *testing.T) {
	cmd := &fakePublish{version: storedVersion()}
	srv := newPublishServer(t, cmd)

	anon := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequest(http.MethodPost, srv.ts.URL+publishURL(testProjectID), strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := anon.Do(req)
	if err != nil {
		t.Fatalf("anonymous POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous publish = %d, want 401: %s", resp.StatusCode, readBody(t, resp))
	}
	if cmd.calls != 0 {
		t.Errorf("the command ran %d times for an anonymous request", cmd.calls)
	}
}

// TestPublishCreatesTheVersion: the happy path's whole surface — the path
// becomes the command's project, the body becomes its candidate, and the
// 201 carries the stored version's PUBLIC identities.
func TestPublishCreatesTheVersion(t *testing.T) {
	cmd := &fakePublish{version: storedVersion()}
	srv := newPublishServer(t, cmd)

	resp := srv.post(t, publishURL(testProjectID), publishBody(t, func(body map[string]any) {
		// A create: no asset is named, so the command mints the pid and the
		// display fields are the caller's (the gate's own rule, pinned in
		// internal/application/assetpublish).
		body["asset_pid"] = ""
		body["title"] = "A published dataset"
		body["slug"] = "a-published-dataset"
	}))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("publish = %d, want 201: %s", resp.StatusCode, readBody(t, resp))
	}
	got := decodeJSON(t, resp)

	if got["asset_pid"] != "01j9z6k3m4n5p6q7r8s9t0v1w2" {
		t.Errorf("asset_pid = %v, want the stored asset's pid", got["asset_pid"])
	}
	if got["version"] != "1.0" || got["visibility"] != "public" {
		t.Errorf("version/visibility = %v/%v, want 1.0/public", got["version"], got["visibility"])
	}
	if got["integrity_hash"] != "sha256:abc" {
		t.Errorf("integrity_hash = %v, want the stored hash", got["integrity_hash"])
	}
	if got["published_by"] != "55555555-5555-4555-8555-555555555555" {
		t.Errorf("published_by = %v, want the publishing user", got["published_by"])
	}
	if got["published_at"] != "2026-09-16T10:30:00.000Z" {
		t.Errorf("published_at = %v, want the stored instant in UTC", got["published_at"])
	}
	// The internal uuids are not published: the pair (pid, version) is the
	// public identity of a version.
	for _, key := range []string{"id", "asset_id"} {
		if _, present := got[key]; present {
			t.Errorf("the response carries the internal key %q: %v", key, got)
		}
	}
	if got["manifest"] == nil || got["rights"] == nil {
		t.Errorf("the response does not carry the stored documents: %v", got)
	}

	// What the command was handed.
	if cmd.got.ProjectID != testProjectID {
		t.Errorf("project = %q, want the path's project", cmd.got.ProjectID)
	}
	if cmd.got.Title != "A published dataset" || cmd.got.Slug != "a-published-dataset" {
		t.Errorf("title/slug = %q/%q, want the body's", cmd.got.Title, cmd.got.Slug)
	}
	if cmd.got.AssetType != "dataset" || cmd.got.Version != "1.0" {
		t.Errorf("candidate = %+v, want the body's", cmd.got)
	}
	if cmd.actor.User.ID == "" {
		t.Errorf("the command was handed an empty actor")
	}
	if cmd.actor.IsAgent {
		t.Errorf("the handler marked the caller an agent; a session is not one")
	}
	if cmd.got.AssetPID != "" {
		t.Errorf("asset_pid = %q, want the empty pid of a create (the command mints it)", cmd.got.AssetPID)
	}
}

// TestPublishTakesTheIdempotencyKeyFromTheHeader: docs/22 puts the key in
// the header, so that is the only place it is read from — and an absent
// header is nil, not an empty key (an empty key would be a key every
// caller shares).
func TestPublishTakesTheIdempotencyKeyFromTheHeader(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    *string
	}{
		{"present", map[string]string{"Idempotency-Key": "publish-1"}, ptr("publish-1")},
		{"absent", nil, nil},
		{"empty", map[string]string{"Idempotency-Key": ""}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := &fakePublish{version: storedVersion()}
			srv := newPublishServer(t, cmd)
			resp := srv.postWithHeaders(t, publishURL(testProjectID), publishBody(t, nil), c.headers)
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("publish = %d: %s", resp.StatusCode, readBody(t, resp))
			}
			resp.Body.Close()
			switch {
			case c.want == nil && cmd.got.IdempotencyKey != nil:
				t.Errorf("key = %q, want nil", *cmd.got.IdempotencyKey)
			case c.want != nil && cmd.got.IdempotencyKey == nil:
				t.Errorf("key = nil, want %q", *c.want)
			case c.want != nil && *cmd.got.IdempotencyKey != *c.want:
				t.Errorf("key = %q, want %q", *cmd.got.IdempotencyKey, *c.want)
			}
		})
	}
}

// TestPublishDoesNotGateTheProjectRead: the publish route never consults
// the project read gate. The authorization belongs to the command (it is
// not "may the caller read this project" but "may the caller publish
// here"), and a read gate in front of it would answer 404 for a project
// the caller may not see — disclosing by the shape of the refusal which
// projects exist, before the question that decides the write was asked.
func TestPublishDoesNotGateTheProjectRead(t *testing.T) {
	cmd := &fakePublish{version: storedVersion()}
	srv := newPublishServer(t, cmd)
	srv.gate.err = errors.New("the gate must not be reached")

	resp := srv.post(t, publishURL(testProjectID), publishBody(t, nil))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("publish = %d, want the command's answer: %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
	if srv.gate.calls != 0 {
		t.Errorf("the publish route ran the project read gate %d times", srv.gate.calls)
	}
	if srv.state.requests != 0 {
		t.Errorf("the publish route ran the preview's state reader %d times; the publish re-runs it inside its own transaction", srv.state.requests)
	}
}

// TestPublishRejectsANonJSONBody: a body that is not a JSON object is
// refused before the command runs, with the code the command's own
// validation uses.
func TestPublishRejectsANonJSONBody(t *testing.T) {
	cmd := &fakePublish{version: storedVersion()}
	srv := newPublishServer(t, cmd)

	resp := srv.post(t, publishURL(testProjectID), `not json`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("publish = %d, want 400", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != assetpublish.CodeValidationFailed {
		t.Errorf("code = %q, want %q", code, assetpublish.CodeValidationFailed)
	}
	if cmd.calls != 0 {
		t.Errorf("the command ran %d times for an unparseable body", cmd.calls)
	}
}

// TestPublishBlockedAnswersWithTheCompletePreview is the acceptance
// criterion docs/23 §4 turns on: when the server-side impact re-check
// refuses the publication, the caller receives the WHOLE report — every
// entry the refusal was decided on — not a sentence that says "refused".
// A client that is told only "forbidden" cannot see which dependency it
// must fix, nor check that the refusal was about the entry it thinks it
// was.
func TestPublishBlockedAnswersWithTheCompletePreview(t *testing.T) {
	refused := &assetpublish.PublishRefused{
		Preview: assets.ImpactPreview{
			ProjectID:        testProjectID,
			Version:          "1.0",
			TargetVisibility: assets.VisibilityPublic,
			Publishable:      false,
			PublishBlockers: []assets.Blocker{
				{Code: assets.CodePreviewPinUnresolved, Field: "dependency_pins", Detail: "the pin names no stored version"},
			},
			PrivateDependencies: []assets.PrivateDependency{
				{Kind: assets.DependencyAssetVersion, Ref: "01j9z6k3m4n5p6q7r8s9t0v1w9@0.1", Blocking: true, Detail: "the pinned version is private"},
				{Kind: assets.DependencyBlob, Ref: "66666666-6666-4666-8666-666666666666", Blocking: false, Detail: "not openly attached"},
			},
		},
		Reasons: []string{"the pinned version is private"},
	}
	srv := newPublishServer(t, &fakePublish{err: refused})

	resp := srv.post(t, publishURL(testProjectID), publishBody(t, nil))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("a blocked publish = %d, want 409: %s", resp.StatusCode, readBody(t, resp))
	}
	body := decodeJSON(t, resp)

	if body["code"] != assetpublish.CodePublishBlocked {
		t.Errorf("code = %v, want %q", body["code"], assetpublish.CodePublishBlocked)
	}
	if body["request_id"] == nil {
		t.Errorf("the refusal has no request_id: %v", body)
	}
	preview, ok := body["preview"].(map[string]any)
	if !ok {
		t.Fatalf("the refusal carries no preview object: %v", body)
	}
	// The report is the preview, not a summary of one: the identity of the
	// publication, the verdict, and EVERY entry — the blocking ones the
	// refusal was decided on and the non-blocking ones the caller still has
	// to see.
	if preview["project_id"] != testProjectID || preview["version"] != "1.0" {
		t.Errorf("preview = %v, want the publication's identity", preview)
	}
	if preview["publishable"] != false {
		t.Errorf("preview.publishable = %v, want false", preview["publishable"])
	}
	blockers, _ := preview["publish_blockers"].([]any)
	if len(blockers) != 1 {
		t.Fatalf("publish_blockers = %v, want the candidate's refusal", preview["publish_blockers"])
	}
	first, _ := blockers[0].(map[string]any)
	if first["code"] != assets.CodePreviewPinUnresolved {
		t.Errorf("publish_blocker = %v, want the gate's refusal code", first)
	}
	deps, _ := preview["private_dependencies"].([]any)
	if len(deps) != 2 {
		t.Fatalf("private_dependencies = %v, want both entries", preview["private_dependencies"])
	}
	blocking := depFor(t, deps, "01j9z6k3m4n5p6q7r8s9t0v1w9@0.1")
	if blocking == nil || blocking["blocking"] != true || blocking["kind"] != "asset_version" {
		t.Errorf("the blocking private dependency = %v, want it named and blocking", blocking)
	}
	if nonBlocking := depFor(t, deps, "66666666-6666-4666-8666-666666666666"); nonBlocking == nil || nonBlocking["blocking"] != false {
		t.Errorf("the non-blocking entry = %v, want it reported without blocking", nonBlocking)
	}
}

// TestPublishErrorMapping: every outcome the command can report has a
// status and a code of its own — one a client branches on (docs/45), never
// a raw dependency error.
func TestPublishErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"agent", &assetpublish.AgentNotPermittedError{Action: "publish"}, http.StatusForbidden, assetpublish.CodeAgentPublishDenied},
		{"validation", assetpublish.ErrValidation, http.StatusBadRequest, assetpublish.CodeValidationFailed},
		{"not the actor", assetpublish.ErrForbidden, http.StatusForbidden, assetpublish.CodePublishPrivateToPublicRequiresApproval},
		{"unknown project", assetpublish.ErrProjectNotFound, http.StatusNotFound, assetpublish.CodeProjectNotFound},
		{"unknown asset", assetpublish.ErrAssetNotFound, http.StatusNotFound, assetpublish.CodeAssetNotFound},
		{"pid taken", assetpublish.ErrAssetExists, http.StatusConflict, assetpublish.CodeAssetExists},
		{"version already published", assetpublish.ErrVersionImmutable, http.StatusConflict, assetpublish.CodeAssetVersionImmutable},
		{"key reused", assetpublish.ErrIdempotencyConflict, http.StatusConflict, assetpublish.CodeIdempotencyConflict},
		{"policy", assetpublish.ErrPolicyRefused, http.StatusForbidden, assetpublish.CodePolicyRefused},
		{"store", assetpublish.ErrStore, http.StatusServiceUnavailable, assetpublish.CodeServiceUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newPublishServer(t, &fakePublish{err: c.err})
			resp := srv.post(t, publishURL(testProjectID), publishBody(t, nil))
			if resp.StatusCode != c.status {
				t.Fatalf("publish = %d, want %d: %s", resp.StatusCode, c.status, readBody(t, resp))
			}
			if code := errorCode(t, resp); code != c.code {
				t.Errorf("code = %q, want %q", code, c.code)
			}
		})
	}
}

// TestPublishWrappedErrorsKeepTheirCode: the command's outcomes travel
// through wrapping (the store adds persistence context, the command adds
// the port's), and a wrapped outcome must still be the outcome — an
// agent's refusal does not become an internal error because a layer above
// it had something to say.
func TestPublishWrappedErrorsKeepTheirCode(t *testing.T) {
	srv := newPublishServer(t, &fakePublish{err: errors.Join(
		&assetpublish.AgentNotPermittedError{Action: "publish"},
		errors.New("persistence: asset publish write"),
	)})
	resp := srv.post(t, publishURL(testProjectID), publishBody(t, nil))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("publish = %d, want 403: %s", resp.StatusCode, readBody(t, resp))
	}
	if code := errorCode(t, resp); code != assetpublish.CodeAgentPublishDenied {
		t.Errorf("code = %q, want %q", code, assetpublish.CodeAgentPublishDenied)
	}
}

// TestPublishIsIdempotentOnTheWire: the same key repeated answers with the
// same version. The route itself keeps no state — the command and its
// ledger do — so what is pinned here is that the handler passes the key
// through unchanged and renders the SAME body for the same stored version.
func TestPublishIsIdempotentOnTheWire(t *testing.T) {
	cmd := &fakePublish{version: storedVersion()}
	srv := newPublishServer(t, cmd)
	headers := map[string]string{"Idempotency-Key": "publish-1"}
	body := publishBody(t, nil)

	first := srv.postWithHeaders(t, publishURL(testProjectID), body, headers)
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first = %d: %s", first.StatusCode, readBody(t, first))
	}
	rawFirst := readBody(t, first)

	second := srv.postWithHeaders(t, publishURL(testProjectID), body, headers)
	if second.StatusCode != http.StatusCreated {
		t.Fatalf("second = %d: %s", second.StatusCode, readBody(t, second))
	}
	rawSecond := readBody(t, second)

	if rawFirst != rawSecond {
		t.Errorf("the repeat answered a different body:\n first: %s\nsecond: %s", rawFirst, rawSecond)
	}
	if cmd.calls != 2 {
		t.Errorf("the command ran %d times, want the key to reach it both times (the replay is its decision)", cmd.calls)
	}
}

func ptr[T any](v T) *T { return &v }
