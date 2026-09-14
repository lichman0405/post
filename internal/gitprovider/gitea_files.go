package gitprovider

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The FilesPort implementation over the Gitea REST API v1 (T0307). Every
// read authenticates with the service account token — the same machine
// identity as the rest of the adapter, so the provider sees one controlled
// reader regardless of which user's request triggered it (the platform's
// own permission filter has already run at that point).
//
// Endpoint choices are the ones this deployment actually answers (checked
// against the running instance, like T0303's reads): the trees API
// (GET /git/trees/{ref} — branch names and full SHAs both resolve, and
// recursive=true is the only way to list below the root), the contents
// API (GET /contents/{path}?ref= — base64-encoded content), the commits
// list (GET /commits?sha=&path=&limit= — capped at 50 per page by the
// provider) and the raw route (GET /{owner}/{repo}/raw/{branch|commit}/
// {ref}/{path} — the streaming download channel, outside /api/v1).

// treeBody is the provider-side trees response (a subset of Gitea's
// GitTreeResponse). Entries carry the raw git mode — the type mapping is
// derived from it, never from the provider's classification.
type treeBody struct {
	SHA  string `json:"sha"`
	Tree []struct {
		Path string `json:"path"`
		Mode string `json:"mode"`
		Size int64  `json:"size"`
		SHA  string `json:"sha"`
	} `json:"tree"`
}

// GetTree implements FilesPort: one recursive trees read, filtered to the
// entries directly under prefix. The provider only lists from the root
// (recursive=true), so deep listings cost the same one request; the
// response cap bounds how large a tree this can serve (V1 repositories
// are small by design — large data lives in blobs, docs/17 §2).
func (a *GiteaAdapter) GetTree(ctx context.Context, repo Repository, ref, prefix string) (TreeListing, error) {
	var body treeBody
	code, raw, err := a.call(ctx, http.MethodGet,
		"/api/v1/repos/"+urlSegment(repo.Owner)+"/"+urlSegment(repo.Name)+
			"/git/trees/"+urlSegment(ref)+"?recursive=true", nil, &body)
	if err != nil {
		return TreeListing{}, err
	}
	if code == http.StatusNotFound {
		return TreeListing{}, ErrNotFound
	}
	if code != http.StatusOK {
		return TreeListing{}, a.mapStatus(code, "get tree", raw)
	}
	entries := make([]TreeEntry, 0, len(body.Tree))
	for _, e := range body.Tree {
		rest, direct := entryUnder(e.Path, prefix)
		if !direct {
			continue
		}
		entries = append(entries, TreeEntry{
			Name: rest,
			Path: e.Path,
			Type: typeForMode(e.Mode),
			Mode: e.Mode,
			Size: e.Size,
			SHA:  e.SHA,
		})
	}
	return TreeListing{SHA: body.SHA, Entries: entries}, nil
}

// entryUnder reports whether path is directly under prefix ("" = the
// repository root) and returns the basename.
func entryUnder(path, prefix string) (name string, direct bool) {
	if prefix == "" {
		if strings.Contains(path, "/") {
			return "", false
		}
		return path, true
	}
	rest, ok := strings.CutPrefix(path, prefix+"/")
	if !ok || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}

// typeForMode maps the git tree mode onto the display vocabulary. The
// mode is authoritative: Gitea's own type field has changed across
// versions and deployment paths, the mode has not.
func typeForMode(mode string) string {
	switch mode {
	case "040000":
		return "tree"
	case "120000":
		return "symlink"
	case "160000":
		return "gitlink"
	default:
		return "blob"
	}
}

// contentsBody is the provider-side contents response (a subset of
// Gitea's ContentsResponse). Content is base64 when Encoding says so;
// Target is the symlink destination / submodule URL when the path is one.
type contentsBody struct {
	Name     string  `json:"name"`
	Path     string  `json:"path"`
	Type     string  `json:"type"`
	Size     int64   `json:"size"`
	SHA      string  `json:"sha"`
	Encoding *string `json:"encoding"`
	Content  *string `json:"content"`
	Target   *string `json:"target"`
}

// GetFileContent implements FilesPort. The caller keeps the blob below
// the preview fetch bound (the response cap is ~4 MiB; a bound-violating
// call would surface as a decode failure, not as content).
func (a *GiteaAdapter) GetFileContent(ctx context.Context, repo Repository, ref, path string) (FileContent, error) {
	var body contentsBody
	code, raw, err := a.call(ctx, http.MethodGet,
		"/api/v1/repos/"+urlSegment(repo.Owner)+"/"+urlSegment(repo.Name)+
			"/contents/"+escapePath(path)+"?ref="+url.QueryEscape(ref), nil, &body)
	if err != nil {
		return FileContent{}, err
	}
	if code == http.StatusNotFound {
		return FileContent{}, ErrNotFound
	}
	if code != http.StatusOK {
		return FileContent{}, a.mapStatus(code, "get file content", raw)
	}
	out := FileContent{
		Path: body.Path,
		Type: body.Type,
		Size: body.Size,
		SHA:  body.SHA,
	}
	if body.Target != nil {
		out.Target = *body.Target
	}
	if body.Content != nil && body.Encoding != nil && *body.Encoding == "base64" {
		decoded, err := base64.StdEncoding.DecodeString(*body.Content)
		if err != nil {
			return FileContent{}, fmt.Errorf("%w: decode file content", ErrUnavailable)
		}
		out.Content = decoded
	}
	return out, nil
}

