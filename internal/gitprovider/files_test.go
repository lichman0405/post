package gitprovider_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/gitprovider"
)

// The FilesReader preview policy (T0307): ref/path validation, size caps,
// text sniffing, symlink/gitlink handling and blob-pointer display — all
// against a scripted port, so the policy is exercised without a provider.

// stubFilesPort scripts the provider half of the FilesPort.
type stubFilesPort struct {
	tree     map[string]gitprovider.TreeListing // key: prefix
	treeErr  error
	files    map[string]gitprovider.FileContent // key: path
	fileErr  error
	raw      map[string]stubRawFile // key: path
	rawErr   error
	patch    map[string]stubRawFile // key: commit sha
	patchErr error
	history  []gitprovider.CommitEntry
	histErr  error

	treeRefs, fileRefs, rawRefs, histRefs []string
	treePrefixes, filePaths, rawPaths     []string
	patchSHAs                             []string
	histPaths                             []string
	histLimits                            []int
	fileCalls                             int
}

type stubRawFile struct {
	size int64
	body string
}

func newStubFilesPort() *stubFilesPort {
	return &stubFilesPort{
		tree:  map[string]gitprovider.TreeListing{},
		files: map[string]gitprovider.FileContent{},
		raw:   map[string]stubRawFile{},
		patch: map[string]stubRawFile{},
	}
}

// stubRepoResolver scripts the canonical-store half.
type stubRepoResolver struct {
	ref   gitprovider.RepoRef
	err   error
	calls int
}

func (s *stubRepoResolver) RepoRef(_ context.Context, _ string) (gitprovider.RepoRef, error) {
	s.calls++
	return s.ref, s.err
}

func (p *stubFilesPort) GetTree(_ context.Context, _ gitprovider.Repository, ref, prefix string) (gitprovider.TreeListing, error) {
	p.treeRefs = append(p.treeRefs, ref)
	p.treePrefixes = append(p.treePrefixes, prefix)
	if p.treeErr != nil {
		return gitprovider.TreeListing{}, p.treeErr
	}
	return p.tree[prefix], nil
}

func (p *stubFilesPort) GetFileContent(_ context.Context, _ gitprovider.Repository, ref, path string) (gitprovider.FileContent, error) {
	p.fileRefs = append(p.fileRefs, ref)
	p.filePaths = append(p.filePaths, path)
	p.fileCalls++
	if p.fileErr != nil {
		return gitprovider.FileContent{}, p.fileErr
	}
	return p.files[path], nil
}

func (p *stubFilesPort) GetRaw(_ context.Context, _ gitprovider.Repository, ref, path string) (int64, io.ReadCloser, error) {
	p.rawRefs = append(p.rawRefs, ref)
	p.rawPaths = append(p.rawPaths, path)
	if p.rawErr != nil {
		return 0, nil, p.rawErr
	}
	f, ok := p.raw[path]
	if !ok {
		return 0, nil, gitprovider.ErrNotFound
	}
	return f.size, io.NopCloser(strings.NewReader(f.body)), nil
}

func (p *stubFilesPort) GetHistory(_ context.Context, _ gitprovider.Repository, ref, path string, limit int) ([]gitprovider.CommitEntry, error) {
	p.histRefs = append(p.histRefs, ref)
	p.histPaths = append(p.histPaths, path)
	p.histLimits = append(p.histLimits, limit)
	if p.histErr != nil {
		return nil, p.histErr
	}
	return p.history, nil
}

func (p *stubFilesPort) GetCommitPatch(_ context.Context, _ gitprovider.Repository, sha string) (int64, io.ReadCloser, error) {
	p.patchSHAs = append(p.patchSHAs, sha)
	if p.patchErr != nil {
		return 0, nil, p.patchErr
	}
	f, ok := p.patch[sha]
	if !ok {
		return 0, nil, gitprovider.ErrNotFound
	}
	return f.size, io.NopCloser(strings.NewReader(f.body)), nil
}

func newReader(port gitprovider.FilesPort, repos gitprovider.RepoResolver) *gitprovider.FilesReader {
	return gitprovider.NewFilesReader(port, repos)
}

