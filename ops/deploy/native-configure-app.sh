#!/usr/bin/env bash
# Configure POST services for the temporary native HTTP trial.
# Internal services still bind loopback; nginx is the only public listener.
set -euo pipefail
umask 077
[[ $EUID -eq 0 ]] || { echo 'Run with sudo' >&2; exit 1; }
[[ -s /etc/post/gitea-token && -f /etc/post/secrets.env ]] || {
  echo 'Initialize database and Gitea token first' >&2; exit 1;
}
# Generated root-only file.
# shellcheck disable=SC1091
source /etc/post/secrets.env

if [[ ! -e /etc/post/app.env ]]; then
  gitea_token=$(tr -d '\r\n' < /etc/post/gitea-token)
  cat > /etc/post/app.env <<ENV
POST_ENV=prod
POST_API_ADDR=127.0.0.1:8080
POST_MCP_ADDR=127.0.0.1:9080
POST_DB_HOST=127.0.0.1
POST_DB_PORT=5432
POST_DB_USER=post
POST_DB_PASSWORD=${POST_DB_PASSWORD}
POST_DB_NAME=post
POST_DB_SSLMODE=disable
POST_REDIS_ADDR=127.0.0.1:6379
POST_BLOB_ENDPOINT=http://127.0.0.1:9000
POST_BLOB_ACCESS_KEY=${MINIO_ROOT_USER}
POST_BLOB_SECRET_KEY=${MINIO_ROOT_PASSWORD}
POST_BLOB_BUCKET=post
POST_BLOB_USE_TLS=false
POST_GITEA_BASE_URL=http://127.0.0.1:3000
POST_GITEA_TOKEN=${gitea_token}
POST_GITEA_WEBHOOK_URL=http://127.0.0.1:8080/api/v1/git/hooks/gitea
POST_GITEA_ADMIN_USER=postadmin
POST_GITEA_ADMIN_PASSWORD=${GITEA_ADMIN_PASSWORD}
POST_WEB_ORIGIN=http://43.164.136.29
API_BASE_URL=http://43.164.136.29
POST_AUTH_ALLOW_INSECURE_HTTP=true
SCIENTIFIC_ADAPTER_URL=http://127.0.0.1:9100
POST_SCIENTIFIC_ADAPTER_HOST=127.0.0.1
POST_SCIENTIFIC_ADAPTER_PORT=9100
ENV
  chown root:post /etc/post/app.env
  chmod 0640 /etc/post/app.env
fi

install_unit() {
  local name=$1 description=$2 command=$3 working_dir=$4 after=$5
  local unit="/etc/systemd/system/${name}.service"
  [[ ! -e $unit ]] || return 0
  cat > "$unit" <<UNIT
[Unit]
Description=$description
After=network-online.target postgresql.service redis-server.service post-minio.service post-gitea.service $after
Wants=network-online.target

[Service]
Type=simple
User=post
Group=post
Environment=HOME=/var/lib/post
EnvironmentFile=/etc/post/app.env
WorkingDirectory=$working_dir
ExecStart=$command
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
UNIT
}

install_unit post-api 'POST API' '/srv/post/current/bin/api' \
  /srv/post/current ''
install_unit post-worker 'POST background worker' '/srv/post/current/bin/worker' \
  /srv/post/current 'post-api.service'
install_unit post-mcp 'POST MCP server' '/srv/post/current/bin/mcp-server' \
  /srv/post/current 'post-api.service'
install_unit post-adapter 'POST scientific adapter' \
  '/srv/post/current/services/scientific-adapter/.venv/bin/scientific-adapter' \
  /srv/post/current/services/scientific-adapter ''
install_unit post-web 'POST web application' \
  '/usr/local/bin/node /srv/post/current/apps/web/node_modules/next/dist/bin/next start --hostname 127.0.0.1 --port 3001' \
  /srv/post/current/apps/web 'post-api.service post-adapter.service'

# Next writes cache beneath .next while serving. The source tree stays owned
# by the deploy user, and only this ignored build output belongs to post.
chown -R post:post /srv/post/current/apps/web/.next
systemctl daemon-reload
systemctl enable --now post-api post-worker post-mcp post-adapter post-web
echo 'POST application services started on loopback addresses.'
