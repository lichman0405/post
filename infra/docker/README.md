# Local infrastructure (Docker Compose)

POST's local development infrastructure (T0003). Infrastructure runs in
containers; **all applications stay host-native** (docs/66 §2) — the Go API,
worker, MCP server, `rddev`, the Next.js app and the Python scientific adapter
are run directly, never containerized by this stack.

Gitea here is **infrastructure only** (ADR-019, docs/68): the product's
internal Git provider, not a user-facing surface. The local Gitea UI exists
for debugging; the product never presents it.

## Quickstart

```bash
make infra-up       # docker compose up -d --wait (waits for healthy)
make infra-init     # one-shot, idempotent: MinIO bucket + Gitea admin/org/service account
make infra-down     # stop the stack (named volumes are kept — data survives)
```

`make infra-init` can be re-run any number of times: every step is
existence-checked, nothing is deleted or recreated, and a second run is a
pure no-op.

## Services, ports, credentials

All host ports bind to **127.0.0.1 only** — the stack is never reachable from
the network. Every credential below is a **DEV-ONLY default**; never reuse
any of it outside local development. Each value is overridable through an
environment variable (see [Overrides](#overrides)).

| Service | Host port (default) | Purpose | Credential (dev-only default) |
|---|---|---|---|
| PostgreSQL 16 + pgvector 0.8.6 | `127.0.0.1:5432` | canonical semantic store (docs/68) | user `postgres` / password `postgres_dev_pw`, database `post` |
| Redis 7.4 | `127.0.0.1:6379` | cache / async jobs | none (localhost-bound) |
| MinIO | `127.0.0.1:9000` (S3 API), `127.0.0.1:9001` (console) | S3-compatible blob store | `minio_dev` / `minio_dev_pw` |
| Gitea 1.27 | `127.0.0.1:3000` (web/API), `127.0.0.1:2222` (SSH) | internal Git provider (infra only, ADR-019) | admin `postadmin` / `postadmin_dev_pw` (created by `make infra-init`) |
| Mailpit | `127.0.0.1:8025` (UI), `127.0.0.1:1025` (SMTP) | dev mail catch-all (Gitea's mailer is wired to it; nothing is ever sent externally) | none |

Postgres also hosts the Gitea database (`gitea` / `gitea_dev_pw`, created by
the one-time bootstrap script `postgres/initdb.d/01-init.sh`, which also
enables the `vector` extension in the `post` database — pgvector ships
prebuilt in the image, it is not installed by any runtime script).

### What `make infra-init` creates

- **MinIO bucket** `post` (override with `MINIO_INIT_BUCKET`) — the bucket the
  application uses.
- **Gitea admin user** `postadmin` (instance admin).
- **Gitea test organization** `post-test`, owned by `postadmin`.
- **Gitea service account** `post-git-svc` — a token-only *bot user* plus a
  `svc` team (write permission) in `post-test`. Note: Gitea ≥ 1.27 removed
  native service accounts; bot users are its machine-identity mechanism, and
  org membership now goes through teams.

**No access token is created by `make infra-init`.** Minting one on demand:

```bash
GITEA_SVC_MINT_TOKEN=1 make infra-init          # prints the token once
# or, equivalently, directly:
docker compose exec --user git gitea \
  gitea admin user generate-access-token \
  --username post-git-svc --token-name local-dev \
  --scopes write:repository,write:user --raw
```

`--scopes` defaults to `all`; prefer the narrowest set the caller actually needs
(`docs/23` §8, service-token least privilege). Redirect the token straight into a
secret store — it is shown once and must never be pasted into a log, an issue or a
committed file.

> Earlier revisions minted the token as part of init and printed it to stdout,
> which put a credential into terminal scrollback and any CI log. That is why
> minting is now an explicit, opt-in step.

The admin password above is the *initial* password, set at first creation only.

## Overrides

Every host port and credential in `docker-compose.yml` defaults inline via
`${VAR:-default}`. Override with environment variables:

| Variable | Default | Variable | Default |
|---|---|---|---|
| `POSTGRES_PORT` | `5432` | `REDIS_PORT` | `6379` |
| `MINIO_API_PORT` | `9000` | `MINIO_CONSOLE_PORT` | `9001` |
| `GITEA_WEB_PORT` | `3000` | `GITEA_SSH_PORT` | `2222` |
| `MAILPIT_WEB_PORT` | `8025` | `MAILPIT_SMTP_PORT` | `1025` |
| `POSTGRES_USER` | `postgres` | `POSTGRES_PASSWORD` | `postgres_dev_pw` |
| `POSTGRES_DB` | `post` | `MINIO_ROOT_USER` | `minio_dev` |
| `MINIO_ROOT_PASSWORD` | `minio_dev_pw` | `GITEA_DB_*` | `gitea` / `gitea_dev_pw` / `gitea` |

Init-time values (admin, org, service account) are overridable too:
`GITEA_ADMIN_USER`, `GITEA_TEST_ORG`, `GITEA_SERVICE_ACCOUNT`,
`GITEA_SVC_MINT_TOKEN`, `MINIO_INIT_BUCKET`, … — see the scripts.

`infra/docker/init.sh` forwards these explicitly with `docker compose exec -e`,
because compose does **not** pass host environment into `exec` on its own. If you
add a new overridable variable, add it to the allowlist in `init.sh` as well, or it
will be silently ignored and the default will be used instead.

## Parallel Workers / isolated stacks

`docker-compose.yml` deliberately sets no `name:` — `COMPOSE_PROJECT_NAME`
comes from the environment (rddev presets it per task), so several Workers
can run isolated stacks on one host (docs/66 §3). If host ports collide, run
a second stack on its own ports:

```bash
COMPOSE_PROJECT_NAME=post-dev2 \
POSTGRES_PORT=15432 REDIS_PORT=16379 MINIO_API_PORT=19000 MINIO_CONSOLE_PORT=19001 \
GITEA_WEB_PORT=13000 GITEA_SSH_PORT=12222 MAILPIT_WEB_PORT=18025 MAILPIT_SMTP_PORT=11025 \
docker compose up -d
```

(Note: compose also auto-loads a `.env` from the project directory if one
exists later — values there take precedence over the inline defaults.)

## Persistence and reset

- All stateful data lives in named volumes (`postgres_data`, `redis_data`,
  `minio_data`, `gitea_data` — project-prefixed), so
  `docker compose down` → `docker compose up` keeps everything.
- Mailpit messages are intentionally ephemeral (in-memory by default) — dev
  mail is not app state.
- `docker compose down -v` **deletes all volumes** — a full reset. There is
  no global prune anywhere in these targets; the canonical reset commands
  remain `rddev env reset ...` (docs/66 §4).

## Image pins

Exact, immutable tags only — no `latest`:

| Image | Pin | Note |
|---|---|---|
| `pgvector/pgvector` | `0.8.6-pg16` | PostgreSQL 16 + pgvector 0.8.6 |
| `redis` | `7.4.11-alpine` | matches the pinned redis-cli 7.x (docs/64) |
| `quay.io/minio/minio` | `RELEASE.2025-09-07T16-13-09Z` | MinIO no longer publishes to Docker Hub; quay.io is the official registry |
| `gitea/gitea` | `1.27.3` | |
| `axllent/mailpit` | `v1.31.1` | |

## Troubleshooting

- **A service stays unhealthy**: `docker compose ps` shows per-service
  health; `docker compose logs <service>` for details. Healthchecks probe
  real readiness (e.g. `pg_isready`, `mc ready local`, Gitea `/api/healthz`),
  so "up but not accepting connections" never reads as healthy.
- **Port already in use**: bind another port via the override table above
  (and give the stack its own `COMPOSE_PROJECT_NAME`).
- **Gitea SMTP warning in logs** (`connecting over insecure SMTP protocol to
  non-local address is not recommended`): expected — dev mail goes to Mailpit
  in plain SMTP on purpose.
