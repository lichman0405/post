package gitprovider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// The user-access application service (T0304): one platform user → one
// provider shadow account → per-project collaborator grants → scoped
// tokens minted per grant, with two-level revocation. All policy lives
// here; the provider calls go through UserAccessPort and every canonical
// fact through AccessStore. Authorization (membership, role) happens
// BEFORE this service — the caller passes the verified level; the
// service owns the provider work and the canonical records.

// IssuedToken is what an issue call hands back: the canonical record,
// the credential itself (exactly once — it is never stored), and a
// ready-to-use clone URL.
type IssuedToken struct {
	Record TokenRecord
	// Value is the credential. It exists only in this value and in the
	// user's client: the provider reveals it once, the platform forgets
	// it (docs/55 SECRET).
	Value string
	// CloneURL embeds the credential as basic-auth userinfo — the form
	// stock git clients accept without extra configuration (the standard
	// PAT-in-URL pattern). The user must treat it like the Value.
	CloneURL string
}

// ErrTokenNotOwned: the token belongs to a different platform user (the
// revocation boundary — a user revokes their own credentials).
var ErrTokenNotOwned = errors.New("gitprovider: token not owned by caller")

// UserAccess is the scoped-token service.
type UserAccess struct {
	port    UserAccessPort
	store   AccessStore
	baseURL string
}

// NewUserAccess wires the service. baseURL is the provider's base URL
// (validated config, POST_GITEA_BASE_URL): it also serves the git smart
// HTTP transport, so the clone URL derives from it.
func NewUserAccess(port UserAccessPort, store AccessStore, baseURL string) *UserAccess {
	return &UserAccess{port: port, store: store, baseURL: strings.TrimSuffix(baseURL, "/")}
}

// IssueToken issues one scoped token for the user on the project's
// repository: ensure the shadow account, grant (or repair) the
// collaborator access, mint the scoped token, record everything
// canonically. Idempotent in effect — a redelivered call produces
// another credential for the same grant, never a second grant or a
// second account. The repository must be provisioned first (T0301's
// job; ErrRepoNotProvisioned otherwise).
//
// level is the caller's verified decision (the HTTP handler derives it
// from the platform role via the permission matrix); the service does
// not re-check membership — it cannot, membership is the project
// service's fact.
func (s *UserAccess) IssueToken(ctx context.Context, userID, projectID string, level AccessLevel) (IssuedToken, error) {
	if !projectIDRe.MatchString(projectID) || !projectIDRe.MatchString(userID) {
		return IssuedToken{}, fmt.Errorf("%w: user or project id is not a uuid", ErrConflict)
	}
	repo, err := s.store.RepoRef(ctx, projectID)
	if err != nil {
		return IssuedToken{}, err
	}
	guser, err := s.port.EnsureGitUser(ctx, userID)
	if err != nil {
		return IssuedToken{}, err
	}
	// The canonical mapping row backs the git_access_tokens.gitea_username
	// FK and makes the user→account link readable without the provider
	// (the mapping is a platform fact once the account exists).
	if err := s.store.UpsertGitIdentity(ctx, userID, guser.Name); err != nil {
		return IssuedToken{}, err
	}
	if err := s.port.GrantAccess(ctx, Repository{Owner: repo.Owner, Name: repo.Name}, guser, level); err != nil {
		return IssuedToken{}, err
	}
	if err := s.store.UpsertAccess(ctx, AccessRecord{
		ProjectID:  projectID,
		UserID:     userID,
		Permission: level,
	}); err != nil {
		return IssuedToken{}, err
	}
	name, err := userTokenName(projectID)
	if err != nil {
		return IssuedToken{}, err
	}
	tok, err := s.port.CreateUserToken(ctx, guser, UserTokenSpec{Name: name, Level: level})
	if err != nil {
		return IssuedToken{}, err
	}
	rec, err := s.store.InsertToken(ctx, TokenRecord{
		ProjectID:     projectID,
		UserID:        userID,
		GiteaUsername: guser.Name,
		TokenName:     name,
		GiteaTokenID:  tok.ID,
		Scope:         level,
		Status:        "active",
	})
	if err != nil {
		// The credential exists provider-side but can no longer be
		// recorded — destroy it best-effort so no unrecorded credential
		// survives. If the destroy fails too, the provider-side token is
		// an orphan only an admin can see; the value was never stored
		// anywhere, so it cannot leak from the platform.
		_ = s.port.DeleteUserToken(ctx, guser, tok.ID)
		return IssuedToken{}, err
	}
	cloneURL, err := s.cloneURL(repo, guser.Name, tok.Value)
	if err != nil {
		return IssuedToken{}, err
	}
	return IssuedToken{Record: rec, Value: tok.Value, CloneURL: cloneURL}, nil
}

// ListTokens returns the user's tokens (active and revoked) for one
// project, newest first. The history stays visible — revocation is a
// state change, not a deletion (domain invariant 8).
func (s *UserAccess) ListTokens(ctx context.Context, userID, projectID string) ([]TokenRecord, error) {
	if !projectIDRe.MatchString(projectID) || !projectIDRe.MatchString(userID) {
		return nil, fmt.Errorf("%w: user or project id is not a uuid", ErrConflict)
	}
	return s.store.ListTokens(ctx, projectID, userID)
}

