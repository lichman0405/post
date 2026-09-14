package gitprovider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// GiteaUserAccess is the UserAccessPort implementation over the Gitea REST
// API v1 (T0304). It is a separate adapter type from GiteaAdapter because
// it acts under two identities the provisioning port never uses:
//
//   - the instance ADMIN's basic auth, for creating shadow accounts
//     (POST /admin/users) and minting/revoking their tokens. Gitea 1.27
//     rejects API-token authentication on the whole /users/{username}/
//     tokens surface ("auth required", verified against the live
//     instance) — token management is basic-auth-only there, so the
//     platform config carries the admin's credentials for exactly these
//     calls;
//   - the SERVICE ACCOUNT token, for collaborator grants — only the
//     repository owner may add/remove collaborators, and the platform's
//     repositories live in the service account's namespace (T0301).
//
// The adapter never touches Gitea's database — HTTP only, like
// GiteaAdapter (ADR-019 holds by construction).
type GiteaUserAccess struct {
	baseURL string // no trailing slash
	// svcToken authenticates collaborator calls as the repository owner
	// (the service account, T0301).
	svcToken string
	// adminUser/adminPassword authenticate account-and-token calls as the
	// instance admin (basic auth only — see above).
	adminUser     string
	adminPassword string
	client        *http.Client
}

// NewGiteaUserAccess builds the user-access adapter on the validated
// configuration (the same Config as the provisioning adapter — the admin
// fields ride on it since both adapters are wired from the same load).
func NewGiteaUserAccess(cfg Config) *GiteaUserAccess {
	return &GiteaUserAccess{
		baseURL:       strings.TrimSuffix(cfg.BaseURL, "/"),
		svcToken:      string(cfg.Token),
		adminUser:     cfg.AdminUser,
		adminPassword: string(cfg.AdminPassword),
		client:        &http.Client{Timeout: 10 * time.Second},
	}
}

// GitUserName derives the provider-side shadow login from the platform
// user id: "u-" + uuid. Deterministic (the same user always maps to the
// same account), globally unique (uuids are), and charset- and
// length-safe for the provider (38 chars) — the user→account mapping
// made mechanical, mirroring RepositoryName's project→repo rule.
func GitUserName(platformUserID string) string { return "u-" + platformUserID }

// shadowEmail derives the shadow account's email from the platform user
// id. The .invalid TLD cannot receive mail and the uuid keeps the
// provider's uniqueness constraint satisfied without ever storing (or
// reusing) a real address — the account is infrastructure, not a
// mailbox.
func shadowEmail(platformUserID string) string { return platformUserID + "@users.invalid" }

// shadowPassword generates the one-time creation password of a shadow
// account: 24 random bytes, hex-encoded, never stored and never used
// again (the account becomes token-only the moment it exists). Gitea's
// admin-create API requires a password; a discarded random one keeps it
// unknowable to everyone.
func shadowPassword() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("gitprovider: entropy source failed")
	}
	return hex.EncodeToString(b[:]), nil
}

// EnsureGitUser implements UserAccessPort: the shadow account exists or
// is created, and its login comes back. The existence check is GET
// /users/{name} as admin; creation is POST /admin/users with a discarded
// random password; the 422 race (a concurrent creator won between our
// lookup and our create) adopts the existing account.
func (a *GiteaUserAccess) EnsureGitUser(ctx context.Context, platformUserID string) (GitUser, error) {
	name := GitUserName(platformUserID)
	code, raw, err := a.callBasic(ctx, http.MethodGet, "/api/v1/users/"+urlSegment(name), nil, nil)
	if err != nil {
		return GitUser{}, err
	}
	if code == http.StatusOK {
		return GitUser{Name: name}, nil
	}
	if code != http.StatusNotFound {
		return GitUser{}, statusError(code, "look up shadow account", raw)
	}

	password, err := shadowPassword()
	if err != nil {
		return GitUser{}, err
	}
	body := map[string]any{
		"username":             name,
		"email":                shadowEmail(platformUserID),
		"password":             password,
		"must_change_password": false,
		"send_notify":          false,
	}
	code, raw, err = a.callBasic(ctx, http.MethodPost, "/api/v1/admin/users", body, nil)
	if err != nil {
		return GitUser{}, err
	}
	switch code {
	case http.StatusCreated:
		return GitUser{Name: name}, nil
	case http.StatusUnprocessableEntity, http.StatusConflict:
		// Lost a create race: the account now exists — adopt it.
		if strings.Contains(providerMessage(raw), "already exists") {
			code, raw, err := a.callBasic(ctx, http.MethodGet, "/api/v1/users/"+urlSegment(name), nil, nil)
			if err != nil {
				return GitUser{}, err
			}
			if code == http.StatusOK {
				return GitUser{Name: name}, nil
			}
			return GitUser{}, statusError(code, "adopt shadow account", raw)
		}
	}
	return GitUser{}, statusError(code, "create shadow account", raw)
}

