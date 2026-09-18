package knowledgehttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/knowledgepublish"
	"github.com/lichman0405/post/internal/application/projects"
)

// GET /api/v1/knowledge/{knowledgeId} — the public read. specs/api/
// openapi.yaml spells it `security: []`, so an anonymous caller reaches it
// and the AUDIENCE RULE decides what that caller may see.
//
// What is pinned here is the shape of that decision at the transport: the
// pid is checked before anything is read, the audience is the
// publication's own (knowledgepublish.AudienceFor, through
// PublishedKnowledge.Audience), a members-only publication is a 404 for
// everyone who is not a member, and every refusal is the SAME 404 — an
// unknown pid, a segment that is not a pid, and a publication this caller
// may not see must be indistinguishable, or the route becomes a directory
// of private publications.

// TestKnowledgeReadIsReachableWithoutASession: the contract says
// `security: []`, and the v1 guard lets a read through unauthenticated.
func TestKnowledgeReadIsReachableWithoutASession(t *testing.T) {
	srv := newDefaultServer(t)

	resp := srv.getAnonymous(t, knowledgeURL(testPID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous read = %d, want 200: %s", resp.StatusCode, readBody(t, resp))
	}
	body := decodeJSON(t, resp)
	if body["audience"] != "network" {
		t.Errorf("audience = %v, want network", body["audience"])
	}
	if srv.read.gotPID != testPID {
		t.Errorf("the store was asked for %q, want the path's pid", srv.read.gotPID)
	}
}

// TestKnowledgeReadRejectsASegmentThatIsNotAPidWithoutReading: the path
// segment is the publication's pid, so a segment that is not one names no
// publication by construction. It is answered before the store is touched —
// a slug-shaped segment cannot resolve here, and answering it with a read
// would make the route an oracle for what slugs exist.
func TestKnowledgeReadRejectsASegmentThatIsNotAPidWithoutReading(t *testing.T) {
	cases := []struct {
		name string
		id   string
	}{
		{"a name", "v1.0"},
		{"uppercase", strings.ToUpper(testPID)},
		{"one character outside the alphabet", testPID[:25] + "i"},
		{"too short", testPID[:25]},
		{"too long", testPID + "2"},
		{"a uuid", testVersionID},
	}
	srv := newDefaultServer(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := srv.getAnonymous(t, knowledgeURL(c.id))
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("read of %q = %d, want 404", c.id, resp.StatusCode)
			}
			if code := errorCode(t, resp); code != CodeKnowledgeNotFound {
				t.Errorf("code = %q, want %q", code, CodeKnowledgeNotFound)
			}
		})
	}
	if srv.read.calls != 0 {
		t.Errorf("the store was read %d times for segments that are not pids", srv.read.calls)
	}
}

// TestKnowledgeReadWithholdsAMembersOnlyPublication is the headline case of
// owner ruling L3-20260916-1 #1: the publication's version pins a
// visibility policy of its own, in a PUBLIC project — and publishing it must
// not have made it readable by the network. The transport asks the
// publication's own audience rule, which reads the version's axis first.
//
// A non-member is answered exactly what an unknown pid is answered, and the
// body carries none of the publication's content: not the title, not the
// version, not the pid.
func TestKnowledgeReadWithholdsAMembersOnlyPublication(t *testing.T) {
	entry := membersKnowledge()
	if entry.ProjectVisibility != "public" {
		t.Fatalf("the fixture must be a version in a PUBLIC project, got %q", entry.ProjectVisibility)
	}
	srv := newDefaultServer(t)
	srv.read.entry = entry

	// Anonymous.
	resp := srv.getAnonymous(t, knowledgeURL(testPID))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("anonymous read of a members-only publication = %d, want 404", resp.StatusCode)
	}
	raw := readBody(t, resp)
	if !strings.Contains(raw, CodeKnowledgeNotFound) {
		t.Errorf("body = %s, want %s", raw, CodeKnowledgeNotFound)
	}
	for _, secret := range []string{entry.Title, entry.PublicVersion, entry.ObjectVersionID, entry.ObjectID, entry.IntegrityHash} {
		if strings.Contains(raw, secret) {
			t.Errorf("the refusal body discloses %q: %s", secret, raw)
		}
	}

	// An authenticated caller who is not a member of the owning project.
	srv.members.member = false
	resp = srv.get(t, knowledgeURL(testPID))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a non-member read = %d, want 404", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != CodeKnowledgeNotFound {
		t.Errorf("code = %q, want %q", code, CodeKnowledgeNotFound)
	}
	if srv.members.got != testProjectID {
		t.Errorf("the membership was read for %q, want the owning project", srv.members.got)
	}
}