// RevokeToken revokes one of the user's own tokens: the provider-side
// token is deleted first (that is the enforcement — a deleted token
// fails every git operation immediately), then the canonical row flips
// to revoked. Provider-first ordering makes a mid-revocation crash fail
// SECURE: the credential dies even if the canonical row lags, and a
// retry converges (the provider delete is idempotent). Already-revoked
// tokens are a no-op success.
func (s *UserAccess) RevokeToken(ctx context.Context, userID, projectID, tokenID string) error {
	// Shape-check every id BEFORE any store call (same rule as IssueToken/
	// ListTokens/RevokeAccess): a malformed token id must answer the
	// caller's 400, not a PostgreSQL uuid-cast error surfacing as a 503.
	// Well-formed-but-missing still answers ErrTokenNotFound (the
	// existence-hiding 404) — this check only guards the shape.
	if !projectIDRe.MatchString(projectID) || !projectIDRe.MatchString(userID) || !projectIDRe.MatchString(tokenID) {
		return fmt.Errorf("%w: user, project or token id is not a uuid", ErrConflict)
	}
	rec, err := s.store.GetToken(ctx, tokenID)
	if err != nil {
		return err
	}
	if rec.UserID != userID || rec.ProjectID != projectID {
		return ErrTokenNotOwned
	}
	if rec.Status != "active" {
		return nil
	}
	if err := s.port.DeleteUserToken(ctx, GitUser{Name: rec.GiteaUsername}, rec.GiteaTokenID); err != nil {
		return err
	}
	return s.store.MarkTokenRevoked(ctx, tokenID)
}

// RevokeAccess removes the user's entire git access on the project: the
// collaborator grant dies provider-side (with it, every token of the
// user on that repository — the provider enforces reach through the
// grant), then each active token is deleted and the canonical rows flip
// to revoked. Provider-first, idempotent — a retry converges from any
// mid-flight state. This is the hook future membership removal wires
// into: a user leaving a project must lose git reach at the same moment.
//
// It reports whether a LIVE grant existed (revoked == true): the caller
// owns the audit write, and a revocation that was a no-op must not
// fabricate an audit event (review finding — without this, any
// authenticated user could write a fake git.access_revoked entry on any
// provisioned project). No grant row, or an already-revoked one, is a
// clean no-op: false, nil.
func (s *UserAccess) RevokeAccess(ctx context.Context, userID, projectID string) (bool, error) {
	if !projectIDRe.MatchString(projectID) || !projectIDRe.MatchString(userID) {
		return false, fmt.Errorf("%w: user or project id is not a uuid", ErrConflict)
	}
	acc, err := s.store.GetAccess(ctx, projectID, userID)
	if errors.Is(err, ErrAccessNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if acc.RevokedAt != nil {
		return false, nil
	}
	repo, err := s.store.RepoRef(ctx, projectID)
	if err != nil {
		if errors.Is(err, ErrRepoNotProvisioned) {
			// Nothing provider-side can exist for an unprovisioned
			// project; still clear whatever canonical rows exist. A
			// grant row did exist (GetAccess above), so this IS a real
			// revocation.
			if err := s.revokeCanonicalOnly(ctx, userID, projectID); err != nil {
				return false, err
			}
			return true, nil
		}
		return false, err
	}
	guser := GitUser{Name: GitUserName(userID)}
	if err := s.port.RevokeAccess(ctx, Repository{Owner: repo.Owner, Name: repo.Name}, guser); err != nil {
		return false, err
	}
	if err := s.revokeTokens(ctx, userID, projectID); err != nil {
		return false, err
	}
	if err := s.store.MarkAccessRevoked(ctx, projectID, userID); err != nil {
		return false, err
	}
	return true, nil
}

// revokeCanonicalOnly clears canonical token rows without any provider
// work (the repository does not exist, so no credential can live).
func (s *UserAccess) revokeCanonicalOnly(ctx context.Context, userID, projectID string) error {
	if err := s.revokeTokens(ctx, userID, projectID); err != nil {
		return err
	}
	return s.store.MarkAccessRevoked(ctx, projectID, userID)
}

// revokeTokens deletes every active token of the user on the project
// provider-side and marks the rows revoked. A provider failure aborts
// (retry converges: deletes and row flips are both idempotent).
func (s *UserAccess) revokeTokens(ctx context.Context, userID, projectID string) error {
	tokens, err := s.store.ListTokens(ctx, projectID, userID)
	if err != nil {
		return err
	}
	for _, t := range tokens {
		if t.Status != "active" {
			continue
		}
		if err := s.port.DeleteUserToken(ctx, GitUser{Name: t.GiteaUsername}, t.GiteaTokenID); err != nil {
			return err
		}
		if err := s.store.MarkTokenRevoked(ctx, t.ID); err != nil {
			return err
		}
	}
	return nil
}

// cloneURL builds the ready-to-use clone URL: the provider's smart HTTP
// URL with the shadow login and the credential as basic-auth userinfo —
// the only interoperable way to hand a git client a credential. The
// token never appears in a path or query (where proxies and logs would
// collect it); it rides userinfo, which git strips from its own logs and
// history.
func (s *UserAccess) cloneURL(repo RepoRef, gitUser, value string) (string, error) {
	u, err := url.Parse(s.baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("%w: git base URL %q is not absolute", ErrConflict, s.baseURL)
	}
	u.User = url.UserPassword(gitUser, value)
	u.Path = "/" + repo.Owner + "/" + repo.Name + ".git"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}
