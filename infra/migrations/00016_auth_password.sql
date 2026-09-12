-- +goose Up
-- Email+password authentication (T0101): users.password_hash stores the
-- encoded password hash of an email+password account. NULL means the
-- account authenticates via OIDC only — both account kinds share one
-- identity space, and the credential column is deliberately opaque to
-- everything except the authn application layer (the encoding carries its
-- own parameters, so the KDF can be upgraded in place later).
ALTER TABLE users ADD COLUMN password_hash text NULL;

COMMENT ON COLUMN users.password_hash IS
  'argon2id PHC encoding ($argon2id$v=19$m=65536,t=3,p=2$...); NULL = OIDC-only account';

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
