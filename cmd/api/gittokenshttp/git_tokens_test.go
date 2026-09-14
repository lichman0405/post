package gittokenshttp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/gitprovider"
	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/persistence/memstore"
)

// The git-token surface's wire mapping, tested at the handler level
// through the REAL guard: a real signup issues the session, the real
// projects service (stub store, permissive engine) resolves membership,
// and a real UserAccess service (stub port + stub store) runs the policy.
// The assertions are about the handler's decisions — the disabled 503,
// the role→scope projection, the error codes — while the service and
// adapter policies have their own suites.

const testProjectID = "11111111-2222-4333-8444-555555555555"

// stubProjectStore answers one project with a configurable membership:
// member=true answers the given role for any caller; member=false answers
// "no membership".
type stubProjectStore struct {
	member bool
	role   domain.ProjectRole
}

func (s *stubProjectStore) GetProject(_ context.Context, projectID string) (domain.Project, error) {
	if projectID != testProjectID {
		return domain.Project{}, projects.ErrProjectNotFound
	}
	return domain.Project{
		ID: testProjectID, Slug: "lab", Name: "Lab",
		Visibility: domain.VisibilityPrivate,
	}, nil
}

func (s *stubProjectStore) GetMembership(_ context.Context, projectID, userID string) (domain.ProjectMembership, error) {
	if !s.member || projectID != testProjectID {
		return domain.ProjectMembership{}, projects.ErrMemberNotFound
	}
	return domain.ProjectMembership{
		ProjectID: projectID, UserID: userID, Role: s.role,
		CreatedAt: time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC),
	}, nil
}

func (s *stubProjectStore) CreateProject(context.Context, domain.Project, string) (domain.Project, domain.ProjectMembership, error) {
	return domain.Project{}, domain.ProjectMembership{}, projects.ErrStore
}

func (s *stubProjectStore) ListProjectMembers(context.Context, string) ([]domain.ProjectMember, error) {
	return nil, projects.ErrStore
}

func (s *stubProjectStore) UpdateMembershipRole(context.Context, string, string, domain.ProjectRole, domain.AuditEntry) (domain.ProjectMembership, error) {
	return domain.ProjectMembership{}, projects.ErrStore
}

func (s *stubProjectStore) UpdateProjectSettings(context.Context, string, *string, *string, domain.AuditEntry) (domain.Project, error) {
	return domain.Project{}, projects.ErrStore
}

func (s *stubProjectStore) ListProjectsForUser(context.Context, string) ([]domain.Project, error) {
	return nil, projects.ErrStore
}

func (s *stubProjectStore) ListPublicProjects(context.Context) ([]domain.Project, error) {
	return nil, projects.ErrStore
}

func (s *stubProjectStore) GetProgram(context.Context, string) (domain.Program, error) {
	return domain.Program{}, projects.ErrProgramNotFound
}

// stubEngine allows every request — the matrix is covered elsewhere; the
// handler's role projection is what this suite exercises.
type stubEngine struct{}

func (stubEngine) Authorize(context.Context, authz.Request) (authz.Decision, error) {
	return authz.Decision{Verdict: authz.VerdictAllow}, nil
}

// stubOrgGate satisfies projects.OrgGate (unused by these routes).
type stubOrgGate struct{}

func (stubOrgGate) GetOrganization(context.Context, string) (domain.Organization, error) {
	return domain.Organization{}, projects.ErrOrgNotFound
}

func (stubOrgGate) GetMembership(context.Context, string, string) (domain.OrganizationMembership, error) {
	return domain.OrganizationMembership{}, projects.ErrMemberNotFound
}

// stubUserPort implements gitprovider.UserAccessPort in memory: every
// provider call is recorded, mints hand back fresh credentials.
type stubUserPort struct {
	grants  []string // "repo user level"
	mints   []gitprovider.UserTokenSpec
	deletes []string // "user id"
	revokes []string // "repo user"
	nextID  int64
}

