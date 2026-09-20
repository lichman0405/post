package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/backupdr"
	"github.com/lichman0405/post/internal/application/releases"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
	rsgmanifest "github.com/lichman0405/post/internal/rsg/manifest"
)

// Required test "restore drill": docs/37 §V1 验收's empty-environment
// restore, against real PostgreSQL + MinIO + Gitea.
//
//	必须至少做一次空环境 restore test，并成功打开 Seed Project、Release、
//	Asset、Files、Evidence Graph
//
// The test crosses three real services, so it is a HEAVY test and every
// dependency it cannot reach is a LOUD skip naming that dependency — the
// shape tests/integration/git_reconciliation_test.go:18-24 establishes.
// CI runs PostgreSQL only, so the drill's real coverage is the G3 gate
// (ops/backup-restore-drill.sh); a silent pass here would be the one
// outcome worse than a skip.
//
// What it asserts, in the order it happens:
//
//  1. the four classes are backed up into an artifact outside the
//     repository, carrying ONE snapshot timestamp;
//  2. the target is PROVEN empty before anything is written to it;
//  3. the restore fills it, and the restore document carries the backup's
//     instant forward (docs/37 §一致性: snapshot timestamp);
//  4. the reconciliation of the four axes passes CLEAN on the restored
//     environment — the control;
//  5. the target holds what the backup recorded: restore.json's
//     tables_restored/rows_restored are COUNTED IN THE TARGET, and this
//     test recounts it and compares per table against the backup manifest.
//     Content is checked too, but by the means each class really has: the
//     restored blob object digests to its blobs.content_hash (docs/17 §3),
//     and the restored Git repository is verified by COMMIT IDENTITY (the
//     source commit is read back out of it) — NOT by any sha256 of the
//     files, which is not a check openFiles performs: opening a Files
//     object is an existence read (provider-reported sha and size);
//  6. one thing is deliberately broken — a restored blob's bytes are
//     replaced with different bytes of the SAME length — and the
//     reconciliation REPORTS it: a high-severity finding per class, an
//     audit row per finding, and the broken bytes still broken after
//     (the reconciler NEVER repairs, cmd/api/reconciliation.go:11-19);
//  7. all five objects open through real product read paths;
//  8. no credential value appears anywhere in the artifacts.

const restoreDrillTaskID = "T1110"

// drillFixture is the source environment: one project with a branch, a
// state, a released version, a published asset version, a blob, an
// evidence assertion and a real Gitea repository carrying a real commit.
type drillFixture struct {
	srcPool   *pgxpool.Pool
	srcURL    string
	tgtPool   *pgxpool.Pool
	tgtURL    string
	runID     string
	srcOwner  string
	tgtOwner  string
	repoName  string
	commitSHA string
	blobBytes []byte
	blobHash  string
	storage   string
	blobID    string
	projectID string
	releaseID string
	assetID   string
	stateID   string
	userID    string
	artifacts string
	srcBucket string
	tgtBucket string
}

// requiredRestoreDrillDeps gates the whole test on the three services.
// Each skip names its missing dependency and what runs it for real, because
// a heavy test that quietly turns green without its services is a test that
// certifies nothing.
func requireRestoreDrillDeps(t *testing.T, ctx context.Context) (admin, blobEndpoint, giteaBase string) {
	t.Helper()
	// PostgreSQL: the drill dumps and restores two databases.
	//
	// pgxpool.New is LAZY — it does not connect — so the liveness probe has
	// to be an actual round trip. A pool constructor that "succeeded" and a
	// skip that never fired would leave testdb.Setup failing with a
	// connection refused inside the test body, which reads as a broken
	// drill rather than as a missing dependency.
	admin = adminURL(t)
	probe, err := pgxpool.New(ctx, admin)
	if err != nil {
		t.Skipf("restore drill: cannot build a pool for the admin PostgreSQL URL (%v) — the drill's "+
			"postgres half needs the dev stack (`make infra-up`); the G3 gate runs it "+
			"(ops/backup-restore-drill.sh)", err)
	}
	if err := probe.Ping(ctx); err != nil {
		probe.Close()
		// The error names the host and the user; pgx does not echo the
		// password, and the URL is not printed here at all.
		t.Skipf("restore drill: no PostgreSQL reachable at the configured admin URL (%v) — the drill's "+
			"postgres half needs the dev stack (`make infra-up` + `make infra-init`); the G3 gate runs "+
			"it (ops/backup-restore-drill.sh)", err)
	}
	probe.Close()

	// MinIO: the blobs/manifests class. Probed by creating the run-scoped
	// source bucket, which is work the test needs anyway.
	blobEndpoint = envDefault("POST_BLOB_ENDPOINT", "http://127.0.0.1:9000")

	// Gitea: the repositories class (and the Files open).
	giteaBase = requireGitea(t)
	return admin, blobEndpoint, giteaBase
}

