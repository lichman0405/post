#!/usr/bin/env bash
# ops/seed-demo.sh — the one command that turns an empty database into the
# demo project docs/34_SEED_DEMO_PROJECT.md describes.
#
#   ops/seed-demo.sh                 # build into $SEED_DEMO_DB (default post_seed_demo)
#   ops/seed-demo.sh --reset         # drop and recreate that database first
#   ops/seed-demo.sh --external=0    # skip the fork-dependent part
#   ops/seed-demo.sh --db post       # build into the repository's default dev database
#
# What it does, in order:
#   1. checks PostgreSQL, Redis, Gitea and MinIO are listening (they are
#      infrastructure: docker compose up -d, docs/66);
#   2. mints a Gitea service token through Gitea's own API — the fork path is
#      disabled without one, and the external contribution needs it;
#   3. [--reset] drops and recreates the target database (seeddemo reset says
#      exactly what it deletes);
#   4. runs the product's own migrations (rddev db migrate);
#   5. builds and starts cmd/api host-native against that database;
#   6. runs the seed driver (tests/acceptance/seeddemo build) — every write it
#      makes is an HTTP call to that API;
#   7. runs the verifier (seeddemo verify) — every count it reports is a SQL
#      query against PostgreSQL, never the builder's return value;
#   8. stops the API and prints one machine-readable JSON summary on stdout.
#
# Exit code: 0 when every stage of the build completed and every check of the
# verifier passed. A refusal the plan ASKED for is not a failure and does not
# change the exit code: docs/34 asks the demo to carry a protocol scientific
# conflict, and the product's own refusal to merge it is what proves the
# conflict exists (it is reported under build_refusals, not build_failures).
# The summary is the last line of stdout; everything else goes to stderr, so
# `ops/seed-demo.sh | jq .` works.
#
# Everything in the demo is fabricated. See examples/seed-demo/README.md.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# --- configuration ---------------------------------------------------------
DB_NAME="${SEED_DEMO_DB:-post_seed_demo}"
DB_HOST="${POST_DB_HOST:-127.0.0.1}"
DB_PORT="${POST_DB_PORT:-5432}"
DB_USER="${POST_DB_USER:-postgres}"
DB_PASSWORD="${POST_DB_PASSWORD:-postgres_dev_pw}"
REDIS_ADDR="${POST_REDIS_ADDR:-127.0.0.1:6379}"
BLOB_ENDPOINT="${POST_BLOB_ENDPOINT:-http://127.0.0.1:9000}"
GITEA_BASE="${POST_GITEA_BASE_URL:-http://127.0.0.1:3000}"
GITEA_ADMIN_USER="${GITEA_ADMIN_USER:-postadmin}"
GITEA_ADMIN_PASSWORD="${GITEA_ADMIN_PASSWORD:-postadmin_dev_pw}"
API_ADDR="${SEED_DEMO_API_ADDR:-}"
PLAN="$ROOT/examples/seed-demo/demo-plan.json"
EXTERNAL=1
RESET=0
# Where the machine-readable summary lands. Overridable so a run that is NOT
# the demo's own (tests/acceptance/seeddemo/mutation-check.sh builds a
# deliberately short plan to prove the verifier can fail) cannot overwrite the
# summary of the last real one.
SUMMARY="${SEED_DEMO_SUMMARY:-$ROOT/.seed-demo-summary.json}"

while [ $# -gt 0 ]; do
  case "$1" in
    --reset)     RESET=1 ;;
    --external=*) EXTERNAL="${1#*=}" ;;
    --external)  EXTERNAL="$2"; shift ;;
    --db)        DB_NAME="$2"; shift ;;
    --plan)      PLAN="$2"; shift ;;
    --api-addr)  API_ADDR="$2"; shift ;;
    -h|--help)   sed -n '2,40p' "$0"; exit 0 ;;
    *) echo "seed-demo: unknown argument $1" >&2; exit 2 ;;
  esac
  shift
done

log() { echo "seed-demo: $*" >&2; }
die() { log "$*"; exit 1; }

DB_URL="postgres://${DB_USER}:${DB_PASSWORD}@${DB_HOST}:${DB_PORT}/${DB_NAME}?sslmode=disable"

# --- 1. preflight ----------------------------------------------------------
port_open() { (exec 3<>"/dev/tcp/$1/$2") 2>/dev/null && exec 3<&- && exec 3>&-; }

for probe in "PostgreSQL $DB_HOST $DB_PORT" "Redis ${REDIS_ADDR%%:*} ${REDIS_ADDR##*:}" \
             "Gitea 127.0.0.1 3000" "MinIO 127.0.0.1 9000"; do
  set -- $probe
  name="$1"; host="$2"; port="$3"
  port_open "$host" "$port" || die "$name is not listening on $host:$port — bring the infrastructure up first (docker compose up -d)"
