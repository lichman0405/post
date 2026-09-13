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
	"strings"
	"sync"
	"time"
)

// GiteaAdapter is the GitPort implementation over the Gitea REST API v1
// (ADR-003, ADR-019): every call authenticates with the service account
// token, so all provider-side changes the platform makes are attributable
// to one machine identity. The adapter never touches Gitea's database —
// it talks HTTP only; the acceptance criterion "domain 不引用 Gitea DB"
// holds by construction (and the domain layer never sees this package).
type GiteaAdapter struct {
	baseURL string // no trailing slash
	token   string
	client  *http.Client

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

// NewGiteaAdapter builds the adapter on the validated configuration.
func NewGiteaAdapter(cfg Config) *GiteaAdapter {
	return &GiteaAdapter{
		baseURL: strings.TrimSuffix(cfg.BaseURL, "/"),
		token:   string(cfg.Token),
		client:  &http.Client{Timeout: 10 * time.Second},
	}
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
	CloneURL string `json:"clone_url"`
	Private  bool   `json:"private"`
}

// repository maps the provider shape onto the port type.
func (b repoBody) repository(owner, name string) Repository {
	return Repository{
		Owner:    owner,
		Name:     name,
		ID:       b.ID,
		CloneURL: b.CloneURL,
		Private:  b.Private,
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
