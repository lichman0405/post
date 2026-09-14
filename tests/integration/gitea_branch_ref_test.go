package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/config"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/gitprovider"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

// Required test "git branch integration": the full T0303 chain against a
// LIVE Gitea instance and real PostgreSQL — a semantic branch created
// through the product service produces the corresponding provider ref at
// the semantic fork point, and merge/abort delete the ref after recording
// its final head. CI has no Gitea, so every test here skips loudly when
// the instance is unreachable; the real-instance coverage is also the
// Supervisor's G3 gate (gates.json task_overrides gitea-real-services).

// branchRefGiteaFixture: one migrated test database, one user, one
// provision-pending project row, its genesis state, the branches service
// and the live adapters — the exact production graph, plus the T0301
// provisioner for the repository half.
type branchRefGiteaFixture struct {
	pool     *pgxpool.Pool
	base     string
	token    string
	user     domain.User
	project  domain.Project
	genesis  domain.ProjectState
	branches *branches.Service
	cfg      gitprovider.Config
}

func newBranchRefGiteaFixture(t *testing.T, ctx context.Context) *branchRefGiteaFixture {
	t.Helper()
	base := requireGitea(t)
	token := giteaServiceToken(t, base)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), branchRefTaskID)

	user, err := persistence.NewCredentialStore(pool).CreateWithPassword(
		ctx, "branchref-gitea@example.com", "hash", "branchref-gitea", "Branch Ref Gitea")
	if err != nil {
		t.Fatalf("gitea integration: seed user: %v", err)
	}
	project, _, err := persistence.NewProjectStore(pool).CreateProject(ctx, domain.Project{
		Slug:            "branchref-gitea",
		Name:            "Branch Ref Gitea",
		Purpose:         "T0303 integration",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, user.ID)
	if err != nil {
		t.Fatalf("gitea integration: create project: %v", err)
	}
	stateSvc := states.NewService(persistence.NewStateStore(pool), newCommitGuard(t))
	genesis, err := stateSvc.CreateInitialState(ctx, states.CreateInitialStateParams{
		ProjectID:       project.ID,
		ManifestVersion: "v1",
	})
	if err != nil {
		t.Fatalf("gitea integration: create genesis state: %v", err)
	}
	return &branchRefGiteaFixture{
		pool:     pool,
		base:     base,
		token:    token,
		user:     user,
		project:  project,
		genesis:  genesis,
		branches: branches.NewService(persistence.NewBranchStore(pool)),
		cfg: gitprovider.Config{
			BaseURL:    base,
			Token:      config.Secret(token),
			WebhookURL: "http://host.invalid/api/v1/git/hooks/gitea",
		},
	}
}

func (fx *branchRefGiteaFixture) syncer() *gitprovider.BranchRefSyncer {
	return gitprovider.NewBranchRefSyncer(
		gitprovider.NewGiteaAdapter(fx.cfg), gitprovider.NewBranchRefStore(fx.pool))
}

func (fx *branchRefGiteaFixture) owner(t *testing.T, ctx context.Context) string {
	t.Helper()
	owner, err := gitprovider.NewGiteaAdapter(fx.cfg).Owner(ctx)
	if err != nil {
		t.Fatalf("gitea integration: Owner: %v", err)
	}
	return owner
}

// provision provisions the repository through the T0301 chain and cleans
// it up at test end. It returns the repository's owner and name.
func (fx *branchRefGiteaFixture) provision(t *testing.T, ctx context.Context) (owner, name string) {
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
	t.Cleanup(func() { deleteGiteaRepo(t, fx.base, fx.token, owner, name) })
	return owner, name
}