func testReader(t *testing.T, port *stubFilesPort, ref gitprovider.RepoRef) *gitprovider.FilesReader {
	t.Helper()
	return newReader(port, &stubRepoResolver{ref: ref})
}

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

// TestValidateRef: branch names and full SHAs pass; anything else is
// ErrInvalidRef — including CLI-shaped hazards. The boundary is per
// segment: exactly "." / ".." are rejected, so dotted segment names like
// "a..b" stay legal branch names.
func TestValidateRef(t *testing.T) {
	valid := []string{
		"", "main", "feature/x", "v1.2-rc_1", "a..b",
		"0123456789abcdef0123456789abcdef01234567",
	}
	invalid := []string{
		"bad ref", "-leading", ".", "..", "/abs", "trail/", "a//b",
		"a\\b", "a/..", "x:y",
	}
	for _, ref := range valid {
		if err := gitprovider.ValidateRef(ref); err != nil {
			t.Errorf("ValidateRef(%q) = %v, want nil", ref, err)
		}
	}
	for _, ref := range invalid {
		if err := gitprovider.ValidateRef(ref); !errors.Is(err, gitprovider.ErrInvalidRef) {
			t.Errorf("ValidateRef(%q) = %v, want ErrInvalidRef", ref, err)
		}
	}
}

// TestValidatePath: relative paths pass; traversal, absolute, empty and
// backslash shapes are ErrInvalidPath.
func TestValidatePath(t *testing.T) {
	valid := []string{"", "f.txt", "a/b/c.txt", "a.b/c-d_e"}
	invalid := []string{"../x", "a/../b", "/abs", "a//b", "a/", "a/.", "a/./b", ".", "..", "a\\b", "a/.."}
	for _, path := range valid {
		if err := gitprovider.ValidatePath(path); err != nil {
			t.Errorf("ValidatePath(%q) = %v, want nil", path, err)
		}
	}
	for _, path := range invalid {
		if err := gitprovider.ValidatePath(path); !errors.Is(err, gitprovider.ErrInvalidPath) {
			t.Errorf("ValidatePath(%q) = %v, want ErrInvalidPath", path, err)
		}
	}
}

// TestParseBlobPointer: the manifest shape parses; near-misses do not —
// the "format" marker plus a non-empty hash are what make a pointer.
func TestParseBlobPointer(t *testing.T) {
	ok := `{"format":"post-blob-pointer","content_hash":"sha256:ab","size_bytes":123,"blob_id":"b1"}`
	ptr, matched := gitprovider.ParseBlobPointer([]byte(ok))
	if !matched {
		t.Fatalf("ParseBlobPointer(ok) did not match")
	}
	if ptr.ContentHash != "sha256:ab" || ptr.SizeBytes != 123 || ptr.BlobID != "b1" {
		t.Errorf("pointer = %+v, want the manifest fields", ptr)
	}

	withoutID := `{"format":"post-blob-pointer","content_hash":"sha256:ab","size_bytes":1}`
	if ptr, matched := gitprovider.ParseBlobPointer([]byte(withoutID)); !matched || ptr.BlobID != "" {
		t.Errorf("pointer without blob_id = %+v/%v, want match with empty id", ptr, matched)
	}

	for name, body := range map[string]string{
		"not json":            "not json at all",
		"no marker":           `{"content_hash":"sha256:ab","size_bytes":1}`,
		"missing hash":        `{"format":"post-blob-pointer","size_bytes":1}`,
		"negative size":       `{"format":"post-blob-pointer","content_hash":"x","size_bytes":-1}`,
		"other format":        `{"format":"other","content_hash":"x","size_bytes":1}`,
		"json array":          `[{"format":"post-blob-pointer"}]`,
		"marker not a string": `{"format":1,"content_hash":"x","size_bytes":1}`,
	} {
		if _, matched := gitprovider.ParseBlobPointer([]byte(body)); matched {
			t.Errorf("ParseBlobPointer(%s) matched, want no match", name)
		}
	}
}

