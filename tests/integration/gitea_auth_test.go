package integration

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/config"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/gitprovider"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

// The git auth integration gate for T0304 (gitea integration): the
// acceptance criteria against a LIVE Gitea instance and real PostgreSQL —
//
//   - 无权限用户 clone private 失败: a user without a grant cannot clone
//     the private repository, credentials or none;
//   - token revoke 生效: a revoked token stops working immediately (the
//     provider delete is the enforcement), and the canonical row flips;
//   - the scoped credential: read tokens clone but cannot push, write
//     tokens push;
//   - access revoke cascades: every token dies with the grant.
//
// CI has no Gitea, so the test skips loudly when the instance is
// unreachable; the real-instance run is also the Supervisor's G3 gate
// (gates.json task_overrides gitea-real-services).

const giteaAuthTaskID = "T0304"

// giteaAdminPair returns the instance admin's credentials for the T0304
// config keys, falling back to the provisioning suite's key names and the
// init-gitea.sh dev defaults.
func giteaAdminPair() (string, string) {
	return envDefault("POST_GITEA_ADMIN_USER", envDefault("GITEA_ADMIN_USER", "postadmin")),
		envDefault("POST_GITEA_ADMIN_PASSWORD", envDefault("GITEA_ADMIN_PASSWORD", "postadmin_dev_pw"))
}

// gitRun runs one git command in dir and returns the combined output and
// error (never t.Fatal — the failure is what some assertions test).
func gitRun(t *testing.T, dir string, args ...string) ([]byte, error) {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "credential.helper="}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	return cmd.CombinedOutput()
}

