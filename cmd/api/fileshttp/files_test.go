package fileshttp

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
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/gitprovider"
	"github.com/lichman0405/post/internal/persistence/memstore"
)

// The Files HTTP surface, tested at the handler level through the REAL
// guard and the REAL matrix engine (the visibility_test composition): a
// real signup issues the session the guard resolves, the projects service
// evaluates the actual read policy, and the FilesReader runs its real
// preview policy over a scripted port. The assertions are about the
// handlers' decisions — the permission filter, the disabled 503, the wire
// codes, the safe-preview payload and the mutation rejection — while the
// reader policy and the adapter have their own suites.

const filesProjectID = "11111111-2222-4333-8444-555555555555"

// filesProjectStore implements projects.ProjectStore with configurable
// visibility and membership (the visProjectStore shape): member answers
// every caller as an owner, so the matrix decides what a member may read.
type filesProjectStore struct {
	project domain.Project
	member  bool
}

func (s *filesProjectStore) CreateProject(context.Context, domain.Project, string) (domain.Project, domain.ProjectMembership, error) {
	return domain.Project{}, domain.ProjectMembership{}, projects.ErrStore
}

func (s *filesProjectStore) GetProject(_ context.Context, projectID string) (domain.Project, error) {
	if s.project.ID == projectID {
		return s.project, nil
	}
	return domain.Project{}, projects.ErrProjectNotFound
}

func (s *filesProjectStore) GetMembership(_ context.Context, projectID, userID string) (domain.ProjectMembership, error) {
	if s.member && s.project.ID == projectID {
		return domain.ProjectMembership{
			ProjectID: projectID,
			UserID:    userID,
			Role:      domain.ProjectRoleOwner,
			CreatedAt: time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC),
		}, nil
	}
	return domain.ProjectMembership{}, projects.ErrMemberNotFound
}

func (s *filesProjectStore) ListProjectMembers(context.Context, string) ([]domain.ProjectMember, error) {
	return nil, nil
}

func (s *filesProjectStore) UpdateMembershipRole(context.Context, string, string, domain.ProjectRole, domain.AuditEntry) (domain.ProjectMembership, error) {
	return domain.ProjectMembership{}, projects.ErrStore
}

func (s *filesProjectStore) UpdateProjectSettings(context.Context, string, *string, *string, domain.AuditEntry) (domain.Project, error) {
	return domain.Project{}, projects.ErrStore
}

func (s *filesProjectStore) ListProjectsForUser(context.Context, string) ([]domain.Project, error) {
	return nil, nil
}

func (s *filesProjectStore) ListPublicProjects(context.Context) ([]domain.Project, error) {
	return nil, nil
}

func (s *filesProjectStore) GetProgram(context.Context, string) (domain.Program, error) {
	return domain.Program{}, projects.ErrProgramNotFound
}

// filesOrgGate satisfies projects.OrgGate (unused by the files routes).
type filesOrgGate struct{}

func (filesOrgGate) GetOrganization(context.Context, string) (domain.Organization, error) {
	return domain.Organization{}, projects.ErrOrgNotFound
}

func (filesOrgGate) GetMembership(context.Context, string, string) (domain.OrganizationMembership, error) {
	return domain.OrganizationMembership{}, projects.ErrMemberNotFound
}

// filesResolver scripts the canonical-store half of the FilesReader.
type filesResolver struct {
	ref gitprovider.RepoRef
	err error
}

func (r *filesResolver) RepoRef(_ context.Context, _ string) (gitprovider.RepoRef, error) {
	return r.ref, r.err
}

// filesRaw is one scripted raw download.
type filesRaw struct {
	size int64
	body string
}

// filesPort implements gitprovider.FilesPort in memory: scripted answers
// plus a record of every provider call, so the permission filter can be
// pinned — a denied read must reach the provider exactly zero times.
type filesPort struct {
	tree    map[string]gitprovider.TreeListing // key: prefix
	treeErr error
	files   map[string]gitprovider.FileContent // key: path
	fileErr error
	raw     map[string]filesRaw // key: path
	rawErr  error
	history []gitprovider.CommitEntry
	histErr error

	calls         int
	gotTreeRef    string
	gotTreePrefix string
	gotHistLimit  int
}

func newFilesPort() *filesPort {
	return &filesPort{
		tree:  map[string]gitprovider.TreeListing{},
		files: map[string]gitprovider.FileContent{},
		raw:   map[string]filesRaw{},
	}
}

