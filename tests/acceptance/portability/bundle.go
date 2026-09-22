package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lichman0405/post/internal/application/releases"
	"github.com/lichman0405/post/internal/rsg/manifest"
)

// The portable bundle's on-disk shape.
//
// A bundle directory holds exactly what CLAUDE.md §8 says a Release pins,
// each class in its own file so a reader can see the four at a glance:
//
//	EXPORT.json            the index: the four identifier classes, the blob
//	                       rows, and a digest for every other file
//	manifest.json          the RSG snapshot: internal/rsg/manifest's own
//	                       canonical export document of the released state
//	release-manifest.json  releases.manifest, byte for byte as stored
//	state.json             the PostgreSQL rows the state's truth is rebuilt
//	                       from (the relational half of §8)
//	git.bundle             the project repository's refs and history, a real
//	                       git bundle made by the real git CLI
//	blobs/<content_hash>   the large-object bytes, one file per blob, named
//	                       by the content hash they are pinned by
//
// What is deliberately NOT here: any identifier's *source of truth* other
// than the product's own artifact. manifest.json is the product's export
// document, release-manifest.json is the product's stored release document,
// and the digest inside EXPORT.json is a transport digest — it protects the
// bytes on the way, it never replaces the product's own content addressing.
// That distinction is what the three tamper classes exercise.
const (
	// FormatV1 is this bundle layout's version. A future layout is a new
	// version, never a silent reinterpretation of this one.
	FormatV1 = "portability-v1"

	indexFile   = "EXPORT.json"
	manifestF   = "manifest.json"
	releaseF    = "release-manifest.json"
	stateF      = "state.json"
	gitBundleF  = "git.bundle"
	blobsDir    = "blobs"
	blobFilesGl = blobsDir + "/*"
)

// FileEntry is one file of the bundle with the digest the index recorded for
// it. The digest is computed over the file's bytes as written.
type FileEntry struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// BlobRow is one blob as the export side recorded it. StorageKey is the
// source's addressing of the bytes and is carried for the reader's benefit
// only: every comparison in this driver is by ContentHash, never by path
// (requirement: "blob 逐字节一致（按内容 hash 比，不按路径比）").
type BlobRow struct {
	ID          string `json:"id"`
	ContentHash string `json:"content_hash"`
	SizeBytes   int64  `json:"size_bytes"`
	MediaType   string `json:"media_type"`
	StorageKey  string `json:"storage_key"`
}

// GitID is the Git half of the four identifier classes.
//
// Repository/Refs/CommitSHA come from the project's repository in the real
// GitProvider, read back with `git ls-remote`/the bundle rather than from a
// database column. StateGitRef is the RSG manifest's own git_ref field,
// carried verbatim — it is the state-to-commit pin, a different fact from
// the repository's head, and this fixture has it null (see the RESULT's
// honest-report section: push ingestion is the only writer of
// project_states.git_commit_sha).
type GitID struct {
	Repository  string            `json:"repository"`
	Refs        map[string]string `json:"refs"`
	HeadRef     string            `json:"head_ref"`
	CommitSHA   string            `json:"commit_sha"`
	StateGitRef *string           `json:"state_git_ref"`
}

// IdentifierSet is the four classes the acceptance criterion asks to be
// printed for both sides and asserted equal.
type IdentifierSet struct {
	RSGStateHash        string   `json:"rsg_state_hash"`
	ReleaseManifestHash string   `json:"release_manifest_hash"`
	Git                 GitID    `json:"git"`
	BlobHashes          []string `json:"blob_hashes"`
	SchemaVersions      []string `json:"schema_versions"`
	PolicyVersions      []string `json:"policy_versions"`
}

// Index is EXPORT.json. GeneratedAt is export-time metadata: it moves
// between exports of the same project and is excluded from ContentDigest,
// exactly as manifest.generated_at is excluded from a state hash.
type Index struct {
	FormatVersion  string        `json:"format_version"`
	ProjectID      string        `json:"project_id"`
	ProjectSlug    string        `json:"project_slug"`
	ReleaseID      string        `json:"release_id"`
	ReleaseVersion string        `json:"release_version"`
	StateID        string        `json:"state_id"`
	GeneratedAt    time.Time     `json:"generated_at"`
	Identifiers    IdentifierSet `json:"identifiers"`
	Blobs          []BlobRow     `json:"blobs"`
	Files          []FileEntry   `json:"files"`
}

