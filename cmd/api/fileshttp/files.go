package fileshttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/gitprovider"
	"github.com/lichman0405/post/internal/observability"
)

// The Files HTTP surface (T0307): the read-only Files API over the
// platform's git backend — tree listings, file previews, commit history
// and raw downloads (docs/06: tree, preview, history, download; docs/17:
// Files is a read-only projection, never a write path).
//
// Read-only is structural here: only GET routes are registered, and the
// mux answers every other method with 405 before any handler runs
// (pinned by TestMutationMethodsRejected). The permission filter is the
// shared project read gate — the same projects.Service instance that
// serves /api/v1/projects — resolved through the guard's principal, so
// anonymous callers read public projects only (public_policy) while
// private projects stay existence-hidden behind the standard 404
// (authorized = member). No provider byte is fetched before that gate
// has passed.

// Wire error codes (docs/45): stable codes on the standard envelope.
const (
	codeDisabled            = "FILES_DISABLED"
	codeProjectNotFound     = projects.CodeProjectNotFound
	codeNotProvisioned      = "PROJECT_NOT_PROVISIONED"
	codeInvalidRef          = "FILES_INVALID_REF"
	codeInvalidSHA          = "FILES_INVALID_SHA"
	codeInvalidPath         = "FILES_INVALID_PATH"
	codeInvalidLimit        = "FILES_INVALID_LIMIT"
	codePathIsDirectory     = "FILES_PATH_IS_DIRECTORY"
	codeNotFound            = "FILES_NOT_FOUND"
	codeProviderUnavailable = "GIT_PROVIDER_UNAVAILABLE"
)

// Deps carries the collaborators the files surface needs.
type Deps struct {
	// Projects is the projects service instance that serves
	// /api/v1/projects — shared on purpose: the files of a project are
	// exactly as visible as the project itself.
	Projects *projects.Service
	// Files is the T0307 read service. Nil means the feature is disabled
	// (missing configuration): every route answers 503 naming the missing
	// keys instead of a misleading 404.
	Files *gitprovider.FilesReader
	// Missing names the configuration keys that disabled the feature
	// (key names only — never values).
	Missing []string
}

// API is the mounted files surface.
type API struct {
	handlers *handlers
}

type handlers struct {
	projects *projects.Service
	files    *gitprovider.FilesReader
	missing  []string
}

// New wires the surface.
func New(deps Deps) *API {
	return &API{handlers: &handlers{
		projects: deps.Projects,
		files:    deps.Files,
		missing:  deps.Missing,
	}}
}

// Register mounts the files routes on the guarded /api/v1 mux. Only GET
// routes exist — the Files API has no mutation methods, structurally: the
// Go 1.22+ mux answers 405 for every other method on these paths.
func (a *API) Register(mux *http.ServeMux) {
	h := a.handlers
	mux.HandleFunc("GET /api/v1/projects/{projectId}/files/tree", h.handleTree)
	mux.HandleFunc("GET /api/v1/projects/{projectId}/files/content", h.handleContent)
	mux.HandleFunc("GET /api/v1/projects/{projectId}/files/history", h.handleHistory)
	mux.HandleFunc("GET /api/v1/projects/{projectId}/files/raw", h.handleRaw)
	mux.HandleFunc("GET /api/v1/projects/{projectId}/files/diff", h.handleDiff)
}

// enabled answers 503 in place when the feature is disabled, returning
// false — the same fail-loud convention as the git-token surface.
func (h *handlers) enabled(w http.ResponseWriter, r *http.Request) bool {
	if h.files != nil {
		return true
	}
	authhttp.WriteError(w, r, http.StatusServiceUnavailable, codeDisabled,
		"files reads are disabled: missing configuration "+strings.Join(h.missing, ", "))
	return false
}

