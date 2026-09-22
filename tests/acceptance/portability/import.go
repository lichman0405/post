package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/manifests"
	"github.com/lichman0405/post/internal/application/releases"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/rsg/manifest"
)

// conflictError is a refusal that is not about integrity: the request is
// well formed but the target is not in a state to accept it. Both kinds
// print the same REFUSED line and exit the same way, so a caller can never
// mistake "the bundle is corrupt" for "the target already holds this
// project" — the location and the detail say which.
type conflictError struct{ Detail string }

func (e *conflictError) Error() string { return "refused: " + e.Detail }
func (e *conflictError) isRefusal()    {}

func (e *integrityError) isRefusal() {}

// refusal is implemented by every error this driver reports as a deliberate
// NO rather than a crash.
type refusal interface{ isRefusal() }

// ---------------------------------------------------------------------------
// fingerprint
// ---------------------------------------------------------------------------

// Fingerprint is the read-only proof: the state of the SOURCE side, taken
// before and after an export. Two fingerprints that differ mean the export
// wrote something, which the task forbids.
type Fingerprint struct {
	ProjectID           string         `json:"project_id"`
	ProjectSlug         string         `json:"project_slug"`
	ProvisionStatus     string         `json:"provision_status"`
	GitRepository       *string        `json:"git_repository_external_id"`
	RowCounts           map[string]int `json:"row_counts"`
	BranchHeads         []string       `json:"branch_heads"`
	BlobContentHashes   []string       `json:"blob_content_hashes"`
	ReleaseVersions     []string       `json:"release_versions"`
	StateHashes         []string       `json:"state_hashes"`
	GitRefs             []string       `json:"git_refs"`
	ReleasedStateHash   string         `json:"released_state_hash"`
	ReleaseManifestHash string         `json:"release_manifest_hash"`
}