func (p *stubUserPort) EnsureGitUser(_ context.Context, platformUserID string) (gitprovider.GitUser, error) {
	return gitprovider.GitUser{Name: "u-" + platformUserID}, nil
}

func (p *stubUserPort) GrantAccess(_ context.Context, repo gitprovider.Repository, user gitprovider.GitUser, level gitprovider.AccessLevel) error {
	p.grants = append(p.grants, repo.Owner+"/"+repo.Name+" "+user.Name+" "+string(level))
	return nil
}

func (p *stubUserPort) RevokeAccess(_ context.Context, repo gitprovider.Repository, user gitprovider.GitUser) error {
	p.revokes = append(p.revokes, repo.Owner+"/"+repo.Name+" "+user.Name)
	return nil
}

func (p *stubUserPort) CreateUserToken(_ context.Context, user gitprovider.GitUser, spec gitprovider.UserTokenSpec) (gitprovider.UserToken, error) {
	p.nextID++
	p.mints = append(p.mints, spec)
	return gitprovider.UserToken{ID: p.nextID, Name: spec.Name, Value: fmt.Sprintf("cred-%d", p.nextID)}, nil
}

func (p *stubUserPort) DeleteUserToken(_ context.Context, user gitprovider.GitUser, tokenID int64) error {
	p.deletes = append(p.deletes, user.Name+" "+fmt.Sprint(tokenID))
	return nil
}

// stubAccessStore implements gitprovider.AccessStore in memory. The
// canonical token rows are mutable copies so revocations are observable.
type stubAccessStore struct {
	repoRef     gitprovider.RepoRef
	repoRefErr  error
	access      gitprovider.AccessRecord
	accessErr   error
	rows        map[string]gitprovider.TokenRecord
	nextRow     int
	upserts     []string
	accessMarks []string
}

func newStubAccessStore() *stubAccessStore {
	return &stubAccessStore{
		repoRef: gitprovider.RepoRef{
			ProjectID: testProjectID, Owner: "post-git-svc", Name: "p-" + testProjectID,
		},
		rows: map[string]gitprovider.TokenRecord{},
	}
}

func (s *stubAccessStore) RepoRef(_ context.Context, projectID string) (gitprovider.RepoRef, error) {
	if s.repoRefErr != nil {
		return gitprovider.RepoRef{}, s.repoRefErr
	}
	return s.repoRef, nil
}

func (s *stubAccessStore) UpsertGitIdentity(_ context.Context, userID, giteaUsername string) error {
	s.upserts = append(s.upserts, "identity "+userID+" "+giteaUsername)
	return nil
}

func (s *stubAccessStore) GetAccess(context.Context, string, string) (gitprovider.AccessRecord, error) {
	if s.accessErr != nil {
		return gitprovider.AccessRecord{}, s.accessErr
	}
	return s.access, nil
}

func (s *stubAccessStore) UpsertAccess(_ context.Context, rec gitprovider.AccessRecord) error {
	s.upserts = append(s.upserts, rec.ProjectID+" "+rec.UserID+" "+string(rec.Permission))
	s.access = rec
	return nil
}

func (s *stubAccessStore) MarkAccessRevoked(_ context.Context, projectID, userID string) error {
	s.accessMarks = append(s.accessMarks, projectID+" "+userID)
	now := time.Now()
	s.access.RevokedAt = &now
	return nil
}

func (s *stubAccessStore) InsertToken(_ context.Context, rec gitprovider.TokenRecord) (gitprovider.TokenRecord, error) {
	s.nextRow++
	rec.ID = fmt.Sprintf("00000000-0000-4000-8000-%012d", s.nextRow)
	s.rows[rec.ID] = rec
	return rec, nil
}

func (s *stubAccessStore) GetToken(_ context.Context, tokenID string) (gitprovider.TokenRecord, error) {
	rec, ok := s.rows[tokenID]
	if !ok {
		return gitprovider.TokenRecord{}, gitprovider.ErrTokenNotFound
	}
	return rec, nil
}

