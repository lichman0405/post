package backupdr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// The Gitea repositories class of the backup (docs/37 §需要备份).
//
// A repository is backed up the way Git backs one up: a mirror clone
// (`--mirror`), which carries every ref and every object, and then the ref
// list read out of the mirror by git itself. Both halves are recorded
// because they answer different questions — the mirror answers "can this be
// restored", the ref list answers "restored to WHAT", and the second is
// what the "DB ↔ Git refs" axis compares against.
//
// # Credentials
//
// The drill uses the local dev stack's Gitea and the dev admin credential
// infra/docker/gitea/init-gitea.sh creates (POST_GITEA_ADMIN_USER /
// POST_GITEA_ADMIN_PASSWORD, dev-only defaults). It mints one run-scoped
// token, uses it, and revokes it. docs/25 §32 forbids a Worker touching
// production deployment credentials; nothing here reads one, and the
// repository the drill is pointed at is named by the caller, not
// discovered.
//
// # Why basic auth is put in neither a remote URL nor an argument
//
// A URL carrying a password ends up in `git`'s error output and in the
// remote's stored config inside the mirror. `-c http.extraHeader=...` is no
// better: a `-c` value IS an argument, and an argument is readable by every
// account on the machine through `ps aux` and /proc/<pid>/cmdline. Credentials
// are injected per invocation into the process ENVIRONMENT instead (gitEnv),
// the shape internal/gitprovider/gitea.go uses — the header rides in
// GIT_CONFIG_VALUE_0, which unlike argv no other account can read. Every error
// this file returns has also been passed through redactSecrets: that is a
// separate path (git's own messages can quote the header it was given), and
// closing one is no reason to open the other.

// gitClient drives Gitea's REST API and git's own plumbing for the Git half
// of the drill.
type gitClient struct {
	base  string
	token string
	http  *http.Client
	// runID names the repositories this drill creates, so a concurrent run
	// never collides with this one and cleanup can only ever delete the
	// drill's own.
	runID string
	// runner executes git. newGitClient installs the real binary; the field
	// is the unit-test seam (the shape internal/gitprovider's GitRunner
	// uses), and it exists because where a credential does and does not go
	// is a property of the invocation — argv and environment — which cannot
	// be asserted by reading this function's source.
	runner gitRunner
}

func newGitClient(base, token, runID string) *gitClient {
	return &gitClient{
		base:   strings.TrimSuffix(base, "/"),
		token:  token,
		http:   &http.Client{Timeout: 60 * time.Second},
		runID:  runID,
		runner: defaultGitRunner,
	}
}

// gitRunner executes one already-resolved git binary in the given working
// directory and environment, and returns its stdout and stderr separately.
// Everything a caller observes around the invocation — the LookPath
// message, the wrapping, the redaction — stays in git below, so
// substituting a runner changes nothing but the execution.
type gitRunner func(ctx context.Context, bin, dir string, env []string, args ...string) (stdout, stderr []byte, err error)

// defaultGitRunner runs the real git binary.
func defaultGitRunner(ctx context.Context, bin, dir string, env []string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// gitAuthHeader is the extra header every git invocation carries. Gitea
// accepts the token as a bearer credential; it is written into the header,
// which git does not echo on failure.
func (g *gitClient) gitAuthHeader() string {
	return "Authorization: Bearer " + g.token
}

// gitEnv is the environment of one git invocation. The two config entries
// are the two `-c` flags this used to pass — the auth header and an empty
// credential.helper, so no ambient helper can answer a prompt this drill
// cannot see — expressed the way internal/gitprovider/gitea.go expresses
// them, because a `-c` value is an ARGUMENT and argv is readable by every
// account on the machine (/proc/<pid>/cmdline, `ps aux`), while a
// GIT_CONFIG_VALUE_n is in neither of those.
//
// The entries are appended to the inherited environment rather than
// replacing it: PATH and the proxy/TLS settings git needs flow through, and
// exec's last-one-wins de-duplication is what makes these values the ones a
// subprocess sees.
func (g *gitClient) gitEnv() []string {
	return append(os.Environ(),
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0="+g.gitAuthHeader(),
		"GIT_CONFIG_KEY_1=credential.helper",
		"GIT_CONFIG_VALUE_1=",
		"GIT_TERMINAL_PROMPT=0")
}

// git runs one git command. Credentials arrive in the process environment
// (gitEnv), never in the argument list and never in a remote URL, and the
// error text is redacted before it is returned — git's own messages can
// quote the header it was given.
func (g *gitClient) git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	bin, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("git: not on PATH: %w", err)
	}
	stdout, stderr, err := g.runner(ctx, bin, dir, g.gitEnv(), args...)
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err,
			redactSecrets(string(stderr), g.token))
	}
	return stdout, nil
}