// TestFilesReaderTreeDefaultsRefAndPassesThrough: the empty ref defaults
// to main and the listing rides through.
func TestFilesReaderTreeDefaultsRefAndPassesThrough(t *testing.T) {
	port := newStubFilesPort()
	port.tree[""] = gitprovider.TreeListing{SHA: "abc", Entries: []gitprovider.TreeEntry{
		{Name: "f.txt", Path: "f.txt", Type: "blob", Mode: "100644", Size: 3, SHA: "s"},
	}}
	r := testReader(t, port, gitprovider.RepoRef{ProjectID: "p", Owner: "o", Name: "n"})

	listing, err := r.Tree(context.Background(), "p", "", "")
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if len(listing.Entries) != 1 || listing.SHA != "abc" {
		t.Errorf("listing = %+v, want the scripted one", listing)
	}
	if len(port.treeRefs) != 1 || port.treeRefs[0] != "main" {
		t.Errorf("refs = %v, want [main]", port.treeRefs)
	}
}

// TestFilesReaderTreeRejectsInvalidInput: bad refs and paths never reach
// the port.
func TestFilesReaderTreeRejectsInvalidInput(t *testing.T) {
	port := newStubFilesPort()
	r := testReader(t, port, gitprovider.RepoRef{ProjectID: "p", Owner: "o", Name: "n"})
	if _, err := r.Tree(context.Background(), "p", "bad ref", ""); !errors.Is(err, gitprovider.ErrInvalidRef) {
		t.Errorf("Tree bad ref = %v, want ErrInvalidRef", err)
	}
	if _, err := r.Tree(context.Background(), "p", "main", "../x"); !errors.Is(err, gitprovider.ErrInvalidPath) {
		t.Errorf("Tree bad path = %v, want ErrInvalidPath", err)
	}
	if len(port.treeRefs) != 0 {
		t.Errorf("invalid input reached the port: %v", port.treeRefs)
	}
}

// TestFilesReaderUnprovisioned: the resolver's sentinels ride through.
func TestFilesReaderUnprovisioned(t *testing.T) {
	port := newStubFilesPort()
	r := newReader(port, &stubRepoResolver{err: gitprovider.ErrRepoNotProvisioned})
	if _, err := r.Tree(context.Background(), "p", "main", ""); !errors.Is(err, gitprovider.ErrRepoNotProvisioned) {
		t.Errorf("Tree unprovisioned = %v, want ErrRepoNotProvisioned", err)
	}
	r2 := newReader(port, &stubRepoResolver{err: gitprovider.ErrProjectNotFound})
	if _, err := r2.Tree(context.Background(), "p", "main", ""); !errors.Is(err, gitprovider.ErrProjectNotFound) {
		t.Errorf("Tree unknown project = %v, want ErrProjectNotFound", err)
	}
}

// TestFilesReaderFileTooLarge: a blob over the fetch bound previews as
// too_large and the content channel is never touched.
func TestFilesReaderFileTooLarge(t *testing.T) {
	port := newStubFilesPort()
	port.tree["data"] = gitprovider.TreeListing{Entries: []gitprovider.TreeEntry{
		blobEntry("data/huge.bin", "s1", gitprovider.FetchMaxBytes+1, "blob"),
	}}
	r := testReader(t, port, gitprovider.RepoRef{ProjectID: "p", Owner: "o", Name: "n"})

	view, err := r.File(context.Background(), "p", "main", "data/huge.bin")
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if view.Kind != "too_large" || view.Content != "" || view.Size != gitprovider.FetchMaxBytes+1 {
		t.Errorf("view = %+v, want kind too_large without content", view)
	}
	if port.fileCalls != 0 {
		t.Errorf("content channel called %d times for a too_large file", port.fileCalls)
	}
}

