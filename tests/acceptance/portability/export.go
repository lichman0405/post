package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/backupdr"
	"github.com/lichman0405/post/internal/application/manifests"
	"github.com/lichman0405/post/internal/application/releases"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/rsg/manifest"
)

// s3Target splits a storage_key of the form s3://<bucket>/<key> into the two
// halves S3 needs. A storage_key that is not an s3:// URI is an error naming
// the row: guessing an address would make "the bytes are the ones this blob
// names" a claim about a guess.
func s3Target(storageKey string) (bucket, key string, err error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(storageKey), "s3://")
	if !ok {
		return "", "", fmt.Errorf("storage_key %q is not an s3:// URI", storageKey)
	}
	bucket, key, ok = strings.Cut(rest, "/")
	if !ok || bucket == "" || key == "" {
		return "", "", fmt.Errorf("storage_key %q does not name a bucket and a key", storageKey)
	}
	return bucket, key, nil
}

// blobStore is the real S3 half, reached through the repository's own S3
// client (cmd/api/backupdr/s3.go): the hand-rolled SigV4 client is the only
// S3 implementation in the tree, and a second one written here would make
// "the export and the import agree about the object store" a statement about
// two clients.
type blobStore struct {
	cfg backupdr.SourceConfig
}

func newBlobStore(e env) *blobStore {
	return &blobStore{cfg: backupdr.SourceConfig{
		Env: backupdr.Env{
			PostgresURL:  e.PostgresURL,
			BlobEndpoint: e.BlobEndpoint,
			BlobBucket:   e.BlobBucket,
			GiteaBaseURL: e.GiteaBase,
			GiteaOwner:   e.GiteaOwner,
		},
		BlobAccessKey: e.BlobAccess,
		BlobSecretKey: e.BlobSecret,
		GiteaToken:    e.GiteaToken,
		RunID:         "t1208-portability",
	}}
}

func (b *blobStore) get(ctx context.Context, bucket, key string) ([]byte, error) {
	return backupdr.GetRestoredObject(ctx, b.cfg, bucket, key)
}

func (b *blobStore) put(ctx context.Context, bucket, key string, body []byte) error {
	if err := backupdr.EnsureBucket(ctx, b.cfg, bucket); err != nil {
		return fmt.Errorf("ensure bucket %s: %w", bucket, err)
	}
	return backupdr.PutRestoredObject(ctx, b.cfg, bucket, key, body)
}

// ---------------------------------------------------------------------------
// assemble
// ---------------------------------------------------------------------------

// planFiles indexes every documented file in a seed plan, by the object key
// its blob row was written under.
//
// The walk is recursive on purpose: the plan keeps its objects in several
// places (main_objects, and one objects array per branch under
// branches_content), and a walk that only read the first of them would
// silently skip every branch object's bytes — a hole that would surface much
// later as "the export carried fewer blobs than the project has".
func planFiles(doc any) map[string]string {
	out := map[string]string{}
	var walk func(v any)
	walk = func(v any) {
		switch node := v.(type) {
		case map[string]any:
			key, hasKey := node["key"].(string)
			if hasKey {
				if file, ok := node["file"].(map[string]any); ok {
					if content, ok := file["content"].(string); ok && content != "" {
						out[key] = content
					}
				}
			}
			for _, child := range node {
				walk(child)
			}
		case []any:
			for _, child := range node {
				walk(child)
			}
		}
	}
	walk(doc)
	return out
}