func (p *filesPort) GetTree(_ context.Context, _ gitprovider.Repository, ref, prefix string) (gitprovider.TreeListing, error) {
	p.calls++
	p.gotTreeRef, p.gotTreePrefix = ref, prefix
	if p.treeErr != nil {
		return gitprovider.TreeListing{}, p.treeErr
	}
	return p.tree[prefix], nil
}

func (p *filesPort) GetFileContent(_ context.Context, _ gitprovider.Repository, _, path string) (gitprovider.FileContent, error) {
	p.calls++
	if p.fileErr != nil {
		return gitprovider.FileContent{}, p.fileErr
	}
	return p.files[path], nil
}

func (p *filesPort) GetRaw(_ context.Context, _ gitprovider.Repository, _, path string) (int64, io.ReadCloser, error) {
	p.calls++
	if p.rawErr != nil {
		return 0, nil, p.rawErr
	}
	f, ok := p.raw[path]
	if !ok {
		return 0, nil, gitprovider.ErrNotFound
	}
	return f.size, io.NopCloser(strings.NewReader(f.body)), nil
}

func (p *filesPort) GetHistory(_ context.Context, _ gitprovider.Repository, _, _ string, limit int) ([]gitprovider.CommitEntry, error) {
	p.calls++
	p.gotHistLimit = limit
	if p.histErr != nil {
		return nil, p.histErr
	}
	return p.history, nil
}

// blobEntry builds one tree entry of the given display type.
func blobEntry(path, sha string, size int64, typ string) gitprovider.TreeEntry {
	name := path
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		name = path[i+1:]
	}
	mode := "100644"
	switch typ {
	case "tree":
		mode = "040000"
	case "symlink":
		mode = "120000"
	case "gitlink":
		mode = "160000"
	}
	return gitprovider.TreeEntry{Name: name, Path: path, Type: typ, Mode: mode, Size: size, SHA: sha}
}

// filesTestServer composes the guarded surface: real auth (real signup →
// session), real projects service over the REAL matrix engine, and the
// files surface over a scripted FilesReader.
type filesTestServer struct {
	ts     *httptest.Server
	client *http.Client // signed-in browser (session cookie jar)
	anon   *http.Client // no session
	csrf   string
	port   *filesPort
}

