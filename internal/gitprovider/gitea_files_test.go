package gitprovider_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/gitprovider"
)

// The FilesPort implementation over the Gitea REST surface (T0307),
// scripted against the same fakeGitea harness as the rest of the adapter
// (package gitprovider_test). The response shapes match what the running
// 1.27.3 instance actually answers (probed during development): trees
// answer from a branch name or a SHA with recursive=true, contents answer
// base64-encoded, the commits list caps at 50 per page, and the raw route
// streams under /{owner}/{repo}/raw/{branch|commit}/….

func filesRepo() gitprovider.Repository {
	return gitprovider.Repository{Owner: "o", Name: "n"}
}

func treeRoute() giteaRoute {
	return giteaRoute{
		method: http.MethodGet,
		prefix: "/api/v1/repos/o/n/git/trees/",
		status: http.StatusOK,
		body: `{"sha":"abc123","tree":[
			{"path":"README.md","mode":"100644","size":12,"sha":"s1"},
			{"path":"src","mode":"040000","size":0,"sha":"s2"},
			{"path":"src/main.go","mode":"100644","size":30,"sha":"s3"},
			{"path":"link","mode":"120000","size":9,"sha":"s4"},
			{"path":"sub","mode":"160000","size":0,"sha":"s5"},
			{"path":"sub/readme","mode":"100644","size":1,"sha":"s6"}]}`,
	}
}

// TestGiteaGetTreeMapsAndFilters: the recursive trees response is reduced
// to the entries directly under the prefix, with the display type derived
// from the git mode.
func TestGiteaGetTreeMapsAndFilters(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{treeRoute()}}
	a := newGiteaAdapter(t, f)

	listing, err := a.GetTree(testCtx(t), filesRepo(), "main", "")
	if err != nil {
		t.Fatalf("GetTree root: %v", err)
	}
	if listing.SHA != "abc123" {
		t.Errorf("SHA = %q, want abc123", listing.SHA)
	}
	var paths []string
	for _, e := range listing.Entries {
		paths = append(paths, e.Path)
	}
	want := []string{"README.md", "src", "link", "sub"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("root entries = %v, want %v", paths, want)
	}
	types := map[string]string{}
	for _, e := range listing.Entries {
		types[e.Path] = e.Type
	}
	if types["README.md"] != "blob" || types["src"] != "tree" ||
		types["link"] != "symlink" || types["sub"] != "gitlink" {
		t.Errorf("derived types = %v, want blob/tree/symlink/gitlink", types)
	}

	sub, err := a.GetTree(testCtx(t), filesRepo(), "main", "src")
	if err != nil {
		t.Fatalf("GetTree src: %v", err)
	}
	if len(sub.Entries) != 1 || sub.Entries[0].Path != "src/main.go" ||
		sub.Entries[0].Name != "main.go" || sub.Entries[0].Mode != "100644" ||
		sub.Entries[0].Size != 30 || sub.Entries[0].SHA != "s3" {
		t.Errorf("src entries = %+v, want exactly src/main.go mapped", sub.Entries)
	}

	reqs := f.requests(http.MethodGet, "/api/v1/repos/o/n/git/trees/")
	if len(reqs) != 2 {
		t.Fatalf("tree requests = %d, want 2", len(reqs))
	}
	for _, req := range reqs {
		if !strings.Contains(req.path, "main") {
			t.Errorf("tree request %q does not carry the ref", req.path)
		}
	}
	last := reqs[1]
	if !strings.Contains(last.query, "recursive=true") {
		t.Errorf("tree request missing recursive=true: %s", last.query)
	}
	if last.auth != "token test-token" {
		t.Errorf("tree request auth = %q, want the service token", last.auth)
	}
}

// TestGiteaGetTreeRefNotFound: an unknown ref (or repo) is ErrNotFound.
func TestGiteaGetTreeRefNotFound(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{{
		method: http.MethodGet, prefix: "/api/v1/repos/o/n/git/trees/",
		status: http.StatusNotFound, body: `{"message":"sha not found [nope]"}`,
	}}}
	a := newGiteaAdapter(t, f)
	if _, err := a.GetTree(testCtx(t), filesRepo(), "nope", ""); !errors.Is(err, gitprovider.ErrNotFound) {
		t.Fatalf("GetTree unknown ref = %v, want ErrNotFound", err)
	}
}