// indexContent is the repeatability input: the index minus generated_at.
//
// Files is excluded as well, and for the same reason rather than as a
// convenience: an entry in Files is a transport digest of a file, and
// manifest.json embeds the product's own generated_at — a value that moves by
// design on every export (internal/rsg/manifest excludes it from state_hash
// for exactly that reason). What makes two exports "the same content" is
// therefore the identifiers, which ARE the content addresses, plus the one
// document whose bytes are expected to repeat exactly.
type indexContent struct {
	FormatVersion  string        `json:"format_version"`
	ProjectID      string        `json:"project_id"`
	ProjectSlug    string        `json:"project_slug"`
	ReleaseID      string        `json:"release_id"`
	ReleaseVersion string        `json:"release_version"`
	StateID        string        `json:"state_id"`
	Identifiers    IdentifierSet `json:"identifiers"`
	Blobs          []BlobRow     `json:"blobs"`
	StateJSON      string        `json:"state_json_sha256"`
	GitRefs        string        `json:"git_refs"`
}

// ContentDigest is the repeatability digest: two exports of the same
// released project must produce the same value.
func (x *Index) ContentDigest(bundleDir string) (string, error) {
	stateSum, _, err := sha256File(filepath.Join(bundleDir, stateF))
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(indexContent{
		FormatVersion:  x.FormatVersion,
		ProjectID:      x.ProjectID,
		ProjectSlug:    x.ProjectSlug,
		ReleaseID:      x.ReleaseID,
		ReleaseVersion: x.ReleaseVersion,
		StateID:        x.StateID,
		Identifiers:    x.Identifiers,
		Blobs:          x.Blobs,
		StateJSON:      stateSum,
		GitRefs:        refDisplay(x.Identifiers.Git.Refs),
	})
	if err != nil {
		return "", err
	}
	return sha256Hex(b), nil
}

// Compare asserts that two bundles carry the same content, and says which
// class differs when they do not.
//
// It compares the four identifier classes, the state document's bytes and the
// Git refs the bundles carry — NOT the bytes of manifest.json, which embeds
// the product's own generation timestamp and therefore cannot be equal
// between two exports taken at different moments. The digest of that document
// is still covered, by the state_hash the manifest itself records.
func Compare(a, b string) error {
	xa, err := Check(a)
	if err != nil {
		return err
	}
	xb, err := Check(b)
	if err != nil {
		return err
	}
	da, err := xa.ContentDigest(a)
	if err != nil {
		return err
	}
	db, err := xb.ContentDigest(b)
	if err != nil {
		return err
	}
	for _, f := range []struct {
		name string
		av   string
		bv   string
	}{
		{"rsg_state_hash", xa.Identifiers.RSGStateHash, xb.Identifiers.RSGStateHash},
		{"release_manifest_hash", xa.Identifiers.ReleaseManifestHash, xb.Identifiers.ReleaseManifestHash},
		{"state_id", xa.StateID, xb.StateID},
		{"git head ref", xa.Identifiers.Git.HeadRef, xb.Identifiers.Git.HeadRef},
		{"git commit_sha", xa.Identifiers.Git.CommitSHA, xb.Identifiers.Git.CommitSHA},
		{"git refs", refDisplay(xa.Identifiers.Git.Refs), refDisplay(xb.Identifiers.Git.Refs)},
		{"blob hashes", strings.Join(xa.Identifiers.BlobHashes, " "), strings.Join(xb.Identifiers.BlobHashes, " ")},
		{"schema versions", strings.Join(xa.Identifiers.SchemaVersions, " "), strings.Join(xb.Identifiers.SchemaVersions, " ")},
		{"policy versions", strings.Join(xa.Identifiers.PolicyVersions, " "), strings.Join(xb.Identifiers.PolicyVersions, " ")},
		{"content digest", da, db},
	} {
		mark := "same"
		if f.av != f.bv {
			mark = "DIFFERENT"
			err = fmt.Errorf("the two bundles are not the same content: %s\n  a: %s\n  b: %s", f.name, f.av, f.bv)
		}
		fmt.Printf("COMPARE %-22s %s\n", f.name+":", mark)
		if err != nil {
			return err
		}
	}
	fmt.Printf("COMPARE a content_digest=%s\n", da)
	fmt.Printf("COMPARE b content_digest=%s\n", db)
	fmt.Printf("COMPARE SAME CONTENT — the four identifier classes, the state document and the git refs agree; manifest.json's own bytes are excluded because the product stamps generated_at into them\n")
	return nil
}

// integrityError is a refusal that names where. Every rejection this driver
// makes carries one, so the acceptance criterion "拒绝并指名是哪儿" is
// satisfiable from the message alone.
type integrityError struct {
	Location string
	Detail   string
}

