package gitprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

// The read-only Files surface (T0307): git tree listings, file previews,
// commit history and raw downloads — the reads behind the Files page
// (docs/17 §1: Files is a read-only projection over the underlying Git
// tree, never a write path; docs/06: tree, preview, history, download,
// nothing else).
//
// The provider half is FilesPort — a deliberately read-only port: no
// method on it can mutate provider state, so "Files API 无 mutation
// method" is structural (pinned by TestFilesPortIsReadOnly). The service
// half is FilesReader, which layers the preview policy (size caps, text
// sniffing, blob-pointer display) on top of the port. The permission
// filter is NOT here: the transport resolves the caller's project read
// gate (projects.Service) before any provider byte is fetched, exactly
// like the git-token surface — a denied read never reaches the provider.
//
// The Files read path never touches the blobs table: blob pointers are
// displayed from the pointer file's own fields only, so private blob
// metadata (storage key, encryption metadata, access policy, created
// actor — docs/17 §3) cannot leak through this surface (acceptance
// criterion, pinned by TestFileViewCarriesNoBlobStoreMetadata).

// FilesPort is the provider-side read surface of the Files API. It is the
// only shape through which the FilesReader talks to the provider, and it
// is read-only by construction.
type FilesPort interface {
	// GetTree returns the entries directly under prefix ("" = repository
	// root) at ref, plus the resolved commit/tree SHA the listing was read
	// from. ErrNotFound when the ref (or the repository) does not exist.
	GetTree(ctx context.Context, repo Repository, ref, prefix string) (TreeListing, error)
	// GetFileContent reads one blob's content at ref. Callers must keep the
	// blob below the preview fetch bound (FilesReader checks the tree entry
	// first) — the response channel caps the payload regardless.
	// ErrNotFound when the ref or the path does not exist.
	GetFileContent(ctx context.Context, repo Repository, ref, path string) (FileContent, error)
	// GetRaw opens one file as a raw byte stream at ref (the download
	// channel — no JSON envelope, no preview policy). The caller closes
	// Body. ErrNotFound when the ref or the path does not exist.
	GetRaw(ctx context.Context, repo Repository, ref, path string) (size int64, body io.ReadCloser, err error)
	// GetHistory returns the commit history touching path ("" = the whole
	// repository) at ref, newest first, at most limit entries.
	// ErrNotFound when the ref (or the repository) does not exist.
	GetHistory(ctx context.Context, repo Repository, ref, path string, limit int) ([]CommitEntry, error)
	// GetCommitPatch opens one commit's raw patch at sha (the raw diff
	// channel — the provider's patch bytes straight through, never parsed).
	// The caller closes Body. ErrNotFound when the commit (or the
	// repository) does not exist.
	GetCommitPatch(ctx context.Context, repo Repository, sha string) (size int64, body io.ReadCloser, err error)
}

// TreeEntry is one entry of a tree listing.
type TreeEntry struct {
	// Name is the entry's basename; Path is its repository-relative path.
	Name string
	Path string
	// Type is "blob", "tree", "symlink" or "gitlink" — derived from the
	// git mode, never guessed from the name.
	Type string
	// Mode is the raw git mode ("100644", "040000", "120000", "160000").
	Mode string
	// Size is the blob size in bytes (0 for trees; the target-path length
	// for symlinks).
	Size int64
	SHA  string
}

// TreeListing is one tree read: the entries plus the SHA the provider
// resolved the ref to (the client pins later reads to it for history).
type TreeListing struct {
	SHA     string
	Entries []TreeEntry
}

// FileContent is the provider-side answer for one blob read.
type FileContent struct {
	Path string
	// Type is the provider's own classification: "file", "symlink" or
	// "submodule".
	Type string
	Size int64
	SHA  string
	// Content is the decoded blob content (the provider channel encodes it;
	// the adapter hands back raw bytes).
	Content []byte
	// Target is the symlink destination (or submodule URL) when the
	// provider answers one.
	Target string
}

// CommitEntry is one commit of a history listing.
type CommitEntry struct {
	SHA         string
	Author      string
	AuthorEmail string
	Date        time.Time
	Message     string
}

// RawFile is one open raw file stream.
type RawFile struct {
	Size int64
	Body io.ReadCloser
}

// Preview policy bounds (L1, documented here so the transport and the
// frontend share one vocabulary):
//
//	FetchMaxBytes   the largest blob the preview path fetches at all;
//	                above it the answer is kind "too_large" (size+SHA only).
//	PreviewMaxBytes the largest content the preview path returns; larger
//	                content is cut here and flagged truncated.
const (
	// FetchMaxBytes bounds the preview fetch at 1 MiB — well under the
	// adapter's response cap, so a huge file answers "too_large" instead of
	// a provider-read failure.
	FetchMaxBytes = 1 << 20
	// PreviewMaxBytes bounds the returned preview content at 256 KiB.
	PreviewMaxBytes = 256 << 10
	// HistoryDefaultEntries is the history page size when the client asks
	// for none; HistoryMaxEntries is the provider's own page bound.
	HistoryDefaultEntries = 50
	HistoryMaxEntries     = 50
)