// Assemble puts the bytes the blobs rows describe into the real object store.
//
// WHY THIS EXISTS, and why it is not cheating:
//
// This build has no product-side blob write path. The `blobs` table is real,
// the read path is real (queries/manifest.sql), and sqlc's CreateBlob exists
// — but its only caller is tests/integration/manifest_test.go:178, and every
// `INSERT INTO blobs` in the tree is inside a _test.go file. On the object
// store side the only production caller of S3 is cmd/api/backupdr, the
// disaster-recovery drill. The seed therefore wrote blob ROWS whose content
// hash and size are real but whose bytes were never stored anywhere
// (tests/acceptance/seeddemo/blobs.go sets integrity_state 'pending' for
// exactly that reason).
//
// A portable export must carry the bytes, so the TEST assembles them: the
// same shape tests/integration/asset_canonical_e2e_test.go:386 and
// asset_preview_test.go:347 already use — real rows, real bucket, real bytes,
// with the assembly step done by the test. What this function is careful NOT
// to do is invent content or trust the row: it reads the documented content
// from the plan and REFUSES if its hash is not the hash the row pins, so the
// bytes that land in the bucket are the bytes the manifest already
// content-addresses.
//
// The consequence for the portability conclusion is reported in RESULT and is
// not hidden here: what this driver proves is that a released project's four
// identifier classes survive a round trip through a clean environment. It
// does not prove that the product can WRITE a blob, because it cannot.
func Assemble(ctx context.Context, pool *pgxpool.Pool, e env, projectRef, planPath string) error {
	projectID, slug, err := projectBySlugOrID(ctx, pool, projectRef)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(planPath)
	if err != nil {
		return fmt.Errorf("read the fixture plan %s: %w", planPath, err)
	}
	var plan any
	if err := json.Unmarshal(raw, &plan); err != nil {
		return fmt.Errorf("parse the fixture plan %s: %w", planPath, err)
	}
	contentByKey := planFiles(plan)

	rows, err := pool.Query(ctx, `
		SELECT b.id::text, b.content_hash, b.size_bytes, coalesce(b.storage_key, ''), b.integrity_state
		FROM blobs b
		WHERE b.id IN (
			SELECT ba.blob_id FROM blob_attachments ba
			JOIN scientific_object_versions sov ON sov.id = ba.scientific_object_version_id
			JOIN scientific_objects so ON so.id = sov.object_id
			WHERE so.project_id = $1)
		ORDER BY b.content_hash, b.id`, projectID)
	if err != nil {
		return err
	}
	type blobRow struct {
		id, hash, storageKey, integrity string
		size                            int64
	}
	var blobs []blobRow
	for rows.Next() {
		var b blobRow
		if err := rows.Scan(&b.id, &b.hash, &b.size, &b.storageKey, &b.integrity); err != nil {
			rows.Close()
			return err
		}
		blobs = append(blobs, b)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(blobs) == 0 {
		return fmt.Errorf("project %s has no attached blobs — there is nothing for a portable export to carry, and an export that claims otherwise would be the failure this task exists to catch", slug)
	}

	store := newBlobStore(e)
	assembled, reused := 0, 0
	for _, b := range blobs {
		bucket, key, err := s3Target(b.storageKey)
		if err != nil {
			return fmt.Errorf("blob %s: %w", b.id, err)
		}
		// The plan names the content by the object key the storage_key
		// ends with, exactly as tests/acceptance/seeddemo/blobs.go built it.
		base := filepath.Base(key)
		objectKey := strings.TrimSuffix(base, filepath.Ext(base))
		content, ok := contentByKey[objectKey]
		if !ok {
			return fmt.Errorf("blob %s names object %q (storage_key %s), which the fixture plan %s does not document — refusing to invent bytes for it", b.id, objectKey, b.storageKey, planPath)
		}
		sum := sha256.Sum256([]byte(content))
		got := hex.EncodeToString(sum[:])
		if got != b.hash {
			return fmt.Errorf("blob %s pins content hash %s but the plan's content for %q hashes to %s — the row and the fixture disagree, and storing either one would be a guess", b.id, b.hash, objectKey, got)
		}
		if _, err := store.get(ctx, bucket, key); err == nil {
			reused++
			fmt.Printf("ASSEMBLE reuse  %s %s/%s sha256=%s\n", b.id, bucket, key, short(b.hash))
			continue
		}
		if err := store.put(ctx, bucket, key, []byte(content)); err != nil {
			return fmt.Errorf("store blob %s at %s/%s: %w", b.id, bucket, key, err)
		}
		assembled++
		fmt.Printf("ASSEMBLE put    %s %s/%s bytes=%d sha256=%s\n", b.id, bucket, key, len(content), short(b.hash))
	}
	fmt.Printf("ASSEMBLE DONE blobs=%d put=%d reuse=%d — the bytes were assembled by the TEST: this build has no product-side blob write path (blobs.CreateBlob's only caller is tests/integration/manifest_test.go, S3's only production caller is cmd/api/backupdr); the rows themselves are untouched and integrity_state stays %q\n",
		len(blobs), assembled, reused, blobs[0].integrity)
	return nil
}

// ---------------------------------------------------------------------------
// export
// ---------------------------------------------------------------------------

// Export writes a portable bundle of one released project.
//
// Read-only, and the read-only claim is not a comment: nothing below issues
// an INSERT/UPDATE/DELETE, and the e2e compares a fingerprint of the source
// taken before this runs with one taken after.
func Export(ctx context.Context, pool *pgxpool.Pool, e env, projectRef, releaseVersion, outDir string) (*Index, error) {
	projectID, slug, err := projectBySlugOrID(ctx, pool, projectRef)
	if err != nil {
		return nil, err
	}
	rel, err := resolveRelease(ctx, pool, projectID, releaseVersion)
	if err != nil {
		return nil, err
	}

	// 1. the RSG snapshot, through the product's own exporter.
	svc := manifests.NewService(persistence.NewStateStore(pool), persistence.NewManifestStore(pool))
	m, err := svc.Export(ctx, rel.StateID)
	if err != nil {
		return nil, fmt.Errorf("export the state manifest: %w", err)
	}
	if !m.VerifyHash() {
		return nil, fmt.Errorf("the state manifest the product exported does not verify its own hash — refusing to export it")
	}
	manifestBytes, err := m.CanonicalJSON()
	if err != nil {
		return nil, err
	}

	// 2. the release document, byte for byte as stored. The stored bytes
	// ARE the bytes manifest_hash was computed over (00053 converted the
	// column from jsonb to text for exactly that reason), so rewriting
	// them here would be the bug that migration fixed.
	var relMan releases.ReleaseManifest
	if err := json.Unmarshal([]byte(rel.Manifest), &relMan); err != nil {
		return nil, fmt.Errorf("parse the stored release manifest of %s: %w", rel.Version, err)
	}
	if !relMan.VerifyHash() {
		return nil, fmt.Errorf("the stored release manifest of %s does not verify its own manifest_hash — refusing to export it", rel.Version)
	}
	if relMan.State.StateHash != m.StateHash {
		return nil, fmt.Errorf("release %s pins state hash %s while the state's own export derives %s", rel.Version, relMan.State.StateHash, m.StateHash)
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}

	// 3. the blob bytes.
	//
	// The carried set is the transitive closure of the row set — every blob
	// the exported blob_attachments name — UNIONED with the release's own
	// pinned blob_refs. Both halves are needed and they are not the same set:
	// the manifest pins the blobs of the released state, the attachments span
	// every version the project holds. Exporting only the pinned ones leaves
	// attachment rows pointing at a blob row that never travelled (the
	// importer's foreign key finds out, one row at a time); exporting only the
	// attached ones would let a pinned blob the row set does not reach go
	// missing. Every carried blob's BYTES are read from the real object store
	// and hashed here — a row whose bytes stayed behind would make "the target
	// holds the same large objects" a statement about a path.
	ids, err := blobIDsForProject(ctx, pool, projectID)
	if err != nil {
		return nil, err
	}
	pinned := map[string]string{}
	seen := map[string]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	for _, r := range m.BlobRefs {
		pinned[r.ID] = r.Hash
		if !seen[r.ID] {
			seen[r.ID] = true
			ids = append(ids, r.ID)
		}
	}
	blobRows, err := blobRowsFor(ctx, pool, ids)
	if err != nil {
		return nil, err
	}
	if len(blobRows) != len(ids) {
		return nil, fmt.Errorf("the row set and the release manifest name %d blobs but only %d rows exist for project %s", len(ids), len(blobRows), slug)
	}
	store := newBlobStore(e)
	byID := map[string]BlobRow{}
	for _, b := range blobRows {
		byID[b.ID] = b
	}
	hashes := map[string]bool{}
	for _, id := range ids {
		row := byID[id]
		if want, ok := pinned[id]; ok && row.ContentHash != want {
			return nil, fmt.Errorf("blob %s: the release manifest pins content hash %s, the blobs row says %s", id, want, row.ContentHash)
		}
		bucket, key, err := s3Target(row.StorageKey)
		if err != nil {
			return nil, fmt.Errorf("blob %s: %w", id, err)
		}
		bytesOut, err := store.get(ctx, bucket, key)
		if err != nil {
			return nil, fmt.Errorf("blob %s (a row of this project, %s by the release manifest) has no bytes at %s/%s: %w — an export that skipped it would not be portable", id, pinnedWord(pinned, id), bucket, key, err)
		}
		if got := sha256Hex(bytesOut); got != row.ContentHash {
			return nil, fmt.Errorf("blob %s at %s/%s hashes to sha256:%s, the content address pins sha256:%s", id, bucket, key, got, row.ContentHash)
		}
		if int64(len(bytesOut)) != row.SizeBytes {
			return nil, fmt.Errorf("blob %s is %d bytes in the store, the row records %d", id, len(bytesOut), row.SizeBytes)
		}
		if _, err := writeFile(outDir, blobsDir+"/"+row.ContentHash, bytesOut); err != nil {
			return nil, err
		}
		hashes[row.ContentHash] = true
	}
	fmt.Printf("EXPORT blobs=%d carried (%d of them pinned by the release manifest, every byte read back from the store and hashed)\n", len(ids), len(pinned))

	// 4. the Git half: the project's repository, read from the provider.
	gitID, err := exportGit(ctx, pool, e, projectID, m, outDir)
	if err != nil {
		return nil, err
	}

	// 5. the relational half.
	doc, err := dumpRows(ctx, pool, projectID, rel.ID, ids)
	if err != nil {
		return nil, err
	}
	stateBytes, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}

	// 6. the three documents that are not the index.
	for _, f := range []struct {
		path string
		body []byte
	}{
		{manifestF, manifestBytes},
		{releaseF, []byte(rel.Manifest)},
		{stateF, append(stateBytes, '\n')},
	} {
		if _, err := writeFile(outDir, f.path, f.body); err != nil {
			return nil, err
		}
	}

	schemas := make([]string, 0, len(relMan.Schemas))
	for _, s := range relMan.Schemas {
		schemas = append(schemas, fmt.Sprintf("%s@%s#%s", s.ID, s.Version, short(s.ContentHash)))
	}
	sort.Strings(schemas)
	policies := []string{}
	if relMan.Policy != nil {
		if relMan.Policy.Organization != nil {
			policies = append(policies, fmt.Sprintf("organization:%s@%s", relMan.Policy.Organization.ID, relMan.Policy.Organization.Version))
		}
		if relMan.Policy.Project != nil {
			policies = append(policies, fmt.Sprintf("project:%s@%s", relMan.Policy.Project.ID, relMan.Policy.Project.Version))
		}
	}
	sort.Strings(policies)

	x := &Index{
		FormatVersion:  FormatV1,
		ProjectID:      projectID,
		ProjectSlug:    slug,
		ReleaseID:      rel.ID,
		ReleaseVersion: rel.Version,
		StateID:        rel.StateID,
		GeneratedAt:    time.Now().UTC(),
		Identifiers: IdentifierSet{
			RSGStateHash:        m.StateHash,
			ReleaseManifestHash: relMan.ManifestHash,
			Git:                 gitID,
			BlobHashes:          sortedBlobHashes(hashes),
			SchemaVersions:      schemas,
			PolicyVersions:      policies,
		},
		Blobs: blobRows,
	}
	if err := rewriteIndex(outDir, x); err != nil {
		return nil, err
	}
	return x, nil
}

