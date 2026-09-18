package knowledgehttp

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/knowledgepublish"
)

// The two write routes: POST /api/v1/projects/{projectId}/knowledge:publish
// and its preview. The publish is a governance write, so what is pinned
// here is who gets refused before it runs, what the command is handed, and
// that every outcome it can report has a status and a code.

// ---------------------------------------------------------------------------
// The preview route

// TestPreviewRouteIsTheContractPath: the route exists where the contract
// puts it, and only there.
func TestPreviewRouteIsTheContractPath(t *testing.T) {
	srv := newDefaultServer(t)

	resp := srv.post(t, previewURL(testProjectID), previewRequestBody(t))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preview = %d, want 200: %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()

	for _, path := range []string{
		"/api/v1/projects/" + testProjectID + "/knowledge/preview",
		"/api/v1/projects/" + testProjectID + "/knowledge:publish-preview/",
		"/api/v1/projects/knowledge:publish-preview",
		"/api/v1/knowledge:publish-preview",
	} {
		resp := srv.post(t, path, previewRequestBody(t))
		status := resp.StatusCode
		resp.Body.Close()
		if status == http.StatusOK {
			t.Errorf("POST %s answered 200: the route must be the contract's path only", path)
		}
	}
	if srv.command.previews != 1 {
		t.Errorf("the command ran %d previews, want the one contract-path call", srv.command.previews)
	}
}

// TestPreviewRequiresASession: the preview is a POST, so the v1 guard's
// session rule applies before it, and the handler's own backstop answers
// the same way.
func TestPreviewRequiresASession(t *testing.T) {
	srv := newDefaultServer(t)

	resp := srv.postAnonymous(t, previewURL(testProjectID), previewRequestBody(t))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous preview = %d, want 401: %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
	if srv.command.previews != 0 {
		t.Errorf("the command ran %d previews for an anonymous request", srv.command.previews)
	}
}

// TestPreviewRunsTheProjectReadGate: the preview is a READ of the
// project's own state, so it is gated the way every project read is — and
// a gate refusal is the existence-hiding 404, whatever the gate said.
func TestPreviewRunsTheProjectReadGate(t *testing.T) {
	srv := newDefaultServer(t)

	resp := srv.post(t, previewURL(testProjectID), previewRequestBody(t))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preview = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
	if srv.gate.calls != 1 || srv.gate.gotID != testProjectID {
		t.Errorf("the gate ran %d times for %q, want once for the path's project", srv.gate.calls, srv.gate.gotID)
	}
	if !srv.gate.got.Authenticated || srv.gate.got.UserID != srv.userID {
		t.Errorf("the gate was handed %+v, want the session's reader", srv.gate.got)
	}

	srv.gate.err = errors.New("not yours to read")
	resp = srv.post(t, previewURL(testProjectID), previewRequestBody(t))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a refused gate = %d, want 404", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != knowledgepublish.CodeProjectNotFound {
		t.Errorf("code = %q, want %q", code, knowledgepublish.CodeProjectNotFound)
	}
	if srv.command.previews != 1 {
		t.Errorf("the command ran after the gate refused (%d previews)", srv.command.previews)
	}
}