func newFilesTestServer(t *testing.T, store projects.ProjectStore, files *gitprovider.FilesReader, missing []string) *filesTestServer {
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
	api := New(Deps{
		Projects: projects.NewService(store, filesOrgGate{}, authz.NewMatrixEngine()),
		Files:    files,
		Missing:  missing,
	})
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	api.Register(mux)
	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)

	// Real signup: the session lands in the in-memory store and the cookie
	// jar carries it on every request.
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Post(ts.URL+"/api/v1/auth/signup", "application/json",
		strings.NewReader(`{"email":"files@example.com","password":"long-enough-password-1","handle":"files","display_name":"Files Test"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("signup = %d (body %s)", resp.StatusCode, filesReadBody(resp))
	}
	var payload struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	return &filesTestServer{
		ts:     ts,
		client: client,
		anon:   &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		csrf:   payload.CSRFToken,
	}
}

func filesReadBody(resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// get performs one signed-in GET.
func (s *filesTestServer) get(t *testing.T, path string) *http.Response {
	t.Helper()
	resp, err := s.client.Get(s.ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// anonGet performs one GET with no session at all.
func (s *filesTestServer) anonGet(t *testing.T, path string) *http.Response {
	t.Helper()
	resp, err := s.anon.Get(s.ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// write performs one non-GET request with the session cookie and the CSRF
// token the guard demands of every mutation attempt.
func (s *filesTestServer) write(t *testing.T, method, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, s.ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-CSRF-Token", s.csrf)
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// codeOf decodes the envelope's stable code.
func codeOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	var envelope struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	return envelope.Code
}

// memberFiles wires the default happy-path surface: a private project the
// signed-in caller owns, a provisioned repository, a scripted port.
func memberFiles(t *testing.T, configure func(*filesPort)) *filesTestServer {
	t.Helper()
	port := newFilesPort()
	if configure != nil {
		configure(port)
	}
	reader := gitprovider.NewFilesReader(port, &filesResolver{ref: gitprovider.RepoRef{
		ProjectID: filesProjectID, Owner: "post-git-svc", Name: "p-" + filesProjectID,
	}})
	srv := newFilesTestServer(t, &filesProjectStore{project: domain.Project{
		ID: filesProjectID, Slug: "lab", Name: "Lab", Visibility: domain.VisibilityPrivate,
	}, member: true}, reader, nil)
	srv.port = port
	return srv
}

// TestFilesTreeListsDirectory: a member's tree read rides through with the
// empty ref defaulted to main and the listing mapped onto the wire.
func TestFilesTreeListsDirectory(t *testing.T) {
	srv := memberFiles(t, func(p *filesPort) {
		p.tree[""] = gitprovider.TreeListing{SHA: "abc", Entries: []gitprovider.TreeEntry{
			{Name: "f.txt", Path: "f.txt", Type: "blob", Mode: "100644", Size: 3, SHA: "s"},
		}}
	})

	resp := srv.get(t, "/api/v1/projects/"+filesProjectID+"/files/tree")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tree = %d (body %s)", resp.StatusCode, filesReadBody(resp))
	}
	var payload struct {
		Ref     string `json:"ref"`
		Path    string `json:"path"`
		SHA     string `json:"sha"`
		Entries []struct {
			Name, Path, Type, Mode, SHA string
			Size                        int64
		} `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Ref != "main" || payload.Path != "" || payload.SHA != "abc" {
		t.Errorf("payload = %+v, want ref main, root path, sha abc", payload)
	}
	if len(payload.Entries) != 1 || payload.Entries[0].Name != "f.txt" ||
		payload.Entries[0].Size != 3 || payload.Entries[0].SHA != "s" {
		t.Errorf("entries = %+v, want the scripted entry mapped", payload.Entries)
	}
	if srv.port.gotTreeRef != "main" || srv.port.gotTreePrefix != "" {
		t.Errorf("port saw (ref %q, prefix %q), want the defaulted main at the root", srv.port.gotTreeRef, srv.port.gotTreePrefix)
	}
}

// TestFilesAnonymousReadsPublicProject: read_files public_policy — an
// anonymous caller reads a public project's files.
func TestFilesAnonymousReadsPublicProject(t *testing.T) {
	port := newFilesPort()
	port.tree[""] = gitprovider.TreeListing{SHA: "abc", Entries: []gitprovider.TreeEntry{
		blobEntry("f.txt", "s", 3, "blob"),
	}}
	reader := gitprovider.NewFilesReader(port, &filesResolver{ref: gitprovider.RepoRef{
		ProjectID: filesProjectID, Owner: "post-git-svc", Name: "p-" + filesProjectID,
	}})
	srv := newFilesTestServer(t, &filesProjectStore{project: domain.Project{
		ID: filesProjectID, Slug: "pub", Name: "Pub", Visibility: domain.VisibilityPublic,
	}}, reader, nil)

	resp := srv.anonGet(t, "/api/v1/projects/"+filesProjectID+"/files/tree")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous tree = %d (body %s)", resp.StatusCode, filesReadBody(resp))
	}
	if port.calls == 0 {
		t.Error("the public project read never reached the provider")
	}
}

// TestFilesAnonymousPrivateProjectHidden: a private project answers the
// existence-hiding 404 for an anonymous caller, and — the acceptance
// half — the provider is never touched (the permission filter runs before
// any provider byte is fetched).
func TestFilesAnonymousPrivateProjectHidden(t *testing.T) {
	port := newFilesPort()
	reader := gitprovider.NewFilesReader(port, &filesResolver{ref: gitprovider.RepoRef{
		ProjectID: filesProjectID, Owner: "post-git-svc", Name: "p-" + filesProjectID,
	}})
	srv := newFilesTestServer(t, &filesProjectStore{project: domain.Project{
		ID: filesProjectID, Slug: "lab", Name: "Lab", Visibility: domain.VisibilityPrivate,
	}}, reader, nil)

	resp := srv.anonGet(t, "/api/v1/projects/"+filesProjectID+"/files/tree")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (existence hiding)", resp.StatusCode)
	}
	if code := codeOf(t, resp); code != codeProjectNotFound {
		t.Errorf("code = %q, want %q", code, codeProjectNotFound)
	}
	if port.calls != 0 {
		t.Errorf("a denied anonymous read reached the provider %d times", port.calls)
	}
}