// drillGiteaToken mints this run's token for the Git half.
//
// It is not giteaServiceToken: that helper's token carries
// write:repository + write:user, and creating an ORGANIZATION needs
// write:organization (verified — the same call answers 403 without it and
// 201 with it). The drill's own mintToken asks for all three, so the test
// asks for the same three, for the same account, through the same dev
// admin's basic auth, and revokes it the same way. No new credential and
// no new account: it is the scope list that differs.
//
// POST_GITEA_TOKEN is deliberately NOT honoured here. Its documented dev
// value is exactly that two-scope token (init-gitea.sh, .env.dev), and the
// G3 gate injects it into every G3 job's environment — so a suite run as a
// G3 job would 403 at org creation the moment this helper deferred to it.
// Deferring buys nothing anyway: org deletion below already authenticates
// as the dev admin, and backupdr's restore-side mint does the same, so no
// environment the drill runs in lacks the basic-auth credential.
func drillGiteaToken(t *testing.T, base string) string {
	t.Helper()
	adminUser := envDefault("GITEA_ADMIN_USER", "postadmin")
	adminPass := envDefault("GITEA_ADMIN_PASSWORD", "postadmin_dev_pw")
	svc := envDefault("GITEA_SERVICE_ACCOUNT", "post-git-svc")
	body := fmt.Sprintf(`{"name":"t1110-drill-%d","scopes":["write:repository","write:user","write:organization"]}`,
		time.Now().UnixNano())
	req, err := http.NewRequest(http.MethodPost,
		base+"/api/v1/users/"+url.PathEscape(svc)+"/tokens", strings.NewReader(body))
	if err != nil {
		t.Fatalf("restore drill: build token request: %v", err)
	}
	req.SetBasicAuth(adminUser, adminPass)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("restore drill: mint token: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("restore drill: mint token = %d (admin %s basic auth)", resp.StatusCode, adminUser)
	}
	var minted struct {
		ID   int64  `json:"id"`
		SHA1 string `json:"sha1"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&minted); err != nil {
		t.Fatalf("restore drill: decode minted token: %v", err)
	}
	if minted.SHA1 == "" {
		t.Fatal("restore drill: minted token has no sha1")
	}
	t.Cleanup(func() {
		req, err := http.NewRequest(http.MethodDelete,
			fmt.Sprintf("%s/api/v1/users/%s/tokens/%d", base, url.PathEscape(svc), minted.ID), nil)
		if err != nil {
			return
		}
		req.SetBasicAuth(adminUser, adminPass)
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Logf("restore drill: revoke token failed: %v", err)
			return
		}
		resp.Body.Close()
	})
	return minted.SHA1
}

// TestDrillGiteaTokenIgnoresTheEnvironmentToken pins the helper's refusal
// to honour POST_GITEA_TOKEN: the documented dev value of that variable is
// the two-scope service token (init-gitea.sh, .env.dev), the G3 gate
// injects it into every G3 job's environment, and org creation answers 403
// without write:organization. On code that defers to the variable this test
// fails — no mint happens and the environment value comes back.
func TestDrillGiteaTokenIgnoresTheEnvironmentToken(t *testing.T) {
	minted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/tokens"):
			minted = true
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1,"sha1":"minted-three-scope-token"}`)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("POST_GITEA_TOKEN", "env-token-must-not-be-used")

	if got := drillGiteaToken(t, srv.URL); got != "minted-three-scope-token" {
		t.Fatalf("drillGiteaToken returned %q, want the freshly minted three-scope token", got)
	}
	if !minted {
		t.Fatal("drillGiteaToken never minted: POST_GITEA_TOKEN short-circuited the three-scope mint")
	}
}

// newDrillFixture builds the source environment: one migrated database with
// the fixture rows, one target database created EMPTY, one run-scoped source
// organization holding a real repository with a real commit, and one
// run-scoped source bucket holding the fixture blob.
func newDrillFixture(t *testing.T, ctx context.Context) *drillFixture {
	t.Helper()
	admin, blobEndpoint, giteaBase := requireRestoreDrillDeps(t, ctx)

	f := &drillFixture{
		runID:     drillRunID(),
		srcOwner:  "",
		tgtOwner:  "",
		srcBucket: "",
		tgtBucket: "",
	}
	f.srcOwner = "drill-src-" + f.runID
	f.tgtOwner = "drill-tgt-" + f.runID
	f.repoName = "drill-repo-" + f.runID
	f.srcBucket = "post-drill-src-" + f.runID
	f.tgtBucket = "post-drill-tgt-" + f.runID

	// The two databases. The SOURCE is migrated and seeded; the TARGET is
	// created EMPTY and gets its schema from the backup — restoring into a
	// migrated database would be a different test (and would collide on
	// every CREATE TABLE).
	f.srcPool, f.srcURL = testdb.Setup(t, ctx, admin, restoreDrillTaskID)
	f.tgtPool, f.tgtURL = testdb.SetupEmpty(t, ctx, admin, restoreDrillTaskID)

	// The artifact directory: OUTSIDE the repository, because a restore
	// drill writes dumps and blob copies and none of them may be
	// committable.
	//
	// POST_DRILL_ARTIFACTS overrides it, which is how the G3 gate aims a run
	// at .backup-dr/ (ops/backup-restore-drill.sh). That location is inside
	// the working tree and is allowed for exactly one reason — .gitignore
	// covers it — so an override is still put through the drill's own guard
	// rather than trusted: pointing this variable at a tracked directory
	// fails the run, which is the behaviour a backup artifact needs.
	f.artifacts = os.Getenv("POST_DRILL_ARTIFACTS")
	if f.artifacts == "" {
		f.artifacts = filepath.Join(t.TempDir(), "artifacts")
	}
	if err := backupdr.EnsureArtifactsDirOutsideRepo(f.artifacts, repoRoot(t)); err != nil {
		t.Fatalf("restore drill: artifact directory: %v", err)
	}

	// The source repository: a real org, a real repo and a real commit,
	// pushed with git itself.
	token := drillGiteaToken(t, giteaBase)
	giteaCreateOrg(t, giteaBase, token, f.srcOwner)
	f.commitSHA = giteaPushCommit(t, giteaBase, token, f.srcOwner, f.repoName, f.runID)
	t.Cleanup(func() { giteaDeleteOrg(t, giteaBase, f.srcOwner, f.runID) })
	// The target organization must NOT exist yet: the emptiness proof and
	// the restore both depend on that, and a leftover organization from a
	// previous run of the same name would make "the target was empty" a
	// claim about something else.
	t.Cleanup(func() { giteaDeleteOrg(t, giteaBase, f.tgtOwner, f.runID) })

	// The source blob object.
	f.blobBytes = []byte("the drill's blob bytes, published as " + f.runID)
	f.blobHash = drillSHA256(f.blobBytes)
	f.storage = "drill/" + f.runID + "/" + f.blobHash

	srcCfg := backupdr.SourceConfig{
		Env: backupdr.Env{
			BlobEndpoint: blobEndpoint,
			BlobBucket:   f.srcBucket,
			GiteaBaseURL: giteaBase,
		},
		BlobAccessKey: envDefault("POST_BLOB_ACCESS_KEY", "minio_dev"),
		BlobSecretKey: envDefault("POST_BLOB_SECRET_KEY", "minio_dev_pw"),
		RunID:         f.runID,
	}
	if err := backupdr.EnsureBucket(ctx, srcCfg, f.srcBucket); err != nil {
		t.Skipf("restore drill: no object store at %s (%v) — the drill's blob half needs MinIO "+
			"(`make infra-up`); the G3 gate runs it (ops/backup-restore-drill.sh)", blobEndpoint, err)
	}
	t.Cleanup(func() { _ = backupdr.DeleteRestoredObject(context.Background(), srcCfg, f.srcBucket, f.storage) })
	if err := backupdr.PutRestoredObject(ctx, srcCfg, f.srcBucket, f.storage, f.blobBytes); err != nil {
		t.Fatalf("restore drill: seed the source blob: %v", err)
	}

	f.seedRows(t, ctx)
	return f
}