// refsSHA reads one ref's tip sha through the refs API (the /branches API
// cannot answer for pushed refs on this instance — checked against the
// running instance, same workaround the G3 script documents).
func (fx *branchRefGiteaFixture) refsSHA(t *testing.T, owner, name, ref string) (int, string) {
	t.Helper()
	code, raw := giteaCall(t, http.MethodGet, fx.base, fx.token,
		"/api/v1/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+"/git/refs/heads/"+url.PathEscape(ref))
	if code != http.StatusOK {
		return code, ""
	}
	var refs []struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(raw, &refs); err != nil {
		t.Fatalf("gitea integration: decode ref %s: %v", ref, err)
	}
	if len(refs) == 0 {
		return http.StatusNotFound, ""
	}
	return code, refs[0].Object.SHA
}

// pushMain pushes one commit to refs/heads/main out-of-band and returns
// the pushed sha: the "initial commit arrives" simulation for a
// pre-T0302 empty repository, where no protection rule stands in the way
// (that repository's provisioner never reached EnsureMainProtection).
func (fx *branchRefGiteaFixture) pushMain(t *testing.T, owner, name string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# "+name+"\n"), 0o644); err != nil {
		t.Fatalf("gitea integration: write push file: %v", err)
	}
	remoteURL := fx.base + "/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + ".git"
	authHeader := "http.extraHeader=Authorization: token " + fx.token
	run := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", dir, "-c", authHeader}, args...)
		cmd := exec.Command("git", full...)
		cmd.Env = append(os.Environ(), "HOME="+dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gitea integration: git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "gitea-integration@example.com")
	run("config", "user.name", "Gitea Integration")
	run("add", ".")
	run("commit", "-m", "T0303 empty-repo initial commit")
	run("push", remoteURL, "main:refs/heads/main")
	code, sha := fx.refsSHA(t, owner, name, "main")
	if code != http.StatusOK || sha == "" {
		t.Fatalf("gitea integration: main after initial push = code %d", code)
	}
	return sha
}

// seedMain returns main's tip sha as Gitea sees it. On the merged tree
// (T0302) provisioning already seeds main with the bootstrap commit, so
// the helper's push (a non-main ref, the T0301 delivery trigger) does not
// move main; the returned sha is the seeded tip the branch forks from.
func (fx *branchRefGiteaFixture) seedMain(t *testing.T, owner, name string) string {
	t.Helper()
	pushToRepo(t, fx.base, fx.token, owner, name)
	code, sha := fx.refsSHA(t, owner, name, "main")
	if code != http.StatusOK {
		t.Fatalf("gitea integration: get main ref = %d", code)
	}
	if sha == "" {
		t.Fatal("gitea integration: main ref has no commit sha")
	}
	return sha
}

// giteaBranch returns the provider-side ref's status code and tip sha.
func (fx *branchRefGiteaFixture) giteaBranch(t *testing.T, owner, name, branch string) (int, string) {
	t.Helper()
	return fx.refsSHA(t, owner, name, branch)
}

// createRefAt pushes the ref <name> pointing at sha provider-side, out of
// band (the same protocol the adapter uses: the fork commit is
// shallow-fetched, then pushed to the new ref — the git client needs the
// object locally).
func (fx *branchRefGiteaFixture) createRefAt(t *testing.T, owner, name, ref, sha string) {
	t.Helper()
	dir := t.TempDir()
	remoteURL := fx.base + "/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + ".git"
	authHeader := "http.extraHeader=Authorization: token " + fx.token
	run := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", dir, "-c", authHeader}, args...)
		cmd := exec.Command("git", full...)
		cmd.Env = append(os.Environ(), "HOME="+dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gitea integration: git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "--bare", "-q", dir)
	run("fetch", "--depth=1", remoteURL, sha)
	run("push", remoteURL, sha+":refs/heads/"+ref)
}

// deleteRef removes one provider ref out-of-band (the same protocol the
// adapter uses): the "provider-side delete whose DB commit failed"
// simulation — the ref is gone, the mapping row still waits for its close.
func (fx *branchRefGiteaFixture) deleteRef(t *testing.T, owner, name, ref string) {
	t.Helper()
	dir := t.TempDir()
	remoteURL := fx.base + "/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + ".git"
	authHeader := "http.extraHeader=Authorization: token " + fx.token
	run := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", dir, "-c", authHeader}, args...)
		cmd := exec.Command("git", full...)
		cmd.Env = append(os.Environ(), "HOME="+dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gitea integration: git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "--bare", "-q", dir)
	run("push", remoteURL, ":refs/heads/"+ref)
}

