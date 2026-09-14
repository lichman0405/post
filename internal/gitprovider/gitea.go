package gitprovider

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
	"strings"
	"sync"
	"time"
)

// GiteaAdapter is the GitPort implementation over the Gitea REST API v1
// (ADR-003, ADR-019): every call authenticates with the service account
// token, so all provider-side changes the platform makes are attributable
// to one machine identity. The adapter never touches Gitea's database —
// it talks HTTP and the git protocol only; the acceptance criterion
// "domain 不引用 Gitea DB" holds by construction (and the domain layer
// never sees this package).
//
// Branch refs split across two channels, decided by what the deployed
// instance actually answers (checked against the running instance, not a
// doc): reads go through the refs API (GET /git/refs/heads/<name> — the
// /branches API 500s for refs that arrived by push on this deployment),
// and writes go through the git protocol, because Gitea's refs API is
// read-only (POST/DELETE answer 405) and the /branches API refuses refs
// of pushed repositories. The git protocol is the one channel the G3
// script (tests/acceptance/gitea-real-services-e2e.sh) also uses.
type GiteaAdapter struct {
	baseURL string // no trailing slash
	token   string
	client  *http.Client
	runner  GitRunner

	// owner is the service account login, resolved from the token
	// (GET /user) and cached once it succeeds: repositories are provisioned
	// into the service account's own namespace (project→repo 1:1 needs one
	// globally-unique name, which the caller derives from the project id).
	// Failures are deliberately NOT cached — a transient outage or a
	// rotated token must not poison every later call for the process
	// lifetime.
	ownerMu  sync.Mutex
	owner    string
	ownerSet bool
}

// GitRunner executes git CLI commands and returns the combined output —
// git's failure surface is its stderr text, which the adapter classifies
// onto the port's sentinels. The unit-test seam: the default runs the real
// git binary, tests substitute a scripted runner.
type GitRunner func(ctx context.Context, env []string, args ...string) (string, error)

