package integration

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/config"
	"github.com/lichman0405/post/internal/gitprovider"
)

// The G1 real-services gate for T0302 (gitea protection): main's double
// protection exercised against a LIVE Gitea instance and real PostgreSQL —
// the canonical rule the adapter ensures, the refusals the provider
// enforces for the owner AND the instance admin, the force-push and
// delete refusals, the merge whitelist as the single controlled write
// path, the bootstrap seed, and the sweeper's healing of a rule an
// operator removed. CI has no Gitea, so every test here skips loudly when
// the instance is unreachable; the instance-level coverage is also the
// Supervisor's G3 gate (gates.json task_overrides gitea-real-services →
// tests/acceptance/gitea-real-services-e2e.sh).

// giteaAdmin returns the instance-admin credentials for the human-identity
// probes (dev defaults from infra/docker/gitea/init-gitea.sh). The admin
// is the strongest identity on the instance: a refusal that holds for the
// admin holds for the owner and every user.
func giteaAdmin() (user, pass string) {
	return envDefault("GITEA_ADMIN_USER", "postadmin"), envDefault("GITEA_ADMIN_PASSWORD", "postadmin_dev_pw")
}

// giteaCallBasic performs one authenticated provider call with basic auth
// (the admin identity; the token path is giteaCall).
func giteaCallBasic(t *testing.T, method, base, user, pass, path, body string) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		t.Fatalf("gitea integration: build %s %s: %v", method, path, err)
	}
	req.SetBasicAuth(user, pass)
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