// Fingerprint reads the source side. Every statement here is a SELECT; the
// Git half is a `git ls-remote`, which reads the provider and writes
// nothing.
func Fingerprint0(ctx context.Context, pool *pgxpool.Pool, e env, projectRef string) (*Fingerprint, error) {
	projectID, slug, err := projectBySlugOrID(ctx, pool, projectRef)
	if err != nil {
		return nil, err
	}
	f := &Fingerprint{ProjectID: projectID, ProjectSlug: slug, RowCounts: map[string]int{}}
	if err := pool.QueryRow(ctx, `SELECT provision_status, git_repository_external_id FROM projects WHERE id = $1`, projectID).
		Scan(&f.ProvisionStatus, &f.GitRepository); err != nil {
		return nil, err
	}

	counts := map[string]string{
		"branches":           `SELECT count(*) FROM branches WHERE project_id = $1`,
		"project_states":     `SELECT count(*) FROM project_states WHERE project_id = $1`,
		"state_commits":      `SELECT count(*) FROM state_commits WHERE project_id = $1`,
		"scientific_objects": `SELECT count(*) FROM scientific_objects WHERE project_id = $1`,
		"scientific_object_versions": `SELECT count(*) FROM scientific_object_versions
			WHERE object_id IN (SELECT id FROM scientific_objects WHERE project_id = $1)`,
		"relations": `SELECT count(*) FROM relations WHERE project_id = $1`,
		"relation_versions": `SELECT count(*) FROM relation_versions
			WHERE relation_id IN (SELECT id FROM relations WHERE project_id = $1)`,
		"releases": `SELECT count(*) FROM releases WHERE project_id = $1`,
		"policy_versions": `SELECT count(*) FROM policy_versions
			WHERE project_id = $1 OR organization_id IN (SELECT organization_id FROM projects WHERE id = $1)`,
		"project_schema_profiles": `SELECT count(*) FROM project_schema_profiles WHERE project_id = $1`,
		"blob_attachments": `SELECT count(*) FROM blob_attachments
			WHERE scientific_object_version_id IN (
				SELECT sov.id FROM scientific_object_versions sov
				JOIN scientific_objects so ON so.id = sov.object_id WHERE so.project_id = $1)`,
	}
	for name, q := range counts {
		var n int
		if err := pool.QueryRow(ctx, q, projectID).Scan(&n); err != nil {
			return nil, fmt.Errorf("count %s: %w", name, err)
		}
		f.RowCounts[name] = n
	}

	// Branch heads: the branch's own head state, which is what a branch
	// pointer moves. A branch with no base_state_id is reported as "none"
	// rather than skipped — a head that appears or disappears is exactly
	// the change this fingerprint is looking for.
	rows, err := pool.Query(ctx, `
		SELECT name || ' ' || lifecycle_state || ' head=' || coalesce(base_state_id::text, 'none')
		FROM branches WHERE project_id = $1 ORDER BY name`, projectID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return nil, err
		}
		f.BranchHeads = append(f.BranchHeads, s)
	}
	rows.Close()

	// The blob content-hash set, plus the addressing and the integrity
	// state: requirement 2 asks for the hash set, and the two extra
	// columns make a write that does not move a hash (a storage_key
	// rewrite, an integrity_state flip) visible instead of invisible.
	rows, err = pool.Query(ctx, `
		SELECT content_hash || ' size=' || size_bytes || ' key=' || coalesce(storage_key, '') || ' integrity=' || integrity_state
		FROM blobs WHERE id IN (
			SELECT ba.blob_id FROM blob_attachments ba
			JOIN scientific_object_versions sov ON sov.id = ba.scientific_object_version_id
			JOIN scientific_objects so ON so.id = sov.object_id WHERE so.project_id = $1)
		ORDER BY content_hash, id`, projectID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return nil, err
		}
		f.BlobContentHashes = append(f.BlobContentHashes, s)
	}
	rows.Close()

	rows, err = pool.Query(ctx, `SELECT version FROM releases WHERE project_id = $1 ORDER BY version`, projectID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return nil, err
		}
		f.ReleaseVersions = append(f.ReleaseVersions, s)
	}
	rows.Close()

	rows, err = pool.Query(ctx, `SELECT state_hash FROM project_states WHERE project_id = $1 ORDER BY state_hash`, projectID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return nil, err
		}
		f.StateHashes = append(f.StateHashes, s)
	}
	rows.Close()

	rel, err := resolveRelease(ctx, pool, projectID, "")
	if err != nil {
		return nil, err
	}
	svc := manifests.NewService(persistence.NewStateStore(pool), persistence.NewManifestStore(pool))
	m, err := svc.Export(ctx, rel.StateID)
	if err != nil {
		return nil, err
	}
	f.ReleasedStateHash = m.StateHash
	f.ReleaseManifestHash = rel.ManifestHash

	if f.GitRepository != nil && *f.GitRepository != "" {
		parts := strings.SplitN(*f.GitRepository, "/", 2)
		if len(parts) == 2 {
			home, err := workDir("fp-git")
			if err != nil {
				return nil, err
			}
			defer os.RemoveAll(home)
			refs, err := remoteRefs(repoURL(e.GiteaBase, parts[0], parts[1]), e.GiteaToken, home)
			if err != nil {
				return nil, err
			}
			f.GitRefs = sortedRefs(refs)
		}
	}
	return f, nil
}

// ---------------------------------------------------------------------------
// import
// ---------------------------------------------------------------------------

