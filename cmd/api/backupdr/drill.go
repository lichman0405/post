package backupdr

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The drill itself: back up the four classes, restore them into a target
// that was PROVEN empty, reconcile the four axes, and open the five
// objects docs/37 §V1 验收 names. Everything in this file is orchestration;
// the rules live in the files that state them (postgres.go for the dump,
// s3.go for the store, git.go for the repositories, secrets.go for the
// fourth class, reconcile.go for the axes, open.go for the acceptance).

// Cluster is one environment plus the credentials the drill reaches it
// with. The credentials are the dev stack's (infra/docker, ops/
// DEV_COMMANDS.md); nothing in this package reads a production deployment
// credential, and docs/25 §32 forbids a Worker doing so at all.
type Cluster struct {
	Env
	BlobAccessKey    string
	BlobSecretKey    string
	GiteaAdminUser   string
	GiteaAdminPass   string
	GiteaServiceAcct string
}

// Config is one drill run's inputs.
type Config struct {
	// RunID scopes every resource the drill creates (the target bucket,
	// the target organization, the token) so a run never collides with
	// another and cleanup can only delete its own — docs/66 §3's rule for
	// parallel work against one stack.
	RunID string
	// ArtifactsDir is where the backup and the report are written. It is
	// outside version control by construction (the drill refuses a
	// directory inside the repository's tracked tree — see
	// EnsureArtifactsDirOutsideRepo).
	ArtifactsDir string
	// Source is the environment backed up. The drill never writes to it.
	Source Cluster
	// Target is the EMPTY environment restored into.
	Target Cluster
	// SecretValues are the credential values the artifacts must not
	// contain; they are looked for, never written. The caller supplies
	// them because only the caller knows which values its fixture used.
	SecretValues map[string]string
	// RepoRoot is the repository working tree, used only to refuse an
	// artifact directory inside it.
	RepoRoot string
}