// FileView is the service-level answer of one file read: the tree facts
// plus the safe preview, never anything the Git backend does not itself
// carry for that file.
type FileView struct {
	Name string
	Path string
	// Type is "blob", "symlink" or "gitlink".
	Type string
	Mode string
	Size int64
	SHA  string
	// Kind is the preview outcome: "text", "binary", "too_large",
	// "symlink" or "gitlink".
	Kind string
	// Content carries the preview text when Kind == "text" ("" otherwise).
	Content string
	// Truncated reports Content was cut at PreviewMaxBytes.
	Truncated bool
	// Target is the symlink destination when Kind == "symlink".
	Target string
	// BlobPointer is set when the preview parses as a platform blob-pointer
	// manifest (docs/17 §2: Git holds the immutable pointer, the platform
	// holds the blob).
	BlobPointer *BlobPointer
}

// BlobPointer is the displayed form of a blob-pointer manifest. It carries
// exactly the pointer file's own fields — the Files read path never joins
// the blobs table, so nothing beyond these fields can appear here.
type BlobPointer struct {
	ContentHash string
	SizeBytes   int64
	// BlobID is the platform blob id when the manifest carries one.
	BlobID string
}

// Sentinel errors of the Files surface. Callers distinguish "no such
// file/ref" (404), "not provisioned" (409), "bad request" (400) and
// provider failures (503), like every other product surface.
var (
	// ErrInvalidRef: the ref parameter is neither a branch name nor a
	// full commit SHA.
	ErrInvalidRef = errors.New("gitprovider: invalid ref")
	// ErrInvalidSHA: the sha parameter is not a commit SHA (7-40 hex
	// characters).
	ErrInvalidSHA = errors.New("gitprovider: invalid commit sha")
	// ErrInvalidPath: the path parameter cannot name a repository file.
	ErrInvalidPath = errors.New("gitprovider: invalid path")
	// ErrIsDirectory: the path names a tree, not a blob (the content
	// endpoint reads blobs only — the tree endpoint lists directories).
	ErrIsDirectory = errors.New("gitprovider: path is a directory")
)

// RepoResolver is the canonical-store port FilesReader needs: the
// project's provisioned repository identity. PGUserAccessStore implements
// it (RepoRef) — the provisions table is this package's own.
type RepoResolver interface {
	RepoRef(ctx context.Context, projectID string) (RepoRef, error)
}

// FilesReader is the Files API service: repo resolution + provider reads +
// preview policy. Read-only by construction — every method only reads.
type FilesReader struct {
	port  FilesPort
	repos RepoResolver
}

// NewFilesReader wires the reader.
func NewFilesReader(port FilesPort, repos RepoResolver) *FilesReader {
	return &FilesReader{port: port, repos: repos}
}

// resolveRepo maps the project onto its provisioned repository.
func (r *FilesReader) resolveRepo(ctx context.Context, projectID string) (Repository, error) {
	ref, err := r.repos.RepoRef(ctx, projectID)
	if err != nil {
		return Repository{}, err
	}
	return Repository{Owner: ref.Owner, Name: ref.Name}, nil
}

// Tree lists one directory at ref ("" = the repository root, "" for ref =
// the canonical main branch).
func (r *FilesReader) Tree(ctx context.Context, projectID, ref, path string) (TreeListing, error) {
	ref = defaultRef(ref)
	if err := ValidateRef(ref); err != nil {
		return TreeListing{}, err
	}
	if err := ValidatePath(path); err != nil {
		return TreeListing{}, err
	}
	repo, err := r.resolveRepo(ctx, projectID)
	if err != nil {
		return TreeListing{}, err
	}
	return r.port.GetTree(ctx, repo, ref, path)
}