// TestFilesReaderFileTruncated: content between the fetch and preview
// bounds is cut at PreviewMaxBytes and flagged.
func TestFilesReaderFileTruncated(t *testing.T) {
	port := newStubFilesPort()
	port.tree[""] = gitprovider.TreeListing{Entries: []gitprovider.TreeEntry{
		blobEntry("big.txt", "s1", 300*1024, "blob"),
	}}
	port.files["big.txt"] = gitprovider.FileContent{
		Path: "big.txt", Type: "file", Size: 300 * 1024, SHA: "s1",
		Content: []byte(strings.Repeat("x", 300*1024)),
	}
	r := testReader(t, port, gitprovider.RepoRef{ProjectID: "p", Owner: "o", Name: "n"})

	view, err := r.File(context.Background(), "p", "main", "big.txt")
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if view.Kind != "text" || !view.Truncated || len(view.Content) != gitprovider.PreviewMaxBytes {
		t.Errorf("view = kind %s truncated %v len %d, want text/true/%d",
			view.Kind, view.Truncated, len(view.Content), gitprovider.PreviewMaxBytes)
	}
}

// TestFilesReaderFileBinary: NUL bytes (or invalid UTF-8) preview as
// binary — a pointer display (name, size, SHA), never decoded bytes.
func TestFilesReaderFileBinary(t *testing.T) {
	for name, content := range map[string][]byte{
		"nul":     []byte("ab\x00cd"),
		"badutf8": {0xff, 0xfe, 0x00, 0x00},
	} {
		port := newStubFilesPort()
		port.tree[""] = gitprovider.TreeListing{Entries: []gitprovider.TreeEntry{
			blobEntry(name, "s1", int64(len(content)), "blob"),
		}}
		port.files[name] = gitprovider.FileContent{
			Path: name, Type: "file", Size: int64(len(content)), SHA: "s1", Content: content,
		}
		r := testReader(t, port, gitprovider.RepoRef{ProjectID: "p", Owner: "o", Name: "n"})
		view, err := r.File(context.Background(), "p", "main", name)
		if err != nil {
			t.Fatalf("File(%s): %v", name, err)
		}
		if view.Kind != "binary" || view.Content != "" {
			t.Errorf("File(%s) = kind %s content %q, want binary without content", name, view.Kind, view.Content)
		}
	}
}

// TestFilesReaderFilePointerDisplay: a pointer manifest previews as text
// AND as the displayed pointer — exactly the pointer file's own fields,
// nothing from the blobs table (the acceptance criterion: private blob
// metadata does not leak).
func TestFilesReaderFilePointerDisplay(t *testing.T) {
	port := newStubFilesPort()
	port.tree["data"] = gitprovider.TreeListing{Entries: []gitprovider.TreeEntry{
		blobEntry("data/traj.xyz.ptr", "s1", 85, "blob"),
	}}
	port.files["data/traj.xyz.ptr"] = gitprovider.FileContent{
		Path: "data/traj.xyz.ptr", Type: "file", Size: 85, SHA: "s1",
		Content: []byte(`{"format":"post-blob-pointer","content_hash":"sha256:aa","size_bytes":999,"blob_id":"b1"}`),
	}
	r := testReader(t, port, gitprovider.RepoRef{ProjectID: "p", Owner: "o", Name: "n"})

	view, err := r.File(context.Background(), "p", "main", "data/traj.xyz.ptr")
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if view.Kind != "text" || view.BlobPointer == nil {
		t.Fatalf("view = %+v, want text preview with a blob pointer", view)
	}
	if view.BlobPointer.ContentHash != "sha256:aa" || view.BlobPointer.SizeBytes != 999 ||
		view.BlobPointer.BlobID != "b1" {
		t.Errorf("pointer = %+v, want the manifest fields only", view.BlobPointer)
	}
}

// TestFilesReaderFileSymlinkShowsTargetOnly: symlinks surface the link
// target, never the target's content.
func TestFilesReaderFileSymlinkShowsTargetOnly(t *testing.T) {
	port := newStubFilesPort()
	port.tree[""] = gitprovider.TreeListing{Entries: []gitprovider.TreeEntry{
		blobEntry("link", "s1", 7, "symlink"),
	}}
	port.files["link"] = gitprovider.FileContent{
		Path: "link", Type: "symlink", Size: 7, SHA: "s1",
		Content: []byte("big.bin"), Target: "big.bin",
	}
	r := testReader(t, port, gitprovider.RepoRef{ProjectID: "p", Owner: "o", Name: "n"})
	view, err := r.File(context.Background(), "p", "main", "link")
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if view.Kind != "symlink" || view.Target != "big.bin" || view.Content != "" {
		t.Errorf("view = %+v, want symlink with target only", view)
	}
}