func (e *integrityError) Error() string {
	return fmt.Sprintf("integrity refused at %s: %s", e.Location, e.Detail)
}

func refuse(location, format string, args ...any) error {
	return &integrityError{Location: location, Detail: fmt.Sprintf(format, args...)}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func sha256File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// readIndex loads EXPORT.json.
func readIndex(bundleDir string) (*Index, error) {
	raw, err := os.ReadFile(filepath.Join(bundleDir, indexFile))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", indexFile, err)
	}
	var x Index
	if err := json.Unmarshal(raw, &x); err != nil {
		return nil, fmt.Errorf("parse %s: %w", indexFile, err)
	}
	if x.FormatVersion != FormatV1 {
		return nil, fmt.Errorf("%s declares format_version %q, this driver speaks %q", indexFile, x.FormatVersion, FormatV1)
	}
	return &x, nil
}

// Check verifies a bundle's integrity and refuses with the location named.
//
// The order is deliberate: the transport digests first (a file that does not
// match its recorded digest is not the file that was exported, and nothing
// downstream of that is worth interpreting), then each class's own content
// addressing — which is the check that survives an attacker who recomputes
// the transport digest after tampering. The tamper cases in the e2e
// deliberately recompute EXPORT.json's digests so that what fires is the
// product's content addressing and not this driver's file digest.
func Check(bundleDir string) (*Index, error) {
	x, err := readIndex(bundleDir)
	if err != nil {
		return nil, err
	}

	// 1. transport digests.
	for _, f := range x.Files {
		got, n, err := sha256File(filepath.Join(bundleDir, filepath.FromSlash(f.Path)))
		if err != nil {
			return nil, refuse(f.Path, "the file the index records cannot be read: %v", err)
		}
		if got != f.SHA256 {
			return nil, refuse(f.Path, "digest mismatch: the index recorded sha256:%s (%d bytes), the bytes on disk hash to sha256:%s (%d bytes)", f.SHA256, f.Bytes, got, n)
		}
	}

	// 2. the RSG snapshot's own content address.
	mraw, err := os.ReadFile(filepath.Join(bundleDir, manifestF))
	if err != nil {
		return nil, refuse(manifestF, "cannot read: %v", err)
	}
	var m manifest.Manifest
	if err := json.Unmarshal(mraw, &m); err != nil {
		return nil, refuse(manifestF, "cannot parse as an RSG manifest: %v", err)
	}
	if !m.VerifyHash() {
		content, derr := m.ContentCanonicalJSON()
		if derr != nil {
			return nil, refuse(manifestF, "state_hash cannot be re-derived: %v", derr)
		}
		return nil, refuse(manifestF, "state_hash mismatch: the document records %s, its content re-derives to %s", m.StateHash, manifest.Digest(content))
	}
	if m.StateID != x.StateID {
		return nil, refuse(manifestF, "state_id is %s, the index names state %s", m.StateID, x.StateID)
	}

	// 3. the release document's own content address, and the state pin it
	// embeds — the second, independent statement of the same state hash.
	rraw, err := os.ReadFile(filepath.Join(bundleDir, releaseF))
	if err != nil {
		return nil, refuse(releaseF, "cannot read: %v", err)
	}
	var rel releases.ReleaseManifest
	if err := json.Unmarshal(rraw, &rel); err != nil {
		return nil, refuse(releaseF, "cannot parse as a release manifest: %v", err)
	}
	if !rel.VerifyHash() {
		return nil, refuse(releaseF, "manifest_hash mismatch: the document records %s, its content re-derives to something else", rel.ManifestHash)
	}
	if rel.State.StateHash != m.StateHash {
		return nil, refuse(releaseF+":state.state_hash", "pins %s while %s records %s — the release and the snapshot are not the same state", rel.State.StateHash, manifestF, m.StateHash)
	}
	if got := manifest.Digest(rel.State.Content); got != rel.State.StateHash {
		return nil, refuse(releaseF+":state.content", "re-derives to %s, which is not the pinned state_hash %s", got, rel.State.StateHash)
	}

	// 4. the Git class: every ref the bundle carries, and the commit the
	// index pins, read out of the bundle with the real git CLI.
	refs, err := bundleRefs(filepath.Join(bundleDir, gitBundleF))
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, refuse(gitBundleF, "carries no refs at all")
	}
	for ref, want := range x.Identifiers.Git.Refs {
		got, ok := refs[ref]
		if !ok {
			return nil, refuse(gitBundleF, "the index pins %s at %s but the bundle does not carry that ref", ref, short(want))
		}
		if got != want {
			return nil, refuse(gitBundleF, "%s resolves to %s, the index pins %s — the commit was swapped", ref, short(got), short(want))
		}
	}
	if got := refs[x.Identifiers.Git.HeadRef]; got != x.Identifiers.Git.CommitSHA {
		return nil, refuse(gitBundleF, "%s resolves to %s, the index pins commit %s", x.Identifiers.Git.HeadRef, short(got), short(x.Identifiers.Git.CommitSHA))
	}

	// 5. the blob bytes, by content hash and never by path.
	// The file NAMES are the content hashes: a blob whose name does not
	// match its recorded hash is already a contradiction, so both
	// directions are checked here.
	onDisk, err := filepath.Glob(filepath.Join(bundleDir, blobFilesGl))
	if err != nil {
		return nil, err
	}
	byHash := map[string]BlobRow{}
	for _, b := range x.Blobs {
		byHash[b.ContentHash] = b
	}
	if len(onDisk) != len(x.Blobs) {
		return nil, refuse(blobsDir, "the index records %d blobs, the directory holds %d files", len(x.Blobs), len(onDisk))
	}
	for _, p := range onDisk {
		name := filepath.Base(p)
		row, ok := byHash[name]
		if !ok {
			return nil, refuse(blobsDir+"/"+name, "no blob with this content hash is recorded in the index")
		}
		got, n, err := sha256File(p)
		if err != nil {
			return nil, refuse(blobsDir+"/"+name, "cannot read: %v", err)
		}
		if got != row.ContentHash {
			return nil, refuse(blobsDir+"/"+name, "the blob bytes hash to sha256:%s, the content address pins sha256:%s (%d bytes on disk, %d recorded)", got, row.ContentHash, n, row.SizeBytes)
		}
		if n != row.SizeBytes {
			return nil, refuse(blobsDir+"/"+name, "the blob is %d bytes, the index records %d", n, row.SizeBytes)
		}
	}

	// 6. the manifest and the indexed blob set must agree: a manifest that
	// pins a blob the bundle does not carry is not a portable export.
	for _, r := range m.BlobRefs {
		if _, ok := byHash[r.Hash]; !ok {
			return nil, refuse(manifestF+":blob_refs", "pins blob %s (content hash %s) which the bundle does not carry", r.ID, short(r.Hash))
		}
	}
	return x, nil
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}

