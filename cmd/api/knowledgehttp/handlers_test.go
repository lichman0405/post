package knowledgehttp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/evidencenetwork"
	"github.com/lichman0405/post/internal/application/knowledgepublish"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/rights"
)

// Task T0805 required test "knowledge publish tests" — the transport half.
//
// The surface is three routes and these tests pin the wiring around them:
// that only the contract's paths exist, that the guard's session rule
// applies to the two writes and not to the read, that the preview runs the
// project read gate and the publish does not, that the Idempotency-Key
// comes off the HEADER (and only from there), that the actor handed to the
// command is the session's user with the agent flag false, that every
// outcome the command can report is answered with a status and a code a
// client can branch on, and that the public read applies the publication's
// own audience rule rather than a rule of its own.
//
// What the routes answer WHEN they can answer is the command's business and
// is pinned in internal/application/knowledgepublish; what is pinned here
// is that the handler hands the command the request's fields — spelled the
// way specs/api/openapi.yaml and specs/mcp/tools.json spell them — and
// renders what comes back.

const (
	testProjectID = "11111111-1111-4111-8111-111111111111"
	testVersionID = "33333333-3333-4333-8333-333333333333"
	testPID       = "01j9z6k3m4n5p6q7r8s9t0v1w2"
	testUserID    = "55555555-5555-4555-8555-555555555555"
)

// fakePublish is the command: what it was handed, and what it answers.
type fakePublish struct {
	preview knowledgepublish.Preview
	err     error
	stored  knowledgepublish.Published

	gotPreview knowledgepublish.PreviewParams
	gotPublish knowledgepublish.PublishParams
	gotActor   knowledgepublish.Actor
	previews   int
	executions int
}

func (f *fakePublish) Preview(_ context.Context, in knowledgepublish.PreviewParams) (knowledgepublish.Preview, error) {
	f.previews++
	f.gotPreview = in
	if f.err != nil {
		return knowledgepublish.Preview{}, f.err
	}
	return f.preview, nil
}

func (f *fakePublish) Publish(_ context.Context, actor knowledgepublish.Actor, in knowledgepublish.PublishParams) (knowledgepublish.Published, error) {
	f.executions++
	f.gotActor, f.gotPublish = actor, in
	if f.err != nil {
		return knowledgepublish.Published{}, f.err
	}
	return f.stored, nil
}

// fakeRead is the public read: a canned publication or failure, recording
// the pid it was asked for.
type fakeRead struct {
	entry knowledgepublish.PublishedKnowledge
	found bool
	err   error

	gotPID string
	calls  int
}

func (f *fakeRead) GetPublishedKnowledge(_ context.Context, pid string) (knowledgepublish.PublishedKnowledge, bool, error) {
	f.calls++
	f.gotPID = pid
	if f.err != nil {
		return knowledgepublish.PublishedKnowledge{}, false, f.err
	}
	return f.entry, f.found, nil
}

// fakeGate is the project read gate: a canned project or error, recording
// the reader and the project it was handed.
type fakeGate struct {
	err   error
	calls int
	got   projects.Reader
	gotID string
}

func (f *fakeGate) Get(_ context.Context, r projects.Reader, projectID string) (domain.Project, error) {
	f.calls++
	f.got, f.gotID = r, projectID
	if f.err != nil {
		return domain.Project{}, f.err
	}
	return domain.Project{ID: projectID}, nil
}

// fakeMembers answers the membership read: a member, a non-member
// (ErrMemberNotFound — "no role", not a failure) or a failure. The three
// answers are fields rather than a derivation so they stay
// distinguishable.
type fakeMembers struct {
	member bool
	err    error
	calls  int
	got    string
}

func (f *fakeMembers) GetMembership(_ context.Context, _ domain.User, projectID string) (domain.ProjectMembership, error) {
	f.calls++
	f.got = projectID
	switch {
	case f.err != nil:
		return domain.ProjectMembership{}, f.err
	case f.member:
		return domain.ProjectMembership{ProjectID: projectID, UserID: testUserID, Role: domain.ProjectRoleViewer}, nil
	default:
		return domain.ProjectMembership{}, projects.ErrMemberNotFound
	}
}

// fakeEvidence is the evidence-network read (T0806): the rows it answers
// with, the truncation flag, and what it was asked for — so a case can pin
// that the read is scoped to the PUBLISHED version and to the project that
// owns it.
type fakeEvidence struct {
	rows      []evidencenetwork.Assertion
	truncated bool
	err       error

	gotVersion string
	gotProject string
	gotLimit   int
	calls      int
}