// File reads one blob with the preview policy applied.
func (r *FilesReader) File(ctx context.Context, projectID, ref, path string) (FileView, error) {
	ref = defaultRef(ref)
	if err := ValidateRef(ref); err != nil {
		return FileView{}, err
	}
	if err := ValidatePath(path); err != nil {
		return FileView{}, err
	}
	if path == "" {
		return FileView{}, fmt.Errorf("%w: the content endpoint reads one file; use the tree endpoint to list", ErrInvalidPath)
	}
	repo, err := r.resolveRepo(ctx, projectID)
	if err != nil {
		return FileView{}, err
	}
	// Read only the parent directory (the adapter filters the prefix), so
	// one file read never drags the whole recursive tree along.
	listing, err := r.port.GetTree(ctx, repo, ref, parentDir(path))
	if err != nil {
		return FileView{}, err
	}
	var entry *TreeEntry
	for i := range listing.Entries {
		if listing.Entries[i].Path == path {
			entry = &listing.Entries[i]
			break
		}
	}
	if entry == nil {
		return FileView{}, ErrNotFound
	}
	if entry.Type == "tree" {
		return FileView{}, ErrIsDirectory
	}
	view := FileView{
		Name: entry.Name, Path: entry.Path, Type: entry.Type,
		Mode: entry.Mode, Size: entry.Size, SHA: entry.SHA,
	}
	switch entry.Type {
	case "symlink":
		// The blob content of a symlink IS its target path; read it (it is
		// tiny by construction) and surface the link, never the target's
		// content — previews do not follow links.
		view.Kind = "symlink"
		if fc, err := r.port.GetFileContent(ctx, repo, ref, path); err == nil {
			view.Target = strings.TrimSpace(string(fc.Content))
			if fc.Target != "" {
				view.Target = fc.Target
			}
		}
		return view, nil
	case "gitlink":
		view.Kind = "gitlink"
		return view, nil
	}
	if entry.Size > FetchMaxBytes {
		view.Kind = "too_large"
		return view, nil
	}
	fc, err := r.port.GetFileContent(ctx, repo, ref, path)
	if err != nil {
		return FileView{}, err
	}
	// The tree said blob, the provider may still answer symlink/submodule
	// (races and provider quirks): trust the provider's classification.
	switch fc.Type {
	case "symlink":
		view.Type, view.Kind = "symlink", "symlink"
		view.Target = strings.TrimSpace(string(fc.Content))
		if fc.Target != "" {
			view.Target = fc.Target
		}
		return view, nil
	case "submodule":
		view.Type, view.Kind = "gitlink", "gitlink"
		view.Target = fc.Target
		return view, nil
	}
	if !looksTextual(fc.Content) {
		view.Kind = "binary"
		return view, nil
	}
	content := fc.Content
	view.Truncated = int64(len(content)) > PreviewMaxBytes
	if view.Truncated {
		content = content[:PreviewMaxBytes]
	}
	view.Kind = "text"
	view.Content = string(content)
	// Pointer display: only untruncated text can be a manifest (a cut
	// manifest must not parse as a half-truth).
	if !view.Truncated {
		if ptr, ok := ParseBlobPointer(content); ok {
			view.BlobPointer = ptr
		}
	}
	return view, nil
}

// History lists the commit history at ref touching path ("" = the whole
// repository). limit is clamped to [1, HistoryMaxEntries]; 0 means the
// default page size.
func (r *FilesReader) History(ctx context.Context, projectID, ref, path string, limit int) ([]CommitEntry, error) {
	ref = defaultRef(ref)
	if err := ValidateRef(ref); err != nil {
		return nil, err
	}
	if err := ValidatePath(path); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = HistoryDefaultEntries
	}
	if limit > HistoryMaxEntries {
		limit = HistoryMaxEntries
	}
	repo, err := r.resolveRepo(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return r.port.GetHistory(ctx, repo, ref, path, limit)
}

// Raw opens one file as a raw byte stream (the download channel: the
// transport streams it with an attachment disposition — no envelope, no
// preview policy, no provider metadata).
func (r *FilesReader) Raw(ctx context.Context, projectID, ref, path string) (RawFile, error) {
	ref = defaultRef(ref)
	if err := ValidateRef(ref); err != nil {
		return RawFile{}, err
	}
	if err := ValidatePath(path); err != nil {
		return RawFile{}, err
	}
	if path == "" {
		return RawFile{}, fmt.Errorf("%w: the raw endpoint downloads one file", ErrInvalidPath)
	}
	repo, err := r.resolveRepo(ctx, projectID)
	if err != nil {
		return RawFile{}, err
	}
	size, body, err := r.port.GetRaw(ctx, repo, ref, path)
	if err != nil {
		return RawFile{}, err
	}
	return RawFile{Size: size, Body: body}, nil
}

// CommitPatch opens one commit's raw patch (the raw diff channel, docs/06
// §7: the Files page offers tree, preview, history, raw diff and download —
// this is the raw diff). The sha is validated as a commit SHA only: a
// branch name never reaches the provider here, and the patch itself is
// never parsed — provider bytes stream straight through.
func (r *FilesReader) CommitPatch(ctx context.Context, projectID, sha string) (RawFile, error) {
	sha = strings.ToLower(sha)
	if err := ValidateCommitSHA(sha); err != nil {
		return RawFile{}, err
	}
	repo, err := r.resolveRepo(ctx, projectID)
	if err != nil {
		return RawFile{}, err
	}
	size, body, err := r.port.GetCommitPatch(ctx, repo, sha)
	if err != nil {
		return RawFile{}, err
	}
	return RawFile{Size: size, Body: body}, nil
}

