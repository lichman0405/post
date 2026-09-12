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

"$HERE/wait-healthy.sh" 300

echo "== MinIO init =="
docker compose exec -T minio sh < "$HERE/minio/init-minio.sh"

echo "== Gitea init =="
docker compose exec -T --user git gitea sh < "$HERE/gitea/init-gitea.sh"

echo "init complete (idempotent — safe to re-run)"
