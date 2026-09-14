package gitprovider

import "context"

// The user-access surface (T0304): scoped git tokens, revocation and the
// repo access mapping over the internal Git transport (docs/16 §2, the
// identity-mapping half of ADR-019). It lives on its own port instead of
// extending GitPort: the calls authenticate with DIFFERENT credentials —
// the admin's basic auth for account and token management (Gitea 1.27
// rejects API-token auth on the whole /users/{username}/tokens surface)
// and the service account token for collaborator grants, which the repo
// owner alone may make. One adapter implements both ports; the
// interfaces stay separate so the two actor identities never blur.

// AccessLevel is the Git-layer permission a platform role projects onto:
// read (viewer) or write (contributor and above). The permission matrix
// owns the mapping; this type only carries it across the port boundary.
type AccessLevel string

const (
	// AccessRead grants pull/fetch only.
	AccessRead AccessLevel = "read"
	// AccessWrite grants pull and push.
	AccessWrite AccessLevel = "write"
)

// GitUser is the provider-side identity of one platform user: the shadow
// account the platform created (and owns) for them. Platform code only
// ever refers to it by the canonical mapping in git_user_identities.
type GitUser struct {
	// Name is the provider login ("u-<user uuid>").
	Name string
}

// UserTokenSpec describes the scoped token to mint for a GitUser.
type UserTokenSpec struct {
	// Name is the provider-side token name. Unique per user
	// provider-side (Gitea rejects duplicates), so callers derive it
	// with fresh entropy on every issue — it is an operator/debugging
	// label, never an identifier the canonical store relies on.
	Name string
	// Level selects the provider scope set: read → read:repository,
	// write → read:repository + write:repository.
	Level AccessLevel
}

// UserToken is the provider-side identity of one minted token.
type UserToken struct {
	// ID is the provider-side token id — the revocation handle. It is
	// what the canonical store keeps; the Value is not.
	ID int64
	// Name echoes the spec's token name.
	Name string
	// Value is the credential itself, returned EXACTLY once by the
	// provider at mint time. It is never stored platform-side (docs/55
	// SECRET): the caller hands it to the user and forgets it.
	Value string
}

// UserAccessPort is the port over the provider's account-and-access
// surface. Every method maps provider failures onto the shared sentinel
// errors (ErrNotFound, ErrConflict, ErrUnauthorized, ErrUnavailable).
type UserAccessPort interface {
	// EnsureGitUser makes sure the platform user's shadow account
	// exists and returns its login. Idempotent: the name derives
	// deterministically from the platform user id, so a concurrent (or
	// redelivered) ensure resolves to the same account — including the
	// 422 race where a concurrent creator made it between our lookup
	// and our create.
	EnsureGitUser(ctx context.Context, platformUserID string) (GitUser, error)
	// GrantAccess makes the shadow account a collaborator on the
	// repository with the given permission, or updates the permission
	// in place. Idempotent: re-granting the same level is not an
	// error, and a grant from before this task's mapping rules
	// (e.g. a manual one) is repaired to match. Fails with ErrNotFound
	// when the repository does not exist.
	GrantAccess(ctx context.Context, repo Repository, user GitUser, level AccessLevel) error
	// RevokeAccess removes the shadow account's collaborator grant on
	// the repository. Idempotent: a missing grant — or a repository
	// that no longer exists — is success (the goal state "no access"
	// already holds).
	RevokeAccess(ctx context.Context, repo Repository, user GitUser) error
	// CreateUserToken mints one scoped token for the shadow account.
	// The credential comes back in UserToken.Value — the only time the
	// provider ever reveals it.
	CreateUserToken(ctx context.Context, user GitUser, spec UserTokenSpec) (UserToken, error)
	// DeleteUserToken revokes one token provider-side. Idempotent: a
	// token that is already gone is success (the goal state "credential
	// dead" already holds).
	DeleteUserToken(ctx context.Context, user GitUser, tokenID int64) error
}