// TestFilesReaderFileGitlink: submodule entries surface as gitlinks with
// no provider content fetch at all.
func TestFilesReaderFileGitlink(t *testing.T) {
	port := newStubFilesPort()
	port.tree["vendor"] = gitprovider.TreeListing{Entries: []gitprovider.TreeEntry{
		blobEntry("vendor/lib", "s1", 0, "gitlink"),
	}}
	r := testReader(t, port, gitprovider.RepoRef{ProjectID: "p", Owner: "o", Name: "n"})
	view, err := r.File(context.Background(), "p", "main", "vendor/lib")
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if view.Kind != "gitlink" || port.fileCalls != 0 {
		t.Errorf("view = %+v (content calls %d), want gitlink with no fetch", view, port.fileCalls)
	}
}

// TestFilesReaderFileDirectory: the content endpoint on a tree entry is
// ErrIsDirectory (listing is the tree endpoint's job).
func TestFilesReaderFileDirectory(t *testing.T) {
	port := newStubFilesPort()
	port.tree[""] = gitprovider.TreeListing{Entries: []gitprovider.TreeEntry{
		blobEntry("data", "s1", 0, "tree"),
	}}
	r := testReader(t, port, gitprovider.RepoRef{ProjectID: "p", Owner: "o", Name: "n"})
	if _, err := r.File(context.Background(), "p", "main", "data"); !errors.Is(err, gitprovider.ErrIsDirectory) {
		t.Errorf("File on a directory = %v, want ErrIsDirectory", err)
	}
}

// TestFilesReaderFileMissing: a path outside the tree is ErrNotFound.
func TestFilesReaderFileMissing(t *testing.T) {
	port := newStubFilesPort()
	r := testReader(t, port, gitprovider.RepoRef{ProjectID: "p", Owner: "o", Name: "n"})
	if _, err := r.File(context.Background(), "p", "main", "gone.txt"); !errors.Is(err, gitprovider.ErrNotFound) {
		t.Errorf("File missing = %v, want ErrNotFound", err)
	}
}

// TestFilesReaderHistoryClamps: the page size clamps into
// [1, HistoryMaxEntries] and 0 means the default.
func TestFilesReaderHistoryClamps(t *testing.T) {
	port := newStubFilesPort()
	port.history = []gitprovider.CommitEntry{{SHA: "c1"}}
	r := testReader(t, port, gitprovider.RepoRef{ProjectID: "p", Owner: "o", Name: "n"})
	for in, want := range map[int]int{0: gitprovider.HistoryDefaultEntries, 1: 1, 25: 25, 500: gitprovider.HistoryMaxEntries} {
		if _, err := r.History(context.Background(), "p", "main", "", in); err != nil {
			t.Fatalf("History(%d): %v", in, err)
		}
		got := port.histLimits[len(port.histLimits)-1]
		if got != want {
			t.Errorf("History limit %d → port saw %d, want %d", in, got, want)
		}
	}
}

// TestFilesReaderRawRejectsInvalid: the raw channel validates like every
// other read and refuses an empty path.
func TestFilesReaderRawRejectsInvalid(t *testing.T) {
	port := newStubFilesPort()
	r := testReader(t, port, gitprovider.RepoRef{ProjectID: "p", Owner: "o", Name: "n"})
	if _, err := r.Raw(context.Background(), "p", "main", ""); !errors.Is(err, gitprovider.ErrInvalidPath) {
		t.Errorf("Raw empty path = %v, want ErrInvalidPath", err)
	}
	if _, err := r.Raw(context.Background(), "p", "bad ref", "f.txt"); !errors.Is(err, gitprovider.ErrInvalidRef) {
		t.Errorf("Raw bad ref = %v, want ErrInvalidRef", err)
	}
	if len(port.rawRefs) != 0 {
		t.Errorf("invalid input reached the port: %v", port.rawRefs)
	}
}