// ImportBundle rebuilds a bundle's project in a clean environment and then
// verifies it. It refuses before touching anything if the bundle does not
// verify, and refuses again if the target already holds the project.
func ImportBundle(ctx context.Context, pool *pgxpool.Pool, e env, bundleDir, repoName, targetOrg string) (*Index, *IdentifierSet, error) {
	x, err := Check(bundleDir)
	if err != nil {
		return nil, nil, err
	}

	raw, err := os.ReadFile(filepath.Join(bundleDir, stateF))
	if err != nil {
		return nil, nil, err
	}
	var doc stateDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", stateF, err)
	}
	if doc.ProjectID != x.ProjectID {
		return nil, nil, refuse(stateF, "holds project %s, the index names %s", doc.ProjectID, x.ProjectID)
	}

	// Refuse an import onto a target that already holds the project. The
	// choice between "idempotent" and "explicitly refused" is made here
	// and it is the second: a silent second import would leave two rows
	// for one identity, and this driver's whole claim is that the
	// identifier classes are compared against a KNOWN target.
	var existing int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM projects WHERE id = $1`, doc.ProjectID).Scan(&existing); err != nil {
		return nil, nil, err
	}
	if existing > 0 {
		return nil, nil, &conflictError{Detail: fmt.Sprintf("the target database already holds project %s — this importer refuses rather than merging two histories under one identity", doc.ProjectID)}
	}

	srcParts := strings.SplitN(x.Identifiers.Git.Repository, "/", 2)
	if len(srcParts) != 2 {
		return nil, nil, fmt.Errorf("the export records repository %q, which is not owner/name", x.Identifiers.Git.Repository)
	}
	if repoName == "" {
		repoName = srcParts[1] + "-import"
	}

	ctxG := context.Background()
	g := newGitea(e.GiteaBase, e.GiteaToken)
	exists, err := g.repoExists(ctxG, srcParts[0], repoName)
	if err != nil {
		return nil, nil, err
	}
	if exists {
		return nil, nil, &conflictError{Detail: fmt.Sprintf("the target GitProvider already holds repository %s/%s — refusing to import over it", srcParts[0], repoName)}
	}
	owner, err := g.createRepo(ctxG, targetOrg, repoName, true)
	if err != nil {
		return nil, nil, err
	}
	fmt.Printf("IMPORT git repo created  %s/%s\n", owner, repoName)

	// The blob bytes, into the target's object store under the target's
	// own prefix. The path is deliberately DIFFERENT from the source's:
	// the comparison that has to hold is by content hash, and an import
	// that reused the source path could not tell the two apart.
	store := newBlobStore(e)
	targetKey := func(hash string) string { return "t1208-import/" + x.ProjectID + "/" + hash }
	for _, b := range x.Blobs {
		body, err := os.ReadFile(filepath.Join(bundleDir, blobsDir, b.ContentHash))
		if err != nil {
			return nil, nil, err
		}
		if err := store.put(ctx, e.BlobBucket, targetKey(b.ContentHash), body); err != nil {
			return nil, nil, err
		}
	}
	fmt.Printf("IMPORT blobs stored      bucket=%s prefix=t1208-import/%s blobs=%d\n", e.BlobBucket, x.ProjectID, len(x.Blobs))

	rewriteProject := func(obj map[string]any) error {
		obj["git_repository_external_id"] = owner + "/" + repoName
		return nil
	}
	byHash := map[string]bool{}
	for _, b := range x.Blobs {
		byHash[b.ContentHash] = true
	}
	rewriteBlob := func(obj map[string]any) error {
		hash, _ := obj["content_hash"].(string)
		if !byHash[hash] {
			return fmt.Errorf("the exported rows carry a blob (%s) the index does not pin — refusing to guess its bytes", hash)
		}
		obj["storage_key"] = "s3://" + e.BlobBucket + "/" + targetKey(hash)
		return nil
	}
	inserted, err := loadRows(ctx, pool, &doc, rewriteProject, rewriteBlob)
	if err != nil {
		return nil, nil, err
	}
	names := make([]string, 0, len(inserted))
	for t, n := range inserted {
		names = append(names, fmt.Sprintf("%s=%d", t, n))
	}
	sort.Strings(names)
	fmt.Printf("IMPORT rows inserted     %s\n", strings.Join(names, " "))

	// The Git half, pushed with the real git CLI and then read BACK from
	// the provider: the push's exit status is not evidence that the ref
	// moved, and this repository's own gates say so.
	home, err := workDir("git-import")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(home)
	targetURL := repoURL(e.GiteaBase, owner, repoName)
	if err := pushBundleRefs(filepath.Join(bundleDir, gitBundleF), targetURL, e.GiteaToken, home); err != nil {
		return nil, nil, err
	}
	got, err := remoteRefs(targetURL, e.GiteaToken, home)
	if err != nil {
		return nil, nil, err
	}
	fmt.Printf("IMPORT git refs read back %s\n", refDisplay(got))

	ids, err := DeriveIdentifiers(ctx, pool, e, doc.ProjectID)
	if err != nil {
		return nil, nil, err
	}
	ids.Git = x.Identifiers.Git
	ids.Git.Repository = owner + "/" + repoName
	if got[ids.Git.HeadRef] != ids.Git.CommitSHA {
		return nil, nil, fmt.Errorf("the target's %s is %s, the export pins commit %s — the push did not land", ids.Git.HeadRef, short(got[ids.Git.HeadRef]), short(ids.Git.CommitSHA))
	}
	return x, &ids, nil
}

// DeriveIdentifiers reads the four identifier classes out of an environment
// and derives the RSG hash with the product's own exporter. This is the
// point of the whole driver: the hash the import side reports is computed
// from the rows the import just wrote, by the same code the export side used,
// and never copied from the bundle.
func DeriveIdentifiers(ctx context.Context, pool *pgxpool.Pool, e env, projectID string) (IdentifierSet, error) {
	var ids IdentifierSet
	svc := manifests.NewService(persistence.NewStateStore(pool), persistence.NewManifestStore(pool))

	var (
		stateID     string
		relID       string
		relManifest string
		relHash     string
	)
	if err := pool.QueryRow(ctx, `
		SELECT id::text, state_id::text, manifest, manifest_hash
		FROM releases WHERE project_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, projectID).
		Scan(&relID, &stateID, &relManifest, &relHash); err != nil {
		return ids, fmt.Errorf("read the imported release: %w", err)
	}
	m, err := svc.Export(ctx, stateID)
	if err != nil {
		return ids, fmt.Errorf("re-derive the state manifest from the target: %w", err)
	}
	var relMan releases.ReleaseManifest
	if err := json.Unmarshal([]byte(relManifest), &relMan); err != nil {
		return ids, err
	}
	if !relMan.VerifyHash() || relMan.ManifestHash != relHash {
		return ids, fmt.Errorf("the imported release document does not verify its hash")
	}
	ids.RSGStateHash = m.StateHash
	ids.ReleaseManifestHash = relMan.ManifestHash
	ids.Git.StateGitRef = m.GitRef

	for _, s := range relMan.Schemas {
		ids.SchemaVersions = append(ids.SchemaVersions, fmt.Sprintf("%s@%s#%s", s.ID, s.Version, short(s.ContentHash)))
	}
	sort.Strings(ids.SchemaVersions)
	if relMan.Policy != nil {
		if relMan.Policy.Organization != nil {
			ids.PolicyVersions = append(ids.PolicyVersions, fmt.Sprintf("organization:%s@%s", relMan.Policy.Organization.ID, relMan.Policy.Organization.Version))
		}
		if relMan.Policy.Project != nil {
			ids.PolicyVersions = append(ids.PolicyVersions, fmt.Sprintf("project:%s@%s", relMan.Policy.Project.ID, relMan.Policy.Project.Version))
		}
	}
	sort.Strings(ids.PolicyVersions)

	// The blob identifier class, read from the TARGET's object store: the
	// bytes are fetched and hashed here, so a target that stored the wrong
	// bytes under the right key is caught.
	rows, err := pool.Query(ctx, `
		SELECT id::text, content_hash, coalesce(storage_key, '')
		FROM blobs WHERE id IN (
			SELECT ba.blob_id FROM blob_attachments ba
			JOIN scientific_object_versions sov ON sov.id = ba.scientific_object_version_id
			JOIN scientific_objects so ON so.id = sov.object_id WHERE so.project_id = $1)
		ORDER BY content_hash, id`, projectID)
	if err != nil {
		return ids, err
	}
	type blobTarget struct{ id, hash, key string }
	var targets []blobTarget
	for rows.Next() {
		var b blobTarget
		if err := rows.Scan(&b.id, &b.hash, &b.key); err != nil {
			rows.Close()
			return ids, err
		}
		targets = append(targets, b)
	}
	rows.Close()
	store := newBlobStore(e)
	hashes := map[string]bool{}
	for _, b := range targets {
		bucket, key, err := s3Target(b.key)
		if err != nil {
			return ids, err
		}
		body, err := store.get(ctx, bucket, key)
		if err != nil {
			return ids, fmt.Errorf("read blob %s back from %s/%s: %w", b.id, bucket, key, err)
		}
		got := sha256Hex(body)
		if got != b.hash {
			return ids, fmt.Errorf("blob %s: the bytes at %s/%s hash to sha256:%s while the row pins sha256:%s", b.id, bucket, key, got, b.hash)
		}
		hashes[got] = true
	}
	ids.BlobHashes = sortedBlobHashes(hashes)
	return ids, nil
}