// defaultGitRunner runs the real git binary with the given environment.
func defaultGitRunner(ctx context.Context, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = env
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// AdapterOption tunes the adapter (unit-test seam only).
type AdapterOption func(*GiteaAdapter)

// WithGitRunner substitutes the git CLI executor.
func WithGitRunner(r GitRunner) AdapterOption {
	return func(a *GiteaAdapter) { a.runner = r }
}

// NewGiteaAdapter builds the adapter on the validated configuration.
func NewGiteaAdapter(cfg Config, opts ...AdapterOption) *GiteaAdapter {
	a := &GiteaAdapter{
		baseURL: strings.TrimSuffix(cfg.BaseURL, "/"),
		token:   string(cfg.Token),
		client:  &http.Client{Timeout: 10 * time.Second},
		runner:  defaultGitRunner,
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Owner resolves the service account login the adapter operates as.
// Exposed for tests and diagnostics; production callers get it through the
// Repository values the port methods return.
func (a *GiteaAdapter) Owner(ctx context.Context) (string, error) {
	a.ownerMu.Lock()
	defer a.ownerMu.Unlock()
	if a.ownerSet {
		return a.owner, nil
	}
	owner, err := a.fetchOwner(ctx)
	if err != nil {
		return "", err
	}
	a.owner, a.ownerSet = owner, true
	return owner, nil
}

func (a *GiteaAdapter) fetchOwner(ctx context.Context) (string, error) {
	var user struct {
		Login string `json:"login"`
	}
	code, raw, err := a.call(ctx, http.MethodGet, "/api/v1/user", nil, &user)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", a.mapStatus(code, "resolve service account", raw)
	}
	return user.Login, nil
}

// repoBody is the provider-side repository shape the adapter reads back
// (a subset of Gitea's Repository API type).
type repoBody struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
	CloneURL      string `json:"clone_url"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
}

// repository maps the provider shape onto the port type.
func (b repoBody) repository(owner, name string) Repository {
	return Repository{
		Owner:         owner,
		Name:          name,
		ID:            b.ID,
		CloneURL:      b.CloneURL,
		Private:       b.Private,
		DefaultBranch: b.DefaultBranch,
	}
}

// EnsureRepository implements GitPort: the repository is created in the
// adapter's own service-account namespace (POST /user/repos). An existing
// repository of the same name is adopted, not an error — including the
// 409 race where a concurrent provisioner created it between our lookup
// and our create. Adoption repairs the Git-layer visibility first: an
// existing repository must never stay public.
func (a *GiteaAdapter) EnsureRepository(ctx context.Context, spec RepositorySpec) (Repository, error) {
	owner, err := a.Owner(ctx)
	if err != nil {
		return Repository{}, err
	}
	if repo, err := a.adopt(ctx, owner, spec); err == nil {
		return repo, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Repository{}, err
	}

	body := map[string]any{
		"name":        spec.Name,
		"private":     spec.Private,
		"auto_init":   false, // no initial commit: T0302 protects main before any ref exists
		"description": spec.Description,
	}
	var repo repoBody
	code, raw, err := a.call(ctx, http.MethodPost, "/api/v1/user/repos", body, &repo)
	if err != nil {
		return Repository{}, err
	}
	if code == http.StatusConflict {
		// Lost a create race: the repository now exists — adopt it.
		return a.adopt(ctx, owner, spec)
	}
	if code != http.StatusCreated {
		return Repository{}, a.mapStatus(code, "create repository", raw)
	}
	return repo.repository(owner, spec.Name), nil
}

// adopt returns the pre-existing repository, repairing its Git-layer
// visibility to match spec first. Git-layer read access is granted through
// per-user tokens (T0304), never through publicity — so adopting a
// repository that exists but is public must flip it private, not hand it
// back as-is.
func (a *GiteaAdapter) adopt(ctx context.Context, owner string, spec RepositorySpec) (Repository, error) {
	repo, err := a.GetRepository(ctx, owner, spec.Name)
	if err != nil {
		return Repository{}, err
	}
	if spec.Private && !repo.Private {
		if err := a.setPrivate(ctx, owner, repo.Name); err != nil {
			return Repository{}, err
		}
		repo.Private = true
	}
	return repo, nil
}

// setPrivate forces the provider-side repository private. The PATCH body
// is visibility-only: Gitea leaves omitted fields untouched, so adopt
// never rewrites the name or description an operator may have set.
func (a *GiteaAdapter) setPrivate(ctx context.Context, owner, name string) error {
	body := map[string]any{"private": true}
	code, raw, err := a.call(ctx, http.MethodPatch,
		"/api/v1/repos/"+urlSegment(owner)+"/"+urlSegment(name), body, nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return a.mapStatus(code, "repair repository privacy", raw)
	}
	return nil
}

// GetRepository implements GitPort.
func (a *GiteaAdapter) GetRepository(ctx context.Context, owner, name string) (Repository, error) {
	var repo repoBody
	code, raw, err := a.call(ctx, http.MethodGet,
		"/api/v1/repos/"+urlSegment(owner)+"/"+urlSegment(name), nil, &repo)
	if err != nil {
		return Repository{}, err
	}
	if code == http.StatusNotFound {
		return Repository{}, ErrNotFound
	}
	if code != http.StatusOK {
		return Repository{}, a.mapStatus(code, "get repository", raw)
	}
	return repo.repository(owner, name), nil
}

// EnsureWebhook implements GitPort: exactly one active gitea-type webhook
// delivers spec.Events to spec.URL. An existing hook for the same URL is
// updated in place with the (possibly rotated) secret; Gitea never returns
// the secret, so the stored platform-side secret stays the only authority.
func (a *GiteaAdapter) EnsureWebhook(ctx context.Context, spec WebhookSpec) (Webhook, error) {
	owner, name := spec.Repository.Owner, spec.Repository.Name
	hooksPath := "/api/v1/repos/" + urlSegment(owner) + "/" + urlSegment(name) + "/hooks"
	var hooks []struct {
		ID     int64  `json:"id"`
		Type   string `json:"type"`
		Active bool   `json:"active"`
		Config struct {
			URL string `json:"url"`
		} `json:"config"`
	}
	code, raw, err := a.call(ctx, http.MethodGet, hooksPath, nil, &hooks)
	if err != nil {
		return Webhook{}, err
	}
	if code == http.StatusNotFound {
		return Webhook{}, ErrNotFound
	}
	if code != http.StatusOK {
		return Webhook{}, a.mapStatus(code, "list webhooks", raw)
	}

	hookBody := map[string]any{
		"type":   "gitea",
		"active": true,
		"events": spec.Events,
		"config": map[string]any{
			"url":          spec.URL,
			"content_type": "json",
			"secret":       spec.Secret,
		},
	}
	for _, h := range hooks {
		if h.Type == "gitea" && h.Config.URL == spec.URL {
			// Update in place: rotate the secret instead of stacking hooks.
			var updated struct {
				ID     int64 `json:"id"`
				Active bool  `json:"active"`
			}
			code, raw, err := a.call(ctx, http.MethodPatch,
				fmt.Sprintf("%s/%d", hooksPath, h.ID), hookBody, &updated)
			if err != nil {
				return Webhook{}, err
			}
			if code != http.StatusOK {
				return Webhook{}, a.mapStatus(code, "update webhook", raw)
			}
			return Webhook{ID: updated.ID, Active: updated.Active}, nil
		}
	}

	var created struct {
		ID     int64 `json:"id"`
		Active bool  `json:"active"`
	}
	code, raw, err = a.call(ctx, http.MethodPost, hooksPath, hookBody, &created)
	if err != nil {
		return Webhook{}, err
	}
	if code != http.StatusCreated {
		return Webhook{}, a.mapStatus(code, "create webhook", raw)
	}
	return Webhook{ID: created.ID, Active: created.Active}, nil
}

// gitRefBody is the provider-side ref shape the adapter reads back (a
// subset of Gitea's Reference API type). GET /git/refs/heads/<name>
// answers an ARRAY — one element for the exact ref (checked against the
// running instance).
type gitRefBody struct {
	Ref    string `json:"ref"`
	Object struct {
		Type string `json:"type"`
		SHA  string `json:"sha"`
	} `json:"object"`
}

// isFullSHA reports whether s is a full 40-hex commit SHA (the shape
// project_states.git_commit_sha carries): then it names the fork commit
// directly; anything else is treated as a provider ref name.
func isFullSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// pushURL is the git-protocol URL of a provisioned repository (the smart
// HTTP endpoint the git CLI talks to). The token is NOT embedded here —
// it rides in the process environment (gitEnv), never in argv.
func (a *GiteaAdapter) pushURL(owner, name string) string {
	return a.baseURL + "/" + urlSegment(owner) + "/" + urlSegment(name) + ".git"
}

// gitEnv builds the environment for one git invocation: hermetic against
// an ambient git config (no system config; HOME and the entire GIT_*
// namespace are this adapter's alone — an ambient GIT_DIR, GIT_OBJECT_DIRECTORY,
// GIT_ALTERNATE_OBJECT_DIRECTORIES, GIT_WORK_TREE, GIT_ASKPASS or GIT_SSH
// would redirect or reconfigure the invocation away from the scratch
// repository) and authenticated without the token ever reaching argv — it
// rides in GIT_CONFIG_VALUE_0, process-visible only. Everything else
// (PATH, proxy settings, TLS roots) flows through.
func (a *GiteaAdapter) gitEnv(dir string) []string {
	var env []string
	for _, kv := range os.Environ() {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if key == "HOME" || strings.HasPrefix(key, "GIT_") {
			// Overridden below — an ambient git configuration or state
			// pointer must not reach a provider call.
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"HOME="+dir,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: token "+a.token,
		"GIT_TERMINAL_PROMPT=0")
}

// gitRun runs one git command inside a per-operation bare scratch
// repository, created and removed here: no shared state between concurrent
// syncs, no lock, no growth over the process lifetime. The scratch
// repository only needs to exist — fetch and push never touch a worktree.
func (a *GiteaAdapter) gitRun(ctx context.Context, dir string, args ...string) (string, error) {
	full := append([]string{"-C", dir}, args...)
	out, err := a.runner(ctx, a.gitEnv(dir), full...)
	if err != nil {
		return out, fmt.Errorf("git %s: %w", args[0], err)
	}
	return out, nil
}

// newScratchRepo creates the per-operation bare repository gitRun works in.
func (a *GiteaAdapter) newScratchRepo(ctx context.Context) (string, error) {
	dir, err := os.MkdirTemp("", "post-git-*")
	if err != nil {
		return "", fmt.Errorf("%w: scratch repository: %v", ErrUnavailable, err)
	}
	if _, err := a.gitRun(ctx, dir, "init", "--bare", "-q", dir); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("%w: scratch repository: %v", ErrUnavailable, err)
	}
	return dir, nil
}

// mapGitErr classifies a git CLI failure onto the port's sentinels, on the
// output text (the messages the real protocol emits, checked against the
// running instance). The output never contains the token by construction —
// it is only ever passed through the environment (gitEnv).
func (a *GiteaAdapter) mapGitErr(action, out string) error {
	msg := strings.Join(strings.Fields(out), " ")
	if r := []rune(msg); len(r) > 300 {
		msg = string(r[:300])
	}
	switch {
	case strings.Contains(out, "not our ref"),
		strings.Contains(out, "couldn't find remote ref"),
		strings.Contains(out, "bad object"),
		strings.Contains(out, "unknown revision"),
		strings.Contains(out, "does not exist"):
		return fmt.Errorf("%w: %s: %s", ErrNotFound, action, msg)
	case strings.Contains(out, "could not read Username"),
		strings.Contains(out, "terminal prompts disabled"),
		strings.Contains(out, "Authentication failed"),
		strings.Contains(out, "Access denied"):
		return fmt.Errorf("%w: %s: %s", ErrUnauthorized, action, msg)
	case strings.Contains(out, "protected branch"),
		strings.Contains(out, "hook declined"):
		return fmt.Errorf("%w: %s: %s", ErrConflict, action, msg)
	default:
		return fmt.Errorf("%w: %s: %s", ErrUnavailable, action, msg)
	}
}

// gitUpdateRejected reports whether a failed push was refused because the
// ref already exists (a concurrent creator won the race between our read
// and our push): the adoption path, never an error.
func gitUpdateRejected(out string) bool {
	for _, marker := range []string{"[rejected]", "non-fast-forward", "already exists", "cannot lock ref", "fetch first"} {
		if strings.Contains(out, marker) {
			return true
		}
	}
	return false
}

// EnsureBranch implements GitPort (T0303): the branch ref spec.Name is
// created forked from spec.ForkRef — a provider ref name or commit SHA. An
// empty ForkRef resolves to the repository's default branch; an empty
// default branch means the repository has no refs at all (T0301 provisions
// with auto_init false), which is ErrNotFound — the syncer keeps the row
// retryable until the initial commit lands (T0302). An existing ref of the
// same name is adopted, not an error, including the race where a
// concurrent sync created it between our read and our push.
//
// The write channel is the git protocol: the fork commit is shallow-fetched
// into a per-operation scratch repository (the git client needs the object
// locally to push it), then pushed to the new ref. The fetch doubles as the
// fork-point existence check: a fork sha the provider does not have fails
// here with ErrNotFound.
func (a *GiteaAdapter) EnsureBranch(ctx context.Context, spec BranchSpec) (BranchRef, error) {
	repo := spec.Repository

	// Adopt first: an existing ref is the truth — idempotent sync, a
	// redelivered job never fails on an already-created ref.
	if ref, err := a.GetBranch(ctx, repo, spec.Name); err == nil {
		return ref, nil
	} else if !errors.Is(err, ErrNotFound) {
		return BranchRef{}, err
	}

	// Resolve the fork point to one commit SHA.
	forkSHA := spec.ForkRef
	if forkSHA == "" {
		info, err := a.GetRepository(ctx, repo.Owner, repo.Name)
		if err != nil {
			return BranchRef{}, err
		}
		forkSHA = info.DefaultBranch
		if forkSHA == "" {
			return BranchRef{}, fmt.Errorf("%w: repository %s/%s has no refs to fork from (the initial commit arrives with T0302)",
				ErrNotFound, repo.Owner, repo.Name)
		}
	}
	if !isFullSHA(forkSHA) {
		ref, err := a.GetBranch(ctx, repo, forkSHA)
		if err != nil {
			return BranchRef{}, fmt.Errorf("%w: fork point %q does not exist in %s/%s",
				ErrNotFound, forkSHA, repo.Owner, repo.Name)
		}
		forkSHA = ref.HeadSHA
	}

	dir, err := a.newScratchRepo(ctx)
	if err != nil {
		return BranchRef{}, err
	}
	defer os.RemoveAll(dir)
	pushURL := a.pushURL(repo.Owner, repo.Name)
	if out, err := a.gitRun(ctx, dir, "fetch", "--depth=1", pushURL, forkSHA); err != nil {
		return BranchRef{}, a.mapGitErr("fetch the fork point", out)
	}
	if out, err := a.gitRun(ctx, dir, "push", pushURL, forkSHA+":refs/heads/"+spec.Name); err != nil {
		if gitUpdateRejected(out) {
			// A concurrent creator won: adopt the ref that is actually
			// there instead of failing.
			if ref, gerr := a.GetBranch(ctx, repo, spec.Name); gerr == nil {
				return ref, nil
			}
		}
		return BranchRef{}, a.mapGitErr("create the branch ref", out)
	}

	// Read the provider-side head back: the pushed sha is the fork point,
	// but a concurrent push may already have moved the ref — the ref is the
	// truth, and the record commits what the provider actually carries.
	return a.GetBranch(ctx, repo, spec.Name)
}

// GetBranch implements GitPort over the refs API — on this deployment the
// /branches API cannot answer for refs that arrived by push (checked
// against the running instance), so the refs API is the one that can.
func (a *GiteaAdapter) GetBranch(ctx context.Context, repo Repository, name string) (BranchRef, error) {
	var refs []gitRefBody
	code, raw, err := a.call(ctx, http.MethodGet,
		"/api/v1/repos/"+urlSegment(repo.Owner)+"/"+urlSegment(repo.Name)+"/git/refs/heads/"+urlSegment(name), nil, &refs)
	if err != nil {
		return BranchRef{}, err
	}
	if code == http.StatusNotFound || (code == http.StatusOK && len(refs) == 0) {
		return BranchRef{}, ErrNotFound
	}
	if code != http.StatusOK {
		return BranchRef{}, a.mapStatus(code, "get branch ref", raw)
	}
	if refs[0].Object.SHA == "" {
		return BranchRef{}, ErrNotFound
	}
	return BranchRef{Name: name, HeadSHA: refs[0].Object.SHA}, nil
}

// DeleteBranch implements GitPort over the git protocol: a push with an
// empty source deletes the remote ref. A missing ref is the goal already
// achieved (the port contract): this instance's provider reports the
// delete of a nonexistent ref as success, but servers exist that refuse it
// ("unable to delete <name>: remote ref does not exist"), so the adapter
// maps exactly that refusal to success too — an out-of-band deletion or a
// redelivered close job must never fail on work that is already done.
func (a *GiteaAdapter) DeleteBranch(ctx context.Context, repo Repository, name string) error {
	dir, err := a.newScratchRepo(ctx)
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	out, err := a.gitRun(ctx, dir, "push", a.pushURL(repo.Owner, repo.Name), ":refs/heads/"+name)
	if err != nil {
		if strings.Contains(out, "remote ref does not exist") {
			return nil
		}
		return a.mapGitErr("delete the branch ref", out)
	}
	return nil
}

// call performs one API request and decodes the JSON response body into
// out (may be nil). The token is attached as an Authorization header.
func (a *GiteaAdapter) call(ctx context.Context, method, path string, body any, out any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("%w: encode request: %v", ErrUnavailable, err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.baseURL+path, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: build request: %v", ErrUnavailable, err)
	}
	req.Header.Set("Authorization", "token "+a.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		// Never embed the raw error: it can contain transport details and
		// (for credential-embedding base URLs) secrets. Callers log and
		// store through config.RedactForOutput anyway.
		return 0, nil, fmt.Errorf("%w: provider request failed", ErrUnavailable)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: read response: %v", ErrUnavailable, err)
	}
	// Decode success bodies only: provider error bodies are {"message": ...}
	// objects that do not match the success shape (a []struct target fails
	// outright), and the status code — not the body — decides the sentinel.
	if out != nil && len(raw) > 0 && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err := json.Unmarshal(raw, out); err != nil {
			return 0, nil, fmt.Errorf("%w: decode response", ErrUnavailable)
		}
	}
	return resp.StatusCode, raw, nil
}

// mapStatus maps a provider status onto the port's sentinel errors,
// attaching the provider's own message (truncated, single-line) for the
// operator when the response carried one.
func (a *GiteaAdapter) mapStatus(code int, action string, raw []byte) error {
	msg := providerMessage(raw)
	var err error
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		err = ErrUnauthorized
	case code == http.StatusNotFound:
		err = ErrNotFound
	case code == http.StatusConflict, code == http.StatusUnprocessableEntity,
		code == http.StatusBadRequest:
		err = ErrConflict
	default:
		err = fmt.Errorf("%w (status %d)", ErrUnavailable, code)
	}
	if msg != "" {
		return fmt.Errorf("%w: %s: %s", err, action, msg)
	}
	return fmt.Errorf("%w: %s", err, action)
}

// providerMessage extracts Gitea's {"message": "..."} error text, bounded
// and flattened so an error string can never balloon or inject newlines
// into structured logs.
func providerMessage(raw []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &e) != nil || e.Message == "" {
		return ""
	}
	msg := strings.Join(strings.Fields(e.Message), " ")
	if r := []rune(msg); len(r) > 200 {
		// Truncate on rune boundaries: byte-slicing could split a multibyte
		// rune and store invalid UTF-8 on the project row (PostgreSQL
		// rejects it).
		msg = string(r[:200]) + "…"
	}
	return msg
}

// urlSegment escapes one path segment for use in an API path. Owner and
// name are provider-validated identifiers, never raw user input — this is
// defense-in-depth, not a security boundary.
func urlSegment(s string) string { return url.PathEscape(s) }
