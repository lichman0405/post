#!/usr/bin/env bash
# One-time, repeatable database bootstrap for a native Ubuntu POST host.
# Run as root after install-native-ubuntu.sh and the Go build.
set -euo pipefail
umask 077

[[ $EUID -eq 0 ]] || { echo 'Run with sudo' >&2; exit 1; }
[[ -x /srv/post/current/bin/rddev ]] || { echo 'Build /srv/post/current/bin/rddev first' >&2; exit 1; }

id -u post >/dev/null 2>&1 || useradd --system --create-home \
  --home-dir /var/lib/post --shell /usr/sbin/nologin post
install -d -o root -g post -m 0750 /etc/post
install -d -o post -g post -m 0750 /var/lib/post

secrets=/etc/post/secrets.env
if [[ ! -e $secrets ]]; then
  {
    printf 'POST_DB_PASSWORD=%s\n' "$(openssl rand -hex 24)"
    printf 'GITEA_DB_PASSWORD=%s\n' "$(openssl rand -hex 24)"
    printf 'MINIO_ROOT_USER=postminio\n'
    printf 'MINIO_ROOT_PASSWORD=%s\n' "$(openssl rand -hex 24)"
    printf 'GITEA_ADMIN_PASSWORD=%s\n' "$(openssl rand -hex 24)"
    printf 'GITEA_SECRET_KEY=%s\n' "$(openssl rand -hex 32)"
    printf 'GITEA_INTERNAL_TOKEN=%s\n' "$(openssl rand -hex 32)"
  } > "$secrets"
  chmod 0600 "$secrets"
fi
# Fixed root-only file created above.
# shellcheck disable=SC1090
source "$secrets"

create_role() {
  local role=$1 password=$2
  if [[ $(runuser -u postgres -- psql -Atqc "SELECT 1 FROM pg_roles WHERE rolname = '$role'") != 1 ]]; then
    runuser -u postgres -- psql -v ON_ERROR_STOP=1 -v role="$role" -v pass="$password" <<'SQL'
CREATE ROLE :"role" LOGIN PASSWORD :'pass';
SQL
  fi
}
create_db() {
  local database=$1 owner=$2
  if [[ $(runuser -u postgres -- psql -Atqc "SELECT 1 FROM pg_database WHERE datname = '$database'") != 1 ]]; then
    runuser -u postgres -- createdb --owner="$owner" "$database"
  fi
}

create_role post "$POST_DB_PASSWORD"
create_role gitea "$GITEA_DB_PASSWORD"
create_db post post
create_db gitea gitea
runuser -u postgres -- psql -v ON_ERROR_STOP=1 -d post <<'SQL'
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pgcrypto;
SQL

# The current repository exposes its forward-only migration runner via rddev.
# Keep the password in the process environment, never in command arguments.
export POSTGRES_TEST_ADMIN_URL="postgres://post:${POST_DB_PASSWORD}@127.0.0.1:5432/post?sslmode=disable"
/srv/post/current/bin/rddev db migrate
unset POSTGRES_TEST_ADMIN_URL
echo 'Database roles, databases, extensions, and migrations are ready.'
