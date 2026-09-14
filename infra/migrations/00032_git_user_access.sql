-- +goose Up
-- Git user access (T0304): scoped git tokens, revocation and the
-- platform→GitProvider repo access mapping.
--
-- Product identity is never Gitea identity (ADR-019): a platform user who
-- wants to use a real git client gets
--
--   1. a provider-side shadow account (git_user_identities, login
--      "u-<user uuid>" — deterministic, like the repository naming rule
--      in 00022), created token-only: the platform generates an
--      unguessable password at creation and discards it, so the account
--      is usable exclusively through tokens the platform mints;
--   2. one repo access grant per project (git_repo_access): the shadow
--      account becomes a collaborator on the project's provisioned
--      repository with a permission derived from the platform role
--      (viewer → read, contributor/maintainer/owner → write);
--   3. scoped access tokens minted per grant (git_access_tokens): the
--      token carries the narrowest provider scopes (read:repository,
--      or +write:repository) and the collaborator grant above decides
--      which repository the account can reach — that pair
--      (collaborator grant + scoped token) is the "scoped git token"
--      of docs/16 §2 identity mapping. NOTE: a Gitea token is an
--      INSTANCE-scoped capability — it can reach any repository the
--      account can see, a public one included; the platform's tokens
--      are project-scoped in effect only because every platform
--      repository is private. A public-repository feature must revisit
--      this premise.
--
-- Revocation has two levels, and both keep their history (domain
-- invariant 8: nothing disappears, state only evolves):
--
--   - token revocation: the provider-side token is deleted and the row
--     moves to status 'revoked' with revoked_at — the credential dies,
--     the audit trail stays;
--   - access revocation: the collaborator grant is removed provider-side
--     (every token of that user on that repository dies with it) and
--     git_repo_access.revoked_at records when.
--
-- The token VALUE is never stored anywhere on the platform (docs/55
-- SECRET class): Gitea returns it exactly once at mint time, the API
-- hands it to the user exactly once, and only the provider-side token id
-- (gitea_token_id) is kept for revocation. Nothing here can be read back
-- into a credential.

CREATE TABLE git_user_identities (
  user_id uuid PRIMARY KEY REFERENCES users(id) ON DELETE RESTRICT,
  gitea_username text UNIQUE NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE git_user_identities IS
  'The platform→GitProvider user identity mapping (T0304): one shadow Gitea account per platform user (login "u-<user uuid>"). The account is token-only; the platform discards its creation password, so every credential the user ever receives is a scoped token from git_access_tokens.';

CREATE TABLE git_repo_access (
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  permission text NOT NULL CHECK (permission IN ('read','write')),
  granted_at timestamptz NOT NULL DEFAULT now(),
  revoked_at timestamptz,
  PRIMARY KEY (project_id, user_id),
  CHECK (revoked_at IS NULL OR revoked_at >= granted_at)
);

COMMENT ON TABLE git_repo_access IS
  'The repo access mapping (T0304): which platform user holds which Git-layer permission on which project''s provisioned repository. The provider-side enforcement is the collaborator membership this row mirrors; revoking access sets revoked_at (nothing disappears, domain invariant 8).';

COMMENT ON COLUMN git_repo_access.permission IS
  'Git-layer permission derived from the platform role: read (viewer) or write (contributor/maintainer/owner). Never wider than the platform role — Git access is a projection of platform membership, not a parallel permission system.';

CREATE TABLE git_access_tokens (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  gitea_username text NOT NULL REFERENCES git_user_identities(gitea_username) ON DELETE RESTRICT,
  token_name text NOT NULL,
  gitea_token_id bigint NOT NULL,
  scope text NOT NULL CHECK (scope IN ('read','write')),
  status text NOT NULL DEFAULT 'active' CHECK (status IN ('active','revoked')),
  issued_at timestamptz NOT NULL DEFAULT now(),
  revoked_at timestamptz,
  CHECK (revoked_at IS NULL OR revoked_at >= issued_at),
  CHECK (status = 'active' OR revoked_at IS NOT NULL)
);

COMMENT ON TABLE git_access_tokens IS
  'Issued scoped git tokens (T0304): one row per minted provider-side access token. The token value itself is never stored (docs/55 SECRET — Gitea shows it once); gitea_token_id is the revocation handle. Rows are never deleted: revocation flips status and sets revoked_at.';

COMMENT ON COLUMN git_access_tokens.scope IS
  'The Gitea scope set the token was minted with: read (read:repository) or write (read:repository + write:repository). Bounded by the user''s collaborator grants — a token can only reach repositories the user holds a git_repo_access row for.';

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