// authorize runs the permission filter: the shared project read gate.
// Anonymous callers read public projects only; private projects answer
// the existence-hiding 404 for everyone who is not a member (the same
// shape as the projects surface — read_files public_policy / authorized
// resolution, specs/policies/permissions-matrix.csv).
func (h *handlers) authorize(w http.ResponseWriter, r *http.Request, projectID string) bool {
	if _, err := h.projects.Get(r.Context(), reader(r), projectID); err != nil {
		h.projectsError(w, r, err)
		return false
	}
	return true
}

// reader mirrors projectshttp's transport-layer Reader resolution:
// a resolved session makes the caller authenticated, its absence makes
// them anonymous (reads never 401 — the service hides what it must).
func reader(r *http.Request) projects.Reader {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		return projects.Reader{}
	}
	return projects.Reader{UserID: p.User.ID, Authenticated: true}
}

// treePayload is the tree-listing envelope. The resolved SHA lets the
// client pin follow-up reads (history) to the exact listing it saw.
type treePayload struct {
	Ref     string             `json:"ref"`
	Path    string             `json:"path"`
	SHA     string             `json:"sha"`
	Entries []treeEntryPayload `json:"entries"`
}

type treeEntryPayload struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"`
	Mode string `json:"mode"`
	Size int64  `json:"size"`
	SHA  string `json:"sha"`
}

// filePayload is the single-file envelope: tree facts plus the safe
// preview. It carries nothing but the file's own git facts and (for
// pointer manifests) the pointer's own fields — private blob metadata
// never appears here (acceptance criterion, docs/17 §3).
type filePayload struct {
	Name        string              `json:"name"`
	Path        string              `json:"path"`
	Type        string              `json:"type"`
	Mode        string              `json:"mode"`
	Size        int64               `json:"size"`
	SHA         string              `json:"sha"`
	Kind        string              `json:"kind"` // text | binary | too_large | symlink | gitlink
	Content     string              `json:"content"`
	Truncated   bool                `json:"truncated"`
	Target      string              `json:"target,omitempty"`
	BlobPointer *blobPointerPayload `json:"blob_pointer,omitempty"`
}

type blobPointerPayload struct {
	ContentHash string `json:"content_hash"`
	SizeBytes   int64  `json:"size_bytes"`
	BlobID      string `json:"blob_id,omitempty"`
}

type historyPayload struct {
	Ref     string          `json:"ref"`
	Path    string          `json:"path"`
	Entries []commitPayload `json:"entries"`
}

type commitPayload struct {
	SHA         string    `json:"sha"`
	Author      string    `json:"author"`
	AuthorEmail string    `json:"author_email"`
	Date        time.Time `json:"date"`
	Message     string    `json:"message"`
}

// handleTree: GET /api/v1/projects/{projectId}/files/tree?ref=&path= —
// one directory listing at ref ("" = main, "" path = root).
func (h *handlers) handleTree(w http.ResponseWriter, r *http.Request) {
	if !h.enabled(w, r) {
		return
	}
	projectID := r.PathValue("projectId")
	if !h.authorize(w, r, projectID) {
		return
	}
	ref, path := r.URL.Query().Get("ref"), r.URL.Query().Get("path")
	listing, err := h.files.Tree(r.Context(), projectID, ref, path)
	if err != nil {
		h.filesError(w, r, err)
		return
	}
	payload := treePayload{
		Ref:     defaultedRef(ref),
		Path:    path,
		SHA:     listing.SHA,
		Entries: make([]treeEntryPayload, 0, len(listing.Entries)),
	}
	for _, e := range listing.Entries {
		payload.Entries = append(payload.Entries, treeEntryPayload{
			Name: e.Name, Path: e.Path, Type: e.Type, Mode: e.Mode, Size: e.Size, SHA: e.SHA,
		})
	}
	authhttp.WriteJSON(w, http.StatusOK, payload)
}

