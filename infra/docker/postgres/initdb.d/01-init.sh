#!/bin/bash
# One-time Postgres bootstrap (T0003) — runs only on a fresh data volume:
# docker-entrypoint-initdb.d scripts are sourced by the stock Postgres
# entrypoint during initial database creation and never re-run afterwards,
# so `docker compose up` stays a no-op on existing volumes.
#
# Creates the DEV-ONLY Gitea role/database (values come from the compose
# environment, see docker-compose.yml) and enables pgvector in the
# application database (the extension ships prebuilt in the
# pgvector/pgvector image).
#
# Sourced by the entrypoint: do not `exit` from this file. The errexit guard
# is scoped to a subshell so a failure aborts initialization without leaking
# shell options into the entrypoint.
(
  set -euo pipefail

  psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-EOSQL
	CREATE USER ${GITEA_DB_USER} WITH PASSWORD '${GITEA_DB_PASSWORD}';
	CREATE DATABASE ${GITEA_DB_NAME} OWNER ${GITEA_DB_USER};
	CREATE EXTENSION IF NOT EXISTS vector;
EOSQL
)
