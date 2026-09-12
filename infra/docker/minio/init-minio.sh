#!/bin/sh
# Idempotent MinIO init for local dev — runs INSIDE the minio container:
#
#   docker compose exec -T minio sh < infra/docker/minio/init-minio.sh
#   (or simply: make infra-init)
#
# Re-running is a no-op: the alias is rewritten and `mc mb --ignore-existing`
# leaves an existing bucket untouched (it never deletes or recreates).
set -eu

BUCKET="${MINIO_INIT_BUCKET:-post}"

mc alias set local http://localhost:9000 "$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD" >/dev/null
mc mb --ignore-existing "local/$BUCKET"

echo "MinIO buckets:"
mc ls local
