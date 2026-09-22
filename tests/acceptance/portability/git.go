package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The Git half, driven by the real git CLI against the real GitProvider.
//
// Everything here follows the shape the repository already uses for
// authenticated git (internal/gitprovider/gitea.go's gitEnv,
// tests/acceptance/gitea-real-services-e2e.sh's git_authed,
// tests/acceptance/mof-canonical-workflow.sh's ls-remote): the credential
// rides in the environment as a git config override and NEVER in argv,
// because argv is readable by every account on the machine. The same rule
// applies to the error text: git echoes its environment into some failures,
// so every error this file returns passes through redact first.

// gitEnv is the environment one git invocation gets. HOME points at a
// scratch directory so no ambient ~/.gitconfig can change the result.
func gitEnv(token, home string) []string {
	return append(os.Environ(),
		"HOME="+home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: token "+token,
	)
}

// redact removes a credential from a string before it is reported.
func redact(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "***")
}

// runGit runs one git command in dir. Output is returned trimmed; a failure
// carries git's stderr with the token redacted.
func runGit(dir, token, home string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = gitEnv(token, home)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, redact(strings.TrimSpace(errb.String()), token))
	}
	return strings.TrimSpace(out.String()), nil
}

// bundleRefs reads every ref out of a git bundle. It fetches the bundle into
// a scratch bare repository rather than trusting the index's own record: the
// bundle is the artifact, and what it carries is what an importer would get.
func bundleRefs(bundlePath string) (map[string]string, error) {
	if _, err := os.Stat(bundlePath); err != nil {
		return nil, refuse(gitBundleF, "cannot read: %v", err)
	}
	home, err := workDir("gitrefs")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(home)
	dir := filepath.Join(home, "repo.git")
	if _, err := runGit(home, "", home, "init", "-q", "--bare", dir); err != nil {
		return nil, err
	}
	if _, err := runGit(dir, "", home, "fetch", "--force", "--no-tags", bundlePath, "+refs/*:refs/*"); err != nil {
		return nil, refuse(gitBundleF, "cannot be fetched as a git bundle: %v", err)
	}
	return localRefs(dir, home)
}

// localRefs lists refs/heads/* and refs/tags/* in a local repository.
func localRefs(dir, home string) (map[string]string, error) {
	out, err := runGit(dir, "", home, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads", "refs/tags")
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			refs[f[0]] = f[1]
		}
	}
	return refs, nil
}

// remoteRefs asks the provider itself what its refs hold. This is the read
// the repository's own gates use instead of trusting a push's exit status
// ("the ref is the provider's truth").
func remoteRefs(repoURL, token, home string) (map[string]string, error) {
	out, err := runGit(home, token, home, "ls-remote", repoURL)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.HasPrefix(f[1], "refs/") {
			refs[f[1]] = f[0]
		}
	}
	return refs, nil
}

// makeBundle clones a repository from the real provider (mirror, so every ref
// and the whole object graph come across) and writes a real git bundle of it.
//
// A mirror clone, not a working clone: the export must carry the repository's
// history as the provider holds it, including refs no checkout would create.
func makeBundle(repoURL, token, out, home string) (map[string]string, error) {
	mirror := filepath.Join(home, "mirror.git")
	if _, err := runGit(home, token, home, "clone", "-q", "--mirror", repoURL, mirror); err != nil {
		return nil, fmt.Errorf("clone %s: %w", repoURL, err)
	}
	if _, err := runGit(mirror, token, home, "bundle", "create", out, "--all"); err != nil {
		return nil, fmt.Errorf("git bundle create: %w", err)
	}
	return localRefs(mirror, home)
}

// pushBundleRefs pushes every ref a bundle carries into a destination
// repository, using the real git CLI over the real smart-HTTP endpoint.
func pushBundleRefs(bundlePath, repoURL, token, home string) error {
	dir := filepath.Join(home, "push.git")
	if _, err := runGit(home, "", home, "init", "-q", "--bare", dir); err != nil {
		return err
	}
	if _, err := runGit(dir, "", home, "fetch", "--force", "--no-tags", bundlePath, "+refs/*:refs/*"); err != nil {
		return fmt.Errorf("fetch the bundle: %w", err)
	}
	if _, err := runGit(dir, token, home, "push", "--force", repoURL, "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"); err != nil {
		return fmt.Errorf("push into %s: %w", repoURL, err)
	}
	return nil
}

// repoTokenSlug renders owner/name the way Gitea's REST paths want it.
func repoPath(owner, name string) string {
	return url.PathEscape(owner) + "/" + url.PathEscape(name)
}

// gitea is the thin REST client this driver needs: create a repository in a
// clean namespace, ask whether one exists, and delete what it created. It is
// deliberately not internal/gitprovider's adapter — that adapter is the
// product's provisioning path (and its write methods are unexported for good
// reason); what a test needs is the plain provider API the acceptance gates
// already call with curl.
type gitea struct {
	base  string
	token string
	http  *http.Client
}