// escapePath escapes one repository path for the contents/raw routes,
// segment by segment (the path is already validated; this is the same
// defense-in-depth urlSegment is).
func escapePath(path string) string {
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// GetRaw implements FilesPort over the raw route: bytes straight from the
// git backend, no envelope. The response body is the caller's to close.
// The route segment is "commit" for full SHAs and "branch" for names —
// the two forms the raw route answers on this deployment.
func (a *GiteaAdapter) GetRaw(ctx context.Context, repo Repository, ref, path string) (int64, io.ReadCloser, error) {
	kind := "branch"
	if isFullSHA(ref) {
		kind = "commit"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		a.baseURL+"/"+urlSegment(repo.Owner)+"/"+urlSegment(repo.Name)+
			"/raw/"+kind+"/"+urlSegment(ref)+"/"+escapePath(path), nil)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: build raw request", ErrUnavailable)
	}
	req.Header.Set("Authorization", "token "+a.token)
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: provider request failed", ErrUnavailable)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return contentLength(resp), resp.Body, nil
	case http.StatusNotFound:
		_ = resp.Body.Close()
		return 0, nil, ErrNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		_ = resp.Body.Close()
		return 0, nil, ErrUnauthorized
	default:
		_ = resp.Body.Close()
		return 0, nil, fmt.Errorf("%w (status %d)", ErrUnavailable, resp.StatusCode)
	}
}

// contentLength reads the provider's Content-Length defensively: absent
// or unparseable answers 0 (the transport streams without it).
func contentLength(resp *http.Response) int64 {
	if v := resp.Header.Get("Content-Length"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	return 0
}

// GetCommitPatch implements FilesPort over the commit patch route
// (/{owner}/{repo}/commit/{sha}.patch) — the raw diff channel, outside
// /api/v1 exactly like the raw route. Gitea serves this plain-text route
// on every commit page ("download patch"); the adapter authenticates with
// the service token so private repositories answer it too.
func (a *GiteaAdapter) GetCommitPatch(ctx context.Context, repo Repository, sha string) (int64, io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		a.baseURL+"/"+urlSegment(repo.Owner)+"/"+urlSegment(repo.Name)+
			"/commit/"+urlSegment(sha)+".patch", nil)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: build commit patch request", ErrUnavailable)
	}
	req.Header.Set("Authorization", "token "+a.token)
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: provider request failed", ErrUnavailable)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return contentLength(resp), resp.Body, nil
	case http.StatusNotFound:
		_ = resp.Body.Close()
		return 0, nil, ErrNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		_ = resp.Body.Close()
		return 0, nil, ErrUnauthorized
	default:
		_ = resp.Body.Close()
		return 0, nil, fmt.Errorf("%w (status %d)", ErrUnavailable, resp.StatusCode)
	}
}

// commitListBody is the provider-side commits-list element (a subset of
// Gitea's Commit). The list endpoint is the history channel: newest
// first, path-filterable, provider-capped at 50 per page.
type commitListBody struct {
	SHA    string `json:"sha"`
	Commit struct {
		Author struct {
			Name  string    `json:"name"`
			Email string    `json:"email"`
			Date  time.Time `json:"date"`
		} `json:"author"`
		Message string `json:"message"`
	} `json:"commit"`
}

// GetHistory implements FilesPort over the commits list.
func (a *GiteaAdapter) GetHistory(ctx context.Context, repo Repository, ref, path string, limit int) ([]CommitEntry, error) {
	query := "/api/v1/repos/" + urlSegment(repo.Owner) + "/" + urlSegment(repo.Name) +
		"/commits?sha=" + url.QueryEscape(ref) + "&limit=" + strconv.Itoa(limit)
	if path != "" {
		query += "&path=" + url.QueryEscape(path)
	}
	var body []commitListBody
	code, raw, err := a.call(ctx, http.MethodGet, query, nil, &body)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if code != http.StatusOK {
		return nil, a.mapStatus(code, "get commit history", raw)
	}
	out := make([]CommitEntry, 0, len(body))
	for _, c := range body {
		out = append(out, CommitEntry{
			SHA:         c.SHA,
			Author:      c.Commit.Author.Name,
			AuthorEmail: c.Commit.Author.Email,
			Date:        c.Commit.Author.Date,
			Message:     boundedRunes(c.Commit.Message, 1000),
		})
	}
	return out, nil
}

// boundedRunes truncates on rune boundaries (byte-slicing could split a
// multibyte rune and store invalid UTF-8), mirroring providerMessage.
func boundedRunes(s string, max int) string {
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}