// TestPreviewHandsTheCommandTheContractsArguments: the body's
// knowledge_version_ref is the ref spelling specs/mcp/tools.json names, and
// the transport is the only place the object_version: prefix is stripped;
// rights is the caller's document, byte for byte.
func TestPreviewHandsTheCommandTheContractsArguments(t *testing.T) {
	srv := newDefaultServer(t)

	resp := srv.post(t, previewURL(testProjectID), previewRequestBody(t))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preview = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
	if srv.command.gotPreview.ProjectID != testProjectID {
		t.Errorf("project = %q, want the path's project", srv.command.gotPreview.ProjectID)
	}
	if srv.command.gotPreview.ObjectVersionID != testVersionID {
		t.Errorf("object_version = %q, want the ref's uuid (the object_version: prefix is the transport's to strip)", srv.command.gotPreview.ObjectVersionID)
	}
	if got := string(srv.command.gotPreview.Rights); !strings.Contains(got, `"version":1`) {
		t.Errorf("rights = %s, want the caller's document", got)
	}

	// A bare uuid is accepted too: the prefix is a convenience for a reader,
	// not a second identity.
	resp = srv.post(t, previewURL(testProjectID), `{"knowledge_version_ref":"`+testVersionID+`","rights":`+rightsBody(t)+`}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a bare uuid = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
	if srv.command.gotPreview.ObjectVersionID != testVersionID {
		t.Errorf("object_version = %q, want the bare uuid", srv.command.gotPreview.ObjectVersionID)
	}
}

// TestPreviewAnswersWithTheCommandsOwnDocument: the response is the
// command's Preview, field for field. The transport renders it rather than
// re-deciding what a client may know, because a field dropped here would be
// a second implementation of the disclosure rule in the layer with no test
// for it.
func TestPreviewAnswersWithTheCommandsOwnDocument(t *testing.T) {
	srv := newDefaultServer(t)

	resp := srv.post(t, previewURL(testProjectID), previewRequestBody(t))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preview = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	body := decodeJSON(t, resp)

	for _, key := range []string{
		"project_id", "object_version_id", "object_id", "object_type",
		"audience", "project_visibility", "review_approved",
		"approved_review_kinds", "required_review_kinds", "blocking", "publishable",
	} {
		if _, present := body[key]; !present {
			t.Errorf("the preview body has no %q: %v", key, body)
		}
	}
	if body["audience"] != "network" || body["publishable"] != true {
		t.Errorf("audience/publishable = %v/%v, want network/true", body["audience"], body["publishable"])
	}
	// visibility_policy_id is present and null for a version that inherits
	// the project's visibility: a client has to be able to see which axis
	// applied, and "null" is an answer.
	if v, present := body["visibility_policy_id"]; !present || v != nil {
		t.Errorf("visibility_policy_id = %v (present %v), want an explicit null", v, present)
	}
	// A proposal names no name: public_version is the publisher's, and the
	// preview does not take one.
	if v, present := body["public_version"]; present && v != "" {
		t.Errorf("public_version = %v, want absent or empty", v)
	}
}

// TestPreviewAndReadAgreeAboutAnExistingPublication: the preview and the
// public read give the SAME caller the same answer about the same
// publication.
//
// The preview's gate is the PROJECT read, and that gate admits a non-member
// of a public project. The publication a preview reports as existing is an
// object of its OWN, though, with an audience of its own — so a transport
// that rendered it unchecked handed a caller the pid, the name, the
// publisher and the rights of a members-only publication the read route
// answers 404 for. The rule is one rule (mayRead), asked by both routes.
//
// The member case below is the control: a gate that simply never rendered
// Existing would pass the first half and be wrong.
func TestPreviewAndReadAgreeAboutAnExistingPublication(t *testing.T) {
	// The same caller, the same publication: a non-member, and a members-only
	// publication in a public project.
	nonMember := newDefaultServer(t)
	nonMember.read.entry = membersKnowledge()
	nonMember.members.member = false
	existing := knowledgepublish.Published{
		PID:             testPID,
		ObjectVersionID: testVersionID,
		PublicVersion:   "v0.9 — 只有成员",
		RightsJSON:      mustRightsJSON(),
		PublishedBy:     testUserID,
	}
	preview := previewAnswer()
	preview.Existing = &existing
	nonMember.command.preview = preview

	// The read route: 404, and the body names nothing.
	readResp := nonMember.get(t, knowledgeURL(testPID))
	if readResp.StatusCode != http.StatusNotFound {
		t.Fatalf("the read route answered %d for a members-only publication, want 404", readResp.StatusCode)
	}
	readRaw := readBody(t, readResp)

	// The preview: the same answer — nothing about that publication.
	resp := nonMember.post(t, previewURL(testProjectID), previewRequestBody(t))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preview = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	raw := readBody(t, resp)
	if strings.Contains(raw, `"existing_publication"`) {
		t.Errorf("the preview reports an existing publication the caller cannot read: %s", raw)
	}
	for _, secret := range []string{testPID, existing.PublicVersion, testUserID} {
		if strings.Contains(raw, secret) {
			t.Errorf("the preview body discloses %q: %s", secret, raw)
		}
	}
	// The rest of the preview is still there: the facts are about a version
	// in a project this caller WAS admitted to read, and the read route's
	// 404 above is about the publication, not about the version.
	if body := errorEnvelopeOf(t, raw); body["object_version_id"] != testVersionID {
		t.Errorf("the preview dropped the version it is about: %v", body["object_version_id"])
	}
	if !strings.Contains(readRaw, CodeKnowledgeNotFound) {
		t.Errorf("the read route's refusal is not %s: %s", CodeKnowledgeNotFound, readRaw)
	}

	// The control: the same publication, a caller who IS a member. The read
	// route serves it, and the preview reports it — the gate is the
	// membership, not a blanket removal of the field.
	member := newDefaultServer(t)
	member.read.entry = membersKnowledge()
	member.members.member = true
	member.command.preview = preview

	memberRead := member.get(t, knowledgeURL(testPID))
	if memberRead.StatusCode != http.StatusOK {
		t.Fatalf("the member's read = %d, want 200: %s", memberRead.StatusCode, readBody(t, memberRead))
	}
	if got := decodeJSON(t, memberRead); got["audience"] != "members" {
		t.Errorf("the member's read audience = %v, want members", got["audience"])
	}
	resp = member.post(t, previewURL(testProjectID), previewRequestBody(t))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("member preview = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	body := decodeJSON(t, resp)
	got, ok := body["existing_publication"].(map[string]any)
	if !ok {
		t.Fatalf("a member's preview dropped the existing publication: %v", body)
	}
	if got["pid"] != testPID || got["public_version"] != existing.PublicVersion {
		t.Errorf("existing_publication = %v, want the publication the member may read", got)
	}
}

// TestPreviewFailsClosedWhenTheExistingPublicationCannotBeRead: the extra
// read the gate performs is a READ, and a read that failed is not a "yes".
// The preview is answered 503 rather than rendered without the field: a
// silent drop would tell a member "this version was never published" on the
// strength of a store blip.
func TestPreviewFailsClosedWhenTheExistingPublicationCannotBeRead(t *testing.T) {
	srv := newDefaultServer(t)
	preview := previewAnswer()
	preview.Existing = &knowledgepublish.Published{PID: testPID, ObjectVersionID: testVersionID, PublicVersion: "v0.9"}
	srv.command.preview = preview
	srv.read.err = errors.New("connection reset")

	resp := srv.post(t, previewURL(testProjectID), previewRequestBody(t))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("preview = %d, want 503: %s", resp.StatusCode, readBody(t, resp))
	}
	if code := errorCode(t, resp); code != CodeKnowledgeUnavailable {
		t.Errorf("code = %q, want %q", code, CodeKnowledgeUnavailable)
	}
}

// TestPreviewDoesNotReadTheExistingPublicationWhenThereIsNone: the gate is
// about the field, so a preview with no existing publication does not spend
// a read on nothing.
func TestPreviewDoesNotReadTheExistingPublicationWhenThereIsNone(t *testing.T) {
	srv := newDefaultServer(t)

	resp := srv.post(t, previewURL(testProjectID), previewRequestBody(t))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preview = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
	if srv.read.calls != 0 {
		t.Errorf("a preview with no existing publication ran the read %d times", srv.read.calls)
	}
}

// TestPreviewRefusesABodyThatIsNotJSON: a body that is not a JSON object is
// refused before the command runs, with the code the command's own
// validation uses — a client branches on one code for "this body is not a
// publication candidate".
func TestPreviewRefusesABodyThatIsNotJSON(t *testing.T) {
	srv := newDefaultServer(t)

	resp := srv.post(t, previewURL(testProjectID), `not json`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("preview = %d, want 400", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != knowledgepublish.CodeValidationFailed {
		t.Errorf("code = %q, want %q", code, knowledgepublish.CodeValidationFailed)
	}
	if srv.command.previews != 0 {
		t.Errorf("the command ran %d previews for a malformed body", srv.command.previews)
	}
}

// ---------------------------------------------------------------------------
// The publish route

// TestPublishRouteIsTheContractPath.
func TestPublishRouteIsTheContractPath(t *testing.T) {
	srv := newDefaultServer(t)

	resp := srv.post(t, publishURL(testProjectID), publishRequestBody(t, "v1.0"))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("publish = %d, want 201: %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()

	for _, path := range []string{
		"/api/v1/projects/" + testProjectID + "/knowledge/publish",
		"/api/v1/projects/" + testProjectID + "/knowledge:publish/",
		"/api/v1/knowledge:publish",
	} {
		resp := srv.post(t, path, publishRequestBody(t, "v1.0"))
		status := resp.StatusCode
		resp.Body.Close()
		if status == http.StatusCreated {
			t.Errorf("POST %s answered 201: the route must be the contract's path only", path)
		}
	}
	if srv.command.executions != 1 {
		t.Errorf("the command ran %d publishes, want the one contract-path call", srv.command.executions)
	}
}

// TestPublishRequiresASession: publishing is a write, so the guard's
// session rule applies before the handler, and the handler's own backstop
// answers the same way.
func TestPublishRequiresASession(t *testing.T) {
	srv := newDefaultServer(t)

	resp := srv.postAnonymous(t, publishURL(testProjectID), publishRequestBody(t, "v1.0"))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous publish = %d, want 401: %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
	if srv.command.executions != 0 {
		t.Errorf("the command ran %d publishes for an anonymous request", srv.command.executions)
	}
}

// TestPublishDoesNotGateTheProjectRead: the publish route never consults
// the project read gate. The authorization belongs to the command (it is
// not "may the caller read this project" but "may the caller publish
// here"), and a read gate in front of it would answer 404 for a project the
// caller may not see — disclosing by the shape of the refusal which projects
// exist, before the question that decides the write was asked.
func TestPublishDoesNotGateTheProjectRead(t *testing.T) {
	srv := newDefaultServer(t)
	srv.gate.err = errors.New("the gate must not be reached")

	resp := srv.post(t, publishURL(testProjectID), publishRequestBody(t, "v1.0"))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("publish = %d, want the command's answer: %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
	if srv.gate.calls != 0 {
		t.Errorf("the publish route ran the project read gate %d times", srv.gate.calls)
	}
}

// TestPublishCreatesThePublication: the happy path's whole surface — the
// path becomes the command's project, the body becomes its request, and the
// 201 carries the publication's PUBLIC identities.
func TestPublishCreatesThePublication(t *testing.T) {
	srv := newDefaultServer(t)

	const name = "v1.0 — 预印本"
	resp := srv.post(t, publishURL(testProjectID), publishRequestBody(t, name))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("publish = %d, want 201: %s", resp.StatusCode, readBody(t, resp))
	}
	got := decodeJSON(t, resp)

	if got["pid"] != testPID {
		t.Errorf("pid = %v, want the stored publication's pid", got["pid"])
	}
	if got["public_version"] != name {
		t.Errorf("public_version = %v, want the publisher's name %q", got["public_version"], name)
	}
	if got["object_version_id"] != testVersionID {
		t.Errorf("object_version_id = %v, want the published version", got["object_version_id"])
	}
	if got["published_by"] != testUserID {
		t.Errorf("published_by = %v, want the publishing user", got["published_by"])
	}
	if got["published_at"] != "2026-09-18T10:30:00.000Z" {
		t.Errorf("published_at = %v, want the stored instant in UTC", got["published_at"])
	}
	if got["rights"] == nil {
		t.Errorf("the response does not carry the stored rights document: %v", got)
	}
	// The row's internal uuid is not published: a publication is named by
	// its pid and, for a human, by the public_version the publisher chose.
	if _, present := got["id"]; present {
		t.Errorf("the response carries the internal row id: %v", got)
	}

	// What the command was handed.
	if srv.command.gotPublish.ProjectID != testProjectID {
		t.Errorf("project = %q, want the path's project", srv.command.gotPublish.ProjectID)
	}
	if srv.command.gotPublish.ObjectVersionID != testVersionID {
		t.Errorf("version = %q, want the body's ref as a uuid", srv.command.gotPublish.ObjectVersionID)
	}
	if srv.command.gotPublish.PublicVersion != name {
		t.Errorf("public_version = %q, want %q byte for byte", srv.command.gotPublish.PublicVersion, name)
	}
	if srv.command.gotActor.User.ID != srv.userID {
		t.Errorf("actor = %q, want the session's user %q", srv.command.gotActor.User.ID, srv.userID)
	}
	if srv.command.gotActor.IsAgent {
		t.Errorf("the handler marked the caller an agent; a session is not one")
	}
}

// TestPublishTakesTheIdempotencyKeyFromTheHeaderAndNowhereElse: docs/22
// puts the key in the header, so that is the only place it is read from —
// and a body field that spells it is ignored, because a caller who could
// set the key in the body could set it to another caller's.
func TestPublishTakesTheIdempotencyKeyFromTheHeaderAndNowhereElse(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		body    func(t *testing.T) string
		want    *string
	}{
		{"present", map[string]string{"Idempotency-Key": "publish-1"}, nil, ptr("publish-1")},
		{"absent", nil, nil, nil},
		{"empty", map[string]string{"Idempotency-Key": ""}, nil, nil},
		{
			"a body field is not the header",
			nil,
			func(t *testing.T) string {
				t.Helper()
				return strings.TrimSuffix(publishRequestBody(t, "v1.0"), "}") + `,"idempotency_key":"smuggled"}`
			},
			nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newDefaultServer(t)
			body := publishRequestBody(t, "v1.0")
			if c.body != nil {
				body = c.body(t)
			}
			resp := srv.postWithHeaders(t, publishURL(testProjectID), body, c.headers)
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("publish = %d: %s", resp.StatusCode, readBody(t, resp))
			}
			resp.Body.Close()
			got := srv.command.gotPublish.IdempotencyKey
			switch {
			case c.want == nil && got != nil:
				t.Errorf("key = %q, want nil", *got)
			case c.want != nil && got == nil:
				t.Errorf("key = nil, want %q", *c.want)
			case c.want != nil && *got != *c.want:
				t.Errorf("key = %q, want %q", *got, *c.want)
			}
		})
	}
}

// TestPublishAnswersEveryOutcomeWithItsCode: the command's outcomes are a
// closed vocabulary (docs/45: one stable code per outcome, no dependency
// detail), and each one is answered with the status a client acts on — 400
// for "not a candidate", 403 for "not you", 409 for "not in this state",
// 503 for "cannot read".
func TestPublishAnswersEveryOutcomeWithItsCode(t *testing.T) {
	agentRefusal := &knowledgepublish.AgentNotPermittedError{Action: "publish"}
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"validation", knowledgepublish.ErrValidation, http.StatusBadRequest, knowledgepublish.CodeValidationFailed},
		{"agent backstop", agentRefusal, http.StatusForbidden, knowledgepublish.CodeAgentPublishDenied},
		{"forbidden", knowledgepublish.ErrForbidden, http.StatusForbidden, knowledgepublish.CodePublishPrivateToPublicRequiresApproval},
		{"unknown project", knowledgepublish.ErrProjectNotFound, http.StatusNotFound, knowledgepublish.CodeProjectNotFound},
		{"unknown version", knowledgepublish.ErrVersionNotFound, http.StatusNotFound, knowledgepublish.CodeVersionNotFound},
		{"already published", knowledgepublish.ErrAlreadyPublished, http.StatusConflict, knowledgepublish.CodeAlreadyPublished},
		{"review required", knowledgepublish.ErrReviewRequired, http.StatusConflict, knowledgepublish.CodeReviewRequired},
		{"idempotency conflict", knowledgepublish.ErrIdempotencyConflict, http.StatusConflict, knowledgepublish.CodeIdempotencyConflict},
		{"store failure", knowledgepublish.ErrStore, http.StatusServiceUnavailable, knowledgepublish.CodeServiceUnavailable},
		{"unexpected", errors.New("boom"), http.StatusInternalServerError, "INTERNAL_ERROR"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newDefaultServer(t)
			srv.command.err = c.err

			resp := srv.post(t, publishURL(testProjectID), publishRequestBody(t, "v1.0"))
			if resp.StatusCode != c.status {
				t.Fatalf("publish = %d, want %d: %s", resp.StatusCode, c.status, readBody(t, resp))
			}
			if code := errorCode(t, resp); code != c.code {
				t.Errorf("code = %q, want %q", code, c.code)
			}
		})
	}
}

// TestPublishRefusalCarriesTheCompletePreview: a refusal by the server-side
// re-run answers with the WHOLE report, not a sentence about one — the same
// document the preview route answers with, computed over the state the
// publish itself saw. A caller told only "refused" cannot see which entry
// blocked it, nor check that the refusal was about the entry it thinks it
// was.
func TestPublishRefusalCarriesTheCompletePreview(t *testing.T) {
	srv := newDefaultServer(t)
	srv.command.err = &knowledgepublish.PublicationRefused{
		Preview: knowledgepublish.Preview{
			ProjectID:       testProjectID,
			ObjectVersionID: testVersionID,
			Audience:        knowledgepublish.AudienceMembers,
			Publishable:     false,
			Existing: &knowledgepublish.Published{
				PID:             testPID,
				ObjectVersionID: testVersionID,
				PublicVersion:   "v0.9",
			},
			Blocking: []knowledgepublish.Reason{
				{Code: knowledgepublish.ReasonAlreadyPublished, Detail: `already published as "v0.9"`},
				{Code: knowledgepublish.ReasonReviewRequired, Detail: "no approved integrity review"},
			},
		},
		Reasons: []string{`already published as "v0.9"`, "no approved integrity review"},
	}

	resp := srv.post(t, publishURL(testProjectID), publishRequestBody(t, "v1.0"))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("refused publish = %d, want 409: %s", resp.StatusCode, readBody(t, resp))
	}
	body := decodeJSON(t, resp)
	if body["code"] != knowledgepublish.CodePublishBlocked {
		t.Errorf("code = %v, want %q", body["code"], knowledgepublish.CodePublishBlocked)
	}
	preview, ok := body["preview"].(map[string]any)
	if !ok {
		t.Fatalf("the refusal has no preview report: %v", body)
	}
	blocking, ok := preview["blocking"].([]any)
	if !ok || len(blocking) != 2 {
		t.Fatalf("preview.blocking = %v, want both entries", preview["blocking"])
	}
	if preview["publishable"] != false {
		t.Errorf("preview.publishable = %v, want false", preview["publishable"])
	}
	existing, ok := preview["existing_publication"].(map[string]any)
	if !ok || existing["public_version"] != "v0.9" {
		t.Errorf("preview.existing_publication = %v, want the publication that blocked it", preview["existing_publication"])
	}
	if reasons, ok := body["reasons"].([]any); !ok || len(reasons) != 2 {
		t.Errorf("reasons = %v, want both", body["reasons"])
	}
	// The envelope's shared fields are the ones every refusal carries, so a
	// client that branches on `code` first reads one shape everywhere.
	for _, key := range []string{"code", "message", "request_id", "retryable"} {
		if _, present := body[key]; !present {
			t.Errorf("the refusal envelope has no %q: %v", key, body)
		}
	}
}

// TestPublishRefusesAMalformedBodyBeforeTheCommand: the body is decoded
// first, and a body that is not JSON never reaches the command.
func TestPublishRefusesAMalformedBodyBeforeTheCommand(t *testing.T) {
	srv := newDefaultServer(t)

	resp := srv.post(t, publishURL(testProjectID), `{"public_version":`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("publish = %d, want 400", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != knowledgepublish.CodeValidationFailed {
		t.Errorf("code = %q, want %q", code, knowledgepublish.CodeValidationFailed)
	}
	if srv.command.executions != 0 {
		t.Errorf("the command ran %d publishes for a malformed body", srv.command.executions)
	}
}

// TestPublishDoesNotInventAName: a body with no public_version reaches the
// command with an empty one, and the command is what refuses it — the
// server does not derive a name from the version, the object or the clock
// (owner ruling L3-20260916-1 #2).
func TestPublishDoesNotInventAName(t *testing.T) {
	srv := newDefaultServer(t)

	resp := srv.post(t, publishURL(testProjectID), previewRequestBody(t))
	if resp.StatusCode != http.StatusCreated {
		// The fake command accepts anything; what is pinned is what it was
		// HANDED, not what it answered.
		t.Fatalf("publish = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
	if srv.command.gotPublish.PublicVersion != "" {
		t.Errorf("public_version handed to the command = %q, want the body's empty one", srv.command.gotPublish.PublicVersion)
	}
}

func ptr[T any](v T) *T { return &v }