func newGitea(base, token string) *gitea {
	return &gitea{base: strings.TrimRight(base, "/"), token: token, http: &http.Client{Timeout: 30 * time.Second}}
}

func (g *gitea) call(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "token "+g.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, raw, nil
}

// whoami returns the login the token belongs to. A repo created with
// POST /api/v1/user/repos lands under that login, so the export has to name
// it rather than assume it.
func (g *gitea) whoami(ctx context.Context) (string, error) {
	code, raw, err := g.call(ctx, http.MethodGet, "/api/v1/user", nil)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("gitea GET /api/v1/user: HTTP %d", code)
	}
	var u struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(raw, &u); err != nil {
		return "", err
	}
	if u.Login == "" {
		return "", fmt.Errorf("gitea GET /api/v1/user returned no login")
	}
	return u.Login, nil
}

func (g *gitea) repoExists(ctx context.Context, owner, name string) (bool, error) {
	code, _, err := g.call(ctx, http.MethodGet, "/api/v1/repos/"+repoPath(owner, name), nil)
	if err != nil {
		return false, err
	}
	switch code {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("gitea GET /api/v1/repos/%s/%s: HTTP %d", owner, name, code)
	}
}

// createRepo creates a repository for the import.
//
// owner == "" (or the token's own login) puts it in the token owner's
// namespace through Gitea's USER endpoint. Any other owner is a Gitea
// ORGANISATION, created if it does not exist, through the ORG endpoint — that
// is what makes the import side a genuinely separate namespace rather than a
// second repository beside the first; the dev stack has one Gitea instance,
// so a separate namespace is the strongest "clean environment" it can offer,
// and the RESULT says so. The two endpoints are not interchangeable: asking
// the org endpoint for a repository under a user's login makes Gitea answer
// "user already exists", which reads like a credential problem and is not
// one.
//
// auto_init is false on purpose: the target's history comes from the bundle,
// and a bootstrap commit of the provider's own would make the ref comparison
// a comparison of two different histories.
func (g *gitea) createRepo(ctx context.Context, owner, name string, private bool) (string, error) {
	me, err := g.whoami(ctx)
	if err != nil {
		return "", err
	}
	// A repository belongs either to the token's USER or to an ORGANISATION,
	// and Gitea has a different endpoint for each. The named-owner path is
	// the import's clean namespace; the fallback is the owner's own.
	if owner == "" || owner == me {
		owner = me
		code, raw, err := g.call(ctx, http.MethodPost, "/api/v1/user/repos", map[string]any{
			"name": name, "private": private, "auto_init": false, "default_branch": "main",
		})
		if err != nil {
			return "", err
		}
		if code != http.StatusCreated {
			return "", fmt.Errorf("gitea create repo %s/%s (own namespace): HTTP %d: %s", owner, name, code, strings.TrimSpace(string(raw)))
		}
		return owner, nil
	}
	code, raw, err := g.call(ctx, http.MethodPost, "/api/v1/orgs/"+url.PathEscape(owner)+"/repos", map[string]any{
		"name": name, "private": private, "auto_init": false, "default_branch": "main",
	})
	if err != nil {
		return "", err
	}
	if code == http.StatusCreated {
		return owner, nil
	}
	// The organisation may not exist yet; create it and retry once. A
	// second failure is reported as it came, with the status named.
	if exists, err := g.orgExists(ctx, owner); err == nil && !exists {
		c2, raw2, err2 := g.call(ctx, http.MethodPost, "/api/v1/orgs", map[string]any{
			"username": owner, "visibility": "private",
		})
		if err2 != nil {
			return "", err2
		}
		if c2 != http.StatusCreated && c2 != http.StatusConflict {
			return "", fmt.Errorf("gitea create org %s: HTTP %d: %s", owner, c2, strings.TrimSpace(string(raw2)))
		}
		code, raw, err = g.call(ctx, http.MethodPost, "/api/v1/orgs/"+url.PathEscape(owner)+"/repos", map[string]any{
			"name": name, "private": private, "auto_init": false, "default_branch": "main",
		})
		if err != nil {
			return "", err
		}
	}
	if code != http.StatusCreated {
		return "", fmt.Errorf("gitea create repo %s/%s: HTTP %d: %s", owner, name, code, strings.TrimSpace(string(raw)))
	}
	return owner, nil
}

func (g *gitea) orgExists(ctx context.Context, name string) (bool, error) {
	code, _, err := g.call(ctx, http.MethodGet, "/api/v1/orgs/"+url.PathEscape(name), nil)
	if err != nil {
		return false, err
	}
	switch code {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("gitea GET /api/v1/orgs/%s: HTTP %d", name, code)
	}
}

// repoURL is the smart-HTTP endpoint the git CLI talks to.
func repoURL(base, owner, name string) string {
	return strings.TrimRight(base, "/") + "/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + ".git"
}

// sortedRefs renders a ref map for a report, stable across runs.
func sortedRefs(refs map[string]string) []string {
	out := make([]string, 0, len(refs))
	for k, v := range refs {
		out = append(out, k+" "+v)
	}
	sort.Strings(out)
	return out
}
