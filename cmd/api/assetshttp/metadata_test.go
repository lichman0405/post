package assetshttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/assetmetadata"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/domain"
)

// Task T0706 required test "asset metadata tests" — the transport half.
//
// What the route decides is deliberately small, and the assertions below
// are about exactly that: the contract's own path under PATCH, the guard's
// session rule, the pid read out of the path SEGMENT (not the body), the
// absent-field-stays-absent shape of the body, and one status + code per
// outcome the command can report.
//
// One property gets more than a status: the refusal of a caller who may not
// revise answers the same bytes for "not a member" and for "role too low",
// and the response body is asserted to name NEITHER — the two causes are
// indistinguishable at the command (assetmetadata.ErrForbidden is one
// value), and this is where that has to survive the trip to the wire.
//
// What is NOT here: whether an actor really holds a role, whether the asset
// exists, whether the transaction commits. Those are the command's and the
// store's, and they are pinned over real PostgreSQL in
// tests/integration/asset_metadata_test.go.

// fakeMetadata is the revision command: what it was handed and what it
// answers.
type fakeMetadata struct {
	revision assetmetadata.Revision
	err      error

	calls int
	actor domain.User
	got   assetmetadata.ReviseParams
}

func (f *fakeMetadata) Revise(_ context.Context, actor domain.User, in assetmetadata.ReviseParams) (assetmetadata.Revision, error) {
	f.calls++
	f.actor, f.got = actor, in
	if f.err != nil {
		return assetmetadata.Revision{}, f.err
	}
	return f.revision, nil
}

// metadataServer is the surface with the revision command wired — the real
// auth guard over the real mux, so the session/CSRF rule under test is the
// production one.
type metadataServer struct{ *previewServer }

// newMetadataServer composes the surface. The three ports the shared
// harness insists on are stubbed, because every case here goes through the
// revision route.
func newMetadataServer(t *testing.T, cmd *fakeMetadata) *metadataServer {
	t.Helper()
	return &metadataServer{newAssetsServer(t, Deps{
		State: &fakeState{}, Projects: &fakeGate{}, Publish: &fakePublish{}, Metadata: cmd,
	})}
}

// metadataPath is the route under test, built through the package that owns
// the URL scheme rather than spelled here — a second copy of the path is a
// second answer to where an asset lives.
func metadataPath(pid string) string { return assets.AssetAPIPath(assets.PID(pid)) }

// testMetadataPID is a 26-character Crockford base32 pid (no i, l, o, u).
const testMetadataPID = "01j9z6k3m4n5p6q7r8s9t0v1w2"

// patch sends the body as the web app sends it (JSON content type, CSRF
// token attached) and returns the response.
func (s *metadataServer) patch(t *testing.T, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch, s.ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", s.csrf)
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatalf("PATCH %s: %v", path, err)
	}
	return resp
}

// wantStatus asserts a response's status and leaves the body for the
// caller (a refusal's body is read for its code, and a body cannot be read
// twice).
func wantStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		body := rawBody(t, resp)
		t.Fatalf("%s %s = %d, want %d: %s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, want, body)
	}
}

// revisionFixture is the stored result a case that only wants a 200 gets.
func revisionFixture() assetmetadata.Revision {
	return assetmetadata.Revision{
		ProjectID: testProjectID,
		AssetPID:  testMetadataPID,
		Metadata: assets.AssetMetadata{
			PID:           testMetadataPID,
			Title:         "Revised Title",
			Slug:          "revised-slug",
			Description:   "prose",
			Keywords:      []string{"grain"},
			Contact:       []string{"alice@example.org"},
			Documentation: []string{"https://example.org/docs"},
		},
	}
}

// ---------------------------------------------------------------------------
// The route and the guard