// EnsureArtifactsDirOutsideRepo creates the artifact directory and refuses
// one inside the repository's tracked tree.
//
// The refusal is the point: a restore drill writes a database dump, blob
// copies and git mirrors, and the one place those must never end up is a
// commit. `.gitignore` covers the documented default location, but a
// caller that overrides POST_DRILL_ARTIFACTS to somewhere tracked would be
// one `git add -A` from committing a dump — so the drill checks the
// location instead of trusting the ignore file to save it.
func EnsureArtifactsDirOutsideRepo(dir, repoRoot string) error {
	if dir == "" {
		return fmt.Errorf("drill: artifacts directory is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if repoRoot != "" {
		root, err := filepath.Abs(repoRoot)
		if err != nil {
			return err
		}
		switch {
		case abs == root:
			return fmt.Errorf("drill: artifacts directory %s is the repository root itself", abs)
		case strings.HasPrefix(abs, root+string(filepath.Separator)):
			rel, relErr := filepath.Rel(root, abs)
			if relErr != nil {
				rel = abs
			}
			// Exactly one tracked-tree location is allowed, and it is the
			// gitignored default: a directory the repository explicitly
			// ignores cannot be committed by accident.
			if !ignoredArtifactDir(rel) {
				return fmt.Errorf("drill: artifacts directory %s is inside the repository (%s) and is not the "+
					"ignored default %s — a backup artifact must never be committable",
					abs, filepath.ToSlash(rel), DefaultArtifactsRelPath)
			}
		}
	}
	return os.MkdirAll(abs, 0o755)
}

// DefaultArtifactsRelPath is the one location inside the repository the
// drill accepts, because .gitignore ignores it (the entry T1110 added).
const DefaultArtifactsRelPath = ".backup-dr"

func ignoredArtifactDir(rel string) bool {
	rel = filepath.ToSlash(rel)
	return rel == DefaultArtifactsRelPath || strings.HasPrefix(rel, DefaultArtifactsRelPath+"/")
}

// DefaultArtifactsDir returns the artifact directory a run uses when the
// caller names none: POST_DRILL_ARTIFACTS when set, otherwise the ignored
// default under the repository root.
func DefaultArtifactsDir(repoRoot string) string {
	if v := os.Getenv("POST_DRILL_ARTIFACTS"); v != "" {
		return v
	}
	return filepath.Join(repoRoot, DefaultArtifactsRelPath)
}

// ---------------------------------------------------------------------------
// The concrete read-only source

// drillSource is the reconciler's whole world: a target database, a target
// object store and a target Git infrastructure, exposed through reads only.
// It satisfies ReconcileSource and NOTHING else — the write methods the
// same collaborators have (PutObject, createRepo, pushMirror) are not on
// the interface, so Reconcile cannot reach them (see reconcile.go).
type drillSource struct {
	pool   *pgxpool.Pool
	bucket string
	s3     *s3Client
	git    *gitClient
	// defaultRefs caches each repository's default ref so the reverse ref
	// check does not re-ask per ref.
	defaultRefs map[string]string
}

func newDrillSource(pool *pgxpool.Pool, bucket string, s3 *s3Client, git *gitClient) *drillSource {
	return &drillSource{pool: pool, bucket: bucket, s3: s3, git: git, defaultRefs: map[string]string{}}
}

func (d *drillSource) BranchRefs(ctx context.Context) ([]BranchRefRow, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT b.project_id::text,
		       COALESCE(p.owner, ''), COALESCE(p.name, ''),
		       r.git_ref, COALESCE(r.head_sha, ''), r.sync_state
		  FROM git_branch_refs r
		  JOIN branches b ON b.id = r.branch_id
		  LEFT JOIN git_repository_provisions p ON p.project_id = b.project_id
		 ORDER BY r.git_ref`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BranchRefRow
	for rows.Next() {
		var r BranchRefRow
		if err := rows.Scan(&r.ProjectID, &r.Owner, &r.Repo, &r.GitRef, &r.HeadSHA, &r.SyncState); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *drillSource) Blobs(ctx context.Context) ([]BlobRow, error) {
	rows, err := d.pool.Query(ctx, `SELECT id::text, content_hash, size_bytes, storage_key FROM blobs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BlobRow
	for rows.Next() {
		var b BlobRow
		if err := rows.Scan(&b.ID, &b.ContentHash, &b.SizeBytes, &b.StorageKey); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (d *drillSource) Releases(ctx context.Context) ([]ReleaseRow, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id::text, project_id::text, version, state_id::text, manifest, manifest_hash
		  FROM releases ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReleaseRow
	for rows.Next() {
		var r ReleaseRow
		var manifest string
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.Version, &r.StateID, &manifest, &r.ManifestHash); err != nil {
			return nil, err
		}
		r.Manifest = []byte(manifest)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *drillSource) AssetVersions(ctx context.Context) ([]AssetVersionRow, error) {
	rows, err := d.pool.Query(ctx, `SELECT id::text, manifest::text, integrity_hash FROM research_asset_versions ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AssetVersionRow
	for rows.Next() {
		var a AssetVersionRow
		var manifest string
		if err := rows.Scan(&a.ID, &manifest, &a.IntegrityHash); err != nil {
			return nil, err
		}
		a.Manifest = []byte(manifest)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (d *drillSource) States(ctx context.Context) (map[string]StateRow, error) {
	rows, err := d.pool.Query(ctx, `SELECT id::text, project_id::text, state_hash, git_commit_sha FROM project_states`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]StateRow{}
	for rows.Next() {
		var id string
		var s StateRow
		if err := rows.Scan(&id, &s.ProjectID, &s.StateHash, &s.GitCommitSHA); err != nil {
			return nil, err
		}
		out[id] = s
	}
	return out, rows.Err()
}

func (d *drillSource) ReadObject(ctx context.Context, storageKey string) ([]byte, error) {
	return d.s3.GetObject(ctx, d.bucket, storageKey)
}

func (d *drillSource) RemoteRefs(ctx context.Context, owner, repo string) (map[string]string, error) {
	return d.git.remoteRefs(ctx, owner, repo)
}

func (d *drillSource) DefaultRefs(ctx context.Context, owner, repo string) (string, error) {
	key := owner + "/" + repo
	if v, ok := d.defaultRefs[key]; ok {
		return v, nil
	}
	raw, err := d.git.api(ctx, "GET", "/api/v1/repos/"+owner+"/"+repo, nil)
	if err != nil {
		return "", err
	}
	var info repoInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return "", err
	}
	if info.DefaultBranch == "" {
		return "", fmt.Errorf("git: %s reports no default branch", key)
	}
	ref := "refs/heads/" + info.DefaultBranch
	d.defaultRefs[key] = ref
	return ref, nil
}

// HasCommit answers whether a commit is an object of a restored repository.
// It clones the repository once into a scratch directory and asks git
// directly — the question is about an object, not about a ref tip, so
// ls-remote cannot answer it.
func (d *drillSource) HasCommit(ctx context.Context, owner, repo, sha string) (bool, error) {
	dir, err := os.MkdirTemp("", "post-drill-objectcheck-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(dir)
	mirror := filepath.Join(dir, "repo.git")
	if _, err := d.git.git(ctx, "", "clone", "--mirror", "--quiet", d.git.cloneURL(owner, repo), mirror); err != nil {
		return false, err
	}
	if _, err := d.git.git(ctx, mirror, "cat-file", "-e", sha+"^{commit}"); err != nil {
		// git answers "not an object of this repository" with a non-zero
		// exit; that is the answer, not a failure of the check.
		if strings.Contains(err.Error(), "Not a valid object name") ||
			strings.Contains(err.Error(), "could not get object info") ||
			strings.Contains(err.Error(), "exists on disk, but not in") ||
			strings.Contains(err.Error(), "exit status 128") ||
			strings.Contains(err.Error(), "exit status 1") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// auditSink records a pass's findings in audit_log — the platform's own
// activity record (migration 00014's append-only guard makes the rows
// immutable, so a finding cannot later be edited out).
//
// It implements FindingSink and NOTHING ELSE. There is no Repair or Apply
// method on it, and there is no path from a Finding to a write of any kind
// other than this append — which is what makes the "report, never repair"
// rule structural rather than a matter of care.
type auditSink struct{ pool *pgxpool.Pool }

// NewAuditSink returns the audit_log sink for a restored environment's
// database.
func NewAuditSink(pool *pgxpool.Pool) FindingSink { return &auditSink{pool: pool} }

// RecordFindings appends one row per finding: via 'system' (no actor — this
// is the platform watching itself), action backup.restore.drift_detected,
// correlation_id = the pass, and the proposal recorded under
// metadata.repair_proposal with metadata.applied = false.
func (s *auditSink) RecordFindings(ctx context.Context, passID string, findings []Finding) error {
	for _, f := range findings {
		metadata, err := json.Marshal(map[string]any{
			"severity":        SeverityHigh,
			"axis":            string(f.Axis),
			"kind":            f.Kind,
			"expected":        f.Expected,
			"actual":          f.Actual,
			"repair_proposal": f.RepairProposal,
			"pass_id":         passID,
			"applied":         false,
		})
		if err != nil {
			return err
		}
		var projectID *string
		if f.ProjectID != "" {
			id := f.ProjectID
			projectID = &id
		}
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO audit_log (actor_id, via, action, target_ref, project_id, correlation_id, metadata)
			VALUES (NULL, 'system', $1, $2, $3, $4, $5)`,
			driftAuditAction, string(f.Axis)+":"+f.Subject, projectID, passID, metadata); err != nil {
			return fmt.Errorf("record finding %s/%s: %w", f.Axis, f.Kind, err)
		}
	}
	return nil
}

// Backup takes the four classes into dir and returns the sealed manifest.
func Backup(ctx context.Context, cfg Config) (*BackupManifest, error) {
	if err := EnsureArtifactsDirOutsideRepo(cfg.ArtifactsDir, cfg.RepoRoot); err != nil {
		return nil, err
	}
	dir := cfg.ArtifactsDir

	m := &BackupManifest{
		FormatVersion: FormatVersion,
		// One instant for the whole backup (docs/37 §一致性). Taken here,
		// before anything is read, so it is a lower bound on the backup's
		// coverage rather than a claim about when each class finished.
		SnapshotTimestamp: time.Now().UTC().Truncate(time.Millisecond),
		Source:            cfg.Source.Env,
		ConfigMetadata:    configMetadata(),
	}
	// The manifest is written early and rewritten as classes land, so a
	// backup that fails halfway leaves a reader a record of how far it got
	// instead of an empty directory.
	if err := writeJSON(filepath.Join(dir, fileManifest), m); err != nil {
		return nil, err
	}

	// 1. Postgres.
	schemaFile := filepath.Join(dir, fileSchemaSQL)
	if err := dumpSchema(ctx, cfg.Source.PostgresURL, schemaFile); err != nil {
		return nil, fmt.Errorf("backup: postgres schema: %w", err)
	}
	schemaRaw, err := os.ReadFile(schemaFile)
	if err != nil {
		return nil, err
	}
	records, redactions, rows, err := dumpData(ctx, cfg.Source.PostgresURL, filepath.Join(dir, dirTableData))
	if err != nil {
		return nil, fmt.Errorf("backup: postgres data: %w", err)
	}
	m.Postgres = DumpRecord{
		SchemaFile:      fileSchemaSQL,
		SchemaSHA256:    sha256Hex(schemaRaw),
		DataDir:         dirTableData,
		TableCount:      len(records),
		RowCount:        rows,
		Tables:          records,
		RedactedColumns: redactions,
	}
	if seq, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(fileSequences))); err == nil {
		m.Postgres.SequenceFile = fileSequences
		m.Postgres.SequenceSHA256 = sha256Hex(seq)
	}
	if err := writeJSON(filepath.Join(dir, fileManifest), m); err != nil {
		return nil, err
	}

	// 2. S3 blobs/manifests.
	s3, err := newS3Client(cfg.Source.BlobEndpoint, cfg.Source.BlobAccessKey, cfg.Source.BlobSecretKey, false)
	if err != nil {
		return nil, err
	}
	objects, err := s3.ListObjects(ctx, cfg.Source.BlobBucket, "")
	if err != nil {
		return nil, fmt.Errorf("backup: list blobs: %w", err)
	}
	blobDir := filepath.Join(dir, dirBlobs)
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		return nil, err
	}
	for _, o := range objects {
		body, err := s3.GetObject(ctx, cfg.Source.BlobBucket, o.Key)
		if err != nil {
			return nil, fmt.Errorf("backup: read blob %s: %w", o.Key, err)
		}
		sum := sha256Hex(body)
		// Content-addressed inside the artifact too: the file name IS the
		// digest, so a corrupted copy is detectable before any restore.
		if err := os.WriteFile(filepath.Join(blobDir, sum), body, 0o644); err != nil {
			return nil, err
		}
		m.Blobs = append(m.Blobs, BlobRecord{
			StorageKey:  o.Key,
			ContentHash: sum,
			SizeBytes:   int64(len(body)),
			File:        filepath.ToSlash(filepath.Join(dirBlobs, sum)),
		})
	}
	sort.Slice(m.Blobs, func(i, j int) bool { return m.Blobs[i].StorageKey < m.Blobs[j].StorageKey })
	if err := writeJSON(filepath.Join(dir, fileManifest), m); err != nil {
		return nil, err
	}

	// 3. Gitea repositories.
	if cfg.Source.GiteaOwner != "" {
		tokenID, token, err := mintToken(ctx, cfg.Source.GiteaBaseURL,
			cfg.Source.GiteaAdminUser, cfg.Source.GiteaAdminPass, cfg.Source.GiteaServiceAcct,
			"t1110-backup-"+cfg.RunID)
		if err != nil {
			return nil, fmt.Errorf("backup: gitea credential: %w", err)
		}
		defer func() {
			_ = revokeToken(context.WithoutCancel(ctx), cfg.Source.GiteaBaseURL,
				cfg.Source.GiteaAdminUser, cfg.Source.GiteaAdminPass, cfg.Source.GiteaServiceAcct, tokenID)
		}()
		git := newGitClient(cfg.Source.GiteaBaseURL, token, cfg.RunID)
		repos, err := git.listRepos(ctx, cfg.Source.GiteaOwner)
		if err != nil {
			return nil, fmt.Errorf("backup: list repositories: %w", err)
		}
		for _, repo := range repos {
			mirror := filepath.Join(dir, dirGitMirrors, cfg.Source.GiteaOwner, repo.Name+".git")
			def, refs, err := git.mirrorRepo(ctx, cfg.Source.GiteaOwner, repo.Name, mirror)
			if err != nil {
				return nil, fmt.Errorf("backup: %s: %w", repo.Name, err)
			}
			m.GitRepositories = append(m.GitRepositories, GitRecord{
				Coordinate: cfg.Source.GiteaOwner + "/" + repo.Name,
				MirrorPath: filepath.ToSlash(filepath.Join(dirGitMirrors, cfg.Source.GiteaOwner, repo.Name+".git")),
				DefaultRef: def,
				Refs:       refs,
			})
		}
	}
	sort.Slice(m.GitRepositories, func(i, j int) bool {
		return m.GitRepositories[i].Coordinate < m.GitRepositories[j].Coordinate
	})

	if err := m.Seal(); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(dir, fileManifest), m); err != nil {
		return nil, err
	}

	// The check that makes the redaction a verified property: no artifact
	// this backup just wrote may contain any declared credential value.
	secrets := secretValueList(cfg.SecretValues)
	hits, err := ScanArtifactsForSecrets(dir, secrets)
	if err != nil {
		return nil, err
	}
	if len(hits) > 0 {
		return nil, fmt.Errorf("backup: REFUSED — an artifact contains a credential value (%s). "+
			"A backup that leaks a secret is not a backup", describeHits(hits, nameByValue(cfg.SecretValues)))
	}
	return m, nil
}