// parentDir returns the parent directory of a repository path ("" for
// root-level files).
func parentDir(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return ""
}

// defaultRef resolves the empty ref to the platform's canonical main
// branch (T0302 seeds it before research begins).
func defaultRef(ref string) string {
	if ref == "" {
		return "main"
	}
	return ref
}

// ValidateRef accepts the empty string (the caller defaults it), a full
// 40-hex commit SHA, or a branch name: path-like segments of letters,
// digits, dot, underscore and hyphen, never empty, never "." / "..",
// never starting with a hyphen (the CLI-shape guard every git input of
// this package carries). Anything else is ErrInvalidRef.
func ValidateRef(ref string) error {
	if ref == "" {
		return nil
	}
	if isFullSHA(ref) {
		return nil
	}
	for _, seg := range strings.Split(ref, "/") {
		if seg == "" || seg == "." || seg == ".." || seg[0] == '-' {
			return fmt.Errorf("%w: %q", ErrInvalidRef, ref)
		}
		for i := 0; i < len(seg); i++ {
			c := seg[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
				c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
				return fmt.Errorf("%w: %q", ErrInvalidRef, ref)
			}
		}
	}
	return nil
}

// ValidateCommitSHA accepts a commit SHA of 7-40 hex characters (the
// range Gitea's commit routes answer — the history channel hands the UI
// full 40-hex SHAs, while abbreviated ones stay valid for hand-built
// links). Anything else is ErrInvalidSHA.
func ValidateCommitSHA(sha string) error {
	if len(sha) < 7 || len(sha) > 40 {
		return fmt.Errorf("%w: %q", ErrInvalidSHA, sha)
	}
	for i := 0; i < len(sha); i++ {
		c := sha[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return fmt.Errorf("%w: %q", ErrInvalidSHA, sha)
		}
	}
	return nil
}

// ValidatePath accepts the empty string (the repository root) and any
// relative path of slash-separated segments where no segment is empty,
// "." or ".." (git tree entries can never be those) and no backslash
// sneaks in. Anything else is ErrInvalidPath.
func ValidatePath(path string) error {
	if path == "" {
		return nil
	}
	if strings.Contains(path, "\\") {
		return fmt.Errorf("%w: %q", ErrInvalidPath, path)
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%w: %q", ErrInvalidPath, path)
		}
	}
	return nil
}

// looksTextual classifies one blob as previewable text: no NUL byte and
// valid UTF-8 on the sampled prefix. Everything else previews as binary —
// the preview is a text surface, and a binary blob is displayed as a
// pointer (name, size, SHA), never decoded.
func looksTextual(b []byte) bool {
	const sample = 8000
	if len(b) > sample {
		b = b[:sample]
	}
	return !bytes.ContainsRune(b, 0) && utf8.Valid(b)
}

// pointerManifest is the V1 blob-pointer file shape (docs/17 §2: large
// data lives in the platform blob store while Git holds the immutable
// pointer). The manifest is a small JSON object:
//
//	{"format": "post-blob-pointer", "content_hash": "sha256:…",
//	 "size_bytes": 123456, "blob_id": "…"}
//
// content_hash and size_bytes are the blobs-table identity pair; blob_id
// is optional (older manifests may not carry it). The "format" marker is
// what makes detection unambiguous: an ordinary JSON file that happens to
// contain hash-like fields is not a pointer.
type pointerManifest struct {
	Format      string `json:"format"`
	ContentHash string `json:"content_hash"`
	SizeBytes   int64  `json:"size_bytes"`
	BlobID      string `json:"blob_id"`
}

// ParseBlobPointer recognizes one blob-pointer manifest and returns its
// display form. Only the pointer file's own fields come back — this is the
// one place the Files surface mentions platform blobs, and it never reads
// the blobs table (private blob metadata stays out by construction).
func ParseBlobPointer(content []byte) (*BlobPointer, bool) {
	var m pointerManifest
	if err := json.Unmarshal(content, &m); err != nil {
		return nil, false
	}
	if m.Format != "post-blob-pointer" || m.ContentHash == "" || m.SizeBytes < 0 {
		return nil, false
	}
	return &BlobPointer{
		ContentHash: m.ContentHash,
		SizeBytes:   m.SizeBytes,
		BlobID:      m.BlobID,
	}, true
}
