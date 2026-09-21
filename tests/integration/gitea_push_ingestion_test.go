package integration

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/config"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/gitprovider"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

// Required test "git ingestion integration": the full T0305 chain against a
// LIVE Gitea instance and real PostgreSQL — a real `git push` to a semantic
// branch, its Gitea-shaped delivery replayed into the REAL receiver (the
// container→host delivery hop itself is environment-specific — the G3
// script covers it — so the delivery is built exactly as the provider would
// sign it and posted to the production handler), and the canonical rows the
// pipeline must produce: the ingestion record, the inspected changed files
// (diff-driven, not payload-driven), the candidate semantic diff, the
// branch's head pointer and the pushed-head project state. The acceptance
// criteria are exercised end to end: "branch push 被 ingest" and
// "重复 webhook 不重复 state" (a redelivered webhook is a complete no-op).
// CI has no Gitea, so the test skips loudly when the instance is
// unreachable; the real-instance coverage is also the Supervisor's G3 gate
// (gates.json task_overrides gitea-real-services).

const pushIngestionTaskID = "T0305"

// pushMatDoc is the pushed material manifest (the schema the canonical
// registry ships).
func pushMatDoc() string {
	return `{
  "id": "mat-0002",
  "type": "material",
  "version": 1,
  "project_id": "push-ingestion-gitea",
  "title": "Pushed Steel",
  "lifecycle_state": "active",
  "schema_ref": {"id": "` + schemareg.CanonicalNamespace + `material.schema.json", "version": "1"},
  "created_by": "u1",
  "created_at": "2026-09-14T00:00:00Z"
}`
}

// pushIngestionGiteaFixture reuses the T0303 fixture's live-adapters shape
// (embedded for its provider helpers) on its own task database, plus the
// receiver under test.
type pushIngestionGiteaFixture struct {
	*branchRefGiteaFixture
	reg *schemareg.Registry
}

func newPushIngestionGiteaFixture(t *testing.T, ctx context.Context) *pushIngestionGiteaFixture {
	t.Helper()
	base := requireGitea(t)
	token := giteaServiceToken(t, base)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), pushIngestionTaskID)

	user, err := persistence.NewCredentialStore(pool).CreateWithPassword(
		ctx, "push-ingestion-gitea@example.com", "hash", "push-ingestion-gitea", "Push Ingestion Gitea")
	if err != nil {
		t.Fatalf("gitea integration: seed user: %v", err)
	}
	project, _, err := persistence.NewProjectStore(pool).CreateProject(ctx, domain.Project{
		Slug:            "push-ingestion-gitea",
		Name:            "Push Ingestion Gitea",
		Purpose:         "T0305 integration",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, user.ID)
	if err != nil {
		t.Fatalf("gitea integration: create project: %v", err)
	}
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("gitea integration: schema registry: %v", err)
	}
	return &pushIngestionGiteaFixture{
		branchRefGiteaFixture: &branchRefGiteaFixture{
			pool:     pool,
			base:     base,
			token:    token,
			user:     user,
			project:  project,
			branches: branches.NewService(persistence.NewBranchStore(pool)),
			cfg: gitprovider.Config{
				BaseURL:    base,
				Token:      config.Secret(token),
				WebhookURL: "http://host.invalid/api/v1/git/hooks/gitea",
			},
		},
		reg: reg,
	}
}

// provision provisions the repository through the T0301 chain and returns
// its owner, name, provider repository id and the stored webhook secret
// (the production read path — the receiver will resolve the same row).
func (fx *pushIngestionGiteaFixture) provision(t *testing.T, ctx context.Context) (owner, name string, repoID int64, secret string) {
	t.Helper()
	provisioner := gitprovider.NewProvisioner(
		gitprovider.NewGiteaAdapter(fx.cfg),
		gitprovider.NewProvisionStore(fx.pool),
		fx.cfg.WebhookURL)
	if err := provisioner.Provision(ctx, fx.project.ID); err != nil {
		t.Fatalf("gitea integration: Provision: %v", err)
	}
	owner = fx.owner(t, ctx)
	name = gitprovider.RepositoryName(fx.project.ID)
	if err := fx.pool.QueryRow(ctx,
		`SELECT gitea_repo_id, webhook_secret FROM git_repository_provisions WHERE project_id = $1`,
		fx.project.ID).Scan(&repoID, &secret); err != nil {
		t.Fatalf("gitea integration: read provision row: %v", err)
	}
	t.Cleanup(func() { deleteGiteaRepo(t, fx.base, fx.token, owner, name) })
	return owner, name, repoID, secret
}

// forkState records the main bootstrap commit's ingested state — the
// T0305-shaped fact a real main ingestion would have written (fixture
// setup, not under test), which the branch forks and the first push's
// parent-state lookup resolves.
func (fx *pushIngestionGiteaFixture) forkState(t *testing.T, ctx context.Context, sha string) string {
	t.Helper()
	var id string
	if err := fx.pool.QueryRow(ctx, `INSERT INTO project_states
		(project_id, state_hash, manifest_version, git_commit_sha)
		VALUES ($1, $2, 'v1', $3) RETURNING id`,
		fx.project.ID, gitprovider.GitStateHash(sha), sha).Scan(&id); err != nil {
		t.Fatalf("gitea integration: insert fork state: %v", err)
	}
	return id
}

// handler builds the REAL receiver: the live Gitea adapter, the canonical
// store and the canonical registry — exactly what cmd/api wires.
func (fx *pushIngestionGiteaFixture) handler() http.Handler {
	store := gitprovider.NewPushIngestStore(fx.pool)
	ingester := gitprovider.NewPushIngester(gitprovider.NewGiteaAdapter(fx.cfg), store, fx.reg)
	return gitprovider.NewPushWebhookHandler(ingester, store)
}