func (fx *branchRefGiteaFixture) createBranch(t *testing.T, ctx context.Context, name, baseStateID string) domain.Branch {
	t.Helper()
	branch, err := fx.branches.Create(ctx, branches.CreateBranchParams{
		ProjectID:   fx.project.ID,
		Name:        name,
		Visibility:  domain.BranchVisibilityPrivate,
		BaseStateID: baseStateID,
		CreatedBy:   fx.user.ID,
	})
	if err != nil {
		t.Fatalf("gitea integration: create branch %s: %v", name, err)
	}
	return branch
}

// ingestState records the pushed main commit as a project state — the
// T0305 ingestion fact (project_states is append-only: the provider-side
// commit arrives WITH the row, never by UPDATE).
func (fx *branchRefGiteaFixture) ingestState(t *testing.T, ctx context.Context, sha string) string {
	t.Helper()
	var id string
	if err := fx.pool.QueryRow(ctx, `INSERT INTO project_states
		(project_id, state_hash, manifest_version, git_commit_sha)
		VALUES ($1, $2, 'v1', $3) RETURNING id`,
		fx.project.ID, "gitea-ingest-"+sha, sha).Scan(&id); err != nil {
		t.Fatalf("gitea integration: ingest state with commit sha: %v", err)
	}
	return id
}

// mappingRow probes one git_branch_refs row (same shape as the store-side
// fixture's probe).
func (fx *branchRefGiteaFixture) mappingRow(t *testing.T, ctx context.Context, branchID string) (gitRef, state, forkSHA, headSHA string, closeRequested bool) {
	t.Helper()
	if err := fx.pool.QueryRow(ctx, `SELECT git_ref, sync_state, COALESCE(fork_sha, ''),
		COALESCE(head_sha, ''), close_requested_at IS NOT NULL
		FROM git_branch_refs WHERE branch_id = $1`, branchID).
		Scan(&gitRef, &state, &forkSHA, &headSHA, &closeRequested); err != nil {
		t.Fatalf("gitea integration: probe git_branch_refs: %v", err)
	}
	return
}