// handleContent: GET /api/v1/projects/{projectId}/files/content?ref=&path=
// — one file's safe preview. path is required (a directory answers 400:
// listing is the tree endpoint's job).
func (h *handlers) handleContent(w http.ResponseWriter, r *http.Request) {
	if !h.enabled(w, r) {
		return
	}
	projectID := r.PathValue("projectId")
	if !h.authorize(w, r, projectID) {
		return
	}
	ref, path := r.URL.Query().Get("ref"), r.URL.Query().Get("path")
	view, err := h.files.File(r.Context(), projectID, ref, path)
	if err != nil {
		h.filesError(w, r, err)
		return
	}
	payload := filePayload{
		Name:      view.Name,
		Path:      view.Path,
		Type:      view.Type,
		Mode:      view.Mode,
		Size:      view.Size,
		SHA:       view.SHA,
		Kind:      view.Kind,
		Content:   view.Content,
		Truncated: view.Truncated,
		Target:    view.Target,
	}
	if view.BlobPointer != nil {
		payload.BlobPointer = &blobPointerPayload{
			ContentHash: view.BlobPointer.ContentHash,
			SizeBytes:   view.BlobPointer.SizeBytes,
			BlobID:      view.BlobPointer.BlobID,
		}
	}
	authhttp.WriteJSON(w, http.StatusOK, payload)
}

// handleHistory: GET /api/v1/projects/{projectId}/files/history?ref=&path=&limit=
// — the commit history touching path at ref, newest first.
func (h *handlers) handleHistory(w http.ResponseWriter, r *http.Request) {
	if !h.enabled(w, r) {
		return
	}
	projectID := r.PathValue("projectId")
	if !h.authorize(w, r, projectID) {
		return
	}
	ref, path := r.URL.Query().Get("ref"), r.URL.Query().Get("path")
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			authhttp.WriteError(w, r, http.StatusBadRequest, codeInvalidLimit,
				"limit must be a positive integer")
			return
		}
		limit = n
	}
	entries, err := h.files.History(r.Context(), projectID, ref, path, limit)
	if err != nil {
		h.filesError(w, r, err)
		return
	}
	payload := historyPayload{
		Ref:     defaultedRef(ref),
		Path:    path,
		Entries: make([]commitPayload, 0, len(entries)),
	}
	for _, e := range entries {
		payload.Entries = append(payload.Entries, commitPayload{
			SHA: e.SHA, Author: e.Author, AuthorEmail: e.AuthorEmail,
			Date: e.Date, Message: e.Message,
		})
	}
	authhttp.WriteJSON(w, http.StatusOK, payload)
}

// handleRaw: GET /api/v1/projects/{projectId}/files/raw?ref=&path= — the
// download channel: provider bytes streamed straight through, with a
// browser-safe disposition. No envelope, no metadata, no execution
// context: Content-Type is octet-stream and the attachment disposition
// plus nosniff keep the payload from ever rendering in a browser context.
func (h *handlers) handleRaw(w http.ResponseWriter, r *http.Request) {
	if !h.enabled(w, r) {
		return
	}
	projectID := r.PathValue("projectId")
	if !h.authorize(w, r, projectID) {
		return
	}
	ref, path := r.URL.Query().Get("ref"), r.URL.Query().Get("path")
	raw, err := h.files.Raw(r.Context(), projectID, ref, path)
	if err != nil {
		h.filesError(w, r, err)
		return
	}
	defer raw.Body.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, safeFilename(path)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if raw.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(raw.Size, 10))
	}
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, raw.Body); err != nil {
		observability.LoggerFromContext(r.Context()).Warn(
			"files: raw stream interrupted", "error", err.Error())
	}
}