// TestKnowledgeReadServesAMembersOnlyPublicationToItsOwnProject: the same
// publication, read by a member. A publication from a private project is
// legal (docs/12 §2) and its members read it here — the route is not a
// public-only route, it is one whose audience rule decides.
func TestKnowledgeReadServesAMembersOnlyPublicationToItsOwnProject(t *testing.T) {
	srv := newDefaultServer(t)
	srv.read.entry = membersKnowledge()
	srv.members.member = true

	resp := srv.get(t, knowledgeURL(testPID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a member read = %d, want 200: %s", resp.StatusCode, readBody(t, resp))
	}
	body := decodeJSON(t, resp)
	if body["audience"] != "members" {
		t.Errorf("audience = %v, want members — the answer to \"is this public?\" is stated, not left to the client", body["audience"])
	}
	version, ok := body["version"].(map[string]any)
	if !ok || version["title"] != "A reproducible finding" {
		t.Errorf("version = %v, want the published version", body["version"])
	}
}

// TestKnowledgeReadDoesNotConsultMembershipForANetworkPublication: the
// membership read happens only where the audience rule says it is needed.
// A network-readable publication is served without one, so the route does
// not turn every public citation into a membership lookup (and does not
// fail closed on a membership store blip it never needed).
func TestKnowledgeReadDoesNotConsultMembershipForANetworkPublication(t *testing.T) {
	srv := newDefaultServer(t)
	srv.members.err = errors.New("the membership store must not be reached")

	resp := srv.getAnonymous(t, knowledgeURL(testPID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read = %d, want 200: %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
	if srv.members.calls != 0 {
		t.Errorf("a network-readable publication ran the membership read %d times", srv.members.calls)
	}
}

// TestKnowledgeReadAnswersAnUnknownPIDExactlyAsAWithheldOne: the two 404s
// must be the same answer. If a caller could tell "no such pid" from "this
// pid exists and you may not see it", the route would enumerate the
// repository's publications.
func TestKnowledgeReadAnswersAnUnknownPIDExactlyAsAWithheldOne(t *testing.T) {
	unknown := newDefaultServer(t)
	unknown.read.found = false
	resp := unknown.getAnonymous(t, knowledgeURL(testPID))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown pid = %d, want 404", resp.StatusCode)
	}
	unknownBody := readBody(t, resp)

	withheld := newDefaultServer(t)
	withheld.read.entry = membersKnowledge()
	resp = withheld.getAnonymous(t, knowledgeURL(testPID))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("withheld pid = %d, want 404", resp.StatusCode)
	}
	withheldBody := readBody(t, resp)

	if unknownBody != withheldBody {
		t.Errorf("the two 404s differ:\n  unknown:  %s\n  withheld: %s", unknownBody, withheldBody)
	}
}

// TestKnowledgeReadFailsClosedOnAStoreFailure: a read that could not be
// completed is not a "not found" and not a "yes". The page is a statement
// about the repository as it was read, so a failed read answers 503 — for
// the publication read and for the membership read alike.
func TestKnowledgeReadFailsClosedOnAStoreFailure(t *testing.T) {
	t.Run("the publication read fails", func(t *testing.T) {
		srv := newDefaultServer(t)
		srv.read.err = errors.New("connection reset")

		resp := srv.getAnonymous(t, knowledgeURL(testPID))
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("read = %d, want 503", resp.StatusCode)
		}
		if code := errorCode(t, resp); code != CodeKnowledgeUnavailable {
			t.Errorf("code = %q, want %q", code, CodeKnowledgeUnavailable)
		}
	})

	t.Run("the membership read fails on a members-only publication", func(t *testing.T) {
		srv := newDefaultServer(t)
		srv.read.entry = membersKnowledge()
		srv.members.err = errors.New("connection reset")

		resp := srv.get(t, knowledgeURL(testPID))
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("read = %d, want 503", resp.StatusCode)
		}
		if code := errorCode(t, resp); code != CodeKnowledgeUnavailable {
			t.Errorf("code = %q, want %q", code, CodeKnowledgeUnavailable)
		}
	})

	t.Run("an unknown membership is not a failure", func(t *testing.T) {
		// projects.ErrMemberNotFound is "no role", not "the store is down":
		// a non-member of a members-only publication gets the 404.
		srv := newDefaultServer(t)
		srv.read.entry = membersKnowledge()
		srv.members.member = false

		resp := srv.get(t, knowledgeURL(testPID))
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("read = %d, want 404: %s", resp.StatusCode, readBody(t, resp))
		}
	})

	t.Run("an unreadable project is not a failure", func(t *testing.T) {
		// GetMembership resolves the project through the project READ GATE
		// before it reads a membership, so a project the caller may not read
		// comes back as projects.ErrProjectNotFound — the project surface's
		// deliberate existence hiding, and the answer a private project's
		// non-member gets. It is the same "no" as ErrMemberNotFound, and
		// leaving it to the failure branch made this route an existence
		// oracle: 503 "temporarily unavailable, retryable" for a pid that
		// names a real members-only publication, 404 for one that names
		// nothing.
		srv := newDefaultServer(t)
		srv.read.entry = membersKnowledge()
		srv.members.err = projects.ErrProjectNotFound

		resp := srv.get(t, knowledgeURL(testPID))
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("a non-member of an unreadable project was answered %d, want 404: %s",
				resp.StatusCode, readBody(t, resp))
		}
		body := readBody(t, resp)
		if code := errorEnvelopeOf(t, body)["code"]; code != CodeKnowledgeNotFound {
			t.Errorf("code = %v, want %q", code, CodeKnowledgeNotFound)
		}
		// Byte for byte what an id that names nothing at all is answered:
		// the caller must not be able to tell the two apart.
		unknown := newDefaultServer(t)
		unknown.read.found = false
		unknownResp := unknown.get(t, knowledgeURL(testPID))
		if unknownResp.StatusCode != http.StatusNotFound {
			t.Fatalf("the unknown-pid control = %d, want 404", unknownResp.StatusCode)
		}
		if unknownBody := readBody(t, unknownResp); body != unknownBody {
			t.Errorf("an unreadable project and an unknown pid answer different bodies:\nunreadable: %s\nunknown:    %s",
				body, unknownBody)
		}
	})
}

