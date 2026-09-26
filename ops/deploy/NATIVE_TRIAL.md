# Native Ubuntu trial deployment

This trial runs on Ubuntu 24.04 at `43.164.136.29`. The public endpoint is
`http://43.164.136.29/`. Nginx is the only public listener; the API, worker,
MCP server, scientific adapter, PostgreSQL, Redis, MinIO and Gitea use local
addresses. No Docker image is involved.

## Server layout

- Source and build output: `/srv/post/current` (`ubuntu` owns the checkout).
- Application services: `post-api`, `post-worker`, `post-mcp`, `post-adapter`,
  `post-web` (run as the restricted `post` user).
- Infrastructure services: `postgresql`, `redis-server`, `post-minio`,
  `post-gitea`, `nginx`.
- Application environment: `/etc/post/app.env` (`root:post`, mode 0640).
- Generated credentials: `/etc/post/secrets.env` and `/etc/post/gitea-token`
  (root-only). Never copy these into the repository or command output.
- Gitea configuration: `/etc/gitea/app.ini`; MinIO environment:
  `/etc/minio-post.env`.
- HTTP routing: `/etc/nginx/sites-available/post`.
- The `post-ip-loopback` unit makes the public IP resolve locally on the
  server, allowing Next's server-side requests to use the same API URL as
  the browser on a cloud host without public-IP hairpin routing.

## Initial order

1. Run `install-native-ubuntu.sh` to install prerequisites.
2. Clone the repository into `/srv/post/current`; build Go commands and the
   web and adapter packages.
3. Run `native-init-db.sh` (roles, databases, extensions, migrations).
4. Run `native-configure-infra.sh` (Gitea and MinIO, local only).
5. Run `native-init-gitea.sh` (admin, bot, token).
6. Run `native-configure-app.sh` (application systemd units).
7. Install `native-ip-http.nginx.conf` and `native-ip-loopback.service`.

These scripts create configuration only when it does not already exist. A
second run does not rotate credentials or overwrite operator edits.

## Checks

```bash
systemctl is-active post-api post-worker post-mcp post-adapter post-web post-minio post-gitea nginx
curl -fsS http://127.0.0.1:8080/readyz
curl -fsS http://127.0.0.1:9100/readyz
curl -fsS http://43.164.136.29/readyz
curl -fsS -o /dev/null -w '%{http_code}\n' http://43.164.136.29/login
```

## HTTP login exception

The default production cookie is `Secure`. This trial explicitly sets
`POST_AUTH_ALLOW_INSECURE_HTTP=true` in `/etc/post/app.env`, which permits
session cookies over HTTP when `POST_WEB_ORIGIN` is also HTTP. Remove the
setting before a public HTTPS deployment. Passwords and sessions cross the
network unencrypted during this trial.

## Limits before production use

The current repository has no production backup destination or email
transport. The Gitea bot has an internal token with broad scopes for the
trial. Set up off-host backups, a mail provider, tighter token scopes and
HTTPS before relying on this deployment for real user data.