// TestGiteaGetFileContentDecodes: base64 content decodes to raw bytes and
// the path is escaped segment-wise with the ref as a query parameter.
func TestGiteaGetFileContentDecodes(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{{
		method: http.MethodGet, prefix: "/api/v1/repos/o/n/contents/",
		status: http.StatusOK,
		body:   `{"name":"f.txt","path":"f.txt","type":"file","size":5,"sha":"s1","encoding":"base64","content":"aGVsbG8="}`,
	}}}
	a := newGiteaAdapter(t, f)

	fc, err := a.GetFileContent(testCtx(t), filesRepo(), "feature/x", "f.txt")
	if err != nil {
		t.Fatalf("GetFileContent: %v", err)
	}
	if string(fc.Content) != "hello" {
		t.Errorf("content = %q, want hello", fc.Content)
	}
	if fc.Type != "file" || fc.Size != 5 || fc.SHA != "s1" {
		t.Errorf("facts = %+v, want file/5/s1", fc)
	}
	reqs := f.requests(http.MethodGet, "/api/v1/repos/o/n/contents/")
	if len(reqs) != 1 {
		t.Fatalf("content requests = %d, want 1", len(reqs))
	}
	if !strings.Contains(reqs[0].path, "/contents/f.txt") {
		t.Errorf("content request path = %q, want /contents/f.txt", reqs[0].path)
	}
	q, _ := url.ParseQuery(reqs[0].query)
	if q.Get("ref") != "feature/x" {
		t.Errorf("content ref = %q, want feature/x", q.Get("ref"))
	}
}

// TestGiteaGetFileContentSymlinkCarriesTarget: the provider's target field
// rides through (the FilesReader surfaces it, never follows it).
func TestGiteaGetFileContentSymlinkCarriesTarget(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{{
		method: http.MethodGet, prefix: "/api/v1/repos/o/n/contents/",
		status: http.StatusOK,
		body:   `{"name":"link","path":"link","type":"symlink","size":4,"sha":"s4","encoding":"base64","content":"YmlnLmJpbg==","target":"big.bin"}`,
	}}}
	a := newGiteaAdapter(t, f)

	fc, err := a.GetFileContent(testCtx(t), filesRepo(), "main", "link")
	if err != nil {
		t.Fatalf("GetFileContent symlink: %v", err)
	}
	if fc.Type != "symlink" || fc.Target != "big.bin" {
		t.Errorf("symlink facts = %+v, want type symlink, target big.bin", fc)
	}
}

// TestGiteaGetFileContentNotFound: a missing path is ErrNotFound.
func TestGiteaGetFileContentNotFound(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{{
		method: http.MethodGet, prefix: "/api/v1/repos/o/n/contents/",
		status: http.StatusNotFound, body: `{"message":"object does not exist"}`,
	}}}
	a := newGiteaAdapter(t, f)
	if _, err := a.GetFileContent(testCtx(t), filesRepo(), "main", "gone.txt"); !errors.Is(err, gitprovider.ErrNotFound) {
		t.Fatalf("GetFileContent missing = %v, want ErrNotFound", err)
	}
}

// TestGiteaGetRawStreams: the raw route streams provider bytes with the
// service token; branch names take the branch segment, full SHAs the
// commit segment.
func TestGiteaGetRawStreams(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{{
		method: http.MethodGet, prefix: "/o/n/raw/branch/main/",
		status: http.StatusOK, body: "raw-bytes",
	}}}
	a := newGiteaAdapter(t, f)

	size, body, err := a.GetRaw(testCtx(t), filesRepo(), "main", "f.bin")
	if err != nil {
		t.Fatalf("GetRaw: %v", err)
	}
	got, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		t.Fatalf("read raw body: %v", err)
	}
	if string(got) != "raw-bytes" {
		t.Errorf("raw body = %q, want raw-bytes", got)
	}
	if size != 9 {
		t.Errorf("size = %d, want 9 (the fake's Content-Length parsed)", size)
	}
	reqs := f.requests(http.MethodGet, "/o/n/raw/branch/main/")
	if len(reqs) != 1 || reqs[0].auth != "token test-token" {
		t.Fatalf("raw request = %+v, want one token-authenticated call", reqs)
	}

	// A full SHA takes the commit segment.
	f2 := &fakeGitea{routes: []giteaRoute{{
		method: http.MethodGet, prefix: "/o/n/raw/commit/", status: http.StatusOK, body: "x",
	}}}
	a2 := newGiteaAdapter(t, f2)
	sha := "0123456789abcdef0123456789abcdef01234567"
	if _, body, err := a2.GetRaw(testCtx(t), filesRepo(), sha, "f.bin"); err != nil {
		t.Fatalf("GetRaw by SHA: %v", err)
	} else {
		_ = body.Close()
	}
	if reqs := f2.requests(http.MethodGet, "/o/n/raw/commit/"); len(reqs) != 1 {
		t.Errorf("raw-by-SHA requests = %d, want 1 on the commit segment", len(reqs))
	}
}