func (f *fakeEvidence) ListPublishedEvidence(_ context.Context, targetObjectVersionID, targetProjectID string, limit int) ([]evidencenetwork.Assertion, bool, error) {
	f.calls++
	f.gotVersion, f.gotProject, f.gotLimit = targetObjectVersionID, targetProjectID, limit
	if f.err != nil {
		return nil, false, f.err
	}
	return f.rows, f.truncated, nil
}

// server is the composed surface: the real auth guard (so the
// session/CSRF rule is the production one, not a stub) over a mux carrying
// this package's three routes.
type server struct {
	ts      *httptest.Server
	client  *http.Client
	csrf    string
	userID  string
	gate    *fakeGate
	members *fakeMembers
	command *fakePublish
	read    *fakeRead
	network *fakeEvidence
}

// newServer composes the surface and signs up one session, so that every
// case starts from an authenticated caller unless it says otherwise.
func newServer(t *testing.T, deps Deps) *server {
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
			// Every case signs up from the same address, and the real budget
			// (authn.DefaultSignupPerIP) is small on purpose — raise it here
			// so a case tests the route, not the limiter.
			SignupLimitPerIP: 10000,
		},
	})
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	New(deps).Register(mux)
	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Post(ts.URL+"/api/v1/auth/signup", "application/json",
		strings.NewReader(`{"email":"publisher@example.com","password":"long-enough-password-1","handle":"publisher","display_name":"Publisher"}`))
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("signup = %d, want 201", resp.StatusCode)
	}
	var payload struct {
		CSRFToken string `json:"csrf_token"`
		User      struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode signup: %v", err)
	}
	return &server{
		ts: ts, client: client, csrf: payload.CSRFToken, userID: payload.User.ID,
		gate:    gateOf(deps.Projects),
		members: membersOf(deps.Members),
		command: publishOf(deps.Publish),
		read:    readOf(deps.Read),
		network: evidenceOf(deps.Evidence),
	}
}

// gateOf, membersOf, publishOf, readOf and evidenceOf recover the fakes from
// Deps.
func gateOf(g Gate) *fakeGate {
	f, _ := g.(*fakeGate)
	return f
}

func membersOf(m Membership) *fakeMembers {
	f, _ := m.(*fakeMembers)
	return f
}

func publishOf(c PublishCommand) *fakePublish {
	f, _ := c.(*fakePublish)
	return f
}

func readOf(r Read) *fakeRead {
	f, _ := r.(*fakeRead)
	return f
}

func evidenceOf(e Evidence) *fakeEvidence {
	f, _ := e.(*fakeEvidence)
	return f
}

// newDefaultServer wires the five fakes with benign answers: a gate that
// admits, a non-member, a command that answers a publishable preview, a read
// that finds a network-visible publication, and an evidence read that finds
// nothing asserted against it.
func newDefaultServer(t *testing.T) *server {
	t.Helper()
	return newServer(t, Deps{
		Publish:  &fakePublish{preview: previewAnswer(), stored: storedPublication()},
		Read:     &fakeRead{entry: networkKnowledge(), found: true},
		Projects: &fakeGate{},
		Members:  &fakeMembers{},
		Evidence: &fakeEvidence{},
	})
}

// previewURL and publishURL are the routes under test, spelled exactly as
// the contract spells them (specs/api/openapi.yaml:
// /projects/{projectId}/knowledge:publish-preview and
// /projects/{projectId}/knowledge:publish).
func previewURL(projectID string) string {
	return "/api/v1/projects/" + projectID + "/knowledge:publish-preview"
}

func publishURL(projectID string) string {
	return "/api/v1/projects/" + projectID + "/knowledge:publish"
}

// knowledgeURL is the public read (specs/api/openapi.yaml:
// /knowledge/{knowledgeId}, security: []).
func knowledgeURL(pid string) string {
	return "/api/v1/knowledge/" + pid
}

// post sends the body as the web app sends it (JSON content type, CSRF
// token attached) and returns the response.
func (s *server) post(t *testing.T, url, body string) *http.Response {
	t.Helper()
	return s.postWithHeaders(t, url, body, nil)
}