// TestFilesReaderRawStreams: a valid raw read passes the stream through.
func TestFilesReaderRawStreams(t *testing.T) {
	port := newStubFilesPort()
	port.raw["f.bin"] = stubRawFile{size: 9, body: "raw-bytes"}
	r := testReader(t, port, gitprovider.RepoRef{ProjectID: "p", Owner: "o", Name: "n"})
	raw, err := r.Raw(context.Background(), "p", "", "f.bin")
	if err != nil {
		t.Fatalf("Raw: %v", err)
	}
	got, _ := io.ReadAll(raw.Body)
	_ = raw.Body.Close()
	if string(got) != "raw-bytes" || raw.Size != 9 {
		t.Errorf("raw = %q size %d, want raw-bytes/9", got, raw.Size)
	}
	if port.rawRefs[0] != "main" {
		t.Errorf("raw ref = %q, want the main default", port.rawRefs[0])
	}
}

// TestValidateCommitSHA: 7-40 hex characters pass; branch names, short
// abbreviations and non-hex shapes are ErrInvalidSHA.
func TestValidateCommitSHA(t *testing.T) {
	valid := []string{
		"abcdef1", "abcdef1234567890",
		"0123456789abcdef0123456789abcdef01234567",
	}
	invalid := []string{
		"", "main", "abc123", "abcdefg1", "xyz1234", "abc-def1",
		"0123456789abcdef0123456789abcdef012345678", // 41 chars
	}
	for _, sha := range valid {
		if err := gitprovider.ValidateCommitSHA(sha); err != nil {
			t.Errorf("ValidateCommitSHA(%q) = %v, want nil", sha, err)
		}
	}
	for _, sha := range invalid {
		if err := gitprovider.ValidateCommitSHA(sha); !errors.Is(err, gitprovider.ErrInvalidSHA) {
			t.Errorf("ValidateCommitSHA(%q) = %v, want ErrInvalidSHA", sha, err)
		}
	}
}

// TestFilesReaderCommitPatchStreams: a valid sha streams the provider
// patch bytes through; uppercase shas normalize before the port call.
func TestFilesReaderCommitPatchStreams(t *testing.T) {
	const sha = "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0"
	port := newStubFilesPort()
	port.patch[sha] = stubRawFile{size: 11, body: "patch-bytes"}
	r := testReader(t, port, gitprovider.RepoRef{ProjectID: "p", Owner: "o", Name: "n"})
	raw, err := r.CommitPatch(context.Background(), "p", strings.ToUpper(sha))
	if err != nil {
		t.Fatalf("CommitPatch: %v", err)
	}
	got, _ := io.ReadAll(raw.Body)
	_ = raw.Body.Close()
	if string(got) != "patch-bytes" || raw.Size != 11 {
		t.Errorf("patch = %q size %d, want patch-bytes/11", got, raw.Size)
	}
	if port.patchSHAs[0] != sha {
		t.Errorf("port saw sha %q, want the normalized lowercase %q", port.patchSHAs[0], sha)
	}
}

// TestFilesReaderCommitPatchRejectsInvalid: a non-commit sha never
// reaches the provider.
func TestFilesReaderCommitPatchRejectsInvalid(t *testing.T) {
	port := newStubFilesPort()
	r := testReader(t, port, gitprovider.RepoRef{ProjectID: "p", Owner: "o", Name: "n"})
	for _, sha := range []string{"", "main", "abc123"} {
		if _, err := r.CommitPatch(context.Background(), "p", sha); !errors.Is(err, gitprovider.ErrInvalidSHA) {
			t.Errorf("CommitPatch(%q) = %v, want ErrInvalidSHA", sha, err)
		}
	}
	if len(port.patchSHAs) != 0 {
		t.Errorf("invalid sha reached the port: %v", port.patchSHAs)
	}
}