done
if [ "$GITEA_BASE" != "http://127.0.0.1:3000" ]; then
  log "note: POST_GITEA_BASE_URL=$GITEA_BASE, but the preflight probed 127.0.0.1:3000"
fi

# --- 2. Gitea service token ------------------------------------------------
# Minted through Gitea's API rather than the container CLI: the product's
# fork path needs a token for its GitProvider actor, and a demo script that
# required docker access could not run where the demo runs (host-native,
# docs/66 §2). DEV-ONLY credentials, the same defaults docker-compose.yml
# ships; override with GITEA_ADMIN_USER / GITEA_ADMIN_PASSWORD.
TOKEN_NAME="seed-demo-$(date +%s)"
GITEA_TOKEN="$(curl -fsS -u "${GITEA_ADMIN_USER}:${GITEA_ADMIN_PASSWORD}" \
  -X POST "${GITEA_BASE}/api/v1/users/${GITEA_ADMIN_USER}/tokens" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"${TOKEN_NAME}\",\"scopes\":[\"write:repository\",\"write:user\",\"write:admin\"]}" \
  2>/dev/null | sed -n 's/.*"sha1":"\([^"]*\)".*/\1/p')"
if [ -z "$GITEA_TOKEN" ]; then
  if [ "$EXTERNAL" = "0" ]; then
    log "warning: no Gitea token could be minted; --external=0 was asked for, so the build continues without the fork"
  else
    die "could not mint a Gitea token for ${GITEA_ADMIN_USER} at ${GITEA_BASE} — the fork path needs one (run scripts with the dev Gitea up, or pass --external=0)"
  fi
fi

# --- 3. an empty database, if asked ---------------------------------------
if [ "$RESET" = "1" ]; then
  log "resetting database $DB_NAME (drop + recreate)"
  go run ./tests/acceptance/seeddemo reset --db "$DB_URL" || die "reset failed"
fi

# --- 4. migrate ------------------------------------------------------------
log "applying migrations to $DB_NAME"
go run ./cmd/rddev db migrate --url "$DB_URL" >&2 || die "migrations failed for $DB_NAME"

# --- 5. start the API ------------------------------------------------------
if [ -z "$API_ADDR" ]; then
  PORT="$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)"
  API_ADDR="127.0.0.1:${PORT}"
fi

BIN="$(mktemp -d)/post-api"
log "building cmd/api"
go build -o "$BIN" ./cmd/api || die "go build ./cmd/api failed"

# Where the API's own stdout/stderr goes. A temp file by default; set
# SEED_DEMO_API_LOG to keep it, which is how a refusal that surfaces as a bare
# 503 (the fork path reports its provider failures that way) is diagnosed —
# from the API's log, not from the seed's summary.
if [ -n "${SEED_DEMO_API_LOG:-}" ]; then
  API_LOG="$SEED_DEMO_API_LOG"
  : > "$API_LOG"
else
  API_LOG="$(mktemp)"
fi
export POST_ENV=dev
export POST_API_ADDR="$API_ADDR"
export POST_MCP_ADDR="127.0.0.1:$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')"
export POST_DB_HOST="$DB_HOST" POST_DB_PORT="$DB_PORT" POST_DB_USER="$DB_USER"
export POST_DB_PASSWORD="$DB_PASSWORD" POST_DB_NAME="$DB_NAME" POST_DB_SSLMODE=disable
export POST_REDIS_ADDR="$REDIS_ADDR"
export POST_BLOB_ENDPOINT="$BLOB_ENDPOINT"
export POST_BLOB_ACCESS_KEY="${POST_BLOB_ACCESS_KEY:-post-dev-access}"
export POST_BLOB_SECRET_KEY="${POST_BLOB_SECRET_KEY:-post-dev-secret}"
export POST_BLOB_BUCKET="${POST_BLOB_BUCKET:-post}"
export POST_BLOB_USE_TLS=false
export POST_GITEA_BASE_URL="$GITEA_BASE"
if [ -n "$GITEA_TOKEN" ]; then
  export POST_GITEA_TOKEN="$GITEA_TOKEN"
  export POST_GITEA_WEBHOOK_URL="http://${API_ADDR}/api/v1/git/hooks/gitea"
  export POST_GITEA_ADMIN_USER="$GITEA_ADMIN_USER"
  export POST_GITEA_ADMIN_PASSWORD="$GITEA_ADMIN_PASSWORD"
fi