// CompareIdentifiers asserts the four classes agree and reports every value
// on both sides. It is the acceptance criterion made executable.
func CompareIdentifiers(x *Index, target IdentifierSet) error {
	fmt.Printf("IDENTIFIERS export side\n")
	fmt.Printf("  rsg_state_hash         %s\n", x.Identifiers.RSGStateHash)
	fmt.Printf("  release_manifest_hash  %s\n", x.Identifiers.ReleaseManifestHash)
	fmt.Printf("  git repository         %s\n", x.Identifiers.Git.Repository)
	fmt.Printf("  git head ref           %s\n", x.Identifiers.Git.HeadRef)
	fmt.Printf("  git commit_sha         %s\n", x.Identifiers.Git.CommitSHA)
	fmt.Printf("  git state_git_ref      %s\n", gitRefText(x.Identifiers.Git.StateGitRef))
	fmt.Printf("  blob hashes (%d)        %s\n", len(x.Identifiers.BlobHashes), strings.Join(shortAll(x.Identifiers.BlobHashes), " "))
	fmt.Printf("  schema versions        %s\n", strings.Join(x.Identifiers.SchemaVersions, " "))
	fmt.Printf("  policy versions        %s\n", strings.Join(x.Identifiers.PolicyVersions, " "))
	fmt.Printf("IDENTIFIERS import side\n")
	fmt.Printf("  rsg_state_hash         %s\n", target.RSGStateHash)
	fmt.Printf("  release_manifest_hash  %s\n", target.ReleaseManifestHash)
	fmt.Printf("  git repository         %s\n", target.Git.Repository)
	fmt.Printf("  git head ref           %s\n", target.Git.HeadRef)
	fmt.Printf("  git commit_sha         %s\n", target.Git.CommitSHA)
	fmt.Printf("  git state_git_ref      %s\n", gitRefText(target.Git.StateGitRef))
	fmt.Printf("  blob hashes (%d)        %s\n", len(target.BlobHashes), strings.Join(shortAll(target.BlobHashes), " "))
	fmt.Printf("  schema versions        %s\n", strings.Join(target.SchemaVersions, " "))
	fmt.Printf("  policy versions        %s\n", strings.Join(target.PolicyVersions, " "))

	if x.Identifiers.RSGStateHash != target.RSGStateHash {
		return fmt.Errorf("RSG snapshot hash differs: export %s, import %s", x.Identifiers.RSGStateHash, target.RSGStateHash)
	}
	if x.Identifiers.ReleaseManifestHash != target.ReleaseManifestHash {
		return fmt.Errorf("release manifest hash differs: export %s, import %s", x.Identifiers.ReleaseManifestHash, target.ReleaseManifestHash)
	}
	if x.Identifiers.Git.CommitSHA != target.Git.CommitSHA {
		return fmt.Errorf("git commit differs: export %s, import %s", x.Identifiers.Git.CommitSHA, target.Git.CommitSHA)
	}
	if strings.Join(x.Identifiers.BlobHashes, ",") != strings.Join(target.BlobHashes, ",") {
		return fmt.Errorf("blob content-hash set differs:\n  export %s\n  import %s", strings.Join(x.Identifiers.BlobHashes, " "), strings.Join(target.BlobHashes, " "))
	}
	if strings.Join(x.Identifiers.SchemaVersions, ",") != strings.Join(target.SchemaVersions, ",") {
		return fmt.Errorf("schema versions differ:\n  export %s\n  import %s", strings.Join(x.Identifiers.SchemaVersions, " "), strings.Join(target.SchemaVersions, " "))
	}
	if strings.Join(x.Identifiers.PolicyVersions, ",") != strings.Join(target.PolicyVersions, ",") {
		return fmt.Errorf("policy versions differ:\n  export %s\n  import %s", strings.Join(x.Identifiers.PolicyVersions, " "), strings.Join(target.PolicyVersions, " "))
	}
	if gitRefText(x.Identifiers.Git.StateGitRef) != gitRefText(target.Git.StateGitRef) {
		return fmt.Errorf("the RSG manifest's own git_ref differs: export %s, import %s", gitRefText(x.Identifiers.Git.StateGitRef), gitRefText(target.Git.StateGitRef))
	}
	fmt.Printf("IDENTIFIERS AGREE (all four classes)\n")
	return nil
}