// errorEnvelopeOf decodes a refusal body already read out of the response —
// the twin of errorEnvelope for the cases that need the bytes themselves
// (to compare two refusals, or to assert on what they must NOT contain).
func errorEnvelopeOf(t *testing.T, body string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("the refusal body is not the JSON envelope: %v (%q)", err, body)
	}
	return out
}

// TestKnowledgeReadRendersTheStoredFactsAndNoMore: the body is the
// publication and the version it published — the pid the caller addressed,
// the publisher's name, the rights document AS STORED, the audience, and the
// version's integrity facts. The publication's own row uuid is not on the
// wire: the pid is the identity a client cites.
func TestKnowledgeReadRendersTheStoredFactsAndNoMore(t *testing.T) {
	srv := newDefaultServer(t)
	resp := srv.getAnonymous(t, knowledgeURL(testPID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	body := decodeJSON(t, resp)

	if body["pid"] != testPID {
		t.Errorf("pid = %v, want the addressed pid", body["pid"])
	}
	if body["public_version"] != "v1.0 — 预印本" {
		t.Errorf("public_version = %v, want the publisher's name verbatim", body["public_version"])
	}
	rights, ok := body["rights"].(map[string]any)
	if !ok {
		t.Fatalf("rights = %v, want the stored document", body["rights"])
	}
	if _, present := rights["visibility"]; !present {
		t.Errorf("the rights document was re-rendered: %v", rights)
	}
	version, ok := body["version"].(map[string]any)
	if !ok {
		t.Fatalf("version = %v, want the version facts", body["version"])
	}
	for _, key := range []string{"id", "title", "lifecycle_state", "schema_id", "schema_version", "integrity_hash"} {
		if _, present := version[key]; !present {
			t.Errorf("version has no %q: %v", key, version)
		}
	}
	object, ok := body["object"].(map[string]any)
	if !ok || object["object_type"] != "finding" {
		t.Errorf("object = %v, want the published object", body["object"])
	}
	project, ok := body["project"].(map[string]any)
	if !ok || project["id"] != testProjectID || project["visibility"] != "public" {
		t.Errorf("project = %v, want the owning project's id and preset", body["project"])
	}
}

// TestKnowledgeReadRendersAnUnreadableRightsDocumentAsNull: a document this
// build cannot read is reported as JSON null rather than as an empty object
// — "the stored declaration is not one this build can render", which no
// reader should mistake for "no rights reserved". The branch is only
// reachable on a members-only read: the audience rule has already refused a
// publication whose declaration does not say project_policy.
func TestKnowledgeReadRendersAnUnreadableRightsDocumentAsNull(t *testing.T) {
	srv := newDefaultServer(t)
	entry := membersKnowledge()
	entry.RightsValid = false
	srv.read.entry = entry
	srv.members.member = true

	resp := srv.get(t, knowledgeURL(testPID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	raw := readBody(t, resp)
	if !strings.Contains(raw, `"rights":null`) {
		t.Errorf("body = %s, want the rights document reported as null", raw)
	}
}

// TestKnowledgeReadIsNotBehindAProjectSegment: the identity a client holds
// is the publication's pid, and the route is the contract's — the same
// publication is not readable under a project-nested path (there is no such
// route), so a client cannot be asked to know the project to cite a
// publication.
func TestKnowledgeReadIsNotBehindAProjectSegment(t *testing.T) {
	srv := newDefaultServer(t)

	resp := srv.getAnonymous(t, "/api/v1/projects/"+testProjectID+"/knowledge/"+testPID)
	status := resp.StatusCode
	resp.Body.Close()
	if status == http.StatusOK {
		t.Errorf("a project-nested knowledge path answered 200: the route is the contract's /knowledge/{knowledgeId}")
	}
}

// TestKnowledgeReadDoesNotRequireTheVersionToBeUnpublishedTwice: the read
// answers the publication the pid names and nothing else — it does not
// consult the write command at all. The two surfaces share the AUDIENCE
// rule (knowledgepublish.AudienceFor) and nothing more, so a read cannot
// accidentally become a publish or vice versa.
func TestKnowledgeReadDoesNotTouchThePublishCommand(t *testing.T) {
	srv := newDefaultServer(t)

	resp := srv.getAnonymous(t, knowledgeURL(testPID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
	if srv.command.previews != 0 || srv.command.executions != 0 {
		t.Errorf("the read ran the publish command (%d previews, %d publishes)", srv.command.previews, srv.command.executions)
	}
	if !knowledgepublish.ValidPID(testPID) {
		t.Fatalf("the fixture pid is not a pid")
	}
}