// postWithHeaders sends the body with the extra headers a case needs.
func (s *server) postWithHeaders(t *testing.T, url, body string, headers map[string]string) *http.Response {
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

// postAnonymous sends the body with no session at all.
func (s *server) postAnonymous(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(s.ts.URL+url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("anonymous POST %s: %v", url, err)
	}
	return resp
}

// get reads a route as the session's user.
func (s *server) get(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := s.client.Get(s.ts.URL + url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

// getAnonymous reads a route with no session.
func (s *server) getAnonymous(t *testing.T, url string) *http.Response {
	t.Helper()
	anon := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := anon.Get(s.ts.URL + url)
	if err != nil {
		t.Fatalf("anonymous GET %s: %v", url, err)
	}
	return resp
}

// rightsBody is a rights document as the request body carries it: the
// canonical bytes of the fail-closed document (metadata = project_policy,
// data_access = restricted).
func rightsBody(t *testing.T) string {
	t.Helper()
	b, err := rights.New().Marshal()
	if err != nil {
		t.Fatalf("marshal rights: %v", err)
	}
	return string(b)
}

// previewRequestBody is the document specs/mcp/tools.json gives
// knowledge.publish_preview: knowledge_version_ref and rights, and nothing
// else.
func previewRequestBody(t *testing.T) string {
	t.Helper()
	return `{"knowledge_version_ref":"object_version:` + testVersionID + `","rights":` + rightsBody(t) + `}`
}

// publishRequestBody is the preview document plus the one field publishing
// adds.
func publishRequestBody(t *testing.T, publicVersion string) string {
	t.Helper()
	body := previewRequestBody(t)
	name, err := json.Marshal(publicVersion)
	if err != nil {
		t.Fatalf("marshal public_version: %v", err)
	}
	return strings.TrimSuffix(body, "}") + `,"public_version":` + string(name) + `}`
}

// previewAnswer is what the fake command answers with on a preview.
func previewAnswer() knowledgepublish.Preview {
	return knowledgepublish.Preview{
		ProjectID:           testProjectID,
		ObjectVersionID:     testVersionID,
		ObjectID:            "77777777-7777-4777-8777-777777777777",
		ObjectType:          "finding",
		Audience:            knowledgepublish.AudienceNetwork,
		ProjectVisibility:   "public",
		ReviewApproved:      true,
		ApprovedReviewKinds: []string{"scientific", "integrity"},
		RequiredReviewKinds: []string{"scientific", "integrity"},
		Blocking:            []knowledgepublish.Reason{},
		Publishable:         true,
	}
}

// storedPublication is what the fake command answers with on a publish.
func storedPublication() knowledgepublish.Published {
	return knowledgepublish.Published{
		ID:              "88888888-8888-4888-8888-888888888888",
		PID:             testPID,
		ObjectVersionID: testVersionID,
		PublicVersion:   "v1.0 — 预印本",
		RightsJSON:      mustRightsJSON(),
		PublishedBy:     testUserID,
		PublishedAt:     time.Date(2026, 9, 18, 10, 30, 0, 0, time.UTC),
	}
}

// mustRightsJSON is the stored document of storedPublication.
func mustRightsJSON() json.RawMessage {
	b, err := rights.New().Marshal()
	if err != nil {
		panic(err)
	}
	return b
}

// networkKnowledge is a publication the network may read.
func networkKnowledge() knowledgepublish.PublishedKnowledge {
	return knowledgepublish.PublishedKnowledge{
		PID:               testPID,
		PublicVersion:     "v1.0 — 预印本",
		PublishedBy:       testUserID,
		PublishedAt:       time.Date(2026, 9, 18, 10, 30, 0, 0, time.UTC),
		Rights:            rights.New(),
		RightsValid:       true,
		ObjectVersionID:   testVersionID,
		ObjectID:          "77777777-7777-4777-8777-777777777777",
		ObjectType:        "finding",
		Title:             "A reproducible finding",
		LifecycleState:    "published",
		SchemaID:          "post.finding",
		SchemaVersion:     "1",
		IntegrityHash:     "sha256:abc",
		ProjectID:         testProjectID,
		ProjectVisibility: "public",
	}
}

// membersKnowledge is the same publication with the version's own
// visibility policy pinned — a version in a PUBLIC project that the
// network may not read (owner ruling L3-20260916-1 #1).
func membersKnowledge() knowledgepublish.PublishedKnowledge {
	k := networkKnowledge()
	pinned := "9f0f0e0d-0000-4000-8000-000000000001"
	k.VisibilityPolicyID = &pinned
	return k
}

// ---------------------------------------------------------------------------
// Shared assertions

// readBody reads a response body, failing loudly rather than silently
// returning "".
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// decodeJSON reads a response body into a map, failing loudly on a body
// that is not JSON.
func decodeJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}

// errorEnvelope is a refusal body: the code and the message.
func errorEnvelope(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	return decodeJSON(t, resp)
}

// errorCode is the code of a refusal.
func errorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	code, _ := errorEnvelope(t, resp)["code"].(string)
	return code
}
