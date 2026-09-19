# Staging deployment — runbook and template (T1203)

This directory is the staging deployment template: the Compose orchestration,
the environment contract, the TLS reverse proxy, and the order to bring them
up in. It follows the deployment sequence the specifications already fix —
`docs/35_DEPLOYMENT_RUNBOOK.md:9-11` for the order, `:17` for rollback,
`docs/25_CICD_DEVOPS.md:32` for immutable image digests and the staging-first
rule — rather than inventing a process of its own.

> **Read this before planning a deployment: nothing here can deploy yet.**
> There is no `Dockerfile` anywhere in this repository — `find . -iname
> 'Dockerfile*'` returns nothing — so none of the images the compose file
> names exist, and `docker compose up` cannot start anything. What exists is
> the orchestration and its contract: what runs, in what order, with which
> secrets, behind which TLS terminator, waited on by which healthcheck. §
> [What is missing](#what-is-missing) names each gap and who owns it. Until
> those are closed, **staging is not deployable and this runbook has never
> been executed end to end**; the checks that do pass are static and are
> named as such.

| File | What it is |
| --- | --- |
| `docker-compose.staging.yml` | The deployment: migrations job, API, worker, MCP, scientific adapter, web, TLS proxy, and the four infrastructure services behind the `bundled-infra` profile. |
| `staging.env.example` | Every variable the compose file reads, with placeholders. Copy to `.env` (git-ignored) and fill it in. |
| `reverse-proxy/nginx.conf` | TLS termination and routing. Mounted as `/etc/nginx/conf.d/default.conf`. |
| `validate-staging-compose.py` | The checks below, as a command. `--selftest` proves each one can fail. |
| `tests/acceptance/deploy-staging-smoke.sh` | The `deployment smoke` gate: runs the validator, the self-test and the runbook checks. |

## Image contract

The compose file runs images this repository does not build yet. When they are
built, they must satisfy what the orchestration already assumes:

- **Nothing runs as root**, and the processes do not need to write to their
  own image.
- **The Go images contain `curl`.** Every healthcheck probes over HTTP with
  `curl -fsS`; `-f` is deliberate, so a 503 is a failure. The proxy image is
  the exception: it uses busybox `wget`, which is why it must be the
  alpine-family nginx (see `POST_PROXY_IMAGE`).
- **The migrate image contains a forward-only migration runner** and nothing
  the job does not need — see [What is missing](#what-is-missing).
- **Images are referenced by digest**, never by a moving tag
  (`docs/25_CICD_DEVOPS.md:32`). The compose file refuses to interpret without
  a value; the validator refuses a value that is not `@sha256:<64 hex>`.
- **Configuration comes from the environment**, validated at startup and
  failing closed (the Go loader already refuses to start on a missing or
  ambiguous value; the web app and the adapter do the same independently).

## Prerequisites

`docs/35_DEPLOYMENT_RUNBOOK.md:4` — domain, TLS, PostgreSQL, Redis, S3, Gitea,
SMTP/email provider, OIDC credentials, secret manager. Concretely:

- **Domain and DNS** pointing at the staging host, and a certificate + key on
  that host at `POST_TLS_DIR` (`tls.crt`, `tls.key`).
- **PostgreSQL 16 with pgvector 0.8.6**, the application database and role
  already created, and a migration role that may `CREATE EXTENSION` — the
  first migration is `00001_extensions.sql`. The bundled profile runs
  `pgvector/pgvector:0.8.6-pg16`, the same image the local stack and CI use.
- **Redis** with persistence configured. It holds the job queue and the
  idempotency markers; a job lost there is a job that never runs.
- **An S3-compatible bucket**, with the access policy and CORS rules the web
  origin needs (`docs/35:13`).
- **Gitea** — internal git infrastructure, ADR-019 — plus an access token
  (`POST_GITEA_TOKEN`). Its own database and role live in the same PostgreSQL.
- **OIDC credentials** if OIDC login is wanted. Optional: the API starts with
  OIDC off and says so.
- **A secret manager or an equivalent** for the values in `ops/deploy/.env`.
  That file is git-ignored; the values in it are not in this repository and
  must not be.

## Deploy order

Follow `docs/35_DEPLOYMENT_RUNBOOK.md:9-11`. The steps below are the same
sequence, and the compose file encodes it as `depends_on`, so the order is
enforced by the orchestration and not by remembering the list.

Everything runs from the repository root, with the environment file in place:

```bash
cp ops/deploy/staging.env.example ops/deploy/.env
$EDITOR ops/deploy/.env          # every placeholder replaced
python3 ops/deploy/validate-staging-compose.py   # the template still agrees with itself
```

**1. Back up / confirm the database** (`docs/35:8`). Take a restore point
before anything touches the schema. Note the state of this today — see
[What is missing](#what-is-missing): there is no production backup entry point
in the tree, only the restore *drill* (`ops/backup-restore-drill.sh`), so this
step is scoped to whatever backup your PostgreSQL provider performs.

**2. Migrations job.** It runs to head and exits; nothing else starts until it
exits zero.

```bash
docker compose -f ops/deploy/docker-compose.staging.yml run --rm migrate
```

**3. API, worker, MCP, scientific adapter** (`docs/35:10`):

```bash
docker compose -f ops/deploy/docker-compose.staging.yml up -d --wait api worker mcp adapter
```

**4. Web and the proxy** (`docs/35:11`):

```bash
docker compose -f ops/deploy/docker-compose.staging.yml up -d --wait web proxy
```

The proxy starts last because it is the only published surface: it waits for
the API and the web app to report healthy, so no traffic reaches a
half-started deployment.

Two details of steps 3 and 4 that are easy to be surprised by, and are the
compose file working as intended:

- **Compose runs the `migrate` job again as a dependency** of step 3 before it
  starts anything. That is deliberate rather than wasteful: the job is
  idempotent and reports how many migrations it applied (`0` when the database
  is already at head), so the guarantee "nothing is deployed against an
  unmigrated schema" holds for every `up`, not only for the one that followed
  this runbook in order.
- **`up -d --wait` on a partially running project only waits for what it was
  asked to start**, which is why the steps are separated: an operator who wants
  to stop between "schema migrated" and "traffic served" can, and that is
  exactly the moment to take the backup checkpoint of step 1 if it was not
  already taken.

A self-contained staging box can bring the four infrastructure services up in
the same project instead of pointing at managed ones — the same images and
pinned tags the local stack uses:

```bash
docker compose -f ops/deploy/docker-compose.staging.yml --profile bundled-infra up -d --wait
```

**5. Gitea webhook and service account** (`docs/35:12`). Check that the API's
webhook endpoint is reachable *from Gitea* — the compose file defaults it to
`http://api:8080/api/v1/git/hooks/gitea` for the bundled case. A webhook Gitea
cannot reach is a webhook that is accepted and never delivered.

**6. Object storage policy and CORS** (`docs/35:13`) — bucket policy, and CORS
allowing the `POST_WEB_ORIGIN`.

**7. Smoke** (`docs/35:14`) — health, auth, public, private, branch, PR,
upload, search. From outside the deployment, through the proxy:

```bash
curl -fsS https://staging.example.com/healthz
curl -fsS https://staging.example.com/readyz
curl -fsS https://staging.example.com/            # the web app's status page
```

**8. Canonical minimal E2E** (`docs/35:15`).

## Secrets

- **Nothing secret is in this repository.** `staging.env.example` carries
  placeholders only, and that is enforced: every committed `*.env.example` is
  swept for secret-shaped values by `internal/config/secretscan.go` on every
  `go test ./...`, and the `deployment smoke` gate runs that scan.
- **The deployed values live in `ops/deploy/.env`** — the file Compose reads
  for the `${...}` substitutions, and the file the containers get their
  environment from. It is covered by the repository `.gitignore`, is never
  committed, and should be readable only by the account that deploys.
- **A `${VAR:?reason}` in the compose file has no default.** Leaving the key
  out is a hard failure that names the variable; there is no fallback to a dev
  value and no silent empty string.
- **The DSN is passed in the environment, never on a command line**, because a
  command line is visible in `docker inspect`. The migrate job reads it from
  `POSTGRES_TEST_ADMIN_URL`, the variable the existing runner reads.
- **The TLS private key stays outside the repository** and is mounted
  read-only; nothing in this tree holds one.
- **What this does not yet give you**: delivery from a secret manager
  (Vault, SOPS, a cloud secret store) into the container. The values have to
  be materialised into `ops/deploy/.env` on the host by whatever tool the
  operator already uses. Compose's own `secrets:` mechanism is deliberately
  not used: the Go loader has no `*_FILE` support, so a file-mounted secret
  would look configured and reach nothing.

## TLS and the reverse proxy

`reverse-proxy/nginx.conf` is the whole public surface.

- **TLS terminates at the proxy**, TLS 1.2 and 1.3 only, HSTS for a year, and
  port 80 does nothing but redirect (plus the ACME http-01 challenge path,
  served from a shared volume).
- **Staging runs `POST_ENV=prod`, so the session cookie is `Secure`.** That is
  why `POST_WEB_ORIGIN` must be an `https://` origin: a browser will not send a
  `Secure` cookie over plain http, and a login that appears to succeed and
  never sticks is the symptom.
- **Only the proxy publishes ports.** PostgreSQL, Redis, MinIO, Gitea, the API,
  the worker, MCP and the adapter are reachable on the compose network and
  nowhere else; the validator refuses a stack where that stops being true. The
  Gitea UI is not routed at all — it is infrastructure, never a user-facing
  surface (ADR-019).
- **Upstream addresses are resolved through Docker's DNS with a 10s TTL**, not
  bound at startup. A literal `proxy_pass http://api:8080` keeps the first
  resolved IP forever, so the rollback step below (which recreates the API
  container, and with it its address) would leave the proxy dialling a dead
  container until somebody restarted it.
- **The proxy's own healthcheck does not go through TLS or through the public
  hostname.** It is a loopback-only endpoint on port 8081 inside the container,
  never published. What it proves is bounded and stated below.

## Healthchecks

Every long-running service has one, or a written-down reason why not. The
compose file's `x-post-healthcheck-waivers` block is that reason; the validator
refuses a service that has neither, and refuses a waiver that has gone stale.

| Service | Probes | What a passing check proves | What it does not |
| --- | --- | --- | --- |
| `api` | `/readyz` | The API reports itself ready — PostgreSQL and Redis probed, `503 not_ready` when a dependency is down, never a false `ok` | That the schema is migrated, or that the web app can reach it |
| `mcp` | `/readyz` | Same contract, MCP surface | — |
| `adapter` | `/readyz` | The scientific adapter answers | That pymatgen can load a real structure |
| `web` | `/` (the status page) | The Next process answered and its validated configuration loaded — a missing `API_BASE_URL` is a 500, which `curl -f` reports as a failure | That the API is up: that page renders "down" instead of failing |
| `proxy` | loopback `:8081/healthz-proxy` | nginx is serving | Anything about the upstreams, TLS, or the public hostname |
| `postgres`, `redis`, `minio`, `gitea` | their own readiness tools | The service is up (bundled profile only) | — |
| `worker` | **none** | — | See below |
| `migrate` | none, by design | It is a one-shot job; `up --wait` and the `service_completed_successfully` dependencies read its exit code, which is a stronger statement than a liveness probe | — |

**The worker has no healthcheck, deliberately.** `cmd/worker` exposes no HTTP
surface — no `/healthz`, no `/readyz` — and writes no heartbeat key to Redis;
the only key it writes is the per-job idempotency marker, which a wedged worker
stops writing but an idle worker never writes either. That leaves
process-liveness (`pgrep`), which answers healthy while every consumer
goroutine is stuck in a retry loop. The honest state is therefore "running",
covered by `restart: unless-stopped` plus the queue step of the runbook smoke,
and named in the waiver rather than papered over. Giving the worker a real
health surface is a code change in `cmd/worker`, outside this task's scope.

## Rollback

`docs/35_DEPLOYMENT_RUNBOOK.md:17`: **the application rolls back to the
previous image; the database only moves forward.**

```bash
# Roll the API (or worker / mcp / web / adapter) back to the previous digest:
$EDITOR ops/deploy/.env            # POST_API_IMAGE=<previous digest>
docker compose -f ops/deploy/docker-compose.staging.yml up -d --no-deps api
```

Because the proxy resolves upstreams through Docker's DNS rather than binding
their addresses at startup, the recreated container is picked up within the
resolver's 10s TTL; no proxy restart is needed.

**A migration is not rolled back.** There is no down-migration and there will
not be one: forward-only is the invariant (`docs/35:17`, CLAUDE.md §8). If a
migration breaks compatibility, the runbook's answer is to stop the traffic and
repair it under an ADR — never to hand-patch production SQL and move on. This
is also why the migrate job is `restart: "no"`: a failing migration stops the
deployment instead of being retried in a loop against a half-applied schema.

## Verifying the template

```bash
python3 ops/deploy/validate-staging-compose.py             # 19 rules over the three files
python3 ops/deploy/validate-staging-compose.py --selftest  # ... and proof each rule can fail
bash tests/acceptance/deploy-staging-smoke.sh              # the gate; includes both of the above
```

The rules check the things that rot silently: a `depends_on` that no longer
expresses the runbook's order, a healthcheck that cannot fail (a `curl` without
`-f` treats the API's `503 not_ready` as success), a `:?` variable nobody
documented, an image pinned by tag instead of digest, a TLS path that disagrees
with what the proxy mounts, a secret written into a committed file.

## What is missing

Named rather than implied. Every item below is a reason **this runbook cannot
yet be executed**, with the owner it needs:

1. **Application images — and therefore a `Dockerfile` for each service.** Six
   images are referenced (`api`, `worker`, `mcp`, `web`, `adapter`, `migrate`)
   and none exists; there is no `Dockerfile` in the repository at all. This is
   the blocker for everything else, and it is a **separate task**: it needs
   `cmd/**`, `apps/**`, `services/**` and a repository-root build context,
   none of which this task's `allowed_scope` covers. Provider: the image/build
   task that owns the Dockerfiles, plus CI's container build/SBOM stage
   (`docs/25_CICD_DEVOPS.md:24` "container build/SBOM", which also does not
   exist yet).
2. **A migration runner that is not the orchestrator CLI.** The only
   forward-only runner in the tree is `rddev db migrate`, and `rddev` is the
   Supervisor's orchestration binary — it carries the git control plane and
   Worker management. Shipping that into a staging host to apply migrations is
   the wrong dependency to bless; a `cmd/migrate` that does one thing is the
   right owner. `cmd/**` is outside this task's scope.
3. **A production backup entry point.** `docs/35_DEPLOYMENT_RUNBOOK.md:8` makes
   "back up / confirm the DB" step 1. The tree has the *restore drill*
   (`cmd/api/backupdr`, `ops/backup-restore-drill.sh`) and no scheduled or
   on-demand production backup. Provider: a backup/DR task.
4. **An email transport.** `docs/35:4` lists an SMTP/email provider as a
   prerequisite, and `internal/application/notifications/ports.go:72` says
   production SMTP "is a transport this port is waiting for". The only
   implementation is the development file sink, which writes whole plaintext
   notification bodies to a directory — so the compose file leaves email
   **off** by default rather than pointing that sink at a staging volume and
   calling it email delivery.
5. **`X-Forwarded-Proto` support in the API.** TLS terminates at the proxy, so
   the API sees plain http. `cmd/api/authhttp/auth_handler.go:250-256`
   (`schemeHost`) derives the OIDC `redirect_uri` from `r.TLS`, and nothing in
   the Go core reads `X-Forwarded-Proto` (grep: zero hits). Behind this proxy
   the callback URL is therefore built as `http://…`, which a strict OIDC
   provider rejects as a redirect-uri mismatch. The proxy sends the header
   anyway because it is the correct thing to send; the API does not read it
   yet. **Until this is fixed, OIDC login cannot be enabled on this
   topology** — the workaround is to leave OIDC off, or to configure the
   provider to accept the http scheme. Provider: an auth task (and it is a
   security-adjacent change, so it is not this task's to make).
6. **This runbook has not been executed.** With no images there is nothing to
   bring up, so the steps above are unverified as a sequence. The gate that
   does run — `tests/acceptance/deploy-staging-smoke.sh` — is **static**: it
   proves the template is internally consistent, spec-conformant and
   fail-closed, and that each of its rules can fail. It does not prove a
   deployment works, and nothing in this directory claims it does.

## See also

- `docs/35_DEPLOYMENT_RUNBOOK.md` — the deployment, backup and rollback policy
  this directory implements.
- `docs/25_CICD_DEVOPS.md` — immutable digests, staging-before-production, and
  the CI gate list.
- `docs/37_BACKUP_DR.md`, `ops/backup-restore-drill.sh` — the restore drill.
- `infra/docker/README.md` — the local development stack, which is a different
  thing with different credentials on purpose.