// TestGiteaBranchRefCreatedAndClosedByLifecycle is the acceptance
// evidence: "semantic branch 与 Git ref 一致" over the live boundary —
// creation forks the ref at the semantic fork point, merge and abort both
// delete the ref after recording its final head, and a redelivered job is
// a no-op.
func TestGiteaBranchRefCreatedAndClosedByLifecycle(t *testing.T) {
	ctx := testCtx(t)
	fx := newBranchRefGiteaFixture(t, ctx)
	owner, name := fx.provision(t, ctx)
	mainSHA := fx.seedMain(t, owner, name)

	// ---- Create direction: a semantic branch must produce the ref, at
	// the fork point, before any sync has run. The branch forks the state
	// that carries the pushed commit (the T0305 ingestion fact).
	forkState := fx.ingestState(t, ctx, mainSHA)
	branch := fx.createBranch(t, ctx, "feature-x", forkState)
	if code, _ := fx.giteaBranch(t, owner, name, "feature-x"); code != http.StatusNotFound {
		t.Fatalf("gitea integration: ref exists before sync (code %d)", code)
	}
	if err := fx.syncer().Sync(ctx, branch.ID); err != nil {
		t.Fatalf("gitea integration: Sync (create): %v", err)
	}
	code, sha := fx.giteaBranch(t, owner, name, "feature-x")
	if code != http.StatusOK || sha != mainSHA {
		t.Errorf("gitea integration: ref after create = code %d tip %q, want 200 at the fork point %s", code, sha, mainSHA)
	}
	gitRef, state, forkSHA, headSHA, _ := fx.mappingRow(t, ctx, branch.ID)
	if gitRef != "refs/heads/feature-x" || state != "synced" {
		t.Errorf("gitea integration: mapping row = %s/%s, want refs/heads/feature-x/synced", gitRef, state)
	}
	if forkSHA != mainSHA || headSHA != mainSHA {
		t.Errorf("gitea integration: mapping row shas = fork %q head %q, want both the fork point %s", forkSHA, headSHA, mainSHA)
	}

	// ---- Redelivery: idempotent — the ref is untouched.
	if err := fx.syncer().Sync(ctx, branch.ID); err != nil {
		t.Fatalf("gitea integration: Sync (redelivery): %v", err)
	}
	if code, sha := fx.giteaBranch(t, owner, name, "feature-x"); code != http.StatusOK || sha != mainSHA {
		t.Errorf("gitea integration: redelivery changed the ref: code %d tip %q", code, sha)
	}

	// ---- Merge direction: the semantic close deletes the ref after
	// recording its final head.
	if _, err := fx.branches.Merge(ctx, fx.project.ID, branch.ID); err != nil {
		t.Fatalf("gitea integration: Merge: %v", err)
	}
	if err := fx.syncer().Sync(ctx, branch.ID); err != nil {
		t.Fatalf("gitea integration: Sync (merge close): %v", err)
	}
	if code, _ := fx.giteaBranch(t, owner, name, "feature-x"); code != http.StatusNotFound {
		t.Errorf("gitea integration: ref after merge = code %d, want 404 (deleted)", code)
	}
	gitRef, state, _, headSHA, _ = fx.mappingRow(t, ctx, branch.ID)
	if gitRef != "refs/heads/feature-x" || state != "closed" {
		t.Errorf("gitea integration: mapping row after merge = %s/%s, want closed", gitRef, state)
	}
	if headSHA != mainSHA {
		t.Errorf("gitea integration: final head = %q, want %s recorded before deletion", headSHA, mainSHA)
	}

	// ---- Abort direction: same contract — the ref is deleted, the final
	// head recorded.
	branch2 := fx.createBranch(t, ctx, "feature-y", forkState)
	if err := fx.syncer().Sync(ctx, branch2.ID); err != nil {
		t.Fatalf("gitea integration: Sync (feature-y create): %v", err)
	}
	if code, _ := fx.giteaBranch(t, owner, name, "feature-y"); code != http.StatusOK {
		t.Fatalf("gitea integration: feature-y ref after create = code %d, want 200", code)
	}
	if _, err := fx.branches.Abort(ctx, fx.project.ID, branch2.ID); err != nil {
		t.Fatalf("gitea integration: Abort: %v", err)
	}
	if err := fx.syncer().Sync(ctx, branch2.ID); err != nil {
		t.Fatalf("gitea integration: Sync (abort close): %v", err)
	}
	if code, _ := fx.giteaBranch(t, owner, name, "feature-y"); code != http.StatusNotFound {
		t.Errorf("gitea integration: ref after abort = code %d, want 404 (deleted)", code)
	}
	_, state, _, headSHA, _ = fx.mappingRow(t, ctx, branch2.ID)
	if state != "closed" || headSHA != mainSHA {
		t.Errorf("gitea integration: feature-y row after abort = %s head %q, want closed with the final head", state, headSHA)
	}
}