// gitMust runs one git command that must succeed.
func gitMust(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	out, err := gitRun(t, dir, args...)
	if err != nil {
		t.Fatalf("gitea integration: git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// gitClone tries to clone remoteURL into a fresh directory; it reports
// success/failure instead of failing the test (the failure IS the
// assertion for unauthorized clones).
func gitClone(t *testing.T, remoteURL string) error {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	_, err := gitRun(t, filepath.Dir(dir), "clone", remoteURL, "repo")
	return err
}

// gitPush pushes one commit to origin from a freshly cloned repository.
// The target is a research branch: on the merged baseline direct pushes to
// main are refused for EVERY identity by the canonical protection rule
// (T0302, internal/gitprovider/mainprotection.go — enable_push=false, no
// bypass), so the scope probes must target the branch that IS the legal
// push path for a collaborator.
func gitPush(t *testing.T, repoDir, branch, message string) error {
	t.Helper()
	gitMust(t, repoDir, "config", "user.email", "git-auth@example.com")
	gitMust(t, repoDir, "config", "user.name", "Git Auth Integration")
	file := filepath.Join(repoDir, "proof.txt")
	if err := os.WriteFile(file, []byte(message+"\n"), 0o644); err != nil {
		t.Fatalf("gitea integration: write push file: %v", err)
	}
	gitMust(t, repoDir, "add", ".")
	gitMust(t, repoDir, "commit", "-m", message)
	_, err := gitRun(t, repoDir, "push", "origin", "main:refs/heads/"+branch)
	return err
}

// injectUserinfo builds a clone URL carrying the given credential as
// basic-auth userinfo (the same form the service issues).
func injectUserinfo(t *testing.T, raw, username, password string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("gitea integration: parse clone URL: %v", err)
	}
	u.User = url.UserPassword(username, password)
	return u.String()
}

// deleteGiteaUser removes one shadow account best-effort (admin basic
// auth). Test users are run-specific; a leftover would be inert but the
// instance must stay clean (docs/66 §3).
func deleteGiteaUser(base, adminUser, adminPass, username string) {
	req, err := http.NewRequest(http.MethodDelete, base+"/api/v1/admin/users/"+url.PathEscape(username), nil)
	if err != nil {
		return
	}
	req.SetBasicAuth(adminUser, adminPass)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// giteaAuthFixture provisions the two projects the acceptance flow needs:
// the member owns P1, the outsider owns P2 and holds no access to P1.
type giteaAuthFixture struct {
	pool      *pgxpool.Pool
	base      string
	svcToken  string
	adminUser string
	adminPass string
	owner     string // service account login (repo owner)
	member    domain.User
	outsider  domain.User
	project   domain.Project // P1, the private repo under test
	otherProj domain.Project // P2, the outsider's own project
	service   *gitprovider.UserAccess
	store     *gitprovider.PGUserAccessStore
	repoOwner string
	repoName  string
}

func newGiteaAuthFixture(t *testing.T, ctx context.Context) *giteaAuthFixture {
	t.Helper()
	base := requireGitea(t)
	svcToken := giteaServiceToken(t, base)
	adminUser, adminPass := giteaAdminPair()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), giteaAuthTaskID)

	cfg := gitprovider.Config{
		BaseURL:       base,
		Token:         config.Secret(svcToken),
		WebhookURL:    "http://host.invalid/api/v1/git/hooks/gitea",
		AdminUser:     adminUser,
		AdminPassword: config.Secret(adminPass),
	}

	users := persistence.NewCredentialStore(pool)
	member, err := users.CreateWithPassword(ctx, "git-auth-member@example.com", "hash", "git-auth-member", "Git Auth Member")
	if err != nil {
		t.Fatalf("gitea integration: seed member: %v", err)
	}
	outsider, err := users.CreateWithPassword(ctx, "git-auth-outsider@example.com", "hash", "git-auth-outsider", "Git Auth Outsider")
	if err != nil {
		t.Fatalf("gitea integration: seed outsider: %v", err)
	}
	projects := persistence.NewProjectStore(pool)
	project, _, err := projects.CreateProject(ctx, domain.Project{
		Slug:            "git-auth-e2e",
		Name:            "Git Auth E2E",
		Purpose:         "T0304 integration",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, member.ID)
	if err != nil {
		t.Fatalf("gitea integration: create P1: %v", err)
	}
	otherProj, _, err := projects.CreateProject(ctx, domain.Project{
		Slug:            "git-auth-other",
		Name:            "Git Auth Other",
		Purpose:         "T0304 outsider project",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, outsider.ID)
	if err != nil {
		t.Fatalf("gitea integration: create P2: %v", err)
	}

	provisioner := gitprovider.NewProvisioner(gitprovider.NewGiteaAdapter(cfg),
		gitprovider.NewProvisionStore(pool), cfg.WebhookURL)
	if err := provisioner.Provision(ctx, project.ID); err != nil {
		t.Fatalf("gitea integration: provision P1: %v", err)
	}
	if err := provisioner.Provision(ctx, otherProj.ID); err != nil {
		t.Fatalf("gitea integration: provision P2: %v", err)
	}

	adapter := gitprovider.NewGiteaAdapter(cfg)
	owner, err := adapter.Owner(ctx)
	if err != nil {
		t.Fatalf("gitea integration: Owner: %v", err)
	}
	repoName := gitprovider.RepositoryName(project.ID)
	otherName := gitprovider.RepositoryName(otherProj.ID)
	t.Cleanup(func() {
		deleteGiteaRepo(t, base, svcToken, owner, repoName)
		deleteGiteaRepo(t, base, svcToken, owner, otherName)
		deleteGiteaUser(base, adminUser, adminPass, "u-"+member.ID)
		deleteGiteaUser(base, adminUser, adminPass, "u-"+outsider.ID)
	})

	return &giteaAuthFixture{
		pool: pool, base: base, svcToken: svcToken, adminUser: adminUser, adminPass: adminPass,
		owner: owner, member: member, outsider: outsider, project: project, otherProj: otherProj,
		service: gitprovider.NewUserAccess(
			gitprovider.NewGiteaUserAccess(cfg),
			gitprovider.NewUserAccessStore(pool),
			base),
		store:     gitprovider.NewUserAccessStore(pool),
		repoOwner: owner,
		repoName:  repoName,
	}
}

// TestGitAuthIntegration is the T0304 acceptance test: scoped tokens,
// the repo access mapping, and revocation — verified with real git
// clients against the live provider transport.
func TestGitAuthIntegration(t *testing.T) {
	ctx := testCtx(t)
	fx := newGiteaAuthFixture(t, ctx)

	// ---- Phase 1: a member issues a WRITE token; clone + push work.
	writeTok, err := fx.service.IssueToken(ctx, fx.member.ID, fx.project.ID, gitprovider.AccessWrite)
	if err != nil {
		t.Fatalf("gitea integration: issue write token: %v", err)
	}
	if writeTok.Value == "" || writeTok.CloneURL == "" {
		t.Fatalf("gitea integration: issued token has no value or clone URL: %+v", writeTok.Record)
	}
	if err := gitClone(t, writeTok.CloneURL); err != nil {
		t.Fatalf("gitea integration: member clone with write token failed: %v", err)
	}
	// The canonical mapping row exists with the write permission.
	acc, err := fx.store.GetAccess(ctx, fx.project.ID, fx.member.ID)
	if err != nil || acc.Permission != gitprovider.AccessWrite {
		t.Errorf("gitea integration: access row = %+v (err %v), want a write grant", acc, err)
	}

	// ---- Phase 2: 无权限用户 clone private 失败.
	// (a) No credentials at all: the private repo must refuse.
	noCredURL := fx.base + "/" + url.PathEscape(fx.repoOwner) + "/" + url.PathEscape(fx.repoName) + ".git"
	if err := gitClone(t, noCredURL); err == nil {
		t.Error("gitea integration: credential-less clone of the private repo succeeded — must fail")
	}
	// (b) The outsider has a VALID credential — issued by the platform for
	// their own project — but holds no grant on P1: the clone must still
	// fail (the access mapping is what governs reach, not token existence).
	otherTok, err := fx.service.IssueToken(ctx, fx.outsider.ID, fx.otherProj.ID, gitprovider.AccessWrite)
	if err != nil {
		t.Fatalf("gitea integration: issue outsider token: %v", err)
	}
	// Sanity: the outsider's own clone works — the credential itself is
	// good, so a failure on P1 is about permission, not a broken token.
	if err := gitClone(t, otherTok.CloneURL); err != nil {
		t.Fatalf("gitea integration: outsider clone of their own repo failed: %v", err)
	}
	foreignURL := fx.base + "/" + url.PathEscape(fx.repoOwner) + "/" + url.PathEscape(fx.repoName) + ".git"
	foreignURL = injectUserinfo(t, foreignURL, "u-"+fx.outsider.ID, otherTok.Value)
	if err := gitClone(t, foreignURL); err == nil {
		t.Error("gitea integration: a user without access cloned the private repo with a foreign token — must fail")
	}

	// ---- Phase 3: a WRITE token pushes; a READ token cannot (the scoped
	// credential requirement). Both probes target a research branch: main
	// is frozen by T0302's canonical protection rule on every provisioned
	// repository (direct push refused for all identities, tokens included),
	// so scope is measured on the branch that is the collaborator's legal
	// push path.
	cloneDir := filepath.Join(t.TempDir(), "write")
	gitMust(t, filepath.Dir(cloneDir), "clone", writeTok.CloneURL, "write")
	if err := gitPush(t, cloneDir, "t0304-write-probe", "T0304 write-token push"); err != nil {
		t.Fatalf("gitea integration: write token push failed: %v", err)
	}
	readTok, err := fx.service.IssueToken(ctx, fx.member.ID, fx.project.ID, gitprovider.AccessRead)
	if err != nil {
		t.Fatalf("gitea integration: issue read token: %v", err)
	}
	if err := gitClone(t, readTok.CloneURL); err != nil {
		t.Fatalf("gitea integration: read-token clone failed: %v", err)
	}
	readDir := filepath.Join(t.TempDir(), "read")
	gitMust(t, filepath.Dir(readDir), "clone", readTok.CloneURL, "read")
	if err := gitPush(t, readDir, "t0304-read-probe", "T0304 read-token push must fail"); err == nil {
		t.Error("gitea integration: a read-scoped token pushed — the scope is not enforced")
	}

	// ---- Phase 4: token revoke 生效 — the revoked token stops working
	// immediately and the canonical row flips.
	if err := fx.service.RevokeToken(ctx, fx.member.ID, fx.project.ID, readTok.Record.ID); err != nil {
		t.Fatalf("gitea integration: revoke read token: %v", err)
	}
	rec, err := fx.store.GetToken(ctx, readTok.Record.ID)
	if err != nil || rec.Status != "revoked" {
		t.Errorf("gitea integration: canonical row = %+v (err %v), want revoked", rec, err)
	}
	if err := gitClone(t, readTok.CloneURL); err == nil {
		t.Error("gitea integration: a revoked token still cloned — revocation is not enforced")
	}

	// ---- Phase 5: access revoke cascades — the collaborator grant dies
	// and with it every remaining token (the write token included). A
	// live grant existed (issued in phase 1), so the service must report
	// a real revocation.
	revoked, err := fx.service.RevokeAccess(ctx, fx.member.ID, fx.project.ID)
	if err != nil {
		t.Fatalf("gitea integration: revoke access: %v", err)
	}
	if !revoked {
		t.Error("gitea integration: revoke access reported false, want a real revocation (the grant existed)")
	}
	acc, err = fx.store.GetAccess(ctx, fx.project.ID, fx.member.ID)
	if err != nil || acc.RevokedAt == nil {
		t.Errorf("gitea integration: access row after revoke = %+v (err %v), want revoked_at set", acc, err)
	}
	if err := gitClone(t, writeTok.CloneURL); err == nil {
		t.Error("gitea integration: the write token survived an access revoke — must be dead")
	}
	tokens, err := fx.store.ListTokens(ctx, fx.project.ID, fx.member.ID)
	if err != nil {
		t.Fatalf("gitea integration: list tokens: %v", err)
	}
	for _, tok := range tokens {
		if tok.Status != "revoked" {
			t.Errorf("gitea integration: token %s still %s after access revoke", tok.ID, tok.Status)
		}
	}

	// ---- Phase 6: the outsider's own access is untouched by all of the
	// above — their token still clones P2.
	if err := gitClone(t, otherTok.CloneURL); err != nil {
		t.Errorf("gitea integration: the outsider's own access was damaged: %v", err)
	}
}