// giteaAPIError carries Gitea's own status code.
//
// The status is carried as a value rather than folded into the message
// because two callers have to tell "the server answered that the thing is
// not there" (404 — an ANSWER, and sometimes the empty case) from "the
// server could not answer" (403, 500, a timeout — NOT an answer, and never
// evidence that something is empty). A caller that pattern-matches on
// message text gets that distinction wrong the first time Gitea changes
// its wording.
type giteaAPIError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *giteaAPIError) Error() string {
	return fmt.Sprintf("gitea %s %s: %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// isGiteaNotFound reports whether err is Gitea saying the thing is absent.
func isGiteaNotFound(err error) bool {
	var apiErr *giteaAPIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

// api performs one Gitea REST call. A non-2xx is a *giteaAPIError carrying
// the server's own status and message, redacted.
func (g *gitClient) api(ctx context.Context, method, path string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.http.Do(req)
	if err != nil {
		// A transport failure has no status: the caller cannot mistake it for
		// an answer.
		return nil, fmt.Errorf("gitea %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("gitea %s %s: read body: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &giteaAPIError{
			Method: method,
			Path:   path,
			Status: resp.StatusCode,
			Body:   redactSecrets(string(raw), g.token),
		}
	}
	return raw, nil
}

// repoInfo is the slice of Gitea's repository payload the drill reads.
type repoInfo struct {
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Empty         bool   `json:"empty"`
}

// listRepos returns every repository under owner, following pagination to
// the end. A truncated listing would silently back up a prefix of the
// owner's repositories and call it complete.
func (g *gitClient) listRepos(ctx context.Context, owner string) ([]repoInfo, error) {
	var all []repoInfo
	for page := 1; page <= 100; page++ {
		raw, err := g.api(ctx, http.MethodGet,
			fmt.Sprintf("/api/v1/orgs/%s/repos?limit=50&page=%d", url.PathEscape(owner), page), nil)
		if err != nil {
			return nil, err
		}
		var page1 []repoInfo
		if err := json.Unmarshal(raw, &page1); err != nil {
			return nil, fmt.Errorf("gitea: parse repository list of %s: %w", owner, err)
		}
		all = append(all, page1...)
		if len(page1) < 50 {
			sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
			return all, nil
		}
	}
	return nil, fmt.Errorf("gitea: repository listing of %s did not terminate after 100 pages", owner)
}

// createOrg creates the drill's own organization if it is not there.
func (g *gitClient) createOrg(ctx context.Context, org string) error {
	if _, err := g.api(ctx, http.MethodGet, "/api/v1/orgs/"+url.PathEscape(org), nil); err == nil {
		return nil
	}
	_, err := g.api(ctx, http.MethodPost, "/api/v1/orgs", map[string]any{
		"username":    org,
		"description": "POST backup/restore drill target (T1110) — created and deleted by the drill",
	})
	return err
}

// createRepo creates repo under org, tolerating an existing one (409).
func (g *gitClient) createRepo(ctx context.Context, org, repo string) error {
	_, err := g.api(ctx, http.MethodPost, "/api/v1/orgs/"+url.PathEscape(org)+"/repos", map[string]any{
		"name":           repo,
		"private":        true,
		"auto_init":      false,
		"default_branch": "main",
	})
	var apiErr *giteaAPIError
	if err != nil && errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
		return nil
	}
	return err
}

// deleteRepo removes a repository the drill created.
func (g *gitClient) deleteRepo(ctx context.Context, org, repo string) error {
	_, err := g.api(ctx, http.MethodDelete,
		"/api/v1/repos/"+url.PathEscape(org)+"/"+url.PathEscape(repo), nil)
	return err
}

// deleteOrg removes the drill's own organization.
func (g *gitClient) deleteOrg(ctx context.Context, org string) error {
	_, err := g.api(ctx, http.MethodDelete, "/api/v1/orgs/"+url.PathEscape(org), nil)
	return err
}

// cloneURL is the URL git fetches from. It carries no credential: the
// token travels in the per-invocation header (gitAuthHeader), handed to git
// through the process environment (gitEnv), not through this string.
func (g *gitClient) cloneURL(owner, repo string) string {
	return g.base + "/" + owner + "/" + repo + ".git"
}

// mirrorRepo clones one repository into dest as a mirror and returns its
// refs as git itself reports them.
func (g *gitClient) mirrorRepo(ctx context.Context, owner, repo, dest string) (string, []GitRef, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", nil, err
	}
	// A previous run's directory would make --mirror update it instead of
	// creating it; removing it first keeps the artifact a pure function of
	// this run's source.
	if err := os.RemoveAll(dest); err != nil {
		return "", nil, err
	}
	if _, err := g.git(ctx, "", "clone", "--mirror", "--quiet", g.cloneURL(owner, repo), dest); err != nil {
		return "", nil, fmt.Errorf("mirror clone %s/%s: %w", owner, repo, err)
	}
	defaultRef, err := g.defaultRef(ctx, owner, repo, dest)
	if err != nil {
		return "", nil, err
	}
	refs, err := g.listRefs(ctx, dest)
	if err != nil {
		return "", nil, err
	}
	return defaultRef, refs, nil
}

// defaultRef names the repository's default branch (recorded so the
// reconciler's reverse check — "a ref no branch row names" — knows which
// ref is legitimately unmapped).
func (g *gitClient) defaultRef(ctx context.Context, owner, repo, mirror string) (string, error) {
	out, err := g.git(ctx, mirror, "symbolic-ref", "HEAD")
	if err == nil {
		if ref := strings.TrimSpace(string(out)); ref != "" {
			return ref, nil
		}
	}
	// A mirror of an empty repository has no HEAD to read; fall back to the
	// provider's own answer.
	raw, apiErr := g.api(ctx, http.MethodGet, "/api/v1/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(repo), nil)
	if apiErr != nil {
		return "", apiErr
	}
	var info repoInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return "", err
	}
	if info.DefaultBranch == "" {
		return "", fmt.Errorf("git: %s/%s reports no default branch", owner, repo)
	}
	return "refs/heads/" + info.DefaultBranch, nil
}

// listRefs reads every ref of a mirror with git's own plumbing. Packed and
// loose refs both come back, which is the point: `git for-each-ref` is the
// authority on what the repository contains, and a listing assembled from
// anywhere else would be a claim about the repository rather than a reading
// of it.
func (g *gitClient) listRefs(ctx context.Context, mirror string) ([]GitRef, error) {
	out, err := g.git(ctx, mirror, "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		return nil, err
	}
	var refs []GitRef
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("git: unparseable ref line %q", line)
		}
		refs = append(refs, GitRef{Name: fields[0], SHA: fields[1]})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs, nil
}

// pushMirror pushes a mirror into a freshly created repository, creating
// every ref exactly as the source had it.
func (g *gitClient) pushMirror(ctx context.Context, mirror, owner, repo string) error {
	_, err := g.git(ctx, mirror, "push", "--mirror", "--quiet", g.cloneURL(owner, repo))
	if err != nil {
		return fmt.Errorf("push mirror to %s/%s: %w", owner, repo, err)
	}
	return nil
}

// remoteRefs reads a remote's refs without cloning — the reconciler's Git
// half. A ref that is gone and a ref that moved are different findings, so
// the map is returned whole rather than probed one ref at a time.
func (g *gitClient) remoteRefs(ctx context.Context, owner, repo string) (map[string]string, error) {
	out, err := g.git(ctx, "", "ls-remote", "--heads", "--tags", "--quiet", g.cloneURL(owner, repo))
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 {
			continue
		}
		// A peeled tag line ("refs/tags/v1^{}") is the tag's target, not a
		// ref of its own.
		if strings.HasSuffix(fields[1], "^{}") {
			continue
		}
		refs[fields[1]] = fields[0]
	}
	return refs, nil
}