// TestGiteaBranchRefCloseAlreadyGoneSucceeds: the close direction must
// tolerate a ref that is ALREADY absent — an out-of-band deletion, or a
// provider delete whose DB commit failed and whose job is redelivered.
// The git client itself refuses the delete of a nonexistent remote ref
// ("remote ref does not exist"), so the adapter must map exactly that
// refusal to success: the close strategy's goal — "the ref must not
// exist" — is already achieved, and the row must reach 'closed' (its
// terminal state), never wedge in 'failed' retrying work that is done.
func TestGiteaBranchRefCloseAlreadyGoneSucceeds(t *testing.T) {
	ctx := testCtx(t)
	fx := newBranchRefGiteaFixture(t, ctx)
	owner, name := fx.provision(t, ctx)
	mainSHA := fx.seedMain(t, owner, name)

	forkState := fx.ingestState(t, ctx, mainSHA)
	branch := fx.createBranch(t, ctx, "feature-z", forkState)
	if err := fx.syncer().Sync(ctx, branch.ID); err != nil {
		t.Fatalf("gitea integration: Sync (create): %v", err)
	}
	if code, sha := fx.giteaBranch(t, owner, name, "feature-z"); code != http.StatusOK || sha != mainSHA {
		t.Fatalf("gitea integration: feature-z ref after create = code %d tip %q, want 200 at %s", code, sha, mainSHA)
	}
	if _, err := fx.branches.Merge(ctx, fx.project.ID, branch.ID); err != nil {
		t.Fatalf("gitea integration: Merge: %v", err)
	}
	_, state, _, _, closeRequested := fx.mappingRow(t, ctx, branch.ID)
	if state != "closing" || !closeRequested {
		t.Fatalf("gitea integration: row after Merge = %s closeRequested %v, want closing with a close request", state, closeRequested)
	}

	// The ref disappears out-of-band: the provider delete happened, the DB
	// commit did not. The redelivered close job must still reach 'closed'.
	fx.deleteRef(t, owner, name, "feature-z")
	if code, _ := fx.giteaBranch(t, owner, name, "feature-z"); code != http.StatusNotFound {
		t.Fatalf("gitea integration: out-of-band delete left the ref (code %d)", code)
	}
	if err := fx.syncer().Sync(ctx, branch.ID); err != nil {
		t.Fatalf("gitea integration: Sync (close with ref already gone) = %v, want success", err)
	}
	_, state, _, headSHA, _ := fx.mappingRow(t, ctx, branch.ID)
	if state != "closed" {
		t.Errorf("gitea integration: row after already-gone close = %s, want closed (never wedged in failed)", state)
	}
	if headSHA != mainSHA {
		t.Errorf("gitea integration: final head = %q, want the last recorded %s", headSHA, mainSHA)
	}
	if code, _ := fx.giteaBranch(t, owner, name, "feature-z"); code != http.StatusNotFound {
		t.Errorf("gitea integration: ref after close = code %d, want 404 (still absent)", code)
	}
}

