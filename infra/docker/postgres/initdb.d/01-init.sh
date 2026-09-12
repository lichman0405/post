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

  # Values are passed as psql variables and referenced with :"ident" (quoted
  # identifier) / :'literal' (quoted literal) so psql performs the escaping.
  # Interpolating them into the SQL text from the shell would make the script
  # injectable through an overridden environment variable.
  psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
    -v gitea_user="$GITEA_DB_USER" \
    -v gitea_password="$GITEA_DB_PASSWORD" \
    -v gitea_db="$GITEA_DB_NAME" <<-'EOSQL'
	CREATE USER :"gitea_user" WITH PASSWORD :'gitea_password';
	CREATE DATABASE :"gitea_db" OWNER :"gitea_user";
	CREATE EXTENSION IF NOT EXISTS vector;
EOSQL
)