// mintToken creates a run-scoped Gitea token through the dev admin's basic
// auth — the same mechanism tests/integration/gitea_provisioning_test.go
// uses. The token is scoped to what the drill does and is revoked at
// cleanup, so a drill never leaves a credential behind.
func mintToken(ctx context.Context, base, adminUser, adminPass, serviceAccount, name string) (id int64, token string, err error) {
	body, err := json.Marshal(map[string]any{
		"name":   name,
		"scopes": []string{"write:repository", "write:user", "write:organization"},
	})
	if err != nil {
		return 0, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(base, "/")+"/api/v1/users/"+url.PathEscape(serviceAccount)+"/tokens", bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.SetBasicAuth(adminUser, adminPass)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("gitea: mint token: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		return 0, "", fmt.Errorf("gitea: mint token for %s = %s (admin basic auth): %s",
			serviceAccount, resp.Status, redactSecrets(string(raw), adminPass))
	}
	var minted struct {
		ID    int64  `json:"id"`
		SHA1  string `json:"sha1"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &minted); err != nil {
		return 0, "", fmt.Errorf("gitea: parse minted token: %w", err)
	}
	// Gitea's mint response carries the token under `sha1` (the field the
	// dev image returns) and, in some versions, under `token`. Both name the
	// same secret; whichever is present is the one to use, and neither being
	// present is a hard failure rather than an empty credential that would
	// fail later as a confusing 401.
	value := minted.Token
	if value == "" {
		value = minted.SHA1
	}
	if value == "" {
		return 0, "", fmt.Errorf("gitea: mint returned no token value (neither `token` nor `sha1`)")
	}
	return minted.ID, value, nil
}

// revokeToken deletes a token the drill minted. Best-effort by design: the
// drill's own failure must not be masked by cleanup's, and the test asserts
// revocation separately.
func revokeToken(ctx context.Context, base, adminUser, adminPass, serviceAccount string, id int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		fmt.Sprintf("%s/api/v1/users/%s/tokens/%d", strings.TrimSuffix(base, "/"), url.PathEscape(serviceAccount), id), nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(adminUser, adminPass)
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("gitea: revoke token %d: %s", id, resp.Status)
	}
	return nil
}

// redactSecrets removes every occurrence of each secret from s. It is the
// one gate between a credential and an error message: everything this
// package returns to a caller (and therefore to a log, a report or a test
// failure) goes through it, so a token cannot reach the artifact or the
// terminal even when a library decides to quote the request it was given.
func redactSecrets(s string, secrets ...string) string {
	for _, secret := range secrets {
		if len(secret) < 8 {
			continue // too short to be a credential; redacting it would mangle prose
		}
		s = strings.ReplaceAll(s, secret, "[redacted]")
	}
	return s
}
