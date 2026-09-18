package integration

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/config"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/gitprovider"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

// The G1 real-services gate for T0301 (gitea integration): the full
// provisioning chain against a LIVE Gitea instance and real PostgreSQL —
// project row → GitProvider repository → push webhook with HMAC secret →
// canonical mapping row. CI has no Gitea, so every test here skips loudly
// when the instance is unreachable; the real-instance coverage is also the
// Supervisor's G3 gate (gates.json task_overrides
// gitea-real-services → tests/acceptance/gitea-real-services-e2e.sh).

const giteaProvisionTaskID = "T0301"

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// giteaBase returns the instance under test (POST_GITEA_BASE_URL, dev
// default http://127.0.0.1:3000).
func giteaBase(t *testing.T) string {
	t.Helper()
	return strings.TrimSuffix(envDefault("POST_GITEA_BASE_URL", gitprovider.DefaultBaseURL), "/")
}

// requireGitea probes the instance and skips loudly when it cannot answer
// (CI's postgres-only service matrix — the G3 gate covers the real stack).
func requireGitea(t *testing.T) string {
	t.Helper()
	base := giteaBase(t)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(base + "/api/v1/version")
	if err != nil {
		t.Skipf("gitea integration: %s unreachable (%v) — CI has no Gitea service; "+
			"the real-instance coverage runs at the G3 gate (gates.json task_overrides gitea-real-services)", base, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("gitea integration: %s answered %d — no usable Gitea instance; "+
			"the real-instance coverage runs at the G3 gate", base, resp.StatusCode)
	}
	return base
}

// giteaServiceToken returns the service account token: POST_GITEA_TOKEN
// when the operator provided one, otherwise a run-scoped token minted
// through the admin's basic auth (dev defaults from init-gitea.sh) and
// revoked again at cleanup — Gitea test resources are run-specific
// (docs/66 §3).
func giteaServiceToken(t *testing.T, base string) string {
	t.Helper()
	if v := os.Getenv("POST_GITEA_TOKEN"); v != "" {
		return v
	}
	adminUser := envDefault("GITEA_ADMIN_USER", "postadmin")
	adminPass := envDefault("GITEA_ADMIN_PASSWORD", "postadmin_dev_pw")
	svc := envDefault("GITEA_SERVICE_ACCOUNT", "post-git-svc")

	// Gitea 1.27 requires an explicit scope list when minting; the service
	// account needs repositories (create/delete/push) and users (self read),
	// the same set infra/docker/gitea/init-gitea.sh mints.
	body := fmt.Sprintf(`{"name":"t0301-test-%d","scopes":["write:repository","write:user"]}`, time.Now().UnixNano())
	req, err := http.NewRequest(http.MethodPost,
		base+"/api/v1/users/"+url.PathEscape(svc)+"/tokens", strings.NewReader(body))
	if err != nil {
		t.Fatalf("gitea integration: build token request: %v", err)
	}
	req.SetBasicAuth(adminUser, adminPass)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("gitea integration: mint token: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("gitea integration: mint token = %d (admin %s basic auth) — set POST_GITEA_TOKEN or fix GITEA_ADMIN_USER/GITEA_ADMIN_PASSWORD",
			resp.StatusCode, adminUser)
	}
	var minted struct {
		ID   int64  `json:"id"`
		SHA1 string `json:"sha1"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&minted); err != nil {
		t.Fatalf("gitea integration: decode minted token: %v", err)
	}
	if minted.SHA1 == "" {
		t.Fatal("gitea integration: minted token has no sha1")
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
			t.Logf("gitea integration: revoke token failed: %v", err)
			return
		}
		resp.Body.Close()
	})
	return minted.SHA1
}

// gitAuthEnv is the environment that hands one git invocation its
// Authorization header. The header rides GIT_CONFIG_VALUE_0 — the shape
// internal/gitprovider/gitea.go uses — and NOT a `-c http.extraHeader=...`
// argument: a `-c` value is an argument, and argv is readable by every
// account on the machine (`ps aux`, /proc/<pid>/cmdline). The parameter is
// the header value itself ("Authorization: token <token>"), so a caller's
// intent reads the same as before the move.
func gitAuthEnv(header string) []string {
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=" + header,
	}
}

// giteaCall performs one authenticated provider call (the token attaches as
// the Authorization header; never in a URL).
func giteaCall(t *testing.T, method, base, token, path string) (int, []byte) {
	return giteaCallBody(t, method, base, token, path, "")
}

// giteaCallBody is giteaCall with a JSON request body.
func giteaCallBody(t *testing.T, method, base, token, path, body string) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		t.Fatalf("gitea integration: build %s %s: %v", method, path, err)
	}
	req.Header.Set("Authorization", "token "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("gitea integration: %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("gitea integration: read %s %s: %v", method, path, err)
	}
	return resp.StatusCode, raw
}

func giteaRepo(t *testing.T, base, token, owner, name string) (int64, bool, string, bool) {
	t.Helper()
	code, raw := giteaCall(t, http.MethodGet, base, token,
		"/api/v1/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name))
	if code != http.StatusOK {
		t.Fatalf("gitea integration: get repo = %d (body %s)", code, raw)
	}
	var repo struct {
		ID          int64  `json:"id"`
		Private     bool   `json:"private"`
		Description string `json:"description"`
		Empty       bool   `json:"empty"`
	}
	if err := json.Unmarshal(raw, &repo); err != nil {
		t.Fatalf("gitea integration: decode repo: %v", err)
	}
	return repo.ID, repo.Private, repo.Description, repo.Empty
}

type giteaHook struct {
	ID     int64  `json:"id"`
	Type   string `json:"type"`
	Active bool   `json:"active"`
	Config struct {
		URL string `json:"url"`
	} `json:"config"`
}

func giteaHooks(t *testing.T, base, token, owner, name string) []giteaHook {
	t.Helper()
	code, raw := giteaCall(t, http.MethodGet, base, token,
		"/api/v1/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+"/hooks")
	if code != http.StatusOK {
		t.Fatalf("gitea integration: list hooks = %d (body %s)", code, raw)
	}
	var hooks []giteaHook
	if err := json.Unmarshal(raw, &hooks); err != nil {
		t.Fatalf("gitea integration: decode hooks: %v", err)
	}
	return hooks
}

func deleteGiteaRepo(t *testing.T, base, token, owner, name string) {
	t.Helper()
	code, raw := giteaCall(t, http.MethodDelete, base, token,
		"/api/v1/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name))
	if code != http.StatusNoContent && code != http.StatusOK && code != http.StatusNotFound {
		t.Logf("gitea integration: repo cleanup = %d (body %s)", code, raw)
	}
}

// giteaFixture: one migrated test database, one user, one provision-pending
// project row, and the live adapter — the exact production graph.
type giteaFixture struct {
	pool        *pgxpool.Pool
	base, token string
	user        domain.User
	project     domain.Project
	cfg         gitprovider.Config
}

func newGiteaFixture(t *testing.T, ctx context.Context) *giteaFixture {
	t.Helper()
	base := requireGitea(t)
	token := giteaServiceToken(t, base)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), giteaProvisionTaskID)

	users := persistence.NewCredentialStore(pool)
	user, err := users.CreateWithPassword(ctx, "gitea-prov@example.com", "hash", "gitea-prov", "Gitea Prov")
	if err != nil {
		t.Fatalf("gitea integration: seed user: %v", err)
	}
	project, _, err := persistence.NewProjectStore(pool).CreateProject(ctx, domain.Project{
		Slug:            "gitea-e2e",
		Name:            "Gitea E2E",
		Purpose:         "T0301 integration",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, user.ID)
	if err != nil {
		t.Fatalf("gitea integration: create project: %v", err)
	}
	if project.ProvisionStatus != domain.ProvisionPending {
		t.Fatalf("gitea integration: new project provision status = %q, want pending", project.ProvisionStatus)
	}
	return &giteaFixture{
		base: base, token: token, user: user, project: project, pool: pool,
		cfg: gitprovider.Config{
			BaseURL:    base,
			Token:      config.Secret(token),
			WebhookURL: "http://host.invalid/api/v1/git/hooks/gitea",
		},
	}
}

func (fx *giteaFixture) provisioner() *gitprovider.Provisioner {
	return gitprovider.NewProvisioner(
		gitprovider.NewGiteaAdapter(fx.cfg),
		gitprovider.NewProvisionStore(fx.pool),
		fx.cfg.WebhookURL)
}

// TestGiteaProvisioningEndToEnd: the acceptance criterion — a new Project
// can provision a repository — with every link of the chain asserted on
// both sides of the boundary (provider facts AND canonical facts), plus
// idempotency of a redelivered job.
func TestGiteaProvisioningEndToEnd(t *testing.T) {
	ctx := testCtx(t)
	fx := newGiteaFixture(t, ctx)

	if err := fx.provisioner().Provision(ctx, fx.project.ID); err != nil {
		t.Fatalf("gitea integration: Provision: %v", err)
	}

	adapter := gitprovider.NewGiteaAdapter(fx.cfg)
	owner, err := adapter.Owner(ctx)
	if err != nil {
		t.Fatalf("gitea integration: Owner: %v", err)
	}
	if owner == "" {
		t.Fatal("gitea integration: resolved owner is empty")
	}
	name := gitprovider.RepositoryName(fx.project.ID)
	t.Cleanup(func() { deleteGiteaRepo(t, fx.base, fx.token, owner, name) })

	// ---- Gitea side: the repository exists with the expected shape.
	repoID, private, description, empty := giteaRepo(t, fx.base, fx.token, owner, name)
	if !private {
		t.Error("gitea integration: repository is public — Git-layer privacy is unconditional")
	}
	if want := "POST project gitea-e2e (Gitea E2E)"; description != want {
		t.Errorf("gitea integration: description = %q, want %q", description, want)
	}
	if empty {
		t.Error("gitea integration: repository is empty — T0302 seeds main with a bootstrap commit during provisioning (the rule blocks main's first push, and the provider refuses PRs against a main that does not exist, so an empty main would deadlock)")
	}
	// main carries exactly one commit: the platform's bootstrap, authored by
	// the service identity.
	code, raw := giteaCall(t, http.MethodGet, fx.base, fx.token,
		"/api/v1/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+"/commits?sha=main")
	if code != http.StatusOK {
		t.Fatalf("gitea integration: list main commits = %d (body %s)", code, raw)
	}
	var commits []struct {
		Commit struct {
			Message string `json:"message"`
			Author  struct {
				Name string `json:"name"`
			} `json:"author"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(raw, &commits); err != nil {
		t.Fatalf("gitea integration: decode main commits: %v", err)
	}
	if len(commits) != 1 {
		t.Fatalf("gitea integration: main commits = %d, want exactly 1 (the bootstrap)", len(commits))
	}
	if got := strings.TrimSpace(commits[0].Commit.Message); got != "POST repository bootstrap" {
		t.Errorf("gitea integration: bootstrap message = %q, want the platform-identifying message", got)
	}
	if commits[0].Commit.Author.Name != owner {
		t.Errorf("gitea integration: bootstrap author = %q, want the service identity %q", commits[0].Commit.Author.Name, owner)
	}

	// ---- Gitea side: exactly one active gitea-type webhook at the URL.
	hooks := giteaHooks(t, fx.base, fx.token, owner, name)
	matching := 0
	var hookID int64
	for _, h := range hooks {
		if h.Type == "gitea" && h.Config.URL == fx.cfg.WebhookURL && h.Active {
			matching++
			hookID = h.ID
		}
	}
	if matching != 1 {
		t.Errorf("gitea integration: matching active push webhooks = %d, want exactly 1 (hooks: %+v)", matching, hooks)
	}

	// ---- Canonical side: the project row and the mapping row.
	var status, external string
	if err := fx.pool.QueryRow(ctx,
		`SELECT provision_status, git_repository_external_id FROM projects WHERE id = $1`,
		fx.project.ID).Scan(&status, &external); err != nil {
		t.Fatalf("gitea integration: probe project row: %v", err)
	}
	if status != "provisioned" {
		t.Errorf("gitea integration: provision_status = %q, want provisioned", status)
	}
	if external != owner+"/"+name {
		t.Errorf("gitea integration: git_repository_external_id = %q, want %q", external, owner+"/"+name)
	}

	var rowOwner, rowName, secret string
	var rowRepoID, rowHookID int64
	var provisionedAt time.Time
	if err := fx.pool.QueryRow(ctx,
		`SELECT owner, name, gitea_repo_id, webhook_id, webhook_secret, provisioned_at
		   FROM git_repository_provisions WHERE project_id = $1`,
		fx.project.ID).Scan(&rowOwner, &rowName, &rowRepoID, &rowHookID, &secret, &provisionedAt); err != nil {
		t.Fatalf("gitea integration: probe provision row: %v", err)
	}
	if rowOwner != owner || rowName != name {
		t.Errorf("gitea integration: provision row = %s/%s, want %s/%s", rowOwner, rowName, owner, name)
	}
	if rowRepoID != repoID || rowHookID != hookID {
		t.Errorf("gitea integration: provision row ids = repo %d hook %d, want repo %d hook %d", rowRepoID, rowHookID, repoID, hookID)
	}
	if _, err := hex.DecodeString(secret); err != nil || len(secret) != 64 {
		t.Errorf("gitea integration: webhook_secret = %q, want 64 hex chars", secret)
	}
	if time.Since(provisionedAt) > 5*time.Minute {
		t.Errorf("gitea integration: provisioned_at = %v, want fresh", provisionedAt)
	}

	// ---- Idempotency: a redelivered job is a no-op (skip path), and the
	// provider side stays at exactly one hook.
	if err := fx.provisioner().Provision(ctx, fx.project.ID); err != nil {
		t.Fatalf("gitea integration: second Provision: %v", err)
	}
	hooks = giteaHooks(t, fx.base, fx.token, owner, name)
	matching = 0
	for _, h := range hooks {
		if h.Type == "gitea" && h.Config.URL == fx.cfg.WebhookURL && h.Active {
			matching++
		}
	}
	if matching != 1 {
		t.Errorf("gitea integration: after redelivery, matching hooks = %d, want still 1", matching)
	}
	var n int
	if err := fx.pool.QueryRow(ctx,
		`SELECT count(*) FROM git_repository_provisions WHERE project_id = $1`, fx.project.ID).Scan(&n); err != nil {
		t.Fatalf("gitea integration: count provision rows: %v", err)
	}
	if n != 1 {
		t.Errorf("gitea integration: provision rows = %d, want 1", n)
	}
}

// TestGiteaProvisioningFailureThenRetry: a provider outage marks the
// project failed and surfaces a redacted reason on the returned error
// (the text the job loop logs — never a secret), the failed row stays in
// the provisioning backlog (the boot sweep's work list), and a later
// attempt recovers the project. A failed row is still retriable, so a
// transient outage never wedges a project in pending limbo.
func TestGiteaProvisioningFailureThenRetry(t *testing.T) {
	ctx := testCtx(t)
	fx := newGiteaFixture(t, ctx)

	// Nothing listens on 127.0.0.1:1 — the adapter fails fast.
	badCfg := fx.cfg
	badCfg.BaseURL = "http://127.0.0.1:1"
	bad := gitprovider.NewProvisioner(gitprovider.NewGiteaAdapter(badCfg),
		gitprovider.NewProvisionStore(fx.pool), badCfg.WebhookURL)

	err := bad.Provision(ctx, fx.project.ID)
	if !errors.Is(err, gitprovider.ErrUnavailable) {
		t.Fatalf("gitea integration: outage Provision = %v, want ErrUnavailable", err)
	}
	// The reason is on the error (the canonical store records state only —
	// see 00022): it names the failure and must never leak the token.
	if !strings.Contains(err.Error(), "provider unavailable") {
		t.Errorf("gitea integration: outage error = %q, want the provider-unavailable reason", err)
	}
	if strings.Contains(err.Error(), fx.token) {
		t.Error("gitea integration: outage error leaks the service account token")
	}
	var status string
	if err := fx.pool.QueryRow(ctx,
		`SELECT provision_status FROM projects WHERE id = $1`,
		fx.project.ID).Scan(&status); err != nil {
		t.Fatalf("gitea integration: probe failed row: %v", err)
	}
	if status != "failed" {
		t.Errorf("gitea integration: provision_status after outage = %q, want failed", status)
	}
	var rows int
	if err := fx.pool.QueryRow(ctx,
		`SELECT count(*) FROM git_repository_provisions WHERE project_id = $1`,
		fx.project.ID).Scan(&rows); err != nil {
		t.Fatalf("gitea integration: count provision rows: %v", err)
	}
	if rows != 0 {
		t.Errorf("gitea integration: provision rows after outage = %d, want 0 (nothing was created)", rows)
	}

	// The failed row stays in the provisioning backlog — the boot sweep's
	// work list. That is the bounded retry policy: the next sweep (or a
	// redelivered job) re-attempts it.
	store := gitprovider.NewProvisionStore(fx.pool)
	backlog, err := store.ProvisioningBacklog(ctx)
	if err != nil {
		t.Fatalf("gitea integration: ProvisioningBacklog: %v", err)
	}
	inBacklog := false
	for _, p := range backlog {
		if p.ID == fx.project.ID {
			inBacklog = true
		}
	}
	if !inBacklog {
		t.Error("gitea integration: failed project missing from the provisioning backlog — no sweep could ever re-attempt it")
	}

	// Retry against the real instance: the row recovers.
	if err := fx.provisioner().Provision(ctx, fx.project.ID); err != nil {
		t.Fatalf("gitea integration: retry Provision: %v", err)
	}
	if err := fx.pool.QueryRow(ctx,
		`SELECT provision_status FROM projects WHERE id = $1`,
		fx.project.ID).Scan(&status); err != nil {
		t.Fatalf("gitea integration: probe recovered row: %v", err)
	}
	if status != "provisioned" {
		t.Errorf("gitea integration: after retry = %q, want provisioned", status)
	}
	backlog, err = store.ProvisioningBacklog(ctx)
	if err != nil {
		t.Fatalf("gitea integration: ProvisioningBacklog after recovery: %v", err)
	}
	for _, p := range backlog {
		if p.ID == fx.project.ID {
			t.Error("gitea integration: recovered project is still in the backlog")
		}
	}

	adapter := gitprovider.NewGiteaAdapter(fx.cfg)
	owner, err := adapter.Owner(ctx)
	if err != nil {
		t.Fatalf("gitea integration: Owner: %v", err)
	}
	t.Cleanup(func() { deleteGiteaRepo(t, fx.base, fx.token, owner, gitprovider.RepositoryName(fx.project.ID)) })
}

// TestGiteaAdoptRepairsPublicRepository: when the deterministic name
// already exists provider-side as a PUBLIC repository (a stray manual
// create would leave it that way), provisioning adopts it and repairs the
// Git-layer visibility — the project provisions, the repository ends
// private, and exactly one webhook lands (the full chain over the adopt
// path, against live Gitea).
func TestGiteaAdoptRepairsPublicRepository(t *testing.T) {
	ctx := testCtx(t)
	fx := newGiteaFixture(t, ctx)

	adapter := gitprovider.NewGiteaAdapter(fx.cfg)
	owner, err := adapter.Owner(ctx)
	if err != nil {
		t.Fatalf("gitea integration: Owner: %v", err)
	}
	name := gitprovider.RepositoryName(fx.project.ID)
	t.Cleanup(func() { deleteGiteaRepo(t, fx.base, fx.token, owner, name) })

	// Pre-create the repository PUBLIC, the way a stray manual create
	// would leave it.
	code, raw := giteaCallBody(t, http.MethodPost, fx.base, fx.token, "/api/v1/user/repos",
		fmt.Sprintf(`{"name":%q,"private":false,"auto_init":false}`, name))
	if code != http.StatusCreated {
		t.Fatalf("gitea integration: pre-create public repo = %d (body %s)", code, raw)
	}
	if _, isPrivate, _, _ := giteaRepo(t, fx.base, fx.token, owner, name); isPrivate {
		t.Fatal("gitea integration: fixture failed — the pre-created repository is not public")
	}

	// Provision: the adopt path must flip the repository private.
	if err := fx.provisioner().Provision(ctx, fx.project.ID); err != nil {
		t.Fatalf("gitea integration: Provision over an existing public repo: %v", err)
	}
	if _, isPrivate, _, _ := giteaRepo(t, fx.base, fx.token, owner, name); !isPrivate {
		t.Error("gitea integration: adopted repository is still public — the repair PATCH did not take")
	}

	// The canonical row and the webhook landed exactly like the create
	// path.
	var status, external string
	if err := fx.pool.QueryRow(ctx,
		`SELECT provision_status, git_repository_external_id FROM projects WHERE id = $1`,
		fx.project.ID).Scan(&status, &external); err != nil {
		t.Fatalf("gitea integration: probe project row: %v", err)
	}
	if status != "provisioned" || external != owner+"/"+name {
		t.Errorf("gitea integration: adopted project = %q/%q, want provisioned/%q", status, external, owner+"/"+name)
	}
	matching := 0
	for _, h := range giteaHooks(t, fx.base, fx.token, owner, name) {
		if h.Type == "gitea" && h.Config.URL == fx.cfg.WebhookURL && h.Active {
			matching++
		}
	}
	if matching != 1 {
		t.Errorf("gitea integration: matching active webhooks after adopt = %d, want exactly 1", matching)
	}
}

// ---- delivery round-trip --------------------------------------------------

// deliveryRecorder is the platform-side webhook receiver stand-in: it
// captures every delivery's headers and raw body (the signature must be
// verified over the RAW body, exactly as T0305 will).
type deliveryRecorder struct {
	mu   sync.Mutex
	seen []delivery
}

type delivery struct {
	headers http.Header
	body    []byte
}

func (d *deliveryRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	d.mu.Lock()
	d.seen = append(d.seen, delivery{headers: r.Header.Clone(), body: body})
	d.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// waitPush polls for a push delivery until the deadline.
func (d *deliveryRecorder) waitPush(t *testing.T, deadline time.Duration) *delivery {
	t.Helper()
	deadlineAt := time.Now().Add(deadline)
	for time.Now().Before(deadlineAt) {
		d.mu.Lock()
		for i := range d.seen {
			if d.seen[i].headers.Get("X-Gitea-Event") == "push" {
				d.mu.Unlock()
				return &d.seen[i]
			}
		}
		d.mu.Unlock()
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

// TestGiteaWebhookDeliverySignature: the full secret round-trip — the
// provisioner registers the webhook with a fresh secret, a real push makes
// Gitea deliver it, and the delivered X-Gitea-Signature equals
// hex(hmac-sha256(stored_secret, raw_body)). Gated on
// POST_GITEA_WEBHOOK_URL: its host must be reachable FROM the Gitea
// container (in local dev that is the docker bridge gateway, e.g.
// http://172.18.0.1:18099 — the test swaps in its own ephemeral port).
func TestGiteaWebhookDeliverySignature(t *testing.T) {
	ctx := testCtx(t)
	base := requireGitea(t)
	envURL := os.Getenv("POST_GITEA_WEBHOOK_URL")
	if envURL == "" {
		t.Skipf("gitea integration: POST_GITEA_WEBHOOK_URL not set — the delivery round-trip is skipped; " +
			"set it to a URL the Gitea container can reach to verify X-Gitea-Signature end to end")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("gitea integration: git CLI not available for the push round-trip: %v", err)
	}
	u, err := url.Parse(envURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		t.Fatalf("gitea integration: POST_GITEA_WEBHOOK_URL %q is not an http(s) URL", envURL)
	}
	token := giteaServiceToken(t, base)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), giteaProvisionTaskID)

	users := persistence.NewCredentialStore(pool)
	user, err := users.CreateWithPassword(ctx, "gitea-delivery@example.com", "hash", "gitea-delivery", "Gitea Delivery")
	if err != nil {
		t.Fatalf("gitea integration: seed user: %v", err)
	}
	project, _, err := persistence.NewProjectStore(pool).CreateProject(ctx, domain.Project{
		Slug:            "gitea-delivery",
		Name:            "Gitea Delivery",
		Purpose:         "T0301 webhook delivery",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, user.ID)
	if err != nil {
		t.Fatalf("gitea integration: create project: %v", err)
	}

	// The receiver binds an ephemeral host port; the delivery URL keeps the
	// operator-provided host (the address the Gitea container dials) and
	// swaps in our port.
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("gitea integration: listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	recorder := &deliveryRecorder{}
	srv := &http.Server{Handler: recorder}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
	})
	path := u.Path
	if path == "" {
		path = "/"
	}
	webhookURL := fmt.Sprintf("%s://%s:%d%s", u.Scheme, u.Hostname(), port, path)

	cfg := gitprovider.Config{
		BaseURL:    base,
		Token:      config.Secret(token),
		WebhookURL: webhookURL,
	}
	store := gitprovider.NewProvisionStore(pool)
	provisioner := gitprovider.NewProvisioner(gitprovider.NewGiteaAdapter(cfg), store, webhookURL)
	if err := provisioner.Provision(ctx, project.ID); err != nil {
		t.Fatalf("gitea integration: Provision: %v", err)
	}
	adapter := gitprovider.NewGiteaAdapter(cfg)
	owner, err := adapter.Owner(ctx)
	if err != nil {
		t.Fatalf("gitea integration: Owner: %v", err)
	}
	name := gitprovider.RepositoryName(project.ID)
	t.Cleanup(func() { deleteGiteaRepo(t, base, token, owner, name) })

	var secret string
	if err := pool.QueryRow(ctx,
		`SELECT webhook_secret FROM git_repository_provisions WHERE project_id = $1`,
		project.ID).Scan(&secret); err != nil {
		t.Fatalf("gitea integration: read stored secret: %v", err)
	}

	// A real push fires the delivery (the token rides the Authorization
	// header via http.extraHeader, handed to git in the environment —
	// gitAuthEnv — never a URL and never argv).
	pushToRepo(t, base, token, owner, name)

	delivered := recorder.waitPush(t, 10*time.Second)
	if delivered == nil {
		t.Fatalf("gitea integration: no push delivery within 10s (deliveries seen: %d)", len(recorder.seen))
	}

	sig := delivered.headers.Get("X-Gitea-Signature")
	if sig == "" {
		t.Fatal("gitea integration: delivery has no X-Gitea-Signature header")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(delivered.body)
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(want)) {
		t.Errorf("gitea integration: X-Gitea-Signature = %s, want %s (hex hmac-sha256 of the raw body with the stored secret)", sig, want)
	} else {
		t.Logf("gitea integration: delivered X-Gitea-Signature verifies against the stored secret (repo %s/%s)", owner, name)
	}
}

// pushToRepo pushes one commit through the provider's own Git transport
// (the exact path future product pushes take), authenticating with the
// service account token via an extra header carried in the process
// environment (gitAuthEnv), so the credential lands in neither a URL nor
// the argument list. The commit goes to a NON-main branch:
// T0302 protects main from direct pushes (the delivery trigger is the
// push event, not the branch).
func pushToRepo(t *testing.T, base, token, owner, name string) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "README.md")
	if err := os.WriteFile(file, []byte("# "+name+"\n"), 0o644); err != nil {
		t.Fatalf("gitea integration: write push file: %v", err)
	}
	remoteURL := base + "/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + ".git"
	env := append(os.Environ(), gitAuthEnv("Authorization: token "+token)...)

	run := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", dir}, args...)
		cmd := exec.Command("git", full...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gitea integration: git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "gitea-integration@example.com")
	run("config", "user.name", "Gitea Integration")
	run("add", ".")
	run("commit", "-m", "T0301 webhook delivery round-trip")
	run("push", remoteURL, "main:refs/heads/gitea-delivery")
}