// mainHead returns main's head SHA, or "" when main has no ref.
func mainHead(t *testing.T, base, token, owner, name string) string {
	t.Helper()
	code, raw := giteaCall(t, http.MethodGet, base, token,
		"/api/v1/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+"/git/refs/heads/main")
	if code == http.StatusNotFound {
		return ""
	}
	if code != http.StatusOK {
		t.Fatalf("gitea integration: read main ref = %d (body %s)", code, raw)
	}
	var refs []struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(raw, &refs); err != nil {
		t.Fatalf("gitea integration: decode main ref: %v", err)
	}
	if len(refs) == 0 {
		return ""
	}
	return refs[0].Object.SHA
}

// scratchGit initialises a scratch repository (t.TempDir, never the tree
// under test) with one root commit on main — the shape used where main
// must NOT exist yet on the provider.
func scratchGit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", dir}, args...)
		out, err := exec.Command("git", full...).CombinedOutput()
		if err != nil {
			t.Fatalf("gitea integration: git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "gitea-protection@example.com")
	run("config", "user.name", "Gitea Protection")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("scratch\n"), 0o644); err != nil {
		t.Fatalf("gitea integration: write scratch file: %v", err)
	}
	run("add", ".")
	run("commit", "-m", "scratch probe commit")
	return dir
}

// researchDir fetches main and builds one appended-line commit on top of
// it in a scratch repository (never the tree under test). Branching from
// main's HEAD matters: a push of this commit to main is fast-forward, so
// the provider reaches its protection check and the refusal names
// protection — an unrelated root would be refused "fetch first" before
// protection is consulted, and that refusal proves nothing.
func researchDir(t *testing.T, base, token, owner, name, branch, line string) string {
	t.Helper()
	dir := t.TempDir()
	authHeader := "Authorization: token " + token
	remoteURL := base + "/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + ".git"
	run := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", dir, "-c", "http.extraHeader=" + authHeader}, args...)
		out, err := exec.Command("git", full...).CombinedOutput()
		if err != nil {
			t.Fatalf("gitea integration: git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-b", "scratch")
	run("config", "user.email", "gitea-protection@example.com")
	run("config", "user.name", "Gitea Protection")
	run("fetch", remoteURL, "main")
	run("checkout", "-b", branch, "FETCH_HEAD")
	f, err := os.OpenFile(filepath.Join(dir, "README.md"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("gitea integration: open README for append: %v", err)
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatalf("gitea integration: append README: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("gitea integration: close README: %v", err)
	}
	run("add", ".")
	run("commit", "-m", "research probe: "+line)
	return dir
}

// pushAttempt pushes from dir's scratch repository with an Authorization
// header (the credential never lands in a URL) and returns the exit code
// and combined output. It never fails the test — refusals are the point.
func pushAttempt(t *testing.T, dir, base, owner, name, authHeader string, args ...string) (int, string) {
	t.Helper()
	remoteURL := base + "/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + ".git"
	full := append([]string{"-C", dir, "-c", "http.extraHeader=" + authHeader, "push", remoteURL}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	rc := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			rc = ee.ExitCode()
		} else {
			t.Fatalf("gitea integration: git push: %v\n%s", err, out)
		}
	}
	return rc, string(out)
}

// expectMainRefusal executes a push to main and asserts the INSTANCE
// refused it BY PROTECTION with main unmoved: a non-zero exit alone is not
// the property (a broken probe also fails), and the refusal must name
// protection (a wrong credential must not pass as a refusal).
func expectMainRefusal(t *testing.T, base, token, owner, name, before, dir, authHeader string, args ...string) {
	t.Helper()
	rc, out := pushAttempt(t, dir, base, owner, name, authHeader, args...)
	after := mainHead(t, base, token, owner, name)
	if rc == 0 {
		t.Errorf("gitea integration: push %v to protected main reported success — protection is configured but not enforced", args)
	}
	if !regexp.MustCompile(`(?i)protected|pre-receive hook declined`).MatchString(out) {
		t.Errorf("gitea integration: push %v refused for a reason that is not protection: %s",
			args, strings.Join(strings.Fields(out), " "))
	}
	if after != before {
		t.Errorf("gitea integration: push %v moved main %s -> %s", args, before, after)
	}
}

// pushResearchBranch fetches main and pushes one appended-line commit to a
// research branch (the path every product research-branch push takes).
func pushResearchBranch(t *testing.T, base, token, owner, name, branch, line string) {
	t.Helper()
	dir := researchDir(t, base, token, owner, name, branch, line)
	authHeader := "Authorization: token " + token
	remoteURL := base + "/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + ".git"
	cmd := exec.Command("git", "-C", dir, "-c", "http.extraHeader="+authHeader,
		"push", remoteURL, "HEAD:refs/heads/"+branch)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gitea integration: git push %s: %v\n%s", branch, err, out)
	}
}

// createPR opens a pull request head -> main and returns its number.
func createPR(t *testing.T, base, token, owner, name, head string) int64 {
	t.Helper()
	code, raw := giteaCallBody(t, http.MethodPost, base, token,
		"/api/v1/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+"/pulls",
		fmt.Sprintf(`{"title":"probe","head":%q,"base":"main"}`, head))
	if code != http.StatusCreated {
		t.Fatalf("gitea integration: create PR = %d (body %s)", code, raw)
	}
	var pr struct {
		Number int64 `json:"number"`
	}
	if err := json.Unmarshal(raw, &pr); err != nil {
		t.Fatalf("gitea integration: decode PR: %v", err)
	}
	return pr.Number
}

// mainREADME reads main's README.md (the bootstrap file) as text.
func mainREADME(t *testing.T, base, token, owner, name string) string {
	t.Helper()
	code, raw := giteaCall(t, http.MethodGet, base, token,
		"/api/v1/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+"/contents/README.md?ref=main")
	if code != http.StatusOK {
		t.Fatalf("gitea integration: read main README = %d (body %s)", code, raw)
	}
	var file struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("gitea integration: decode README: %v", err)
	}
	if file.Encoding != "base64" {
		t.Fatalf("gitea integration: README encoding = %q, want base64", file.Encoding)
	}
	dec, err := base64.StdEncoding.DecodeString(file.Content)
	if err != nil {
		t.Fatalf("gitea integration: decode README content: %v", err)
	}
	return string(dec)
}

// requireGit skips when the git CLI is unavailable (the push probes run
// the real transport).
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("gitea integration: git CLI not available for the push probes: %v", err)
	}
}

// ---- the acceptance criteria ----------------------------------------------