API_PID=""
stop_api() {
  if [ -n "$API_PID" ] && kill -0 "$API_PID" 2>/dev/null; then
    kill "$API_PID" 2>/dev/null
    for _ in 1 2 3 4 5 6 7 8 9 10; do
      kill -0 "$API_PID" 2>/dev/null || break
      sleep 0.3
    done
    kill -9 "$API_PID" 2>/dev/null
  fi
}
trap stop_api EXIT

log "starting the API on $API_ADDR"
"$BIN" >"$API_LOG" 2>&1 &
API_PID=$!
READY=0
for _ in $(seq 1 60); do
  if ! kill -0 "$API_PID" 2>/dev/null; then
    log "the API exited during startup; last lines of its log:"
    tail -20 "$API_LOG" >&2
    exit 1
  fi
  if curl -fsS "http://${API_ADDR}/healthz" >/dev/null 2>&1; then READY=1; break; fi
  sleep 0.5
done
[ "$READY" = "1" ] || { log "the API did not become healthy in 30s; last lines of its log:"; tail -20 "$API_LOG" >&2; exit 1; }

# --- 6. build --------------------------------------------------------------
log "seeding through the product API"
# The builder's own item-by-item report. A temp file by default; set
# SEED_DEMO_BUILD_REPORT to keep it — it is what a reader wants when they ask
# "did the second run find this item, or skip it?" (the summary counts
# created/reused/refused but does not list them).
BUILD_REPORT="${SEED_DEMO_BUILD_REPORT:-$(mktemp)}"
BUILD_RC=0
go run ./tests/acceptance/seeddemo build \
  --api "http://${API_ADDR}" \
  --db "$DB_URL" \
  --plan "$PLAN" \
  --external "$EXTERNAL" \
  --report "$BUILD_REPORT" || BUILD_RC=$?

# --- 7. verify -------------------------------------------------------------
log "verifying against PostgreSQL"
# The verifier's own report, every check with the SQL it ran. Same story as
# the build report above: temp by default, kept on request.
VERIFY_REPORT="${SEED_DEMO_VERIFY_REPORT:-$(mktemp)}"
VERIFY_RC=0
go run ./tests/acceptance/seeddemo verify \
  --db "$DB_URL" \
  --plan "$PLAN" \
  --external "$EXTERNAL" \
  --json "$VERIFY_REPORT" || VERIFY_RC=$?

# --- 8. stop the API and summarise ----------------------------------------
stop_api
API_PID=""

python3 - "$BUILD_REPORT" "$VERIFY_REPORT" "$SUMMARY" "$DB_NAME" "$BUILD_RC" "$VERIFY_RC" "$EXTERNAL" <<'PY'
import json, sys
build_path, verify_path, out_path, db_name, build_rc, verify_rc, external = sys.argv[1:8]
def load(p):
    try:
        with open(p) as fh:
            return json.load(fh)
    except Exception as exc:
        return {"unreadable": str(exc)}
build = load(build_path)
verify = load(verify_path)
summary = {
    "command": "ops/seed-demo.sh",
    "database": db_name,
    "external_contribution": external == "1",
    "build_exit_code": int(build_rc),
    "verify_exit_code": int(verify_rc),
    "ok": int(build_rc) == 0 and int(verify_rc) == 0,
    "project_id": verify.get("project_id") or build.get("project_id"),
    "project_slug": verify.get("project_slug") or build.get("project_slug"),
    "object_counts": {c["item"].split(" (")[0]: c["actual"] for c in verify.get("checks", [])},
    "checks": [{"item": c["item"], "required": c["required"], "actual": c["actual"], "status": c["status"]} for c in verify.get("checks", [])],
    "failed": verify.get("failed") or [],
    "not_checked": verify.get("unavailable") or [],
    "build_failures": [i for i in build.get("items", []) if i.get("status") == "failed"],
    # A refusal the plan asked for: docs/34 wants the demo to show a protocol
    # scientific conflict, and what proves it exists is the product refusing
    # the merge (MERGE_CONFLICT_UNDECIDED). Reporting it apart from
    # build_failures keeps "the demo worked" readable.
    "build_refusals": [i for i in build.get("items", []) if i.get("status") == "refused"],
    "notes": (build.get("notes", []) + verify.get("notes", [])),
}
with open(out_path, "w") as fh:
    json.dump(summary, fh, indent=2)
print(json.dumps(summary))
PY

if [ "$BUILD_RC" != "0" ] || [ "$VERIFY_RC" != "0" ]; then
  log "FAILED — see $SUMMARY"
  exit 1
fi
log "OK — see $SUMMARY"
exit 0
