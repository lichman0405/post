#!/usr/bin/env bash
# Configure local-only MinIO and Gitea on the native Ubuntu POST host.
set -euo pipefail
umask 077
[[ $EUID -eq 0 ]] || { echo 'Run with sudo' >&2; exit 1; }
[[ -f /etc/post/secrets.env ]] || { echo 'Run native-init-db.sh first' >&2; exit 1; }
# Generated root-only file.
# shellcheck disable=SC1091
source /etc/post/secrets.env

if [[ ! -e /etc/minio-post.env ]]; then
  {
    printf 'MINIO_ROOT_USER=%s\n' "$MINIO_ROOT_USER"
    printf 'MINIO_ROOT_PASSWORD=%s\n' "$MINIO_ROOT_PASSWORD"
    printf 'MINIO_BROWSER=off\n'
  } > /etc/minio-post.env
  chown root:minio /etc/minio-post.env
  chmod 0640 /etc/minio-post.env
fi

if [[ ! -e /etc/gitea/app.ini ]]; then
  cat > /etc/gitea/app.ini <<INI
APP_NAME = POST Git Infrastructure
RUN_MODE = prod
APP_DATA_PATH = /var/lib/gitea/data

[database]
DB_TYPE = postgres
HOST = 127.0.0.1:5432
NAME = gitea
USER = gitea
PASSWD = ${GITEA_DB_PASSWORD}
SSL_MODE = disable

[server]
PROTOCOL = http
HTTP_ADDR = 127.0.0.1
HTTP_PORT = 3000
DOMAIN = localhost
ROOT_URL = http://127.0.0.1:3000/
DISABLE_SSH = true
OFFLINE_MODE = true
LFS_START_SERVER = true
LFS_JWT_SECRET = $(openssl rand 32 | base64 -w0 | tr '+/' '-_' | tr -d '=')

[security]
INSTALL_LOCK = true
SECRET_KEY = ${GITEA_SECRET_KEY}
INTERNAL_TOKEN = ${GITEA_INTERNAL_TOKEN}
ALLOWED_HOST_LIST = loopback,private

[service]
DISABLE_REGISTRATION = true
REQUIRE_SIGNIN_VIEW = true

[oauth2]
JWT_SECRET = $(openssl rand 32 | base64 -w0 | tr '+/' '-_' | tr -d '=')
INI
  chown root:gitea /etc/gitea/app.ini
  chmod 0640 /etc/gitea/app.ini
fi

if [[ ! -e /etc/systemd/system/post-minio.service ]]; then
  cat > /etc/systemd/system/post-minio.service <<'UNIT'
[Unit]
Description=POST local MinIO object storage
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=minio
Group=minio
EnvironmentFile=/etc/minio-post.env
WorkingDirectory=/var/lib/minio
ExecStart=/usr/local/bin/minio server /var/lib/minio/data --address 127.0.0.1:9000 --console-address 127.0.0.1:9001
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
UNIT
fi

if [[ ! -e /etc/systemd/system/post-gitea.service ]]; then
  cat > /etc/systemd/system/post-gitea.service <<'UNIT'
[Unit]
Description=POST local Gitea Git service
After=network-online.target postgresql.service
Wants=network-online.target
Requires=postgresql.service

[Service]
Type=simple
User=gitea
Group=gitea
Environment=HOME=/var/lib/gitea
WorkingDirectory=/var/lib/gitea
ExecStart=/usr/local/bin/gitea --work-path /var/lib/gitea --config /etc/gitea/app.ini web
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
UNIT
fi

systemctl daemon-reload
systemctl enable --now post-minio.service post-gitea.service
systemctl is-active --quiet post-minio.service
systemctl is-active --quiet post-gitea.service
for ((attempt=1; attempt<=30; attempt++)); do
  if curl -fsS http://127.0.0.1:9000/minio/health/ready >/dev/null; then break; fi
  sleep 1
done
mc_config=$(mktemp -d)
trap 'rm -rf "$mc_config"' EXIT
MC_CONFIG_DIR="$mc_config" mc alias set post-local http://127.0.0.1:9000 \
  "$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD" >/dev/null
MC_CONFIG_DIR="$mc_config" mc mb --ignore-existing post-local/post
echo 'MinIO and Gitea services started on loopback addresses.'