// TestGiteaProtectionEndToEnd: acceptance criterion (a) — no identity,
// service account or instance admin, can direct-push main (the refusals
// hold and main never moves; force pushes and ref deletion are refused
// too) — and criterion (b) — the merge service's PR merge is the one
// controlled write path (a non-whitelisted admin merge is refused and main
// stays put, the whitelisted merge moves it).
func TestGiteaProtectionEndToEnd(t *testing.T) {
	ctx := testCtx(t)
	requireGit(t)
	fx := newGiteaFixture(t, ctx)

	if err := fx.provisioner().Provision(ctx, fx.project.ID); err != nil {
		t.Fatalf("gitea integration: Provision: %v", err)
	}
	adapter := gitprovider.NewGiteaAdapter(fx.cfg)
	owner, err := adapter.Owner(ctx)
	if err != nil {
		t.Fatalf("gitea integration: Owner: %v", err)
	}
	name := gitprovider.RepositoryName(fx.project.ID)
	repo := gitprovider.Repository{Owner: owner, Name: name}
	t.Cleanup(func() { deleteGiteaRepo(t, fx.base, fx.token, owner, name) })

	// ---- The rule the provisioning applied is canonical, and it is the
	// only rule the platform owns on main.
	prot, err := adapter.GetMainProtection(ctx, repo)
	if err != nil {
		t.Fatalf("gitea integration: GetMainProtection: %v", err)
	}
	if !prot.Canonical([]string{owner}) {
		t.Errorf("gitea integration: rule = %+v, want canonical (direct+force blocked, merge whitelist [%s], no bypass)", prot, owner)
	}
	code, raw := giteaCall(t, http.MethodGet, fx.base, fx.token,
		"/api/v1/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+"/branch_protections")
	if code != http.StatusOK {
		t.Fatalf("gitea integration: list rules = %d (body %s)", code, raw)
	}
	var rules []struct {
		RuleName   string `json:"rule_name"`
		BranchName string `json:"branch_name"`
	}
	if err := json.Unmarshal(raw, &rules); err != nil {
		t.Fatalf("gitea integration: decode rules: %v", err)
	}
	platformRules := 0
	for _, r := range rules {
		if r.RuleName == "main" && r.BranchName == "main" {
			platformRules++
		}
	}
	if platformRules != 1 {
		t.Errorf("gitea integration: platform rules on main = %d, want exactly 1 (no stacking)", platformRules)
	}

	before := mainHead(t, fx.base, fx.token, owner, name)
	if before == "" {
		t.Fatal("gitea integration: main has no ref after provisioning — the bootstrap seed is missing")
	}
	// The refusal probes branch from main's HEAD: the push is fast-forward,
	// so the provider reaches its protection check and the refusal names
	// protection (an unrelated root would be refused "fetch first" before
	// protection is consulted).
	dir := researchDir(t, fx.base, fx.token, owner, name, "probe", "should not land")
	svcAuth := "Authorization: token " + fx.token
	adminUser, adminPass := giteaAdmin()
	adminAuth := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(adminUser+":"+adminPass))

	// ---- (a) direct pushes: the service account (owner) and the instance
	// admin are both refused by name, and main never moves.
	expectMainRefusal(t, fx.base, fx.token, owner, name, before, dir, svcAuth, "HEAD:main")
	expectMainRefusal(t, fx.base, fx.token, owner, name, before, dir, adminAuth, "HEAD:main")

	// ---- (a) force pushes are refused the same way (a rewritten history).
	if out, err := exec.Command("git", "-C", dir, "commit", "--amend", "-m", "rewritten probe commit").CombinedOutput(); err != nil {
		t.Fatalf("gitea integration: amend probe commit: %v\n%s", err, out)
	}
	expectMainRefusal(t, fx.base, fx.token, owner, name, before, dir, svcAuth, "--force", "HEAD:main")

	// ---- (a) deleting main is refused; the default branch cannot be
	// removed from under a protected main.
	rc, out := pushAttempt(t, dir, fx.base, owner, name, svcAuth, ":main")
	if rc == 0 {
		t.Error("gitea integration: deleting main reported success — the default branch must be undeletable")
	}
	if !regexp.MustCompile(`(?i)denied|protected|pre-receive|delete`).MatchString(out) {
		t.Errorf("gitea integration: delete-main refusal is unexplained: %s", strings.Join(strings.Fields(out), " "))
	}
	if after := mainHead(t, fx.base, fx.token, owner, name); after != before {
		t.Errorf("gitea integration: the refused deletion moved main %s -> %s", before, after)
	}

	// ---- Research branches stay pushable (their lifecycle is untouched).
	pushResearchBranch(t, fx.base, fx.token, owner, name, "gitea-prot-r1", "research line one")

	// ---- (b) the merge whitelist: the admin (not whitelisted) cannot
	// merge — the instance refuses with "User not allowed to merge PR"
	// (403 or 405, both observed on 1.27.3) — and the refused merge leaves
	// main exactly where it was.
	pr := createPR(t, fx.base, fx.token, owner, name, "gitea-prot-r1")
	mergePath := fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d/merge", url.PathEscape(owner), url.PathEscape(name), pr)
	code, raw = giteaCallBasic(t, http.MethodPost, fx.base, adminUser, adminPass, mergePath, `{"Do":"merge"}`)
	if code != http.StatusForbidden && code != http.StatusMethodNotAllowed {
		t.Errorf("gitea integration: admin merge = %d (body %s), want 403/405 — the merge whitelist must exclude every human identity", code, raw)
	}
	if !strings.Contains(string(raw), "not allowed to merge") {
		t.Errorf("gitea integration: admin merge refusal does not name the whitelist: %s", raw)
	}
	if after := mainHead(t, fx.base, fx.token, owner, name); after != before {
		t.Errorf("gitea integration: the refused admin merge moved main %s -> %s", before, after)
	}

	// ---- (b) the whitelisted merge service merges, and main moves.
	code, raw = giteaCallBody(t, http.MethodPost, fx.base, fx.token, mergePath, `{"Do":"merge"}`)
	if code != http.StatusOK {
		t.Fatalf("gitea integration: service merge = %d (body %s), want 200 — the controlled write path must work", code, raw)
	}
	if after := mainHead(t, fx.base, fx.token, owner, name); after == before {
		t.Error("gitea integration: the service merge reported success but main did not move")
	}
	if readme := mainREADME(t, fx.base, fx.token, owner, name); !strings.Contains(readme, "research line one") {
		t.Errorf("gitea integration: main README after merge = %q, want it to contain the research line", readme)
	}

	// ---- Redelivery stays idempotent: the skip path never re-applies or
	// duplicates the rule.
	if err := fx.provisioner().Provision(ctx, fx.project.ID); err != nil {
		t.Fatalf("gitea integration: redelivered Provision: %v", err)
	}
	code, raw = giteaCall(t, http.MethodGet, fx.base, fx.token,
		"/api/v1/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+"/branch_protections")
	if err := json.Unmarshal(raw, &rules); code != http.StatusOK || err != nil {
		t.Fatalf("gitea integration: re-list rules = %d (%v)", code, err)
	}
	platformRules = 0
	for _, r := range rules {
		if r.RuleName == "main" && r.BranchName == "main" {
			platformRules++
		}
	}
	if platformRules != 1 {
		t.Errorf("gitea integration: platform rules after redelivery = %d, want still 1", platformRules)
	}
}

// TestGiteaProtectionConvergesAndSweeperHeals: the platform layer of the
// double protection — a rule an operator weakened is converged back, a
// rule an operator deleted is re-created by the sweep, and both paths are
// idempotent.
func TestGiteaProtectionConvergesAndSweeperHeals(t *testing.T) {
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
	name := gitprovider.RepositoryName(fx.project.ID)
	repo := gitprovider.Repository{Owner: owner, Name: name}
	t.Cleanup(func() { deleteGiteaRepo(t, fx.base, fx.token, owner, name) })
	rulesPath := "/api/v1/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/branch_protections"

	// Drift: the operator opens merges to everyone with write access.
	code, raw := giteaCallBody(t, http.MethodPatch, fx.base, fx.token, rulesPath+"/main",
		`{"enable_merge_whitelist":false,"merge_whitelist_usernames":[]}`)
	if code != http.StatusOK {
		t.Fatalf("gitea integration: drift PATCH = %d (body %s)", code, raw)
	}
	if _, err := adapter.EnsureMainProtection(ctx, repo, gitprovider.MainProtectionSpec{}); err != nil {
		t.Fatalf("gitea integration: converge: %v", err)
	}
	prot, err := adapter.GetMainProtection(ctx, repo)
	if err != nil {
		t.Fatalf("gitea integration: GetMainProtection after converge: %v", err)
	}
	if !prot.Canonical([]string{owner}) {
		t.Errorf("gitea integration: rule after converge = %+v, want canonical again", prot)
	}

	// The operator deletes the rule outright — the sweep must re-create it.
	code, raw = giteaCall(t, http.MethodDelete, fx.base, fx.token, rulesPath+"/main")
	if code != http.StatusNoContent && code != http.StatusOK {
		t.Fatalf("gitea integration: delete rule = %d (body %s)", code, raw)
	}
	if _, err := adapter.GetMainProtection(ctx, repo); !errors.Is(err, gitprovider.ErrNotFound) {
		t.Fatalf("gitea integration: GetMainProtection after delete = %v, want ErrNotFound", err)
	}
	sweeper := gitprovider.NewProtectionSweeper(adapter, gitprovider.NewProvisionStore(fx.pool))
	ensured, failures, err := sweeper.Sweep(ctx)
	if err != nil {
		t.Fatalf("gitea integration: Sweep: %v", err)
	}
	if len(failures) != 0 {
		t.Fatalf("gitea integration: Sweep failures = %v, want none", failures)
	}
	if ensured < 1 {
		t.Errorf("gitea integration: Sweep ensured = %d, want >= 1 (the provisioned repository)", ensured)
	}
	prot, err = adapter.GetMainProtection(ctx, repo)
	if err != nil {
		t.Fatalf("gitea integration: GetMainProtection after sweep: %v", err)
	}
	if !prot.Canonical([]string{owner}) {
		t.Errorf("gitea integration: rule after sweep = %+v, want canonical again (the sweep healed the deletion)", prot)
	}

	// Idempotency: the healed rule costs one read and no rewrites.
	if _, failures, err := sweeper.Sweep(ctx); err != nil || len(failures) != 0 {
		t.Fatalf("gitea integration: second Sweep: err %v failures %v, want clean", err, failures)
	}
	prot, err = adapter.GetMainProtection(ctx, repo)
	if err != nil || !prot.Canonical([]string{owner}) {
		t.Fatalf("gitea integration: rule after second sweep = %+v (err %v), want still canonical", prot, err)
	}
}

// TestGiteaProtectionBlocksFirstPushAndBootstraps: the rule blocks main's
// very first push, the provider refuses to create a PR targeting a main
// that does not exist, and even the contents API cannot create main under
// the rule — the three walls that make a protected-but-empty main a
// permanent deadlock. The bootstrap seed is the exit: seeded first (the
// production order — the provisioner seeds BEFORE it protects), the rule
// then freezes main, and the first PR merge brings the first research
// commit onto it.
func TestGiteaProtectionBlocksFirstPushAndBootstraps(t *testing.T) {
	ctx := testCtx(t)
	base := requireGitea(t)
	requireGit(t)
	token := giteaServiceToken(t, base)
	adapter := gitprovider.NewGiteaAdapter(gitprovider.Config{
		BaseURL: base,
		Token:   config.Secret(token),
	})
	owner, err := adapter.Owner(ctx)
	if err != nil {
		t.Fatalf("gitea integration: Owner: %v", err)
	}
	name := "p-protect-" + fxSuffix()
	code, raw := giteaCallBody(t, http.MethodPost, base, token, "/api/v1/user/repos",
		fmt.Sprintf(`{"name":%q,"auto_init":false,"private":true}`, name))
	if code != http.StatusCreated {
		t.Fatalf("gitea integration: create repo = %d (body %s)", code, raw)
	}
	repo := gitprovider.Repository{Owner: owner, Name: name}
	t.Cleanup(func() { deleteGiteaRepo(t, base, token, owner, name) })
	repoPath := "/api/v1/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	svcAuth := "Authorization: token " + token

	// The rule exists before any ref: it must hold from the first push on.
	if _, err := adapter.EnsureMainProtection(ctx, repo, gitprovider.MainProtectionSpec{}); err != nil {
		t.Fatalf("gitea integration: EnsureMainProtection on empty repo: %v", err)
	}
	if got := mainHead(t, base, token, owner, name); got != "" {
		t.Fatalf("gitea integration: main already exists (%s) — the fixture must start empty", got)
	}
	dir := scratchGit(t)
	rc, out := pushAttempt(t, dir, base, owner, name, svcAuth, "HEAD:main")
	if rc == 0 {
		t.Fatal("gitea integration: the very first push to a protected main succeeded — main must be uncreatable through Git")
	}
	if !regexp.MustCompile(`(?i)protected|pre-receive hook declined`).MatchString(out) {
		t.Errorf("gitea integration: first-push refusal does not name protection: %s", strings.Join(strings.Fields(out), " "))
	}
	if got := mainHead(t, base, token, owner, name); got != "" {
		t.Errorf("gitea integration: the refused first push created main (%s)", got)
	}

	// The deadlock is total: with main absent a PR cannot even be created,
	// and the contents API write that could seed main is refused by the
	// rule too. Nothing can bring an unseeded protected main into
	// existence — which is why the seed must precede the rule.
	if rc, _ := pushAttempt(t, dir, base, owner, name, svcAuth, "HEAD:refs/heads/r0"); rc != 0 {
		t.Fatal("gitea integration: the research-branch push failed — the PR probe needs it")
	}
	code, raw = giteaCallBody(t, http.MethodPost, base, token, repoPath+"/pulls",
		`{"title":"probe","head":"r0","base":"main"}`)
	if code != http.StatusNotFound {
		t.Errorf("gitea integration: PR targeting a nonexistent main = %d (body %s), want 404 — the provider must refuse it", code, raw)
	}
	code, raw = giteaCallBody(t, http.MethodPost, base, token, repoPath+"/contents/README.md",
		`{"content":"c2VlZA==","message":"seed attempt","branch":"main"}`)
	if code != http.StatusForbidden {
		t.Errorf("gitea integration: contents write under the rule = %d (body %s), want 403 — even the API write must not create main", code, raw)
	}
	if got := mainHead(t, base, token, owner, name); got != "" {
		t.Errorf("gitea integration: a refused write created main (%s)", got)
	}

	// The bootstrap: the rule is removed for the seed (exactly the order
	// the provisioner produces — seed first, protect after), the seed
	// lands authored by the service identity, and the rule goes back on.
	code, raw = giteaCall(t, http.MethodDelete, base, token, repoPath+"/branch_protections/main")
	if code != http.StatusNoContent && code != http.StatusOK {
		t.Fatalf("gitea integration: remove rule for the seed = %d (body %s)", code, raw)
	}
	sha, err := adapter.EnsureInitialMain(ctx, repo)
	if err != nil {
		t.Fatalf("gitea integration: EnsureInitialMain: %v", err)
	}
	if sha == "" || mainHead(t, base, token, owner, name) != sha {
		t.Errorf("gitea integration: main after seed = %q (head %q), want the returned SHA", mainHead(t, base, token, owner, name), sha)
	}
	if _, err := adapter.EnsureMainProtection(ctx, repo, gitprovider.MainProtectionSpec{}); err != nil {
		t.Fatalf("gitea integration: re-apply rule after seed: %v", err)
	}

	// The first PR against the seeded main merges — the controlled write
	// path works on a fresh repository, and main is still protected after.
	pushResearchBranch(t, base, token, owner, name, "gitea-prot-r2", "first research line")
	pr := createPR(t, base, token, owner, name, "gitea-prot-r2")
	mergePath := repoPath + fmt.Sprintf("/pulls/%d/merge", pr)
	code, raw = giteaCallBody(t, http.MethodPost, base, token, mergePath, `{"Do":"merge"}`)
	if code != http.StatusOK {
		t.Fatalf("gitea integration: first PR merge = %d (body %s), want 200", code, raw)
	}
	if readme := mainREADME(t, base, token, owner, name); !strings.Contains(readme, "first research line") {
		t.Errorf("gitea integration: main README after first merge = %q, want it to contain the research line", readme)
	}
	if prot, err := adapter.GetMainProtection(ctx, repo); err != nil || !prot.Canonical([]string{owner}) {
		t.Errorf("gitea integration: rule after first merge = %+v (err %v), want still canonical", prot, err)
	}
}

// fxSuffix gives a time-based unique suffix for repository names created
// outside the provisioned naming scheme.
func fxSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