// secretValueList flattens the declared secret values.
func secretValueList(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// nameByValue inverts the declared secret map so a hit can name WHAT leaked
// without quoting it.
func nameByValue(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for name, v := range m {
		out[v] = name
	}
	return out
}

// Restore fills the empty target from the backup directory and returns the
// sealed restore manifest.
func Restore(ctx context.Context, cfg Config, backupDir string) (*RestoreManifest, error) {
	raw, err := os.ReadFile(filepath.Join(backupDir, fileManifest))
	if err != nil {
		return nil, fmt.Errorf("restore: read backup manifest: %w", err)
	}
	backup, err := ParseBackupManifest(raw)
	if err != nil {
		return nil, err
	}

	m := &RestoreManifest{
		FormatVersion: FormatVersion,
		// The backup's instant, carried forward: this is the field that
		// makes "restored to which moment" answerable.
		SnapshotTimestamp: backup.SnapshotTimestamp,
		Target:            cfg.Target.Env,
	}

	// 1. The target must be EMPTY, and the proof is recorded before
	// anything is written (docs/37 §V1 验收: 空环境 restore test).
	emptiness, err := proveEmpty(ctx, cfg)
	if err != nil {
		return nil, err
	}
	m.Emptiness = emptiness
	for _, c := range emptiness {
		if !c.Empty {
			return nil, fmt.Errorf("restore: REFUSED — the target is not empty (%s %s: %s). "+
				"A restore into a non-empty environment would satisfy the words of the "+
				"acceptance criterion while disproving its claim", c.Class, c.Subject, c.Observed)
		}
	}
	if err := writeJSON(filepath.Join(backupDir, fileRestore), m); err != nil {
		return nil, err
	}

	// 2. Postgres.
	if err := restoreSchema(ctx, cfg.Target.PostgresURL, filepath.Join(backupDir, backup.Postgres.SchemaFile)); err != nil {
		return nil, fmt.Errorf("restore: schema: %w", err)
	}
	dumpRows, err := restoreData(ctx, cfg.Target.PostgresURL, filepath.Join(backupDir, backup.Postgres.DataDir), backup.Postgres.Tables)
	if err != nil {
		return nil, fmt.Errorf("restore: data: %w", err)
	}
	// The two numbers in restore.json are COUNTED IN THE TARGET, not copied
	// from the backup manifest. The manifest's TableCount/rows describe what
	// the backup wrote; repeating them here would report the same numbers
	// whether the load landed or not, which is the shape of a check that
	// reads green while measuring nothing. tableCounts is the exact-count
	// instrument the emptiness proof already used on this same target a
	// moment ago, so the before and after numbers come from one definition.
	counts, err := restoredCounts(ctx, cfg.Target.PostgresURL)
	if err != nil {
		return nil, err
	}
	m.Tables = len(counts)
	m.Rows = 0
	for _, n := range counts {
		m.Rows += n
	}
	if m.Rows != dumpRows {
		// The dump carried dumpRows rows' worth of COPY data and the target
		// holds a different number: the load did not land what it says it
		// did, and a restore report that papered over that would be the one
		// lie this drill exists to catch.
		return nil, fmt.Errorf("restore: the dump carried %d rows but the target holds %d — the data load did not land what the backup recorded",
			dumpRows, m.Rows)
	}

	// 3. Blobs.
	s3, err := newS3Client(cfg.Target.BlobEndpoint, cfg.Target.BlobAccessKey, cfg.Target.BlobSecretKey, false)
	if err != nil {
		return nil, err
	}
	if err := s3.CreateBucket(ctx, cfg.Target.BlobBucket); err != nil {
		return nil, err
	}
	for _, b := range backup.Blobs {
		body, err := os.ReadFile(filepath.Join(backupDir, filepath.FromSlash(b.File)))
		if err != nil {
			return nil, fmt.Errorf("restore: read artifact blob %s: %w", b.File, err)
		}
		if got := sha256Hex(body); got != b.ContentHash {
			return nil, fmt.Errorf("restore: artifact blob %s digests to %s, manifest says %s — the backup is corrupt",
				b.File, got, b.ContentHash)
		}
		if err := s3.PutObject(ctx, cfg.Target.BlobBucket, b.StorageKey, body); err != nil {
			return nil, fmt.Errorf("restore: write blob %s: %w", b.StorageKey, err)
		}
		m.Blobs++
	}

	// 4. Gitea repositories.
	var pool *pgxpool.Pool
	if len(backup.GitRepositories) > 0 {
		tokenID, token, err := mintToken(ctx, cfg.Target.GiteaBaseURL,
			cfg.Target.GiteaAdminUser, cfg.Target.GiteaAdminPass, cfg.Target.GiteaServiceAcct,
			"t1110-restore-"+cfg.RunID)
		if err != nil {
			return nil, fmt.Errorf("restore: gitea credential: %w", err)
		}
		defer func() {
			_ = revokeToken(context.WithoutCancel(ctx), cfg.Target.GiteaBaseURL,
				cfg.Target.GiteaAdminUser, cfg.Target.GiteaAdminPass, cfg.Target.GiteaServiceAcct, tokenID)
		}()
		git := newGitClient(cfg.Target.GiteaBaseURL, token, cfg.RunID)
		if err := git.createOrg(ctx, cfg.Target.GiteaOwner); err != nil {
			return nil, fmt.Errorf("restore: create target organization: %w", err)
		}
		for _, rec := range backup.GitRepositories {
			_, name, _ := strings.Cut(rec.Coordinate, "/")
			if err := git.createRepo(ctx, cfg.Target.GiteaOwner, name); err != nil {
				return nil, fmt.Errorf("restore: create repository %s: %w", name, err)
			}
			mirror := filepath.Join(backupDir, filepath.FromSlash(rec.MirrorPath))
			if err := git.pushMirror(ctx, mirror, cfg.Target.GiteaOwner, name); err != nil {
				return nil, err
			}
			m.GitRepos++
			if rec.Coordinate != cfg.Target.GiteaOwner+"/"+name {
				m.Rebindings = append(m.Rebindings, Rebinding{
					Class: "git",
					From:  rec.Coordinate,
					To:    cfg.Target.GiteaOwner + "/" + name,
					Why: "a shared Git infrastructure already hosts the source organization, so the restored " +
						"repositories live under the drill's own owner; refs and commits are untouched — only " +
						"the address changed",
				})
			}
		}
	}

	// 5. Repoint the restored database at the restored infrastructure, and
	// record every repointing. Only addresses move: no ref, no commit, no
	// content hash and no manifest digest is rewritten anywhere.
	pool, err = pgxpool.New(ctx, cfg.Target.PostgresURL)
	if err != nil {
		return nil, fmt.Errorf("restore: open target for rebinding: %w", err)
	}
	defer pool.Close()
	for _, rb := range m.Rebindings {
		if rb.Class != "git" {
			continue
		}
		srcOwner, _, _ := strings.Cut(rb.From, "/")
		rows, err := pool.Exec(ctx,
			`UPDATE git_repository_provisions SET owner = $1 WHERE owner = $2`,
			cfg.Target.GiteaOwner, srcOwner)
		if err != nil {
			return nil, fmt.Errorf("restore: rebind repository coordinates: %w", err)
		}
		if rows.RowsAffected() == 0 {
			return nil, fmt.Errorf("restore: rebinding %s → %s affected no rows", rb.From, rb.To)
		}
	}

	if err := m.Seal(); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(backupDir, fileRestore), m); err != nil {
		return nil, err
	}
	return m, nil
}