func gitRefText(p *string) string {
	if p == nil || *p == "" {
		return "<null>"
	}
	return *p
}

func shortAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, short(s))
	}
	return out
}

// ---------------------------------------------------------------------------
// tamper
// ---------------------------------------------------------------------------

// Tamper writes a damaged copy of a bundle for one of the three classes the
// acceptance criterion names.
//
// Each case refreshes EXPORT.json's own digests after damaging the file. That
// is the point: without it the driver's transport digest would fire first and
// the tamper would prove nothing about the product's content addressing —
// which is the thing that has to hold. With it, what fires is either the
// state_hash re-derivation (manifest), the blob content address (blob) or the
// bundle's own refs (commit), and the refusal names which.
func Tamper(bundleDir, outDir, kind string) error {
	if err := copyTree(bundleDir, outDir); err != nil {
		return err
	}
	x, err := readIndex(outDir)
	if err != nil {
		return err
	}
	switch kind {
	case "blob":
		if len(x.Blobs) == 0 {
			return fmt.Errorf("the bundle carries no blob to damage")
		}
		target := filepath.Join(outDir, blobsDir, x.Blobs[0].ContentHash)
		body, err := os.ReadFile(target)
		if err != nil {
			return err
		}
		if len(body) == 0 {
			return fmt.Errorf("blob %s is empty; flipping a byte would be meaningless", x.Blobs[0].ContentHash)
		}
		body[len(body)/2] ^= 0x01
		if err := os.WriteFile(target, body, 0o644); err != nil {
			return err
		}
		fmt.Printf("TAMPER blob: flipped one byte in %s/%s\n", blobsDir, short(x.Blobs[0].ContentHash))
	case "manifest":
		path := filepath.Join(outDir, manifestF)
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var m manifest.Manifest
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		if len(m.ObjectVersions) == 0 {
			return fmt.Errorf("the manifest has no object version to change a line of")
		}
		before := m.ObjectVersions[0].Title
		m.ObjectVersions[0].Title = before + " (tampered)"
		body, err := json.Marshal(m)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			return err
		}
		fmt.Printf("TAMPER manifest: changed the title of object version %s from %q to %q\n", short(m.ObjectVersions[0].ID), before, m.ObjectVersions[0].Title)
	case "commit":
		home, err := workDir("git-tamper")
		if err != nil {
			return err
		}
		defer os.RemoveAll(home)
		work := filepath.Join(home, "work")
		// The scratch repository's initial branch is deliberately NOT
		// "main": git refuses to fetch into the branch HEAD is on, and an
		// unborn HEAD already counts. Initialising on a name no bundle
		// carries keeps the fetch below from colliding with the very ref
		// this case is about to swap.
		initBranch := "t1208-tamper-scratch"
		for {
			if _, taken := x.Identifiers.Git.Refs["refs/heads/"+initBranch]; !taken {
				break
			}
			initBranch += "-x"
		}
		if _, err := runGit(home, "", home, "init", "-q", "-b", initBranch, work); err != nil {
			return err
		}
		if _, err := runGit(work, "", home, "fetch", "--force", filepath.Join(outDir, gitBundleF), "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"); err != nil {
			return err
		}
		headRef := x.Identifiers.Git.HeadRef
		branch, ok := strings.CutPrefix(headRef, "refs/heads/")
		if !ok {
			return fmt.Errorf("the pinned head ref %s is not a branch, so this case does not know what to swap", headRef)
		}
		// -B puts HEAD ON the branch rather than detaching it. A plain
		// `checkout <full ref>` leaves a detached HEAD, the commit below
		// then advances only HEAD, and the branch keeps the pinned commit —
		// a "swapped commit" that never reaches the bundle.
		if _, err := runGit(work, "", home, "checkout", "-q", "-B", branch, headRef); err != nil {
			return err
		}
		if _, err := runGit(work, "", home, "-c", "user.email=gate@post.local", "-c", "user.name=T1208 tamper", "commit", "-q", "--allow-empty", "-m", "a swapped commit on the pinned ref"); err != nil {
			return err
		}
		swapped, err := runGit(work, "", home, "rev-parse", headRef)
		if err != nil {
			return err
		}
		if swapped == x.Identifiers.Git.CommitSHA {
			return fmt.Errorf("the tamper did not land: %s still resolves to the pinned %s", headRef, short(swapped))
		}
		if _, err := runGit(work, "", home, "bundle", "create", filepath.Join(outDir, gitBundleF), "--all"); err != nil {
			return err
		}
		fmt.Printf("TAMPER commit: %s now points at %s instead of the pinned %s\n", x.Identifiers.Git.HeadRef, short(swapped), short(x.Identifiers.Git.CommitSHA))
	default:
		return fmt.Errorf("unknown tamper kind %q (blob|manifest|commit)", kind)
	}
	return rewriteIndex(outDir, x)
}