// GrantAccess implements UserAccessPort: PUT the collaborator grant with
// the service account token — the repository owner is the only identity
// Gitea lets manage collaborators, and the platform's repositories live
// in the service account's namespace.
func (a *GiteaUserAccess) GrantAccess(ctx context.Context, repo Repository, user GitUser, level AccessLevel) error {
	body := map[string]any{"permission": string(level)}
	code, raw, err := a.callToken(ctx, http.MethodPut,
		"/api/v1/repos/"+urlSegment(repo.Owner)+"/"+urlSegment(repo.Name)+
			"/collaborators/"+urlSegment(user.Name), body, nil)
	if err != nil {
		return err
	}
	if code != http.StatusNoContent && code != http.StatusCreated {
		return statusError(code, "grant repository access", raw)
	}
	return nil
}

// RevokeAccess implements UserAccessPort: DELETE the collaborator grant.
// Gitea answers 204 for a missing grant too (verified against the live
// instance), so a gone collaborator or a gone repository is success —
// the goal state "no access" already holds.
func (a *GiteaUserAccess) RevokeAccess(ctx context.Context, repo Repository, user GitUser) error {
	code, raw, err := a.callToken(ctx, http.MethodDelete,
		"/api/v1/repos/"+urlSegment(repo.Owner)+"/"+urlSegment(repo.Name)+
			"/collaborators/"+urlSegment(user.Name), nil, nil)
	if err != nil {
		return err
	}
	if code != http.StatusNoContent && code != http.StatusOK && code != http.StatusNotFound {
		return statusError(code, "revoke repository access", raw)
	}
	return nil
}

// CreateUserToken implements UserAccessPort: POST /users/{name}/tokens
// as the admin (basic auth — Gitea 1.27 refuses API-token auth on the
// token surface). The provider answers with the credential exactly once;
// everything the platform keeps is the token id.
func (a *GiteaUserAccess) CreateUserToken(ctx context.Context, user GitUser, spec UserTokenSpec) (UserToken, error) {
	scopes := []string{"read:repository"}
	if spec.Level == AccessWrite {
		scopes = append(scopes, "write:repository")
	}
	body := map[string]any{"name": spec.Name, "scopes": scopes}
	var minted struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		SHA1 string `json:"sha1"`
	}
	code, raw, err := a.callBasic(ctx, http.MethodPost,
		"/api/v1/users/"+urlSegment(user.Name)+"/tokens", body, &minted)
	if err != nil {
		return UserToken{}, err
	}
	if code != http.StatusCreated {
		return UserToken{}, statusError(code, "mint user token", raw)
	}
	return UserToken{ID: minted.ID, Name: minted.Name, Value: minted.SHA1}, nil
}

// DeleteUserToken implements UserAccessPort: DELETE the token by id as
// the admin. A token that is already gone answers 404 — success, the
// credential is dead either way.
func (a *GiteaUserAccess) DeleteUserToken(ctx context.Context, user GitUser, tokenID int64) error {
	code, raw, err := a.callBasic(ctx, http.MethodDelete,
		fmt.Sprintf("/api/v1/users/%s/tokens/%d", urlSegment(user.Name), tokenID), nil, nil)
	if err != nil {
		return err
	}
	if code != http.StatusNoContent && code != http.StatusOK && code != http.StatusNotFound {
		return statusError(code, "revoke user token", raw)
	}
	return nil
}

// callToken performs one request authenticated with the service account
// token (the collaborator-management identity).
func (a *GiteaUserAccess) callToken(ctx context.Context, method, path string, body any, out any) (int, []byte, error) {
	return a.call(ctx, method, path, body, out, "token "+a.svcToken)
}

// callBasic performs one request authenticated with the admin's basic
// auth (the account-and-token-management identity).
func (a *GiteaUserAccess) callBasic(ctx context.Context, method, path string, body any, out any) (int, []byte, error) {
	return a.call(ctx, method, path, body, out, "",
		func(r *http.Request) { r.SetBasicAuth(a.adminUser, a.adminPassword) })
}

// call is the shared request helper. auth is the literal Authorization
// header value for token auth ("" for basic auth, which req sets); req,
// when non-nil, decorates the built request. It mirrors GiteaAdapter.call
// (deliberately duplicated rather than shared: the two adapters belong to
// different task generations and a shared helper would couple their
// review surfaces — the port contracts, not the plumbing, are the stable
// boundary).
func (a *GiteaUserAccess) call(ctx context.Context, method, path string, body any, out any, auth string, reqDecorators ...func(*http.Request)) (int, []byte, error) {
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
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, d := range reqDecorators {
		d(req)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: provider request failed", ErrUnavailable)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: read response: %v", ErrUnavailable, err)
	}
	if out != nil && len(raw) > 0 && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err := json.Unmarshal(raw, out); err != nil {
			return 0, nil, fmt.Errorf("%w: decode response", ErrUnavailable)
		}
	}
	return resp.StatusCode, raw, nil
}

// statusError maps a provider status onto the port's sentinel errors with
// the provider's own message attached (same contract as
// GiteaAdapter.mapStatus; kept package-level here so the user-access
// adapter can use it without an artificial GiteaAdapter instance).
func statusError(code int, action string, raw []byte) error {
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

// userTokenName derives a provider-side token name: the project id
// prefix (operator debugging: which project the credential belongs to)
// plus fresh entropy, because Gitea rejects duplicate token names per
// user (verified against the live instance).
func userTokenName(projectID string) (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("gitprovider: entropy source failed")
	}
	prefix := projectID
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	return "post-" + prefix + "-" + hex.EncodeToString(b[:]), nil
}