// seedRows builds the source environment's canonical rows. It uses the
// product's own stores where they exist (users, projects, branches) and SQL
// where the product's write path is another task's (releases T0606, assets
// T0705): the drill is about restoring and reconciling, not about
// re-testing those writers, and the documents it stores are the ones those
// writers' own verifiers accept.
func (f *drillFixture) seedRows(t *testing.T, ctx context.Context) {
	t.Helper()
	passwordHash := "drill-secret-password-hash-" + f.runID
	webhookSecret := "drill-webhook-secret-" + f.runID

	user, err := persistence.NewCredentialStore(f.srcPool).CreateWithPassword(
		ctx, "drill-"+f.runID+"@example.com", passwordHash, "drill-"+f.runID, "Drill Seed")
	if err != nil {
		t.Fatalf("restore drill: seed user: %v", err)
	}
	f.userID = user.ID

	project, _, err := persistence.NewProjectStore(f.srcPool).CreateProject(ctx, domain.Project{
		Slug:            "drill-" + f.runID,
		Name:            "Restore Drill Seed",
		Purpose:         "T1110 — the seed project the drill restores and opens",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, f.userID)
	if err != nil {
		t.Fatalf("restore drill: seed project: %v", err)
	}
	f.projectID = project.ID

	// The provisioning row: the source coordinate, and a webhook secret
	// the backup must NOT carry.
	if _, err := f.srcPool.Exec(ctx, `INSERT INTO git_repository_provisions
		(project_id, owner, name, gitea_repo_id, webhook_id, webhook_secret)
		VALUES ($1::uuid, $2, $3, 1, 1, $4)`, f.projectID, f.srcOwner, f.repoName, webhookSecret); err != nil {
		t.Fatalf("restore drill: seed provision row: %v", err)
	}

	// The genesis state, pinned to the real commit.
	stateHash := "drill-state-" + f.runID
	if err := f.srcPool.QueryRow(ctx, `INSERT INTO project_states
		(project_id, state_hash, manifest_version, git_commit_sha)
		VALUES ($1, $2, 'v1', $3) RETURNING id`,
		f.projectID, stateHash, f.commitSHA).Scan(&f.stateID); err != nil {
		t.Fatalf("restore drill: seed state: %v", err)
	}

	// A branch and its canonical ref mapping, driven through the state
	// machine the same way tests/integration/git_reconciliation_test.go
	// does.
	var branchID string
	if err := f.srcPool.QueryRow(ctx, `INSERT INTO branches
		(project_id, name, visibility, git_ref, base_state_id, created_by)
		VALUES ($1, 'main', 'private', 'refs/heads/main', $2, $3) RETURNING id`,
		f.projectID, f.stateID, f.userID).Scan(&branchID); err != nil {
		t.Fatalf("restore drill: seed branch: %v", err)
	}
	if _, err := f.srcPool.Exec(ctx, `UPDATE git_branch_refs
		SET git_ref = 'refs/heads/main', sync_state = 'synced', fork_sha = $2, head_sha = $2
		WHERE branch_id = $1`, branchID, f.commitSHA); err != nil {
		t.Fatalf("restore drill: seed ref mapping: %v", err)
	}

	// The blob row AND its object (the object was written above).
	if err := f.srcPool.QueryRow(ctx, `INSERT INTO blobs
		(content_hash, size_bytes, storage_key, integrity_state, created_by)
		VALUES ($1, $2, $3, 'verified', $4) RETURNING id`,
		f.blobHash, len(f.blobBytes), f.storage, f.userID).Scan(&f.blobID); err != nil {
		t.Fatalf("restore drill: seed blob row: %v", err)
	}

	// One scientific object version and one evidence assertion about it,
	// so the Evidence Graph open has a graph to read.
	var objectVersionID string
	if err := f.srcPool.QueryRow(ctx, `INSERT INTO scientific_objects
		(project_id, object_type, created_by) VALUES ($1, 'dataset', $2) RETURNING id`,
		f.projectID, f.userID).Scan(&objectVersionID); err != nil {
		t.Fatalf("restore drill: seed object: %v", err)
	}
	var versionID string
	if err := f.srcPool.QueryRow(ctx, `INSERT INTO scientific_object_versions
		(object_id, version_no, state_id, branch_id, schema_id, schema_version, title,
		 lifecycle_state, payload, integrity_hash, created_by)
		VALUES ($1, 1, $2, $3, 'dataset', '1', 'Drill Seed Dataset', 'active', '{}'::jsonb, $4, $5)
		RETURNING id`, objectVersionID, f.stateID, branchID, "drill-ov-"+f.runID, f.userID).Scan(&versionID); err != nil {
		t.Fatalf("restore drill: seed object version: %v", err)
	}
	var evidenceObjectVersionID string
	if err := f.srcPool.QueryRow(ctx, `INSERT INTO scientific_objects
		(project_id, object_type, created_by) VALUES ($1, 'dataset', $2) RETURNING id`,
		f.projectID, f.userID).Scan(&evidenceObjectVersionID); err != nil {
		t.Fatalf("restore drill: seed evidence object: %v", err)
	}
	var evidenceVersionID string
	if err := f.srcPool.QueryRow(ctx, `INSERT INTO scientific_object_versions
		(object_id, version_no, state_id, branch_id, schema_id, schema_version, title,
		 lifecycle_state, payload, integrity_hash, created_by)
		VALUES ($1, 1, $2, $3, 'dataset', '1', 'Drill Seed Evidence', 'active', '{}'::jsonb, $4, $5)
		RETURNING id`, evidenceObjectVersionID, f.stateID, branchID, "drill-ev-"+f.runID, f.userID).Scan(&evidenceVersionID); err != nil {
		t.Fatalf("restore drill: seed evidence version: %v", err)
	}
	if _, err := f.srcPool.Exec(ctx, `INSERT INTO evidence_assertions
		(project_id, state_id, target_object_version_id, evidence_object_version_id,
		 relation_type, evidence_type, created_by)
		VALUES ($1, $2, $3, $4, 'supports', 'dataset', $5)`,
		f.projectID, f.stateID, versionID, evidenceVersionID, f.userID); err != nil {
		t.Fatalf("restore drill: seed evidence assertion: %v", err)
	}

	// The release: a document the product's own export path accepts, with
	// the state pin and the blob pin the release_pins axis resolves.
	manifest, manifestHash := drillReleaseManifest(t, f.projectID, f.stateID, stateHash, f.commitSHA, versionID, f.blobID, f.blobHash)
	// The manifest is inserted as TEXT, and that is not an accident of the
	// column type: migration 00053 turned releases.manifest from jsonb into
	// text precisely because jsonb reorders a nested document's keys and
	// the manifest's hash is taken over those exact bytes ("T0606 e2e: the
	// manifest endpoint 503'd on VerifyHash after the jsonb round trip").
	// Inserting through ::jsonb here would reproduce that bug in the
	// fixture and make the restore look like it broke the release.
	if err := f.srcPool.QueryRow(ctx, `INSERT INTO releases
		(project_id, version, title, state_id, manifest, manifest_hash, created_by)
		VALUES ($1, '1.0.0', 'Drill Seed Release', $2, $3, $4, $5) RETURNING id`,
		f.projectID, f.stateID, manifest, manifestHash, f.userID).Scan(&f.releaseID); err != nil {
		t.Fatalf("restore drill: seed release: %v", err)
	}

	// The published asset version.
	var assetRowID string
	if err := f.srcPool.QueryRow(ctx, `INSERT INTO research_assets
		(asset_type, slug, title, origin_project_id) VALUES ('dataset', $1, 'Drill Seed Asset', $2) RETURNING id`,
		"drill-asset-"+f.runID, f.projectID).Scan(&assetRowID); err != nil {
		t.Fatalf("restore drill: seed asset: %v", err)
	}
	assetManifest, assetHash := drillAssetManifest(t)
	if err := f.srcPool.QueryRow(ctx, `INSERT INTO research_asset_versions
		(asset_id, version, source_release_id, manifest, rights_json, visibility, integrity_hash, published_by, origin_refs)
		VALUES ($1, '1.0.0', $2, $3::jsonb, '{}'::jsonb, 'public', $4, $5, ARRAY[$6]::text[])
		RETURNING id`, assetRowID, f.releaseID, assetManifest, assetHash, f.userID, "project:"+f.projectID).Scan(&f.assetID); err != nil {
		t.Fatalf("restore drill: seed asset version: %v", err)
	}
}

// drillReleaseManifest builds the release document through the release
// package's own type and digest rule — nothing here reimplements a hash.
func drillReleaseManifest(t *testing.T, projectID, stateID, stateHash, commitSHA, objectVersionID, blobID, blobHash string) (string, string) {
	t.Helper()
	stateContent, err := json.Marshal(map[string]any{
		"format_version": "v1",
		"project_id":     projectID,
		"state_id":       stateID,
		"object_versions": []map[string]any{
			{"id": objectVersionID, "object_type": "dataset", "version_no": 1},
		},
		"relation_versions": []any{},
		"schema_refs":       []string{"dataset"},
		"policy_refs":       []string{},
		"blob_refs": []map[string]string{
			{"id": blobID, "hash": blobHash},
		},
		"git_ref":    commitSHA,
		"state_hash": stateHash,
	})
	if err != nil {
		t.Fatalf("restore drill: marshal state content: %v", err)
	}
	m := releases.ReleaseManifest{
		FormatVersion: releases.FormatV1,
		ProjectID:     projectID,
		StateID:       stateID,
		Version:       "1.0.0",
		GeneratedAt:   time.Now().UTC(),
		State:         releases.StatePin{StateHash: stateHash, Content: stateContent},
		Schemas:       []releases.SchemaPin{},
		Reviews:       []releases.ReviewRecord{},
	}
	hashInput, err := m.ContentCanonicalJSON()
	if err != nil {
		t.Fatalf("restore drill: canonical content: %v", err)
	}
	m.ManifestHash = rsgmanifest.Digest(hashInput)
	raw, err := m.CanonicalJSON()
	if err != nil {
		t.Fatalf("restore drill: canonical document: %v", err)
	}
	if !m.VerifyHash() {
		t.Fatal("restore drill: the fixture's release document does not verify against its own hash")
	}
	return string(raw), m.ManifestHash
}

// drillAssetManifest builds a manifest the publish gate's own verifier
// accepts: every required metadata field of the dataset type, filled with a
// value of the kind the table declares.
func drillAssetManifest(t *testing.T) (string, string) {
	t.Helper()
	md := assets.Metadata{}
	for _, field := range assets.RequiredMetadata(assets.TypeDataset) {
		switch field.Kind {
		case assets.KindText:
			md[field.Key] = "drill value for " + field.Key
		case assets.KindTextList:
			// []any and map[string]any, not []string: the manifest's value
			// shapes are the ones a jsonb round trip produces, which is what
			// the publisher's own validator sees.
			md[field.Key] = []any{"one", "two"}
		case assets.KindTextObject:
			md[field.Key] = map[string]any{"unit": "kelvin"}
		case assets.KindEnum:
			if len(field.Values) == 0 {
				t.Fatalf("restore drill: enum metadata field %s has no vocabulary", field.Key)
			}
			md[field.Key] = field.Values[0]
		default:
			t.Fatalf("restore drill: unknown metadata kind %q for %s", field.Kind, field.Key)
		}
	}
	m := assets.Manifest{
		Version:        assets.ManifestFormatVersion,
		AssetType:      assets.TypeDataset,
		Metadata:       md,
		DependencyPins: []assets.DependencyPin{},
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("restore drill: the fixture's asset manifest does not validate: %v", err)
	}
	raw, err := m.CanonicalJSON()
	if err != nil {
		t.Fatalf("restore drill: canonical asset manifest: %v", err)
	}
	hash, err := m.Hash()
	if err != nil {
		t.Fatalf("restore drill: hash the asset manifest: %v", err)
	}
	// The hash must round-trip through the same derivation the drill's
	// Asset open uses; otherwise the open would fail on a fixture bug.
	reparsed, err := assets.ManifestHash(raw)
	if err != nil || reparsed != hash {
		t.Fatalf("restore drill: the asset manifest hash does not round-trip (%v, %q vs %q)", err, reparsed, hash)
	}
	return string(raw), hash
}

// ---------------------------------------------------------------------------
// The required test

func TestRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	f := newDrillFixture(t, ctx)

	giteaBase := giteaBase(t)
	blobEndpoint := envDefault("POST_BLOB_ENDPOINT", "http://127.0.0.1:9000")
	token := giteaServiceToken(t, giteaBase)
	adminUser := envDefault("GITEA_ADMIN_USER", "postadmin")
	adminPass := envDefault("GITEA_ADMIN_PASSWORD", "postadmin_dev_pw")
	svcAccount := envDefault("GITEA_SERVICE_ACCOUNT", "post-git-svc")

	// Every declared credential value. The fixture's two are recognisable
	// and UNIQUE to this run, so a scan that finds one has found a real
	// leak of a value that could only come from a backup artifact.
	secrets := map[string]string{
		"POST_DB_PASSWORD":                         passwordOf(t, f.srcURL),
		"POST_BLOB_SECRET_KEY":                     envDefault("POST_BLOB_SECRET_KEY", "minio_dev_pw"),
		"users.password_hash":                      "drill-secret-password-hash-" + f.runID,
		"git_repository_provisions.webhook_secret": "drill-webhook-secret-" + f.runID,
	}
	if adminPass != "" {
		secrets["GITEA_ADMIN_PASSWORD"] = adminPass
	}

	cluster := func(env backupdr.Env) backupdr.Cluster {
		return backupdr.Cluster{
			Env:              env,
			BlobAccessKey:    envDefault("POST_BLOB_ACCESS_KEY", "minio_dev"),
			BlobSecretKey:    envDefault("POST_BLOB_SECRET_KEY", "minio_dev_pw"),
			GiteaAdminUser:   adminUser,
			GiteaAdminPass:   adminPass,
			GiteaServiceAcct: svcAccount,
		}
	}

	cfg := backupdr.RunConfig{
		Config: backupdr.Config{
			RunID:        f.runID,
			ArtifactsDir: f.artifacts,
			RepoRoot:     repoRoot(t),
			Source: cluster(backupdr.Env{
				PostgresURL:  f.srcURL,
				BlobEndpoint: blobEndpoint,
				BlobBucket:   f.srcBucket,
				GiteaBaseURL: giteaBase,
				GiteaOwner:   f.srcOwner,
			}),
			Target: cluster(backupdr.Env{
				PostgresURL:  f.tgtURL,
				BlobEndpoint: blobEndpoint,
				BlobBucket:   f.tgtBucket,
				GiteaBaseURL: giteaBase,
				GiteaOwner:   f.tgtOwner,
			}),
			SecretValues: secrets,
		},
		Open: backupdr.OpenParams{
			ProjectID:  f.projectID,
			ReleaseID:  f.releaseID,
			AssetID:    f.assetID,
			FilesOwner: f.tgtOwner,
			FilesRepo:  f.repoName,
		},
		GiteaServiceAccount: svcAccount,
	}

	run, err := backupdr.Run(ctx, cfg)
	if err != nil {
		t.Fatalf("restore drill: %v", err)
	}
	report := run.Report

	// --- 1. the backup carries one instant, readable after the restore ---
	t.Run("snapshot timestamp survives the restore", func(t *testing.T) {
		if report.SnapshotTimestamp == "" {
			t.Fatal("the run reports no snapshot timestamp: nothing says which moment was restored")
		}
		if report.Backup.SnapshotTimestamp.IsZero() {
			t.Fatal("the backup manifest carries no snapshot timestamp (docs/37 §一致性)")
		}
		if !report.Backup.VerifyHash() {
			t.Fatal("the backup manifest does not verify against its own hash")
		}
		restoreRaw, err := os.ReadFile(filepath.Join(f.artifacts, "restore.json"))
		if err != nil {
			t.Fatalf("read the restore document: %v", err)
		}
		rm, err := backupdr.ParseRestoreManifest(restoreRaw)
		if err != nil {
			t.Fatalf("the restore document does not parse and verify: %v", err)
		}
		if !rm.SnapshotTimestamp.Equal(report.Backup.SnapshotTimestamp.UTC()) {
			t.Fatalf("the restore records instant %s but the backup was taken at %s — the restored-to moment is ambiguous",
				rm.SnapshotTimestamp, report.Backup.SnapshotTimestamp)
		}
		rep, err := backupdr.ReadReport(run.ReportPath)
		if err != nil {
			t.Fatalf("the written report does not read back: %v", err)
		}
		if rep.SnapshotTimestamp != report.SnapshotTimestamp {
			t.Fatalf("the report's instant %q is not the restore's %q", rep.SnapshotTimestamp, report.SnapshotTimestamp)
		}
	})

	// --- 2. the target was proven empty before anything was written ---
	t.Run("empty environment, proven not assumed", func(t *testing.T) {
		if len(report.Restore.Emptiness) == 0 {
			t.Fatal("the restore records no emptiness evidence — an empty-environment restore with no proof of emptiness is an assumption")
		}
		classes := map[string]bool{}
		for _, c := range report.Restore.Emptiness {
			if !c.Empty {
				t.Fatalf("the %s class reported a NON-empty target: %s", c.Class, c.Observed)
			}
			classes[c.Class] = true
		}
		for _, want := range []string{"postgres", "blobs", "git"} {
			if !classes[want] {
				t.Fatalf("no emptiness evidence for the %s class (got %v)", want, classes)
			}
		}
		t.Logf("emptiness evidence: %+v", report.Restore.Emptiness)
	})

	// --- 3. the restore actually filled the target, by content -----------
	t.Run("restored content matches, not counts", func(t *testing.T) {
		var restoredBlobHash, restoredStorage string
		if err := f.tgtPool.QueryRow(ctx,
			`SELECT content_hash, storage_key FROM blobs WHERE id = $1`, f.blobID).
			Scan(&restoredBlobHash, &restoredStorage); err != nil {
			t.Fatalf("the restored blobs row is not there: %v", err)
		}
		if restoredBlobHash != f.blobHash || restoredStorage != f.storage {
			t.Fatalf("restored blob row = (%s, %s), want (%s, %s)", restoredBlobHash, restoredStorage, f.blobHash, f.storage)
		}
		tgtCfg := cfg.Target
		got, err := backupdr.GetRestoredObject(ctx, backupdr.SourceConfig{
			Env:           tgtCfg.Env,
			BlobAccessKey: tgtCfg.BlobAccessKey,
			BlobSecretKey: tgtCfg.BlobSecretKey,
			RunID:         f.runID,
		}, f.tgtBucket, f.storage)
		if err != nil {
			t.Fatalf("read the restored object: %v", err)
		}
		if drillSHA256(got) != f.blobHash {
			t.Fatalf("the restored bytes digest to %s, the recorded content_hash is %s — the CONTENT does not match",
				drillSHA256(got), f.blobHash)
		}
		if string(got) != string(f.blobBytes) {
			t.Fatal("the restored bytes are not the backed-up bytes")
		}

		// The Git half, by commit identity rather than by ref count: the
		// source commit must be an object of the restored repository.
		out := gitOutAuth(t, "", token,
			"ls-remote", strings.TrimSuffix(giteaBase, "/")+"/"+f.tgtOwner+"/"+f.repoName+".git", "refs/heads/main")
		if !strings.Contains(out, f.commitSHA) {
			t.Fatalf("the restored repository's main is not at %s:\n%s", f.commitSHA, out)
		}

		// The postgres half, by counting the target itself. restore.json's
		// tables_restored/rows_restored are measured in the target, and this
		// counts it a second time, independently, and compares PER TABLE
		// against what the backup recorded writing. The assertion this
		// replaces was `!= 0` — a number the manifest supplied, checked only
		// for being non-zero, which a restore that dropped every row but one
		// would still pass.
		recorded := map[string]int{}
		for _, tr := range report.Backup.Postgres.Tables {
			recorded[tr.Name] = tr.Rows
		}
		if len(recorded) == 0 {
			t.Fatal("the backup manifest records no tables at all")
		}
		held := drillTableCounts(t, ctx, f.tgtPool)
		for name, rows := range recorded {
			got, ok := held[name]
			if !ok {
				t.Errorf("the backup recorded %d rows in %s; the restored target has no such table", rows, name)
				continue
			}
			if got != rows {
				t.Errorf("the backup recorded %d rows in %s; the restored target holds %d", rows, name, got)
			}
		}
		total := 0
		for name, n := range held {
			total += n
			if _, known := recorded[name]; !known && n != 0 {
				t.Errorf("%s holds %d rows but is not in the backup manifest", name, n)
			}
		}
		if total == 0 {
			t.Error("the restored target holds no rows at all")
		}
		if report.Restore.Rows != total {
			t.Errorf("restore.json reports rows_restored=%d; the target holds %d", report.Restore.Rows, total)
		}
		if report.Restore.Tables != len(held) {
			t.Errorf("restore.json reports tables_restored=%d; schema public of the target has %d tables",
				report.Restore.Tables, len(held))
		}
		t.Logf("target holds %d rows across %d tables, matching the manifest per table", total, len(held))
	})

	// --- 4. the control: a consistent restore reconciles CLEAN -----------
	t.Run("reconciliation is clean on the restored environment", func(t *testing.T) {
		rec := report.Reconciliation
		if rec == nil {
			t.Fatal("the run produced no reconciliation")
		}
		if !rec.Clean() {
			t.Fatalf("the restored environment reported drift: %v", rec.Findings)
		}
		if rec.SnapshotTimestamp != report.SnapshotTimestamp {
			t.Fatalf("the pass reconciled against %q, the restore's instant is %q", rec.SnapshotTimestamp, report.SnapshotTimestamp)
		}
		for _, axis := range backupdr.AllAxes {
			if rec.Checked[axis] == 0 {
				t.Fatalf("axis %q was not checked at all (checked=%v) — a clean verdict from an axis that did not run is not a clean verdict",
					axis, rec.Checked)
			}
		}
		if report.FindingsRecorded != 0 {
			t.Fatalf("a clean pass recorded %d audit rows", report.FindingsRecorded)
		}
		t.Logf("axes checked: %v", rec.Checked)
	})

	// --- 5. all five open -------------------------------------------------
	t.Run("all five objects open on the restored environment", func(t *testing.T) {
		for _, obj := range report.Opened.Objects {
			if !obj.Opened {
				t.Errorf("%s did NOT open: %s (via %s)", obj.Kind, obj.Detail, obj.Via)
				continue
			}
			t.Logf("%s opened via %s: %s", obj.Kind, obj.Via, obj.Detail)
		}
		if !report.Opened.AllOpened() {
			t.Fatalf("docs/37 §V1 验收 names FIVE objects; missing: %v", report.Opened.Missing())
		}
		if len(report.Opened.Objects) != 5 {
			t.Fatalf("got %d opens, want 5", len(report.Opened.Objects))
		}
	})

	// --- 6. no credential value anywhere in the artifacts ---------------
	t.Run("no credential value in the artifacts", func(t *testing.T) {
		if report.SecretScan.Searched == 0 {
			t.Fatal("the scan searched for no values — it proves nothing")
		}
		// The two counts have to account for the declared set exactly, or
		// "searched N declared values" is a security-relevant number
		// standing for work the scan did not do.
		if declared := len(cfg.SecretValues); report.SecretScan.Searched+report.SecretScan.SkippedTooShort != declared {
			t.Fatalf("the scan accounts for %d searched + %d skipped values; the run declared %d",
				report.SecretScan.Searched, report.SecretScan.SkippedTooShort, declared)
		}
		if report.SecretScan.SkippedTooShort != 0 {
			t.Fatalf("the scan skipped %d declared values as shorter than %d bytes, so its empty hit list does not "+
				"cover the set this fixture declared", report.SecretScan.SkippedTooShort, backupdr.MinScannableSecretBytes)
		}
		if report.SecretScan.Hits != 0 {
			t.Fatalf("the artifact scan reported %d files carrying a credential value", report.SecretScan.Hits)
		}
		// The scan is not taken on trust: the redaction is checked where
		// it acts, and the dump the backup wrote is checked for the
		// fixture's own recognisable value.
		usersCopy, err := os.ReadFile(filepath.Join(f.artifacts, "postgres", "data", "users.copy"))
		if err != nil {
			t.Fatalf("the dump has no users table: %v", err)
		}
		if strings.Contains(string(usersCopy), "drill-secret-password-hash-"+f.runID) {
			t.Fatal("the credential value the fixture stored is IN the dump")
		}
		if !strings.Contains(string(usersCopy), backupdr.RedactionPlaceholder) {
			t.Fatal("the password_hash column is neither the value nor the placeholder — the redaction did not run")
		}
		provisionsCopy, err := os.ReadFile(filepath.Join(f.artifacts, "postgres", "data", "git_repository_provisions.copy"))
		if err != nil {
			t.Fatalf("the dump has no provisions table: %v", err)
		}
		if strings.Contains(string(provisionsCopy), "drill-webhook-secret-"+f.runID) {
			t.Fatal("the webhook secret is IN the dump")
		}
	})

	// --- 7. break one thing: the reconciliation must report it ----------
	t.Run("deliberately corrupted content is reported and NOT repaired", func(t *testing.T) {
		tgtSource := backupdr.SourceConfig{
			Env: backupdr.Env{
				PostgresURL:  f.tgtURL,
				BlobEndpoint: blobEndpoint,
				BlobBucket:   f.tgtBucket,
				GiteaBaseURL: giteaBase,
			},
			BlobAccessKey: envDefault("POST_BLOB_ACCESS_KEY", "minio_dev"),
			BlobSecretKey: envDefault("POST_BLOB_SECRET_KEY", "minio_dev_pw"),
			GiteaToken:    token,
			RunID:         f.runID,
		}

		// A blob's CONTENT is replaced — same length, different bytes, so a
		// size or count comparison would still call this consistent.
		corrupted := make([]byte, len(f.blobBytes))
		copy(corrupted, f.blobBytes)
		corrupted[0] ^= 0xff
		corrupted[len(corrupted)-1] ^= 0xff
		if drillSHA256(corrupted) == f.blobHash {
			t.Fatal("the corruption did not change the digest — the test would be asserting nothing")
		}
		if err := backupdr.PutRestoredObject(ctx, tgtSource, f.tgtBucket, f.storage, corrupted); err != nil {
			t.Fatalf("corrupt the restored blob: %v", err)
		}

		src, pool, err := backupdr.NewRestoredSource(ctx, tgtSource)
		if err != nil {
			t.Fatalf("open the restored environment for the drift pass: %v", err)
		}
		defer pool.Close()

		passID := "drill-drift-" + f.runID
		// The sink goes in WITH the pass: the pass records its own findings
		// before it returns, so the rows asserted below are the ones the
		// pass that produced the report wrote, not a second call a caller
		// might have skipped.
		rec, err := backupdr.Reconcile(ctx, backupdr.ReconcileParams{
			Source:            src,
			PassID:            passID,
			SnapshotTimestamp: report.SnapshotTimestamp,
			Sink:              backupdr.NewAuditSink(pool),
		})
		if err != nil {
			t.Fatalf("the drift pass failed instead of reporting: %v", err)
		}
		if rec.Clean() {
			t.Fatal("a restored blob's bytes were replaced and the reconciliation still says CLEAN — " +
				"a reconciler that cannot report drift is not a reconciler")
		}
		var found *backupdr.Finding
		for i := range rec.Findings {
			if rec.Findings[i].Kind == backupdr.KindBlobHashMismatch {
				found = &rec.Findings[i]
			}
		}
		if found == nil {
			t.Fatalf("no blob content-hash finding; got %v", rec.Findings)
		}
		if found.Expected != f.blobHash {
			t.Fatalf("the finding's expected hash is %q, want the recorded %q", found.Expected, f.blobHash)
		}
		if found.Actual != drillSHA256(corrupted) {
			t.Fatalf("the finding's actual hash is %q, want the digest of the corrupted bytes %q", found.Actual, drillSHA256(corrupted))
		}
		if found.RepairProposal == "" {
			t.Fatal("the finding records no repair proposal — a reader is told what is wrong and not what could be done")
		}

		// The audit line: one immutable row per finding, via 'system',
		// severity high.
		var auditRows int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log
			WHERE correlation_id = $1 AND action = 'backup.restore.drift_detected'`, passID).Scan(&auditRows); err != nil {
			t.Fatalf("count the audit rows: %v", err)
		}
		if auditRows != len(rec.Findings) {
			t.Fatalf("%d findings but %d audit rows — a finding without an audit line is a finding nobody will see",
				len(rec.Findings), auditRows)
		}
		var via, severity string
		if err := pool.QueryRow(ctx, `SELECT via, metadata->>'severity' FROM audit_log
			WHERE correlation_id = $1 ORDER BY id LIMIT 1`, passID).Scan(&via, &severity); err != nil {
			t.Fatalf("read the audit row: %v", err)
		}
		if via != "system" || severity != backupdr.SeverityHigh {
			t.Fatalf("audit row is via=%q severity=%q, want system/high (docs/16 §5: ANY drift is a high-severity alert)", via, severity)
		}

		// NEVER repairs: the broken bytes are still broken, and the
		// proposal was recorded rather than applied.
		after, err := backupdr.GetRestoredObject(ctx, tgtSource, f.tgtBucket, f.storage)
		if err != nil {
			t.Fatalf("read the object after the pass: %v", err)
		}
		if drillSHA256(after) != drillSHA256(corrupted) {
			t.Fatalf("the reconciler CHANGED the object it reported on: %s → %s", drillSHA256(corrupted), drillSHA256(after))
		}
		var proposal string
		if err := pool.QueryRow(ctx, `SELECT metadata->>'repair_proposal' FROM audit_log
			WHERE correlation_id = $1 AND metadata->>'kind' = $2 LIMIT 1`,
			passID, backupdr.KindBlobHashMismatch).Scan(&proposal); err != nil {
			t.Fatalf("read the recorded proposal: %v", err)
		}
		if proposal == "" {
			t.Fatal("the audit row records no proposal")
		}
		var applied bool
		if err := pool.QueryRow(ctx, `SELECT (metadata->>'applied')::boolean FROM audit_log
			WHERE correlation_id = $1 AND metadata->>'kind' = $2 LIMIT 1`,
			passID, backupdr.KindBlobHashMismatch).Scan(&applied); err != nil {
			t.Fatalf("read the applied flag: %v", err)
		}
		if applied {
			t.Fatal("the audit row says the proposal was APPLIED — the reconciler must never repair")
		}
		t.Logf("drift reported: %d findings, %d audit rows; proposal recorded and not applied: %s",
			len(rec.Findings), auditRows, proposal)
	})

	// --- 8. audit_log is the report surface, and it is append-only -------
	t.Run("the audit row cannot be edited away", func(t *testing.T) {
		if _, err := f.tgtPool.Exec(ctx, `UPDATE audit_log SET action = 'x'`); err == nil {
			t.Fatal("audit_log accepted an UPDATE — a finding could be edited out of the record")
		}
		if _, err := f.tgtPool.Exec(ctx, `DELETE FROM audit_log`); err == nil {
			t.Fatal("audit_log accepted a DELETE — a finding could be removed from the record")
		}
	})

	// --- 9. this round's acceptance is the process ----------------------
	t.Run("the report claims the process, not an RPO", func(t *testing.T) {
		if !strings.Contains(report.RoundAcceptance, "流程能跑通") {
			t.Fatalf("the report does not state this round's acceptance in docs/37's own words: %s", report.RoundAcceptance)
		}
		if report.RPOClaim != "" {
			t.Fatalf("the report claims %q — docs/37 §RPO/RTO makes V1's acceptance the process, and no RPO was measured", report.RPOClaim)
		}
		if report.RepairsApplied != 0 {
			t.Fatalf("repairs_applied = %d, want 0", report.RepairsApplied)
		}
		// What the restore wrote is checked where it can fail — subtest 3
		// counts the target and compares it per table against the backup.
		// A non-zero test here would only restate numbers the manifest
		// supplied, which is the shape that reads green while measuring
		// nothing; these are printed for the record, not asserted on.
		t.Logf("restored %d tables, %d rows, %d blobs, %d repositories",
			report.Restore.Tables, report.Restore.Rows, report.Restore.Blobs, report.Restore.GitRepos)
	})
}

// ---------------------------------------------------------------------------
// Helpers

func drillRunID() string {
	return time.Now().UTC().Format("20060102t150405") + fmt.Sprintf("%04d", os.Getpid()%10000)
}

func drillSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// drillTableCounts counts the target's rows exactly, per table, over the
// same definition the drill's own instrument uses (schema public, ordinary
// tables) — exact counts rather than pg_class.reltuples, which is a planner
// statistic that reads -1 on a table nobody has analyzed.
func drillTableCounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[string]int {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT c.relname
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind = 'r'
		ORDER BY c.relname`)
	if err != nil {
		t.Fatalf("list the target's tables: %v", err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatalf("scan a table name: %v", err)
		}
		names = append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("list the target's tables: %v", err)
	}
	counts := map[string]int{}
	for _, name := range names {
		var n int
		// The identifier comes from pg_class, never from a caller.
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM public.`+pgx.Identifier{name}.Sanitize()).Scan(&n); err != nil {
			t.Fatalf("count rows in %s: %v", name, err)
		}
		counts[name] = n
	}
	return counts
}

// repoRoot returns the working tree the test is running in. It is used only
// to refuse an artifact directory inside it.
func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("restore drill: not a git working tree (%v) — the drill refuses an artifact directory it "+
			"cannot prove is outside the repository, and this test proves it by asking git", err)
	}
	return strings.TrimSpace(string(out))
}

// passwordOf extracts the password from a postgres URL, so the drill's
// credential scan declares the value that a dump could leak.
func passwordOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("restore drill: parse %s: %v", raw, err)
	}
	pw, _ := u.User.Password()
	return pw
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return gitOutEnv(t, dir, append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), args...)
}

// gitOutAuth is gitOut for a command that talks to the provider: the
// service token rides the environment as an extra header (gitAuthEnv) — the
// shape internal/gitprovider/gitea.go uses — so it is in neither argv nor
// the remote URL.
func gitOutAuth(t *testing.T, dir, token string, args ...string) string {
	t.Helper()
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	return gitOutEnv(t, dir, append(env, gitAuthEnv("Authorization: token "+token)...), args...)
}

// gitOutEnv is gitOut with an explicit environment.
func gitOutEnv(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = env
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, errb.String())
	}
	return out.String()
}

// giteaCreateOrg creates the drill's own organization. The failure message
// carries the provider's answer body: a 403 here is which-middleware
// information ("token does not have required scope" vs "not allowed to
// create organization"), and a run that throws it away has to be reproduced
// to be diagnosed.
func giteaCreateOrg(t *testing.T, base, token, org string) {
	t.Helper()
	body := fmt.Sprintf(`{"username":%q,"description":"T1110 restore drill (created and deleted by the test)"}`, org)
	resp := giteaDo(t, http.MethodPost, base+"/api/v1/orgs", token, body)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// The organization endpoints exist on the dev image; a 404 here
		// means the instance is not the documented one.
		t.Skipf("restore drill: %s/api/v1/orgs answered 404 — the Git half of the drill needs the "+
			"documented dev Gitea (`make infra-up`)", base)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusConflict {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		t.Fatalf("restore drill: create organization %s = %s: %s", org, resp.Status,
			strings.TrimSpace(string(raw)))
	}
}

func adminUserOf() string { return envDefault("GITEA_ADMIN_USER", "postadmin") }

func adminPassOf() string { return envDefault("GITEA_ADMIN_PASSWORD", "postadmin_dev_pw") }

// giteaDeleteOrg removes an organization and its repositories.
//
// Best effort, and LOUD about it. Gitea deletes a repository through a
// background queue, and an organization whose repositories are still queued
// answers 500 "user still has ownership of repositories"; so the deletion is
// retried a few times and, if it still will not go, the leftovers are NAMED
// rather than passed over. A silent cleanup failure would accumulate
// run-scoped organizations in a shared dev instance and make the next run's
// emptiness evidence a claim about somebody else's residue.
//
// It authenticates as the dev ADMIN rather than with the run's token: the
// token is revoked before this runs, and the admin is the identity the
// environment already documents.
func giteaDeleteOrg(t *testing.T, base, org, runID string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last string
	for attempt := 0; attempt < 6; attempt++ {
		resp := giteaAdminDo(t, http.MethodDelete, base+"/api/v1/orgs/"+url.PathEscape(org), "")
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusNoContent, http.StatusOK, http.StatusNotFound:
			return
		default:
			last = fmt.Sprintf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(3 * time.Second)
	}
	t.Logf("restore drill: could not remove the run-scoped organization %s (run %s: %s) — "+
		"left behind in the dev Gitea; it holds no source data and a later run under a new run id is unaffected",
		org, runID, last)
}

// giteaPushCommit creates a repository and pushes one commit to main with
// the real git binary, returning the commit sha.
func giteaPushCommit(t *testing.T, base, token, org, repo, runID string) string {
	t.Helper()
	body := fmt.Sprintf(`{"name":%q,"private":true,"auto_init":false,"default_branch":"main"}`, repo)
	resp := giteaDo(t, http.MethodPost, base+"/api/v1/orgs/"+url.PathEscape(org)+"/repos", token, body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusConflict {
		t.Fatalf("restore drill: create repository %s/%s = %s", org, repo, resp.Status)
	}

	work := t.TempDir()
	gitOut(t, work, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "README.md"),
		[]byte("# restore drill\n\nseeded by T1110 in run "+runID+"\n"), 0o644); err != nil {
		t.Fatalf("write the seed file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(work, "data"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(work, "data", "measurements.csv"),
		[]byte("sample,value\na,1\nb,2\n"), 0o644); err != nil {
		t.Fatalf("write the seed file: %v", err)
	}
	gitOut(t, work, "add", "-A")
	gitOut(t, work, "-c", "user.email=drill@example.com", "-c", "user.name=Restore Drill",
		"commit", "-q", "-m", "drill seed "+runID)
	remote := strings.TrimSuffix(base, "/") + "/" + org + "/" + repo + ".git"
	gitOut(t, work, "remote", "add", "origin", remote)
	gitOutAuth(t, work, token, "push", "-q", "origin", "main")
	sha := strings.TrimSpace(gitOut(t, work, "rev-parse", "HEAD"))
	if len(sha) != 40 {
		t.Fatalf("restore drill: rev-parse returned %q", sha)
	}
	return sha
}

func giteaDo(t *testing.T, method, endpoint, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatalf("restore drill: build request: %v", err)
	}
	req.Header.Set("Authorization", "token "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("restore drill: %s %s: %v", method, endpoint, err)
	}
	return resp
}

// giteaAdminDo performs one call as the dev admin (basic auth), the identity
// the environment documents for administration. Cleanup uses it because the
// run's own token is revoked by then.
func giteaAdminDo(t *testing.T, method, endpoint, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatalf("restore drill: build request: %v", err)
	}
	req.SetBasicAuth(adminUserOf(), adminPassOf())
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("restore drill: %s %s: %v", method, endpoint, err)
	}
	return resp
}