// TestFilesPortIsReadOnly pins the acceptance criterion "Files API 无
// mutation method" at the port level: the interface carries exactly the
// five read methods, so a mutating provider call cannot be added without
// this test failing.
func TestFilesPortIsReadOnly(t *testing.T) {
	typ := reflect.TypeOf((*gitprovider.FilesPort)(nil)).Elem()
	allowed := map[string]bool{
		"GetTree": true, "GetFileContent": true, "GetRaw": true, "GetHistory": true,
		"GetCommitPatch": true,
	}
	if typ.NumMethod() != len(allowed) {
		t.Fatalf("FilesPort has %d methods, want exactly %d: %v", typ.NumMethod(), len(allowed), methodNames(typ))
	}
	for i := 0; i < typ.NumMethod(); i++ {
		if name := typ.Method(i).Name; !allowed[name] {
			t.Errorf("FilesPort method %q is not one of the read set — a mutation method has no place here", name)
		}
	}
}

// TestFilesReaderIsReadOnly pins the same criterion at the service level.
func TestFilesReaderIsReadOnly(t *testing.T) {
	typ := reflect.TypeOf(&gitprovider.FilesReader{})
	allowed := map[string]bool{"Tree": true, "File": true, "History": true, "Raw": true, "CommitPatch": true}
	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i)
		if m.PkgPath != "" {
			continue // unexported helpers (resolveRepo etc.)
		}
		if name := m.Name; !allowed[name] {
			t.Errorf("FilesReader method %q is not one of the read set — a mutation method has no place here", name)
		}
	}
}

func methodNames(typ reflect.Type) []string {
	var out []string
	for i := 0; i < typ.NumMethod(); i++ {
		out = append(out, typ.Method(i).Name)
	}
	return out
}