// sortedBlobHashes is the blob identifier set in a stable order: the
// comparison between two sides must not depend on map iteration.
func sortedBlobHashes(hashes map[string]bool) []string {
	out := make([]string, 0, len(hashes))
	for h := range hashes {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// writeFile writes one bundle file and returns its index entry.
func writeFile(bundleDir, rel string, body []byte) (FileEntry, error) {
	full := filepath.Join(bundleDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return FileEntry{}, err
	}
	if err := os.WriteFile(full, body, 0o644); err != nil {
		return FileEntry{}, err
	}
	return FileEntry{Path: rel, Bytes: int64(len(body)), SHA256: sha256Hex(body)}, nil
}

// indexEntriesFor walks a bundle directory and returns an entry for every
// file an index is expected to account for (everything but EXPORT.json
// itself), sorted by path.
func indexEntriesFor(bundleDir string) ([]FileEntry, error) {
	var out []FileEntry
	err := filepath.WalkDir(bundleDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(bundleDir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == indexFile {
			return nil
		}
		sum, n, err := sha256File(p)
		if err != nil {
			return err
		}
		out = append(out, FileEntry{Path: rel, Bytes: n, SHA256: sum})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// copyTree copies a bundle directory (used by the tamper cases, which must
// not damage the bundle the export produced).
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return os.WriteFile(target, body, info.Mode().Perm())
	})
}

// rewriteIndex writes EXPORT.json back with refreshed digests. The tamper
// cases use it so that what fires is the product's content addressing, not
// this driver's transport digest.
func rewriteIndex(bundleDir string, x *Index) error {
	entries, err := indexEntriesFor(bundleDir)
	if err != nil {
		return err
	}
	x.Files = entries
	raw, err := json.MarshalIndent(x, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(bundleDir, indexFile), append(raw, '\n'), 0o644)
}

// refDisplay renders a ref set for a report.
func refDisplay(refs map[string]string) string {
	keys := make([]string, 0, len(refs))
	for k := range refs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+short(refs[k]))
	}
	return strings.Join(parts, " ")
}
