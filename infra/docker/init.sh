#!/usr/bin/env bash
# POST one-command, idempotent infrastructure init (T0003).
#
#   make infra-init                 # recommended
#   bash infra/docker/init.sh       # equivalent
#
# Requires the stack to be up (`make infra-up` / `docker compose up -d`).
# Creates — without ever deleting or recreating:
#   - the MinIO bucket the app uses (default: post),
#   - the Gitea admin user, test organization, and service account.
# Re-running is a no-op.
#
# Both init scripts run inside their service containers via `docker compose
# exec`, so no host-side HTTP client or extra image is needed.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# The init scripts run INSIDE their containers, and `docker compose exec` does
# not forward host environment variables. Without this allowlist the documented
# `VAR=... make infra-init` overrides would be silently ignored — the script
# would appear to accept them and quietly use the defaults instead.
forward_env() { # forward_env VAR... -> -e flags for every variable that is set
  local v
  for v in "$@"; do
    if [[ -n "${!v:-}" ]]; then
      printf '%s\0' -e "$v=${!v}"
    fi
  done
}

"$HERE/wait-healthy.sh" 300

echo "== MinIO init =="
mapfile -d '' MINIO_ENV < <(forward_env MINIO_INIT_BUCKET)
docker compose exec -T "${MINIO_ENV[@]}" minio sh < "$HERE/minio/init-minio.sh"

echo "== Gitea init =="
mapfile -d '' GITEA_ENV < <(forward_env \
  GITEA_ADMIN_USER GITEA_ADMIN_PASSWORD GITEA_ADMIN_EMAIL \
  GITEA_TEST_ORG GITEA_SERVICE_ACCOUNT GITEA_SERVICE_ACCOUNT_EMAIL \
  GITEA_SVC_TEAM GITEA_SVC_MINT_TOKEN GITEA_SVC_TOKEN_SCOPES)
docker compose exec -T "${GITEA_ENV[@]}" --user git gitea sh < "$HERE/gitea/init-gitea.sh"

echo "init complete (idempotent — safe to re-run)"