// handleDiff: GET /api/v1/projects/{projectId}/files/diff?sha= — one
// commit's raw patch, streamed through unparsed (the raw diff view). The
// disposition stays inline so the patch renders as plain text in the
// browser, and nosniff keeps the payload from ever being sniffed into
// anything executable.
func (h *handlers) handleDiff(w http.ResponseWriter, r *http.Request) {
	if !h.enabled(w, r) {
		return
	}
	projectID := r.PathValue("projectId")
	if !h.authorize(w, r, projectID) {
		return
	}
	sha := r.URL.Query().Get("sha")
	raw, err := h.files.CommitPatch(r.Context(), projectID, sha)
	if err != nil {
		h.filesError(w, r, err)
		return
	}
	defer raw.Body.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if raw.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(raw.Size, 10))
	}
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, raw.Body); err != nil {
		observability.LoggerFromContext(r.Context()).Warn(
			"files: diff stream interrupted", "error", err.Error())
	}
}

// safeFilename reduces one repository path to a Content-Disposition-safe
// basename: separators and anything outside [A-Za-z0-9._-] collapse to
// "-", and an empty result becomes "download". Path values are validated
// before this point; this is the header-injection guard, not validation.
func safeFilename(path string) string {
	base := path
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	var b strings.Builder
	for i := 0; i < len(base); i++ {
		c := base[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
			c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-' {
			b.WriteByte(c)
		} else {
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "download"
	}
	return b.String()
}

// defaultedRef mirrors the service's empty-ref default so the envelope
// echoes the ref actually read.
func defaultedRef(ref string) string {
	if ref == "" {
		return "main"
	}
	return ref
}

// projectsError maps projects-service errors with the same codes the
// projects surface uses (a shared service deserves a shared vocabulary).
// The denied read of a private project is the existence-hiding 404 —
// indistinguishable from an unknown project, by design (docs/45).
func (h *handlers) projectsError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, projects.ErrProjectNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, codeProjectNotFound,
			"project not found")
	case errors.Is(err, projects.ErrForbidden):
		authhttp.WriteError(w, r, http.StatusForbidden, codeProjectNotFound,
			"you may not read this project")
	default:
		observability.LoggerFromContext(r.Context()).Error(
			"files: project lookup failed", "error", err.Error())
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE",
			"the project store could not answer")
	}
}

// filesError maps service/provider errors to the wire. Provider
// credential rejections are an operator problem (the service token is
// wrong) — 503 naming it, never a 401 to the caller.
func (h *handlers) filesError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, gitprovider.ErrInvalidRef):
		authhttp.WriteError(w, r, http.StatusBadRequest, codeInvalidRef,
			"invalid ref: use a branch name or a full commit SHA")
	case errors.Is(err, gitprovider.ErrInvalidSHA):
		authhttp.WriteError(w, r, http.StatusBadRequest, codeInvalidSHA,
			"invalid sha: use a commit SHA (7-40 hex characters)")
	case errors.Is(err, gitprovider.ErrInvalidPath):
		authhttp.WriteError(w, r, http.StatusBadRequest, codeInvalidPath, err.Error())
	case errors.Is(err, gitprovider.ErrIsDirectory):
		authhttp.WriteError(w, r, http.StatusBadRequest, codePathIsDirectory,
			"the path names a directory; list it with the tree endpoint")
	case errors.Is(err, gitprovider.ErrProjectNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, codeProjectNotFound,
			"project not found")
	case errors.Is(err, gitprovider.ErrRepoNotProvisioned):
		authhttp.WriteError(w, r, http.StatusConflict, codeNotProvisioned,
			"the project's repository is not provisioned yet — retry after provisioning completes")
	case errors.Is(err, gitprovider.ErrNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, codeNotFound,
			"no such file, directory or ref in the project's repository")
	case errors.Is(err, gitprovider.ErrUnauthorized):
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, codeProviderUnavailable,
			"the git provider rejected the platform's credentials — check POST_GITEA_TOKEN")
	case errors.Is(err, context.Canceled):
		// A client that goes away mid-read is not a server error to log.
		authhttp.WriteError(w, r, http.StatusRequestTimeout, "REQUEST_CANCELED",
			"the request was canceled")
	default:
		observability.LoggerFromContext(r.Context()).Error(
			"files: read failed", "error", err.Error())
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, codeProviderUnavailable,
			"the git provider could not answer")
	}
}