func (s *stubAccessStore) ListTokens(_ context.Context, projectID, userID string) ([]gitprovider.TokenRecord, error) {
	var out []gitprovider.TokenRecord
	for _, rec := range s.rows {
		if rec.ProjectID == projectID && rec.UserID == userID {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (s *stubAccessStore) MarkTokenRevoked(_ context.Context, tokenID string) error {
	rec, ok := s.rows[tokenID]
	if !ok {
		return nil
	}
	rec.Status = "revoked"
	now := time.Now()
	rec.RevokedAt = &now
	s.rows[tokenID] = rec
	return nil
}

// recordingAudit captures the audit entries the handlers write.
type recordingAudit struct {
	entries []domain.AuditEntry
}

func (a *recordingAudit) Record(_ context.Context, e domain.AuditEntry) error {
	a.entries = append(a.entries, e)
	return nil
}

// testServer composes the guarded surface: real auth, real projects
// service (stub store), real UserAccess service (stub port + store). It
// returns the server, an authenticated client and the signup's CSRF token.
type testServer struct {
	ts     *httptest.Server
	client *http.Client
	csrf   string
	userID string
	port   *stubUserPort
	store  *stubAccessStore
	audit  *recordingAudit
}

func newTestServer(t *testing.T, st projects.ProjectStore, access *gitprovider.UserAccess, missing []string) *testServer {
	t.Helper()
	authAPI := authhttp.New(authhttp.Deps{
		Users:      memstore.NewUsers(),
		Sessions:   memstore.NewSessions(),
		Limiter:    memstore.NewLimiter(),
		OIDCClient: nil,
		Cfg: authn.Config{
			WebOrigin:          "http://web.test",
			SessionTTL:         time.Hour,
			LoginLimitPerEmail: 1000,
			LoginLimitPerIP:    10000,
			LoginWindow:        time.Minute,
			SignupLimitPerIP:   10000,
		},
		Secure: false,
	})
	audit := &recordingAudit{}
	api := New(Deps{
		Projects: projects.NewService(st, &stubOrgGate{}, stubEngine{}),
		Access:   access,
		Audit:    audit,
		Missing:  missing,
	})
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	api.Register(mux)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(observability.Middleware(logger)(authAPI.Guard(mux)))
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Post(ts.URL+"/api/v1/auth/signup", "application/json",
		strings.NewReader(`{"email":"git-tokens@example.com","password":"long-enough-password-1","handle":"git-tokens","display_name":"Git Tokens"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("signup = %d (body %s)", resp.StatusCode, readBody(resp))
	}
	var payload struct {
		CSRFToken string `json:"csrf_token"`
		User      struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	return &testServer{ts: ts, client: client, csrf: payload.CSRFToken, userID: payload.User.ID, audit: audit}
}

// wired builds a test server whose access service is the real one over the
// in-memory port and store.
func wired(t *testing.T, st projects.ProjectStore, configure func(*stubAccessStore)) *testServer {
	t.Helper()
	store := newStubAccessStore()
	if configure != nil {
		configure(store)
	}
	port := &stubUserPort{}
	srv := newTestServer(t, st,
		gitprovider.NewUserAccess(port, store, "http://127.0.0.1:3000"), nil)
	srv.port = port
	srv.store = store
	return srv
}

func readBody(resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// request performs one request with the session cookie and CSRF header.
func (s *testServer) request(t *testing.T, method, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, s.ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet {
		req.Header.Set("X-CSRF-Token", s.csrf)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (s *testServer) issue(t *testing.T) (string, map[string]any) {
	t.Helper()
	resp := s.request(t, http.MethodPost, "/api/v1/projects/"+testProjectID+"/git-tokens")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("issue = %d (body %s)", resp.StatusCode, readBody(resp))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	id, _ := payload["id"].(string)
	if id == "" {
		t.Fatalf("issue payload has no id: %v", payload)
	}
	return id, payload
}

// TestLevelForRole: the matrix projection — viewer reads, contributor and
// above write.
func TestLevelForRole(t *testing.T) {
	for role, want := range map[domain.ProjectRole]gitprovider.AccessLevel{
		domain.ProjectRoleViewer:      gitprovider.AccessRead,
		domain.ProjectRoleContributor: gitprovider.AccessWrite,
		domain.ProjectRoleMaintainer:  gitprovider.AccessWrite,
		domain.ProjectRoleOwner:       gitprovider.AccessWrite,
	} {
		if got := levelForRole(role); got != want {
			t.Errorf("levelForRole(%s) = %s, want %s", role, got, want)
		}
	}
}

// TestIssueDisabledAnswers503: without the access service the route
// answers 503 naming the missing keys — never a misleading 404.
func TestIssueDisabledAnswers503(t *testing.T) {
	srv := newTestServer(t, &stubProjectStore{member: true, role: domain.ProjectRoleOwner},
		nil, []string{"POST_GITEA_ADMIN_PASSWORD"})

	resp := srv.request(t, http.MethodPost, "/api/v1/projects/"+testProjectID+"/git-tokens")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	var envelope struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Code != codeDisabled {
		t.Errorf("code = %q, want %q", envelope.Code, codeDisabled)
	}
}

// TestIssueOwnerGetsWriteToken: the owner's issue succeeds with a write
// scope, a one-time credential and a ready clone URL.
func TestIssueOwnerGetsWriteToken(t *testing.T) {
	srv := wired(t, &stubProjectStore{member: true, role: domain.ProjectRoleOwner}, nil)

	id, payload := srv.issue(t)
	if payload["scope"] != "write" {
		t.Errorf("scope = %v, want write", payload["scope"])
	}
	if tok, _ := payload["token"].(string); tok == "" {
		t.Error("payload must carry the one-time token value")
	}
	if !strings.HasPrefix(fmt.Sprint(payload["name"]), "post-"+testProjectID[:8]+"-") {
		t.Errorf("name = %v, want the post-<project prefix>-<entropy> shape", payload["name"])
	}
	wantClone := "http://u-" + srv.userID + ":cred-1@127.0.0.1:3000/post-git-svc/p-" + testProjectID + ".git"
	if payload["clone_url"] != wantClone {
		t.Errorf("clone_url = %v, want %v", payload["clone_url"], wantClone)
	}
	if payload["id"] != id {
		t.Errorf("id = %v", payload["id"])
	}

	if len(srv.port.mints) != 1 || srv.port.mints[0].Level != gitprovider.AccessWrite {
		t.Errorf("mints = %+v, want one write-scope mint", srv.port.mints)
	}
	if len(srv.port.grants) != 1 || !strings.HasSuffix(srv.port.grants[0], "write") {
		t.Errorf("grants = %v, want the write grant", srv.port.grants)
	}
	if len(srv.audit.entries) != 1 || srv.audit.entries[0].Action != actionTokenIssued {
		t.Errorf("audit = %+v, want one token_issued entry", srv.audit.entries)
	}
}

// TestIssueViewerGetsReadToken: the same route for a viewer — read scope.
func TestIssueViewerGetsReadToken(t *testing.T) {
	srv := wired(t, &stubProjectStore{member: true, role: domain.ProjectRoleViewer}, nil)

	_, payload := srv.issue(t)
	if payload["scope"] != "read" {
		t.Errorf("scope = %v, want read", payload["scope"])
	}
	if len(srv.port.mints) != 1 || srv.port.mints[0].Level != gitprovider.AccessRead {
		t.Errorf("mints = %+v, want one read-scope mint", srv.port.mints)
	}
}

// TestIssueNonMemberForbidden: no membership, no token — 403 with the
// stable code.
func TestIssueNonMemberForbidden(t *testing.T) {
	srv := wired(t, &stubProjectStore{member: false}, nil)

	resp := srv.request(t, http.MethodPost, "/api/v1/projects/"+testProjectID+"/git-tokens")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", resp.StatusCode, readBody(resp))
	}
	var envelope struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Code != codeAccessForbidden {
		t.Errorf("code = %q, want %q", envelope.Code, codeAccessForbidden)
	}
	if len(srv.port.mints) != 0 {
		t.Errorf("a non-member minted a token: %+v", srv.port.mints)
	}
}

// TestIssueUnprovisioned: the repository is not provisioned yet — 409
// naming the retry semantics.
func TestIssueUnprovisioned(t *testing.T) {
	srv := wired(t, &stubProjectStore{member: true, role: domain.ProjectRoleOwner},
		func(s *stubAccessStore) { s.repoRefErr = gitprovider.ErrRepoNotProvisioned })

	resp := srv.request(t, http.MethodPost, "/api/v1/projects/"+testProjectID+"/git-tokens")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", resp.StatusCode, readBody(resp))
	}
	var envelope struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Code != codeNotProvisioned {
		t.Errorf("code = %q, want %q", envelope.Code, codeNotProvisioned)
	}
}

// TestListShowsTokensWithoutCredential: the list carries the canonical
// facts — never the token value.
func TestListShowsTokensWithoutCredential(t *testing.T) {
	srv := wired(t, &stubProjectStore{member: true, role: domain.ProjectRoleOwner}, nil)
	id, _ := srv.issue(t)

	resp := srv.request(t, http.MethodGet, "/api/v1/projects/"+testProjectID+"/git-tokens")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list = %d (body %s)", resp.StatusCode, readBody(resp))
	}
	var payload struct {
		GitTokens []map[string]any `json:"git_tokens"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.GitTokens) != 1 {
		t.Fatalf("tokens = %d, want 1", len(payload.GitTokens))
	}
	tok := payload.GitTokens[0]
	if tok["id"] != id || tok["status"] != "active" || tok["scope"] != "write" {
		t.Errorf("token = %v, want the issued token active with write scope", tok)
	}
	if _, leak := tok["token"]; leak {
		t.Error("the list must never carry the token value")
	}
}

// TestRevokeTokenFlow: revoking one token kills the provider credential
// and flips the canonical row — the list shows it revoked.
func TestRevokeTokenFlow(t *testing.T) {
	srv := wired(t, &stubProjectStore{member: true, role: domain.ProjectRoleOwner}, nil)
	id, _ := srv.issue(t)

	resp := srv.request(t, http.MethodDelete, "/api/v1/projects/"+testProjectID+"/git-tokens/"+id)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke = %d (body %s)", resp.StatusCode, readBody(resp))
	}
	if len(srv.port.deletes) != 1 || srv.port.deletes[0] != "u-"+srv.userID+" 1" {
		t.Errorf("deletes = %v, want the provider token 1 deleted", srv.port.deletes)
	}
	rec, err := srv.store.GetToken(context.Background(), id)
	if err != nil || rec.Status != "revoked" {
		t.Errorf("canonical row = %+v (err %v), want revoked", rec, err)
	}

	list := srv.request(t, http.MethodGet, "/api/v1/projects/"+testProjectID+"/git-tokens")
	var payload struct {
		GitTokens []map[string]any `json:"git_tokens"`
	}
	if err := json.NewDecoder(list.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.GitTokens) != 1 || payload.GitTokens[0]["status"] != "revoked" {
		t.Errorf("list after revoke = %v, want the token present as revoked", payload.GitTokens)
	}
}

// TestRevokeTokenUnknown: an unknown (or foreign) token answers the
// existence-hiding 404.
func TestRevokeTokenUnknown(t *testing.T) {
	srv := wired(t, &stubProjectStore{member: true, role: domain.ProjectRoleOwner}, nil)

	resp := srv.request(t, http.MethodDelete,
		"/api/v1/projects/"+testProjectID+"/git-tokens/00000000-0000-4000-8000-000000000077")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", resp.StatusCode, readBody(resp))
	}
	var envelope struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Code != codeTokenNotFound {
		t.Errorf("code = %q, want %q", envelope.Code, codeTokenNotFound)
	}
}

// TestRevokeAccessFlow: the self-service disconnect kills the grant and
// every token, and — because a live grant really was revoked — the
// revocation lands in the audit stream.
func TestRevokeAccessFlow(t *testing.T) {
	srv := wired(t, &stubProjectStore{member: true, role: domain.ProjectRoleOwner}, nil)
	id, _ := srv.issue(t)

	resp := srv.request(t, http.MethodDelete, "/api/v1/projects/"+testProjectID+"/git-access")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke access = %d (body %s)", resp.StatusCode, readBody(resp))
	}
	if len(srv.port.revokes) != 1 {
		t.Errorf("revokes = %v, want the collaborator grant removed", srv.port.revokes)
	}
	if len(srv.port.deletes) != 1 {
		t.Errorf("deletes = %v, want every active token deleted", srv.port.deletes)
	}
	if len(srv.store.accessMarks) != 1 {
		t.Errorf("accessMarks = %v, want the access row revoked", srv.store.accessMarks)
	}
	rec, err := srv.store.GetToken(context.Background(), id)
	if err != nil || rec.Status != "revoked" {
		t.Errorf("canonical token row = %+v (err %v), want revoked", rec, err)
	}
	found := false
	for _, e := range srv.audit.entries {
		if e.Action == actionAccessRevoked {
			found = true
		}
	}
	if !found {
		t.Error("the revocation of a live grant did not reach the audit stream")
	}
}

// TestRevokeAccessNoGrantNoAudit: a disconnect without any recorded grant
// is a clean no-op — and must NOT write a git.access_revoked entry (the
// review's major finding: the previous unconditional audit write let any
// authenticated user plant a fake revocation event on any provisioned
// project).
func TestRevokeAccessNoGrantNoAudit(t *testing.T) {
	srv := wired(t, &stubProjectStore{member: true, role: domain.ProjectRoleOwner},
		func(s *stubAccessStore) { s.accessErr = gitprovider.ErrAccessNotFound })

	resp := srv.request(t, http.MethodDelete, "/api/v1/projects/"+testProjectID+"/git-access")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke access (no grant) = %d (body %s)", resp.StatusCode, readBody(resp))
	}
	if len(srv.port.revokes) != 0 || len(srv.port.deletes) != 0 || len(srv.store.accessMarks) != 0 {
		t.Errorf("a no-grant revoke did provider/store work: revokes=%v deletes=%v marks=%v",
			srv.port.revokes, srv.port.deletes, srv.store.accessMarks)
	}
	for _, e := range srv.audit.entries {
		if e.Action == actionAccessRevoked {
			t.Errorf("a no-grant revoke wrote a fake audit entry: %+v", e)
		}
	}
}

// TestRevokeAccessAlreadyRevokedNoAudit: an already-revoked grant is
// equally a no-op — nothing provider-side, nothing in the audit stream.
func TestRevokeAccessAlreadyRevokedNoAudit(t *testing.T) {
	srv := wired(t, &stubProjectStore{member: true, role: domain.ProjectRoleOwner},
		func(s *stubAccessStore) {
			now := time.Now()
			s.access = gitprovider.AccessRecord{ProjectID: testProjectID, RevokedAt: &now}
		})

	resp := srv.request(t, http.MethodDelete, "/api/v1/projects/"+testProjectID+"/git-access")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke access (already revoked) = %d (body %s)", resp.StatusCode, readBody(resp))
	}
	if len(srv.port.revokes) != 0 || len(srv.port.deletes) != 0 || len(srv.store.accessMarks) != 0 {
		t.Errorf("an already-revoked revoke did provider/store work: revokes=%v deletes=%v marks=%v",
			srv.port.revokes, srv.port.deletes, srv.store.accessMarks)
	}
	for _, e := range srv.audit.entries {
		if e.Action == actionAccessRevoked {
			t.Errorf("an already-revoked revoke wrote a fake audit entry: %+v", e)
		}
	}
}