// exportGit reads the project's repository out of the real GitProvider and
// writes a real git bundle of it.
func exportGit(ctx context.Context, pool *pgxpool.Pool, e env, projectID string, m *manifest.Manifest, outDir string) (GitID, error) {
	var externalID *string
	if err := pool.QueryRow(ctx, `SELECT git_repository_external_id FROM projects WHERE id = $1`, projectID).Scan(&externalID); err != nil {
		return GitID{}, err
	}
	if externalID == nil || *externalID == "" {
		return GitID{}, fmt.Errorf("project %s has no git_repository_external_id: the project was never provisioned against the GitProvider, so the Git half of the export does not exist. An export that omitted it would carry three of the four classes CLAUDE.md §8 pins, which is not portable (see the e2e's provisioning stage)", projectID)
	}
	parts := strings.SplitN(*externalID, "/", 2)
	if len(parts) != 2 {
		return GitID{}, fmt.Errorf("git_repository_external_id %q is not owner/name", *externalID)
	}
	owner, name := parts[0], parts[1]

	home, err := workDir("git-export")
	if err != nil {
		return GitID{}, err
	}
	defer os.RemoveAll(home)
	refs, err := makeBundle(repoURL(e.GiteaBase, owner, name), e.GiteaToken, filepath.Join(outDir, gitBundleF), home)
	if err != nil {
		return GitID{}, err
	}
	head, err := pickHead(refs, "main")
	if err != nil {
		return GitID{}, err
	}
	return GitID{
		Repository:  owner + "/" + name,
		Refs:        refs,
		HeadRef:     head,
		CommitSHA:   refs[head],
		StateGitRef: m.GitRef,
	}, nil
}

// pickHead chooses the ref the export pins as the repository's head: the
// default branch when it is there, otherwise the first branch in name order.
func pickHead(refs map[string]string, defaultBranch string) (string, error) {
	want := "refs/heads/" + defaultBranch
	if _, ok := refs[want]; ok {
		return want, nil
	}
	names := make([]string, 0, len(refs))
	for r := range refs {
		if strings.HasPrefix(r, "refs/heads/") {
			names = append(names, r)
		}
	}
	if len(names) == 0 {
		return "", fmt.Errorf("the project's repository carries no branch at all (refs: %s)", refDisplay(refs))
	}
	sort.Strings(names)
	return names[0], nil
}

// pinnedWord says whether the release manifest pins a blob, for a message
// that has to distinguish "the release promises this" from "the project's own
// rows reference this".
func pinnedWord(pinned map[string]string, id string) string {
	if _, ok := pinned[id]; ok {
		return "pinned"
	}
	return "not pinned"
}