// TestGiteaBranchRefEmptyRepositoryRetries: a branch whose repository has
// no refs at all cannot fork anything — the sync fails with ErrNotFound,
// the row lands in the retryable backlog, and once the initial commit
// lands the retry forks the repository's default branch. fork_sha stays
// NULL: the semantic fork point was never recorded, "forked the default
// branch" is the fact.
//
// T0302 merged after this test was written and changed the world it set
// up: provisioning now seeds main with the bootstrap commit, so the
// product chain no longer produces an empty provisioned repository. The
// empty-repo state still exists on an upgraded instance, though — a
// repository T0301 provisioned (auto_init false) with its provision row
// recorded has no refs, and T0302's protection sweep re-applies the rule
// but never seeds. The fixture therefore constructs that pre-T0302 state
// out-of-band (repository + provision row, no pushes), the same way
// createRefAt constructs a racing ref; every assertion is unchanged.
func TestGiteaBranchRefEmptyRepositoryRetries(t *testing.T) {
	ctx := testCtx(t)
	fx := newBranchRefGiteaFixture(t, ctx)
	owner := fx.owner(t, ctx)
	name := gitprovider.RepositoryName(fx.project.ID)
	body, err := json.Marshal(map[string]any{
		"name":        name,
		"private":     true,
		"auto_init":   false,
		"description": "T0303 empty-repo fixture",
	})
	if err != nil {
		t.Fatalf("gitea integration: marshal repo body: %v", err)
	}
	code, raw := giteaCallBody(t, http.MethodPost, fx.base, fx.token, "/api/v1/user/repos", string(body))
	if code != http.StatusCreated {
		t.Fatalf("gitea integration: create empty repository = %d (body %s)", code, raw)
	}
	t.Cleanup(func() { deleteGiteaRepo(t, fx.base, fx.token, owner, name) })
	if _, err := fx.pool.Exec(ctx, `INSERT INTO git_repository_provisions
		(project_id, owner, name, gitea_repo_id, webhook_id, webhook_secret)
		VALUES ($1::uuid, $2, 'p-' || $1, 1, 1, 's')`, fx.project.ID, owner); err != nil {
		t.Fatalf("gitea integration: seed provision row: %v", err)
	}

	branch := fx.createBranch(t, ctx, "feature-x", fx.genesis.ID)
	err = fx.syncer().Sync(ctx, branch.ID)
	if !errors.Is(err, gitprovider.ErrNotFound) {
		t.Fatalf("gitea integration: Sync on an empty repository = %v, want ErrNotFound", err)
	}
	_, state, _, _, _ := fx.mappingRow(t, ctx, branch.ID)
	if state != "failed" {
		t.Errorf("gitea integration: empty-repo failure state = %q, want failed (retryable)", state)
	}

	// The initial commit lands (a plain push, the way every external commit
	// arrives). The retry forks the default branch.
	mainSHA := fx.pushMain(t, owner, name)
	if err := fx.syncer().Sync(ctx, branch.ID); err != nil {
		t.Fatalf("gitea integration: Sync (retry after main): %v", err)
	}
	code, sha := fx.giteaBranch(t, owner, name, "feature-x")
	if code != http.StatusOK || sha != mainSHA {
		t.Errorf("gitea integration: ref after retry = code %d tip %q, want 200 at main's tip %s", code, sha, mainSHA)
	}
	_, state, forkSHA, headSHA, _ := fx.mappingRow(t, ctx, branch.ID)
	if state != "synced" || headSHA != mainSHA {
		t.Errorf("gitea integration: row after retry = %s head %q, want synced with the recorded head", state, headSHA)
	}
	if forkSHA != "" {
		t.Errorf("gitea integration: fork_sha = %q, want empty — the fallback forked the default branch, not a recorded state commit", forkSHA)
	}
}

// TestGiteaBranchRefAdoptsExistingRef: when the ref already exists
// provider-side (created by a racing attempt, or out-of-band), the syncer
// adopts it — records the ACTUAL head, never recreates or fails. The ref
// is the truth: the semantic row and the provider ref stay consistent
// without a spurious conflict.
func TestGiteaBranchRefAdoptsExistingRef(t *testing.T) {
	ctx := testCtx(t)
	fx := newBranchRefGiteaFixture(t, ctx)
	owner, name := fx.provision(t, ctx)
	mainSHA := fx.seedMain(t, owner, name)

	// The ref exists before the semantic branch row does (out of band —
	// e.g. a racing attempt, or a push that arrived first).
	fx.createRefAt(t, owner, name, "feature-z", mainSHA)

	forkState := fx.ingestState(t, ctx, mainSHA)
	branch := fx.createBranch(t, ctx, "feature-z", forkState)
	if err := fx.syncer().Sync(ctx, branch.ID); err != nil {
		t.Fatalf("gitea integration: Sync (adopt): %v", err)
	}
	if code, sha := fx.giteaBranch(t, owner, name, "feature-z"); code != http.StatusOK || sha != mainSHA {
		t.Errorf("gitea integration: adopted ref = code %d tip %q, want 200 at %s", code, sha, mainSHA)
	}
	_, state, _, headSHA, _ := fx.mappingRow(t, ctx, branch.ID)
	if state != "synced" || headSHA != mainSHA {
		t.Errorf("gitea integration: row after adopt = %s head %q, want synced with the ADOPTED head", state, headSHA)
	}
}