// TestFilesAuthedNonMemberPrivateProjectHidden: the same gate holds for a
// signed-in non-member — 404, provider untouched.
func TestFilesAuthedNonMemberPrivateProjectHidden(t *testing.T) {
	port := newFilesPort()
	reader := gitprovider.NewFilesReader(port, &filesResolver{ref: gitprovider.RepoRef{
		ProjectID: filesProjectID, Owner: "post-git-svc", Name: "p-" + filesProjectID,
	}})
	srv := newFilesTestServer(t, &filesProjectStore{project: domain.Project{
		ID: filesProjectID, Slug: "lab", Name: "Lab", Visibility: domain.VisibilityPrivate,
	}, member: false}, reader, nil)

	resp := srv.get(t, "/api/v1/projects/"+filesProjectID+"/files/tree")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (existence hiding for the non-member)", resp.StatusCode)
	}
	if code := codeOf(t, resp); code != codeProjectNotFound {
		t.Errorf("code = %q, want %q", code, codeProjectNotFound)
	}
	if port.calls != 0 {
		t.Errorf("a denied non-member read reached the provider %d times", port.calls)
	}
}

// TestFilesContentPointerDisplayLeaksNoBlobMetadata: the pointer preview
// carries exactly the pointer file's own fields — the wire answer contains
// none of the blobs-table's private metadata (storage key, encryption
// metadata, access policy, created actor — docs/17 §3). The Files read
// path never queries the blobs table; this test pins the wire shape.
func TestFilesContentPointerDisplayLeaksNoBlobMetadata(t *testing.T) {
	srv := memberFiles(t, func(p *filesPort) {
		p.tree["data"] = gitprovider.TreeListing{Entries: []gitprovider.TreeEntry{
			blobEntry("data/traj.xyz.ptr", "s1", 85, "blob"),
		}}
		p.files["data/traj.xyz.ptr"] = gitprovider.FileContent{
			Path: "data/traj.xyz.ptr", Type: "file", Size: 85, SHA: "s1",
			Content: []byte(`{"format":"post-blob-pointer","content_hash":"sha256:aa","size_bytes":999,"blob_id":"b1"}`),
		}
	})

	resp := srv.get(t, "/api/v1/projects/"+filesProjectID+"/files/content?path=data%2Ftraj.xyz.ptr")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("content = %d (body %s)", resp.StatusCode, filesReadBody(resp))
	}
	body := filesReadBody(resp)
	var payload struct {
		Kind        string `json:"kind"`
		Content     string `json:"content"`
		BlobPointer *struct {
			ContentHash string `json:"content_hash"`
			SizeBytes   int64  `json:"size_bytes"`
			BlobID      string `json:"blob_id"`
		} `json:"blob_pointer"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Kind != "text" || payload.BlobPointer == nil {
		t.Fatalf("payload = %+v, want a text preview with a blob pointer", payload)
	}
	if payload.BlobPointer.ContentHash != "sha256:aa" || payload.BlobPointer.SizeBytes != 999 ||
		payload.BlobPointer.BlobID != "b1" {
		t.Errorf("blob_pointer = %+v, want the manifest's own fields only", payload.BlobPointer)
	}
	for _, forbidden := range []string{"storage_key", "encryption", "access_policy", "created_by", "actor"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the files answer leaks private blob metadata: %q appears in %s", forbidden, body)
		}
	}
}

// TestFilesContentTooLarge: a blob over the fetch bound previews as
// too_large with size+SHA only — no content channel call at all.
func TestFilesContentTooLarge(t *testing.T) {
	srv := memberFiles(t, func(p *filesPort) {
		p.tree["data"] = gitprovider.TreeListing{Entries: []gitprovider.TreeEntry{
			blobEntry("data/huge.bin", "s1", gitprovider.FetchMaxBytes+1, "blob"),
		}}
	})

	resp := srv.get(t, "/api/v1/projects/"+filesProjectID+"/files/content?path=data%2Fhuge.bin")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("content = %d (body %s)", resp.StatusCode, filesReadBody(resp))
	}
	var payload struct {
		Kind    string `json:"kind"`
		Content string `json:"content"`
		Size    int64  `json:"size"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Kind != "too_large" || payload.Content != "" || payload.Size != gitprovider.FetchMaxBytes+1 {
		t.Errorf("payload = %+v, want too_large with size only", payload)
	}
}