// proveEmpty gathers the evidence that the target holds no data, class by
// class. It is the drill's own reading of the target BEFORE it writes
// anything, and it is what makes "空环境" a checked property instead of an
// assumption about how the environment was built.
// restoredCounts reads the target's exact per-table row counts, opening and
// closing its own pool so it can be called right after the data load.
func restoredCounts(ctx context.Context, targetURL string) (map[string]int, error) {
	pool, err := pgxpool.New(ctx, targetURL)
	if err != nil {
		return nil, fmt.Errorf("restore: target database unreachable: %w", err)
	}
	defer pool.Close()
	counts, err := tableCounts(ctx, pool)
	if err != nil {
		return nil, fmt.Errorf("restore: count the target's rows: %w", err)
	}
	return counts, nil
}

func proveEmpty(ctx context.Context, cfg Config) ([]EmptinessCheck, error) {
	var checks []EmptinessCheck

	pool, err := pgxpool.New(ctx, cfg.Target.PostgresURL)
	if err != nil {
		return nil, fmt.Errorf("restore: target database unreachable: %w", err)
	}
	defer pool.Close()
	counts, err := tableCounts(ctx, pool)
	if err != nil {
		return nil, fmt.Errorf("restore: read target row counts: %w", err)
	}
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)
	total := 0
	for _, name := range names {
		total += counts[name]
	}
	checks = append(checks, EmptinessCheck{
		Class:    "postgres",
		Subject:  "every table in schema public",
		Observed: fmt.Sprintf("%d tables, %d rows", len(names), total),
		Empty:    total == 0,
	})

	s3, err := newS3Client(cfg.Target.BlobEndpoint, cfg.Target.BlobAccessKey, cfg.Target.BlobSecretKey, false)
	if err != nil {
		return nil, err
	}
	exists, err := s3.BucketExists(ctx, cfg.Target.BlobBucket)
	switch {
	case err != nil:
		return nil, fmt.Errorf("restore: probe target bucket %s: %w", cfg.Target.BlobBucket, err)
	case !exists:
		checks = append(checks, EmptinessCheck{
			Class: "blobs", Subject: "bucket " + cfg.Target.BlobBucket,
			Observed: "bucket does not exist", Empty: true,
		})
	default:
		objects, err := s3.ListObjects(ctx, cfg.Target.BlobBucket, "")
		if err != nil {
			return nil, fmt.Errorf("restore: list target bucket: %w", err)
		}
		checks = append(checks, EmptinessCheck{
			Class: "blobs", Subject: "bucket " + cfg.Target.BlobBucket,
			Observed: fmt.Sprintf("%d objects", len(objects)), Empty: len(objects) == 0,
		})
	}

	if cfg.Target.GiteaOwner != "" {
		tokenID, token, err := mintToken(ctx, cfg.Target.GiteaBaseURL,
			cfg.Target.GiteaAdminUser, cfg.Target.GiteaAdminPass, cfg.Target.GiteaServiceAcct,
			"t1110-emptyprobe-"+cfg.RunID)
		if err != nil {
			return nil, fmt.Errorf("restore: gitea credential for the emptiness probe: %w", err)
		}
		defer func() {
			_ = revokeToken(context.WithoutCancel(ctx), cfg.Target.GiteaBaseURL,
				cfg.Target.GiteaAdminUser, cfg.Target.GiteaAdminPass, cfg.Target.GiteaServiceAcct, tokenID)
		}()
		git := newGitClient(cfg.Target.GiteaBaseURL, token, cfg.RunID)
		repos, err := git.listRepos(ctx, cfg.Target.GiteaOwner)
		switch {
		case err == nil:
			checks = append(checks, EmptinessCheck{
				Class: "git", Subject: "organization " + cfg.Target.GiteaOwner,
				Observed: fmt.Sprintf("%d repositories", len(repos)), Empty: len(repos) == 0,
			})
		case isGiteaNotFound(err):
			// An organization that is not there has no repositories, which is
			// the empty case.
			checks = append(checks, EmptinessCheck{
				Class: "git", Subject: "organization " + cfg.Target.GiteaOwner,
				Observed: "organization does not exist", Empty: true,
			})
		default:
			// Anything else is the probe FAILING to ask. Reporting that as
			// "empty" would be the whole drill's worst outcome — an empty
			// environment asserted on the strength of a question that never
			// got answered.
			return nil, fmt.Errorf("restore: probe target organization %s: %w", cfg.Target.GiteaOwner, err)
		}
	}
	return checks, nil
}

// writeJSON writes a document with the same canonical rendering the digests
// are taken over, so an artifact re-reads to the bytes it was sealed from.
func writeJSON(path string, v any) error {
	raw, err := canonicalJSON(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}