// TestFileViewCarriesNoBlobStoreMetadata pins the acceptance criterion
// "private blob metadata 不泄漏" structurally: the FileView type (the
// service-level answer of a file read) has no field that could carry
// storage keys, encryption metadata, created-actor or access-policy facts
// — the fields a blobs-table join would bring. The Files read path never
// queries the blobs table, and this test keeps the wire type honest.
func TestFileViewCarriesNoBlobStoreMetadata(t *testing.T) {
	typ := reflect.TypeOf(gitprovider.FileView{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		name := strings.ToLower(field.Name)
		for _, forbidden := range []string{"storage", "encrypt", "key", "policy", "actor", "created", "integrity"} {
			if strings.Contains(name, forbidden) {
				t.Errorf("FileView field %q can carry private blob metadata — it must not exist on the Files read path", field.Name)
			}
		}
	}
	ptr := reflect.TypeOf(gitprovider.BlobPointer{})
	for i := 0; i < ptr.NumField(); i++ {
		if name := strings.ToLower(ptr.Field(i).Name); strings.Contains(name, "key") ||
			strings.Contains(name, "storage") || strings.Contains(name, "policy") {
			t.Errorf("BlobPointer field %q can carry private blob metadata", ptr.Field(i).Name)
		}
	}
	// The pointer display is exactly the manifest's own fields.
	if ptr.NumField() != 3 {
		t.Errorf("BlobPointer has %d fields, want exactly content_hash/size_bytes/blob_id", ptr.NumField())
	}
}

// TestFilesReaderFileProviderErrorsRideThrough: provider sentinels surface
// unmangled so the transport maps them.
func TestFilesReaderFileProviderErrorsRideThrough(t *testing.T) {
	port := newStubFilesPort()
	port.treeErr = gitprovider.ErrNotFound
	r := testReader(t, port, gitprovider.RepoRef{ProjectID: "p", Owner: "o", Name: "n"})
	if _, err := r.File(context.Background(), "p", "main", "x.txt"); !errors.Is(err, gitprovider.ErrNotFound) {
		t.Errorf("File with provider ErrNotFound = %v, want it unmangled", err)
	}
	port2 := newStubFilesPort()
	port2.histErr = gitprovider.ErrNotFound
	r2 := testReader(t, port2, gitprovider.RepoRef{ProjectID: "p", Owner: "o", Name: "n"})
	if _, err := r2.History(context.Background(), "p", "main", "", 50); !errors.Is(err, gitprovider.ErrNotFound) {
		t.Errorf("History with provider ErrNotFound = %v, want it unmangled", err)
	}
}

// TestFilesReaderFileResolvesThroughParentDir: one file read lists only
// the parent directory, not the repository root.
func TestFilesReaderFileResolvesThroughParentDir(t *testing.T) {
	port := newStubFilesPort()
	port.tree["data"] = gitprovider.TreeListing{Entries: []gitprovider.TreeEntry{
		blobEntry("data/f.txt", "s1", 3, "blob"),
	}}
	port.files["data/f.txt"] = gitprovider.FileContent{
		Path: "data/f.txt", Type: "file", Size: 3, SHA: "s1", Content: []byte("hi\n"),
	}
	r := testReader(t, port, gitprovider.RepoRef{ProjectID: "p", Owner: "o", Name: "n"})
	if _, err := r.File(context.Background(), "p", "main", "data/f.txt"); err != nil {
		t.Fatalf("File: %v", err)
	}
	if len(port.treePrefixes) != 1 || port.treePrefixes[0] != "data" {
		t.Errorf("tree prefixes = %v, want exactly [data] — one file read must not list the repository root", port.treePrefixes)
	}
}

// TestLooksTextualBoundaries: the sniff samples the prefix, so a text file
// with a binary tail still previews (and truncation covers the rest).
func TestLooksTextualBoundaries(t *testing.T) {
	big := strings.Repeat("a", gitprovider.PreviewMaxBytes) + "\x00tail"
	port := newStubFilesPort()
	port.tree[""] = gitprovider.TreeListing{Entries: []gitprovider.TreeEntry{
		blobEntry("f.txt", "s1", int64(len(big)), "blob"),
	}}
	port.files["f.txt"] = gitprovider.FileContent{
		Path: "f.txt", Type: "file", Size: int64(len(big)), SHA: "s1", Content: []byte(big),
	}
	r := testReader(t, port, gitprovider.RepoRef{ProjectID: "p", Owner: "o", Name: "n"})
	view, err := r.File(context.Background(), "p", "main", "f.txt")
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if view.Kind != "text" || !view.Truncated {
		t.Errorf("view = kind %s truncated %v, want text/truncated", view.Kind, view.Truncated)
	}
	if strings.ContainsRune(view.Content, 0) {
		t.Errorf("preview carries a NUL byte — the sampled prefix should be text")
	}
}

// TestFilesReaderHistoryValidation: invalid input never reaches the port.
func TestFilesReaderHistoryValidation(t *testing.T) {
	port := newStubFilesPort()
	r := testReader(t, port, gitprovider.RepoRef{ProjectID: "p", Owner: "o", Name: "n"})
	if _, err := r.History(context.Background(), "p", "bad ref", "", 50); !errors.Is(err, gitprovider.ErrInvalidRef) {
		t.Errorf("History bad ref = %v, want ErrInvalidRef", err)
	}
	if _, err := r.History(context.Background(), "p", "main", "../x", 50); !errors.Is(err, gitprovider.ErrInvalidPath) {
		t.Errorf("History bad path = %v, want ErrInvalidPath", err)
	}
	if len(port.histRefs) != 0 {
		t.Errorf("invalid input reached the port: %v", port.histRefs)
	}
}

// TestFilesReaderFileErrorsFromMissingContentChannel: a content fetch
// failure rides through (never a silent empty preview).
func TestFilesReaderFileErrorsFromMissingContentChannel(t *testing.T) {
	port := newStubFilesPort()
	port.tree[""] = gitprovider.TreeListing{Entries: []gitprovider.TreeEntry{
		blobEntry("f.txt", "s1", 3, "blob"),
	}}
	port.fileErr = fmt.Errorf("%w: gone", gitprovider.ErrNotFound)
	r := testReader(t, port, gitprovider.RepoRef{ProjectID: "p", Owner: "o", Name: "n"})
	if _, err := r.File(context.Background(), "p", "main", "f.txt"); !errors.Is(err, gitprovider.ErrNotFound) {
		t.Errorf("File with content ErrNotFound = %v, want it unmangled", err)
	}
}