// TestFilesContentBinary: a binary blob previews as a pointer display —
// kind binary, never decoded bytes.
func TestFilesContentBinary(t *testing.T) {
	srv := memberFiles(t, func(p *filesPort) {
		p.tree[""] = gitprovider.TreeListing{Entries: []gitprovider.TreeEntry{
			blobEntry("bin.dat", "s1", 4, "blob"),
		}}
		p.files["bin.dat"] = gitprovider.FileContent{
			Path: "bin.dat", Type: "file", Size: 4, SHA: "s1", Content: []byte("ab\x00cd"),
		}
	})

	resp := srv.get(t, "/api/v1/projects/"+filesProjectID+"/files/content?path=bin.dat")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("content = %d (body %s)", resp.StatusCode, filesReadBody(resp))
	}
	var payload struct {
		Kind    string `json:"kind"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Kind != "binary" || payload.Content != "" {
		t.Errorf("payload = %+v, want binary without content", payload)
	}
}

// TestFilesContentTruncated: text between the fetch and preview bounds is
// cut at PreviewMaxBytes and flagged on the wire.
func TestFilesContentTruncated(t *testing.T) {
	srv := memberFiles(t, func(p *filesPort) {
		p.tree[""] = gitprovider.TreeListing{Entries: []gitprovider.TreeEntry{
			blobEntry("big.txt", "s1", 300*1024, "blob"),
		}}
		p.files["big.txt"] = gitprovider.FileContent{
			Path: "big.txt", Type: "file", Size: 300 * 1024, SHA: "s1",
			Content: []byte(strings.Repeat("x", 300*1024)),
		}
	})

	resp := srv.get(t, "/api/v1/projects/"+filesProjectID+"/files/content?path=big.txt")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("content = %d (body %s)", resp.StatusCode, filesReadBody(resp))
	}
	var payload struct {
		Kind      string `json:"kind"`
		Content   string `json:"content"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Kind != "text" || !payload.Truncated || len(payload.Content) != gitprovider.PreviewMaxBytes {
		t.Errorf("payload = kind %s truncated %v len %d, want text/true/%d",
			payload.Kind, payload.Truncated, len(payload.Content), gitprovider.PreviewMaxBytes)
	}
}

// TestFilesHistoryLimitClamped: the client's page size clamps at the
// provider bound before the port is called, and the envelope echoes the
// defaulted ref.
func TestFilesHistoryLimitClamped(t *testing.T) {
	srv := memberFiles(t, func(p *filesPort) {
		p.history = []gitprovider.CommitEntry{{
			SHA: "c1", Author: "Probe", AuthorEmail: "p@e.c",
			Date: time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC), Message: "first",
		}}
	})

	resp := srv.get(t, "/api/v1/projects/"+filesProjectID+"/files/history?limit=500")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history = %d (body %s)", resp.StatusCode, filesReadBody(resp))
	}
	var payload struct {
		Ref     string `json:"ref"`
		Path    string `json:"path"`
		Entries []struct {
			SHA, Author, AuthorEmail, Message string
		} `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Ref != "main" || len(payload.Entries) != 1 || payload.Entries[0].SHA != "c1" {
		t.Errorf("payload = %+v, want main ref and the scripted commit", payload)
	}
	if srv.port.gotHistLimit != gitprovider.HistoryMaxEntries {
		t.Errorf("port saw limit %d, want the clamped %d", srv.port.gotHistLimit, gitprovider.HistoryMaxEntries)
	}
}

// TestFilesHistoryBadLimit: a non-integer limit is a 400 naming the
// parameter, not a provider read.
func TestFilesHistoryBadLimit(t *testing.T) {
	srv := memberFiles(t, nil)

	resp := srv.get(t, "/api/v1/projects/"+filesProjectID+"/files/history?limit=abc")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if code := codeOf(t, resp); code != codeInvalidLimit {
		t.Errorf("code = %q, want %q", code, codeInvalidLimit)
	}
	if srv.port.calls != 0 {
		t.Errorf("a bad limit reached the provider %d times", srv.port.calls)
	}
}

// TestFilesRawAttachmentHeaders: the download channel streams provider
// bytes with a browser-safe disposition — octet-stream, attachment
// filename, nosniff — and the exact body.
func TestFilesRawAttachmentHeaders(t *testing.T) {
	srv := memberFiles(t, func(p *filesPort) {
		p.raw["f.bin"] = filesRaw{size: 9, body: "raw-bytes"}
	})

	resp := srv.get(t, "/api/v1/projects/"+filesProjectID+"/files/raw?path=f.bin")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("raw = %d (body %s)", resp.StatusCode, filesReadBody(resp))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="f.bin"` {
		t.Errorf("Content-Disposition = %q, want the attachment with the filename", cd)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff on the raw download")
	}
	if cl := resp.Header.Get("Content-Length"); cl != "9" {
		t.Errorf("Content-Length = %q, want 9", cl)
	}
	if body := filesReadBody(resp); body != "raw-bytes" {
		t.Errorf("body = %q, want the provider bytes", body)
	}
}