// pushCommit advances the branch ref by one commit whose parent is the
// given base (the branch tip): add/update files, remove files, push with a
// real git client. Returns the new head sha.
func (fx *pushIngestionGiteaFixture) pushCommit(t *testing.T, owner, name, branch, base string, add map[string]string, remove []string, msg string) string {
	t.Helper()
	dir := t.TempDir()
	remoteURL := fx.base + "/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + ".git"
	env := append(os.Environ(), "HOME="+dir)
	env = append(env, gitAuthEnv("Authorization: token "+fx.token)...)
	run := func(args ...string) string {
		t.Helper()
		full := append([]string{"-C", dir}, args...)
		cmd := exec.Command("git", full...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gitea integration: git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-b", "work")
	run("fetch", "--depth=1", remoteURL, base)
	run("reset", "--hard", "FETCH_HEAD")
	for path, content := range add {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("gitea integration: mkdir for %s: %v", path, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("gitea integration: write %s: %v", path, err)
		}
	}
	for _, path := range remove {
		if err := os.Remove(filepath.Join(dir, path)); err != nil {
			t.Fatalf("gitea integration: remove %s: %v", path, err)
		}
	}
	run("config", "user.email", "gitea-integration@example.com")
	run("config", "user.name", "Gitea Integration")
	run("add", "-A")
	run("commit", "-m", msg)
	sha := run("rev-parse", "HEAD")
	run("push", remoteURL, "HEAD:refs/heads/"+branch)
	if sha == "" {
		t.Fatal("gitea integration: rev-parse HEAD came back empty")
	}
	return sha
}

// deliver posts one Gitea-shaped, correctly signed delivery into the real
// receiver and returns its status code. commitList is delivered verbatim
// as the payload's commit list (the provider truncates it — the pipeline
// must not consume it).
func (fx *pushIngestionGiteaFixture) deliver(t *testing.T, repoID int64, owner, name, ref, before, after string, totalCommits int, commitList []map[string]any, secret, deliveryID string) int {
	t.Helper()
	payload := map[string]any{
		"ref":         ref,
		"before":      before,
		"after":       after,
		"compare_url": fx.base + "/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/compare/" + before + "..." + after,
		"commits":     commitList,
		"repository": map[string]any{
			"id":        repoID,
			"name":      name,
			"full_name": owner + "/" + name,
			"owner":     map[string]any{"login": owner},
			"private":   true,
		},
		"pusher":        map[string]any{"login": "post-git-svc"},
		"sender":        map[string]any{"login": "post-git-svc"},
		"total_commits": totalCommits,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("gitea integration: marshal delivery: %v", err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/git/hooks/gitea", bytes.NewReader(body))
	req.Header.Set("X-Gitea-Event", "push")
	req.Header.Set("X-Gitea-Signature", hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-Gitea-Delivery", deliveryID)
	rec := httptest.NewRecorder()
	fx.handler().ServeHTTP(rec, req)
	return rec.Code
}

// ---- Canonical-row probes (the shapes the assertions consume).

type ingestionRow struct {
	deliveryID     string
	giteaRepo      int64
	projectID      string
	branchName     string
	gitRef         string
	beforeSHA      string
	afterSHA       string
	commits        int
	pusher         string
	headSkipReason string
}

func (fx *pushIngestionGiteaFixture) ingestionRows(t *testing.T, ctx context.Context) []ingestionRow {
	t.Helper()
	rows, err := fx.pool.Query(ctx, `SELECT COALESCE(delivery_id, ''), gitea_repo_id,
		COALESCE(project_id::text, ''), branch_name, git_ref, before_sha, after_sha,
		commit_count, COALESCE(pusher, ''), COALESCE(head_skip_reason, '')
		FROM git_push_ingestions ORDER BY created_at, id`)
	if err != nil {
		t.Fatalf("gitea integration: probe ingestions: %v", err)
	}
	defer rows.Close()
	var out []ingestionRow
	for rows.Next() {
		var r ingestionRow
		if err := rows.Scan(&r.deliveryID, &r.giteaRepo, &r.projectID, &r.branchName,
			&r.gitRef, &r.beforeSHA, &r.afterSHA, &r.commits, &r.pusher, &r.headSkipReason); err != nil {
			t.Fatalf("gitea integration: scan ingestion: %v", err)
		}
		out = append(out, r)
	}
	return out
}

type changeRow struct {
	path       string
	kind       string
	fileKind   string
	schemaID   string
	contentSHA string
}

func (fx *pushIngestionGiteaFixture) changeRows(t *testing.T, ctx context.Context) []changeRow {
	t.Helper()
	rows, err := fx.pool.Query(ctx, `SELECT path, change_kind, file_kind,
		COALESCE(schema_id, ''), COALESCE(content_sha256, '')
		FROM git_push_changes ORDER BY path`)
	if err != nil {
		t.Fatalf("gitea integration: probe changes: %v", err)
	}
	defer rows.Close()
	var out []changeRow
	for rows.Next() {
		var r changeRow
		if err := rows.Scan(&r.path, &r.kind, &r.fileKind, &r.schemaID, &r.contentSHA); err != nil {
			t.Fatalf("gitea integration: scan change: %v", err)
		}
		out = append(out, r)
	}
	return out
}

type candidateRow struct {
	path     string
	kind     string
	schemaID string
	status   string
	content  any
}

func (fx *pushIngestionGiteaFixture) candidateRows(t *testing.T, ctx context.Context) []candidateRow {
	t.Helper()
	rows, err := fx.pool.Query(ctx, `SELECT path, change_kind, schema_id, status, candidate
		FROM git_push_semantic_candidates ORDER BY path, created_at`)
	if err != nil {
		t.Fatalf("gitea integration: probe candidates: %v", err)
	}
	defer rows.Close()
	var out []candidateRow
	for rows.Next() {
		var r candidateRow
		if err := rows.Scan(&r.path, &r.kind, &r.schemaID, &r.status, &r.content); err != nil {
			t.Fatalf("gitea integration: scan candidate: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// stateBySHA returns the project state pinned to a git commit, with its
// parent state id.
func (fx *pushIngestionGiteaFixture) stateBySHA(t *testing.T, ctx context.Context, sha string) (id, parentID string, ok bool) {
	t.Helper()
	err := fx.pool.QueryRow(ctx, `SELECT id::text, COALESCE(parent_state_id::text, '')
		FROM project_states WHERE project_id = $1 AND git_commit_sha = $2
		ORDER BY created_at, id LIMIT 1`, fx.project.ID, sha).Scan(&id, &parentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false
	}
	if err != nil {
		t.Fatalf("gitea integration: probe state by sha: %v", err)
	}
	return id, parentID, true
}

// semanticFlag returns the branch's semantic completeness flag (T0306's
// projection — the value migration 00042's gates read).
func (fx *pushIngestionGiteaFixture) semanticFlag(t *testing.T, ctx context.Context, branchID string) string {
	t.Helper()
	var state string
	if err := fx.pool.QueryRow(ctx, `SELECT semantic_state
		FROM git_branch_semantic_states WHERE branch_id = $1`, branchID).Scan(&state); err != nil {
		t.Fatalf("gitea integration: probe semantic state: %v", err)
	}
	return state
}

// TestGiteaPushIngestionEndToEnd is the acceptance evidence: a real branch
// push is ingested (inspection over the git diff, manifest classified,
// candidate recorded, head pointer and state advanced), a redelivered
// webhook changes nothing, and unverifiable deliveries are refused.
func TestGiteaPushIngestionEndToEnd(t *testing.T) {
	ctx := testCtx(t)
	fx := newPushIngestionGiteaFixture(t, ctx)
	owner, name, repoID, secret := fx.provision(t, ctx)
	mainSHA := fx.seedMain(t, owner, name)

	// The semantic branch the pushes land on (created at the fork point,
	// exactly as T0303's fixture prepares it — the branch row is what the
	// ingestion maps the ref onto).
	forkStateID := fx.forkState(t, ctx, mainSHA)
	branch := fx.createBranch(t, ctx, "semantic", forkStateID)
	if err := fx.syncer().Sync(ctx, branch.ID); err != nil {
		t.Fatalf("gitea integration: Sync (create): %v", err)
	}
	if code, sha := fx.giteaBranch(t, owner, name, "semantic"); code != http.StatusOK || sha != mainSHA {
		t.Fatalf("gitea integration: semantic ref after create = code %d tip %q, want 200 at %s", code, sha, mainSHA)
	}

	matSchema := schemareg.CanonicalNamespace + "material.schema.json"
	sum := sha256.Sum256([]byte(pushMatDoc()))
	matContentSHA := hex.EncodeToString(sum[:])

	// ---- Push 1: a material manifest + a README. The delivery's commit
	// list is empty even though the push carries one commit (the provider
	// truncates; the pipeline must not consume it).
	shaA := fx.pushCommit(t, owner, name, "semantic", mainSHA, map[string]string{
		"manifests/mat.json": pushMatDoc(),
		"README.md":          "# semantic\n",
	}, nil, "add material manifest")
	if code := fx.deliver(t, repoID, owner, name, "refs/heads/semantic", mainSHA, shaA, 1, nil, secret, "d-1"); code != http.StatusNoContent {
		t.Fatalf("gitea integration: delivery 1 = %d, want 204", code)
	}

	ings := fx.ingestionRows(t, ctx)
	if len(ings) != 1 {
		t.Fatalf("gitea integration: ingestions = %d, want 1", len(ings))
	}
	if ings[0].deliveryID != "d-1" || ings[0].giteaRepo != repoID ||
		ings[0].projectID != fx.project.ID || ings[0].branchName != "semantic" ||
		ings[0].gitRef != "refs/heads/semantic" || ings[0].beforeSHA != mainSHA ||
		ings[0].afterSHA != shaA || ings[0].pusher != "post-git-svc" {
		t.Errorf("gitea integration: ingestion 1 = %+v", ings[0])
	}
	changes := fx.changeRows(t, ctx)
	if len(changes) != 2 {
		t.Fatalf("gitea integration: changes after push 1 = %+v", changes)
	}
	if changes[0].path != "manifests/mat.json" || changes[0].kind != "added" ||
		changes[0].fileKind != "semantic_manifest" || changes[0].schemaID != matSchema ||
		changes[0].contentSHA != matContentSHA {
		t.Errorf("gitea integration: manifest change = %+v", changes[0])
	}
	if changes[1].path != "README.md" || changes[1].kind != "modified" || changes[1].fileKind != "unstructured" {
		t.Errorf("gitea integration: README change = %+v", changes[1])
	}
	// T0306: the README is retained AND counted — the branch is marked
	// unstructured_changes by the same delivery.
	if got := fx.semanticFlag(t, ctx, branch.ID); got != string(gitprovider.BranchSemanticUnstructured) {
		t.Errorf("gitea integration: semantic flag after push 1 = %q, want unstructured_changes", got)
	}
	cands := fx.candidateRows(t, ctx)
	if len(cands) != 1 || cands[0].path != "manifests/mat.json" || cands[0].kind != "added" ||
		cands[0].schemaID != matSchema || cands[0].status != "candidate" {
		t.Fatalf("gitea integration: candidates after push 1 = %+v", cands)
	}
	var wantDoc any
	if err := json.Unmarshal([]byte(pushMatDoc()), &wantDoc); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cands[0].content, wantDoc) {
		t.Errorf("gitea integration: candidate content = %v, want the pushed document", cands[0].content)
	}
	if _, _, _, head, _ := fx.mappingRow(t, ctx, branch.ID); head != shaA {
		t.Errorf("gitea integration: head after push 1 = %q, want %s", head, shaA)
	}
	stateA, parentA, okA := fx.stateBySHA(t, ctx, shaA)
	if !okA || stateA == "" {
		t.Fatal("gitea integration: no project state for the pushed head")
	}
	if parentA != forkStateID {
		t.Errorf("gitea integration: state A parent = %q, want the fork state %s", parentA, forkStateID)
	}

	// ---- Push 2: 60 bulk files in one commit — the payload's commit list
	// is truncated to nothing, the git diff must still see every path.
	bulk := make(map[string]string, 60)
	for i := 0; i < 60; i++ {
		bulk[fmt.Sprintf("data/f-%02d.txt", i)] = fmt.Sprintf("bulk %d\n", i)
	}
	shaB := fx.pushCommit(t, owner, name, "semantic", shaA, bulk, nil, "bulk data")
	if code := fx.deliver(t, repoID, owner, name, "refs/heads/semantic", shaA, shaB, 1, nil, secret, "d-2"); code != http.StatusNoContent {
		t.Fatalf("gitea integration: delivery 2 = %d, want 204", code)
	}
	if got := len(fx.changeRows(t, ctx)); got != 62 {
		t.Errorf("gitea integration: changes after push 2 = %d, want 62 (diff-driven, not payload-driven)", got)
	}
	if _, _, _, head, _ := fx.mappingRow(t, ctx, branch.ID); head != shaB {
		t.Errorf("gitea integration: head after push 2 = %q, want %s", head, shaB)
	}
	stateB, parentB, okB := fx.stateBySHA(t, ctx, shaB)
	if !okB || parentB != stateA {
		t.Errorf("gitea integration: state B = %s parent %q, want chained to state A %s", stateB, parentB, stateA)
	}

	// ---- Push 3: the manifest is removed — the delete candidate is
	// classified on its PRE-push content (the base commit's version).
	shaC := fx.pushCommit(t, owner, name, "semantic", shaB, nil, []string{"manifests/mat.json"}, "drop material manifest")
	if code := fx.deliver(t, repoID, owner, name, "refs/heads/semantic", shaB, shaC, 1, nil, secret, "d-3"); code != http.StatusNoContent {
		t.Fatalf("gitea integration: delivery 3 = %d, want 204", code)
	}
	cands = fx.candidateRows(t, ctx)
	if len(cands) != 2 {
		t.Fatalf("gitea integration: candidates after push 3 = %+v", cands)
	}
	if cands[1].kind != "removed" || cands[1].schemaID != matSchema {
		t.Errorf("gitea integration: removal candidate = %+v", cands[1])
	}
	if _, _, _, head, _ := fx.mappingRow(t, ctx, branch.ID); head != shaC {
		t.Errorf("gitea integration: head after push 3 = %q, want %s", head, shaC)
	}
	if _, parentC, okC := fx.stateBySHA(t, ctx, shaC); !okC || parentC != stateB {
		t.Errorf("gitea integration: state C parent = %q, want chained to state B %s", parentC, stateB)
	}
	// Removing the manifest resolves only ITS path — README.md (and the
	// bulk files) are still unstructured, so the flag stays.
	if got := fx.semanticFlag(t, ctx, branch.ID); got != string(gitprovider.BranchSemanticUnstructured) {
		t.Errorf("gitea integration: semantic flag after push 3 = %q, want still unstructured_changes", got)
	}

	// ---- The acceptance criterion 重复 webhook 不重复 state: the SAME
	// delivery replayed with a fresh delivery id (the provider regenerates
	// it on redelivery) is a complete no-op — no new ingestion, change,
	// candidate or state, and the head pointer is NOT moved backward.
	countsBefore := len(fx.ingestionRows(t, ctx))
	if code := fx.deliver(t, repoID, owner, name, "refs/heads/semantic", shaA, shaB, 1, nil, secret, "d-2-replay"); code != http.StatusNoContent {
		t.Fatalf("gitea integration: redelivery = %d, want 204", code)
	}
	ings = fx.ingestionRows(t, ctx)
	if len(ings) != countsBefore {
		t.Errorf("gitea integration: redelivery added an ingestion: %d → %d", countsBefore, len(ings))
	}
	if len(fx.changeRows(t, ctx)) != 63 {
		t.Errorf("gitea integration: redelivery changed the change set: want 63")
	}
	if len(fx.candidateRows(t, ctx)) != 2 {
		t.Errorf("gitea integration: redelivery changed the candidates: want 2")
	}
	for _, r := range ings {
		if r.afterSHA == shaB && r.deliveryID != "d-2" {
			t.Errorf("gitea integration: redelivery rewrote the recorded delivery id: %q", r.deliveryID)
		}
	}
	if _, _, _, head, _ := fx.mappingRow(t, ctx, branch.ID); head != shaC {
		t.Errorf("gitea integration: redelivery moved the head backward: %q, want %s", head, shaC)
	}

	// ---- A delivery signed with a wrong secret is refused and ingests
	// nothing; a delivery for a repository the platform does not own is
	// 404 (retrying cannot fix it).
	countsBefore = len(fx.ingestionRows(t, ctx))
	if code := fx.deliver(t, repoID, owner, name, "refs/heads/semantic", shaB, shaC, 1, nil, "wrong-secret", "d-bad"); code != http.StatusUnauthorized {
		t.Errorf("gitea integration: bad-signature delivery = %d, want 401", code)
	}
	if code := fx.deliver(t, 999999, owner, name, "refs/heads/semantic", shaB, shaC, 1, nil, secret, "d-orphan"); code != http.StatusNotFound {
		t.Errorf("gitea integration: unprovisioned delivery = %d, want 404", code)
	}
	if got := len(fx.ingestionRows(t, ctx)); got != countsBefore {
		t.Errorf("gitea integration: refused deliveries ingested something: %d → %d", countsBefore, got)
	}

	// ---- Out-of-order delivery (the guarded head pointer): push P then Q,
	// deliver Q FIRST. The provider ref is already at Q, but the platform
	// head is still shaC — Q's before (shaP) no longer matches, so its
	// advance is REFUSED: the head stays shaC, the refusal is recorded on
	// Q's row (head_skip_reason = 'stale_before'), and Q's audit rows (the
	// change set and the state) are still recorded — they are facts of the
	// delivery. Delivering P afterwards advances the head to shaP, and a
	// replay of Q is a duplicate no-op: an older delivery can never rewind
	// the pointer past a newer head.
	shaP := fx.pushCommit(t, owner, name, "semantic", shaC, map[string]string{
		"data/p.txt": "p\n",
	}, nil, "add p")
	shaQ := fx.pushCommit(t, owner, name, "semantic", shaP, map[string]string{
		"data/q.txt": "q\n",
	}, nil, "add q")
	findIng := func(sha string) ingestionRow {
		t.Helper()
		for _, r := range fx.ingestionRows(t, ctx) {
			if r.afterSHA == sha {
				return r
			}
		}
		t.Fatalf("gitea integration: no ingestion row for %s", sha)
		return ingestionRow{}
	}
	if code := fx.deliver(t, repoID, owner, name, "refs/heads/semantic", shaP, shaQ, 1, nil, secret, "d-q-first"); code != http.StatusNoContent {
		t.Fatalf("gitea integration: out-of-order Q delivery = %d, want 204", code)
	}
	if got := findIng(shaQ).headSkipReason; got != "stale_before" {
		t.Errorf("gitea integration: Q's skip reason = %q, want stale_before", got)
	}
	if _, _, _, head, _ := fx.mappingRow(t, ctx, branch.ID); head != shaC {
		t.Errorf("gitea integration: out-of-order Q moved the head: %q, want still %s", head, shaC)
	}
	qChangeSeen := false
	for _, ch := range fx.changeRows(t, ctx) {
		if ch.path == "data/q.txt" && ch.kind == "added" && ch.fileKind == "unstructured" {
			qChangeSeen = true
		}
	}
	if !qChangeSeen {
		t.Error("gitea integration: Q's change set was not recorded (a refused advance must not drop the audit rows)")
	}
	if _, _, ok := fx.stateBySHA(t, ctx, shaQ); !ok {
		t.Error("gitea integration: Q's pushed head was not recorded as a state (only the pointer is guarded)")
	}
	if code := fx.deliver(t, repoID, owner, name, "refs/heads/semantic", shaC, shaP, 1, nil, secret, "d-p"); code != http.StatusNoContent {
		t.Fatalf("gitea integration: P delivery = %d, want 204", code)
	}
	if got := findIng(shaP).headSkipReason; got != "" {
		t.Errorf("gitea integration: P's skip reason = %q, want none (its before matches the head)", got)
	}
	if _, _, _, head, _ := fx.mappingRow(t, ctx, branch.ID); head != shaP {
		t.Errorf("gitea integration: head after P = %q, want %s", head, shaP)
	}
	countsBefore = len(fx.ingestionRows(t, ctx))
	if code := fx.deliver(t, repoID, owner, name, "refs/heads/semantic", shaP, shaQ, 1, nil, secret, "d-q-replay"); code != http.StatusNoContent {
		t.Fatalf("gitea integration: out-of-order Q replay = %d, want 204", code)
	}
	if got := len(fx.ingestionRows(t, ctx)); got != countsBefore {
		t.Errorf("gitea integration: Q replay added an ingestion: %d → %d", countsBefore, got)
	}
	if got := findIng(shaQ).headSkipReason; got != "stale_before" {
		t.Errorf("gitea integration: Q's skip reason after replay = %q, want stale_before", got)
	}
	if _, _, _, head, _ := fx.mappingRow(t, ctx, branch.ID); head != shaP {
		t.Errorf("gitea integration: Q replay moved the head: %q, want still %s", head, shaP)
	}

	// ---- T0306 negative at the boundary: the branch still carries
	// unstructured files (README.md, the bulk set, p.txt, q.txt), so its
	// flag is unstructured_changes and the database refuses the merge —
	// even the strongest possible path, a raw lifecycle UPDATE. Nothing
	// about the out-of-order choreography may have cleared it.
	if got := fx.semanticFlag(t, ctx, branch.ID); got != string(gitprovider.BranchSemanticUnstructured) {
		t.Errorf("gitea integration: semantic flag at the end = %q, want unstructured_changes", got)
	}
	_, err := fx.pool.Exec(ctx, `UPDATE branches SET lifecycle_state = 'merged' WHERE id = $1`, branch.ID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "P0001" {
		t.Errorf("gitea integration: merge of an unstructured branch = %v, want the semantic gate's P0001 refusal", err)
	}
}

// TestGiteaUnstructuredResolutionEndToEnd closes T0306's loop over the
// live boundary: files the platform cannot parse are RETAINED and the
// branch is marked unstructured_changes (its merge refused by the
// database); the agent's follow-up pushes replace one garbage file with a
// valid manifest at the SAME path and remove the other — only then is the
// branch semantic_complete and the merge goes through.
func TestGiteaUnstructuredResolutionEndToEnd(t *testing.T) {
	ctx := testCtx(t)
	fx := newPushIngestionGiteaFixture(t, ctx)
	owner, name, repoID, secret := fx.provision(t, ctx)
	mainSHA := fx.seedMain(t, owner, name)

	forkStateID := fx.forkState(t, ctx, mainSHA)
	branch := fx.createBranch(t, ctx, "semantic", forkStateID)
	if err := fx.syncer().Sync(ctx, branch.ID); err != nil {
		t.Fatalf("gitea integration: Sync (create): %v", err)
	}
	mergeBranch := func() error {
		t.Helper()
		_, err := fx.pool.Exec(ctx, `UPDATE branches SET lifecycle_state = 'merged' WHERE id = $1`, branch.ID)
		return err
	}
	isSemanticGate := func(err error) bool {
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) && pgErr.Code == "P0001"
	}

	// ---- Push 1: a junk .json (manifest-shaped but invalid) and a raw
	// CSV. BOTH are retained as change rows (任意文件 push 不丢失), the
	// branch is unstructured_changes, and its merge is refused.
	shaA := fx.pushCommit(t, owner, name, "semantic", mainSHA, map[string]string{
		"data/junk.json": `{"not":"a manifest"}`,
		"data/raw.csv":   "x,y\n1,2\n",
	}, nil, "agent drops raw artifacts")
	if code := fx.deliver(t, repoID, owner, name, "refs/heads/semantic", mainSHA, shaA, 1, nil, secret, "d-a"); code != http.StatusNoContent {
		t.Fatalf("gitea integration: delivery A = %d, want 204", code)
	}
	if got := len(fx.changeRows(t, ctx)); got != 2 {
		t.Errorf("gitea integration: changes after push 1 = %d, want 2 (both unparseable files retained)", got)
	}
	if got := len(fx.candidateRows(t, ctx)); got != 0 {
		t.Errorf("gitea integration: candidates after push 1 = %d, want 0 (nothing classified)", got)
	}
	if got := fx.semanticFlag(t, ctx, branch.ID); got != string(gitprovider.BranchSemanticUnstructured) {
		t.Fatalf("gitea integration: flag after push 1 = %q, want unstructured_changes", got)
	}
	if err := mergeBranch(); !isSemanticGate(err) {
		t.Errorf("gitea integration: merge after push 1 = %v, want the semantic gate's P0001 refusal", err)
	}

	// ---- Push 2: the agent replaces data/junk.json with a VALID material
	// manifest at the same path. That resolves only junk.json — the CSV is
	// still outstanding, so the flag stays unstructured_changes and the
	// merge is still refused.
	shaB := fx.pushCommit(t, owner, name, "semantic", shaA, map[string]string{
		"data/junk.json": pushMatDoc(),
	}, nil, "agent replaces junk with a manifest")
	if code := fx.deliver(t, repoID, owner, name, "refs/heads/semantic", shaA, shaB, 1, nil, secret, "d-b"); code != http.StatusNoContent {
		t.Fatalf("gitea integration: delivery B = %d, want 204", code)
	}
	if got := len(fx.changeRows(t, ctx)); got != 3 {
		t.Errorf("gitea integration: changes after push 2 = %d, want 3 (history retained)", got)
	}
	cands := fx.candidateRows(t, ctx)
	if len(cands) != 1 || cands[0].path != "data/junk.json" || cands[0].kind != "modified" {
		t.Errorf("gitea integration: candidates after push 2 = %+v, want the junk.json manifest replacement", cands)
	}
	if got := fx.semanticFlag(t, ctx, branch.ID); got != string(gitprovider.BranchSemanticUnstructured) {
		t.Errorf("gitea integration: flag after push 2 = %q, want still unstructured_changes (the CSV is still outstanding)", got)
	}
	if err := mergeBranch(); !isSemanticGate(err) {
		t.Errorf("gitea integration: merge after push 2 = %v, want the semantic gate still refusing", err)
	}

	// ---- Push 3: the agent removes the CSV. The removal resolves the last
	// outstanding path — the flag clears and the merge completes.
	shaC := fx.pushCommit(t, owner, name, "semantic", shaB, nil, []string{"data/raw.csv"}, "agent removes the raw csv")
	if code := fx.deliver(t, repoID, owner, name, "refs/heads/semantic", shaB, shaC, 1, nil, secret, "d-c"); code != http.StatusNoContent {
		t.Fatalf("gitea integration: delivery C = %d, want 204", code)
	}
	if got := len(fx.changeRows(t, ctx)); got != 4 {
		t.Errorf("gitea integration: changes after push 3 = %d, want 4 (history retained)", got)
	}
	if got := fx.semanticFlag(t, ctx, branch.ID); got != string(gitprovider.BranchSemanticComplete) {
		t.Errorf("gitea integration: flag after push 3 = %q, want semantic_complete (the removal resolved the last path)", got)
	}
	if err := mergeBranch(); err != nil {
		t.Errorf("gitea integration: merge after push 3 = %v, want success", err)
	}
}

// TestPushIngestionStaleDeliveryDoesNotRewindHead is the regression test
// for the guarded head pointer, at the store boundary: ingest push B, then
// ingest an OLDER push A, and the head must still be B's commit. The stale
// delivery's audit rows (ingestion, changes, state) are recorded — they are
// facts of the delivery — but its pointer advance is refused and marked
// head_skip_reason = 'stale_before', and a replay of A is a complete
// duplicate no-op. Pure database: the pointer guard is a store fact, no
// provider needed.
func TestPushIngestionStaleDeliveryDoesNotRewindHead(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), pushIngestionTaskID)

	user, err := persistence.NewCredentialStore(pool).CreateWithPassword(
		ctx, "stale-head@example.com", "hash", "stale-head", "Stale Head")
	if err != nil {
		t.Fatalf("gitea integration: seed user: %v", err)
	}
	project, _, err := persistence.NewProjectStore(pool).CreateProject(ctx, domain.Project{
		Slug:            "stale-head",
		Name:            "Stale Head",
		Purpose:         "T0305 store regression",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, user.ID)
	if err != nil {
		t.Fatalf("gitea integration: create project: %v", err)
	}
	// The provision row (T0301's stored fact — the store resolves the
	// project through it).
	if _, err := pool.Exec(ctx, `INSERT INTO git_repository_provisions
		(project_id, owner, name, gitea_repo_id, webhook_id, webhook_secret)
		VALUES ($1, 'post-git-svc', 'stale-head-repo', 42, 1, 'secret')`,
		project.ID); err != nil {
		t.Fatalf("gitea integration: insert provision row: %v", err)
	}
	// The branch at its fork state.
	forkSHA := strings.Repeat("2", 40)
	var forkStateID string
	if err := pool.QueryRow(ctx, `INSERT INTO project_states
		(project_id, state_hash, manifest_version, git_commit_sha)
		VALUES ($1, $2, 'v1', $3) RETURNING id`,
		project.ID, gitprovider.GitStateHash(forkSHA), forkSHA).Scan(&forkStateID); err != nil {
		t.Fatalf("gitea integration: insert fork state: %v", err)
	}
	branch, err := branches.NewService(persistence.NewBranchStore(pool)).Create(ctx, branches.CreateBranchParams{
		ProjectID:   project.ID,
		Name:        "semantic",
		Visibility:  domain.BranchVisibilityPrivate,
		BaseStateID: forkStateID,
		CreatedBy:   user.ID,
	})
	if err != nil {
		t.Fatalf("gitea integration: create branch: %v", err)
	}
	// The syncer's arrival at shaA: the older push A reached the provider,
	// its own delivery 503'd before recording anything, and a later sync
	// set the pointer to the true tip without an ingestion row — the exact
	// shape that makes a stale redelivery possible.
	shaA := strings.Repeat("a", 40)
	if _, err := pool.Exec(ctx, `UPDATE git_branch_refs
		SET sync_state = 'synced', fork_sha = $1, head_sha = $1, synced_at = now()
		WHERE branch_id = $2`, shaA, branch.ID); err != nil {
		t.Fatalf("gitea integration: sync the refs row: %v", err)
	}

	shaB := strings.Repeat("b", 40)
	store := gitprovider.NewPushIngestStore(pool)
	event := func(before, after, delivery string) gitprovider.PushEvent {
		return gitprovider.PushEvent{
			Ref:          "refs/heads/semantic",
			Before:       before,
			After:        after,
			RepositoryID: 42,
			Owner:        "post-git-svc",
			Name:         "stale-head-repo",
			Pusher:       "post-git-svc",
			TotalCommits: 1,
			DeliveryID:   delivery,
		}
	}
	ingest := func(ev gitprovider.PushEvent, changes []gitprovider.ClassifiedChange) bool {
		t.Helper()
		inserted, err := store.IngestPush(ctx, gitprovider.IngestPushParams{Event: ev, Changes: changes})
		if err != nil {
			t.Fatalf("gitea integration: IngestPush(%s): %v", ev.DeliveryID, err)
		}
		return inserted
	}
	headOf := func() string {
		t.Helper()
		var head string
		if err := pool.QueryRow(ctx, `SELECT COALESCE(head_sha, '')
			FROM git_branch_refs WHERE branch_id = $1`, branch.ID).Scan(&head); err != nil {
			t.Fatalf("gitea integration: probe head: %v", err)
		}
		return head
	}
	ingRow := func(sha string) (skipReason string, changeCount int) {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, `SELECT id::text, COALESCE(head_skip_reason, '')
			FROM git_push_ingestions
			WHERE gitea_repo_id = 42 AND git_ref = 'refs/heads/semantic' AND after_sha = $1`,
			sha).Scan(&id, &skipReason); err != nil {
			t.Fatalf("gitea integration: probe ingestion %s: %v", sha, err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM git_push_changes
			WHERE ingestion_id = $1`, id).Scan(&changeCount); err != nil {
			t.Fatalf("gitea integration: probe changes of %s: %v", sha, err)
		}
		return skipReason, changeCount
	}
	stateExists := func(sha string) bool {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM project_states
			WHERE project_id = $1 AND git_commit_sha = $2`, project.ID, sha).Scan(&n); err != nil {
			t.Fatalf("gitea integration: probe state %s: %v", sha, err)
		}
		return n == 1
	}

	// ---- Ingest B (the NEWER push): the head advances shaA → shaB.
	if !ingest(event(shaA, shaB, "d-b"), []gitprovider.ClassifiedChange{
		{Path: "b.txt", Kind: gitprovider.ChangeAdded, File: gitprovider.FileKindUnstructured},
	}) {
		t.Fatal("gitea integration: B not inserted")
	}
	if head := headOf(); head != shaB {
		t.Fatalf("gitea integration: head after B = %q, want %s", head, shaB)
	}

	// ---- Ingest A (the OLDER push, its first delivery arriving late):
	// before (forkSHA) no longer matches the head (shaB) — the advance is
	// REFUSED, but the delivery is still recorded in full.
	if !ingest(event(forkSHA, shaA, "d-a"), []gitprovider.ClassifiedChange{
		{Path: "a.txt", Kind: gitprovider.ChangeAdded, File: gitprovider.FileKindUnstructured},
	}) {
		t.Fatal("gitea integration: A not inserted (its audit rows must still be recorded)")
	}
	if head := headOf(); head != shaB {
		t.Errorf("gitea integration: stale A rewound the head: %q, want still %s (B's commit)", head, shaB)
	}
	if reason, changes := ingRow(shaA); reason != "stale_before" || changes != 1 {
		t.Errorf("gitea integration: A's row = reason %q with %d changes, want stale_before with its change recorded", reason, changes)
	}
	if reason, _ := ingRow(shaB); reason != "" {
		t.Errorf("gitea integration: B's skip reason = %q, want none (its before matched the head)", reason)
	}
	if !stateExists(shaB) || !stateExists(shaA) {
		t.Error("gitea integration: the pushed heads of B and A must both exist as states (only the pointer is guarded)")
	}

	// ---- Replay A: an exact duplicate — a complete no-op (counts and
	// head untouched, the refusal still the one recorded fact).
	if ingest(event(forkSHA, shaA, "d-a-replay"), nil) {
		t.Fatal("gitea integration: A replay reported inserted, want duplicate")
	}
	if reason, changes := ingRow(shaA); reason != "stale_before" || changes != 1 {
		t.Errorf("gitea integration: A's row after replay = reason %q with %d changes, want unchanged stale_before with 1 change", reason, changes)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM git_push_ingestions
		WHERE gitea_repo_id = 42`).Scan(&n); err != nil {
		t.Fatalf("gitea integration: probe ingestion count: %v", err)
	}
	if n != 2 {
		t.Errorf("gitea integration: ingestion count = %d, want 2 (replay added nothing)", n)
	}
	if head := headOf(); head != shaB {
		t.Errorf("gitea integration: A replay moved the head: %q, want still %s", head, shaB)
	}
}

// TestPushIngestionReusesAStateOnASecondBranch pins the push path's
// documented REUSE semantics — the same commit on another branch of the
// project reuses the state row (see the contract on
// gitprovider.PGPushIngestStore.IngestPush) — which the fork import's own
// stricter rule must not touch. The shape is the one `git push origin
// main:<branch>` produces: a commit that is ALREADY a state of the project
// arrives as the head of a delivery to a SECOND branch. Both variants are
// exercised:
//
//   - a creation delivery (before = zeros, the new ref's first push), and
//   - a fast-forward delivery (before = the branch's previous head) whose
//     head commit is likewise already a state of the first branch.
//
// In both, the delivery must SUCCEED in full (ingestion row recorded, no
// head_skip_reason, head pointer advanced) and the state row must be REUSED:
// exactly one row for the commit, the id the first branch recorded, still
// owned by that branch — a second row, an error, or a lost advance all fail
// the test. This is the regression pin for the owner check the fork import
// needs: applied to every delivery, it would refuse this push.
//
// Pure database: the reuse is a store fact, no provider needed.
func TestPushIngestionReusesAStateOnASecondBranch(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), pushIngestionTaskID)

	user, err := persistence.NewCredentialStore(pool).CreateWithPassword(
		ctx, "shared-state@example.com", "hash", "shared-state", "Shared State")
	if err != nil {
		t.Fatalf("gitea integration: seed user: %v", err)
	}
	project, _, err := persistence.NewProjectStore(pool).CreateProject(ctx, domain.Project{
		Slug:            "shared-state",
		Name:            "Shared State",
		Purpose:         "T0305 reuse regression",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, user.ID)
	if err != nil {
		t.Fatalf("gitea integration: create project: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO git_repository_provisions
		(project_id, owner, name, gitea_repo_id, webhook_id, webhook_secret)
		VALUES ($1, 'post-git-svc', 'shared-state-repo', 63, 1, 'secret')`,
		project.ID); err != nil {
		t.Fatalf("gitea integration: insert provision row: %v", err)
	}
	forkSHA := strings.Repeat("3", 40)
	var forkStateID string
	if err := pool.QueryRow(ctx, `INSERT INTO project_states
		(project_id, state_hash, manifest_version, git_commit_sha)
		VALUES ($1, $2, 'v1', $3) RETURNING id`,
		project.ID, gitprovider.GitStateHash(forkSHA), forkSHA).Scan(&forkStateID); err != nil {
		t.Fatalf("gitea integration: insert base state: %v", err)
	}
	branchSvc := branches.NewService(persistence.NewBranchStore(pool))
	first, err := branchSvc.Create(ctx, branches.CreateBranchParams{
		ProjectID:   project.ID,
		Name:        "semantic",
		Visibility:  domain.BranchVisibilityPrivate,
		BaseStateID: forkStateID,
		CreatedBy:   user.ID,
	})
	if err != nil {
		t.Fatalf("gitea integration: create the first branch: %v", err)
	}
	second, err := branchSvc.Create(ctx, branches.CreateBranchParams{
		ProjectID:   project.ID,
		Name:        "release",
		Visibility:  domain.BranchVisibilityPrivate,
		BaseStateID: forkStateID,
		CreatedBy:   user.ID,
	})
	if err != nil {
		t.Fatalf("gitea integration: create the second branch: %v", err)
	}
	// The second branch's ref is unborn: a creation delivery is the shape its
	// first push takes (`git push origin main:release`), and the pointer
	// advances from NULL.
	var secondHead *string
	if err := pool.QueryRow(ctx, `SELECT head_sha FROM git_branch_refs WHERE branch_id = $1`,
		second.ID).Scan(&secondHead); err != nil {
		t.Fatalf("gitea integration: read the second branch's refs row: %v", err)
	}
	if secondHead != nil {
		t.Fatalf("gitea integration: the second branch's ref starts at %q, want unborn (head_sha NULL)", *secondHead)
	}
	// The first branch starts where the fork did (the syncer's arrival), so
	// its pushes fast-forward from there.
	shaA := strings.Repeat("d", 40)
	shaB := strings.Repeat("e", 40)
	if _, err := pool.Exec(ctx, `UPDATE git_branch_refs
		SET sync_state = 'synced', fork_sha = $1, head_sha = $1, synced_at = now()
		WHERE branch_id = $2`, forkSHA, first.ID); err != nil {
		t.Fatalf("gitea integration: sync the first branch's refs row: %v", err)
	}

	store := gitprovider.NewPushIngestStore(pool)
	event := func(ref, before, after, delivery string) gitprovider.PushEvent {
		return gitprovider.PushEvent{
			Ref:          ref,
			Before:       before,
			After:        after,
			RepositoryID: 63,
			Owner:        "post-git-svc",
			Name:         "shared-state-repo",
			Pusher:       "post-git-svc",
			TotalCommits: 1,
			DeliveryID:   delivery,
		}
	}
	ingest := func(ev gitprovider.PushEvent, changes []gitprovider.ClassifiedChange) bool {
		t.Helper()
		inserted, err := store.IngestPush(ctx, gitprovider.IngestPushParams{Event: ev, Changes: changes})
		if err != nil {
			t.Fatalf("gitea integration: IngestPush(%s on %s): %v", ev.DeliveryID, ev.Ref, err)
		}
		return inserted
	}
	headOf := func(branchID string) string {
		t.Helper()
		var head string
		if err := pool.QueryRow(ctx, `SELECT COALESCE(head_sha, '')
			FROM git_branch_refs WHERE branch_id = $1`, branchID).Scan(&head); err != nil {
			t.Fatalf("gitea integration: probe head of %s: %v", branchID, err)
		}
		return head
	}
	// stateRowOf reads the ONE state row of a commit: its id, the branch that
	// owns it, and how many rows the commit has (a reuse writes none).
	stateRowOf := func(sha string) (id, owner string, count int) {
		t.Helper()
		var ownerID *string
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM project_states
			WHERE project_id = $1 AND git_commit_sha = $2`, project.ID, sha).Scan(&count); err != nil {
			t.Fatalf("gitea integration: count the states of %s: %v", sha, err)
		}
		if count != 1 {
			return "", "", count
		}
		if err := pool.QueryRow(ctx, `SELECT id::text, branch_id::text FROM project_states
			WHERE project_id = $1 AND git_commit_sha = $2`, project.ID, sha).Scan(&id, &ownerID); err != nil {
			t.Fatalf("gitea integration: read the state of %s: %v", sha, err)
		}
		if ownerID != nil {
			owner = *ownerID
		}
		return id, owner, count
	}
	skipReasonOf := func(ref, sha string) *string {
		t.Helper()
		var reason *string
		if err := pool.QueryRow(ctx, `SELECT head_skip_reason FROM git_push_ingestions
			WHERE gitea_repo_id = 63 AND git_ref = $1 AND after_sha = $2`, ref, sha).Scan(&reason); err != nil {
			t.Fatalf("gitea integration: read the ingestion row of %s on %s: %v", sha, ref, err)
		}
		return reason
	}

	// ---- The first branch records both commits as states of its own chain.
	for _, push := range []struct{ before, after, delivery string }{
		{forkSHA, shaA, "d-first-a"},
		{shaA, shaB, "d-first-b"},
	} {
		if !ingest(event("refs/heads/semantic", push.before, push.after, push.delivery),
			[]gitprovider.ClassifiedChange{
				{Path: push.after[:4] + ".txt", Kind: gitprovider.ChangeAdded, File: gitprovider.FileKindUnstructured},
			}) {
			t.Fatalf("gitea integration: %s was not inserted", push.delivery)
		}
	}
	stateAID, stateAOwner, nA := stateRowOf(shaA)
	stateBID, _, nB := stateRowOf(shaB)
	if nA != 1 || nB != 1 {
		t.Fatalf("gitea integration: the fixture's commits hold %d/%d state rows, want 1 each", nA, nB)
	}
	if stateAOwner != first.ID {
		t.Fatalf("gitea integration: the fixture's state A belongs to branch %s, want the first branch %s", stateAOwner, first.ID)
	}

	// ---- (a) `git push origin main:release`: a CREATION delivery whose head
	// is already a state of the project (the first branch recorded it). The
	// delivery must succeed in full and REUSE the row.
	if !ingest(event("refs/heads/release", strings.Repeat("0", 40), shaA, "d-second-create"),
		[]gitprovider.ClassifiedChange{
			{Path: "copied.txt", Kind: gitprovider.ChangeAdded, File: gitprovider.FileKindUnstructured},
		}) {
		t.Fatal("gitea integration: the second branch's creation delivery was not inserted")
	}
	if reason := skipReasonOf("refs/heads/release", shaA); reason != nil {
		t.Errorf("gitea integration: the creation delivery's head advance was refused (%q), want it advanced", *reason)
	}
	if head := headOf(second.ID); head != shaA {
		t.Errorf("gitea integration: the second branch's head = %q, want the pushed %s", head, shaA)
	}
	reusedID, reusedOwner, reusedCount := stateRowOf(shaA)
	if reusedCount != 1 {
		t.Fatalf("gitea integration: the commit now holds %d state rows, want the existing one REUSED", reusedCount)
	}
	if reusedID != stateAID {
		t.Errorf("gitea integration: the reused state's id = %s, want the first branch's row %s", reusedID, stateAID)
	}
	if reusedOwner != first.ID {
		t.Errorf("gitea integration: the reused state now belongs to branch %s, want it left with %s", reusedOwner, first.ID)
	}
	var parents int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM project_states
		WHERE project_id = $1 AND parent_state_id = $2`, project.ID, stateAID).Scan(&parents); err != nil {
		t.Fatalf("gitea integration: count the children of state A: %v", err)
	}
	if parents != 1 {
		t.Errorf("gitea integration: the reused state has %d children, want the one the first branch's chain made", parents)
	}

	// ---- (b) The fast-forward variant: the second branch advances to a
	// commit the first branch already recorded.
	if !ingest(event("refs/heads/release", shaA, shaB, "d-second-ff"),
		[]gitprovider.ClassifiedChange{
			{Path: "forward.txt", Kind: gitprovider.ChangeAdded, File: gitprovider.FileKindUnstructured},
		}) {
		t.Fatal("gitea integration: the second branch's fast-forward delivery was not inserted")
	}
	if reason := skipReasonOf("refs/heads/release", shaB); reason != nil {
		t.Errorf("gitea integration: the fast-forward's head advance was refused (%q), want it advanced", *reason)
	}
	if head := headOf(second.ID); head != shaB {
		t.Errorf("gitea integration: the second branch's head = %q, want the pushed %s", head, shaB)
	}
	ffID, ffOwner, ffCount := stateRowOf(shaB)
	if ffCount != 1 || ffID != stateBID || ffOwner != first.ID {
		t.Errorf("gitea integration: the fast-forward's state = %s owned by %s in %d row(s), want the first branch's %s owned by %s in 1",
			ffID, ffOwner, ffCount, stateBID, first.ID)
	}

	// ---- Both deliveries are recorded facts of their own (an ingestion row
	// per delivery, the second branch's two rows included).
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM git_push_ingestions
		WHERE gitea_repo_id = 63 AND git_ref = 'refs/heads/release'`).Scan(&rows); err != nil {
		t.Fatalf("gitea integration: count the second branch's ingestions: %v", err)
	}
	if rows != 2 {
		t.Errorf("gitea integration: the second branch holds %d ingestion row(s), want 2", rows)
	}
}

// TestForkImportRefusesAStateOwnedByAnotherBranch pins the rule the fork
// import needs and the generic push path must NOT have: an import LANDS its
// branch on the copied content (it moves the branch's head to the state and
// writes the commit naming it), so it may only use a state row of its own
// branch. A copied commit that is already a state of ANOTHER branch of the
// project is refused — the delivery rolls back whole (no ingestion row, no
// change rows, no head move, no commit), because a transition that chained
// this branch to a state of a foreign chain would describe a history that
// never happened.
//
// The contrast with TestPushIngestionReusesAStateOnASecondBranch is the
// point of both tests: the same commit arriving as an ordinary push is
// REUSED (that is the push path's documented contract), and it is only the
// import — the caller that must own the row it lands on — that refuses it.
//
// Pure database: the rule is a store fact, no provider needed.
func TestForkImportRefusesAStateOwnedByAnotherBranch(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), pushIngestionTaskID)

	user, err := persistence.NewCredentialStore(pool).CreateWithPassword(
		ctx, "import-owner@example.com", "hash", "import-owner", "Import Owner")
	if err != nil {
		t.Fatalf("gitea integration: seed user: %v", err)
	}
	project, _, err := persistence.NewProjectStore(pool).CreateProject(ctx, domain.Project{
		Slug:            "import-owner",
		Name:            "Import Owner",
		Purpose:         "T0817 fork import regression",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, user.ID)
	if err != nil {
		t.Fatalf("gitea integration: create project: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO git_repository_provisions
		(project_id, owner, name, gitea_repo_id, webhook_id, webhook_secret)
		VALUES ($1, 'post-git-svc', 'import-owner-repo', 71, 1, 'secret')`,
		project.ID); err != nil {
		t.Fatalf("gitea integration: insert provision row: %v", err)
	}
	forkSHA := strings.Repeat("4", 40)
	var forkStateID string
	if err := pool.QueryRow(ctx, `INSERT INTO project_states
		(project_id, state_hash, manifest_version, git_commit_sha)
		VALUES ($1, $2, 'v1', $3) RETURNING id`,
		project.ID, gitprovider.GitStateHash(forkSHA), forkSHA).Scan(&forkStateID); err != nil {
		t.Fatalf("gitea integration: insert base state: %v", err)
	}
	branchSvc := branches.NewService(persistence.NewBranchStore(pool))
	upstream, err := branchSvc.Create(ctx, branches.CreateBranchParams{
		ProjectID:   project.ID,
		Name:        "upstream",
		Visibility:  domain.BranchVisibilityPrivate,
		BaseStateID: forkStateID,
		CreatedBy:   user.ID,
	})
	if err != nil {
		t.Fatalf("gitea integration: create the first branch: %v", err)
	}
	copied, err := branchSvc.Create(ctx, branches.CreateBranchParams{
		ProjectID:   project.ID,
		Name:        "copied",
		Visibility:  domain.BranchVisibilityPrivate,
		BaseStateID: forkStateID,
		CreatedBy:   user.ID,
	})
	if err != nil {
		t.Fatalf("gitea integration: create the second branch: %v", err)
	}
	shaX := strings.Repeat("f", 40)
	if _, err := pool.Exec(ctx, `UPDATE git_branch_refs
		SET sync_state = 'synced', fork_sha = $1, head_sha = $1, synced_at = now()
		WHERE branch_id = $2`, forkSHA, upstream.ID); err != nil {
		t.Fatalf("gitea integration: sync the first branch's refs row: %v", err)
	}

	store := gitprovider.NewPushIngestStore(pool)
	// The first branch records the commit as a state of its chain.
	if _, err := store.IngestPush(ctx, gitprovider.IngestPushParams{
		Event: gitprovider.PushEvent{
			Ref: "refs/heads/upstream", Before: forkSHA, After: shaX,
			RepositoryID: 71, Owner: "post-git-svc", Name: "import-owner-repo",
			Pusher: "post-git-svc", TotalCommits: 1, DeliveryID: "d-upstream-x",
		},
		Changes: []gitprovider.ClassifiedChange{
			{Path: "x.txt", Kind: gitprovider.ChangeAdded, File: gitprovider.FileKindUnstructured},
		},
	}); err != nil {
		t.Fatalf("gitea integration: the first branch's push: %v", err)
	}
	var ownerID, ownerBranch string
	if err := pool.QueryRow(ctx, `SELECT id::text, branch_id::text FROM project_states
		WHERE project_id = $1 AND git_commit_sha = $2`, project.ID, shaX).Scan(&ownerID, &ownerBranch); err != nil {
		t.Fatalf("gitea integration: read the recorded state: %v", err)
	}
	if ownerBranch != upstream.ID {
		t.Fatalf("gitea integration: the fixture's state belongs to branch %s, want %s", ownerBranch, upstream.ID)
	}

	// The IMPORT of the same commit onto the second branch. It must be
	// refused, and it must leave nothing behind.
	_, err = store.IngestPush(ctx, gitprovider.IngestPushParams{
		Event: gitprovider.PushEvent{
			Ref: "refs/heads/copied", Before: gitprovider.ZerosSHA, After: shaX,
			RepositoryID: 71, Owner: "post-git-svc", Name: "import-owner-repo",
			Pusher: "post-git-svc", TotalCommits: 1, DeliveryID: "d-import-x",
		},
		Changes: []gitprovider.ClassifiedChange{
			{Path: "x.txt", Kind: gitprovider.ChangeAdded, File: gitprovider.FileKindUnstructured},
		},
		ForkImport: &gitprovider.ForkImportTransition{
			ActorID: user.ID,
			Message: "import the parent line into the fork",
		},
	})
	if err == nil {
		t.Fatal("the fork import landed a state row owned by another branch")
	}
	if !strings.Contains(err.Error(), "already the state of another branch") {
		t.Errorf("the import's refusal = %v, want the ownership rule's own error", err)
	}

	// Nothing of the refused delivery was written: the transaction rolled
	// back whole.
	var ingestions, changes, commits, owned int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM git_push_ingestions
		WHERE gitea_repo_id = 71 AND delivery_id = 'd-import-x'`).Scan(&ingestions); err != nil {
		t.Fatalf("gitea integration: count the refused delivery's ingestions: %v", err)
	}
	if ingestions != 0 {
		t.Errorf("the refused import left %d ingestion row(s)", ingestions)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM git_push_changes c
		JOIN git_push_ingestions i ON i.id = c.ingestion_id
		WHERE i.gitea_repo_id = 71`).Scan(&changes); err != nil {
		t.Fatalf("gitea integration: count the change rows: %v", err)
	}
	if changes != 1 {
		t.Errorf("the project holds %d change row(s), want only the first push's one", changes)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM state_commits
		WHERE branch_id = $1`, copied.ID).Scan(&commits); err != nil {
		t.Fatalf("gitea integration: count the second branch's commits: %v", err)
	}
	if commits != 0 {
		t.Errorf("the refused import wrote %d state commit(s) on the second branch", commits)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM project_states
		WHERE branch_id = $1`, copied.ID).Scan(&owned); err != nil {
		t.Fatalf("gitea integration: count the second branch's states: %v", err)
	}
	if owned != 0 {
		t.Errorf("the refused import left %d state row(s) on the second branch, want none", owned)
	}
	var head *string
	if err := pool.QueryRow(ctx, `SELECT head_sha FROM git_branch_refs WHERE branch_id = $1`,
		copied.ID).Scan(&head); err != nil {
		t.Fatalf("gitea integration: read the second branch's head: %v", err)
	}
	if head != nil {
		t.Errorf("the refused import moved the second branch's head to %q, want it unborn", *head)
	}
	// The refused delivery changed nothing on the branch that owns the
	// state either: its head is still the pushed commit and its row is
	// still the one the fixture recorded (an ordinary push writes no state
	// commit — that is the import's rule, not the push path's).
	var upstreamHead string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(head_sha, '') FROM git_branch_refs
		WHERE branch_id = $1`, upstream.ID).Scan(&upstreamHead); err != nil {
		t.Fatalf("gitea integration: read the first branch's head: %v", err)
	}
	if upstreamHead != shaX {
		t.Errorf("the first branch's head = %q, want its own pushed %s", upstreamHead, shaX)
	}
	var stillOwned int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM project_states
		WHERE id = $1 AND branch_id = $2`, ownerID, upstream.ID).Scan(&stillOwned); err != nil {
		t.Fatalf("gitea integration: re-read the owner of the state: %v", err)
	}
	if stillOwned != 1 {
		t.Error("the refused import took the state row away from the branch that recorded it")
	}
}

// TestPushIngestionStaleCreationPushCannotRewindHead is the regression test
// for the guarded CREATION path, at the store boundary. The exact 5-step
// shape the guard exists for:
//
//  1. the branch is created but its first ref-sync fails — head_sha stays
//     NULL;
//  2. the branch-creation push A (before = zeros) 503s its first delivery
//     — nothing is recorded, there is no A row;
//  3. the newer push B is ingested — its before (shaA) matches nothing
//     (the head is NULL), so B's advance is refused as 'stale_before' and
//     the head stays NULL;
//  4. the syncer's sweep retries the ref and sets head_sha = shaB;
//  5. A's redelivery arrives: it is NOT a duplicate (no A row), but the
//     creation path is guarded — the ref already carries shaB, so A's
//     advance is refused as 'stale_creation' and the head stays shaB.
//
// A replay of either delivery afterwards is a duplicate no-op: the older
// push can never rewind the pointer past the newer head. Pure database:
// the pointer guard is a store fact, no provider needed.
func TestPushIngestionStaleCreationPushCannotRewindHead(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), pushIngestionTaskID)

	user, err := persistence.NewCredentialStore(pool).CreateWithPassword(
		ctx, "stale-creation@example.com", "hash", "stale-creation", "Stale Creation")
	if err != nil {
		t.Fatalf("gitea integration: seed user: %v", err)
	}
	project, _, err := persistence.NewProjectStore(pool).CreateProject(ctx, domain.Project{
		Slug:            "stale-creation",
		Name:            "Stale Creation",
		Purpose:         "T0305 store regression",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, user.ID)
	if err != nil {
		t.Fatalf("gitea integration: create project: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO git_repository_provisions
		(project_id, owner, name, gitea_repo_id, webhook_id, webhook_secret)
		VALUES ($1, 'post-git-svc', 'stale-creation-repo', 43, 1, 'secret')`,
		project.ID); err != nil {
		t.Fatalf("gitea integration: insert provision row: %v", err)
	}
	forkSHA := strings.Repeat("3", 40)
	var forkStateID string
	if err := pool.QueryRow(ctx, `INSERT INTO project_states
		(project_id, state_hash, manifest_version, git_commit_sha)
		VALUES ($1, $2, 'v1', $3) RETURNING id`,
		project.ID, gitprovider.GitStateHash(forkSHA), forkSHA).Scan(&forkStateID); err != nil {
		t.Fatalf("gitea integration: insert fork state: %v", err)
	}
	branch, err := branches.NewService(persistence.NewBranchStore(pool)).Create(ctx, branches.CreateBranchParams{
		ProjectID:   project.ID,
		Name:        "semantic",
		Visibility:  domain.BranchVisibilityPrivate,
		BaseStateID: forkStateID,
		CreatedBy:   user.ID,
	})
	if err != nil {
		t.Fatalf("gitea integration: create branch: %v", err)
	}

	// Step 1: the branch-creation sync fails (the mapping row is born
	// 'pending' with head_sha NULL; the failed attempt moves it to
	// 'failed' without ever recording a head).
	if _, err := pool.Exec(ctx, `UPDATE git_branch_refs
		SET sync_state = 'failed', updated_at = now()
		WHERE branch_id = $1`, branch.ID); err != nil {
		t.Fatalf("gitea integration: fail the first sync: %v", err)
	}
	headOf := func() string {
		t.Helper()
		var head string
		if err := pool.QueryRow(ctx, `SELECT COALESCE(head_sha, '')
			FROM git_branch_refs WHERE branch_id = $1`, branch.ID).Scan(&head); err != nil {
			t.Fatalf("gitea integration: probe head: %v", err)
		}
		return head
	}
	if head := headOf(); head != "" {
		t.Fatalf("gitea integration: head after failed create sync = %q, want NULL", head)
	}

	shaA := strings.Repeat("a", 40)
	shaB := strings.Repeat("b", 40)
	store := gitprovider.NewPushIngestStore(pool)
	event := func(before, after, delivery string) gitprovider.PushEvent {
		return gitprovider.PushEvent{
			Ref:          "refs/heads/semantic",
			Before:       before,
			After:        after,
			RepositoryID: 43,
			Owner:        "post-git-svc",
			Name:         "stale-creation-repo",
			Pusher:       "post-git-svc",
			TotalCommits: 1,
			DeliveryID:   delivery,
		}
	}
	ingest := func(ev gitprovider.PushEvent, changes []gitprovider.ClassifiedChange) bool {
		t.Helper()
		inserted, err := store.IngestPush(ctx, gitprovider.IngestPushParams{Event: ev, Changes: changes})
		if err != nil {
			t.Fatalf("gitea integration: IngestPush(%s): %v", ev.DeliveryID, err)
		}
		return inserted
	}
	ingRow := func(sha string) (skipReason string, changeCount int) {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, `SELECT id::text, COALESCE(head_skip_reason, '')
			FROM git_push_ingestions
			WHERE gitea_repo_id = 43 AND git_ref = 'refs/heads/semantic' AND after_sha = $1`,
			sha).Scan(&id, &skipReason); err != nil {
			t.Fatalf("gitea integration: probe ingestion %s: %v", sha, err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM git_push_changes
			WHERE ingestion_id = $1`, id).Scan(&changeCount); err != nil {
			t.Fatalf("gitea integration: probe changes of %s: %v", sha, err)
		}
		return skipReason, changeCount
	}

	// Step 2: push A's first delivery 503'd before recording anything —
	// there is no A row. (Nothing to do here; the absence is the point.)

	// Step 3: the newer push B is ingested. Its before (shaA) matches no
	// head (NULL) — the advance is refused as 'stale_before' and the head
	// stays NULL, but B is recorded in full.
	if !ingest(event(shaA, shaB, "d-b"), []gitprovider.ClassifiedChange{
		{Path: "b.txt", Kind: gitprovider.ChangeAdded, File: gitprovider.FileKindUnstructured},
	}) {
		t.Fatal("gitea integration: B not inserted")
	}
	if head := headOf(); head != "" {
		t.Fatalf("gitea integration: B moved a NULL head: %q, want still NULL", head)
	}
	if reason, changes := ingRow(shaB); reason != "stale_before" || changes != 1 {
		t.Errorf("gitea integration: B's row = reason %q with %d changes, want stale_before with its change recorded", reason, changes)
	}

	// Step 4: the syncer's sweep retries the ref and lands the true tip:
	// failed → synced with head_sha = shaB (the machine's own transition).
	if _, err := pool.Exec(ctx, `UPDATE git_branch_refs
		SET sync_state = 'synced', synced_at = now(), fork_sha = $1, head_sha = $2, updated_at = now()
		WHERE branch_id = $3`, forkSHA, shaB, branch.ID); err != nil {
		t.Fatalf("gitea integration: sweep the ref: %v", err)
	}
	if head := headOf(); head != shaB {
		t.Fatalf("gitea integration: head after sweep = %q, want %s", head, shaB)
	}

	// Step 5: creation push A's redelivery arrives. It is NOT a duplicate
	// (no A row), but the creation path is guarded: the ref already
	// carries shaB, so A's advance is refused as 'stale_creation' — the
	// head stays shaB, and A is still recorded in full.
	if !ingest(event(gitprovider.ZerosSHA, shaA, "d-a"), []gitprovider.ClassifiedChange{
		{Path: "a.txt", Kind: gitprovider.ChangeAdded, File: gitprovider.FileKindUnstructured},
	}) {
		t.Fatal("gitea integration: A not inserted (its audit rows must still be recorded)")
	}
	if head := headOf(); head != shaB {
		t.Errorf("gitea integration: stale creation A rewound the head: %q, want still %s (B's commit)", head, shaB)
	}
	if reason, changes := ingRow(shaA); reason != "stale_creation" || changes != 1 {
		t.Errorf("gitea integration: A's row = reason %q with %d changes, want stale_creation with its change recorded", reason, changes)
	}
	if reason, _ := ingRow(shaB); reason != "stale_before" {
		t.Errorf("gitea integration: B's skip reason = %q, want unchanged stale_before", reason)
	}

	// Replays: both deliveries are now exact duplicates — complete
	// no-ops, and the head stays shaB either way.
	if ingest(event(gitprovider.ZerosSHA, shaA, "d-a-replay"), nil) {
		t.Fatal("gitea integration: A replay reported inserted, want duplicate")
	}
	if ingest(event(shaA, shaB, "d-b-replay"), nil) {
		t.Fatal("gitea integration: B replay reported inserted, want duplicate")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM git_push_ingestions
		WHERE gitea_repo_id = 43`).Scan(&n); err != nil {
		t.Fatalf("gitea integration: probe ingestion count: %v", err)
	}
	if n != 2 {
		t.Errorf("gitea integration: ingestion count = %d, want 2 (replays added nothing)", n)
	}
	if head := headOf(); head != shaB {
		t.Errorf("gitea integration: replays moved the head: %q, want still %s", head, shaB)
	}
}