// TestGiteaGetRawErrorMapping: 404 → ErrNotFound, 401 → ErrUnauthorized.
func TestGiteaGetRawErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{{http.StatusNotFound, gitprovider.ErrNotFound}, {http.StatusUnauthorized, gitprovider.ErrUnauthorized}} {
		f := &fakeGitea{routes: []giteaRoute{{
			method: http.MethodGet, prefix: "/o/n/raw/branch/main/", status: tc.status,
		}}}
		a := newGiteaAdapter(t, f)
		_, body, err := a.GetRaw(testCtx(t), filesRepo(), "main", "f.bin")
		if !errors.Is(err, tc.want) {
			t.Errorf("GetRaw status %d = %v, want %v", tc.status, err, tc.want)
		}
		if body != nil {
			_ = body.Close()
		}
	}
}

// TestGiteaGetHistoryMapsAndBindsParams: the commits list maps onto
// CommitEntry, passes the ref/limit and only sends path when set.
func TestGiteaGetHistoryMapsAndBindsParams(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{{
		method: http.MethodGet, prefix: "/api/v1/repos/o/n/commits",
		status: http.StatusOK,
		body:   `[{"sha":"c1","commit":{"author":{"name":"Probe","email":"p@e.c","date":"2026-09-14T10:27:13+08:00"},"message":"first\n\nbody"}}]`,
	}}}
	a := newGiteaAdapter(t, f)

	entries, err := a.GetHistory(testCtx(t), filesRepo(), "probe-branch", "f.txt", 25)
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.SHA != "c1" || e.Author != "Probe" || e.AuthorEmail != "p@e.c" ||
		e.Message != "first\n\nbody" || e.Date.Year() != 2026 {
		t.Errorf("entry = %+v, want the mapped commit", e)
	}
	reqs := f.requests(http.MethodGet, "/api/v1/repos/o/n/commits")
	if len(reqs) != 1 {
		t.Fatalf("history requests = %d, want 1", len(reqs))
	}
	q, _ := url.ParseQuery(reqs[0].query)
	if q.Get("sha") != "probe-branch" || q.Get("limit") != "25" || q.Get("path") != "f.txt" {
		t.Errorf("history query = %v, want sha/limit/path bound", q)
	}

	// Without a path the parameter is omitted entirely (root history).
	f2 := &fakeGitea{routes: []giteaRoute{{
		method: http.MethodGet, prefix: "/api/v1/repos/o/n/commits",
		status: http.StatusOK, body: `[]`,
	}}}
	a2 := newGiteaAdapter(t, f2)
	if _, err := a2.GetHistory(testCtx(t), filesRepo(), "main", "", 50); err != nil {
		t.Fatalf("GetHistory root: %v", err)
	}
	q2, _ := url.ParseQuery(f2.requests(http.MethodGet, "/api/v1/repos/o/n/commits")[0].query)
	if _, ok := q2["path"]; ok {
		t.Errorf("root history carries a path parameter: %v", q2)
	}
}

// TestGiteaGetHistoryTruncatesMessages: messages bound at 1000 runes on
// rune boundaries (a multibyte split would store invalid UTF-8).
func TestGiteaGetHistoryTruncatesMessages(t *testing.T) {
	long := strings.Repeat("é", 1200)
	body, _ := json.Marshal([]map[string]any{{
		"sha": "c1",
		"commit": map[string]any{"author": map[string]any{
			"name": "a", "email": "a@b.c", "date": "2026-09-14T10:27:13+08:00"}, "message": long},
	}})
	f := &fakeGitea{routes: []giteaRoute{{
		method: http.MethodGet, prefix: "/api/v1/repos/o/n/commits",
		status: http.StatusOK, body: string(body),
	}}}
	a := newGiteaAdapter(t, f)
	entries, err := a.GetHistory(testCtx(t), filesRepo(), "main", "", 50)
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if got := []rune(entries[0].Message); len(got) != 1001 {
		t.Errorf("message runes = %d, want 1001 (1000 + ellipsis)", len(got))
	}
}

// TestGiteaGetHistoryRefNotFound: an unknown ref is ErrNotFound.
func TestGiteaGetHistoryRefNotFound(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{{
		method: http.MethodGet, prefix: "/api/v1/repos/o/n/commits",
		status: http.StatusNotFound, body: `{"message":"revision does not exist"}`,
	}}}
	a := newGiteaAdapter(t, f)
	if _, err := a.GetHistory(testCtx(t), filesRepo(), "nope", "", 50); !errors.Is(err, gitprovider.ErrNotFound) {
		t.Fatalf("GetHistory unknown ref = %v, want ErrNotFound", err)
	}
}