// TestFilesDisabledAnswers503: without the provider token the surface
// fails loud — 503 naming the missing key, before any project or provider
// access (the enabled gate runs first).
func TestFilesDisabledAnswers503(t *testing.T) {
	srv := newFilesTestServer(t, &filesProjectStore{project: domain.Project{
		ID: filesProjectID, Slug: "lab", Name: "Lab", Visibility: domain.VisibilityPrivate,
	}, member: true}, nil, []string{"POST_GITEA_TOKEN"})

	resp := srv.get(t, "/api/v1/projects/"+filesProjectID+"/files/tree")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	body := filesReadBody(resp)
	var envelope struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Code != codeDisabled {
		t.Errorf("code = %q, want %q", envelope.Code, codeDisabled)
	}
	// The message names the missing key (names only — never values).
	if !strings.Contains(body, "POST_GITEA_TOKEN") {
		t.Errorf("disabled message does not name the missing key: %s", body)
	}
}

// TestMutationMethodsRejected: the Files API has NO mutation methods —
// structurally. Only GET routes are registered, so every other verb on
// every files route answers 405 before any handler (or provider call)
// runs, even with a fully valid session and CSRF token.
func TestMutationMethodsRejected(t *testing.T) {
	srv := memberFiles(t, nil)
	routes := []string{"tree", "content", "history", "raw"}
	for _, route := range routes {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			resp := srv.write(t, method, "/api/v1/projects/"+filesProjectID+"/files/"+route)
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s /files/%s = %d, want 405", method, route, resp.StatusCode)
				continue
			}
			if allow := resp.Header.Get("Allow"); !strings.Contains(allow, http.MethodGet) {
				t.Errorf("%s /files/%s Allow = %q, want GET (the only method served)", method, route, allow)
			}
		}
	}
	if srv.port.calls != 0 {
		t.Errorf("a rejected mutation reached the provider %d times", srv.port.calls)
	}
}

// TestFilesInvalidRef: a CLI-shaped ref is a 400 naming the ref, before
// the provider is asked.
func TestFilesInvalidRef(t *testing.T) {
	srv := memberFiles(t, nil)

	resp := srv.get(t, "/api/v1/projects/"+filesProjectID+"/files/tree?ref=bad%20ref")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if code := codeOf(t, resp); code != codeInvalidRef {
		t.Errorf("code = %q, want %q", code, codeInvalidRef)
	}
	if srv.port.calls != 0 {
		t.Errorf("an invalid ref reached the provider %d times", srv.port.calls)
	}
}

// TestFilesDirectoryPathRejected: the content endpoint reads blobs only —
// a directory path answers 400 pointing at the tree endpoint.
func TestFilesDirectoryPathRejected(t *testing.T) {
	srv := memberFiles(t, func(p *filesPort) {
		p.tree[""] = gitprovider.TreeListing{Entries: []gitprovider.TreeEntry{
			blobEntry("data", "s1", 0, "tree"),
		}}
	})

	resp := srv.get(t, "/api/v1/projects/"+filesProjectID+"/files/content?path=data")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if code := codeOf(t, resp); code != codePathIsDirectory {
		t.Errorf("code = %q, want %q", code, codePathIsDirectory)
	}
}

// TestFilesNotProvisioned: a project without a provisioned repository
// answers 409 with the retry semantics.
func TestFilesNotProvisioned(t *testing.T) {
	port := newFilesPort()
	reader := gitprovider.NewFilesReader(port, &filesResolver{err: gitprovider.ErrRepoNotProvisioned})
	srv := newFilesTestServer(t, &filesProjectStore{project: domain.Project{
		ID: filesProjectID, Slug: "lab", Name: "Lab", Visibility: domain.VisibilityPrivate,
	}, member: true}, reader, nil)

	resp := srv.get(t, "/api/v1/projects/"+filesProjectID+"/files/tree")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if code := codeOf(t, resp); code != codeNotProvisioned {
		t.Errorf("code = %q, want %q", code, codeNotProvisioned)
	}
}

// TestFilesRefNotFound: an unknown ref (or missing repository) is the
// files-not-found 404.
func TestFilesRefNotFound(t *testing.T) {
	srv := memberFiles(t, func(p *filesPort) {
		p.treeErr = gitprovider.ErrNotFound
	})

	resp := srv.get(t, "/api/v1/projects/"+filesProjectID+"/files/tree?ref=gone")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if code := codeOf(t, resp); code != codeNotFound {
		t.Errorf("code = %q, want %q", code, codeNotFound)
	}
}