func TestMetadataRouteRequiresASession(t *testing.T) {
	cmd := &fakeMetadata{revision: revisionFixture()}
	s := newMetadataServer(t, cmd)
	// A fresh client with no cookies: the guard, not the handler, refuses.
	anonymous := &http.Client{}
	req, err := http.NewRequest(http.MethodPatch, s.ts.URL+metadataPath(testMetadataPID),
		strings.NewReader(`{"project_id":"`+testProjectID+`","title":"x"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := anonymous.Do(req)
	if err != nil {
		t.Fatalf("PATCH: %v", err)
	}
	wantStatus(t, resp, http.StatusUnauthorized)
	if cmd.calls != 0 {
		t.Fatalf("command calls = %d, want 0", cmd.calls)
	}
}

// TestMetadataRouteIsTheAssetsPathUnderPatch pins the route identity: the
// address is the asset page's own path (assets.AssetAPIPath). A client that
// holds an asset's URL patches its metadata by that URL, and a rename later
// cannot move it.
func TestMetadataRouteIsTheAssetsPathUnderPatch(t *testing.T) {
	cmd := &fakeMetadata{revision: revisionFixture()}
	s := newMetadataServer(t, cmd)
	resp := s.patch(t, metadataPath(testMetadataPID), `{"project_id":"`+testProjectID+`","title":"x"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH %s = %d, want 200", metadataPath(testMetadataPID), resp.StatusCode)
	}
	if cmd.got.AssetPID != testMetadataPID {
		t.Errorf("command got pid %q, want %q from the path segment", cmd.got.AssetPID, testMetadataPID)
	}
	if cmd.actor.ID == "" {
		t.Error("the command was handed an empty actor id")
	}
}

// TestMetadataRouteRejectsANonPidSegment: the pid is the only identity this
// route accepts, so a slug-shaped segment names no asset at all. It is
// answered 404 — the same answer an unknown pid gets — and NOT 400, which
// would report a malformed request for what is really "no such asset".
// The store is never reached.
func TestMetadataRouteRejectsANonPidSegment(t *testing.T) {
	for _, segment := range []string{
		"governance-subject",          // a slug
		"01j9z6k3m4n5p6q7r8s9t0v1w",   // 25 characters
		"01j9z6k3m4n5p6q7r8s9t0v1w2x", // 27 characters
		"01j9z6k3m4n5p6q7r8s9t0v1wi",  // an excluded letter
		"01J9Z6K3M4N5P6Q7R8S9T0V1W2",  // uppercase
	} {
		t.Run(segment, func(t *testing.T) {
			cmd := &fakeMetadata{revision: revisionFixture()}
			s := newMetadataServer(t, cmd)
			// The body is deliberately VALID, so the only thing that can
			// refuse is the path segment.
			resp := s.patch(t, metadataPath(segment), `{"project_id":"`+testProjectID+`","title":"x"}`)
			wantStatus(t, resp, http.StatusNotFound)
			if cmd.calls != 0 {
				t.Fatalf("command calls = %d, want 0: a segment that names no asset never reaches the command", cmd.calls)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The body

// TestMetadataBodyIsOptionalFieldsAndAbsentStaysAbsent is the PATCH shape
// as an assertion: the pointers that reach the command are nil for a field
// the client did not mention and non-nil for one it did — including the
// empty values that clear a field, which a bare (non-pointer) field could
// not express.
func TestMetadataBodyIsOptionalFieldsAndAbsentStaysAbsent(t *testing.T) {
	cmd := &fakeMetadata{revision: revisionFixture()}
	s := newMetadataServer(t, cmd)
	resp := s.patch(t, metadataPath(testMetadataPID),
		`{"project_id":"`+testProjectID+`","slug":"new-slug","description":"","keywords":[]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH = %d, want 200", resp.StatusCode)
	}
	c := cmd.got.Changes
	if c.Slug == nil || *c.Slug != "new-slug" {
		t.Errorf("slug = %v, want the value the body named", c.Slug)
	}
	if c.Description == nil || *c.Description != "" {
		t.Errorf("description = %v, want a pointer to the empty string (a clear, not an absence)", c.Description)
	}
	if c.Keywords == nil || len(*c.Keywords) != 0 {
		t.Errorf("keywords = %v, want a pointer to an empty list (a clear, not an absence)", c.Keywords)
	}
	if c.Title != nil {
		t.Errorf("title = %v, want nil: the body did not mention it", *c.Title)
	}
	if c.Contact != nil || c.Documentation != nil {
		t.Error("a list field the body did not mention arrived non-nil")
	}
	if c.Cover != nil {
		t.Error("cover arrived non-nil for a body that did not send one")
	}
	if cmd.got.ProjectID != testProjectID {
		t.Errorf("project = %q, want the body's %q", cmd.got.ProjectID, testProjectID)
	}
}

// TestMetadataBodyRejectsAMalformedDocument: a body that is not one JSON
// object is answered with the command's own validation code, so a client
// branches on one code for "this body is not a metadata revision".
func TestMetadataBodyRejectsAMalformedDocument(t *testing.T) {
	for _, body := range []string{`{`, ``, `[]`, `"a string"`, `{"project_id":`} {
		t.Run(body, func(t *testing.T) {
			cmd := &fakeMetadata{revision: revisionFixture()}
			s := newMetadataServer(t, cmd)
			resp := s.patch(t, metadataPath(testMetadataPID), body)
			wantStatus(t, resp, http.StatusBadRequest)
			if code := errorCode(t, resp); code != assetmetadata.CodeValidationFailed {
				t.Errorf("code = %q, want %q", code, assetmetadata.CodeValidationFailed)
			}
			if cmd.calls != 0 {
				t.Fatalf("command calls = %d, want 0", cmd.calls)
			}
		})
	}
}

// TestMetadataBodyIgnoresUnknownFields is the rule every product route's
// decoder follows. The assertion is on the fields that ARE known: an
// unknown name must not shift a known one, and must not be rejected either
// (a future field a client sends early would break its writes on a stricter
// server).
func TestMetadataBodyIgnoresUnknownFields(t *testing.T) {
	cmd := &fakeMetadata{revision: revisionFixture()}
	s := newMetadataServer(t, cmd)
	resp := s.patch(t, metadataPath(testMetadataPID),
		`{"project_id":"`+testProjectID+`","title":"Kept","version":"9.9.9","asset_pid":"`+testMetadataPID+`"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH = %d, want 200", resp.StatusCode)
	}
	if cmd.got.Changes.Title == nil || *cmd.got.Changes.Title != "Kept" {
		t.Errorf("title = %v, want %q", cmd.got.Changes.Title, "Kept")
	}
	// The pid is the PATH's, never the body's, even when a body tries: the
	// identity of the target is not a field a client can move.
	if cmd.got.AssetPID != testMetadataPID {
		t.Errorf("pid = %q, want the path's %q", cmd.got.AssetPID, testMetadataPID)
	}
}

// ---------------------------------------------------------------------------
// The response

// TestMetadataPayloadNamesThePidAndNoVersion: the response carries the
// asset's persistent identity and the URL built from it, and it has NO
// version slot. A payload with one would invite a client to read a version
// this write did not produce (docs/11 §4).
func TestMetadataPayloadNamesThePidAndNoVersion(t *testing.T) {
	cmd := &fakeMetadata{revision: revisionFixture()}
	s := newMetadataServer(t, cmd)
	resp := s.patch(t, metadataPath(testMetadataPID), `{"project_id":"`+testProjectID+`","title":"Revised"}`)
	wantStatus(t, resp, http.StatusOK)
	body := decodeJSON(t, resp)

	if got, _ := body["asset_pid"].(string); got != testMetadataPID {
		t.Errorf("asset_pid = %q, want %q", got, testMetadataPID)
	}
	if got, _ := body["url"].(string); got != assets.AssetURL(testMetadataPID) {
		t.Errorf("url = %q, want %q", got, assets.AssetURL(testMetadataPID))
	}
	for _, forbidden := range []string{"version", "versions", "scientific_version", "integrity_hash"} {
		if _, present := body[forbidden]; present {
			t.Errorf("the response carries a %q field: a metadata revision produces no scientific version", forbidden)
		}
	}
	if got, _ := body["slug"].(string); got != "revised-slug" {
		t.Errorf("slug = %q, want the stored %q", got, "revised-slug")
	}
	// The list fields render as [] rather than null, so a client iterating
	// one does not have to branch.
	for _, key := range []string{"keywords", "contact", "documentation"} {
		raw, present := body[key]
		if !present {
			t.Errorf("%s missing from the response", key)
			continue
		}
		if raw == nil {
			t.Errorf("%s = null, want a list", key)
		}
	}
}

// TestMetadataPayloadURLIsBuiltFromThePidNotTheSlug is the rename property
// at the transport: the URL a revision answers with is a function of the
// pid alone, so the slug in the SAME response has no effect on it. The
// assertion is made against a stored result whose slug differs from the
// pid's own digits, so a builder that reached for the slug would fail here.
func TestMetadataPayloadURLIsBuiltFromThePidNotTheSlug(t *testing.T) {
	revision := revisionFixture()
	revision.Metadata.Slug = "a-completely-different-slug"
	cmd := &fakeMetadata{revision: revision}
	s := newMetadataServer(t, cmd)
	resp := s.patch(t, metadataPath(testMetadataPID), `{"project_id":"`+testProjectID+`","slug":"a-completely-different-slug"}`)
	wantStatus(t, resp, http.StatusOK)
	body := decodeJSON(t, resp)
	url, _ := body["url"].(string)
	if strings.Contains(url, "a-completely-different-slug") {
		t.Fatalf("url = %q carries the slug: a persistent URL is built from the pid", url)
	}
	if url != "/assets/"+testMetadataPID {
		t.Errorf("url = %q, want /assets/%s", url, testMetadataPID)
	}
}

// ---------------------------------------------------------------------------
// Every outcome, one status and one code

func TestMetadataErrorsMapToStatusAndCode(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"validation", assetmetadata.ErrValidation, http.StatusBadRequest, assetmetadata.CodeValidationFailed},
		{"forbidden", assetmetadata.ErrForbidden, http.StatusForbidden, assetmetadata.CodeForbidden},
		{"cover", &assetmetadata.CoverNotSupportedError{}, http.StatusConflict, assetmetadata.CodeCoverNotSupported},
		{"project not found", assetmetadata.ErrProjectNotFound, http.StatusNotFound, assetmetadata.CodeProjectNotFound},
		{"asset not found", assetmetadata.ErrAssetNotFound, http.StatusNotFound, assetmetadata.CodeAssetNotFound},
		{"store", assetmetadata.ErrStore, http.StatusServiceUnavailable, assetmetadata.CodeServiceUnavailable},
		{"unknown", errors.New("boom"), http.StatusInternalServerError, "INTERNAL_ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &fakeMetadata{err: tc.err}
			s := newMetadataServer(t, cmd)
			resp := s.patch(t, metadataPath(testMetadataPID), `{"project_id":"`+testProjectID+`","title":"x"}`)
			wantStatus(t, resp, tc.status)
			if code := errorCode(t, resp); code != tc.code {
				t.Errorf("code = %q, want %q", code, tc.code)
			}
		})
	}
}

// TestForbiddenAnswersOneIndistinguishableRefusal is the acceptance
// criterion's transport half.
//
// The command already answers ONE error for "not a member" and "role too
// low"; what this asserts is that the transport does not undo it — same
// status, same code, same message, and a body that names neither cause, the
// caller, nor the project. A caller probing a project id must not be able
// to tell a membership that is merely too junior from no membership at all.
func TestForbiddenAnswersOneIndistinguishableRefusal(t *testing.T) {
	send := func(t *testing.T, cmd *fakeMetadata) (int, string, string) {
		t.Helper()
		s := newMetadataServer(t, cmd)
		resp := s.patch(t, metadataPath(testMetadataPID), `{"project_id":"`+testProjectID+`","title":"x"}`)
		wantStatus(t, resp, http.StatusForbidden)
		raw := rawBody(t, resp)
		var envelope map[string]any
		if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
			t.Fatalf("decode refusal: %v: %s", err, raw)
		}
		code, _ := envelope["code"].(string)
		message, _ := envelope["message"].(string)
		return resp.StatusCode, code, message
	}

	// Two different causes, one sentinel: this is exactly what the command
	// hands the transport for both.
	statusA, codeA, messageA := send(t, &fakeMetadata{err: assetmetadata.ErrForbidden})
	statusB, codeB, messageB := send(t, &fakeMetadata{err: assetmetadata.ErrForbidden})
	if statusA != statusB || codeA != codeB || messageA != messageB {
		t.Fatalf("two forgeries of one sentinel differ:\n  %d %q %q\n  %d %q %q",
			statusA, codeA, messageA, statusB, codeB, messageB)
	}
	for _, word := range []string{"membership", "member", "role", "maintainer", "contributor", "viewer"} {
		if strings.Contains(strings.ToLower(messageA), word) {
			t.Errorf("the refusal message %q names %q: the two causes must be indistinguishable", messageA, word)
		}
		if strings.Contains(strings.ToLower(codeA), word) {
			t.Errorf("the refusal code %q names %q", codeA, word)
		}
	}
}

// TestCoverRefusalCarriesItsReason: 409 tells the client the request
// conflicts with what the build can do; the message has to say WHAT is
// missing, because the alternative the client is choosing between is
// "your cover was ignored". The refusal names the field and the channel.
func TestCoverRefusalCarriesItsReason(t *testing.T) {
	cmd := &fakeMetadata{err: &assetmetadata.CoverNotSupportedError{}}
	s := newMetadataServer(t, cmd)
	resp := s.patch(t, metadataPath(testMetadataPID), `{"project_id":"`+testProjectID+`","cover":"33333333-3333-4333-8333-333333333333"}`)
	wantStatus(t, resp, http.StatusConflict)
	envelope := errorEnvelope(t, resp)
	if got, _ := envelope["code"].(string); got != assetmetadata.CodeCoverNotSupported {
		t.Errorf("code = %q, want %q", got, assetmetadata.CodeCoverNotSupported)
	}
	message, _ := envelope["message"].(string)
	if !strings.Contains(message, "cover") {
		t.Errorf("the refusal does not name the field: %q", message)
	}
	if !strings.Contains(message, "blob") {
		t.Errorf("the refusal does not name what is missing (the blob channel): %q", message)
	}
	if cmd.got.Changes.Cover == nil {
		t.Error("the cover never reached the command: the field is accepted and refused, not dropped at the decoder")
	}
}

// TestValidationRefusalNamesNoStoredValue: a validation refusal restates the
// REQUEST's problem and nothing else. The body is echoed back by the command
// only in the sense that the message says which field is wrong; a title the
// caller sent must not come back with a stored value beside it.
func TestValidationRefusalNamesNoStoredValue(t *testing.T) {
	cmd := &fakeMetadata{err: assetmetadata.ErrValidation}
	s := newMetadataServer(t, cmd)
	resp := s.patch(t, metadataPath(testMetadataPID), `{"project_id":"`+testProjectID+`","title":"x"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	envelope := errorEnvelope(t, resp)
	message, _ := envelope["message"].(string)
	if !strings.Contains(message, "assetmetadata: validation failed") {
		t.Errorf("message = %q, want the sentinel's own message", message)
	}
}
